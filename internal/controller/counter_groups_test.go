package controller

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	resourcev1 "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

func makeCounter(value int64) resourcev1.Counter {
	return resourcev1.Counter{
		Value: *resource.NewQuantity(value, resource.DecimalSI),
	}
}

func makePartitionableGPUDevice(name string, numaNode int64, pcieRoot string, counterSet string, counters map[string]int64) TopologyDevice {
	consumptions := []resourcev1.DeviceCounterConsumption{
		{
			CounterSet: counterSet,
			Counters:   make(map[string]resourcev1.Counter),
		},
	}
	for k, v := range counters {
		consumptions[0].Counters[k] = makeCounter(v)
	}

	return TopologyDevice{
		DriverName:         "gpu.amd.com",
		DeviceName:         name,
		NodeName:           "node-1",
		PoolName:           "gpu-pool",
		NUMANode:           intPtr(numaNode),
		PCIeRoot:           strPtr(pcieRoot),
		ConsumesCounters:   consumptions,
		ExtendedAttributes: map[string]DeviceAttributeValue{},
	}
}

func TestGroupOverlappingDevices_NoCounters(t *testing.T) {
	devices := []TopologyDevice{
		{DriverName: "gpu.nvidia.com", DeviceName: "gpu-0", ExtendedAttributes: map[string]DeviceAttributeValue{}},
		{DriverName: "gpu.nvidia.com", DeviceName: "gpu-1", ExtendedAttributes: map[string]DeviceAttributeValue{}},
	}

	nonOverlapping, groups := GroupOverlappingDevices(devices)
	assert.Len(t, nonOverlapping, 2)
	assert.Empty(t, groups)
}

func TestGroupOverlappingDevices_SingleGPUWithPartitions(t *testing.T) {
	// Simulate one AMD MI355X GPU: 1 SPX + 2 DPX + 8 CPX = 11 advertised devices
	devices := []TopologyDevice{
		makePartitionableGPUDevice("gpu-0-spx", 0, "pcie-0", "gpu-0-counters", map[string]int64{"xcds": 8, "memory": 288}),
		makePartitionableGPUDevice("gpu-0-dpx-0", 0, "pcie-0", "gpu-0-counters", map[string]int64{"xcds": 4, "memory": 144}),
		makePartitionableGPUDevice("gpu-0-dpx-1", 0, "pcie-0", "gpu-0-counters", map[string]int64{"xcds": 4, "memory": 144}),
		makePartitionableGPUDevice("gpu-0-cpx-0", 0, "pcie-0", "gpu-0-counters", map[string]int64{"xcds": 1, "memory": 36}),
		makePartitionableGPUDevice("gpu-0-cpx-1", 0, "pcie-0", "gpu-0-counters", map[string]int64{"xcds": 1, "memory": 36}),
		makePartitionableGPUDevice("gpu-0-cpx-2", 0, "pcie-0", "gpu-0-counters", map[string]int64{"xcds": 1, "memory": 36}),
		makePartitionableGPUDevice("gpu-0-cpx-3", 0, "pcie-0", "gpu-0-counters", map[string]int64{"xcds": 1, "memory": 36}),
		makePartitionableGPUDevice("gpu-0-cpx-4", 0, "pcie-0", "gpu-0-counters", map[string]int64{"xcds": 1, "memory": 36}),
		makePartitionableGPUDevice("gpu-0-cpx-5", 0, "pcie-0", "gpu-0-counters", map[string]int64{"xcds": 1, "memory": 36}),
		makePartitionableGPUDevice("gpu-0-cpx-6", 0, "pcie-0", "gpu-0-counters", map[string]int64{"xcds": 1, "memory": 36}),
		makePartitionableGPUDevice("gpu-0-cpx-7", 0, "pcie-0", "gpu-0-counters", map[string]int64{"xcds": 1, "memory": 36}),
	}

	nonOverlapping, groups := GroupOverlappingDevices(devices)

	assert.Empty(t, nonOverlapping, "all devices have ConsumesCounters")
	require.Len(t, groups, 1, "all 11 devices share one counter set → one group")
	assert.Equal(t, 1, groups[0].MaxEffective, "one physical GPU = 1 effective device")
	assert.Len(t, groups[0].Devices, 11)
}

func TestGroupOverlappingDevices_MultipleGPUs(t *testing.T) {
	// 2 physical GPUs, each with SPX + 2 DPX
	devices := []TopologyDevice{
		makePartitionableGPUDevice("gpu-0-spx", 0, "pcie-0", "gpu-0-counters", map[string]int64{"xcds": 8}),
		makePartitionableGPUDevice("gpu-0-dpx-0", 0, "pcie-0", "gpu-0-counters", map[string]int64{"xcds": 4}),
		makePartitionableGPUDevice("gpu-0-dpx-1", 0, "pcie-0", "gpu-0-counters", map[string]int64{"xcds": 4}),

		makePartitionableGPUDevice("gpu-1-spx", 1, "pcie-1", "gpu-1-counters", map[string]int64{"xcds": 8}),
		makePartitionableGPUDevice("gpu-1-dpx-0", 1, "pcie-1", "gpu-1-counters", map[string]int64{"xcds": 4}),
		makePartitionableGPUDevice("gpu-1-dpx-1", 1, "pcie-1", "gpu-1-counters", map[string]int64{"xcds": 4}),
	}

	nonOverlapping, groups := GroupOverlappingDevices(devices)

	assert.Empty(t, nonOverlapping)
	require.Len(t, groups, 2, "2 distinct counter sets → 2 groups")
	assert.Equal(t, 1, groups[0].MaxEffective)
	assert.Equal(t, 1, groups[1].MaxEffective)
}

func TestGroupOverlappingDevices_MixedDrivers(t *testing.T) {
	// One partitionable GPU + one non-partitionable NIC
	devices := []TopologyDevice{
		makePartitionableGPUDevice("gpu-0-spx", 0, "pcie-0", "gpu-0-counters", map[string]int64{"xcds": 8}),
		makePartitionableGPUDevice("gpu-0-dpx-0", 0, "pcie-0", "gpu-0-counters", map[string]int64{"xcds": 4}),
		{
			DriverName:         "rdma.mellanox.com",
			DeviceName:         "nic-0",
			NodeName:           "node-1",
			PoolName:           "nic-pool",
			NUMANode:           intPtr(0),
			PCIeRoot:           strPtr("pcie-0"),
			ExtendedAttributes: map[string]DeviceAttributeValue{},
		},
	}

	nonOverlapping, groups := GroupOverlappingDevices(devices)

	assert.Len(t, nonOverlapping, 1, "NIC has no ConsumesCounters")
	assert.Equal(t, "rdma.mellanox.com", nonOverlapping[0].DriverName)
	require.Len(t, groups, 1, "GPUs share one counter set")
	assert.Equal(t, 1, groups[0].MaxEffective)
}

func TestEffectiveDeviceCount_NoCounters(t *testing.T) {
	devices := []TopologyDevice{
		{DriverName: "gpu.nvidia.com", DeviceName: "gpu-0", ExtendedAttributes: map[string]DeviceAttributeValue{}},
		{DriverName: "gpu.nvidia.com", DeviceName: "gpu-1", ExtendedAttributes: map[string]DeviceAttributeValue{}},
		{DriverName: "rdma.mellanox.com", DeviceName: "nic-0", ExtendedAttributes: map[string]DeviceAttributeValue{}},
	}

	counts := EffectiveDeviceCount(devices)
	assert.Equal(t, 2, counts["gpu.nvidia.com"])
	assert.Equal(t, 1, counts["rdma.mellanox.com"])
}

func TestEffectiveDeviceCount_WithPartitions(t *testing.T) {
	// 2 physical GPUs (each SPX + 2 DPX = 3 advertised) + 2 NICs
	devices := []TopologyDevice{
		makePartitionableGPUDevice("gpu-0-spx", 0, "pcie-0", "gpu-0-counters", map[string]int64{"xcds": 8}),
		makePartitionableGPUDevice("gpu-0-dpx-0", 0, "pcie-0", "gpu-0-counters", map[string]int64{"xcds": 4}),
		makePartitionableGPUDevice("gpu-0-dpx-1", 0, "pcie-0", "gpu-0-counters", map[string]int64{"xcds": 4}),

		makePartitionableGPUDevice("gpu-1-spx", 1, "pcie-1", "gpu-1-counters", map[string]int64{"xcds": 8}),
		makePartitionableGPUDevice("gpu-1-dpx-0", 1, "pcie-1", "gpu-1-counters", map[string]int64{"xcds": 4}),
		makePartitionableGPUDevice("gpu-1-dpx-1", 1, "pcie-1", "gpu-1-counters", map[string]int64{"xcds": 4}),

		{DriverName: "rdma.mellanox.com", DeviceName: "nic-0", ExtendedAttributes: map[string]DeviceAttributeValue{}},
		{DriverName: "rdma.mellanox.com", DeviceName: "nic-1", ExtendedAttributes: map[string]DeviceAttributeValue{}},
	}

	counts := EffectiveDeviceCount(devices)
	assert.Equal(t, 2, counts["gpu.amd.com"], "6 advertised GPU devices → 2 effective (2 physical GPUs)")
	assert.Equal(t, 2, counts["rdma.mellanox.com"], "2 NICs unchanged")
}

func TestFilterToEffectiveDevices(t *testing.T) {
	devices := []TopologyDevice{
		makePartitionableGPUDevice("gpu-0-spx", 0, "pcie-0", "gpu-0-counters", map[string]int64{"xcds": 8, "memory": 288}),
		makePartitionableGPUDevice("gpu-0-dpx-0", 0, "pcie-0", "gpu-0-counters", map[string]int64{"xcds": 4, "memory": 144}),
		makePartitionableGPUDevice("gpu-0-dpx-1", 0, "pcie-0", "gpu-0-counters", map[string]int64{"xcds": 4, "memory": 144}),
		{DriverName: "rdma.mellanox.com", DeviceName: "nic-0", NUMANode: intPtr(0), ExtendedAttributes: map[string]DeviceAttributeValue{}},
	}

	effective := FilterToEffectiveDevices(devices)
	assert.Len(t, effective, 2, "1 representative GPU + 1 NIC")

	// The representative should be the SPX (highest counter consumption)
	var gpuDevice TopologyDevice
	for _, d := range effective {
		if d.DriverName == "gpu.amd.com" {
			gpuDevice = d
		}
	}
	assert.Equal(t, "gpu-0-spx", gpuDevice.DeviceName, "SPX should be selected as representative")
}

func TestSmallestPartitionDevices(t *testing.T) {
	devices := []TopologyDevice{
		makePartitionableGPUDevice("gpu-0-spx", 0, "pcie-0", "gpu-0-counters", map[string]int64{"xcds": 8}),
		makePartitionableGPUDevice("gpu-0-dpx-0", 0, "pcie-0", "gpu-0-counters", map[string]int64{"xcds": 4}),
		makePartitionableGPUDevice("gpu-0-dpx-1", 0, "pcie-0", "gpu-0-counters", map[string]int64{"xcds": 4}),
		makePartitionableGPUDevice("gpu-0-cpx-0", 0, "pcie-0", "gpu-0-counters", map[string]int64{"xcds": 1}),
		makePartitionableGPUDevice("gpu-0-cpx-1", 0, "pcie-0", "gpu-0-counters", map[string]int64{"xcds": 1}),
		{DriverName: "rdma.mellanox.com", DeviceName: "nic-0", ExtendedAttributes: map[string]DeviceAttributeValue{}},
	}

	smallest := SmallestPartitionDevices(devices)

	// Should get all CPX devices (smallest) + NIC
	assert.Len(t, smallest, 3, "2 CPX devices + 1 NIC")

	var gpuNames []string
	for _, d := range smallest {
		if d.DriverName == "gpu.amd.com" {
			gpuNames = append(gpuNames, d.DeviceName)
		}
	}
	assert.Contains(t, gpuNames, "gpu-0-cpx-0")
	assert.Contains(t, gpuNames, "gpu-0-cpx-1")
}

func TestTotalCounterConsumption(t *testing.T) {
	consumptions := []resourcev1.DeviceCounterConsumption{
		{
			CounterSet: "gpu-0-counters",
			Counters: map[string]resourcev1.Counter{
				"xcds":   makeCounter(4),
				"memory": makeCounter(144),
			},
		},
	}

	total := totalCounterConsumption(consumptions)
	assert.Equal(t, int64(148), total)
}

func TestSelectRepresentative_SingleDevice(t *testing.T) {
	devices := []TopologyDevice{
		makePartitionableGPUDevice("gpu-0-spx", 0, "pcie-0", "gpu-0-counters", map[string]int64{"xcds": 8}),
	}

	rep := selectRepresentative(devices)
	assert.Equal(t, "gpu-0-spx", rep.DeviceName)
}

func TestSelectRepresentative_PicksLargest(t *testing.T) {
	devices := []TopologyDevice{
		makePartitionableGPUDevice("gpu-0-cpx-0", 0, "pcie-0", "gpu-0-counters", map[string]int64{"xcds": 1}),
		makePartitionableGPUDevice("gpu-0-spx", 0, "pcie-0", "gpu-0-counters", map[string]int64{"xcds": 8}),
		makePartitionableGPUDevice("gpu-0-dpx-0", 0, "pcie-0", "gpu-0-counters", map[string]int64{"xcds": 4}),
	}

	rep := selectRepresentative(devices)
	assert.Equal(t, "gpu-0-spx", rep.DeviceName, "should pick device with highest counter consumption")
}

func TestGroupOverlappingDevices_EmptyInput(t *testing.T) {
	nonOverlapping, groups := GroupOverlappingDevices(nil)
	assert.Empty(t, nonOverlapping)
	assert.Empty(t, groups)
}
