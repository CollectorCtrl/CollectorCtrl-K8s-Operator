# CollectorCtrl Operator — Quick Deploy Script for EKS
# Server IP: 172.31.30.24 (this machine)
# Registry: ghcr.io/rahulmhatre2505/collectorctrl-operator

set -e

echo "=== CollectorCtrl Operator Deploy ==="
echo "Server: 172.31.30.24:4320"
echo "Registry: ghcr.io/rahulmhatre2505/collectorctrl-operator"
echo ""

# 1. Create namespace
echo "[1/5] Creating namespace..."
kubectl apply -f - <<EOF
apiVersion: v1
kind: Namespace
metadata:
  name: collectorctrl
EOF

# 2. Create auth secret
echo "[2/5] Creating auth secret..."
read -sp "Enter OpAMP shared secret key: " SECRET_KEY
echo ""
kubectl create secret generic collectorctrl-auth \
  --namespace collectorctrl \
  --from-literal=secret-key="$SECRET_KEY" \
  --dry-run=client -o yaml | kubectl apply -f -

# 3. Apply CRD
echo "[3/5] Applying CRD..."
kubectl apply -f deploy/crd.yaml

# 4. Apply RBAC
echo "[4/5] Applying RBAC..."
kubectl apply -f deploy/rbac.yaml

# 5. Apply operator deployment
echo "[5/5] Applying operator deployment..."
kubectl apply -f deploy/deployment.yaml

echo ""
echo "=== Deployment Complete ==="
echo ""
echo "Watch pod status:"
echo "  kubectl get pods -n collectorctrl -w"
echo ""
echo "View operator logs:"
echo "  kubectl logs -n collectorctrl -l app.kubernetes.io/name=collectorctrl-operator -f"
echo ""
echo "Next: Create a CollectorMonitor for your Coralogix collectors:"
echo "  kubectl apply -f deploy/coralogix-collectormonitor.yaml"
