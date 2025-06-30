## Migrate from statefulset to cloud operator

This guide will walk you through migrating a crdb cluster managed via statefulset to the crdb cloud operator. We assume you've configured a statefulset cluster using the helm chart. The goals of this process are to migrate without affecting cluster availability, and to preserve existing disks so that we don't have to replica data into empty volumes. Note that this process scales down the statefulset by one node before adding each operator-managed pod, so cluster capacity will be reduced by one node at times.

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

Next, we need to re-map and generate tls certs. The crdb cloud operator uses slightly different certs than the cockroachdb helm chart and mounts them in configmaps and secrets with different names. Run the `migration-helper` utility with `migrate-certs` option to generate and upload certs to your cluster.

```
bin/migration-helper migrate-certs --statefulset-name $STS_NAME --namespace $NAMESPACE
```

Next, generate manifests for each crdbnode and the crdbcluster based on the state of the statefulset. We generate a manifest for each crdbnode because we want the crdb pods and their associated pvcs to have the same names as the original statefulset-managed pods and pvcs. This means that the new operator-managed pods will use the original pvcs, and won't have to replicate data into empty nodes.

```
mkdir -p manifests
bin/migration-helper build-manifest helm --statefulset-name $STS_NAME --namespace $NAMESPACE --cloud-provider $CLOUD_PROVIDER --cloud-region $REGION --output-dir ./manifests
```

To migrate seamlessly from the cockroachdb helm chart to the cloud operator, we'll scale down statefulset-managed pods and replace them with crdbnode objects, one by one. Then we'll create the crdbcluster that manages the crdbnodes. Because of this order of operations, we need to create some objects that the crdbcluster will eventually own:

```
kubectl create priorityclass crdb-critical --value 500000000
```

Next, install the cloud operator:

```
helm upgrade --install crdb-operator ./cockroachdb-parent/charts/operator
```

For each crdb pod, scale the statefulset down by one replica. For example, for a three-node cluster, first scale the statefulset down to two replicas:

```
kubectl scale statefulset/$STS_NAME --replicas=2
```

Then create the crdbnode corresponding to the statefulset pod you just scaled down:

```
kubectl apply -f manifests/crdbnode-2.yaml
```

Wait for the new pod to become ready. If it doesn't, check the cloud operator logs for errors.

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
However, the CockroachDB Enterprise Operator uses a different port for gRPC communication.
To ensure compatibility, you’ll need to update the public Service to reflect the correct gRPC port used by the operator.

Apply the updated service manifest with:
```
kubectl apply -f manifests/public-service.yaml
```

The existing StatefulSet creates a PodDisruptionBudget (PDB) that conflicts with the one managed by the CockroachDB Enterprise Operator.
To avoid this conflict, delete the existing PDB before applying the CrdbCluster manifest:

```
kubectl delete poddisruptionbudget $STS_NAME-budget
```

Delete the StatefulSet that you previously scaled down to zero, as the Helm upgrade can proceed only if no StatefulSet is present.

```
kubectl delete statefulset $STS_NAME
```

Finally, apply the crdbcluster manifest using helm upgrade:

```
helm upgrade $RELEASE_NAME ./cockroachdb-parent/charts/cockroachdb -f manifests/values.yaml
```
