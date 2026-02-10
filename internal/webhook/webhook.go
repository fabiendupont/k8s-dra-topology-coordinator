// Package webhook implements a mutating admission webhook that expands
// partition ResourceClaims into multi-request claims with topology alignment.
package webhook

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	admissionv1 "k8s.io/api/admission/v1"
	resourcev1 "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/client-go/kubernetes"
	klog "k8s.io/klog/v2"

	"github.com/fabiendupont/k8s-dra-driver-nodepartition/internal/controller"
)

const (
	driverName = "nodepartition.dra.k8s.io"
)

// ClaimExpander is a mutating admission webhook that expands partition
// ResourceClaims into multi-request claims with alignment constraints.
type ClaimExpander struct {
	client  kubernetes.Interface
	decoder runtime.Decoder
}

// jsonPatch represents a single JSON Patch operation.
type jsonPatch struct {
	Op    string      `json:"op"`
	Path  string      `json:"path"`
	Value interface{} `json:"value,omitempty"`
}

// NewClaimExpander creates a new ClaimExpander webhook handler.
func NewClaimExpander(client kubernetes.Interface) *ClaimExpander {
	scheme := runtime.NewScheme()
	codecs := serializer.NewCodecFactory(scheme)
	return &ClaimExpander{
		client:  client,
		decoder: codecs.UniversalDeserializer(),
	}
}

// Handler returns the HTTP handler for the webhook.
func (ce *ClaimExpander) Handler() http.Handler {
	return ce
}

// ServeHTTP handles admission review requests.
func (ce *ClaimExpander) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		klog.Errorf("Failed to read request body: %v", err)
		http.Error(w, "failed to read body", http.StatusBadRequest)
		return
	}

	var review admissionv1.AdmissionReview
	if err := json.Unmarshal(body, &review); err != nil {
		klog.Errorf("Failed to unmarshal admission review: %v", err)
		http.Error(w, "failed to unmarshal admission review", http.StatusBadRequest)
		return
	}

	if review.Request == nil {
		klog.Error("Admission review has no request")
		http.Error(w, "admission review has no request", http.StatusBadRequest)
		return
	}

	response := ce.handleAdmission(r.Context(), review.Request)
	review.Response = response
	review.Response.UID = review.Request.UID

	respBytes, err := json.Marshal(review)
	if err != nil {
		klog.Errorf("Failed to marshal admission response: %v", err)
		http.Error(w, "failed to marshal response", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	if _, err := w.Write(respBytes); err != nil {
		klog.Errorf("Failed to write admission response: %v", err)
	}
}

// handleAdmission processes a single admission request and returns the response.
func (ce *ClaimExpander) handleAdmission(ctx context.Context, req *admissionv1.AdmissionRequest) *admissionv1.AdmissionResponse {
	// Only handle ResourceClaims
	if req.Resource.Group != "resource.k8s.io" || req.Resource.Resource != "resourceclaims" {
		return allowResponse()
	}

	// Only handle CREATE and UPDATE
	if req.Operation != admissionv1.Create && req.Operation != admissionv1.Update {
		return allowResponse()
	}

	var claim resourcev1.ResourceClaim
	if err := json.Unmarshal(req.Object.Raw, &claim); err != nil {
		klog.Errorf("Failed to unmarshal ResourceClaim: %v", err)
		return allowResponse()
	}

	patches, err := ce.expandClaim(ctx, &claim)
	if err != nil {
		klog.Errorf("Failed to expand claim %s/%s: %v", claim.Namespace, claim.Name, err)
		return allowResponse()
	}

	if len(patches) == 0 {
		return allowResponse()
	}

	patchBytes, err := json.Marshal(patches)
	if err != nil {
		klog.Errorf("Failed to marshal patches: %v", err)
		return allowResponse()
	}

	patchType := admissionv1.PatchTypeJSONPatch
	return &admissionv1.AdmissionResponse{
		Allowed:   true,
		PatchType: &patchType,
		Patch:     patchBytes,
	}
}

// expandClaim examines a ResourceClaim and generates JSON patches to expand
// partition requests into sub-resource requests with alignment constraints.
func (ce *ClaimExpander) expandClaim(ctx context.Context, claim *resourcev1.ResourceClaim) ([]jsonPatch, error) { //nolint:unparam // error return reserved for future use
	requests := claim.Spec.Devices.Requests
	if len(requests) == 0 {
		return nil, nil
	}

	var expandedRequests []resourcev1.DeviceRequest
	var constraints []resourcev1.DeviceConstraint
	anyExpanded := false

	for _, req := range requests {
		if req.Exactly == nil {
			expandedRequests = append(expandedRequests, req)
			continue
		}

		partitionConfig, err := ce.getPartitionConfig(ctx, req.Exactly.DeviceClassName)
		if err != nil {
			klog.Warningf("Failed to get partition config for DeviceClass %q: %v", req.Exactly.DeviceClassName, err)
			expandedRequests = append(expandedRequests, req)
			continue
		}

		if partitionConfig == nil {
			expandedRequests = append(expandedRequests, req)
			continue
		}

		// Expand this request into sub-resource requests
		subRequests, subConstraints := ce.expandRequest(req, partitionConfig)
		expandedRequests = append(expandedRequests, subRequests...)
		constraints = append(constraints, subConstraints...)
		anyExpanded = true
	}

	if !anyExpanded {
		return nil, nil
	}

	var patches []jsonPatch

	// Replace requests
	patches = append(patches, jsonPatch{
		Op:    "replace",
		Path:  "/spec/devices/requests",
		Value: expandedRequests,
	})

	// Add or replace constraints
	if len(constraints) > 0 {
		if len(claim.Spec.Devices.Constraints) == 0 {
			patches = append(patches, jsonPatch{
				Op:    "add",
				Path:  "/spec/devices/constraints",
				Value: constraints,
			})
		} else {
			merged := append(claim.Spec.Devices.Constraints, constraints...)
			patches = append(patches, jsonPatch{
				Op:    "replace",
				Path:  "/spec/devices/constraints",
				Value: merged,
			})
		}
	}

	return patches, nil
}

// expandRequest expands a single partition DeviceRequest into sub-resource requests
// and alignment constraints based on the PartitionConfig.
func (ce *ClaimExpander) expandRequest(req resourcev1.DeviceRequest, config *controller.PartitionConfig) ([]resourcev1.DeviceRequest, []resourcev1.DeviceConstraint) {
	var subRequests []resourcev1.DeviceRequest
	var constraints []resourcev1.DeviceConstraint

	// Build a map of generated request names for constraint resolution
	requestNameMap := make(map[string]string) // subresource device class -> generated request name

	for _, sr := range config.SubResources {
		sanitized := sanitizeDeviceClassName(sr.DeviceClass)
		name := req.Name + "-" + sanitized
		requestNameMap[sr.DeviceClass] = name

		count := int64(sr.Count)
		subRequests = append(subRequests, resourcev1.DeviceRequest{
			Name: name,
			Exactly: &resourcev1.ExactDeviceRequest{
				DeviceClassName: sr.DeviceClass,
				Count:           count,
			},
		})
	}

	// Build constraints from alignments
	for _, alignment := range config.Alignments {
		var resolvedRequests []string
		for _, reqName := range alignment.Requests {
			// Try to resolve the request name through the mapping
			if mapped, ok := requestNameMap[reqName]; ok {
				resolvedRequests = append(resolvedRequests, mapped)
			} else {
				// Use as-is (might be a reference like "partition" or a direct name)
				// Try prefixing with the original request name
				found := false
				for _, sr := range config.SubResources {
					sanitized := sanitizeDeviceClassName(sr.DeviceClass)
					if reqName == sanitized {
						resolvedRequests = append(resolvedRequests, req.Name+"-"+sanitized)
						found = true
						break
					}
				}
				if !found {
					// Skip references to the original request name (e.g., "partition")
					// since it no longer exists after expansion
					continue
				}
			}
		}

		if len(resolvedRequests) < 2 {
			// A match constraint needs at least 2 requests to be meaningful
			continue
		}

		attr := resourcev1.FullyQualifiedName(alignment.Attribute)
		constraints = append(constraints, resourcev1.DeviceConstraint{
			Requests:       resolvedRequests,
			MatchAttribute: &attr,
		})
	}

	return subRequests, constraints
}

// getPartitionConfig looks up a DeviceClass and extracts the PartitionConfig
// from its opaque configuration if present.
func (ce *ClaimExpander) getPartitionConfig(ctx context.Context, deviceClassName string) (*controller.PartitionConfig, error) {
	dc, err := ce.client.ResourceV1().DeviceClasses().Get(ctx, deviceClassName, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to get DeviceClass %q: %w", deviceClassName, err)
	}

	for _, cfg := range dc.Spec.Config {
		if cfg.Opaque == nil {
			continue
		}
		if cfg.Opaque.Driver != driverName {
			continue
		}

		var partConfig controller.PartitionConfig
		if err := json.Unmarshal(cfg.Opaque.Parameters.Raw, &partConfig); err != nil {
			klog.Warningf("Failed to unmarshal opaque config for DeviceClass %q: %v", deviceClassName, err)
			continue
		}

		if partConfig.Kind != "PartitionConfig" {
			continue
		}

		return &partConfig, nil
	}

	return nil, nil
}

// sanitizeDeviceClassName converts a device class name into a DNS-label-safe suffix.
func sanitizeDeviceClassName(name string) string {
	sanitized := strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			return r
		}
		if r >= 'A' && r <= 'Z' {
			return r + ('a' - 'A')
		}
		return '-'
	}, name)

	// Collapse consecutive dashes
	for strings.Contains(sanitized, "--") {
		sanitized = strings.ReplaceAll(sanitized, "--", "-")
	}
	sanitized = strings.Trim(sanitized, "-")

	if sanitized == "" {
		sanitized = "sub"
	}

	return sanitized
}

// allowResponse returns an admission response that allows the request without mutation.
func allowResponse() *admissionv1.AdmissionResponse {
	return &admissionv1.AdmissionResponse{
		Allowed: true,
	}
}
