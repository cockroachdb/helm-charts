---
name: validating-cockroachdb-helm-multiregion
description: Validates CockroachDB Helm chart values and Kubernetes prerequisites for operator-managed multi-region deployments. Use before adding a region, deploying CockroachDB across multiple Kubernetes clusters, checking region DNS domains, or confirming that all regions share certificate and networking assumptions.
compatibility: CockroachDB Helm v2 charts with crdb.cockroachlabs.com/v1beta1 CrdbCluster resources. Requires access to each Kubernetes context participating in the multi-region deployment.
metadata:
  author: cockroachdb
  version: "1.1"
---

# Validating CockroachDB Helm Multi-Region

Validates the high-risk prerequisites for multi-region CockroachDB deployments managed by the Helm v2 charts and operator. Run this before the first region install and before adding every additional region.

## When to Use This Skill

- A customer is deploying CockroachDB across multiple Kubernetes clusters or regions
- A new region is being added to an existing Helm-managed CockroachDB cluster
- An agent needs to validate `cockroachdb.crdbCluster.regions` before `helm install` or `helm upgrade`
- Cross-region DNS, namespace, or certificate assumptions are unclear

## Required Inputs

| Input | Example | Why It Matters |
|---|---|---|
| Region list in deployment order | `us-central1`, `us-east1` | Each region values file must include current and previously deployed regions |
| Kubernetes context per region | `gke-prod-us-central1` | Commands must run against the correct cluster |
| Namespace per region | `cockroachdb` | Used in generated join addresses |
| Cluster domain per region | `cluster.gke.gcp-us-east1` | Other regions connect through this domain |
| CA/certificate mode | self-signer with shared CA, cert-manager, external | Multi-region requires compatible trust across regions |

## Safety Considerations

- Do not deploy a new region until cross-region service discovery and network paths are proven.
- Do not use different CA trust roots across regions.
- Do not omit previously deployed regions from a new region's values file. The operator uses the full list to compute join addresses.
- Confirm `operator.cloudRegion` matches the region reconciled by that operator instance.
- If an existing multi-region cluster cannot join or reconcile, use the deployment diagnostic skill first and collect the operator escalation packet before restarting the operator.

## Execution Discipline

- Execute one step at a time and inspect the output before moving on. Region inventory, DNS results, network results, and certificate state determine which later checks are relevant.
- Do not create debug pods, run interactive shells, use external diagnostic images, or perform Helm upgrades unless the user explicitly approves them for each participating cluster.
- In air-gapped or private-registry environments, use customer-approved diagnostic images mirrored into the customer's registry.
- In production or when cross-region networking, certificate trust, or locality is unclear, involve TSE or the operator team before adding regions or changing chart values.

## Step 1: Validate Node Locality Labels

For each Kubernetes context:

```bash
kubectl --context <context> get nodes \
  -L topology.kubernetes.io/region,topology.kubernetes.io/zone
```

Expected:

- Every schedulable node has the correct `topology.kubernetes.io/region`.
- Nodes are spread across expected zones where zone failure survival is expected.
- `cockroachdb.crdbCluster.regions[].code` matches the node region label for that cluster.

## Step 2: Validate Values File Completeness

Each region's values file must include all already deployed regions plus the current region.

Example for deploying `us-east1` after `us-central1`:

```yaml
cockroachdb:
  clusterDomain: cluster.gke.gcp-us-east1
  crdbCluster:
    regions:
      - code: us-central1
        nodes: 3
        cloudProvider: gcp
        domain: cluster.gke.gcp-us-central1
        namespace: cockroachdb
      - code: us-east1
        nodes: 3
        cloudProvider: gcp
        domain: cluster.gke.gcp-us-east1
        namespace: cockroachdb
```

Checklist:

- `cockroachdb.clusterDomain` equals the current region's domain.
- Every previous region appears under `cockroachdb.crdbCluster.regions`.
- Every non-local region has `domain` and `namespace` set.
- `nodes` matches the intended node count in each region.
- `cloudProvider` is one of `gcp`, `aws`, `azure`, `k3d`, or empty for other environments.

## Step 3: Validate Cross-Region DNS and Network Paths

From a temporary debugging pod in each region, verify that peer region service names resolve and connect. Replace names with the actual release and namespace.

```bash
kubectl --context <context> -n <namespace> run crdb-dns-check \
  --rm -it --restart=Never --image=busybox:1.36 -- sh
```

Inside the pod:

```sh
nslookup <release-name>.<peer-namespace>.svc.<peer-domain>
nc -vz <release-name>.<peer-namespace>.svc.<peer-domain> 26258
```

If `nc` is unavailable, use an approved network diagnostic image or cloud-native connectivity test. Do not proceed until DNS and TCP connectivity are proven in both directions.

## Step 4: Validate Certificate Trust Across Regions

Use [configuring-cockroachdb-helm-tls](../../cockroachdb-operations-and-lifecycle/configuring-cockroachdb-helm-tls/SKILL.md) for the selected TLS mode, then apply these multi-region checks:

- Self-signer with generated CA is appropriate only if the same CA trust material is shared across regions.
- Self-signer with customer-provided CA requires the same CA Secret in every region namespace.
- Cert-manager requires issuer configuration that produces certificates trusted by the same CA ConfigMap in every region.
- External certificate mode requires all node, HTTP, and root client certificates to chain to the same CA trust root.

Check CA presence without printing contents:

```bash
kubectl --context <context> -n <namespace> get configmap <ca-configmap> -o jsonpath='{.data.ca\.crt}' >/dev/null
kubectl --context <context> -n <namespace> get secret <ca-secret> >/dev/null
```

## Step 5: Render Before Applying

Use `helm template` before installing or upgrading each region:

```bash
helm template <release-name> cockroachdb-v2/cockroachdb-chart \
  --version <cockroachdb-chart-version> \
  --namespace <namespace> \
  -f values-<region>.yaml > rendered-<region>.yaml

grep -n "regions:" -A30 rendered-<region>.yaml
grep -n "clusterDomain:" -A3 values-<region>.yaml
```

Confirm the rendered `CrdbCluster` contains the expected region list and certificate references.

## Outputs

Return a preflight report:

- Region inventory and deployment order
- Per-context node labels and zone spread
- Per-region `clusterDomain`, `namespace`, and `regions` completeness
- Cross-region DNS/TCP results
- Certificate trust validation result
- A clear go/no-go recommendation before Helm install or upgrade

## References

- [CockroachDB Helm Chart README: Multi Region Deployments](../../../cockroachdb-parent/charts/cockroachdb/README.md)
- [Multi-region values example](../../../examples/cockroachdb-operator/multi-region-values.yaml)
- [Diagnosing CockroachDB Helm deployments](../../cockroachdb-observability-and-diagnostics/diagnosing-cockroachdb-helm-deployments/SKILL.md)
- [Operator escalation packet collection](../../cockroachdb-observability-and-diagnostics/collecting-cockroachdb-operator-escalation-packet/SKILL.md)
- [CockroachDB Docs: Multi-Region Capabilities](https://www.cockroachlabs.com/docs/stable/multiregion-overview.html)
- [CockroachDB Docs: cockroach start locality](https://www.cockroachlabs.com/docs/stable/cockroach-start.html#locality)
