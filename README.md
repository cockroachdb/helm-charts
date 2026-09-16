# CockroachDB Helm Charts

This repository contains Helm charts for deploying [CockroachDB](https://github.com/cockroachdb/cockroach), 
the open-source, cloud-native distributed SQL database.

## Installation Options

You can install CockroachDB using two approaches, depending on your requirements:


### 1. [cockroachdb-legacy](./cockroachdb-legacy)

The traditional chart that deploys CockroachDB in **StatefulSet mode**.  
This is a direct installation method for running CockroachDB clusters.

➡️ See the [cockroachdb-legacy/README.md](./cockroachdb-legacy/README.md) for detailed installation instructions.



### 2. [cockroachdb-operator](./cockroachdb-operator)

The **new recommended way** of installing CockroachDB using the **CockroachDB Operator**.  
This umbrella chart manages both the Operator and the CockroachDB cluster it provisions.

➡️ See the [cockroachdb-operator/charts/operator/README.md](./cockroachdb-operator/charts/operator/README.md) for details on installing the Operator.
➡️ See the [cockroachdb-operator/charts/cockroachdb/README.md](./cockroachdb-operator/charts/cockroachdb/README.md) for details on installing cockroachdb.
➡️ See [VERSIONING.md](./cockroachdb-operator/docs/VERSIONING.md) for operator-managed chart versions, upgrade order, and published chart locations.



## Certificates and Security

1. **Self-Signer**
      - Information about certificate management with self-signer can be found [here](./docs/certificate-management/self-signer.md).

2. **Cert-manager**
      - Information about certificate management with cert-manager can be found [here](./docs/certificate-management/cert-manager.md).  


## Migration

There are two common migration paths:

1. **From the public operator → CockroachDB Operator**
   - Automatic (recommended): [docs/migration/operator/controller_migration.md](docs/migration/operator/controller_migration.md)
   - Manual: [docs/migration/operator/manual_migration.md](docs/migration/operator/manual_migration.md)

2. **From a StatefulSet deployment → CockroachDB Operator**
   - Automatic (recommended): [docs/migration/helm/controller_migration.md](docs/migration/helm/controller_migration.md)
   - Manual: [docs/migration/helm/manual_migration.md](docs/migration/helm/manual_migration.md)
