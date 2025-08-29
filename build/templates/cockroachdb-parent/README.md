
## Compatibility Metrics

This operator is tested against a range of Kubernetes versions.  
The table below indicates which versions are officially supported.

| Operator Version  | Kubernetes Versions Tested                                    |
|-------------------|---------------------------------------------------------------|
| 25.3.0-preview+1  | 1.27, 1.28, 1.29, 1.30, 1.31, 1.32, 1.33                      |

⚠️ **Note** → Older versions may still work, but **not officially tested**

**NOTE: The below contents of this README are currently work in progress.**

# CockroachDB Parent Helm Chart

[CockroachDB](https://github.com/cockroachdb/cockroach) - the cloud-native distributed SQL database.

Below is a brief overview of operating the CockroachDB Helm Chart(v2) with Operator.

This Helm chart installs both CockroachDB and its Operator using Helm Spray for proper dependency management and installation order.



## Prerequisites

* Kubernetes 1.30 or higher
* Helm 3.0 or higher
* [Helm Spray Plugin](https://github.com/ThalesGroup/helm-spray)
* If you want to secure your cluster to use TLS certificates for all network communications, [Helm must be installed with RBAC privileges](https://helm.sh/docs/topics/rbac/) or else you will get an "attempt to grant extra privileges" error.


## Installing the Helm Spray Plugin

```bash
helm plugin install https://github.com/ThalesGroup/helm-spray
```


## Architecture

This parent chart includes two sub-charts:

1. **operator** - The CockroachDB Operator chart that installs first
2. **cockroachdb** - The CockroachDB database chart that installs after operator is ready

The chart uses both Helm hooks and Helm Spray annotations to ensure proper installation order.

## Configuration

The following table lists the configurable parameters of the chart and their default values.

| Parameter | Description | Default |
| --------- | ----------- | ------- |
| `operator.enabled` | Enable/disable operator installation | `true` |
| `cockroachdb.enabled` | Enable/disable cockroachdb installation | `true` |


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

``` bash
helm dependencies update ./cockroachdb-parent
```

- Modify the `regions` configuration under the `cockroachdb` section of [`cockroachdb-parent/values.yaml`](/cockroachdb-parent/values.yaml). The default `regions` configuration uses k3d, so update it as per your cloud provider (e.g. `gcp`, `aws`, etc.)
- The cloudProvider field is optional, so it can be ignored for deployments hosting on other than `gcp`, `aws`, `azure`and `k3d`.

```
  regions:
    - code: us-central1
      nodes: 3
      cloudProvider: gcp
      namespace: cockroach-ns
```

- Modify the other relevant configuration like `topologySpreadConstraints`, `service.ports`, etc. under the `cockroachdb` section, as required.
- By default, the certs are created by the self-signer utility. In case of a custom CA cert, modify the configuration under the `tls` section:

```
tls:
  certs:
    selfSigner:
      caProvided: true
      caSecret: <ca-secret-name>
```

To install both the charts follow below commands:

```bash
# Install using Helm Spray
helm spray -n cockroachdb-ns --create-namespace --timeout 10m ./cockroachdb-parent
```

You can specify each parameter using the `--set` flag when installing the chart:

```bash
helm spray -n cockroachdb-ns --create-namespace ./cockroachdb-parent --set global.environment=development
```

Alternatively, a YAML file that specifies the values for the parameters can be provided:

```bash
helm spray -n cockroachdb-ns --create-namespace ./cockroachdb-parent -f values-override.yaml
```

To install only the operator or cockroachdb chart, use the following commands:

### Multi Region Deployments

For multi-region cluster deployments, ensure the required networking is setup which allows for service discovery across regions. Also, ensure that the same CA cert is used across all the regions.

For each region, modify the `regions` configuration under the `cockroachdb` section of [`cockroachdb-parent/values.yaml`](/cockroachdb-parent/values.yaml) and perform `helm install` as above against the respective Kubernetes cluster.

While applying `helm install` in a given region:
- Verify that the domain matches the `clusterDomain` in [`cockroachdb-parent/values.yaml`](/cockroachdb-parent/values.yaml) under `cockroachdb` section for the corresponding region.
- Ensure `regions` captures the information for regions that have already been deployed, including the current region. This enables CockroachDB in the current region to connect to CockroachDB deployed in the existing regions.

For example, if `us-central1` has already been deployed, and `us-east1` is being deployed to:

```
clusterDomain: cluster.gke.gcp-us-east1
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


Upgrade using helm spray:

Modify the required configuration in [`cockroachdb-parent/values.yaml`](/cockroachdb-parent/values.yaml) and perform an upgrade through Helm:

```shell
$ helm spray --reuse-values $CRDBCLUSTER ./cockroachdb --values ./cockroachdb-parent/values.yaml -n $NAMESPACE --exclude=operator
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
$ helm spray --reuse-values $CRDBCLUSTER ./cockroachdb-parent --values ./cockroachdb-parent/values.yaml -n $NAMESPACE --exclude=operator
```

## Rolling Restart of CockroachDB Cluster

Update the timestamp annotation to do a rolling restart of all CockroachDB pods:

```shell
$ helm spray --reuse-values $CRDBCLUSTER ../cockroachdb-parent --set-string timestamp="$(date -u +"%Y-%m-%dT%H:%M:%SZ")" -n $NAMESPACE
```

## Kill a CockroachDB Node

```shell
$ kubectl delete pod <pod-name> -n $NAMESPACE
```
## Uninstalling the Chart

To uninstall/delete the `my-cockroachdb` deployment:

```bash
helm uninstall $CRDBCLUSTER -n $NAMESPACE
```
... wait till the cluster is deleted

```bash
helm uninstall $CRDBOPERATOR -n $NAMESPACE
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

## Additional Resources

- [CockroachDB Documentation](https://www.cockroachlabs.com/docs/)
- [Helm Spray Documentation](https://github.com/ThalesGroup/helm-spray) 