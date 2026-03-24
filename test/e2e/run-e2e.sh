#!/bin/bash
# E2E test for the Node Partition Topology Coordinator
#
# Tests cross-driver topology partitioning using mock-accel (required)
# and dra-driver-cpu (optional, tested if present). When both drivers
# are available, validates that partitions contain devices from both
# drivers grouped by shared NUMA topology.
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
CPU_DRIVER="dra.cpu"
LABEL_MANAGED="${COORDINATOR_DRIVER}/managed=true"
TIMEOUT=120

# Track which drivers are available
HAS_MOCK_ACCEL=false
HAS_CPU_DRIVER=false

pass=0
fail=0
skip=0

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

check_skip() {
    local desc=$1
    shift
    if "$@" >/dev/null 2>&1; then
        echo -e "  ${GREEN}✓ $desc${NC}"
        pass=$((pass + 1))
    else
        echo -e "  ${YELLOW}⊘ $desc (skipped — driver not present)${NC}"
        skip=$((skip + 1))
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

# count_driver_slices returns the number of ResourceSlices for a given driver
count_driver_slices() {
    local driver=$1
    kubectl get resourceslices -o json 2>/dev/null | \
        python3 -c "
import sys, json
data = json.load(sys.stdin)
print(len([s for s in data['items'] if s['spec']['driver'] == '$driver']))
" 2>/dev/null || echo "0"
}

# count_driver_nodes returns the number of distinct nodes for a given driver
count_driver_nodes() {
    local driver=$1
    kubectl get resourceslices -o json 2>/dev/null | \
        python3 -c "
import sys, json
data = json.load(sys.stdin)
nodes = set()
for s in data['items']:
    if s['spec']['driver'] == '$driver' and s['spec'].get('nodeName'):
        nodes.add(s['spec']['nodeName'])
print(len(nodes))
" 2>/dev/null || echo "0"
}

echo -e "${GREEN}=== Node Partition Topology Coordinator E2E Test ===${NC}"
echo

# --- Pre-checks ---
echo -e "${YELLOW}Pre-checks...${NC}"

check "kubectl is available" command -v kubectl
check "helm is available" command -v helm

# Detect available DRA drivers
MOCK_SLICE_COUNT=$(count_driver_slices "$MOCK_ACCEL_DRIVER")
CPU_SLICE_COUNT=$(count_driver_slices "$CPU_DRIVER")

if [ "$MOCK_SLICE_COUNT" -gt 0 ]; then
    HAS_MOCK_ACCEL=true
fi
if [ "$CPU_SLICE_COUNT" -gt 0 ]; then
    HAS_CPU_DRIVER=true
fi

# mock-accel is required
if [ "$HAS_MOCK_ACCEL" = false ]; then
    echo -e "  ${RED}✗ No mock-accel ResourceSlices found — is the DRA driver deployed?${NC}"
    exit 1
fi
check "mock-accel ResourceSlices present ($MOCK_SLICE_COUNT)" [ "$MOCK_SLICE_COUNT" -gt 0 ]

# dra-driver-cpu is optional
if [ "$HAS_CPU_DRIVER" = true ]; then
    echo -e "  ${GREEN}✓ dra-driver-cpu ResourceSlices present ($CPU_SLICE_COUNT)${NC}"
    pass=$((pass + 1))
else
    echo -e "  ${YELLOW}⊘ dra-driver-cpu not detected — cross-driver tests will be skipped${NC}"
    skip=$((skip + 1))
fi

DRIVER_SUMMARY="mock-accel"
if [ "$HAS_CPU_DRIVER" = true ]; then
    DRIVER_SUMMARY="mock-accel + dra-driver-cpu"
fi
echo -e "  Drivers under test: ${GREEN}$DRIVER_SUMMARY${NC}"
echo

# --- Deploy topology rules ---
echo -e "${YELLOW}Deploying topology rules...${NC}"
kubectl apply -f "$SCRIPT_DIR/topology-rules.yaml"
check "mock-accel topology rules created" kubectl get configmap mock-accel-numa-rule
check "mock-accel PCIe topology rule created" kubectl get configmap mock-accel-pci-rule
if [ "$HAS_CPU_DRIVER" = true ]; then
    check "cpu NUMA topology rule created" kubectl get configmap cpu-numa-rule
    check "cpu socket topology rule created" kubectl get configmap cpu-socket-rule
else
    check_skip "cpu NUMA topology rule created" kubectl get configmap cpu-numa-rule
    check_skip "cpu socket topology rule created" kubectl get configmap cpu-socket-rule
fi
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

# Check node coverage — coordinator should cover all nodes that have mock-accel devices
COORD_NODES=$(kubectl get resourceslices -l "$LABEL_MANAGED" -o jsonpath='{range .items[*]}{.spec.nodeName}{"\n"}{end}' 2>/dev/null | sort -u | wc -l)
MOCK_NODES=$(count_driver_nodes "$MOCK_ACCEL_DRIVER")
check "coordinator covers all mock-accel nodes ($COORD_NODES/$MOCK_NODES)" [ "$COORD_NODES" -eq "$MOCK_NODES" ]

# Check device count attributes include mock-accel
MOCK_ACCEL_DEVICE_COUNTS=$(kubectl get resourceslices -l "$LABEL_MANAGED" -o json 2>/dev/null | \
    python3 -c "
import sys, json
data = json.load(sys.stdin)
found = False
for s in data['items']:
    for d in s['spec'].get('devices', []):
        for attr_name in d.get('attributes', {}):
            if 'deviceCount_' in attr_name and 'mock-accel' in attr_name:
                found = True
                break
print('yes' if found else 'no')
" 2>/dev/null)
check "device count attributes include mock-accel" [ "$MOCK_ACCEL_DEVICE_COUNTS" = "yes" ]

echo

# --- Cross-driver validation (mock-accel + CPU) ---
if [ "$HAS_CPU_DRIVER" = true ]; then
    echo -e "${YELLOW}Validating cross-driver partitioning (mock-accel + dra-driver-cpu)...${NC}"

    # Check that device counts include CPU driver
    CPU_DEVICE_COUNTS=$(kubectl get resourceslices -l "$LABEL_MANAGED" -o json 2>/dev/null | \
        python3 -c "
import sys, json
data = json.load(sys.stdin)
found = False
for s in data['items']:
    for d in s['spec'].get('devices', []):
        for attr_name in d.get('attributes', {}):
            if 'deviceCount_' in attr_name and 'dra.cpu' in attr_name:
                found = True
                break
print('yes' if found else 'no')
" 2>/dev/null)
    check "device count attributes include dra-driver-cpu" [ "$CPU_DEVICE_COUNTS" = "yes" ]

    # Check that both drivers' nodes overlap (they should — both run on the same nodes)
    CPU_NODES=$(count_driver_nodes "$CPU_DRIVER")
    SHARED_NODES=$(kubectl get resourceslices -o json 2>/dev/null | \
        python3 -c "
import sys, json
data = json.load(sys.stdin)
mock_nodes = set()
cpu_nodes = set()
for s in data['items']:
    node = s['spec'].get('nodeName', '')
    if not node:
        continue
    if s['spec']['driver'] == '$MOCK_ACCEL_DRIVER':
        mock_nodes.add(node)
    elif s['spec']['driver'] == '$CPU_DRIVER':
        cpu_nodes.add(node)
print(len(mock_nodes & cpu_nodes))
" 2>/dev/null)
    check "drivers share nodes ($SHARED_NODES nodes with both mock-accel + cpu)" [ "$SHARED_NODES" -gt 0 ]

    # Validate partition profile includes both drivers
    PARTITION_PROFILES=$(kubectl get resourceslices -l "$LABEL_MANAGED" -o json 2>/dev/null | \
        python3 -c "
import sys, json
data = json.load(sys.stdin)
profiles = set()
for s in data['items']:
    for d in s['spec'].get('devices', []):
        attrs = d.get('attributes', {})
        profile = attrs.get('${COORDINATOR_DRIVER}/profile', {})
        if 'stringValue' in profile:
            profiles.add(profile['stringValue'])
for p in sorted(profiles):
    print(p)
" 2>/dev/null)
    echo -e "  Partition profiles: ${GREEN}${PARTITION_PROFILES}${NC}"

    # Profile name should reference both drivers when both are present
    MULTI_DRIVER_PROFILE=$(echo "$PARTITION_PROFILES" | grep -c ".*_.*" || true)
    check "partition profile reflects multiple drivers" [ "$MULTI_DRIVER_PROFILE" -gt 0 ]

    echo
fi

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

    # Validate PartitionConfig sub-resources reference mock-accel
    PARTITION_CONFIG_HAS_MOCK=$(kubectl get deviceclasses -l "$LABEL_MANAGED" -o json 2>/dev/null | \
        python3 -c "
import sys, json
data = json.load(sys.stdin)
for dc in data['items']:
    for cfg in dc['spec'].get('config', []):
        opaque = cfg.get('opaque', {})
        if opaque.get('driver') != '$COORDINATOR_DRIVER':
            continue
        params = json.loads(opaque.get('parameters', '{}'))
        for sr in params.get('subResources', []):
            if 'mock-accel' in sr.get('deviceClass', ''):
                print('yes')
                sys.exit(0)
print('no')
" 2>/dev/null)
    check "PartitionConfig references mock-accel sub-resources" [ "$PARTITION_CONFIG_HAS_MOCK" = "yes" ]

    # If CPU driver present, check PartitionConfig also references it
    if [ "$HAS_CPU_DRIVER" = true ]; then
        PARTITION_CONFIG_HAS_CPU=$(kubectl get deviceclasses -l "$LABEL_MANAGED" -o json 2>/dev/null | \
            python3 -c "
import sys, json
data = json.load(sys.stdin)
for dc in data['items']:
    for cfg in dc['spec'].get('config', []):
        opaque = cfg.get('opaque', {})
        if opaque.get('driver') != '$COORDINATOR_DRIVER':
            continue
        params = json.loads(opaque.get('parameters', '{}'))
        for sr in params.get('subResources', []):
            if 'dra.cpu' in sr.get('deviceClass', ''):
                print('yes')
                sys.exit(0)
print('no')
" 2>/dev/null)
        check "PartitionConfig references dra-driver-cpu sub-resources" [ "$PARTITION_CONFIG_HAS_CPU" = "yes" ]

        # Validate alignments include NUMA constraint spanning both drivers
        ALIGNMENT_HAS_NUMA=$(kubectl get deviceclasses -l "$LABEL_MANAGED" -o json 2>/dev/null | \
            python3 -c "
import sys, json
data = json.load(sys.stdin)
for dc in data['items']:
    for cfg in dc['spec'].get('config', []):
        opaque = cfg.get('opaque', {})
        if opaque.get('driver') != '$COORDINATOR_DRIVER':
            continue
        params = json.loads(opaque.get('parameters', '{}'))
        for al in params.get('alignments', []):
            if 'numaNode' in al.get('attribute', '') and len(al.get('requests', [])) > 1:
                print('yes')
                sys.exit(0)
print('no')
" 2>/dev/null)
        check "PartitionConfig has NUMA alignment across drivers" [ "$ALIGNMENT_HAS_NUMA" = "yes" ]
    fi
fi

echo

# --- Summary ---
total=$((pass + fail + skip))
echo -e "${GREEN}=== E2E Test Results ===${NC}"
echo -e "  Passed:  ${GREEN}$pass${NC}"
echo -e "  Failed:  ${RED}$fail${NC}"
echo -e "  Skipped: ${YELLOW}$skip${NC}"
echo -e "  Total:   $total"
echo

if [ "$fail" -gt 0 ]; then
    echo -e "${RED}E2E FAILED${NC}"
    exit 1
fi

echo -e "${GREEN}E2E PASSED${NC}"
