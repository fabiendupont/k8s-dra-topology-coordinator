package controller

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"k8s.io/apimachinery/pkg/api/resource"
	klog "k8s.io/klog/v2"
)

// PartitionType identifies the size of a partition relative to the whole node.
type PartitionType string

const (
	PartitionEighth  PartitionType = "eighth"
	PartitionQuarter PartitionType = "quarter"
	PartitionHalf    PartitionType = "half"
	PartitionFull    PartitionType = "full"
)

// PartitionDevice represents a computed partition that the coordinator will publish
// as a device in its own ResourceSlice.
type PartitionDevice struct {
	// Name is the unique device name within the ResourceSlice.
	Name string
	// NodeName is the Kubernetes node this partition belongs to.
	NodeName string
	// Type is the partition size (eighth, quarter, half, full).
	Type PartitionType
	// NUMANode is the NUMA node ID for this partition (may span multiple for larger partitions).
	NUMANodes []int64
	// PCIeRoots lists the PCIe root complexes included in this partition.
	PCIeRoots []string
	// Sockets lists the CPU sockets included in this partition.
	Sockets []int64

	// DeviceCounts maps driver name -> count of devices from that driver in this partition.
	DeviceCounts map[string]int
	// DeviceCapacity maps driver name -> capacity requests for shared devices.
	// Used when a device is shared via DRAConsumableCapacity (count=1 but subdivided).
	DeviceCapacity map[string]map[string]string
	// Devices lists the individual topology devices grouped into this partition.
	Devices []TopologyDevice

	// CPUCount is the number of CPUs in this partition (from kubelet plugin discovery).
	CPUCount int
	// MemoryBytes is the amount of memory in this partition.
	MemoryBytes int64

	// ExtendedAttributes contains additional topology attributes from topology rules.
	ExtendedAttributes map[string]DeviceAttributeValue

	// Profile is a human-readable label for the node hardware profile (e.g., "hgx-b200").
	Profile string
}

// PartitionResult holds all computed partitions for a single node.
type PartitionResult struct {
	NodeName   string
	Profile    string
	Partitions []PartitionDevice
}

// PartitionBuilder computes aligned partition combinations from the topology model.
type PartitionBuilder struct {
	model *TopologyModel
	rules *TopologyRuleStore
}

// NewPartitionBuilder creates a partition builder.
func NewPartitionBuilder(model *TopologyModel, rules *TopologyRuleStore) *PartitionBuilder {
	return &PartitionBuilder{
		model: model,
		rules: rules,
	}
}

// BuildPartitions computes partition devices for all nodes in the topology model.
func (b *PartitionBuilder) BuildPartitions() []PartitionResult {
	nodes := b.model.GetNodeTopologies()
	groupingRules := b.rules.GetGroupingRules()

	var results []PartitionResult
	for nodeName, nodeTopo := range nodes {
		result := b.buildNodePartitions(nodeName, nodeTopo, groupingRules)
		if len(result.Partitions) > 0 {
			results = append(results, result)
		}
	}
	return results
}

// buildNodePartitions computes partitions for a single node.
func (b *PartitionBuilder) buildNodePartitions(
	nodeName string,
	nodeTopo *NodeTopology,
	groupingRules []TopologyRule,
) PartitionResult {
	result := PartitionResult{
		NodeName: nodeName,
	}

	allDevices := nodeTopo.AllDevices()
	if len(allDevices) == 0 {
		return result
	}

	// Identify distinct drivers (excluding our own coordinator driver)
	driverDeviceCounts := make(map[string]int)
	for _, d := range allDevices {
		baseName := baseDriverName(d.DriverName)
		driverDeviceCounts[baseName]++
	}

	// Group devices by NUMA node
	byNUMA := groupDevicesByAttribute(allDevices, func(d TopologyDevice) string {
		if d.NUMANode != nil {
			return fmt.Sprintf("%d", *d.NUMANode)
		}
		return ""
	})

	// Group devices by socket
	bySocket := groupDevicesByAttribute(allDevices, func(d TopologyDevice) string {
		if d.Socket != nil {
			return fmt.Sprintf("%d", *d.Socket)
		}
		return ""
	})

	// Validate grouping alignment using extended rules
	for _, rule := range groupingRules {
		if !b.validateGroupingAlignment(allDevices, rule) {
			klog.Warningf("Node %s: devices not aligned by rule attribute %s, skipping extended grouping",
				nodeName, rule.Attribute)
		}
	}

	// Build partitions by successive bisection.
	// Walk socket → NUMA → PCIe root; assign half/quarter/eighth
	// to boundaries that actually split. Skip boundaries that don't.
	profile := b.inferProfile(driverDeviceCounts)
	result.Profile = profile

	tiers := []PartitionType{PartitionHalf, PartitionQuarter, PartitionEighth}
	tierIdx := 0
	prevCount := 1

	socketCount := countNonEmptyGroups(bySocket)
	numaCount := countNonEmptyGroups(byNUMA)

	socketTier := PartitionType("")
	numaTier := PartitionType("")
	pcieTier := PartitionType("")

	if socketCount > prevCount && tierIdx < len(tiers) {
		socketTier = tiers[tierIdx]
		prevCount = socketCount
		tierIdx++
	}
	if numaCount > prevCount && tierIdx < len(tiers) {
		numaTier = tiers[tierIdx]
		tierIdx++
	}
	if tierIdx < len(tiers) {
		pcieTier = tiers[tierIdx]
	}

	if pcieTier != "" {
		p := b.buildProportionalPartitions(nodeName, profile, pcieTier, byNUMA, allDevices)
		result.Partitions = append(result.Partitions, p...)
	}

	if numaTier != "" {
		p := b.buildPartitionsFromGroups(nodeName, profile, numaTier, byNUMA, groupingRules)
		result.Partitions = append(result.Partitions, p...)
	}

	if socketTier != "" {
		p := b.buildPartitionsFromGroups(nodeName, profile, socketTier, bySocket, groupingRules)
		result.Partitions = append(result.Partitions, p...)
	}

	full := b.buildFullPartition(nodeName, profile, allDevices, groupingRules)
	if full != nil {
		result.Partitions = append(result.Partitions, *full)
	}

	tierCounts := make(map[PartitionType]int)
	for _, p := range result.Partitions {
		tierCounts[p.Type]++
	}
	klog.Infof("Node %s (profile=%s): computed %d partitions (%d eighth, %d quarter, %d half, %d full)",
		nodeName, profile, len(result.Partitions),
		tierCounts[PartitionEighth], tierCounts[PartitionQuarter],
		tierCounts[PartitionHalf], tierCounts[PartitionFull])

	return result
}

// buildPartitionsFromGroups creates partition devices from grouped devices.
func (b *PartitionBuilder) buildPartitionsFromGroups(
	nodeName, profile string,
	partType PartitionType,
	groups map[string][]TopologyDevice,
	groupingRules []TopologyRule,
) []PartitionDevice {
	// Don't create partitions if there's only one group (it would duplicate the full partition)
	// or if the grouping key is empty (devices lack the attribute)
	validGroups := make(map[string][]TopologyDevice)
	for key, devices := range groups {
		if key != "" {
			validGroups[key] = devices
		}
	}

	if len(validGroups) <= 1 {
		return nil
	}

	// Validate that extended grouping rules are satisfied.
	// Build a new map to avoid mutating validGroups during iteration.
	for _, rule := range groupingRules {
		splitGroups := make(map[string][]TopologyDevice)
		for key, devices := range validGroups {
			if !devicesShareAttribute(devices, rule.Attribute) {
				klog.V(4).Infof("Partition group %s on node %s: devices don't share %s, splitting further",
					key, nodeName, rule.Attribute)
				subGroups := groupDevicesByExtendedAttribute(devices, rule.Attribute)
				for subKey, subDevices := range subGroups {
					splitGroups[key+"-"+subKey] = subDevices
				}
			} else {
				splitGroups[key] = devices
			}
		}
		validGroups = splitGroups
	}

	// Sort group keys for deterministic output
	keys := make([]string, 0, len(validGroups))
	for k := range validGroups {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var partitions []PartitionDevice
	for i, key := range keys {
		devices := validGroups[key]
		p := buildPartitionFromDevices(
			fmt.Sprintf("%s-%s-%d", nodeName, partType, i),
			nodeName, profile, partType, devices,
		)
		partitions = append(partitions, p)
	}

	return partitions
}

// buildProportionalPartitions subdivides each NUMA node into equal partitions.
// It counts PCIe root groups per NUMA to determine the subdivision factor,
// then divides all device types proportionally. Shared devices (count=1 per NUMA)
// get capacity divided via DRAConsumableCapacity.
func (b *PartitionBuilder) buildProportionalPartitions(
	nodeName, profile string,
	partType PartitionType,
	byNUMA map[string][]TopologyDevice,
	allDevices []TopologyDevice,
) []PartitionDevice {
	// Count PCIe root groups per NUMA node to determine subdivision factor
	numaSubdivisions := make(map[string]int) // NUMA key → number of PCIe roots
	for _, d := range allDevices {
		if d.NUMANode == nil || d.PCIeRoot == nil {
			continue
		}
		numaKey := fmt.Sprintf("%d", *d.NUMANode)
		numaSubdivisions[numaKey]++
	}

	// Deduplicate: count unique PCIe roots per NUMA
	numaPCIeRoots := make(map[string]map[string]bool)
	for _, d := range allDevices {
		if d.NUMANode == nil || d.PCIeRoot == nil {
			continue
		}
		numaKey := fmt.Sprintf("%d", *d.NUMANode)
		if numaPCIeRoots[numaKey] == nil {
			numaPCIeRoots[numaKey] = make(map[string]bool)
		}
		numaPCIeRoots[numaKey][*d.PCIeRoot] = true
	}

	// For each NUMA node, compute how many quarter partitions to create
	var partitions []PartitionDevice
	partIdx := 0

	numaKeys := make([]string, 0, len(byNUMA))
	for k := range byNUMA {
		if k != "" {
			numaKeys = append(numaKeys, k)
		}
	}
	sort.Strings(numaKeys)

	for _, numaKey := range numaKeys {
		devices := byNUMA[numaKey]
		numPCIeRoots := len(numaPCIeRoots[numaKey])
		if numPCIeRoots <= 1 {
			continue // no subdivision possible
		}

		// Count devices per driver on this NUMA node
		driverCounts := make(map[string]int)
		for _, d := range devices {
			baseName := baseDriverName(d.DriverName)
			driverCounts[baseName]++
		}

		// Determine subdivision factor: use the number of unique PCIe roots
		// but only consider drivers with multiple devices (>1).
		// Drivers with 1 device per NUMA (e.g., CPU, memory) are shared
		// via DRAConsumableCapacity and don't limit subdivision.
		subdivisions := numPCIeRoots
		for driver, count := range driverCounts {
			if count > 1 && count < subdivisions {
				klog.V(4).Infof("NUMA %s: reducing subdivisions from %d to %d (limited by %s with %d devices)",
					numaKey, subdivisions, count, driver, count)
				subdivisions = count
			}
		}

		if subdivisions <= 1 {
			continue
		}

		// Parse NUMA node ID for topology-aware CEL selectors
		numaVal, _ := strconv.ParseInt(numaKey, 10, 64)

		// Create subdivided partitions
		for i := 0; i < subdivisions; i++ {
			p := PartitionDevice{
				Name:               fmt.Sprintf("%s-%s-%d", nodeName, partType, partIdx),
				NodeName:           nodeName,
				Type:               partType,
				Profile:            profile,
				NUMANodes:          []int64{numaVal},
				DeviceCounts:       make(map[string]int),
				Devices:            nil, // representative only
				ExtendedAttributes: make(map[string]DeviceAttributeValue),
			}

			p.DeviceCapacity = make(map[string]map[string]string)
			for driver, count := range driverCounts {
				divided := count / subdivisions
				if divided == 0 {
					divided = 1 // shared devices get count=1 per partition
					// For shared devices, compute capacity per partition
					// by dividing total capacity by number of subdivisions
					for _, d := range devices {
						if baseDriverName(d.DriverName) == driver && len(d.Capacity) > 0 {
							capPerPartition := make(map[string]string)
							for capName, capVal := range d.Capacity {
								divided := divideQuantity(capVal, subdivisions)
								if divided != "" {
									capPerPartition[capName] = divided
								}
							}
							if len(capPerPartition) > 0 {
								p.DeviceCapacity[driver] = capPerPartition
							}
							break // use first device's capacity as representative
						}
					}
				}
				p.DeviceCounts[driver] = divided
			}

			// Use a subset of devices as representative
			p.Devices = devices // all same-NUMA devices for attribute lookup

			partitions = append(partitions, p)
			partIdx++
		}
	}

	return partitions
}

// buildFullPartition creates a single partition containing all devices on the node.
func (b *PartitionBuilder) buildFullPartition(
	nodeName, profile string,
	devices []TopologyDevice,
	_ []TopologyRule,
) *PartitionDevice {
	if len(devices) == 0 {
		return nil
	}
	p := buildPartitionFromDevices(
		fmt.Sprintf("%s-full-0", nodeName),
		nodeName, profile, PartitionFull, devices,
	)
	return &p
}

// buildPartitionFromDevices constructs a PartitionDevice from a set of TopologyDevices.
func buildPartitionFromDevices(
	name, nodeName, profile string,
	partType PartitionType,
	devices []TopologyDevice,
) PartitionDevice {
	p := PartitionDevice{
		Name:               name,
		NodeName:           nodeName,
		Type:               partType,
		Profile:            profile,
		DeviceCounts:       make(map[string]int),
		Devices:            devices,
		ExtendedAttributes: make(map[string]DeviceAttributeValue),
	}

	numaSet := make(map[int64]bool)
	pcieSet := make(map[string]bool)
	socketSet := make(map[int64]bool)

	for _, d := range devices {
		baseName := baseDriverName(d.DriverName)
		p.DeviceCounts[baseName]++

		if d.NUMANode != nil {
			numaSet[*d.NUMANode] = true
		}
		if d.PCIeRoot != nil {
			pcieSet[*d.PCIeRoot] = true
		}
		if d.Socket != nil {
			socketSet[*d.Socket] = true
		}

		// Collect extended attributes (use first device's values as representative)
		for k, v := range d.ExtendedAttributes {
			if _, exists := p.ExtendedAttributes[k]; !exists {
				p.ExtendedAttributes[k] = v
			}
		}
	}

	for n := range numaSet {
		p.NUMANodes = append(p.NUMANodes, n)
	}
	sort.Slice(p.NUMANodes, func(i, j int) bool { return p.NUMANodes[i] < p.NUMANodes[j] })

	for r := range pcieSet {
		p.PCIeRoots = append(p.PCIeRoots, r)
	}
	sort.Strings(p.PCIeRoots)

	for s := range socketSet {
		p.Sockets = append(p.Sockets, s)
	}
	sort.Slice(p.Sockets, func(i, j int) bool { return p.Sockets[i] < p.Sockets[j] })

	return p
}

// inferProfile attempts to identify the hardware profile from device counts and driver names.
func (b *PartitionBuilder) inferProfile(driverDeviceCounts map[string]int) string {
	// Build a simple profile string from driver names and counts
	var parts []string
	for driver, count := range driverDeviceCounts {
		parts = append(parts, fmt.Sprintf("%s-%d", driver, count))
	}
	sort.Strings(parts)
	if len(parts) == 0 {
		return "unknown"
	}
	return strings.Join(parts, "_")
}

// validateGroupingAlignment checks if all devices that share a standard topology group
// also share the extended attribute value.
func (b *PartitionBuilder) validateGroupingAlignment(devices []TopologyDevice, rule TopologyRule) bool {
	// Group devices by their NUMA node and check that within each NUMA group,
	// all devices from the rule's driver share the same extended attribute value.
	byNUMA := make(map[int64][]TopologyDevice)
	for _, d := range devices {
		if d.NUMANode != nil && baseDriverName(d.DriverName) == rule.Driver {
			byNUMA[*d.NUMANode] = append(byNUMA[*d.NUMANode], d)
		}
	}

	for _, group := range byNUMA {
		if !devicesShareAttribute(group, rule.Attribute) {
			return false
		}
	}
	return true
}

// groupDevicesByAttribute groups devices by a string key derived from each device.
func groupDevicesByAttribute(devices []TopologyDevice, keyFn func(TopologyDevice) string) map[string][]TopologyDevice {
	groups := make(map[string][]TopologyDevice)
	for _, d := range devices {
		key := keyFn(d)
		groups[key] = append(groups[key], d)
	}
	return groups
}

// groupDevicesByExtendedAttribute groups devices by an extended attribute value.
func groupDevicesByExtendedAttribute(devices []TopologyDevice, attribute string) map[string][]TopologyDevice {
	groups := make(map[string][]TopologyDevice)
	for _, d := range devices {
		val, ok := d.ExtendedAttributes[attribute]
		if !ok {
			groups["none"] = append(groups["none"], d)
			continue
		}
		groups[val.String()] = append(groups[val.String()], d)
	}
	return groups
}

// devicesShareAttribute checks if all devices share the same value for an extended attribute.
func devicesShareAttribute(devices []TopologyDevice, attribute string) bool {
	if len(devices) == 0 {
		return true
	}

	var firstVal *string
	for _, d := range devices {
		val, ok := d.ExtendedAttributes[attribute]
		if !ok {
			continue
		}
		s := val.String()
		if firstVal == nil {
			firstVal = &s
		} else if s != *firstVal {
			return false
		}
	}
	return true
}

// baseDriverName extracts the base driver name from a driver/pool key.
func baseDriverName(driverName string) string {
	if idx := strings.Index(driverName, "/"); idx >= 0 {
		return driverName[:idx]
	}
	return driverName
}

func countNonEmptyGroups(groups map[string][]TopologyDevice) int {
	count := 0
	for key := range groups {
		if key != "" {
			count++
		}
	}
	return count
}

// divideQuantity divides a Kubernetes quantity string by a divisor.
// Returns the divided quantity as a string, or empty string on failure.
func divideQuantity(qty string, divisor int) string {
	q, err := resource.ParseQuantity(qty)
	if err != nil {
		klog.V(4).Infof("Failed to parse quantity %q: %v", qty, err)
		return ""
	}
	val := q.Value()
	divided := val / int64(divisor)
	if divided <= 0 {
		return ""
	}
	result := resource.NewQuantity(divided, q.Format)
	return result.String()
}
