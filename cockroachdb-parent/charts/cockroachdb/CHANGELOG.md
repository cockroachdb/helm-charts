# CockroachDB Chart — CHANGELOG

## [26.1.3] — Unreleased
### Changed
- **Per-chart versioning**: The CockroachDB chart's major.minor now tracks the CockroachDB database
  series (e.g., chart 26.1.x is for CockroachDB 26.1). The patch version increments independently.
  Check `appVersion` in Chart.yaml for the exact CockroachDB version bundled.
- Hook images (`bitnami/kubectl`, `dtzar/helm-kubectl`) are now configurable via
  `hooks.kubectlImage.{registry,repository,tag,pullPolicy}` for air-gapped deployments.

### Upgrade Notes
- Users must be on the latest preview version (`26.1.3-preview+1`) before upgrading.
- `helm upgrade <release> cockroachdb/cockroachdb --version 26.1.3`

### Previous releases
For changes prior to per-chart versioning, see the [root CHANGELOG](../../../CHANGELOG.md).
