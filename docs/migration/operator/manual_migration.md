## Migrate from the Legacy Operator to the CockroachDB Operator

Use the [controller migration](controller_migration.md) unless your deployment requires the
manual path. Rehearse on a separate cluster with similar configuration first. Do not upgrade
either operator or change cluster configuration during migration. Complete or roll back first.

This guide migrates an existing CockroachDB cluster managed by the Legacy Operator to the
CockroachDB Operator. The process preserves the existing PVCs and replaces one StatefulSet pod
at a time with a `CrdbNode`. Cluster capacity is temporarily reduced by one node during each
replacement.

Before starting, confirm that all CockroachDB pods are Running and Ready and that no upgrade,
scale operation, or rolling restart is in progress. Use Legacy Operator v2.18.7 as the
recommended source version. Coexistence requires at least v2.18.6.
Keep the Legacy Operator running until every cluster it manages has been migrated.
For an OLM-managed source or a new operator installed through GitOps, review the
[installation prerequisites](controller_migration.md#step-5-install-cockroachdb-operator-with-migration-enabled)
before changing CRDs, webhooks or operator certificate ownership.

## Insecure clusters

Keep the source TLS mode throughout migration and adoption. For insecure clusters, the
exported values disable TLS and all certificate modes. If editing values yourself, set
`cockroachdb.tls.enabled`, `cockroachdb.tls.selfSigner.enabled`,
`cockroachdb.tls.certManager.enabled` and
`cockroachdb.tls.externalCertificates.enabled` to `false`. Disabling TLS alone fails
because the self-signer defaults to enabled.

Do not enable TLS on this running cluster later. Changing TLS mode is unsupported and
can leave the cluster unhealthy and unrecoverable. Check pod readiness and SQL through
the original public Service with `--insecure` after adoption or rollback.

## Certificates and renewal

Verify custom certificate DNS and IP names before and after running the migration helper.
The manual helper has separate certificate handling from the controller. Do not assume that
controller certificate-preservation behavior applies to this path.

The helper needs `get` and `list` access to Secrets in the cluster namespace when
checking for source Helm signer settings. Review the generated TLS values before chart adoption. The legacy operator's built-in
self-signed certificates stay externally managed. The exporter does not enable chart
self-signer rotation for them. Keep the existing CA, check node and client certificate
expiry, and arrange renewal before either expires. Preserve the required DNS/IP SANs.
Do not delete Secrets or the CA ConfigMap while pods still use them. See
[certificate management after adoption](controller_migration.md#check-certificate-management-after-adoption)
for how to check expiry and references. The controller's reissuance lifetime does not
apply to the manual helper.

The CA ConfigMap is copied once and isn't refreshed. Update it yourself after the CA is
renewed, before nodes pick up certificates signed by the new CA. Check that exported values
name the same node and client Secrets as the migrated CrdbCluster, especially with custom
`nodeTLSSecret` and `clientTLSSecret` names.
The helper preserves custom names. If only one is set, the other keeps its source Secret,
normally `<name>-node` or `<name>-root`. These certificates remain externally managed.

## Review exported start flags

Before chart adoption, review `cockroachdb.crdbCluster.startFlags.upsert` in the generated
values. Preserved flags, including `--join`, override operator defaults. You can change or
remove them after migration when they are no longer needed. Before removing a join override,
verify the region namespaces, DNS domains, join Service connectivity and certificate names.
Changes can trigger a rolling restart, so check readiness and SQL connectivity afterward.

## Prepare the migration helper

Build the migration helper, and add the ./bin directory to your PATH:

```
make bin/migration-helper
export PATH=$PATH:$(pwd)/bin
```

Set environment variables as per your setup:

```

# CRDBCLUSTER refers to your crdbcluster CR in legacy operator.
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

Back up the CrdbCluster and StatefulSet before making changes:

```
mkdir -p backup
kubectl get crdbcluster $CRDBCLUSTER -n $NAMESPACE -o yaml \
  > backup/crdbcluster-$CRDBCLUSTER.yaml
kubectl get statefulset $CRDBCLUSTER -n $NAMESPACE -o yaml \
  > backup/statefulset-$CRDBCLUSTER.yaml
```

Next, migrate the TLS certificates. The CockroachDB Operator uses different certificate Secret
and ConfigMap names from the Legacy Operator. The `migrate-certs` command generates and uploads
the required resources:

```
bin/migration-helper migrate-certs --statefulset-name $CRDBCLUSTER --namespace $NAMESPACE
```

Generate a manifest for each CrdbNode and the CrdbCluster from the current StatefulSet. The
generated resources retain the pod and PVC names, allowing operator-managed pods to reuse the
existing storage without replicating data into empty volumes:

```
mkdir -p manifests
bin/migration-helper build-manifest operator \
  --crdb-cluster $CRDBCLUSTER \
  --namespace $NAMESPACE \
  --cloud-provider $CLOUD_PROVIDER \
  --cloud-region $REGION \
  --output-dir ./manifests
```

The migration helper reads the legacy operator's `v1alpha1` CrdbCluster and StatefulSet before
generating these manifests. Do not delete the target `CrdbCluster` or the CRDs. The converted
`v1beta1` view is served from the same Kubernetes object during migration.

## Prepare for CockroachDB Operator installation

The manual migration can be run while the legacy operator continues managing other clusters.
In that case, keep the legacy operator and legacy operator CRD installed. Pause only the
target `CrdbCluster` before installing the CockroachDB Operator.

```
# Prevent the legacy operator from reconciling this cluster during migration.
kubectl label crdbcluster $CRDBCLUSTER crdb.io/skip-reconcile="true" -n $NAMESPACE --overwrite

# Preserve the source cluster's region and provider through v1alpha1/v1beta1 conversion.
kubectl annotate crdbcluster $CRDBCLUSTER -n $NAMESPACE --overwrite \
  crdb.cockroachlabs.com/cloudProvider=$CLOUD_PROVIDER \
  crdb.cockroachlabs.com/regionCode=$REGION
```

Because the legacy operator remains installed in this coexistence mode, install the
CockroachDB Operator with migration enabled so Kubernetes can serve both API versions during
the transition. This does not create a second object for each cluster. Existing clusters remain
managed by the Legacy Operator until they are explicitly migrated. Use a distinct `appLabel` if
both operators run in the same namespace.

Patch the legacy operator webhooks to use `matchPolicy: Exact` before Helm applies the
`v1beta1` CrdbCluster. Without this, the legacy operator webhooks can intercept v1beta1
requests after Kubernetes converts them to v1alpha1.

```
kubectl patch validatingwebhookconfiguration cockroach-operator-validating-webhook-configuration \
  --type=json -p='[{"op":"add","path":"/webhooks/0/matchPolicy","value":"Exact"}]'
kubectl patch mutatingwebhookconfiguration cockroach-operator-mutating-webhook-configuration \
  --type=json -p='[{"op":"add","path":"/webhooks/0/matchPolicy","value":"Exact"}]'

helm upgrade --install crdb-operator ./cockroachdb-operator/charts/operator \
  --namespace $NAMESPACE \
  --set migration.enabled=true \
  --set appLabel=cockroachdb-operator \
  --set watchNamespaces=$NAMESPACE \
  --set cloudRegion=$REGION \
  --wait
```

Do not delete the target `CrdbCluster` during manual migration. It stays in place and is
served through both API versions. The migration helper has already captured the required pod
and StatefulSet state in `manifests/values.yaml` and `manifests/crdbnode-*.yaml`. Helm later
adopts and updates this CrdbCluster from those generated values.

Create the PriorityClass and RBAC resources required by the generated manifests before replacing
any pods:

```
kubectl get priorityclass crdb-critical >/dev/null 2>&1 || \
  kubectl create priorityclass crdb-critical --value 500000000
kubectl apply -f manifests/rbac.yaml
```

If the PriorityClass already existed, reuse it and do not delete it during rollback.

Label the existing StatefulSet pods so the CockroachDB Operator Services and health checks can
select both StatefulSet and CrdbNode pods during migration:

```bash
kubectl label pods -n $NAMESPACE \
  -l app.kubernetes.io/instance=$CRDBCLUSTER \
  crdb.cockroachlabs.com/cluster=$CRDBCLUSTER \
  svc=cockroachdb \
  --overwrite
```

For each CRDB pod, gradually scale down the StatefulSet by reducing its replica count one at a time.
For example, in a three-node cluster, first scale the StatefulSet down to two replicas:
```
kubectl scale statefulset/$CRDBCLUSTER --replicas=2 -n $NAMESPACE
```

Next, create the CRDB node corresponding to the pod that was scaled down:

```
kubectl apply -f manifests/crdbnode-2.yaml
```

> ⚠️ If you want to rollback follow [rollback section](#rollback-plan-in-case-of-migration-failure).

Wait until the new pod is ready. If it fails to become ready, check the CockroachDB operator logs for errors.

Before replacing the next replica, verify that every `ranges_underreplicated` value is zero and
that the expected nodes are live. Ordinal zero remains a StatefulSet pod until the final
replacement, so it can be used for this check:

```bash
kubectl exec $CRDBCLUSTER-0 -n $NAMESPACE -c db -- \
  /cockroach/cockroach node status --ranges --format table \
  --certs-dir=/cockroach/cockroach-certs
```

For insecure clusters, replace `--certs-dir=/cockroach/cockroach-certs` with `--insecure`.
`cockroach node status --ranges` targets the system virtual cluster automatically. For the
direct SQL alternative and its `allow_unsafe_internals` and UA routing requirements, see
[Inspect cluster health manually](../../../cockroachdb-operator/charts/operator/README.md#inspect-cluster-health-manually).

Repeat this process for each CRDB node until the StatefulSet reaches zero replicas.

After the final replacement, run the health check from a CrdbNode pod before Helm adoption:

```bash
kubectl exec $CRDBCLUSTER-0 -n $NAMESPACE -c cockroachdb -- \
  /cockroach/cockroach node status --ranges --format table \
  --certs-dir=/cockroach/cockroach-certs
```

All nodes must be live and every `ranges_underreplicated` value must be zero. For insecure
clusters, use `--insecure` instead of `--certs-dir`.

Delete the Legacy Operator PDB before Helm adoption because it conflicts with the
CockroachDB Operator PDB:

```
kubectl delete poddisruptionbudget $CRDBCLUSTER -n $NAMESPACE --ignore-not-found
```

Annotate existing objects so that they can be managed by the Helm chart:

```
kubectl annotate service $CRDBCLUSTER-public -n $NAMESPACE \
  meta.helm.sh/release-name="$CRDBCLUSTER" \
  meta.helm.sh/release-namespace="$NAMESPACE" --overwrite
kubectl label service $CRDBCLUSTER-public -n $NAMESPACE \
  app.kubernetes.io/managed-by=Helm --overwrite

kubectl annotate crdbcluster $CRDBCLUSTER -n $NAMESPACE \
  meta.helm.sh/release-name="$CRDBCLUSTER" \
  meta.helm.sh/release-namespace="$NAMESPACE" --overwrite
kubectl label crdbcluster $CRDBCLUSTER -n $NAMESPACE \
  app.kubernetes.io/managed-by=Helm --overwrite

# Remove the old v1alpha1 last-applied-configuration to avoid merge conflicts.
kubectl annotate crdbcluster $CRDBCLUSTER kubectl.kubernetes.io/last-applied-configuration- -n $NAMESPACE

```

If the Legacy Operator CrdbCluster created Ingress resources, compare the rendered
hosts, paths, ingress class, annotations and TLS settings before transferring them to
Helm. These commands use the cluster name as the release name so the chart's
`ui-<release>` and `sql-<release>` names match. Otherwise, keep the existing Ingresses
under separate management. The operator does not manage Ingress resources. See
[Ingress adoption and backend ports](controller_migration.md#annotate-resources-for-helm-ownership).

```
for INGRESS in ui-$CRDBCLUSTER sql-$CRDBCLUSTER; do
  kubectl annotate ingress $INGRESS -n $NAMESPACE \
    meta.helm.sh/release-name="$CRDBCLUSTER" \
    meta.helm.sh/release-namespace="$NAMESPACE" --overwrite
  kubectl label ingress $INGRESS -n $NAMESPACE \
    app.kubernetes.io/managed-by=Helm --overwrite
done
```

Verify that the cluster is healthy. Reset field ownership while the migration StatefulSet still
exists, then delete the zero-replica StatefulSet. This is the final ownership handoff before Helm
takes over:

```bash
kubectl patch crdbclusters.v1beta1.crdb.cockroachlabs.com $CRDBCLUSTER \
  -n $NAMESPACE \
  --type=merge \
  -p '{"metadata":{"managedFields":[{}]}}'

kubectl delete statefulset $CRDBCLUSTER -n $NAMESPACE --wait=true
```

Resetting field ownership allows Helm 4 to take ownership cleanly after the migration StatefulSet
is deleted. This is a one-time handoff before Helm adopts the CrdbCluster.

After the StatefulSet is gone, adopt the CrdbCluster through Helm:

```bash
helm upgrade --install $CRDBCLUSTER ./cockroachdb-operator/charts/cockroachdb \
  --namespace $NAMESPACE \
  -f manifests/values.yaml \
  --force-conflicts
```

`--force-conflicts` performs the one-time ownership handoff for resources previously managed by
the Legacy Operator. Helm applies the generated `values.yaml`, including `mode: MutableOnly`.

Verify that Helm set `mode: MutableOnly`:

```bash
kubectl get crdbclusters.v1beta1.crdb.cockroachlabs.com $CRDBCLUSTER -n $NAMESPACE \
  -o jsonpath='{.spec.mode}'
# Expected: MutableOnly
```

Subsequent upgrades should use the normal command without `--force-conflicts`:

```bash
helm upgrade $CRDBCLUSTER ./cockroachdb-operator/charts/cockroachdb \
  --namespace $NAMESPACE \
  -f manifests/values.yaml
```

Keep `crdb.io/skip-reconcile=true` on the migrated cluster while the Legacy Operator remains
installed. Removing it early allows the Legacy Operator to recreate the StatefulSet.

## Complete coexistence cleanup after the last cluster is migrated

You can migrate and adopt clusters one at a time. Perform this cluster-wide cleanup only after
every cluster managed by the Legacy Operator has been migrated and is healthy.

1. Verify that no Legacy Operator cluster remains in `Mode=Disabled`:

   ```bash
   kubectl get crdbclusters.v1beta1.crdb.cockroachlabs.com --all-namespaces \
     -o custom-columns='NAMESPACE:.metadata.namespace,NAME:.metadata.name,MODE:.spec.mode'
   ```

2. Uninstall the Legacy Operator using the same method used to install it. Remove its Deployment,
   webhook Service, ServiceAccount, ClusterRole, ClusterRoleBinding, and validating and mutating
   webhook configurations. Do not delete the shared CrdbCluster or CrdbNode CRDs. The exact
   v2.18.7 resource names are listed in
   [Uninstall the Legacy Operator and clean up coexistence](controller_migration.md#step-12-uninstall-the-legacy-operator-and-clean-up-coexistence).

3. After the Legacy Operator and its webhooks are gone, remove coexistence metadata from each
   migrated cluster:

   ```bash
   kubectl label crdbclusters.v1beta1.crdb.cockroachlabs.com $CRDBCLUSTER \
     crdb.io/skip-reconcile- -n $NAMESPACE
   kubectl annotate crdbclusters.v1beta1.crdb.cockroachlabs.com $CRDBCLUSTER \
     crdb.cockroachlabs.com/cloudProvider- \
     crdb.cockroachlabs.com/regionCode- \
     -n $NAMESPACE
   ```

4. Remove `v1alpha1` from the CRD's stored versions, then verify the result:

   ```bash
   kubectl patch crd crdbclusters.crdb.cockroachlabs.com \
     --subresource=status \
     --type=json \
     -p='[{"op":"replace","path":"/status/storedVersions","value":["v1beta1"]}]'
   kubectl get crd crdbclusters.crdb.cockroachlabs.com \
     -o jsonpath='{.status.storedVersions}'
   # Expected: ["v1beta1"]
   ```

5. Disable migration mode only after `storedVersions` contains only `v1beta1`:

   ```bash
   helm upgrade crdb-operator ./cockroachdb-operator/charts/operator \
     --namespace $NAMESPACE \
     --reuse-values \
     --set migration.enabled=false
   ```

## Rollback Plan (in case of migration failure)

This rollback procedure is valid only while the original StatefulSet still exists. After the
StatefulSet is deleted, stop and contact support instead of following this rollback procedure.
The Legacy Operator and the shared CRDs must remain installed throughout rollback because other
clusters and the conversion webhook may still depend on them.

If migration fails while applying CrdbNode manifests, use the preserved StatefulSet and PVCs to
restore the original deployment.

1. Delete CrdbNodes in reverse order and restore StatefulSet replicas

Delete CrdbNode manifests in reverse creation order. After deleting each CrdbNode, scale the
StatefulSet up by one replica.

**Example**:

1. If `crdbnode-2.yaml` and `crdbnode-1.yaml` were applied, delete `crdbnode-1.yaml` first.
2. Scale the StatefulSet to two replicas and wait for the restored pod to become Ready.
3. Verify that under-replicated ranges return to zero.
4. Delete `crdbnode-2.yaml`, scale the StatefulSet to three replicas, and repeat verification.

```
kubectl delete -f manifests/crdbnode-1.yaml
kubectl scale statefulset $CRDBCLUSTER --replicas=2 -n $NAMESPACE
```

**Verification step**

After the StatefulSet pod becomes Ready, verify that all ranges are replicated before restoring
the next ordinal:

```bash
kubectl exec $CRDBCLUSTER-0 -n $NAMESPACE -c db -- \
  /cockroach/cockroach node status --ranges --format table \
  --certs-dir=/cockroach/cockroach-certs
```

For insecure clusters, replace `--certs-dir=/cockroach/cockroach-certs` with `--insecure`.

Note: It might take some time for the `under-replicated` value to be zero.

Repeat the kubectl delete -f ... Command for each crdbnode manifest you applied during migration.


2. Remove Helm adoption metadata if it was already applied

If you already annotated resources for Helm adoption, remove those annotations before handing
the cluster back to the legacy operator:

```
kubectl annotate service $CRDBCLUSTER-public meta.helm.sh/release-name- meta.helm.sh/release-namespace- -n $NAMESPACE
kubectl label service $CRDBCLUSTER-public app.kubernetes.io/managed-by- -n $NAMESPACE

kubectl annotate crdbcluster $CRDBCLUSTER meta.helm.sh/release-name- meta.helm.sh/release-namespace- -n $NAMESPACE
kubectl label crdbcluster $CRDBCLUSTER app.kubernetes.io/managed-by- -n $NAMESPACE
```

If ingress adoption annotations were applied, remove them from the ingress resources that
exist in your cluster:

```
for INGRESS in ui-$CRDBCLUSTER sql-$CRDBCLUSTER; do
  kubectl annotate ingress $INGRESS -n $NAMESPACE \
    meta.helm.sh/release-name- meta.helm.sh/release-namespace-
  kubectl label ingress $INGRESS -n $NAMESPACE app.kubernetes.io/managed-by-
done
```

3. Delete the PriorityClass and RBAC resources created for manual migration

```
# Skip the PriorityClass command if crdb-critical existed before migration.
kubectl delete priorityclass crdb-critical --ignore-not-found
kubectl delete -f manifests/rbac.yaml --ignore-not-found
```

4. Disable CockroachDB Operator reconciliation for the target cluster

If the Helm command was started and changed the converted v1beta1 CrdbCluster to
`MutableOnly`, set it back to `Disabled` before handing the cluster back to the legacy
operator:

```
kubectl patch crdbclusters.v1beta1.crdb.cockroachlabs.com $CRDBCLUSTER -n $NAMESPACE \
  --type merge -p '{"spec":{"mode":"Disabled"}}'
```

Do not run `helm uninstall $CRDBCLUSTER` as a rollback step. Once Helm has adopted the
CrdbCluster, uninstalling the release can delete the CrdbCluster object that the legacy
operator needs to resume control.

5. Resume legacy operator reconciliation for the target cluster

Remove migration-only labels from restored StatefulSet pods, then remove `skip-reconcile` so the
Legacy Operator can resume:

```
kubectl label pods -n $NAMESPACE \
  -l crdb.cockroachlabs.com/cluster=$CRDBCLUSTER \
  crdb.cockroachlabs.com/cluster- svc-
kubectl label crdbcluster $CRDBCLUSTER crdb.io/skip-reconcile- -n $NAMESPACE
```

After `skip-reconcile` is removed, the legacy operator should resume reconciliation and
recreate any legacy-operator resources that were removed during the attempted migration,
such as the legacy operator PDB.

6. Verify the target cluster is back under legacy operator control

```
kubectl rollout status statefulset/$CRDBCLUSTER -n $NAMESPACE
kubectl get pods -n $NAMESPACE
```

Keep the CockroachDB Operator installed with `migration.enabled=true` while any v1alpha1
clusters remain in the Kubernetes cluster. It provides the conversion webhook required for
the legacy operator and Kubernetes API server to read and write those objects. Only uninstall
it or disable migration mode after all v1alpha1 clusters have been migrated and
`status.storedVersions` has been patched as described in the controller migration guide.

If you need to restore the original CrdbCluster manifest for any reason, apply the backup
only after confirming it will not overwrite intentional changes made after the backup:

```
kubectl apply -f backup/crdbcluster-$CRDBCLUSTER.yaml
```
