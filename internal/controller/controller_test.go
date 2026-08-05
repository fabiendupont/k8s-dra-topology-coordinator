package controller

import (
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestMergeGroupings_UserOverridesAuto(t *testing.T) {
	auto := []DeviceGrouping{
		{Name: "gpu-nic-pair", Alignment: "pcieRoot", Devices: []GroupingDevice{{Class: "gpu.auto", Count: 1}}},
	}
	user := []DeviceGrouping{
		{Name: "gpu-nic-pair", Alignment: "numaNode", Devices: []GroupingDevice{{Class: "gpu.user", Count: 2}}},
	}

	merged := mergeGroupings(auto, user)
	assert.Len(t, merged, 1)
	assert.Equal(t, "numaNode", merged[0].Alignment, "user-defined should override auto-detected")
	assert.Equal(t, "gpu.user", merged[0].Devices[0].Class)
}

func TestMergeGroupings_BothEmpty(t *testing.T) {
	merged := mergeGroupings(nil, nil)
	assert.Empty(t, merged)
}

func TestMergeGroupings_NoOverlap(t *testing.T) {
	auto := []DeviceGrouping{
		{Name: "auto-pair", Alignment: "pcieRoot"},
	}
	user := []DeviceGrouping{
		{Name: "user-pair", Alignment: "numaNode"},
	}

	merged := mergeGroupings(auto, user)
	assert.Len(t, merged, 2)

	names := make([]string, len(merged))
	for i, g := range merged {
		names[i] = g.Name
	}
	sort.Strings(names)
	assert.Equal(t, []string{"auto-pair", "user-pair"}, names)
}

func TestMergeGroupings_AutoOnly(t *testing.T) {
	auto := []DeviceGrouping{
		{Name: "pair-a"},
		{Name: "pair-b"},
	}
	merged := mergeGroupings(auto, nil)
	assert.Len(t, merged, 2)
}

func TestMergeGroupings_UserOnly(t *testing.T) {
	user := []DeviceGrouping{
		{Name: "pair-a"},
	}
	merged := mergeGroupings(nil, user)
	assert.Len(t, merged, 1)
}
