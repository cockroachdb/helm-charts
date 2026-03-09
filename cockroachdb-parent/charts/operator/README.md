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

By default the operator watches all namespaces cluster-wide. You can restrict it to specific
namespaces using `watchNamespaces`.

The operator's own namespace and the namespaces it watches are independent. You can install the
operator into `cockroach-operator-system` and have it watch only `prod-a,prod-b`.

```yaml
# Global mode (default) — watches all namespaces.
watchNamespaces: ""

# Single namespace.
watchNamespaces: "prod"

# Multiple namespaces.
watchNamespaces: "prod-a,prod-b,prod-c"
```

### Use cases

- Side-by-side version testing: deploy operator v2.12 scoped to `staging` and v2.13 scoped to `prod`.
- Reconciliation scoping: limits which namespaces the operator reconciles, not which resources it
  can access. The cluster role still grants cluster-wide permissions regardless of this setting.
- Gradual rollouts: promote a new version namespace-by-namespace before making it global.

### Constraints

- Do not configure multiple operators to watch the same namespace. Both will reconcile the same
  clusters with no coordination, and different versions will fight each other with unpredictable results.
  Overlapping is only safe briefly during migrations when both operators run the same version.
- Run the same operator version across all scoped deployments. CRDs and webhooks are cluster-scoped
  and shared. If two operators register different CRD schemas, the last writer wins.
- Admission webhooks are not yet scoped to `watchNamespaces`. Every `CrdbCluster` in the cluster is
  validated by whichever operator's webhook is registered, regardless of which namespaces that
  operator watches. Only reconciliation is scoped.

### Migration: global to multiple scoped operators

1. Deploy the scoped operators. The global operator keeps running and both will reconcile the same
   clusters during this window. Only do this when all operators run the same version.
   ```bash
   helm install operator-prod ./cockroachdb-parent/charts/operator \
     --namespace cockroach-prod-operator --create-namespace \
     --set watchNamespaces="prod-a,prod-b"

   helm install operator-staging ./cockroachdb-parent/charts/operator \
     --namespace cockroach-staging-operator --create-namespace \
     --set watchNamespaces="staging-a,staging-b"
   ```

2. Verify the new operators are reconciling clusters correctly.

3. Uninstall the global operator. Helm removes its cluster-scoped resources automatically.
   ```bash
   helm uninstall $CRDBOPERATOR -n $NAMESPACE
   ```

Complete the migration quickly. Minutes to hours, not days.

### Migration: scoped to global

1. Deploy a global operator with `watchNamespaces: ""`.
2. Verify it reconciles all namespaces.
3. Uninstall each scoped operator. Helm removes their cluster-scoped resources automatically.
   ```bash
   helm uninstall operator-prod -n cockroach-prod-operator
   helm uninstall operator-staging -n cockroach-staging-operator
   ```

## Upgrading from a previous chart version

### What changes after upgrading

Cluster-scoped resources are renamed with a `cockroachdb-` prefix. In scoped mode they also include
the release namespace as a suffix. `<namespace>` below is the Helm release namespace (`-n` value).

| Resource | Old name | Global mode | Scoped mode |
|---|---|---|---|
| PriorityClass | `cockroach-operator` | `cockroachdb-operator` | `cockroachdb-operator-<namespace>` |
| ClusterRole | `cockroach-operator-role` | `cockroachdb-operator-role` | `cockroachdb-operator-role-<namespace>` |
| ClusterRoleBinding | `cockroach-operator-default` | `cockroachdb-operator` | `cockroachdb-operator-<namespace>` |

The Deployment selector and pod labels are unchanged (`app: cockroach-operator`) when upgrading
without modifying `appLabel`, so a normal `helm upgrade` works without `--force` and without any
downtime. Changing `appLabel` requires `helm upgrade --force` because the deployment selector is
immutable, which causes a brief operator restart.

### Step 1 — Run helm upgrade

```bash
helm upgrade $CRDBOPERATOR ./cockroachdb-parent/charts/operator -n $NAMESPACE --reuse-values
```

### Step 2 — Remove stale cluster-scoped resources

The new chart creates differently-named resources, so the old ones are orphaned. Remove them once
the operator is healthy:

```bash
kubectl delete priorityclass cockroach-operator
kubectl delete clusterrole cockroach-operator-role
kubectl delete clusterrolebinding cockroach-operator-default
```

Switching between global and scoped modes via `helm upgrade` does not leave stale resources. Helm
automatically handles the rename as part of the upgrade.
