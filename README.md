# CockroachDB Helm Charts

This repository contains Helm charts for deploying [CockroachDB](https://github.com/cockroachdb/cockroach), 
the open-source, cloud-native distributed SQL database.

## Installation Options

You can install CockroachDB using two approaches, depending on your requirements:


### 1. [cockroachdb](./cockroachdb)

The traditional chart that deploys CockroachDB in **StatefulSet mode**.  
This is a direct installation method for running CockroachDB clusters.

➡️ See the [cockroachdb/README.md](./cockroachdb/README.md) for detailed installation instructions.



### 2. [cockroachdb-parent](./cockroachdb-parent)

The **new recommended way** of installing CockroachDB using the **CockroachDB Operator**.  
This parent chart manages both the Operator and CockroachDB installation.

➡️ See the [cockroachdb-parent/charts/operator/README.md](./cockroachdb-parent/charts/operator/README.md) for details on installing the Operator.
➡️ See the [cockroachdb-parent/charts/cockroachdb/README.md](./cockroachdb-parent/charts/cockroachdb//README.md) for details on installing cockroachdb.



## Certificates and Security

1. **Self-Signer**
      - Information about certificate management with self-signer can be found [here](./docs/certificate-management/self-signer.md).

2. **Cert-manager**
      - Information about certificate management with cert-manager can be found [here](./docs/certificate-management/cert-manager.md).  


## Migration

There are two common migration paths:

1. **From the public operator → CockroachDB Operator**
   - Follow the migration guide provided [here](./docs/migration/operator/README.md).  

2. **From a StatefulSet deployment → CockroachDB Operator**
   - Follow the migration guide provided [here](./docs/migration/helm/README.md).  