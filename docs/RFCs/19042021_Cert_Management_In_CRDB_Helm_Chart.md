Feature Name: Cert Management in CockroachDB Helm Chart
 
Status: Draft
 
Start Date: 19-04-2021
 
Authors: @prafull01, @abhishekdwivedi3060, @madhurnawandar
 
# Summary
 
This RFC proposes a method of deploying the CockroachDB Helm chart in a secure mode by generating certificates without using Kubernetes CA.  
The new method allows users to specify their own CA or if not provided will generate a self-generated CA.  
It handles the creation of Node and Client certificates using the CA.  
It allows rotation of CA, Node, and Client certificates.  
It allows specifying a duration for the generated certificates
 
# Motivation
 
Currently, CockroachDB Helm chart supports 3 ways of cert management
* Built-in CSR's in Kubernetes: Depends on the user to approve CSR for all the generated certificates manually. CSR is no longer support with Certificates.k8s.io/v1 API and will be deprecated in Kubernetes v1.22. Many Kubernetes distributions like VMware Tanzu, EKS, etc have deprecated use of Kubernetes CA.
* Cert-manager: This is the most efficient way of managing certificates, but not everyone uses cert-manager to manage the certificates. Also using cert-manager may be overkill for dev/test environments
* Manual: The user has to generate the certificates and provide them in the form of secrets. This method puts the overhead of certificate management, leading to multiple manual steps for the user
 
CockroachDB user needs a default mechanism of cert management which should work on all the k8s distributions without the need for manual intervention. While cert-manager fits into the requirement, it makes CockroachDB deployment dependent on external application. This new method of cert-management satisfies the user requirement without the need for 3rd party software like cert-manager.
 
## Goals
 
* Helm install command should be self-sufficient to launch the CockroachDB cluster in a secure mode.
 
* Dependency on external cert-manager should not be mandatory for creating CockroachDB cluster in a secure mode.
 
* Manual steps should not be required for creating the CockroachDB cluster in a secure mode.

* Deprecation plan for the CA signing method using kubernetes CA.
  
## Non-Goals
 
* This RFC does not intend to fix the issues in the current default method of using Kubernetes CA
 
## Helm Configuration
This section specifies the suggested changes around user input in the Helm chart
 
1. Add option specifying CockroachDB to manage the certificates, `tls.certs.selfSigner.enabled` as true/false.  
   Enabling this option will result in CockroachDB creating Node and Client certificates using a CA(either generated or provided by the user).
 
2. Add option specifying CockroachDB to use user provided CA, `tls.certs.selfSigner.caProvided` as true/false.  
   Enabling this option will result in the generation of Node and Client certificates using the CA provided by the user.
 
3. Add option specifying the secret name containing user provide CA, `tls.certs.selfSigner.caSecret`.  
   The secret name specified in this option will be used as a source for user-provided CA.
   This option is mandatory if the `tls.certs.selfSigner.caProvided` is true.
 
4. Add option specifying the minimum certificate duration, `tls.certs.selfSigner.minimumCertDuration`
   This duration will be used to validate all other certificate durations, which must be greater than this duration.
   If this input is not provide by user,it will be derived from client/node cert duration and expiry.
   
5. Add option specifying the CA certificate duration, `tls.certs.selfSigner.caCertDuration`.  
   This duration will only be used when we create our own CA. The duration value from this option will be used to set the expiry of the generated CA certificate.
   By default, the CA expiry would be set to 10 years.
 
6. Add option specifying the CA certificate expiry window, `tls.certs.selfSigner.caCertExpiryWindow`
   This duration will be used to rotate the CA certs before its actual expiry.    

7. Add option specifying the Client certificate duration, `tls.certs.selfSigner.clientCertDuration`.  
   The duration value from this option will be used to set the expiry of the generated Client certificates. By default, Client certificate expiry would be set to 1 year.
 
8. Add option specifying the Client certificate expiry window, `tls.certs.selfSigner.clientCertExpiryWindow`
   This duration will be used to rotate the Client certs before its actual expiry.
   
9. Add option specifying the Node certificate duration, `tls.certs.selfSigner.nodeCertDuration`.  
   The duration value from this option will be used to set the expiry of the generated Node certificates. By default, Node certificate expiry would be set to 1 year.
 
10. Add option specifying the Node certificate expiry window, `tls.certs.selfSigner.nodeCertExpiryWindow`
    This duration will be used to rotate the Node certs before its actual expiry.
    
11. Add option specifying CockroachDB to manage rotation of the generated certificates, `tls.certs.selfSigner.rotateCerts` as true/false.  
    Enabling this option will result in auto-rotation of the certificates, before the expiry.
 
## Helm Input Validation
 
1. If `tls.certs.selfSigner.caProvided` is set to true, then value for `tls.certs.selfSigner.caSecret` must be provided.
 
2. If value for `tls.certs.selfSigner.caSecret` is provided, secret should exist in the CockroachDB install namespace.

3. If value for `tls.certs.selfSigner.minimumCertDuration` is not provided, it will derive from following:
   ```
   tls.certs.selfSigner.minimumCertDuration = Min((tls.certs.selfSigner.clientCertDuration - tls.certs.selfSigner.clientCertExpiryWindow), 
   (tls.certs.selfSigner.nodeCertDuration - tls.certs.selfSigner.nodeCertExpiryWindow))
   ```

4. Value for `tls.certs.selfSigner.caCertExpiryWindow` should be greater than `tls.certs.selfSigner.minimumCertDuration`
 
5. Value for `tls.certs.selfSigner.caCertDuration` - `tls.certs.selfSigner.caCertExpiryWindow` should be greater than `tls.certs.selfSigner.minimumCertDuration` 
   `tls.certs.selfSigner.clientCertDuration` - `tls.certs.selfSigner.clientCertExpiryWindow` and `tls.certs.selfSigner.nodeCertDuration` - `tls.certs.selfSigner.nodeCertExpiryWindow` 
   should also be greater than `tls.certs.selfSigner.minimumCertDuration`.
   
## Implementation Details
 
### Helm Components:
 
When `tls.certs.selfSigner.enabled` is set to `true`, the following components are created for certificate generation and rotation:
 1. Certificate Management Service as a `pre-install` job.
 2. ServiceAccount, for `pre-install` job. (deleted after pre-install hook succeeds)
 3. Role, for adding a role to perform an operation on the secret resource. (deleted after pre-install hook succeeds)
 4. RoleBinding, for assigning permission to ServiceAccount for secret-related operation. (deleted after pre-install hook succeeds)
 5. Cron-job, for certificate rotation.  
 
### Helm Flow:   
* A `pre-install` [chart hook](https://helm.sh/docs/topics/charts_hooks/) will be used to create a job for the Certificate Management Service, that runs before all the Helm chart resources are installed.
  * This job will only run when `tls.certs.selfSigner.enabled` is set to `true`.
  * This job will take care of generating all the required certificates.
  * Along with the `pre-install` hook job, serviceAccount, role, and roleBinding will also be created as part of `pre-install` hooks with different `hook-weight` so that the `pre-install`
    job has sufficient permissions to perform certificate generation.
 
   | Resource          | Hook-weight   | Order of Installation     |
   |----------------   |-------------  |-----------------------    |
   | ServiceAccount    | 1             | 1st                       |
   | Role              | 2             | 2nd                       |
   | RoleBindings      | 3             | 3rd                       |
   | Job               | 4             | 4th                       |
 
 * After all the `pre-install` hooks completed successfully, only the job will be deleted. The other resources will be required to rotate the certificates by cronjob. 
 
 * All the certificate generation-related info will be passed on to the `pre-install` job as env variables.
    ```yaml
    env:
       - name: CA_CERT_DURATION
         value: {{ default 3650 .Values.tls.certs.selfSigner.caCertDuration}}
       - name: NODE_CERT_DURATION
         value: {{ default 365 .Values.tls.certs.selfSigner.nodeCertDuration}}
       - name: Client_CERT_DURATION
         value: {{ default 365 .Values.tls.certs.selfSigner.clientCertDuration}}
       {{- if and (tls.certs.selfSigner.caProvided .Values.tls.certs.selfSigner.caSecret) }}
           {{- if not (lookup "v1" "Secret" ".Release.Namespace" ".Values.tls.certs.selfSigner.caSecret")}}
           {{ fail "CA secret doesn't exist in cluster"}}
           {{- end }}
       - name: CA_SECRET
         values:
       {{- end }}
    ```
 
* 3 empty secret will be created in Helm chart for `cockroachdb-ca`, `cockroachdb-node` and `cockroachdb-root` if `tls.certs.selfSigner.enabled`
    is set.
  * Data to these secrets will be populated in the `pre-install` job.
  * In case CA is provided by the user, then `cockroachdb-ca` secret is skipped.
  * Annotation is set on all the secrets created by CockroachDB; eg: `managed-by: cockroachdb`
 
* Two cronjob will be created in Helm chart when `tls.certs.selfSigner.rotateCerts` is set.
  * These cronjobs will run periodically to rotate the certificates. One will rotate node and client certificates and another one rotate CA certificate.
  * The schedule of the node and client rotation cronjob will be `tls.certs.selfSigner.minimumCertDuration`.
  * The schedule of the CA rotation cronjonb will be `tls.certs.selfSigner.caCertDuration` - `tls.certs.selfSigner.caCertExpiryWindow`  
  * On every scheduled run, cronjobs will check if there is any certificate that is going to expire before the next scheduled run,
    if yes then it will renew the certificates.
  
  * <b>The cronjob will use the same `pre-install` job image for certificate rotations. The `pre-install` job image binary will have an argument `--rotate` for handling certificate rotation.</b>
 
* The Stateful pod will be changed to only run `copy-certs` initContainer to copy the certificates from Node secret to emptyDir volume.
  The rest of the main DB container flow will remain the same.
* The post-install job will be changed to only run `copy-certs` initContainer to copy the certificates from Client secret to emptyDir volume.
  The rest of the main cluster-init container flow will remain the same.
 
### Certificate Management Service Implementation

### Overall Flow
 
* Check if CA secret is empty
  * if yes, [Generate-CA](#generate-ca), [Generate-Node-Cert](#generate-node-cert) and [Generate-Client-Cert](#generate-client-cert); return
  * if not, [Validate Secret Annotations for CA](#validate-secret-annotations)
    * if valid, CA is intact
    * if not, [Annotate-CA](#annotate-ca), [Generate-Node-Cert](#generate-node-cert) and [Generate-Client-Cert](#generate-client-cert); return
* Check if [CA requires rotation](#check-cert-for-regeneration)
   * if yes, follow [Rotate-CA](#rotate-ca); return
* Check if Node or Client certificates [needs to be regenerated](#check-cert-for-regeneration):
   * if yes, follow [Generate-Node-Cert](#generate-node-cert) and [Generate-Client-Cert](#generate-client-cert)
 
 
 
#### Generate-CA
* A self-signed CA will be generated
* Expiry of the certificate will be driven by the Helm value for `tls.certs.selfSigner.caCertDuration`, passed as env variable
* Contents of the CA certificate will be stored in the default CA secret `cockroachdb-ca`.
* An annotation `managed-by: cockroachdb` will be added on secret
* Follow [CA Annotation Workflow](#annotate-ca)
 
#### Generate-Node-Cert
* A Node certificate will be generated signed by the generated CA or user custom CA.
* Expiry of the certificate will be driven by the Helm value for `tls.certs.selfSigner.nodeCertDuration`, passed as env variable
* Contents of the Node certificate will be stored in the Node secret `cockroachdb-node`.
* An annotation `managed-by: cockroachdb` will be added on secret
* Node secret will be patched with annotation `resourceVersion` with the value of its current resourceVersion
* Node secret will be patched with annotation `creationTime` and `duration` with current UTC time and value from `tls.certs.selfSigner.nodeCertDuration`
 
#### Generate-Client-Cert
* A Client certificate will be generated by signing it with the generated CA.
* Expiry of the certificate will be driven by the Helm value for `tls.certs.selfSigner.clientCertDuration`, passed as env variable
* Contents of the Client certificate will be stored in the Client secret `cockroachdb-root`
* An annotation `managed-by: cockroachdb` will be added on secret
* Client secret will be patched with annotation `resourceVersion` with the value of its current resourceVersion
* Client secret will be patched with annotation `creationTime` and `duration` with current UTC time and value from `tls.certs.selfSigner.clientCertDuration`

#### Generate-Certs-for-additional-users
* A helm plugin will be provided for generating any user's certificate apart from the root user certificate.
* The helm plugin command will look like: 
```
helm crdb create-certs --user=<required> --user-secret=<optional> --release=<cockroachDB release required> --namespace=<optional> 
--duration=<defaults to root cert> --expiry=<defaults to root cert>
```
* This command will create a job which will create the user provided by the user, according to the inputs provided.
* The rotation of its certificate will be handled in the cronjob created for node and client certificates.
 
#### Annotate-CA
* CA secret will be patched with annotation `resourceVersion` with the value of its current resourceVersion
* CA secret will be patched with annotation `creationTime` and `duration` with current UTC time and value from `tls.certs.selfSigner.caCertDuration`
* Now that CA is annotated with required info, it will be followed by [Node cert generation workflow](#generate-node-cert)
 
#### Rotate-CA
* CA rotation will follow the same workflow as [CA Generation Workflow](#generate-ca), except the new CA is a combined CA certificate that contains 
  the new CA certificate followed by the old CA certificate.
* This new combined CA certificate will be updated in the CA secret and all the Node and Client secret.
  Follow [Node restart after certificate generation](#node-restart-after-cert-rotation)
* In addition, Client secret and Node secret will be patched with annotation `needsRegeneration: True`, which specifies that their certificates
  need to be regenerated in the next cronjob run. This is done in accordance with the suggestion in CockroachDB [doc](https://www.cockroachlabs.com/docs/v20.2/rotate-certificates.html#why-rotate-ca-certificates-in-advance)
 
#### Check-node-cert-for-regeneration
* Follow checks from [Check cert for regeneration](#check-cert-for-regeneration)
* In addition, check if annotation `needsRegeneration: True` exists, if yes return True (This will be case when CA certificate has rotated, but Client and node certs are still to be recreated)
 
#### Check-cert-for-regeneration
* Check if the certificate secret has annotation `managed-by: cockroachdb`, if not return False
* Check if the certificate is expiring before the next cron schedule using the values from annotations `creationTime` and `duration` on secret, if expiring return True, else False
 
#### Validate-Secret-Annotations
* Check if annotation for `resourceVersion` and `creationTime` exists, if not return False
* Check if resourceVersion of the secret matches with the value of `resourceVersion` annotation, if not means secret is changed, return False
* Else, return True
 
#### Node-restart-after-cert-rotation
This could be done in either of the two ways mentioned:
1. Trigger a rolling restart of the nodes so that the new certs are consumed.
2. Trigger a SIGHUP signal using a sidecar container, which will restart the cockroachDB process without restarting entire node.


### Certificate Generation cases during Helm upgrade:
 
In case of Helm upgrade:
 
* User has given CA and changes contents of the CA secret:
  * Check if the current value `resourceVersion` or hash matches with the annotation value. if annotation does not match, so this is a new CA scenario.
* User has given CA and changes the secret name:
  * Annotation will not be found, so this is a new CA scenario.
* User had not given CA previously, but now has given the CA:
  * Annotation will not be found, so this is a new CA scenario
* If the user changes the duration of CA:
  * Identify and compare using existing annotation on CA secret and current value, this will be a case for certificate rotation. This will only be the case when the CA is managed by CockroachDB.
* User changes the duration of all certificates:
  * Compare old and new CA duration from CA secret annotation values and current value. This will be a case for certificate rotation.
  * Rotate CA certificate and add an annotation on CA secret with the date of rotation.
  * Add annotations on Node and Client certs specifying the new expected duration and `to-be-rotated: true`.
  * These secret certificates will be renewed in the next cron cycle and `to-be-rotated:true` and expected duration annotations will be removed.
* User only changes the duration of either Node or Client certificate:
  * Identify and compare duration with existing annotation value and current value and renew Node or Client certificate.
* User certificate management method is changed from certificate generation to `cert-manager` or `default manual k8s CSR approval`:
  * Do nothing as this `pre-install` job  won't be triggered.
 
### Periodic Rotation scenarios:
* CA certificate is near expiry: This will be identified using the generation time put on the CA secret. This will lead to a certificate rotation scenario.
* Node or Client certificate near expiry: This will be identified using the generation time put on the respective secret. This will lead to regeneration of Node or Client certificate
 
### CA Certificate Rotation scenario:
 
* Only renew CA certificate by generating new CA key and new combined CA along with the old CA.
* Update CA secret and add CA certificate in Node and Client secret.
* Follow [Node restart after certificate generation](#node-restart-after-cert-rotation)
* Add annotation on CA secret with the date of rotation.
* Add annotations on Node and Client certs `to-be-rotated: true`.
* Do not process Node and Client certificate.
* On the next scheduled iteration, if `to-be-rotated: true` annotation found, then renew Node or Client certificate.
* Remove `to-be-rotated: true` from Node secret and Client secret.
* Follow [Node restart after certificate generation](#node-restart-after-cert-rotation)
