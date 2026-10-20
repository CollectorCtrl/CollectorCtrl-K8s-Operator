# Security policy

## Reporting a vulnerability

Please **do not open a public issue** for security problems.

- Use GitHub's private reporting: **Security → Report a vulnerability** on this repository
  ([direct link](https://github.com/CollectorCtrl/CollectorCtrl-K8s-Operator/security/advisories/new)), or
- email **engineering@collectorctrl.com** with "SECURITY" in the subject.

Include the operator version, Kubernetes platform, what an attacker could do, and steps to reproduce.
We acknowledge reports within 3 business days and agree a disclosure date with you once a fix is ready.

## Supported versions

| Version | Supported |
| --- | --- |
| 0.7.x | Yes |
| < 0.7 | No: upgrade to 0.7 |

## Security model, in short

The operator is designed so that installing it adds as little risk to a cluster as possible:

- **Read-only towards workloads.** It never creates, patches or deletes collector workloads, ConfigMaps
  or application pods. The only object it writes is the `status` of its own `CollectorMonitor` resources.
- **Least-privilege RBAC.** `get/list/watch` on Deployments, DaemonSets, StatefulSets, ReplicaSets, Pods,
  ConfigMaps and Services; `get` on the `kube-system` namespace (cluster ID only). No Secret access unless you
  name specific Secrets in `rbac.authSecretNames`.
- **Opt-in only.** Configuration is read only from collectors labelled `collectorctrl.io/observe: "true"` or
  selected by a `CollectorMonitor`. The cluster inventory of unobserved collectors uses workload metadata and
  image names only.
- **Configuration content stays in the cluster by default.** Collector YAML is sent to CollectorCtrl only when a
  workload sets `collectorctrl.io/report-config: "true"` (or `reportConfig: true` on its monitor). Derived facts
  (pipeline component names, inferred role, exporter endpoints) are sent so the UI can show role and topology.
- **Outbound only, verified TLS.** One outbound `wss://` connection per observed collector plus one per cluster.
  TLS verification cannot be disabled; a private CA can be added with `opampCAConfigMap`. The endpoint is pinned
  by the administrator (`OPAMP_SERVER`); a `CollectorMonitor` cannot redirect credentials elsewhere.
- **Per-identity credentials.** An enrollment token is exchanged for a per-agent credential, stored on a
  dedicated volume and reused on restart. Revoked identities never fall back to enrollment.
- **Hardened pod.** Distroless non-root image (UID 65532), read-only root filesystem, all capabilities dropped,
  no privilege escalation, `RuntimeDefault` seccomp profile.
- **Server commands rejected.** The operator refuses OpAMP commands, connection-setting changes and emergency
  overrides from the server.
