package controller

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func makeGroupingConfigMap(name string, data map[string]string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "default",
			Labels: map[string]string{
				DeviceGroupingLabel: "true",
			},
		},
		Data: data,
	}
}

func TestGroupingStore_LoadFromConfigMap(t *testing.T) {
	store := NewGroupingStore()

	cm := makeGroupingConfigMap("gpu-dpu-pair", map[string]string{
		"name":      "gpu-dpu-pair",
		"alignment": "pcieRoot",
		"fallback":  "numaNode",
		"devices": `- class: gpu.amd.com
  count: 1
- class: dpu.pensando.com
  count: 1`,
	})

	err := store.LoadFromConfigMap(cm)
	require.NoError(t, err)

	groupings := store.GetGroupings()
	require.Len(t, groupings, 1)

	g := groupings[0]
	assert.Equal(t, "gpu-dpu-pair", g.Name)
	assert.Equal(t, "pcieRoot", g.Alignment)
	assert.Equal(t, "numaNode", g.Fallback)
	require.Len(t, g.Devices, 2)
	assert.Equal(t, "gpu.amd.com", g.Devices[0].Class)
	assert.Equal(t, 1, g.Devices[0].Count)
	assert.Equal(t, "dpu.pensando.com", g.Devices[1].Class)
	assert.Equal(t, 1, g.Devices[1].Count)
}

func TestGroupingStore_LoadFromConfigMap_NoFallback(t *testing.T) {
	store := NewGroupingStore()

	cm := makeGroupingConfigMap("gpu-only", map[string]string{
		"name":      "gpu-only",
		"alignment": "numaNode",
		"devices": `- class: gpu.amd.com
  count: 2`,
	})

	err := store.LoadFromConfigMap(cm)
	require.NoError(t, err)

	groupings := store.GetGroupings()
	require.Len(t, groupings, 1)
	assert.Equal(t, "", groupings[0].Fallback)
}

func TestGroupingStore_LoadFromConfigMap_WithCapacity(t *testing.T) {
	store := NewGroupingStore()

	cm := makeGroupingConfigMap("gpu-cpu", map[string]string{
		"name":      "gpu-cpu",
		"alignment": "numaNode",
		"devices": `- class: gpu.amd.com
  count: 1
- class: dra.cpu
  count: 1
  capacity:
    dra.cpu/cpu: "16"`,
	})

	err := store.LoadFromConfigMap(cm)
	require.NoError(t, err)

	groupings := store.GetGroupings()
	require.Len(t, groupings, 1)
	require.Len(t, groupings[0].Devices, 2)
	assert.Equal(t, map[string]string{"dra.cpu/cpu": "16"}, groupings[0].Devices[1].Capacity)
}

func TestGroupingStore_LoadFromConfigMap_Errors(t *testing.T) {
	tests := []struct {
		name string
		data map[string]string
	}{
		{"missing name", map[string]string{"alignment": "pcieRoot", "devices": "- class: x\n  count: 1"}},
		{"missing alignment", map[string]string{"name": "test", "devices": "- class: x\n  count: 1"}},
		{"invalid alignment", map[string]string{"name": "test", "alignment": "invalid", "devices": "- class: x\n  count: 1"}},
		{"invalid fallback", map[string]string{"name": "test", "alignment": "pcieRoot", "fallback": "invalid", "devices": "- class: x\n  count: 1"}},
		{"missing devices", map[string]string{"name": "test", "alignment": "pcieRoot"}},
		{"empty devices", map[string]string{"name": "test", "alignment": "pcieRoot", "devices": "[]"}},
		{"device missing class", map[string]string{"name": "test", "alignment": "pcieRoot", "devices": "- count: 1"}},
		{"device zero count", map[string]string{"name": "test", "alignment": "pcieRoot", "devices": "- class: x\n  count: 0"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := NewGroupingStore()
			cm := makeGroupingConfigMap("test", tt.data)
			err := store.LoadFromConfigMap(cm)
			assert.Error(t, err)
		})
	}
}

func TestGroupingStore_RemoveConfigMap(t *testing.T) {
	store := NewGroupingStore()

	cm := makeGroupingConfigMap("test", map[string]string{
		"name":      "test",
		"alignment": "pcieRoot",
		"devices":   "- class: gpu.amd.com\n  count: 1",
	})

	require.NoError(t, store.LoadFromConfigMap(cm))
	require.Len(t, store.GetGroupings(), 1)

	store.RemoveConfigMap("default", "test")
	assert.Len(t, store.GetGroupings(), 0)
}

// --- GroupingBuilder tests ---

func makeTestDevice(driver, name string, numa *int64, pcieRoot *string) TopologyDevice {
	return TopologyDevice{
		DriverName:         driver,
		DeviceName:         name,
		NUMANode:           numa,
		PCIeRoot:           pcieRoot,
		ExtendedAttributes: make(map[string]DeviceAttributeValue),
	}
}

func int64Ptr(v int64) *int64    { return &v }
func stringPtr(v string) *string { return &v }

func TestGroupingBuilder_BasicPCIeRootAlignment(t *testing.T) {
	model := NewTopologyModel()
	rules := NewTopologyRuleStore()

	// Simulate 2 PCIe roots, each with 1 GPU + 1 DPU
	nodeTopo := &NodeTopology{
		NodeName:        "node-1",
		DevicesByDriver: make(map[string][]TopologyDevice),
	}
	nodeTopo.DevicesByDriver["gpu.amd.com"] = []TopologyDevice{
		makeTestDevice("gpu.amd.com", "gpu-0", int64Ptr(0), stringPtr("pcie-0")),
		makeTestDevice("gpu.amd.com", "gpu-1", int64Ptr(0), stringPtr("pcie-1")),
	}
	nodeTopo.DevicesByDriver["dpu.pensando.com"] = []TopologyDevice{
		makeTestDevice("dpu.pensando.com", "dpu-0", int64Ptr(0), stringPtr("pcie-0")),
		makeTestDevice("dpu.pensando.com", "dpu-1", int64Ptr(0), stringPtr("pcie-1")),
	}

	model.mu.Lock()
	model.nodes = map[string]*NodeTopology{"node-1": nodeTopo}
	model.mu.Unlock()

	builder := NewGroupingBuilder(model, rules)

	grouping := DeviceGrouping{
		Name:      "gpu-dpu-pair",
		Alignment: "pcieRoot",
		Devices: []GroupingDevice{
			{Class: "gpu.amd.com", Count: 1},
			{Class: "dpu.pensando.com", Count: 1},
		},
	}

	results := builder.BuildGroupings([]DeviceGrouping{grouping})
	require.Len(t, results, 1)

	instances := results[0].Instances
	assert.Len(t, instances, 2)

	for _, inst := range instances {
		assert.Equal(t, "gpu-dpu-pair", inst.GroupingName)
		assert.Equal(t, "pcieRoot", inst.Alignment)
		assert.Equal(t, 1, inst.DeviceCounts["gpu.amd.com"])
		assert.Equal(t, 1, inst.DeviceCounts["dpu.pensando.com"])
	}
}

func TestGroupingBuilder_UnsatisfiableGrouping(t *testing.T) {
	model := NewTopologyModel()
	rules := NewTopologyRuleStore()

	// Node has GPUs but no DPUs
	nodeTopo := &NodeTopology{
		NodeName:        "node-1",
		DevicesByDriver: make(map[string][]TopologyDevice),
	}
	nodeTopo.DevicesByDriver["gpu.amd.com"] = []TopologyDevice{
		makeTestDevice("gpu.amd.com", "gpu-0", int64Ptr(0), stringPtr("pcie-0")),
	}

	model.mu.Lock()
	model.nodes = map[string]*NodeTopology{"node-1": nodeTopo}
	model.mu.Unlock()

	builder := NewGroupingBuilder(model, rules)

	grouping := DeviceGrouping{
		Name:      "gpu-dpu-pair",
		Alignment: "pcieRoot",
		Devices: []GroupingDevice{
			{Class: "gpu.amd.com", Count: 1},
			{Class: "dpu.pensando.com", Count: 1},
		},
	}

	results := builder.BuildGroupings([]DeviceGrouping{grouping})
	// No instances because DPU class doesn't exist on node
	totalInstances := 0
	for _, r := range results {
		totalInstances += len(r.Instances)
	}
	assert.Equal(t, 0, totalInstances)
}

func TestGroupingBuilder_AsymmetricDevices(t *testing.T) {
	model := NewTopologyModel()
	rules := NewTopologyRuleStore()

	// SMC6217-like: 3 PCIe roots, only 2 have NVMe
	nodeTopo := &NodeTopology{
		NodeName:        "node-1",
		DevicesByDriver: make(map[string][]TopologyDevice),
	}
	nodeTopo.DevicesByDriver["gpu.amd.com"] = []TopologyDevice{
		makeTestDevice("gpu.amd.com", "gpu-0", int64Ptr(0), stringPtr("pcie-0")),
		makeTestDevice("gpu.amd.com", "gpu-1", int64Ptr(0), stringPtr("pcie-1")),
		makeTestDevice("gpu.amd.com", "gpu-2", int64Ptr(0), stringPtr("pcie-2")),
	}
	nodeTopo.DevicesByDriver["nvme.kioxia.com"] = []TopologyDevice{
		makeTestDevice("nvme.kioxia.com", "nvme-0", int64Ptr(0), stringPtr("pcie-0")),
		makeTestDevice("nvme.kioxia.com", "nvme-1", int64Ptr(0), stringPtr("pcie-1")),
		// pcie-2 has no NVMe
	}

	model.mu.Lock()
	model.nodes = map[string]*NodeTopology{"node-1": nodeTopo}
	model.mu.Unlock()

	builder := NewGroupingBuilder(model, rules)

	grouping := DeviceGrouping{
		Name:      "gpu-nvme",
		Alignment: "pcieRoot",
		Devices: []GroupingDevice{
			{Class: "gpu.amd.com", Count: 1},
			{Class: "nvme.kioxia.com", Count: 1},
		},
	}

	results := builder.BuildGroupings([]DeviceGrouping{grouping})
	require.Len(t, results, 1)

	// Only 2 of 3 PCIe roots have both GPU + NVMe
	assert.Len(t, results[0].Instances, 2)
}

func TestGroupingBuilder_FallbackAlignment(t *testing.T) {
	model := NewTopologyModel()
	rules := NewTopologyRuleStore()

	// 2 NUMA nodes, 4 PCIe roots
	// NIC only on pcie-0 (NUMA 0). Other GPUs have no co-located NIC.
	// At numaNode level, NUMA 0 has 2 GPUs + 1 NIC → 1 pair possible at numaNode.
	nodeTopo := &NodeTopology{
		NodeName:        "node-1",
		DevicesByDriver: make(map[string][]TopologyDevice),
	}
	nodeTopo.DevicesByDriver["gpu.amd.com"] = []TopologyDevice{
		makeTestDevice("gpu.amd.com", "gpu-0", int64Ptr(0), stringPtr("pcie-0")),
		makeTestDevice("gpu.amd.com", "gpu-1", int64Ptr(0), stringPtr("pcie-1")),
		makeTestDevice("gpu.amd.com", "gpu-2", int64Ptr(1), stringPtr("pcie-2")),
		makeTestDevice("gpu.amd.com", "gpu-3", int64Ptr(1), stringPtr("pcie-3")),
	}
	nodeTopo.DevicesByDriver["nic.mellanox.com"] = []TopologyDevice{
		makeTestDevice("nic.mellanox.com", "nic-0", int64Ptr(0), stringPtr("pcie-0")),
		makeTestDevice("nic.mellanox.com", "nic-1", int64Ptr(1), stringPtr("pcie-4")),
	}

	model.mu.Lock()
	model.nodes = map[string]*NodeTopology{"node-1": nodeTopo}
	model.mu.Unlock()

	builder := NewGroupingBuilder(model, rules)

	grouping := DeviceGrouping{
		Name:      "gpu-nic",
		Alignment: "pcieRoot",
		Fallback:  "numaNode",
		Devices: []GroupingDevice{
			{Class: "gpu.amd.com", Count: 1},
			{Class: "nic.mellanox.com", Count: 1},
		},
	}

	results := builder.BuildGroupings([]DeviceGrouping{grouping})
	require.Len(t, results, 1)

	instances := results[0].Instances

	// Count by alignment level
	pcieRootCount := 0
	numaNodeCount := 0
	for _, inst := range instances {
		switch inst.Alignment {
		case "pcieRoot":
			pcieRootCount++
		case "numaNode":
			numaNodeCount++
		}
	}

	// pcie-0 has GPU+NIC → 1 pcieRoot instance
	assert.Equal(t, 1, pcieRootCount, "should have 1 pcieRoot instance (pcie-0)")
	// Remaining: gpu-1 (NUMA 0) can pair with nic-1? No, nic-0 was used.
	// nic-1 is on NUMA 1, pcie-4 — not consumed at pcieRoot level.
	// At numaNode fallback, NUMA 1 has gpu-2, gpu-3, nic-1 → 1 numaNode pair.
	assert.Equal(t, 1, numaNodeCount, "should have 1 numaNode fallback instance (NUMA 1)")
}

func TestGroupingBuilder_MultipleGroupings(t *testing.T) {
	model := NewTopologyModel()
	rules := NewTopologyRuleStore()

	nodeTopo := &NodeTopology{
		NodeName:        "node-1",
		DevicesByDriver: make(map[string][]TopologyDevice),
	}
	nodeTopo.DevicesByDriver["gpu.amd.com"] = []TopologyDevice{
		makeTestDevice("gpu.amd.com", "gpu-0", int64Ptr(0), stringPtr("pcie-0")),
	}
	nodeTopo.DevicesByDriver["dpu.pensando.com"] = []TopologyDevice{
		makeTestDevice("dpu.pensando.com", "dpu-0", int64Ptr(0), stringPtr("pcie-0")),
	}
	nodeTopo.DevicesByDriver["nvme.kioxia.com"] = []TopologyDevice{
		makeTestDevice("nvme.kioxia.com", "nvme-0", int64Ptr(0), stringPtr("pcie-0")),
	}

	model.mu.Lock()
	model.nodes = map[string]*NodeTopology{"node-1": nodeTopo}
	model.mu.Unlock()

	builder := NewGroupingBuilder(model, rules)

	groupings := []DeviceGrouping{
		{
			Name:      "gpu-dpu",
			Alignment: "pcieRoot",
			Devices: []GroupingDevice{
				{Class: "gpu.amd.com", Count: 1},
				{Class: "dpu.pensando.com", Count: 1},
			},
		},
		{
			Name:      "gpu-dpu-nvme",
			Alignment: "pcieRoot",
			Devices: []GroupingDevice{
				{Class: "gpu.amd.com", Count: 1},
				{Class: "dpu.pensando.com", Count: 1},
				{Class: "nvme.kioxia.com", Count: 1},
			},
		},
	}

	results := builder.BuildGroupings(groupings)
	require.Len(t, results, 1)

	// Both groupings should be satisfiable
	nameCount := make(map[string]int)
	for _, inst := range results[0].Instances {
		nameCount[inst.GroupingName]++
	}
	assert.Equal(t, 1, nameCount["gpu-dpu"])
	assert.Equal(t, 1, nameCount["gpu-dpu-nvme"])
}

func TestGroupingBuilder_MultipleInstancesUniqueDevices(t *testing.T) {
	model := NewTopologyModel()
	rules := NewTopologyRuleStore()

	// 1 NUMA node with 4 GPUs and 4 DPUs — should yield 4 instances,
	// each with unique devices.
	nodeTopo := &NodeTopology{
		NodeName:        "node-1",
		DevicesByDriver: make(map[string][]TopologyDevice),
	}
	nodeTopo.DevicesByDriver["gpu.amd.com"] = []TopologyDevice{
		makeTestDevice("gpu.amd.com", "gpu-0", int64Ptr(0), stringPtr("pcie-0")),
		makeTestDevice("gpu.amd.com", "gpu-1", int64Ptr(0), stringPtr("pcie-1")),
		makeTestDevice("gpu.amd.com", "gpu-2", int64Ptr(0), stringPtr("pcie-2")),
		makeTestDevice("gpu.amd.com", "gpu-3", int64Ptr(0), stringPtr("pcie-3")),
	}
	nodeTopo.DevicesByDriver["dpu.pensando.com"] = []TopologyDevice{
		makeTestDevice("dpu.pensando.com", "dpu-0", int64Ptr(0), stringPtr("pcie-0")),
		makeTestDevice("dpu.pensando.com", "dpu-1", int64Ptr(0), stringPtr("pcie-1")),
		makeTestDevice("dpu.pensando.com", "dpu-2", int64Ptr(0), stringPtr("pcie-2")),
		makeTestDevice("dpu.pensando.com", "dpu-3", int64Ptr(0), stringPtr("pcie-3")),
	}

	model.mu.Lock()
	model.nodes = map[string]*NodeTopology{"node-1": nodeTopo}
	model.mu.Unlock()

	builder := NewGroupingBuilder(model, rules)

	grouping := DeviceGrouping{
		Name:      "gpu-dpu-pair",
		Alignment: "numaNode",
		Devices: []GroupingDevice{
			{Class: "gpu.amd.com", Count: 1},
			{Class: "dpu.pensando.com", Count: 1},
		},
	}

	results := builder.BuildGroupings([]DeviceGrouping{grouping})
	require.Len(t, results, 1)
	instances := results[0].Instances

	// 4 GPUs / 1 per instance = 4 instances
	require.Len(t, instances, 4)

	// Each instance should have exactly 2 devices (1 GPU + 1 DPU)
	allDeviceNames := make(map[string]bool)
	for _, inst := range instances {
		assert.Len(t, inst.Devices, 2, "each instance should have exactly 2 devices")
		assert.Equal(t, 1, inst.DeviceCounts["gpu.amd.com"])
		assert.Equal(t, 1, inst.DeviceCounts["dpu.pensando.com"])

		for _, d := range inst.Devices {
			key := d.DriverName + "/" + d.DeviceName
			assert.False(t, allDeviceNames[key], "device %s should not appear in multiple instances", key)
			allDeviceNames[key] = true
		}
	}
	assert.Len(t, allDeviceNames, 8, "all 8 devices should be allocated")
}
