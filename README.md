# CockroachDB Helm Charts Repository

[CockroachDB](https://github.com/cockroachdb/cockroach) - the open source, cloud-native distributed SQL database.


# Charts

- [cockroachdb](cockroachdb)

# Self-Cert-Signer Utility

Certificate Self-Signer utility is developed to allow the cockroachdb helm chart to be able to deploy secure cluster,
without any dependency on the outside tool to create or sign its certificate.

You can enable/disable this utility by setting the `tls.certs.selfSigner.enabled` option as true/false.

## Certificates and CA managed by cockroachdb

This option allow cockroachdb to generate the CA, node and client certificates and use those certificates to form a secure
cockroachdb cluster. User can configure the duration and expiry window of each certificate types. Following are the options provided as
default values in hours.

```shell
# Minimum Certificate duration for all the certificates, all certs duration will be validated against this.
tls.certs.selfSigner.minimumCertDuration: 624h
# Duration of CA certificates in hour
tls.certs.selfSigner.caCertDuration: 43800h
# Expiry window of CA certificates means a window before actual expiry in which CA certs should be rotated.
tls.certs.selfSigner.caCertExpiryWindow: 648h
# Duration of Client certificates in hour
tls.certs.selfSigner.clientCertDuration: 672h
# Expiry window of client certificates means a window before actual expiry in which client certs should be rotated.
tls.certs.selfSigner.clientCertExpiryWindow: 48h
# Duration of node certificates in hour
tls.certs.selfSigner.nodeCertDuration: 8760h
# Expiry window of node certificates means a window before actual expiry in which node certs should be rotated.
tls.certs.selfSigner.nodeCertExpiryWindow: 168h
```

These durations can be configured by user with following validations:

1. CaCertExpiryWindow should be be greater than minimumCertDuration.
2. Other certificateDuration - certificateExpiryWindow should be greater than minimumCertDuration.

This utility also handles certificate rotation when they come near expiry. You can enable or disable the certificate
rotation with following setting:

```shell
 # If set, the cockroachdb cert selfSigner will rotate the certificates before expiry.
tls.certs.selfSigner.rotateCerts: true
```

## Certificate managed by cockroachdb && CA provided by user

If user has a custom CA which they already use for certificate signing in their organisation, this utility provides a way
for user to provide the custom CA. All the node and client certificates are signed by this user provided CA.

To provide the CA certificate to the crdb you have to create a tls certificate with `ca.crt` and `ca.key` and provide the
secret as:

```shell
# If set, the user should provide the CA certificate to sign other certificates.
tls.certs.selfSigner.caProvided: true
# It holds the name of the secret with caCerts. If caProvided is set, this can not be empty.
tls.certs.selfSigner.caSecret: "custom-ca-secret"
```

You will still have options to configure the duration and expiry window of the certificates:
```shell
# Minimum Certificate duration for all the certificates, all certs duration will be validated against this.
tls.certs.selfSigner.minimumCertDuration: 624h
# Expiry window of CA certificates means a window before actual expiry in which CA certs should be rotated.
tls.certs.selfSigner.caCertExpiryWindow: 648h
# Duration of Client certificates in hour
tls.certs.selfSigner.clientCertDuration: 672h
# Expiry window of client certificates means a window before actual expiry in which client certs should be rotated.
tls.certs.selfSigner.clientCertExpiryWindow: 48h
# Duration of node certificates in hour
tls.certs.selfSigner.nodeCertDuration: 8760h
# Expiry window of node certificates means a window before actual expiry in which node certs should be rotated.
tls.certs.selfSigner.nodeCertExpiryWindow: 168h
```

This utility will only handle the rotation of client and node certificates, the rotation of custom CA should be done by user.


## Installation of Helm Chart 

When user install cockroachdb cluster with self-signer enabled, you will see the self-signer job.

```
kubectl get pods
NAME                                 READY   STATUS    RESTARTS   AGE
crdb-cockroachdb-self-signer-mmxp8   1/1     Running   0          15s
```

This job will generate CA, client and node certificates based on the user input mentioned in previous section. You can 
see the following secrets representing each certificates:

```
kubectl get secrets 
NAME                                       TYPE                                  DATA   AGE
crdb-cockroachdb-ca-secret                 Opaque                                2      3m10s
crdb-cockroachdb-client-secret             kubernetes.io/tls                     3      3m9s
crdb-cockroachdb-node-secret               kubernetes.io/tls                     3      3m10s
crdb-cockroachdb-self-signer-token-qcc72   kubernetes.io/service-account-token   3      3m29s
crdb-cockroachdb-token-jpbms               kubernetes.io/service-account-token   3      3m8s
default-token-gmhdf                        kubernetes.io/service-account-token   3      11m
sh.helm.release.v1.crdb.v1                 helm.sh/release.v1                    1      3m30s
```

After this, the cockroachdb init jobs starts and copies this certificate to each nodes:

```
prafull@EMPID18004:helm-charts$ kubectl get pods
NAME                          READY   STATUS     RESTARTS   AGE
crdb-cockroachdb-0            0/1     Init:0/1   0          18s
crdb-cockroachdb-1            0/1     Init:0/1   0          18s
crdb-cockroachdb-2            0/1     Init:0/1   0          18s
crdb-cockroachdb-init-fclbb   1/1     Running    0          16s
```

At last, the cockroach db cluster comes into running state with following output:
```
helm install crdb ./cockroachdb/
NAME: crdb
LAST DEPLOYED: Thu Aug 19 18:03:37 2021
NAMESPACE: crdb
STATUS: deployed
REVISION: 1
NOTES:
CockroachDB can be accessed via port 26257 at the
following DNS name from within your cluster:

crdb-cockroachdb-public.crdb.svc.cluster.local

Because CockroachDB supports the PostgreSQL wire protocol, you can connect to
the cluster using any available PostgreSQL client.

Note that because the cluster is running in secure mode, any client application
that you attempt to connect will either need to have a valid client certificate
or a valid username and password.

Finally, to open up the CockroachDB admin UI, you can port-forward from your
local machine into one of the instances in the cluster:

    kubectl port-forward crdb-cockroachdb-0 8080

Then you can access the admin UI at https://localhost:8080/ in your web browser.

For more information on using CockroachDB, please see the project's docs at:
https://www.cockroachlabs.com/docs/
```

## Upgrade of cockroachdb Cluster

Kick off the upgrade process by changing the new Docker image, where `$new_version` is the CockroachDB version to which you are upgrading:

```shell
helm upgrade my-release cockroachdb/cockroachdb \
--set image.tag=$new_version \
--reuse-values
```

Kubernetes will carry out a safe [rolling upgrade](https://kubernetes.io/docs/tutorials/stateful-application/basic-stateful-set/#updating-statefulsets) of your CockroachDB nodes one-by-one. Monitor the cluster's pods until all have been successfully restarted:

## Migration from Kubernetes Signed Certificates to Self-Signer Certificates

Kubernetes signed certificates is deprecated from the Kubernetes v1.22+ and user will not be able to use this methods for
signing certificates.

User can move from old kubernetes signing certificates by performing following steps:

Run the upgrade command with upgrade strategy set as "onDelete" which only upgrades the pods when deleted by the user.

```shell
helm upgrade crdb-test cockroachdb --set statefulset.updateStrategy.type="OnDelete"
```

While monitor all the pods, once the init-job is created, you can delete all the cockroachdb pods with following command:

```shell
kubectl delete pods -l app.kubernetes.io/component=cockroachdb
```

This will delete all the cockroachdb pods and restart the cluster with new certificates generated by the self-signer utility.
The migration will have some downtime as all the pods are upgraded at the same time instead of rolling update.

## Installation of Helm Chart with Cert Manager

User should have [cert manager >=1.0](https://cert-manager.io/docs/installation/) version installed.

Create a Issuer for signing self-signed CA certificate.

```yaml
apiVersion: cert-manager.io/v1
kind: Issuer
metadata:
  name: cockroachdb
spec:
  selfSigned: {}
```

Now you can enable the cert-manager from `values.yaml` as follows:

```yaml
# Disable the self signing certificates for cockroachdb
tls.certs.selfSigner.enabled: false
# Enable the cert manager
tls.certs.certManager: true
# Provide the kind
tls.certs.certManagerIssuer.kind: Issuer
# Provide the Issuer you have created in previous step
tls.certs.certManagerIssuer.name: cockroachdb
```

```shell
% helm install crdb ./cockroachdb
NAME: crdb
LAST DEPLOYED: Fri Aug  4 14:42:11 2023
NAMESPACE: crdb
STATUS: deployed
REVISION: 1
```
