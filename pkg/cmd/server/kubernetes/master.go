package kubernetes

import (
	"fmt"
	"io/ioutil"
	"os"
	"time"

	"github.com/emicklei/go-restful"
	"github.com/golang/glog"

	kapi "github.com/GoogleCloudPlatform/kubernetes/pkg/api"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/api/resource"
	kclient "github.com/GoogleCloudPlatform/kubernetes/pkg/client"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/client/record"
	minioncontroller "github.com/GoogleCloudPlatform/kubernetes/pkg/cloudprovider/nodecontroller"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/controller"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/master"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/resourcequota"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/service"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/util"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/volumeclaimbinder"
	"github.com/GoogleCloudPlatform/kubernetes/plugin/pkg/scheduler"
	_ "github.com/GoogleCloudPlatform/kubernetes/plugin/pkg/scheduler/algorithmprovider"
	schedulerapi "github.com/GoogleCloudPlatform/kubernetes/plugin/pkg/scheduler/api"
	latestschedulerapi "github.com/GoogleCloudPlatform/kubernetes/plugin/pkg/scheduler/api/latest"
	"github.com/GoogleCloudPlatform/kubernetes/plugin/pkg/scheduler/factory"

	"github.com/GoogleCloudPlatform/kubernetes/pkg/namespace"
)

const (
	KubeAPIPrefix        = "/api"
	KubeAPIPrefixV1Beta1 = "/api/v1beta1"
	KubeAPIPrefixV1Beta2 = "/api/v1beta2"
	KubeAPIPrefixV1Beta3 = "/api/v1beta3"
	KubeAPIPrefixV1      = "/api/v1"
)

// TODO: Longer term we should read this from some config store, rather than a flag.
func (c *MasterConfig) EnsurePortalFlags() {
	if c.PortalNet == nil {
		glog.Fatal("No --portal-net specified")
	}
}

// InstallAPI starts a Kubernetes master and registers the supported REST APIs
// into the provided mux, then returns an array of strings indicating what
// endpoints were started (these are format strings that will expect to be sent
// a single string value).
func (c *MasterConfig) InstallAPI(container *restful.Container) []string {
	kubeletClient, err := kclient.NewKubeletClient(c.KubeletClientConfig)
	if err != nil {
		glog.Fatalf("Unable to configure Kubelet client: %v", err)
	}

	masterConfig := &master.Config{
		PublicAddress: c.MasterIP,
		ReadWritePort: c.MasterPort,
		ReadOnlyPort:  c.MasterPort,

		EtcdHelper: c.EtcdHelper,

		EventTTL: 2 * time.Hour,

		PortalNet: c.PortalNet,

		RequestContextMapper: c.RequestContextMapper,

		RestfulContainer: container,
		KubeletClient:    kubeletClient,
		APIPrefix:        KubeAPIPrefix,

		EnableV1: true,

		EnableCoreControllers: true,

		MasterCount: c.MasterCount,

		Authorizer:       c.Authorizer,
		AdmissionControl: c.AdmissionControl,
	}
	_ = master.New(masterConfig)

	return []string{
		fmt.Sprintf("Started Kubernetes API at %%s%s", KubeAPIPrefixV1Beta1),
		fmt.Sprintf("Started Kubernetes API at %%s%s", KubeAPIPrefixV1Beta3),
		fmt.Sprintf("Started Kubernetes API at %%s%s", KubeAPIPrefixV1Beta3),
		fmt.Sprintf("Started Kubernetes API at %%s%s (experimental)", KubeAPIPrefixV1),
	}
}

func (c *MasterConfig) RunNamespaceController() {
	namespaceController := namespace.NewNamespaceManager(c.KubeClient, 5*time.Minute)
	namespaceController.Run()
	glog.Infof("Started Kubernetes Namespace Manager")
}

func (c *MasterConfig) RunPersistentVolumeClaimBinder() {
	binder := volumeclaimbinder.NewPersistentVolumeClaimBinder(c.KubeClient, 5*time.Minute)
	binder.Run()
	glog.Infof("Started Kubernetes Persistent Volume Claim Binder")
}

// RunReplicationController starts the Kubernetes replication controller sync loop
func (c *MasterConfig) RunReplicationController() {
	controllerManager := controller.NewReplicationManager(c.KubeClient, controller.BurstReplicas)
	go controllerManager.Run(5, util.NeverStop)
	glog.Infof("Started Kubernetes Replication Manager")
}

// RunEndpointController starts the Kubernetes replication controller sync loop
func (c *MasterConfig) RunEndpointController() {
	endpoints := service.NewEndpointController(c.KubeClient)
	go endpoints.Run(5, util.NeverStop)

	glog.Infof("Started Kubernetes Endpoint Controller")
}

// RunScheduler starts the Kubernetes scheduler
func (c *MasterConfig) RunScheduler() {
	config, err := c.createSchedulerConfig()
	if err != nil {
		glog.Fatalf("Unable to start scheduler: %v", err)
	}
	eventcast := record.NewBroadcaster()
	config.Recorder = eventcast.NewRecorder(kapi.EventSource{Component: "scheduler"})
	eventcast.StartRecordingToSink(c.KubeClient.Events(""))

	s := scheduler.New(config)
	s.Run()
	glog.Infof("Started Kubernetes Scheduler")
}

func (c *MasterConfig) RunResourceQuotaManager() {
	resourceQuotaManager := resourcequota.NewResourceQuotaManager(c.KubeClient)
	resourceQuotaManager.Run(10 * time.Second)
}

func (c *MasterConfig) RunMinionController() {
	nodeResources := &kapi.NodeResources{
		Capacity: kapi.ResourceList{
			kapi.ResourceCPU:    *resource.NewMilliQuantity(int64(1*1000), resource.DecimalSI),
			kapi.ResourceMemory: *resource.NewQuantity(int64(3*1024*1024*1024), resource.BinarySI),
		},
	}

	minionController := minioncontroller.NewNodeController(
		nil, "", c.NodeHosts, nodeResources,
		c.KubeClient,
		10,            // registerRetryCount
		5*time.Minute, // podEvictionTimeout

		util.NewTokenBucketRateLimiter(0.1, 10), // deleting pods qps / burst

		40*time.Second, // monitor grace
		1*time.Minute,  // startup grace
		10*time.Second, // monitor period

		nil,   // clusterCIDR
		false, // allocateNodeCIDRs
	)
	minionController.Run(10*time.Second, true)

	glog.Infof("Started Kubernetes Minion Controller")
}

func (c *MasterConfig) createSchedulerConfig() (*scheduler.Config, error) {
	var policy schedulerapi.Policy
	var configData []byte

	configFactory := factory.NewConfigFactory(c.KubeClient)
	if _, err := os.Stat(c.SchedulerConfigFile); err == nil {
		configData, err = ioutil.ReadFile(c.SchedulerConfigFile)
		if err != nil {
			return nil, fmt.Errorf("Unable to read scheduler config: %v", err)
		}
		err = latestschedulerapi.Codec.DecodeInto(configData, &policy)
		if err != nil {
			return nil, fmt.Errorf("Invalid scheduler configuration: %v", err)
		}

		return configFactory.CreateFromConfig(policy)
	}

	// if the config file isn't provided, use the default provider
	return configFactory.CreateFromProvider(factory.DefaultProvider)
}
