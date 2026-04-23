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
helm upgrade <operator-release> cockroachdb/operator --version <new-version>

# Step 2: Upgrade cockroachdb
helm upgrade <cockroachdb-release> cockroachdb/cockroachdb --version <new-version>
```

The cockroachdb chart includes a pre-upgrade hook that validates the operator is in the expected
state before proceeding. If the operator is not upgraded first, the hook blocks the upgrade with
a clear error message.

## Common Upgrade Scenarios

### CockroachDB patch release (e.g., 26.1.1 → 26.1.2)

Only the cockroachdb chart changes. The operator chart stays the same.

```bash
helm upgrade <cockroachdb-release> cockroachdb/cockroachdb --version <new-chart-version>
```

No operator upgrade needed. No downtime.

### Operator bug fix or image update

Only the operator chart changes. The cockroachdb chart stays the same.

```bash
helm upgrade <operator-release> cockroachdb/operator --version <new-version>
```

The operator rolls out a new deployment. CockroachDB pods are not affected.

### New CockroachDB series (e.g., 26.1 → 26.2)

A new chart line is created (26.2.x). The operator may or may not need an upgrade depending on
whether the new CockroachDB version requires operator changes.

```bash
# Check the chart's compatibility notes for minimum operator version
helm upgrade <operator-release> cockroachdb/operator --version <required-version>
helm upgrade <cockroachdb-release> cockroachdb/cockroachdb --version 26.2.0
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

To run a different CockroachDB version than the chart default, override the image tag:

```bash
helm upgrade <release> cockroachdb/cockroachdb \
  --set cockroachdb.crdbCluster.cockroachDBVersion=v26.1.5
```

This is useful when a new CockroachDB patch is released before the chart is updated, or when
running an older CockroachDB version on the same chart line.
