# CHANGELOG

All notable changes to this project will be documented in this file.

## [cockroachdb-parent-25.3.3-preview+1] 2025-10-29
- Remove the following deprecated fields in favor of the corresponding podTemplate fields::
    - cockroachdb.crdbcluster.resources
    - cockroachdb.crdbcluster.podLabels
    - cockroachdb.crdbcluster.env
    - cockroachdb.crdbcluster.topologySpreadConstraints
    - cockroachdb.crdbcluster.podAnnotations
    - cockroachdb.crdbcluster.nodeSelector
    - cockroachdb.crdbcluster.affinity
    - cockroachdb.crdbcluster.tolerations
- Add WAL failover custom path support in CockroachDB operator.
- Add virtual cluster support in CockroachDB operator

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

