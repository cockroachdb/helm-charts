
# CockroachDB Helm Chart

[CockroachDB](https://github.com/cockroachdb/cockroach) - the open source, cloud-native distributed SQL database.

## Documentation

Below is a brief overview of operating the CockroachDB Helm Chart(v2) with Operator.

Note that the documentation requires Helm 3.0 or higher.

## Prerequisites Details

* Kubernetes 1.8
* If you want to secure your cluster to use TLS certificates for all network communication, [Helm must be installed with RBAC privileges](https://helm.sh/docs/topics/rbac/) or else you will get an "attempt to grant extra privileges" error.

# Single Region Deployment

## Installing the Operator Chart

To install the chart with the release name `crdb-operator`:

```shell
$ helm install crdb-operator ./operator -n test-cockroach
```

## Installing the CockroachDB Chart

To install the chart with the release name `cockroachdb`:

- Modify the regions config under operator section of [`values.yaml`](values.yaml)

```
    regions:
        - code: us-east-1
          nodes: 3
          cloudProvider: k3d
          namespace: test-cockroach
```
- Modify the other relevant config like topologySpreadConstraints, nodeAffinity, service ports under operator section etc;
- Modify the certs and self-signer config under tls section

```shell
 $ helm install cockroachdb ./cockroachdb -n test-cockroach
```


## Upgrading the CockroachDB Cluster

Modify the [`values.yaml`](values.yaml) and apply below command

```shell
 $ helm upgrade cockroachdb ./cockroachdb -n test-cockroach
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
 $ helm upgrade cockroachdb ./cockroachdb -n test-cockroach
```

## Rolling Restart CockroachDB Cluster

Update the timestamp annotation to do rolling restart of CockroachDB pods

```shell
 helm upgrade cockroachdb . --set timestamp=$(date +%s) -n test-cockroach
```

## Kill a CockroachDB Node


```shell
 kubectl delete pod <pod-name> -n test-cockroach
```

# Multi Region Deployment

- Setup the Infra by following [this](https://docs.google.com/document/d/1QxLX4qDDI2gckHAUQ-aE-KMCRwh9p9zIhlOgJpV-5NM/edit?tab=t.0)
- Update the coredns config in each cluster as mentioned in the doc


## Installing the Operator Chart

Install the chart with the release name `crdb-operator` in each cluster

```shell
$ helm install crdb-operator ./operator -n test-cockroach
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
 $ helm install cockroachdb ./cockroachdb -n test-cockroach
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
 $ helm install cockroachdb ./cockroachdb -n test-cockroach
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
 $ helm install cockroachdb ./cockroachdb -n test-cockroach
```

## Upgrading the CockroachDB Chart
Modify the [`values.yaml`](values.yaml) and apply below command

Run the below command in the cluster you intend to perform cluster upgrade

```shell
 $ helm upgrade cockroachdb ./cockroachdb -n test-cockroach
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
 $ helm upgrade cockroachdb ./cockroachdb -n test-cockroach
```

## Rolling Restart CockroachDB Cluster

Update the timestamp annotation to do rolling restart of CockroachDB pods

Run the below command in the cluster you intend to perform rolling restart

```shell
 helm upgrade cockroachdb . --set timestamp=$(date +%s) -n test-cockroach
```

## Kill a CockroachDB Node

Run the below command in the cluster you intend to perform killing a CockroachDB Node

```shell
 kubectl delete pod <pod-name> -n test-cockroach
```


## Deep dive

### Connecting to the CockroachDB cluster

Once you've created the cluster, you can start talking to it by connecting to its `-public` Service. CockroachDB is PostgreSQL wire protocol compatible, so there's a [wide variety of supported clients](https://www.cockroachlabs.com/docs/install-client-drivers.html). As an example, we'll open up a SQL shell using CockroachDB's built-in shell and play around with it a bit, like this (likely needing to replace `my-release-cockroachdb-public` with the name of the `-public` Service that was created with your installed chart):

```shell
$ kubectl run cockroach-client --rm -it \
--image=cockroachdb/cockroach \
--restart=Never \
-- sql --insecure --host my-release-cockroachdb-public
```
```
Waiting for pod default/cockroach-client to be running, status is Pending,
pod ready: false
If you don't see a command prompt, try pressing enter.
root@cockroachdb-public:26257/defaultdb> SHOW regions;
      region      |                          zones                          | database_names | primary_region_of | secondary_region_of
------------------+---------------------------------------------------------+----------------+-------------------+----------------------
  gcp-us-central1 | {gcp-us-central1-b,gcp-us-central1-c,gcp-us-central1-f} | {}             | {}                | {}
  gcp-us-east1    | {gcp-us-east1-b,gcp-us-east1-c,gcp-us-east1-d}          | {}             | {}                | {}
  gcp-us-west1    | {gcp-us-west1-a,gcp-us-west1-b,gcp-us-west1-c}          | {}             | {}                | {}
(3 rows)

root@cockroachdb-public:26257/defaultdb> SHOW DATABASES;
  database_name | owner | primary_region | secondary_region | regions | survival_goal
----------------+-------+----------------+------------------+---------+----------------
  defaultdb     | root  | NULL           | NULL             | {}      | NULL
  postgres      | root  | NULL           | NULL             | {}      | NULL
  system        | node  | NULL           | NULL             | {}      | NULL
(3 rows)

root@cockroachdb-public:26257/defaultdb> CREATE DATABASE bank;
CREATE DATABASE

root@cockroachdb-public:26257/defaultdb> CREATE TABLE bank.accounts (id INT
PRIMARY KEY, balance DECIMAL);
CREATE TABLE

root@cockroachdb-public:26257/defaultdb> INSERT INTO bank.accounts VALUES(1234, 10000.50);
INSERT 0 1

root@cockroachdb-public:26257/defaultdb> SELECT * FROM bank.accounts;
   id  | balance
-------+-----------
  1234 | 10000.50
  
(1 row)
root@my-release-cockroachdb-public:26257> \q
```

> If you are running in secure mode, you will have to provide a client certificate to the cluster in order to authenticate, so the above command will not work. See [here](https://github.com/cockroachdb/cockroach/blob/master/cloud/kubernetes/client-secure.yaml) for an example of how to set up an interactive SQL shell against a secure cluster or [here](https://github.com/cockroachdb/cockroach/blob/master/cloud/kubernetes/example-app-secure.yaml) for an example application connecting to a secure cluster.


### Accessing the Admin UI


Create the admin credentials to Log in to the UI. See [here](https://www.cockroachlabs.com/docs/stable/deploy-cockroachdb-with-kubernetes?#step-3-use-the-built-in-sql-client)
> CREATE USER roach WITH PASSWORD 'Q7gc8rEdS';

> GRANT admin TO roach;

Port-forward the CockroachDB http service
```shell
 $ kubectl port-forward my-release-cockroachdb-public 8080
```

You should then be able to access the Admin UI by visiting <http://localhost:8080/> in your web browser.

Login with admin credentials and you should be able to see all the information about cluster 

