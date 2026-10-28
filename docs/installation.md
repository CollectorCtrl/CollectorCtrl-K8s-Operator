# Installation

The Helm chart in [`helm-charts/collectorctrl-operator`](../helm-charts/collectorctrl-operator) is the supported way
to install the operator. Raw manifests for clusters without Helm are in [`deploy/`](../deploy/README.md).

## Prerequisites

| Requirement | Details |
| --- | --- |
| Kubernetes | 1.30 or later (CI tests 1.33 and 1.37); permission to create a CRD, a ClusterRole and a ClusterRoleBinding (or a namespace-scoped install, see below) |
| CollectorCtrl server | One with Kubernetes role and topology support (server `main` from 2026-10-19 or later) |
| Network | Pods can reach the server over `wss://` (default port 4320) and the Kubernetes API |
| TLS | The server certificate is trusted by the image's system roots or by a CA you provide. Verification cannot be turned off |
| Enrollment token | An **Agent Enrollment** token (`cce_…`) from **Settings → API Tokens → Agent Enrollment**. The cluster connection uses one enrollment and each observed collector one more; size its use limit accordingly |
| Storage | A storage class that can provision a 128 Mi ReadWriteOnce volume for credentials |

## Install with Helm

```bash
kubectl create namespace collectorctrl-operator
kubectl -n collectorctrl-operator create secret generic collectorctrl-auth \
  --from-literal=enrollment-token=cce_YOUR_TOKEN

helm upgrade --install collectorctrl-operator oci://ghcr.io/collectorctrl/charts/collectorctrl-operator \
  --version 0.4.0 --namespace collectorctrl-operator \
  --set opamp.server=wss://collectorctrl.example.com:4320/v1/opamp \
  --set opamp.enrollmentSecret=collectorctrl-auth \
  --set clusterName=prod-eu
```

The chart is published as a signed OCI artifact from release 0.8.0 (chart 0.4.0) on; verify it as described in
[verifying.md](verifying.md). From a checkout, use `./helm-charts/collectorctrl-operator` instead. Air-gapped
clusters can mirror both the chart and the image to an internal registry.

Check it started and connected:

```bash
kubectl -n collectorctrl-operator rollout status deploy/collectorctrl-operator
kubectl -n collectorctrl-operator logs deploy/collectorctrl-operator | grep -i "starting manager"
```

The cluster appears in CollectorCtrl under **Fleet → Kubernetes** within a minute, with a list of collector
workloads that have not opted in yet. Opt one in:

```bash
kubectl -n observability label daemonset/otel-agent collectorctrl.io/observe=true
```

Prefer setting the label through the collector's own Helm values or GitOps source, so the next deploy does not remove
it.

### Image tags

The chart uses the image tag that matches its `appVersion` (for example `0.8.0`). Release tags are immutable.
`main` and `latest` follow the main branch and are for testing only:

```bash
--set image.tag=main --set image.pullPolicy=Always
```

If your cluster cannot pull from GHCR directly, mirror the image and set `image.repository`, plus
`imagePullSecrets` if your registry needs credentials.

## Private CA

```bash
kubectl -n collectorctrl-operator create configmap collectorctrl-opamp-ca --from-file=ca.crt=./ca.pem
helm upgrade --install ... --set opampCAConfigMap=collectorctrl-opamp-ca
```

The bundle is added to the system roots; the key must be `ca.crt`.

## Namespace-scoped install

When you cannot grant cluster-wide read access, the operator can watch only its own namespace. The `kube-system`
UID is not readable then, so set a stable, unique cluster ID yourself:

```bash
helm upgrade --install collectorctrl-operator ./helm-charts/collectorctrl-operator \
  --namespace observability \
  --set rbac.clusterScoped=false \
  --set clusterID=prod-eu-1 \
  ...
```

Only collectors in that namespace are observed and listed. Never change `clusterID` afterwards: it is part of every
identity, so changing it creates new entries and consumes new enrollments.

## OpenShift

```bash
helm upgrade --install ... -f helm-charts/collectorctrl-operator/values-openshift.yaml
```

The overlay lets OpenShift assign the UID from the project's SCC range and keeps non-root, RuntimeDefault seccomp,
dropped capabilities and a read-only root filesystem. Validate it against `restricted-v2` in your cluster before
production use.

## Install without Helm

See [`deploy/README.md`](../deploy/README.md) for `deploy/deploy.sh` and the plain manifests. They mirror the chart
defaults.

## Credentials and recovery

On first connect each identity (the cluster connection and each observed collector) exchanges one use of the
enrollment token for its own credential. Credentials are stored on the volume mounted at
`/var/lib/collectorctrl/credentials` and reused after restarts and upgrades. Things to know:

- **Keep the volume.** The claim carries `helm.sh/resource-policy: keep`, so `helm uninstall` does not delete it.
  To reuse an existing claim, set `credentialStorage.existingClaim`.
- **One replica.** The volume has one writer; the Deployment uses the `Recreate` strategy and `replicaCount` must
  stay 1.
- **Lost the volume?** Stored credentials cannot be recreated. In CollectorCtrl, reset (or delete) the affected
  Kubernetes entries so they can enroll again, make sure the enrollment token has uses left, then restart the
  operator.
- **Rotating the enrollment token** affects only new identities. Update the Secret and restart the operator:

  ```bash
  kubectl -n collectorctrl-operator create secret generic collectorctrl-auth \
    --from-literal=enrollment-token=cce_NEW_TOKEN --dry-run=client -o yaml | kubectl apply -f -
  kubectl -n collectorctrl-operator rollout restart deploy/collectorctrl-operator
  ```

- **Revoking** an identity in CollectorCtrl disconnects it for good; the operator never falls back to enrollment for
  a revoked credential.

## Upgrading

```bash
helm upgrade collectorctrl-operator ./helm-charts/collectorctrl-operator -n collectorctrl-operator --reuse-values
```

Helm does not upgrade CRDs. When a release changes the CRD, apply it first:

```bash
kubectl apply -f helm-charts/collectorctrl-operator/crds/crd.yaml
```

### From 0.6

- The default namespace changed from `collectorctrl` to `collectorctrl-operator`. Upgrade in the namespace you already
  use: the credential volume lives there, and moving it means re-enrolling every identity.
- Existing `CollectorMonitor` resources keep working. The label is now the recommended way to opt in.
- Installs that used `authMode: legacy` keep working until each identity has its own credential. Switch to
  `authMode: enroll` with an enrollment Secret afterwards.
- RBAC gains read access to Services (for topology). `helm upgrade` applies it.
- Check [CHANGELOG.md](../CHANGELOG.md) for the full list.

## Uninstall

```bash
helm uninstall collectorctrl-operator -n collectorctrl-operator
# Optional, permanent: delete the credentials and the CRD (this also deletes every CollectorMonitor)
kubectl -n collectorctrl-operator delete pvc collectorctrl-operator-credentials
kubectl delete crd collectormonitors.collectorctrl.io
```

Uninstalling changes nothing in your collectors. Their entries in CollectorCtrl go offline and can be removed there.
