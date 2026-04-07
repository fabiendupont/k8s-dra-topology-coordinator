// Package controller implements the topology coordinator controller that watches
// ResourceSlices from all DRA drivers and computes aligned partition combinations.
package controller

import (
	"fmt"
	"sync"

	resourcev1 "k8s.io/api/resource/v1"
	klog "k8s.io/klog/v2"
)

// Standard topology attribute qualified names that all participating DRA drivers must publish.
const (
	AttrNUMANode = "nodepartition.dra.k8s.io/numaNode"
	AttrPCIeRoot = "resource.kubernetes.io/pcieRoot"
	AttrSocket   = "nodepartition.dra.k8s.io/socket"
)

// TopologyDevice represents a single device from a DRA driver's ResourceSlice,
// enriched with its topology attributes.
type TopologyDevice struct {
	// DriverName is the DRA driver that published this device.
	DriverName string
	// DeviceName is the device name within its ResourceSlice.
	DeviceName string
	// NodeName is the Kubernetes node where this device exists.
	NodeName string
	// PoolName is the ResourceSlice pool name.
	PoolName string

	// Standard topology attributes
	NUMANode *int64
	PCIeRoot *string
	Socket   *int64

	// Extended attributes from topology rules (attribute qualified name -> value).
	ExtendedAttributes map[string]DeviceAttributeValue
}

// DeviceAttributeValue holds a typed attribute value.
type DeviceAttributeValue struct {
	IntValue    *int64
	StringValue *string
	BoolValue   *bool
}

// String returns a string representation for logging.
func (v DeviceAttributeValue) String() string {
	if v.IntValue != nil {
		return fmt.Sprintf("%d", *v.IntValue)
	}
	if v.StringValue != nil {
		return *v.StringValue
	}
	if v.BoolValue != nil {
		return fmt.Sprintf("%t", *v.BoolValue)
	}
	return "<nil>"
}

// NodeTopology holds all topology devices discovered on a single node across all drivers.
type NodeTopology struct {
	NodeName string
	// Devices grouped by driver name.
	DevicesByDriver map[string][]TopologyDevice
}

// AllDevices returns a flat list of all devices on this node.
func (nt *NodeTopology) AllDevices() []TopologyDevice {
	var all []TopologyDevice
	for _, devices := range nt.DevicesByDriver {
		all = append(all, devices...)
	}
	return all
}

// DevicesForDriver returns devices published by a specific driver on this node.
func (nt *NodeTopology) DevicesForDriver(driverName string) []TopologyDevice {
	return nt.DevicesByDriver[driverName]
}

// rawSliceData holds the raw information from a ResourceSlice needed
// for re-extraction when topology rules change.
type rawSliceData struct {
	DriverName string
	NodeName   string
	PoolName   string
	Devices    []resourcev1.Device
}

// TopologyModel is the cross-driver topology model built from ResourceSlices.
// It is thread-safe for concurrent read/write access.
type TopologyModel struct {
	mu sync.RWMutex
	// nodes maps node name -> NodeTopology.
	nodes map[string]*NodeTopology
	// rules are the active topology rules from ConfigMaps.
	rules []TopologyRule
	// rawSlices stores raw slice data for re-extraction when rules change.
	// Keyed by "nodeName/driverName/poolName".
	rawSlices map[string]rawSliceData
}

// NewTopologyModel creates an empty topology model.
func NewTopologyModel() *TopologyModel {
	return &TopologyModel{
		nodes:     make(map[string]*NodeTopology),
		rawSlices: make(map[string]rawSliceData),
	}
}

// SetRules updates the topology rules and re-extracts all devices
// with the new rules to ensure attribute mappings are up to date.
func (m *TopologyModel) SetRules(rules []TopologyRule) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rules = rules
	m.reextractAllLocked()
}

// reextractAllLocked re-extracts all devices from stored raw slice data
// using the current rules. Must be called with m.mu held.
func (m *TopologyModel) reextractAllLocked() {
	// Clear existing topology
	m.nodes = make(map[string]*NodeTopology)

	// Re-extract all devices with current rules
	for _, raw := range m.rawSlices {
		m.applySliceDataLocked(raw)
	}
}

// GetRules returns the current topology rules.
func (m *TopologyModel) GetRules() []TopologyRule {
	m.mu.RLock()
	defer m.mu.RUnlock()
	result := make([]TopologyRule, len(m.rules))
	copy(result, m.rules)
	return result
}

// UpdateFromResourceSlice processes a ResourceSlice and updates the topology model.
// It extracts devices with their topology attributes and stores them by node.
func (m *TopologyModel) UpdateFromResourceSlice(slice *resourcev1.ResourceSlice) {
	if slice == nil {
		return
	}

	driverName := slice.Spec.Driver
	nodeName := ""
	if slice.Spec.NodeName != nil {
		nodeName = *slice.Spec.NodeName
	}
	if nodeName == "" {
		klog.V(4).Infof("Skipping ResourceSlice %s/%s: no node name", slice.Namespace, slice.Name)
		return
	}

	poolName := slice.Spec.Pool.Name

	m.mu.Lock()
	defer m.mu.Unlock()

	// Store a deep copy of raw data for re-extraction when rules change.
	// The informer cache may reuse the slice object, so we must not hold references.
	rawKey := nodeName + "/" + driverName + "/" + poolName
	devicesCopy := make([]resourcev1.Device, len(slice.Spec.Devices))
	for i, d := range slice.Spec.Devices {
		devicesCopy[i] = *d.DeepCopy()
	}
	m.rawSlices[rawKey] = rawSliceData{
		DriverName: driverName,
		NodeName:   nodeName,
		PoolName:   poolName,
		Devices:    devicesCopy,
	}

	// Extract and apply
	m.applySliceDataLocked(m.rawSlices[rawKey])

	klog.V(4).Infof("Updated topology model: node=%s driver=%s pool=%s devices=%d",
		nodeName, driverName, poolName, len(slice.Spec.Devices))
}

// applySliceDataLocked extracts topology devices from raw slice data
// and updates the node topology. Must be called with m.mu held.
func (m *TopologyModel) applySliceDataLocked(raw rawSliceData) {
	var devices []TopologyDevice
	for _, device := range raw.Devices {
		td := m.extractTopologyDevice(raw.DriverName, device.Name, raw.NodeName, raw.PoolName, device.Attributes)
		devices = append(devices, td)
	}

	nt, ok := m.nodes[raw.NodeName]
	if !ok {
		nt = &NodeTopology{
			NodeName:        raw.NodeName,
			DevicesByDriver: make(map[string][]TopologyDevice),
		}
		m.nodes[raw.NodeName] = nt
	}

	sliceKey := raw.DriverName + "/" + raw.PoolName
	nt.DevicesByDriver[sliceKey] = devices
}

// RemoveResourceSlice removes devices from a ResourceSlice that was deleted.
func (m *TopologyModel) RemoveResourceSlice(slice *resourcev1.ResourceSlice) {
	if slice == nil {
		return
	}

	driverName := slice.Spec.Driver
	nodeName := ""
	if slice.Spec.NodeName != nil {
		nodeName = *slice.Spec.NodeName
	}
	if nodeName == "" {
		return
	}

	poolName := slice.Spec.Pool.Name
	sliceKey := driverName + "/" + poolName

	m.mu.Lock()
	defer m.mu.Unlock()

	// Remove raw data
	rawKey := nodeName + "/" + driverName + "/" + poolName
	delete(m.rawSlices, rawKey)

	nt, ok := m.nodes[nodeName]
	if !ok {
		return
	}

	delete(nt.DevicesByDriver, sliceKey)

	// Clean up empty nodes
	if len(nt.DevicesByDriver) == 0 {
		delete(m.nodes, nodeName)
	}

	klog.V(4).Infof("Removed from topology model: node=%s driver=%s pool=%s", nodeName, driverName, poolName)
}

// GetNodeTopology returns a deep copy of the topology for a specific node.
func (m *TopologyModel) GetNodeTopology(nodeName string) *NodeTopology {
	m.mu.RLock()
	defer m.mu.RUnlock()
	nt, ok := m.nodes[nodeName]
	if !ok {
		return nil
	}
	return nt.deepCopy()
}

// GetAllNodes returns the names of all nodes with topology information.
func (m *TopologyModel) GetAllNodes() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	nodes := make([]string, 0, len(m.nodes))
	for name := range m.nodes {
		nodes = append(nodes, name)
	}
	return nodes
}

// GetNodeTopologies returns a deep copy of all node topologies.
func (m *TopologyModel) GetNodeTopologies() map[string]*NodeTopology {
	m.mu.RLock()
	defer m.mu.RUnlock()
	result := make(map[string]*NodeTopology, len(m.nodes))
	for k, v := range m.nodes {
		result[k] = v.deepCopy()
	}
	return result
}

// deepCopy returns a deep copy of the NodeTopology.
func (nt *NodeTopology) deepCopy() *NodeTopology {
	cp := &NodeTopology{
		NodeName:        nt.NodeName,
		DevicesByDriver: make(map[string][]TopologyDevice, len(nt.DevicesByDriver)),
	}
	for driver, devices := range nt.DevicesByDriver {
		devicesCopy := make([]TopologyDevice, len(devices))
		for i, d := range devices {
			devicesCopy[i] = d.deepCopy()
		}
		cp.DevicesByDriver[driver] = devicesCopy
	}
	return cp
}

// deepCopy returns a deep copy of the TopologyDevice.
func (td TopologyDevice) deepCopy() TopologyDevice {
	cp := td
	if td.NUMANode != nil {
		v := *td.NUMANode
		cp.NUMANode = &v
	}
	if td.PCIeRoot != nil {
		v := *td.PCIeRoot
		cp.PCIeRoot = &v
	}
	if td.Socket != nil {
		v := *td.Socket
		cp.Socket = &v
	}
	cp.ExtendedAttributes = make(map[string]DeviceAttributeValue, len(td.ExtendedAttributes))
	for k, v := range td.ExtendedAttributes {
		cp.ExtendedAttributes[k] = v
	}
	return cp
}

// IsConstraintSatisfiable checks whether at least one node in the topology model
// has a group of devices sharing the same value for the given attribute where
// every driver in driverCounts has at least the required number of devices.
// This is used by the webhook to decide whether to emit "preferred" constraints.
func (m *TopologyModel) IsConstraintSatisfiable(attribute string, driverCounts map[string]int) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()

	for _, nt := range m.nodes {
		if m.isConstraintSatisfiableOnNode(nt, attribute, driverCounts) {
			return true
		}
	}
	return false
}

// isConstraintSatisfiableOnNode checks whether a single node has at least one
// group (same attribute value) that satisfies all driver count requirements.
func (m *TopologyModel) isConstraintSatisfiableOnNode(nt *NodeTopology, attribute string, driverCounts map[string]int) bool {
	// Collect all devices on this node, grouped by the attribute value.
	// Key: attribute value as string, Value: map[driverName]count
	groups := make(map[string]map[string]int)

	for _, devices := range nt.DevicesByDriver {
		for _, dev := range devices {
			val := deviceAttributeValueString(dev, attribute)
			if val == "" {
				continue
			}
			if groups[val] == nil {
				groups[val] = make(map[string]int)
			}
			groups[val][dev.DriverName]++
		}
	}

	// Check if any group satisfies all driver count requirements.
	for _, driverMap := range groups {
		satisfied := true
		for driver, needed := range driverCounts {
			if driverMap[driver] < needed {
				satisfied = false
				break
			}
		}
		if satisfied {
			return true
		}
	}
	return false
}

// deviceAttributeValueString returns the string representation of the device's
// value for the given attribute, checking standard attributes and extended attributes.
func deviceAttributeValueString(dev TopologyDevice, attribute string) string {
	switch attribute {
	case AttrNUMANode:
		if dev.NUMANode != nil {
			return fmt.Sprintf("%d", *dev.NUMANode)
		}
	case AttrPCIeRoot:
		if dev.PCIeRoot != nil {
			return *dev.PCIeRoot
		}
	case AttrSocket:
		if dev.Socket != nil {
			return fmt.Sprintf("%d", *dev.Socket)
		}
	default:
		if v, ok := dev.ExtendedAttributes[attribute]; ok {
			return v.String()
		}
	}
	return ""
}

// extractTopologyDevice extracts topology attributes from a device's attributes.
func (m *TopologyModel) extractTopologyDevice(
	driverName, deviceName, nodeName, poolName string,
	attrs map[resourcev1.QualifiedName]resourcev1.DeviceAttribute,
) TopologyDevice {
	td := TopologyDevice{
		DriverName:         driverName,
		DeviceName:         deviceName,
		NodeName:           nodeName,
		PoolName:           poolName,
		ExtendedAttributes: make(map[string]DeviceAttributeValue),
	}

	for qn, attr := range attrs {
		name := string(qn)

		// Check standard attribute names first
		switch name {
		case AttrNUMANode:
			if attr.IntValue != nil {
				td.NUMANode = attr.IntValue
			}
			continue
		case AttrPCIeRoot:
			if attr.StringValue != nil {
				td.PCIeRoot = attr.StringValue
			}
			continue
		case AttrSocket:
			if attr.IntValue != nil {
				td.Socket = attr.IntValue
			}
			continue
		}

		// Check topology rules for driver-specific attribute mappings
		for _, rule := range m.rules {
			if name != rule.Attribute || driverName != rule.Driver {
				continue
			}

			// If the rule maps to a standard topology attribute, apply the mapping
			switch rule.MapsTo {
			case MapsToNUMANode:
				if attr.IntValue != nil {
					td.NUMANode = attr.IntValue
				}
			case MapsToPCIeRoot:
				if attr.StringValue != nil {
					td.PCIeRoot = attr.StringValue
				}
			case MapsToSocket:
				if attr.IntValue != nil {
					td.Socket = attr.IntValue
				}
			}

			// Also store as extended attribute for propagation to partition devices
			td.ExtendedAttributes[name] = deviceAttributeFromDRA(attr)
		}
	}

	return td
}

// deviceAttributeFromDRA converts a DRA DeviceAttribute to our DeviceAttributeValue.
func deviceAttributeFromDRA(attr resourcev1.DeviceAttribute) DeviceAttributeValue {
	v := DeviceAttributeValue{}
	if attr.IntValue != nil {
		v.IntValue = attr.IntValue
	}
	if attr.StringValue != nil {
		v.StringValue = attr.StringValue
	}
	if attr.BoolValue != nil {
		v.BoolValue = attr.BoolValue
	}
	return v
}
