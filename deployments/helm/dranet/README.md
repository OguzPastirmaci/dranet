# DRANET Helm Chart

## Installation

From a local checkout:

```sh
helm upgrade --install dranet ./deployments/helm/dranet -n kube-system
```

## Configuration

The following table lists the configurable parameters and their default values:

| Parameter | Description | Default |
|-----------|-------------|---------|
| `nameOverride` | Override the chart name | `""` |
| `fullnameOverride` | Override the full release name | `""` |
| `image.repository` | Container image repository | `registry.k8s.io/networking/dranet` |
| `image.tag` | Container image tag | `stable` |
| `image.pullPolicy` | Image pull policy | `IfNotPresent` |
| `imagePullSecrets` | List of image pull secrets | `[]` |
| `rbac.create` | Create RBAC resources | `true` |
| `podAnnotations` | Annotations to add to pods | `{}` |
| `podLabels` | Labels to add to pods | `{}` |
| `logVerbosity` | Log verbosity level | `4` |
| `metricsPort` | Port for the metrics/healthz server and readiness probe | binary default: `9177` |
| `metricsPath` | HTTP path for the readiness probe | `/healthz` |
| `tolerations` | Pod tolerations | `[{operator: Exists, effect: NoSchedule}]` |
| `resources.requests.cpu` | CPU resource request | `100m` |
| `resources.requests.memory` | Memory resource request | `50Mi` |
| `resources.limits.cpu` | CPU resource limit | `""` (not set) |
| `resources.limits.memory` | Memory resource limit | `""` (not set) |
| `extraVolumeMounts` | Additional volume mounts for the DRANET container | `[]` |
| `extraVolumes` | Additional volumes for the DRANET pod | `[]` |
| `args.filter` | CEL expression to filter network interface attributes | see binary default |
| `args.inventoryMinPollInterval` | Minimum interval between two consecutive inventory polls | binary default: `2s` |
| `args.inventoryMaxPollInterval` | Maximum interval between two consecutive inventory polls | binary default: `1m` |
| `args.inventoryPollBurst` | Number of inventory polls that can be run in a burst | binary default: `5` |
| `args.moveIBInterfaces` | If true, InfiniBand (IPoIB) interfaces are moved into the pod network namespace | binary default: `true` |
| `args.cloudProviderHint` | Hint for the cloud provider plugin (`GCE`, `AZURE`, `OKE`, `AWS`, `ALIBABA`, `webhook`, `NONE`); auto-detected if unset | binary default: `""` |
| `args.profileProvider` | Provider for user profile configuration (`cloud`, `webhook`, `none`) | binary default: `cloud` |
| `args.webhookURL` | HTTP, HTTPS, or Unix socket URL for cloud and profile webhook providers | binary default: `""` |
| `args.deviceLifecycleProvider` | Provider for node-local device lifecycle hooks (`webhook`, `none`) | binary default: `none` |
| `args.deviceLifecycleWebhookURL` | HTTP, HTTPS, or Unix socket URL for the device lifecycle webhook provider | binary default: `""` |
| `args.deviceLifecycleTimeout` | Timeout for each concurrent device lifecycle webhook batch | binary default: `1s` |

> **Note:** All `args.*` fields are optional. When omitted, the flag is not passed to the binary and the binary's built-in default applies.

Parameters can be set at install time using `--set` or a custom values file:

```sh
helm upgrade --install dranet ./deployments/helm/dranet -n kube-system --set logVerbosity=6
helm upgrade --install dranet ./deployments/helm/dranet -n kube-system -f my-values.yaml
```

### Native OKE with independent webhook providers

The cloud, profile, and device lifecycle providers can be selected independently.
This example keeps native OKE hardware discovery, uses a profile webhook over
HTTP, and uses a separate node-local lifecycle webhook over a Unix socket:

```yaml
args:
  cloudProviderHint: OKE
  profileProvider: webhook
  webhookURL: http://127.0.0.1:18081
  deviceLifecycleProvider: webhook
  deviceLifecycleWebhookURL: unix:///var/run/dranet-lifecycle/provider.sock
  deviceLifecycleTimeout: 1s

extraVolumeMounts:
  - name: device-lifecycle-webhook
    mountPath: /var/run/dranet-lifecycle

extraVolumes:
  - name: device-lifecycle-webhook
    hostPath:
      path: /var/run/dranet-lifecycle
      type: DirectoryOrCreate
```

Mount the socket's parent directory rather than the socket file. This allows the
provider to create or replace the socket after the DRANET pod starts. The
provider must mount the same host directory and must run on the same node.
