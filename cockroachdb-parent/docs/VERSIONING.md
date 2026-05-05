# Versioning and Upgrades

This document explains how CockroachDB Helm chart versions work and how to upgrade.

## Chart Versions

There are two charts, each with its own versioning scheme.

### Operator chart

The operator chart uses independent semantic versioning (e.g., 1.0.0, 1.1.0, 2.0.0). The version
is not tied to any CockroachDB version. A single operator version supports multiple CockroachDB
versions simultaneously.

| Bump | When |
|---|---|
| Patch (1.0.0 → 1.0.1) | Bug fix, operator image update with no CR changes |
| Minor (1.0.0 → 1.1.0) | New CRD field, new CockroachDB version support |
| Major (1.0.0 → 2.0.0) | Breaking CRD API change (field removal, storage migration) |

Every operator version bump (`make bump/operator/<version>`) updates both the chart version and
appVersion together. The operator image tag is derived from appVersion (`v<appVersion>`), so a
matching operator image must exist before publishing the chart.

For rare chart-only fixes (template or values changes without a new operator image), manually
edit `version` in `cockroachdb-parent/charts/operator/Chart.yaml` without changing `appVersion`,
then run `go run build/build.go generate`. Choose a version that will not collide with a future
operator image release.

#### Release candidates

The operator chart supports release candidate versions (e.g., `1.0.0-rc.1`, `1.0.0-rc.2`).
RC versions follow standard semver prerelease ordering:

```
1.0.0-rc.1  <  1.0.0-rc.2  <  1.0.0
```

RC charts are published and installable like any other version. Upgrading from RC to GA is a
normal Helm upgrade with no special flags required. The operator and cockroachdb charts are
independent, so the operator chart can be at an RC version while the cockroachdb chart is GA.

Every RC bump requires a matching operator image. There is no chart-only shortcut for RC versions.

### CockroachDB chart

The CockroachDB chart's major.minor matches the CockroachDB database series. The patch version
increments independently.

```
Chart 26.1.x  →  for CockroachDB 26.1
Chart 26.2.x  →  for CockroachDB 26.2
```

The chart patch number may differ from the CockroachDB patch number. For example, chart 26.1.3
might ship CockroachDB 26.1.1. Check `appVersion` in Chart.yaml for the exact CockroachDB
version bundled with the chart.

## Upgrade Order

Always upgrade the operator chart before the cockroachdb chart.

```bash
# Step 1: Upgrade operator
helm upgrade <operator-release> cockroachdb-v2/cockroachdb-operator --version <new-version>

# Step 2: Upgrade cockroachdb
helm upgrade <cockroachdb-release> cockroachdb-v2/cockroachdb --version <new-version>
```

The cockroachdb chart includes a pre-upgrade hook that validates the operator is in the expected
state before proceeding. If the operator is not upgraded first, the hook blocks the upgrade with
a clear error message.

## Common Upgrade Scenarios

### CockroachDB patch release (e.g., 26.1.1 → 26.1.2)

Only the cockroachdb chart changes. The operator chart stays the same.

```bash
helm upgrade <cockroachdb-release> cockroachdb-v2/cockroachdb --version <new-chart-version>
```

No operator upgrade needed. No downtime.

### Operator bug fix or image update

Only the operator chart changes. The cockroachdb chart stays the same.

```bash
helm upgrade <operator-release> cockroachdb-v2/cockroachdb-operator --version <new-version>
```

The operator rolls out a new deployment. CockroachDB pods are not affected.

### New CockroachDB series (e.g., 26.1 → 26.2)

A new chart line is created (26.2.x). The operator may or may not need an upgrade depending on
whether the new CockroachDB version requires operator changes.

```bash
# Check the chart's compatibility notes for minimum operator version
helm upgrade <operator-release> cockroachdb-v2/cockroachdb-operator --version <required-version>
helm upgrade <cockroachdb-release> cockroachdb-v2/cockroachdb --version 26.2.0
```

### Breaking operator change (major version bump)

Rare. Happens when CRD fields are removed or the API version changes (e.g., v1alpha1 → v1beta1).

1. Upgrade the operator chart to the new major version.
2. Upgrade the cockroachdb chart, which will have updated templates.
3. The cockroachdb chart's pre-upgrade hook enforces that the operator is upgraded first.

Follow the migration guide included in the release notes for the specific major version.

## Pre-Upgrade Validation

The cockroachdb chart includes a pre-upgrade hook that runs before every upgrade. The hook checks:

- The CockroachDB CRD (`crdbclusters.crdb.cockroachlabs.com`) exists in the cluster
- The CRD serves the expected API version (v1beta1)
- CRD storage migration is complete

If any check fails, the upgrade is blocked with a message explaining what to do.

## Overriding the CockroachDB Image Version

To run a different CockroachDB version than the chart default, override the image name:

```bash
helm upgrade <release> cockroachdb-v2/cockroachdb \
  --set cockroachdb.crdbCluster.image.name=cockroachdb/cockroach:v26.1.5
```

This is useful when a new CockroachDB patch is released before the chart is updated, or when
running an older CockroachDB version on the same chart line.

## Distribution

The v2 release publishes two independent charts:

| Chart | Package name | Purpose |
|---|---|---|
| `cockroachdb-operator` | `cockroachdb-operator-<version>.tgz` | Installs and upgrades the CockroachDB operator |
| `cockroachdb` | `cockroachdb-<version>.tgz` | Installs and upgrades CockroachDB clusters managed by the operator |

The parent chart is local-only and is not published.

### Helm repository

Production charts are available from:

```bash
helm repo add cockroachdb-v2 https://charts.cockroachdb.com/v2 --force-update
helm repo update cockroachdb-v2
helm search repo cockroachdb-v2 --devel

helm install crdb-operator cockroachdb-v2/cockroachdb-operator --version 1.0.0-rc.1
helm install crdb cockroachdb-v2/cockroachdb --version 26.1.3
```

`cockroachdb-v2` is a local Helm repository alias. You can choose a different alias, but the chart
references must use the same alias, for example `<alias>/cockroachdb-operator` and
`<alias>/cockroachdb`.

`--force-update` only updates the local Helm repo entry if it already exists. It does not force an
upgrade of any installed release.

ArtifactHub indexes the Helm repository for discovery. It does not store chart artifacts.

### OCI registries

Charts are also published as OCI artifacts.

Google Artifact Registry:

```bash
helm pull oci://us-docker.pkg.dev/releases-prod/self-hosted/charts/cockroachdb-operator --version 1.0.0-rc.1
helm pull oci://us-docker.pkg.dev/releases-prod/self-hosted/charts/cockroachdb --version 26.1.3
```

DockerHub:

```bash
helm pull oci://registry-1.docker.io/cockroachdb-charts/cockroachdb-operator --version 1.0.0-rc.1
helm pull oci://registry-1.docker.io/cockroachdb-charts/cockroachdb --version 26.1.3
```

The DockerHub repositories must exist before charts appear there unless DockerHub auto-create is
enabled for the org.
