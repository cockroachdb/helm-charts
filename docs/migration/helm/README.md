## Migrate from Statefulset to CockroachDB operator

This guide will walk you through migrating a crdb cluster managed via Statefulset to the CockroachDB operator. We assume you've configured a Statefulset cluster using the helm chart. The goals of this process are to migrate without affecting cluster availability, and to preserve existing disks so that we don't have to replica data into empty volumes. Note that this process scales down the statefulset by one node before adding each operator-managed pod, so cluster capacity will be reduced by one node at times.

```
helm upgrade --install --set operator.enabled=false crdb-test --debug ./cockroachdb
```


Build the migration helper, and add the ./bin directory to your PATH:

```
make bin/migration-helper
export PATH=$PATH:$(pwd)/bin
```

First, export environment variables about the current deployment:

```
# STS_NAME refers to the cockroachdb statefulset deployed via helm chart.
export STS_NAME="crdb-test-cockroachdb"

# NAMESPACE refers to the namespace where statefulset is installed.
export NAMESPACE="default"

# RELEASE_NAME refers to the release name of the installed helm chart release.
export RELEASE_NAME=$(kubectl get sts $STS_NAME -n $NAMESPACE -o yaml | yq '.metadata.annotations."meta.helm.sh/release-name"')

# CLOUD_PROVIDER is the cloud vendor where k8s cluster is residing. 
# Right now, we support all the major cloud providers (gcp,aws,azure)
export CLOUD_PROVIDER=gcp

# REGION corresponds to the cloud provider's identifier of this region.
# It must match the "topology.kubernetes.io/region" label on Kubernetes 
# Nodes in this cluster.
export REGION=us-central1
```

Next, we need to re-map and generate tls certs. The CockroachDB operator uses slightly different certs than the cockroachdb helm chart and mounts them in configmaps and secrets with different names. Run the `migration-helper` utility with `migrate-certs` option to generate and upload certs to your cluster.

```
bin/migration-helper migrate-certs --statefulset-name $STS_NAME --namespace $NAMESPACE
```

Next, generate manifests for each crdbnode and the crdbcluster based on the state of the statefulset. We generate a manifest for each crdbnode because we want the crdb pods and their associated pvcs to have the same names as the original statefulset-managed pods and pvcs. This means that the new operator-managed pods will use the original pvcs, and won't have to replicate data into empty nodes.

```
mkdir -p manifests
bin/migration-helper build-manifest helm --statefulset-name $STS_NAME --namespace $NAMESPACE --cloud-provider $CLOUD_PROVIDER --cloud-region $REGION --output-dir ./manifests
```

To migrate seamlessly from the statefulset-based Helm chart to the CockroachDB operator, we'll scale down statefulset-managed pods and replace them with crdbnode objects, one by one. Then we'll create the crdbcluster that manages the crdbnodes. Because of this order of operations, we need to create some objects that the crdbcluster will eventually own:

```
kubectl create priorityclass crdb-critical --value 500000000
```

Next, install the CockroachDB operator:

```
helm upgrade --install crdb-operator ./cockroachdb-parent/charts/operator
```

For each crdb pod, scale the statefulset down by one replica. For example, for a three-node cluster, first scale the Statefulset down to two replicas:

```
kubectl scale statefulset/$STS_NAME --replicas=2
```

Then create the crdbnode corresponding to the Statefulset pod you just scaled down:

```
kubectl apply -f manifests/crdbnode-2.yaml
```
> ⚠️ If you want to rollback follow [rollback section](#rollback-plan-in-case-of-migration-failure).

Wait for the new pod to become ready. If it doesn't, check the CockroachDB operator logs for errors.

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

Repeat this process for each crdb node until the statefulset has zero replicas.

The official Helm chart creates a public Service that exposes both SQL and gRPC connections over a single port.
However, the CockroachDB operator uses a different port for gRPC communication.
To ensure compatibility, you’ll need to update the public Service to reflect the correct gRPC port used by the operator.

Apply the updated service manifest with:
```
kubectl apply -f manifests/public-service.yaml
```

The existing StatefulSet creates a PodDisruptionBudget (PDB) that conflicts with the one managed by the CockroachDB operator.
To avoid this conflict, delete the existing PDB before applying the CrdbCluster manifest:

```
kubectl delete poddisruptionbudget $STS_NAME-budget
```

Delete the StatefulSet that you previously scaled down to zero, as the Helm upgrade can proceed only if no StatefulSet is present.

```
kubectl delete statefulset $STS_NAME
```

Delete the headless Service created by the StatefulSet. The CockroachDB operator creates Services with different selectors, so the old Service must be removed:

```
kubectl delete svc $STS_NAME
```

Finally, apply the CrdbCluster manifest using helm upgrade to complete the migration:

```
helm upgrade $RELEASE_NAME ./cockroachdb-parent/charts/cockroachdb -f manifests/values.yaml
```

**Note**: The final step creates the `CrdbCluster` resource object. The CockroachDB operator will immediately take over management of the existing database pods.

Verify the cluster mode is set correctly:

```bash
kubectl get crdbcluster $RELEASE_NAME -o jsonpath='{.spec.mode}'
# Should output: MutableOnly
```

## Rollback Plan (in case of migration failure)

### ⚠️ Critical Warning: Point of No Return
This rollback procedure is **only valid** while the original StatefulSet still exists. Once you have successfully completed the migration and deleted the original StatefulSet (as described in the final steps of the migration guide), you **cannot** use this rollback procedure.

If the migration to the CockroachDB operator fails during the stage where you are applying the generated crdbnode manifests, follow the steps below to safely restore the original state using the previously backed-up resources and preserved volumes. This assumes the StatefulSet and PVCs are not deleted.

1. Restore Service Connectivity and Ownership

Before scaling back the StatefulSet, you must ensure the Service correctly points to the pods managed by the original Helm release and is no longer owned by the CockroachDB operator CRD.

```bash
# Remove ownerReferences from the Service to prevent accidental deletion
kubectl patch svc $STS_NAME -n $NAMESPACE --type='json' -p='[{"op": "remove", "path": "/metadata/ownerReferences"}]'

# Update Service selectors to match original Helm pods
kubectl patch svc $STS_NAME -n $NAMESPACE --type='json' -p="[{\"op\": \"replace\", \"path\": \"/spec/selector\", \"value\": {\"app.kubernetes.io/component\": \"cockroachdb\", \"app.kubernetes.io/instance\": \"$RELEASE_NAME\", \"app.kubernetes.io/name\": \"cockroachdb\"}}]"
```

2. Delete the applied crdbnode resources and simultaneously scale the StatefulSet back up

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
kubectl scale statefulset $STS_NAME --replicas=2
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

3. Delete the PriorityClass and RBAC Resources Created for the CockroachDB Operator

```bash
kubectl delete priorityclass crdb-critical
```

4. Uninstall the CockroachDB Operator

```bash
helm uninstall crdb-operator
```

5. Clean Up CockroachDB Operator Resources and CRDs

**Crucial Step to Prevent Data Loss:**
Before deleting the CRD, you must **remove the owner references** from the StatefulSet. This prevents Kubernetes from garbage-collecting (deleting) your database pods when the Custom Resource definition is removed.

```bash
# Remove ownerReferences from StatefulSet to prevent cascading deletion
kubectl patch statefulset $STS_NAME -n $NAMESPACE --type='json' -p='[{"op": "remove", "path": "/metadata/ownerReferences"}]'
```

Now it is safe to remove the CRD and other operator resources:

```bash
# Delete CRDs
kubectl delete crd crdbnodes.crdb.cockroachlabs.com
kubectl delete crd crdbtenants.crdb.cockroachlabs.com
kubectl delete crd crdbclusters.crdb.cockroachlabs.com

# Delete webhook service and configurations
kubectl delete service account cockroach-operator-default -n $NAMESPACE --ignore-not-found
kubectl delete service cockroach-webhook-service -n $NAMESPACE --ignore-not-found
kubectl delete validatingwebhookconfiguration cockroach-webhook-config --ignore-not-found
kubectl delete mutatingwebhookconfiguration cockroach-mutating-webhook-config --ignore-not-found

# Delete auxiliary Services created by CockroachDB operator
kubectl delete svc ${STS_NAME}-join -n $NAMESPACE --ignore-not-found

# Delete PDB created by CockroachDB operator
kubectl delete pdb ${STS_NAME}-pdb -n $NAMESPACE --ignore-not-found

# Clear kubectl cache to ensure fresh CRD definitions
rm -rf ~/.kube/cache
```

6. Confirm that all CockroachDB pods are running and Ready:

```bash
kubectl get pods -l app.kubernetes.io/name=cockroachdb
```
