## Migrate from public operator to CockroachDB operator

This guide will walk you through migrating a crdb cluster managed via the public operator to the CockroachDB operator. We assume you've created a cluster using the public operator. The goals of this process are to migrate without affecting cluster availability, and to preserve existing disks so that we don't have to replica data into empty volumes. Note that this process scales down the statefulset by one node before adding each operator-managed pod, so cluster capacity will be reduced by one node at times.

Before starting migration, clear your kubectl cache to avoid stale CRD definitions:

```bash
rm -rf ~/.kube/cache
```

Pre-requisite: Install the public operator and create an operator-managed cluster:

```
kubectl apply -f https://raw.githubusercontent.com/cockroachdb/cockroach-operator/v2.18.3/install/crds.yaml
kubectl apply -f https://raw.githubusercontent.com/cockroachdb/cockroach-operator/v2.18.3/install/operator.yaml

kubectl apply -f https://raw.githubusercontent.com/cockroachdb/cockroach-operator/v2.18.3/examples/example.yaml
```

Build the migration helper, and add the ./bin directory to your PATH:

```
make bin/migration-helper
export PATH=$PATH:$(pwd)/bin
```

Set environment variables as per your setup:

```

# CRDBCLUSTER refers to your crdbcluster CR in public operator.
export CRDBCLUSTER=cockroachdb

# NAMESPACE refers to the namespace where crdbcluster CR is installed.
export NAMESPACE=default

# CLOUD_PROVIDER is the cloud vendor where k8s cluster is residing.
# Right now, we support all the major cloud providers (gcp,aws,azure)
export CLOUD_PROVIDER=gcp

# REGION corresponds to the cloud provider's identifier of this region.
# It must match the "topology.kubernetes.io/region" label on Kubernetes
# Nodes in this cluster.
export REGION=us-central1
```

Back up crdbcluster resource in case we need to revert:

```
mkdir -p backup
kubectl get crdbcluster -o yaml $CRDBCLUSTER > backup/crdbcluster-$CRDBCLUSTER.yaml
```

Next, we need to re-map and generate tls certs. The CockroachDB operator uses slightly different certs than the public operator and mounts them in configmaps and secrets with different names. Run the `migration-helper` utility with `migrate-certs` option to generate and upload certs to your cluster.

```
bin/migration-helper migrate-certs --statefulset-name $CRDBCLUSTER --namespace $NAMESPACE
```

Next, generate manifests for each crdbnode and the crdbcluster based on the state of the statefulset. We generate a manifest for each crdbnode because we want the crdb pods and their associated pvcs to use the same names as the original statefulset-managed pods and pvcs. This means that the new operator-managed pods will use the original pvcs, and won't have to replicate data into empty nodes.

```
mkdir -p manifests
migration-helper build-manifest operator --crdb-cluster $CRDBCLUSTER --namespace $NAMESPACE --cloud-provider $CLOUD_PROVIDER --cloud-region $REGION --output-dir ./manifests
```

The migration helper reads the public operator's `v1alpha1` CrdbCluster and StatefulSet before
generating these manifests. Do not delete the target `CrdbCluster` or the CRDs; the converted
`v1beta1` view is served from the same Kubernetes object during migration.

## Prepare for CockroachDB Operator installation

The manual migration can be run while the public operator continues managing other clusters.
In that case, keep the public operator and public operator CRD installed. Pause only the
target `CrdbCluster` before installing the CockroachDB Operator.

```
# Prevent the public operator from reconciling this cluster during migration.
kubectl label crdbcluster $CRDBCLUSTER crdb.io/skip-reconcile="true" -n $NAMESPACE --overwrite
```

Because the public operator remains installed in this coexistence mode, install the
CockroachDB Operator with migration enabled. The migration flag registers the conversion
webhook required to serve existing `v1alpha1` clusters through the `v1beta1` API. This does
not create a second object for each cluster. Kubernetes serves the same object through both
API versions, and conversion sets non-migrating clusters to `Mode=Disabled` so the
CockroachDB Operator ignores them. Use a distinct `appLabel` if both operators run in the
same namespace.

Patch the public operator webhooks to use `matchPolicy: Exact` before Helm applies the
`v1beta1` CrdbCluster. Without this, the public operator webhooks can intercept v1beta1
requests after Kubernetes converts them to v1alpha1.

```
kubectl patch validatingwebhookconfiguration cockroach-operator-validating-webhook-configuration \
  --type=json -p='[{"op":"add","path":"/webhooks/0/matchPolicy","value":"Exact"}]'
kubectl patch mutatingwebhookconfiguration cockroach-operator-mutating-webhook-configuration \
  --type=json -p='[{"op":"add","path":"/webhooks/0/matchPolicy","value":"Exact"}]'

helm upgrade --install crdb-operator ./cockroachdb-parent/charts/operator \
  --set migration.enabled=true \
  --set appLabel=cockroachdb-operator
kubectl rollout status deployment/cockroach-operator --timeout=60s
```

Do not delete the target `CrdbCluster` during manual migration. It stays in place and is
served as `v1beta1` through the conversion webhook. The migration helper has already
captured the required pod and StatefulSet state in `manifests/values.yaml` and
`manifests/crdbnode-*.yaml`. Helm later adopts and updates the converted `CrdbCluster` from
those generated values.

To migrate seamlessly from the statefulset to the CockroachDB operator, we'll scale down statefulset-managed pods and replace them with crdbnode objects, one by one. Then we'll create the crdbcluster that manages the crdbnodes. Because of this order of operations, we need to create some objects that the crdbcluster will eventually own:

```
kubectl create priorityclass crdb-critical --value 500000000
kubectl apply -f manifests/rbac.yaml
```

For each CRDB pod, gradually scale down the StatefulSet by reducing its replica count one at a time.
For example, in a three-node cluster, first scale the StatefulSet down to two replicas:
```
kubectl scale statefulset/$CRDBCLUSTER --replicas=2
```

Next, create the CRDB node corresponding to the pod that was scaled down:

```
kubectl apply -f manifests/crdbnode-2.yaml
```

> ⚠️ If you want to rollback follow [rollback section](#rollback-plan-in-case-of-migration-failure).

Wait until the new pod is ready. If it fails to become ready, check the CockroachDB operator logs for errors.

To ensure your CockroachDB node is fully ready before proceeding with the next replica migration, verify that there are no under-replicated ranges. You can check this using the `ranges_underreplicated` metric, which should be zero.

First, set up port forwarding to access the CockroachDB node's HTTP interface:
```
kubectl port-forward pod/cockroachdb-2 8080:8080
```
Note: CockroachDB's UI is running on 8080 port by default.

Now, you can verify the metric by running following command:
```
curl --insecure -s https://localhost:8080/_status/vars | grep "ranges_underreplicated{" | awk '
{print $2}'
```
The above command will emit the number of under-replicated ranges on the particular CockroachDB
node and it should be zero before proceeding to next crdb node.

Repeat this process for each CRDB node until the StatefulSet reaches zero replicas.

The public operator creates a pod disruption budget that conflicts with a pod disruption budget managed by the CockroachDB operator. Before applying the crdbcluster manifest, delete the existing pod disruption budget:

```
kubectl delete poddisruptionbudget $CRDBCLUSTER
```

Annotate existing objects so that they can be managed by the Helm chart:

```
kubectl annotate service $CRDBCLUSTER-public meta.helm.sh/release-name="$CRDBCLUSTER"
kubectl annotate service $CRDBCLUSTER-public meta.helm.sh/release-namespace="$NAMESPACE"
kubectl label service $CRDBCLUSTER-public app.kubernetes.io/managed-by=Helm --overwrite=true

kubectl annotate crdbcluster $CRDBCLUSTER meta.helm.sh/release-name="$CRDBCLUSTER" -n $NAMESPACE
kubectl annotate crdbcluster $CRDBCLUSTER meta.helm.sh/release-namespace="$NAMESPACE" -n $NAMESPACE
kubectl label crdbcluster $CRDBCLUSTER app.kubernetes.io/managed-by=Helm --overwrite=true -n $NAMESPACE

# Remove the old v1alpha1 last-applied-configuration to avoid merge conflicts.
kubectl annotate crdbcluster $CRDBCLUSTER kubectl.kubernetes.io/last-applied-configuration- -n $NAMESPACE
```

If Ingress is install, use the below command to migrate it as well if it was speceifed in CRDBCluster

```
kubectl annotate ingress ui-cockroachdb meta.helm.sh/release-name="$CRDBCLUSTER"
kubectl annotate ingress ui-cockroachdb  meta.helm.sh/release-namespace="$NAMESPACE"
kubectl label ingress ui-cockroachdb app.kubernetes.io/managed-by=Helm --overwrite=true
```

```
kubectl annotate ingress sql-cockroachdb meta.helm.sh/release-name="$CRDBCLUSTER"
kubectl annotate ingress sql-cockroachdb meta.helm.sh/release-namespace="$NAMESPACE"
kubectl label ingress sql-cockroachdb app.kubernetes.io/managed-by=Helm --overwrite=true
```


Finally, install the CrdbCluster through Helm:

```
helm upgrade --install $CRDBCLUSTER ./cockroachdb-parent/charts/cockroachdb -f manifests/values.yaml --force
```

The `--force` flag lets Helm take ownership of the existing converted `CrdbCluster` from the
public operator and apply the generated `values.yaml`, including `mode: MutableOnly`. The
database pods are already managed through `CrdbNode` resources at this point.

Once the migration is successful and the StatefulSet has zero replicas, delete the
StatefulSet object created by the public operator:
```
kubectl delete statefulset $CRDBCLUSTER
```

## Rollback Plan (in case of migration failure)

This rollback procedure is for failures before the final Helm adoption succeeds and before
the original StatefulSet is deleted. The public operator and public operator CRD must remain
installed. Do not delete `crdbclusters.crdb.cockroachlabs.com`, `crdbnodes.crdb.cockroachlabs.com`,
or `crdbtenants.crdb.cockroachlabs.com` during rollback because those CRDs and the conversion
webhook are cluster-scoped and may be required by other clusters.

If the migration to the CockroachDB operator fails during the stage where you are applying the generated crdbnode manifests, follow the steps below to safely restore the original state using the previously backed-up resources and preserved volumes. This assumes the StatefulSet and PVCs are not deleted.

1. Delete the applied crdbnode resources in the reverse order you created them and simultaneously scale the StatefulSet back up

Delete the individual crdbnode manifests in the reverse order of their creation (starting with the last one created, e.g., crdbnode-1.yaml) and scale the StatefulSet back to its original replica count (e.g., 2).

**Example**:

1. Lets say you applied two crdbnode yaml file (`crdbnode-2.yaml` & `crdbnode-1.yaml`)
2. Now you want to rollback due to any issue.
3. Delete the crdbnodes in reverse order.
4. First delete the `crdbnode-1.yaml`, scale the replica count to 2
5. Do the verification by checking the under replicated range to zero.
6. Then delete the `crdbnode-2.yaml` and scale replica count to 3 and so on.

```
kubectl delete -f manifests/crdbnode-1.yaml
kubectl scale statefulset $CRDBCLUSTER --replicas=2
```

**Verification Step**
To ensure your CockroachDB node is fully ready before proceeding with the next replica rollback, verify that there are no under-replicated ranges. You can check this using the `ranges_underreplicated` metric, which should be zero.

First, set up port forwarding to access the CockroachDB node's HTTP interface:
```
kubectl port-forward pod/cockroachdb-2 8080:8080
```
Note: CockroachDB's UI is running on 8080 port by default.

Now, you can verify the metric by running following command:
```
curl --insecure -s https://localhost:8080/_status/vars | grep "ranges_underreplicated{" | awk '
{print $2}'
```
The above command will emit the number of under-replicated ranges on the particular CockroachDB
node and it should be zero before proceeding to next crdb node.

Note: It might take some time for the `under-replicated` value to be zero.

Repeat the kubectl delete -f ... command for each crdbnode manifest you applied during migration.


2. Remove Helm adoption metadata if it was already applied

If you already annotated resources for Helm adoption, remove those annotations before handing
the cluster back to the public operator:

```
kubectl annotate service $CRDBCLUSTER-public meta.helm.sh/release-name- meta.helm.sh/release-namespace- -n $NAMESPACE
kubectl label service $CRDBCLUSTER-public app.kubernetes.io/managed-by- -n $NAMESPACE

kubectl annotate crdbcluster $CRDBCLUSTER meta.helm.sh/release-name- meta.helm.sh/release-namespace- -n $NAMESPACE
kubectl label crdbcluster $CRDBCLUSTER app.kubernetes.io/managed-by- -n $NAMESPACE
```

If ingress adoption annotations were applied, remove them from the ingress resources that
exist in your cluster:

```
kubectl annotate ingress ui-cockroachdb meta.helm.sh/release-name- meta.helm.sh/release-namespace- -n $NAMESPACE
kubectl label ingress ui-cockroachdb app.kubernetes.io/managed-by- -n $NAMESPACE
kubectl annotate ingress sql-cockroachdb meta.helm.sh/release-name- meta.helm.sh/release-namespace- -n $NAMESPACE
kubectl label ingress sql-cockroachdb app.kubernetes.io/managed-by- -n $NAMESPACE
```

3. Delete the PriorityClass and RBAC resources created for manual migration

```
kubectl delete priorityclass crdb-critical --ignore-not-found
kubectl delete -f manifests/rbac.yaml --ignore-not-found
```

4. Disable CockroachDB Operator reconciliation for the target cluster

If the Helm command was started and changed the converted v1beta1 CrdbCluster to
`MutableOnly`, set it back to `Disabled` before handing the cluster back to the public
operator:

```
kubectl patch crdbclusters.v1beta1.crdb.cockroachlabs.com $CRDBCLUSTER -n $NAMESPACE \
  --type merge -p '{"spec":{"mode":"Disabled"}}'
```

Do not run `helm uninstall $CRDBCLUSTER` as a rollback step. Once Helm has adopted the
CrdbCluster, uninstalling the release can delete the CrdbCluster object that the public
operator needs to resume control.

5. Resume public operator reconciliation for the target cluster

```
kubectl label crdbcluster $CRDBCLUSTER crdb.io/skip-reconcile- -n $NAMESPACE
```

After `skip-reconcile` is removed, the public operator should resume reconciliation and
recreate any public-operator resources that were removed during the attempted migration,
such as the public operator PDB.

6. Verify the target cluster is back under public operator control

```
kubectl rollout status statefulset/$CRDBCLUSTER -n $NAMESPACE
kubectl get pods -n $NAMESPACE
```

Keep the CockroachDB Operator installed with `migration.enabled=true` while any v1alpha1
clusters remain in the Kubernetes cluster. It provides the conversion webhook required for
the public operator and Kubernetes API server to read and write those objects. Only uninstall
it or disable migration mode after all v1alpha1 clusters have been migrated and
`status.storedVersions` has been patched as described in the controller migration guide.

If you need to restore the original CrdbCluster manifest for any reason, apply the backup
only after confirming it will not overwrite intentional changes made after the backup:

```
kubectl apply -f backup/crdbcluster-$CRDBCLUSTER.yaml
```
