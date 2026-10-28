# Changelog

All notable changes to this project are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and the project uses [Semantic Versioning](https://semver.org/).

## [Unreleased]

## [0.8.0] - 2026-10-28

### Added
- **Instrumentation readiness** (`k8s_instrumentation_readiness_v1`, every 5 minutes on the cluster connection).
  For every application Deployment, StatefulSet and DaemonSet: inferred language, APM agents of any vendor
  (OpenTelemetry, Splunk, Datadog, New Relic, AppDynamics, Dynatrace, Elastic, Instana, unrecognised jars and
  profilers), injection requests, which APM injector webhooks select the pod, launcher variables an injector would
  have to merge with, and a verdict (`conflict`, `review`, `ready`, `unknown`, `unsupported`, `instrumented`) with
  reasons. Agents added by admission webhooks are found in the newest running pod and marked as injected.
- Findings with stable codes and severities (`multiple_vendors`, `injector_adds_vendor`, `injector_selects`,
  `launcher_unreadable`, `language_unknown`, ...) on every container, so UIs and AI agents act on codes rather than
  text. `reasons[]` mirrors the finding messages.
- `collectorctrl.io/language` workload annotation to state the language when it cannot be inferred.
- Helm values `readiness.enabled`, `readiness.interval`, `readiness.excludeNamespaces`; environment variables
  `READINESS`, `READINESS_INTERVAL`, `READINESS_EXCLUDE_NAMESPACES`.

- **Signed releases.** Images are signed with Sigstore cosign (keyless) and carry SPDX SBOM and SLSA provenance
  attestations; the Helm chart is published as a signed OCI artifact at `oci://ghcr.io/collectorctrl/charts`.
  Pushing a `vX.Y.Z` tag creates the GitHub Release with notes, the chart, raw manifests and checksums
  ([docs/verifying.md](docs/verifying.md)).
- End-to-end tests on kind (Kubernetes 1.33 and 1.37) for every change, including a least-privilege RBAC check.
- CodeQL, OpenSSF Scorecard, dependency review, golangci-lint and govulncheck in CI; SPDX licence headers;
  `CODE_OF_CONDUCT.md`, `SUPPORT.md`, `MAINTAINERS.md`.

### Changed
- Dependencies: Kubernetes client libraries 0.37, controller-runtime 0.25, opamp-go 0.25, Go 1.26 (toolchain
  1.26.10). Minimum supported Kubernetes is now 1.30.
- Base images and GitHub Actions are pinned by digest; Dependabot groups updates weekly.
- RBAC adds `list` on namespaces and `admissionregistration.k8s.io/mutatingwebhookconfigurations` while readiness is
  enabled. Without them (namespace-scoped installs) the report says injectors were not checked.

- The image build follows the Go version in `go.mod` (`GOTOOLCHAIN=auto`), so dependency updates that raise it no
  longer break the build.

### Fixed
- The `CollectorMonitor` CRD schema had a mis-indented `enum` under condition `status`, which made the API server
  reject the CRD on install (found by the new kind end-to-end test). A unit test now decodes both CRD copies
  strictly and checks they are identical.

### Security
- Fixes reachable vulnerabilities in `golang.org/x/net` (HTTP/2: GO-2026-6603, -6610, -6611, -6612, -6617) and in the
  Go standard library by moving to `golang.org/x/net` 0.62 and Go 1.26.10.
- No registry access: variables baked into images are not read, and every verdict says so. Secrets are never read.
  Launcher values are reduced to agent paths, module names and profiler IDs before they leave the cluster.

### Requires
- CollectorCtrl server with Kubernetes readiness support (`GET /api/k8s/instrumentation-readiness`). Older servers
  ignore the new message.

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
