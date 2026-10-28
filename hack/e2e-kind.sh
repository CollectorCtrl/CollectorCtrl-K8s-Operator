#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright 2026 CollectorCtrl
#
# End-to-end smoke test on a kind cluster (used by CI, runnable locally):
# installs the chart with a locally built image, adds sample workloads, and
# checks that the operator starts, holds only the permissions it documents,
# reconciles without RBAC errors, and keeps its credential volume on uninstall.
# No CollectorCtrl server is needed: the OpAMP endpoint is unreachable on
# purpose, so connection errors are expected and ignored.
#
# Usage: hack/e2e-kind.sh <image:tag>
set -euo pipefail

IMAGE="${1:?usage: hack/e2e-kind.sh <image:tag>}"
NS=collectorctrl-operator
RELEASE=collectorctrl-operator
SA="system:serviceaccount:${NS}:${RELEASE}"
CHART="$(cd "$(dirname "$0")/.." && pwd)/helm-charts/collectorctrl-operator"

fail() { echo "FAIL: $*" >&2; kubectl -n "$NS" logs "deploy/${RELEASE}" --tail=200 >&2 || true; exit 1; }
step() { echo; echo "==> $*"; }

step "Sample workloads: an opted-in collector and two applications"
kubectl create namespace observability --dry-run=client -o yaml | kubectl apply -f -
kubectl create namespace shop --dry-run=client -o yaml | kubectl apply -f -
kubectl apply -f - <<'EOF'
apiVersion: v1
kind: ConfigMap
metadata: {name: agent-config, namespace: observability}
data:
  config.yaml: |
    receivers: {otlp: {protocols: {grpc: {}}}, kubeletstats: {}}
    exporters: {otlp: {endpoint: gateway.observability:4317}}
    service: {pipelines: {metrics: {receivers: [kubeletstats, otlp], exporters: [otlp]}}}
---
apiVersion: apps/v1
kind: DaemonSet
metadata:
  name: otel-agent
  namespace: observability
  labels: {collectorctrl.io/observe: "true", app.kubernetes.io/instance: platform}
spec:
  selector: {matchLabels: {app: otel-agent}}
  template:
    metadata: {labels: {app: otel-agent}}
    spec:
      containers:
        - name: otel-collector
          image: busybox:1.36
          command: ["sh", "-c", "sleep 3600"]
          volumeMounts: [{name: config, mountPath: /etc/otel}]
      volumes: [{name: config, configMap: {name: agent-config}}]
---
apiVersion: apps/v1
kind: Deployment
metadata: {name: orders, namespace: shop}
spec:
  selector: {matchLabels: {app: orders}}
  template:
    metadata: {labels: {app: orders}}
    spec:
      containers:
        - name: app
          image: busybox:1.36
          command: ["sh", "-c", "sleep 3600"]
          env: [{name: JAVA_TOOL_OPTIONS, value: "-Xmx256m"}, {name: JAVA_HOME, value: /opt/java}]
---
apiVersion: apps/v1
kind: Deployment
metadata: {name: web, namespace: shop, annotations: {collectorctrl.io/language: nodejs}}
spec:
  selector: {matchLabels: {app: web}}
  template:
    metadata: {labels: {app: web}}
    spec:
      containers:
        - name: app
          image: busybox:1.36
          command: ["sh", "-c", "sleep 3600"]
          env: [{name: NODE_OPTIONS, value: "--require newrelic"}]
EOF

step "Install the chart"
kubectl create namespace "$NS" --dry-run=client -o yaml | kubectl apply -f -
kubectl -n "$NS" create secret generic collectorctrl-auth --from-literal=enrollment-token=cce_e2e_not_a_real_token \
  --dry-run=client -o yaml | kubectl apply -f -
helm upgrade --install "$RELEASE" "$CHART" --namespace "$NS" --wait --timeout 3m \
  --set image.repository="${IMAGE%:*}" --set image.tag="${IMAGE##*:}" --set image.pullPolicy=Never \
  --set opamp.server=wss://collectorctrl.invalid:4320/v1/opamp \
  --set opamp.enrollmentSecret=collectorctrl-auth --set clusterName=e2e
kubectl -n "$NS" rollout status "deploy/${RELEASE}" --timeout=120s || fail "operator did not become ready"

step "RBAC is exactly what the docs promise"
can() { kubectl auth can-i --as="$SA" "$@" >/dev/null 2>&1; }
for want in "list deployments.apps -A" "watch pods -A" "list configmaps -A" "list services -A" \
  "list namespaces" "list mutatingwebhookconfigurations.admissionregistration.k8s.io" "get namespaces/kube-system"; do
  # shellcheck disable=SC2086
  can $want || fail "missing permission: $want"
done
for deny in "get secrets -A" "list secrets -A" "update deployments.apps -A" "patch configmaps -A" \
  "delete pods -A" "create pods -A" "update mutatingwebhookconfigurations.admissionregistration.k8s.io"; do
  # shellcheck disable=SC2086
  if can $deny; then fail "unexpected permission: $deny"; fi
done
echo "RBAC ok"

step "Reconcile for a while, then read the logs"
sleep 45
LOGS="$(kubectl -n "$NS" logs "deploy/${RELEASE}")"
grep -q 'starting manager' <<<"$LOGS" || fail "no startup log line"
if grep -iE 'is forbidden|panic|unable to create controller' <<<"$LOGS"; then fail "errors in operator logs"; fi
RESTARTS="$(kubectl -n "$NS" get pods -l app.kubernetes.io/name=collectorctrl-operator -o jsonpath='{.items[0].status.containerStatuses[0].restartCount}')"
[ "${RESTARTS:-0}" = "0" ] || fail "operator restarted ${RESTARTS} times"
echo "Operator healthy, no RBAC errors, no restarts"

step "Uninstall keeps the credential volume"
helm uninstall "$RELEASE" --namespace "$NS" --wait
kubectl -n "$NS" get pvc "${RELEASE}-credentials" >/dev/null || fail "credential PVC was deleted on uninstall"
echo "Credential PVC retained"

echo; echo "E2E passed"
