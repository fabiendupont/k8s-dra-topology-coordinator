package controller

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func makeTopologyRuleConfigMap(name, namespace string, data map[string]string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels: map[string]string{
				TopologyRuleLabel: "true",
			},
		},
		Data: data,
	}
}

func TestTopologyRuleStore_LoadFromConfigMap(t *testing.T) {
	store := NewTopologyRuleStore()

	cm := makeTopologyRuleConfigMap("nvlink-rule", "default", map[string]string{
		"attribute":    "gpu.nvidia.com/nvlinkDomain",
		"type":         "int",
		"driver":       "gpu.nvidia.com",
		"partitioning": "group",
		"constraint":   "match",
		"description":  "NVLink domain for GPU-GPU interconnect",
	})

	err := store.LoadFromConfigMap(cm)
	require.NoError(t, err)

	rules := store.GetRules()
	require.Len(t, rules, 1)

	rule := rules[0]
	assert.Equal(t, "nvlink-rule", rule.Name)
	assert.Equal(t, "gpu.nvidia.com/nvlinkDomain", rule.Attribute)
	assert.Equal(t, "int", rule.Type)
	assert.Equal(t, "gpu.nvidia.com", rule.Driver)
	assert.Equal(t, PartitioningGroup, rule.Partitioning)
	assert.Equal(t, ConstraintMatch, rule.Constraint)
	assert.Equal(t, "NVLink domain for GPU-GPU interconnect", rule.Description)
}

func TestTopologyRuleStore_LoadFromConfigMap_Defaults(t *testing.T) {
	store := NewTopologyRuleStore()

	cm := makeTopologyRuleConfigMap("minimal-rule", "default", map[string]string{
		"attribute": "example.com/topology",
		"type":      "string",
		"driver":    "example.com",
		// partitioning and constraint omitted — should default
	})

	err := store.LoadFromConfigMap(cm)
	require.NoError(t, err)

	rules := store.GetRules()
	require.Len(t, rules, 1)

	assert.Equal(t, PartitioningInfo, rules[0].Partitioning, "default partitioning should be info")
	assert.Equal(t, ConstraintNone, rules[0].Constraint, "default constraint should be none")
}

func TestTopologyRuleStore_LoadFromConfigMap_MissingFields(t *testing.T) {
	store := NewTopologyRuleStore()

	tests := []struct {
		name string
		data map[string]string
	}{
		{
			name: "missing attribute",
			data: map[string]string{"type": "int", "driver": "test"},
		},
		{
			name: "missing type",
			data: map[string]string{"attribute": "test/attr", "driver": "test"},
		},
		{
			name: "missing driver",
			data: map[string]string{"attribute": "test/attr", "type": "int"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cm := makeTopologyRuleConfigMap("rule", "default", tt.data)
			err := store.LoadFromConfigMap(cm)
			assert.Error(t, err)
		})
	}
}

func TestTopologyRuleStore_LoadFromConfigMap_InvalidType(t *testing.T) {
	store := NewTopologyRuleStore()

	cm := makeTopologyRuleConfigMap("bad-type", "default", map[string]string{
		"attribute": "test/attr",
		"type":      "float",
		"driver":    "test",
	})

	err := store.LoadFromConfigMap(cm)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "invalid type")
}

func TestTopologyRuleStore_LoadFromConfigMap_InvalidPartitioning(t *testing.T) {
	store := NewTopologyRuleStore()

	cm := makeTopologyRuleConfigMap("bad-part", "default", map[string]string{
		"attribute":    "test/attr",
		"type":         "int",
		"driver":       "test",
		"partitioning": "invalid",
	})

	err := store.LoadFromConfigMap(cm)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "invalid partitioning mode")
}

func TestTopologyRuleStore_LoadFromConfigMap_InvalidConstraint(t *testing.T) {
	store := NewTopologyRuleStore()

	cm := makeTopologyRuleConfigMap("bad-constraint", "default", map[string]string{
		"attribute":  "test/attr",
		"type":       "int",
		"driver":     "test",
		"constraint": "invalid",
	})

	err := store.LoadFromConfigMap(cm)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "invalid constraint mode")
}

func TestTopologyRuleStore_LoadFromConfigMap_MissingLabel(t *testing.T) {
	store := NewTopologyRuleStore()

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "no-label",
			Namespace: "default",
			// No topology rule label
		},
		Data: map[string]string{
			"attribute": "test/attr",
			"type":      "int",
			"driver":    "test",
		},
	}

	err := store.LoadFromConfigMap(cm)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "not a topology rule")
}

func TestTopologyRuleStore_RemoveConfigMap(t *testing.T) {
	store := NewTopologyRuleStore()

	cm := makeTopologyRuleConfigMap("nvlink-rule", "default", map[string]string{
		"attribute": "gpu.nvidia.com/nvlinkDomain",
		"type":      "int",
		"driver":    "gpu.nvidia.com",
	})

	err := store.LoadFromConfigMap(cm)
	require.NoError(t, err)
	assert.Len(t, store.GetRules(), 1)

	store.RemoveConfigMap("default", "nvlink-rule")
	assert.Len(t, store.GetRules(), 0)
}

func TestTopologyRuleStore_GetGroupingRules(t *testing.T) {
	store := NewTopologyRuleStore()

	// Add a grouping rule
	err := store.LoadFromConfigMap(makeTopologyRuleConfigMap("group-rule", "default", map[string]string{
		"attribute":    "gpu.nvidia.com/nvlinkDomain",
		"type":         "int",
		"driver":       "gpu.nvidia.com",
		"partitioning": "group",
	}))
	require.NoError(t, err)

	// Add an info rule
	err = store.LoadFromConfigMap(makeTopologyRuleConfigMap("info-rule", "default", map[string]string{
		"attribute":    "example.com/info",
		"type":         "string",
		"driver":       "example.com",
		"partitioning": "info",
	}))
	require.NoError(t, err)

	grouping := store.GetGroupingRules()
	assert.Len(t, grouping, 1)
	assert.Equal(t, "gpu.nvidia.com/nvlinkDomain", grouping[0].Attribute)
}

func TestTopologyRuleStore_GetMatchConstraintRules(t *testing.T) {
	store := NewTopologyRuleStore()

	// Add a match rule
	err := store.LoadFromConfigMap(makeTopologyRuleConfigMap("match-rule", "default", map[string]string{
		"attribute":  "gpu.nvidia.com/nvlinkDomain",
		"type":       "int",
		"driver":     "gpu.nvidia.com",
		"constraint": "match",
	}))
	require.NoError(t, err)

	// Add a no-constraint rule
	err = store.LoadFromConfigMap(makeTopologyRuleConfigMap("no-constraint", "default", map[string]string{
		"attribute":  "example.com/info",
		"type":       "string",
		"driver":     "example.com",
		"constraint": "none",
	}))
	require.NoError(t, err)

	match := store.GetMatchConstraintRules()
	assert.Len(t, match, 1)
	assert.Equal(t, "gpu.nvidia.com/nvlinkDomain", match[0].Attribute)
}

func TestTopologyRuleStore_NilConfigMap(t *testing.T) {
	store := NewTopologyRuleStore()
	err := store.LoadFromConfigMap(nil)
	assert.Error(t, err)
}

func TestTopologyRuleStore_MultipleRules(t *testing.T) {
	store := NewTopologyRuleStore()

	err := store.LoadFromConfigMap(makeTopologyRuleConfigMap("rule-1", "ns-a", map[string]string{
		"attribute": "a.com/attr",
		"type":      "int",
		"driver":    "a.com",
	}))
	require.NoError(t, err)

	err = store.LoadFromConfigMap(makeTopologyRuleConfigMap("rule-2", "ns-b", map[string]string{
		"attribute": "b.com/attr",
		"type":      "string",
		"driver":    "b.com",
	}))
	require.NoError(t, err)

	assert.Len(t, store.GetRules(), 2)
}

func TestTopologyRuleStore_LoadFromConfigMap_MapsTo(t *testing.T) {
	store := NewTopologyRuleStore()

	cm := makeTopologyRuleConfigMap("numa-mapping", "default", map[string]string{
		"attribute": "mock-accel.example.com/numaNode",
		"type":      "int",
		"driver":    "mock-accel.example.com",
		"mapsTo":    "numaNode",
	})

	err := store.LoadFromConfigMap(cm)
	require.NoError(t, err)

	rules := store.GetRules()
	require.Len(t, rules, 1)
	assert.Equal(t, MapsToNUMANode, rules[0].MapsTo)
}

func TestTopologyRuleStore_LoadFromConfigMap_MapsTo_PCIeRoot(t *testing.T) {
	store := NewTopologyRuleStore()

	cm := makeTopologyRuleConfigMap("pcie-mapping", "default", map[string]string{
		"attribute": "mock-accel.example.com/pciAddress",
		"type":      "string",
		"driver":    "mock-accel.example.com",
		"mapsTo":    "pcieRoot",
	})

	err := store.LoadFromConfigMap(cm)
	require.NoError(t, err)

	rules := store.GetRules()
	require.Len(t, rules, 1)
	assert.Equal(t, MapsToPCIeRoot, rules[0].MapsTo)
}

func TestTopologyRuleStore_LoadFromConfigMap_Enforcement(t *testing.T) {
	store := NewTopologyRuleStore()

	cm := makeTopologyRuleConfigMap("preferred-rule", "default", map[string]string{
		"attribute":   "mock-accel.example.com/numaNode",
		"type":        "int",
		"driver":      "mock-accel.example.com",
		"constraint":  "match",
		"enforcement": "preferred",
	})

	err := store.LoadFromConfigMap(cm)
	require.NoError(t, err)

	rules := store.GetRules()
	require.Len(t, rules, 1)
	assert.Equal(t, EnforcementPreferred, rules[0].Enforcement)
}

func TestTopologyRuleStore_LoadFromConfigMap_EnforcementDefault(t *testing.T) {
	store := NewTopologyRuleStore()

	cm := makeTopologyRuleConfigMap("no-enforcement", "default", map[string]string{
		"attribute": "test/attr",
		"type":      "int",
		"driver":    "test",
		// enforcement omitted — should default to required
	})

	err := store.LoadFromConfigMap(cm)
	require.NoError(t, err)

	rules := store.GetRules()
	require.Len(t, rules, 1)
	assert.Equal(t, EnforcementRequired, rules[0].Enforcement, "default enforcement should be required")
}

func TestTopologyRuleStore_LoadFromConfigMap_InvalidEnforcement(t *testing.T) {
	store := NewTopologyRuleStore()

	cm := makeTopologyRuleConfigMap("bad-enforcement", "default", map[string]string{
		"attribute":   "test/attr",
		"type":        "int",
		"driver":      "test",
		"enforcement": "invalid",
	})

	err := store.LoadFromConfigMap(cm)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "invalid enforcement mode")
}

func TestTopologyRuleStore_LoadFromConfigMap_InvalidMapsTo(t *testing.T) {
	store := NewTopologyRuleStore()

	cm := makeTopologyRuleConfigMap("bad-mapping", "default", map[string]string{
		"attribute": "test/attr",
		"type":      "int",
		"driver":    "test",
		"mapsTo":    "invalid",
	})

	err := store.LoadFromConfigMap(cm)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "invalid mapsTo")
}
