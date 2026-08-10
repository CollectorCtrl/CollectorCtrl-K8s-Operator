# CollectorCtrl K8s Operator — Engineering Codebase

This directory contains the foundational code for the CollectorCtrl Kubernetes Operator and supporting libraries.

## Directory Structure

```
.
├── cmd/operator/              # Operator binary entrypoint
├── internal/
│   └── validator/             # OTel Collector YAML validation engine
├── pkg/
│   ├── api/                   # Shared types (Agent, K8sContext, FleetPolicy)
│   ├── git/                   # Git integration (GitHub, GitLab, Azure DevOps)
│   └── opamp/                 # Shared OpAMP client library (server + operator)
├── operator/
│   ├── api/v1alpha1/          # CollectorMonitor CRD Go types
│   ├── config/crd/            # CRD YAML samples
│   └── controllers/           # Main reconcile loop
├── helm-charts/
│   └── collectorctrl-operator/# Helm chart for operator deployment
├── Dockerfile.operator        # Multi-stage distroless build
├── Makefile                   # Build, test, lint, scan, deploy targets
└── go.mod                     # Go module definition
```

## Architecture Decisions

### 1. Same Repo, Different Directory
The operator lives in `/operator` within the same repo as the server. Shared packages (`pkg/api`, `pkg/opamp`) are imported by both.

### 2. GitOps Strategy
- **Standard changes:** CollectorCtrl writes to Git (values.yaml or raw ConfigMap). ArgoCD/Flux deploys.
- **Emergency overrides:** Operator patches ConfigMap directly (30s). Auto-commit back to Git within seconds.
- **Source of truth:** Git for K8s. CollectorCtrl DB for VMs/Windows.

### 3. OpAMP Client — One Per Cluster
The Operator maintains **one WebSocket** per cluster, aggregating all pod health. This reduces server connection load vs. one connection per pod.

### 4. Authentication — Shared Secret via K8s Secret
Simple and sufficient for on-prem networks. No OIDC/mTLS complexity. Secret mounted via `SecretRef` in CRD.

## Key Files

| File | Purpose |
|---|---|
| `pkg/api/types.go` | Shared Agent model with K8sContext. Imported by server and operator. |
| `pkg/opamp/client.go` | OpAMP client with handlers for config updates and emergency commands. |
| `operator/api/v1alpha1/collectormonitor_types.go` | CRD spec with kubebuilder markers. |
| `operator/controllers/collectormonitor_controller.go` | Reconcile loop: discover, connect, report, drift-detect. |
| `pkg/git/client.go` | Git abstraction: commit, PR, fast-forward. |
| `internal/validator/otel.go` | Pre-flight OTel config validation before Git commit. |

## Build

```bash
# Build binary
make build

# Run tests
make test

# Build Docker image
make docker-build IMG=ghcr.io/collectorctrl/operator:v1.0.0

# Multi-arch image
make docker-build-multiarch IMG=ghcr.io/collectorctrl/operator:v1.0.0

# Security scan
make scan

# Deploy to cluster
make install   # Install CRDs
make deploy    # Deploy operator
```

## Install via Helm

```bash
helm install collectorctrl-operator ./helm-charts/collectorctrl-operator \
  --namespace collectorctrl-system --create-namespace \
  --set opamp.server=wss://collectorctrl.corp.internal:4320/v1/opamp \
  --set opamp.existingSecret=collectorctrl-credentials
```

## Create a CollectorMonitor

```bash
kubectl apply -f operator/config/crd/collectormonitor-sample.yaml
```

## Known TODOs in Code

- `discoverConfigMap` auto-discovery from pod template volumes
- `applyEmergencyConfig` annotation patching for DaemonSet/Deployment/StatefulSet
- `reportFleetHealth` pod list aggregation and metrics scraping
- `detectDrift` exec into pod and compare config hashes
- Git provider implementations (GitHub, GitLab, Azure, Generic)
- Reconnect logic with exponential backoff in OpAMP client

## Next Steps for Engineering

1. **Phase 1:** Wire up the OpAMP client, implement ConfigMap auto-discovery, deploy on EKS with official OTel chart.
2. **Phase 2:** Build GitHub client, validate standard flow (UI → Git → Argo → K8s).
3. **Phase 3:** Emergency override, drift detection, canary rollout logic.
4. **Phase 4:** UI updates for cluster topology, multi-cluster dashboard.
