---
name: configuring-cockroachdb-helm-tls
description: Selects and validates TLS settings for CockroachDB Helm chart deployments, including self-signer, cert-manager, and external certificate modes. Use when a customer needs secure CockroachDB Helm values, certificate secret mapping, cert-manager integration, or TLS install troubleshooting before deploying the chart.
compatibility: CockroachDB Helm v2 charts. Requires Kubernetes Secret or cert-manager access for externally managed certificates. TLS mode must be chosen before initial cluster creation.
metadata:
  author: cockroachdb
  version: "1.1"
---

# Configuring CockroachDB Helm TLS

Guides TLS configuration for operator-managed CockroachDB clusters installed with the Helm v2 charts. The operator API receives `externalCertificates`; the Helm chart is responsible for translating self-signer, cert-manager, or external certificate values into the `CrdbCluster` spec.

## When to Use This Skill

- A customer asks which TLS mode to use with CockroachDB Helm charts
- A values file needs a secure `cockroachdb.tls` block
- The install fails with chart TLS validation errors
- The customer already has cert-manager or externally generated certificates
- The customer needs to understand which Secrets and ConfigMaps the chart expects

## Safety Considerations

- Do not change `cockroachdb.tls.enabled` on a running cluster.
- Enable exactly one of `selfSigner.enabled`, `certManager.enabled`, or `externalCertificates.enabled` when `cockroachdb.tls.enabled=true`.
- Disable all three certificate providers when `cockroachdb.tls.enabled=false`.
- For production, confirm the certificate rotation owner before install. Self-signer can rotate node/client certs, but a customer-provided CA remains the customer's responsibility.
- Do not print private key contents. Only reference Secret names and required keys.
- For certificate rotation or trust failures on a running operator-managed cluster, collect certificate metadata and cert-reloader logs before changing Secrets, ConfigMaps, or cert-manager resources.

## Execution Discipline

- Execute one step at a time and inspect the output before moving on. Certificate mode, Secret names, and issuer state determine which later checks are relevant.
- Do not change TLS mode, replace Secrets, patch cert-manager resources, run debug containers, or perform Helm upgrades unless the user explicitly approves the action for the target cluster.
- Never print private key data. Use metadata checks for expiry, issuer, subject, SANs, and required key presence.
- In production or when certificate ownership is unclear, involve TSE or the operator team before rotation, regeneration, debug containers, or restart actions.

## Step 1: Choose the TLS Mode

| Mode | Use When | Required Inputs |
|---|---|---|
| Self-signer | Fastest secure install, dev/test, or customer accepts chart-managed cert generation | Optional CA Secret if customer provides CA |
| Cert-manager | Customer already runs cert-manager and wants Kubernetes-native renewal | Issuer or ClusterIssuer, CA ConfigMap, node Secret, root client Secret |
| External certificates | Customer PKI owns all certificates and rotation | CA ConfigMap, node Secret, HTTP Secret, root SQL client Secret |
| Insecure | Non-production test only | Explicit user confirmation |

If the user is unsure, default to self-signer for a first secure non-production deployment and recommend cert-manager or external certificates for production environments with existing PKI.

## Self-Signer Values

Chart-managed CA, node certs, and root client certs:

```yaml
cockroachdb:
  tls:
    enabled: true
    selfSigner:
      enabled: true
      rotateCerts: true
    certManager:
      enabled: false
    externalCertificates:
      enabled: false
```

Customer-provided CA with chart-generated node and client certs:

```yaml
cockroachdb:
  tls:
    enabled: true
    selfSigner:
      enabled: true
      caProvided: true
      caSecret: custom-ca-secret
      rotateCerts: true
    certManager:
      enabled: false
    externalCertificates:
      enabled: false
```

The CA Secret must contain `ca.crt` and `ca.key` in the CockroachDB namespace before install.

Validate:

```bash
kubectl -n <namespace> get secret custom-ca-secret
helm template crdb ./cockroachdb-parent/charts/cockroachdb -n <namespace> -f values.yaml >/tmp/crdb-rendered.yaml
```

## Cert-Manager Values

Use cert-manager when an Issuer or ClusterIssuer can issue CockroachDB node and root client certificates.

```yaml
cockroachdb:
  tls:
    enabled: true
    selfSigner:
      enabled: false
    certManager:
      enabled: true
      caConfigMap: cockroachdb-ca
      nodeSecret: cockroachdb-node
      clientRootSecret: cockroachdb-root
      issuer:
        group: cert-manager.io
        kind: Issuer
        name: cockroachdb
    externalCertificates:
      enabled: false
```

Preflight:

```bash
kubectl -n <namespace> get issuer cockroachdb
kubectl -n <namespace> get configmap cockroachdb-ca || true
kubectl -n <namespace> get secret cockroachdb-node cockroachdb-root || true
kubectl get crd certificates.cert-manager.io issuers.cert-manager.io
```

If cert-manager stores CA material in a Secret but the chart needs a ConfigMap, configure trust-manager or another approved process to publish `ca.crt` into the namespace.

## External Certificate Values

Use external certificates when the customer has already generated Kubernetes resources with the names the operator expects:

```yaml
cockroachdb:
  tls:
    enabled: true
    selfSigner:
      enabled: false
    certManager:
      enabled: false
    externalCertificates:
      enabled: true
      certificates:
        caConfigMapName: cockroachdb-ca
        nodeSecretName: cockroachdb-node
        httpSecretName: cockroachdb-node
        rootSqlClientSecretName: cockroachdb-root
```

Expected data keys:

| Resource | Required Keys |
|---|---|
| CA ConfigMap | `ca.crt` |
| Node TLS Secret | `tls.crt`, `tls.key` |
| HTTP TLS Secret | `tls.crt`, `tls.key` |
| Root SQL client Secret | `tls.crt`, `tls.key` or chart-compatible root client cert keys |

Validate names and keys without printing secret values:

```bash
kubectl -n <namespace> get configmap cockroachdb-ca -o jsonpath='{.data.ca\.crt}' >/dev/null
kubectl -n <namespace> get secret cockroachdb-node -o jsonpath='{.data.tls\.crt}' >/dev/null
kubectl -n <namespace> get secret cockroachdb-node -o jsonpath='{.data.tls\.key}' >/dev/null
kubectl -n <namespace> get secret cockroachdb-root -o jsonpath='{.data.tls\.crt}' >/dev/null
kubectl -n <namespace> get secret cockroachdb-root -o jsonpath='{.data.tls\.key}' >/dev/null
```

## Insecure Non-Production Values

Only use for local testing or temporary non-production validation:

```yaml
cockroachdb:
  tls:
    enabled: false
    selfSigner:
      enabled: false
    certManager:
      enabled: false
    externalCertificates:
      enabled: false
```

State clearly that insecure mode has no TLS or authentication protections and is not suitable for production.

## Post-Install Verification

```bash
kubectl -n <namespace> get crdbcluster <release-name> -o yaml | grep -A12 certificates
kubectl -n <namespace> get secret,configmap | grep -E 'cockroach|crdb'
kubectl -n <namespace> get pods
```

For self-signer, confirm the self-signer job ran and the generated CA, node, and client resources exist. For cert-manager, confirm `Certificate` resources are Ready. For external certificates, confirm the `CrdbCluster` references the expected names.

## Certificate Debugging and Rotation Evidence

Use this section when pods report `x509` errors, certificate rotation is not reflected in pods, the `cert-reloader` sidecar fails, or TSC asks for certificate evidence. Collect metadata only; do not print private keys.

```bash
kubectl -n <namespace> get crdbcluster <release-name> -o yaml | grep -A20 certificates
kubectl -n <namespace> get secret,configmap | grep -E 'ca|node|client|tls|cert|cockroach|crdb'
kubectl -n <namespace> get pod <pod-name> -o jsonpath='{.spec.containers[*].name}{"\n"}'
kubectl -n <namespace> logs <pod-name> -c cert-reloader --tail=100
```

Inspect the node certificate:

```bash
kubectl -n <namespace> get secret <node-tls-secret> -o jsonpath='{.data.tls\.crt}' | base64 -d | \
  openssl x509 -noout -dates -subject -issuer -ext subjectAltName
```

Inspect cert-manager resources, if cert-manager is used:

```bash
kubectl -n <namespace> get certificate,issuer -o wide
kubectl get clusterissuer -o wide 2>/dev/null || true
kubectl -n <namespace> describe certificate <certificate-name>
kubectl -n <namespace> describe issuer <issuer-name>
```

If the CockroachDB image does not include network or OpenSSL tooling, use an approved debug image according to the customer's policy. In air-gapped environments, mirror the approved image into the customer's registry and use that registry path instead of pulling a public image directly.

```bash
kubectl -n <namespace> debug <pod-name> --image=<approved-network-debug-image> --target=cockroachdb -it -- \
  openssl s_client -connect <pod-ip>:26257 \
    -CAfile /cockroach/cockroach-certs/ca.crt \
    -cert /cockroach/cockroach-certs/node.crt \
    -key /cockroach/cockroach-certs/node.key
```

For full in-pod certificate metadata:

```bash
kubectl -n <namespace> debug <pod-name> --image=<approved-network-debug-image> --target=cockroachdb -it -- bash -c '
for DIR in /cockroach/cockroach-certs /certs /cockroach-certs; do
  if [ -d "$DIR" ]; then
    echo "=== Cert directory: $DIR ==="
    ls -la "$DIR" 2>/dev/null
    for CERT in "$DIR"/*.crt; do
      if [ -f "$CERT" ]; then
        echo "--- $CERT ---"
        openssl x509 -in "$CERT" -noout -subject -issuer -dates -ext subjectAltName 2>&1
      fi
    done
  fi
done'
```

Escalate with [collecting-cockroachdb-operator-escalation-packet](../../cockroachdb-observability-and-diagnostics/collecting-cockroachdb-operator-escalation-packet/SKILL.md) if certificates look correct but pods still cannot join, rotate, or become Ready.

## Common TLS Failures

| Symptom | Likely Cause | Action |
|---|---|---|
| `Exactly one of selfSigner, certManager or externalCertificates must be enabled when TLS is on` | Multiple or zero providers enabled | Set exactly one provider true |
| `selfSigner, certManager and externalCertificates must all be disabled when TLS is off` | Provider enabled while TLS disabled | Disable all providers or enable TLS |
| `caProvided` with empty `caSecret` | Customer CA mode missing Secret name | Set `cockroachdb.tls.selfSigner.caSecret` |
| Pods fail with certificate trust errors | CA and issued certs do not match | Verify CA ConfigMap/Secret and regenerate certs from the same CA |
| Cert rotation does not take effect | cert-reloader sidecar issue, stale mounted Secret, or cert-manager failure | Check cert-reloader logs, cert-manager `Certificate` status, and node cert expiry/SAN metadata |
| Pods fail after migration with TLS errors | Migrated cert resources or CA names do not match operator references | Use the migration debugging skill and verify the `CrdbCluster` certificate references |

## References

- [Self-signer certificate management](../../../docs/certificate-management/self-signer.md)
- [cert-manager certificate management](../../../docs/certificate-management/cert-manager.md)
- [CockroachDB Helm Chart README](../../../cockroachdb-parent/charts/cockroachdb/README.md)
- [CockroachDB Docs: Authentication](https://www.cockroachlabs.com/docs/stable/authentication.html)
- [Operator escalation packet collection](../../cockroachdb-observability-and-diagnostics/collecting-cockroachdb-operator-escalation-packet/SKILL.md)
