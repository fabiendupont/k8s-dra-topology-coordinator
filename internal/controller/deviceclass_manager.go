package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	resourcev1 "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	klog "k8s.io/klog/v2"
)

const (
	// CoordinatorDriverName is the identifier used in DeviceClass labels
	// and opaque configuration for the topology coordinator.
	CoordinatorDriverName = "nodepartition.dra.k8s.io"
)

// PartitionConfig is the opaque configuration embedded in DeviceClass config.
// It tells the webhook how to expand a partition claim into sub-resource requests.
type PartitionConfig struct {
	Kind         string              `json:"kind"`
	SubResources []SubResourceConfig `json:"subResources"`
	Alignments   []AlignmentConfig   `json:"alignments"`
}

// SubResourceConfig defines a sub-resource that a partition contains.
type SubResourceConfig struct {
	DeviceClass string `json:"deviceClass"`
	Count       int    `json:"count"`
}

// AlignmentConfig defines a matchAttribute constraint for the combined claim.
type AlignmentConfig struct {
	Attribute   string          `json:"attribute"`
	Requests    []string        `json:"requests"`
	Enforcement EnforcementMode `json:"enforcement"`
}

// DeviceClassManager creates and manages DeviceClass objects based on discovered partition types.
type DeviceClassManager struct {
	client     kubernetes.Interface
	driverName string
	rules      *TopologyRuleStore
}

// NewDeviceClassManager creates a new DeviceClassManager.
func NewDeviceClassManager(client kubernetes.Interface, driverName string, rules *TopologyRuleStore) *DeviceClassManager {
	if driverName == "" {
		driverName = CoordinatorDriverName
	}
	return &DeviceClassManager{
		client:     client,
		driverName: driverName,
		rules:      rules,
	}
}

// SyncDeviceClasses creates or updates DeviceClass objects for each partition type
// discovered across all nodes.
func (m *DeviceClassManager) SyncDeviceClasses(ctx context.Context, results []PartitionResult) error {
	// Collect all unique partition types and their profiles
	type profilePartition struct {
		profile  string
		partType PartitionType
		// Representative partition for computing sub-resource counts
		representative PartitionDevice
	}

	seen := make(map[string]*profilePartition)
	for _, result := range results {
		for _, partition := range result.Partitions {
			key := result.Profile + "-" + string(partition.Type)
			if _, ok := seen[key]; !ok {
				seen[key] = &profilePartition{
					profile:        result.Profile,
					partType:       partition.Type,
					representative: partition,
				}
			}
		}
	}

	// Create/update a DeviceClass for each profile+partitionType
	for _, pp := range seen {
		dc := m.buildDeviceClass(pp.profile, pp.partType, pp.representative)
		if err := m.publishDeviceClass(ctx, dc); err != nil {
			return fmt.Errorf("failed to publish DeviceClass %s: %w", dc.Name, err)
		}
	}

	// Clean up stale DeviceClasses no longer matching any partition
	activeKeys := make(map[string]bool, len(seen))
	for key := range seen {
		activeKeys[key] = true
	}
	if err := m.cleanupStaleDeviceClasses(ctx, activeKeys); err != nil {
		klog.Errorf("Failed to cleanup stale DeviceClasses: %v", err)
	}

	klog.Infof("Synced %d DeviceClasses", len(seen))
	return nil
}

// cleanupStaleDeviceClasses removes DeviceClasses that no longer match any partition.
func (m *DeviceClassManager) cleanupStaleDeviceClasses(ctx context.Context, active map[string]bool) error {
	labelSelector := fmt.Sprintf("%s/managed=true", CoordinatorDriverName)
	classes, err := m.client.ResourceV1().DeviceClasses().List(ctx, metav1.ListOptions{
		LabelSelector: labelSelector,
	})
	if err != nil {
		return fmt.Errorf("failed to list DeviceClasses: %w", err)
	}

	for _, dc := range classes.Items {
		profile := dc.Labels[CoordinatorDriverName+"/profile"]
		partType := dc.Labels[CoordinatorDriverName+"/partitionType"]
		key := profile + "-" + partType
		if _, exists := active[key]; !exists {
			if err := m.client.ResourceV1().DeviceClasses().Delete(ctx, dc.Name, metav1.DeleteOptions{}); err != nil {
				if !errors.IsNotFound(err) {
					klog.Errorf("Failed to delete stale DeviceClass %s: %v", dc.Name, err)
				}
			} else {
				klog.Infof("Deleted stale DeviceClass %s", dc.Name)
			}
		}
	}
	return nil
}

// buildDeviceClass constructs a DeviceClass for a given profile and partition type.
func (m *DeviceClassManager) buildDeviceClass(profile string, partType PartitionType, representative PartitionDevice) *resourcev1.DeviceClass {
	name := m.deviceClassName(profile, partType)

	// Build the CEL selector
	celExpr := fmt.Sprintf(
		`device.driver == %q && device.attributes[%q].partitionType == %q`,
		m.driverName, m.driverName, string(partType),
	)

	// Build the opaque config with sub-resource definitions
	config := m.buildPartitionConfig(partType, representative)
	configJSON, err := json.Marshal(config)
	if err != nil {
		klog.Errorf("Failed to marshal partition config: %v", err)
		configJSON = []byte("{}")
	}

	return &resourcev1.DeviceClass{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
			Labels: map[string]string{
				CoordinatorDriverName + "/managed":       "true",
				CoordinatorDriverName + "/profile":       profile,
				CoordinatorDriverName + "/partitionType": string(partType),
			},
		},
		Spec: resourcev1.DeviceClassSpec{
			Selectors: []resourcev1.DeviceSelector{
				{
					CEL: &resourcev1.CELDeviceSelector{
						Expression: celExpr,
					},
				},
			},
			Config: []resourcev1.DeviceClassConfiguration{
				{
					DeviceConfiguration: resourcev1.DeviceConfiguration{
						Opaque: &resourcev1.OpaqueDeviceConfiguration{
							Driver:     m.driverName,
							Parameters: runtime.RawExtension{Raw: configJSON},
						},
					},
				},
			},
		},
	}
}

// buildPartitionConfig builds the opaque PartitionConfig from the representative partition.
func (m *DeviceClassManager) buildPartitionConfig(_ PartitionType, representative PartitionDevice) PartitionConfig {
	config := PartitionConfig{
		Kind: "PartitionConfig",
	}

	// Sub-resources: one entry per driver with its device count
	for driver, count := range representative.DeviceCounts {
		config.SubResources = append(config.SubResources, SubResourceConfig{
			DeviceClass: driver,
			Count:       count,
		})
	}

	// Standard alignments
	requestNames := []string{"partition"}
	for driver := range representative.DeviceCounts {
		requestNames = append(requestNames, driver)
	}

	// NUMA alignment between partition and all sub-resources
	config.Alignments = append(config.Alignments, AlignmentConfig{
		Attribute:   AttrNUMANode,
		Requests:    requestNames,
		Enforcement: EnforcementRequired,
	})

	// PCIe alignment between PCI sub-resources only.
	// Non-PCI drivers (e.g., dra.cpu) don't publish pcieRoot and must be excluded,
	// otherwise the matchAttribute constraint is unsatisfiable.
	// Alignment is only needed when 2+ PCI drivers exist to align.
	if len(representative.DeviceCounts) > 1 {
		pciDrivers := make(map[string]bool)
		for _, dev := range representative.Devices {
			if dev.PCIeRoot != nil {
				pciDrivers[dev.DriverName] = true
			}
		}

		if len(pciDrivers) > 1 {
			subResourceNames := make([]string, 0, len(pciDrivers))
			for driver := range pciDrivers {
				subResourceNames = append(subResourceNames, driver)
			}
			config.Alignments = append(config.Alignments, AlignmentConfig{
				Attribute:   AttrPCIeRoot,
				Requests:    subResourceNames,
				Enforcement: EnforcementRequired,
			})
		}
	}

	// Match constraint alignments from topology rules
	matchRules := m.rules.GetMatchConstraintRules()
	for _, rule := range matchRules {
		// Only add match constraint if the representative has devices from this driver
		if _, ok := representative.DeviceCounts[rule.Driver]; ok {
			enforcement := rule.Enforcement
			if enforcement == "" {
				enforcement = EnforcementRequired
			}
			config.Alignments = append(config.Alignments, AlignmentConfig{
				Attribute:   rule.Attribute,
				Requests:    requestNames,
				Enforcement: enforcement,
			})
		}
	}

	return config
}

// deviceClassName generates a deterministic DeviceClass name.
func (m *DeviceClassManager) deviceClassName(profile string, partType PartitionType) string {
	// Sanitize profile for DNS label compatibility
	sanitized := strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			return r
		}
		if r >= 'A' && r <= 'Z' {
			return r + ('a' - 'A')
		}
		return '-'
	}, profile)

	// Trim leading/trailing dashes and collapse consecutive dashes
	for strings.Contains(sanitized, "--") {
		sanitized = strings.ReplaceAll(sanitized, "--", "-")
	}
	sanitized = strings.Trim(sanitized, "-")

	if sanitized == "" {
		sanitized = "default"
	}

	// Truncate to fit within DNS label limits (63 chars max)
	name := sanitized + "-" + string(partType)
	if len(name) > 63 {
		name = name[:63]
	}

	return name
}

// publishDeviceClass creates or updates a DeviceClass.
func (m *DeviceClassManager) publishDeviceClass(ctx context.Context, dc *resourcev1.DeviceClass) error {
	existing, err := m.client.ResourceV1().DeviceClasses().Get(ctx, dc.Name, metav1.GetOptions{})
	if errors.IsNotFound(err) {
		_, err = m.client.ResourceV1().DeviceClasses().Create(ctx, dc, metav1.CreateOptions{})
		if err != nil {
			return fmt.Errorf("failed to create DeviceClass %s: %w", dc.Name, err)
		}
		klog.Infof("Created DeviceClass %s", dc.Name)
		return nil
	}
	if err != nil {
		return fmt.Errorf("failed to get DeviceClass %s: %w", dc.Name, err)
	}

	// Update existing
	dc.ResourceVersion = existing.ResourceVersion
	_, err = m.client.ResourceV1().DeviceClasses().Update(ctx, dc, metav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("failed to update DeviceClass %s: %w", dc.Name, err)
	}
	klog.Infof("Updated DeviceClass %s", dc.Name)
	return nil
}
