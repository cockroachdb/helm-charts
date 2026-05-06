# CockroachDB Chart — CHANGELOG

## [26.1.3] — 2026-05-06
### Added
- Support for additional Subject Alternative Names (SANs) in self-signer node certificates via
  `cockroachdb.selfSigner.additionalSANs`. This enables SSL verification when connecting through
  load balancers or custom hostnames/IPs. Specify as a list of hostnames or IP addresses
  (e.g., `["my-loadbalancer.example.com", "10.20.30.40"]`).
  - **Note:** For existing clusters, the additional SANs take effect only after the next certificate
    rotation. New installations include the SANs immediately. To apply additional SANs to an existing
    cluster without waiting for automatic rotation, manually trigger certificate rotation or enable
    `tls.certs.selfSigner.rotateCerts: true` during upgrade.
### Changed
- **Per-chart versioning**: The CockroachDB chart's major.minor now tracks the CockroachDB database
  series (e.g., chart 26.1.x is for CockroachDB 26.1). The patch version increments independently.
  Check `appVersion` in Chart.yaml for the exact CockroachDB version bundled.
- **Chart name**: The chart name is `cockroachdb-chart` for published Helm and OCI artifacts.
  Rendered Kubernetes names remain based on `cockroachdb`.
- Hook images (`bitnami/kubectl`, `dtzar/helm-kubectl`) are now configurable via
  `hooks.kubectlImage.{registry,repository,tag,pullPolicy}` for air-gapped deployments.
- Self-signer image tag updated from `1.9` to `1.10` to include additional SANs support.

### Upgrade Notes
- Users must be on the latest preview version (`26.1.3-preview+1`) before upgrading.
- `helm upgrade <release> cockroachdb-v2/cockroachdb-chart --version 26.1.3`
- See [VERSIONING.md](../../docs/VERSIONING.md) for upstream Helm repository and OCI locations.

### Previous releases
For changes prior to per-chart versioning, see the [root CHANGELOG](../../../CHANGELOG.md).
