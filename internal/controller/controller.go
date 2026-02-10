package controller

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	resourcev1 "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	klog "k8s.io/klog/v2"

	"github.com/fabiendupont/k8s-dra-driver-nodepartition/internal/metrics"
)

const (
	defaultResyncPeriod    = 5 * time.Minute
	reconcileDebounceDelay = 2 * time.Second
)

// Controller is the main topology coordinator controller.
// It watches ResourceSlices and ConfigMaps, builds a cross-driver topology model,
// computes aligned partitions, and publishes DeviceClasses describing partition shapes.
type Controller struct {
	client     kubernetes.Interface
	driverName string

	model            *TopologyModel
	ruleStore        *TopologyRuleStore
	partitionBuilder *PartitionBuilder
	classManager     *DeviceClassManager

	// workqueue triggers a full reconciliation when topology changes
	workqueue workqueue.TypedRateLimitingInterface[string]
}

// NewController creates a new topology coordinator controller.
func NewController(client kubernetes.Interface, driverName string) *Controller {
	if driverName == "" {
		driverName = CoordinatorDriverName
	}

	model := NewTopologyModel()
	ruleStore := NewTopologyRuleStore()

	return &Controller{
		client:           client,
		driverName:       driverName,
		model:            model,
		ruleStore:        ruleStore,
		partitionBuilder: NewPartitionBuilder(model, ruleStore),
		classManager:     NewDeviceClassManager(client, driverName, ruleStore),
		workqueue:        workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[string]()),
	}
}

// Run starts the controller. It blocks until the context is cancelled.
func (c *Controller) Run(ctx context.Context) error {
	klog.Info("Starting topology coordinator controller")

	// Set up informers
	factory := informers.NewSharedInformerFactory(c.client, defaultResyncPeriod)

	// Filtered factory for ConfigMaps with the topology-rule label only
	cmFactory := informers.NewSharedInformerFactoryWithOptions(c.client, defaultResyncPeriod,
		informers.WithTweakListOptions(func(opts *metav1.ListOptions) {
			opts.LabelSelector = TopologyRuleLabel + "=true"
		}),
	)

	// Watch ResourceSlices from ALL drivers
	sliceInformer := factory.Resource().V1().ResourceSlices().Informer()
	if _, err := sliceInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    c.onSliceAdd,
		UpdateFunc: c.onSliceUpdate,
		DeleteFunc: c.onSliceDelete,
	}); err != nil {
		return fmt.Errorf("failed to add ResourceSlice event handler: %w", err)
	}

	// Watch only ConfigMaps with the topology-rule label
	cmInformer := cmFactory.Core().V1().ConfigMaps().Informer()
	if _, err := cmInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    c.onConfigMapAdd,
		UpdateFunc: c.onConfigMapUpdate,
		DeleteFunc: c.onConfigMapDelete,
	}); err != nil {
		return fmt.Errorf("failed to add ConfigMap event handler: %w", err)
	}

	// Start informers
	factory.Start(ctx.Done())
	cmFactory.Start(ctx.Done())

	// Wait for cache sync
	if !cache.WaitForCacheSync(ctx.Done(), sliceInformer.HasSynced, cmInformer.HasSynced) {
		return fmt.Errorf("failed to sync informer caches")
	}
	klog.Info("Informer caches synced")

	// Run reconciliation loop
	go c.runWorker(ctx)

	// Trigger initial reconciliation
	c.workqueue.Add("reconcile")

	<-ctx.Done()
	c.workqueue.ShutDown()
	klog.Info("Topology coordinator controller stopped")
	return nil
}

// runWorker processes items from the workqueue.
func (c *Controller) runWorker(ctx context.Context) {
	for {
		key, shutdown := c.workqueue.Get()
		if shutdown {
			return
		}

		if err := c.reconcile(ctx); err != nil {
			klog.Errorf("Reconciliation failed: %v", err)
			c.workqueue.AddRateLimited(key)
		} else {
			c.workqueue.Forget(key)
		}
		c.workqueue.Done(key)
	}
}

// reconcile performs a full reconciliation: recompute partitions and sync DeviceClasses.
func (c *Controller) reconcile(ctx context.Context) error {
	klog.V(4).Info("Running reconciliation")
	start := time.Now()

	// Update rules in the model
	rules := c.ruleStore.GetRules()
	c.model.SetRules(rules)
	metrics.TopologyRulesTotal.Set(float64(len(rules)))

	// Build partitions from the current topology
	results := c.partitionBuilder.BuildPartitions()

	// Sync DeviceClasses
	if err := c.classManager.SyncDeviceClasses(ctx, results); err != nil {
		metrics.ReconciliationErrors.Inc()
		return fmt.Errorf("failed to sync DeviceClasses: %w", err)
	}

	metrics.ReconciliationDuration.Observe(time.Since(start).Seconds())
	metrics.NodesTotal.Set(float64(len(results)))
	metrics.DeviceClassesTotal.Set(float64(countDeviceClasses(results)))

	klog.Infof("Reconciliation complete: %d nodes, %d DeviceClasses",
		len(results), countDeviceClasses(results))
	return nil
}

// onSliceAdd handles a new ResourceSlice.
func (c *Controller) onSliceAdd(obj interface{}) {
	slice, ok := obj.(*resourcev1.ResourceSlice)
	if !ok {
		return
	}
	c.model.UpdateFromResourceSlice(slice)
	c.workqueue.AddAfter("reconcile", reconcileDebounceDelay)
}

// onSliceUpdate handles a ResourceSlice update.
func (c *Controller) onSliceUpdate(_, newObj interface{}) {
	slice, ok := newObj.(*resourcev1.ResourceSlice)
	if !ok {
		return
	}
	c.model.UpdateFromResourceSlice(slice)
	c.workqueue.AddAfter("reconcile", reconcileDebounceDelay)
}

// onSliceDelete handles a ResourceSlice deletion.
func (c *Controller) onSliceDelete(obj interface{}) {
	slice, ok := obj.(*resourcev1.ResourceSlice)
	if !ok {
		tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
		if !ok {
			return
		}
		slice, ok = tombstone.Obj.(*resourcev1.ResourceSlice)
		if !ok {
			return
		}
	}
	c.model.RemoveResourceSlice(slice)
	c.workqueue.AddAfter("reconcile", reconcileDebounceDelay)
}

// onConfigMapAdd handles a new ConfigMap.
func (c *Controller) onConfigMapAdd(obj interface{}) {
	cm, ok := obj.(*corev1.ConfigMap)
	if !ok {
		return
	}
	if cm.Labels[TopologyRuleLabel] != "true" {
		return
	}
	if err := c.ruleStore.LoadFromConfigMap(cm); err != nil {
		klog.Errorf("Failed to load topology rule from %s/%s: %v", cm.Namespace, cm.Name, err)
		return
	}
	c.workqueue.AddAfter("reconcile", reconcileDebounceDelay)
}

// onConfigMapUpdate handles a ConfigMap update.
func (c *Controller) onConfigMapUpdate(_, newObj interface{}) {
	cm, ok := newObj.(*corev1.ConfigMap)
	if !ok {
		return
	}
	if cm.Labels[TopologyRuleLabel] != "true" {
		return
	}
	if err := c.ruleStore.LoadFromConfigMap(cm); err != nil {
		klog.Errorf("Failed to update topology rule from %s/%s: %v", cm.Namespace, cm.Name, err)
		return
	}
	c.workqueue.AddAfter("reconcile", reconcileDebounceDelay)
}

// onConfigMapDelete handles a ConfigMap deletion.
func (c *Controller) onConfigMapDelete(obj interface{}) {
	cm, ok := obj.(*corev1.ConfigMap)
	if !ok {
		tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
		if !ok {
			return
		}
		cm, ok = tombstone.Obj.(*corev1.ConfigMap)
		if !ok {
			return
		}
	}
	if cm.Labels[TopologyRuleLabel] != "true" {
		return
	}
	c.ruleStore.RemoveConfigMap(cm.Namespace, cm.Name)
	c.workqueue.AddAfter("reconcile", reconcileDebounceDelay)
}

func countDeviceClasses(results []PartitionResult) int {
	seen := make(map[string]bool)
	for _, r := range results {
		for _, p := range r.Partitions {
			seen[r.Profile+"-"+string(p.Type)] = true
		}
	}
	return len(seen)
}
