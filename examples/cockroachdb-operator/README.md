# Helm values examples

The YAML files in this directory are values for the CockroachDB Operator umbrella chart.
Pass them to Helm with `-f`; do not apply them directly with `kubectl`.

Directly applicable `CrdbCluster` manifests for non-Helm installations are in
[`cockroachdb-operator/charts/operator/manifests/examples/crdb`](../../cockroachdb-operator/charts/operator/manifests/examples/crdb).
