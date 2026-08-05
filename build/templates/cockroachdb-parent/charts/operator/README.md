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

All the helm commands below reference the chart folder available locally after checking out this GitHub repository. See [VERSIONING.md](../../docs/VERSIONING.md) for published chart locations, chart versions, and upgrade order.

## Installation
- Update `cloudRegion` accordingly in [`operator/values.yaml`](/cockroachdb-parent/charts/operator/values.yaml). This value must be same as the current region provided under regions section at [`cockroachdb/values.yaml`](/cockroachdb-parent/charts/cockroachdb/values.yaml).

```
  cloudRegion: us-central1
```

```shell
$ helm install $CRDBOPERATOR ./cockroachdb-parent/charts/operator -n $NAMESPACE
```

## Install without Helm

Non-Helm users can install the operator from the checked-in Kubernetes manifests.
See [`manifests/README.md`](manifests/README.md) for the CRDs, rendered operator
bundle, direct `CrdbCluster` examples, configuration guidance, and `kubectl`
install order.

## Upgrade

Modify the required configuration in [`operator/values.yaml`](/cockroachdb-parent/charts/operator/values.yaml) and perform an upgrade through Helm:

```shell
$ helm upgrade $CRDBOPERATOR ./cockroachdb-parent/charts/operator -n $NAMESPACE
```

### Helm 4 server-side apply

Helm 4 uses server-side apply by default for releases first installed with Helm
4. Normal installs and upgrades do not require any additional flags when
`CrdbCluster` changes are made through chart values. Manually changing a
Helm-managed `CrdbCluster` with `kubectl` or another tool can give that tool
field ownership and cause a later Helm 4 upgrade to report an SSA conflict. See
the CockroachDB chart's
[Helm 4 server-side apply guidance](../cockroachdb/README.md#helm-4-server-side-apply)
before using `--force-conflicts` or changing the release's apply mode.

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
     --set watchNamespaces="prod-a\,prod-b"

   helm install operator-staging ./cockroachdb-parent/charts/operator \
     --namespace cockroach-staging-operator --create-namespace \
     --set watchNamespaces="staging-a\,staging-b"
   ```

2. Verify the new operators are reconciling clusters correctly.

3. Uninstall the global operator. Helm removes its cluster-scoped resources automatically.
   ```bash
   helm uninstall $CRDBOPERATOR -n $NAMESPACE
   ```

   The operator creates admission webhook configurations at runtime. Verify that no stale global
   webhook configurations remain after the global operator is removed. Stale webhooks can continue
   to route admission or conversion traffic to the old operator service.

   ```bash
   kubectl delete validatingwebhookconfiguration cockroach-webhook-config --ignore-not-found
   kubectl delete mutatingwebhookconfiguration cockroach-mutating-webhook-config --ignore-not-found
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

## Operator TLS Certificates (`selfSignedOperatorCerts`)

Controls who provisions the `cockroach-operator-certs` Secret used by the operator's webhook.

```yaml
# false (default): Helm provisions the Secret. Cert is stable and only changes on helm upgrade.
selfSignedOperatorCerts: false

# true: the operator provisions and owns the shared Secret.
# Each replica copies the same certificates into its Pod-local emptyDir.
selfSignedOperatorCerts: true
```

In operator-managed mode, certificates persist across Pod restarts and change
only when the shared Secret is replaced.

Switching this flag on an existing installation requires the `cockroach-operator-certs` Secret
to be deleted first. If it is not, the chart will fail with a clear error explaining the required steps.

## Split-Chart Node Reader RBAC

CockroachDB pod init containers read Kubernetes Node labels to derive locality, so the pod
ServiceAccount needs cluster-scoped node read permissions. In standalone installs, the
CockroachDB chart can create that RBAC itself.

In split-chart installs where tenants cannot create ClusterRoles or ClusterRoleBindings, the
platform team can pre-authorize the tenant CockroachDB ServiceAccounts through the operator chart:

```yaml
nodeReader:
  enabled: true
  subjects:
    - namespace: tenant-a
      serviceAccountName: crdb-cockroachdb
```

The listed ServiceAccount does not need to exist when the operator chart is installed. Kubernetes
applies the binding once the CockroachDB chart creates the ServiceAccount. The namespace and
ServiceAccount name must exactly match the CockroachDB chart values. If tenants override
`cockroachdb.crdbCluster.rbac.serviceAccount.name`, use that exact value here. For split-chart
installs, explicitly setting `serviceAccount.name` in the CockroachDB chart is recommended so the
platform-owned binding and tenant-owned ServiceAccount cannot drift.

If `nodeReader.name` is empty, the chart uses `cockroachdb-node-reader` in global mode. When
`watchNamespaces` is set, the default becomes `cockroachdb-node-reader-<operator-namespace>`
so multiple scoped operator releases do not try to own the same ClusterRole/ClusterRoleBinding.
Most installations should leave `nodeReader.name` empty. Set it explicitly only when the platform
requires a specific cluster-scoped resource name.

After this RBAC is installed by the platform, tenants can set
`cockroachdb.crdbCluster.rbac.nodeReader.create=false` in the CockroachDB chart.
Do not set `nodeReader.name` to the CockroachDB chart's generated node-reader name while
`cockroachdb.crdbCluster.rbac.nodeReader.create=true` because both Helm releases would try to own
the same ClusterRole/ClusterRoleBinding.

## Under-Replicated Ranges Check

Before advancing a rolling upgrade or scale-down, the operator checks cluster-wide replication
health so it does not take another node out of service while ranges are already under-replicated.

The check runs right before the operator starts a new pod eviction during a rolling upgrade and
before it starts a new node decommission during scale-down. It does not block a pod update or node
decommission that is already in flight.

If under-replicated ranges are present, or if the check cannot complete, the operator holds the next
disruptive step and retries later. Other reconciliation work can continue while scale-down is held.

The result is written to the `RangesUnderReplicated` condition on the `CrdbCluster`:

- `False` / `AllReplicated`: no under-replicated ranges were found.
- `True` / `UnderReplicated`: one or more ranges are under-replicated.
- `True` / `CheckError`: the check failed, so the operator fails closed.
- `False` / `CheckSkipped`: the check was bypassed by feature flag.

### Inspect cluster health manually

Use `cockroach node status --ranges` to inspect the same cluster-wide health
signals used by the operator:

```shell
kubectl exec <cockroachdb-pod> -n <namespace> -c cockroachdb -- \
  /cockroach/cockroach node status --ranges --format table --port 26257 \
  --certs-dir=/cockroach/cockroach-certs
```

The `ranges_underreplicated` column should be zero for every node, and the
`is_live` column identifies live nodes. This command handles access to the
system virtual cluster automatically, including on virtualized (UA) clusters.
For insecure clusters, replace `--certs-dir=/cockroach/cockroach-certs` with
`--insecure`.
See the [`cockroach node`
documentation](https://www.cockroachlabs.com/docs/stable/cockroach-node).

Prefer this command over querying `crdb_internal.kv_store_status` directly.
That table is an unsafe, system-only internal table. If Cockroach Labs Support
asks you to query it for diagnostics, CockroachDB 26.1 and later require
`allow_unsafe_internals` to be enabled in the same session. On a virtualized
(UA) cluster, explicitly target the system virtual cluster; otherwise the
connection can land on the default virtual cluster and the query will fail.

```shell
kubectl exec <cockroachdb-pod> -n <namespace> -c cockroachdb -- \
  /cockroach/cockroach sql --port 26257 --certs-dir=/cockroach/cockroach-certs \
  --database=cluster:system/defaultdb \
  --execute="SET allow_unsafe_internals = on; SELECT COALESCE(sum((metrics->>'ranges.underreplicated')::INT8), 0) FROM crdb_internal.kv_store_status;"
```

For CockroachDB versions older than 26.1, omit the `SET` statement because the
session variable may not exist. See the
[`crdb_internal` access guidance](https://www.cockroachlabs.com/docs/stable/crdb-internal).

Use the `skip-under-replicated-ranges-check` feature flag only as a recovery override when the
gate blocks the change needed to recover the cluster. Preserve any existing `spec.features`
entries when adding the override:

```yaml
spec:
  features:
    - skip-under-replicated-ranges-check
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
