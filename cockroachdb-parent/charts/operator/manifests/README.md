# Install the CockroachDB Operator without Helm

This directory contains a rendered Kubernetes bundle for customers that deploy
the CockroachDB Operator directly with `kubectl`, Argo CD, Flux, or another
GitOps system.

The public artifacts are:

- [`../api/v1beta1`](../api/v1beta1): Go API type reference.
- [`crds`](crds): `CrdbCluster` and `CrdbNode` CRDs.
- [`cockroachdb-operator.yaml`](cockroachdb-operator.yaml): operator RBAC,
  ServiceAccount, Service, and Deployment.
- [`examples/crdb`](examples/crdb): directly applicable `CrdbCluster` examples
  for insecure, secure, multi-region, CMEK, WAL failover, and Pod template
  configurations.

## Prerequisites

- Kubernetes 1.30 or later.
- Cluster-admin access to install CRDs and cluster-scoped RBAC.
- Access to the images listed in
  [`../../../images.txt`](../../../images.txt).

The checked-in bundle uses namespace `cockroachdb`, runs three operator replicas,
sets `CLOUD_REGION=local`, watches all namespaces, and uses operator-managed
self-signed webhook certificates. The operator persists one CA and serving
certificate in the shared `cockroach-operator-certs` Secret. Each replica
copies that same certificate material into its local `emptyDir` at startup.

## Install

Run these commands from the repository root:

```shell
kubectl create namespace cockroachdb

kubectl apply --server-side \
  -f cockroachdb-parent/charts/operator/manifests/crds

kubectl apply \
  -f cockroachdb-parent/charts/operator/manifests/cockroachdb-operator.yaml

kubectl -n cockroachdb rollout status deployment/cockroach-operator
```

The operator creates its validating and mutating webhook configurations at
runtime.

## Deploy a CockroachDB cluster

The direct-manifest examples are under [`examples/crdb`](examples/crdb).
Apply their shared RBAC first, then customize and apply one cluster example:

```shell
kubectl apply \
  -f cockroachdb-parent/charts/operator/manifests/examples/crdb/rbac.yaml

kubectl apply \
  -f cockroachdb-parent/charts/operator/manifests/examples/crdb/insecure.yaml
```

Read the comments and prerequisites in each example. In particular, provision
referenced certificate, CMEK, image pull, and other Secrets before creating the
`CrdbCluster`.

## Configure the bundle

Because this is rendered YAML, customize a copy with Kustomize, a GitOps
overlay, or an equivalent manifest-management tool:

Use the [`operator` chart README](../README.md) and the comments in
[`values.yaml`](../values.yaml) as the configuration reference. Non-Helm
installations do not read the values file directly; apply the equivalent
settings to the rendered Kubernetes resources. For example,
`watchNamespaces` maps to the Deployment's `WATCH_NAMESPACE` environment
variable.

The repository also contains
[`examples/cockroachdb-operator`](../../../../examples/cockroachdb-operator),
which are Helm values examples rather than directly applicable manifests.

- Change every namespaced resource if the operator should run outside the
  `cockroachdb` namespace.
- Set `WATCH_NAMESPACE` on the Deployment to a namespace or comma-separated
  list to restrict reconciliation. Do not run operators with overlapping watch
  scopes.
- Change `CLOUD_REGION`, replica count, and resource requests/limits as needed.
- Mirror and replace the operator image and the
  `RELATED_IMAGE_INIT_CONTAINER` and `RELATED_IMAGE_INOTIFYWAIT` values for
  air-gapped installations.
- Replace the self-signed certificate mode if stable, externally managed
  webhook certificates are required.

Cockroach Labs maintainers can compare the published artifacts with the
[CockroachDB Operator source README](https://github.com/cockroachlabs/cockroach-enterprise-operator#readme).

## Update and uninstall

Apply updated CRDs before updating the operator bundle:

```shell
kubectl apply --server-side \
  -f cockroachdb-parent/charts/operator/manifests/crds
kubectl apply \
  -f cockroachdb-parent/charts/operator/manifests/cockroachdb-operator.yaml
```

Do not delete the CRDs during an operator uninstall or rollback. Deleting a CRD
also deletes its custom resources.

The rendered operator bundle is generated from the chart:

```shell
make generate/operator-manifest
```
