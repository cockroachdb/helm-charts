<!--- Generated file, DO NOT EDIT. Source: build/templates/cockroachdb-operator/README.md --->
# CockroachDB Operator charts

The `cockroachdb-operator/` directory contains the operator and CockroachDB charts.
Only these two individual charts are published.

Install them as separate releases (installing the umbrella chart directly is
unsupported, including for local testing). Install the operator first, then the
CockroachDB chart. Uninstall them in reverse order so the operator can finish
cleaning up the CockroachDB resources.

## Supported installation

Configure each chart using its installation guide:

- [Operator chart](charts/operator/README.md).
- [CockroachDB chart](charts/cockroachdb/README.md).
- [Self-signed certificates](../docs/certificate-management/self-signer.md#installation-of-cockroachdb-operator-with-self-signed-certificates)
  or [cert-manager](../docs/certificate-management/cert-manager.md).

The published charts and their local source directories are:

| Published chart | Local source |
| --- | --- |
| `cockroachdb-v2/cockroachdb-operator-chart` | `./cockroachdb-operator/charts/operator` |
| `cockroachdb-v2/cockroachdb-chart` | `./cockroachdb-operator/charts/cockroachdb` |

Create `operator-values.yaml` and `cockroachdb-values.yaml` using the guides above,
with matching regions and certificate settings. Add the repository and install
the operator with your operator values:

```shell
helm repo add cockroachdb-v2 https://charts.cockroachdb.com/v2 --force-update
helm repo update cockroachdb-v2
helm install crdb-operator cockroachdb-v2/cockroachdb-operator-chart \
  --namespace cockroach-ns --create-namespace \
  --values operator-values.yaml --wait --timeout 10m
```

Wait for the operator to become ready and install its CRDs and webhooks before
installing the CockroachDB chart:

```shell
kubectl -n cockroach-ns rollout status deployment/cockroach-operator --timeout=300s
kubectl wait --for=condition=Established \
  crd/crdbclusters.crdb.cockroachlabs.com \
  crd/crdbnodes.crdb.cockroachlabs.com --timeout=120s

helm install cockroachdb cockroachdb-v2/cockroachdb-chart \
  --namespace cockroach-ns --values cockroachdb-values.yaml --wait --timeout 10m
```

For local testing, use the source directories in the table above and install
the charts in the same order.

Do not pre-apply `charts/operator/manifests/crds/` as a workaround for Helm
installation. Those files are reference copies and may differ from the operator
image. The operator installs the CRDs it needs at startup.

## Uninstall

Uninstall the CockroachDB release first and wait for its `CrdbCluster` and
`CrdbNode` resources to be deleted while the operator is still running:

```shell
helm uninstall cockroachdb --namespace cockroach-ns --wait --timeout 10m
helm uninstall crdb-operator --namespace cockroach-ns --wait --timeout 10m
```

Removing the operator first can leave finalizers behind and keep the namespace
in `Terminating`. See [issue #694](https://github.com/cockroachdb/helm-charts/issues/694).

## CockroachDB cluster operations

The [CockroachDB chart README](charts/cockroachdb/README.md) has the steps for
[upgrading](charts/cockroachdb/README.md#upgrade-cockroachdb-cluster),
[scaling](charts/cockroachdb/README.md#scale-updown-cockroachdb-cluster),
[restarting](charts/cockroachdb/README.md#rolling-restart-of-cockroachdb-cluster)
and [connecting to a cluster](charts/cockroachdb/README.md#connecting-to-the-cockroachdb-cluster).

## Kubernetes compatibility

The operator requires Kubernetes 1.30 or later. We test the latest seven GA
Kubernetes versions in CI:

| Test suite | Kubernetes minors |
| --- | --- |
| Single region | 1.31, 1.32, 1.33, 1.34, 1.35, 1.36, 1.37 |
| Multi region | 1.31, 1.34, 1.37 |

When a new Kubernetes minor version is released, we add it and remove the oldest
one. Multi-region tests cover the oldest, middle and latest versions. See the
[Kubernetes compatibility workflow](../.github/workflows/k8s-compatibility-tests.yaml)
for the patch versions we use.

⚠️ **Note** → Older versions may still work, but are **not officially tested in CI**.
