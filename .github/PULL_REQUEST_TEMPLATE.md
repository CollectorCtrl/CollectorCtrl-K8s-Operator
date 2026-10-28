## What and why

<!-- What does this change, and what problem does it solve? -->

## How it was verified

- [ ] `make verify lint vuln` passes (format, vet, licence headers, race tests, Helm lint, golangci-lint, govulncheck)
- [ ] New behaviour has tests (vendor-chart fixtures updated if inference changed)
- [ ] Tried on a cluster (say which: kind, EKS, AKS, GKE, OpenShift…), or explain why not needed

## Safety checklist

- [ ] The operator still never writes to collector workloads, ConfigMaps or application pods
- [ ] No configuration content leaves the cluster unless the workload opted in with `report-config`
- [ ] Wire-contract changes (`k8s_observation_v1`, `k8s_cluster_inventory_v1`, `k8s_instrumentation_readiness_v1`) are additive, with matching server changes; finding codes keep their meaning
- [ ] No new RBAC permission, or it is documented in SECURITY.md and checked in `hack/e2e-kind.sh`
- [ ] Docs and `CHANGELOG.md` updated
