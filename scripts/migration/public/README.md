## Migrate from public operator to cloud operator

This guide will walk you through migrating a crdb cluster managed via the public operator to the crdb cloud operator. We assume you've created a cluster using the public operator. The goals of this process are to migrate without affecting cluster availability, and to preserve existing disks so that we don't have to replica data into empty volumes. Note that this process scales down the statefulset by one node before adding each operator-managed pod, so cluster capacity will be reduced by one node at times.

Pre-requisite: Install the public operator and create an operator-managed cluster:

```
kubectl apply -f https://raw.githubusercontent.com/cockroachdb/cockroach-operator/v2.17.0/install/crds.yaml
kubectl apply -f https://raw.githubusercontent.com/cockroachdb/cockroach-operator/v2.17.0/install/operator.yaml

kubectl apply -f https://raw.githubusercontent.com/cockroachdb/cockroach-operator/v2.17.0/examples/example.yaml
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

Next, we need to re-map and generate tls certs. The crdb cloud operator uses slightly different certs than the public operator and mounts them in configmaps and secrets with different names. Run the `migration-helper` utility with `migrate-certs` option to generate and upload certs to your cluster.

```
bin/migration-helper migrate-certs --statefulset-name $STS_NAME --namespace $NAMESPACE
```

Next, generate manifests for each crdbnode and the crdbcluster based on the state of the statefulset. We generate a manifest for each crdbnode because we want the crdb pods and their associated pvcs to use the same names as the original statefulset-managed pods and pvcs. This means that the new operator-managed pods will use the original pvcs, and won't have to replicate data into empty nodes.

```
mkdir -p manifests
migration-helper build-manifest operator --crdb-cluster $CRDBCLUSTER --namespace $NAMESPACE --cloud-provider $CLOUD_PROVIDER --cloud-region $REGION --output-dir ./manifests
```

The public operator and cloud operator use custom resource definitions with the same names, so we have to remove the public operator before installing the cloud operator. Uninstall the public operator, without deleting its managed pods, pvc, etc.:

```
# Ensure that operator can't accidentally delete managed k8s objects.
kubectl delete clusterrolebinding cockroach-operator-rolebinding

# Delete public operator cr.
kubectl delete crdbcluster $CRDBCLUSTER --cascade=orphan

# Delete public operator resources and crd.
kubectl delete -f https://raw.githubusercontent.com/cockroachdb/cockroach-operator/v2.17.0/install/crds.yaml
kubectl delete serviceaccount cockroach-operator-sa -n cockroach-operator-system
kubectl delete clusterrole cockroach-operator-role
kubectl delete clusterrolebinding cockroach-operator-rolebinding
kubectl delete service cockroach-operator-webhook-service -n cockroach-operator-system
kubectl delete deployment cockroach-operator-manager -n cockroach-operator-system
kubectl delete mutatingwebhookconfigurations cockroach-operator-mutating-webhook-configuration
kubectl delete validatingwebhookconfigurations cockroach-operator-validating-webhook-configuration
```

Install the cloud operator and wait for it to become ready:

```
helm upgrade --install crdb-operator ./operator
kubectl rollout status deployment/cockroach-operator --timeout=60s
```

To migrate seamlessly from the statefulset to the cloud operator, we'll scale down statefulset-managed pods and replace them with crdbnode objects, one by one. Then we'll create the crdbcluster that manages the crdbnodes. Because of this order of operations, we need to create some objects that the crdbcluster will eventually own:

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

Wait until the new pod is ready. If it fails to become ready, check the Cloud Operator logs for errors.

Repeat this process for each CRDB node until the StatefulSet reaches zero replicas.

The public operator creates a pod disruption budget that conflicts with a pod disruption budget managed by the cloud operator. Before applying the crdbcluster manifest, delete the existing pod disruption budget:

```
kubectl delete poddisruptionbudget $CRDBCLUSTER
```

Annotate existing objects so that they can be managed by the Helm chart:

```
kubectl annotate service $CRDBCLUSTER-public meta.helm.sh/release-name="$CRDBCLUSTER"
kubectl annotate service $CRDBCLUSTER-public meta.helm.sh/release-namespace="$NAMESPACE"
kubectl label service $CRDBCLUSTER-public app.kubernetes.io/managed-by=Helm --overwrite=true
```

Finally, apply the crdbcluster manifest:

```
helm install $CRDBCLUSTER ./cockroachdb -f manifests/values.yaml
```

One the migration is successful, now delete the statefulset created by public operator:
```
kubectl delete statefulset $CRDBCLUSTER 
```