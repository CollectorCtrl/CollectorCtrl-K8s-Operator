# CollectorCtrl Kubernetes observer

CollectorCtrl observes OpenTelemetry collectors deployed through your existing
Helm charts, upstream operators, GitOps controllers, or deployment pipelines.
It does not replace those tools or change their workloads.

This checkout implements the first observation milestone. Build an image from
this checkout; existing published images may still contain the older prototype.
Deploy the matching CollectorCtrl server and frontend before onboarding.

## Current behavior

- One CollectorMonitor selects exactly one Deployment, DaemonSet, or StatefulSet.
  Ambiguous selectors report an error instead of selecting an arbitrary workload.
- The observer resolves ConfigMaps mounted by the selected collector container,
  including projected volumes. Multiple candidates require an explicit selection.
- Sidecars require collectorContainer; the application container is not inspected.
- Pods are attributed through owner UIDs, including Deployment -> ReplicaSet.
- Reports include desired/ready instances, image, restarts, waiting/termination
  reason, ConfigMap reference, resource version, hash, and observation time.
- Configuration YAML is not uploaded unless reportConfig is explicitly enabled.
  Review inline credentials and sensitive values before opting in. Secret-backed
  configuration is not resolved or uploaded.
- Observations use the versioned collectorctrl/k8s_observation_v1 custom OpAMP
  message. They are not reported as runtime EffectiveConfig.
- Runtime configuration activation and telemetry delivery remain unverified.
- Server connections require wss:// and certificate verification. A private CA
  bundle can be supplied; disabling verification is not supported.
- Emergency commands, restarts, package changes, and direct configuration pushes
  are unsupported. emergencyMode.enabled is deprecated and has no control effect.
- The observer writes only CollectorMonitor status, never collector workloads,
  ConfigMaps, or application pods.

## Install

Build and publish a matching image through your normal release pipeline. For a
local development registry:

```sh
docker build -f Dockerfile.operator -t YOUR_REGISTRY/collectorctrl-observer:observe-dev .
docker push YOUR_REGISTRY/collectorctrl-observer:observe-dev

helm upgrade --install collectorctrl-observer ./helm-charts/collectorctrl-operator \
  --namespace collectorctrl-system --create-namespace \
  --set image.repository=YOUR_REGISTRY/collectorctrl-observer \
  --set image.tag=observe-dev \
  --set opamp.server=wss://collectorctrl.example.internal:4320/v1/opamp \
  --set opamp.enrollmentSecret=collectorctrl-auth
```

Create the authentication Secret in the operator namespace using your secret
management workflow. Its `enrollment-token` entry must contain a `cce_` token
created in Settings > API Tokens > Agent Enrollment. Each new workload/container
identity consumes one enrollment use; allow enough uses for the monitored fleet.
The kubelet supplies this Secret as an environment variable; the observer does
not need API access to read it. Secure enrollment works with the server's default
legacy mode disabled.

The chart provisions a retained credential PVC by default. A provisionable storage
class is required, or set `credentialStorage.existingClaim` to a writable existing
claim. Credentials are bound to the server endpoint and workload identity, saved
before acknowledgement, and reused on reconnect and process restart. Keep the
claim across upgrades and reinstallations. For a retained claim after uninstall,
set `credentialStorage.existingClaim` to its original name when reinstalling.
The chart supports one replica with a Recreate strategy to prevent concurrent
credential writers. OpenShift's values overlay leaves UID and fsGroup assignment
to the cluster's security policy.

Enrollment token expiry affects new workload identities, not already enrolled
ones. Update the Secret and roll the operator when rotating the environment-based
token. Revoked identities never fall back to enrollment. Recovery from lost
credential storage or explicit revocation requires an administrator to reset the
identity under Agent Enrollment and remove only that identity's saved credential
before enrolling it again; restoring a valid credential-volume backup is preferable
when no revocation occurred. Do not remove the whole credential volume to recover
one workload.

Existing shared-secret deployments must explicitly set `opamp.authMode=legacy`
and `opamp.existingSecret` (key `secret-key`) while the server allows legacy mode.
The operator persists the issued per-agent credentials during this migration;
switch to `enroll` mode and disable server legacy mode afterwards. New deployments
should use enrollment directly.

Cluster-scoped installations discover a stable cluster ID from the kube-system
namespace UID. Namespace-scoped installations require rbac.clusterScoped=false
and clusterID set to a unique, stable ID; their watch cache is scoped to the
release namespace. Reuse the same ID across reinstalls of the same cluster and
use distinct IDs across clusters.

For an internal CA, create a ConfigMap containing ca.crt in the operator namespace
and set opampCAConfigMap to its name. The certificate must cover the endpoint
hostname. Installations previously relying on skipped verification must configure
trust before upgrading.

## Monitor an existing collector

```yaml
apiVersion: collectorctrl.io/v1alpha1
kind: CollectorMonitor
metadata:
  name: existing-gateway
  namespace: observability
  labels:
    k8s.cluster.name: production-eu
spec:
  workloadSelector:
    kind: Deployment
    name: otel-gateway
    matchLabels: {}
  collectorContainer: otel-collector
  opampServer: wss://collectorctrl.example.internal:4320/v1/opamp
  reportConfig: false
  healthCheck:
    interval: 30s
```

Use an existing workload and container name. A single-container workload can omit
collectorContainer. If discovery finds multiple mounted ConfigMaps or YAML keys,
add configMapSelector.name and configMapSelector.key. Keys excluded by volume
items are rejected. This release does not reconstruct command-line config merges,
environment expansion, secret providers, remote config providers, or subPath
runtime update behavior.

Authentication defaults to the operator's OPAMP_ENROLL_TOKEN environment variable.
`OPAMP_CREDENTIAL_DIRECTORY` must point to persistent writable storage (the chart
configures it). `spec.opampServer` must exactly match the administrator-configured
`OPAMP_SERVER`; a monitor cannot route credentials to another endpoint. Offered
server connection changes are also rejected. Per-monitor `auth.secretRef` is
optional and must be in the monitor's own namespace; grant API get permission only
for the required names through `rbac.authSecretNames`. Its default key is
`enrollment-token`, or `secret-key` in explicit legacy mode. Remove an old explicit
`secret-key` field when switching an existing monitor to enrollment.

Inspect discovery and transport errors with:

```sh
kubectl get collectormonitors -A
kubectl describe collectormonitor existing-gateway -n observability
```

The Kubernetes view distinguishes observer connectivity, container readiness, and
unverified telemetry delivery. Freshness checks both source observation age and
server receipt age; delayed messages do not become current on arrival. The budget
is at least two minutes, or twice the configured reporting interval plus thirty
seconds, with thirty seconds of source clock-skew tolerance. Reporting intervals
are clamped to five seconds through one hour. Older or replayed observations cannot
replace newer evidence, including after a server reconnect.
LastHeartbeat records an observation queued with a connected transport, not a
server acknowledgement.

## Compatibility and limits

Observation works at the rendered workload level, independent of the chart that
created it. It does not establish which Git file owns that workload, promise
compatibility with every collector distribution, or automatically discover every
collector in the cluster. Create one monitor per workload. A sidecar injected into
a pod without a corresponding workload template ConfigMap mount needs a future
upstream-operator adapter; it is not supported by template discovery.

healthCheck.metricsPort, healthCheck.enabled, driftDetection, and
enrichWithNodeMetadata remain reserved legacy fields. This release always observes
container readiness at healthCheck.interval; it does not scrape collector metrics,
verify pod annotations as runtime evidence, or enrich node labels.

Do not create multiple monitors for the same collector container. Identity is
derived from cluster ID, workload UID, and container; duplicate monitors would
connect with the same OpAMP instance identity.

## Multi-cloud installation and validation

Use one observer installation per cluster, with a unique stable clusterID and an
outbound trusted wss:// connection to CollectorCtrl. Human-readable cluster names
may repeat; cluster IDs must not. No cloud SDK or central kubeconfig is needed
for the in-cluster Kubernetes API observation path.

For OpenShift, overlay helm-charts/collectorctrl-operator/values-openshift.yaml
with Helm's -f option, followed by your connection/image/cluster values. This
removes the chart's fixed user/group IDs so SCC admission can assign the project
UID. Do not grant anyuid or privileged SCC to work around an installation error.
The image keeps its root filesystem read-only and writes only to its credential PVC;
verify arbitrary UID execution with your registry/image policy on a real cluster.

GKE Standard, GKE Autopilot, AKS, EKS, OpenShift, and self-managed Kubernetes are
validation targets, not certified platforms in this source revision. Test chart
admission, CRD/RBAC, Deployment/DaemonSet/StatefulSet discovery, ConfigMap mounts,
rollouts, pod deletion, scale-to-zero, WSS egress, CA trust, Secret rotation,
disconnect/reconnect, and namespace isolation on each target/version. In
Autopilot, also check admission/resource adjustments and provide explicit clusterID
if cluster identity discovery is unavailable. Private clusters require outbound
access to the CollectorCtrl endpoint. Provider/region/account metadata and
cluster/namespace authorization scopes remain follow-up work.

Pod snapshots replace the previous list. Deleted pods disappear after the next
successful observation. Terminating pods remain visible and are never ready;
Succeeded/Failed pods are never ready and the UI hides them by default. Pod UID
distinguishes replacements even when their names repeat. These fields are additive
and optional for older observation clients. No per-pod history is stored.
Deleting/recreating an entire workload changes its fleet identity; old offline
workload entries still require manual pruning. Automatic workload archival needs
a durable deletion signal and a retention policy; disconnection alone is not proof
of workload deletion.

## Engineering validation

```sh
go test ./...
go vet ./...
helm lint ./helm-charts/collectorctrl-operator --set opamp.enrollmentSecret=collectorctrl-auth
helm template observer ./helm-charts/collectorctrl-operator --set opamp.enrollmentSecret=collectorctrl-auth
```

Regression coverage includes ambiguous discovery, mounted/projected config,
sidecar container selection, owner attribution, informer-safe deep copies,
TLS defaults, desired replica health, and the custom observation contract.
Endpoint restriction, cross-namespace secret rejection and durable credentials
have dedicated tests. Run `scripts/test-k8s-integration.ps1` from the server
repository with this checkout alongside it (or pass `-OperatorPath`) to test the
actual server and operator as separate processes over TLS: initial enrollment,
first observation, restart with stored credentials and revoked-identity rejection.
Before release, run a live-cluster pilot covering rollout, scale-to-zero,
credential rotation, ConfigMap changes, disconnect/reconnect, and namespace scope.

## Next milestones

1. Validate pod lifecycle handling and installation across Kubernetes platforms.
2. Establish verified ownership and explicit Git source bindings.
3. Add validated PR proposals for raw ConfigMaps, the official Helm chart, and
   OpenTelemetryCollector resources. Keep Git credentials in the server.
4. Add tested vendor chart adapters and upstream sidecar discovery.
5. Add emergency ownership handoff only after durable recovery and controller
   coordination are implemented.

The older Git package remains an unused prototype. Its provider stubs are not part
of the observation path. No Git provider or emergency override is shipped here.
