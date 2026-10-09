#!/usr/bin/env bash
# CollectorCtrl Kubernetes observer: quick deploy with kubectl (no Helm).
# The Helm chart (helm-charts/collectorctrl-operator) is the supported install path.
#
# Usage (from the repository root):
#   OPAMP_SERVER=wss://collectorctrl.example.com:4320/v1/opamp ./deploy/deploy.sh
#   OPAMP_SERVER=wss://... OPAMP_CA_FILE=./ca.pem ./deploy/deploy.sh   # private CA
#
# Environment:
#   OPAMP_SERVER      required  wss:// OpAMP endpoint of your CollectorCtrl server
#   OPAMP_CA_FILE     optional  CA bundle (PEM) if the server uses a private CA
#   ENROLL_TOKEN      optional  cce_... token; prompted for if not set
# Image: ghcr.io/collectorctrl/collectorctrl-k8s-operator/operator:latest

set -euo pipefail

NAMESPACE=collectorctrl
PLACEHOLDER="wss://collectorctrl.example.internal:4320/v1/opamp"

if [[ -z "${OPAMP_SERVER:-}" ]]; then
  echo "Set OPAMP_SERVER, e.g. OPAMP_SERVER=wss://collectorctrl.example.com:4320/v1/opamp $0" >&2
  exit 1
fi
if [[ "$OPAMP_SERVER" != wss://* ]]; then
  echo "OPAMP_SERVER must use wss:// (certificate verification cannot be disabled)." >&2
  exit 1
fi
if [[ -n "${OPAMP_CA_FILE:-}" && ! -f "$OPAMP_CA_FILE" ]]; then
  echo "OPAMP_CA_FILE not found: $OPAMP_CA_FILE" >&2
  exit 1
fi

echo "=== CollectorCtrl observer deploy ==="
echo "Server: $OPAMP_SERVER"
echo ""

echo "[1/6] Namespace"
kubectl apply -f deploy/namespace.yaml

echo "[2/6] Enrollment Secret"
if [[ -z "${ENROLL_TOKEN:-}" ]]; then
  echo "Create a token in CollectorCtrl: Settings -> API Tokens -> Agent Enrollment."
  echo "Each monitored workload/container uses one enrollment."
  read -rsp "Enrollment token (cce_...): " ENROLL_TOKEN
  echo ""
fi
if [[ "$ENROLL_TOKEN" != cce_* ]]; then
  echo "Expected an enrollment token starting with cce_." >&2
  exit 1
fi
kubectl create secret generic collectorctrl-auth \
  --namespace "$NAMESPACE" \
  --from-literal=enrollment-token="$ENROLL_TOKEN" \
  --dry-run=client -o yaml | kubectl apply -f -

echo "[3/6] CRD"
kubectl apply -f deploy/crd.yaml

echo "[4/6] RBAC"
kubectl apply -f deploy/rbac.yaml

echo "[5/6] Credential storage (retained PVC)"
kubectl apply -f deploy/credentials-pvc.yaml

echo "[6/6] Observer deployment"
sed "s#${PLACEHOLDER}#${OPAMP_SERVER}#" deploy/deployment.yaml | kubectl apply -f -

if [[ -n "${OPAMP_CA_FILE:-}" ]]; then
  echo "Adding private CA from $OPAMP_CA_FILE"
  kubectl create configmap collectorctrl-opamp-ca \
    --namespace "$NAMESPACE" \
    --from-file=ca.crt="$OPAMP_CA_FILE" \
    --dry-run=client -o yaml | kubectl apply -f -
  if ! kubectl get deployment collectorctrl-operator -n "$NAMESPACE" \
      -o jsonpath='{.spec.template.spec.volumes[*].name}' | grep -qw opamp-ca; then
    kubectl patch deployment collectorctrl-operator -n "$NAMESPACE" --type=json -p='[
      {"op":"add","path":"/spec/template/spec/volumes/-","value":{"name":"opamp-ca","configMap":{"name":"collectorctrl-opamp-ca"}}},
      {"op":"add","path":"/spec/template/spec/containers/0/volumeMounts/-","value":{"name":"opamp-ca","mountPath":"/etc/collectorctrl/ca","readOnly":true}},
      {"op":"add","path":"/spec/template/spec/containers/0/env/-","value":{"name":"OPAMP_CA_FILE","value":"/etc/collectorctrl/ca/ca.crt"}}
    ]'
  fi
fi

echo ""
echo "=== Done ==="
echo "Watch the observer:   kubectl get pods -n $NAMESPACE -w"
echo "Observer logs:        kubectl logs -n $NAMESPACE -l app.kubernetes.io/name=collectorctrl-operator -f"
echo "Next: edit deploy/example-collectormonitor.yaml (workload, container, opampServer"
echo "      must equal $OPAMP_SERVER) and apply it."
