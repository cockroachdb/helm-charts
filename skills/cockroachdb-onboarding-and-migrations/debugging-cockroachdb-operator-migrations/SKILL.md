---
name: debugging-cockroachdb-operator-migrations
description: Debugs CockroachDB Operator migration scenarios, including Helm StatefulSet to v1beta1 CrdbNode migration and legacy operator v1alpha1 to v1beta1 migration. Use when migration labels, migration phases, source StatefulSet ownership, converted CRDs, PVC ownership, or post-migration reconciliation are unclear or stuck.
compatibility: CockroachDB Helm v2 charts and CockroachDB Operator resources, including crdb.cockroachlabs.com/v1alpha1 and v1beta1. Requires read access to the CockroachDB namespace and operator namespace. Final source StatefulSet deletion must be explicitly approved by the customer.
metadata:
  author: cockroachdb
  version: "1.1"
---

# Debugging CockroachDB Operator Migrations

Debugs migration from Helm StatefulSet or legacy operator v1alpha1 workloads to CockroachDB Operator v1beta1 `CrdbNode` management. Use this after collecting the general baseline from [diagnosing-cockroachdb-helm-deployments](../../cockroachdb-observability-and-diagnostics/diagnosing-cockroachdb-helm-deployments/SKILL.md).

## When to Use This Skill

- A Helm StatefulSet to operator migration is stuck or unclear
- A legacy operator v1alpha1 to v1beta1 migration has conversion or webhook issues
- Migration intent, a health wait, or a stopped migration needs diagnosis
- The schema-grounded migration status field or migration labels report an error
- Source StatefulSet ownership, pod ownerReferences, or PVC ownerReferences are unclear
- The operator stops reconciling after migration

## Safety Considerations

- Delete the source StatefulSet only after both `phase` and `settledPhase` are `Finalization`, its replicas are zero, and the user has approved completing the handoff. Deletion ends rollback availability.
- Rehearse on a separate cluster with similar configuration before migrating any real cluster, including development clusters.
- Do not upgrade either operator or change cluster configuration during migration. Finish or roll back first.
- Do not modify the source StatefulSet spec during migration, except to restore the exact unreplaced count when a contention pause explicitly requires it.
- Do not remove the migration label `crdb.io/migrate` during an active migration.
- Do not manually create `CrdbNode` resources during migration.
- Do not delete migrated PVCs. They contain CockroachDB data.
- If the operator stalls after migration, collect the escalation packet before restarting it.

## Execution Discipline

- Execute one step at a time and inspect the output before moving on. Migration phase, source workload state, and ownership determine which later checks are safe.
- Run every step in the same shell session. The commands rely on `OPERATOR_NAMESPACE`, `CRDB_NAMESPACE`, `CRDB_MIGRATION_DIR`, and the `CRDB_*_PATH` exports set in Step 1. A fresh shell loses those variables and later `jq`/`kubectl` calls will silently target the wrong object.
- Never infer a target `CrdbCluster` object name from a Helm release, service name, or CockroachDB image/version string. List `CrdbCluster` objects and use the exact `metadata.name`.
- Before reading migration, condition, or status fields from a `CrdbCluster`, save the live CRD YAML and derive field paths from the served CRD schema for the object's `apiVersion`.
- Do not delete the source StatefulSet, patch labels, patch mode, restart the operator, run Helm upgrades, or change ownerReferences unless the user explicitly approves the action for the target cluster.
- Do not run interactive `kubectl exec` shells or debug containers unless the user approves them and the customer policy allows the image/source.
- In production or when the migration phase is ambiguous, involve TSE or the operator team before changing source or target resources.

For operator logs, use the deployed `appLabel` if it differs from
`app=cockroach-operator` in the commands below.

## Required Inputs

- Migration type: Helm StatefulSet to operator, or legacy operator v1alpha1 to v1beta1
- Source resource name and namespace
- Target namespace and discovered `CrdbCluster.metadata.name`
- Operator namespace and version
- Values file or migration command used
- Current migration label and phase
- Whether the source StatefulSet has already been deleted

## Step 1: Confirm Migration Documentation and Versions

```bash
export OPERATOR_NAMESPACE="<operator-namespace>"
export CRDB_NAMESPACE="<cockroachdb-namespace>"
export CRDB_HELM_RELEASE="<cockroachdb-release>"
export CRDB_MIGRATION_DIR="${CRDB_MIGRATION_DIR:-$(mktemp -d)}"

kubectl -n "$OPERATOR_NAMESPACE" get deploy cockroach-operator -o jsonpath='{.spec.template.spec.containers[*].image}{"\n"}{.spec.template.spec.containers[*].args}{"\n"}'
kubectl -n "$OPERATOR_NAMESPACE" get pods -o json | jq '[.items[] | {name: .metadata.name, containers: [.status.containerStatuses[]? | {name, image, imageID}]}]'
kubectl get crd crdbclusters.crdb.cockroachlabs.com -o jsonpath='{.spec.versions[*].name}{"\n"}{.status.storedVersions}{"\n"}'
kubectl get crd crdbclusters.crdb.cockroachlabs.com -o yaml > "$CRDB_MIGRATION_DIR/crdbclusters-crd.yaml"
kubectl get crd crdbclusters.crdb.cockroachlabs.com -o json > "$CRDB_MIGRATION_DIR/crdbclusters-crd.json"
helm -n "$CRDB_NAMESPACE" history "$CRDB_HELM_RELEASE" || true

kubectl -n "$CRDB_NAMESPACE" get crdbcluster -o json | jq -r '
  .items[]
  | [.metadata.name, .apiVersion, (.metadata.labels["app.kubernetes.io/instance"] // ""), (.metadata.labels["crdb.io/migrate"] // ""), (.metadata.labels["crdb.cockroachlabs.com/migration"] // "")]
  | @tsv
'
```

If no `CrdbCluster` exists yet, inspect the source StatefulSet labels, events, and operator logs. An early prerequisite wait can happen before the target exists. Do not invent a target name or status. Continue object-specific checks once the target is discoverable.

If multiple rows are returned, choose the target by `metadata.name`. Do not use a CockroachDB version or image tag as the object name.

```bash
export CRDBCLUSTER="<metadata.name-from-crdbcluster-list>"
test -n "$CRDBCLUSTER"

kubectl -n "$CRDB_NAMESPACE" get crdbcluster "$CRDBCLUSTER" -o yaml > "$CRDB_MIGRATION_DIR/crdbcluster.yaml"
kubectl -n "$CRDB_NAMESPACE" get crdbcluster "$CRDBCLUSTER" -o json > "$CRDB_MIGRATION_DIR/crdbcluster.json"

export CRDBCLUSTER_API_VERSION="$(jq -r '.apiVersion | split("/")[-1]' "$CRDB_MIGRATION_DIR/crdbcluster.json")"
export CRDBCLUSTER_SCHEMA_JSON="$CRDB_MIGRATION_DIR/crdbcluster-schema.json"
jq -e --arg version "$CRDBCLUSTER_API_VERSION" '
  .spec.versions[] | select(.name == $version) | .schema.openAPIV3Schema
' "$CRDB_MIGRATION_DIR/crdbclusters-crd.json" > "$CRDBCLUSTER_SCHEMA_JSON"

crdb_schema_has() {
  jq -e --arg path "$1" '
    def has_schema_path($schema; $parts):
      if ($parts | length) == 0 then true
      elif (($schema.properties? // {}) | has($parts[0])) then
        has_schema_path($schema.properties[$parts[0]]; $parts[1:])
      else false
      end;
    has_schema_path(.; $path | split("."))
  ' "$CRDBCLUSTER_SCHEMA_JSON" >/dev/null
}

crdb_first_schema_path() {
  for schema_path in "$@"; do
    if crdb_schema_has "$schema_path"; then
      printf '%s\n' "$schema_path"
      return 0
    fi
  done
  printf '\n'
}

export CRDB_MODE_PATH="$(crdb_first_schema_path spec.mode)"
export CRDB_MIGRATION_PATH="$(crdb_first_schema_path status.migration)"
export CRDB_CONDITIONS_PATH="$(crdb_first_schema_path status.conditions)"
export CRDB_OBSERVED_GENERATION_PATH="$(crdb_first_schema_path status.observedGeneration)"

printf '%s\n' \
  "apiVersion=$CRDBCLUSTER_API_VERSION" \
  "mode=$CRDB_MODE_PATH" \
  "migration=$CRDB_MIGRATION_PATH" \
  "conditions=$CRDB_CONDITIONS_PATH" \
  "observedGeneration=$CRDB_OBSERVED_GENERATION_PATH"
```

Check the migration guide for the source and the operator chart changelog for version-specific migration fixes:

- [Helm StatefulSet controller migration guide](../../../docs/migration/helm/controller_migration.md)
- [legacy operator controller migration guide](../../../docs/migration/operator/controller_migration.md)
- [CockroachDB Operator chart changelog](../../../cockroachdb-operator/charts/operator/CHANGELOG.md)

## Step 2: Read Current Intent and Progress

Refresh the snapshot whenever diagnosing a new wait or stop. Migration progress is in `status.migration`. Confirm it exists in the served schema collected above. For another version, use that version's schema and guide.

```bash
kubectl -n "$CRDB_NAMESPACE" get crdbclusters.v1beta1.crdb.cockroachlabs.com "$CRDBCLUSTER" -o json > "$CRDB_MIGRATION_DIR/crdbcluster.json"
jq '{name: .metadata.name, mode: .spec.mode,
  migrate: .metadata.labels["crdb.io/migrate"],
  skipReconcile: .metadata.labels["crdb.io/skip-reconcile"],
  migration: (.status.migration | {phase, settledPhase, message, lastError, migratedNodes, totalPods}),
  conditions: .status.conditions}' "$CRDB_MIGRATION_DIR/crdbcluster.json"

export SOURCE_STS="<source-statefulset>"
kubectl -n "$CRDB_NAMESPACE" get sts "$SOURCE_STS" -o json | jq '{
  name: .metadata.name, replicas: .spec.replicas, readyReplicas: .status.readyReplicas,
  labels: .metadata.labels, annotations: .metadata.annotations,
  retention: .spec.persistentVolumeClaimRetentionPolicy}'
kubectl -n "$CRDB_NAMESPACE" get events -o json | jq '[.items[] |
  select(.reason | test("^(MigrationPaused|MigrationBlocked|MigrationSourceUnreadable|MigrationSourceLost|RollbackSourceLost|DegradedHandoff|UnsupportedStartFlags|UnreadableStartCommand|DynamicStartFlag)$")) |
  {time: (.lastTimestamp // .eventTime), reason, message, involvedObject}]'
kubectl -n "$OPERATOR_NAMESPACE" logs -l app=cockroach-operator --all-containers --prefix --max-log-requests=20 --since=30m --tail=1000
```

For a legacy source, also inspect labels through `crdbclusters.v1alpha1.crdb.cockroachlabs.com`. `skip-reconcile=true` must be explicitly set before `migrate=start`. The controller waits and never opts the user in. Keep `--enable-migration` enabled while legacy operator clusters exist. Recommend legacy operator 2.18.7. 2.18.6 is the coexistence minimum.

Compare StatefulSet replicas with the pinned `status.migration.totalPods` and the number of replaced members. Do not scale it to the original count during an active forward migration. If contention is reported, use the exact unreplaced count in the message, after stopping the other writer.

## Step 3: Check Resources and Certificates

```bash
kubectl -n "$CRDB_NAMESPACE" get crdbnodes -o yaml
kubectl -n "$CRDB_NAMESPACE" get pods -o wide
kubectl -n "$CRDB_NAMESPACE" get pvc -o json | jq '[.items[] | {
  name: .metadata.name, uid: .metadata.uid, phase: .status.phase,
  volume: .spec.volumeName, dataSource: .spec.dataSource,
  labels: .metadata.labels, annotations: .metadata.annotations,
  owners: .metadata.ownerReferences}]'
kubectl -n "$CRDB_NAMESPACE" get svc "$CRDBCLUSTER-join" -o yaml
kubectl -n "$CRDB_NAMESPACE" get endpoints "$CRDBCLUSTER-join" -o yaml
kubectl -n "$CRDB_NAMESPACE" get configmap "$CRDBCLUSTER-ca-crt" -o json | jq '{metadata: .metadata, keys: (.data // {} | keys)}'
kubectl -n "$CRDB_NAMESPACE" get secrets -o json | jq '[.items[] | {name: .metadata.name, type, keys: (.data // {} | keys)}]'
kubectl -n "$CRDB_NAMESPACE" get certificates.cert-manager.io -o yaml
```

Read node and client Secret names and certificate keys from the live pod volumes and cluster spec. Do not assume default names. Decode only the public node certificate, never a key:

```bash
export NODE_SECRET="<node-secret-from-live-spec>"
export CERT_KEY="<public-certificate-key>"  # commonly node.crt or tls.crt
export CLUSTER_DOMAIN="<domain-from-live-source>"
kubectl -n "$CRDB_NAMESPACE" get secret "$NODE_SECRET" -o json |
  jq -r --arg key "$CERT_KEY" '.data[$key]' | openssl base64 -d -A |
  openssl x509 -noout -dates -issuer -subject -text
```

Inspect the Subject Alternative Name section for
`$CRDBCLUSTER-join.$CRDB_NAMESPACE.svc.$CLUSTER_DOMAIN`. For an automated hostname
check, use OpenSSL 3 with `-checkhost` and require the output to say
`does match certificate`. Do not rely on its exit status alone. The LibreSSL shipped
with macOS does not support `-checkhost` or `-ext`.

- Self-signed sources regenerate certificates and retain existing DNS and IP SANs. Legacy operator source Secrets remain for rollback. Replacements use generated Secrets. Legacy Helm pods keep their old mounted certificate until restarted.
- cert-manager sources add join names to the existing Certificate without changing its issuer or custom names. The operator needs `get` and `patch` on `cert-manager.io/certificates`. Nonexternal TLS sources can need `get` even when self-signed, because the operator probes for cert-manager ownership when its CRD exists.
- External sources must already cover the join FQDN. Migration cannot regenerate them. Explicit legacy external-Secret sources bypass cert-manager discovery.
- Full key/pair, trust, usage, and join validation runs at certificate migration completion. Pod migration checks the join name again. The controller does not validate certificate subject identities.

Collect deployment args, images and running image digests for both operators. Discover the legacy operator namespace/deployment first. Run the same inventory used in Step 1 there. Check namespace scope, webhooks and RBAC when multiple legacy operators coexist. For webhook issues, inspect the conversion service endpoints, CRD conversion configuration, and admission `matchPolicy=Exact`.

## Step 4: Choose One Recovery Action

| Live symptom | Meaning | Single next action |
|---|---|---|
| No target yet, source is healthy, prerequisite message | Migration has not disrupted the source | Fix the named prerequisite. Do not repeatedly relabel |
| Active phase, health wait, intent `in-progress` | Controller is still waiting | Inspect the unhealthy node/pod and wait within the reported deadline |
| `Stopped`, label `stopped`, `MigrationPaused` | A health deadline expired | Fix `lastError`, then resume the recorded direction |
| `Stopped`, label `stopped`, no pause event | A user stop or another pause was recorded | Read the message and `settledPhase`, then resume only after the cause is resolved |
| Stop or rollback waits on source readiness and logs confirm a source pod was drained | The source process may need a restart | Verify the source StatefulSet still desires and owns that pod, its PVCs are intact, the other nodes are healthy and no ranges are under-replicated. Restart only that pod and wait for Ready. A stopped migration still needs an explicit resume in its recorded direction |
| Contention pause | Another writer restored a released source member | Stop that writer, scale the source to the message's unreplaced count, then resume |
| `phase=Stopped`, `settledPhase=RollbackInProgress` | Rollback is the saved direction | After fixing the blocker, set `migrate=rollback`. Never `start` |
| `MigrationSourceUnreadable` | Source read failed, not proof it was deleted | Restore API access and inspect the source before any deletion or relabel |
| `MigrationBlocked` on the Helm source StatefulSet | Helm preflight or target creation failed | Fix the named cause and let the active intent retry |
| `Failed` with `MigrationSourceLost` | The source disappeared before finalization settled. This is terminal | Do not relabel. Preserve surviving resources and escalate |
| `RollbackSourceLost` | The source disappeared during rollback | Preserve replacements and PVCs. Restore a compatible source with operator-team guidance, then resume with `rollback` |
| `DegradedHandoff` | The source is gone and handoff completed with unhealthy replacements | Check database health and escalate. Controller rollback is unavailable |
| `UnsupportedStartFlags` or `UnreadableStartCommand` | Source start flags cannot be reproduced safely | Resolve the reported source command issue before scale-down. Do not repeatedly relabel an active wait |
| `DynamicStartFlag` | A carried flag runs a command in every replacement | Verify the command dependencies and resolved value during rehearsal. The warning alone does not stop migration |
| `Finalization` without matching `settledPhase` | Final spec preparation is still running | Wait. Do not delete the StatefulSet |
| `Finalization` settled, source replicas zero | Ready for final handoff. Rollback is still available | With user approval, delete only the source StatefulSet |
| `MutableOnly`, `Complete`, `migrate=complete` | Handoff finished. Controller rollback is unavailable | Run the after-handoff checklist and adopt through the chart/GitOps |
| `RollbackComplete` without matching `settledPhase` | Cleanup/validation is still running | Wait. Keep legacy `skip-reconcile` in place |
| `RollbackComplete` settled, legacy skip label absent | Control returned to the source operator | Verify source health and that a normal alpha update does not reinsert the label |

Use labels on the source StatefulSet for Helm migrations and on the legacy `v1alpha1` CrdbCluster for legacy-operator migrations. Removing `migrate` during an active migration requests rollback for compatibility. It is not a harmless metadata cleanup. Prefer explicit `rollback`. A forward stop can resume with `start`. Never edit operator-written annotations or status to force progress.

Rollback retains data, logs and WAL PVCs and original bindings. It removes the join Service and certificate/RBAC resources carrying `crdb.cockroachlabs.com/migration-created=true`. A preexisting `<cluster>-ca-crt` ConfigMap or source Secret is preserved. Do not add or remove the marker yourself, or delete source Secrets/PVCs. A settled legacy-operator rollback retains its audit status. Helm rollback deletes the controller-created CrdbCluster after restoring the source.

Existing source RBAC is reused, and the source PDB covers replacements until handoff. Rollback preserves both. At handoff the controller creates `<cluster>-pdb` before removing the source PDB. Check its budget and selector. The shared `crdb-critical` PriorityClass remains. Check whether other workloads use it before considering cleanup.

## Step 5: Verify After Handoff

Follow the source-specific guide. Check all three completion markers, database health, node/pod readiness and original PVC bindings. Test chart/GitOps adoption with a changed value and no CrdbCluster field-ownership conflict. Configuration belongs on the v1beta1 parent. Keep legacy `skip-reconcile` until the legacy operator is uninstalled. Review exported TLS values and custom SANs before chart adoption. Recognized legacy self-signer sources regain chart rotation. Other sources need their existing renewal owner.

Before advancing, the controller checks replacement readiness, discovery registration, storage adoption and client Service port targets. It checks cluster health through SQL for under-replicated ranges and live nodes. During rehearsal and after adoption or rollback, check an actual client connection through the original public Service, with TLS hostname verification for secure sources.

For an insecure source, preserve TLS-off and disable self-signer, cert-manager and external certificates in adoption values. Check SQL through the original public Service with `sslmode=disable`. Do not toggle TLS on this running cluster. See [insecure adoption checks](../../../docs/migration/helm/controller_migration.md#insecure-clusters).

Before a same-release Helm adoption, review [source resource retention](../../../docs/migration/helm/controller_migration.md#resources-to-preserve-before-helm-adoption). Helm can prune the discovery Service, issuer chain, ingress and other resources the new chart does not render. Existing valid certificates do not prove renewal will continue.

### Certificate Renewal After Adoption

- Review the exported mode and Secret names. A CA key alone does not enable rotation. Recognized legacy Helm signer metadata or explicit source `caProvided` settings can restore it. Missing `secret-data-hash` with signer lifetime annotations is an export error. Do not invent metadata to bypass it.
- External certificates, including legacy operator built-in certificates and custom Secret names, need a renewal plan. Controller-reissued leaves last at most one year, capped by CA expiry. Preserve cert-manager Certificates and their issuer chain.
- After self-signer adoption, check CA ConfigMap mounts and rotation CronJobs. Remove the old CA ConfigMap only after confirming nothing uses it. For external or cert-manager modes, update the referenced CA ConfigMap after CA renewal.
- Verify SQL through the public Service at the original client port with `sslmode=verify-full`, including custom client DNS names. A direct pod connection or port-forward does not verify Service routing.

See the source guide's [certificate lifecycle](../../../docs/migration/helm/controller_migration.md#certificates-during-and-after-migration)
and [legacy operator renewal guidance](../../../docs/migration/operator/controller_migration.md#certificates-during-and-after-migration).

Keep explicit `--locality` initially. Move to `localityMappings` only after comparing the resulting tiers on every node. Check affinity and podTemplate overrides and retained-disk behavior before changing topology or removing nodes. Rehearse configuration changes separately from migration.

## Output Format

Report the phase and settled phase, current intent, wait versus stop, source replicas versus pinned original count, whether rollback is still available, the evidence for the blocker, and one next action. Do not offer competing relabel commands. If state is contradictory or recovery would discard data, collect the escalation packet before proposing mutation.

## References

- [Helm controller migration guide](../../../docs/migration/helm/controller_migration.md)
- [Legacy operator controller migration guide](../../../docs/migration/operator/controller_migration.md)
- [Collecting an escalation packet](../../cockroachdb-observability-and-diagnostics/collecting-cockroachdb-operator-escalation-packet/SKILL.md)
