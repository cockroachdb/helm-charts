# CockroachDB Chart — CHANGELOG

## [26.3.2] — 2026-09-03
### Fixed
- `cockroachdb.crdbCluster.godebug` now reaches the cockroachdb container. It was parsed but
  never rendered, so the `disablethp: "1"` default had no effect. Set `godebug: null` to opt out.

### Removed
- Removed `cockroachdb.crdbCluster.localityLabels` and `cockroachdb.crdbCluster.sideCars`.
  Use `cockroachdb.crdbCluster.localityMappings` and `cockroachdb.crdbCluster.podTemplate.spec`
  instead.
- The chart now fails with an upgrade message if any value the CrdbCluster template no longer
  applies is still set, so an upgrade cannot silently drop a sidecar or a scheduling constraint.

### Upgrade Notes
- Upgrading may roll CockroachDB pods to apply the default `GODEBUG` setting.
- `podTemplate` containers and init containers are appended after the operator's default ones,
  where `sideCars` used to go first. Init containers run in order, so an init container that has
  to run before the operator's `cockroachdb-init` needs rework before you move it.

## [26.3.1] — 2026-08-26
### Changed
- Updated the default CockroachDB image version from `v26.3.0` to `v26.3.1`.

## [26.3.0] — 2026-08-19
### Changed
- Updated the default CockroachDB image version from `v26.2.5` to `v26.3.0`.

## [26.2.4] — 2026-08-05
### Changed
- Updated the default CockroachDB image version from `v26.2.3` to `v26.2.5`.
- Updated the [Helm 4 Server-Side Apply documentation](README.md#helm-4-server-side-apply).

## [26.2.3] — 2026-07-31
### Changed
- Updated the default CockroachDB image version from `v26.2.2` to `v26.2.3`.
- Documented Helm 4 server-side apply field ownership conflicts and the supported
  client-side compatibility and ownership-transfer options.

## [26.2.2] — 2026-07-23
### Added
- Added `cockroachdb.crdbCluster.postInitSQL` for post-initialization SQL; see
  [Post-Init SQL](README.md#post-init-sql).

### Changed
- Updated the default CockroachDB image version from `v26.2.1` to `v26.2.2`.

## [26.2.1] — 2026-06-11
### Changed
- Updated the default CockroachDB image version from `v26.2.0` to `v26.2.1`.

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
