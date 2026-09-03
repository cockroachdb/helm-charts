## Migrate from a Helm StatefulSet to the CockroachDB Operator

This guide migrates an existing CockroachDB cluster managed by the StatefulSet-based Helm
chart to the CockroachDB Operator. The process preserves the existing PVCs and replaces one
StatefulSet pod at a time with a `CrdbNode`. Cluster capacity is temporarily reduced by one
node during each replacement.

Before starting, confirm that all CockroachDB pods are Running and Ready and that no upgrade,
scale operation, or rolling restart is in progress. Keep the original chart version and values
available for rollback.

Build the migration helper and add the `bin` directory to your `PATH`:

```
make bin/migration-helper
export PATH=$PATH:$(pwd)/bin
```

Export environment variables for the current deployment:

```
# STS_NAME refers to the cockroachdb statefulset deployed via helm chart.
export STS_NAME="crdb-test-cockroachdb"

# NAMESPACE refers to the namespace where statefulset is installed.
export NAMESPACE="default"

# RELEASE_NAME refers to the release name of the installed Helm chart release.
export RELEASE_NAME=$(kubectl get sts $STS_NAME -n $NAMESPACE \
  -o jsonpath='{.metadata.annotations.meta\.helm\.sh/release-name}')

# ORIGINAL_CHART is the same chart source and version used by the current release.
export ORIGINAL_CHART="./cockroachdb"

# CLOUD_PROVIDER is the cloud vendor where the Kubernetes cluster runs.
# Supported values include gcp, aws, and azure.
export CLOUD_PROVIDER=gcp

# REGION corresponds to the cloud provider's identifier of this region.
# It must match the "topology.kubernetes.io/region" label on Kubernetes
# Nodes in this cluster.
export REGION=us-central1
```

Back up the release values and StatefulSet before making changes:

```bash
mkdir -p backup
helm get values $RELEASE_NAME -n $NAMESPACE -o yaml > backup/values.yaml
kubectl get statefulset $STS_NAME -n $NAMESPACE -o yaml > backup/statefulset-$STS_NAME.yaml
```

Next, migrate the TLS certificates. The CockroachDB Operator uses different certificate Secret
and ConfigMap names from the StatefulSet-based chart. The `migrate-certs` command generates and
uploads the required resources:

```
bin/migration-helper migrate-certs --statefulset-name $STS_NAME --namespace $NAMESPACE
```

Generate a manifest for each CrdbNode and the CrdbCluster from the current StatefulSet. The
generated resources retain the pod and PVC names, allowing operator-managed pods to reuse the
existing storage without replicating data into empty volumes:

```
mkdir -p manifests
bin/migration-helper build-manifest helm \
  --statefulset-name $STS_NAME \
  --namespace $NAMESPACE \
  --cloud-provider $CLOUD_PROVIDER \
  --cloud-region $REGION \
  --output-dir ./manifests
```

If the StatefulSet passes `--locality`, the generated manifests carry the same flag in
`spec.startFlags.upsert`, so each node keeps the locality it has today. The CockroachDB
Operator uses a supplied locality flag as is and does not compute one from Kubernetes node
labels. To switch to node labels after the migration, set
`cockroachdb.crdbCluster.localityMappings` and remove `--locality` from
`cockroachdb.crdbCluster.startFlags.upsert`. Changing locality restarts the pods, so plan it
as a separate change once the cluster is healthy.

Create the PriorityClass required by the generated resources before replacing any pods:

```
kubectl get priorityclass crdb-critical >/dev/null 2>&1 || \
  kubectl create priorityclass crdb-critical --value 500000000
```

If the PriorityClass already existed, reuse it and do not delete it during rollback.

Next, install the CockroachDB Operator. `cloudRegion` must match the region generated in
`manifests/values.yaml`, and `watchNamespaces` limits this operator to the migrated cluster's
namespace:

```
helm upgrade --install crdb-operator ./cockroachdb-parent/charts/operator \
  --namespace $NAMESPACE \
  --set watchNamespaces=$NAMESPACE \
  --set cloudRegion=$REGION \
  --wait
```

For each CockroachDB pod, scale the StatefulSet down by one replica. For example, first scale a
three-node StatefulSet down to two replicas:

```
kubectl scale statefulset/$STS_NAME --replicas=2 -n $NAMESPACE
```

Then create the crdbnode corresponding to the Statefulset pod you just scaled down:

```
kubectl apply -f manifests/crdbnode-2.yaml
```
> ⚠️ If you want to rollback follow [rollback section](#rollback-plan-in-case-of-migration-failure).

Wait for the new pod to become ready. If it doesn't, check the CockroachDB operator logs for errors.

Before replacing the next replica, verify that every `ranges_underreplicated` value is zero and
that the expected nodes are live. Ordinal zero remains a StatefulSet pod until the final
replacement, so it can be used for this check:

```bash
kubectl exec $STS_NAME-0 -n $NAMESPACE -c db -- \
  /cockroach/cockroach node status --ranges --format table \
  --certs-dir=/cockroach/cockroach-certs
```

For insecure clusters, replace `--certs-dir=/cockroach/cockroach-certs` with `--insecure`.
`cockroach node status --ranges` targets the system virtual cluster automatically. For the
direct SQL alternative and its `allow_unsafe_internals` and UA routing requirements, see
[Inspect cluster health manually](../../../cockroachdb-parent/charts/operator/README.md#inspect-cluster-health-manually).

Repeat this process for each crdb node until the statefulset has zero replicas.

After the final replacement, run the health check from a CrdbNode pod before deleting the
StatefulSet:

```bash
kubectl exec $STS_NAME-0 -n $NAMESPACE -c cockroachdb -- \
  /cockroach/cockroach node status --ranges --format table \
  --certs-dir=/cockroach/cockroach-certs
```

All nodes must be live and every `ranges_underreplicated` value must be zero. For insecure
clusters, use `--insecure` instead of `--certs-dir`.

The StatefulSet-based chart exposes SQL and gRPC connections differently from the CockroachDB
Operator. Apply the generated public Service so it uses the operator's gRPC port:

```
kubectl apply -f manifests/public-service.yaml
```

Delete the StatefulSet chart's PDB before applying the CrdbCluster because it conflicts with the
operator-managed PDB:

```
kubectl delete poddisruptionbudget $STS_NAME-budget -n $NAMESPACE --ignore-not-found
```

After confirming the final health check, delete the zero-replica StatefulSet. The chart blocks
the migration upgrade while this object still exists:

```
kubectl delete statefulset $STS_NAME -n $NAMESPACE --wait=true
```

Finally, apply the CrdbCluster manifest using helm upgrade to complete the migration:

```
helm upgrade $RELEASE_NAME ./cockroachdb-parent/charts/cockroachdb \
  --namespace $NAMESPACE \
  -f manifests/values.yaml \
  --force-conflicts
```

`--force-conflicts` performs the one-time ownership handoff for resources that were updated
during migration. Subsequent upgrades should not require it unless resources are modified
outside Helm.

The final step creates the `CrdbCluster` resource. The CockroachDB Operator immediately takes
over management of the existing database pods.

Verify the cluster mode is set correctly:

```bash
kubectl get crdbcluster $STS_NAME -n $NAMESPACE -o jsonpath='{.spec.mode}'
# Should output: MutableOnly
```

## Rollback Plan (in case of migration failure)

### ⚠️ Critical Warning: Point of No Return

This rollback procedure is valid only while the original StatefulSet still exists. After deleting
it for the final Helm upgrade, stop and contact support instead of following this rollback
procedure.

If migration fails while applying CrdbNode manifests, use the preserved StatefulSet and PVCs to
restore the original deployment.

1. Restore Service Connectivity and Ownership

Before scaling back the StatefulSet, ensure its Service selects the original Helm pods and has no
CockroachDB Operator owner reference.

```bash
# Remove ownerReferences from the Service to prevent accidental deletion
kubectl patch svc $STS_NAME -n $NAMESPACE --type=merge \
  -p='{"metadata":{"ownerReferences":[]}}'

# Update Service selectors to match original Helm pods
kubectl patch svc $STS_NAME -n $NAMESPACE --type='json' -p="[{\"op\": \"replace\", \"path\": \"/spec/selector\", \"value\": {\"app.kubernetes.io/component\": \"cockroachdb\", \"app.kubernetes.io/instance\": \"$RELEASE_NAME\", \"app.kubernetes.io/name\": \"cockroachdb\"}}]"
```

2. Delete the applied crdbnode resources and simultaneously scale the StatefulSet back up

Delete CrdbNode manifests in reverse creation order. After deleting each CrdbNode, scale the
StatefulSet up by one replica.

**Example**: 

1. If `crdbnode-2.yaml` and `crdbnode-1.yaml` were applied, delete `crdbnode-1.yaml` first.
2. Scale the StatefulSet to two replicas and wait for the restored pod to become Ready.
3. Verify that under-replicated ranges return to zero.
4. Delete `crdbnode-2.yaml`, scale the StatefulSet to three replicas, and repeat verification.

```
kubectl delete -f manifests/crdbnode-1.yaml
kubectl scale statefulset $STS_NAME --replicas=2 -n $NAMESPACE
```

**Verification step**

After the StatefulSet pod becomes Ready, verify that all ranges are replicated before restoring
the next ordinal:

```bash
kubectl exec $STS_NAME-0 -n $NAMESPACE -c db -- \
  /cockroach/cockroach node status --ranges --format table \
  --certs-dir=/cockroach/cockroach-certs
```

For insecure clusters, replace `--certs-dir=/cockroach/cockroach-certs` with `--insecure`.

Note: It might take some time for the `under-replicated` value to be zero.

Repeat the kubectl delete -f ... command for each crdbnode manifest you applied during migration.

3. Restore the original Helm-managed resources

After every CrdbNode has been removed and the StatefulSet is back at its original replica count,
run an upgrade with the original chart version and values. This restores the original Services,
PDB, and other Helm-managed resources.

```bash
# ORIGINAL_CHART must be the same chart source and version used before migration.
helm upgrade $RELEASE_NAME $ORIGINAL_CHART \
  --namespace $NAMESPACE \
  -f backup/values.yaml
```

4. Delete the PriorityClass created for the CockroachDB Operator

Skip this command if `crdb-critical` existed before migration.

```bash
kubectl delete priorityclass crdb-critical --ignore-not-found
```

5. Uninstall the CockroachDB Operator

```bash
helm uninstall crdb-operator --namespace $NAMESPACE
```

Do not manually delete the CockroachDB Operator CRDs or cluster-wide webhook resources. CRDs are
not removed by `helm uninstall` and may be shared by other CockroachDB clusters or operators.

6. Confirm that all CockroachDB pods are Running and Ready

```bash
kubectl get pods -n $NAMESPACE -l app.kubernetes.io/name=cockroachdb
```
