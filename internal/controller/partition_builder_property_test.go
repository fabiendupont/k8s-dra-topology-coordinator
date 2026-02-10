package controller

import (
	"fmt"
	"math/rand"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	resourcev1 "k8s.io/api/resource/v1"
)

// --- Random topology generators ---

type randomTopology struct {
	nodeCount      int
	driversPerNode int
	devicesPerPool int
	numaNodes      int
	pcieRoots      int
	sockets        int
}

func (rt randomTopology) generate(rng *rand.Rand) *TopologyModel {
	model := NewTopologyModel()

	drivers := make([]string, rt.driversPerNode)
	for d := range drivers {
		drivers[d] = fmt.Sprintf("driver-%d.example.com", d)
	}

	for n := 0; n < rt.nodeCount; n++ {
		nodeName := fmt.Sprintf("node-%d", n)
		for _, driver := range drivers {
			poolName := fmt.Sprintf("pool-%s", driver)
			var devices []resourcev1.Device
			for i := 0; i < rt.devicesPerPool; i++ {
				numaNode := int64(rng.Intn(rt.numaNodes))
				pcieRoot := fmt.Sprintf("pcie-%d", rng.Intn(rt.pcieRoots))
				socket := int64(rng.Intn(rt.sockets))

				dev := resourcev1.Device{
					Name: fmt.Sprintf("%s-dev-%d", driver, i),
					Attributes: map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
						resourcev1.QualifiedName(AttrNUMANode): {IntValue: &numaNode},
						resourcev1.QualifiedName(AttrPCIeRoot): {StringValue: &pcieRoot},
						resourcev1.QualifiedName(AttrSocket):   {IntValue: &socket},
					},
				}
				devices = append(devices, dev)
			}

			slice := &resourcev1.ResourceSlice{}
			slice.Name = fmt.Sprintf("%s-%s-%s", nodeName, driver, poolName)
			slice.Spec.Driver = driver
			slice.Spec.NodeName = &nodeName
			slice.Spec.Pool.Name = poolName
			slice.Spec.Pool.Generation = 1
			slice.Spec.Pool.ResourceSliceCount = 1
			slice.Spec.Devices = devices

			model.UpdateFromResourceSlice(slice)
		}
	}

	return model
}

// --- Property 1: Every device appears in exactly one partition per granularity ---

func TestProperty_DeviceAppearsOncePerGranularity(t *testing.T) {
	rng := rand.New(rand.NewSource(42))

	for trial := 0; trial < 50; trial++ {
		topo := randomTopology{
			nodeCount:      rng.Intn(3) + 1,
			driversPerNode: rng.Intn(3) + 1,
			devicesPerPool: rng.Intn(6) + 1,
			numaNodes:      rng.Intn(4) + 1,
			pcieRoots:      rng.Intn(8) + 1,
			sockets:        rng.Intn(2) + 1,
		}

		model := topo.generate(rng)
		rules := NewTopologyRuleStore()
		builder := NewPartitionBuilder(model, rules)
		results := builder.BuildPartitions()

		for _, result := range results {
			byType := make(map[PartitionType][]PartitionDevice)
			for _, p := range result.Partitions {
				byType[p.Type] = append(byType[p.Type], p)
			}

			for partType, partitions := range byType {
				seen := make(map[string]string)
				for _, p := range partitions {
					for _, d := range p.Devices {
						key := d.DriverName + "/" + d.DeviceName
						if existingPart, exists := seen[key]; exists {
							t.Errorf("trial %d, node %s: device %s appears in both %s and %s at granularity %s",
								trial, result.NodeName, key, existingPart, p.Name, partType)
						}
						seen[key] = p.Name
					}
				}
			}
		}
	}
}

// --- Property 2: Determinism — same input always produces same output ---

func TestProperty_Deterministic(t *testing.T) {
	for trial := 0; trial < 20; trial++ {
		seed := int64(trial * 1000)
		topo := randomTopology{
			nodeCount:      2,
			driversPerNode: 2,
			devicesPerPool: 4,
			numaNodes:      2,
			pcieRoots:      4,
			sockets:        2,
		}

		rng1 := rand.New(rand.NewSource(seed))
		model1 := topo.generate(rng1)
		rules1 := NewTopologyRuleStore()
		builder1 := NewPartitionBuilder(model1, rules1)
		results1 := builder1.BuildPartitions()

		rng2 := rand.New(rand.NewSource(seed))
		model2 := topo.generate(rng2)
		rules2 := NewTopologyRuleStore()
		builder2 := NewPartitionBuilder(model2, rules2)
		results2 := builder2.BuildPartitions()

		require.Equal(t, len(results1), len(results2), "trial %d: different number of nodes", trial)

		resultMap1 := make(map[string]PartitionResult)
		for _, r := range results1 {
			resultMap1[r.NodeName] = r
		}
		resultMap2 := make(map[string]PartitionResult)
		for _, r := range results2 {
			resultMap2[r.NodeName] = r
		}

		for nodeName, r1 := range resultMap1 {
			r2, exists := resultMap2[nodeName]
			require.True(t, exists, "trial %d: node %s missing in second run", trial, nodeName)
			require.Equal(t, len(r1.Partitions), len(r2.Partitions),
				"trial %d, node %s: different partition count", trial, nodeName)
			assert.Equal(t, r1.Profile, r2.Profile,
				"trial %d, node %s: different profiles", trial, nodeName)

			names1 := make(map[string]int)
			names2 := make(map[string]int)
			for _, p := range r1.Partitions {
				names1[p.Name] = len(p.Devices)
			}
			for _, p := range r2.Partitions {
				names2[p.Name] = len(p.Devices)
			}
			assert.Equal(t, names1, names2,
				"trial %d, node %s: different partition names/device counts", trial, nodeName)
		}
	}
}

// --- Property 3: No data loss on rule changes ---

func TestProperty_NoDataLossOnRuleChange(t *testing.T) {
	rng := rand.New(rand.NewSource(99))

	for trial := 0; trial < 20; trial++ {
		topo := randomTopology{
			nodeCount:      rng.Intn(3) + 1,
			driversPerNode: rng.Intn(2) + 1,
			devicesPerPool: rng.Intn(4) + 2,
			numaNodes:      rng.Intn(4) + 1,
			pcieRoots:      rng.Intn(4) + 1,
			sockets:        rng.Intn(2) + 1,
		}

		model := topo.generate(rng)

		nodesBefore := model.GetNodeTopologies()
		totalDevicesBefore := 0
		for _, nt := range nodesBefore {
			totalDevicesBefore += len(nt.AllDevices())
		}

		model.SetRules([]TopologyRule{
			{
				Attribute:    "some.driver/attr",
				Type:         "int",
				Driver:       "some.driver",
				MapsTo:       MapsToNUMANode,
				Partitioning: PartitioningGroup,
			},
		})

		nodesAfter := model.GetNodeTopologies()
		totalDevicesAfter := 0
		for _, nt := range nodesAfter {
			totalDevicesAfter += len(nt.AllDevices())
		}

		assert.Equal(t, totalDevicesBefore, totalDevicesAfter,
			"trial %d: device count changed after SetRules (%d -> %d)",
			trial, totalDevicesBefore, totalDevicesAfter)
		assert.Equal(t, len(nodesBefore), len(nodesAfter),
			"trial %d: node count changed after SetRules", trial)
	}
}

// --- Property 4: Full partition always contains all devices ---

func TestProperty_FullPartitionContainsAllDevices(t *testing.T) {
	rng := rand.New(rand.NewSource(77))

	for trial := 0; trial < 50; trial++ {
		topo := randomTopology{
			nodeCount:      rng.Intn(3) + 1,
			driversPerNode: rng.Intn(3) + 1,
			devicesPerPool: rng.Intn(6) + 1,
			numaNodes:      rng.Intn(4) + 1,
			pcieRoots:      rng.Intn(8) + 1,
			sockets:        rng.Intn(2) + 1,
		}

		model := topo.generate(rng)
		rules := NewTopologyRuleStore()
		builder := NewPartitionBuilder(model, rules)
		results := builder.BuildPartitions()
		nodes := model.GetNodeTopologies()

		for _, result := range results {
			var fullPartition *PartitionDevice
			for i, p := range result.Partitions {
				if p.Type == PartitionFull {
					fullPartition = &result.Partitions[i]
					break
				}
			}

			require.NotNil(t, fullPartition,
				"trial %d, node %s: no full partition found", trial, result.NodeName)

			nodeTopo := nodes[result.NodeName]
			allDevices := nodeTopo.AllDevices()

			assert.Equal(t, len(allDevices), len(fullPartition.Devices),
				"trial %d, node %s: full partition has %d devices but node has %d",
				trial, result.NodeName, len(fullPartition.Devices), len(allDevices))
		}
	}
}

// --- Property 5: Partition device counts are consistent ---

func TestProperty_DeviceCountsMatchActualDevices(t *testing.T) {
	rng := rand.New(rand.NewSource(55))

	for trial := 0; trial < 50; trial++ {
		topo := randomTopology{
			nodeCount:      rng.Intn(2) + 1,
			driversPerNode: rng.Intn(3) + 1,
			devicesPerPool: rng.Intn(4) + 1,
			numaNodes:      rng.Intn(4) + 1,
			pcieRoots:      rng.Intn(4) + 1,
			sockets:        rng.Intn(2) + 1,
		}

		model := topo.generate(rng)
		rules := NewTopologyRuleStore()
		builder := NewPartitionBuilder(model, rules)
		results := builder.BuildPartitions()

		for _, result := range results {
			for _, p := range result.Partitions {
				actualCounts := make(map[string]int)
				for _, d := range p.Devices {
					actualCounts[baseDriverName(d.DriverName)]++
				}

				assert.Equal(t, actualCounts, p.DeviceCounts,
					"trial %d, node %s, partition %s: DeviceCounts mismatch",
					trial, result.NodeName, p.Name)
			}
		}
	}
}

// --- Property 6: Quarter partitions group by NUMA node ---

func TestProperty_QuarterPartitionsShareNUMA(t *testing.T) {
	rng := rand.New(rand.NewSource(33))

	for trial := 0; trial < 50; trial++ {
		topo := randomTopology{
			nodeCount:      1,
			driversPerNode: rng.Intn(2) + 1,
			devicesPerPool: rng.Intn(6) + 2,
			numaNodes:      rng.Intn(3) + 2,
			pcieRoots:      rng.Intn(8) + 2,
			sockets:        rng.Intn(2) + 1,
		}

		model := topo.generate(rng)
		rules := NewTopologyRuleStore()
		builder := NewPartitionBuilder(model, rules)
		results := builder.BuildPartitions()

		for _, result := range results {
			for _, p := range result.Partitions {
				if p.Type != PartitionQuarter {
					continue
				}

				numaNodes := make(map[int64]bool)
				for _, d := range p.Devices {
					if d.NUMANode != nil {
						numaNodes[*d.NUMANode] = true
					}
				}

				assert.LessOrEqual(t, len(numaNodes), 1,
					"trial %d, partition %s: quarter partition spans %d NUMA nodes (should be 1)",
					trial, p.Name, len(numaNodes))
			}
		}
	}
}
