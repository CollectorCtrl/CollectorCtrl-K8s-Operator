# Deploying the CollectorCtrl observer with kubectl

The **Helm chart** in `helm-charts/collectorctrl-operator` is the supported install
path; see the [main README](../README.md). These raw manifests mirror the chart's
defaults for clusters where Helm isn't available.

What gets installed in the `collectorctrl` namespace:

| File | Contents |
|------|----------|
| `namespace.yaml` | The `collectorctrl` namespace |
| `crd.yaml` | The `CollectorMonitor` CRD (same as the chart's `crds/crd.yaml`) |
| `rbac.yaml` | ServiceAccount plus read-only ClusterRole/Binding. No Secret access is needed |
| `credentials-pvc.yaml` | Retained 128Mi claim for per-workload credentials |
| `deployment.yaml` | One observer replica, `Recreate` strategy, enrollment auth |

Image: `ghcr.io/collectorctrl/collectorctrl-k8s-operator/operator:latest`

## Prerequisites

- Kubernetes 1.25+ and `kubectl` access with permission to create CRDs and cluster RBAC.
- A CollectorCtrl server **v0.6.0-beta or later**, reachable from the cluster over
  `wss://` (port 4320) with a certificate the observer trusts. Certificate
  verification cannot be turned off; supply your CA if it is private.
- An **Agent Enrollment** token (`cce_…`) from **Settings → API Tokens → Agent Enrollment**. Each
  monitored workload/container uses one enrollment.
- A storage class that can provision the credential claim.

## Install with the script

From the repository root:

```bash
OPAMP_SERVER=wss://collectorctrl.example.com:4320/v1/opamp ./deploy/deploy.sh
# Private CA:
OPAMP_SERVER=wss://collectorctrl.example.com:4320/v1/opamp \
OPAMP_CA_FILE=./ca.pem ./deploy/deploy.sh
```

The script prompts for the enrollment token (or reads `ENROLL_TOKEN`), creates the
`collectorctrl-auth` Secret, applies the manifests and sets `OPAMP_SERVER`.

## Install by hand

```bash
kubectl apply -f deploy/namespace.yaml
kubectl create secret generic collectorctrl-auth -n collectorctrl \
  --from-literal=enrollment-token=cce_YOUR_TOKEN
kubectl apply -f deploy/crd.yaml
kubectl apply -f deploy/rbac.yaml
kubectl apply -f deploy/credentials-pvc.yaml
# Edit OPAMP_SERVER in deployment.yaml first
kubectl apply -f deploy/deployment.yaml
```

For a private CA, create `kubectl create configmap collectorctrl-opamp-ca -n collectorctrl --from-file=ca.crt=./ca.pem`
and uncomment the `OPAMP_CA_FILE` lines in `deployment.yaml`.

## Monitor a collector

Edit `example-collectormonitor.yaml` (or `coralogix-collectormonitor.yaml`) so the
selector matches **exactly one** workload, set `collectorContainer` for
multi-container pods, and set `opampServer` to the same endpoint as the observer.
Then:

```bash
kubectl apply -f deploy/example-collectormonitor.yaml
kubectl get collectormonitors -A
kubectl describe collectormonitor splunk-otel-collector -n observability
```

The workload appears in CollectorCtrl under **Fleet → Kubernetes workloads**.

## Upgrading from the shared-secret prototype

Earlier manifests used `OPAMP_SECRET_KEY` and a `secret-key` Secret. To migrate
without re-enrolling by hand:

1. Keep server legacy mode on, apply `credentials-pvc.yaml`, and run the observer
   once with `OPAMP_AUTH_MODE=legacy` and `OPAMP_SECRET_KEY` so it saves per-workload
   credentials.
2. Switch to `deployment.yaml` from this folder (enrollment mode) and keep the claim.
3. Remove `auth.secretRef`/`secret-key` and the deprecated `driftDetection`,
   `emergencyMode`, `enrichWithNodeMetadata` and `healthCheck.metricsPort` fields
   from your monitors.
4. Turn server legacy mode off.

New installs should use enrollment directly.

## Troubleshooting

| Symptom | Check |
|---------|-------|
| Pod `CreateContainerConfigError` | Secret `collectorctrl-auth` with key `enrollment-token` exists in `collectorctrl` |
| Pod `Pending` | The credential claim is bound (`kubectl get pvc -n collectorctrl`) |
| TLS or `x509` errors in logs | Endpoint hostname matches the certificate; supply the CA |
| Monitor error "opampServer must match" | `spec.opampServer` equals the observer's `OPAMP_SERVER` exactly |
| Monitor reports ambiguous workload or ConfigMap | Narrow `matchLabels`, or set `configMapSelector.name`/`key` |
| Workload re-enrolment fails after revoke | Reset the identity under Agent Enrollment; see the main README |

```bash
kubectl logs -n collectorctrl -l app.kubernetes.io/name=collectorctrl-operator
```

The observer exposes metrics on `:8080/metrics` and health on `:8081`.
