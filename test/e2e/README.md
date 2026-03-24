# E2E Tests

End-to-end tests for the Node Partition Topology Coordinator, testing
cross-driver topology partitioning with:

- [mock-device](https://github.com/fabiendupont/mock-device) — mock accelerator DRA driver (required)
- [dra-driver-cpu](https://github.com/kubernetes-sigs/dra-driver-cpu) — CPU DRA driver (optional)

When both drivers are present, the tests validate that partitions contain
devices from both drivers grouped by shared NUMA topology.

## Quick Start (mock-device Vagrant cluster)

If you have the mock-device Vagrant cluster running with the DRA driver deployed:

```sh
# From the mock-device directory
cd ../mock-device/vagrant
vagrant provision --provision-with nodepartition

# Or manually from this repo
./test/e2e/run-e2e.sh
```

## Manual Setup

### Prerequisites

1. A Kubernetes 1.34+ cluster with DRA enabled
2. The mock-accel DRA driver deployed and publishing ResourceSlices
3. (Optional) The dra-driver-cpu deployed in individual mode (`--cpu-device-mode=individual`)
4. `kubectl` and `helm` available and configured
5. The coordinator container image built and available to the cluster

### Deploy dra-driver-cpu (optional)

The CPU DRA driver must run in **individual mode** so each CPU appears as
a separate device with NUMA and socket attributes:

```sh
# Clone and deploy (see https://github.com/kubernetes-sigs/dra-driver-cpu)
# Ensure --cpu-device-mode=individual is set in the DaemonSet args
```

Verify it publishes ResourceSlices:

```sh
kubectl get resourceslices -o json | \
  python3 -c "import sys,json; [print(s['metadata']['name']) for s in json.load(sys.stdin)['items'] if s['spec']['driver']=='dra.cpu']"
```

### Build the coordinator image

```sh
make build
docker build -t ghcr.io/fabiendupont/nodepartition-controller:dev .
```

For k3s clusters, import the image directly:

```sh
docker save ghcr.io/fabiendupont/nodepartition-controller:dev | \
  ssh <node> sudo k3s ctr images import -
```

### Run the tests

```sh
./test/e2e/run-e2e.sh
```

The script will:
1. Detect available DRA drivers (mock-accel required, dra-driver-cpu optional)
2. Deploy topology rule ConfigMaps for all detected drivers
3. Deploy the coordinator via Helm
4. Wait for coordinator to publish partition ResourceSlices
5. Validate partition types (eighth, quarter, full), node coverage,
   device counts, and DeviceClass creation
6. If dra-driver-cpu is present, validate cross-driver partitioning:
   partitions include both mock-accel and CPU devices, profiles reflect
   multiple drivers, and NUMA alignment constraints span both drivers
7. Clean up all deployed resources

## What Gets Validated

### Single-driver checks (always run)

| Check | Description |
|-------|-------------|
| ResourceSlice driver | Published under `nodepartition.dra.k8s.io` |
| Partition types | eighth, quarter, full partitions created |
| Node coverage | All nodes with mock-accel devices have partitions |
| Device counts (mock-accel) | `deviceCount_*` attributes include mock-accel counts |
| DeviceClasses | Created with CEL selectors and opaque partition config |
| PartitionConfig sub-resources | Opaque config references mock-accel |

### Cross-driver checks (when dra-driver-cpu is present)

| Check | Description |
|-------|-------------|
| Device counts (cpu) | `deviceCount_*` attributes include dra-driver-cpu counts |
| Shared nodes | Both drivers publish ResourceSlices on the same nodes |
| Multi-driver profile | Partition profile name reflects both drivers |
| PartitionConfig sub-resources (cpu) | Opaque config references dra-driver-cpu |
| NUMA alignment | Alignment constraints span requests from both drivers |

## Topology Rules

The `topology-rules.yaml` file contains ConfigMaps that teach the coordinator
how to read each driver's topology attributes:

### mock-accel

- `mock-accel.example.com/numaNode` → standard `numaNode` (NUMA grouping)
- `mock-accel.example.com/pciAddress` → standard `pcieRoot` (PCIe grouping)

### dra-driver-cpu

- `dra.cpu/numaNodeID` → standard `numaNode` (NUMA grouping)
- `dra.cpu/socketID` → standard `socket` (socket grouping)
