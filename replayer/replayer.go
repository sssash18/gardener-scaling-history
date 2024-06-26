package replayer

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	gsh "github.com/elankath/gardener-scaling-history"
	"github.com/elankath/gardener-scaling-history/db"
	gst "github.com/elankath/gardener-scaling-types"
	"github.com/samber/lo"
	"golang.org/x/exp/maps"
	"k8s.io/apimachinery/pkg/util/json"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"log/slog"
	"os"
	"strings"
	"time"
)

type defaultReplayer struct {
	dataAccess *db.DataAccess
	clientSet  *kubernetes.Clientset
	params     gsh.ReplayerParams
}

var _ gsh.Replayer = (*defaultReplayer)(nil)

func NewDefaultReplayer(params gsh.ReplayerParams) (gsh.Replayer, error) {
	// Load kubeconfig file
	config, err := clientcmd.BuildConfigFromFlags("", params.VirtualClusterKubeConfig)
	if err != nil {
		return nil, fmt.Errorf("cannot create client config: %w", err)
	}
	// Create clientset
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("cannot create clientset: %w", err)
	}
	return &defaultReplayer{
		dataAccess: db.NewDataAccess(params.DBPath),
		clientSet:  clientset,
		params:     params,
	}, nil
}

func (d *defaultReplayer) Start(ctx context.Context) error {
	err := d.dataAccess.Init()
	if err != nil {
		return err
	}
	clusterSnapshot, err := d.GetInitialClusterSnapshot()
	if err != nil {
		return err
	}
	autoscalerConfig := clusterSnapshot.AutoscalerConfig
	bytes, err := json.Marshal(autoscalerConfig)
	if err != nil {
		return err
	}
	err = os.WriteFile(d.params.VirtualAutoScalerConfig, bytes, 0644)
	if err != nil {
		return err
	}
	slog.Info("wrote initial autoscaler config, waiting for stabilization", "config", d.params.VirtualAutoScalerConfig,
		"stabilizeInterval", d.params.StabilizeInterval)
	<-time.After(d.params.StabilizeInterval)
	for {
		select {}
	}
	return nil
}

func (d *defaultReplayer) Close() error {
	return d.dataAccess.Close()
}

func (d *defaultReplayer) GetClusterSnapshot(startTime time.Time) (cs gsh.ClusterSnapshot, err error) {
	mccs, err := d.dataAccess.LoadMachineClassInfosBefore(startTime)
	if err != nil {
		return
	}
	mcds, err := d.dataAccess.LoadMachineDeploymentInfosBefore(startTime)
	if err != nil {
		return
	}
	workerPools, err := d.dataAccess.LoadWorkerPoolInfosBefore(startTime)
	if err != nil {
		return
	}

	var autoscalerConfig gst.AutoScalerConfig
	autoscalerConfig.NodeTemplates, err = GetNodeTemplates(mccs, mcds)
	if err != nil {
		return
	}

	autoscalerConfig.NodeGroups, err = GetNodeGroups(mcds, workerPools)
	if err != nil {
		return
	}

	autoscalerConfig.CASettings, err = d.dataAccess.LoadCASettingsBefore(startTime)

	cs.AutoscalerConfig = autoscalerConfig
	cs.AutoscalerConfig.Mode = gst.AutoscalerReplayerMode
	cs.SnapshotTime = startTime

	cs.UnscheduledPods, err = d.dataAccess.GetLatestUnscheduledPodsBeforeTimestamp(startTime)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {

		return
	}

	cs.ScheduledPods, err = d.dataAccess.GetLatestScheduledPodsBeforeTimestamp(startTime)
	if err != nil {
		return
	}

	cs.Nodes, err = d.dataAccess.LoadNodeInfosBefore(startTime)
	if err != nil {
		return
	}

	return
}

func (d *defaultReplayer) GetParams() gsh.ReplayerParams {
	//TODO implement me
	panic("implement me")
}

func GetNodeGroupNameFromMCCName(namespace, mccName string) string {
	idx := strings.LastIndex(mccName, "-")
	// mcc name - shoot--i585976--suyash-local-worker-1-z1-0af3f , we omit the hash from the mcc name to match it with the nodegroup name
	trimmedName := mccName[0:idx]
	return fmt.Sprintf("%s.%s", namespace, trimmedName)
}

func constructNodeTemplateFromMCC(mcc gsh.MachineClassInfo) gst.NodeTemplate {
	return gst.NodeTemplate{
		Name:         GetNodeGroupNameFromMCCName(mcc.Namespace, mcc.Name),
		Capacity:     mcc.Capacity,
		InstanceType: mcc.InstanceType,
		Region:       mcc.Region,
		Zone:         mcc.Zone,
		Labels:       mcc.Labels,
		Taints:       nil,
	}
}

func GetNodeTemplates(mccs []gsh.MachineClassInfo, mcds []gst.MachineDeploymentInfo) (nodeTemplates map[string]gst.NodeTemplate, err error) {
	nodeTemplates = make(map[string]gst.NodeTemplate)
	for _, mcc := range mccs {
		nodeTemplate := constructNodeTemplateFromMCC(mcc)
		nodeTemplates[nodeTemplate.Name] = nodeTemplate
	}
	for _, mcd := range mcds {
		ngName := fmt.Sprintf("%s.%s", mcd.Namespace, mcd.Name)
		nodeTemplate, ok := nodeTemplates[ngName]
		if !ok {
			err = fmt.Errorf("cannot find the node template for nodegroup: %s", ngName)
			return
		}
		nodeTemplate.Taints = mcd.Taints
		maps.Copy(nodeTemplate.Labels, mcd.Labels)
		nodeTemplates[ngName] = nodeTemplate
	}
	return
}

func GetNodeGroups(mcds []gst.MachineDeploymentInfo, workerPools []gst.WorkerPoolInfo) (nodeGroups map[string]gst.NodeGroupInfo, err error) {
	nodeGroups = make(map[string]gst.NodeGroupInfo)
	workerPoolsByName := lo.KeyBy(workerPools, func(item gst.WorkerPoolInfo) string {
		return item.Name
	})
	for _, mcd := range mcds {
		workerPool, ok := workerPoolsByName[mcd.PoolName]
		if !ok {
			err = fmt.Errorf("cannot find pool name with name %q: %w", mcd.PoolName, gst.ErrKeyNotFound)
			return
		}
		nodeGroup := gst.NodeGroupInfo{
			Name:       fmt.Sprintf("%s.%s", mcd.Namespace, mcd.Name),
			PoolName:   mcd.PoolName,
			Zone:       mcd.Zone,
			TargetSize: mcd.Replicas,
			MinSize:    workerPool.Minimum,
			MaxSize:    workerPool.Maximum,
		}
		nodeGroup.Hash = nodeGroup.GetHash()
		nodeGroups[nodeGroup.Name] = nodeGroup
	}
	return
}

func (d *defaultReplayer) GetInitialClusterSnapshot() (gsh.ClusterSnapshot, error) {
	startTime, err := d.dataAccess.GetInitialRecorderStartTime()
	if err != nil {
		return gsh.ClusterSnapshot{}, err
	}
	startTime = startTime.Add(1 * time.Minute)

	return d.GetClusterSnapshot(startTime)
}