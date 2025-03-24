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

- Update `operator.enabled` to `true` in [`cockroachdb/values.yaml`](/cockroachdb/values.yaml).
```
  operator:
    enabled: true
```
- Modify the `regions` configuration under the `operator` section of [`cockroachdb/values.yaml`](/cockroachdb/values.yaml). The default `regions` configuration uses k3d, so update it as per your cloud provider (e.g. `gcp`, `aws`, etc.)

```
  regions:
    - code: us-central1
      nodes: 3
      cloudProvider: gcp
      namespace: cockroach-ns
```

- Modify the other relevant configuration like `topologySpreadConstraints`, `service.ports`, etc. under the `operator` section, as required.
- By default, the certs are created by the self-signer utility. In case of a custom CA cert, modify the configuration under the `tls` section:

```
tls:
  certs:
    selfSigner:
      caProvided: true
      caSecret: <ca-secret-name>
```

Install the cockroachdb chart:

```shell
$ helm install $CRDBCLUSTER ./cockroachdb -n $NAMESPACE
```

### Multi Region Deployments

For multi-region cluster deployments, ensure the required networking is setup which allows for service discovery across regions. Also, ensure that the same CA cert is used across all the regions.

For each region, modify the `regions` configuration under the `operator` section of [`cockroachdb/values.yaml`](/cockroachdb/values.yaml) and perform `helm install` as above against the respective Kubernetes cluster.

While applying `helm install` in a given region:
- Verify that the domain matches the `clusterDomain` in `values.yaml` for the corresponding region
- Ensure `regions` captures the information for regions that have already been deployed, including the current region. This enables CockroachDB in the current region to connect to CockroachDB deployed in the existing regions.

For example, if `us-central1` has already been deployed, and `us-east1` is being deployed to:

```
clusterDomain: cluster.gke.gcp-us-east1
operator:
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

Modify the required configuration in [`cockroachdb/values.yaml`](/cockroachdb/values.yaml) and perform an upgrade through Helm:

```shell
$ helm upgrade --reuse-values $CRDBCLUSTER ./cockroachdb --values ./cockroachdb/values.yaml -n $NAMESPACE
```

## Scale Up/Down CockroachDB cluster

Update the nodes accordingly under `regions` section and perform the helm upgrade:

```
  regions:
    - code: us-central1
      nodes: 4
      cloudProvider: gcp
      domain: cluster.gke.gcp-us-central1
      namespace: cockroach-ns
```

```shell
$ helm upgrade --reuse-values $CRDBCLUSTER ./cockroachdb --values ./cockroachdb/values.yaml -n $NAMESPACE
```

## Rolling Restart of CockroachDB Cluster

Update the timestamp annotation to do a rolling restart of all CockroachDB pods:

```shell
$ helm upgrade --reuse-values $CRDBCLUSTER ./cockroachdb --set-string timestamp="$(date -u +"%Y-%m-%dT%H:%M:%SZ")" -n $NAMESPACE
```

## Kill a CockroachDB Node

```shell
$ kubectl delete pod <pod-name> -n $NAMESPACE
```

## Connecting to the CockroachDB cluster

Follow the steps documented in https://www.cockroachlabs.com/docs/stable/deploy-cockroachdb-with-kubernetes?filters=helm#step-3-use-the-built-in-sql-client to create a secure CockroachDB client.
You could confirm the regions using a SQL command as below:

```
> SHOW regions;
      region      |                          zones                          | database_names | primary_region_of | secondary_region_of
------------------+---------------------------------------------------------+----------------+-------------------+----------------------
  gcp-us-central1 | {gcp-us-central1-b,gcp-us-central1-c,gcp-us-central1-f} | {}             | {}                | {}
  gcp-us-east1    | {gcp-us-east1-b,gcp-us-east1-c,gcp-us-east1-d}          | {}             | {}                | {}
(2 rows)
```

In order to access the DB console, follow the steps documented in https://www.cockroachlabs.com/docs/stable/deploy-cockroachdb-with-kubernetes?filters=helm#step-4-access-the-db-console.
Use the corresponding Service name that is suffixed by `-public` (in this case, `$CRDBCLUSTER-public`).
