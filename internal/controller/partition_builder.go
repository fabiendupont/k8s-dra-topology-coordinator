package controller

import (
	"fmt"
	"sort"
	"strings"

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

	// Group devices by PCIe root for the finest-grained partitioning
	byPCIeRoot := groupDevicesByAttribute(allDevices, func(d TopologyDevice) string {
		if d.PCIeRoot != nil {
			return *d.PCIeRoot
		}
		return ""
	})

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

	// Build partitions at each granularity level
	profile := b.inferProfile(driverDeviceCounts)
	result.Profile = profile

	// Eighth: one partition per PCIe root (finest grain)
	eighths := b.buildPartitionsFromGroups(nodeName, profile, PartitionEighth, byPCIeRoot, groupingRules)
	result.Partitions = append(result.Partitions, eighths...)

	// Quarter: one partition per NUMA node
	quarters := b.buildPartitionsFromGroups(nodeName, profile, PartitionQuarter, byNUMA, groupingRules)
	result.Partitions = append(result.Partitions, quarters...)

	// Half: one partition per socket
	halves := b.buildPartitionsFromGroups(nodeName, profile, PartitionHalf, bySocket, groupingRules)
	result.Partitions = append(result.Partitions, halves...)

	// Full: all devices on the node
	full := b.buildFullPartition(nodeName, profile, allDevices, groupingRules)
	if full != nil {
		result.Partitions = append(result.Partitions, *full)
	}

	klog.Infof("Node %s (profile=%s): computed %d partitions (%d eighth, %d quarter, %d half, %d full)",
		nodeName, profile,
		len(result.Partitions),
		len(eighths), len(quarters), len(halves),
		boolToInt(full != nil))

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

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
