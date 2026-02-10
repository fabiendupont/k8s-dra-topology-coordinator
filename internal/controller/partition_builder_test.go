package controller

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	resourcev1 "k8s.io/api/resource/v1"
)

// buildHGXB200Topology populates a topology model with an HGX B200-like node:
// 8 GPUs, 8 NICs, 2 sockets, 4 NUMA nodes, 8 PCIe roots
func buildHGXB200Topology(model *TopologyModel) {
	// GPU slice: 8 GPUs across 4 NUMA nodes
	gpuDevices := make([]resourcev1.Device, 8)
	pcieRoots := []string{"pcie-0", "pcie-1", "pcie-2", "pcie-3", "pcie-4", "pcie-5", "pcie-6", "pcie-7"}
	for i := 0; i < 8; i++ {
		numaNode := int64(i / 2) // 2 GPUs per NUMA
		socket := numaNode / 2   // 2 NUMA per socket
		gpuDevices[i] = resourcev1.Device{
			Name: makeDeviceName("gpu", i),
			Attributes: map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
				resourcev1.QualifiedName(AttrNUMANode): {IntValue: intPtr(numaNode)},
				resourcev1.QualifiedName(AttrPCIeRoot): {StringValue: strPtr(pcieRoots[i])},
				resourcev1.QualifiedName(AttrSocket):   {IntValue: intPtr(socket)},
			},
		}
	}
	gpuSlice := makeResourceSlice("gpu-slice", "gpu.nvidia.com", "hgx-node-1", "gpu-pool", gpuDevices)
	model.UpdateFromResourceSlice(gpuSlice)

	// NIC slice: 8 NICs across 4 NUMA nodes, same PCIe roots
	nicDevices := make([]resourcev1.Device, 8)
	for i := 0; i < 8; i++ {
		numaNode := int64(i / 2)
		socket := numaNode / 2
		nicDevices[i] = resourcev1.Device{
			Name: makeDeviceName("nic", i),
			Attributes: map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
				resourcev1.QualifiedName(AttrNUMANode): {IntValue: intPtr(numaNode)},
				resourcev1.QualifiedName(AttrPCIeRoot): {StringValue: strPtr(pcieRoots[i])},
				resourcev1.QualifiedName(AttrSocket):   {IntValue: intPtr(socket)},
			},
		}
	}
	nicSlice := makeResourceSlice("nic-slice", "rdma.mellanox.com", "hgx-node-1", "nic-pool", nicDevices)
	model.UpdateFromResourceSlice(nicSlice)
}

func makeDeviceName(prefix string, index int) string {
	return prefix + "-" + string(rune('0'+index))
}

func TestPartitionBuilder_HGXScenario(t *testing.T) {
	model := NewTopologyModel()
	rules := NewTopologyRuleStore()
	builder := NewPartitionBuilder(model, rules)

	buildHGXB200Topology(model)

	results := builder.BuildPartitions()
	require.Len(t, results, 1, "should have partitions for exactly one node")

	result := results[0]
	assert.Equal(t, "hgx-node-1", result.NodeName)
	assert.NotEmpty(t, result.Profile)

	// Count partition types
	counts := make(map[PartitionType]int)
	for _, p := range result.Partitions {
		counts[p.Type]++
	}

	// With 8 unique PCIe roots: should get 8 eighth-partitions
	assert.Equal(t, 8, counts[PartitionEighth], "expected 8 eighth-partitions (one per PCIe root)")

	// With 4 NUMA nodes: should get 4 quarter-partitions
	assert.Equal(t, 4, counts[PartitionQuarter], "expected 4 quarter-partitions (one per NUMA)")

	// With 2 sockets: should get 2 half-partitions
	assert.Equal(t, 2, counts[PartitionHalf], "expected 2 half-partitions (one per socket)")

	// Should get 1 full partition
	assert.Equal(t, 1, counts[PartitionFull], "expected 1 full partition")
}

func TestPartitionBuilder_EighthPartitionContents(t *testing.T) {
	model := NewTopologyModel()
	rules := NewTopologyRuleStore()
	builder := NewPartitionBuilder(model, rules)

	buildHGXB200Topology(model)

	results := builder.BuildPartitions()
	require.Len(t, results, 1)

	// Each eighth-partition should contain 1 GPU + 1 NIC
	for _, p := range results[0].Partitions {
		if p.Type == PartitionEighth {
			totalDevices := 0
			for _, count := range p.DeviceCounts {
				totalDevices += count
			}
			assert.Equal(t, 2, totalDevices,
				"eighth partition %s should contain exactly 2 devices (1 GPU + 1 NIC)", p.Name)
			assert.Len(t, p.PCIeRoots, 1, "eighth partition should span exactly 1 PCIe root")
		}
	}
}

func TestPartitionBuilder_QuarterPartitionContents(t *testing.T) {
	model := NewTopologyModel()
	rules := NewTopologyRuleStore()
	builder := NewPartitionBuilder(model, rules)

	buildHGXB200Topology(model)

	results := builder.BuildPartitions()
	require.Len(t, results, 1)

	for _, p := range results[0].Partitions {
		if p.Type == PartitionQuarter {
			totalDevices := 0
			for _, count := range p.DeviceCounts {
				totalDevices += count
			}
			assert.Equal(t, 4, totalDevices,
				"quarter partition %s should contain 4 devices (2 GPU + 2 NIC)", p.Name)
			assert.Len(t, p.NUMANodes, 1, "quarter partition should span exactly 1 NUMA node")
		}
	}
}

func TestPartitionBuilder_HalfPartitionContents(t *testing.T) {
	model := NewTopologyModel()
	rules := NewTopologyRuleStore()
	builder := NewPartitionBuilder(model, rules)

	buildHGXB200Topology(model)

	results := builder.BuildPartitions()
	require.Len(t, results, 1)

	for _, p := range results[0].Partitions {
		if p.Type == PartitionHalf {
			totalDevices := 0
			for _, count := range p.DeviceCounts {
				totalDevices += count
			}
			assert.Equal(t, 8, totalDevices,
				"half partition %s should contain 8 devices (4 GPU + 4 NIC)", p.Name)
			assert.Len(t, p.Sockets, 1, "half partition should span exactly 1 socket")
		}
	}
}

func TestPartitionBuilder_FullPartitionContents(t *testing.T) {
	model := NewTopologyModel()
	rules := NewTopologyRuleStore()
	builder := NewPartitionBuilder(model, rules)

	buildHGXB200Topology(model)

	results := builder.BuildPartitions()
	require.Len(t, results, 1)

	for _, p := range results[0].Partitions {
		if p.Type == PartitionFull {
			totalDevices := 0
			for _, count := range p.DeviceCounts {
				totalDevices += count
			}
			assert.Equal(t, 16, totalDevices,
				"full partition should contain 16 devices (8 GPU + 8 NIC)")
		}
	}
}

func TestPartitionBuilder_WithNVLinkGrouping(t *testing.T) {
	model := NewTopologyModel()
	rules := NewTopologyRuleStore()

	// Add NVLink topology rule
	err := rules.LoadFromConfigMap(makeTopologyRuleConfigMap("nvlink", "default", map[string]string{
		"attribute":    "gpu.nvidia.com/nvlinkDomain",
		"type":         "int",
		"driver":       "gpu.nvidia.com",
		"partitioning": "group",
		"constraint":   "match",
	}))
	require.NoError(t, err)

	// Set rules in the model so extraction works
	model.SetRules(rules.GetRules())

	// Create 4 GPUs: 2 in NVLink domain 0 on NUMA 0, 2 in domain 1 on NUMA 1
	gpuDevices := []resourcev1.Device{
		{
			Name: "gpu-0",
			Attributes: map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
				resourcev1.QualifiedName(AttrNUMANode):                  {IntValue: intPtr(0)},
				resourcev1.QualifiedName(AttrPCIeRoot):                  {StringValue: strPtr("pcie-0")},
				resourcev1.QualifiedName(AttrSocket):                    {IntValue: intPtr(0)},
				resourcev1.QualifiedName("gpu.nvidia.com/nvlinkDomain"): {IntValue: intPtr(0)},
			},
		},
		{
			Name: "gpu-1",
			Attributes: map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
				resourcev1.QualifiedName(AttrNUMANode):                  {IntValue: intPtr(0)},
				resourcev1.QualifiedName(AttrPCIeRoot):                  {StringValue: strPtr("pcie-1")},
				resourcev1.QualifiedName(AttrSocket):                    {IntValue: intPtr(0)},
				resourcev1.QualifiedName("gpu.nvidia.com/nvlinkDomain"): {IntValue: intPtr(0)},
			},
		},
		{
			Name: "gpu-2",
			Attributes: map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
				resourcev1.QualifiedName(AttrNUMANode):                  {IntValue: intPtr(1)},
				resourcev1.QualifiedName(AttrPCIeRoot):                  {StringValue: strPtr("pcie-2")},
				resourcev1.QualifiedName(AttrSocket):                    {IntValue: intPtr(1)},
				resourcev1.QualifiedName("gpu.nvidia.com/nvlinkDomain"): {IntValue: intPtr(1)},
			},
		},
		{
			Name: "gpu-3",
			Attributes: map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
				resourcev1.QualifiedName(AttrNUMANode):                  {IntValue: intPtr(1)},
				resourcev1.QualifiedName(AttrPCIeRoot):                  {StringValue: strPtr("pcie-3")},
				resourcev1.QualifiedName(AttrSocket):                    {IntValue: intPtr(1)},
				resourcev1.QualifiedName("gpu.nvidia.com/nvlinkDomain"): {IntValue: intPtr(1)},
			},
		},
	}

	gpuSlice := makeResourceSlice("gpu-slice", "gpu.nvidia.com", "node-1", "gpu-pool", gpuDevices)
	model.UpdateFromResourceSlice(gpuSlice)

	builder := NewPartitionBuilder(model, rules)
	results := builder.BuildPartitions()
	require.Len(t, results, 1)

	// Verify that devices in each partition share the same NVLink domain
	for _, p := range results[0].Partitions {
		if p.Type == PartitionQuarter || p.Type == PartitionEighth {
			// All GPU devices in the partition should have the same nvlinkDomain
			var domain *int64
			for _, d := range p.Devices {
				if val, ok := d.ExtendedAttributes["gpu.nvidia.com/nvlinkDomain"]; ok {
					if domain == nil {
						domain = val.IntValue
					} else if val.IntValue != nil {
						assert.Equal(t, *domain, *val.IntValue,
							"all GPUs in partition %s should share the same NVLink domain", p.Name)
					}
				}
			}
		}
	}
}

func TestPartitionBuilder_EmptyModel(t *testing.T) {
	model := NewTopologyModel()
	rules := NewTopologyRuleStore()
	builder := NewPartitionBuilder(model, rules)

	results := builder.BuildPartitions()
	assert.Empty(t, results)
}

func TestPartitionBuilder_SingleDeviceNode(t *testing.T) {
	model := NewTopologyModel()
	rules := NewTopologyRuleStore()

	// Single GPU on a node
	model.UpdateFromResourceSlice(makeResourceSlice("gpu-slice", "gpu.nvidia.com", "node-1", "gpu-pool", []resourcev1.Device{
		makeGPUDevice("gpu-0", 0, "pcie-0"),
	}))

	builder := NewPartitionBuilder(model, rules)
	results := builder.BuildPartitions()
	require.Len(t, results, 1)

	// Should have only a full partition (no subdivisions possible with 1 group per topology level)
	counts := make(map[PartitionType]int)
	for _, p := range results[0].Partitions {
		counts[p.Type]++
	}
	assert.Equal(t, 1, counts[PartitionFull], "should have 1 full partition")
	// Single device means single group for all topology levels, so no sub-partitions
	assert.Equal(t, 0, counts[PartitionEighth]+counts[PartitionQuarter]+counts[PartitionHalf],
		"should have no sub-partitions with a single device")
}

func TestPartitionBuilder_MultipleNodes(t *testing.T) {
	model := NewTopologyModel()
	rules := NewTopologyRuleStore()

	// Node 1: 2 GPUs on different NUMA
	model.UpdateFromResourceSlice(makeResourceSlice("gpu-slice-1", "gpu.nvidia.com", "node-1", "gpu-pool-1", []resourcev1.Device{
		makeGPUDevice("gpu-0", 0, "pcie-0"),
		makeGPUDevice("gpu-1", 1, "pcie-1"),
	}))

	// Node 2: 2 GPUs on different NUMA
	model.UpdateFromResourceSlice(makeResourceSlice("gpu-slice-2", "gpu.nvidia.com", "node-2", "gpu-pool-2", []resourcev1.Device{
		makeGPUDevice("gpu-0", 0, "pcie-0"),
		makeGPUDevice("gpu-1", 1, "pcie-1"),
	}))

	builder := NewPartitionBuilder(model, rules)
	results := builder.BuildPartitions()
	assert.Len(t, results, 2, "should have partition results for 2 nodes")
}

func TestPartitionBuilder_TopologyRecomputation(t *testing.T) {
	model := NewTopologyModel()
	rules := NewTopologyRuleStore()

	// Initial: 2 GPUs
	model.UpdateFromResourceSlice(makeResourceSlice("gpu-slice", "gpu.nvidia.com", "node-1", "gpu-pool", []resourcev1.Device{
		makeGPUDevice("gpu-0", 0, "pcie-0"),
		makeGPUDevice("gpu-1", 1, "pcie-1"),
	}))

	builder := NewPartitionBuilder(model, rules)
	results := builder.BuildPartitions()
	require.Len(t, results, 1)

	initialPartitionCount := len(results[0].Partitions)

	// Add a third GPU
	model.UpdateFromResourceSlice(makeResourceSlice("gpu-slice", "gpu.nvidia.com", "node-1", "gpu-pool", []resourcev1.Device{
		makeGPUDevice("gpu-0", 0, "pcie-0"),
		makeGPUDevice("gpu-1", 1, "pcie-1"),
		makeGPUDevice("gpu-2", 2, "pcie-2"),
	}))

	results = builder.BuildPartitions()
	require.Len(t, results, 1)

	// Should have more partitions now due to additional topology groups
	newPartitionCount := len(results[0].Partitions)
	assert.Greater(t, newPartitionCount, initialPartitionCount,
		"adding a device on a new NUMA node should increase partition count")
}

func TestBaseDriverName(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"gpu.nvidia.com", "gpu.nvidia.com"},
		{"gpu.nvidia.com/gpu-pool", "gpu.nvidia.com"},
		{"simple", "simple"},
	}

	for _, tt := range tests {
		assert.Equal(t, tt.want, baseDriverName(tt.input))
	}
}
