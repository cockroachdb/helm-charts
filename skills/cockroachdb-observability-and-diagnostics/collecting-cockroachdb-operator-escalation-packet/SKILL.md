---
name: collecting-cockroachdb-operator-escalation-packet
description: Collects a complete CockroachDB Operator escalation packet for TSC/TSE or operator-team handoff, including Helm state, Kubernetes resources, logs, operation-specific evidence, pprof goroutine dumps, metrics, and a customer action timeline. Use when general diagnosis cannot resolve an operator-managed CockroachDB Helm issue or before restarting a stuck operator.
compatibility: CockroachDB Helm v2 charts and operator-managed crdb.cockroachlabs.com/v1beta1 resources. Requires Kubernetes read access to the operator namespace and CockroachDB namespace. Pprof and metrics collection require port-forward access to the operator Deployment.
metadata:
  author: cockroachdb
  version: "1.1"
---

# Collecting CockroachDB Operator Escalation Packet

Collects the artifacts needed for TSC/TSE or operator-team escalation. Use this after basic diagnosis identifies an unresolved operator, Kubernetes, migration, upgrade, scale, DNS, PVC, or certificate issue. Keep collection read-only unless the customer explicitly approves a mitigation.

## When to Use This Skill

- A customer needs to escalate an operator-managed CockroachDB Helm issue to TSC
- The operator is running but not reconciling and logs/status do not explain why
- A rollout, upgrade, migration, or scale operation is stuck
- Operator logs do not explain the failure
- TSC needs consistent artifacts before advising restart, rollback, manual decommission, or status repair

## Safety Considerations

- Collect this packet before restarting, scaling, or deleting the operator during active operations.
- Do not delete `CrdbNode`, PVC, Secret, service, version checker job, or StatefulSet resources while collecting data.
- Never save Secret YAML, `.data`, `.stringData`, tokens, passwords, or private keys. Collect only Secret names, key names, and decoded public certificate metadata. Helm values, resource YAML, logs, annotations and command lines can also contain credentials. Redact them before sharing. Keep an unshared original only under the user's access controls.
- Do not manually edit `CrdbCluster.status`.
- Do not run `cockroach init` on an existing cluster.
- If the operator is paused or disabled, record that state before changing anything.

## Execution Discipline

- Execute one step at a time and inspect the output before moving on. Do not run the packet collection in parallel. Earlier results determine which operation-specific sections are relevant.
- Run every step in the same shell session. Packet collection depends on `OPERATOR_NAMESPACE`, `CRDB_NAMESPACE`, `CRDBCLUSTER`, and the `CRDB_*_PATH` exports set in Step 1. Opening a new shell drops those variables and downstream `jq`/`kubectl` calls will read from the wrong object or emit empty artifacts.
- Keep collection read-only unless the user explicitly approves a mutating action for the target cluster.
- Never infer a `CrdbCluster` object name from a Helm release, service name, or CockroachDB image/version string. List `CrdbCluster` objects and use the exact `metadata.name`.
- Before reading version-sensitive `CrdbCluster` fields, save the live CRD YAML and derive field paths from the served CRD schema for the object's `apiVersion`.
- Do not run interactive `kubectl exec` shells, `kubectl debug`, port-forwards, pprof/metrics collection, or commands that use external images in production unless the user approves them. If impact or policy is unclear, involve TSE or the operator team first.
- Do not patch, annotate, delete, restart, scale, drain, decommission, or run `helm upgrade` as part of packet collection.

For operator logs, use the deployed `appLabel` if it differs from
`app=cockroach-operator` in the commands below.

## Required Inputs

- Operator namespace and Helm release
- CockroachDB namespace, Helm release, and discovered `CrdbCluster.metadata.name`
- Kubernetes context and cluster/provider
- Current operation: install, upgrade, scale up/down, cert rotation, migration, routine maintenance, or recovery
- Current and target CockroachDB image, if an upgrade is involved
- Customer timeline: what changed, what commands were run, and what was already tried

## Packet Layout

Ask the customer to gather outputs into a single directory:

```bash
mkdir -p crdb-operator-escalation
```

Use clear filenames, for example `operator-logs.txt`, `crdbcluster.yaml`, `events.txt`, `goroutine_dump.txt`, and `metrics_dump.txt`.

## Step 1: Context and Version Inventory

```bash
export OPERATOR_NAMESPACE="<operator-namespace>"
export OPERATOR_RELEASE="<operator-release>"
export CRDB_NAMESPACE="<cockroachdb-namespace>"
export CRDB_HELM_RELEASE="<cockroachdb-release>"

kubectl config current-context > crdb-operator-escalation/kube-context.txt
kubectl version -o yaml > crdb-operator-escalation/kubernetes-version.txt

helm -n "$OPERATOR_NAMESPACE" status "$OPERATOR_RELEASE" > crdb-operator-escalation/operator-helm-status.txt 2>&1 || true
helm -n "$OPERATOR_NAMESPACE" history "$OPERATOR_RELEASE" > crdb-operator-escalation/operator-helm-history.txt 2>&1 || true
helm -n "$CRDB_NAMESPACE" status "$CRDB_HELM_RELEASE" > crdb-operator-escalation/crdb-helm-status.txt 2>&1 || true
helm -n "$CRDB_NAMESPACE" history "$CRDB_HELM_RELEASE" > crdb-operator-escalation/crdb-helm-history.txt 2>&1 || true

kubectl -n "$OPERATOR_NAMESPACE" get deploy cockroach-operator -o jsonpath='{.spec.template.spec.containers[0].image}{"\n"}' > crdb-operator-escalation/operator-image.txt
kubectl get crd crdbclusters.crdb.cockroachlabs.com crdbnodes.crdb.cockroachlabs.com -o wide > crdb-operator-escalation/crds.txt
kubectl get crd crdbclusters.crdb.cockroachlabs.com -o yaml > crdb-operator-escalation/crdbclusters-crd.yaml
kubectl get crd crdbclusters.crdb.cockroachlabs.com -o json > crdb-operator-escalation/crdbclusters-crd.json
kubectl get crd crdbnodes.crdb.cockroachlabs.com -o yaml > crdb-operator-escalation/crdbnodes-crd.yaml

kubectl -n "$CRDB_NAMESPACE" get crdbcluster -o json | jq -r '
  .items[]
  | [.metadata.name, .apiVersion, (.metadata.labels["app.kubernetes.io/instance"] // ""), (.metadata.generation | tostring)]
  | @tsv
' > crdb-operator-escalation/crdbcluster-candidates.tsv
```

Inspect `crdbcluster-candidates.tsv`. If it is empty, record that no live target exists yet and collect source StatefulSet, events and logs for an early migration wait. Do not invent a CrdbCluster name or status. If more than one `CrdbCluster` exists in the namespace, select the target by `metadata.name`. Do not use a CockroachDB version or image tag as the object name.

```bash
export CRDBCLUSTER="<metadata.name-from-crdbcluster-candidates.tsv>"
test -n "$CRDBCLUSTER"

kubectl -n "$CRDB_NAMESPACE" get crdbcluster "$CRDBCLUSTER" -o yaml > crdb-operator-escalation/crdbcluster.yaml
kubectl -n "$CRDB_NAMESPACE" get crdbcluster "$CRDBCLUSTER" -o json > crdb-operator-escalation/crdbcluster.json

export CRDBCLUSTER_API_VERSION="$(jq -r '.apiVersion | split("/")[-1]' crdb-operator-escalation/crdbcluster.json)"
export CRDBCLUSTER_SCHEMA_JSON=crdb-operator-escalation/crdbcluster-schema.json
jq -e --arg version "$CRDBCLUSTER_API_VERSION" '
  .spec.versions[] | select(.name == $version) | .schema.openAPIV3Schema
' crdb-operator-escalation/crdbclusters-crd.json > "$CRDBCLUSTER_SCHEMA_JSON"

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
export CRDB_REGIONS_PATH="$(crdb_first_schema_path spec.regions)"
export CRDB_DESIRED_IMAGE_PATH="$(crdb_first_schema_path spec.template.spec.image spec.image.name spec.image)"
export CRDB_OBSERVED_GENERATION_PATH="$(crdb_first_schema_path status.observedGeneration)"
export CRDB_RECONCILED_PATH="$(crdb_first_schema_path status.reconciled)"
export CRDB_READY_NODES_PATH="$(crdb_first_schema_path status.readyNodes)"
export CRDB_STATUS_IMAGE_PATH="$(crdb_first_schema_path status.image status.crdbcontainerimage)"
export CRDB_STATUS_VERSION_PATH="$(crdb_first_schema_path status.version)"
export CRDB_ACTIONS_PATH="$(crdb_first_schema_path status.actions status.operatorActions)"
export CRDB_CONDITIONS_PATH="$(crdb_first_schema_path status.conditions)"

printf '%s\n' \
  "apiVersion=$CRDBCLUSTER_API_VERSION" \
  "mode=$CRDB_MODE_PATH" \
  "regions=$CRDB_REGIONS_PATH" \
  "desiredImage=$CRDB_DESIRED_IMAGE_PATH" \
  "observedGeneration=$CRDB_OBSERVED_GENERATION_PATH" \
  "reconciled=$CRDB_RECONCILED_PATH" \
  "readyNodes=$CRDB_READY_NODES_PATH" \
  "statusImage=$CRDB_STATUS_IMAGE_PATH" \
  "statusVersion=$CRDB_STATUS_VERSION_PATH" \
  "actions=$CRDB_ACTIONS_PATH" \
  "conditions=$CRDB_CONDITIONS_PATH" \
  > crdb-operator-escalation/crdbcluster-schema-paths.txt
```

Get the CockroachDB version from a Ready pod:

```bash
kubectl -n "$CRDB_NAMESPACE" exec <ready-crdb-pod> -c cockroachdb -- \
  /cockroach/cockroach version > crdb-operator-escalation/crdb-version.txt 2>&1
```

## Step 2: Resource Specifications and Status

```bash
kubectl -n "$OPERATOR_NAMESPACE" get deploy,pod,svc,endpoints -o wide > crdb-operator-escalation/operator-resources.txt
kubectl -n "$OPERATOR_NAMESPACE" describe deploy cockroach-operator > crdb-operator-escalation/operator-deploy-describe.txt
kubectl -n "$OPERATOR_NAMESPACE" describe pods -l app=cockroach-operator > crdb-operator-escalation/operator-pods-describe.txt
kubectl -n "$OPERATOR_NAMESPACE" get deploy cockroach-operator -o yaml > crdb-operator-escalation/operator-deploy.yaml

kubectl -n "$CRDB_NAMESPACE" describe crdbcluster "$CRDBCLUSTER" > crdb-operator-escalation/crdbcluster-describe.txt
kubectl -n "$CRDB_NAMESPACE" get crdbnodes -o yaml > crdb-operator-escalation/crdbnodes.yaml
kubectl -n "$CRDB_NAMESPACE" describe crdbnodes > crdb-operator-escalation/crdbnodes-describe.txt
kubectl -n "$CRDB_NAMESPACE" get pod,svc,endpoints,pvc,pdb -o wide > crdb-operator-escalation/crdb-resources-wide.txt
kubectl -n "$CRDB_NAMESPACE" describe pods -l app.kubernetes.io/name=cockroachdb > crdb-operator-escalation/crdb-pods-describe.txt
kubectl -n "$CRDB_NAMESPACE" describe pvc > crdb-operator-escalation/pvc-describe.txt
kubectl -n "$CRDB_NAMESPACE" describe pdb > crdb-operator-escalation/pdb-describe.txt
kubectl -n "$CRDB_NAMESPACE" get events --sort-by=.lastTimestamp > crdb-operator-escalation/events.txt
```

Summarize key cluster status:

```bash
jq \
  --arg modePath "$CRDB_MODE_PATH" \
  --arg regionsPath "$CRDB_REGIONS_PATH" \
  --arg desiredImagePath "$CRDB_DESIRED_IMAGE_PATH" \
  --arg observedGenerationPath "$CRDB_OBSERVED_GENERATION_PATH" \
  --arg reconciledPath "$CRDB_RECONCILED_PATH" \
  --arg readyNodesPath "$CRDB_READY_NODES_PATH" \
  --arg statusImagePath "$CRDB_STATUS_IMAGE_PATH" \
  --arg statusVersionPath "$CRDB_STATUS_VERSION_PATH" \
  --arg actionsPath "$CRDB_ACTIONS_PATH" \
  --arg conditionsPath "$CRDB_CONDITIONS_PATH" \
  '
  def value($path): if $path == "" then null else getpath($path | split(".")) end;
  {
  apiVersion,
  name: .metadata.name,
  schemaPaths: {
    mode: $modePath,
    regions: $regionsPath,
    desiredImage: $desiredImagePath,
    observedGeneration: $observedGenerationPath,
    reconciled: $reconciledPath,
    readyNodes: $readyNodesPath,
    statusImage: $statusImagePath,
    statusVersion: $statusVersionPath,
    actions: $actionsPath,
    conditions: $conditionsPath
  },
  mode: value($modePath),
  nodes: (value($regionsPath) // [] | map(.nodes)),
  regions: value($regionsPath),
  desiredImage: value($desiredImagePath),
  generation: .metadata.generation,
  observedGeneration: value($observedGenerationPath),
  reconciled: value($reconciledPath),
  readyNodes: value($readyNodesPath),
  statusImage: value($statusImagePath),
  statusVersion: value($statusVersionPath),
  actions: value($actionsPath),
  conditions: value($conditionsPath),
  labels: .metadata.labels,
  annotations: .metadata.annotations
}' crdb-operator-escalation/crdbcluster.json > crdb-operator-escalation/crdbcluster-summary.json

kubectl -n "$CRDB_NAMESPACE" get crdbnodes \
  -o 'custom-columns=NAME:.metadata.name,GENERATION:.metadata.generation,OBSERVED:.status.observedGeneration,DECOMMISSION:.status.decommission,REVISION:.metadata.annotations.crdb\.cockroachlabs\.com/clusterNodeRevision,NODE_ID:.status.nodeID' \
  > crdb-operator-escalation/crdbnodes-summary.txt
```

## Step 3: Logs

Collect full recent logs, not filtered snippets:

```bash
kubectl -n "$OPERATOR_NAMESPACE" logs -l app=cockroach-operator --since=30m --tail=2000 > crdb-operator-escalation/operator-logs.txt 2>&1
kubectl -n "$OPERATOR_NAMESPACE" logs -l app=cockroach-operator --previous --since=30m --tail=2000 > crdb-operator-escalation/operator-previous-logs.txt 2>&1 || true
```

For each CockroachDB pod:

```bash
kubectl -n "$CRDB_NAMESPACE" logs <crdb-pod> -c cockroachdb --since=30m --tail=2000 > crdb-operator-escalation/<crdb-pod>-cockroachdb.log 2>&1
kubectl -n "$CRDB_NAMESPACE" logs <crdb-pod> -c cockroachdb --previous --since=30m --tail=2000 > crdb-operator-escalation/<crdb-pod>-cockroachdb-previous.log 2>&1 || true
kubectl -n "$CRDB_NAMESPACE" logs <crdb-pod> -c cert-reloader --since=30m --tail=500 > crdb-operator-escalation/<crdb-pod>-cert-reloader.log 2>&1 || true
```

## Step 4: Operation-Specific Evidence

### Upgrade

```bash
jq \
  --arg desiredImagePath "$CRDB_DESIRED_IMAGE_PATH" \
  --arg statusImagePath "$CRDB_STATUS_IMAGE_PATH" \
  --arg statusVersionPath "$CRDB_STATUS_VERSION_PATH" \
  --arg actionsPath "$CRDB_ACTIONS_PATH" \
  --arg conditionsPath "$CRDB_CONDITIONS_PATH" \
  '
  def value($path): if $path == "" then null else getpath($path | split(".")) end;
  {
  apiVersion,
  name: .metadata.name,
  desiredImagePath: $desiredImagePath,
  desiredImage: value($desiredImagePath),
  statusImagePath: $statusImagePath,
  statusImage: value($statusImagePath),
  statusVersionPath: $statusVersionPath,
  statusVersion: value($statusVersionPath),
  actionsPath: $actionsPath,
  actions: value($actionsPath),
  conditionsPath: $conditionsPath,
  conditions: value($conditionsPath),
  annotations: .metadata.annotations,
}' crdb-operator-escalation/crdbcluster.json > crdb-operator-escalation/upgrade-status.json

kubectl -n "$CRDB_NAMESPACE" get jobs -o wide > crdb-operator-escalation/jobs.txt
kubectl -n "$CRDB_NAMESPACE" get pods -o 'custom-columns=NAME:.metadata.name,IMAGE:.spec.containers[0].image,PHASE:.status.phase' > crdb-operator-escalation/pod-images.txt
kubectl -n "$CRDB_NAMESPACE" describe job <version-checker-job> > crdb-operator-escalation/version-checker-job.txt 2>&1 || true
kubectl -n "$CRDB_NAMESPACE" logs -l job-name=<version-checker-job> > crdb-operator-escalation/version-checker-logs.txt 2>&1 || true
```

Include current and target CRDB image and whether any version checker job or pod was deleted.

### Scale Down or Decommission

```bash
kubectl -n "$CRDB_NAMESPACE" exec <ready-crdb-pod> -c cockroachdb -- \
  /cockroach/cockroach node status --decommission > crdb-operator-escalation/node-decommission-status.txt 2>&1

kubectl -n "$CRDB_NAMESPACE" get crdbnodes -o json | jq '[.items[] | select(.status.decommission != null and .status.decommission != "")] | {count: length, nodes: [.[] | {name: .metadata.name, decommission: .status.decommission}]}' \
  > crdb-operator-escalation/decommissioning-crdbnodes.json
```

Include original and target node count, whether multiple nodes changed at once, and whether manual drain/decommission was attempted.

### Scale Up

Capture whether machines, Kubernetes nodes, disks, storage classes, topology spread constraints, or node labels changed:

```bash
kubectl get nodes -L topology.kubernetes.io/region,topology.kubernetes.io/zone > crdb-operator-escalation/nodes-locality.txt
kubectl get storageclass > crdb-operator-escalation/storageclasses.txt
kubectl -n "$CRDB_NAMESPACE" get pvc -o wide > crdb-operator-escalation/pvc-wide.txt
```

### Certificate Rotation

```bash
kubectl -n "$CRDB_NAMESPACE" get secret,configmap | grep -E 'ca|node|client|tls|cert' > crdb-operator-escalation/cert-resources.txt || true
kubectl -n "$CRDB_NAMESPACE" get certificate,issuer,clusterissuer -o wide > crdb-operator-escalation/cert-manager-resources.txt 2>&1 || true

export NODE_SECRET="<node-tls-secret>"
export CERT_KEY="<public-certificate-key>"  # tls.crt or node.crt, as used by the source
kubectl -n "$CRDB_NAMESPACE" get secret "$NODE_SECRET" -o json |
  jq -r --arg key "$CERT_KEY" '.data[$key]' | openssl base64 -d -A |
  openssl x509 -noout -dates -subject -issuer -text > crdb-operator-escalation/node-cert-metadata.txt
```

The `-text` output includes DNS and IP SANs and works with macOS LibreSSL too.
Do not collect private key values.

### Migration

Use [debugging-cockroachdb-operator-migrations](../../cockroachdb-onboarding-and-migrations/debugging-cockroachdb-operator-migrations/SKILL.md) for interpretation and the public certificate join-name check. Record whether this is forward migration or rollback, whether it is waiting or stopped, whether the source still exists, and the one proposed next action. Collect this before any approved mitigation and again afterward using `before` and `after` directories. An unavailable pre-change snapshot must be called out.

```bash
export SNAPSHOT="before"  # use after for the second snapshot
export MIGRATION_PACKET="crdb-operator-escalation/$SNAPSHOT"
export SOURCE_STS="<source-statefulset>"
mkdir -p "$MIGRATION_PACKET"
kubectl -n "$CRDB_NAMESPACE" get crdbclusters.v1beta1.crdb.cockroachlabs.com "$CRDBCLUSTER" -o yaml > "$MIGRATION_PACKET/crdbcluster-beta.yaml"
kubectl -n "$CRDB_NAMESPACE" get crdbclusters.v1alpha1.crdb.cockroachlabs.com "$CRDBCLUSTER" -o yaml > "$MIGRATION_PACKET/crdbcluster-alpha.yaml" 2>&1
kubectl -n "$CRDB_NAMESPACE" get crdbnodes -o yaml > "$MIGRATION_PACKET/crdbnodes.yaml"
kubectl -n "$CRDB_NAMESPACE" get sts "$SOURCE_STS" -o yaml > "$MIGRATION_PACKET/source-sts.yaml" 2>&1
kubectl -n "$CRDB_NAMESPACE" get crdbclusters.v1beta1.crdb.cockroachlabs.com "$CRDBCLUSTER" -o json | jq '{
  mode: .spec.mode, migrate: .metadata.labels["crdb.io/migrate"],
  skipReconcile: .metadata.labels["crdb.io/skip-reconcile"],
  migration: (.status.migration | {phase, settledPhase, message, lastError, migratedNodes, totalPods})
}' > "$MIGRATION_PACKET/migration-state.json"
kubectl -n "$CRDB_NAMESPACE" get events -o json | jq '[.items[] |
  select(.reason | test("^(MigrationPaused|MigrationBlocked|MigrationSourceUnreadable|MigrationSourceLost|RollbackSourceLost|DegradedHandoff|UnsupportedStartFlags|UnreadableStartCommand|DynamicStartFlag)$"))]
' > "$MIGRATION_PACKET/migration-events.json"
kubectl -n "$CRDB_NAMESPACE" get pods -o yaml > "$MIGRATION_PACKET/pods.yaml"
kubectl -n "$CRDB_NAMESPACE" get svc "$CRDBCLUSTER-join" -o yaml > "$MIGRATION_PACKET/join-service.yaml" 2>&1
kubectl -n "$CRDB_NAMESPACE" get endpoints "$CRDBCLUSTER-join" -o yaml > "$MIGRATION_PACKET/join-endpoints.yaml" 2>&1
kubectl -n "$CRDB_NAMESPACE" get configmap "$CRDBCLUSTER-ca-crt" -o json | jq '{metadata: .metadata, keys: (.data // {} | keys)}' > "$MIGRATION_PACKET/ca-configmap.json"
kubectl -n "$CRDB_NAMESPACE" get secrets -o json | jq '[.items[] | {name: .metadata.name, type, keys: (.data // {} | keys)}]' > "$MIGRATION_PACKET/secret-metadata.json"
kubectl -n "$CRDB_NAMESPACE" get certificates.cert-manager.io -o yaml > "$MIGRATION_PACKET/certificates.yaml" 2>&1
kubectl -n "$CRDB_NAMESPACE" get pvc -o json | jq '[.items[] | {
  name: .metadata.name, uid: .metadata.uid, phase: .status.phase, volume: .spec.volumeName,
  dataSource: .spec.dataSource, owners: .metadata.ownerReferences,
  labels: .metadata.labels, annotations: .metadata.annotations}]
' > "$MIGRATION_PACKET/pvcs.json"
# Optional: requires cluster-scoped read access to PersistentVolumes.
kubectl get pv -o json | jq --arg ns "$CRDB_NAMESPACE" '[.items[] |
  select(.spec.claimRef.namespace == $ns) | {name: .metadata.name, phase: .status.phase,
  claim: .spec.claimRef, reclaimPolicy: .spec.persistentVolumeReclaimPolicy}]
' > "$MIGRATION_PACKET/pvs.json"
kubectl get crd crdbclusters.crdb.cockroachlabs.com -o json | jq '{
  versions: [.spec.versions[] | {name, served, storage}], storedVersions: .status.storedVersions,
  conversion: .spec.conversion}' > "$MIGRATION_PACKET/crd-versions.json"
kubectl -n "$OPERATOR_NAMESPACE" get deploy cockroach-operator -o yaml > "$MIGRATION_PACKET/operator-deployment.yaml"
kubectl -n "$OPERATOR_NAMESPACE" get pods -o json | jq '[.items[] | {name: .metadata.name,
  containers: [.status.containerStatuses[]? | {name, image, imageID}]}]' > "$MIGRATION_PACKET/operator-digests.json"
kubectl -n "$OPERATOR_NAMESPACE" logs -l app=cockroach-operator --all-containers --prefix --max-log-requests=20 --since=30m --tail=2000 > "$MIGRATION_PACKET/operator.log" 2>&1
```


For adoption or renewal failures, also collect the public Service and its EndpointSlices,
rotation CronJobs, the parent/node certificate references, and live pod CA mounts. Record
the original client port, effective TLS mode and the result of a SQL connection through the
Service. For secure sources use TLS hostname verification, including a custom client DNS
name when configured. For insecure sources use `sslmode=disable` and record whether all
certificate modes stayed disabled in exported values. Direct pod SQL
success does not establish Service connectivity. Collect only the four signer annotation
values (`certificate-valid-from`, `certificate-valid-upto`, `certificate-duration` and
`secret-data-hash`), Secret names/key names, and decoded public certificate expiry/SANs.
Never save Secret data. Record whether export kept external certificates or restored the
self-signer, and any missing-data-hash error. A CA key alone does not restore rotation.
Explicit `caProvided` settings in a matching source Helm release can restore the signer
without CA annotations. Record the exported CA name and rotation setting, never the stored
release Secret. Custom node/client Secret names remain external. Check references before
calling the old `<cluster>-ca-crt` ConfigMap unused.

For a same-release Helm adoption, record which source resources disappeared and the
retention plan, especially the cert-manager issuer chain and discovery Service. After
rollback, verify the source PDB budget and Role/RoleBinding are unchanged. Record the
`crdb.cockroachlabs.com/migration-created` labels on certificate/RBAC resource metadata
when cleanup appears incomplete. Only marked certificate/RBAC resources are deleted.
Do not collect
the old Helm release manifest or provisioning Secret in the packet because they can
contain credentials. Collect relevant resource names, specs and redacted metadata instead.

For legacy-operator migrations, discover the legacy operator deployment and namespace and collect its deployment args, image digests and bounded logs with the same commands. Include admission webhook configurations and CRD conversion service endpoints when coexistence or source reads fail. Compare source replicas with pinned `totalPods` and the unreplaced count in the controller message. Record certificate SANs and the join-host check result without saving Secret contents. Record missing resources as evidence, not as permission to recreate or delete them.

Before sharing, redact credentials, SQL literals with sensitive data, connection strings, customer hostnames/IPs where required, and sensitive labels/annotations from YAML, logs, Helm output and diagnostics. Keep consistent placeholders so resource relationships remain understandable. Do not include kubeconfig or cloud credentials.

## Step 5: Operator pprof and Metrics

Collect these when the operator is running but not reconciling, logs/status do not explain why, or a worker may be blocked. In production or restricted environments, confirm with the customer, TSE, or the operator team before opening port-forwards.

In one terminal:

```bash
kubectl -n "$OPERATOR_NAMESPACE" port-forward deployment/cockroach-operator 7080:7080
```

In another terminal:

```bash
curl -s 'http://localhost:7080/debug/pprof/goroutine?debug=2' > crdb-operator-escalation/goroutine_dump.txt
curl -s 'http://localhost:7080/debug/pprof/goroutine?debug=1' > crdb-operator-escalation/goroutine_summary.txt
curl -s 'http://localhost:7080/debug/pprof/heap' > crdb-operator-escalation/heap.prof
curl -s 'http://localhost:7080/debug/pprof/profile?seconds=30' > crdb-operator-escalation/cpu.prof
curl -s 'http://localhost:7080/debug/pprof/mutex' > crdb-operator-escalation/mutex.prof
```

Metrics:

```bash
kubectl -n "$OPERATOR_NAMESPACE" port-forward deployment/cockroach-operator 8080:8080
curl -s http://localhost:8080/metrics > crdb-operator-escalation/metrics_dump.txt
grep 'controller_runtime_reconcile_total' crdb-operator-escalation/metrics_dump.txt > crdb-operator-escalation/reconcile-total.txt || true
grep 'controller_runtime_reconcile_errors_total' crdb-operator-escalation/metrics_dump.txt > crdb-operator-escalation/reconcile-errors.txt || true
grep 'workqueue_depth' crdb-operator-escalation/metrics_dump.txt > crdb-operator-escalation/workqueue-depth.txt || true
grep 'workqueue_longest_running_processor_seconds' crdb-operator-escalation/metrics_dump.txt > crdb-operator-escalation/workqueue-longest-running.txt || true
grep 'workqueue_unfinished_work_seconds' crdb-operator-escalation/metrics_dump.txt > crdb-operator-escalation/workqueue-unfinished.txt || true
```

What TSC should inspect:

- `processNextWorkItem` followed by `Reconcile` and a long-running call in `goroutine_dump.txt`
- `kube.(*Ctl).Exec`, `cockroach init`, HTTP calls, or mutex waits with long durations
- increasing `workqueue_longest_running_processor_seconds`
- growing `workqueue_depth`
- no reconcile count movement for a controller that should be active

## Step 6: Timeline

Create `crdb-operator-escalation/timeline.md` with:

- When the issue started
- What operation was in progress
- What docs or runbooks were followed
- Whether VMs, Kubernetes nodes, disks, storage classes, certs, network policies, or Helm values changed
- Every manual delete, patch, edit, restart, drain, decommission, or Helm command, in order
- What is currently serving traffic and what is unavailable
- Customer restrictions, such as no cluster-admin, no debug containers, no external images, or private registry only

## Step 7: Escalation Summary

Return a concise summary with:

1. Operation type and current impact
2. Current operator and CockroachDB versions
3. Current schema-grounded `CrdbCluster` mode, generation, observedGeneration, image, version, actions, and conditions
4. Stuck resource or symptom
5. Any unsafe operations already performed
6. Missing artifacts, if any
7. Whether pprof/metrics suggest a blocked worker

## References

- [diagnosing-cockroachdb-helm-deployments](../diagnosing-cockroachdb-helm-deployments/SKILL.md)
- [debugging-cockroachdb-operator-migrations](../../cockroachdb-onboarding-and-migrations/debugging-cockroachdb-operator-migrations/SKILL.md)
- [CockroachDB Operator chart changelog](../../../cockroachdb-operator/charts/operator/CHANGELOG.md)
