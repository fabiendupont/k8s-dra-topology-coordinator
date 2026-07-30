package controller

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestDeviceClassManager_SyncDeviceClasses(t *testing.T) {
	client := fake.NewSimpleClientset()
	rules := NewTopologyRuleStore()
	manager := NewDeviceClassManager(client, CoordinatorDriverName, rules)

	results := []PartitionResult{
		{
			NodeName: "node-1",
			Profile:  "gpu-nvidia-com-8_rdma-mellanox-com-8",
			Partitions: []PartitionDevice{
				{
					Name:    "node-1-quarter-0",
					Type:    PartitionQuarter,
					Profile: "gpu-nvidia-com-8_rdma-mellanox-com-8",
					DeviceCounts: map[string]int{
						"gpu.nvidia.com":    2,
						"rdma.mellanox.com": 2,
					},
				},
				{
					Name:    "node-1-half-0",
					Type:    PartitionHalf,
					Profile: "gpu-nvidia-com-8_rdma-mellanox-com-8",
					DeviceCounts: map[string]int{
						"gpu.nvidia.com":    4,
						"rdma.mellanox.com": 4,
					},
				},
			},
		},
	}

	err := manager.SyncDeviceClasses(context.Background(), results)
	require.NoError(t, err)

	// Verify DeviceClasses were created
	classes, err := client.ResourceV1().DeviceClasses().List(context.Background(), metav1.ListOptions{})
	require.NoError(t, err)

	// Should have 2 classes: one quarter, one half
	assert.Len(t, classes.Items, 2)

	// Verify labels
	for _, dc := range classes.Items {
		assert.Equal(t, "true", dc.Labels[CoordinatorDriverName+"/managed"])
		assert.NotEmpty(t, dc.Labels[CoordinatorDriverName+"/partitionType"])
	}
}

func TestDeviceClassManager_DeviceClassContents(t *testing.T) {
	client := fake.NewSimpleClientset()
	rules := NewTopologyRuleStore()
	manager := NewDeviceClassManager(client, CoordinatorDriverName, rules)

	results := []PartitionResult{
		{
			NodeName: "node-1",
			Profile:  "test",
			Partitions: []PartitionDevice{
				{
					Name:    "node-1-half-0",
					Type:    PartitionHalf,
					Profile: "test",
					DeviceCounts: map[string]int{
						"gpu.nvidia.com":    4,
						"rdma.mellanox.com": 4,
					},
					Devices: func() []TopologyDevice {
						pcieRoot := "pci0000:00"
						var devs []TopologyDevice
						for i := 0; i < 4; i++ {
							devs = append(devs, TopologyDevice{DriverName: "gpu.nvidia.com", PCIeRoot: &pcieRoot})
						}
						for i := 0; i < 4; i++ {
							devs = append(devs, TopologyDevice{DriverName: "rdma.mellanox.com", PCIeRoot: &pcieRoot})
						}
						return devs
					}(),
				},
			},
		},
	}

	err := manager.SyncDeviceClasses(context.Background(), results)
	require.NoError(t, err)

	classes, err := client.ResourceV1().DeviceClasses().List(context.Background(), metav1.ListOptions{})
	require.NoError(t, err)
	require.Len(t, classes.Items, 1)

	dc := classes.Items[0]

	// Verify CEL selector
	require.Len(t, dc.Spec.Selectors, 1)
	require.NotNil(t, dc.Spec.Selectors[0].CEL)
	assert.Contains(t, dc.Spec.Selectors[0].CEL.Expression, "partitionType")
	assert.Contains(t, dc.Spec.Selectors[0].CEL.Expression, "half")

	// Verify opaque config
	require.Len(t, dc.Spec.Config, 1)
	require.NotNil(t, dc.Spec.Config[0].Opaque)
	assert.Equal(t, CoordinatorDriverName, dc.Spec.Config[0].Opaque.Driver)

	// Parse the opaque parameters
	var config PartitionConfig
	err = json.Unmarshal(dc.Spec.Config[0].Opaque.Parameters.Raw, &config)
	require.NoError(t, err)

	assert.Equal(t, "PartitionConfig", config.Kind)
	assert.Len(t, config.SubResources, 2, "should have 2 sub-resources (GPU + NIC)")

	// Verify sub-resources
	subResourceMap := make(map[string]int)
	for _, sr := range config.SubResources {
		subResourceMap[sr.DeviceClass] = sr.Count
	}
	assert.Equal(t, 4, subResourceMap["gpu.nvidia.com"])
	assert.Equal(t, 4, subResourceMap["rdma.mellanox.com"])

	// NUMA alignment is now handled by per-driver CEL selectors, not matchAttribute.
	// This partition has no NUMANodes set, so sub-resources should have fallback
	// CEL selectors using the standard dra.net/numaNode attribute.
	// No NUMA matchAttribute alignment should exist.
	for _, a := range config.Alignments {
		if a.Attribute == AttrNUMANode {
			t.Fatal("should NOT have NUMA matchAttribute — use per-driver CEL selectors instead")
		}
		if a.Attribute == AttrPCIeRoot {
			t.Fatal("should NOT have cross-driver PCIe alignment")
		}
	}
}

// TestDeviceClassManager_MixedPCIAndNonPCIDrivers verifies that pcieRoot alignment
// constraints are only emitted for PCI-based drivers. When a partition contains a
// mix of PCI devices (NICs, GPUs) and non-PCI devices (CPUs, memory), the pcieRoot
// constraint must exclude non-PCI drivers — they don't publish
// resource.kubernetes.io/pcieRoot, so including them makes the matchAttribute
// constraint unsatisfiable at scheduling time.
//
// This is the primary regression test for the fix. Without it, a quarter partition
// containing dra.cpu + SR-IOV NICs would produce a claim the scheduler rejects
// with "cannot allocate all claims" because dra.cpu devices lack pcieRoot.
//
// NUMA alignment (numaNode) should still include ALL drivers regardless of PCI
// status, since both PCI and non-PCI devices have NUMA affinity.
func TestDeviceClassManager_MixedPCIAndNonPCIDrivers(t *testing.T) {
	client := fake.NewSimpleClientset()
	rules := NewTopologyRuleStore()
	manager := NewDeviceClassManager(client, CoordinatorDriverName, rules)

	pcieRoot := "pci0000:15"
	results := []PartitionResult{
		{
			NodeName: "node-1",
			Profile:  "test",
			Partitions: []PartitionDevice{
				{
					Name:      "node-1-quarter-0",
					Type:      PartitionQuarter,
					Profile:   "test",
					NUMANodes: []int64{0},
					// Simulates a real-world partition: SR-IOV NICs (PCI) + CPUs (non-PCI)
					// on the same NUMA node, as seen on multi-NUMA servers like Dell XE9680.
					DeviceCounts: map[string]int{
						"sriovnetwork.k8snetworkplumbingwg.io": 2,
						"dra.cpu":                              1,
					},
					Devices: []TopologyDevice{
						// NIC VFs are PCI devices — they publish pcieRoot
						{DriverName: "sriovnetwork.k8snetworkplumbingwg.io", PCIeRoot: &pcieRoot},
						{DriverName: "sriovnetwork.k8snetworkplumbingwg.io", PCIeRoot: &pcieRoot},
						// CPUs are not PCI devices — PCIeRoot is nil
						{DriverName: "dra.cpu", PCIeRoot: nil},
					},
				},
			},
		},
	}

	err := manager.SyncDeviceClasses(context.Background(), results)
	require.NoError(t, err)

	classes, err := client.ResourceV1().DeviceClasses().List(context.Background(), metav1.ListOptions{})
	require.NoError(t, err)
	require.Len(t, classes.Items, 1)

	var config PartitionConfig
	err = json.Unmarshal(classes.Items[0].Spec.Config[0].Opaque.Parameters.Raw, &config)
	require.NoError(t, err)

	// NUMA alignment is now per-driver CEL selectors, not matchAttribute.
	// No NUMA or PCIe matchAttribute should exist.
	for _, a := range config.Alignments {
		if a.Attribute == AttrNUMANode {
			t.Fatal("should NOT have NUMA matchAttribute — use per-driver CEL selectors instead")
		}
		if a.Attribute == AttrPCIeRoot {
			t.Fatal("should NOT have PCIe alignment")
		}
	}

	// Each sub-resource should have a fallback NUMA CEL selector using dra.net/numaNode
	// (no topology rules configured, so fallback to standard attribute)
	for _, sr := range config.SubResources {
		assert.NotEmpty(t, sr.Selectors,
			"sub-resource %s should have NUMA CEL selector", sr.DeviceClass)
	}
}

func TestDeviceClassManager_WithMatchConstraintRules(t *testing.T) {
	client := fake.NewSimpleClientset()
	rules := NewTopologyRuleStore()

	// Add NVLink match constraint rule
	err := rules.LoadFromConfigMap(makeTopologyRuleConfigMap("nvlink", "default", map[string]string{
		"attribute":  "gpu.nvidia.com/nvlinkDomain",
		"type":       "int",
		"driver":     "gpu.nvidia.com",
		"constraint": "match",
	}))
	require.NoError(t, err)

	manager := NewDeviceClassManager(client, CoordinatorDriverName, rules)

	results := []PartitionResult{
		{
			NodeName: "node-1",
			Profile:  "test",
			Partitions: []PartitionDevice{
				{
					Name:    "node-1-half-0",
					Type:    PartitionHalf,
					Profile: "test",
					DeviceCounts: map[string]int{
						"gpu.nvidia.com": 4,
					},
				},
			},
		},
	}

	err = manager.SyncDeviceClasses(context.Background(), results)
	require.NoError(t, err)

	classes, err := client.ResourceV1().DeviceClasses().List(context.Background(), metav1.ListOptions{})
	require.NoError(t, err)
	require.Len(t, classes.Items, 1)

	var config PartitionConfig
	err = json.Unmarshal(classes.Items[0].Spec.Config[0].Opaque.Parameters.Raw, &config)
	require.NoError(t, err)

	// Verify NVLink match constraint is present
	hasNVLinkAlignment := false
	for _, a := range config.Alignments {
		if a.Attribute == "gpu.nvidia.com/nvlinkDomain" {
			hasNVLinkAlignment = true
		}
	}
	assert.True(t, hasNVLinkAlignment, "should have NVLink match constraint alignment")
}

func TestDeviceClassManager_EnforcementPropagation(t *testing.T) {
	client := fake.NewSimpleClientset()
	rules := NewTopologyRuleStore()

	// Add a preferred enforcement match constraint rule
	err := rules.LoadFromConfigMap(makeTopologyRuleConfigMap("preferred-numa", "default", map[string]string{
		"attribute":   "gpu.nvidia.com/numaNode",
		"type":        "int",
		"driver":      "gpu.nvidia.com",
		"constraint":  "match",
		"enforcement": "preferred",
	}))
	require.NoError(t, err)

	manager := NewDeviceClassManager(client, CoordinatorDriverName, rules)

	results := []PartitionResult{
		{
			NodeName: "node-1",
			Profile:  "test",
			Partitions: []PartitionDevice{
				{
					Name:    "node-1-half-0",
					Type:    PartitionHalf,
					Profile: "test",
					DeviceCounts: map[string]int{
						"gpu.nvidia.com": 4,
					},
				},
			},
		},
	}

	err = manager.SyncDeviceClasses(context.Background(), results)
	require.NoError(t, err)

	classes, err := client.ResourceV1().DeviceClasses().List(context.Background(), metav1.ListOptions{})
	require.NoError(t, err)
	require.Len(t, classes.Items, 1)

	var config PartitionConfig
	err = json.Unmarshal(classes.Items[0].Spec.Config[0].Opaque.Parameters.Raw, &config)
	require.NoError(t, err)

	// NUMA is now per-driver CEL, no matchAttribute.
	// But rule-based match constraints (like gpu.nvidia.com/numaNode with constraint=match)
	// should still be emitted as matchAttribute alignments with their enforcement mode.
	hasPreferred := false
	for _, a := range config.Alignments {
		if a.Attribute == AttrNUMANode {
			t.Fatal("should NOT have NUMA matchAttribute")
		}
		if a.Attribute == "gpu.nvidia.com/numaNode" {
			hasPreferred = true
			assert.Equal(t, EnforcementPreferred, a.Enforcement,
				"rule-based alignment should propagate preferred enforcement")
		}
	}
	assert.True(t, hasPreferred, "should have preferred enforcement alignment from rule")
}

func TestDeviceClassManager_PerDriverCELSelectors(t *testing.T) {
	client := fake.NewSimpleClientset()
	rules := NewTopologyRuleStore()

	// Configure topology rules for GPU and CPU drivers with different NUMA attribute names
	err := rules.LoadFromConfigMap(makeTopologyRuleConfigMap("gpu-numa", "default", map[string]string{
		"attribute": "gpu.amd.com/numaNode",
		"type":      "int",
		"driver":    "gpu.amd.com",
		"mapsTo":    "numaNode",
	}))
	require.NoError(t, err)

	err = rules.LoadFromConfigMap(makeTopologyRuleConfigMap("cpu-numa", "default", map[string]string{
		"attribute": "dra.cpu/numaNodeID",
		"type":      "int",
		"driver":    "dra.cpu",
		"mapsTo":    "numaNode",
	}))
	require.NoError(t, err)

	manager := NewDeviceClassManager(client, CoordinatorDriverName, rules)

	results := []PartitionResult{
		{
			NodeName: "node-1",
			Profile:  "test",
			Partitions: []PartitionDevice{
				{
					Name:      "node-1-quarter-0",
					Type:      PartitionQuarter,
					Profile:   "test",
					NUMANodes: []int64{0},
					DeviceCounts: map[string]int{
						"gpu.amd.com": 1,
						"dra.cpu":     1,
					},
				},
			},
		},
	}

	err = manager.SyncDeviceClasses(context.Background(), results)
	require.NoError(t, err)

	classes, err := client.ResourceV1().DeviceClasses().List(context.Background(), metav1.ListOptions{})
	require.NoError(t, err)
	require.Len(t, classes.Items, 1)

	var config PartitionConfig
	err = json.Unmarshal(classes.Items[0].Spec.Config[0].Opaque.Parameters.Raw, &config)
	require.NoError(t, err)

	// Each sub-resource should have a CEL selector using its driver's own attribute name
	selectorMap := make(map[string][]string)
	for _, sr := range config.SubResources {
		selectorMap[sr.DeviceClass] = sr.Selectors
	}

	// GPU should use gpu.amd.com/numaNode
	gpuSelectors := selectorMap["gpu.amd.com"]
	require.Len(t, gpuSelectors, 1)
	assert.Equal(t, `device.attributes["gpu.amd.com"].numaNode == 0`, gpuSelectors[0])

	// CPU should use dra.cpu/numaNodeID
	cpuSelectors := selectorMap["dra.cpu"]
	require.Len(t, cpuSelectors, 1)
	assert.Equal(t, `device.attributes["dra.cpu"].numaNodeID == 0`, cpuSelectors[0])

	// No NUMA matchAttribute alignment should exist
	for _, a := range config.Alignments {
		if a.Attribute == AttrNUMANode {
			t.Fatal("should NOT have NUMA matchAttribute — per-driver CEL selectors replace it")
		}
	}

	// DeviceClass name should include NUMA suffix
	assert.Contains(t, classes.Items[0].Name, "numa0")
}

func TestDeviceClassManager_DeviceClassName(t *testing.T) {
	manager := &DeviceClassManager{driverName: CoordinatorDriverName}

	tests := []struct {
		profile  string
		partType PartitionType
		want     string
	}{
		{"test-profile", PartitionHalf, "test-profile-half"},
		{"test-profile", PartitionQuarter, "test-profile-quarter"},
		{"UPPER_CASE", PartitionFull, "upper-case-full"},
		{"has spaces", PartitionEighth, "has-spaces-eighth"},
		{"", PartitionFull, "default-full"},
	}

	for _, tt := range tests {
		t.Run(tt.profile+"-"+string(tt.partType), func(t *testing.T) {
			got := manager.deviceClassName(tt.profile, tt.partType)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestDeviceClassManager_Update(t *testing.T) {
	client := fake.NewSimpleClientset()
	rules := NewTopologyRuleStore()
	manager := NewDeviceClassManager(client, CoordinatorDriverName, rules)

	results := []PartitionResult{
		{
			NodeName: "node-1",
			Profile:  "test",
			Partitions: []PartitionDevice{
				{
					Name:         "node-1-full-0",
					Type:         PartitionFull,
					Profile:      "test",
					DeviceCounts: map[string]int{"gpu.nvidia.com": 8},
				},
			},
		},
	}

	// First sync
	err := manager.SyncDeviceClasses(context.Background(), results)
	require.NoError(t, err)

	// Update device count
	results[0].Partitions[0].DeviceCounts["gpu.nvidia.com"] = 4

	// Second sync (update)
	err = manager.SyncDeviceClasses(context.Background(), results)
	require.NoError(t, err)

	// Should still have 1 class
	classes, err := client.ResourceV1().DeviceClasses().List(context.Background(), metav1.ListOptions{})
	require.NoError(t, err)
	assert.Len(t, classes.Items, 1)
}

func TestDeviceClassManager_CleansUpStaleClasses(t *testing.T) {
	client := fake.NewSimpleClientset()
	rules := NewTopologyRuleStore()
	manager := NewDeviceClassManager(client, CoordinatorDriverName, rules)

	// First sync: create classes for quarter and half
	results := []PartitionResult{
		{
			NodeName: "node-1",
			Profile:  "test",
			Partitions: []PartitionDevice{
				{Name: "p-quarter", Type: PartitionQuarter, Profile: "test", DeviceCounts: map[string]int{"gpu": 2}},
				{Name: "p-half", Type: PartitionHalf, Profile: "test", DeviceCounts: map[string]int{"gpu": 4}},
			},
		},
	}
	err := manager.SyncDeviceClasses(context.Background(), results)
	require.NoError(t, err)

	classes, _ := client.ResourceV1().DeviceClasses().List(context.Background(), metav1.ListOptions{})
	assert.Len(t, classes.Items, 2, "should have 2 DeviceClasses after first sync")

	// Second sync: only quarter remains (GPUs removed, no half partition anymore)
	results = []PartitionResult{
		{
			NodeName: "node-1",
			Profile:  "test",
			Partitions: []PartitionDevice{
				{Name: "p-quarter", Type: PartitionQuarter, Profile: "test", DeviceCounts: map[string]int{"gpu": 2}},
			},
		},
	}
	err = manager.SyncDeviceClasses(context.Background(), results)
	require.NoError(t, err)

	classes, _ = client.ResourceV1().DeviceClasses().List(context.Background(), metav1.ListOptions{})
	assert.Len(t, classes.Items, 1, "stale half DeviceClass should be cleaned up")
	assert.Contains(t, classes.Items[0].Name, "quarter")
}

func TestDeviceClassManager_FallbackCouplingTight(t *testing.T) {
	client := fake.NewSimpleClientset()
	rules := NewTopologyRuleStore()

	// Add a pcieRoot match rule with numaNode fallback
	err := rules.LoadFromConfigMap(makeTopologyRuleConfigMap("pcie-rule", "default", map[string]string{
		"attribute":         "resource.kubernetes.io/pcieRoot",
		"type":              "string",
		"driver":            "gpu.nvidia.com",
		"constraint":        "match",
		"fallbackAttribute": "numaNode",
	}))
	require.NoError(t, err)

	manager := NewDeviceClassManager(client, CoordinatorDriverName, rules)

	// Partition where GPU and NIC share a pcieRoot — tight coupling
	pcieRoot := "pci0000:15"
	results := []PartitionResult{
		{
			NodeName: "node-1",
			Profile:  "test",
			Partitions: []PartitionDevice{
				{
					Name:      "node-1-eighth-0",
					Type:      PartitionEighth,
					Profile:   "test",
					NUMANodes: []int64{0},
					DeviceCounts: map[string]int{
						"gpu.nvidia.com":    1,
						"rdma.mellanox.com": 1,
					},
					Devices: []TopologyDevice{
						{DriverName: "gpu.nvidia.com", PCIeRoot: &pcieRoot},
						{DriverName: "rdma.mellanox.com", PCIeRoot: &pcieRoot},
					},
				},
			},
		},
	}

	err = manager.SyncDeviceClasses(context.Background(), results)
	require.NoError(t, err)

	classes, err := client.ResourceV1().DeviceClasses().List(context.Background(), metav1.ListOptions{})
	require.NoError(t, err)
	require.Len(t, classes.Items, 1)

	// Should have tight coupling label
	assert.Equal(t, "tight", classes.Items[0].Labels[CoordinatorDriverName+"/coupling"])

	// Should have pcieRoot matchAttribute alignment
	var config PartitionConfig
	err = json.Unmarshal(classes.Items[0].Spec.Config[0].Opaque.Parameters.Raw, &config)
	require.NoError(t, err)

	hasPCIeAlignment := false
	for _, a := range config.Alignments {
		if a.Attribute == "resource.kubernetes.io/pcieRoot" {
			hasPCIeAlignment = true
		}
	}
	assert.True(t, hasPCIeAlignment, "tight partition should have pcieRoot alignment")
}

func TestDeviceClassManager_FallbackCouplingLoose(t *testing.T) {
	client := fake.NewSimpleClientset()
	rules := NewTopologyRuleStore()

	err := rules.LoadFromConfigMap(makeTopologyRuleConfigMap("pcie-rule", "default", map[string]string{
		"attribute":         "resource.kubernetes.io/pcieRoot",
		"type":              "string",
		"driver":            "gpu.nvidia.com",
		"constraint":        "match",
		"fallbackAttribute": "numaNode",
	}))
	require.NoError(t, err)

	manager := NewDeviceClassManager(client, CoordinatorDriverName, rules)

	// Partition where GPU and NIC have DIFFERENT pcieRoots — loose coupling
	gpuRoot := "pci0000:37"
	nicRoot := "pci0000:15"
	results := []PartitionResult{
		{
			NodeName: "node-1",
			Profile:  "test",
			Partitions: []PartitionDevice{
				{
					Name:      "node-1-eighth-1",
					Type:      PartitionEighth,
					Profile:   "test",
					NUMANodes: []int64{0},
					DeviceCounts: map[string]int{
						"gpu.nvidia.com":    1,
						"rdma.mellanox.com": 1,
					},
					Devices: []TopologyDevice{
						{DriverName: "gpu.nvidia.com", PCIeRoot: &gpuRoot},
						{DriverName: "rdma.mellanox.com", PCIeRoot: &nicRoot},
					},
				},
			},
		},
	}

	err = manager.SyncDeviceClasses(context.Background(), results)
	require.NoError(t, err)

	classes, err := client.ResourceV1().DeviceClasses().List(context.Background(), metav1.ListOptions{})
	require.NoError(t, err)
	require.Len(t, classes.Items, 1)

	// Should have loose coupling label
	assert.Equal(t, "loose", classes.Items[0].Labels[CoordinatorDriverName+"/coupling"])

	// Should NOT have pcieRoot matchAttribute alignment
	var config PartitionConfig
	err = json.Unmarshal(classes.Items[0].Spec.Config[0].Opaque.Parameters.Raw, &config)
	require.NoError(t, err)

	for _, a := range config.Alignments {
		if a.Attribute == "resource.kubernetes.io/pcieRoot" {
			t.Fatal("loose partition should NOT have pcieRoot alignment")
		}
	}
}

func TestDeviceClassManager_FallbackMixedCoupling(t *testing.T) {
	client := fake.NewSimpleClientset()
	rules := NewTopologyRuleStore()

	err := rules.LoadFromConfigMap(makeTopologyRuleConfigMap("pcie-rule", "default", map[string]string{
		"attribute":         "resource.kubernetes.io/pcieRoot",
		"type":              "string",
		"driver":            "gpu.nvidia.com",
		"constraint":        "match",
		"fallbackAttribute": "numaNode",
	}))
	require.NoError(t, err)

	manager := NewDeviceClassManager(client, CoordinatorDriverName, rules)

	sharedRoot := "pci0000:15"
	gpuOnlyRoot := "pci0000:37"
	nicRoot := "pci0000:15"
	results := []PartitionResult{
		{
			NodeName: "node-1",
			Profile:  "test",
			Partitions: []PartitionDevice{
				{
					Name:      "node-1-eighth-0",
					Type:      PartitionEighth,
					Profile:   "test",
					NUMANodes: []int64{0},
					DeviceCounts: map[string]int{
						"gpu.nvidia.com":    1,
						"rdma.mellanox.com": 1,
					},
					Devices: []TopologyDevice{
						{DriverName: "gpu.nvidia.com", PCIeRoot: &sharedRoot},
						{DriverName: "rdma.mellanox.com", PCIeRoot: &nicRoot},
					},
				},
				{
					Name:      "node-1-eighth-1",
					Type:      PartitionEighth,
					Profile:   "test",
					NUMANodes: []int64{0},
					DeviceCounts: map[string]int{
						"gpu.nvidia.com":    1,
						"rdma.mellanox.com": 1,
					},
					Devices: []TopologyDevice{
						{DriverName: "gpu.nvidia.com", PCIeRoot: &gpuOnlyRoot},
						{DriverName: "rdma.mellanox.com", PCIeRoot: &nicRoot},
					},
				},
			},
		},
	}

	err = manager.SyncDeviceClasses(context.Background(), results)
	require.NoError(t, err)

	classes, err := client.ResourceV1().DeviceClasses().List(context.Background(), metav1.ListOptions{})
	require.NoError(t, err)

	// Should have 2 DeviceClasses: one tight, one loose
	assert.Len(t, classes.Items, 2, "mixed coupling should produce 2 DeviceClasses")

	couplings := map[string]bool{}
	for _, dc := range classes.Items {
		couplings[dc.Labels[CoordinatorDriverName+"/coupling"]] = true
	}
	assert.True(t, couplings["tight"], "should have a tight DeviceClass")
	assert.True(t, couplings["loose"], "should have a loose DeviceClass")
}

func TestDeviceClassManager_NoFallbackAttribute(t *testing.T) {
	client := fake.NewSimpleClientset()
	rules := NewTopologyRuleStore()

	// Rule WITHOUT fallbackAttribute — should behave as before
	err := rules.LoadFromConfigMap(makeTopologyRuleConfigMap("nvlink", "default", map[string]string{
		"attribute":  "gpu.nvidia.com/nvlinkDomain",
		"type":       "int",
		"driver":     "gpu.nvidia.com",
		"constraint": "match",
	}))
	require.NoError(t, err)

	manager := NewDeviceClassManager(client, CoordinatorDriverName, rules)

	results := []PartitionResult{
		{
			NodeName: "node-1",
			Profile:  "test",
			Partitions: []PartitionDevice{
				{
					Name:    "node-1-half-0",
					Type:    PartitionHalf,
					Profile: "test",
					DeviceCounts: map[string]int{
						"gpu.nvidia.com": 4,
					},
				},
			},
		},
	}

	err = manager.SyncDeviceClasses(context.Background(), results)
	require.NoError(t, err)

	classes, err := client.ResourceV1().DeviceClasses().List(context.Background(), metav1.ListOptions{})
	require.NoError(t, err)
	require.Len(t, classes.Items, 1)

	// No coupling label when no fallback
	_, hasCoupling := classes.Items[0].Labels[CoordinatorDriverName+"/coupling"]
	assert.False(t, hasCoupling, "should NOT have coupling label without fallbackAttribute")

	// Should still have the matchAttribute alignment
	var config PartitionConfig
	err = json.Unmarshal(classes.Items[0].Spec.Config[0].Opaque.Parameters.Raw, &config)
	require.NoError(t, err)

	hasNVLink := false
	for _, a := range config.Alignments {
		if a.Attribute == "gpu.nvidia.com/nvlinkDomain" {
			hasNVLink = true
		}
	}
	assert.True(t, hasNVLink, "should have NVLink alignment without fallback")
}
