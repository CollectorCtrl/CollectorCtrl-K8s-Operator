# Changelog

All notable changes to this project are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and the project uses [Semantic Versioning](https://semver.org/).

## [Unreleased]

## [0.7.0] - 2026-10-20

### Added
- **Label opt-in.** Any Deployment, DaemonSet or StatefulSet labelled `collectorctrl.io/observe: "true"` is observed
  without a `CollectorMonitor`. Optional annotations: `container`, `config-map`, `config-key`, `stack`, `role`,
  `report-config`, `report-interval`. An explicit `CollectorMonitor` for the same workload takes precedence.
- **Role inference** from the configuration's pipelines (agent, gateway, cluster-receiver, all-in-one, unknown),
  with the evidence reported. Snake_case component names are recognised; connectors are listed separately.
- **Stacks**: workloads deployed together are grouped by annotation, owning OpenTelemetryCollector, Helm release or
  Argo CD application.
- **Topology**: exporter endpoints are resolved to Services and the workload behind them, or marked external,
  local or unresolved.
- **Cluster connection** (`k8s://<cluster-id>/cluster-observer`) sending `k8s_cluster_inventory_v1` every minute:
  collector-like workloads found by image name, marked opted in, monitored or not yet observed.
  Configure with `clusterObserver.enabled` and `clusterName`.
- Golden tests against rendered upstream, Splunk, Coralogix and AWS ADOT charts (`hack/render-vendor-charts.sh`).
- `LICENSE` (Apache-2.0), `SECURITY.md`, `CONTRIBUTING.md`, issue and PR templates, Dependabot.

### Changed
- Default namespace is now `collectorctrl-operator` (was `collectorctrl`). Existing installs can keep their
  namespace; the credential volume lives there.
- Config key discovery falls back to the key mounted to a `*.yaml` path, then to a single-key ConfigMap. Vendor
  charts store their config as `relay` and previously needed `config-key`.
- `groupbytrace` no longer marks a collector as a gateway.
- RBAC adds `get/list/watch` on Services (topology).
- CI runs format, vet, race tests and Helm lint before building images; images carry their version.
- The Makefile and Dockerfile build from a clean checkout; the Dockerfile builds reproducibly from `go.sum`.

### Fixed
- `serviceMonitor.enabled=true` now also creates the metrics Service the ServiceMonitor selects; before, nothing
  was scraped.

### Removed
- Unused prototype packages `pkg/git` and `internal/validator`.
- The superseded installation guide; replaced by `docs/installation.md`, `docs/configuration.md`,
  `docs/architecture.md`, `docs/troubleshooting.md` and `docs/development.md`.

## [0.6.0-beta]

- First observation release: `CollectorMonitor` resources, mounted ConfigMap discovery, pod readiness reporting
  over `k8s_observation_v1`, enrollment-token authentication with persisted per-agent credentials.

[Unreleased]: https://github.com/CollectorCtrl/CollectorCtrl-K8s-Operator/compare/v0.7.0...HEAD
[0.7.0]: https://github.com/CollectorCtrl/CollectorCtrl-K8s-Operator/releases/tag/v0.7.0
