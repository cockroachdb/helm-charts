---
name: installing-cockroachdb-with-helm
description: Guides customer-facing installation of CockroachDB on Kubernetes using the CockroachDB split Helm charts and operator-managed v1beta1 resources. Use when installing CockroachDB with Helm, choosing between published and local split charts, verifying a new install, or helping an agent complete first-time Kubernetes onboarding.
compatibility: CockroachDB Helm v2 charts with crdb.cockroachlabs.com/v1beta1 CrdbCluster resources. Requires Kubernetes 1.30+, Helm 3+, and cluster permissions to install CRDs and cluster-scoped RBAC unless pre-provisioned by a platform team.
metadata:
  author: cockroachdb
  version: "1.1"
---

# Installing CockroachDB with Helm

Guides an agent through a customer-facing CockroachDB install on Kubernetes using the CockroachDB split Helm charts. Prefer the published split charts for customers. Do not use the unsupported parent or umbrella chart path for customer installs.

## When to Use This Skill

- A customer asks to install CockroachDB on Kubernetes using Helm
- An agent needs to decide whether to install the published split charts or local split charts
- A first install needs structured preflight, chart values, and post-install verification
- A deployment has partially completed and needs the next Helm step

**Related skills:** Use [configuring-cockroachdb-helm-tls](../../cockroachdb-operations-and-lifecycle/configuring-cockroachdb-helm-tls/SKILL.md) for TLS decisions, [validating-cockroachdb-helm-multiregion](../validating-cockroachdb-helm-multiregion/SKILL.md) before multi-region installs, [debugging-cockroachdb-operator-migrations](../debugging-cockroachdb-operator-migrations/SKILL.md) for StatefulSet or v1alpha1 migrations, and [diagnosing-cockroachdb-helm-deployments](../../cockroachdb-observability-and-diagnostics/diagnosing-cockroachdb-helm-deployments/SKILL.md) when any step fails.

## Inputs

Collect these before changing the cluster:

| Input | Example | Why It Matters |
|---|---|---|
| Kubernetes context and namespace | `prod-us-east1`, `cockroachdb` | Avoids installing into the wrong cluster or namespace |
| Install path | Published split charts or local split charts | Determines Helm commands and dependency order |
| Operator release and CockroachDB release names | `crdb-operator`, `crdb` | Resource names and generated services depend on release names |
| Region and cloud provider | `us-east1`, `gcp` | `operator.cloudRegion` must match the region reconciled by this operator |
| TLS mode | self-signer, cert-manager, external certificates, insecure non-prod | Determines chart values and required secrets |
| Topology | single-region or multi-region | Multi-region requires complete `regions` entries and cross-region DNS/networking |

## Safety Considerations

- Confirm the target Kubernetes context with the user before running `helm install`, `helm upgrade`, or `kubectl apply`.
- Do not toggle `cockroachdb.tls.enabled` on an existing cluster. The chart documents this as unsafe and unrecoverable for a running cluster.
- Do not configure multiple operators with overlapping `watchNamespaces`; both can reconcile the same `CrdbCluster` resources.
- If the customer cannot create ClusterRoles or ClusterRoleBindings, coordinate split-chart node-reader RBAC with the platform team before installing the CockroachDB chart.
- Do not use unsupported parent or umbrella chart installs for customer-facing deployment guidance.

## Execution Discipline

- Execute one step at a time and inspect the output before moving on. Preflight output determines whether installation can continue.
- Do not run `helm install`, `helm upgrade`, `kubectl apply`, or any mutating command unless the user explicitly approves it for the target Kubernetes context and namespace.
- Stop before installing if cluster-scoped RBAC, node labels, storage class, TLS mode, registry access, or multi-region prerequisites are unclear.
- In production or restricted environments, involve TSE, the platform team, or the operator team before changing RBAC, webhook, operator, certificate, storage, or network configuration.

## Step 1: Preflight the Kubernetes Context

Run read-only checks first:

```bash
kubectl config current-context
kubectl version --short
helm version --short
kubectl auth can-i create customresourcedefinitions.apiextensions.k8s.io
kubectl auth can-i create clusterroles.rbac.authorization.k8s.io
kubectl auth can-i create clusterrolebindings.rbac.authorization.k8s.io
kubectl get nodes -o wide
kubectl get nodes --show-labels | grep -E 'topology.kubernetes.io/(region|zone)' || true
```

Interpretation:

- Kubernetes must be 1.30 or newer for the current v2 chart line.
- Helm must be v3.
- Node labels `topology.kubernetes.io/region` and `topology.kubernetes.io/zone` should exist before relying on default locality behavior.
- Missing cluster-scoped RBAC is not automatically fatal, but it changes the install path. Use operator-chart `nodeReader` values or have a platform team pre-create the required bindings.

## Step 2: Choose the Install Path

### Published Split Charts (Recommended for Customers)

Use this path for normal customer installs.

```bash
helm repo add cockroachdb-v2 https://charts.cockroachdb.com/v2 --force-update
helm repo update cockroachdb-v2
helm search repo cockroachdb-v2 --devel
```

Install the operator first:

```bash
helm upgrade --install crdb-operator cockroachdb-v2/cockroachdb-operator-chart \
  --namespace cockroach-operator-system \
  --create-namespace \
  --version <operator-chart-version> \
  --set cloudRegion=<current-region>

kubectl -n cockroach-operator-system rollout status deploy/cockroach-operator --timeout=5m
kubectl get crd crdbclusters.crdb.cockroachlabs.com crdbnodes.crdb.cockroachlabs.com
```

Then install CockroachDB:

```bash
helm upgrade --install crdb cockroachdb-v2/cockroachdb-chart \
  --namespace cockroachdb \
  --create-namespace \
  --version <cockroachdb-chart-version> \
  -f values.yaml
```

### Local Split Charts

Use this path when working from a checkout of this repository:

```bash
helm upgrade --install crdb-operator ./cockroachdb-parent/charts/operator \
  --namespace cockroach-operator-system \
  --create-namespace \
  --set cloudRegion=<current-region>

kubectl -n cockroach-operator-system rollout status deploy/cockroach-operator --timeout=5m

helm upgrade --install crdb ./cockroachdb-parent/charts/cockroachdb \
  --namespace cockroachdb \
  --create-namespace \
  -f values.yaml
```

## Step 3: Create a Minimal Values File

For a single-region secure install using the chart self-signer:

```yaml
cockroachdb:
  clusterDomain: cluster.local
  tls:
    enabled: true
    selfSigner:
      enabled: true
    certManager:
      enabled: false
    externalCertificates:
      enabled: false
  crdbCluster:
    regions:
      - code: us-east1
        nodes: 3
        cloudProvider: gcp
        namespace: cockroachdb
    dataStore:
      volumeClaimTemplate:
        spec:
          accessModes:
            - ReadWriteOnce
          resources:
            requests:
              storage: 100Gi
          volumeMode: Filesystem
```

Adjust `code`, `cloudProvider`, `namespace`, storage size, and storage class to the customer environment. The region `code` should match the Kubernetes node region label and the operator chart `cloudRegion`.

## Step 4: Verify the Install

Check Kubernetes resources:

```bash
kubectl -n cockroach-operator-system get deploy cockroach-operator
kubectl -n cockroach-operator-system logs deploy/cockroach-operator --tail=100
kubectl -n cockroachdb get crdbcluster,crdbnode,pods,svc
kubectl -n cockroachdb get crdbcluster crdb -o jsonpath='{.status.readyNodes}{" ready, reconciled="}{.status.reconciled}{" version="}{.status.version}{"\n"}'
kubectl -n cockroachdb get crdbnodes -o custom-columns=NAME:.metadata.name,NODE_ID:.status.nodeID,CONDITIONS:.status.conditions[*].type,TOPOLOGY:.status.topologyValues
```

Expected state:

- `cockroach-operator` Deployment is available.
- CRDs `crdbclusters.crdb.cockroachlabs.com` and `crdbnodes.crdb.cockroachlabs.com` exist.
- `CrdbCluster.status.reconciled` is `true` and `readyNodes` equals the regional node count.
- Each `CrdbNode` has a `nodeID` and Ready/Running conditions.
- CockroachDB pods are Running and Ready.

For SQL verification, connect with the certificate mode selected by [configuring-cockroachdb-helm-tls](../../cockroachdb-operations-and-lifecycle/configuring-cockroachdb-helm-tls/SKILL.md), then run:

```sql
SELECT version();
SHOW REGIONS;
SHOW JOBS;
```

## Outputs

Return a concise install report:

- Kubernetes context, namespace, and chart path used
- Operator release, CockroachDB release, and chart versions
- Values file path or inline values that were applied
- CRD registration status
- `CrdbCluster` ready/reconciled status and node count
- SQL verification result or the exact blocker if SQL could not be verified

## Troubleshooting Handoff

If any command fails, stop the install flow and use [diagnosing-cockroachdb-helm-deployments](../../cockroachdb-observability-and-diagnostics/diagnosing-cockroachdb-helm-deployments/SKILL.md). Preserve the exact command, stderr, namespace, release names, and values file path. If the failure remains unresolved or the operator appears stuck, gather [the operator escalation packet](../../cockroachdb-observability-and-diagnostics/collecting-cockroachdb-operator-escalation-packet/SKILL.md) before restarting or changing operator state.

## References

- [CockroachDB Helm Chart Versioning](../../../cockroachdb-parent/docs/VERSIONING.md)
- [Operator Helm Chart README](../../../cockroachdb-parent/charts/operator/README.md)
- [CockroachDB Helm Chart README](../../../cockroachdb-parent/charts/cockroachdb/README.md)
- [CockroachDB Docs: Deploy with Kubernetes](https://www.cockroachlabs.com/docs/stable/deploy-cockroachdb-with-kubernetes)
- [Helm Docs: Role-based Access Control](https://helm.sh/docs/topics/rbac/)
