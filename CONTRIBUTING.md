# Contributing

Thanks for helping improve the CollectorCtrl Kubernetes Operator.

## Ground rules

- **The operator observes; it never changes customer workloads.** Changes that write to collector workloads,
  ConfigMaps or application pods need a design discussion in an issue first.
- **Infer from evidence, never from names.** Role, stack and topology come from configuration and Kubernetes
  metadata. Workload names and kinds may break ties, never decide.
- **Say what is not verified.** "Config mounted" is not "config loaded"; "pod ready" is not "telemetry delivered".
- **Wire contracts are additive.** `k8s_observation_v1`, `k8s_cluster_inventory_v1` and
  `k8s_instrumentation_readiness_v1` gain optional fields only, with a matching change in the CollectorCtrl server.
  Readiness finding codes never change meaning once released.

## Development setup

You need Go (version in `go.mod`), Helm 3 and, for images, Docker.

```sh
make verify        # gofmt check, go vet, licence headers, race tests, Helm lint/template
make lint          # golangci-lint (same version as CI)
make vuln          # govulncheck
make build         # bin/manager
make docker-build  # local image (IMG=... to override)
make e2e           # end-to-end smoke test on a kind cluster (see docs/development.md)
```

`make help` lists every target. See [docs/development.md](docs/development.md) for running against a cluster,
the vendor-chart fixtures and the end-to-end test with the server.

## Pull requests

1. Open an issue first for anything beyond a small fix.
2. Keep each PR to one change, with tests. Inference changes must update `vendorcharts_test.go` expectations,
   reviewed by hand, not regenerated blindly.
3. Run `make verify lint vuln` before pushing; CI runs the same targets, plus CodeQL, dependency review and the
   end-to-end test on kind. New Go and shell files start with the SPDX header
   (`// SPDX-License-Identifier: Apache-2.0` and `// Copyright 2026 CollectorCtrl`).
4. Update `CHANGELOG.md` under **Unreleased** and the docs for anything user-visible.
5. Follow the [Code of Conduct](CODE_OF_CONDUCT.md).
6. By contributing you agree that your contribution is licensed under the [Apache License 2.0](LICENSE).
