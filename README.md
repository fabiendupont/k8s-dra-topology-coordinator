# Node Partition Topology Coordinator

A Kubernetes topology-aware claim expander for [Dynamic Resource Allocation (DRA)](https://kubernetes.io/docs/concepts/scheduling-eviction/dynamic-resource-allocation/). It watches ResourceSlices from all DRA drivers, computes aligned partition combinations, publishes DeviceClasses, and expands simple partition claims into multi-request claims with topology alignment constraints via a mutating webhook.

## How It Works

Users request a single partition (e.g., "give me a quarter of an HGX node"). The coordinator expands this into the complex multi-device claim with alignment constraints that the scheduler needs:

```
User creates:                        Webhook expands to:
┌──────────────────────────┐         ┌─────────────────────────────────┐
│ ResourceClaim            │         │ ResourceClaim                   │
│   requests:              │         │   requests:                     │
│   - name: partition      │  ────►  │   - name: partition-gpu         │
│     deviceClassName:     │         │     deviceClassName: gpu.nvidia │
│       hgx-b200-quarter   │         │     count: 2                    │
│     count: 1             │         │   - name: partition-rdma        │
└──────────────────────────┘         │     deviceClassName: rdma.mlnx  │
                                     │     count: 1                    │
                                     │   constraints:                  │
                                     │   - matchAttribute: numaNode    │
                                     │     requests: [partition-gpu,   │
                                     │       partition-rdma]           │
                                     └─────────────────────────────────┘
```

## Architecture

```
┌────────────────────────────────────────────────────────────────┐
│                      Kubernetes API                            │
│                                                                │
│  ResourceSlices ◄── GPU driver, NIC driver, etc.               │
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
│  (leader election)                   (all replicas)            │
└────────────────────────────────────────────────────────────────┘
```

The controller and webhook run in the same binary:
- **Controller** (leader-only): watches ResourceSlices, builds topology model, publishes DeviceClasses with `PartitionConfig`
- **Webhook** (all replicas): intercepts ResourceClaim creation, reads `PartitionConfig` from DeviceClass, expands into multi-request claims

## Topology Rules

Topology rules are defined via ConfigMaps with the `nodepartition.dra.k8s.io/topology-rule: "true"` label. Rules can:

- **Map** driver-specific attributes to standard topology attributes (`mapsTo: numaNode|pcieRoot|socket`), so the coordinator works with any DRA driver without requiring standardized attribute names
- **Group** devices by vendor-specific attributes for finer-grained partitioning
- **Constrain** expanded claims with `matchAttribute` alignment

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
