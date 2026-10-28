# Support

## Getting help

| Need | Where |
| --- | --- |
| A bug, or the operator behaving differently from the docs | [Open a bug report](https://github.com/CollectorCtrl/CollectorCtrl-K8s-Operator/issues/new?template=bug_report.yml) |
| A feature idea or a collector chart that is not detected well | [Open a feature request](https://github.com/CollectorCtrl/CollectorCtrl-K8s-Operator/issues/new?template=feature_request.yml) |
| A security problem | Privately, as described in [SECURITY.md](SECURITY.md). Never in a public issue |
| Commercial support, onboarding or a pilot | [connect@collectorctrl.com](mailto:connect@collectorctrl.com) |

Before opening an issue, check [docs/troubleshooting.md](docs/troubleshooting.md). Include the operator version
(the `starting manager` log line), the Kubernetes version and platform, the collector chart and version, and the
relevant log lines with tokens and hostnames removed.

## What we support

- **Versions:** the latest minor release receives fixes; the previous minor receives security fixes until the next
  minor ships (see [SECURITY.md](SECURITY.md#supported-versions)).
- **Kubernetes:** CI runs end-to-end tests on the Kubernetes versions listed in the
  [README](README.md#compatibility-and-limits). Other conformant distributions are expected to work.
- **Response times:** issues are triaged within 5 business days; security reports are acknowledged within 3.

## Releases

Releases follow [Semantic Versioning](https://semver.org/). Each release is a Git tag with a GitHub Release, a
multi-arch image signed with Sigstore cosign (with SBOM and provenance), and a signed OCI Helm chart. Changes are
listed in [CHANGELOG.md](CHANGELOG.md). While the version is below 1.0, minor releases may change behaviour; the
changelog calls out anything that needs action.
