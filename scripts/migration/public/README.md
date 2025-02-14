## Migrate from public operator to cloud operator

This guide will walk you through migrating a crdb cluster managed via the public operator to the crdb cloud operator. We assume you've created a cluster using the public operator. The goals of this process are to migrate without affecting cluster availability, and to preserve existing disks so that we don't have to replica data into empty volumes. Note that this process scales down the statefulset by one node before adding each operator-managed pod, so cluster capacity will be reduced by one node at times.

Pre-requisite: Install the public operator and create an operator-managed cluster:

```
kubectl apply -f https://raw.githubusercontent.com/cockroachdb/cockroach-operator/v2.17.0/install/crds.yaml
kubectl apply -f https://raw.githubusercontent.com/cockroachdb/cockroach-operator/v2.17.0/install/operator.yaml

kubectl apply -f https://raw.githubusercontent.com/cockroachdb/cockroach-operator/v2.17.0/examples/example.yaml
```

Set environment variables:

```
export CRDBCLUSTER=cockroachdb
export NAMESPACE=default
export CLOUD_PROVIDER=gcp
export REGION=us-central1
```

Back up crdbcluster resource in case we need to revert:

```
mkdir -p backup
kubectl get crdbcluster -o yaml $CRDBCLUSTER > backup/crdbcluster-$CRDBCLUSTER.yaml
```

Next, we need to re-map and generate tls certs. The crdb cloud operator uses slightly different certs than the public operator and mounts them in configmaps and secrets with different names. Run the `generate-certs.sh` script to generate and upload certs to your cluster.

```
./generate-certs.sh
```

Next, generate manifests for each crdbnode and the crdbcluster based on the state of the statefulset. We generate a manifest for each crdbnode because we want the crdb pods and their associated pvcs to have the same names as the original statefulset-managed pods and pvcs. This means that the new operator-managed pods will use the original pvcs, and won't have to replicate data into empty nodes.

```
./generate-manifests.sh

The public operator and cloud operator use custom resource definitions with the same names, so we have to remove the public operator before installing the cloud operator. Uninstall the public operator, without deleting its managed pods, pvc, etc.:

```

# Ensure that operator can't accidentally delete managed k8s objects.
kubectl delete clusterrolebinding cockroach-operator-rolebinding

# Delete public operator cr.
kubectl delete crdbcluster $CRDBCLUSTER --cascade=orphan

# Delete public operator resources and crd.
kubectl delete -f https://raw.githubusercontent.com/cockroachdb/cockroach-operator/v2.17.0/install/crds.yaml
kubectl delete -f https://raw.githubusercontent.com/cockroachdb/cockroach-operator/v2.17.0/install/operator.yaml
```

Install the cloud operator and wait for it to become ready:

```
helm upgrade --install crdb-operator ./operator
kubectl rollout status deployment/cockroach-operator --timeout=60s
```

To migrate seamlessly from the statefulset to the cloud operator, we'll scale down statefulset-managed pods and replace them with crdbnode objects, one by one. Then we'll create the crdbcluster that manages the crdbnodes. Because of this order of operations, we need to create some objects that the crdbcluster will eventually own:

```
kubectl create priorityclass crdb-critical --value 500000000
yq '(.. | select(tag == "!!str")) |= envsubst' rbac-template.yaml > manifests/rbac.yaml
kubectl apply -f manifests/rbac.yaml
```

For each crdb pod, scale the statefulset down by one replica. For example, for a three-node cluster, first scale the statefulset down to two replicas:

```
kubectl scale statefulset/$CRDBCLUSTER --replicas=2
```

Then create the crdbnode corresponding to the statefulset pod you just scaled down:

```
kubectl apply -f manifests/crdbnode-$CRDBCLUSTER-2.yaml
```

Wait for the new pod to become ready. If it doesn't, check the cloud operator logs for errors.

Repeat this process for each crdb node until the statefulset has zero replicas.

The public operator creates a pod disruption budget that conflicts with a pod disruption budget managed by the cloud operator. Before applying the crdbcluster manifest, delete the existing pod disruption budget:

```
kubectl delete poddisruptionbudget $CRDBCLUSTER
```

Finally, apply the crdbcluster manifest:

```
kubectl apply -f manifests/crdbcluster-$CRDBCLUSTER.yaml
```
