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

	// Verify alignments include NUMA
	hasNUMAAlignment := false
	for _, a := range config.Alignments {
		if a.Attribute == AttrNUMANode {
			hasNUMAAlignment = true
			assert.Contains(t, a.Requests, "partition")
		}
	}
	assert.True(t, hasNUMAAlignment, "should have NUMA alignment")

	// Verify PCIe alignment between sub-resources
	hasPCIeAlignment := false
	for _, a := range config.Alignments {
		if a.Attribute == AttrPCIeRoot {
			hasPCIeAlignment = true
		}
	}
	assert.True(t, hasPCIeAlignment, "should have PCIe alignment between sub-resources")
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
