# Development

## Requirements

- Go (version from `go.mod`)
- Helm 3
- Docker with Buildx, for images
- Optional: [kind](https://kind.sigs.k8s.io/) or any cluster, for running the operator against a real API

## Layout

| Path | Contents |
| --- | --- |
| `cmd/operator` | Entry point: flags, environment, manager setup |
| `operator/api/v1alpha1` | `CollectorMonitor` types |
| `operator/controllers` | Reconcilers (`collectormonitor_controller.go`, `label_controller.go`), analysis (`insight.go`), cluster observer |
| `internal/otelconfig` | Collector config parsing, role inference, endpoint extraction |
| `pkg/opamp` | OpAMP client, enrollment and credential storage, custom messages |
| `pkg/api` | Wire types shared with the server |
| `helm-charts/collectorctrl-operator` | Helm chart (CRD in `crds/`) |
| `deploy/` | Plain manifests mirroring the chart |
| `hack/` | Scripts that regenerate test fixtures |

## Everyday commands

```bash
make help          # list targets
make verify        # what CI runs: gofmt check, go vet, race tests, Helm lint and render
make test          # unit tests only (writes cover.out)
make build         # bin/manager
make docker-build  # local image (IMG=... to override)
```

Run the operator against your current kubeconfig:

```bash
export OPAMP_SERVER=wss://localhost:4320/v1/opamp
export OPAMP_ENROLL_TOKEN=cce_...
export OPAMP_CREDENTIAL_DIRECTORY=$PWD/.credentials
export OPAMP_CA_FILE=./ca.pem          # if the server uses a private CA
make run
```

## Tests

- **Unit tests** use the controller-runtime fake client. Most reconciler behaviour is covered in
  `operator/controllers/*_test.go`, and config analysis in `internal/otelconfig`.
- **Vendor chart golden tests** (`operator/controllers/vendorcharts_test.go`) run discovery and analysis against
  manifests rendered from the upstream, Splunk, Coralogix and AWS ADOT charts. To bump a chart version, edit
  `hack/render-vendor-charts.sh`, run `make vendor-fixtures`, and review every changed expectation by hand. A new
  fixture file fails until its expectations are added; the failure prints what was observed.
- **End-to-end on kind** (`hack/e2e-kind.sh`, run by CI on two Kubernetes versions). It installs the chart with a
  local image, adds an opted-in collector and two applications, checks that the service account has exactly the
  documented permissions (and no Secret access), that the operator reconciles without RBAC errors or restarts, and
  that uninstalling keeps the credential volume. No server is needed:

  ```bash
  kind create cluster --name e2e
  docker build -f Dockerfile.operator -t collectorctrl-operator:e2e .
  kind load docker-image collectorctrl-operator:e2e --name e2e
  make e2e IMG=collectorctrl-operator:e2e
  ```

- **End-to-end with the server.** The server repository has `scripts/test-k8s-integration.ps1`, which starts a
  server, deploys the operator into kind, opts in sample collectors and checks what the API reports.

## Changing the wire contract

`k8s_observation_v1` and `k8s_cluster_inventory_v1` are consumed by released servers. Only add optional fields, add
them to `pkg/api` and the server's validation in the same change, and document them in
[architecture.md](architecture.md#wire-contracts). A breaking change needs a new message type (`…_v2`).

## Releasing

Releases are cut from `main` by pushing a tag; GitHub Actions does the rest.

1. In one pull request:
   - move the *Unreleased* entries in `CHANGELOG.md` to `## [X.Y.Z] - YYYY-MM-DD`;
   - set `appVersion` in `helm-charts/collectorctrl-operator/Chart.yaml` to `X.Y.Z`, bump the chart `version` and
     update its `artifacthub.io/changes` annotation;
   - set the image tag in `deploy/deployment.yaml` and the README badge/commands if the chart version changed.
2. Merge it, then tag the merge commit:

   ```bash
   git tag -a vX.Y.Z -m "vX.Y.Z"
   git push origin vX.Y.Z
   ```

3. Two workflows run on the tag:
   - **CI** (`build-operator.yml`) verifies, runs the kind end-to-end tests, then builds
     `ghcr.io/collectorctrl/collectorctrl-k8s-operator/operator:X.Y.Z` and `:X.Y` for linux/amd64 and linux/arm64
     with SBOM and provenance, and signs it with cosign.
   - **Release** (`release.yml`) checks that the tag, chart `appVersion`, changelog section and raw-manifest image
     tag agree, pushes the signed chart to `oci://ghcr.io/collectorctrl/charts`, and creates the GitHub Release
     with notes from the changelog, the chart package, the raw manifests and `SHA256SUMS`. Versions with a
     hyphen (`0.9.0-rc.1`) become pre-releases.

Tags are never moved or reused. Each push to `main` also publishes `:main` and `:latest`, for testing only.

## Continuous integration

| Workflow | Runs on | Checks |
| --- | --- | --- |
| CI | push, pull request, tags | `make verify`, golangci-lint, govulncheck, kind end-to-end tests (two Kubernetes versions), image build; push and sign outside PRs |
| CodeQL | push, pull request, weekly | Go security analysis (`security-extended`) |
| Dependency review | pull request | New dependencies with known vulnerabilities (high and above) or incompatible licences |
| OpenSSF Scorecard | push to `main`, weekly | Repository security practices; results in code scanning and on the badge |
| Release | tags | Chart, raw manifests and GitHub Release |

All actions are pinned to commit SHAs; Dependabot proposes grouped updates every Monday.
