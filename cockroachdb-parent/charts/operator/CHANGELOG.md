# CockroachDB Operator Chart — CHANGELOG

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
