# CollectorCtrl K8s Operator — Installation & Troubleshooting Guide

> **Version:** 1.0  
> **Target Audience:** DevOps / Platform Engineers managing OpenTelemetry Collector fleets on Kubernetes  
> **Scope:** EKS, AKS, GKE, and on-prem K8s clusters

---

## Table of Contents

1. [Architecture Overview](#architecture-overview)
2. [Prerequisites](#prerequisites)
3. [Installation](#installation)
   - [Step 1: Deploy CRDs](#step-1-deploy-crds)
   - [Step 2: Deploy RBAC](#step-2-deploy-rbac)
   - [Step 3: Deploy the Operator](#step-3-deploy-the-operator)
   - [Step 4: Create the Auth Secret](#step-4-create-the-auth-secret)
   - [Step 5: Apply CollectorMonitor CRs](#step-5-apply-collectormonitor-crs)
4. [Configuration Reference](#configuration-reference)
5. [Verifying the Deployment](#verifying-the-deployment)
6. [Troubleshooting](#troubleshooting)
7. [Important Learnings & Gotchas](#important-learnings--gotchas)

---

## Architecture Overview

The CollectorCtrl Operator runs inside your Kubernetes cluster as a **Deployment** and watches **CollectorMonitor** custom resources. Each `CollectorMonitor` CR represents one collector workload (DaemonSet, Deployment, or StatefulSet) that you want to monitor and manage remotely.

```
┌─────────────────────────────────────────────────────────────────┐
│                         EKS Cluster                              │
│                                                                  │
│  ┌──────────────┐      ┌──────────────┐      ┌──────────────┐  │
│  │  Collector   │      │  Collector   │      │  Collector   │  │
│  │  (DaemonSet) │      │ (Deployment) │      │  (StatefulSet)│  │
│  └──────┬───────┘      └──────┬───────┘      └──────┬───────┘  │
│         │ ConfigMap            │ ConfigMap            │          │
│         └──────────────────────┴──────────────────────┘          │
│                          │                                       │
│                   ┌──────┴──────┐                                │
│                   │  Operator   │◄── WebSocket (OpAMP)           │
│                   │  (1 pod)    │    wss://server:4320/v1/opamp  │
│                   └──────┬──────┘                                │
│                          │                                       │
└──────────────────────────┼───────────────────────────────────────┘
                           │
              ┌────────────┴────────────┐
              │  CollectorCtrl Server   │
              │  (UI + GitOps Engine)   │
              └─────────────────────────┘
```

### Key Design Decisions

| Decision | Rationale |
|----------|-----------|
| **One WebSocket per workload, not per pod** | Scales to 1000+ node clusters without connection exhaustion |
| **Operator is read-only by default** | Config changes flow through Git → ArgoCD/Flux; Operator only reports status |
| **Emergency mode is opt-in** | Direct ConfigMap patches only when `emergencyMode.enabled: true` |
| **OpAMP auth via shared secret** | Simple, stateless, works across clusters without token management overhead |

---

## Prerequisites

### Kubernetes Requirements

- **K8s version:** 1.25+ (uses `policy/v1` PodDisruptionBudget)
- **Control plane access:** Operator needs `pods`, `pods/exec`, `configmaps`, `daemonsets`, `deployments`, `statefulsets`, `events` permissions
- **Network:** Egress to CollectorCtrl server on port `4320` (or your custom port)

### Container Registry

The operator image is published to GitHub Container Registry (GHCR):

```
ghcr.io/collectorctrl/collectorctrl/operator:latest
```

> **Note:** GHCR images are private by default. You need a GitHub Personal Access Token (PAT) with `read:packages` scope to pull.

### CollectorCtrl Server

- Server must be reachable from the cluster (public IP, private VPC endpoint, or VPN)
- Server must have the OpAMP WebSocket endpoint enabled: `wss://<server-ip>:4320/v1/opamp`
- Server must share the same auth secret as the operator

---

## Installation

### Step 1: Deploy CRDs

Apply the Custom Resource Definitions:

```bash
kubectl apply -f https://raw.githubusercontent.com/CollectorCtrl/CollectorCtrl/main/deploy/crd.yaml
```

Or from a local file:

```bash
kubectl apply -f deploy/crd.yaml
```

Verify:

```bash
kubectl get crd collectormonitors.collectorctrl.io
```

### Step 2: Deploy RBAC

The operator needs broad permissions to discover workloads and patch ConfigMaps. Apply the RBAC manifest:

```bash
kubectl apply -f https://raw.githubusercontent.com/CollectorCtrl/CollectorCtrl/main/deploy/rbac.yaml
```

Verify the ServiceAccount exists:

```bash
kubectl get serviceaccount collectorctrl-operator -n collectorctrl
```

### Step 3: Deploy the Operator

Create the namespace and pull secret:

```bash
# Create namespace
kubectl create namespace collectorctrl

# Create GHCR pull secret (replace YOUR_PAT)
kubectl create secret docker-registry ghcr-pull-secret \
  --docker-server=ghcr.io \
  --docker-username=YOUR_GITHUB_USERNAME \
  --docker-password=YOUR_PAT \
  -n collectorctrl

# Apply the operator deployment
kubectl apply -f deploy/operator.yaml
```

Verify the pod is running:

```bash
kubectl get pods -n collectorctrl
```

Expected output:
```
NAME                                     READY   STATUS
collectorctrl-operator-5f57789bf-x6zrk   1/1     Running
```

### Step 4: Create the Auth Secret

The operator needs a secret to authenticate to the CollectorCtrl server. Create it in the same namespace as your `CollectorMonitor` CRs (e.g., `observability`):

```bash
kubectl create secret generic collectorctrl-auth \
  --from-literal=secret-key=collectorctrl-test-key-2024 \
  -n observability
```

> **Production:** Use a strong random secret. The server must have the same secret configured via `OPAMP_SHARED_SECRET` environment variable.

### Step 5: Apply CollectorMonitor CRs

For each collector workload you want to monitor, create a `CollectorMonitor` CR:

```yaml
apiVersion: collectorctrl.io/v1alpha1
kind: CollectorMonitor
metadata:
  name: coralogix-agent
  namespace: observability
  labels:
    k8s.cluster.name: "CollectorCtrl-EKS"
spec:
  workloadSelector:
    kind: DaemonSet
    matchLabels:
      app.kubernetes.io/instance: otel-coralogix-integration
      app.kubernetes.io/name: opentelemetry-agent
  configMapSelector:
    name: coralogix-opentelemetry-agent
    key: relay
  opampServer: wss://172.31.30.24:4320/v1/opamp
  auth:
    secretRef:
      name: collectorctrl-auth
      namespace: observability
      key: secret-key
  driftDetection:
    enabled: true
    interval: 60s
  emergencyMode:
    enabled: true
  healthCheck:
    enabled: true
    interval: 30s
    metricsPort: 8888
```

Apply:

```bash
kubectl apply -f collectormonitor.yaml
```

Verify:

```bash
kubectl get collectormonitor -n observability
```

Expected output:
```
NAME                PHASE    AGENTS   HEALTHY   AGE
coralogix-agent     Active   2        2         17h
```

---

## Configuration Reference

### CollectorMonitor Spec

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `workloadSelector` | WorkloadSelector | Yes | — | How to find the collector workload |
| `workloadSelector.kind` | string | No | "" | "DaemonSet", "Deployment", or "StatefulSet" |
| `workloadSelector.matchLabels` | map[string]string | Yes | — | Label selector to find the workload |
| `configMapSelector` | ConfigMapSelector | No | auto-discover | Which ConfigMap holds the collector config |
| `configMapSelector.name` | string | Yes | — | ConfigMap name |
| `configMapSelector.key` | string | No | "config.yaml" | Key within the ConfigMap |
| `opampServer` | string | Yes | — | WebSocket URL of the CollectorCtrl server |
| `auth.secretRef` | SecretRef | No | — | K8s Secret containing the auth key |
| `driftDetection.enabled` | bool | No | true | Compare running config vs ConfigMap |
| `emergencyMode.enabled` | bool | No | true | Allow direct ConfigMap patches from server |
| `healthCheck.enabled` | bool | No | true | Report pod health to server |
| `healthCheck.interval` | Duration | No | 30s | Health check frequency |

### Important: ConfigMap Key Naming

Different Helm charts use different ConfigMap keys for the collector configuration:

| Helm Chart | ConfigMap Key | Example |
|------------|---------------|---------|
| **Coralogix OpenTelemetry** | `relay` | `key: relay` |
| **OpenTelemetry Collector** | `config.yaml` | `key: config.yaml` |
| **Splunk OTel** | `relay.yaml` | `key: relay.yaml` |

Always verify the actual key with:

```bash
kubectl get configmap <configmap-name> -n <namespace> -o yaml
```

---

## Verifying the Deployment

### 1. Check Operator Logs

```bash
kubectl logs -n collectorctrl -l app.kubernetes.io/name=collectorctrl-operator --tail=100 -f
```

Look for:
- `OpAMPConnected: true` — connection established
- `Discovered: true` — workload found
- `ConfigMapResolved: true` — ConfigMap found
- `Active: true` — monitoring active

### 2. Check CollectorMonitor Status

```bash
kubectl get collectormonitor -n observability -o yaml
```

Look for:
- `phase: Active`
- `configMapRef` populated with name and key
- `conditions` showing `OpAMPConnected: True`

### 3. Check the Server UI

Navigate to the Fleet Overview in the CollectorCtrl UI. You should see:
- **K8s Cluster Name:** `CollectorCtrl-EKS` (or whatever you set)
- **Workload Name:** `coralogix-opentelemetry-agent` (from the agent ID)
- **Status:** `Healthy` or `Degraded`
- **Config:** `Pending` (until effective config is reported) or `Applied`

### 4. Verify Pod-Level Topology

The operator reports per-pod health including node name, IP, and readiness. In the server UI, expand the agent detail to see individual pod status.

---

## Troubleshooting

### Issue 1: `ErrImagePull` — Cannot Pull Operator Image

**Symptoms:**
```
Failed to pull image "ghcr.io/rahulmhatre2505/collectorctrl-k8s-code/operator:v1.0.0": rpc error: code = Unknown desc = Error response from daemon: unauthorized
```

**Root Cause:** Image path mismatch or missing GHCR pull secret.

**Fix:**

```bash
# 1. Verify the correct image path
kubectl get deployment collectorctrl-operator -n collectorctrl -o jsonpath='{.spec.template.spec.containers[0].image}'

# 2. Create GHCR pull secret with PAT (read:packages scope)
kubectl create secret docker-registry ghcr-pull-secret \
  --docker-server=ghcr.io \
  --docker-username=YOUR_GITHUB_USERNAME \
  --docker-password=YOUR_PAT \
  -n collectorctrl

# 3. Patch the deployment to use the secret
kubectl patch deployment collectorctrl-operator -n collectorctrl \
  --type='json' -p='[{"op": "add", "path": "/spec/template/spec/imagePullSecrets", "value": [{"name": "ghcr-pull-secret"}]}]'
```

### Issue 2: `CreateContainerConfigError` — Missing Auth Secret

**Symptoms:**
```
Warning  Failed     5s (x3 over 20s)  kubelet  Error: secret "collectorctrl-auth" not found
```

**Root Cause:** The `CollectorMonitor` CR references a secret that doesn't exist in the target namespace.

**Fix:**

```bash
# Create the secret in the same namespace as the CollectorMonitor
kubectl create secret generic collectorctrl-auth \
  --from-literal=secret-key=YOUR_SECRET \
  -n observability
```

> The secret must be in the **same namespace** as the `CollectorMonitor` CR, not the operator namespace.

### Issue 3: `dial tcp 10.100.0.1:443: i/o timeout` — Cannot Reach K8s API

**Symptoms:**
```
Get "https://10.100.0.1:443/api/v1/namespaces/observability/configmaps": dial tcp 10.100.0.1:443: i/o timeout
```

**Root Cause:** EKS security group doesn't allow the operator pod to reach the K8s API server.

**Fix:**

```bash
# 1. Find the cluster security group
aws eks describe-cluster --name CollectorCtrl-EKS --region us-east-1 \
  --query 'cluster.resourcesVpcConfig.clusterSecurityGroupId'

# 2. Add self-referencing ingress rule on port 443
aws ec2 authorize-security-group-ingress \
  --group-id sg-02d8ca04af2883f7b \
  --protocol tcp \
  --port 443 \
  --source-group sg-02d8ca04af2883f7b
```

> The operator must be able to reach `10.100.0.1:443` (the K8s API server) to list pods, ConfigMaps, and workloads.

### Issue 4: `config key "relay.yaml" not found in ConfigMap` — Wrong ConfigMap Key

**Symptoms:**
```
Drift detection failed: config key "relay.yaml" not found in ConfigMap
```

**Root Cause:** The `configMapSelector.key` in the `CollectorMonitor` CR doesn't match the actual key in the ConfigMap.

**Fix:**

```bash
# 1. Check actual keys in the ConfigMap
kubectl get configmap coralogix-opentelemetry-agent -n observability -o yaml

# 2. Patch the CollectorMonitor with the correct key
kubectl patch collectormonitor coralogix-agent -n observability \
  --type='json' -p='[{"op": "replace", "path": "/spec/configMapSelector/key", "value": "relay"}]'
```

### Issue 5: `opamp start: health is nil` — OpAMP Client Fails to Start

**Symptoms:**
```
Failed to establish OpAMP connection: opamp start: health is nil
```

**Root Cause:** The `opamp-go` library requires `SetHealth()` to be called **before** `Start()`. If the client starts without an initial health report, the server rejects the connection.

**Fix:** This is an operator code fix. Ensure the OpAMP client wrapper calls `SetHealth()` with an initial status before `Start()`:

```go
// In the operator's OpAMP client initialization:
if err := c.client.SetHealth(&protobufs.ComponentHealth{
    Healthy: false,
    ComponentHealthMap: map[string]*protobufs.ComponentHealth{
        "fleet": {Healthy: false, Status: "initializing"},
    },
}); err != nil {
    return fmt.Errorf("set initial health: %w", err)
}

if err := c.client.Start(ctx, settings); err != nil {
    return fmt.Errorf("opamp start: %w", err)
}
```

### Issue 6: Only One K8s Agent Shows in Server UI (Duplicate UUID)

**Symptoms:** You have two `CollectorMonitor` CRs (agent + collector) but only one shows in the Fleet Overview.

**Root Cause:** The `instanceUID()` function truncates the agent ID to 16 bytes. Both IDs start with the same prefix, so they collide:
```
k8s://CollectorCtrl-EKS/observability/DaemonSet/coralogix-opentelemetry-agent
k8s://CollectorCtrl-EKS/observability/Deployment/coralogix-opentelemetry-collector
         ↑ first 16 bytes: "k8s://CollectorCt" — identical
```

**Fix:** Hash the full agent ID to generate a stable 16-byte UUID:

```go
func (c *Client) instanceUID() [16]byte {
    var uid [16]byte
    h := md5.Sum([]byte(c.config.AgentID))
    copy(uid[:], h[:])
    return uid
}
```

### Issue 7: VM Agents Cannot Connect After Enabling Auth

**Symptoms:** Linux/Windows VM agents show as `Disconnected` in the UI after the server enforces `Authorization: Secret-Key <key>`.

**Root Cause:** The VM supervisor (pre-v1.0) doesn't send the `Authorization` header on the WebSocket handshake.

**Fix:** Update the supervisor to include the auth header:

```go
// In supervisor's opamp_client.go:
settings := types.StartSettings{
    OpAMPServerURL: s.config.Server.Endpoint,
    TLSConfig:      tlsConfig,
    InstanceUid:    types.InstanceUid(s.instanceId),
    Header:         opampAuthHeader(), // ← ADD THIS
    // ... rest of settings
}

func opampAuthHeader() http.Header {
    secret := os.Getenv("COLLECTORCTRL_OPAMP_SECRET")
    if secret == "" {
        secret = os.Getenv("OPAMP_SHARED_SECRET")
    }
    if secret == "" {
        secret = "collectorctrl-test-key-2024"
    }
    return http.Header{
        "Authorization": []string{"Secret-Key " + secret},
    }
}
```

**Quick workaround:** Disable auth on the server temporarily:
```bash
export OPAMP_AUTH_DISABLED=true
sudo systemctl restart collectorctrl
```

> **Important:** Re-enable auth after all supervisors are updated.

### Issue 8: K8s Agents Show UUID Instead of Workload Name

**Symptoms:** The Fleet Overview shows `6b38733a-2f2f...` instead of `coralogix-opentelemetry-agent`.

**Root Cause:** The server UI defaults to `InstanceIdStr` for the display name and only falls back to `host.name`, which doesn't exist for K8s fleet-level agents.

**Fix:** Update the server's `newFleetAgentRow()` to use `K8sWorkloadName` when available:

```go
if agent.K8sWorkloadName != "" {
    row.DisplayName = agent.K8sWorkloadName
} else if row.HostName != "\u2014" {
    row.DisplayName = row.HostName
}
```

### Issue 9: Operator Pod Keeps Crash-Looping After Restart

**Symptoms:** After `kubectl rollout restart`, the new pod starts but immediately exits.

**Root Cause:** Mixed logs from old and new pods during the rolling restart. The old pod is still terminating while the new one starts.

**Fix:** Wait for the rollout to complete, then check only the new pod's logs:

```bash
kubectl rollout status deployment/collectorctrl-operator -n collectorctrl

# Get the newest pod name
kubectl get pod -n collectorctrl -l app.kubernetes.io/name=collectorctrl-operator \
  --sort-by=.metadata.creationTimestamp -o jsonpath='{.items[-1].metadata.name}'

# Check only that pod's logs
kubectl logs -n collectorctrl <newest-pod-name> --tail=100
```

---

## Important Learnings & Gotchas

### 1. One WebSocket Per Workload, Not Per Pod

The operator connects **one** OpAMP client per `CollectorMonitor` CR (i.e., per workload). It then reports all pod statuses under that single connection. This design prevents connection exhaustion in large clusters.

**Implication:** The server sees one "agent" per workload, not per pod. The per-pod details are in the `ComponentHealthMap` under `pod/<pod-name>`.

### 2. The Operator Is Read-Only by Default

Standard config changes go through:
1. User edits config in UI
2. Server commits to Git
3. ArgoCD/Flux applies the new ConfigMap
4. Workload rolls out automatically

The operator only patches ConfigMaps directly when **emergency mode** is enabled and the server sends an emergency command.

### 3. ConfigMap Key Names Vary by Helm Chart

Always verify the actual key name before deploying:
- Coralogix: `relay`
- OpenTelemetry Helm: `config.yaml`
- Splunk: `relay.yaml`

### 4. K8s API Access Requires Security Group Ingress on EKS

The operator runs inside the cluster but needs to reach the K8s API server at `10.100.0.1:443`. On EKS, this requires a **self-referencing security group ingress rule** on the cluster security group.

### 5. GHCR Images Require Authentication

GitHub Container Registry images are private by default. You must create a `docker-registry` secret with a PAT that has `read:packages` scope, even for public repos.

### 6. OpAMP Auth Must Be Consistent Across All Agents

The server, K8s operator, and VM supervisors must all use the same auth mechanism:

| Component | Secret Source |
|-----------|--------------|
| Server | `OPAMP_SHARED_SECRET` env var |
| K8s Operator | `collectorctrl-auth` K8s Secret |
| VM Supervisor | `COLLECTORCTRL_OPAMP_SECRET` or `OPAMP_SHARED_SECRET` env var |

If auth is enabled on the server but one agent type doesn't send the header, that agent type will show as `Disconnected`.

### 7. The `opamp-go` Library Requires Initial Health Before `Start()`

The official OpAMP Go client requires `SetHealth()` to be called before `Start()`. If you reverse the order, the server rejects the connection with `health is nil`.

### 8. Agent IDs Must Be Unique

The OpAMP protocol uses a 16-byte `InstanceUid` to identify agents. If two agents share the same UID (e.g., by truncating a long string to 16 bytes), the server deduplicates them and only one appears.

**Solution:** Hash the full agent ID (e.g., with MD5) to produce a stable, unique 16-byte UUID.

### 9. Drift Detection Relies on Pod Annotations

The current drift detection implementation checks the pod annotation `collectorctrl.io/config-hash` against the ConfigMap content hash. For full fidelity, extend this to `exec` into pods and read the mounted config file directly.

### 10. Emergency Mode Triggers Rolling Restart

When the server sends an emergency config override:
1. The operator patches the ConfigMap
2. The operator adds `kubectl.kubernetes.io/restartedAt` annotation to the workload
3. Kubernetes automatically performs a rolling restart

This works for DaemonSet, Deployment, and StatefulSet.

---

## Moving the Image to the Official Repo

The operator image is currently at:
```
ghcr.io/rahulmhatre2505/collectorctrl-k8s-code/operator:latest
```

After moving to the official org:
```
ghcr.io/collectorctrl/collectorctrl/operator:latest
```

Update the deployment:

```bash
kubectl set image deployment/collectorctrl-operator \
  operator=ghcr.io/collectorctrl/collectorctrl/operator:latest \
  -n collectorctrl
```

Also update the GitHub Actions workflow to push to the new registry and update any documentation links.

---

## Related Resources

- [CollectorCtrl Server Installation Guide](./server-installation.md)
- [VM Supervisor Installation Guide](./supervisor-installation.md)
- [OpAMP Protocol Specification](https://opentelemetry.io/docs/specs/opamp/)
- [OpenTelemetry Collector Helm Charts](https://github.com/open-telemetry/opentelemetry-helm-charts)
- [Coralogix OpenTelemetry Integration](https://github.com/coralogix/opentelemetry-helm-charts)

---

**Last Updated:** 2026-07-10  
**Maintainer:** CollectorCtrl Team  
**Issues:** https://github.com/CollectorCtrl/CollectorCtrl/issues
