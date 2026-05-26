# CockroachDB Operator Chart — CHANGELOG

## [1.0.0-rc.2] — 2026-05-25
### Added
- Support for automated migration from Helm-managed StatefulSet clusters and Public Operator
  `v1alpha1` clusters to CockroachDB Operator `v1beta1` clusters. Use the operator chart's
  `migration.enabled=true` flag only for these migration flows and follow the controller
  migration guides in [docs/migration](../../../docs/migration). Preview customers upgrading
  existing CockroachDB Operator deployments do not need to enable this flag.
- Support for CMEK rotation in operator-managed CockroachDB clusters.
- Optional `nodeReader` RBAC support for split-chart installations. Platform teams can use the
  operator chart to pre-create the shared node-reader ClusterRole/ClusterRoleBinding for tenant
  CockroachDB pod ServiceAccounts, then tenants can disable node-reader RBAC creation in the
  CockroachDB chart.

### Changed
- Updated the default operator image to `docker.io/cockroachdb/cockroachdb-operator-v2:v1.0.0-rc.2`.

### Fixed
- Fixed version validation when version checker job pods are garbage-collected quickly.

### Upgrade Notes
- Recommended upgrade path: upgrade to `cockroachdb-operator-chart` `1.0.0-rc.1` first, verify
  that the `crdbclusters.crdb.cockroachlabs.com` CRD serves `v1beta1`, stores `v1beta1`, and has
  completed storage migration with `status.storedVersions` set to `["v1beta1"]`, then upgrade to
  `1.0.0-rc.2`.
  **Note:** Preview customers upgrading existing CockroachDB Operator deployments do not need to
  set `migration.enabled=true`. That flag is only for automated Helm StatefulSet and Public
  Operator migration flows. Migrated Public Operator clusters may continue using v1alpha1 until
  the CRD `storedVersions` is patched. Follow the controller migration guides in
  [docs/migration](../../../docs/migration) for when to patch `storedVersions` and disable
  migration mode.
- The operator chart no longer includes pre-upgrade validation hooks. Before upgrading, use the
  verification commands in [MIGRATION_v1alpha1_to_v1beta1.md](../../MIGRATION_v1alpha1_to_v1beta1.md)
  to confirm the cluster is already in the fully migrated `v1beta1` state.

## [1.0.0-rc.1] — 2026-05-06
### Changed
- **Per-chart versioning**: The operator chart now uses independent semantic versioning, starting
  at 1.0.0-rc.1. A single operator version supports multiple CockroachDB versions.
- **Chart name**: The chart name is `cockroachdb-operator-chart` for published Helm and OCI
  artifacts. Rendered Kubernetes names remain based on `cockroachdb-operator`.
- **Image tags**: The operator image now uses semantic version tags (`v1.0.0-rc.1`) instead of
  SHA digests.
- **Image registry**: The operator image now defaults to DockerHub (`docker.io/cockroachdb`)
  instead of Google Artifact Registry.

### Upgrade Notes
- Users must be on the latest preview version (`26.1.3-preview+1`) before upgrading.
- `helm upgrade <release> cockroachdb-v2/cockroachdb-operator-chart --version 1.0.0-rc.1`
- See [VERSIONING.md](../../docs/VERSIONING.md) for upstream Helm repository and OCI locations.

### Previous releases
For changes prior to per-chart versioning, see the [root CHANGELOG](../../../CHANGELOG.md).
