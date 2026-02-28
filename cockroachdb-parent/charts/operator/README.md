<!--- Generated file, DO NOT EDIT. Source: build/templates/cockroachdb-parent/charts/operator/README.md --->
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
- Update `cloudRegion` accordingly in [`operator/values.yaml`](/cockroachdb-parent/charts/operator/values.yaml). This value must be same as the current region provided under regions section at [`cockroachdb/values.yaml`](/cockroachdb-parent/charts/cockroachdb/values.yaml).

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

## Namespace Scoping

By default, the operator watches **all namespaces** cluster-wide (global mode). You can restrict it to specific namespaces using the `watchNamespaces` value.

> **Note:** The operator's own deployment namespace (set via `-n`) is independent of the namespaces it watches. For example, you can deploy the operator into `cockroach-operator-system` and have it watch only `prod-a,prod-b`.

```yaml
# values.yaml

# Global mode (default) — operator manages CockroachDB clusters in all namespaces.
watchNamespaces: ""

# Single namespace — operator only manages clusters in "prod".
watchNamespaces: "prod"

# Multiple namespaces — operator manages clusters in the listed namespaces.
watchNamespaces: "prod-a,prod-b,prod-c"
```

### Use cases

- **Side-by-side version testing**: Deploy operator v2.12 scoped to `staging` and operator v2.13 scoped to `prod` without conflicts.
- **Least-privilege deployments**: Reduce the blast radius by limiting which namespaces the operator can affect at the cache level.
- **Gradual rollouts**: Promote a new operator version namespace-by-namespace before making it global.

### Important constraints

- **No overlapping namespaces**: Do not configure multiple operators to watch the same namespace in production. Both operators reconcile the same clusters independently, which can cause unpredictable behavior — especially if the operators run different versions.
- **Webhooks remain global**: Admission webhooks validate CockroachDB resources across all namespaces regardless of `watchNamespaces`. Only reconciliation is scoped.
- **Shared CRDs and webhooks**: All operator deployments in the cluster share the same CRD and webhook definitions. Run the same operator version across all deployments to avoid schema conflicts.

### Migration: global → multiple scoped operators

If you have a global operator and want to split it into namespace-scoped deployments:

1. Deploy the new scoped operators (the global operator continues running during this period):
   ```bash
   helm install operator-prod ./cockroachdb-parent/charts/operator \
     --namespace cockroach-prod-operator --create-namespace \
     --set watchNamespaces="prod-a,prod-b"

   helm install operator-staging ./cockroachdb-parent/charts/operator \
     --namespace cockroach-staging-operator --create-namespace \
     --set watchNamespaces="staging-a,staging-b"
   ```

2. Verify the new operators are reconciling clusters correctly.

3. Uninstall the global operator:
   ```bash
   helm uninstall $CRDBOPERATOR -n $NAMESPACE
   ```

During the transition, both the global and scoped operators reconcile the same clusters. This is safe if they run the same operator version, but complete the migration quickly (minutes to hours, not days).
