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
- **End-to-end with the server.** The server repository has `scripts/test-k8s-integration.ps1`, which starts a
  server, deploys the operator into kind, opts in sample collectors and checks what the API reports.

## Changing the wire contract

`k8s_observation_v1` and `k8s_cluster_inventory_v1` are consumed by released servers. Only add optional fields, add
them to `pkg/api` and the server's validation in the same change, and document them in
[architecture.md](architecture.md#wire-contracts). A breaking change needs a new message type (`…_v2`).

## Releasing

1. Update `CHANGELOG.md`: move entries from *Unreleased* to the new version.
2. Set `appVersion` in `helm-charts/collectorctrl-operator/Chart.yaml` to the new version. Bump the chart `version`,
   and update its `artifacthub.io/changes` annotation.
3. Update the image tag in `deploy/deployment.yaml`.
4. Merge to `main`, then tag the merge commit:

   ```bash
   git tag -a v0.7.0 -m "v0.7.0"
   git push origin v0.7.0
   ```

   CI publishes `ghcr.io/collectorctrl/collectorctrl-k8s-operator/operator:0.7.0` and `:0.7` for linux/amd64 and
   linux/arm64, with the version embedded in the binary.
5. Create a GitHub release from the tag, with the changelog section as notes.

Each push to `main` also publishes `:main` and `:latest`, for testing only.
