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

// PartitionMode determines which partitioning strategy the controller uses.
type PartitionMode string

const (
	PartitionModeAuto       PartitionMode = "auto"
	PartitionModePartitions PartitionMode = "partitions"
	PartitionModeGroupings  PartitionMode = "groupings"
)

// ValidPartitionMode returns true if the given mode string is recognized.
func ValidPartitionMode(mode string) bool {
	switch PartitionMode(mode) {
	case PartitionModeAuto, PartitionModePartitions, PartitionModeGroupings:
		return true
	}
	return false
}

// Controller is the main topology coordinator controller.
// It watches ResourceSlices and ConfigMaps, builds a cross-driver topology model,
// computes aligned partitions, and publishes DeviceClasses describing partition shapes.
type Controller struct {
	client        kubernetes.Interface
	driverName    string
	partitionMode PartitionMode

	model            *TopologyModel
	ruleStore        *TopologyRuleStore
	groupingStore    *GroupingStore
	partitionBuilder *PartitionBuilder
	groupingBuilder  *GroupingBuilder
	classManager     *DeviceClassManager

	// synced is set after informer caches are synced and initial reconcile completes.
	// Event handlers skip enqueueing reconciles until this is true.
	synced bool

	// workqueue triggers a full reconciliation when topology changes
	workqueue workqueue.TypedRateLimitingInterface[string]
}

// NewController creates a new topology coordinator controller.
func NewController(client kubernetes.Interface, driverName string, partitionMode PartitionMode) *Controller {
	if driverName == "" {
		driverName = CoordinatorDriverName
	}
	if partitionMode == "" {
		partitionMode = PartitionModeAuto
	}

	model := NewTopologyModel()
	ruleStore := NewTopologyRuleStore()
	groupingStore := NewGroupingStore()

	klog.Infof("Partition mode: %s", partitionMode)

	return &Controller{
		client:           client,
		driverName:       driverName,
		partitionMode:    partitionMode,
		model:            model,
		ruleStore:        ruleStore,
		groupingStore:    groupingStore,
		partitionBuilder: NewPartitionBuilder(model, ruleStore),
		groupingBuilder:  NewGroupingBuilder(model, ruleStore),
		classManager:     NewDeviceClassManager(client, driverName, ruleStore),
		workqueue:        workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[string]()),
	}
}

// Model returns the topology model for use by the webhook.
func (c *Controller) Model() *TopologyModel {
	return c.model
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

	// Filtered factory for ConfigMaps with the device-grouping label
	groupingCMFactory := informers.NewSharedInformerFactoryWithOptions(c.client, defaultResyncPeriod,
		informers.WithTweakListOptions(func(opts *metav1.ListOptions) {
			opts.LabelSelector = DeviceGroupingLabel + "=true"
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

	// Watch ConfigMaps with the device-grouping label
	groupingCMInformer := groupingCMFactory.Core().V1().ConfigMaps().Informer()
	if _, err := groupingCMInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    c.onGroupingConfigMapAdd,
		UpdateFunc: c.onGroupingConfigMapUpdate,
		DeleteFunc: c.onGroupingConfigMapDelete,
	}); err != nil {
		return fmt.Errorf("failed to add grouping ConfigMap event handler: %w", err)
	}

	// Start informers
	factory.Start(ctx.Done())
	cmFactory.Start(ctx.Done())
	groupingCMFactory.Start(ctx.Done())

	// Wait for cache sync
	if !cache.WaitForCacheSync(ctx.Done(), sliceInformer.HasSynced, cmInformer.HasSynced, groupingCMInformer.HasSynced) {
		return fmt.Errorf("failed to sync informer caches")
	}
	klog.Info("Informer caches synced")

	// Build the model from the complete informer store to avoid
	// partial state from event handlers during startup.
	for _, obj := range sliceInformer.GetStore().List() {
		if slice, ok := obj.(*resourcev1.ResourceSlice); ok {
			c.model.UpdateFromResourceSlice(slice)
		}
	}
	for _, obj := range cmInformer.GetStore().List() {
		if cm, ok := obj.(*corev1.ConfigMap); ok {
			if err := c.ruleStore.LoadFromConfigMap(cm); err != nil {
				klog.Warningf("Failed to load topology rule from %s: %v", cm.Name, err)
			}
		}
	}
	c.model.SetRules(c.ruleStore.GetRules())

	// Load device grouping ConfigMaps
	for _, obj := range groupingCMInformer.GetStore().List() {
		if cm, ok := obj.(*corev1.ConfigMap); ok {
			if err := c.groupingStore.LoadFromConfigMap(cm); err != nil {
				klog.Warningf("Failed to load device grouping from %s: %v", cm.Name, err)
			}
		}
	}

	// Run reconciliation loop
	go c.runWorker(ctx)

	// Trigger initial reconciliation
	c.workqueue.Add("reconcile")
	c.synced = true

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

	groupings := c.groupingStore.GetGroupings()

	skipPartitions := c.partitionMode == PartitionModeGroupings
	skipGroupings := c.partitionMode == PartitionModePartitions

	partitionCount := 0
	groupingCount := 0
	nodeCount := 0

	if !skipPartitions {
		results := c.partitionBuilder.BuildPartitions()

		if err := c.classManager.SyncDeviceClasses(ctx, results); err != nil {
			metrics.ReconciliationErrors.Inc()
			return fmt.Errorf("failed to sync DeviceClasses: %w", err)
		}

		nodeCount = len(results)
		metrics.NodesTotal.Set(float64(nodeCount))
		partitionCount = countDeviceClasses(results)
	}

	if !skipGroupings {
		// Auto-detect PCIe root pairings (GPU+NIC, GPU+NVMe, etc.)
		autoGroupings := c.partitionBuilder.DetectPCIeRootPairings()

		// Merge: ConfigMap-defined groupings override auto-detected ones with the same name
		mergedGroupings := mergeGroupings(autoGroupings, groupings)

		if len(mergedGroupings) > 0 {
			groupingResults := c.groupingBuilder.BuildGroupings(mergedGroupings)

			if err := c.classManager.SyncGroupingDeviceClasses(ctx, groupingResults); err != nil {
				metrics.ReconciliationErrors.Inc()
				return fmt.Errorf("failed to sync grouping DeviceClasses: %w", err)
			}

			groupingCount = countGroupingDeviceClasses(groupingResults)
		}
	}

	metrics.ReconciliationDuration.Observe(time.Since(start).Seconds())
	metrics.DeviceClassesTotal.Set(float64(partitionCount + groupingCount))

	klog.Infof("Reconciliation complete (partitions): %d nodes, %d DeviceClasses",
		nodeCount, partitionCount)
	if groupingCount > 0 {
		klog.Infof("Reconciliation complete (groupings): %d grouping DeviceClasses", groupingCount)
	}

	return nil
}

// onSliceAdd handles a new ResourceSlice.
func (c *Controller) onSliceAdd(obj interface{}) {
	slice, ok := obj.(*resourcev1.ResourceSlice)
	if !ok {
		return
	}
	c.model.UpdateFromResourceSlice(slice)
	if c.synced {
		c.workqueue.AddAfter("reconcile", reconcileDebounceDelay)
	}
}

// onSliceUpdate handles a ResourceSlice update.
func (c *Controller) onSliceUpdate(_, newObj interface{}) {
	slice, ok := newObj.(*resourcev1.ResourceSlice)
	if !ok {
		return
	}
	c.model.UpdateFromResourceSlice(slice)
	if c.synced {
		c.workqueue.AddAfter("reconcile", reconcileDebounceDelay)
	}
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

// onGroupingConfigMapAdd handles a new grouping ConfigMap.
func (c *Controller) onGroupingConfigMapAdd(obj interface{}) {
	cm, ok := obj.(*corev1.ConfigMap)
	if !ok {
		return
	}
	if cm.Labels[DeviceGroupingLabel] != "true" {
		return
	}
	if err := c.groupingStore.LoadFromConfigMap(cm); err != nil {
		klog.Errorf("Failed to load device grouping from %s/%s: %v", cm.Namespace, cm.Name, err)
		return
	}
	if c.synced {
		c.workqueue.AddAfter("reconcile", reconcileDebounceDelay)
	}
}

// onGroupingConfigMapUpdate handles a grouping ConfigMap update.
func (c *Controller) onGroupingConfigMapUpdate(_, newObj interface{}) {
	cm, ok := newObj.(*corev1.ConfigMap)
	if !ok {
		return
	}
	if cm.Labels[DeviceGroupingLabel] != "true" {
		return
	}
	if err := c.groupingStore.LoadFromConfigMap(cm); err != nil {
		klog.Errorf("Failed to update device grouping from %s/%s: %v", cm.Namespace, cm.Name, err)
		return
	}
	if c.synced {
		c.workqueue.AddAfter("reconcile", reconcileDebounceDelay)
	}
}

// onGroupingConfigMapDelete handles a grouping ConfigMap deletion.
func (c *Controller) onGroupingConfigMapDelete(obj interface{}) {
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
	if cm.Labels[DeviceGroupingLabel] != "true" {
		return
	}
	c.groupingStore.RemoveConfigMap(cm.Namespace, cm.Name)
	c.workqueue.AddAfter("reconcile", reconcileDebounceDelay)
}

// mergeGroupings combines auto-detected and user-defined groupings.
// User-defined groupings (from ConfigMaps) override auto-detected ones
// with the same name.
func mergeGroupings(autoDetected, userDefined []DeviceGrouping) []DeviceGrouping {
	byName := make(map[string]DeviceGrouping)
	for _, g := range autoDetected {
		byName[g.Name] = g
	}
	for _, g := range userDefined {
		byName[g.Name] = g
	}
	merged := make([]DeviceGrouping, 0, len(byName))
	for _, g := range byName {
		merged = append(merged, g)
	}
	return merged
}

func countGroupingDeviceClasses(results []GroupingResult) int {
	seen := make(map[string]bool)
	for _, r := range results {
		for _, inst := range r.Instances {
			seen[inst.GroupingName+"-"+inst.Alignment] = true
		}
	}
	return len(seen)
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
