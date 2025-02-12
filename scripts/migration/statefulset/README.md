## Migrate from statefulset to cloud operator

This guide will walk you through migrating a crdb cluster managed via statefulset to the crdb cloud operator. We assume you've configured a statefulset cluster using the helm chart. The goals of this process are to migrate without affecting cluster availability, and to preserve existing disks so that we don't have to replica data into empty volumes. Note that this process scales down the statefulset by one node before adding each operator-managed pod, so cluster capacity will be reduced by one node at times.

```
helm upgrade --install --set operator.enabled=false crdb-test --debug ./cockroachdb
```

First, export environment variables about the current deployment:

```
export RELEASE_NAME=$(kubectl get sts -o yaml crdb-test-cockroachdb | yq '.metadata.annotations."meta.helm.sh/release-name"')
export NAMESPACE=$(kubectl get sts -o yaml crdb-test-cockroachdb | yq '.metadata.annotations."meta.helm.sh/release-namespace"')
```

Next, we need to re-map and generate tls certs. The crdb cloud operator uses slightly different certs than the statefulset and mounts them in configmaps and secrets with different names. Run the `generate-certs.sh` script to generate and upload certs to your cluster.

```
./generate-certs.sh
```

The helm chart configures crdb to listen for grpc and sql on one port and for http on another. The cloud operator uses three distinct ports for grpc, sql, and http. Back up the existing public kubernetes service in case we need to revert, then patch it to route port 26257 to container port 26257 for sql, and 26258 to the grpc port (which may use different container ports for the statefulset vs the cloud operator).

```
kubectl get svc -o yaml $RELEASE_NAME-cockroachdb-public > backup/$RELEASE_NAME-cockroachdb-svc.yaml
kubectl patch svc $RELEASE_NAME-cockroachdb-public -p '{"spec": {"ports": [{"name": "sql", "port": 26257, "protocol": "TCP", "targetPort": 26257}]}}'
kubectl patch svc $RELEASE_NAME-cockroachdb-public -p '{"spec": {"ports": [{"name": "grpc", "port": 26258, "protocol": "TCP", "targetPort": "grpc"}]}}'
```

To migrate seamlessly from the statefulset to the cloud operator, we'll scale down statefulset-managed pods and replace them with crdbnode objects, one by one. Then we'll create the crdbcluster that manages the crdbnodes. Because of this order of operations, we need to create some objects that the crdbcluster will eventually own:

```
kubectl create priorityclass crdb-critical --value 500000000

yq '(.. | select(tag == "!!str")) |= envsubst' rbac-template.yaml > rbac.yaml
kubectl apply -f rbac.yaml
```

Next, generate manifests for each crdbnode and the crdbcluster based on the state of the statefulset. We generate a manifest for each crdbnode because we want the crdb pods and their associated pvcs to have the same names as the original statefulset-managed pods and pvcs. This means that the new operator-managed pods will use the original pvcs, and won't have to replicate data into empty nodes.

```
./generate-manifests.sh
```

Next, install the cloud operator:

```
helm upgrade --install crdb-operator ./operator
```

For each crdb pod, scale the statefulset down by one replica. For example, for a three-node cluster, first scale the statefulset down to two replicas:

```
kubectl scale statefulset/$RELEASE_NAME-cockroachdb --replicas=2
```

Then create the crdbnode corresponding to the statefulset pod you just scaled down:

```
kubectl apply -f crdbnode-$RELEASE_NAME-2.yaml
```

Wait for the new pod to become ready. If it doesn't, check the cloud operator logs for errors.

Repeat this process for each crdb node until the statefulset has zero replicas.

The statefulset creates a pod disruption budget that conflicts with a pod disruption budget managed by the cloud operator. Before applying the crdbcluster manifest, delete the existing pod disruption budget:

```
kubectl delete poddisruptionbudget $RELEASE_NAME-cockroachdb-budget
```

Finally, apply the crdbcluster manifest:

```
kubectl apply -f crdbcluster-$RELEASE_NAME.yaml
```
