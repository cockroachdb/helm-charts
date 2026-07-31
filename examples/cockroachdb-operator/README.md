# Helm values examples

The YAML files in this directory are values for the CockroachDB parent chart.
Pass them to Helm with `-f`; do not apply them directly with `kubectl`.

Directly applicable `CrdbCluster` manifests for non-Helm installations are in
[`cockroachdb-parent/charts/operator/manifests/examples/crdb`](../../cockroachdb-parent/charts/operator/manifests/examples/crdb).
