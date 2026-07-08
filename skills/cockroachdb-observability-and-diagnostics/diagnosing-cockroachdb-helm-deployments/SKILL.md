---
name: diagnosing-cockroachdb-helm-deployments
description: Diagnoses failed or unhealthy CockroachDB Helm chart deployments by checking Helm release state, operator health, CrdbCluster and CrdbNode status, pod readiness, RBAC, webhooks, TLS, upgrades, scaling, PVCs, DNS, and multi-region assumptions. Use when Helm install or upgrade fails, pods are not Ready, or the operator is not reconciling.
compatibility: CockroachDB Helm v2 charts and operator-managed crdb.cockroachlabs.com/v1beta1 resources. Requires Kubernetes read access to the operator namespace and CockroachDB namespace; some remediation requires cluster-admin or platform-team action.
metadata:
  author: cockroachdb
  version: "1.1"
---

# Diagnosing CockroachDB Helm Deployments

Diagnoses CockroachDB Helm install, upgrade, and readiness failures for operator-managed clusters. Keep the flow customer-facing: collect read-only evidence, classify the failure, and propose the smallest safe remediation. If the issue needs TSC escalation or deep operator forensics, use [collecting-cockroachdb-operator-escalation-packet](../collecting-cockroachdb-operator-escalation-packet/SKILL.md).

## When to Use This Skill

- `helm install` or `helm upgrade` fails
- `CrdbCluster.status.observedGeneration` is behind `metadata.generation`
- `CrdbCluster.status.reconciled` is false or missing
- CockroachDB pods are Pending, Init, CrashLoopBackOff, Running but not Ready, or stuck on an old image
- The operator Deployment is unavailable, silent, or not reconciling
- Errors mention RBAC, CRDs, TLS, certificates, webhooks, node locality, PVCs, DNS, upgrades, scale operations, decommissioning, or multi-region networking

## Related Skills

- Use [collecting-cockroachdb-operator-escalation-packet](../collecting-cockroachdb-operator-escalation-packet/SKILL.md) when the customer needs to escalate or when TSC asks for a complete data bundle.
- Use [debugging-cockroachdb-operator-migrations](../../cockroachdb-onboarding-and-migrations/debugging-cockroachdb-operator-migrations/SKILL.md) for Helm StatefulSet or public operator migration problems.
- Use [configuring-cockroachdb-helm-tls](../../cockroachdb-operations-and-lifecycle/configuring-cockroachdb-helm-tls/SKILL.md) for TLS provider selection and certificate validation.
- Use [validating-cockroachdb-helm-multiregion](../../cockroachdb-onboarding-and-migrations/validating-cockroachdb-helm-multiregion/SKILL.md) for cross-region DNS, networking, and certificate checks.

## Inputs

- Exact failed command and stderr
- Operator namespace, operator Helm release name, and operator chart version
- CockroachDB namespace, CockroachDB Helm release name, and CockroachDB chart version
- Values file or `--set` values used
- Kubernetes context and Kubernetes version
- CockroachDB image/version and operator image/version
- Whether this is install, upgrade, scale up/down, certificate rotation, migration, maintenance, or recovery
- What the customer already tried, in order

## Safety Considerations

- Collect evidence before retrying. Repeated Helm or kubectl attempts can obscure the original failure.
- Do not delete PVCs, Secrets, `CrdbCluster`, or `CrdbNode` resources unless the user explicitly asks for teardown and understands data impact.
- Do not manually edit `CrdbCluster.status`; the operator owns status.
- Do not run `cockroach init` on an existing cluster.
- Do not change service settings such as `publishNotReadyAddresses` without operator-team guidance.
- Do not delete version checker jobs or pods during an upgrade until their status and logs are collected.
- Do not restart or scale down the operator during an active rollout, decommission, or migration unless basic evidence has already been collected.
- Do not uninstall the operator before checking whether it manages other namespaces or clusters.

## Execution Discipline

- Execute one step at a time and inspect the output before moving on. Do not run whole sections, unrelated command groups, or later diagnostic branches in parallel; earlier output determines which later checks are relevant.
- Treat commands as templates. Substitute namespaces, release names, chart names, and pod names deliberately before running anything.
- Do not run any mutating command unless the user explicitly approves it for the target cluster. This includes `kubectl patch`, `kubectl annotate`, `kubectl delete`, `kubectl scale`, `kubectl rollout restart`, `helm upgrade`, drain/decommission commands, and interactive `kubectl exec` or `kubectl debug` shells.
- In production or whenever the impact is unclear, stop and escalate to TSE or the operator team before pprof/metrics collection, debug containers, timestamp-based rolling restarts, mode changes, operator restarts, scale changes, or decommission actions.

## Step 1: Collect Baseline State

```bash
# Helm release state
helm -n <operator-namespace> status <operator-release> || true
helm -n <cockroachdb-namespace> status <cockroachdb-release> || true
helm -n <operator-namespace> history <operator-release> || true
helm -n <cockroachdb-namespace> history <cockroachdb-release> || true

# Operator state
kubectl -n <operator-namespace> get deploy,pod,svc -o wide | grep -E 'cockroach-operator|NAME'
kubectl -n <operator-namespace> logs -l app=cockroach-operator --tail=200 || true

# CRD and CockroachDB resources
kubectl get crd crdbclusters.crdb.cockroachlabs.com crdbnodes.crdb.cockroachlabs.com
kubectl -n <cockroachdb-namespace> get crdbcluster,crdbnode,pod,svc,endpoints,pvc,pdb -o wide
kubectl -n <cockroachdb-namespace> describe crdbcluster <cockroachdb-release> || true
kubectl -n <cockroachdb-namespace> get events --sort-by=.lastTimestamp | tail -50
```

For a stuck pod or node:

```bash
kubectl -n <cockroachdb-namespace> describe pod <crdb-pod>
kubectl -n <cockroachdb-namespace> logs <crdb-pod> -c cockroachdb --tail=200
kubectl -n <cockroachdb-namespace> logs <crdb-pod> -c cockroachdb --previous
kubectl -n <cockroachdb-namespace> describe crdbnode <crdbnode-name>
```

## Step 2: Classify the Failure

| Symptom | Likely Class | Next Check |
|---|---|---|
| `no matches for kind "CrdbCluster"` | CRDs/operator not installed or not ready | [CRD and operator readiness](#crd-and-operator-readiness) |
| `attempt to grant extra privileges` | Helm RBAC restriction | [RBAC and node-reader failures](#rbac-and-node-reader-failures) |
| TLS values validation error | TLS provider conflict | [TLS and certificate failures](#tls-and-certificate-failures) |
| Operator pod CrashLoopBackOff or OOMKilled | Operator crash or resource limit | [Operator health](#operator-health) |
| Operator running but no reconcile logs | Watch namespace mismatch or blocked worker | [Operator health](#operator-health) |
| `observedGeneration` behind `generation` | Reconcile is stuck or skipped | [Reconciliation not progressing](#reconciliation-not-progressing) |
| Pods Pending | Scheduling, storage, topology, image pull, or node labels | [Pod scheduling and storage failures](#pod-scheduling-and-storage-failures) |
| Pods Running but not Ready | CRDB readiness, TLS, join, DNS, network, or recovery | [Pod readiness and CRDB issues](#pod-readiness-and-crdb-issues) |
| Upgrade stuck with mixed pod images | Version validation, rejected image, rollout dependency, scheduling | [Upgrade and version validation](#upgrade-and-version-validation) |
| Multi-region pods cannot join | DNS, network, region list, CA mismatch | [DNS, service, and network issues](#dns-service-and-network-issues) |
| Scale-down stuck | Decommission/drain blocked or multiple nodes decommissioning | [Scale down and decommission](#scale-down-and-decommission) |
| Migration labels/status stuck | Migration controller issue | [debugging-cockroachdb-operator-migrations](../../cockroachdb-onboarding-and-migrations/debugging-cockroachdb-operator-migrations/SKILL.md) |

## CRD and Operator Readiness

```bash
kubectl -n <operator-namespace> rollout status deploy/cockroach-operator --timeout=5m
kubectl get crd crdbclusters.crdb.cockroachlabs.com -o jsonpath='{.spec.versions[*].name}{"\n"}'
kubectl get crd crdbnodes.crdb.cockroachlabs.com -o jsonpath='{.spec.versions[*].name}{"\n"}'
kubectl -n <operator-namespace> get deploy cockroach-operator -o jsonpath='{.spec.template.spec.containers[0].image}{"\n"}'
```

Remediation:

- Install or upgrade the operator chart first.
- Wait for the operator Deployment before installing the CockroachDB chart.
- For split charts, upgrade the operator chart before the CockroachDB chart.
- Check the Helm chart changelog for version-specific operator fixes before deep debugging older releases.

## Operator Health

```bash
kubectl -n <operator-namespace> get pods -l app=cockroach-operator -o wide
kubectl -n <operator-namespace> describe pod <operator-pod>
kubectl -n <operator-namespace> logs -l app=cockroach-operator --tail=100
kubectl -n <operator-namespace> get deploy cockroach-operator -o jsonpath='{.spec.template.spec.containers[0].env}{"\n"}'
```

Interpretation:

- CrashLoopBackOff: collect previous logs and check for panics.
- OOMKilled: inspect limits and consider increasing operator memory.
- Empty or unset `WATCH_NAMESPACE`: global mode.
- Non-empty `WATCH_NAMESPACE`: the operator watches only listed namespaces.
- If the CockroachDB namespace is not watched, deploy an operator for it or add it to the watch scope.

Check recent logs to see whether reconciliation is active:

```bash
kubectl -n <operator-namespace> logs -l app=cockroach-operator --tail=100 | grep -i reconcil || true
```

Do not add ad hoc annotations to trigger reconciliation. If a user-approved reconcile-triggering change is required, use the chart-supported timestamp path through `helm upgrade --reuse-values`; this updates `helm.sh/restartedAt` and may roll CockroachDB pods, so treat it as a mutating operation:

```bash
helm -n <cockroachdb-namespace> upgrade <cockroachdb-release> <cockroachdb-chart> \
  --reuse-values \
  --set-string cockroachdb.crdbCluster.timestamp="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
```

If the operator is healthy but silent and no user-approved mutation is appropriate, use [collecting-cockroachdb-operator-escalation-packet](../collecting-cockroachdb-operator-escalation-packet/SKILL.md) to gather pprof and metrics before restarting it.

## RBAC and Node-Reader Failures

Common error:

```text
attempt to grant extra privileges
```

Cause:

- The installing principal cannot create cluster-scoped RBAC.
- CockroachDB pods need node read access to derive locality.

Checks:

```bash
kubectl auth can-i create clusterroles.rbac.authorization.k8s.io
kubectl auth can-i create clusterrolebindings.rbac.authorization.k8s.io
kubectl auth can-i get nodes
```

Remediation options:

- Platform team installs the operator chart with `nodeReader.enabled=true` and subjects matching the CockroachDB ServiceAccount.
- Tenant chart sets `cockroachdb.crdbCluster.rbac.nodeReader.create=false` only after the platform-owned binding exists.
- If the customer accepts cluster-admin install privileges, rerun the Helm operation with an identity that can create the required ClusterRole and ClusterRoleBinding.

Do not set `nodeReader.create=false` before replacement RBAC exists.

## Webhook Checks

```bash
kubectl -n <operator-namespace> get svc cockroach-webhook-service
kubectl -n <operator-namespace> get endpoints cockroach-webhook-service
kubectl get validatingwebhookconfigurations | grep cockroach
```

If webhook validation fails, verify the CA bundle:

```bash
kubectl get validatingwebhookconfiguration cockroach-webhook-config \
  -o jsonpath='{.webhooks[0].clientConfig.caBundle}' | base64 -d | openssl x509 -noout -dates -subject -issuer
```

For scoped operators, webhook configurations may be namespace-suffixed, such as `cockroach-webhook-config-<namespace>`.

## Reconciliation Not Progressing

```bash
kubectl -n <cockroachdb-namespace> get crdbcluster <cockroachdb-release> -o json | jq '{
  mode: .spec.mode,
  image: .spec.image,
  generation: .metadata.generation,
  observedGeneration: .status.observedGeneration,
  statusImage: .status.image,
  actions: .status.actions,
  conditions: .status.conditions
}'

kubectl -n <cockroachdb-namespace> get crdbnodes \
  -o custom-columns=NAME:.metadata.name,GENERATION:.metadata.generation,OBSERVED:.status.observedGeneration,PHASE:.status.phase,HASH:.metadata.annotations["crdb.cockroachlabs.com/hash-revision"],NODE_ID:.status.nodeID
```

Checklist:

1. Confirm the operator is running and watching the CockroachDB namespace.
2. Confirm `spec.mode` is not `Disabled`.
3. Check whether initialization conditions look correct for an existing cluster.
4. Check operator logs for reconcile start/end pairs and errors.
5. If no progress is visible, collect the escalation packet before restarting the operator.

## Pod Readiness and CRDB Issues

```bash
kubectl -n <cockroachdb-namespace> get pods -l app.kubernetes.io/name=cockroachdb -o wide
kubectl -n <cockroachdb-namespace> describe pod <crdb-pod>
kubectl -n <cockroachdb-namespace> logs <crdb-pod> -c cockroachdb --tail=200
kubectl -n <cockroachdb-namespace> logs <crdb-pod> -c cockroachdb --previous
kubectl -n <cockroachdb-namespace> get pod <crdb-pod> -o jsonpath='{.spec.containers[0].readinessProbe}{"\n"}'
kubectl -n <cockroachdb-namespace> get pods -l app.kubernetes.io/name=cockroachdb \
  -o custom-columns=NAME:.metadata.name,IMAGE:.spec.containers[0].image,PHASE:.status.phase,READY:.status.containerStatuses[0].ready,NODE:.spec.nodeName
```

Common pod issues:

- Pending during upgrade: old pods may still carry pre-upgrade scheduling or affinity constraints.
- CrashLoopBackOff: inspect storage errors, TLS errors, join address failures, and previous logs.
- Running but not Ready: check the readiness probe, CRDB health endpoint, certificate trust, join service, and whether the node is recovering.

## Upgrade and Version Validation

```bash
kubectl -n <cockroachdb-namespace> get crdbcluster <cockroachdb-release> -o json | jq '{
  specImage: .spec.image,
  statusImage: .status.image,
  actions: .status.actions,
  conditions: [.status.conditions[]? | select(.type | test("Upgrade|Version|Validate"))]
}'

kubectl -n <cockroachdb-namespace> get crdbcluster <cockroachdb-release> -o jsonpath='{.metadata.annotations}{"\n"}' | jq .
kubectl -n <cockroachdb-namespace> get jobs
kubectl -n <cockroachdb-namespace> describe job <version-checker-job>
kubectl -n <cockroachdb-namespace> logs -l job-name=<version-checker-job>
kubectl -n <cockroachdb-namespace> get pods -l app.kubernetes.io/name=cockroachdb \
  -o custom-columns=NAME:.metadata.name,IMAGE:.spec.containers[0].image,REVISION:.metadata.annotations["crdb\.cockroachlabs\.com/hash-revision"],PHASE:.status.phase
```

Interpretation:

- If `spec.image` differs from `status.image`, an upgrade is in progress or stuck.
- If a rejected-image annotation exists, inspect its value; the operator rejected the target version.
- If the version checker job exists but the pod is gone, use job status and operator logs for validation messages.
- Do not delete version checker jobs or pods until their status and logs are captured.

## DNS, Service, and Network Issues

The operator creates separate service paths for pod DNS and join traffic. Do not change service settings without operator-team guidance.

```bash
kubectl -n <cockroachdb-namespace> get service <cockroachdb-release> -o yaml
kubectl -n <cockroachdb-namespace> get service <cockroachdb-release>-join -o yaml
kubectl -n <cockroachdb-namespace> get endpoints <cockroachdb-release>
kubectl -n <cockroachdb-namespace> get endpoints <cockroachdb-release>-join

kubectl -n <cockroachdb-namespace> exec <crdb-pod> -c cockroachdb -- \
  nslookup <cockroachdb-release>.<cockroachdb-namespace>.svc.cluster.local 2>&1 || true

kubectl -n <cockroachdb-namespace> exec <crdb-pod> -c cockroachdb -- \
  nslookup <cockroachdb-release>-join.<cockroachdb-namespace>.svc.cluster.local 2>&1 || true
```

For multi-region checks, use [validating-cockroachdb-helm-multiregion](../../cockroachdb-onboarding-and-migrations/validating-cockroachdb-helm-multiregion/SKILL.md).

## TLS and Certificate Failures

Use [configuring-cockroachdb-helm-tls](../../cockroachdb-operations-and-lifecycle/configuring-cockroachdb-helm-tls/SKILL.md) for TLS mode selection and detailed certificate checks.

Quick checks:

```bash
helm template <release> <chart> -n <namespace> -f values.yaml >/tmp/rendered.yaml
kubectl -n <namespace> get secret,configmap | grep -E 'cockroach|crdb|cert|ca|tls'
kubectl -n <namespace> get crdbcluster <release> -o yaml | grep -A20 certificates
kubectl -n <namespace> get pod <crdb-pod> -o jsonpath='{.spec.containers[*].name}{"\n"}'
kubectl -n <namespace> logs <crdb-pod> -c cert-reloader --tail=100
```

Remediation:

- Ensure exactly one TLS provider is enabled.
- Ensure self-signer `caProvided=true` has a valid `caSecret`.
- Ensure cert-manager `Issuer` or `ClusterIssuer` exists and `Certificate` resources become Ready.
- Ensure external certificate Secrets and CA ConfigMap have expected keys and share a trust root.
- Collect expiry, subject, issuer, and SANs, but do not print private key contents.

## Pod Scheduling and Storage Failures

```bash
kubectl -n <namespace> describe pod <pod-name>
kubectl -n <namespace> get pvc -o wide
kubectl get storageclass
kubectl get nodes -L topology.kubernetes.io/region,topology.kubernetes.io/zone
kubectl -n <namespace> exec <crdb-pod> -c cockroachdb -- df -h /cockroach/cockroach-data
```

Common causes:

- PVCs cannot bind because no default StorageClass exists or `storageClassName` is wrong.
- Topology spread constraints cannot be satisfied because node zone labels are missing or insufficient.
- Node resources are too small for requested CPU/memory.
- Image pull failures from registry policy or air-gapped environments.
- Migrated PVCs may be missing ownerReferences; use the migration debugging skill before deleting anything.

## Scale Down and Decommission

```bash
kubectl -n <cockroachdb-namespace> exec <ready-crdb-pod> -c cockroachdb -- \
  /cockroach/cockroach node status --decommission

kubectl -n <cockroachdb-namespace> get crdbnodes -o json | jq '[.items[] | select(.status.phase=="Decommissioning")] | {count: length, nodes: [.[].metadata.name]}'

kubectl -n <operator-namespace> logs -l app=cockroach-operator --tail=300 | grep -Ei 'decommission|drain|scale|blocking_ranges' || true
```

Questions to answer:

- What was the original and target node count?
- Were multiple nodes scaled down at the same time?
- Were manual decommission or drain commands issued?
- Did any pods or PVCs get deleted manually?

## Temporary Mitigations

Only use these after collecting evidence and confirming the risk with the customer or operator team.

Disable reconciliation for one cluster:

```bash
kubectl -n <cockroachdb-namespace> patch crdbcluster <cockroachdb-release> --type=merge -p '{"spec":{"mode":"Disabled"}}'

# Resume reconciliation:
kubectl -n <cockroachdb-namespace> patch crdbcluster <cockroachdb-release> --type=merge -p '{"spec":{"mode":"MutableOnly"}}'
```

Restart the operator after evidence is collected:

```bash
kubectl -n <operator-namespace> rollout restart deploy/cockroach-operator
```

User-approved timestamp rolling restart:

```bash
helm -n <cockroachdb-namespace> upgrade <cockroachdb-release> <cockroachdb-chart> \
  --reuse-values \
  --set-string cockroachdb.crdbCluster.timestamp="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
```

## Output Format

Return findings in this order:

1. Failure class
2. Evidence: exact command output or Kubernetes status field
3. Root cause or most likely cause
4. Minimal remediation
5. Verification command
6. Data/availability risk, if any
7. Whether escalation packet collection is needed

## References

- [CockroachDB Helm Chart Versioning](../../../cockroachdb-parent/docs/VERSIONING.md)
- [Operator Helm Chart README](../../../cockroachdb-parent/charts/operator/README.md)
- [CockroachDB Helm Chart README](../../../cockroachdb-parent/charts/cockroachdb/README.md)
- [v1alpha1 to v1beta1 Migration Guide](../../../cockroachdb-parent/MIGRATION_v1alpha1_to_v1beta1.md)
- [CockroachDB Docs: Kubernetes troubleshooting](https://www.cockroachlabs.com/docs/stable/orchestrate-cockroachdb-with-kubernetes)
