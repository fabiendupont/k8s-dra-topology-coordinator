package webhook

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	admissionv1 "k8s.io/api/admission/v1"
	resourcev1 "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/fabiendupont/k8s-dra-driver-nodepartition/internal/controller"
)

// makePartitionDeviceClass creates a DeviceClass with a PartitionConfig opaque config.
func makePartitionDeviceClass(name string, config controller.PartitionConfig) *resourcev1.DeviceClass {
	configJSON, _ := json.Marshal(config)
	return &resourcev1.DeviceClass{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
		Spec: resourcev1.DeviceClassSpec{
			Config: []resourcev1.DeviceClassConfiguration{
				{
					DeviceConfiguration: resourcev1.DeviceConfiguration{
						Opaque: &resourcev1.OpaqueDeviceConfiguration{
							Driver:     driverName,
							Parameters: runtime.RawExtension{Raw: configJSON},
						},
					},
				},
			},
		},
	}
}

// makeRegularDeviceClass creates a DeviceClass without a PartitionConfig.
func makeRegularDeviceClass(name string) *resourcev1.DeviceClass {
	return &resourcev1.DeviceClass{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
		Spec: resourcev1.DeviceClassSpec{
			Selectors: []resourcev1.DeviceSelector{
				{
					CEL: &resourcev1.CELDeviceSelector{
						Expression: `device.driver == "some.driver"`,
					},
				},
			},
		},
	}
}

// makeAdmissionReview builds an AdmissionReview for a ResourceClaim.
func makeAdmissionReview(claim *resourcev1.ResourceClaim, operation admissionv1.Operation) admissionv1.AdmissionReview { //nolint:unparam // test helper, operation may vary in future tests
	claimJSON, _ := json.Marshal(claim)
	return admissionv1.AdmissionReview{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "admission.k8s.io/v1",
			Kind:       "AdmissionReview",
		},
		Request: &admissionv1.AdmissionRequest{
			UID: "test-uid",
			Resource: metav1.GroupVersionResource{
				Group:    "resource.k8s.io",
				Version:  "v1",
				Resource: "resourceclaims",
			},
			Operation: operation,
			Object: runtime.RawExtension{
				Raw: claimJSON,
			},
		},
	}
}

// sendAdmissionReview sends an AdmissionReview to the webhook handler and returns the response.
func sendAdmissionReview(t *testing.T, handler http.Handler, review admissionv1.AdmissionReview) admissionv1.AdmissionReview {
	t.Helper()
	body, err := json.Marshal(review)
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodPost, "/mutate", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)

	require.Equal(t, http.StatusOK, recorder.Code)

	var resp admissionv1.AdmissionReview
	err = json.Unmarshal(recorder.Body.Bytes(), &resp)
	require.NoError(t, err)
	require.NotNil(t, resp.Response)

	return resp
}

func TestNonPartitionClaimPassesThrough(t *testing.T) {
	regularClass := makeRegularDeviceClass("regular-class")
	client := fake.NewSimpleClientset(regularClass)
	expander := NewClaimExpander(client)

	claim := &resourcev1.ResourceClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-claim",
			Namespace: "default",
		},
		Spec: resourcev1.ResourceClaimSpec{
			Devices: resourcev1.DeviceClaim{
				Requests: []resourcev1.DeviceRequest{
					{
						Name: "my-device",
						Exactly: &resourcev1.ExactDeviceRequest{
							DeviceClassName: "regular-class",
							Count:           1,
						},
					},
				},
			},
		},
	}

	review := makeAdmissionReview(claim, admissionv1.Create)
	resp := sendAdmissionReview(t, expander.Handler(), review)

	assert.True(t, resp.Response.Allowed)
	assert.Nil(t, resp.Response.PatchType, "non-partition claim should not be mutated")
	assert.Empty(t, resp.Response.Patch, "non-partition claim should have no patch")
}

func TestPartitionClaimIsExpanded(t *testing.T) {
	partConfig := controller.PartitionConfig{
		Kind: "PartitionConfig",
		SubResources: []controller.SubResourceConfig{
			{DeviceClass: "gpu.nvidia.com", Count: 4},
			{DeviceClass: "rdma.mellanox.com", Count: 4},
		},
		Alignments: []controller.AlignmentConfig{
			{
				Attribute: "nodepartition.dra.k8s.io/numaNode",
				Requests:  []string{"gpu.nvidia.com", "rdma.mellanox.com"},
			},
		},
	}

	partClass := makePartitionDeviceClass("test-partition", partConfig)
	client := fake.NewSimpleClientset(partClass)
	expander := NewClaimExpander(client)

	claim := &resourcev1.ResourceClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "partition-claim",
			Namespace: "default",
		},
		Spec: resourcev1.ResourceClaimSpec{
			Devices: resourcev1.DeviceClaim{
				Requests: []resourcev1.DeviceRequest{
					{
						Name: "partition",
						Exactly: &resourcev1.ExactDeviceRequest{
							DeviceClassName: "test-partition",
							Count:           1,
						},
					},
				},
			},
		},
	}

	review := makeAdmissionReview(claim, admissionv1.Create)
	resp := sendAdmissionReview(t, expander.Handler(), review)

	assert.True(t, resp.Response.Allowed)
	require.NotNil(t, resp.Response.PatchType)
	assert.Equal(t, admissionv1.PatchTypeJSONPatch, *resp.Response.PatchType)
	require.NotEmpty(t, resp.Response.Patch)

	var patches []jsonPatch
	err := json.Unmarshal(resp.Response.Patch, &patches)
	require.NoError(t, err)

	// Should have at least a requests replacement and constraints addition
	require.GreaterOrEqual(t, len(patches), 2)

	// Find the requests patch
	var requestsPatch *jsonPatch
	var constraintsPatch *jsonPatch
	for i := range patches {
		if patches[i].Path == "/spec/devices/requests" {
			requestsPatch = &patches[i]
		}
		if patches[i].Path == "/spec/devices/constraints" {
			constraintsPatch = &patches[i]
		}
	}

	require.NotNil(t, requestsPatch, "should have a requests patch")
	require.NotNil(t, constraintsPatch, "should have a constraints patch")

	// Verify expanded requests
	reqBytes, err := json.Marshal(requestsPatch.Value)
	require.NoError(t, err)

	var expandedRequests []resourcev1.DeviceRequest
	err = json.Unmarshal(reqBytes, &expandedRequests)
	require.NoError(t, err)

	assert.Len(t, expandedRequests, 2, "should have 2 sub-resource requests")

	// Verify request names contain the original name prefix
	requestNames := make(map[string]bool)
	for _, r := range expandedRequests {
		assert.Contains(t, r.Name, "partition-")
		requestNames[r.Name] = true
		require.NotNil(t, r.Exactly)
		assert.Equal(t, int64(4), r.Exactly.Count)
	}

	// Verify constraints
	conBytes, err := json.Marshal(constraintsPatch.Value)
	require.NoError(t, err)

	var expandedConstraints []resourcev1.DeviceConstraint
	err = json.Unmarshal(conBytes, &expandedConstraints)
	require.NoError(t, err)

	assert.GreaterOrEqual(t, len(expandedConstraints), 1, "should have at least one constraint")

	// Verify constraint references the expanded request names
	for _, c := range expandedConstraints {
		require.NotNil(t, c.MatchAttribute)
		for _, reqName := range c.Requests {
			assert.True(t, requestNames[reqName], "constraint should reference an expanded request name: %s", reqName)
		}
	}
}

func TestMixedClaimOnlyExpandsPartition(t *testing.T) {
	partConfig := controller.PartitionConfig{
		Kind: "PartitionConfig",
		SubResources: []controller.SubResourceConfig{
			{DeviceClass: "gpu.nvidia.com", Count: 2},
		},
		Alignments: []controller.AlignmentConfig{},
	}

	partClass := makePartitionDeviceClass("partition-class", partConfig)
	regularClass := makeRegularDeviceClass("regular-class")
	client := fake.NewSimpleClientset(partClass, regularClass)
	expander := NewClaimExpander(client)

	claim := &resourcev1.ResourceClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "mixed-claim",
			Namespace: "default",
		},
		Spec: resourcev1.ResourceClaimSpec{
			Devices: resourcev1.DeviceClaim{
				Requests: []resourcev1.DeviceRequest{
					{
						Name: "regular",
						Exactly: &resourcev1.ExactDeviceRequest{
							DeviceClassName: "regular-class",
							Count:           1,
						},
					},
					{
						Name: "partition",
						Exactly: &resourcev1.ExactDeviceRequest{
							DeviceClassName: "partition-class",
							Count:           1,
						},
					},
				},
			},
		},
	}

	review := makeAdmissionReview(claim, admissionv1.Create)
	resp := sendAdmissionReview(t, expander.Handler(), review)

	assert.True(t, resp.Response.Allowed)
	require.NotNil(t, resp.Response.PatchType)
	require.NotEmpty(t, resp.Response.Patch)

	var patches []jsonPatch
	err := json.Unmarshal(resp.Response.Patch, &patches)
	require.NoError(t, err)

	// Find the requests patch
	var requestsPatch *jsonPatch
	for i := range patches {
		if patches[i].Path == "/spec/devices/requests" {
			requestsPatch = &patches[i]
		}
	}
	require.NotNil(t, requestsPatch)

	reqBytes, err := json.Marshal(requestsPatch.Value)
	require.NoError(t, err)

	var expandedRequests []resourcev1.DeviceRequest
	err = json.Unmarshal(reqBytes, &expandedRequests)
	require.NoError(t, err)

	// Should have 2 requests: 1 regular (unchanged) + 1 expanded sub-resource
	assert.Len(t, expandedRequests, 2)

	// The regular request should be preserved
	foundRegular := false
	foundExpanded := false
	for _, r := range expandedRequests {
		if r.Name == "regular" {
			foundRegular = true
			require.NotNil(t, r.Exactly)
			assert.Equal(t, "regular-class", r.Exactly.DeviceClassName)
		}
		if r.Name == "partition-gpu-nvidia-com" {
			foundExpanded = true
			require.NotNil(t, r.Exactly)
			assert.Equal(t, "gpu.nvidia.com", r.Exactly.DeviceClassName)
			assert.Equal(t, int64(2), r.Exactly.Count)
		}
	}
	assert.True(t, foundRegular, "regular request should be preserved")
	assert.True(t, foundExpanded, "partition request should be expanded")
}

func TestDeviceClassNotFoundReturnsAllow(t *testing.T) {
	client := fake.NewSimpleClientset() // No DeviceClasses
	expander := NewClaimExpander(client)

	claim := &resourcev1.ResourceClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "missing-class-claim",
			Namespace: "default",
		},
		Spec: resourcev1.ResourceClaimSpec{
			Devices: resourcev1.DeviceClaim{
				Requests: []resourcev1.DeviceRequest{
					{
						Name: "req",
						Exactly: &resourcev1.ExactDeviceRequest{
							DeviceClassName: "nonexistent-class",
							Count:           1,
						},
					},
				},
			},
		},
	}

	review := makeAdmissionReview(claim, admissionv1.Create)
	resp := sendAdmissionReview(t, expander.Handler(), review)

	assert.True(t, resp.Response.Allowed)
	assert.Nil(t, resp.Response.PatchType, "missing DeviceClass should not cause mutation")
	assert.Empty(t, resp.Response.Patch)
}

func TestInvalidPartitionConfigReturnsAllow(t *testing.T) {
	// Create a DeviceClass with our driver but invalid/non-PartitionConfig opaque data
	invalidJSON := []byte(`{"kind":"SomethingElse","foo":"bar"}`)
	dc := &resourcev1.DeviceClass{
		ObjectMeta: metav1.ObjectMeta{
			Name: "invalid-config-class",
		},
		Spec: resourcev1.DeviceClassSpec{
			Config: []resourcev1.DeviceClassConfiguration{
				{
					DeviceConfiguration: resourcev1.DeviceConfiguration{
						Opaque: &resourcev1.OpaqueDeviceConfiguration{
							Driver:     driverName,
							Parameters: runtime.RawExtension{Raw: invalidJSON},
						},
					},
				},
			},
		},
	}

	client := fake.NewSimpleClientset(dc)
	expander := NewClaimExpander(client)

	claim := &resourcev1.ResourceClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "invalid-config-claim",
			Namespace: "default",
		},
		Spec: resourcev1.ResourceClaimSpec{
			Devices: resourcev1.DeviceClaim{
				Requests: []resourcev1.DeviceRequest{
					{
						Name: "req",
						Exactly: &resourcev1.ExactDeviceRequest{
							DeviceClassName: "invalid-config-class",
							Count:           1,
						},
					},
				},
			},
		},
	}

	review := makeAdmissionReview(claim, admissionv1.Create)
	resp := sendAdmissionReview(t, expander.Handler(), review)

	assert.True(t, resp.Response.Allowed)
	assert.Nil(t, resp.Response.PatchType, "invalid PartitionConfig should not cause mutation")
	assert.Empty(t, resp.Response.Patch)
}

func TestExpandClaimDirectly(t *testing.T) {
	partConfig := controller.PartitionConfig{
		Kind: "PartitionConfig",
		SubResources: []controller.SubResourceConfig{
			{DeviceClass: "gpu.nvidia.com", Count: 4},
			{DeviceClass: "rdma.mellanox.com", Count: 4},
		},
		Alignments: []controller.AlignmentConfig{
			{
				Attribute: "nodepartition.dra.k8s.io/numaNode",
				Requests:  []string{"gpu.nvidia.com", "rdma.mellanox.com"},
			},
			{
				Attribute: "resource.kubernetes.io/pcieRoot",
				Requests:  []string{"gpu.nvidia.com", "rdma.mellanox.com"},
			},
		},
	}

	partClass := makePartitionDeviceClass("test-partition", partConfig)
	client := fake.NewSimpleClientset(partClass)
	expander := NewClaimExpander(client)

	claim := &resourcev1.ResourceClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-claim",
			Namespace: "default",
		},
		Spec: resourcev1.ResourceClaimSpec{
			Devices: resourcev1.DeviceClaim{
				Requests: []resourcev1.DeviceRequest{
					{
						Name: "my-partition",
						Exactly: &resourcev1.ExactDeviceRequest{
							DeviceClassName: "test-partition",
							Count:           1,
						},
					},
				},
			},
		},
	}

	patches, err := expander.expandClaim(context.Background(), claim)
	require.NoError(t, err)
	require.NotEmpty(t, patches)

	// Verify request names use the original request name as prefix
	var requestsPatch *jsonPatch
	for i := range patches {
		if patches[i].Path == "/spec/devices/requests" {
			requestsPatch = &patches[i]
		}
	}
	require.NotNil(t, requestsPatch)

	reqBytes, _ := json.Marshal(requestsPatch.Value)
	var reqs []resourcev1.DeviceRequest
	require.NoError(t, json.Unmarshal(reqBytes, &reqs))

	assert.Len(t, reqs, 2)
	expectedNames := map[string]string{
		"my-partition-gpu-nvidia-com":    "gpu.nvidia.com",
		"my-partition-rdma-mellanox-com": "rdma.mellanox.com",
	}
	for _, r := range reqs {
		expectedClass, ok := expectedNames[r.Name]
		assert.True(t, ok, "unexpected request name: %s", r.Name)
		require.NotNil(t, r.Exactly)
		assert.Equal(t, expectedClass, r.Exactly.DeviceClassName)
	}
}

func TestSanitizeDeviceClassName(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"gpu.nvidia.com", "gpu-nvidia-com"},
		{"rdma.mellanox.com", "rdma-mellanox-com"},
		{"simple", "simple"},
		{"UPPER.Case", "upper-case"},
		{"a..b", "a-b"},
		{"", "sub"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := sanitizeDeviceClassName(tt.input)
			assert.Equal(t, tt.want, got)
		})
	}
}
