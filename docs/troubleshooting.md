# Troubleshooting

Start with the operator's logs and, for `CollectorMonitor` resources, their status:

```bash
kubectl -n collectorctrl-operator get pods
kubectl -n collectorctrl-operator logs deploy/collectorctrl-operator --tail=200
kubectl get collectormonitors -A
kubectl -n <namespace> describe collectormonitor <name>
```

Label-discovered collectors have no status object. Their errors appear in the log as
`label-discovered collector could not be observed`, together with the workload and the failing `step`.

If the pod restarted, look at the newest pod and the previous container:

```bash
kubectl -n collectorctrl-operator logs <pod> --previous
```

## The operator does not start

| Symptom | Cause | Fix |
| --- | --- | --- |
| `ImagePullBackOff` | The image tag does not exist yet (for example `0.8.0` before the release tag was pushed), or the registry needs credentials | Use `--set image.tag=main --set image.pullPolicy=Always` for testing, or mirror the image and set `imagePullSecrets` |
| `CreateContainerConfigError`: secret not found | The enrollment Secret is missing, or the key is not `enrollment-token` | Create it in the operator's namespace (see [installation.md](installation.md#install-with-helm)) |
| Pod stays `Pending` | The credential PVC cannot be bound | Check `kubectl -n collectorctrl-operator get pvc`; set `credentialStorage.storageClassName` or `existingClaim` |
| `invalid OpAMP TLS configuration` | The CA file is unreadable or not PEM | Check that the `opampCAConfigMap` key is `ca.crt` and holds PEM certificates |
| `OpAMP endpoint must be an administrator-configured wss URL…` | `opamp.server` is not `wss://`, or has credentials, a query or a fragment | Use a plain `wss://host:port/v1/opamp` URL |
| `dial tcp 10.100.0.1:443: i/o timeout` | The pod cannot reach the Kubernetes API. On EKS this is usually a cluster security group without self-referencing ingress on 443 | Allow pod-to-API traffic (on EKS: ingress on TCP 443 from the cluster security group to itself) |
| `cluster identity is unavailable; set CLUSTER_ID` | Namespace-scoped install without `clusterID` | Set `clusterID` |
| `forbidden` listing resources | Namespace-scoped install, or RBAC changed by hand | Reinstall with the chart's RBAC; set `rbac.clusterScoped` to match what you need |

## The cluster does not appear in CollectorCtrl

- **TLS errors (`x509: certificate signed by unknown authority`).** Provide the CA with `opampCAConfigMap`.
  Verification cannot be disabled.
- **`401` or `403` on connect.** The token is not an Agent Enrollment token, has expired, or has no uses left. Each
  identity uses one enrollment, so a cluster with ten observed collectors needs at least eleven uses.
- **Connection refused or timeout.** Check egress from the operator namespace to the server host and port
  (NetworkPolicies, proxies, firewalls). The connection is a long-lived WebSocket; proxies must allow upgrades.
- **Cluster connection disabled.** `clusterObserver.enabled=false` hides the cluster entry and the candidate list.
  Opted-in collectors still appear.

## A collector does not appear, or shows an error

| Error or symptom | Cause | Fix |
| --- | --- | --- |
| Not listed at all | The label is on the pod template instead of the workload, or its value is not exactly `"true"` | Label the Deployment, DaemonSet or StatefulSet itself |
| Label disappears after a deploy | The label was added by hand, and Helm or GitOps restored the source | Add it to the collector's chart values or Git source |
| `multi-container workload requires collectorContainer…` | The pod has sidecars | Set `collectorctrl.io/container` (or `collectorContainer`) |
| `collectorContainer "x" does not exist` | Wrong container name | Use a name from `kubectl get <kind> <name> -o jsonpath='{.spec.template.spec.containers[*].name}'` |
| `collector container mounts 0 matching ConfigMaps…` | The config comes from a Secret, CLI flags or another source | Not supported in 0.7: the config must be mounted from a ConfigMap |
| `collector container mounts N matching ConfigMaps…` (N > 1) | Several candidate ConfigMaps | Set `collectorctrl.io/config-map` (or `configMapSelector.name`) |
| `ConfigMap x has N candidate config keys…` | Several keys and none is uniquely the YAML file | Set `collectorctrl.io/config-key` (or `configMapSelector.key`) |
| `ConfigMap key x/y is excluded from the collector's mounted volume` | The key is filtered out by volume `items` | Point at the key that is actually mounted |
| `ConfigMap x has no non-empty key "y"` | Typo in the key, or the key is empty | Check `kubectl get configmap x -o yaml` |
| `selector matched N workloads…` | The `CollectorMonitor` selector is ambiguous | Add `workloadSelector.kind` or `name`, or narrow `matchLabels` |
| `monitor opampServer must match the operator's configured OPAMP_SERVER` | The monitor points at another endpoint | Set it to exactly the operator's `opamp.server` |
| Listed under "Likely collectors not yet opted in" after labelling | The label was added in a namespace the operator does not watch | Check `rbac.clusterScoped`; a namespace-scoped operator only sees its own namespace |
| Role shown as `unknown` | No role evidence and no OTLP receiver, or the config could not be parsed (`config_error`) | Fix the config, or set `collectorctrl.io/role` |
| `workload/container is already observed by …` | Two `CollectorMonitor`s select the same workload | Keep one monitor per workload |
| Two collectors merged into one entry | Should not happen: identities use the workload UID and container. Collectors from before 0.6 used truncated IDs | Upgrade, then remove the stale entry in CollectorCtrl |
| Entry offline after recreating a workload | A deleted and recreated workload has a new UID, so it is a new identity | Remove the old entry; the new one enrolls automatically (one enrollment use) |

## Credentials

- **`enrollment token is required for a new workload`.** A new identity has no stored credential and no token is
  configured. Set `opamp.enrollmentSecret`.
- **`enrollment-token must contain a cce_ enrollment token`.** The Secret holds another kind of token.
- **`credential storage must be writable`.** The credential volume is read-only or not mounted.
- **`invalid persisted credential…`, or identities rejected after reinstalling.** The credential volume was deleted,
  replaced or corrupted. Follow [Credentials and recovery](installation.md#credentials-and-recovery).
- **Two operator pods running.** Only one replica is supported. Keep `replicaCount: 1`; the `Recreate` strategy stops
  the old pod first.

## Topology looks wrong

- **Endpoints shown as `unresolved`.** The endpoint uses `${env:…}`, or it names a Service that does not exist in the
  namespace. Environment values are not expanded, because the operator does not read pod environment or Secrets.
- **Target is a `service`, not a workload.** The Service's selector matches no workload, or more than one.
- **A gateway reached through an Ingress or external load balancer** is shown as `external`. This is expected: only
  in-cluster Services are resolved.

## Instrumentation readiness

| Symptom | Cause | What to do |
| --- | --- | --- |
| No readiness panel in CollectorCtrl | Server older than the readiness feature, `readiness.enabled=false`, the cluster connection is off, or your role lacks `zerotouch:view` | Upgrade the server; check the Helm values and your role |
| "Injectors: not checked" | Namespace-scoped install, or the ClusterRole lacks `list` on namespaces or mutatingwebhookconfigurations | Install cluster-scoped, or accept that injector conflicts are not detected |
| Many workloads show **Language unknown** | Application images rarely name their runtime, and image `ENV` is not read in 0.8 | Expected. Set `collectorctrl.io/language` on the workload (the drawer in CollectorCtrl gives the exact `kubectl annotate` command) |
| **Ready** although the image sets `JAVA_TOOL_OPTIONS` | Variables baked into the image are invisible without registry access | Every verdict carries this caveat; check the Dockerfile before instrumenting |
| An agent you know is running is missing | Baked into the image with no trace in the manifest, or the workload has no running pod (webhook-injected agents are found only in pods) | Check the reasons: "No running pod" is stated when it applies |
| A webhook you rely on is not listed | It does not mutate pods on CREATE, or its name, webhook name and service name do not identify an APM vendor | Open an issue with the webhook's name and service so the classifier can learn it |
| Workloads in a namespace are missing | The namespace is in `readiness.excludeNamespaces`, or it is the operator's own namespace | Adjust `readiness.excludeNamespaces` |

## Getting help

Open an issue with the operator version (`starting manager` log line), Kubernetes version, the collector chart and
version, and the relevant log lines. Remove tokens and hostnames you don't want to share. Report security problems
privately as described in [SECURITY.md](../SECURITY.md).
