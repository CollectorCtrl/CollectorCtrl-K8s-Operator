# Architecture

This document explains how the operator turns Kubernetes objects into what CollectorCtrl shows: which parts run,
what triggers them, how each collector is identified, what goes over the wire and how role, stack and topology are
inferred. For installation and settings see [installation.md](installation.md) and [configuration.md](configuration.md).

## Design principles

1. **Observe, never control.** The operator writes nothing except `CollectorMonitor` status. Deployment stays with
   the customer's Helm charts, vendor operators and CI/CD.
2. **Opt in.** Configuration is read only from collectors that opt in (label or `CollectorMonitor`). Everything else
   contributes metadata and image names only.
3. **Infer from evidence.** Role, stack and topology come from configuration and Kubernetes metadata, never from
   workload names. Every inference carries its source and reasons.
4. **Say what is unverified.** A mounted ConfigMap is not proof of the loaded config; a ready pod is not proof of
   delivered telemetry. Both are reported as unverified.
5. **Additive contracts.** Wire messages are versioned and only gain optional fields.

## Components

```mermaid
flowchart TB
    subgraph mgr["Operator process (controller-runtime manager, 1 replica)"]
        direction TB
        cache[("Informer cache<br/>Deployments, DaemonSets, StatefulSets,<br/>ReplicaSets, Pods, ConfigMaps, Services,<br/>CollectorMonitors")]
        crc["CollectorMonitor reconciler<br/><i>explicit monitors</i>"]
        lrc["Label reconcilers ×3<br/><i>DaemonSet · Deployment · StatefulSet<br/>with collectorctrl.io/observe</i>"]
        obs["observe()<br/>workload → container → ConfigMap/key<br/>→ pods → analyse → report"]
        cobs["Cluster observer<br/><i>leader-only, every 60 s</i>"]
        pool["Connection pool<br/>one OpAMP client per identity"]
        cc["Cluster OpAMP client"]
        crc --> obs
        lrc --> obs
        obs --> pool
        cobs --> cc
        cache --> crc & lrc & cobs
    end
    api["Kubernetes API"] -- "list/watch" --> cache
    crc -- "status update" --> api
    pool == "k8s_observation_v1" ==> srv["CollectorCtrl server"]
    cc == "k8s_cluster_inventory_v1" ==> srv
    pool --- vol[("Credential volume<br/>OPAMP_CREDENTIAL_DIRECTORY")]
    cc --- vol
```

| Component | Source | Triggered by | Writes |
| --- | --- | --- | --- |
| CollectorMonitor reconciler | `operator/controllers/collectormonitor_controller.go` | Monitor changes; Pod, ConfigMap or workload changes in the monitor's namespace; report interval | Monitor `status` |
| Label reconcilers | `operator/controllers/label_controller.go` | Labelled workload created, updated or deleted; label added or removed; Pod, ConfigMap or monitor changes in the namespace; report interval | Nothing (errors go to the log) |
| Analysis | `operator/controllers/insight.go`, `internal/otelconfig` | Every observation | Nothing |
| Cluster observer | `operator/controllers/cluster_observer.go` | Timer (60 s), leader only | Nothing |
| OpAMP client | `pkg/opamp` | Reconcilers | Credentials on the credential volume |

## The observation pipeline

Both reconcilers run the same `observe()` step. The label reconciler builds an in-memory monitor from the workload's
annotations; the CollectorMonitor reconciler uses the resource as written.

```mermaid
flowchart LR
    w["Resolve workload<br/><i>exactly one match</i>"] --> c["Pick collector container<br/><i>single container, or<br/>the configured name</i>"]
    c --> m["Find mounted ConfigMap<br/>and config key"]
    m --> a["Analyse config<br/>role · stack · exports"]
    a --> id["Get or open OpAMP client<br/>for the identity"]
    id --> p["List pods by owner UID"]
    p --> r["Send k8s_observation_v1<br/>update health"]
```

Each step fails closed: an ambiguous match is an error, never a guess.

- **Workload.** A monitor must match exactly one Deployment, DaemonSet or StatefulSet in its namespace.
- **Container.** A single-container pod needs nothing. Multi-container pods need `collectorContainer` or the
  `collectorctrl.io/container` annotation; the application container is never inspected.
- **ConfigMap.** Only ConfigMaps mounted into the collector container count, including projected volumes. If several
  match, a name is required. A key excluded by volume `items` or not reachable through `subPath` is rejected.
- **Config key**, when not configured:
  1. the only key named `*.yaml` or `*.yml`;
  2. otherwise the only key mounted through volume `items` to a `*.yaml` or `*.yml` path (vendor charts store `relay`
     and mount it as `relay.yaml`);
  3. otherwise the only key of a single-key ConfigMap;
  4. otherwise an error asking for `config-key`.
- **Pods.** Attributed by controller owner UID (Deployment → ReplicaSet → Pod), so overlapping labels never pull in
  another workload's pods. Terminating, Succeeded and Failed pods are never counted as ready.

### Precedence between label and CollectorMonitor

A workload can be selected by both the label and a `CollectorMonitor`. The monitor wins:

- the label reconciler skips any workload that a monitor in the same namespace resolves to, and closes its own
  connection for it;
- if the label path still holds the identity, the monitor's connection takes it over;
- the reverse never happens: label discovery cannot displace a monitor.

## Identities and connections

| Connection | OpAMP identity (`service.instance.id`) | Count | Carries |
| --- | --- | --- | --- |
| Observed collector | `k8s://<cluster-id>/<workload-uid>/<container>` | One per collector workload | `k8s_observation_v1` and readiness health |
| Cluster observer | `k8s://<cluster-id>/cluster-observer` | One per cluster | `k8s_cluster_inventory_v1` |

- **Cluster ID** is `CLUSTER_ID`, or the UID of the `kube-system` namespace (stable for the life of the cluster).
  Namespace-scoped installs must set it.
- **Agent attributes** include `k8s.cluster.name`, `k8s.cluster.id`, `k8s.namespace`, `k8s.workload.type`,
  `k8s.workload.name`, `k8s.workload.uid`, `k8s.container.name`, `k8s.configmap.name`,
  `collectorctrl.management.mode=observe`, and, when known, `collectorctrl.discovery`, `collectorctrl.stack` and
  `collectorctrl.role`. The cluster connection adds `collectorctrl.k8s.role=cluster-observer`, so the server keeps
  it out of the workload list.
- **Authentication.** On first connect an enrollment token (`cce_…`) is exchanged for a per-identity credential. The
  credential is written to the credential volume **before** it is acknowledged, then reused on reconnects and
  restarts. Revoked identities never fall back to enrollment.
- **Endpoint pinning.** Every connection goes to the administrator's `OPAMP_SERVER`. A monitor's `opampServer` must
  match it exactly, and server-offered connection changes are refused.
- **Server commands** (restart, config push, emergency override) are refused: the operator is observation-only.
- **Reconnect on change.** A client is replaced when its settings change, for example a role or stack change,
  secret rotation or endpoint change. ConfigMap content changes are reported without reconnecting.

## Wire contracts

Both messages are OpAMP custom messages with capability `collectorctrl`. Fields are only ever added.

### `k8s_observation_v1` (per collector, every report interval)

| Field | Meaning |
| --- | --- |
| `source`, `runtime_verified` | Always `kubernetes-api` and `false`: this is an API snapshot, not runtime evidence |
| `observed_at`, `report_interval_seconds` | When the snapshot was taken and how often to expect one |
| `namespace`, `workload_kind`, `workload_name`, `workload_uid`, `collector_container` | The observed workload |
| `configmap_name`, `configmap_key`, `resource_version`, `config_hash` | The mounted config and its SHA-256 |
| `config_yaml` | The config itself, **only** with `report-config` / `reportConfig` |
| `desired_pods`, `ready_pods`, `pods[]` | Readiness, plus per pod: UID, name, node, phase, ready, image, restarts, reason, terminating |
| `discovery` | `label` or `collectormonitor` |
| `stack`, `stack_source` | Group name and where it came from |
| `role`, `role_source`, `role_reasons[]` | Inferred (or annotated) role and the evidence |
| `receivers[]`, `processors[]`, `exporters[]` | Component IDs used in pipelines (connectors excluded) |
| `exports[]` | `exporter`, `endpoint`, `target` (`workload`, `service`, `external`, `local`, `unresolved`), plus `namespace`, `kind`, `name`, `service` when resolved |
| `config_error` | Why the config could not be analysed, if it could not |

The server treats a snapshot as current only if both its source time and its receipt time fall within
`max(2 min, 2 × interval + 30 s)` (with 30 s clock-skew tolerance). Older or replayed snapshots never replace newer
ones, even across reconnects.

### `k8s_cluster_inventory_v1` (per cluster, every 60 s)

| Field | Meaning |
| --- | --- |
| `source`, `observed_at`, `report_interval_seconds` | As above |
| `cluster_id`, `cluster_name` | The cluster |
| `collectors[]` | `namespace`, `kind`, `name`, `uid`, `container`, `image`, `stack`, `stack_source`, `opted_in`, `monitored` |
| `truncated` | `true` if the list was capped at 2,000 entries |

A workload is listed when a container image's **repository path** (never its tag) contains
`opentelemetry-collector`, `otelcol`, `otel-collector`, `aws-otel-collector`, `elastic-otel-collector`,
`dynatrace-otel-collector`, `nrdot-collector` or `grafana/alloy`.

## Inference rules

All analysis lives in `internal/otelconfig` (parsing, roles, endpoints) and `operator/controllers/insight.go`
(stacks, topology). It is tested against rendered vendor charts in `operator/controllers/vendorcharts_test.go`.

### Components

Only components referenced by `service.pipelines` count; defined-but-unused components are ignored. Component types
are compared with underscores removed, so `kubelet_stats` matches `kubeletstats` and `otlp_http` matches `otlphttp`.
Components declared under `connectors:` (e.g. `spanmetrics`, `forward`) are listed separately and are neither
receivers nor exporters.

### Role

| Evidence in pipelines | Role |
| --- | --- |
| `kubeletstats`, `hostmetrics`, `journald`, or `filelog` including `/var/log/pods` or `/var/log/containers` | agent |
| `k8s_cluster`, `k8sobjects`, `k8s_events` | cluster-receiver |
| `tail_sampling` processor, `loadbalancing` exporter | gateway |
| Two or more of the above | all-in-one |
| None, with an OTLP receiver | agent if DaemonSet, otherwise gateway |
| None, without OTLP | agent if DaemonSet, otherwise unknown |
| Config missing or unparsable | unknown |

`groupbytrace` is deliberately not gateway evidence: node agents use it too. The `collectorctrl.io/role` annotation
overrides the result (`role_source: annotation`); unknown values are ignored and noted in the reasons.

### Stack

The first that applies:

1. `collectorctrl.io/stack` annotation (`annotation`)
2. owner reference to an `OpenTelemetryCollector` (`otel-operator`)
3. label `app.kubernetes.io/instance`, i.e. the Helm release (`helm-release`)
4. label `argocd.argoproj.io/instance` (`argocd-app`)
5. the workload's own name (`standalone`)

### Topology

```mermaid
flowchart TD
    e["Exporter endpoint"] --> env{"Contains ${...}?"}
    env -- yes --> un1["unresolved"]
    env -- no --> loc{"localhost / 127.x / ::1?"}
    loc -- yes --> local["local"]
    loc -- no --> ip{"IP address?"}
    ip -- yes --> ext1["external"]
    ip -- no --> form{"Host form"}
    form -- "svc · svc.ns.svc[.cluster.local]" --> look1{"Service exists?"}
    form -- "a.b" --> look2{"Service b/a exists?"}
    form -- "other domain" --> ext2["external"]
    look1 -- no --> un2["unresolved"]
    look2 -- no --> ext3["external (a domain)"]
    look1 -- yes --> sel
    look2 -- yes --> sel{"Exactly one workload<br/>matches its selector?"}
    sel -- yes --> wl["workload<br/>kind/name"]
    sel -- no --> svc["service"]
```

Endpoints are read from `endpoint`, `traces_endpoint`, `metrics_endpoint`, `logs_endpoint` and `ingest_url`, then a
vendor `domain`. For the `loadbalancing` exporter they come from its static, DNS and Kubernetes resolvers. Repeated
hosts from one exporter are collapsed.

## Failure behaviour

| Situation | Result |
| --- | --- |
| Ambiguous workload, container, ConfigMap or key | Monitor status `Error` with the reason (label path: log line); no connection |
| Config unparsable | Still observed and reported; role `unknown`, `config_error` set |
| Server unreachable | Snapshots are dropped while disconnected; the next interval sends a fresh one. Status `Disconnected` |
| Credential volume lost | Affected identities must be reset in CollectorCtrl before they can enroll again (see [installation.md](installation.md#credentials-and-recovery)) |
| Workload deleted | Connection closed; the entry stays offline in CollectorCtrl until removed |
| Operator restart | Identities and credentials reused; observation resumes within one interval |
