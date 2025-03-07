
# CockroachDB Helm Chart

[CockroachDB](https://github.com/cockroachdb/cockroach) - the open source, cloud-native distributed SQL database.

## Documentation

Below is a brief overview of operating the CockroachDB Helm Chart(v2) with Operator.

Note that the documentation requires Helm 3.0 or higher.

## Prerequisites Details

* Kubernetes 1.8
* PV support on the underlying infrastructure (only if using `storage.persistentVolume`). [Docker for windows hostpath provisioner is not supported](https://github.com/cockroachdb/docs/issues/3184).
* If you want to secure your cluster to use TLS certificates for all network communication, [Helm must be installed with RBAC privileges](https://helm.sh/docs/topics/rbac/) or else you will get an "attempt to grant extra privileges" error.

# Single Region Deployment

## Installing the Operator Chart

To install the chart with the release name `crdb-operator`:

```shell
$ helm install crdb-operator ./operator
```

## Installing the CockroachDB Chart

To install the chart with the release name `cockroachdb`:

- Modify the regions config under operator section of [`values.yaml`](values.yaml) 

```
    regions:
        - code: us-east-1
          nodes: 3
          cloudProvider: k3d
          namespace: default
```
- Modify the other relevant config like topologySpreadConstraints, nodeAffinity, service ports under operator section etc;
- Modify the certs and self-signer config under tls section

```shell
 $ helm install cockroachdb ./cockroachdb
```


## Upgrading the CockroachDB Cluster

Modify the [`values.yaml`](values.yaml) and apply below command

```shell
 $ helm upgrade cockroachdb ./cockroachdb
```

## Scale Up/Down CockroachDB Cluster 

Update the nodes accordingly under regions section

```
    regions:
        - code: us-east-1
          nodes: 5
          cloudProvider: k3d
          namespace: default
```

```shell
 $ helm upgrade cockroachdb ./cockroachdb
```

## Rolling Restart CockroachDB Cluster

Update the timestamp annotation to do rolling restart of CockroachDB pods

```shell
 helm upgrade cockroachdb . --set timestamp=$(date +%s)
```

## Kill a CockroachDB Node


```shell
 kubectl delete pod <pod-mame> -n test-cockroach
```

# Multi Region Deployment

- Setup the Infra by following [this](https://docs.google.com/document/d/1QxLX4qDDI2gckHAUQ-aE-KMCRwh9p9zIhlOgJpV-5NM/edit?tab=t.0)
- Update the coredns config in each cluster as mentioned in the doc


## Installing the Operator Chart

Install the chart with the release name `crdb-operator` in each cluster

```shell
$ helm install crdb-operator ./operator
```

## Installing the CockroachDB Chart

Install the chart with the release name `cockroachdb` in cluster-1

- Modify the regions config under operator section of [`values.yaml`](values.yaml) to include the cluster-1 region config

```
    regions:
        - code: us-central1
          nodes: 3
          cloudProvider: gcp
          domain: cluster.gke.gcp-us-central1
          namespace: <crdb-namespace>
```
- Modify the other relevant config like topologySpreadConstraints, nodeAffinity, service ports under operator section etc;
- Modify the certs and self-signer config under tls section

```shell
 $ helm install cockroachdb ./cockroachdb
```

Install the chart with the release name `cockroachdb` in cluster-2

- Modify the regions config under operator section of [`values.yaml`](values.yaml) to include the cluster-1, cluster-2 region config

```
    regions:
        - code: us-east1
          nodes: 3
          cloudProvider: gcp
          domain: cluster.gke.gcp-us-east1
          namespace: <crdb-namespace>
        - code: us-central1
          nodes: 3
          cloudProvider: gcp
          domain: cluster.gke.gcp-us-central1
          namespace: <crdb-namespace>
```
- Modify the other relevant config like topologySpreadConstraints, nodeAffinity, service ports under operator section etc;
- Modify the certs and self-signer config under tls section

```shell
 $ helm install cockroachdb ./cockroachdb
```

Install the chart with the release name `cockroachdb` in cluster-3

- Modify the regions config under operator section of [`values.yaml`](values.yaml) to include the cluster-1, cluster-2, cluster-3 region config

```
    regions:
       - code: us-west1
          nodes: 3
          cloudProvider: gcp
          domain: cluster.gke.gcp-us-west1
          namespace: <crdb-namespace>
        - code: us-east1
          nodes: 3
          cloudProvider: gcp
          domain: cluster.gke.gcp-us-east1
          namespace: <crdb-namespace>
        - code: us-central1
          nodes: 3
          cloudProvider: gcp
          domain: cluster.gke.gcp-us-central1
          namespace: <crdb-namespace>

```
- Modify the other relevant config like topologySpreadConstraints, nodeAffinity, service ports under operator section etc;
- Modify the certs and self-signer config under tls section

```shell
 $ helm install cockroachdb ./cockroachdb
```

## Upgrading the CockroachDB Chart
Modify the [`values.yaml`](values.yaml) and apply below command

Run the below command in the cluster you intend to perform cluster upgrade

```shell
 $ helm upgrade cockroachdb ./cockroachdb
```

## Scale Up/Down CockroachDB Cluster

Update the nodes accordingly under regions section

```
    regions:
        - code: us-central1
          nodes: 5
          cloudProvider: gcp
          domain: cluster.gke.gcp-us-central1
          namespace: <crdb-namespace>
```

Run the below command in the cluster you intend to perform scale up/down

```shell
 $ helm upgrade cockroachdb ./cockroachdb
```

## Rolling Restart CockroachDB Cluster

Update the timestamp annotation to do rolling restart of CockroachDB pods

Run the below command in the cluster you intend to perform rolling restart

```shell
 helm upgrade cockroachdb . --set timestamp=$(date +%s)
```

## Kill a CockroachDB Node

Run the below command in the cluster you intend to perform killing a CockroachDB Node

```shell
 kubectl delete pod <pod-mame> -n test-cockroach
```