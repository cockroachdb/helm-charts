# Automatic Migration from Helm StatefulSet to CockroachDB Operator

> These guides are available to read in the
> [CockroachDB documentation](https://www.cockroachlabs.com/docs/stable/migrate-cockroachdb-kubernetes-helm).

This guide covers the automatic migration of a CockroachDB cluster managed via a Helm
StatefulSet to the CockroachDB Operator.

The migration controller replaces source pods one at a time and carries over the cluster
configuration and certificates. You check health, complete the handoff and adopt the chart.

**Handoff** is the end of controller migration: after you verify finalization and delete
the source StatefulSet, the controller enables normal reconciliation and marks migration
`Complete`. Request rollback before deleting the source StatefulSet; the migration
controller cannot roll back after handoff. **Helm adoption** is the separate step that
follows, using exported values to manage the migrated cluster through the chart.

## Compatibility

Before starting migration, verify your cluster configuration is supported.

| Feature | Supported | Notes |
|---|---|---|
| Self-signer certs (Helm built-in) | Yes | Existing CA reused. Custom DNS/IP SANs preserved and join names added |
| cert-manager certs | Yes | Certificate CR updated with join service DNS SANs. Issuer references preserved |
| User-provided certs (`tls.certs.provided`) | Yes | Without a CA signing key, pods mount the supplied Secrets and certificates must already cover the join names. A source with `ca.key` can use self-signed regeneration |
| Insecure clusters (TLS disabled) | Yes | Detected from `--insecure` flag. Cert migration phase skipped entirely |
| WAL failover (dedicated PVC) | Yes | Detected from `failover*` VolumeClaimTemplate and `--wal-failover` flag |
| Dedicated logs PVC (`logsdir` / `logs-dir`) | Yes | VolumeClaimTemplate and mount path preserved as LogsStore |
| PCR (virtualized/standby) | Yes | Detected from `{name}-init` Job args (`--virtualized`, `--virtualized-empty`) |
| Custom service account | Yes | Preserved with `create: false` |
| Custom start flags | Partial | Known operator-managed flags (`--join`, `--listen-addr`, `--certs-dir`, etc.) are excluded. All others preserved in `startFlags.upsert` |
| Multi-region | Yes | Requires `migration-regions` annotation with all regions before starting |
| Ingress (UI/SQL) | Partial | Existing ingress continues working during migration (service names preserved). Config saved as annotation for reference. Ingress resources themselves are not created or modified. Manual Helm adoption required |
| ServiceMonitor / PodMonitor | No | Not handled by migration. Must be recreated manually after migration to match new pod labels |
| NetworkPolicy | No | Must be recreated manually if present |
| Log-config Secret | Yes | Automatically converted to ConfigMap (operator uses ConfigMaps, Helm uses Secrets) |

## Before You Begin

Rehearse on a separate test cluster first, even when the real cluster is a development
cluster. Use a similar topology, storage, TLS setup and chart values. Complete migration
and chart adoption there, then compare the resulting pods and configuration with the
source before touching the real cluster.

Do not upgrade the CockroachDB Operator or change the database image, version, replicas or chart
configuration while migration is unfinished. Mid-migration upgrades are not supported.
Finish migration or roll it back first. Pause GitOps or other writers that could restore
source replicas or overwrite certificate changes.

Migration preserves the source `--cluster-name` and `--disable-cluster-name-verification`
settings, including a cluster name evaluated by the source shell command. Keep these
settings when adopting the exported values so replacement pods can join the same cluster.

Before starting:

- Check database health as well as pod readiness. No rollout, image or version change may
  be pending. Migration uses the source StatefulSet template, not an unfinished rollout.
- Resolve the full region topology and include the region this operator reconciles.
  For a single-region source, the region comes from the StatefulSet's
  `crdb.cockroachlabs.com/regionCode` annotation, then the source `--locality` region
  tier, then the operator's `cloudRegion`. No annotation is needed when locality or
  `cloudRegion` already resolves the correct region. For cloud deployments, also set
  the StatefulSet's `crdb.cockroachlabs.com/cloudProvider` annotation to your provider,
  such as `gcp`, `aws` or `azure`. Leave it unset for on-premises clusters.
  Multi-region sources require the full `crdb.cockroachlabs.com/migration-regions`
  JSON topology with each region's provider; the standalone `regionCode` and
  `cloudProvider` annotations override only the first region.
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
- Plan retention of source Helm resources before migration. See
  [resources to preserve before Helm adoption](#resources-to-preserve-before-helm-adoption).
  A same-release chart upgrade can delete resources that migration left working.
- The operator needs `get` on `cert-manager.io/certificates` to discover certificate
  ownership when cert-manager CRDs are installed, including self-signed sources and
  Helm-provided certificates. cert-manager sources also need `patch` to add join names.
  Insecure sources do not need it. Use the operator chart or standalone bundle from
  this release; both grant these permissions.

If legacy operators also manage clusters in this Kubernetes cluster, rehearse namespace
scoping during installation. Check `WATCH_NAMESPACE` (`watchNamespaces` in chart values),
webhooks and RBAC for each deployment, especially when several legacy operators run.
Contact Cockroach Labs if their scopes or coexistence behavior are unclear.

Install the CockroachDB Operator with `migration.enabled=true` (`--enable-migration`).
If legacy-operator clusters exist elsewhere in the Kubernetes cluster, keep a reachable
migration-enabled operator for their conversion webhook until coexistence cleanup finishes.

Verify the following before starting migration:

- [ ] CockroachDB cluster is managed by a Helm StatefulSet (not the legacy operator).
- [ ] All StatefulSet pods are Running and Ready with no pending rolling updates, scale operations, or evictions.
- [ ] `kubectl` and `helm` are installed and configured with access to the target cluster.
- [ ] For multi-region clusters: cloud region and provider labels are applied to K8s nodes (see Step 2).
- [ ] For multi-region clusters: `migration-regions` annotation is prepared with the full topology.
- [ ] You have reviewed the Compatibility table above and confirmed your configuration is supported.
- [ ] **If using `tls.certs.provided` (user-provided certs)**: Your certificates include join
  service DNS SANs (`{cluster-name}-join`, `{cluster-name}-join.{namespace}`,
  `{cluster-name}-join.{namespace}.svc.cluster.local`). The controller cannot regenerate
  certificates when the CA private key is not available.
- [ ] **If using `tls.certs.provided` with `tls.certs.tlsSecret: false`**: Add Kubernetes TLS
  keys (`tls.crt`, `tls.key`, `ca.crt`) to the Secrets before starting migration. Keep the
  original raw keys too, since source pods and rollback still need them. The controller
  does not convert these keys and stops at certificate migration if the TLS keys are
  missing. This is not needed for self-signer or cert-manager modes.

## Resources to preserve before Helm adoption

Migration and chart adoption are separate steps. The controller leaves several source
resources working, but a same-release `helm upgrade` deletes resources from the previous
release that the new chart does not render. Ownership annotations alone do not preserve
them. Inventory these resources and rehearse their retention before starting migration.

Save the original release manifest and values in a private backup directory with restricted
permissions. Use `helm get manifest` and `helm get values --all` for your release and
namespace. Also save the live PDB and any resources managed outside Helm. These backups
can contain credentials. Keep them out of source control and escalation packets.

| Source feature or resource | What to arrange before adoption |
|---|---|
| NetworkPolicy | The new chart has no equivalent. Retain or manage it separately and update selectors for replacement pods. Verify SQL and operator access |
| ServiceMonitor / PodMonitor and Prometheus annotations | Recreate monitoring selectors and TLS settings. Copy required scrape annotations into the new pod template or public Service values. Do not assume the legacy `prometheus` values transfer |
| Ingress `<fullname>-ingress` | The new chart uses `ui-<release>` and `sql-<release>`. The exporter looks for the new names and does not translate the legacy ingress. Compare routes, class, annotations and TLS settings explicitly. Retain a separately managed ingress when the new chart cannot express them. Compare rendered routes before switching traffic |
| GCP IAP, BackendConfig and its Secret | There are no matching chart values. Manage these resources separately and retain their Service annotations and ingress configuration if still needed |
| Visus | There is no equivalent chart integration. Arrange a separate deployment and Service if still required |
| Init provisioning and `<fullname>-init` Secret | Existing database data stays on the PVCs, but the new chart does not replay the old provisioning job or render its Secret. Preserve credentials and required SQL separately. Do not rerun provisioning blindly |
| cert-manager Issuer `<fullname>-ca-issuer` and Certificate `<fullname>-ca-cert` | The new chart renders neither. Retain both and the CA Secret, along with the node/client Certificates and their issuer chain. Deleting the Issuer breaks future leaf renewal even while current certificates remain valid |
| Discovery Service `<fullname>` | The new chart leaves this Service to the operator. A same-release upgrade can delete it before reconciliation recreates it, interrupting discovery DNS. Include it in the retention plan and verify its endpoints after adoption |
| Source PDB | Record its selector and budget. It stays through migration and rollback. At handoff, the controller replaces it with a budget of one unavailable pod. Review the replacement budget before allowing voluntary disruptions; the chart has no PDB budget setting |

One retention option is `helm.sh/resource-policy: keep`. Before migration, use the original
chart version and values with a reviewed chart override or post-renderer to record this
annotation on resources that must survive adoption. Verify it in `helm get manifest` and
rehearse the subsequent chart switch. A backup alone does not maintain availability or
certificate renewal. Retained resources need ongoing management outside the new release.
Do not rerun the legacy chart after deleting the source StatefulSet, because it can recreate it.

For cert-manager, keep the existing issuer chain available throughout adoption. The
controller's exported values use external Secret references, so the new chart will not
recreate the old Certificates automatically. Either manage those Certificates separately,
or explicitly configure the new chart's cert-manager mode with the same issuer, Secret
names and SANs. Verify each Certificate is Ready and its Issuer exists after adoption,
then rehearse renewal before considering that path verified.

## Overview

### What the Migration Controller Does Automatically

- Detects cert-manager, self-signer, or user-provided certificates. Self-signed certificates
  are regenerated with their existing custom SANs and the join names. With cert-manager,
  Certificates gain join names while keeping their issuer. User-provided certificates without a CA signing key
  are mounted directly. A source Secret containing `ca.key` can select self-signed regeneration.
- Reuses existing RBAC resources and creates missing ones, with namespace-qualified names
  for cluster-scoped resources. The chart's existing Role and RoleBinding stay unchanged.
- Keeps the chart's PDB until handoff, then replaces it with the operator's PDB. See
  [Services, PDBs and join during migration](#services-pdbs-and-join-during-migration).
- Leaves the source Service selectors alone. Replacements carry the source pod labels.
- Checks replacement node and pod readiness, discovery registration, storage adoption
  and client Service port targets before moving on, along with database health through SQL.
  Verify your client connection through the public Service during rehearsal and after adoption.
- Migrates nodes one at a time (highest index first), verifying cluster health between each node.
- Detects and preserves Ingress configuration as an annotation on the CrdbCluster.
- Migrates `logsdir` / `logs-dir` VolumeClaimTemplate to CrdbNode LogsStore.
- Converts log-config Secret to ConfigMap (Helm uses Secrets, operator uses ConfigMaps).
- Detects Physical Cluster Replication (PCR) configuration from init jobs.
- Detects WAL failover configuration from VolumeClaimTemplates and `--wal-failover` flag.

### What Requires Manual Action

- Cloud region and provider node labels must be applied before deploying the operator.
- The StatefulSet must be manually deleted after finalization to complete migration.
- Helm adoption annotations must be applied to all resources post-migration.
- Remove stale cluster-scoped RBAC only after verifying its actual names and bindings.
- Ingress resources are not annotated for Helm adoption (manual step required).
- ServiceMonitor / PodMonitor resources must be recreated with updated pod labels.

### Migration Phases

```
Init -> CertMigration -> PodMigration -> Finalization -> (user deletes STS) -> Complete
```

| Phase | Duration (approx.) | What happens |
|---|---|---|
| Init | Seconds | Validates prerequisites, creates v1beta1 CrdbCluster, records original replica count |
| CertMigration | Seconds to minutes | Detects cert type, regenerates certs with join service SANs, creates CA ConfigMap, labels existing pods |
| PodMigration | ~5-15 min per node | Scales down one source ordinal, creates its replacement, then waits for readiness and database health (highest index first) |
| Finalization | Seconds | Sets cluster spec (regions, TLS, resources). The source PDB stays until handoff |
| Complete | Seconds (after user deletes STS) | Sets `Mode=MutableOnly`, releases CrdbCluster spec ownership for adoption, records completion and updates migration label to `complete` |

---

## Step 1: Export Environment Variables

Export the necessary environment variables to identify your existing deployment.

```bash
# STS_NAME is the CockroachDB StatefulSet deployed via Helm chart.
export STS_NAME="crdb-test-cockroachdb"

# NAMESPACE is the namespace where the StatefulSet is installed.
export NAMESPACE="default"

# REGION is the region this CockroachDB Operator instance reconciles. It must
# match the source locality or migration-regions entry for this Kubernetes cluster.
export REGION="us-east-1"

# RELEASE_NAME is the Helm release name.
export RELEASE_NAME=$(kubectl get sts $STS_NAME -n $NAMESPACE \
  -o jsonpath='{.metadata.annotations.meta\.helm\.sh/release-name}')
```

## Step 2: Apply Cloud Region and Provider Labels (Multi-Region Only)

CockroachDB uses K8s node labels for locality-based pod placement. These labels must exist
on the nodes before the operator starts scheduling pods.

```bash
# AWS
kubectl label node <node-name> \
  topology.kubernetes.io/region=us-east-1 \
  topology.kubernetes.io/zone=us-east-1a \
  cockroach.io/cloud=aws

# GCP
kubectl label node <node-name> \
  topology.kubernetes.io/region=us-central1 \
  topology.kubernetes.io/zone=us-central1-a \
  cockroach.io/cloud=gcp

# Azure
kubectl label node <node-name> \
  topology.kubernetes.io/region=eastus \
  topology.kubernetes.io/zone=eastus-1 \
  cockroach.io/cloud=azure
```

If the required labels are missing, CrdbNode pods will remain Pending with scheduling errors.

For multi-region clusters, annotate the StatefulSet with the full topology before starting
migration:

```bash
kubectl annotate sts $STS_NAME \
  crdb.cockroachlabs.com/cloudProvider=aws \
  'crdb.cockroachlabs.com/migration-regions=[{"code":"us-east-1","nodes":3,"namespace":"ns-east"},{"code":"eu-west-1","nodes":3,"namespace":"ns-west"}]' \
  -n $NAMESPACE
```

Topology is resolved from the existing cluster configuration, source locality, region
annotations or the operator's resolved region. An empty, malformed or mismatched region
blocks migration before a source pod is removed. Set the full `migration-regions` array
for a multi-region Helm source. A single StatefulSet cannot describe the other regions.
Confirm `cloudRegion` matches an entry in that topology. There is no hard-coded fallback.

## Step 3: Verify StatefulSet Is Stable

All replicas must be ready before migration begins.

```bash
kubectl get sts $STS_NAME -n $NAMESPACE
# READY column must equal DESIRED

kubectl get pods -l app.kubernetes.io/name=cockroachdb -n $NAMESPACE
# All pods must be Running/Ready
```

No ongoing rolling updates, scale operations, or pending evictions.

## Step 4: Install CockroachDB Operator with Migration Enabled

Install the operator with migration enabled. This registers the migration controller
which watches for the `crdb.io/migrate` label.

For a new operator installation managed by GitOps, set `selfSignedOperatorCerts=true` so
the operator manages its webhook TLS Secret instead of Helm generating it during renders.
This setting is separate from database TLS. Choose it before installation. For an existing
operator release, follow the
[certificate ownership switching steps](../../../cockroachdb-operator/charts/operator/README.md#operator-tls-certificates-selfsignedoperatorcerts)
before changing the setting.

```bash
helm upgrade --install crdb-operator ./cockroachdb-operator/charts/operator \
  --set migration.enabled=true \
  --set cloudRegion=$REGION
```

> `migration.enabled=true` on the **operator chart** enables the migration controller and
> registers the conversion webhook.
> Helm StatefulSet migrations do not have a pre-existing legacy operator, so the default
> `appLabel=cockroach-operator` works without modification.

Verify the operator is running and CRDs are available:

```bash
kubectl get pods -l app=cockroachdb-operator -n $NAMESPACE
kubectl get crd crdbclusters.crdb.cockroachlabs.com

# Verify both API versions are accessible (conversion webhook is active)
kubectl api-resources | grep crdbclusters
```

If a legacy operator is also running in the cluster for other namespaces, scope the CockroachDB
Operator to the target namespace and use a distinct `appLabel` to avoid selector conflicts. Without
`watchNamespaces`, the CockroachDB Operator's cluster controller will attempt to reconcile all
CrdbClusters cluster-wide, interfering with clusters managed by the legacy operator.

```bash
helm upgrade --install crdb-operator ./cockroachdb-operator/charts/operator \
  --set migration.enabled=true \
  --set cloudRegion=$REGION \
  --set appLabel=cockroachdb-operator \
  --set watchNamespaces=$NAMESPACE
```

`cloudRegion` must match the region code that migration writes to `spec.regions`. It selects the
region this operator instance reconciles and reports in `status.region`.

When migrating clusters across multiple namespaces, migrate one namespace at a time. Update
`watchNamespaces` (or use a comma-separated list) to include additional namespaces only after
the previous migration is complete.

## Step 5: Start Migration

Initiate the migration by labeling the StatefulSet.

```bash
kubectl label sts $STS_NAME crdb.io/migrate=start -n $NAMESPACE
```

The operator detects this label and begins the migration automatically.

## Step 6: Monitor Migration

### DB Console

The DB Console is accessible via the public service throughout migration.

```bash
# Port-forward to access DB Console
kubectl port-forward svc/${STS_NAME}-public 8080 -n $NAMESPACE
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
kubectl exec $STS_NAME-0 -n $NAMESPACE -c db -- \
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

```bash
kubectl get crdbcluster $STS_NAME -n $NAMESPACE \
  -o jsonpath='{.status.migration.phase} {.status.migration.message}' -w
```

### Watch events

```bash
kubectl get events -n $NAMESPACE \
  --field-selector involvedObject.name=$STS_NAME \
  --sort-by='.lastTimestamp'
```

### Watch CrdbNode creation during PodMigration

```bash
kubectl get crdbnode -n $NAMESPACE \
  -l crdb.cockroachlabs.com/cluster=$STS_NAME -w
```

### Phase-Specific Checks

| Phase | What to watch |
|-------|---------------|
| Init | `kubectl describe crdbcluster $STS_NAME` for MigrationStarted event |
| CertMigration | For self-signer: `kubectl get secret $STS_NAME-node-secret -n $NAMESPACE`. For cert-manager: check the secret name from the Certificate CR (`kubectl get certificate $STS_NAME-node -n $NAMESPACE -o jsonpath='{.spec.secretName}'`). For user-provided certs: your existing secret is used directly. TLS clusters only |
| PodMigration | `kubectl get crdbnode -n $NAMESPACE -w` for one node per pod |
| Finalization | `kubectl get crdbcluster $STS_NAME -o jsonpath='{.status.migration.message}'` for "Finalization complete" |
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

The source `{name}-budget` covers replacements through finalization. At handoff,
the controller creates `{name}-pdb` before deleting the source PDB. The new budget defaults to one
unavailable pod. A custom source budget is not carried over, and the chart has no setting
for changing this budget. Check the live PDB and its selector before allowing voluntary
disruptions.

Rollback leaves the source PDB unchanged. Verify its original budget and selector before
resuming voluntary disruptions.

## Step 7: Delete StatefulSet to Complete Migration

After Finalization completes, the controller stops processing and waits for the user to
delete the StatefulSet. The StatefulSet is intentionally left intact. Once deleted, the
controller sets `spec.mode=MutableOnly` and records `Phase=Complete`.

```bash
# Verify finalization work is settled and the source has zero replicas.
# Mode is still Disabled. Do not delete merely because phase says Finalization.
kubectl get crdbcluster $STS_NAME -n $NAMESPACE \
  -o jsonpath='{.status.migration.phase} {.status.migration.settledPhase}'
# Expected: Finalization Finalization
kubectl get sts $STS_NAME -n $NAMESPACE -o jsonpath='{.spec.replicas}'
# Expected: 0

# Check the finalization message as well
kubectl get crdbcluster $STS_NAME -n $NAMESPACE \
  -o jsonpath='{.status.migration.message}'
# Expected: Finalization complete. Delete the StatefulSet to mark migration complete.

# Delete the StatefulSet to trigger Phase=Complete
kubectl delete sts $STS_NAME -n $NAMESPACE

# Verify completion (may take a few seconds)
# Mode switches to MutableOnly and label changes to "complete" after STS deletion.
kubectl get crdbcluster $STS_NAME -n $NAMESPACE \
  -o jsonpath='{.spec.mode} {.status.migration.phase}'
# Expected: MutableOnly Complete
```

All STS pods are already at replicas=0 by the time Finalization runs (scaled down one by one
during PodMigration). Deleting the StatefulSet object does not evict any running pods.

After handoff, the controller also removes the temporary
`crdb.cockroachlabs.com/migration-storage` annotation from CrdbNodes. This cleanup is
best effort; a leftover annotation is harmless. Do not edit it while migration is running.

## Step 8: Verify Cluster Health Post-Migration

```bash
# All CrdbNodes should be healthy
kubectl get crdbnode -n $NAMESPACE -l crdb.cockroachlabs.com/cluster=$STS_NAME

# All pods running
kubectl get pods -n $NAMESPACE -l crdb.cockroachlabs.com/cluster=$STS_NAME

# Verify data integrity from any CrdbNode pod
# Note: post-migration, the container name is "cockroachdb" (not "db" as in the original STS)
kubectl exec $STS_NAME-0 -n $NAMESPACE -c cockroachdb -- \
  /cockroach/cockroach sql --certs-dir=/cockroach/cockroach-certs \
  -e "SELECT count(*) FROM [SHOW RANGES];"

# Verify under-replicated ranges are zero
kubectl exec $STS_NAME-0 -n $NAMESPACE -c cockroachdb -- \
  /cockroach/cockroach node status --ranges --format table \
  --certs-dir=/cockroach/cockroach-certs
```

### After handoff checklist

- Connect through the original public Service and client port. For secure sources, verify
  TLS against its DNS name and any custom DNS names clients use. For insecure sources,
  confirm TLS and every certificate mode remain disabled.
- Check the discovery Service and its EndpointSlices after Helm adoption. Confirm all
  replacement pods are Ready and registered, and verify the live PDB budget and selector.
- Verify retained ingress, network policies, monitoring and cert-manager resources still
  work after the chart switch. Existing certificates alone do not prove future renewal.

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
- The terminal `crdb.io/migrate=complete` label may stay or be removed after all three
  markers are present.
  Removing the migrate label during an active migration requests rollback.
- Carry custom certificate names into the new values. Use
  `cockroachdb.tls.certManager.additionalDnsNames` and `.additionalIpAddresses`, or
  `cockroachdb.tls.selfSigner.additionalSANs` before enabling the self-signer. Migration
  preserves the issued names, but the next chart render uses its configured values.
- Preserve placement settings during adoption. `podTemplate` affinity overrides the
  generated affinity. It does not merge placement policies.
- Check that the parent template uses the source image. Keep image and configuration
  changes separate from migration.
- Migration carries `persistentVolumeClaimRetentionPolicy.whenDeleted: Retain` into the
  parent template. Removing a migrated node leaves its PVCs and disks. Delete retained
  claims only when their data is no longer needed. The PV reclaim policy controls disk
  deletion. Check retained claims before reusing a node name.

### Verify preserved configurations

```bash
# WAL failover (if configured)
kubectl get crdbcluster $STS_NAME -n $NAMESPACE \
  -o jsonpath='{.spec.template.spec.walFailoverSpec}'

# Logs PVC (if configured)
kubectl get crdbcluster $STS_NAME -n $NAMESPACE \
  -o jsonpath='{.spec.template.spec.logsStore}'

# Log-config ConfigMap (converted from Helm Secret)
kubectl get configmap ${STS_NAME}-log-config -n $NAMESPACE

# TLS certificates (secret names depend on cert type)
# Self-signer: {name}-node-secret, {name}-client-secret (regenerated by migration)
kubectl get secret $STS_NAME-node-secret -n $NAMESPACE
kubectl get secret $STS_NAME-client-secret -n $NAMESPACE
# cert-manager: secrets are managed by cert-manager (names from Certificate CR)
kubectl get certificate $STS_NAME-node -n $NAMESPACE -o jsonpath='{.spec.secretName}'
# User-provided: your original secrets are used directly
# CA ConfigMap (created for all TLS cert types)
kubectl get configmap $STS_NAME-ca-crt -n $NAMESPACE

# PCR (if configured)
kubectl get crdbcluster $STS_NAME -n $NAMESPACE \
  -o jsonpath='{.spec.template.spec.virtualCluster}'
```

## Step 9: Prepare values.yaml for CockroachDB Helm Chart

Before running `helm upgrade`, create a `values.yaml` that matches the migrated CrdbCluster
spec. The CockroachDB Helm chart generates a CrdbCluster from these values, so they must align
with what migration produced.

```bash
# Inspect the migrated CrdbCluster spec to extract the values you need
kubectl get crdbcluster $STS_NAME -n $NAMESPACE -o yaml
```

### Option A: Generate values.yaml automatically

The helper needs `get` access to the migrated CrdbCluster, Services and certificate
Secrets, plus `list` access to Secrets in the cluster namespace to find the source
Helm release settings.

```bash
make bin/migration-helper

./bin/migration-helper export-values \
  --crdb-cluster $STS_NAME \
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

Key values to set (map each to your migrated spec):

| Helm value | Source from migrated CrdbCluster |
|---|---|
| `cockroachdb.crdbCluster.image.name` | `spec.template.spec.image` |
| `cockroachdb.crdbCluster.regions` | `spec.regions` (code, nodes, namespace, cloudProvider) |
| `cockroachdb.tls.enabled` | `spec.tlsEnabled` |
| `cockroachdb.crdbCluster.rbac.serviceAccount.name` | `spec.template.spec.podTemplate.spec.serviceAccountName` (leave empty to use default fullname) |
| `cockroachdb.crdbCluster.podTemplate.spec.containers[0].resources` | `spec.template.spec.podTemplate.spec.containers[0].resources` |
| `cockroachdb.crdbCluster.rollingRestartDelay` | `spec.rollingRestartDelay` |
| `cockroachdb.crdbCluster.clusterSettings` | `spec.clusterSettings` |

If the source Helm release used custom values (affinity, tolerations, nodeSelector, etc.),
carry those forward into the new values.yaml as well.

### Field Mapping Reference

| Source (StatefulSet) | Migrated v1beta1 shape | Helm adoption value |
|---|---|---|
| StatefulSet name | `CrdbCluster.metadata.name` | `k8s.fullnameOverride` |
| Container image | `spec.template.spec.image` | `cockroachdb.crdbCluster.image.name` |
| Data PVC template | `spec.template.spec.dataStore` | `cockroachdb.crdbCluster.dataStore` |
| `--wal-failover=path=...` | `spec.template.spec.walFailoverSpec` | `cockroachdb.crdbCluster.walFailoverSpec` |
| Dedicated `logsdir` PVC | `spec.template.spec.logsStore` | `cockroachdb.crdbCluster.log.logsStore` |
| Helm log Secret | Converted to ConfigMap. `spec.template.spec.loggingConfigMapName` | `cockroachdb.crdbCluster.loggingConfigMapName` |
| Service account | `spec.template.spec.podTemplate.spec.serviceAccountName` | `cockroachdb.crdbCluster.rbac.serviceAccount.name` with `create=false` |
| Explicit `--locality` | `spec.template.spec.startFlags.upsert` | Preserve the flag, or verify and adopt `localityMappings` after handoff |
| Ingress intent | Preserved as annotation on CrdbCluster | `cockroachdb.crdbCluster.service.ingress` |
| PCR config | `spec.template.spec.virtualCluster` | `cockroachdb.crdbCluster.virtualCluster` |

## Step 10: Annotate Resources for Helm Adoption

The migration controller creates resources but does not annotate them for Helm ownership.
For `helm upgrade` to manage these resources, they must carry Helm ownership annotations.
For an insecure cluster, omit `role` and `rolebinding` from the loop because the new
chart renders those resources only with TLS enabled.

```bash
# Namespace-scoped resources
for RESOURCE in \
  "crdbcluster/${STS_NAME}" \
  "serviceaccount/${STS_NAME}" \
  "service/${STS_NAME}-public" \
  "role/${STS_NAME}" \
  "rolebinding/${STS_NAME}"; do
  kubectl annotate "${RESOURCE}" \
    meta.helm.sh/release-name="${RELEASE_NAME}" \
    meta.helm.sh/release-namespace="${NAMESPACE}" \
    -n "${NAMESPACE}" --overwrite
  kubectl label "${RESOURCE}" \
    app.kubernetes.io/managed-by=Helm \
    -n "${NAMESPACE}" --overwrite
done

# Ingress resources (only if your cluster has Ingress resources)
for RESOURCE in \
  "ingress/ui-${RELEASE_NAME}" \
  "ingress/sql-${RELEASE_NAME}"; do
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

The CockroachDB chart manages Ingress resources; the operator and migration controller
leave existing Ingresses unchanged. Only adopt an existing Ingress when its name matches
the chart's `ui-<release>` or `sql-<release>` and the rendered routes match your intent.
Otherwise, retain it under separate management. The exporter reads `ui-<cluster>` and
`sql-<cluster>`; it does not translate the legacy chart's `<fullname>-ingress`.
Review hosts, paths, ingress class, annotations and TLS settings before switching traffic.
The chart's backends use the configured public Service name and port names. When SQL
and gRPC share a port, the SQL Ingress uses the gRPC Service port name because there is
no separate SQL entry. Verify routing through your ingress controller after adoption.

If a rendered resource already exists and its ownership annotations are skipped,
`helm upgrade` will fail with "resource already exists" errors.

Do not annotate the migration-created ClusterRole/ClusterRoleBinding for Helm adoption.
The CockroachDB Helm chart creates its own cluster-scoped RBAC with different names
(`{fullname}-{namespace}-node-reader`). The migration-created ones are stale and should
be deleted (see next step).

## Step 11: Delete Stale Resources

The migration controller creates some resources that do not match what the CockroachDB Helm
chart produces. These must be deleted before or after `helm upgrade`.

```bash
# Migration-created ClusterRole and ClusterRoleBinding use "{namespace}-{name}" naming.
# The CockroachDB Helm chart creates its own with "{fullname}-{namespace}-node-reader" naming.
# Delete the migration versions so they don't accumulate as orphans.
kubectl delete clusterrole ${NAMESPACE}-${STS_NAME} --ignore-not-found
kubectl delete clusterrolebinding ${NAMESPACE}-${STS_NAME} --ignore-not-found

# Inspect source Helm RBAC names before deleting anything. The legacy chart normally
# uses "{fullname}-{namespace}", truncated to 56 characters, not a bare cluster name.
kubectl get clusterrole,clusterrolebinding \
  -l app.kubernetes.io/instance=${RELEASE_NAME} -o yaml

# Handoff should already have removed the source PDB. Expect no output here.
kubectl get pdb ${STS_NAME} ${STS_NAME}-budget -n ${NAMESPACE} --ignore-not-found
# Verify the replacement budget and selector before adoption.
kubectl get pdb ${STS_NAME}-pdb -n ${NAMESPACE} -o yaml
```

Confirm the old binding's subject and `roleRef`, and compare them with the saved release
manifest. Remove only those stale source objects by their actual names after the new
chart's node-reader binding is present. Do not delete RBAC belonging to another release.

## Step 12: Configure LocalityMappings

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
kubectl patch crdbcluster $STS_NAME -n $NAMESPACE --type merge -p '
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

## Step 13: Run Helm Upgrade with CockroachDB Chart

Before upgrading, verify the operator has fully reconciled the migrated cluster. Do not
proceed until `generation` and `observedGeneration` match and all pods are running.

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
kubectl get crdbcluster $STS_NAME -n $NAMESPACE \
  -o jsonpath='{.metadata.generation} {.status.observedGeneration}'
# Both values should match

kubectl get pods -n $NAMESPACE -l crdb.cockroachlabs.com/cluster=$STS_NAME
# All pods should be Running and Ready
```

```bash
helm upgrade ${RELEASE_NAME} ./cockroachdb-operator/charts/cockroachdb \
  --namespace ${NAMESPACE} \
  --force-conflicts \
  --values ./manifests/values.yaml
```

Verify the upgrade succeeded and no resources were duplicated:

```bash
# CrdbCluster should be managed by Helm now
kubectl get crdbcluster $STS_NAME -n $NAMESPACE -o jsonpath='{.metadata.labels.app\.kubernetes\.io/managed-by}'
# Expected: Helm

# ClusterRole should use the new naming convention
kubectl get clusterrole | grep $STS_NAME
# Expected: {fullname}-{namespace}-node-reader (created by Helm chart)
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
kubectl get crdbcluster $STS_NAME -n $NAMESPACE \
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
| Legacy Helm chart self-signer | Reuses the CA, reissues node/client certificates and preserves custom DNS/IP SANs | Export restores signer settings from recognized CA metadata or explicit provided-CA settings in the source Helm release, with standard node/client Secret names |
| Legacy operator built-in self-signed | Reuses the CA and creates new node/client Secrets, leaving source Secrets for rollback | Export keeps external references. You must arrange renewal |
| cert-manager | Adds join names to the node Certificate, keeping its issuer and existing names | Existing cert-manager Certificates continue renewal. Controller export keeps external Secret references |
| External or provided certificates without a CA key | Leaves the certificates unchanged. The owner must supply the join names | The certificate owner remains responsible for renewal |

A CA signing key can make the controller choose self-signed regeneration even for a
provided source. That does not mean the Helm chart owns renewal. Without the chart's CA
metadata or explicit provided-CA settings in the source Helm release, the exporter leaves it externally managed. Do not enable the chart self-signer
just because a CA key is present.

### Secrets nodes use after migration

| Source | Node and client Secrets after migration | Rollback |
|---|---|---|
| Legacy Helm chart self-signer | `<name>-node-secret` and `<name>-client-secret`, reissued in place | Kept, source pods use them again |
| cert-manager | The Secrets the existing Certificates write, under their existing names | Kept |
| Provided certificates | The Secrets you provided, mounted as they are | Kept |

A TLS source also gets the `<name>-ca-crt` ConfigMap unless it already exists. Rollback
deletes it only when migration created it, identified by
`crdb.cockroachlabs.com/migration-created=true`. A preexisting ConfigMap is kept.

`<name>-ca-crt` is copied once from the source CA. A chart self-signer cluster switches to the
chart's synced CA ConfigMap at adoption. With cert-manager or external certificates, update
`<name>-ca-crt` yourself after the CA is renewed, before nodes pick up certificates signed by
the new CA.

### Legacy chart TLS options

| Legacy value | Migration | After adoption |
|---|---|---|
| `tls.certs.provided` with `tlsSecret: false` | Stops at certificate migration, before any pod is replaced. Add `tls.crt`, `tls.key` and `ca.crt` first, keeping the raw keys for source pods and rollback | The new chart reads the TLS keys |
| `tls.certs.selfSigner.caProvided` | Copy `ca.crt` and `ca.key` from your CA Secret into `<name>-ca-secret` first, so the reissued certificates gain the join names | Export preserves `caProvided`, `caSecret` and the rotation setting from the matching source Helm release. Keep that CA Secret. The chart does not rotate the supplied CA |
| `tls.enableSighupRotation` | Supported | No equivalent value. The operator reloads certificates itself |


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
rotation drops that name. External certificate owners must reissue it. Legacy Helm pods
copy certificates at startup and keep serving their previous certificate until restarted.
Rehearse the first replacement join with your TLS setup.

### Legacy Helm self-signer lifecycle

`<name>` is the source StatefulSet name.

| Stage | Certificate resources and references |
|---|---|
| Before migration | `<name>-ca-secret` holds the CA and signer metadata. `<name>-node-secret` and `<name>-client-secret` hold the issued certificates. StatefulSet pods use these Secrets |
| Certificate migration | Creates `<name>-ca-crt` from the source CA if absent. Reissues node/client certificates in place with custom SANs and join names. The CA is reused. Existing signer annotations are not rewritten |
| Pod migration and handoff | Replacement pods mount the same node/client Secrets and `<name>-ca-crt`. The parent keeps these as external certificate references at handoff |
| Export and chart adoption | Export recognizes the original signer metadata and restores rotation. It reads actual certificate lifetimes when annotations are stale and carries node DNS/IP SANs into `additionalSANs`. The chart's pre-install/pre-upgrade hook syncs `<name>-ca-secret-crt` from the existing CA before applying the new reference |
| After adoption | The chart uses `<name>-ca-secret-crt` and the same node/client Secret names. Its rotation CronJobs maintain certificates using the exported settings |

With a supplied CA (`selfSigner.caProvided` and a custom `caSecret`), the chart still
syncs `<name>-ca-secret-crt`. The custom name only selects the Secret to read the CA
from. It copies the CA certificate without regenerating the CA or issued certificates.
The legacy operator's built-in signer stays externally
managed after export; this chart self-signer sync does not apply to it.

The migration controller does not create or remove the legacy chart's rotation CronJobs.
Their presence before adoption does not confirm that the new chart manages renewal.
Verify the adopted chart's jobs and SANs. Legacy StatefulSet pods that copied certificates
at startup can keep serving the old certificate until restarted.

The CA ConfigMap name change affects pod mounts. Plan for a rolling pod update at adoption
and wait for every pod to use the new reference before cleanup. Rehearse this with your
configuration. A successful Helm command alone does not confirm the rollout finished.

### Check certificate management after adoption

The migrated CrdbCluster stores certificate references as `externalCertificates`.
The values exporter restores `selfSigner.enabled=true` and `rotateCerts=true` when
it recognizes the original self-signer CA metadata and the standard node/client Secret
names. It carries the issued DNS and IP SANs and certificate lifetimes into the values.
A CA signing key alone does not enable the self-signer for an externally managed source.
For a chart-owned CA, `<name>-ca-secret` must have the self-signer's
`certificate-valid-from`, `certificate-valid-upto` and `certificate-duration` annotations.
If any are missing, the exported values keep external certificates and leave renewal to
you, even if the controller reissued certificates. Explicit provided-CA settings follow
the separate rule below.

Recognition also requires the default `<name>-node-secret` and `<name>-client-secret`
names, a valid CA certificate/key pair and no cert-manager ownership. Custom node or
client Secret names stay external and are never redirected to the self-signer. If a chart-owned CA
has the signer lifetime annotations but lacks `secret-data-hash`, export fails with a
message to repair the metadata first. It does not silently switch modes. Do not invent
signer annotations or replace the CA just to enable rotation.

For a supplied CA, export reads `caProvided`, `caSecret` and `rotateCerts` from the matching
deployed Helm release in the namespace. It checks the CA pair and that the node/client
certificates were signed by it. The supplied CA does not need chart signer annotations.
The helper needs `get` and `list` access to Secrets in the cluster namespace.
This detection uses Helm's default
Secret storage. If your release uses another storage backend, review and set these three
values yourself before adoption. If restoring the signer manually, also set
`selfSigner.enabled=true` and `externalCertificates.enabled=false` under `cockroachdb.tls`.
Keep renewal of the supplied CA under your control.
With `rotateCerts=true`, the chart creates the combined node/client rotation CronJob and
omits CA rotation. With `rotateCerts=false`, it creates no rotation CronJobs.

Review the generated TLS values before adoption:

- **Recognized self-signer source**: Keep the exported signer settings and SANs. The chart
  reuses the existing CA and node/client Secrets and maintains its own CA ConfigMap,
  `<name>-ca-secret-crt`, including with a supplied CA. Verify that ConfigMap contains
  the same CA and the rotation CronJobs match the exported settings: a supplied CA has no
  CA rotation job, and `rotateCerts=false` disables both jobs. Do not delete `<name>-node-secret` or
  `<name>-client-secret`. The self-signer continues using those names.
- **cert-manager source**: Existing Certificates continue renewing their Secrets while the
  exported values use external references. To manage those Certificates through the new
  chart, first carry over their issuer, Secret names, durations, custom DNS names and IPs
  into `cockroachdb.tls.certManager`. Review the rendered Certificates before changing mode.
- **External or unrecognized source**: Keep `cockroachdb.tls.externalCertificates` and
  arrange ongoing renewal with the certificate owner. This includes legacy-operator
  sources without the chart self-signer metadata. Do not enable a new CA over an existing
  cluster.

For an externally managed source, check the actual expiry of both certificates after
migration and schedule renewal. These commands print certificate dates, not private keys:

```bash
for SECRET in $(kubectl get crdbcluster.v1beta1.crdb.cockroachlabs.com "$STS_NAME" -n "$NAMESPACE" \
  -o jsonpath='{.spec.template.spec.certificates.externalCertificates.nodeSecretName} {.spec.template.spec.certificates.externalCertificates.rootSqlClientSecretName}'); do
  echo "$SECRET"
  kubectl get secret "$SECRET" -n "$NAMESPACE" -o jsonpath='{.data.tls\.crt}' \
    | openssl base64 -d -A | openssl x509 -noout -dates
done
```

Check the current parent and node certificate references:

```bash
kubectl get crdbcluster.v1beta1.crdb.cockroachlabs.com "$STS_NAME" -n "$NAMESPACE" \
  -o jsonpath='{.spec.template.spec.certificates}{"\n"}'
kubectl get crdbnodes -n "$NAMESPACE" \
  -o custom-columns='NAME:.metadata.name,CA:.spec.certificates.externalCertificates.caConfigMapName,NODE:.spec.certificates.externalCertificates.nodeSecretName,CLIENT:.spec.certificates.externalCertificates.rootSqlClientSecretName'
kubectl get pods -n "$NAMESPACE" \
  -o custom-columns='NAME:.metadata.name,CA_CONFIGMAPS:.spec.volumes[*].configMap.name,PROJECTED_CA_CONFIGMAPS:.spec.volumes[*].projected.sources[*].configMap.name'
```

After self-signer adoption, the migration's `<name>-ca-crt` ConfigMap is not automatically
deleted. The chart uses `<name>-ca-secret-crt` instead. Keeping the old ConfigMap is harmless
once nothing references it, but it can become stale after CA rotation. Check the parent
spec, CrdbNodes, live pod volumes and other workloads or GitOps configuration before
optionally deleting it. External-certificate sources can still use `<name>-ca-crt`, so keep
it while referenced. Certificate mode changes belong after migration and should be
rehearsed separately.

---

## Services, PDBs and join during migration

Replacement pods keep the source pod labels, so the chart's Services keep routing to them.
`<name>` is the source StatefulSet name.

| Resource | During migration | Handoff | Rollback |
|---|---|---|---|
| Discovery Service `<name>` | Unchanged. Replacements register in it, so pod DNS keeps working | The operator adopts it as its node Service under the same name | Unchanged |
| Client Service `<name>-public` | Unchanged for shared-port Helm sources, including the external port and target | Kept | Unchanged |
| Join Service `<name>-join` | Created for replacements to join through. It lists only ready pods | Kept, owned by the CrdbCluster | Deleted |
| PDB | `<name>-budget` covers replacements through finalization | Create `<name>-pdb`, then remove the source PDB. Verify the new budget and selector | Source PDB is unchanged |
| Role and RoleBinding `<name>` | Existing chart resources are reused. Missing resources are created and marked | Kept | Only resources created by migration are deleted |
| ClusterRole and ClusterRoleBinding `<namespace>-<name>` | Created and marked if absent. Existing resources are reused | Kept | Only resources created by migration are deleted |

The legacy chart renders its Role and RoleBinding only with TLS on, and so does the new chart.
For an insecure source, skip them in the Step 10 annotation loop and delete them after
adoption, since nothing uses them.

The legacy chart serves SQL and gRPC on one port through `--port`. After migration,
the exported values keep the shared port, and the chart renders one `grpc` Service port
for both protocols. Do not add a second Service entry on that number. Check the
replacement start command during rehearsal as well as the rendered Service.

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
| `Init` prerequisite failure | Fix the named topology, flag, certificate or permission. Retries do not need another `start`. A Helm source may have no CrdbCluster yet. Check StatefulSet `MigrationBlocked` events. |
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
kubectl label sts $STS_NAME crdb.io/migrate=stop --overwrite -n $NAMESPACE
```

Wait for both phase `Stopped` and source label `stopped`. Existing replacements keep
serving. An in-flight replacement gap is repaired before the pause is recorded.

```bash
# Resume a repaired forward migration.
kubectl label sts $STS_NAME crdb.io/migrate=start --overwrite -n $NAMESPACE
# Resume a paused rollback instead.
kubectl label sts $STS_NAME crdb.io/migrate=rollback --overwrite -n $NAMESPACE
```

### Rollback

Request rollback while the source StatefulSet still exists and handoff is incomplete:

```bash
kubectl label sts $STS_NAME crdb.io/migrate=rollback --overwrite -n $NAMESPACE
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
migration. Source Secrets, RBAC and the PDB stay in place. Original Service ports and
logging configuration are restored. Verify the original budget and permissions after rollback.

Helm rollback finishes after all replacements are gone and the source has its original
count with all pods ready. Cleanup removes the migration label and the controller-created
CrdbCluster. Verify source health, PVC bindings and that the migration artifacts are gone.
If cleanup is waiting, repair the reported resource rather than clearing labels by hand.

The shared `crdb-critical` PriorityClass remains after rollback. Leave it if it existed
before migration or another workload uses it. Otherwise, check all namespaces for pods
referencing it before deleting it. Its presence alone does not mean rollback failed.

---

## Troubleshooting

### Migration Stuck at PodMigration

If a node fails health checks for more than 10 minutes, migration auto-pauses to `Stopped`.

```bash
# Check why migration stopped
kubectl get crdbcluster $STS_NAME -n $NAMESPACE \
  -o jsonpath='{.status.migration.lastError}'

# Check pod status
kubectl describe pod $STS_NAME-<index> -n $NAMESPACE

# Check CrdbNode status
kubectl get crdbnode $STS_NAME-<index> -n $NAMESPACE -o yaml

# Resume after fixing the issue
kubectl label sts $STS_NAME crdb.io/migrate=start --overwrite -n $NAMESPACE
```

### Insecure Cluster Migration

See [insecure clusters](#insecure-clusters) for the required adoption values and checks.
Keep TLS disabled throughout migration, adoption and later upgrades.

### Under-replicated Ranges

If health checks report under-replicated ranges, wait for replication to catch up. The
controller retries every 10 seconds. If ranges remain under-replicated for 10 minutes,
migration pauses.

```bash
# StatefulSet pods use -c db. Migrated CrdbNode pods use -c cockroachdb.
kubectl exec $STS_NAME-0 -n $NAMESPACE -c db -- \
  /cockroach/cockroach node status --ranges --format table \
  --certs-dir=/cockroach/cockroach-certs
```

For the direct SQL diagnostic alternative and its `allow_unsafe_internals` and
UA routing requirements, see
[Inspect cluster health manually](../../../cockroachdb-operator/charts/operator/README.md#inspect-cluster-health-manually).

### Status Conflict Errors

Status update conflicts are normal during migration (multiple controllers may update the same
object). The controller retries immediately on conflict. If you see repeated conflict errors
in logs, verify that no other controller is competing for the CrdbCluster status.

### Verifying RBAC

```bash
# Check namespace-scoped RBAC
kubectl get role $STS_NAME -n $NAMESPACE
kubectl get rolebinding $STS_NAME -n $NAMESPACE

# Check cluster-scoped RBAC (namespace-qualified names)
kubectl get clusterrole ${NAMESPACE}-${STS_NAME}
kubectl get clusterrolebinding ${NAMESPACE}-${STS_NAME}

# Verify ServiceAccount
kubectl get sa $STS_NAME -n $NAMESPACE
```

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
