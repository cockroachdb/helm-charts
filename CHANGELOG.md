# CHANGELOG

All notable changes to this project will be documented in this file.

## [operator-1.0.0] [cockroachdb-26.1.0] — GA Release
### Changed
- **GA release**: Removed `-preview` suffix from all chart versions. Charts are now production-ready.
- **Operator chart versioning**: The operator chart now uses its own semantic version (starting at 1.0.0),
  independent of CockroachDB versions. A single operator version supports multiple CockroachDB versions.
- **CockroachDB chart versioning**: Chart major.minor tracks the CockroachDB series (e.g., chart 26.1.x
  is for CockroachDB 26.1). The chart patch version increments independently. Check `appVersion` in
  Chart.yaml for the exact CockroachDB version bundled.
- **Operator image tags**: The operator image now uses semantic version tags (`v1.0.0`) instead of SHA
  digests. Users who need digest pinning can override with `tag: "v1.0.0@sha256:..."`.
- **Default image registry**: All images now default to DockerHub (`docker.io/cockroachdb`) instead of
  Google Artifact Registry.

### Upgrade Notes
- **Operator chart**: Version changes from `26.1.x-preview` to `1.0.0`. Use `--force` because Helm
  treats this as a version format change:
  `helm upgrade <release> cockroachdb/operator --version 1.0.0 --force`
- **CockroachDB chart**: Version changes from `26.1.x-preview` to `26.1.0`. Use `--force` for the
  same reason:
  `helm upgrade <release> cockroachdb/cockroachdb --version 26.1.0 --force`
- **Upgrade order**: Always upgrade the operator chart before the cockroachdb chart. The cockroachdb
  chart's pre-upgrade hook validates that the operator CRDs are in the expected state.

## [cockroachdb-parent-26.1.1-preview+2] 2026-03-26
### Changed
- **API Version Migration**: The operator now uses an image that removes `v1alpha1` entirely and keeps only `v1beta1`.
- Operator and CockroachDB pre-upgrade validation now require the previous fully migrated state before upgrading to this version. 
  This means `v1alpha1` must not be served, `v1beta1` must be served and stored, and CRD `storedVersions` must be `["v1beta1"]`.
- **See [MIGRATION_v1alpha1_to_v1beta1.md](cockroachdb-parent/MIGRATION_v1alpha1_to_v1beta1.md) for detailed instructions.**

## [cockroachdb-parent-26.1.1-preview+1] 2026-03-26
### Added
- Insecure cluster support. Set `cockroachdb.tls.enabled: false` and disable `selfSigner`, `certManager`, and `externalCertificates` to run without TLS. Intended for non-production use only.
- Namespace scoping for the operator via `watchNamespaces`. Set to a single namespace or a
  comma-separated list to restrict which namespaces the operator reconciles. Defaults to `""`
  (all namespaces). See [Namespace Scoping](cockroachdb-parent/charts/operator/README.md#namespace-scoping) for details.
- Configurable `appLabel` for the operator Deployment selector and pod labels. Defaults to
  `cockroach-operator` to preserve backward compatibility. Changing this on an existing installation
  requires `helm upgrade --force` since the Deployment selector is immutable.
- Added dedicated PVC support for CockroachDB log storage via `cockroachdb.crdbCluster.log.logsStore`.
- Added inline log configuration support via `cockroachdb.crdbCluster.log.config`, which renders a ConfigMap consumed by the CockroachDB operator. Use `cockroachdb.crdbCluster.loggingConfigMapName` to supply a custom ConfigMap name, and `cockroachdb.crdbCluster.loggingConfigVars` to expand environment variables within the log configuration.
- Added `selfSignedOperatorCerts` support in the operator chart, allowing the operator to self-generate its own webhook TLS certs.


### Changed
- Cluster-scoped resources now use a `cockroachdb-` prefix. In namespace-scoped mode they also include the
  release namespace as a suffix.

  | Resource | Old name | New name (cluster-scoped) | New name (namespace-scoped) |
  |---|---|---|---|
  | PriorityClass | `cockroach-operator` | `cockroachdb-operator` | `cockroachdb-operator-<ns>` |
  | ClusterRole | `cockroach-operator-role` | `cockroachdb-operator-role` | `cockroachdb-operator-role-<ns>` |
  | ClusterRoleBinding | `cockroach-operator-default` | `cockroachdb-operator` | `cockroachdb-operator-<ns>` |

  where `<ns>` is the Helm release namespace. After upgrading, remove the stale resources once the
  operator is healthy:
  ```bash
  kubectl delete priorityclass cockroach-operator
  kubectl delete clusterrole cockroach-operator-role
  kubectl delete clusterrolebinding cockroach-operator-default
  ```

### Notes
- Running more than one operator watching the same namespace should be avoided as both operators
  independently reconcile the same clusters, leading to unpredictable behavior.
- When transitioning from a cluster-scoped operator to namespace-scoped operators, the cluster-scoped operator
  continues reconciling all namespaces, including those watched by the namespace-scoped operators, until
  it is uninstalled. Complete the transition quickly and uninstall the cluster-scoped operator once the
  namespace-scoped operators are healthy.

## [cockroachdb-parent-26.1.0-preview+1] 2026-03-04
### Changed
- Upgraded CockroachDB to v26.1.0.
### Fixed
- Fixed a bug that prevented setting CockroachDB cluster settings when a custom sql port was used.

## [cockroachdb-parent-25.4.4-preview+1] 2026-02-18
### Changed
- Upgraded CockroachDB to v25.4.4.

### Fixed
- Fixed a bug that prevented setting CockroachDB cluster settings with the CockroachDB operator.
- Fixed a bug that caused repeated PodDisruptionBudget recreation on clusters with the default name `cockroachdb`.

## [cockroachdb-parent-25.4.3-preview+2] 2026-02-01
### Changed
- **API Version Migration**: v1alpha1 API serving is now disabled. Only v1beta1 is served.
  - **⚠️ CRITICAL**: CockroachDB charts MUST be upgraded to the previous version (25.4.3-preview+1) before upgrading to this version.
  - **📖 See [MIGRATION_v1alpha1_to_v1beta1.md](cockroachdb-parent/MIGRATION_v1alpha1_to_v1beta1.md) for detailed instructions.**
- Updated the Operator to disable v1alpha1 API serving.
### Added
- Pre-upgrade validation hook in the operator chart to prevent upgrade if CockroachDB charts still use v1alpha1 Helm manifests.
- Automatic detection of new vs. existing installations (new users can upgrade directly).
- Pre-upgrade validation in the CockroachDB chart to enforce operator-first upgrade for Phase 2.
### Fixed
- Scoped PodDisruptionBudget to a single CockroachDB cluster to prevent conflicts in multi-cluster deployments.

## [cockroachdb-parent-25.4.3-preview+1] 2026-01-19
### Changed
- **API Version Migration (v1alpha1 to v1beta1)**: The CockroachDB custom resources are migrating from `v1alpha1` to `v1beta1`. CockroachDB chart now uses `v1beta1` templates.
  - **IMPORTANT**: Operator MUST be upgraded before CockroachDB chart.
  - **See [MIGRATION_v1alpha1_to_v1beta1.md](cockroachdb-parent/MIGRATION_v1alpha1_to_v1beta1.md) for upgrade instructions.**
- Updated the Operator to support multiple CRD versions (v1alpha1, v1beta1) simultaneously.
### Added
- Pre-upgrade validation hook to ensure smooth upgrades owing to CR version updates and prevent upgrade order issues.
### Fixed
- Relaxed the K8s secret dependency during initial deployment on Azure.

## [cockroachdb-parent-25.4.2-preview+3] 2025-12-22
### Fixed
- Updated the Operator image to fix pkill command failures within the cert-reloader container.

## [cockroachdb-parent-25.4.0-preview] 2025-11-25
### Changed
- Updated the CockroachDB version to v25.4.0.

## [cockroachdb-parent-25.3.4-preview+1] 2025-11-25
### Added
- Added WAL failover custom path support in CockroachDB operator.
- Added virtual cluster support in CockroachDB operator.
- Added `--enable-k8s-node-controller` flag in CockroachDB operator to handle K8s node decommission feature.
### Breaking Changes
- Removed the following deprecated fields in favor of the corresponding podTemplate fields:
  - cockroachdb.crdbcluster.resources
  - cockroachdb.crdbcluster.podLabels
  - cockroachdb.crdbcluster.env
  - cockroachdb.crdbcluster.topologySpreadConstraints
  - cockroachdb.crdbcluster.podAnnotations
  - cockroachdb.crdbcluster.nodeSelector
  - cockroachdb.crdbcluster.affinity
  - cockroachdb.crdbcluster.tolerations

## [cockroachdb-parent-25.3.0-preview] - 2025-08-26
### Added
- `loggingConfigVars` field for supporting multiple environment configuration variables in the `loggingConfigMap`.

## [cockroachdb-parent-25.2.2-preview] - 2025-07-28
### Added
- `startFlags`, `podTemplate` fields for overriding CockroachDB start command and pod spec. 
- `localityMappings` field to allow granular mapping of Kubernetes node label to CockroachDB node locality.

### Changed
- Removed the deprecated `flags` field; use `startFlags` instead.
- Removed the `join` field; specify it using `startFlags`.

## [cockroachdb-17.0.0] - 2025-05-27
### Added
- release: advance app version to v25.2.0
