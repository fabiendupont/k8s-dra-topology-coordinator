#!/bin/bash
# E2E test for the Node Partition Topology Coordinator
#
# Assumes a cluster is running with the mock-accel DRA driver deployed
# and publishing ResourceSlices. This script:
#   1. Deploys topology rule ConfigMaps for mock-accel attribute mapping
#   2. Builds and deploys the nodepartition coordinator
#   3. Waits for coordinator to produce partition ResourceSlices
#   4. Validates partition structure and topology attributes
#   5. Cleans up
#
# Usage:
#   ./test/e2e/run-e2e.sh                    # uses current kubectl context
#   KUBECONFIG=/path/to/kubeconfig ./test/e2e/run-e2e.sh
#
# For use with mock-device Vagrant cluster:
#   export KUBECONFIG=$(vagrant ssh mock-cluster-node1 -c \
#     "sudo cat /etc/rancher/k3s/k3s.yaml" 2>/dev/null | \
#     sed "s/127.0.0.1/$(vagrant ssh mock-cluster-node1 -c \
#     'hostname -I' 2>/dev/null | awk '{print $2}')/")
#   ./test/e2e/run-e2e.sh

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_DIR="$(dirname "$(dirname "$SCRIPT_DIR")")"

# Colors
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
RED='\033[0;31m'
NC='\033[0m'

COORDINATOR_DRIVER="nodepartition.dra.k8s.io"
MOCK_ACCEL_DRIVER="mock-accel.example.com"
LABEL_MANAGED="${COORDINATOR_DRIVER}/managed=true"
TIMEOUT=120

pass=0
fail=0

check() {
    local desc=$1
    shift
    if "$@" >/dev/null 2>&1; then
        echo -e "  ${GREEN}✓ $desc${NC}"
        pass=$((pass + 1))
    else
        echo -e "  ${RED}✗ $desc${NC}"
        fail=$((fail + 1))
    fi
}

cleanup() {
    echo -e "\n${YELLOW}Cleaning up...${NC}"
    kubectl delete -f "$SCRIPT_DIR/topology-rules.yaml" --ignore-not-found >/dev/null 2>&1 || true
    helm uninstall nodepartition --namespace default >/dev/null 2>&1 || true
    # Wait for coordinator slices to be garbage collected
    sleep 5
    kubectl delete resourceslices -l "$LABEL_MANAGED" --ignore-not-found >/dev/null 2>&1 || true
    kubectl delete deviceclasses -l "$LABEL_MANAGED" --ignore-not-found >/dev/null 2>&1 || true
    echo -e "${GREEN}Cleanup complete${NC}"
}
trap cleanup EXIT

echo -e "${GREEN}=== Node Partition Topology Coordinator E2E Test ===${NC}"
echo

# --- Pre-checks ---
echo -e "${YELLOW}Pre-checks...${NC}"

check "kubectl is available" command -v kubectl
check "helm is available" command -v helm

# Verify mock-accel ResourceSlices exist
MOCK_SLICE_COUNT=$(kubectl get resourceslices -o json 2>/dev/null | \
    python3 -c "import sys,json; print(len([s for s in json.load(sys.stdin)['items'] if s['spec']['driver']=='$MOCK_ACCEL_DRIVER']))" 2>/dev/null || echo "0")

if [ "$MOCK_SLICE_COUNT" -eq 0 ]; then
    echo -e "  ${RED}✗ No mock-accel ResourceSlices found — is the DRA driver deployed?${NC}"
    exit 1
fi
check "mock-accel ResourceSlices present ($MOCK_SLICE_COUNT)" [ "$MOCK_SLICE_COUNT" -gt 0 ]
echo

# --- Deploy topology rules ---
echo -e "${YELLOW}Deploying topology rules...${NC}"
kubectl apply -f "$SCRIPT_DIR/topology-rules.yaml"
check "topology rule ConfigMaps created" kubectl get configmap mock-accel-numa-rule
echo

# --- Deploy coordinator ---
echo -e "${YELLOW}Deploying coordinator...${NC}"

# Build image if not already available
if ! kubectl get pods -l app.kubernetes.io/name=nodepartition >/dev/null 2>&1; then
    helm install nodepartition "$PROJECT_DIR/deploy/helm/nodepartition" \
        --set controller.image.tag=dev \
        --set controller.image.pullPolicy=IfNotPresent \
        --wait --timeout 60s 2>/dev/null || {
        # If helm install fails (image not available), try building locally
        echo -e "${YELLOW}  Helm install may need a locally available image.${NC}"
        echo -e "${YELLOW}  Build with: make build && docker build -t ghcr.io/fabiendupont/nodepartition-controller:dev .${NC}"
    }
fi

# Wait for coordinator deployment to be ready
echo -e "${YELLOW}Waiting for coordinator to be ready...${NC}"
kubectl rollout status deployment -l app.kubernetes.io/component=controller --timeout=60s 2>/dev/null || true
check "coordinator deployment ready" kubectl get deployment -l app.kubernetes.io/component=controller -o jsonpath='{.items[0].status.readyReplicas}' 2>/dev/null
echo

# --- Wait for coordinator ResourceSlices ---
echo -e "${YELLOW}Waiting for coordinator to publish ResourceSlices...${NC}"
ELAPSED=0
COORD_SLICES=0
while [ $ELAPSED -lt $TIMEOUT ]; do
    COORD_SLICES=$(kubectl get resourceslices -l "$LABEL_MANAGED" --no-headers 2>/dev/null | wc -l)
    if [ "$COORD_SLICES" -gt 0 ]; then
        break
    fi
    sleep 5
    ELAPSED=$((ELAPSED + 5))
    echo -e "  Waiting... ($ELAPSED/${TIMEOUT}s)"
done

check "coordinator ResourceSlices published ($COORD_SLICES)" [ "$COORD_SLICES" -gt 0 ]
echo

if [ "$COORD_SLICES" -eq 0 ]; then
    echo -e "${RED}No coordinator ResourceSlices found — aborting validation${NC}"
    echo -e "${YELLOW}Check coordinator logs:${NC}"
    kubectl logs -l app.kubernetes.io/component=controller --tail=50 2>/dev/null || true
    exit 1
fi

# --- Validate ResourceSlices ---
echo -e "${YELLOW}Validating coordinator ResourceSlices...${NC}"

# Check driver name
DRIVER=$(kubectl get resourceslices -l "$LABEL_MANAGED" -o jsonpath='{.items[0].spec.driver}' 2>/dev/null)
check "driver name is $COORDINATOR_DRIVER" [ "$DRIVER" = "$COORDINATOR_DRIVER" ]

# Check partition types exist
PARTITION_TYPES=$(kubectl get resourceslices -l "$LABEL_MANAGED" -o json 2>/dev/null | \
    python3 -c "
import sys, json
data = json.load(sys.stdin)
types = set()
for s in data['items']:
    for d in s['spec'].get('devices', []):
        attrs = d.get('attributes', {})
        pt = attrs.get('${COORDINATOR_DRIVER}/partitionType', {})
        if 'stringValue' in pt:
            types.add(pt['stringValue'])
print(' '.join(sorted(types)))
" 2>/dev/null)

echo -e "  Partition types found: ${GREEN}$PARTITION_TYPES${NC}"
check "eighth partitions exist" echo "$PARTITION_TYPES" | grep -q "eighth"
check "quarter partitions exist" echo "$PARTITION_TYPES" | grep -q "quarter"
check "full partitions exist" echo "$PARTITION_TYPES" | grep -q "full"

# Check node coverage
COORD_NODES=$(kubectl get resourceslices -l "$LABEL_MANAGED" -o jsonpath='{range .items[*]}{.spec.nodeName}{"\n"}{end}' 2>/dev/null | sort -u | wc -l)
MOCK_NODES=$(kubectl get resourceslices -o json 2>/dev/null | \
    python3 -c "import sys,json; print(len(set(s['spec'].get('nodeName','') for s in json.load(sys.stdin)['items'] if s['spec']['driver']=='$MOCK_ACCEL_DRIVER')))" 2>/dev/null || echo "0")
check "coordinator covers all mock-accel nodes ($COORD_NODES/$MOCK_NODES)" [ "$COORD_NODES" -eq "$MOCK_NODES" ]

# Check device count attributes
DEVICE_COUNT_ATTRS=$(kubectl get resourceslices -l "$LABEL_MANAGED" -o json 2>/dev/null | \
    python3 -c "
import sys, json
data = json.load(sys.stdin)
found = False
for s in data['items']:
    for d in s['spec'].get('devices', []):
        for attr_name in d.get('attributes', {}):
            if 'deviceCount_' in attr_name:
                found = True
                break
print('yes' if found else 'no')
" 2>/dev/null)
check "device count attributes present" [ "$DEVICE_COUNT_ATTRS" = "yes" ]

echo

# --- Validate DeviceClasses ---
echo -e "${YELLOW}Validating DeviceClasses...${NC}"
DC_COUNT=$(kubectl get deviceclasses -l "$LABEL_MANAGED" --no-headers 2>/dev/null | wc -l)
check "DeviceClasses created ($DC_COUNT)" [ "$DC_COUNT" -gt 0 ]

if [ "$DC_COUNT" -gt 0 ]; then
    DC_NAMES=$(kubectl get deviceclasses -l "$LABEL_MANAGED" -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' 2>/dev/null)
    echo -e "  DeviceClasses: ${GREEN}$DC_NAMES${NC}"

    # Check DeviceClass has selectors
    DC_SELECTORS=$(kubectl get deviceclasses -l "$LABEL_MANAGED" -o jsonpath='{.items[0].spec.selectors}' 2>/dev/null)
    check "DeviceClass has selectors" [ -n "$DC_SELECTORS" ]

    # Check DeviceClass has opaque config
    DC_CONFIG=$(kubectl get deviceclasses -l "$LABEL_MANAGED" -o jsonpath='{.items[0].spec.config}' 2>/dev/null)
    check "DeviceClass has config" [ -n "$DC_CONFIG" ]
fi

echo

# --- Summary ---
total=$((pass + fail))
echo -e "${GREEN}=== E2E Test Results ===${NC}"
echo -e "  Passed: ${GREEN}$pass${NC}"
echo -e "  Failed: ${RED}$fail${NC}"
echo -e "  Total:  $total"
echo

if [ "$fail" -gt 0 ]; then
    echo -e "${RED}E2E FAILED${NC}"
    exit 1
fi

echo -e "${GREEN}E2E PASSED${NC}"
