
## Compatibility Metrics

This operator is tested against a range of Kubernetes versions.  
The table below indicates which versions are officially supported.

| Operator Version  | Kubernetes Versions Tested                                    |
|-------------------|---------------------------------------------------------------|
| 25.3.0-preview+1  | 1.27, 1.28, 1.29, 1.30, 1.31, 1.32, 1.33                      |

⚠️ **Note** → Older versions may still work, but **not officially tested**

**NOTE: The below contents of this README are currently work in progress.**

# CockroachDB Operator Umbrella Helm Chart

[CockroachDB](https://github.com/cockroachdb/cockroach) - the cloud-native distributed SQL database.

Below is a brief overview of operating the CockroachDB Helm Chart (v2) with Operator.

This umbrella Helm chart installs both the CockroachDB Operator and a CockroachDB cluster it manages. Subcharts are ordered via Helm hook weights so the operator becomes ready before the CockroachDB cluster is applied.

## Prerequisites

* Kubernetes 1.30 or higher
* Helm 3.0 or higher
* If you want to secure your cluster to use TLS certificates for all network communications, [Helm must be installed with RBAC privileges](https://helm.sh/docs/topics/rbac/) or else you will get an "attempt to grant extra privileges" error.

## Architecture

This umbrella chart includes two sub-charts:

1. **operator** - The CockroachDB Operator chart that installs first
2. **cockroachdb** - The CockroachDB database chart that installs after operator is ready

The chart uses Helm hooks to ensure proper installation order.

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

All the helm commands below reference the chart folder available locally after checking out this GitHub repository. See [VERSIONING.md](docs/VERSIONING.md) for published chart locations, chart versions, and upgrade order.

## Installation

``` bash
helm dependency update ./cockroachdb-operator
```

- Modify the `regions` configuration under the `cockroachdb` section of [`cockroachdb-operator/values.yaml`](/cockroachdb-operator/values.yaml). The default `regions` configuration uses k3d, so update it as per your cloud provider (e.g. `gcp`, `aws`, etc.)
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

To install both charts follow the command below:

```bash
helm install $CRDBCLUSTER ./cockroachdb-operator -n $NAMESPACE --create-namespace --timeout 10m
```

You can specify each parameter using the `--set` flag when installing the chart:

```bash
helm install $CRDBCLUSTER ./cockroachdb-operator -n $NAMESPACE --create-namespace --set global.environment=development
```

Alternatively, a YAML file that specifies the values for the parameters can be provided:

```bash
helm install $CRDBCLUSTER ./cockroachdb-operator -n $NAMESPACE --create-namespace -f values-override.yaml
```

### Multi Region Deployments

For multi-region cluster deployments, ensure the required networking is setup which allows for service discovery across regions. Also, ensure that the same CA cert is used across all the regions.

For each region, modify the `regions` configuration under the `cockroachdb` section of [`cockroachdb-operator/values.yaml`](/cockroachdb-operator/values.yaml) and perform `helm install` as above against the respective Kubernetes cluster.

While applying `helm install` in a given region:
- Verify that the domain matches the `clusterDomain` in [`cockroachdb-operator/values.yaml`](/cockroachdb-operator/values.yaml) under `cockroachdb` section for the corresponding region.
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

Modify the required configuration in [`cockroachdb-operator/values.yaml`](/cockroachdb-operator/values.yaml) and perform an upgrade through Helm:

```shell
$ helm upgrade --reuse-values $CRDBCLUSTER ./cockroachdb-operator --values ./cockroachdb-operator/values.yaml -n $NAMESPACE
```

### Helm 4 server-side apply

Helm 4 uses server-side apply (SSA) by default for releases first installed with
Helm 4. Existing Helm 3 releases normally retain client-side apply. Normal
installs and upgrades, where `CrdbCluster` changes are made through chart values,
do not require any additional flags.

An SSA ownership conflict can occur if a Helm-managed `CrdbCluster` is edited
manually with `kubectl` or another tool. Make the desired change in the chart
values instead. If a later upgrade reports a conflict, inspect the conflicting
fields and `managedFields`, reconcile the manual change with the chart values,
and retry the upgrade. Use `--force-conflicts` only after reviewing the affected
fields and intentionally choosing to return their ownership to Helm. Do not use
Helm 4's `--force-replace` for SSA ownership conflicts.

See the CockroachDB subchart's [Helm 4 server-side apply
guidance](charts/cockroachdb/README.md#helm-4-server-side-apply), the [Helm 4
overview](https://helm.sh/docs/overview/#server-side-apply), and
[HIP-0023](https://helm.sh/community/hips/hip-0023/).

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
$ helm upgrade --reuse-values $CRDBCLUSTER ./cockroachdb-operator --values ./cockroachdb-operator/values.yaml -n $NAMESPACE
```

## Rolling Restart of CockroachDB Cluster

Update the timestamp annotation to do a rolling restart of all CockroachDB pods:

```shell
$ helm upgrade --reuse-values $CRDBCLUSTER ./cockroachdb-operator --set-string cockroachdb.crdbCluster.timestamp="$(date -u +"%Y-%m-%dT%H:%M:%SZ")" -n $NAMESPACE
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
