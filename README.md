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
  --set opamp.existingSecret=collectorctrl-auth
```

Create the authentication Secret in the operator namespace using your secret
management workflow. Its secret-key entry must match the CollectorCtrl OpAMP
credential. The kubelet supplies this Secret as an environment variable; the
observer does not need API access to read it.

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

Authentication defaults to the operator's OPAMP_SECRET_KEY environment variable.
Per-monitor auth.secretRef is optional; when using it, grant API get permission
only for the required Secret names through rbac.authSecretNames. Namespace-scoped
installations cannot read Secrets in other namespaces.

Inspect discovery and transport errors with:

```sh
kubectl get collectormonitors -A
kubectl describe collectormonitor existing-gateway -n observability
```

The Kubernetes view distinguishes observer connectivity, container readiness, and
unverified telemetry delivery. Disconnected snapshots and snapshots not received
for two minutes are historical. Use an observation interval under two minutes.
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

## Engineering validation

```sh
go test ./...
go vet ./...
helm lint ./helm-charts/collectorctrl-operator
helm template observer ./helm-charts/collectorctrl-operator
```

Regression coverage includes ambiguous discovery, mounted/projected config,
sidecar container selection, owner attribution, informer-safe deep copies,
TLS defaults, desired replica health, and the custom observation contract.
Before release, run a live-cluster pilot covering rollout, scale-to-zero,
credential rotation, ConfigMap changes, disconnect/reconnect, and namespace scope.

## Next milestones

1. Add collector metrics and an end-to-end diagnostic journey.
2. Establish verified ownership and explicit Git source bindings.
3. Add validated PR proposals for raw ConfigMaps, the official Helm chart, and
   OpenTelemetryCollector resources. Keep Git credentials in the server.
4. Add tested vendor chart adapters and upstream sidecar discovery.
5. Add emergency ownership handoff only after durable recovery and controller
   coordination are implemented.

The older Git package remains an unused prototype. Its provider stubs are not part
of the observation path. No Git provider or emergency override is shipped here.
