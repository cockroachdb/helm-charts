# Cloud sweeper

The cloud sweeper brings our provider cleanup scripts together. We can run it
locally or through the [Cloud sweeper CI job](../../.github/workflows/cloud-sweeper.yaml)
to clean up stale test resources in GCP (GKE and OpenShift) and Azure (AKS).
It uses the ownership and age metadata attached to each resource to decide what
can be deleted. AWS is not covered by this workflow yet.

Every cleanup command prints the resources it selects before deleting them.
Deletion is the default. We can use `--dry-run` to preview the resources and
commands without changing anything in the cloud.

## How cleanup works

We have three ways to recover test resources:

1. **Normal teardown:** when a test succeeds or fails, its provider teardown
   cleans up the resources that run created. There is no 24-hour wait here.
2. **Daily or manual sweep:** if a runner disappears or a job is cancelled
   before teardown finishes, the sweeper discovers eligible resources older
   than 24 hours and calls the same provider scripts to delete them.
3. **Older leftovers:** resources with reliable ownership and age evidence can
   be cleaned up on the first sweep. For isolated OpenShift service accounts
   without a reliable age, the first sweep records when we saw them in GCS.
   A later sweep can delete them once at least 24 hours have passed. Running
   the job repeatedly straight away does not shorten that wait.

The difference is discovery: teardown knows what its test created, while the
sweeper has to work out which leftover resources are safe to delete. Use
`sweep.sh` for that discovery. The provider helpers take an explicit resource
or installer directory and do not apply the sweeper's age and completed-run
checks themselves.

GKE and OpenShift teardown call these helpers directly. Azure teardown starts
resource-group deletion, while its CI cleanup step and the sweeper use `azure.sh`
to delete recorded groups and wait for deletion to finish. Azure groups already
carry ownership and run tags, so they do not need the GCP metadata helper or GCS
recovery state. Deleting a group removes its resources, and AKS also removes its
managed node resource group.

## How we select resources

New CI resources carry ownership, GitHub run ID and creation timestamps in
labels, tags or VPC descriptions. OpenShift also saves installer metadata and
an ownership marker to GCS before provisioning. The sweeper checks that the
resource belongs to this repository and cloud scope, is at least 24 hours old,
and belongs to a completed GitHub run. If the GitHub API check fails, discovery
stops.

For older resources, we use these clues:

- **GKE:** known test VPC/cluster names and provider creation times. Every
  cluster in a VPC must qualify, and resource IDs are checked again before
  deletion. Detached test disks and load-balancer dependencies are also
  checked for live references before removal.
- **OpenShift:** saved installer metadata, or a known test infra ID with dated
  resources and a clear region. The installer removes the cluster's resources,
  including its per-cluster service accounts. Isolated accounts without dated
  evidence use the first-seen process above.
- **Azure:** resource-group ownership and run tags. If the creation timestamp
  is missing, we use the completed run's update time as a conservative age.

Discovery without run IDs waits until there are no other active or queued
repository workflows. Resources with unclear ownership, age or region are
skipped. Shared CI accounts, workload identity, shared DNS zones, the state
bucket and retained encryption keys stay outside automatic selection.

We can keep particular resources by setting `CLEANUP_PROTECTED_RESOURCES` as a
repository variable, or as an environment variable for local runs. It accepts
comma- or whitespace-separated VPC names, cluster names and OpenShift infra IDs.
An infra ID also protects resources with that prefix. Provider labels
`cleanup-retain=true` and Azure tags `CleanupRetain=true` are supported too.

## Running through GitHub Actions

The **Cloud sweeper** workflow runs every day at **03:37 UTC**, including weekly
test days. It skips resources belonging to active runs.

For a manual run, open **Actions → Cloud sweeper → Run workflow**. Enable
`dry_run` to preview, or leave it disabled to delete eligible resources. We can
use this for the initial cleanup of existing leftovers and for later retries.
The CLI equivalent is:

```sh
gh workflow run cloud-sweeper.yaml -f dry_run=true
# Use dry_run=false when ready to delete.
```

You need permission to run workflows in the repository. CI uses the existing
GCP workload identity and Azure federated identity, configured through these
repository secrets:

- GCP: `GCP_PROJECT_ID`, `GCP_WORKLOAD_IDENTITY_PROVIDER`, `GCP_SERVICE_ACCOUNT`.
- Azure: `AZURE_SUBSCRIPTION_ID`, `AZURE_CLIENT_ID`, `AZURE_TENANT_ID`.

Those identities need permission to list and delete the selected test resources.
GCP cleanup also needs access to the relevant IAM accounts, storage and DNS
resources used by OpenShift, plus read/write/delete access to the cleanup state
in GCS. The job uses the existing bucket without creating a new one.

## Running locally

We need Bash, jq, `gh` and the provider CLI. GCP cleanup also uses kubectl, the
GKE auth plugin and openshift-install. Authenticate `gh` with repository Actions
read access, and authenticate the cloud CLI with access to the test project or
subscription. A user account or service account can be used if it has the
permissions described above.

For GCP, authenticate both gcloud and application-default credentials for the
OpenShift installer, or use a credentials file through
`GOOGLE_APPLICATION_CREDENTIALS`:

```sh
gh auth login
gcloud auth login
gcloud auth application-default login

export GCP_PROJECT_ID=YOUR_TEST_PROJECT
export GCP_CLEANUP_STATE_URI=gs://YOUR_EXISTING_BUCKET/cleanup
bash scripts/cleanup/sweep.sh gcp --dry-run
```

For Azure, log in to an account with access to the test subscription:

```sh
az login
export AZURE_SUBSCRIPTION_ID=YOUR_TEST_SUBSCRIPTION
bash scripts/cleanup/sweep.sh azure --dry-run
```

Omit `--dry-run` to delete the listed resources. `--max-age-hours HOURS` can
increase the minimum age, but cannot lower it below 24 hours. Dry runs still
need read access and a GitHub run-status check, but do not write GCS state.
GCP sweeps use a temporary kubeconfig.

Sweeper jobs have a 55-minute timeout, and the Azure helper waits up to 45 minutes
for deletion. Provider tools have their own polling and retries, with no
shared retry loop around every command. If a sweep fails, check its output and
rerun it so the saved state can help the next attempt finish.

We can also call a provider helper for a particular resource:

```sh
bash scripts/cleanup/gcp.sh network VPC_NAME --dry-run
bash scripts/cleanup/openshift.sh install INSTALL_DIRECTORY --dry-run
bash scripts/cleanup/azure.sh resource-group RESOURCE_GROUP --dry-run
```

The OpenShift preview lists matching compute resources and installer IDs. The
installer discovers additional dependencies, including DNS and storage, when
it runs the actual deletion.

## Investigating a failure

Start with the failed job's test output and cleanup steps in GitHub Actions.
Sweeper logs show what was selected, skipped or deleted. They stay in Actions
without a separate log archive in GCS.

If we need more detail, rerun **advanced-features-tests** or **integration-tests**
with `collect_debug` enabled. This starts the extra snapshots and background
polling, and uploads a separate debug artifact. OpenShift also uploads snapshots
and available test/installer logs to GCS. These extra diagnostics are disabled
by default. Normal test output and the installer's own logging remain enabled.

Download the run's test-results artifact and, when enabled, its `*-debug-*`
artifact. Collecting diagnostics does not preserve CI clusters: teardown still
runs. For local OpenShift tests, `OPENSHIFT_SKIP_TEARDOWN=true` can keep a cluster
around for investigation. CI ignores that flag.

We can take a snapshot directly while the resources still exist:

```sh
export GCP_PROJECT_ID=YOUR_TEST_PROJECT
export OPENSHIFT_INSTALL_DIRS=/path/to/install-directory
bash scripts/debug/openshift.sh snapshot failure
# Optional: set OPENSHIFT_DEBUG_GCS_URI to upload diagnostics.
bash scripts/debug/openshift.sh ensure-gcs
bash scripts/debug/openshift.sh sync

export AZURE_SUBSCRIPTION_ID=YOUR_TEST_SUBSCRIPTION
export AZURE_RESOURCE_GROUPS_FILE=/path/to/recorded-resource-groups.txt
bash scripts/debug/azure.sh snapshot failure
```

Both debug scripts support `watch SECONDS`, for example `watch 120`. Stop it
with Ctrl-C when finished. Direct script calls collect diagnostics regardless
of the workflow's `E2E_DEBUG` setting. `ensure-gcs` only checks whether the debug
bucket is available. These scripts do not delete cloud resources.

### Environment variables

These are the settings used by the cleanup and diagnostic scripts:

| Variable | What it controls |
| --- | --- |
| `GCP_PROJECT_ID` | Required GCP project to inspect and clean up for GCP/OpenShift. |
| `AZURE_SUBSCRIPTION_ID` | Required Azure subscription to inspect and clean up. |
| `GH_TOKEN` | Optional token for `gh`, or use `gh auth login`. Needs Actions read access for discovery. |
| `GITHUB_REPOSITORY` | Repository whose run status is checked. Defaults to `cockroachdb/helm-charts`. |
| `GCP_CLEANUP_STATE_URI` | Required existing GCS `gs://BUCKET/cleanup` prefix for GCP sweeps. CI uses `gs://${GCP_PROJECT_ID}-openshift-ci-debug/cleanup`. |
| `OPENSHIFT_CLEANUP_GCS_URI` | Where CI provisioning saves installer/ownership metadata and normal teardown removes it. CI uses the same `cleanup/` prefix. |
| `CLEANUP_PROTECTED_RESOURCES` | Resource names or infra IDs to retain. Set the same repository variable for CI. |
| `CLEANUP_DRY_RUN` | `true` previews script deletions and defaults to `false`. `--dry-run` sets this for the helpers too. Test teardown does not use this preview switch. |
| `GOOGLE_APPLICATION_CREDENTIALS` | Optional credentials-file path for Google clients/the installer when not using application-default login. gcloud also needs its own authenticated account. |
| `GOOGLE_CREDENTIALS` | The OpenShift test wrapper also accepts a credentials-file path here and passes it to the installer as `GOOGLE_APPLICATION_CREDENTIALS`. |
| `USE_GKE_GCLOUD_AUTH_PLUGIN` | Set to `true` when using the GKE kubectl auth plugin, as CI does. |
| `E2E_DEBUG` | Workflow switch set from `collect_debug`. Defaults to `false`. Local Make targets do not start snapshots just because this is set. |
| `OPENSHIFT_DEBUG_DIR` | Snapshot output directory. Defaults to `${RUNNER_TEMP:-/tmp}/openshift-debug`. |
| `OPENSHIFT_DEBUG_GCS_URI` | Optional GCS upload prefix. Unset means no diagnostic uploads. CI uses a run/attempt/region prefix in the existing debug bucket. |
| `OPENSHIFT_BASE_DOMAIN` | Optional DNS domain for OpenShift debug snapshots. Unset skips the DNS snapshot. Provisioning uses it separately. |
| `OPENSHIFT_INSTALL_DIRS` | Explicit installer directories, one per line, for diagnostics and recorded-install cleanup. |
| `OPENSHIFT_INSTALL_DIRS_ROOT` | Alternatively, inspect directories immediately under this root. |
| `TMPDIR`, `RUNNER_TEMP` | Installer-directory fallback is `TMPDIR`, then `RUNNER_TEMP`, then `/tmp`, matching `helm-charts-ocp-*`. `RUNNER_TEMP` also supplies debug-output defaults. |
| `AZURE_DEBUG_DIR` | Azure snapshot output directory. Defaults to `${RUNNER_TEMP:-/tmp}/azure-debug`. |
| `AZURE_RESOURCE_GROUPS_FILE` | Recorded groups, one per line, for detailed snapshots and recorded-group cleanup. Defaults to `${RUNNER_TEMP:-/tmp}/azure-resource-groups.txt`. |
| `AZURE_RESOURCE_PREFIX` | Prefix shown/listed in Azure diagnostics. Defaults to `helm-charts-e2e` (`hc-e2e` in CI). It does not change sweeper ownership rules. |
| `OPENSHIFT_INSTALL_LOG_LEVEL` | OpenShift test installer's verbosity. Defaults to `info` and is separate from extra debug snapshots. |
| `OPENSHIFT_SKIP_TEARDOWN` | Keep resources after local OpenShift tests when `true`. Defaults to `false` and is ignored in CI. |

GitHub supplies `GITHUB_ACTIONS`, `GITHUB_RUN_ID`, `GITHUB_RUN_ATTEMPT`,
`GITHUB_WORKFLOW`, `GITHUB_JOB` and `GITHUB_REF_NAME` in CI. They identify runs
and appear in resource metadata or diagnostics.

The GCP helper also accepts `GCP_CLEANUP_OWNER_RUN_ID` and
`GCP_CLEANUP_EXPECTED_CLUSTERS_JSON` for checking cluster ownership/IDs, plus
`GCP_CLEANUP_REUSE_CONTEXTS=true` with `KUBECONFIG` to use existing contexts.
Normal teardown and the sweeper supply the applicable settings. Local sweeps
normally need only the scope, credentials and state settings above.

## What stays in GCS, and when it goes away

We reuse the existing `gs://${GCP_PROJECT_ID}-openshift-ci-debug` bucket in CI.
Cleanup state lives under `cleanup/`, and optional debug uploads live under
`<run-id>/<run-attempt>/<single-region-or-multi-region>/`.

| Cleanup state | Why we keep it |
| --- | --- |
| `<infra-id>/metadata.json` and `ci-cleanup.json` | Installer details and ownership saved before provisioning, so a replacement runner can clean up. |
| `recovered-installs/<infra-id>/metadata.json` | Recovered installer metadata saved before deleting older leftovers. |
| `orphan-accounts/<account-id>.json` | First-seen time and immutable ID for accounts without reliable age evidence. |
| `gcp-<vpc-name>/compute-cleanup.json` | Network and load-balancer dependency references saved before deletion, so retries can finish. |

Cleanup removes its associated state after successful deletion. Failures keep
recovery state for another attempt, and dry runs leave it unchanged. Records for
resources deleted outside these scripts can remain.

Debug uploads and GitHub artifacts are separate. The sweeper does not delete
them. GCS logs expire only if a bucket lifecycle rule removes them, and Actions
artifacts follow the repository/organization retention settings. These workflows
do not set a custom retention period. Local snapshot files need local/runner
cleanup. Successful OpenShift teardown removes its local installer directory,
while failed teardown keeps it for retry.

## Script layout

| Script | What it does |
| --- | --- |
| `sweep.sh` | Select stale resources, then call the provider helpers. |
| `discover-gcp.sh`, `policy.jq` | GCP discovery and shared age/ownership/protection rules. |
| `gcp.sh` | Delete a test VPC, GKE clusters and their cloud dependencies. |
| `openshift.sh` | Destroy installer-managed clusters and custom multi-cluster resources. |
| `azure.sh` | Delete resource groups and wait for completion. |
| `common.sh` | Print resources/commands and handle dry runs. |
| `../debug/` | Extra test snapshots and log uploads. |
| `../lib/cloud-resources.sh` | Find the local installer directories and recorded Azure groups used by cleanup and diagnostics. |

`cloud-resources.sh` is a shared library that helps the OpenShift and Azure
cleanup and debug scripts find the same local resource records. They load it
automatically, so we do not run it directly. For local runs, set
`OPENSHIFT_INSTALL_DIRS`, `OPENSHIFT_INSTALL_DIRS_ROOT` or
`AZURE_RESOURCE_GROUPS_FILE` if the records are outside their default locations,
then run the cleanup or debug command as shown above. No separate command is
needed for this helper.
