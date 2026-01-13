# CHANGELOG

All notable changes to this project will be documented in this file.

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

