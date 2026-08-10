# Deploying the CollectorCtrl Operator

> **Prerequisites:** A Kubernetes cluster (v1.25+) and `kubectl` access.  
> **Docker:** Only needed if building the image yourself. Pre-built images can be pulled from a registry.

---

## Option A: Quick Start with kubectl (No Helm Needed)

```bash
# 1. Install the CRD
kubectl apply -f deploy/crd.yaml

# 2. Create the namespace
kubectl apply -f deploy/namespace.yaml

# 3. Install RBAC (ServiceAccount + ClusterRole + ClusterRoleBinding)
kubectl apply -f deploy/rbac.yaml

# 4. Install the operator
kubectl apply -f deploy/deployment.yaml

# 5. Create a CollectorMonitor to watch your OTel collector workload
kubectl apply -f deploy/example-collectormonitor.yaml
```

---

## Option B: Helm Chart

```bash
# 1. Install the chart (cluster-scoped RBAC by default)
helm install collectorctrl-operator ./helm-charts/collectorctrl-operator \
  --namespace collectorctrl \
  --create-namespace \
  --set opamp.server=wss://your-collectorctrl-server:4320/v1/opamp \
  --set opamp.secretKey=your-shared-secret

# 2. Verify the operator is running
kubectl get pods -n collectorctrl
kubectl logs -n collectorctrl -l app.kubernetes.io/name=collectorctrl-operator

# 3. Create a CollectorMonitor
kubectl apply -f deploy/example-collectormonitor.yaml
```

### Helm Values Reference

| Value | Description | Default |
|-------|-------------|---------|
| `opamp.server` | WebSocket endpoint of CollectorCtrl server | `wss://collectorctrl.corp.internal:4320/v1/opamp` |
| `opamp.existingSecret` | K8s Secret name containing auth key | `""` |
| `opamp.secretKey` | Plaintext auth key (not recommended for prod) | `""` |
| `rbac.clusterScoped` | `true` = cluster-wide, `false` = namespace-only | `true` |
| `replicaCount` | Number of operator replicas | `1` |
| `leaderElection.enabled` | Enable leader election (needs replicaCount > 1) | `false` |
| `serviceMonitor.enabled` | Create Prometheus ServiceMonitor | `false` |

---

## Option C: Build and Push Your Own Image

```bash
# 1. Build the operator binary (Linux, static)
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -a -ldflags '-s -w' -o manager ./cmd/operator

# 2. Build the Docker image
docker build -f Dockerfile.operator -t your-registry/collectorctrl-operator:v1.0.0 .

# 3. Push to your registry
docker push your-registry/collectorctrl-operator:v1.0.0

# 4. Update values.yaml or deployment.yaml to use your image
```

---

## Verifying the Deployment

### 1. Check Operator Pod
```bash
kubectl get pods -n collectorctrl
kubectl logs -n collectorctrl -l app.kubernetes.io/name=collectorctrl-operator
```

Expected: `starting manager` in logs, pod status `Running`.

### 2. Check CollectorMonitor CRD
```bash
kubectl get crd collectormonitors.collectorctrl.io
kubectl api-resources | grep collectormonitor
```

### 3. Create a Test CollectorMonitor
```bash
kubectl apply -f deploy/example-collectormonitor.yaml
kubectl get collectormonitor -n <your-namespace>
kubectl describe collectormonitor -n <your-namespace>
```

### 4. Verify OpAMP Connection
```bash
kubectl logs -n collectorctrl -l app.kubernetes.io/name=collectorctrl-operator | grep -i opamp
```

Expected: `OpAMP connected` or health report messages.

### 5. Check Status
```bash
kubectl get collectormonitor -o yaml
```

Expected fields in `.status`:
- `phase: Active`
- `agentCount` (number of discovered pods)
- `healthyAgents` (number of healthy pods)
- `configMapRef` (resolved ConfigMap name/key)
- `conditions` (list with `Active`, `Discovered`, `OpAMPConnected`, `ConfigMapResolved`)

---

## Troubleshooting

### Operator pod in CrashLoopBackOff
```bash
kubectl logs -n collectorctrl -l app.kubernetes.io/name=collectorctrl-operator --previous
```
Common causes:
- **RBAC missing**: Ensure ClusterRole/ClusterRoleBinding are applied
- **CRD missing**: `kubectl apply -f deploy/crd.yaml`
- **OpAMP server unreachable**: Check network/firewall to CollectorCtrl server
- **Auth secret missing**: Create the Secret referenced by `opamp.existingSecret`

### CollectorMonitor stuck in `Pending`
```bash
kubectl describe collectormonitor <name> -n <namespace>
```
Common causes:
- **Workload not found**: Verify `matchLabels` matches your collector Deployment/DaemonSet
- **ConfigMap not found**: Either set `configMapSelector.name` or ensure the workload mounts a ConfigMap
- **No pods running**: The workload selector might match a workload with 0 replicas

### OpAMP connection failing
```bash
kubectl logs -n collectorctrl -l app.kubernetes.io/name=collectorctrl-operator | grep -i "opamp\|websocket\|dial"
```
Common causes:
- **Wrong server URL**: Verify `wss://` vs `ws://` and port
- **TLS certificate invalid**: If using self-signed certs, configure `tls.insecureSkipVerify` (dev only)
- **Auth secret wrong key**: Ensure Secret has key `secret-key` (or the key specified in `SecretRef.Key`)
- **Network policy blocking**: Check if K8s network policies block egress from the operator pod

---

## Next Steps After Deployment

1. **Connect the CollectorCtrl Server** — The server should see the cluster as a single agent entry in the fleet overview
2. **Test GitOps** — Edit a collector config via the UI; the server should commit to Git, and the operator should detect the change via drift detection
3. **Test Emergency Mode** — Trigger an emergency config update; the operator should patch the ConfigMap directly
4. **Monitor Metrics** — The operator exposes metrics on `:8080/metrics` for Prometheus scraping

---

*Generated for CollectorCtrl Operator v1.0.0 deployment.*
