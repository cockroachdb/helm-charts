# Operator Helm Chart

This is a subchart for installing the CockroachDB operator.

## Prerequisites

* Kubernetes 1.30 or higher
* Helm 3.0 or higher
* Create a namespace to perform the operations against. In this case, we are using `cockroach-ns` namespace.
* If you want to secure your cluster to use TLS certificates for all network communications, [Helm must be installed with RBAC privileges](https://helm.sh/docs/topics/rbac/) or else you will get an "attempt to grant extra privileges" error.

Set the environment variables:

``` shell
export CRDBOPERATOR=crdb-operator
export NAMESPACE=cockroach-ns
```

## Notes

All the helm commands below reference the chart folder available locally after checking out this GitHub repository. Alternatively, you may also reference the charts in the Helm repository.
The operator chart does not exist in the Helm repository yet and will be added soon.

## Installation
- Update `cloudRegion` accordingly in [`operator/values.yaml`](/cockroachdb-parent/charts/operator/values.yaml). This value must be same as any one of the regions provided under regions section at [`cockroachdb/values.yaml`](/cockroachdb-parent/charts/cockroachdb/values.yaml).

```
  cloudRegion: us-central1
```

```shell
$ helm install $CRDBOPERATOR ./cockroachdb-parent/charts/operator -n $NAMESPACE
```

## Upgrade

Modify the required configuration in [`operator/values.yaml`](/cockroachdb-parent/charts/operator/values.yaml) and perform an upgrade through Helm:

```shell
$ helm install $CRDBOPERATOR ./cockroachdb-parent/charts/operator -n $NAMESPACE
```

## Uninstalling the Chart

To uninstall/delete the Operator cluster:

```bash
helm uninstall $CRDBOPERATOR -n $NAMESPACE
```
