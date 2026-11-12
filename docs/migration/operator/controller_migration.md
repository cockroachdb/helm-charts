# Automatic Migration from Legacy Operator to CockroachDB Operator

> These guides are available to read in the
> [CockroachDB documentation](https://www.cockroachlabs.com/docs/stable/migrate-cockroachdb-kubernetes-operator).

This guide covers the automatic migration of a CockroachDB cluster managed by the legacy
operator (v1alpha1 CrdbCluster) to the CockroachDB Operator (v1beta1 CrdbCluster
with CrdbNodes).

The CockroachDB Operator's migration controller handles the transition automatically, including
certificate migration, node-by-node replacement, and RBAC setup.

**Handoff** is the end of controller migration: after you verify finalization and delete
the source StatefulSet, the controller enables normal reconciliation and marks migration
`Complete`. Request rollback before deleting the source StatefulSet; the migration
controller cannot roll back after handoff. **Helm adoption** is the separate step that
follows, using exported values to manage the migrated cluster through the chart.

## Compatibility

Before starting migration, verify your cluster configuration is supported.

| Feature | Supported | Notes |
|---|---|---|
| Self-signer certs (operator built-in) | Yes | Regenerated with join service DNS SANs. Originals preserved |
| cert-manager certs | Yes | Certificate CR updated with join service DNS SANs. Issuer references preserved |
| External certs (user-provided secrets) | Yes | Secret references preserved as ExternalCertificates |
| Custom `NodeTLSSecret` / `ClientTLSSecret` | Yes | CrdbNode pods mount user's secrets directly. Cert regeneration skipped (see prerequisites) |
| Insecure clusters (TLS disabled) | Yes | Detected from `--insecure` flag. Cert migration phase skipped entirely |
| WAL failover (dedicated PVC) | No | WAL failover detection only runs for Helm migrations. Not supported for operator path |
| Dedicated logs PVC (`logsdir` / `logs-dir`) | No | LogsStore detection only runs for Helm migrations. Not supported for operator path |
| PCR (virtualized/standby) | No | PCR detection only runs for Helm migrations (init job pattern). Not supported for operator path |
| Custom service account | Yes | Preserved with `create: false` |
| Custom start flags / `additionalArgs` | Partial | Known operator-managed flags excluded. `additionalArgs` converted to `startFlags.upsert` |
| `cache` / `max-sql-memory` | Yes | Converted to start flags |
| Multi-region | Yes | Provide the full `crdb.cockroachlabs.com/regions` JSON topology before starting. `regionCode` and `cloudProvider` override only the first region |
| Ingress | Partial | v1alpha1 ingress config preserved as annotation on v1beta1. Ingress resources not modified. Manual adoption required |
| ServiceMonitor / PodMonitor | No | Not handled by migration. Must be recreated manually to match new pod labels |
| NetworkPolicy | No | Must be recreated manually if present |
| Log ConfigMap key format | Yes | `logging.yaml` key automatically renamed to `logs.yaml` (reversed on rollback) |

## Before You Begin

Rehearse on a separate test cluster first, even when the real cluster is a development
cluster. Use a similar topology, storage, TLS setup and chart values. Complete migration
and chart adoption there, then compare the resulting pods and configuration with the
source before touching the real cluster.

Do not upgrade either operator or change the database image, version, replicas or chart
configuration while migration is unfinished. Mid-migration upgrades are not supported.
Finish migration or roll it back first. Pause GitOps or other writers that could restore
source replicas or overwrite certificate changes.

Before starting:

- Check database health as well as pod readiness. No rollout, image or version change may
  be pending. Migration uses the source StatefulSet template, not an unfinished rollout.
- Resolve the full region topology and include the region this operator reconciles.
  There is no guessed region. Annotate the CrdbCluster with
  `crdb.cockroachlabs.com/cloudProvider` set to your provider, such as `gcp`, `aws` or
  `azure`, and use that provider's region code format, such as `us-east1` on GCP or
  `us-east-1` on AWS. Leave the provider unset for on-premises clusters.
  For multiple regions, set each region's provider in the full topology JSON; the
  standalone `cloudProvider` annotation overrides only the first region.
- Check every data, logs and WAL PVC. Claims must be Bound, not terminating and no larger
  than the corresponding StatefulSet claim template. `whenScaled=Delete` blocks migration.
  Restored claims are supported. Resolve claim-template size differences before starting.
- Do not edit operator-written migration annotations, including `migration-original-replicas`,
  `migration-original-ports`, `migration-storage` and `rollback-storage` under
  `crdb.cockroachlabs.com/`. They identify the original members, ports and disks.
- Preserve the source TLS mode. For insecure sources, review
  [insecure clusters](#insecure-clusters) before preparing adoption values.
- Record the source PDB selector and budget. It covers replacements until handoff and
  stays in place on rollback. Handoff replaces it with the operator's PDB.
- Plan certificate renewal after handoff. Review
  [certificates during and after migration](#certificates-during-and-after-migration)
  before starting. Migration alone does not set up ongoing renewal.
- Back up Helm-managed cert-manager resources before ownership changes.
- The operator needs `get` on `cert-manager.io/certificates` to discover certificate
  ownership when cert-manager CRDs are installed, including self-signed sources and
  Helm-provided certificates. cert-manager sources also need `patch` to add join names.
  Explicit external legacy-operator Secrets skip that probe. Insecure sources do not
  need it. Use the operator chart or standalone bundle from this release; both grant
  these permissions.

If legacy operators also manage clusters in this Kubernetes cluster, rehearse namespace
scoping during installation. Check `WATCH_NAMESPACE` (`watchNamespaces` in chart values),
webhooks and RBAC for each deployment, especially when several legacy operators run.
Contact Cockroach Labs if their scopes or coexistence behavior are unclear.

Use legacy operator **2.18.7** as the recommended source version. Coexistence requires
at least **2.18.6**. Upgrade and let the cluster settle before starting migration.
Set `crdb.io/skip-reconcile=true` yourself before `crdb.io/migrate=start`. The
CockroachDB Operator does not add this opt-in to the legacy source. Until it is set,
migration waits and reports the missing label. Keep at least one reachable operator
started with `--enable-migration` while legacy-operator clusters remain.

Verify the following before starting migration:

- [ ] CockroachDB cluster is managed by the Legacy Operator (v1alpha1 CrdbCluster).
- [ ] All StatefulSet pods are Running and Ready with no pending rolling updates or scale operations.
- [ ] `kubectl` and `helm` are installed and configured with access to the target cluster.
- [ ] For multi-region clusters: cloud region and provider labels are applied to K8s nodes (see Step 2).
- [ ] You have the `regionCode` and `cloudProvider` values for your cluster (see Step 4).
- [ ] You have reviewed the Compatibility table above and confirmed your configuration is supported.
- [ ] The legacy operator is accessible and running (required for rollback capability).
- [ ] Patch the legacy operator's validating and mutating webhooks to use
  `matchPolicy: Exact` before enabling migration (see Coexistence section in Step 5).
- [ ] **If using custom `NodeTLSSecret` / `ClientTLSSecret`**: Your certificates include join
  service DNS SANs (`{cluster-name}-join`, `{cluster-name}-join.{namespace}`,
  `{cluster-name}-join.{namespace}.svc.cluster.local`). The migration controller cannot
  regenerate certificates when custom secrets are provided (the CA private key is not available).

## Overview

### What the Migration Controller Does Automatically

- Detects cert-manager, self-signer, or custom TLS Secrets. Self-signed certificates are
  regenerated with their existing custom SANs and the join names. cert-manager Certificates
  gain join names while keeping their issuer. Custom `NodeTLSSecret` and `ClientTLSSecret`
  references are mounted directly without regeneration.
- Reuses existing RBAC resources and creates missing ones, with namespace-qualified names
  for cluster-scoped resources. Existing source Roles and RoleBindings stay unchanged.
- Migrates ConfigMap key format (`logging.yaml` to `logs.yaml`).
- Converts v1alpha1 CrdbCluster spec to v1beta1 via conversion webhook.
- Keeps the legacy operator's PDB until handoff, then replaces it with the operator's PDB. See
  [Services, PDBs and join during migration](#services-pdbs-and-join-during-migration).
- Leaves the source Service selectors alone. Replacements carry the source pod labels.
- Checks replacement node and pod readiness, discovery registration, storage adoption
  and client Service port targets before moving on, along with database health through SQL.
  Verify your client connection through the public Service during rehearsal and after adoption.
- Migrates nodes one at a time, verifying cluster health between each node.
- Preserves Ingress configuration from v1alpha1 CR as annotation on v1beta1 CrdbCluster.
- Removes `crdb.io/skip-reconcile` label as the last step of rollback so the legacy operator
  resumes control cleanly.

### What Requires Manual Action

- `crdb.io/skip-reconcile=true` must be applied to the v1alpha1 CrdbCluster before starting migration.
- The legacy operator's validating and mutating webhooks must be patched to use
  `matchPolicy: Exact` before the CockroachDB Operator is installed with migration enabled.
- Cloud region and provider annotations must be applied to the CrdbCluster before starting migration.
- Cloud region and provider node labels must be applied to K8s nodes for multi-region clusters.
- The StatefulSet must be manually deleted after finalization to complete migration.
- After every legacy-operator cluster is migrated, the legacy operator must be uninstalled,
  coexistence metadata removed, `storedVersions` patched, and migration mode disabled.
- ServiceMonitor / PodMonitor resources must be recreated with updated pod labels.

### Migration Phases

```
Init -> CertMigration -> PodMigration -> Finalization -> (user deletes STS) -> Complete
```

| Phase | Duration (approx.) | What happens |
|---|---|---|
| Init | Seconds | Validates prerequisites (including `skip-reconcile`), creates v1beta1 CrdbCluster via conversion webhook, records original replica count |
| CertMigration | Seconds to minutes | Detects cert type, regenerates certs with join service SANs, creates CA ConfigMap, renames ConfigMap key (`logging.yaml` → `logs.yaml`), labels existing pods |
| PodMigration | ~5-15 min per node | Scales down one source ordinal, creates its replacement, then waits for readiness and database health (highest index first) |
| Finalization | Seconds | Sets cluster spec (regions, TLS, resources). The source PDB stays until handoff |
| Complete | Seconds (after user deletes STS) | Sets `Mode=MutableOnly`, releases CrdbCluster spec ownership for adoption, records completion and updates migration label to `complete` |

---

## Step 1: Export Environment Variables

```bash
# CRDBCLUSTER is the name of your v1alpha1 CrdbCluster CR.
export CRDBCLUSTER=cockroachdb

# NAMESPACE is the namespace where the CrdbCluster is installed.
export NAMESPACE=default

# CLOUD_PROVIDER is the cloud vendor where the K8s cluster is running.
# Supported: gcp, aws, azure
export CLOUD_PROVIDER=gcp

# REGION corresponds to the cloud provider's identifier for this region.
# It must match the "topology.kubernetes.io/region" label on K8s nodes.
export REGION=us-central1
```

## Step 2: Apply Cloud Region and Provider Labels (Multi-Region Only)

CockroachDB uses K8s node labels for locality-based pod placement. These labels must exist
before the operator starts scheduling pods.

```bash
# GCP
kubectl label node <node-name> \
  topology.kubernetes.io/region=us-central1 \
  topology.kubernetes.io/zone=us-central1-a \
  cockroach.io/cloud=gcp

# AWS
kubectl label node <node-name> \
  topology.kubernetes.io/region=us-east-1 \
  topology.kubernetes.io/zone=us-east-1a \
  cockroach.io/cloud=aws

# Azure
kubectl label node <node-name> \
  topology.kubernetes.io/region=eastus \
  topology.kubernetes.io/zone=eastus-1 \
  cockroach.io/cloud=azure
```

## Step 3: Pause the Legacy Operator

The legacy operator must stop reconciling the cluster before installing the CockroachDB Operator.
Without this, both controllers will fight over RBAC, PDB, and service selectors.

```bash
kubectl label crdbcluster $CRDBCLUSTER crdb.io/skip-reconcile="true" -n $NAMESPACE
```

Wait until the legacy operator finishes any in-progress reconciliation before proceeding.
Verify the StatefulSet is not currently rolling:

```bash
kubectl rollout status sts $CRDBCLUSTER -n $NAMESPACE
# Expected: statefulset rolling update complete

# No new events should appear after applying the label
kubectl describe crdbcluster $CRDBCLUSTER -n $NAMESPACE | grep -A5 Events
```

If the `skip-reconcile` label is not applied, migration waits with:
`missing required label: crdb.io/skip-reconcile=true`

> **Important**: The `crdb.io/skip-reconcile=true` label must remain on the v1alpha1 CrdbCluster
> for as long as the legacy operator is running. The legacy operator reconciles any CrdbCluster
> without this label and will recreate the StatefulSet and associated resources if it regains
> control. Do not remove this label until after the legacy operator has been fully uninstalled
> (Step 12). Removing it while the legacy operator is still running will cause it to recreate
> the StatefulSet, potentially conflicting with the migrated CrdbNodes.

## Step 4: Annotate CrdbCluster with Region and Cloud Provider

Annotate the CR with region and cloud provider so they are preserved during the v1alpha1 to
v1beta1 conversion:

```bash
kubectl annotate crdbcluster $CRDBCLUSTER \
  crdb.cockroachlabs.com/cloudProvider=$CLOUD_PROVIDER \
  crdb.cockroachlabs.com/regionCode=$REGION \
  --overwrite -n $NAMESPACE
```

Region annotations override the derived topology. For multiple regions, provide the full
`crdb.cockroachlabs.com/regions` JSON array. `regionCode` and `cloudProvider` override only
the first entry. An empty or malformed region blocks migration. An empty `cloudProvider` is valid
for an on-premises cluster. The resulting topology
must include the operator's resolved region. There is no hard-coded fallback.

## Step 5: Install CockroachDB Operator with Migration Enabled

If OLM manages the legacy operator, arrange with your platform administrator to prevent
OLM from upgrading it or reverting the shared CRD and webhook changes during migration.
Rehearse that arrangement first. Do not delete the CRD or remove the legacy operator while
it still manages source clusters. Contact Cockroach Labs if you cannot keep these resources
stable throughout migration and coexistence cleanup.

For a new operator installation managed by GitOps, set `selfSignedOperatorCerts=true` so
the operator manages its webhook TLS Secret instead of Helm generating it during renders.
This setting is separate from database TLS. Choose it before installation. For an existing
operator release, follow the
[certificate ownership switching steps](../../../cockroachdb-operator/charts/operator/README.md#operator-tls-certificates-selfsignedoperatorcerts)
before changing the setting.

Install the operator with migration enabled. The migration controller only acts on
resources that have the `crdb.io/migrate` label, and the cluster controller skips clusters
with `Mode=Disabled` (which the conversion webhook sets for all v1alpha1 clusters). The
CockroachDB Operator will not interfere with clusters that have not been explicitly marked
for migration.

```bash
helm upgrade --install crdb-operator ./cockroachdb-operator/charts/operator \
  --set migration.enabled=true \
  --set cloudRegion=$REGION \
  --set appLabel=cockroachdb-operator
```

Setting `watchNamespaces` is optional. It restricts the CockroachDB Operator to a subset of
namespaces, which can be useful for reducing blast radius in large environments.

```bash
helm upgrade --install crdb-operator ./cockroachdb-operator/charts/operator \
  --set migration.enabled=true \
  --set cloudRegion=$REGION \
  --set appLabel=cockroachdb-operator \
  --set watchNamespaces=$NAMESPACE
```

`cloudRegion` must match the region code in the migrated `spec.regions`. The source annotations
control the converted spec. `cloudRegion` tells this operator which region entry it reconciles.

> `migration.enabled=true` on the **operator chart** enables the migration controller and
> registers the conversion webhook (translates between v1alpha1 and v1beta1).

Verify the operator is running and the conversion webhook is active:

```bash
kubectl get pods -l app=cockroachdb-operator -n $NAMESPACE
kubectl get crd crdbclusters.crdb.cockroachlabs.com

# Verify both API versions are accessible (conversion webhook is active)
kubectl get crdbclusters.v1alpha1.crdb.cockroachlabs.com $CRDBCLUSTER -n $NAMESPACE
kubectl get crdbclusters.v1beta1.crdb.cockroachlabs.com $CRDBCLUSTER -n $NAMESPACE
```

Both commands should return the same CrdbCluster resource, confirming the conversion
webhook is translating between API versions.

The operator's ClusterRole and ClusterRoleBinding must use unique names to avoid overwriting
the legacy operator's RBAC. The Helm chart handles this automatically.

This migration flow sets `appLabel=cockroachdb-operator` so the CockroachDB Operator's
Deployment selector and Service selector do not collide with the legacy operator's
`app=cockroach-operator` labels when both operators run in the same namespace. The operator
chart default remains `appLabel=cockroach-operator` for normal upgrades and preview users to
avoid changing the existing immutable Deployment selector. Use the distinct `appLabel` only
for new migration/coexistence installs.

When migrating clusters across multiple namespaces, migrate one namespace at a time. Update
`watchNamespaces` (or use a comma-separated list) to include additional namespaces only after
the previous migration is complete.

### Coexistence with the Legacy Operator

The CockroachDB Operator and the legacy operator can run side by side during migration.
Installing the CockroachDB Operator with `migration.enabled=true` replaces the CRD with a
dual-version CRD (v1alpha1 + v1beta1) and registers a conversion webhook. Existing clusters
continue to work normally.

#### What works

| Scenario | Behavior |
|---|---|
| Existing v1alpha1 clusters (not being migrated) | Continue to be reconciled by the legacy operator. The CockroachDB Operator ignores them (`Mode=Disabled`). |
| New v1alpha1 clusters created with both operators present | Reconciled by the legacy operator. The CockroachDB Operator's v1beta1 webhooks do not intercept v1alpha1 requests. |
| New v1beta1 clusters created with both operators present | The CockroachDB Operator's mutating webhook automatically injects `crdb.io/skip-reconcile=true`. The legacy operator sees this label and skips reconciliation. |
| Updating existing v1alpha1 clusters | Works normally. Only the legacy operator's webhooks validate the request. |

#### What does not work

| Scenario | Reason |
|---|---|
| Migrating or creating v1beta1 clusters without patching legacy operator webhooks | The legacy operator's webhooks use `matchPolicy: Equivalent` (the Kubernetes default), so they can intercept v1beta1 requests after converting them to v1alpha1. This can block migration and other v1beta1 operations. |
| Both operators reconciling the same cluster | Only one operator should own a cluster at a time. Use `crdb.io/skip-reconcile=true` to hand off. |
| Running both operators with overlapping namespace scopes | Untested. Use `watchNamespaces` to separate them if needed. |

#### How webhook isolation works

Both operators' webhooks must use `matchPolicy: Exact` to prevent cross-version interception.
The CockroachDB Operator's webhooks ship with this setting. The legacy operator's webhooks
use the Kubernetes default (`Equivalent`), which can cause them to intercept v1beta1 requests
after Kubernetes converts them to v1alpha1. Before enabling migration, update both the
validating and mutating legacy operator webhooks to use `matchPolicy: Exact`.

**Option 1: Edit the legacy operator manifest before installing.** Download the manifest,
add `matchPolicy: Exact` to both webhook entries, then apply:

```bash
curl -sL https://raw.githubusercontent.com/cockroachdb/cockroach-operator-legacy/v2.18.7/install/operator.yaml \
  | sed '/rules:/i\    matchPolicy: Exact' \
  | kubectl apply -f -
```

**Option 2: Patch the live webhook configs.** This does not persist across legacy operator
redeploys:

```bash
kubectl patch validatingwebhookconfiguration cockroach-operator-validating-webhook-configuration \
  --type=json -p='[{"op":"add","path":"/webhooks/0/matchPolicy","value":"Exact"}]'
kubectl patch mutatingwebhookconfiguration cockroach-operator-mutating-webhook-configuration \
  --type=json -p='[{"op":"add","path":"/webhooks/0/matchPolicy","value":"Exact"}]'
```

With both sides using `matchPolicy: Exact`, a v1alpha1 request triggers only the legacy
operator's webhooks, and a v1beta1 request triggers only the CockroachDB Operator's webhooks.

This patch is mandatory for the entire period in which the legacy operator and the
migration-enabled CockroachDB Operator coexist. Because patching the live webhook
configurations does not survive a legacy operator redeployment, verify and reapply it after
any redeployment. Keep the patch in place until the legacy operator is uninstalled. Uninstalling
the legacy operator removes its webhook configurations, so there is nothing to revert afterward.

#### Cluster behavior during coexistence

Existing v1alpha1 clusters without `skip-reconcile` continue to be managed by the legacy
operator. The CockroachDB Operator does not reconcile them.

For new v1beta1 clusters, the CockroachDB Operator adds `crdb.io/skip-reconcile=true` so the
legacy operator does not reconcile them.

Fresh v1beta1 clusters remain compatible with Helm, Argo CD, and other Server-Side Apply
configuration managers during coexistence.

In summary, you can:
- Keep existing v1alpha1 clusters running with the legacy operator
- Create new v1alpha1 clusters while the CockroachDB Operator is installed
- Create new v1beta1 clusters while the legacy operator is still running
- Migrate clusters one by one at your own pace

## Step 6: Start Migration

Initiate the migration by labeling the v1alpha1 CrdbCluster.

```bash
kubectl label crdbcluster $CRDBCLUSTER crdb.io/migrate=start -n $NAMESPACE
```

## Step 7: Monitor Migration

### DB Console

The DB Console is accessible via the public service throughout migration.

```bash
# Port-forward to access DB Console
kubectl port-forward svc/${CRDBCLUSTER}-public 8080 -n $NAMESPACE
# Then open https://localhost:8080 in your browser
```

Key things to monitor in the DB Console:

- **Node List**: A source pod is removed before its replacement can reuse the same name
  and PVCs.
  Wait for replacement readiness and database health before the next ordinal advances.
- **Replication Status**: Under-replicated and unavailable ranges should stay at zero.
  The migration controller waits for under-replicated ranges to clear before proceeding
  to the next node.
- **Ranges Dashboard**: Under-replicated ranges counter should not increase during migration.

### CockroachDB Health Checks

The controller performs these checks automatically between each node migration, but you can
verify manually:

```bash
# During migration, STS pods use container name "db". After migration, CrdbNode pods use "cockroachdb"
# Verify ranges_underreplicated is 0 for every node and count the is_live=true rows.
kubectl exec $CRDBCLUSTER-0 -n $NAMESPACE -c db -- \
  /cockroach/cockroach node status --ranges --format table \
  --certs-dir=/cockroach/cockroach-certs
```

`cockroach node status --ranges` targets the system virtual cluster
automatically. For the direct SQL diagnostic alternative and its
`allow_unsafe_internals` and UA routing requirements, see
[Inspect cluster health manually](../../../cockroachdb-operator/charts/operator/README.md#inspect-cluster-health-manually).
For insecure clusters, replace `--certs-dir=/cockroach/cockroach-certs` with
`--insecure`.

### Watch migration status

The migration status is recorded on the v1beta1 CrdbCluster (created via conversion webhook).

```bash
kubectl get crdbcluster $CRDBCLUSTER -n $NAMESPACE \
  -o jsonpath='{.status.migration.phase} {.status.migration.message}' -w
```

### Watch events

```bash
kubectl get events -n $NAMESPACE \
  --field-selector involvedObject.name=$CRDBCLUSTER \
  --sort-by='.lastTimestamp'
```

### Watch CrdbNode creation during PodMigration

```bash
kubectl get crdbnode -n $NAMESPACE \
  -l crdb.cockroachlabs.com/cluster=$CRDBCLUSTER -w
```

### Phase-Specific Checks

| Phase | What to watch |
|-------|---------------|
| Init | `kubectl describe crdbcluster $CRDBCLUSTER` for MigrationStarted event |
| CertMigration | For self-signer: `kubectl get secret $CRDBCLUSTER-node-secret -n $NAMESPACE`. For cert-manager: check the secret name from the Certificate CR (`kubectl get certificate $CRDBCLUSTER-node -n $NAMESPACE -o jsonpath='{.spec.secretName}'`). For custom `NodeTLSSecret`: your existing secret is used directly. TLS clusters only |
| PodMigration | `kubectl get crdbnode -n $NAMESPACE -w` for one node per pod |
| Finalization | `kubectl get crdbcluster $CRDBCLUSTER -o jsonpath='{.status.migration.message}'` for "Finalization complete" |
| Complete | After StatefulSet deletion. `spec.mode` becomes `MutableOnly` and the `crdb.io/migrate` label becomes `complete` |

### Auto-pause behavior

If a health wait exceeds 10 minutes without progress, migration pauses with phase
`Stopped`, label `stopped` and a `MigrationPaused` event. During PodMigration, checks
run every 10 seconds and verify:

1. CrdbNode has `PodReady=True` condition
2. Pod is in `Running` phase with `Ready=True` condition
3. SQL health check passes (zero under-replicated ranges and correct live node count)
4. Pod is registered in the headless service endpoints

After repairing a forward migration, resume with `crdb.io/migrate=start`. A paused
rollback resumes only with `crdb.io/migrate=rollback`. Check the status message first.

### Prometheus Metrics

```
crdb_operator_migration_phase{cluster, namespace, phase}
crdb_operator_migration_pods_total{cluster, namespace}
crdb_operator_migration_pods_migrated{cluster, namespace}
crdb_operator_migration_duration_seconds{cluster, namespace, phase}
crdb_operator_migration_errors_total{cluster, namespace, phase, error_type}
crdb_operator_migration_rollbacks_total{cluster, namespace, reason}
```

### PDB Behavior During Migration

The source `{name}` PDB covers replacements through finalization. At handoff,
the controller creates `{name}-pdb` before deleting the source PDB. The new budget defaults to one
unavailable pod. A custom source budget is not carried over, and the chart has no setting
for changing this budget. Check the live PDB and its selector before allowing voluntary
disruptions.

Rollback leaves the source PDB unchanged. Verify its original budget and selector before
resuming voluntary disruptions.

## Step 8: Delete StatefulSet to Complete Migration

After Finalization completes, the controller stops processing and waits for the user to
delete the StatefulSet. The StatefulSet is intentionally left intact. Once deleted, the
controller sets `spec.mode=MutableOnly` and records `Phase=Complete`.

```bash
# Verify finalization work is settled and the source has zero replicas.
# Mode is still Disabled. Do not delete merely because phase says Finalization.
kubectl get crdbcluster $CRDBCLUSTER -n $NAMESPACE \
  -o jsonpath='{.status.migration.phase} {.status.migration.settledPhase}'
# Expected: Finalization Finalization
kubectl get sts $CRDBCLUSTER -n $NAMESPACE -o jsonpath='{.spec.replicas}'
# Expected: 0

# Check the finalization message as well
kubectl get crdbcluster $CRDBCLUSTER -n $NAMESPACE \
  -o jsonpath='{.status.migration.message}'
# Expected: Finalization complete. Delete the StatefulSet to mark migration complete.

# Delete the StatefulSet to trigger Phase=Complete
kubectl delete sts $CRDBCLUSTER -n $NAMESPACE

# Verify completion (may take a few seconds)
# Mode switches to MutableOnly and label changes to "complete" after STS deletion.
kubectl get crdbcluster $CRDBCLUSTER -n $NAMESPACE \
  -o jsonpath='{.spec.mode} {.status.migration.phase}'
# Expected: MutableOnly Complete
```

All STS pods are already at replicas=0 by the time Finalization runs. Deleting the StatefulSet
object does not evict any running pods.

After handoff, the controller also removes the temporary
`crdb.cockroachlabs.com/migration-storage` annotation from CrdbNodes. This cleanup is
best effort; a leftover annotation is harmless. Do not edit it while migration is running.

## Step 9: Verify Cluster Health Post-Migration

```bash
# All CrdbNodes should be healthy
kubectl get crdbnode -n $NAMESPACE -l crdb.cockroachlabs.com/cluster=$CRDBCLUSTER

# All pods running
kubectl get pods -n $NAMESPACE -l crdb.cockroachlabs.com/cluster=$CRDBCLUSTER

# Verify data integrity from any CrdbNode pod
# Note: post-migration, the container name is "cockroachdb" (not "db" as in the original STS)
kubectl exec $CRDBCLUSTER-0 -n $NAMESPACE -c cockroachdb -- \
  /cockroach/cockroach sql --certs-dir=/cockroach/cockroach-certs \
  -e "SELECT count(*) FROM [SHOW RANGES];"

# Verify under-replicated ranges are zero
kubectl exec $CRDBCLUSTER-0 -n $NAMESPACE -c cockroachdb -- \
  /cockroach/cockroach node status --ranges --format table \
  --certs-dir=/cockroach/cockroach-certs
```

### After handoff checklist

- Connect through the original public Service and client port. For secure sources, verify
  TLS against its DNS name and any custom DNS names clients use. For insecure sources,
  confirm TLS and every certificate mode remain disabled.
- Check the discovery Service and its EndpointSlices after Helm adoption. Confirm all
  replacement pods are Ready and registered, and verify the live PDB budget and selector.

- Verify all three markers together: `spec.mode=MutableOnly`,
  `status.migration.phase=Complete` and `crdb.io/migrate=complete`. A deleted StatefulSet
  or a progress count alone is not proof. Then check live database members, ranges and
  application traffic.
- Compare the migrated configuration and running pod arguments with your source. Put
  subsequent changes on `CrdbCluster.spec.template.spec`, through your chart or GitOps
  configuration. Editing an individual CrdbNode does not configure future nodes.
- Adopt the CrdbCluster with a changed-value Server-Side Apply, without forcing ownership
  conflicts. If the CrdbCluster conflicts, collect evidence before proceeding. Other
  resources such as Services or Certificates can need a reviewed first ownership transfer.
- Keep `crdb.io/skip-reconcile=true` until the legacy operator is uninstalled. The terminal
  `crdb.io/migrate=complete` label may stay or be removed after all three markers are present.
  Removing the migrate label during an active migration requests rollback.
- Carry custom certificate names into the new values. Use
  `cockroachdb.tls.certManager.additionalDnsNames` and `.additionalIpAddresses`, or
  `cockroachdb.tls.selfSigner.additionalSANs` before enabling the self-signer. Migration
  preserves the issued names, but the next chart render uses its configured values.
- Preserve placement settings during adoption. `podTemplate` affinity overrides the
  generated affinity. It does not merge placement policies.
- Check the image in the parent template before deleting the StatefulSet. An explicit
  legacy-operator image can become a health-gated rolling update after handoff. A version-only
  request follows the source image. Do not combine that change with migration.
- Migration carries `persistentVolumeClaimRetentionPolicy.whenDeleted: Retain` into the
  parent template. Removing a migrated node leaves its PVCs and disks. Delete retained
  claims only when their data is no longer needed. The PV reclaim policy controls disk
  deletion. Check retained claims before reusing a node name.

### Verify preserved configurations

```bash
# ConfigMap key format (should be logs.yaml, not logging.yaml)
kubectl get configmap <log-config-name> -n $NAMESPACE -o yaml | grep -E "logging.yaml|logs.yaml"

# TLS certificates (secret names depend on cert type)
# Self-signer: {name}-node-secret, {name}-client-secret (regenerated by migration)
kubectl get secret $CRDBCLUSTER-node-secret -n $NAMESPACE
kubectl get secret $CRDBCLUSTER-client-secret -n $NAMESPACE
# cert-manager: secrets are managed by cert-manager (names from Certificate CR)
kubectl get certificate $CRDBCLUSTER-node -n $NAMESPACE -o jsonpath='{.spec.secretName}'
# Custom NodeTLSSecret/ClientTLSSecret: your original secrets are used directly
# CA ConfigMap (created for all TLS cert types)
kubectl get configmap $CRDBCLUSTER-ca-crt -n $NAMESPACE
```

### Verify v1alpha1 → v1beta1 field conversion

Key fields converted during migration:

| v1alpha1 field | v1beta1 field |
|---|---|
| `spec.image.name` | `spec.template.spec.image` |
| `spec.grpcPort` / `spec.sqlPort` / `spec.httpPort` | `spec.template.spec.grpcPort` / `sqlPort` / `httpPort` |
| `spec.cache` | `spec.template.spec.startFlags.upsert` (`--cache=<value>`) |
| `spec.maxSQLMemory` | `spec.template.spec.startFlags.upsert` (`--max-sql-memory=<value>`) |
| `spec.additionalArgs` | `spec.template.spec.startFlags.upsert` |
| `spec.nodeTLSSecret` | CrdbNode mounts this secret directly (cert regeneration skipped) |
| `spec.clientTLSSecret` | CrdbNode mounts this secret directly (cert regeneration skipped) |
| `spec.logConfigMap` | `spec.template.spec.loggingConfigMapName` (key renamed `logging.yaml` → `logs.yaml`) |
| `spec.terminationGracePeriodSecs` | Used directly in pod spec |
| `spec.resources` | `spec.template.spec.podTemplate.spec.containers[0].resources` |

## Step 10: Configure LocalityMappings

Migration preserves an explicit `--locality` in `startFlags.upsert`, including a command
expression. It remains authoritative and the init container skips locality computation.
Changing to `localityMappings` is optional and belongs after handoff, not in recovery.
Before removing the flag, verify that mappings produce the expected tiers and values on
every Kubernetes node, including AWS zone names versus IDs and any custom tier prefixes.
Mappings also add a `dns` tier. Remove the flag from the chart or GitOps configuration too,
or the next apply will restore it. A source with no locality flag gains default mappings
at handoff and can have one health-gated rolling update.

If your cluster uses custom K8s node labels for locality, update `localityMappings` to
match. Each entry maps a K8s node label key to a CockroachDB locality tier name.

```bash
kubectl patch crdbcluster $CRDBCLUSTER -n $NAMESPACE --type merge -p '
spec:
  template:
    spec:
      localityMappings:
        - nodeLabel: "my-custom-label/region"
          localityLabel: "region"
        - nodeLabel: "my-custom-label/zone"
          localityLabel: "zone"
'
```

If you are using the standard `topology.kubernetes.io/*` labels, the kubebuilder defaults
are correct and no action is needed.

## Step 11: Adopt into CockroachDB Helm Chart (Optional)

Before adopting, verify the operator has fully reconciled the migrated cluster. Do not
proceed until `generation` and `observedGeneration` match and all pods are running.
When migration reaches `Mode=MutableOnly`, `Phase=Complete`, and
`crdb.io/migrate=complete`, Helm 4 and other Server-Side Apply managers can adopt the v1beta1
object directly.

```bash
kubectl get crdbcluster $CRDBCLUSTER -n $NAMESPACE \
  -o jsonpath='{.metadata.generation} {.status.observedGeneration}'
# Both values should match

kubectl get pods -n $NAMESPACE -l crdb.cockroachlabs.com/cluster=$CRDBCLUSTER
# All pods should be Running and Ready
```

If you want `helm upgrade` to manage the migrated CrdbCluster going forward, you need to
annotate existing resources for Helm ownership and provide a `values.yaml` that matches
the migrated spec.

### Prepare values.yaml

Inspect the migrated CrdbCluster and map its spec to Helm values.

```bash
kubectl get crdbcluster $CRDBCLUSTER -n $NAMESPACE -o yaml
```

### Option A: Generate values.yaml automatically

The helper needs `get` access to the migrated CrdbCluster, Services and certificate
Secrets, plus `list` access to Secrets in the cluster namespace to find the source
Helm release settings.

```bash
make bin/migration-helper

./bin/migration-helper export-values \
  --crdb-cluster $CRDBCLUSTER \
  --namespace $NAMESPACE \
  --output-dir ./manifests
```

Review the generated file:

```bash
cat ./manifests/values.yaml
```

If the generated file contains `cockroachdb.crdbCluster.rbac.serviceAccount.name`, keep
that value as is. It preserves the service account used by the migrated cluster.

The export also carries the live public Service's name, type, custom labels and annotations,
and names of ports with `grpc`, `sql` or `http` targets. Compare the rendered Service with
its live configuration before adoption. The chart uses the database listener ports as
Service ports and has no values for fixed NodePorts, load-balancer IPs or traffic policies.
If you rely on those settings or expose a different client port, arrange a separately
managed Service and rehearse its retention before changing charts.

### Option B: Build values.yaml manually

For an insecure source, disable every certificate mode as shown in
[insecure clusters](#insecure-clusters). Setting only `cockroachdb.tls.enabled: false`
fails because the chart enables the self-signer by default.

Key values to set:

| Helm value | Source from migrated CrdbCluster |
|---|---|
| `cockroachdb.crdbCluster.image.name` | `spec.template.spec.image` |
| `cockroachdb.crdbCluster.regions` | `spec.regions` (code, nodes, namespace, cloudProvider) |
| `cockroachdb.tls.enabled` | `spec.tlsEnabled` |
| `cockroachdb.crdbCluster.rbac.serviceAccount.name` | `spec.template.spec.podTemplate.spec.serviceAccountName` (leave empty to use default fullname) |
| `cockroachdb.crdbCluster.podTemplate.spec.containers[0].resources` | `spec.template.spec.podTemplate.spec.containers[0].resources` |
| `cockroachdb.crdbCluster.rollingRestartDelay` | `spec.rollingRestartDelay` |
| `cockroachdb.crdbCluster.clusterSettings` | `spec.clusterSettings` |

The v1alpha1 CrdbCluster fields (cache, max-sql-memory, additionalArgs) were converted to
`spec.template.spec.startFlags.upsert` during migration. Map these to the appropriate Helm
values or pass them via `cockroachdb.crdbCluster.startFlags`.

### Field Mapping Reference

| Source (v1alpha1) | Migrated v1beta1 shape | Helm adoption value |
|---|---|---|
| `metadata.name` | `CrdbCluster.metadata.name` | `k8s.fullnameOverride` |
| `spec.image.name` | `spec.template.spec.image` | `cockroachdb.crdbCluster.image.name` |
| `spec.dataStore` | `spec.template.spec.dataStore` | `cockroachdb.crdbCluster.dataStore` |
| `spec.logConfigMap` | `spec.template.spec.loggingConfigMapName` (key renamed `logging.yaml` → `logs.yaml`) | `cockroachdb.crdbCluster.loggingConfigMapName` |
| `spec.resources` | `spec.template.spec.podTemplate.spec.containers[0].resources` | `cockroachdb.crdbCluster.podTemplate.spec` |
| `spec.cache` | `spec.template.spec.startFlags.upsert` (`--cache=<value>`) | `cockroachdb.crdbCluster.startFlags` |
| `spec.maxSQLMemory` | `spec.template.spec.startFlags.upsert` (`--max-sql-memory=<value>`) | `cockroachdb.crdbCluster.startFlags` |
| `spec.additionalArgs` | `spec.template.spec.startFlags.upsert` | `cockroachdb.crdbCluster.startFlags` |
| Service account | `spec.template.spec.podTemplate.spec.serviceAccountName` | `cockroachdb.crdbCluster.rbac.serviceAccount.name` with `create=false` |
| Pod annotations | `spec.template.spec.podTemplate.metadata.annotations` | `cockroachdb.crdbCluster.podTemplate.metadata.annotations` |
| Priority class | `spec.template.spec.podTemplate.spec.priorityClassName` | `cockroachdb.crdbCluster.podTemplate.spec.priorityClassName` |
| Ingress intent | Preserved as annotation on CrdbCluster | `cockroachdb.crdbCluster.service.ingress` |

### Annotate resources for Helm ownership

The CockroachDB chart manages Ingress resources; the operator and migration controller
leave existing Ingresses unchanged. Export reads live `ui-<cluster>` and `sql-<cluster>`
resources. Review the exported hosts, ingress class and annotations, and compare the
rendered paths and TLS settings with the existing routes. The chart creates
`ui-<release>` and `sql-<release>`, so use the cluster name as the release name to adopt
those existing Ingresses, or retain them under separate management. Annotating a
differently named Ingress does not rename it or make the chart manage it.
Backends use the configured public Service name and port names. When SQL and gRPC
share a port, the SQL backend uses the gRPC Service port name. Verify ingress routing
after adoption.

```bash
export RELEASE_NAME="<your-helm-release-name>"

# Use cockroachdb.crdbCluster.service.public.name from the exported values.
# The client Service may be named ${CRDBCLUSTER} or ${CRDBCLUSTER}-public.
export PUBLIC_SERVICE="<client-service-name>"

# Namespace-scoped resources
for RESOURCE in \
  "crdbcluster/${CRDBCLUSTER}" \
  "serviceaccount/${CRDBCLUSTER}-sa" \
  "service/${PUBLIC_SERVICE}" \
  "role/${CRDBCLUSTER}" \
  "rolebinding/${CRDBCLUSTER}"; do
  kubectl annotate "${RESOURCE}" \
    meta.helm.sh/release-name="${RELEASE_NAME}" \
    meta.helm.sh/release-namespace="${NAMESPACE}" \
    -n "${NAMESPACE}" --overwrite
  kubectl label "${RESOURCE}" \
    app.kubernetes.io/managed-by=Helm \
    -n "${NAMESPACE}" --overwrite
done

# Ingress resources (only when adopting them with RELEASE_NAME=$CRDBCLUSTER)
for RESOURCE in \
  "ingress/ui-${CRDBCLUSTER}" \
  "ingress/sql-${CRDBCLUSTER}"; do
  kubectl get "${RESOURCE}" -n "${NAMESPACE}" 2>/dev/null && \
  kubectl annotate "${RESOURCE}" \
    meta.helm.sh/release-name="${RELEASE_NAME}" \
    meta.helm.sh/release-namespace="${NAMESPACE}" \
    -n "${NAMESPACE}" --overwrite && \
  kubectl label "${RESOURCE}" \
    app.kubernetes.io/managed-by=Helm \
    -n "${NAMESPACE}" --overwrite
done
```

Do not annotate the headless service (`service/${CRDBCLUSTER}`). The headless service is
operator-managed and not templated by the CockroachDB Helm chart.

Do not annotate the migration-created ClusterRole/ClusterRoleBinding. The CockroachDB Helm
chart creates its own cluster-scoped RBAC with different names
(`{fullname}-{namespace}-node-reader`). Delete the stale migration-created ones after
Helm adoption:

```bash
# Migration-created ClusterRole/ClusterRoleBinding use "{namespace}-{name}" naming
kubectl delete clusterrole ${NAMESPACE}-${CRDBCLUSTER} --ignore-not-found
kubectl delete clusterrolebinding ${NAMESPACE}-${CRDBCLUSTER} --ignore-not-found
```

### Run Helm install

There is no existing Helm release for the migrated cluster, so use `--install` to create one.
First check ownership of the CrdbCluster separately. In `./manifests/values.yaml`, set
`cockroachdb.crdbCluster.rollingRestartDelay` to a reviewed value different from the live
value (for example, `10s` instead of `30s`). Render and apply only the CrdbCluster:

```bash
helm template ${RELEASE_NAME} ./cockroachdb-operator/charts/cockroachdb \
  --namespace ${NAMESPACE} \
  --values ./manifests/values.yaml \
  --show-only templates/crdb.yaml > ./manifests/adopt-crdb.yaml
kubectl apply --server-side --field-manager=helm -f ./manifests/adopt-crdb.yaml
```

This apply must succeed without `--force-conflicts`. If it conflicts, stop and collect
the object and its managed fields before proceeding. Keep the reviewed value in your
values file so the following Helm command uses the same configuration.

Other rendered resources, including Services, Roles and RoleBindings, can still have
legacy chart or migration-controller field owners. Review their changes before the first
Helm adoption, especially Service selectors, ports and annotations, and the RoleBinding's
service account and `roleRef`. The chart uses the existing namespaced Role name.
`--force-conflicts` transfers field ownership but cannot change an immutable `roleRef` or
preserve configuration missing from the values file. Stop and resolve any mismatch first.

Controller export keeps cert-manager sources in external-certificate mode, so the chart
does not render or take over their Certificates. Keep those Certificates and their issuer
chain managed separately. If you explicitly enable chart cert-manager mode, compare its
Secret names, issuer, lifetimes and SANs before transferring ownership.

The command below uses `--force-conflicts` once for these other resources, after the
CrdbCluster check has passed. Later upgrades should omit the flag.

```bash
helm upgrade --install ${RELEASE_NAME} ./cockroachdb-operator/charts/cockroachdb \
  --namespace ${NAMESPACE} \
  --force-conflicts \
  --values ./manifests/values.yaml
```

Verify the install succeeded:

```bash
kubectl get crdbcluster $CRDBCLUSTER -n $NAMESPACE \
  -o jsonpath='{.metadata.labels.app\.kubernetes\.io/managed-by}'
# Expected: Helm
```

After adoption is verified, consider performing a rolling restart to confirm chart-managed
configuration is fully applied. Post-migration there is no StatefulSet, so trigger a rolling
restart via `helm upgrade` with an updated timestamp:

```bash
helm upgrade ${RELEASE_NAME} ./cockroachdb-operator/charts/cockroachdb \
  --namespace ${NAMESPACE} \
  --set cockroachdb.crdbCluster.timestamp="$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
  --values ./manifests/values.yaml
```

Verify generation matching to confirm the operator has reconciled:

```bash
kubectl get crdbcluster $CRDBCLUSTER -n $NAMESPACE \
  -o jsonpath='{.metadata.generation} {.status.observedGeneration}'
# Both values should match
```

## Insecure clusters

Migration preserves insecure mode. It skips certificate migration and emits an
`InsecureClusterMigration` warning. The exported values disable TLS and certificate
management. If you build values by hand, include all of these settings:

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

Do not enable TLS during migration or adoption, or toggle it on this running cluster
later. Changing TLS mode on a running cluster is unsupported and can leave it unhealthy
and unrecoverable. Plan a separate move to a secure cluster if TLS is required.

After handoff, check `spec.tlsEnabled` is still `false`, all pods are Ready, no certificate
resources or rotation jobs were introduced, and SQL through the original public Service
works with `--insecure`. Apply the same checks to the restored source after rollback.

## Certificates during and after migration

Migration and ongoing certificate renewal are separate steps. Confirm your source mode
before starting, then review the exported TLS values before chart adoption.

| Source | During controller migration | Renewal after handoff and adoption |
|---|---|---|
| Legacy operator built-in self-signed | Reuses the CA and creates new node/client Secrets, leaving source Secrets for rollback | Export keeps external references. You must arrange renewal |
| cert-manager | Adds join names to the node Certificate, keeping its issuer and existing names | Existing cert-manager Certificates continue renewal. Controller export keeps external Secret references |
| External or provided certificates without a CA key | Leaves the certificates unchanged. The owner must supply the join names | The certificate owner remains responsible for renewal |

A CA signing key can make the controller choose self-signed regeneration even for a
provided source. That does not configure ongoing renewal: legacy operator sources export
as external certificates. Do not enable the chart self-signer just because a CA key is
present.

### Secrets nodes use after migration

| Source | Node and client Secrets after migration | Rollback |
|---|---|---|
| Built-in self-signed | New `<name>-node-secret` and `<name>-client-secret`. The source `<name>-node` and `<name>-root` stay for rollback | The new Secrets are removed and the source ones stay |
| `nodeTLSSecret` and `clientTLSSecret` | The named Secrets, mounted as they are and never reissued | Kept |
| cert-manager | The Secrets the existing Certificates write, under their existing names | Kept |

A TLS source also gets the `<name>-ca-crt` ConfigMap unless it already exists. Rollback
deletes it only when migration created it, identified by
`crdb.cockroachlabs.com/migration-created=true`. A preexisting ConfigMap is kept.

`<name>-ca-crt` is copied once from the source CA and isn't refreshed. Update it yourself
after the CA is renewed, before nodes pick up certificates signed by the new CA.

### Certificate checks during migration

Self-signed migration regenerates node/client certificates using the existing CA and
preserves custom DNS and IP SANs. The node and root client certificates are reissued for
at most one year, capped by the CA expiry. Refresh root client certificates you copied out
of the cluster. With cert-manager, migration adds join names to the node Certificate,
keeps its existing names and issuerRef, and waits for issuance.
External certificates are never rewritten and must already cover
`<name>-join.<namespace>.svc.<domain>` (normally `cluster.local`).

The controller validates mounted certificate/key pairs, trust, validity, usages and the
join FQDN at certificate migration completion. It rechecks the join name before subsequent
replacements. Self-signed and cert-manager sources can return to certificate migration if
rotation drops that name. External certificate owners must reissue it. Rehearse the first replacement join with
your TLS setup.

### Legacy operator self-signed lifecycle

`<name>` is the source CrdbCluster name. The CA normally stays in `<name>-ca`.
Controller migration creates `<name>-node-secret`, `<name>-client-secret` and the CA
ConfigMap `<name>-ca-crt` for replacement pods. It keeps the source certificate Secrets
for rollback. After handoff, the parent references the replacement Secrets and ConfigMap
as external certificates.

The legacy operator's built-in signer is different from the Helm chart self-signer.
Its CA lacks the chart metadata needed to restore rotation. Exporting and adopting the
chart does not set up renewal for these certificates. Check their expiry and arrange
renewal yourself, preserving the CA and required SANs.

### Check certificate management after adoption

The migrated CrdbCluster stores certificate references as `externalCertificates`.
Legacy operator sources export with external certificate references; the built-in
signer does not become the chart self-signer. Automatic restoration of chart signer
settings and CA ConfigMap sync applies to recognized legacy Helm self-signer sources.
See the [Helm self-signer lifecycle](../helm/controller_migration.md#legacy-helm-self-signer-lifecycle)
for that separate path.

Review the generated TLS values before adoption:

- **Legacy operator built-in signer**: Keep the exported external references and arrange
  renewal yourself, preserving the CA and required SANs.
- **cert-manager source**: Existing Certificates continue renewing their Secrets while the
  exported values use external references. To manage those Certificates through the new
  chart, first carry over their issuer, Secret names, durations, custom DNS names and IPs
  into `cockroachdb.tls.certManager`. Review the rendered Certificates before changing mode.
- **External or unrecognized source**: Keep `cockroachdb.tls.externalCertificates` and
  arrange ongoing renewal with the certificate owner. Do not enable a new CA over an
  existing cluster.

For an externally managed source, check the actual expiry of both certificates after
migration and schedule renewal. These commands print certificate dates, not private keys:

```bash
for SECRET in $(kubectl get crdbcluster.v1beta1.crdb.cockroachlabs.com "$CRDBCLUSTER" -n "$NAMESPACE" \
  -o jsonpath='{.spec.template.spec.certificates.externalCertificates.nodeSecretName} {.spec.template.spec.certificates.externalCertificates.rootSqlClientSecretName}'); do
  echo "$SECRET"
  kubectl get secret "$SECRET" -n "$NAMESPACE" -o jsonpath='{.data.tls\.crt}' \
    | openssl base64 -d -A | openssl x509 -noout -dates
done
```

Check the current parent and node certificate references:

```bash
kubectl get crdbcluster.v1beta1.crdb.cockroachlabs.com "$CRDBCLUSTER" -n "$NAMESPACE" \
  -o jsonpath='{.spec.template.spec.certificates}{"\n"}'
kubectl get crdbnodes -n "$NAMESPACE" \
  -o custom-columns='NAME:.metadata.name,CA:.spec.certificates.externalCertificates.caConfigMapName,NODE:.spec.certificates.externalCertificates.nodeSecretName,CLIENT:.spec.certificates.externalCertificates.rootSqlClientSecretName'
kubectl get pods -n "$NAMESPACE" \
  -o custom-columns='NAME:.metadata.name,CA_CONFIGMAPS:.spec.volumes[*].configMap.name,PROJECTED_CA_CONFIGMAPS:.spec.volumes[*].projected.sources[*].configMap.name'
```

Legacy operator sources continue using the migration's `<name>-ca-crt` ConfigMap after
chart adoption. Keep it while referenced, and update it when the external CA is renewed.
Check the parent spec, CrdbNodes, live pod volumes and other workloads or GitOps
configuration before cleaning up certificate resources. Certificate mode changes belong
after migration and should be rehearsed separately.

## Multi-Cluster Migration Strategy

When you have multiple clusters across different namespaces, you can migrate them one at a
time while the legacy operator continues managing the remaining v1alpha1 clusters.

### Deployment pattern

Deploy one namespace-scoped CockroachDB Operator per namespace using `watchNamespaces`.
This provides clean isolation where each operator manages only the clusters in its namespace.
A single operator watching multiple namespaces can also work, but has less isolation.

```
ns-prod-1:  legacy operator cluster-1  +  CockroachDB Operator (scoped, migration=true)
ns-prod-2:  legacy operator cluster-2  (no CockroachDB Operator yet)
ns-staging: CockroachDB Operator (scoped, migration=false)  +  fresh v1beta1 cluster
```

### The migration flag rule

At least one CockroachDB Operator must have `migration.enabled=true` as long as any v1alpha1
cluster exists anywhere in the Kubernetes cluster. One migration-enabled operator can support
all namespaces, but it must remain running and reachable. Operators that manage only fresh
v1beta1 clusters do not need the migration flag as long as another operator has it enabled.

### Creating new v1beta1 clusters during migration

You can create new v1beta1 clusters at any point during migration. Install a namespace-scoped
CockroachDB Operator in a separate namespace and create v1beta1 CrdbCluster resources directly.
The mutating webhook automatically injects `crdb.io/skip-reconcile=true` so the legacy
operator ignores these clusters.

These clusters remain compatible with Helm and other Server-Side Apply clients during
coexistence.

The new operator does not need `migration.enabled=true` as long as another operator already
has it enabled.

### Migrated clusters and the legacy operator

After a cluster is migrated, the legacy operator can remain running while it manages other
v1alpha1 clusters. Migrated clusters can be updated by the CockroachDB Operator or CockroachDB
Helm chart independently of the legacy operator.

Keep `crdb.io/skip-reconcile=true` on migrated clusters while the legacy operator is still
running. Removing it allows the legacy operator to reconcile the migrated cluster and can
recreate StatefulSet resources that conflict with CrdbNodes.

### Gradual migration across namespaces

**Phase 1 - Migrate the first namespace:**

1. Install a CockroachDB Operator in ns-1 with `migration.enabled=true`,
   `cloudRegion=<ns-1-region>`, `appLabel=cockroachdb-operator`, and `watchNamespaces=ns-1`.
2. Apply `skip-reconcile` and cloud annotations on the cluster in ns-1.
3. Label the cluster with `crdb.io/migrate=start`.
4. Monitor migration, delete StatefulSet when in Finalization.
5. The cluster in ns-2 continues running under the legacy operator, unaffected.

**Phase 2 - Deploy fresh v1beta1 clusters in other namespaces (optional):**

1. Install a CockroachDB Operator in ns-3 with `migration.enabled=false`,
   `cloudRegion=<ns-3-region>`, `appLabel=cockroachdb-operator`, and `watchNamespaces=ns-3`.
   This works because the ns-1 operator already has the conversion webhook configured.
2. Create v1beta1 clusters directly using the CockroachDB Helm chart.

**Phase 3 - Migrate the remaining namespace:**

1. Either expand ns-1's `watchNamespaces` to include ns-2, or install a new CockroachDB Operator
   in ns-2 with `migration.enabled=true`, `cloudRegion=<ns-2-region>`,
   `appLabel=cockroachdb-operator`.
2. Apply `skip-reconcile` and cloud annotations on the cluster in ns-2.
3. Migrate the cluster.

**Phase 4 - Finalize (all v1alpha1 clusters are migrated):**

1. Verify every migration has reached `Complete`.
2. Remove the legacy operator (no v1alpha1 clusters remain).
3. Remove `skip-reconcile` and the remaining migration/coexistence metadata from v1beta1 clusters.
4. Patch `storedVersions` to remove v1alpha1 (see Step 13).
5. Upgrade the CockroachDB Operator with migration disabled (see Step 14).

### What happens if things go wrong

| Scenario | What happens | What to do |
|---|---|---|
| The only migration-enabled operator goes down | v1alpha1 reads/writes fail (webhook unreachable). v1beta1 clusters are unaffected. | Restart the operator or enable migration on another operator. |
| Migration flag removed before `storedVersions` is patched | v1alpha1 requests fail because conversion is no longer available. v1beta1 clusters continue to work. | Re-enable migration, or patch `storedVersions` if all clusters are migrated. |
| `storedVersions` patched while v1alpha1 clusters still exist | The API server may not serve those objects correctly. | Do not patch `storedVersions` until all v1alpha1 clusters are migrated. If this happens, restore by adding v1alpha1 back to `storedVersions`. |
| Legacy operator removed while v1alpha1 clusters remain | Those clusters become unmanaged. | Reinstall the legacy operator or migrate them first. |
| `skip-reconcile` removed on a migrated cluster while legacy operator runs | Legacy operator recreates StatefulSet, conflicting with CrdbNodes. | Re-apply `skip-reconcile` immediately. Delete the recreated StatefulSet. |

## Step 12: Uninstall the Legacy Operator and Clean Up Coexistence

Perform this step only after every cluster managed by the legacy operator has completed migration
and is healthy. A migrated cluster can be adopted by Helm in Step 11 while other v1alpha1 clusters
are still migrating. Do not uninstall the legacy operator or remove `skip-reconcile` until no
v1alpha1 clusters remain.

The legacy operator v2.18.7 bundle creates the Deployment, webhook Service, ServiceAccount,
ClusterRole, ClusterRoleBinding, and admission webhook configurations listed below. Remove those
resources after the last legacy-operator cluster is migrated:

```bash
kubectl delete deployment cockroach-operator-manager \
  -n cockroach-operator-system --ignore-not-found
kubectl delete service cockroach-operator-webhook-service \
  -n cockroach-operator-system --ignore-not-found
kubectl delete serviceaccount cockroach-operator-sa \
  -n cockroach-operator-system --ignore-not-found

kubectl delete clusterrole cockroach-operator-role --ignore-not-found
kubectl delete clusterrolebinding cockroach-operator-rolebinding --ignore-not-found

kubectl delete validatingwebhookconfiguration \
  cockroach-operator-validating-webhook-configuration --ignore-not-found
kubectl delete mutatingwebhookconfiguration \
  cockroach-operator-mutating-webhook-configuration --ignore-not-found
```

Do not delete `crdbclusters.crdb.cockroachlabs.com`,
`crdbnodes.crdb.cockroachlabs.com`, or `crdbtenants.crdb.cockroachlabs.com`.
Those CRDs are shared by the migrated CockroachDB Operator resources. Step 13 removes
`v1alpha1` from `storedVersions` after every v1alpha1 cluster has been migrated.

### Remove coexistence and migration metadata

Existing migrated clusters and any fresh v1beta1 clusters created during coexistence have the
`crdb.io/skip-reconcile=true` label so the legacy operator ignores them. After the legacy operator
and its webhooks are removed, this label is no longer required. The completed migration label and
the region/provider annotations used for v1alpha1 conversion can also be removed:

```bash
kubectl label crdbclusters.v1beta1.crdb.cockroachlabs.com $CRDBCLUSTER \
  crdb.io/skip-reconcile- \
  crdb.io/migrate- \
  -n $NAMESPACE

kubectl annotate crdbclusters.v1beta1.crdb.cockroachlabs.com $CRDBCLUSTER \
  crdb.cockroachlabs.com/cloudProvider- \
  crdb.cockroachlabs.com/regionCode- \
  -n $NAMESPACE
```

Repeat this cleanup for every migrated or coexistence-created v1beta1 cluster. The operator
already removes its temporary annotations when migration reaches `Complete`. Do not remove
migration metadata manually while a migration is active.

## Step 13: Patch storedVersions

After all v1alpha1 clusters across the entire Kubernetes cluster have been migrated and the
legacy operator has been removed, patch the CRD's `storedVersions` to remove v1alpha1.

Do not do this until every v1alpha1 cluster has completed migration and the legacy operator
is stopped.

```bash
# Verify all clusters are migrated
kubectl get crdbclusters --all-namespaces \
  -o jsonpath='{range .items[*]}{.metadata.namespace}/{.metadata.name}: phase={.status.migration.phase}{"\n"}{end}'

# Patch storedVersions
kubectl patch crd crdbclusters.crdb.cockroachlabs.com \
  --subresource=status \
  --type=json \
  -p='[{"op":"replace","path":"/status/storedVersions","value":["v1beta1"]}]'

# Verify
kubectl get crd crdbclusters.crdb.cockroachlabs.com \
  -o jsonpath='{.status.storedVersions}'
# Expected: ["v1beta1"]
```

## Step 14: Disable Migration Mode

After `storedVersions` is patched, upgrade the CockroachDB Operator with migration disabled.
When the operator restarts without the flag, it sees that `storedVersions` no longer contains
v1alpha1, sets v1alpha1 `served=false` on the CRD, and removes the conversion webhook.
No manual patch to `spec.conversion` is needed. Changing the migration flag triggers the
required restart. If migration was already disabled, restart the operator Deployment
after patching `storedVersions`.

```bash
helm upgrade crdb-operator ./cockroachdb-operator/charts/operator \
  --reuse-values \
  --set migration.enabled=false
```

After this:
- v1alpha1 is no longer served by the API server.
- The conversion webhook is removed from the CRD.
- All existing v1beta1 clusters continue working normally.

> **Do not disable migration mode** while the legacy operator is still running or before
> `storedVersions` is patched. Keep a reachable migration-enabled operator until then;
> otherwise v1alpha1 requests can fail because the conversion endpoint is unavailable.

### Removing the CockroachDB Operator

If you uninstall the CockroachDB Operator without completing Steps 13-14 first, the CRD
still has the conversion webhook pointing at a dead endpoint. Any v1alpha1 API calls
(including `kubectl get crdbclusters` if the API server chooses v1alpha1 as the storage
version) will fail with webhook connection errors. To recover:

1. Patch `storedVersions` to remove v1alpha1 (Step 13).
2. Remove the conversion webhook from the CRD manually:
   ```bash
   kubectl patch crd crdbclusters.crdb.cockroachlabs.com \
     --type=json \
     -p='[{"op":"remove","path":"/spec/conversion/webhook"},{"op":"replace","path":"/spec/conversion/strategy","value":"None"}]'
   ```
3. Optionally remove v1alpha1 from the CRD's `spec.versions` if no v1alpha1 clusters remain.

Always complete Steps 13-14 before uninstalling the operator.

### Migration metadata

Do not edit or remove operator-managed migration annotations while migration is active. The
operator removes temporary annotations when migration reaches `Complete`. Fresh v1beta1 clusters
do not receive this temporary metadata and remain compatible with Server-Side Apply configuration
managers such as Helm and Argo CD.

---

## Services, PDBs and join during migration

Replacement pods keep the source pod labels, so the legacy operator's Services keep routing to
them. `<name>` is the CrdbCluster name.

| Resource | During migration | Handoff | Rollback |
|---|---|---|---|
| Discovery Service `<name>` | Unchanged. Replacements register in it, so pod DNS keeps working | The operator adopts it as its node Service under the same name | Unchanged |
| Client Service `<name>-public` | Ports point at the named `grpc` and `sql` targets | The saved-ports annotation is removed | Original ports are restored |
| Join Service `<name>-join` | Created for replacements to join through. It lists only ready pods | Kept, owned by the CrdbCluster | Deleted |
| PDB | `<name>` covers replacements through finalization | Create `<name>-pdb`, then remove the source PDB. Verify the new budget and selector | Source PDB is unchanged |
| Role and RoleBinding `<name>` | Existing source resources are reused. Missing resources are created and marked | Kept | Only resources created by migration are deleted |
| ClusterRole and ClusterRoleBinding `<namespace>-<name>` | Created and marked if absent. Existing resources are reused | Kept | Only resources created by migration are deleted |

Rollback deletes certificate and RBAC resources only when they carry
`crdb.cockroachlabs.com/migration-created=true`. Existing source resources are kept.
Do not add or remove this label yourself. At completion the controller removes it from
resources the migrated cluster keeps. At handoff it releases CrdbCluster spec ownership so the chart can
adopt it without `--force-conflicts`.

Before moving to the next ordinal, the controller checks node and pod readiness,
discovery registration, storage adoption and cluster health. It also checks that client
Service ports resolve to the pod's listeners, including the SQL listener. During rehearsal
and after adoption or rollback, connect through the original public Service and port. Use
TLS hostname verification for secure clusters and `--insecure` only for insecure clusters.

---

## Start flags

Migration parses the source start command without executing it. An explicit `--locality`
or custom `--join` is preserved. A join to the source's own pods moves to the migration
join Service. A preserved custom join must include every address the replacements need.
Repeated join flags merge only when their values can be reproduced. Unsupported repeated
flags, missing variables, pipelines or conditionals block before scale-down. A dynamic
expression runs again inside each replacement, so verify its dependencies and resulting
arguments during rehearsal. Ports, TLS paths, logging and WAL settings move to operator
fields or are regenerated rather than being copied as competing flags.

Before chart adoption, review `cockroachdb.crdbCluster.startFlags.upsert` in the exported
values. It preserves explicit source flags, including custom `--join` overrides. The
operator rebuilds normal discovery from `regions[].namespace` and `regions[].domain`;
`cockroachdb.clusterDomain` must match this Kubernetes cluster's DNS domain for certificate
generation. You can change or remove preserved overrides after migration. Before removing
a custom `--join`, verify that the region join Services are reachable and covered by the
node certificates. Apply the revised values through Helm and verify pod readiness and SQL
connectivity; changing start flags can trigger a rolling restart.

---

## Controlling Migration (Stop / Resume / Rollback)

The `crdb.io/migrate` label records intent and progress:

| Value | Meaning |
|---|---|
| `start` | Request forward migration, or resume a repaired forward stop |
| `in-progress` | Forward migration is active. The controller sets this value |
| `stop` | Request a pause |
| `stopped` | Paused. Read `phase`, `settledPhase` and the message before resuming |
| `rollback` | Request rollback, or resume a paused rollback |
| `failed` | Terminal failure. Do not relabel to retry |
| `complete` | Handoff finished. Verify health before removing this label |

Read `status.migration.phase`, `settledPhase`, `message`, `lastError` and the source label
before changing intent. A prerequisite or source read failure retries on its own. A health
wait that exceeds its deadline pauses with `Stopped`, label `stopped` and a
`MigrationPaused` event. A requested stop also sets `Stopped`, but does not emit that
event. Repair the reported cause before resuming.

| State | What to do |
|---|---|
| `Init` prerequisite failure | Fix the named topology, flag, certificate or permission. Retries do not need another `start`. Check the CrdbCluster status and events for the reported prerequisite. |
| `MigrationSourceUnreadable` event | Restore API, permission or conversion-webhook access. A failed read is not a rollback request. |
| Healthy forward wait | Wait for readiness, service membership and database replication. Do not repeatedly relabel. |
| Forward `Stopped` | Repair and verify health, then use `start` to continue or `rollback` to reverse. |
| Source replica contention | Stop the other writer, scale the source to the unreplaced count reported in the message, then resume. |
| `phase=Stopped`, `settledPhase=RollbackInProgress`, or active rollback | Use `rollback` to continue. `start` conflicts with its direction and pauses it again. A new `start` is valid only after rollback is validated. |
| `RollbackSourceLost` | Preserve replacements and PVCs. Restore a compatible StatefulSet. The controller waits rather than deleting the only running members. |
| `Failed` with `MigrationSourceLost` | The source disappeared before handoff was ready. Preserve running resources and PVCs, do not relabel, and contact Cockroach Labs. |
| `DegradedHandoff` | The source is gone and handoff completed with unhealthy replacements. Check database health and contact Cockroach Labs. Automated rollback is unavailable. |
| `UnsupportedStartFlags` or `UnreadableStartCommand` | Resolve the reported source command issue before any pod is removed. Do not repeatedly relabel an active wait. |
| `DynamicStartFlag` | A flag runs a command in every replacement. Verify its dependencies and resolved value during rehearsal. This warning alone does not stop migration. |
| Complete handoff | Automated rollback is unavailable. Contact Cockroach Labs for recovery guidance. |

### Pause and resume

```bash
kubectl label crdbclusters.v1alpha1.crdb.cockroachlabs.com $CRDBCLUSTER crdb.io/migrate=stop --overwrite -n $NAMESPACE
```

Wait for both phase `Stopped` and source label `stopped`. Existing replacements keep
serving. An in-flight replacement gap is repaired before the pause is recorded.

```bash
# Resume a repaired forward migration.
kubectl label crdbclusters.v1alpha1.crdb.cockroachlabs.com $CRDBCLUSTER crdb.io/migrate=start --overwrite -n $NAMESPACE
# Resume a paused rollback instead.
kubectl label crdbclusters.v1alpha1.crdb.cockroachlabs.com $CRDBCLUSTER crdb.io/migrate=rollback --overwrite -n $NAMESPACE
```

### Rollback

Request rollback while the source StatefulSet still exists and handoff is incomplete:

```bash
kubectl label crdbclusters.v1alpha1.crdb.cockroachlabs.com $CRDBCLUSTER crdb.io/migrate=rollback --overwrite -n $NAMESPACE
```

Removing the migrate label also requests rollback during an active migration. Prefer the
explicit value so intent is visible.

Rollback drains and retires replacements one at a time and restores source ordinals on
their original data, logs and WAL claims. It keeps restored-claim metadata and refuses to
scale up over a missing, terminating, differently bound or unexpectedly owned claim. Do
not recreate empty storage, strip finalizers or edit recovery annotations to bypass that
check. If a drain keeps failing, rollback pauses after its health deadline. Repair the
cause and resume with `rollback`.

Cleanup removes the join Service and certificate/RBAC resources marked as created by
migration. It restores original Service ports and logging configuration. Source Secrets,
including custom `NodeTLSSecret` and `ClientTLSSecret` ones, stay in place. Source RBAC
and the PDB are preserved. Verify their original permissions, budget and selector after rollback.

After replacements are gone and the source is fully ready, cleanup removes migration
intent. Validation also checks the headless Service has no v1beta1 owner, then removes
`skip-reconcile` to return control to the legacy operator. Status remains as an audit trail
with both `phase` and `settledPhase` equal to `RollbackComplete`. If validation is waiting,
repair the reported resource rather than removing `skip-reconcile` yourself.

The shared `crdb-critical` PriorityClass remains after rollback. Leave it if it existed
before migration or another workload uses it. Otherwise, check all namespaces for pods
referencing it before deleting it. Its presence alone does not mean rollback failed.

---

## Troubleshooting

### "cannot convert protected v1alpha1 CrdbCluster ... Without its migration StatefulSet"

This message can appear transiently during or after finalization when the legacy operator attempts
one last update. It does not indicate a migration failure. Verify that migration reaches
`Complete` and that `crdb.io/skip-reconcile=true` remains set while the legacy operator is running.

### Migration waits for "missing required label: crdb.io/skip-reconcile=true"

The required user opt-in is missing. Apply it. An active wait retries automatically. If
status is Stopped, repair first and then resume in its recorded direction:

```bash
kubectl label crdbcluster $CRDBCLUSTER crdb.io/skip-reconcile="true" -n $NAMESPACE
```

### Migration Stuck at PodMigration

If a node fails health checks for more than 10 minutes, migration auto-pauses to `Stopped`.

```bash
# Check why migration stopped
kubectl get crdbcluster $CRDBCLUSTER -n $NAMESPACE \
  -o jsonpath='{.status.migration.lastError}'

# Check pod status
kubectl describe pod $CRDBCLUSTER-<index> -n $NAMESPACE

# Check CrdbNode status
kubectl get crdbnode $CRDBCLUSTER-<index> -n $NAMESPACE -o yaml

# Resume after fixing
kubectl label crdbcluster $CRDBCLUSTER crdb.io/migrate=start --overwrite -n $NAMESPACE
```

### CrdbCluster Not Found (Operator Migration)

For operator migrations, the v1beta1 CrdbCluster is created via the conversion webhook.
The conversion webhook is only registered when the operator is started with
`--enable-migration` (or `migration.enabled=true` in Helm). If the v1beta1 CrdbCluster
is missing, ensure:

- The CockroachDB Operator was installed with `migration.enabled=true`.
- The webhook service is running and reachable.
- The v1alpha1 CrdbCluster has not been deleted.

```bash
# Check if conversion webhook is configured
kubectl get crd crdbclusters.crdb.cockroachlabs.com -o yaml | grep -A5 conversion
```

### Under-replicated Ranges

The controller retries health checks every 10 seconds. If under-replicated ranges persist for
more than 10 minutes, migration auto-pauses.

```bash
# StatefulSet pods use -c db. Migrated CrdbNode pods use -c cockroachdb.
kubectl exec $CRDBCLUSTER-0 -n $NAMESPACE -c db -- \
  /cockroach/cockroach node status --ranges --format table \
  --certs-dir=/cockroach/cockroach-certs
```

For the direct SQL diagnostic alternative and its `allow_unsafe_internals` and
UA routing requirements, see
[Inspect cluster health manually](../../../cockroachdb-operator/charts/operator/README.md#inspect-cluster-health-manually).

### RBAC Conflicts Between Legacy and CockroachDB Operator

If both operators are fighting over RBAC:
1. Verify `skip-reconcile` is set on the v1alpha1 CrdbCluster.
2. Verify the CockroachDB Operator uses `watchNamespaces` to limit its scope.
3. Check that ClusterRole names do not collide (CockroachDB Operator uses `{namespace}-{name}`).

```bash
# Check namespace-scoped RBAC
kubectl get role $CRDBCLUSTER -n $NAMESPACE
kubectl get rolebinding $CRDBCLUSTER -n $NAMESPACE

# Check cluster-scoped RBAC (namespace-qualified names)
kubectl get clusterrole ${NAMESPACE}-${CRDBCLUSTER}
kubectl get clusterrolebinding ${NAMESPACE}-${CRDBCLUSTER}
```

### ConfigMap Key Format

The legacy operator uses `logging.yaml` as the key in log-config ConfigMaps. The CockroachDB
Operator uses `logs.yaml`. The migration controller renames this key automatically. On
rollback, the key is renamed back. Verify:

```bash
kubectl get configmap <log-config-name> -n $NAMESPACE -o yaml | grep -E "logging.yaml|logs.yaml"
```

### Operator Fails with "storedVersions: must appear in spec.versions"

You may see an error like:

```text
status.storedVersions[0]: Invalid value: "v1alpha1": must appear in spec.versions
```

This happens when the operator tries to remove v1alpha1 from `spec.versions`, but
`status.storedVersions` still lists it. Kubernetes requires every stored version to remain in
`spec.versions`. A common cause is removing `migration.enabled=true` before patching
`storedVersions` in Step 13.

Fix it by either re-enabling migration temporarily or patching `storedVersions` if all
clusters are already migrated:

```bash
# Option 1: Re-enable migration temporarily
helm upgrade crdb-operator ./cockroachdb-operator/charts/operator \
  --reuse-values \
  --set migration.enabled=true

# Option 2: Patch storedVersions if all clusters are already migrated
kubectl patch crd crdbclusters.crdb.cockroachlabs.com \
  --subresource=status --type=json \
  -p='[{"op":"replace","path":"/status/storedVersions","value":["v1beta1"]}]'
```

After patching, restart the operator without the migration flag as described in Step 14.

### Verifying v1alpha1 Storage State

Running `kubectl get crdbclusters.v1alpha1.crdb.cockroachlabs.com` does not prove that
objects are still stored as v1alpha1. The conversion webhook can convert v1beta1 objects back
to v1alpha1 on read.

The reliable indicator is the CRD's `status.storedVersions` field, but this is a high-water
mark. Kubernetes adds versions when objects have been stored in that version, but does not
remove them automatically. A version listed in `storedVersions` means objects were stored in
that version at some point, not necessarily that they still are.

Before patching `storedVersions`:

1. Ensure the CockroachDB Operator has reconciled every CrdbCluster at least once.
2. Verify no legacy operator remains running.
3. Patch `storedVersions` as described in Step 13.

## Limits to verify during rehearsal

- A retained PVC can still exist when scaling back to a former node name. Check its data
  and intended reuse before scaling up. This guide does not validate that recovery path.
- A source with `runAsUser` but no `fsGroup` can roll once at handoff. Legacy operator
  sources already supply both. Compare pod security settings before and after adoption.
- Rollback removes the CA ConfigMap only when migration created and labeled it.
  Preexisting ConfigMaps stay in place. Review references before any manual cleanup.
- A restored source pod that never becomes Ready can keep rollback validation waiting.
  Diagnose that pod rather than assuming repeated rollback labels will repair it.
- Namespace scope is not a substitute for checking every deployment's webhook and RBAC.
  Contention detection reports a writer after it acts. It does not prove all writes drained.
