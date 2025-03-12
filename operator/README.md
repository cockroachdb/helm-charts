# CockroachDB Helm Chart

[CockroachDB](https://github.com/cockroachdb/cockroach) - the cloud-native distributed SQL database.

Below is a brief overview of operating the CockroachDB Helm Chart(v2) with Operator.

## Prerequisites

* Kubernetes 1.30 or higher
* Helm 3.0 or higher
* Create a namespace to perform the operations against. In this case, we are using `cockroach-ns` namespace.
* If you want to secure your cluster to use TLS certificates for all network communications, [Helm must be installed with RBAC privileges](https://helm.sh/docs/topics/rbac/) or else you will get an "attempt to grant extra privileges" error.

Set the environment variables:

``` shell
export CRDBOPERATOR=crdb-operator
export CRDBCLUSTER=cockroachdb
export NAMESPACE=cockroach-ns
```

## Notes

All the helm commands below reference the chart folder available locally after checking out this GitHub repository. Alternatively, you may also reference the charts in the Helm repository.
The operator chart does not exist in the Helm repository yet and will be added soon.

## Installation

### Install Operator

```shell
$ helm install $CRDBOPERATOR ./operator -n $NAMESPACE
```

### Install CockroachDB

- Modify the `regions` config under the `operator` section of [`cockroachdb/values.yaml`](/cockroachdb/values.yaml)

```
    regions:
        - code: us-central1
          nodes: 3
          cloudProvider: gcp
          namespace: cockroach-ns
```

- Modify the other relevant config like `topologySpreadConstraints`, `service.ports`, etc. under the `operator` section, as required.
- Modify the certs and self-signer config under the `tls` section, as required. If you are using a custom CA, update the values for `clientCaConfigMapName` and `nodeCaConfigMapName` under the `operator.certificates.externalCertificates` section.
    
```   
       externalCertificates:
         clientCaConfigMapName: <custom-ca-secret-name>-crt
         nodeCaConfigMapName: <custom-ca-secret-name>-crt
```

Install the cockroachdb chart:

```shell
 $ helm install $CRDBCLUSTER ./cockroachdb -n $NAMESPACE
```

### Multi Region Deployments

For multi-region cluster deployments, ensure the required networking is setup which allows for service discovery across regions.

For each region, modify the `regions` config under the `operator` section of [`cockroachdb/values.yaml`](/cockroachdb/values.yaml) and perform `helm install` as above against the respective Kubernetes cluster.

While applying `helm install` in a region, please verify that the domain matches the `clusterDomain` in `values.yaml` for the corresponding region.

```
    regions:
        - code: us-central1
          nodes: 3
          cloudProvider: gcp
          domain: cluster.gke.gcp-us-central1
          namespace: cockroach-ns
        - code: us-east1
          nodes: 3
          cloudProvider: gcp
          domain: cluster.gke.gcp-us-east1
          namespace: cockroach-ns
```

## Upgrade CockroachDB cluster

Modify the required config in [`cockroachdb/values.yaml`](/cockroachdb/values.yaml) and perform an upgrade through Helm:

```shell
 $ helm upgrade $CRDBCLUSTER ./cockroachdb -n $NAMESPACE
```

## Scale Up/Down CockroachDB cluster

Update the nodes accordingly under `regions` section and perform the helm upgrade:

```
    regions:
        - code: us-central1
          nodes: 5
          cloudProvider: gcp
          domain: cluster.gke.gcp-us-central1
          namespace: cockroach-ns
```

```shell
 $ helm upgrade $CRDBCLUSTER ./cockroachdb -n $NAMESPACE
```

## Rolling Restart of CockroachDB Cluster

Update the timestamp annotation to do a rolling restart of all CockroachDB pods:

```shell
 helm upgrade $CRDBCLUSTER ./cockroachdb --set-string timestamp="$(date -u +"%Y-%m-%dT%H:%M:%SZ")" --reuse-values -n $NAMESPACE
```

## Kill a CockroachDB Node

```shell
 kubectl delete pod <pod-name> -n $NAMESPACE
```

## Connecting to the CockroachDB cluster

CockroachDB is PostgreSQL wire protocol compatible, so there's a [wide variety of supported clients](https://www.cockroachlabs.com/docs/install-client-drivers.html).
Once the cluster has been created, you can connect to it through a public Service object created during installation (replace `<public-service>` with the Service name that is suffixed by `-public`). As an example, we'll use CockroachDB's built-in SQL client as below:

```shell
$ kubectl run cockroach-client --rm -it \
--image=cockroachdb/cockroach \
--restart=Never \
-- sql --insecure --host <public-service>
```
```
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

Note that if you are running in secure mode, you will have to provide the client certificate for authentication. Kindly refer to one of the below for connecting to a secure cluster:
- https://github.com/cockroachdb/cockroach/blob/master/cloud/kubernetes/client-secure.yaml
- https://github.com/cockroachdb/cockroach/blob/master/cloud/kubernetes/example-app-secure.yaml

## Accessing the UI console

Create admin credentials to log in to the UI console.

```shell
root@cockroachdb-public:26257/defaultdb> CREATE USER roach WITH PASSWORD 'Q7gc8rEdS';

root@cockroachdb-public:26257/defaultdb> GRANT admin TO roach;
```

For further details, kindly refer to https://www.cockroachlabs.com/docs/stable/deploy-cockroachdb-with-kubernetes?#step-3-use-the-built-in-sql-client.

Port-forward the CockroachDB HTTP service:
```shell
 $ kubectl port-forward <public-service> 8080 -n $NAMESPACE
```

You should then be able to access the console by visiting <http://localhost:8080/> in your web browser. Login with the admin credentials and you should be able to see all the information about cluster.
