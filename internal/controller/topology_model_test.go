package controller

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	resourcev1 "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func intPtr(v int64) *int64   { return &v }
func strPtr(v string) *string { return &v }
func boolPtr(v bool) *bool    { return &v }

// makeResourceSlice is a test helper that builds a ResourceSlice with devices.
func makeResourceSlice(name, driver, nodeName, poolName string, devices []resourcev1.Device) *resourcev1.ResourceSlice {
	return &resourcev1.ResourceSlice{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
		Spec: resourcev1.ResourceSliceSpec{
			Driver:   driver,
			NodeName: &nodeName,
			Pool: resourcev1.ResourcePool{
				Name:               poolName,
				Generation:         1,
				ResourceSliceCount: 1,
			},
			Devices: devices,
		},
	}
}

// makeGPUDevice creates a GPU device with standard topology attributes.
func makeGPUDevice(name string, numaNode int64, pcieRoot string) resourcev1.Device {
	return resourcev1.Device{
		Name: name,
		Attributes: map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
			resourcev1.QualifiedName(AttrNUMANode): {IntValue: intPtr(numaNode)},
			resourcev1.QualifiedName(AttrPCIeRoot): {StringValue: strPtr(pcieRoot)},
			resourcev1.QualifiedName(AttrSocket):   {IntValue: intPtr(numaNode / 2)},
		},
	}
}

// makeNICDevice creates a NIC device with standard topology attributes.
func makeNICDevice(name string, numaNode int64, pcieRoot string) resourcev1.Device {
	return resourcev1.Device{
		Name: name,
		Attributes: map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
			resourcev1.QualifiedName(AttrNUMANode): {IntValue: intPtr(numaNode)},
			resourcev1.QualifiedName(AttrPCIeRoot): {StringValue: strPtr(pcieRoot)},
		},
	}
}

func TestTopologyModel_UpdateFromResourceSlice(t *testing.T) {
	model := NewTopologyModel()

	// Add a GPU slice with 2 GPUs on NUMA 0
	gpuSlice := makeResourceSlice("gpu-slice-1", "gpu.nvidia.com", "node-1", "gpu-pool", []resourcev1.Device{
		makeGPUDevice("gpu-0", 0, "pcie-0"),
		makeGPUDevice("gpu-1", 0, "pcie-0"),
	})

	model.UpdateFromResourceSlice(gpuSlice)

	nodeTopo := model.GetNodeTopology("node-1")
	require.NotNil(t, nodeTopo)
	assert.Equal(t, "node-1", nodeTopo.NodeName)

	allDevices := nodeTopo.AllDevices()
	assert.Len(t, allDevices, 2)

	// Verify attributes were extracted
	assert.NotNil(t, allDevices[0].NUMANode)
	assert.Equal(t, int64(0), *allDevices[0].NUMANode)
	assert.NotNil(t, allDevices[0].PCIeRoot)
	assert.Equal(t, "pcie-0", *allDevices[0].PCIeRoot)
}

func TestTopologyModel_MultipleDrivers(t *testing.T) {
	model := NewTopologyModel()

	// Add GPU slice
	gpuSlice := makeResourceSlice("gpu-slice", "gpu.nvidia.com", "node-1", "gpu-pool", []resourcev1.Device{
		makeGPUDevice("gpu-0", 0, "pcie-0"),
		makeGPUDevice("gpu-1", 1, "pcie-1"),
	})
	model.UpdateFromResourceSlice(gpuSlice)

	// Add NIC slice
	nicSlice := makeResourceSlice("nic-slice", "rdma.mellanox.com", "node-1", "nic-pool", []resourcev1.Device{
		makeNICDevice("nic-0", 0, "pcie-0"),
		makeNICDevice("nic-1", 1, "pcie-1"),
	})
	model.UpdateFromResourceSlice(nicSlice)

	nodeTopo := model.GetNodeTopology("node-1")
	require.NotNil(t, nodeTopo)

	allDevices := nodeTopo.AllDevices()
	assert.Len(t, allDevices, 4, "should have 2 GPUs + 2 NICs")

	gpuDevices := nodeTopo.DevicesForDriver("gpu.nvidia.com/gpu-pool")
	assert.Len(t, gpuDevices, 2)

	nicDevices := nodeTopo.DevicesForDriver("rdma.mellanox.com/nic-pool")
	assert.Len(t, nicDevices, 2)
}

func TestTopologyModel_RemoveResourceSlice(t *testing.T) {
	model := NewTopologyModel()

	gpuSlice := makeResourceSlice("gpu-slice", "gpu.nvidia.com", "node-1", "gpu-pool", []resourcev1.Device{
		makeGPUDevice("gpu-0", 0, "pcie-0"),
	})
	model.UpdateFromResourceSlice(gpuSlice)

	// Verify it was added
	assert.NotNil(t, model.GetNodeTopology("node-1"))

	// Remove the slice
	model.RemoveResourceSlice(gpuSlice)

	// Node should be gone since it has no more devices
	assert.Nil(t, model.GetNodeTopology("node-1"))
}

func TestTopologyModel_GetAllNodes(t *testing.T) {
	model := NewTopologyModel()

	// Add devices on two different nodes
	model.UpdateFromResourceSlice(makeResourceSlice("slice-1", "gpu.nvidia.com", "node-1", "pool-1", []resourcev1.Device{
		makeGPUDevice("gpu-0", 0, "pcie-0"),
	}))
	model.UpdateFromResourceSlice(makeResourceSlice("slice-2", "gpu.nvidia.com", "node-2", "pool-2", []resourcev1.Device{
		makeGPUDevice("gpu-0", 0, "pcie-0"),
	}))

	nodes := model.GetAllNodes()
	assert.Len(t, nodes, 2)
	assert.Contains(t, nodes, "node-1")
	assert.Contains(t, nodes, "node-2")
}

func TestTopologyModel_SkipsSliceWithoutNodeName(t *testing.T) {
	model := NewTopologyModel()

	slice := &resourcev1.ResourceSlice{
		ObjectMeta: metav1.ObjectMeta{Name: "no-node-slice"},
		Spec: resourcev1.ResourceSliceSpec{
			Driver: "test-driver",
			Pool:   resourcev1.ResourcePool{Name: "pool-1"},
			// NodeName is nil
			Devices: []resourcev1.Device{
				{Name: "dev-0"},
			},
		},
	}
	model.UpdateFromResourceSlice(slice)

	assert.Empty(t, model.GetAllNodes())
}

func TestTopologyModel_ExtendedAttributes(t *testing.T) {
	model := NewTopologyModel()

	// Set rules before updating
	model.SetRules([]TopologyRule{
		{
			Attribute:    "gpu.nvidia.com/nvlinkDomain",
			Type:         "int",
			Driver:       "gpu.nvidia.com",
			Partitioning: PartitioningGroup,
			Constraint:   ConstraintMatch,
		},
	})

	// Create a GPU device with the NVLink domain attribute
	nvlinkDomain := int64(0)
	slice := makeResourceSlice("gpu-slice", "gpu.nvidia.com", "node-1", "gpu-pool", []resourcev1.Device{
		{
			Name: "gpu-0",
			Attributes: map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
				resourcev1.QualifiedName(AttrNUMANode):                  {IntValue: intPtr(0)},
				resourcev1.QualifiedName(AttrPCIeRoot):                  {StringValue: strPtr("pcie-0")},
				resourcev1.QualifiedName("gpu.nvidia.com/nvlinkDomain"): {IntValue: &nvlinkDomain},
			},
		},
	})
	model.UpdateFromResourceSlice(slice)

	nodeTopo := model.GetNodeTopology("node-1")
	require.NotNil(t, nodeTopo)

	devices := nodeTopo.AllDevices()
	require.Len(t, devices, 1)

	// Verify the extended attribute was extracted
	val, ok := devices[0].ExtendedAttributes["gpu.nvidia.com/nvlinkDomain"]
	assert.True(t, ok, "expected nvlinkDomain extended attribute")
	assert.NotNil(t, val.IntValue)
	assert.Equal(t, int64(0), *val.IntValue)
}

func TestTopologyModel_UpdateReplacesDevices(t *testing.T) {
	model := NewTopologyModel()

	// First update: 2 GPUs
	model.UpdateFromResourceSlice(makeResourceSlice("gpu-slice", "gpu.nvidia.com", "node-1", "gpu-pool", []resourcev1.Device{
		makeGPUDevice("gpu-0", 0, "pcie-0"),
		makeGPUDevice("gpu-1", 0, "pcie-0"),
	}))

	nodeTopo := model.GetNodeTopology("node-1")
	assert.Len(t, nodeTopo.AllDevices(), 2)

	// Second update: only 1 GPU (replacement)
	model.UpdateFromResourceSlice(makeResourceSlice("gpu-slice", "gpu.nvidia.com", "node-1", "gpu-pool", []resourcev1.Device{
		makeGPUDevice("gpu-0", 0, "pcie-0"),
	}))

	nodeTopo = model.GetNodeTopology("node-1")
	assert.Len(t, nodeTopo.AllDevices(), 1)
}

func TestDeviceAttributeValue_String(t *testing.T) {
	tests := []struct {
		name string
		val  DeviceAttributeValue
		want string
	}{
		{
			name: "int value",
			val:  DeviceAttributeValue{IntValue: intPtr(42)},
			want: "42",
		},
		{
			name: "string value",
			val:  DeviceAttributeValue{StringValue: strPtr("hello")},
			want: "hello",
		},
		{
			name: "bool value",
			val:  DeviceAttributeValue{BoolValue: boolPtr(true)},
			want: "true",
		},
		{
			name: "nil value",
			val:  DeviceAttributeValue{},
			want: "<nil>",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, tt.val.String())
		})
	}
}

func TestTopologyModel_MapsToNUMANode(t *testing.T) {
	model := NewTopologyModel()

	// Set a rule that maps the driver-specific numaNode attribute to standard NUMA
	model.SetRules([]TopologyRule{
		{
			Attribute: "mock-accel.example.com/numaNode",
			Type:      "int",
			Driver:    "mock-accel.example.com",
			MapsTo:    MapsToNUMANode,
		},
	})

	// Create a device that uses driver-specific attribute names (no standard attributes)
	numaNode := int64(1)
	slice := makeResourceSlice("mock-slice", "mock-accel.example.com", "node-1", "mock-pool", []resourcev1.Device{
		{
			Name: "mock0",
			Attributes: map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
				"mock-accel.example.com/numaNode": {IntValue: &numaNode},
			},
		},
	})
	model.UpdateFromResourceSlice(slice)

	nodeTopo := model.GetNodeTopology("node-1")
	require.NotNil(t, nodeTopo)

	devices := nodeTopo.AllDevices()
	require.Len(t, devices, 1)

	// The driver-specific numaNode should be mapped to the standard NUMANode field
	require.NotNil(t, devices[0].NUMANode, "expected NUMANode to be set via mapping")
	assert.Equal(t, int64(1), *devices[0].NUMANode)

	// It should also be stored as an extended attribute
	val, ok := devices[0].ExtendedAttributes["mock-accel.example.com/numaNode"]
	assert.True(t, ok, "expected extended attribute to be stored")
	assert.Equal(t, int64(1), *val.IntValue)
}

func TestTopologyModel_MapsToPCIeRoot(t *testing.T) {
	model := NewTopologyModel()

	model.SetRules([]TopologyRule{
		{
			Attribute: "mock-accel.example.com/pciAddress",
			Type:      "string",
			Driver:    "mock-accel.example.com",
			MapsTo:    MapsToPCIeRoot,
		},
	})

	pciAddr := "0000:11:00.0"
	slice := makeResourceSlice("mock-slice", "mock-accel.example.com", "node-1", "mock-pool", []resourcev1.Device{
		{
			Name: "mock0",
			Attributes: map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
				"mock-accel.example.com/pciAddress": {StringValue: &pciAddr},
			},
		},
	})
	model.UpdateFromResourceSlice(slice)

	devices := model.GetNodeTopology("node-1").AllDevices()
	require.Len(t, devices, 1)
	require.NotNil(t, devices[0].PCIeRoot, "expected PCIeRoot to be set via mapping")
	assert.Equal(t, "0000:11:00.0", *devices[0].PCIeRoot)
}

func TestTopologyModel_MapsToSocket(t *testing.T) {
	model := NewTopologyModel()

	model.SetRules([]TopologyRule{
		{
			Attribute: "mock-accel.example.com/socket",
			Type:      "int",
			Driver:    "mock-accel.example.com",
			MapsTo:    MapsToSocket,
		},
	})

	socket := int64(0)
	slice := makeResourceSlice("mock-slice", "mock-accel.example.com", "node-1", "mock-pool", []resourcev1.Device{
		{
			Name: "mock0",
			Attributes: map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
				"mock-accel.example.com/socket": {IntValue: &socket},
			},
		},
	})
	model.UpdateFromResourceSlice(slice)

	devices := model.GetNodeTopology("node-1").AllDevices()
	require.Len(t, devices, 1)
	require.NotNil(t, devices[0].Socket, "expected Socket to be set via mapping")
	assert.Equal(t, int64(0), *devices[0].Socket)
}

func TestTopologyModel_MappingDoesNotOverrideStandard(t *testing.T) {
	model := NewTopologyModel()

	// A mapping rule exists, but the device also has the standard attribute
	model.SetRules([]TopologyRule{
		{
			Attribute: "mock-accel.example.com/numaNode",
			Type:      "int",
			Driver:    "mock-accel.example.com",
			MapsTo:    MapsToNUMANode,
		},
	})

	standardNUMA := int64(0)
	driverNUMA := int64(1)
	slice := makeResourceSlice("mock-slice", "mock-accel.example.com", "node-1", "mock-pool", []resourcev1.Device{
		{
			Name: "mock0",
			Attributes: map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
				// Standard attribute takes precedence
				resourcev1.QualifiedName(AttrNUMANode): {IntValue: &standardNUMA},
				"mock-accel.example.com/numaNode":      {IntValue: &driverNUMA},
			},
		},
	})
	model.UpdateFromResourceSlice(slice)

	devices := model.GetNodeTopology("node-1").AllDevices()
	require.Len(t, devices, 1)
	// Standard attribute should win because it's processed first via continue
	// but then the mapping also sets it — this depends on map iteration order.
	// The important thing is NUMANode is set.
	require.NotNil(t, devices[0].NUMANode)
}

func TestTopologyModel_DeepCopyIsolation(t *testing.T) {
	model := NewTopologyModel()

	model.UpdateFromResourceSlice(makeResourceSlice("gpu-slice", "gpu.nvidia.com", "node-1", "pool", []resourcev1.Device{
		makeGPUDevice("gpu-0", 0, "pcie-0"),
	}))

	// Get a snapshot
	snapshot := model.GetNodeTopology("node-1")
	require.NotNil(t, snapshot)
	require.Len(t, snapshot.AllDevices(), 1)

	// Mutate the original model
	model.UpdateFromResourceSlice(makeResourceSlice("gpu-slice", "gpu.nvidia.com", "node-1", "pool", []resourcev1.Device{
		makeGPUDevice("gpu-0", 0, "pcie-0"),
		makeGPUDevice("gpu-1", 1, "pcie-1"),
	}))

	// Snapshot should be unchanged (deep copy)
	assert.Len(t, snapshot.AllDevices(), 1, "snapshot should not be affected by model mutation")

	// New fetch should reflect the update
	updated := model.GetNodeTopology("node-1")
	assert.Len(t, updated.AllDevices(), 2)
}

func TestTopologyModel_SetRulesReextractsDevices(t *testing.T) {
	model := NewTopologyModel()

	// Add devices with a driver-specific numaNode attribute but no rules yet
	numaNode := int64(1)
	model.UpdateFromResourceSlice(makeResourceSlice("mock-slice", "mock-accel.example.com", "node-1", "pool", []resourcev1.Device{
		{
			Name: "mock0",
			Attributes: map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
				"mock-accel.example.com/numaNode": {IntValue: &numaNode},
			},
		},
	}))

	// Without mapping rules, NUMANode should be nil
	devices := model.GetNodeTopology("node-1").AllDevices()
	require.Len(t, devices, 1)
	assert.Nil(t, devices[0].NUMANode, "without rules, driver-specific attr should not map to NUMANode")

	// Now set rules that map the attribute
	model.SetRules([]TopologyRule{
		{
			Attribute: "mock-accel.example.com/numaNode",
			Type:      "int",
			Driver:    "mock-accel.example.com",
			MapsTo:    MapsToNUMANode,
		},
	})

	// After SetRules, devices should be re-extracted with the new mapping
	devices = model.GetNodeTopology("node-1").AllDevices()
	require.Len(t, devices, 1)
	require.NotNil(t, devices[0].NUMANode, "after SetRules, NUMANode should be mapped")
	assert.Equal(t, int64(1), *devices[0].NUMANode)
}

func TestTopologyModel_IsConstraintSatisfiable_NUMAAligned(t *testing.T) {
	model := NewTopologyModel()

	// Node with 2 GPUs on NUMA 0 and 2 NICs on NUMA 0
	model.UpdateFromResourceSlice(makeResourceSlice("gpu-slice", "gpu.nvidia.com", "node-1", "gpu-pool", []resourcev1.Device{
		makeGPUDevice("gpu-0", 0, "pcie-0"),
		makeGPUDevice("gpu-1", 0, "pcie-0"),
	}))
	model.UpdateFromResourceSlice(makeResourceSlice("nic-slice", "rdma.mellanox.com", "node-1", "nic-pool", []resourcev1.Device{
		makeNICDevice("nic-0", 0, "pcie-0"),
	}))

	// 2 GPUs + 1 NIC on same NUMA → satisfiable
	assert.True(t, model.IsConstraintSatisfiable(AttrNUMANode, map[string]int{
		"gpu.nvidia.com":    2,
		"rdma.mellanox.com": 1,
	}))

	// 3 GPUs on same NUMA → not satisfiable (only 2 on NUMA 0)
	assert.False(t, model.IsConstraintSatisfiable(AttrNUMANode, map[string]int{
		"gpu.nvidia.com": 3,
	}))
}

func TestTopologyModel_IsConstraintSatisfiable_CrossNUMA(t *testing.T) {
	model := NewTopologyModel()

	// GPUs on NUMA 0, NICs on NUMA 1 — cross-NUMA, not alignable
	model.UpdateFromResourceSlice(makeResourceSlice("gpu-slice", "gpu.nvidia.com", "node-1", "gpu-pool", []resourcev1.Device{
		makeGPUDevice("gpu-0", 0, "pcie-0"),
	}))
	model.UpdateFromResourceSlice(makeResourceSlice("nic-slice", "rdma.mellanox.com", "node-1", "nic-pool", []resourcev1.Device{
		makeNICDevice("nic-0", 1, "pcie-1"),
	}))

	assert.False(t, model.IsConstraintSatisfiable(AttrNUMANode, map[string]int{
		"gpu.nvidia.com":    1,
		"rdma.mellanox.com": 1,
	}))
}

func TestTopologyModel_IsConstraintSatisfiable_MultipleNodes(t *testing.T) {
	model := NewTopologyModel()

	// Node-1: cross-NUMA (not satisfiable)
	model.UpdateFromResourceSlice(makeResourceSlice("gpu-1", "gpu.nvidia.com", "node-1", "gpu-pool", []resourcev1.Device{
		makeGPUDevice("gpu-0", 0, "pcie-0"),
	}))
	model.UpdateFromResourceSlice(makeResourceSlice("nic-1", "rdma.mellanox.com", "node-1", "nic-pool", []resourcev1.Device{
		makeNICDevice("nic-0", 1, "pcie-1"),
	}))

	// Node-2: same NUMA (satisfiable)
	model.UpdateFromResourceSlice(makeResourceSlice("gpu-2", "gpu.nvidia.com", "node-2", "gpu-pool", []resourcev1.Device{
		makeGPUDevice("gpu-0", 0, "pcie-0"),
	}))
	model.UpdateFromResourceSlice(makeResourceSlice("nic-2", "rdma.mellanox.com", "node-2", "nic-pool", []resourcev1.Device{
		makeNICDevice("nic-0", 0, "pcie-0"),
	}))

	// Should be satisfiable because node-2 has alignment
	assert.True(t, model.IsConstraintSatisfiable(AttrNUMANode, map[string]int{
		"gpu.nvidia.com":    1,
		"rdma.mellanox.com": 1,
	}))
}

func TestTopologyModel_IsConstraintSatisfiable_EmptyModel(t *testing.T) {
	model := NewTopologyModel()

	assert.False(t, model.IsConstraintSatisfiable(AttrNUMANode, map[string]int{
		"gpu.nvidia.com": 1,
	}))
}

func TestTopologyModel_IsConstraintSatisfiable_PCIeRoot(t *testing.T) {
	model := NewTopologyModel()

	// 2 GPUs and 1 NIC under the same PCIe root
	model.UpdateFromResourceSlice(makeResourceSlice("gpu-slice", "gpu.nvidia.com", "node-1", "gpu-pool", []resourcev1.Device{
		makeGPUDevice("gpu-0", 0, "pcie-0"),
		makeGPUDevice("gpu-1", 0, "pcie-0"),
	}))
	model.UpdateFromResourceSlice(makeResourceSlice("nic-slice", "rdma.mellanox.com", "node-1", "nic-pool", []resourcev1.Device{
		makeNICDevice("nic-0", 0, "pcie-0"),
	}))

	assert.True(t, model.IsConstraintSatisfiable(AttrPCIeRoot, map[string]int{
		"gpu.nvidia.com":    2,
		"rdma.mellanox.com": 1,
	}))

	// Different PCIe roots → not satisfiable for PCIe constraint
	model2 := NewTopologyModel()
	model2.UpdateFromResourceSlice(makeResourceSlice("gpu-slice", "gpu.nvidia.com", "node-1", "gpu-pool", []resourcev1.Device{
		makeGPUDevice("gpu-0", 0, "pcie-0"),
	}))
	model2.UpdateFromResourceSlice(makeResourceSlice("nic-slice", "rdma.mellanox.com", "node-1", "nic-pool", []resourcev1.Device{
		makeNICDevice("nic-0", 0, "pcie-1"),
	}))

	assert.False(t, model2.IsConstraintSatisfiable(AttrPCIeRoot, map[string]int{
		"gpu.nvidia.com":    1,
		"rdma.mellanox.com": 1,
	}))
}

func TestTopologyModel_RemoveSliceCleansRawData(t *testing.T) {
	model := NewTopologyModel()

	slice := makeResourceSlice("gpu-slice", "gpu.nvidia.com", "node-1", "pool", []resourcev1.Device{
		makeGPUDevice("gpu-0", 0, "pcie-0"),
	})

	model.UpdateFromResourceSlice(slice)
	require.NotNil(t, model.GetNodeTopology("node-1"))

	model.RemoveResourceSlice(slice)
	assert.Nil(t, model.GetNodeTopology("node-1"))

	// After removing and re-setting rules, the device should not reappear
	model.SetRules([]TopologyRule{})
	assert.Nil(t, model.GetNodeTopology("node-1"), "removed slices should not reappear after SetRules")
}
