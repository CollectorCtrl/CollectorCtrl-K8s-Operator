# Verifying releases

Every release from 0.8.0 on is published by GitHub Actions from a signed tag, with no manual steps. You can check
that what you run is what this repository built.

| Artifact | Where | How it is protected |
| --- | --- | --- |
| Container image | `ghcr.io/collectorctrl/collectorctrl-k8s-operator/operator:<version>` (linux/amd64, linux/arm64) | Sigstore cosign keyless signature; SPDX SBOM and SLSA provenance attestations |
| Helm chart | `oci://ghcr.io/collectorctrl/charts/collectorctrl-operator` | Sigstore cosign keyless signature |
| Raw manifests | `collectorctrl-operator-<version>-manifests.yaml` on the GitHub Release | `SHA256SUMS` on the same release |

Signatures are "keyless": they are bound to this repository's workflow identity through GitHub's OIDC token and
recorded in the public Rekor transparency log. There is no long-lived signing key to leak.

## Image signature

```sh
VERSION=0.8.0
cosign verify ghcr.io/collectorctrl/collectorctrl-k8s-operator/operator:$VERSION \
  --certificate-identity "https://github.com/CollectorCtrl/CollectorCtrl-K8s-Operator/.github/workflows/build-operator.yml@refs/tags/v$VERSION" \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

To enforce this at admission time, use a policy engine such as Sigstore policy-controller or Kyverno
`verifyImages` with the same identity and issuer.

## SBOM and provenance

The SBOM and the provenance are stored as attestations next to the image:

```sh
docker buildx imagetools inspect ghcr.io/collectorctrl/collectorctrl-k8s-operator/operator:$VERSION \
  --format '{{ json .SBOM }}'        # SPDX package list
docker buildx imagetools inspect ghcr.io/collectorctrl/collectorctrl-k8s-operator/operator:$VERSION \
  --format '{{ json .Provenance }}'  # source commit, workflow and build parameters
```

## Helm chart signature

```sh
CHART_VERSION=0.4.0
cosign verify ghcr.io/collectorctrl/charts/collectorctrl-operator:$CHART_VERSION \
  --certificate-identity "https://github.com/CollectorCtrl/CollectorCtrl-K8s-Operator/.github/workflows/release.yml@refs/tags/v$VERSION" \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

## Raw manifests

```sh
sha256sum --check --ignore-missing SHA256SUMS
```

## What the build guarantees

- Images are built from the tagged commit on GitHub-hosted runners; base images and every GitHub Action are pinned
  by digest.
- The image is pushed only after `make verify` (format, vet, race tests, licence headers, Helm lint),
  golangci-lint, govulncheck and the end-to-end tests on kind pass for that commit. CodeQL and OpenSSF Scorecard
  run on every change to `main`.
- The binary reports its version in the `starting manager` log line.
