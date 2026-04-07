package controller

import (
	"fmt"
	"sync"

	corev1 "k8s.io/api/core/v1"
	klog "k8s.io/klog/v2"
)

// TopologyRuleLabel is the label that identifies ConfigMaps containing topology rules.
const TopologyRuleLabel = "nodepartition.dra.k8s.io/topology-rule"

// PartitioningMode defines how the coordinator uses a topology attribute during partition computation.
type PartitioningMode string

const (
	// PartitioningGroup means devices must share the same value of this attribute
	// to be grouped into a partition.
	PartitioningGroup PartitioningMode = "group"
	// PartitioningInfo means the attribute is included in partition device attributes
	// but does not constrain grouping.
	PartitioningInfo PartitioningMode = "info"
)

// ConstraintMode defines how the attribute is used in combined claims.
type ConstraintMode string

const (
	// ConstraintMatch means a matchAttribute constraint is added to the combined claim.
	ConstraintMatch ConstraintMode = "match"
	// ConstraintNone means no constraint is added.
	ConstraintNone ConstraintMode = "none"
)

// EnforcementMode defines whether a constraint is hard (must be satisfied) or
// best-effort (applied only when the cluster can satisfy it).
type EnforcementMode string

const (
	// EnforcementRequired means the constraint is always emitted. If it cannot
	// be satisfied, the pod will not schedule.
	EnforcementRequired EnforcementMode = "required"
	// EnforcementPreferred means the constraint is emitted only when the
	// topology model indicates it can be satisfied on at least one node.
	EnforcementPreferred EnforcementMode = "preferred"
)

// StandardTopologyAttribute identifies a standard topology attribute for mapping.
type StandardTopologyAttribute string

const (
	MapsToNUMANode StandardTopologyAttribute = "numaNode"
	MapsToPCIeRoot StandardTopologyAttribute = "pcieRoot"
	MapsToSocket   StandardTopologyAttribute = "socket"
	MapsToNone     StandardTopologyAttribute = ""
)

// TopologyRule represents a vendor-specific topology attribute rule loaded from a ConfigMap.
type TopologyRule struct {
	// Name is the ConfigMap name this rule was loaded from.
	Name string
	// Attribute is the fully qualified attribute name (e.g., "gpu.nvidia.com/nvlinkDomain").
	Attribute string
	// Type is the attribute value type: "int", "string", or "bool".
	Type string
	// Driver is the DRA driver that publishes this attribute.
	Driver string
	// MapsTo optionally maps this driver-specific attribute to a standard topology attribute.
	// When set, the coordinator treats this attribute as the specified standard attribute
	// (numaNode, pcieRoot, socket) for topology grouping purposes.
	MapsTo StandardTopologyAttribute
	// Partitioning defines how the coordinator uses this attribute during partition computation.
	Partitioning PartitioningMode
	// Constraint defines how the attribute is used in combined claims.
	Constraint ConstraintMode
	// Enforcement defines whether the constraint is hard or best-effort.
	// "required" (default) always emits the constraint.
	// "preferred" only emits the constraint when satisfiable.
	Enforcement EnforcementMode
	// Description is a human-readable description of the attribute.
	Description string
}

// TopologyRuleStore loads and manages topology rules from ConfigMaps.
type TopologyRuleStore struct {
	mu    sync.RWMutex
	rules map[string]TopologyRule // keyed by ConfigMap name
}

// NewTopologyRuleStore creates an empty rule store.
func NewTopologyRuleStore() *TopologyRuleStore {
	return &TopologyRuleStore{
		rules: make(map[string]TopologyRule),
	}
}

// LoadFromConfigMap extracts a topology rule from a ConfigMap.
// Returns an error if the ConfigMap is missing required fields.
func (s *TopologyRuleStore) LoadFromConfigMap(cm *corev1.ConfigMap) error {
	if cm == nil {
		return fmt.Errorf("configmap cannot be nil")
	}

	// Verify the label is present
	if cm.Labels[TopologyRuleLabel] != "true" {
		return fmt.Errorf("configmap %s/%s is not a topology rule (missing label)", cm.Namespace, cm.Name)
	}

	rule, err := parseTopologyRule(cm)
	if err != nil {
		return fmt.Errorf("failed to parse topology rule from %s/%s: %w", cm.Namespace, cm.Name, err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.rules[cm.Namespace+"/"+cm.Name] = rule

	klog.Infof("Loaded topology rule %q: attribute=%s driver=%s mapsTo=%s partitioning=%s constraint=%s enforcement=%s",
		rule.Name, rule.Attribute, rule.Driver, rule.MapsTo, rule.Partitioning, rule.Constraint, rule.Enforcement)
	return nil
}

// RemoveConfigMap removes the topology rule associated with a ConfigMap.
func (s *TopologyRuleStore) RemoveConfigMap(namespace, name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := namespace + "/" + name
	if _, ok := s.rules[key]; ok {
		delete(s.rules, key)
		klog.Infof("Removed topology rule from %s", key)
	}
}

// GetRules returns all loaded topology rules.
func (s *TopologyRuleStore) GetRules() []TopologyRule {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make([]TopologyRule, 0, len(s.rules))
	for _, rule := range s.rules {
		result = append(result, rule)
	}
	return result
}

// GetGroupingRules returns only rules with PartitioningGroup mode.
func (s *TopologyRuleStore) GetGroupingRules() []TopologyRule {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var result []TopologyRule
	for _, rule := range s.rules {
		if rule.Partitioning == PartitioningGroup {
			result = append(result, rule)
		}
	}
	return result
}

// GetMatchConstraintRules returns only rules with ConstraintMatch mode.
func (s *TopologyRuleStore) GetMatchConstraintRules() []TopologyRule {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var result []TopologyRule
	for _, rule := range s.rules {
		if rule.Constraint == ConstraintMatch {
			result = append(result, rule)
		}
	}
	return result
}

// parseTopologyRule parses a TopologyRule from a ConfigMap's data fields.
func parseTopologyRule(cm *corev1.ConfigMap) (TopologyRule, error) {
	rule := TopologyRule{
		Name: cm.Name,
	}

	// Required fields
	attr, ok := cm.Data["attribute"]
	if !ok || attr == "" {
		return rule, fmt.Errorf("missing required field 'attribute'")
	}
	rule.Attribute = attr

	attrType, ok := cm.Data["type"]
	if !ok || attrType == "" {
		return rule, fmt.Errorf("missing required field 'type'")
	}
	if attrType != "int" && attrType != "string" && attrType != "bool" {
		return rule, fmt.Errorf("invalid type %q: must be int, string, or bool", attrType)
	}
	rule.Type = attrType

	driver, ok := cm.Data["driver"]
	if !ok || driver == "" {
		return rule, fmt.Errorf("missing required field 'driver'")
	}
	rule.Driver = driver

	// Optional fields with defaults
	mapsTo := cm.Data["mapsTo"]
	switch StandardTopologyAttribute(mapsTo) {
	case MapsToNUMANode, MapsToPCIeRoot, MapsToSocket, MapsToNone:
		rule.MapsTo = StandardTopologyAttribute(mapsTo)
	default:
		return rule, fmt.Errorf("invalid mapsTo %q: must be numaNode, pcieRoot, socket, or empty", mapsTo)
	}

	partitioning := cm.Data["partitioning"]
	switch PartitioningMode(partitioning) {
	case PartitioningGroup:
		rule.Partitioning = PartitioningGroup
	case PartitioningInfo:
		rule.Partitioning = PartitioningInfo
	case "":
		rule.Partitioning = PartitioningInfo // default
	default:
		return rule, fmt.Errorf("invalid partitioning mode %q: must be group or info", partitioning)
	}

	constraint := cm.Data["constraint"]
	switch ConstraintMode(constraint) {
	case ConstraintMatch:
		rule.Constraint = ConstraintMatch
	case ConstraintNone:
		rule.Constraint = ConstraintNone
	case "":
		rule.Constraint = ConstraintNone // default
	default:
		return rule, fmt.Errorf("invalid constraint mode %q: must be match or none", constraint)
	}

	enforcement := cm.Data["enforcement"]
	switch EnforcementMode(enforcement) {
	case EnforcementRequired:
		rule.Enforcement = EnforcementRequired
	case EnforcementPreferred:
		rule.Enforcement = EnforcementPreferred
	case "":
		rule.Enforcement = EnforcementRequired // default
	default:
		return rule, fmt.Errorf("invalid enforcement mode %q: must be required or preferred", enforcement)
	}

	rule.Description = cm.Data["description"]

	return rule, nil
}
