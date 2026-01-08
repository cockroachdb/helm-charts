## Migrate from public operator to CockroachDB operator

This guide will walk you through migrating a crdb cluster managed via the public operator to the CockroachDB operator. We assume you've created a cluster using the public operator. The goals of this process are to migrate without affecting cluster availability, and to preserve existing disks so that we don't have to replica data into empty volumes. Note that this process scales down the statefulset by one node before adding each operator-managed pod, so cluster capacity will be reduced by one node at times.

### Important: Clear Kubernetes Cache

⚠️ **REQUIRED STEP**: Before starting migration, clear your kubectl cache to avoid stale CRD definitions:

```bash
rm -rf ~/.kube/cache
```

This is critical because:
- Kubernetes caches CRD schemas locally
- Stale cache can cause validation errors during migration
- Conversion webhook changes won't be recognized without clearing the cache

**When to clear cache:**
- Before starting migration (now)
- After installing/upgrading the CockroachDB operator
- If you see unexpected CRD validation errors

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
**NOTE**: If you are installing the CockroachDB operator in the same namespace as the public operator, you must modify the `cockroachdb-parent/charts/operator/templates/operator.yaml` file. Change the `app` label and selector in both the Deployment and Service definitions to a unique value (e.g., `app: cockroachdb-operator`) to prevent conflicts with the public operator.

Back up crdbcluster resource in case we need to revert:

```
mkdir -p backup
kubectl get crdbcluster -o yaml $CRDBCLUSTER > backup/crdbcluster-$CRDBCLUSTER.yaml
```

Next, we need to re-map and generate tls certs. The crdb CockroachDB operator uses slightly different certs than the public operator and mounts them in configmaps and secrets with different names. Run the `migration-helper` utility with `migrate-certs` option to generate and upload certs to your cluster.

```
bin/migration-helper migrate-certs --statefulset-name $CRDBCLUSTER --namespace $NAMESPACE
```

Next, generate manifests for each crdbnode and the crdbcluster based on the state of the statefulset. We generate a manifest for each crdbnode because we want the crdb pods and their associated pvcs to use the same names as the original statefulset-managed pods and pvcs. This means that the new operator-managed pods will use the original pvcs, and won't have to replicate data into empty nodes.

```
mkdir -p manifests
migration-helper build-manifest operator --crdb-cluster $CRDBCLUSTER --namespace $NAMESPACE --cloud-provider $CLOUD_PROVIDER --cloud-region $REGION --output-dir ./manifests
```

**NOTE:** Do not delete the public operator's `ClusterRole` or `ClusterRoleBinding` if the public operator manages other clusters in the same Kubernetes environment. The new operator will be configured to use a unique ClusterRole name to avoid conflicts.

Before installing the CockroachDB operator, annotate the CR with region and cloud provider labels so they are preserved during conversion:

```bash
kubectl annotate crdbcluster $CRDBCLUSTER crdb.cockroachlabs.com/cloudProvider=$CLOUD_PROVIDER crdb.cockroachlabs.com/regionCode=$REGION --overwrite
```
Install the CockroachDB operator and wait for it to become ready. We set a custom `clusterRole.name` to ensure it does not conflict with the existing public operator's RBAC resources.

```
helm upgrade --install crdb-operator ./cockroachdb-parent/charts/operator --set clusterRole.name=cockroachdb-operator-default
kubectl rollout status deployment/cockroach-operator --timeout=60s
```

> ⚠️ **Important**: The CockroachDB operator must be installed in a **different namespace** than the public operator (`cockroach-operator-system`). Both operators use the same Service selector (`app: cockroach-operator`), so installing them in the same namespace would cause Service routing conflicts. By default, the CockroachDB operator installs in the same namespace as your CockroachDB cluster, which is the recommended approach.

To migrate seamlessly from the Statefulset to the CockroachDB operator, we'll scale down Statefulset-managed pods and replace them with crdbnode objects, one by one. Then we'll create the crdbcluster that manages the crdbnodes. Because of this order of operations, we need to create some objects that the crdbcluster will eventually own:

```
kubectl apply -f manifests/rbac.yaml
```

**IMPORTANT**: Before starting the migration, add the `crdb.io/skip-reconcile` label to your CrdbCluster. This prevents the public operator from scaling up the StatefulSet during migration:

```
kubectl label crdbcluster $CRDBCLUSTER crdb.io/skip-reconcile=true
```

Label all the public operator's CRDB pods with the following label

```
kubectl label po <pod-name> crdb.cockroachlabs.com/cluster=$CRDBCLUSTER svc=cockroachdb
```

For each CRDB pod, gradually scale down the StatefulSet by reducing its replica count one at a time. 
For example, in a three-node cluster, first scale the StatefulSet down to two replicas and wait for it to complete:
```
kubectl scale statefulset/$CRDBCLUSTER --replicas=2
```

Next, create the CRDB node corresponding to the pod that was scaled down:

```
kubectl apply -f manifests/crdbnode-2.yaml
```

> ⚠️ If you want to rollback follow [rollback section](#rollback-plan-in-case-of-migration-failure).

Wait until the new pod is ready. If it fails to become ready, check the CockroachDB Operator logs for errors.

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

# Only for Helm users:

Annotate existing objects so that they can be managed by the Helm chart:

```
kubectl annotate service $CRDBCLUSTER-public meta.helm.sh/release-name="$CRDBCLUSTER"
kubectl annotate service $CRDBCLUSTER-public meta.helm.sh/release-namespace="$NAMESPACE"
kubectl label service $CRDBCLUSTER-public app.kubernetes.io/managed-by=Helm --overwrite=true
```

Annotate the CrdbCluster for helm adoption and clear the old last-applied-configuration:

```
kubectl annotate crdbcluster $CRDBCLUSTER meta.helm.sh/release-name="$CRDBCLUSTER"
kubectl annotate crdbcluster $CRDBCLUSTER meta.helm.sh/release-namespace="$NAMESPACE"
kubectl label crdbcluster $CRDBCLUSTER app.kubernetes.io/managed-by=Helm --overwrite=true

# Remove the old v1alpha1 last-applied-configuration to avoid helm merge conflicts
kubectl annotate crdbcluster $CRDBCLUSTER kubectl.kubernetes.io/last-applied-configuration-
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

The public operator creates a pod disruption budget that conflicts with a pod disruption budget managed by the CockroachDB operator. Before applying the crdbcluster manifest, delete public operator pod disruption budgets:

```
# Delete the PDB created by the public operator
kubectl delete poddisruptionbudget $CRDBCLUSTER
```

Finally, install the CrdbCluster via helm. Use the `--force` flag to handle field ownership transfer:

```bash
helm upgrade --install $CRDBCLUSTER ./cockroachdb-parent/charts/cockroachdb -f manifests/values.yaml --force
```

**Why `--force` is needed:**
The `--force` flag tells helm to recreate the CrdbCluster resource, which transfers field ownership from the public operator to helm. This allows helm to apply all values from `values.yaml` including `mode: MutableOnly`.

**Note**: The `--force` flag only recreates the CRD resource object, not the actual pods. Since:
- The CockroachDB operator starts in Disabled mode (from conversion webhook)
- We haven't deleted the StatefulSet yet
- The actual database pods continue running

This is safe and causes no disruption to the running database.

Verify the cluster was adopted correctly and mode is set to MutableOnly:

```bash
kubectl get crdbcluster $CRDBCLUSTER -n $NAMESPACE -o jsonpath='{.spec.mode}'
# Should output: MutableOnly
```


Once the migration is successful, delete the StatefulSet and headless Service created by the public operator:

```bash
kubectl delete statefulset $CRDBCLUSTER -n $NAMESPACE
kubectl delete svc $CRDBCLUSTER -n $NAMESPACE
```

**Note**: Deleting the headless Service (`cockroachdb`) is required because it was created by the public operator with `v1alpha1` ownerReferences. The CockroachDB operator will immediately recreate it with correct `v1beta1` ownership.

**Note**: The migration helper automatically sets the CrdbCluster mode to `MutableOnly`, so the CockroachDB operator will immediately take over full management after the helm install completes. You can verify the mode with:

```
kubectl get crdbcluster $CRDBCLUSTER -o jsonpath='{.spec.mode}'
```

## Post-Migration Cleanup

After successfully completing the migration and verifying your cluster is stable, you can clean up the public operator resources.

> ⚠️ **Important**: Only perform these cleanup steps once **all** CockroachDB clusters in your environment have been successfully migrated to the new CockroachDB operator. The public operator is required to manage any remaining `v1alpha1` clusters.

### Remove Public Operator Resources and Webhooks

The public operator resources should be removed after a successful migration.

```bash
# Delete the public operator deployment and related resources
kubectl delete service cockroach-operator-webhook-service -n cockroach-operator-system --ignore-not-found
kubectl delete deployment cockroach-operator-manager -n cockroach-operator-system --ignore-not-found
kubectl delete serviceaccount cockroach-operator-sa -n cockroach-operator-system --ignore-not-found

# Delete public operator webhook configurations
# These are cluster-scoped and won't be deleted with the namespace
# If left behind, they will cause errors when updating CrdbCluster resources
kubectl delete mutatingwebhookconfiguration cockroach-operator-mutating-webhook-configuration --ignore-not-found
kubectl delete validatingwebhookconfiguration cockroach-operator-validating-webhook-configuration --ignore-not-found
```

### Clean Up Local Backups (Optional)

Once you're confident the migration is stable, you can remove local backup files:

```bash
# Review files before deleting
ls -la backup/

# Remove backups after verification (be careful!)
# rm -rf backup/
```

## Rollback Plan (in case of migration failure)

### ⚠️ Critical Warning: Point of No Return
This rollback procedure is **only valid** while the original StatefulSet still exists. Once you have successfully completed the migration and deleted the original StatefulSet (as described in the final steps of the migration guide), you **cannot** use this rollback procedure.

### Important: CRD Version Management

⚠️ **CRITICAL**: The CockroachDB operator CRD includes both v1alpha1 and v1beta1 versions with v1beta1 as the storage version. During rollback, Kubernetes will prevent you from removing v1beta1 from the CRD until all data is migrated.

**Error you might see:**
```
The CustomResourceDefinition "crdbclusters.crdb.cockroachlabs.com" is invalid: 
status.storedVersions[1]: Invalid value: "v1beta1": missing from spec.versions; 
v1beta1 was previously a storage version, and must remain in spec.versions 
until a storage migration ensures no data remains persisted in v1beta1
```

If the migration to the CockroachDB operator fails during the stage where you are applying the generated crdbnode manifests, follow the steps below to safely restore the original state using the previously backed-up resources and preserved volumes. This assumes the StatefulSet and PVCs are not deleted.

1. Restore Service Connectivity and Ownership

Before scaling back the StatefulSet, you must ensure the Service correctly points to the pods managed by the public operator and is no longer owned by the CockroachDB operator CRD.

```bash
# Remove ownerReferences from the Service to prevent accidental deletion
kubectl patch svc $CRDBCLUSTER -n $NAMESPACE --type='json' -p='[{"op": "remove", "path": "/metadata/ownerReferences"}]'

# Update Service selectors to match Public Operator pods
kubectl patch svc $CRDBCLUSTER -n $NAMESPACE --type='json' -p="[{\"op\": \"replace\", \"path\": \"/spec/selector\", \"value\": {\"app.kubernetes.io/component\": \"database\", \"app.kubernetes.io/instance\": \"$CRDBCLUSTER\", \"app.kubernetes.io/name\": \"cockroachdb\"}}]"
```

2. Delete the applied crdbnode resources in the reverse order you created them and simultaneously scale the StatefulSet back up.

Also, remove the additional labels from the pods if they persist:

```bash
kubectl label pod <pod-name> crdb.cockroachlabs.com/cluster- svc-
```

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
node, and it should be zero before proceeding to the next crdb node.

Note: It might take some time for the `under-replicated` value to be zero.

Repeat the kubectl delete -f ... command for each crdbnode manifest you applied during migration.


3. Delete the PriorityClass and RBAC Resources Created for the CockroachDB Operator

```bash
kubectl delete priorityclass crdb-critical
kubectl delete -f manifests/rbac.yaml

# Delete the CockroachDB operator's custom ClusterRole and Binding
kubectl delete clusterrole cockroachdb-operator-default --ignore-not-found
kubectl delete clusterrolebinding cockroachdb-operator-default --ignore-not-found
```

4. Uninstall the CockroachDB Operator

```bash
helm uninstall crdb-operator
```

5. Clean Up CockroachDB Operator Resources and CRDs

**IMPORTANT**: You must remove the conversion webhook from the CRD to allow the public operator to manage `v1alpha1` resources without the (now uninstalled) CockroachDB operator.

**Crucial Step to Prevent Data Loss:**
Before modifying the CRD, you must **remove the owner references** from the StatefulSet. This prevents Kubernetes from garbage-collecting (deleting) your database pods if the CRD or Custom Resource is modified.

```bash
# Remove ownerReferences from StatefulSet to prevent cascading deletion
kubectl patch statefulset $CRDBCLUSTER -n $NAMESPACE --type='json' -p='[{"op": "remove", "path": "/metadata/ownerReferences"}]'
```

**Clean up Webhooks:**
Delete the webhook configurations and remove the conversion strategy from the CRD. Note that `v1beta1` will remain in the CRD's `status.storedVersions`, which is expected.

```bash
# 1. Delete webhook configurations
kubectl delete validatingwebhookconfiguration cockroach-webhook-config --ignore-not-found
kubectl delete mutatingwebhookconfiguration cockroach-mutating-webhook-config --ignore-not-found

# 2. Delete service account and services used by the webhook
kubectl delete serviceaccount cockroach-operator-default -n $NAMESPACE --ignore-not-found
kubectl delete service cockroach-webhook-service -n $NAMESPACE --ignore-not-found

# 3. Remove the conversion webhook strategy from the CRD
# This allows v1alpha1 objects to be managed without triggering conversion errors.
kubectl patch crd crdbclusters.crdb.cockroachlabs.com --type='json' -p='[{"op": "remove", "path": "/spec/conversion"}]'
```

**Clean up other resources:**

```bash
# Clean up other CockroachDB operator CRDs (safe to delete as they are specific to the new operator)
kubectl delete crd crdbnodes.crdb.cockroachlabs.com
kubectl delete crd crdbtenants.crdb.cockroachlabs.com

# Delete auxiliary Services created by CockroachDB operator
kubectl delete svc ${CRDBCLUSTER}-join -n $NAMESPACE --ignore-not-found

# Delete PDB created by CockroachDB operator
kubectl delete pdb ${CRDBCLUSTER}-pdb -n $NAMESPACE --ignore-not-found

# CRITICAL: Clear kubectl cache to ensure fresh CRD definitions
rm -rf ~/.kube/cache
```

**Why delete the Services?**
The CockroachDB operator creates Services with selectors like `crdb.cockroachlabs.com/cluster` and `svc`, while the public operator uses `app.kubernetes.io/*` selectors. Since we patched the main service in Step 1, this cleanup focuses on removing auxiliary services like the join service.

6. Restore the Original crdbcluster Custom Resource

```bash
kubectl apply -f backup/crdbcluster-$CRDBCLUSTER.yaml
```

7. Remove Helm Annotations (If Applied)

If you reached the "Only for Helm users" section and applied annotations, you must remove them so the public operator doesn't conflict with Helm management:

```bash
# Remove Helm annotations from Service
kubectl annotate service $CRDBCLUSTER-public meta.helm.sh/release-name- meta.helm.sh/release-namespace-
kubectl label service $CRDBCLUSTER-public app.kubernetes.io/managed-by-

# Remove Helm annotations from CrdbCluster
kubectl annotate crdbcluster $CRDBCLUSTER meta.helm.sh/release-name- meta.helm.sh/release-namespace-
kubectl label crdbcluster $CRDBCLUSTER app.kubernetes.io/managed-by-

# Remove Helm annotations from Ingress (if applicable)
kubectl annotate ingress ui-cockroachdb meta.helm.sh/release-name- meta.helm.sh/release-namespace- --ignore-not-found
kubectl label ingress ui-cockroachdb app.kubernetes.io/managed-by- --ignore-not-found

kubectl annotate ingress sql-cockroachdb meta.helm.sh/release-name- meta.helm.sh/release-namespace- --ignore-not-found
kubectl label ingress sql-cockroachdb app.kubernetes.io/managed-by- --ignore-not-found
```

Confirm that all CockroachDB pods are running and Ready:

```bash
kubectl get pods -l app.kubernetes.io/name=cockroachdb
```

**Note**: After rollback, if you cleared the cache and reinstalled the public operator CRD, you may need to restart your kubectl session or clear the cache again to ensure the correct CRD version is being used.