---
name: debugging-cockroachdb-operator-migrations
description: Debugs CockroachDB Operator migration scenarios, including Helm StatefulSet to v1beta1 CrdbNode migration and public operator v1alpha1 to v1beta1 migration. Use when migration labels, migration phases, source StatefulSet ownership, converted CRDs, PVC ownership, or post-migration reconciliation are unclear or stuck.
compatibility: CockroachDB Helm v2 charts and CockroachDB Operator resources, including crdb.cockroachlabs.com/v1alpha1 and v1beta1. Requires read access to the CockroachDB namespace and operator namespace; final source StatefulSet deletion must be explicitly approved by the customer.
metadata:
  author: cockroachdb
  version: "1.0"
---

# Debugging CockroachDB Operator Migrations

Debugs migration from Helm StatefulSet or public operator v1alpha1 workloads to CockroachDB Operator v1beta1 `CrdbNode` management. Use this after collecting the general baseline from [diagnosing-cockroachdb-helm-deployments](../../cockroachdb-observability-and-diagnostics/diagnosing-cockroachdb-helm-deployments/SKILL.md).

## When to Use This Skill

- A Helm StatefulSet to operator migration is stuck or unclear
- A public operator v1alpha1 to v1beta1 migration has conversion or webhook issues
- Migration labels remain `start` or `finalized`
- The schema-grounded migration status field or migration labels report an error
- Source StatefulSet ownership, pod ownerReferences, or PVC ownerReferences are unclear
- The operator stops reconciling after migration

## Safety Considerations

- Do not delete the source StatefulSet until the migration phase is `finalized` and the customer is ready to complete migration.
- Do not modify the source StatefulSet spec during migration.
- Do not remove the migration label `crdb.io/migrate` during an active migration.
- Do not manually create `CrdbNode` resources during migration.
- Do not delete migrated PVCs. They contain CockroachDB data.
- If the operator stalls after migration, collect the escalation packet before restarting it.

## Execution Discipline

- Execute one step at a time and inspect the output before moving on. Migration phase, source workload state, and ownership determine which later checks are safe.
- Never infer a target `CrdbCluster` object name from a Helm release, service name, or CockroachDB image/version string. List `CrdbCluster` objects and use the exact `metadata.name`.
- Before reading migration, condition, or status fields from a `CrdbCluster`, save the live CRD YAML and derive field paths from the served CRD schema for the object's `apiVersion`.
- Do not delete the source StatefulSet, patch labels, patch mode, restart the operator, run Helm upgrades, or change ownerReferences unless the user explicitly approves the action for the target cluster.
- Do not run interactive `kubectl exec` shells or debug containers unless the user approves them and the customer policy allows the image/source.
- In production or when the migration phase is ambiguous, involve TSE or the operator team before changing source or target resources.

## Required Inputs

- Migration type: Helm StatefulSet to operator, or public operator v1alpha1 to v1beta1
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

kubectl -n "$OPERATOR_NAMESPACE" get deploy cockroach-operator -o jsonpath='{.spec.template.spec.containers[0].image}{"\n"}'
kubectl get crd crdbclusters.crdb.cockroachlabs.com -o jsonpath='{.spec.versions[*].name}{"\n"}'
kubectl get crd crdbclusters.crdb.cockroachlabs.com -o yaml > "$CRDB_MIGRATION_DIR/crdbclusters-crd.yaml"
kubectl get crd crdbclusters.crdb.cockroachlabs.com -o json > "$CRDB_MIGRATION_DIR/crdbclusters-crd.json"
helm -n "$CRDB_NAMESPACE" history "$CRDB_HELM_RELEASE" || true

kubectl -n "$CRDB_NAMESPACE" get crdbcluster -o json | jq -r '
  .items[]
  | [.metadata.name, .apiVersion, (.metadata.labels["app.kubernetes.io/instance"] // ""), (.metadata.labels["crdb.io/migrate"] // ""), (.metadata.labels["crdb.cockroachlabs.com/migration"] // "")]
  | @tsv
'
```

If no `CrdbCluster` rows are returned, stop the object-specific migration debugging and report that no live `CrdbCluster` exists in the namespace. You may collect `CrdbNode` owner references and labels as teardown evidence, but do not treat those values as a replacement for a discovered `CrdbCluster`.

If multiple rows are returned, choose the target by `metadata.name`; do not use a CockroachDB version or image tag as the object name.

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

Check the local migration guide and chart changelog for version-specific migration fixes:

- [v1alpha1 to v1beta1 Migration Guide](../../../cockroachdb-parent/MIGRATION_v1alpha1_to_v1beta1.md)
- [CockroachDB Helm Chart Changelog](../../../CHANGELOG.md)

## Step 2: Inspect Migration State

```bash
jq \
  --arg modePath "$CRDB_MODE_PATH" \
  --arg migrationPath "$CRDB_MIGRATION_PATH" \
  --arg conditionsPath "$CRDB_CONDITIONS_PATH" \
  --arg observedGenerationPath "$CRDB_OBSERVED_GENERATION_PATH" \
  '
  def value($path): if $path == "" then null else getpath($path | split(".")) end;
  {
  apiVersion,
  name: .metadata.name,
  schemaPaths: {
    mode: $modePath,
    migration: $migrationPath,
    conditions: $conditionsPath,
    observedGeneration: $observedGenerationPath
  },
  mode: value($modePath),
  migrateLabel: .metadata.labels["crdb.io/migrate"],
  migrationStatus: .metadata.labels["crdb.cockroachlabs.com/migration"],
  migration: value($migrationPath),
  conditions: value($conditionsPath),
  generation: .metadata.generation,
  observedGeneration: value($observedGenerationPath)
}' "$CRDB_MIGRATION_DIR/crdbcluster.json"

kubectl -n "$OPERATOR_NAMESPACE" logs -l app=cockroach-operator --tail=300 | grep -Ei 'migrationctrl|migration|phase|cert' || true
kubectl -n "$CRDB_NAMESPACE" get crdbnodes -o wide
kubectl -n "$CRDB_NAMESPACE" get crdbnodes -o yaml
```

## Step 3: Inspect Source Workload

For Helm StatefulSet migration:

```bash
kubectl -n "$CRDB_NAMESPACE" get sts <source-statefulset> -o json | jq '{
  replicas: .spec.replicas,
  readyReplicas: .status.readyReplicas,
  migrateLabel: .metadata.labels["crdb.io/migrate"],
  ownerReferences: .metadata.ownerReferences
}'

kubectl -n "$CRDB_NAMESPACE" get sts <source-statefulset> -o yaml
kubectl -n "$CRDB_NAMESPACE" get pods -l app.kubernetes.io/name=cockroachdb -o yaml | grep -E 'name:|ownerReferences:|kind:|uid:|controller:' -A8
kubectl -n "$CRDB_NAMESPACE" get pvc -o yaml | grep -E 'name:|ownerReferences:|kind:|uid:|controller:' -A8
```

For public operator v1alpha1 migration:

```bash
kubectl -n "$CRDB_NAMESPACE" get crdbcluster.crdb.cockroachlabs.com "$CRDBCLUSTER" -o yaml
kubectl -n "$CRDB_NAMESPACE" get crdbcluster.v1beta1.crdb.cockroachlabs.com "$CRDBCLUSTER" -o yaml 2>&1 || true
kubectl -n "$OPERATOR_NAMESPACE" get svc cockroach-webhook-service
kubectl -n "$OPERATOR_NAMESPACE" get endpoints cockroach-webhook-service
kubectl get validatingwebhookconfigurations | grep cockroach
```

The operator supports both v1alpha1 and v1beta1 through conversion webhooks. If conversion fails, check webhook service health and operator logs.

## Step 4: Interpret Migration Phases

| Phase | What Happens | What to Check |
|---|---|---|
| Init | Operator validates prerequisites and prepares migration | Migration label is `start`; check operator logs for validation errors |
| CertMigration | Operator migrates or creates TLS certificate resources | Check certificate Secrets, CA resources, cert-manager resources, and cert metadata |
| PodMigration | Operator creates `CrdbNode` resources to take over pods from the StatefulSet | Check `CrdbNode` creation and pod ownerReferences |
| Finalization | Operator sets `mode=MutableOnly` and stops source migration work | `CrdbCluster` mode should be `MutableOnly`; migration label should be `finalized` |
| Complete | Customer deletes the source StatefulSet and the delete event triggers final reconcile | StatefulSet must be manually deleted only after finalization; migration label should become `complete` |

## Step 5: Debug Common Migration Stalls

### Migration Not Progressing

```bash
jq --arg migrationPath "$CRDB_MIGRATION_PATH" '
  def value($path): if $path == "" then null else getpath($path | split(".")) end;
  {migrationPath: $migrationPath, migration: value($migrationPath)}
' "$CRDB_MIGRATION_DIR/crdbcluster.json"
kubectl -n "$OPERATOR_NAMESPACE" logs -l app=cockroach-operator --tail=300 | grep -Ei 'migration|phase|cert|error' || true
kubectl -n "$OPERATOR_NAMESPACE" get deploy cockroach-operator -o jsonpath='{.spec.template.spec.containers[0].args}{"\n"}'
```

Check:

- The migration feature is enabled in operator args, if the deployed version uses a feature flag.
- The source StatefulSet still exists when phase is before `finalized`.
- The source StatefulSet spec has not been changed mid-migration.
- Certificate prerequisites are present and trusted.
- `CrdbNode` objects are being created and observed.

### CertMigration Issues

Use [configuring-cockroachdb-helm-tls](../../cockroachdb-operations-and-lifecycle/configuring-cockroachdb-helm-tls/SKILL.md). Confirm:

- CA ConfigMap or Secret exists.
- Node, HTTP, and root client certificate resources exist.
- Certificate expiry, issuer, subject, and SANs match generated pod DNS names.
- Cert-manager `Certificate` resources are Ready, when cert-manager is used.
- Secret keys are present without printing private key values.

### PodMigration Issues

```bash
kubectl -n "$CRDB_NAMESPACE" get pods -l app.kubernetes.io/name=cockroachdb -o wide
kubectl -n "$CRDB_NAMESPACE" describe pods -l app.kubernetes.io/name=cockroachdb
kubectl -n "$CRDB_NAMESPACE" get crdbnodes -o custom-columns=NAME:.metadata.name,PHASE:.status.phase,NODE_ID:.status.nodeID,OBSERVED:.status.observedGeneration
kubectl -n "$CRDB_NAMESPACE" get pvc -o json | jq '.items[] | {name: .metadata.name, ownerReferences: .metadata.ownerReferences, storageClass: .spec.storageClassName, capacity: .status.capacity}'
```

Check:

- `CrdbNode` resources are created for expected pods.
- Pods and PVCs have expected ownership after the operator takes over.
- PVCs are not orphaned before deleting source resources.

### Finalization or Completion Stuck

Check:

- `mode` is `MutableOnly`, not `Disabled`.
- migration label is `finalized` before deleting the source StatefulSet.
- source StatefulSet deletion happened only after finalization.
- migration label eventually becomes `complete`.
- operator logs show a final reconcile after the source StatefulSet delete event.

## Step 6: Post-Migration Operator Stall

If the operator stops reconciling after migration completes:

1. Check the initialization condition. If missing on an already-initialized cluster, the operator may try to initialize and block.
2. Verify `mode` is `MutableOnly`.
3. Verify the migration label is `complete`, not `start` or `finalized`.
4. Use [collecting-cockroachdb-operator-escalation-packet](../../cockroachdb-observability-and-diagnostics/collecting-cockroachdb-operator-escalation-packet/SKILL.md) to collect pprof goroutine dump, metrics, and logs before restart.

## Output Format

Return findings in this order:

1. Migration type and current phase
2. Source workload state
3. Target `CrdbCluster` mode, migration labels, and schema-grounded migration status
4. `CrdbNode`, pod, and PVC ownership state
5. Current blocker and likely cause
6. Safe next action
7. Whether escalation packet collection is required

## References

- [diagnosing-cockroachdb-helm-deployments](../../cockroachdb-observability-and-diagnostics/diagnosing-cockroachdb-helm-deployments/SKILL.md)
- [collecting-cockroachdb-operator-escalation-packet](../../cockroachdb-observability-and-diagnostics/collecting-cockroachdb-operator-escalation-packet/SKILL.md)
- [v1alpha1 to v1beta1 Migration Guide](../../../cockroachdb-parent/MIGRATION_v1alpha1_to_v1beta1.md)
- [CockroachDB Helm Chart Changelog](../../../CHANGELOG.md)
