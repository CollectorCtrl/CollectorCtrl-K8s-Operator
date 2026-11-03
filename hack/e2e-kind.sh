#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright 2026 CollectorCtrl
#
# End-to-end smoke test on a kind cluster (used by CI, runnable locally):
# installs the chart with a locally built image, adds sample workloads, and
# checks that the operator starts, holds only the permissions it documents,
# reconciles without RBAC errors, injects the Java agent into opted-in pods
# (one pod for a canary), and keeps its credential volume on uninstall.
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
  --set opamp.enrollmentSecret=collectorctrl-auth --set clusterName=e2e \
  --set instrumentation.enabled=true --set instrumentation.defaultProfile.create=true \
  --set instrumentation.defaultProfile.exporterEndpoint=http://otel-gateway.observability:4318
kubectl -n "$NS" rollout status "deploy/${RELEASE}" --timeout=120s || fail "operator did not become ready"

step "RBAC is exactly what the docs promise"
can() { kubectl auth can-i --as="$SA" "$@" >/dev/null 2>&1; }
for want in "list deployments.apps -A" "watch pods -A" "list configmaps -A" "list services -A" \
  "list namespaces" "list mutatingwebhookconfigurations.admissionregistration.k8s.io" "get namespaces/kube-system" \
  "list instrumentations.collectorctrl.io -A" "patch instrumentations.collectorctrl.io --subresource=status -A" "delete pods -A" \
  "update mutatingwebhookconfigurations.admissionregistration.k8s.io/${RELEASE}-java"; do
  # shellcheck disable=SC2086
  can $want || fail "missing permission: $want"
done
for deny in "get secrets -A" "list secrets -A" "update deployments.apps -A" "patch configmaps -A" \
  "create pods -A" "update pods -A" "update mutatingwebhookconfigurations.admissionregistration.k8s.io/some-other-webhook" \
  "create mutatingwebhookconfigurations.admissionregistration.k8s.io" "update instrumentations.collectorctrl.io -A" \
  "update collectormonitors.collectorctrl.io -A"; do
  # shellcheck disable=SC2086
  if can $deny; then fail "unexpected permission: $deny"; fi
done
echo "RBAC ok"

step "Webhook: CA bundle published, opted-in Java pods get the agent, canary is one pod"
for _ in $(seq 1 30); do
  CA="$(kubectl get mutatingwebhookconfiguration "${RELEASE}-java" -o jsonpath='{.webhooks[0].clientConfig.caBundle}')"
  [ -n "$CA" ] && break
  sleep 2
done
[ -n "$CA" ] || fail "webhook caBundle was not published"
kubectl apply -f - <<'EOF2'
apiVersion: apps/v1
kind: Deployment
metadata: {name: billing, namespace: shop}
spec:
  replicas: 1
  selector: {matchLabels: {app: billing}}
  template:
    metadata: {labels: {app: billing, collectorctrl.io/instrument-java: "true"}}
    spec:
      containers:
        - name: app
          image: busybox:1.36
          command: ["sh", "-c", "sleep 3600"]
          env: [{name: JAVA_TOOL_OPTIONS, value: "-Xmx256m"}, {name: JAVA_HOME, value: /opt/java}]
---
apiVersion: apps/v1
kind: Deployment
metadata: {name: inventory, namespace: shop, annotations: {collectorctrl.io/language: java}}
spec:
  replicas: 2
  selector: {matchLabels: {app: inventory}}
  template:
    metadata: {labels: {app: inventory, collectorctrl.io/instrument-java: canary}}
    spec:
      containers:
        - name: app
          image: busybox:1.36
          command: ["sh", "-c", "sleep 3600"]
---
apiVersion: apps/v1
kind: Deployment
metadata: {name: legacy, namespace: shop}
spec:
  replicas: 1
  selector: {matchLabels: {app: legacy}}
  template:
    metadata: {labels: {app: legacy, collectorctrl.io/instrument-java: "true"}}
    spec:
      containers:
        - name: app
          image: busybox:1.36
          command: ["sh", "-c", "sleep 3600"]
          env: [{name: JAVA_TOOL_OPTIONS, value: "-javaagent:/opt/dd/dd-java-agent.jar"}]
EOF2
kubectl -n shop rollout status deploy/billing --timeout=180s || fail "billing did not roll out (agent init image pull?)"
kubectl -n shop rollout status deploy/inventory --timeout=180s || fail "inventory did not roll out"
kubectl -n shop rollout status deploy/legacy --timeout=120s || fail "legacy did not roll out"
BILLING="$(kubectl -n shop get pods -l app=billing -o jsonpath='{.items[0].metadata.name}')"
kubectl -n shop get pod "$BILLING" -o jsonpath='{.spec.initContainers[*].name}' | grep -q collectorctrl-java-agent || fail "billing pod has no agent init container"
JTO="$(kubectl -n shop get pod "$BILLING" -o jsonpath='{.spec.containers[0].env[?(@.name=="JAVA_TOOL_OPTIONS")].value}')"
case "$JTO" in
  "-Xmx256m -javaagent:/collectorctrl/java/opentelemetry-javaagent.jar"*) ;;
  *) fail "billing JAVA_TOOL_OPTIONS was not merged: $JTO" ;;
esac
kubectl -n shop get pod "$BILLING" -o jsonpath='{.spec.containers[0].env[?(@.name=="OTEL_EXPORTER_OTLP_ENDPOINT")].value}' | grep -q 'otel-gateway.observability:4318' || fail "billing pod lacks exporter settings"
[ "$(kubectl -n shop get pod "$BILLING" -o jsonpath='{.metadata.annotations.collectorctrl\.io/instrumentation-status}')" = "injected" ] || fail "billing pod not marked injected"
INSTRUMENTED="$(kubectl -n shop get pods -l app=inventory,collectorctrl.io/instrumented=java --no-headers | wc -l | tr -d ' ')"
[ "$INSTRUMENTED" = "1" ] || fail "canary should instrument exactly one inventory pod, got $INSTRUMENTED"
LEGACY="$(kubectl -n shop get pods -l app=legacy -o jsonpath='{.items[0].metadata.name}')"
kubectl -n shop get pod "$LEGACY" -o jsonpath='{.metadata.annotations.collectorctrl\.io/instrumentation-reason}' | grep -q '^agent_present' || fail "legacy pod with a Datadog agent was not skipped"
[ -z "$(kubectl -n shop get pod "$LEGACY" -o jsonpath='{.spec.initContainers}')" ] || fail "legacy pod was changed"
for _ in $(seq 1 30); do
  PODS="$(kubectl -n "$NS" get instrumentation default -o jsonpath='{.status.podsInstrumented}')"
  [ "${PODS:-0}" = "2" ] && break
  sleep 2
done
[ "${PODS:-0}" = "2" ] || fail "Instrumentation status reports ${PODS:-0} pods, want 2"
echo "Webhook ok: merged flags, exporter set, canary limited to one pod, foreign agent left alone, status rolled up"

step "Canary revert: a broken agent image reverts the revision once and stays reverted"
kubectl apply -f - <<'EOF2'
apiVersion: collectorctrl.io/v1alpha1
kind: Instrumentation
metadata: {name: broken-agent, namespace: shop}
spec:
  exporter: {endpoint: http://otel-gateway.observability:4318}
  java: {image: "busybox:1.36"}
  canary: {maxRestarts: 2, autoRevert: true}
---
apiVersion: apps/v1
kind: Deployment
metadata: {name: flaky, namespace: shop}
spec:
  replicas: 1
  selector: {matchLabels: {app: flaky}}
  template:
    metadata:
      labels: {app: flaky, collectorctrl.io/instrument-java: canary}
      annotations: {collectorctrl.io/instrumentation: broken-agent}
    spec:
      containers:
        - name: app
          image: busybox:1.36
          command: ["sh", "-c", "sleep 3600"]
          env: [{name: JAVA_HOME, value: /opt/java}]
EOF2
for _ in $(seq 1 60); do
  ST="$(kubectl -n shop get instrumentation broken-agent -o jsonpath='{.status.workloads[0].state}')"
  [ "$ST" = "reverted" ] && break
  sleep 3
done
[ "$ST" = "reverted" ] || fail "broken-agent canary was not reverted (state: ${ST:-none})"
kubectl -n shop rollout status deploy/flaky --timeout=120s || fail "flaky did not recover after the revert"
FLAKY="$(kubectl -n shop get pods -l app=flaky --field-selector=status.phase=Running -o jsonpath='{.items[0].metadata.name}')"
kubectl -n shop get pod "$FLAKY" -o jsonpath='{.metadata.annotations.collectorctrl\.io/instrumentation-reason}' | grep -q '^reverted:' || fail "replacement pod was not skipped as reverted"
[ -z "$(kubectl -n shop get pod "$FLAKY" -o jsonpath='{.spec.initContainers}')" ] || fail "replacement pod still has the agent"
kubectl -n shop delete pod "$FLAKY" --wait=false; sleep 20
kubectl -n shop rollout status deploy/flaky --timeout=120s || fail "flaky did not recover after pod deletion"
REVERTS="$(kubectl -n "$NS" logs "deploy/${RELEASE}" | grep -c 'canary reverted' || true)"
[ "$REVERTS" = "1" ] || fail "expected exactly one revert, saw $REVERTS"
ST="$(kubectl -n shop get instrumentation broken-agent -o jsonpath='{.status.workloads[0].state}')"
[ "$ST" = "reverted" ] || fail "revert record lost after pod recreation (state: ${ST:-none})"
echo "Revert ok: one revert, replacement pods start without the agent, record kept"

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
