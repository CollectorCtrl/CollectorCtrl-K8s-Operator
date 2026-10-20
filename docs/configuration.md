# Configuration

There are three ways to configure the operator:

- **Helm values** apply to the operator as a whole.
- **Labels and annotations** go on each collector workload.
- A **`CollectorMonitor`** resource is for collectors whose labels you cannot change.

## Helm values

| Value | Default | Description |
| --- | --- | --- |
| `opamp.server` | `wss://collectorctrl.corp.internal:4320/v1/opamp` | CollectorCtrl OpAMP endpoint. Must be `wss://`. Every connection uses it |
| `opamp.authMode` | `enroll` | `enroll` (recommended) or `legacy`, which is only for migrating from 0.6 and needs server legacy mode |
| `opamp.enrollmentSecret` | `""` | Existing Secret with key `enrollment-token` (`cce_…`). Required in `enroll` mode |
| `opamp.existingSecret` | `""` | Legacy mode only: existing Secret with key `secret-key` |
| `opamp.secretKey` | `""` | Legacy mode only: inline key (not for production) |
| `opampCAConfigMap` | `""` | ConfigMap with key `ca.crt`, added to the trusted roots |
| `clusterID` | `""` | Stable cluster identity. Empty uses the `kube-system` namespace UID. Required when `rbac.clusterScoped=false`. Never change it after install |
| `clusterName` | `""` | Display name in CollectorCtrl (defaults to the cluster ID) |
| `clusterObserver.enabled` | `true` | One extra connection that lists collector-like workloads by image name, so the UI can suggest what to opt in |
| `rbac.clusterScoped` | `true` | `true` watches all namespaces; `false` watches only the release namespace |
| `rbac.authSecretNames` | `[]` | Secret names the operator may `get`, for monitors that use `auth.secretRef`. Empty grants no Secret access |
| `credentialStorage.existingClaim` | `""` | Reuse an existing PVC for credentials |
| `credentialStorage.size` | `128Mi` | Size of the created PVC |
| `credentialStorage.storageClassName` | `null` | Storage class (`null` uses the cluster default) |
| `image.repository` | `ghcr.io/collectorctrl/collectorctrl-k8s-operator/operator` | Operator image |
| `image.tag` | `""` | Empty uses the chart `appVersion` |
| `image.pullPolicy` | `IfNotPresent` | Use `Always` with `main` or `latest` |
| `imagePullSecrets` | `[]` | Pull secrets for a private mirror |
| `replicaCount` | `1` | Must stay 1 (single credential writer) |
| `leaderElection.enabled` | `false` | Leave disabled; only adds a Lease and its RBAC |
| `resources` | 100m / 128Mi requests, 500m / 256Mi limits | Container resources |
| `serviceMonitor.enabled` | `false` | Create a Prometheus Operator ServiceMonitor for `:8080/metrics` |
| `serviceMonitor.interval`, `scrapeTimeout`, `labels` | `30s`, `10s`, `{}` | ServiceMonitor settings |
| `podSecurityContext`, `securityContext` | non-root 65532, RuntimeDefault seccomp, no capabilities, read-only root | Hardened by default; see `values-openshift.yaml` for OpenShift |
| `nodeSelector`, `tolerations`, `affinity` | empty | Scheduling |
| `serviceAccount.create`, `name`, `annotations` | `true`, generated, `{}` | Service account |

## Environment variables

The chart sets these values. You only need them when you run the binary yourself, for example with `make run`.

| Variable | Description |
| --- | --- |
| `OPAMP_SERVER` | Required. `wss://` endpoint |
| `OPAMP_AUTH_MODE` | `enroll` (default) or `legacy` |
| `OPAMP_ENROLL_TOKEN` | Enrollment token, in `enroll` mode |
| `OPAMP_SECRET_KEY` | Shared key, in `legacy` mode |
| `OPAMP_CREDENTIAL_DIRECTORY` | Where per-identity credentials are stored. Must be persistent |
| `OPAMP_CA_FILE` | Optional PEM bundle added to the trusted roots |
| `CLUSTER_ID` | Cluster identity. Empty reads the `kube-system` UID |
| `CLUSTER_NAME` | Display name |
| `CLUSTER_OBSERVER` | `false` disables the cluster connection |
| `WATCH_NAMESPACE` | Restrict watches to one namespace |
| `POD_NAMESPACE` | The operator's own namespace (set from the downward API) |

Flags: `--metrics-bind-address` (default `:8080`), `--health-probe-bind-address` (default `:8081`, which serves
`/healthz` and `/readyz`) and `--leader-elect`.

## Opting a collector in

Put the label on the **workload** (Deployment, DaemonSet or StatefulSet), not on the pod template:

```yaml
metadata:
  labels:
    collectorctrl.io/observe: "true"
```

To stop observing a collector, remove the label. The connection closes, and the entry goes offline in
CollectorCtrl.

### Annotations

All are optional, and all go on the workload.

| Annotation | Example | Effect |
| --- | --- | --- |
| `collectorctrl.io/container` | `otel-collector` | Collector container in a multi-container pod |
| `collectorctrl.io/config-map` | `otel-agent-config` | ConfigMap to read when several are mounted |
| `collectorctrl.io/config-key` | `relay.yaml` | Key to read when it cannot be chosen automatically |
| `collectorctrl.io/stack` | `payments-telemetry` | Group name in CollectorCtrl. Overrides the Helm release or Argo CD app |
| `collectorctrl.io/role` | `gateway` | Overrides the inferred role: `agent`, `gateway`, `cluster-receiver` or `all-in-one` |
| `collectorctrl.io/report-config` | `"true"` | Also upload the full collector YAML. Leave it off if the config contains inline credentials |
| `collectorctrl.io/report-interval` | `60s` | How often to report (Go duration, 5 s to 1 h, default 30 s) |

[architecture.md](architecture.md#inference-rules) explains how the role, stack and topology are inferred when you
don't set them.

### Through vendor Helm charts

Most collector charts accept extra workload labels and annotations. The exact value names differ by chart, so check
its `values.yaml`. With the upstream `opentelemetry-collector` chart, for example:

```yaml
additionalLabels:
  collectorctrl.io/observe: "true"
annotations:
  collectorctrl.io/stack: platform-telemetry
```

## CollectorMonitor resource

Use a `CollectorMonitor` when you cannot change the collector workload, for example when a vendor operator owns it. It
lives in the collector's namespace and takes precedence over the label for the workload it selects.

```yaml
apiVersion: collectorctrl.io/v1alpha1
kind: CollectorMonitor
metadata:
  name: splunk-agent
  namespace: observability
spec:
  workloadSelector:              # must match exactly one workload
    kind: DaemonSet              # DaemonSet | Deployment | StatefulSet (optional)
    name: splunk-otel-collector-agent   # optional
    matchLabels:
      app.kubernetes.io/name: splunk-otel-collector
  collectorContainer: otel-collector    # required for multi-container pods
  configMapSelector:             # optional; only when several ConfigMaps or keys are mounted
    name: splunk-otel-collector-otel-agent
    key: relay
  opampServer: wss://collectorctrl.example.com:4320/v1/opamp   # must equal the operator's opamp.server
  reportConfig: false
  healthCheck:
    interval: 30s                # report interval, 5 s to 1 h
```

| Field | Required | Description |
| --- | --- | --- |
| `workloadSelector.matchLabels` | yes | Labels of the collector workload |
| `workloadSelector.kind` | no | Restrict to one workload kind |
| `workloadSelector.name` | no | Exact workload name |
| `collectorContainer` | multi-container pods | Container to inspect |
| `configMapSelector.name`, `.matchLabels`, `.key` | no | Disambiguate the mounted config |
| `opampServer` | yes | Must match the operator's endpoint exactly; any other value is rejected |
| `auth.secretRef` | no | Per-monitor enrollment Secret in the same namespace (`name`, `key`). The operator can read it only if it is listed in `rbac.authSecretNames`. By default the operator's own token is used |
| `reportConfig` | no | Upload the full YAML (default `false`) |
| `healthCheck.interval` | no | Report interval (default `30s`) |
| `healthCheck.enabled`, `healthCheck.metricsPort` | no | Reserved |
| `driftDetection` | no | Reserved; runtime verification is always reported as unknown |
| `emergencyMode` | no | Deprecated; server commands are always rejected |
| `enrichWithNodeMetadata` | no | Reserved |

Status:

```bash
kubectl get collectormonitors -A
kubectl -n observability describe collectormonitor splunk-agent
```

| `status` field | Meaning |
| --- | --- |
| `phase` | `Pending`, `Active`, `Error` or `Disconnected` |
| `agentCount`, `healthyAgents` | Pods owned by the workload, and how many are ready |
| `configMapRef` | The ConfigMap and key being read |
| `lastHeartbeat` | Last observation sent while connected |
| `conditions` | Reason and message for the current phase |

More examples are in [`deploy/example-collectormonitor.yaml`](../deploy/example-collectormonitor.yaml) and
[`deploy/coralogix-collectormonitor.yaml`](../deploy/coralogix-collectormonitor.yaml).
