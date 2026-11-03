# Java auto-instrumentation

From 0.9 the operator can add the **OpenTelemetry Java agent** to application pods you opt in. It is the first
feature where the operator changes anything in your cluster, so it is off by default, opt-in per workload, limited
to new pods (nothing running is touched), and designed to fail towards "pod starts without the agent", never
"pod does not start".

- **OpenTelemetry only.** The agent image must carry an OpenTelemetry Java agent (upstream or an OTel-based
  distribution). Other vendors' agents are never injected.
- **Never next to another agent.** A pod that already loads an APM agent of any vendor, or that another injector
  webhook would act on, is left alone and says why.
- **JVM flags are preserved.** `JAVA_TOOL_OPTIONS` from the pod spec or a ConfigMap is extended, not replaced. With
  `registry.readImageConfig: true` the operator also reads flags baked into the image.
- **Canary and revert.** Start with one pod per workload revision. If it crash-loops, the revision is reverted and
  the pod replaced by one without the agent.

## Enable it

```bash
helm upgrade --install collectorctrl-operator oci://ghcr.io/collectorctrl/charts/collectorctrl-operator \
  --namespace collectorctrl-operator --reuse-values \
  --set instrumentation.enabled=true \
  --set instrumentation.defaultProfile.create=true \
  --set instrumentation.defaultProfile.exporterEndpoint=http://otel-agent.observability:4318
```

This installs the `Instrumentation` CRD (with the chart's other CRD), a `MutatingWebhookConfiguration` with
`failurePolicy: Ignore`, a Service for the webhook, and a `default` profile in the release namespace. The operator
generates its own serving certificate at every start and writes the CA into the webhook configuration (no
cert-manager needed). Without Helm, apply `deploy/crd-instrumentation.yaml` and `deploy/webhook.yaml` and set
`INSTRUMENTATION=true` in the deployment (`INSTRUMENTATION=true ./deploy/deploy.sh` does both).

## Profiles

An `Instrumentation` says where telemetry goes and how the rollout behaves. Which profile a pod uses:

1. the one named in the pod template annotation `collectorctrl.io/instrumentation: <name>` or `<namespace>/<name>`;
2. otherwise the single `Instrumentation` in the pod's namespace;
3. otherwise `default` in the operator namespace;
4. otherwise the pod is skipped with `no_profile`.

```yaml
apiVersion: collectorctrl.io/v1alpha1
kind: Instrumentation
metadata:
  name: default
  namespace: collectorctrl-operator
spec:
  exporter:
    endpoint: http://otel-agent.observability:4318   # OTLP; grpc:// or http(s)://
    protocol: http/protobuf                           # or grpc
  propagators: [tracecontext, baggage]
  sampler: {type: parentbased_traceidratio, argument: "0.25"}
  resourceAttributes: {deployment.environment: production}
  env:                                                # extra OTEL_* variables; never overwrite the container's own
    - {name: OTEL_LOGS_EXPORTER, value: none}
  java:
    image: ""                                         # empty: upstream autoinstrumentation-java pinned in the operator
    extraOptions: ""                                  # JVM flags appended after -javaagent
  registry:
    readImageConfig: false                            # see "Image ENV"
    credentialsSecret: ""                             # kubernetes.io/dockerconfigjson in this namespace
  canary:
    maxRestarts: 3
    autoRevert: true
```

The full field list is in [configuration.md](configuration.md#instrumentation-resource).

## Opt a workload in

Add a label to the **pod template** of a Deployment, StatefulSet or DaemonSet (your Helm values or manifests, as
usual), and roll it out:

```yaml
spec:
  template:
    metadata:
      labels:
        collectorctrl.io/instrument-java: "canary"   # one pod per revision; "true" for every pod
```

| Label value | Effect |
| --- | --- |
| `canary` | The first new pod of each workload revision gets the agent; the others start unchanged |
| `true` | Every new pod gets the agent |
| anything else, or absent | Nothing happens; pods already carrying the agent keep it until they are replaced |

Optional pod template annotations:

| Annotation | Purpose |
| --- | --- |
| `collectorctrl.io/instrumentation` | Profile to use (`name` or `namespace/name`) |
| `collectorctrl.io/instrument-container` | Which container, when the pod has more than one application container |
| `collectorctrl.io/language: java` | Settles the language when the operator cannot infer it (also read by readiness) |
| `collectorctrl.io/instrument-force: "true"` | Proceed although `envFrom` references a Secret that might set `JAVA_TOOL_OPTIONS` |

The journey in CollectorCtrl: the readiness report shows the workload as *review* with `injection_requested` until
the first pod restarts, then *instrumented*, with the rollout state (`canary`, `rolled-out`, `reverted`, `skipped`,
`pending`) under `instrumentation`.

## What the webhook does

For a new pod with the label, in this order:

1. **Profile.** None found: skip (`no_profile`).
2. **Revert record.** The profile's status says this workload revision was reverted: skip (`reverted`).
3. **Canary.** Mode `canary` and another instrumented pod of this revision exists: skip (`canary_present`).
4. **Other agents.** The same analysis as the readiness report runs on the pod. Any APM agent (javaagent flag,
   init container, preload library, profiler, ...) of any vendor: skip (`agent_present`). Any other injector webhook
   whose selectors match the pod: skip (`injector_present`).
5. **Container.** The annotated container, else the single Java container, else the single application container
   (noted as `language_assumed`). Two candidates: skip (`container_ambiguous`). A Node.js, Python, .NET or Go
   container only: skip (`not_java`).
6. **Launcher.** The `JAVA_TOOL_OPTIONS` the process would have had is found: a literal value, a ConfigMap key (read
   and inlined), an `envFrom` ConfigMap, else the image configuration when the registry read is allowed. A value
   from a Secret, a field or resource reference, or an `envFrom` Secret without `instrument-force`: skip
   (`launcher_unreadable`). The operator never reads Secrets.
7. **Mutation.** An `emptyDir` volume, an init container that copies `/javaagent.jar` from the agent image into
   it, a read-only mount in the application container, `JAVA_TOOL_OPTIONS=<existing> -javaagent:/collectorctrl/java/opentelemetry-javaagent.jar [extraOptions]`,
   and the OTel SDK variables the container does not already set: `OTEL_EXPORTER_OTLP_ENDPOINT`,
   `OTEL_EXPORTER_OTLP_PROTOCOL`, `OTEL_SERVICE_NAME` (the workload name), `OTEL_RESOURCE_ATTRIBUTES`
   (`k8s.namespace.name`, `k8s.pod.name`, `k8s.container.name`, `k8s.deployment.name`, then the profile's), and the
   profile's propagators, sampler and `env`. The init container inherits the application container's
   `securityContext`.

Every pod the webhook looked at carries the outcome:

```
collectorctrl.io/instrumentation-status: injected | skipped
collectorctrl.io/instrumentation-reason: <code>: <explanation>      (skipped) or notes (injected)
collectorctrl.io/instrumentation:        <namespace>/<profile>      (injected)
collectorctrl.io/instrumentation-mode:   canary | all               (injected)
collectorctrl.io/instrumented:           java                       (label, injected)
```

```bash
kubectl -n shop get pods -L collectorctrl.io/instrumented \
  -o custom-columns='NAME:.metadata.name,STATUS:.metadata.annotations.collectorctrl\.io/instrumentation-status,REASON:.metadata.annotations.collectorctrl\.io/instrumentation-reason'
```

The webhook always answers *allowed*. If the operator is down or slow (5 s timeout), the API server skips it
(`failurePolicy: Ignore`) and the pod starts without the agent.

## Image ENV

Kubernetes `env` shadows a variable of the same name baked into the image. A `JAVA_TOOL_OPTIONS` set with
`ENV` in the Dockerfile is therefore invisible to the webhook unless it reads the image configuration. With
`registry.readImageConfig: true` the operator fetches the manifest and config (two or three small GETs per image,
cached, no layers) from the registry, anonymously or with the `kubernetes.io/dockerconfigjson` Secret named in
`registry.credentialsSecret` (grant it with the Helm value `instrumentation.registryCredentialSecrets`). The value
found is merged and the pod is noted `image_env_merged`; a registry failure is noted `image_env_unreadable` and the
agent flag is set on its own. With the read disabled every injected pod is noted `image_env_not_checked`.

## Canary and revert

With `canary`, the first new pod of a workload revision (pod template hash for Deployments, controller revision for
StatefulSets and DaemonSets) gets the agent and the rest start unchanged. The `Instrumentation` controller watches
instrumented pods. When a canary pod's containers restart `canary.maxRestarts` times (default 3) or enter
`CrashLoopBackOff`, the revision is **reverted**:

- the profile's `status.workloads[]` entry becomes `state: reverted` with the reason, and the webhook skips every
  further pod of that revision (`reverted`);
- with `canary.autoRevert: true` (default) the failing pod is deleted; its replacement starts without the agent;
- a new revision (any change to the pod template) is tried again, and the record is dropped once no pod of the
  reverted revision asks for the agent any more.

Two pods created in the same instant can both become canaries: the webhook reads the pod cache, which can lag a
moment behind. Set `"true"` when the canary looks good; set the label to `"false"` (or remove it) and restart the
rollout to take the agent out again. Pods already carrying the agent are never modified in place.

```bash
kubectl -n collectorctrl-operator get instrumentation default -o yaml   # status.workloads[], podsInstrumented
```

## Permissions

With `instrumentation.enabled=true` the ClusterRole gains: `get`/`list`/`watch` on `instrumentations`,
`update`/`patch` on their status, `get`/`list` on namespaces (injector selectors), `delete` on pods (auto revert
only), `list` on all `mutatingwebhookconfigurations` and `get`/`update`/`patch` on the operator's own, plus `get` on
the Secrets you list in `instrumentation.registryCredentialSecrets`. Nothing else changes: no write access to
workloads, ConfigMaps or other Secrets. The kind end-to-end test checks exactly this set.

## Limits

- Java only in 0.9; Node.js, Python and .NET follow in 1.0.
- `JDK_JAVA_OPTIONS` and `_JAVA_OPTIONS` are left as they are (the JVM reads them in addition).
- Image configuration is read for the `linux/amd64` manifest (or the first Linux one) of a multi-architecture image.
- The webhook decides from the pod and its namespace. Workload-level annotations such as `collectorctrl.io/language`
  must be on the pod template to be seen by the webhook (the readiness report reads both).
- Namespace-scoped installs (`rbac.clusterScoped=false`) instrument pods in the release namespace only, because the
  operator only sees that namespace; the webhook configuration is still cluster-scoped.
