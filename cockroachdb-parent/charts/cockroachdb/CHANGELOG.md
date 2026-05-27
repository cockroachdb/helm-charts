# CockroachDB Chart — CHANGELOG

## [26.2.0] — 2026-05-25
### Added
- Added `cockroachdb.crdbCluster.rbac.nodeReader.create` to let split-chart tenant installs
  skip the CockroachDB chart's node-reader ClusterRole/ClusterRoleBinding when the platform has
  already created equivalent RBAC through the operator chart. See
  [Split-Chart Node Reader RBAC](README.md#split-chart-node-reader-rbac) for setup details.
  **Important:** Existing releases must upgrade the operator chart first with matching
  `nodeReader.subjects` and verify the operator-owned ClusterRole/ClusterRoleBinding exists before
  upgrading the CockroachDB chart with `nodeReader.create=false`; otherwise Helm removes the
  CockroachDB chart-owned binding and CockroachDB pods lose node read permissions.

### Changed
- Updated the default CockroachDB image version from `v26.1.4` to `v26.2.0`.
- Removed the CockroachDB chart pre-upgrade validation hook and its ClusterRole/ClusterRoleBinding.
  The `hooks.kubectlImage.*` values (added in 26.1.3) are no longer used and can be removed from
  custom values files.
  **Notes:**
  - Preview users upgrading existing CockroachDB Operator deployments should verify that the
    `crdbclusters.crdb.cockroachlabs.com` CRD serves `v1beta1`, stores `v1beta1`, and has
    `status.storedVersions` set to `["v1beta1"]` by following the verification commands in
    [MIGRATION_v1alpha1_to_v1beta1.md](../../MIGRATION_v1alpha1_to_v1beta1.md).
  - Users adopting the chart after automated migration from the Public Operator or Helm
    StatefulSet flows should follow the controller migration guides in
    [docs/migration](../../../docs/migration) and verify that the migrated cluster is readable
    through the v1beta1 API before chart adoption:
    `kubectl get crdbclusters.v1beta1.crdb.cockroachlabs.com <cluster-name> -n <namespace>`.

## [26.1.4] — 2026-05-06
### Changed
- Updated the default CockroachDB image version from `v26.1.3` to `v26.1.4`.

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
