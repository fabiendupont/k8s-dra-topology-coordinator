# Node Partition Topology Coordinator

A Kubernetes controller and mutating webhook for [Dynamic Resource Allocation (DRA)](https://kubernetes.io/docs/concepts/scheduling-eviction/dynamic-resource-allocation/) that lets users request a logical partition of a node — like "a quarter of an HGX B200" — without knowing anything about DRA drivers, device attributes, or topology constraints.

## The Problem

AI/HPC workloads need GPUs, NICs, CPUs, and memory co-located on the same NUMA boundary. Cross-NUMA device placement can degrade throughput by 30–50% and prevent GPU Direct RDMA entirely.

Kubernetes DRA allocates each resource type independently. A GPU may land on NUMA node 0 while its NIC lands on NUMA node 1, with no mechanism to prevent this. Writing correct claims requires knowing:

- Which DRA drivers are installed and what attributes they publish
- That driver A calls it `gpu.amd.com/numaNode` while driver B calls it `dra.cpu/numaNodeID`
- How to wire `matchAttribute` constraints across multiple device requests
- Which attribute values to hard-code for cross-driver alignment

Users shouldn't need to know any of this.

## What This Project Does

The coordinator is an **abstraction layer**. Users request a partition by name:

```yaml
apiVersion: resource.k8s.io/v1
kind: ResourceClaim
spec:
  devices:
    requests:
    - name: my-partition
      deviceClassName: hgx-b200-quarter
      count: 1
```

The coordinator automatically expands this into the complex multi-device claim that the scheduler needs — with the right device classes, counts, and alignment constraints for the specific hardware in the cluster:

```
User creates:                        Webhook expands to:
┌──────────────────────────┐         ┌─────────────────────────────────┐
│ ResourceClaim            │         │ ResourceClaim                   │
│   requests:              │         │   requests:                     │
│   - name: my-partition   │  ────►  │   - name: my-partition-gpu      │
│     deviceClassName:     │         │     deviceClassName: gpu.nvidia │
│       hgx-b200-quarter   │         │     count: 2                    │
│     count: 1             │         │   - name: my-partition-rdma     │
└──────────────────────────┘         │     deviceClassName: rdma.mlnx  │
                                     │     count: 1                    │
                                     │   constraints:                  │
                                     │   - matchAttribute: numaNode    │
                                     │     requests: [my-partition-gpu,│
                                     │       my-partition-rdma]        │
                                     └─────────────────────────────────┘
```

The expanded claim's `matchAttribute` constraints serve double duty:

- **Device alignment** — the allocator only picks devices that share the same NUMA node
- **Node filtering** — the scheduler rejects nodes where no NUMA node has all the required devices available

No scheduler plugin is needed. The standard kube-scheduler handles everything.

## Partition Types

The coordinator discovers hardware topology automatically and publishes DeviceClasses for each partition granularity:

| Partition | Scope | Example |
|-----------|-------|---------|
| **eighth** | One PCIe root complex | 1 GPU + 1 NIC |
| **quarter** | One NUMA node | 2 GPUs + 2 NICs |
| **half** | One CPU socket | 4 GPUs + 4 NICs |
| **full** | Entire node | 8 GPUs + 8 NICs |

DeviceClass names are generated from the hardware profile (e.g., `gpu-nvidia-com-8-rdma-mellanox-com-8-quarter`). Users pick the partition size that matches their workload.

## Soft Affinity

Not all constraints need to be hard. Topology rules support two enforcement modes:

| Mode | Behavior |
|------|----------|
| `required` (default) | Constraint is always emitted. Pod fails to schedule if unsatisfiable. |
| `preferred` | Constraint is emitted only when the topology model confirms it can be satisfied. If no node can provide alignment, the constraint is dropped and the pod schedules without it. |

This lets administrators express policies like "enforce PCIe alignment, prefer NUMA alignment":

```yaml
# PCIe alignment rule — hard constraint
apiVersion: v1
kind: ConfigMap
metadata:
  name: pcie-rule
  labels:
    nodepartition.dra.k8s.io/topology-rule: "true"
data:
  attribute: resource.kubernetes.io/pcieRoot
  type: string
  driver: mock-accel.example.com
  constraint: match
  enforcement: required

---
# NUMA alignment rule — best-effort
apiVersion: v1
kind: ConfigMap
metadata:
  name: numa-rule
  labels:
    nodepartition.dra.k8s.io/topology-rule: "true"
data:
  attribute: mock-accel.example.com/numaNode
  type: int
  driver: mock-accel.example.com
  mapsTo: numaNode
  partitioning: group
  constraint: match
  enforcement: preferred
```

The webhook checks the current cluster topology at claim expansion time. If NUMA alignment is achievable on at least one node, the `matchAttribute` constraint is emitted. If not, it's skipped — the pod schedules with PCIe alignment only.

## Architecture

```
┌────────────────────────────────────────────────────────────────┐
│                      Kubernetes API                            │
│                                                                │
│  ResourceSlices ◄── GPU driver, NIC driver, CPU driver, etc.   │
│  DeviceClasses  ◄── Topology Coordinator (controller)          │
│  ResourceClaims ◄── User creates → webhook mutates → scheduler │
└──────┬───────────────────┬───────────────────────┬─────────────┘
       │ watch             │ publish               │ mutate
       ▼                   ▼                       ▼
┌────────────────────────────────────────────────────────────────┐
│              Topology Coordinator (single binary)              │
│                                                                │
│  ┌─────────────┐  ┌──────────────┐       ┌──────────────────┐  │
│  │  Topology   │  │  DeviceClass │       │    Webhook       │  │
│  │   Model     ├──►  Manager     │       │ (claim expander) │  │
│  │  + Rules    │  │              │       │                  │  │
│  └──────▲──────┘  └──────────────┘       └────────┬─────────┘  │
│         │                                         │            │
│  watches ResourceSlices              reads DeviceClass         │
│  watches ConfigMaps                  PartitionConfig           │
│  (leader election)                   checks topology model     │
│                                      (all replicas)            │
└────────────────────────────────────────────────────────────────┘
```

The controller and webhook run in the same binary:
- **Controller** (leader-only): watches ResourceSlices from all DRA drivers, builds a cross-driver topology model, computes aligned partitions, and publishes DeviceClasses with embedded `PartitionConfig`
- **Webhook** (all replicas): intercepts ResourceClaim creation, reads `PartitionConfig` from DeviceClass, evaluates preferred constraints against the topology model, and expands into multi-request claims with alignment constraints

## Topology Rules

Topology rules are defined via ConfigMaps with the `nodepartition.dra.k8s.io/topology-rule: "true"` label. They bridge the gap between vendor-specific driver attributes and the coordinator's topology model, so the coordinator works with any DRA driver without requiring standardized attribute names.

Rules can:
- **Map** driver-specific attributes to standard topology attributes (`mapsTo: numaNode|pcieRoot|socket`)
- **Group** devices by vendor-specific attributes for finer-grained partitioning
- **Constrain** expanded claims with `matchAttribute` alignment
- **Control enforcement** — hard (`required`) or best-effort (`preferred`)

### Example: Mock-Accel DRA Driver

Map driver-specific `numaNode` to the coordinator's standard NUMA attribute:

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: mock-accel-numa-rule
  labels:
    nodepartition.dra.k8s.io/topology-rule: "true"
data:
  attribute: mock-accel.example.com/numaNode
  type: int
  driver: mock-accel.example.com
  mapsTo: numaNode
  partitioning: group
```

### Example: NVIDIA NVLink Domain Grouping

Group GPUs by NVLink domain so partitions respect NVLink connectivity:

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: nvlink-rule
  labels:
    nodepartition.dra.k8s.io/topology-rule: "true"
data:
  attribute: gpu.nvidia.com/nvlinkDomain
  type: int
  driver: gpu.nvidia.com
  mapsTo: ""
  partitioning: group
  constraint: match
```

### Rule Fields

| Field | Required | Values | Description |
|-------|----------|--------|-------------|
| `attribute` | yes | qualified name | Device attribute to read (e.g., `gpu.nvidia.com/nvlinkDomain`) |
| `type` | yes | `int`, `string`, `bool` | Attribute value type |
| `driver` | yes | driver name | DRA driver that publishes this attribute |
| `mapsTo` | no | `numaNode`, `pcieRoot`, `socket` | Map to standard topology attribute |
| `partitioning` | no | `group`, `info` | How attribute affects partition grouping (default: `info`) |
| `constraint` | no | `match`, `none` | Whether expanded claims must match on this attribute (default: `none`) |
| `enforcement` | no | `required`, `preferred` | Whether constraint is hard or best-effort (default: `required`) |

## Driver Compatibility

The coordinator works with any DRA driver that publishes topology attributes in its ResourceSlices. Topology rules map driver-specific attribute names to the coordinator's standard model.

### What Drivers Need to Publish

For NUMA-aware partitioning, a driver must publish at least one attribute that identifies which NUMA node a device belongs to. The attribute name and format don't matter — topology rules handle the mapping.

### Known DRA Drivers

| Driver | NUMA Attribute | PCIe Attribute | Status |
|--------|---------------|----------------|--------|
| [mock-device](https://github.com/fabiendupont/mock-device) | `mock-accel.example.com/numaNode` | `mock-accel.example.com/pciAddress` | Works today (test driver) |
| [dra-driver-cpu](https://github.com/kubernetes-sigs/dra-driver-cpu) | `dra.cpu/numaNodeID` | N/A | Works today (individual mode) |
| [AMD GPU DRA](https://github.com/ROCm/k8s-gpu-dra-driver) | `gpu.amd.com/numaNode` | `resource.kubernetes.io/pcieRoot` | Works today |
| [NVIDIA GPU DRA](https://github.com/NVIDIA/k8s-dra-driver-gpu) | VFIO type only (`gpu.nvidia.com/numa`) | `resource.kubernetes.io/pcieRoot` | Blocked: no NUMA for standard GPU/MIG types |
| [dra-driver-memory](https://github.com/kad/dra-driver-memory) | `dra.memory/numaNode` | N/A | Early development |
| [SR-IOV NIC DRA](https://github.com/k8snetworkplumbingwg/sriov-network-device-plugin) | `dra.net/numaNode` | `resource.kubernetes.io/pciBusID` | Works today |

### Upstream Standardization

There is no `resource.kubernetes.io/numaNode` standard attribute today — each driver uses its own name. The coordinator's topology rules bridge this gap. KEP-5491 proposes standardizing NUMA as a list-typed attribute, which would simplify rules but is not yet merged. The coordinator will continue to add value after standardization through the partition abstraction and automatic claim expansion.

## Prerequisites

- Go 1.25+
- Kubernetes 1.34+ (with DRA enabled)

## Build

```sh
make build         # produces bin/nodepartition-controller
make test          # run tests (unit + envtest integration + property)
make test-coverage # run tests with HTML coverage report
make lint          # run golangci-lint
```

## Container Image

```sh
docker build -t nodepartition-controller .
```

## Configuration

| Flag | Default | Description |
|------|---------|-------------|
| `--kubeconfig` | *(in-cluster)* | Path to kubeconfig file |
| `--driver-name` | `nodepartition.dra.k8s.io` | Coordinator identifier for DeviceClass labels |
| `--shutdown-timeout` | `30s` | Graceful shutdown timeout |
| `--leader-election-namespace` | `kube-system` | Namespace for leader election lease |
| `--leader-election-id` | `nodepartition-controller` | Leader election lease name |
| `--webhook-port` | `9443` | Port for the mutating webhook HTTPS server |
| `--tls-cert` | `/etc/webhook/tls/tls.crt` | Path to TLS certificate |
| `--tls-key` | `/etc/webhook/tls/tls.key` | Path to TLS private key |

## Helm

```sh
helm install nodepartition deploy/helm/nodepartition
```

### Webhook TLS

The webhook requires TLS. Three modes are supported:

| Mode | `webhook.tls.mode` | How it works |
|------|--------------------|-------------|
| **cert-manager** | `cert-manager` (default) | Creates a Certificate resource; cert-manager provisions the TLS Secret |
| **OpenShift** | `openshift` | Annotates the Service for OpenShift serving CA auto-provisioning |
| **Manual** | `manual` | User provides a TLS Secret and sets `caBundle` |

See `deploy/helm/nodepartition/values.yaml` for all configurable values.

## Observability

Prometheus metrics at `:8081/metrics`:

| Metric | Type | Description |
|--------|------|-------------|
| `nodepartition_controller_reconciliation_duration_seconds` | Histogram | Reconciliation cycle duration |
| `nodepartition_controller_reconciliation_errors_total` | Counter | Reconciliation errors |
| `nodepartition_controller_nodes_total` | Gauge | Nodes with topology information |
| `nodepartition_controller_deviceclasses_total` | Gauge | Managed DeviceClasses |
| `nodepartition_controller_topology_rules_total` | Gauge | Active topology rules |
| `nodepartition_webhook_expansions_total` | Counter | Partition claims expanded |
| `nodepartition_webhook_errors_total` | Counter | Webhook errors |

Health check at `:8081/healthz`.

## E2E Testing

See [test/e2e/README.md](test/e2e/README.md) for end-to-end testing with the [mock-device](https://github.com/fabiendupont/mock-device) DRA driver.

## Contributing

1. Fork the repository
2. Create a feature branch
3. Run `make dev` (format, vet, lint, test, build)
4. Submit a pull request

All commits must include a `Signed-off-by` line (`git commit -s`).

## License

Apache 2.0 — see [LICENSE.header](LICENSE.header).
