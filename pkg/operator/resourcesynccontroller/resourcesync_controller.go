package resourcesynccontroller

import (
	"fmt"
	"sync"
	"time"

	"github.com/golang/glog"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"

	operatorv1 "github.com/openshift/api/operator/v1"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/resource/resourceapply"
	"github.com/openshift/library-go/pkg/operator/staticpod/controller/common"
)

const (
	operatorStatusResourceSyncControllerFailing = "ResourceSyncControllerFailing"
	controllerWorkQueueKey                      = "key"
)

// ResourceSyncController is a controller that will copy source configmaps and secrets to their destinations.
// It will also mirror deletions by deleting destinations.
type ResourceSyncController struct {
	// syncRuleLock is used to ensure we avoid races on changes to syncing rules
	syncRuleLock sync.RWMutex
	// configMapSyncRules is a map from destination location to source location
	configMapSyncRules map[ResourceLocation]ResourceLocation
	// secretSyncRules is a map from destination location to source location
	secretSyncRules map[ResourceLocation]ResourceLocation

	// knownNamespaces is the list of namespaces we are watching.
	knownNamespaces sets.String

	preRunCachesSynced []cache.InformerSynced

	// queue only ever has one item, but it has nice error handling backoff/retry semantics
	queue workqueue.RateLimitingInterface

	kubeClient           kubernetes.Interface
	operatorConfigClient common.OperatorClient
	eventRecorder        events.Recorder
}

var _ ResourceSyncer = &ResourceSyncController{}

// NewResourceSyncController creates ResourceSyncController.
func NewResourceSyncController(
	operatorConfigClient common.OperatorClient,
	kubeInformersForNamespaces map[string]informers.SharedInformerFactory,
	kubeClient kubernetes.Interface,
	eventRecorder events.Recorder,
) *ResourceSyncController {
	c := &ResourceSyncController{
		operatorConfigClient: operatorConfigClient,
		eventRecorder:        eventRecorder,

		configMapSyncRules: map[ResourceLocation]ResourceLocation{},
		secretSyncRules:    map[ResourceLocation]ResourceLocation{},
		knownNamespaces:    sets.StringKeySet(kubeInformersForNamespaces),

		queue:      workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "ResourceSyncController"),
		kubeClient: kubeClient,
	}

	for _, informers := range kubeInformersForNamespaces {
		informers.Core().V1().ConfigMaps().Informer().AddEventHandler(c.eventHandler())
		informers.Core().V1().Secrets().Informer().AddEventHandler(c.eventHandler())
		c.preRunCachesSynced = append(c.preRunCachesSynced, informers.Core().V1().ConfigMaps().Informer().HasSynced)
		c.preRunCachesSynced = append(c.preRunCachesSynced, informers.Core().V1().Secrets().Informer().HasSynced)
	}

	// we watch this just in case someone messes with our status
	operatorConfigClient.Informer().AddEventHandler(c.eventHandler())

	return c
}

func (c *ResourceSyncController) SyncConfigMap(destination, source ResourceLocation) error {
	if !c.knownNamespaces.Has(destination.Namespace) {
		return fmt.Errorf("not watching namespace %q", destination.Namespace)
	}
	if source != emptyResourceLocation && !c.knownNamespaces.Has(source.Namespace) {
		return fmt.Errorf("not watching namespace %q", source.Namespace)
	}

	c.syncRuleLock.Lock()
	defer c.syncRuleLock.Unlock()
	c.configMapSyncRules[destination] = source

	// make sure the new rule is picked up
	c.queue.Add(controllerWorkQueueKey)
	return nil
}

func (c *ResourceSyncController) SyncSecret(destination, source ResourceLocation) error {
	if !c.knownNamespaces.Has(destination.Namespace) {
		return fmt.Errorf("not watching namespace %q", destination.Namespace)
	}
	if source != emptyResourceLocation && !c.knownNamespaces.Has(source.Namespace) {
		return fmt.Errorf("not watching namespace %q", source.Namespace)
	}

	c.syncRuleLock.Lock()
	defer c.syncRuleLock.Unlock()
	c.secretSyncRules[destination] = source

	// make sure the new rule is picked up
	c.queue.Add(controllerWorkQueueKey)
	return nil
}

func (c *ResourceSyncController) sync() error {
	operatorSpec, _, _, err := c.operatorConfigClient.Get()
	if err != nil {
		return err
	}

	switch operatorSpec.ManagementState {
	case operatorv1.Unmanaged:
		return nil
	case operatorv1.Removed:
		// TODO: Should we try to actively remove the resources created by this controller here?
		return nil
	}

	c.syncRuleLock.RLock()
	defer c.syncRuleLock.RUnlock()

	errors := []error{}

	for destination, source := range c.configMapSyncRules {
		if source == emptyResourceLocation {
			if err := c.kubeClient.CoreV1().ConfigMaps(destination.Namespace).Delete(destination.Name, nil); err != nil && !apierrors.IsNotFound(err) {
				errors = append(errors, err)
			}
			continue
		}

		_, _, err := resourceapply.SyncConfigMap(c.kubeClient.CoreV1(), c.eventRecorder, source.Namespace, source.Name, destination.Namespace, destination.Name, []metav1.OwnerReference{})
		if err != nil {
			errors = append(errors, err)
		}
	}
	for destination, source := range c.secretSyncRules {
		if source == emptyResourceLocation {
			if err := c.kubeClient.CoreV1().Secrets(destination.Namespace).Delete(destination.Name, nil); err != nil && !apierrors.IsNotFound(err) {
				errors = append(errors, err)
			}
			continue
		}

		_, _, err := resourceapply.SyncSecret(c.kubeClient.CoreV1(), c.eventRecorder, source.Namespace, source.Name, destination.Namespace, destination.Name, []metav1.OwnerReference{})
		if err != nil {
			errors = append(errors, err)
		}
	}

	if len(errors) > 0 {
		cond := operatorv1.OperatorCondition{
			Type:    operatorStatusResourceSyncControllerFailing,
			Status:  operatorv1.ConditionTrue,
			Reason:  "Error",
			Message: common.NewMultiLineAggregate(errors).Error(),
		}
		if _, _, updateError := common.UpdateStatus(c.operatorConfigClient, common.UpdateConditionFn(cond)); updateError != nil {
			return updateError
		}
		return nil
	}

	cond := operatorv1.OperatorCondition{
		Type:   operatorStatusResourceSyncControllerFailing,
		Status: operatorv1.ConditionFalse,
	}
	if _, _, updateError := common.UpdateStatus(c.operatorConfigClient, common.UpdateConditionFn(cond)); updateError != nil {
		return updateError
	}
	return nil
}

func (c *ResourceSyncController) Run(workers int, stopCh <-chan struct{}) {
	defer utilruntime.HandleCrash()
	defer c.queue.ShutDown()

	glog.Infof("Starting ResourceSyncController")
	defer glog.Infof("Shutting down ResourceSyncController")
	if !cache.WaitForCacheSync(stopCh, c.preRunCachesSynced...) {
		return
	}

	// doesn't matter what workers say, only start one.
	go wait.Until(c.runWorker, time.Second, stopCh)

	<-stopCh
}

func (c *ResourceSyncController) runWorker() {
	for c.processNextWorkItem() {
	}
}

func (c *ResourceSyncController) processNextWorkItem() bool {
	dsKey, quit := c.queue.Get()
	if quit {
		return false
	}
	defer c.queue.Done(dsKey)

	err := c.sync()
	if err == nil {
		c.queue.Forget(dsKey)
		return true
	}

	utilruntime.HandleError(fmt.Errorf("%v failed with : %v", dsKey, err))
	c.queue.AddRateLimited(dsKey)

	return true
}

// eventHandler queues the operator to check spec and status
func (c *ResourceSyncController) eventHandler() cache.ResourceEventHandler {
	return cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj interface{}) { c.queue.Add(controllerWorkQueueKey) },
		UpdateFunc: func(old, new interface{}) { c.queue.Add(controllerWorkQueueKey) },
		DeleteFunc: func(obj interface{}) { c.queue.Add(controllerWorkQueueKey) },
	}
}
