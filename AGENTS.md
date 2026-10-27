# AGENTS.md

Short guide for AI assistants and new contributors. Full context: [CONTEXT.md](CONTEXT.md).

## What this is

A read-only Kubernetes **observer** for CollectorCtrl. It watches OpenTelemetry Collector Deployments, DaemonSets and
StatefulSets that opt in (label `collectorctrl.io/observe: "true"` or a `CollectorMonitor` CR), infers role, stack and
export topology from the mounted config, and reports them to the CollectorCtrl server over OpAMP (`wss://`). It never
writes to collector workloads, ConfigMaps or pods. Go 1.24, controller-runtime 0.17, opamp-go 0.23, Helm chart in
`helm-charts/collectorctrl-operator`.

## Commands

```sh
make verify        # what CI runs: gofmt check, go vet, race tests, Helm lint/render
make test          # unit tests only (race, cover.out)
make build         # bin/manager
make run           # against current kubeconfig; needs OPAMP_SERVER, OPAMP_ENROLL_TOKEN, OPAMP_CREDENTIAL_DIRECTORY
make docker-build  # local image (IMG=...)
make vendor-fixtures  # re-render vendor chart fixtures; review expectation changes by hand
```

CI also fails if `go mod tidy` changes `go.mod`/`go.sum`.

## Conventions

- Observe, never control. Infer from config evidence, never from workload names.
- Fail closed on ambiguity with an actionable error message; never guess.
- Report unverified things as unverified (`runtime_verified: false`, `Unknown` conditions).
- In user-facing text say "collectors", not "agents".
- Conventional Commits; one change per PR, with tests; update `CHANGELOG.md` (*Unreleased*) and docs for user-visible changes.
- Inference changes must update `operator/controllers/vendorcharts_test.go` expectations by hand.

## Don't change without asking

- Anything that **writes** to workloads, ConfigMaps, Secrets or application pods, or accepts server commands.
- The wire contracts `k8s_observation_v1` / `k8s_cluster_inventory_v1` (`pkg/opamp/client.go`): additive optional fields only, matched by a server change; breaking changes need a new `_v2` type.
- OpAMP identity format (`k8s://<cluster-id>/<workload-uid>/<container>`), instance-UID derivation, or credential file naming: changes force re-enrollment of every collector.
- TLS and endpoint pinning (`ApprovedEndpoint`, `TLSConfig`): no skip-verify, `wss://` only.
- Credential persistence order (persist before ack) and the single-replica requirement.
- RBAC scope (chart `rbac.yaml` and kubebuilder markers) and the opt-in rule that config content leaves the cluster only with `report-config`.
- The CRD schema or API group/version. Keep `deploy/crd.yaml` and `helm-charts/.../crds/crd.yaml` identical; DeepCopy is hand-written.
- The Go module path (`github.com/collectorctrl/collectorctrl`).
- Release tags, chart `version`/`appVersion`, and image tags.

Never commit tokens (`cce_…`, `cca_…`), keys, kubeconfigs, cluster IDs or internal hostnames.
