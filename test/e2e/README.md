# E2E Tests

End-to-end tests for the Node Partition Topology Coordinator, using the
[mock-device](https://github.com/fabiendupont/mock-device) DRA driver as
the device provider.

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
3. `kubectl` and `helm` available and configured
4. The coordinator container image built and available to the cluster

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
1. Verify mock-accel ResourceSlices are present
2. Deploy topology rule ConfigMaps for mock-accel attribute mapping
3. Deploy the coordinator via Helm
4. Wait for coordinator to publish partition ResourceSlices
5. Validate partition types (eighth, quarter, full), node coverage,
   device counts, and DeviceClass creation
6. Clean up all deployed resources

## What Gets Validated

| Check | Description |
|-------|-------------|
| ResourceSlice driver | Published under `nodepartition.dra.k8s.io` |
| Partition types | eighth, quarter, full partitions created |
| Node coverage | All nodes with mock-accel devices have partitions |
| Device counts | `deviceCount_*` attributes show mock-accel device counts per partition |
| DeviceClasses | Created with CEL selectors and opaque partition config |
| Topology mapping | mock-accel `numaNode`/`pciAddress` mapped via topology rules |

## Topology Rules

The `topology-rules.yaml` file contains ConfigMaps that teach the coordinator
how to read mock-accel's driver-specific topology attributes:

- `mock-accel.example.com/numaNode` → standard `numaNode` (NUMA grouping)
- `mock-accel.example.com/pciAddress` → standard `pcieRoot` (PCIe grouping)
