Feature Name: Add a method to generate certificates without using kubernetes CA

Status: Draft

Start Date: 19-04-2021

Authors: @prafull01, @abhishek, @madhurnawandar

# Summary

This RFC proposes a method of deploying the cockroach db helm chart in a secure mode by generating certificates without using kubernetes CA.  
The new method will allow users to specify their own CA otherwise CA will be generated as part of this method. Using the CA, node and client certificates will be generated. Rotation of CA, node and clinet certificates is also considered.  
This manual steps for any user could be error prone and might discourage users to run cockroach db in secure mode for test environments.  
Also, this RFC eliminates the need of kubernetes CA to sign our certificates. We will sign our own certificates and manage those certificates.

# Motivation

Currently cocroachdb support 3 ways of cert management
* Built-in CSR's in kubernetes: Depends on user to approve CSR for all the generated certificates manually. CSR is no longer support with Certificates.k8s.io/v1 API and will be deprecated in kubernetes v1.22. Many kubernetes distribution like VMware Tanzu, EKS etc are not allowing kubernetes CA to sign the CSR's using kubernetes CA. 
* Cert-manager: This is the most efficient way of managing certificates, but not everyone uses cert-manager to manage the certificates. Also using cert-manager may be an overkill for dev/test environments
* Manual: User has to generate the certificates and provide it in the form of secrets. This method puts the overhead of certificate management leading to multiple manual steps for the user

Cocroachdb user needs a default mechanism of cert management which should work on all the k8s distributions without the need for manual intervention. While cert-manager fits into the requirement, it makes it mandatory for the user to use a cert-manager. This new method of cert-management satisfies the user requirement without the need for 3rd party software like cert-manager. 

## Goals

* Helm install command should be self-sufficient to launch the cockroach db cluster in secure mode.

* Dependency on external cert-manager should not be mandatory for creating cockroach db cluster in secure mode.

* Manual steps should not be required for creating cockroach db cluster in secure mode.
    
## Non-Goals

* This RFC does not intend to fix the issues in the current default method of using kubernetes CA

## Helm Configuration
This section specifies the suggested changes around user input in helm chart

1. Add option specifying cockroach db to manage the certificates, `tls.certs.generate.enabled` as true/false.  
   Enabling this option will result into cockroach db creating node and client certificates using a CA(either generated or provided by user). 

2. Add option specifying cockroach db to use user provided CA, `tls.certs.generate.caProvided` as true/false.  
   Enabling this option will result into generation of node and client certificates using the CA provided by the user.
   
3. Add option specifying the secret name containing user provide CA, `tls.certs.generate.caSecret`.  
   The secret name specified in this option will be used as a source for user provided CA.
   This option is mandatory if the `tls.certs.generate.caProvided` is true.
   
4. Add option specifying the CA certificate expiration duration, `tls.certs.generate.caCertDuration`.  
   This duration will only be used when we create our own CA. The duration value from this option will be used to set the expiry of the generated CA certificate.
   By default, the CA expiry would be set to 10 years.
   
5. Add option specifying the client certificate expiration duration, `tls.certs.generate.clientCertDuration`.  
   The duration value from this option will be used to set the expiry of the generated client certificates. By default, client certificate expiry would be set to 1 year.

6. Add option specifying the node certificate expiration duration, `tls.certs.generate.nodeCertDuration`.  
   The duration value from this option will be used to set the expiry of the generated node certificates. By default, node certificate expiry would be set to 1 year.
   
7. Add option specifying cocroach db to manage rotation of the generated certificates, `tls.certs.generate.rotateCerts` as true/false.  
   Enabling this option will result into auto-rotation of the certificates, before the expiry.

## Helm Input Validation

1. If `tls.certs.generate.caProvided` is set to true, then value for `tls.certs.generate.caSecret` must be provided.
   
2. If value for `tls.certs.generate.caSecret` is provided, secret should exist in the cockroach db install namespace.
   
3. Value for `tls.certs.generate.caCertDuration` should be greater than the value for `tls.certs.generate.clientCertDuration` and `tls.certs.generate.nodeCertDuration`

## Implementation Details 

## Helm Flow:

* A `pre-install` [chart hook](https://helm.sh/docs/topics/charts_hooks/) will be used to create a job that runs before all the helm chart resources are installed. 
  * This job will only run when `tls.certs.generate.enabled` is set to `true`.
  * This job will take care of generating all the required certificates.
  * Along with the `pre-install` hook job, serviceAccount, role and rolebinding will also be created as part of `pre-install` hooks with different `hook-weight` so that the `pre-install`
  job have sufficient permissions to perform certificate generation.

    | Resource       	| Hook-weight 	| Order of Installation 	|
    |----------------	|-------------	|-----------------------	|
    | ServiceAccount 	| 1           	| 1st                   	|
    | Role           	| 2           	| 2nd                   	|
    | RoleBindings   	| 3           	| 3rd                   	|
    | Job            	| 4           	| 4th                   	|

  * After all the `pre-install` hooks completed successfully, they will be deleted by hook deletion-policy defined in
  annotations.

  * This `pre-install` job will have two work-flows depending upon the value of `tls.certs.generate.caProvided` 
      - When CA is self-signed i.e. `tls.certs.generate.caProvided: false`:
        
        CA will be generated by cockroach db and is saved in the CA secret. Name of the secret will be `cockroachdb-ca`.
        Then this CA will be is used to generate nodeSecret and clientRootSecret certs and then saved in `cockroachdb-node` and 
        `cockroachdb-root` secret respectively.
      - When CA is given by the user i.e. `tls.certs.generate.caProvided: true`:
        
        User given CA secret will be used to get the CA information and sign the nodeSecret and clientRootSecret certificates
        and save them in `cockroachdb-node` and`cockroachdb-root` secret.
        
  * For all the generated certificates, their duration will be driven by the duration value set in the values.yaml.
    - Generated CA certificate life duration, default 10 years: `tls.certs.generate.caCertDuration`
    - Generated Node certificate life duration, default 1 year: ``tls.certs.generate.nodeCertDuration``
    - Generate Client certificate life duration, default 1 year: `tls.certs.generate.clientCertDuration`
  
  * All the certificate generation related info will be passed on to the `pre-install` job as env variables.
     ```yaml
     env:
        - name: CA_CERT_DURATION
          value: {{ default 3650 .Values.tls.certs.generate.caCertDuration}}
        - name: NODE_CERT_DURATION
          value: {{ default 365 .Values.tls.certs.generate.nodeCertDuration}}
        - name: Client_CERT_DURATION
          value: {{ default 365 .Values.tls.certs.generate.clientCertDuration}}
        {{- if and (tls.certs.generate.caProvided .Values.tls.certs.generate.caSecret) }}
            {{- if not (lookup "v1" "Secret" ".Release.Namespace" ".Values.tls.certs.generate.caSecret")}}
            {{ fail "CA secret doesn't exist in cluster"}}
            {{- end }}
        - name: CA_SECRET
          values: 
        {{- end }}
     ```

* 3 empty secret will be created in helm chart for `cockroachdb-ca`, `cockroachdb-node` and `cockroachdb-root` if `tls.certs.generate.enabled`
  is set.
  * Data to these secrets will be populated in the `pre-install` job.
  * In case CA is provided by the user, then `cockroachdb-ca` secret is skipped.
  * Empty secrets are added to allow proper cleanup during helm uninstall.

* A cron-job will be created in helm chart when `tls.certs.generate.rotateCerts` is set.
  * This cron-job will run periodically to rotate the certificates.
  * The schedule of the cron-job will be the minimum of `nodeCertDuration` and `clientCertDuration`, minus a week.
  * On every schedule run, it will
  check if there is any certificate which is going to expire before the next scheduled run, if yes then it will renew the certificates.
  
  * If the CA is created by cockroach db:
    * check the CA certificate expiry and if the expiry is less than the next cronjob schedule,then do the certificate rotation.
    * If CA is rotated, then the node certificates and client certificates need to be rotated.

    `TODO: Discuss about the 2 crons, one for CA rotation few months prior to Node cert rotation`
  * If the CA is provided by user:
    * CA cert rotation is not considered at all.
    * Check the for expiry of node certificates and client certificates. If certificate expiry
    is less than the next scheduled run, then do cert rotation.
    
  * <b>The cron-job will use the same `pre-install` job image for certificate rotations. The `pre-install` job image binary will
  have an argument `--rotate` for handling certificate rotation.</b>

`TODO:Need to identify how to generate the SIGHUP signal in all the nodes for certificate renewal`

* The Stateful pod will need to change to only run `copy-certs` initContainer to copy the certificates from nodeSecret to emptyDir volume.
  Rest of the main db container flow will remain the same.
  
- Right now client certificate is generated in the `post-install` job. In case of `tls.certs.generate` set to true, it will be
  generated in `pre-install` job only. So this `post-install` job also will run `copy-certs` initContainer to copy the certificates
  from clientRootSecret to emptyDir volume. Rest of the main cluster-init container flow will remain the same

## Certificate Generation cases during helm upgrade:

In case of helm upgrade:
* if certificate management method is changed from cert generation to `cert-manager` or `default manual k8s CSR approval`,
  do nothing as this `pre-install` job  won't be triggered.
  
* if same cert management method is used, then `pre-install` job will check:
  * When `caCertDuration`, `nodeCertDuration` or `clientCertDuration` is changed: 
    * if any of the certificate duration is decreased:
      * TODO: What to do when any of the cert duration is decreased compared to last value
    * if `caCertDuration` is increased:
      * generate new CA. Sign nodeCert and clientCert with the new CA and trigger refresh command.
    * if `nodeCertDuration` is increased:
      * generate  new nodeCert and trigger refresh command
    * if `clientCertDuration`is increased:
      * generate new clientCert.
    
  * When value of `caProvided` is changed:
    * if certificate management method is changed from cert generation to `custom user CA`, generate new nodeCert and clientCert
  singed by custom user CA and trigger refresh CA.

`TODO: Do we need to check below conditions if same cert generation method is used and duration is also?
     I think CA secret, nodeSecret and clientSecret are already populated with certificate info, and no need to update
    cert. Discuss` 
        if CA secret has data:
            if not generate CA
            if yes then do nothing
        if node cert is empty:
            if yes generate node cert using CA
            if no, validate node cert using CA
                if valid, do nothing
                if not valid, generate new node certificates and follow node cert rotation process
