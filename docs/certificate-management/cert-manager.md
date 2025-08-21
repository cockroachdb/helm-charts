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
$ helm install crdb ./cockroachdb

NAME: crdb
LAST DEPLOYED: Fri Aug  4 14:42:11 2023
NAMESPACE: crdb
STATUS: deployed
REVISION: 1
```

## Installation of CockroachDB Operator with Cert Manager

If you wish to provision certificates using [cert-manager][1], follow the steps below:

  * By default, cert-manager stores the CA certificate in a Secret, which is used by the Issuer.

  * To provide the CA certificate in a ConfigMap (required by some applications like CockroachDB), you can use the [trust-manager][2] project.

  * The trust-manager can be configured to copy the CA cert from a Secret to a ConfigMap automatically.

  * If your CA Secret is in the cockroachdb namespace, your trust-manager deployment must also reference that namespace. You can set the trust namespace using the Helm value: --set app.trust.namespace=cockroachdb. Follow the [docs](https://cert-manager.io/docs/trust/trust-manager/installation/) to know more about it.

Example Setup:

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: cockroachdb-ca
  namespace: cockroachdb
data:
  tls.crt: [BASE64 Encoded ca.crt]
  tls.key: [BASE64 Encoded ca.key]
type: kubernetes.io/tls
---
apiVersion: cert-manager.io/v1alpha3
kind: Issuer
metadata:
  name: cockroachdb
  namespace: cockroachdb
spec:
  ca:
    secretName: cockroachdb-ca
---
apiVersion: trust.cert-manager.io/v1alpha1
kind: Bundle
metadata:
  name: cockroachdb-ca
spec:
  sources:
    - secret:
        name: cockroachdb-ca
        key: tls.crt
  target:
    configMap:
      key: ca.crt
    namespaceSelector:
      matchLabels:
       kubernetes.io/metadata.name: cockroachdb
```
> 🔍 Bundle will create a ConfigMap named cockroach-ca in the cockroachdb namespace with a ca.crt key copied from the Secret's tls.crt.

🔧 **values.yaml configuration** :

To enable cert-manager integration via Helm, configure the following:

```yaml
cockroachdb:
  tls:
    enabled: true
    selfSigner:
      enabled: false
    certManager:
      enabled: true
      # caSecret defines the secret name that contains the CA certificate.
      caConfigMap: cockroachdb-ca
      # nodeSecret defines the secret name that contains the node certificate.
      nodeSecret: cockroachdb-node
      # clientRootSecret defines the secret name that contains the root client certificate.
      clientRootSecret: cockroachdb-root
      # issuer specifies the Issuer or ClusterIssuer resource to use for issuing node and client certificates.
      # The values correspond to the issuerRef in the certificate.
      issuer:
        # group specifies the API group of the Issuer resource.
        group: cert-manager.io
        # kind specifies the kind of the Issuer resource.
        kind: Issuer
        # name specifies the name of the Issuer resource.
        name: cockroachdb
        # clientCertDuration specifies the duration of client certificates.
        clientCertDuration: 672h
        # clientCertExpiryWindow specifies the rotation window before client certificate expiry.
        clientCertExpiryWindow: 48h
        # nodeCertDuration specifies the duration for node certificates.
        nodeCertDuration: 8760h
        # nodeCertExpiryWindow specifies the rotation window before node certificate expiry.
        nodeCertExpiryWindow: 168h
```

[1]: https://cert-manager.io/
[2]: https://cert-manager.io/docs/trust/trust-manager/