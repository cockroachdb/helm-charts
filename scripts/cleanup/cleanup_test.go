package cleanup

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

const cleanupFakeCLI = `#!/usr/bin/env bash
set -eu
tool="$(basename "$0")"
echo "$tool $*" >> "$CLEANUP_FIXTURES/commands"
fixture() { if [[ -f "$CLEANUP_FIXTURES/$1.json" ]]; then cat "$CLEANUP_FIXTURES/$1.json"; else echo '[]'; fi; }
case "$tool" in
  gh)
    [[ "${FAIL_API:-false}" != true ]] || exit 1
    case "$*" in
      *'actions/runs?'*) fixture workflows ;;
      *) fixture "run-${2##*/}" ;;
    esac ;;
  az)
    case "$1 $2" in
      'group list') fixture azure ;;
      'resource list')
        if [[ "${FAIL_LIST:-}" == missing ]]; then echo ResourceGroupNotFound >&2; exit 1; fi
        if [[ "${FAIL_LIST:-}" == permission ]]; then echo 'permission denied' >&2; exit 1; fi
        fixture azure-resources ;;
      'aks list') fixture azure-clusters ;;
      'group delete') [[ "${FAIL_DELETE:-false}" != true ]] ;;
      'group wait') : ;;
      *) exit 2 ;;
    esac ;;
  kubectl)
    case " $* " in *' get services '*) fixture services ;; *' delete service '*) : ;; *) exit 2 ;; esac ;;
  openshift-install) [[ "${FAIL_INSTALL:-false}" != true ]] ;;
  gcloud)
    case "$1 $2" in
      'compute networks')
        case "$3" in
          describe) fixture network ;;
          list) fixture networks ;;
          delete) : ;;
          peerings) : ;;
          subnets) fixture subnets ;;
          *) exit 2 ;;
        esac ;;
      'compute instance-groups') fixture groups ;;
      'compute '*)
        if [[ "$3" == list ]]; then
          if [[ -f "$CLEANUP_FIXTURES/deleted-$2" ]]; then echo '[]'; else fixture "$2"; fi
        elif [[ "$3" == delete ]]; then
          [[ "${FAIL_FAMILY:-}" != "$2" ]] || exit 1
          touch "$CLEANUP_FIXTURES/deleted-$2"
        else exit 2; fi ;;
      'container clusters')
        case "$3" in
          list) fixture clusters ;;
          delete) echo '[]' > "$CLEANUP_FIXTURES/clusters.json" ;;
          get-credentials) echo fake-credentials > "$KUBECONFIG" ;;
          *) exit 2 ;;
        esac ;;
      'iam service-accounts')
        case "$3" in list) fixture accounts ;; describe) fixture account ;; delete) : ;; *) exit 2 ;; esac ;;
      'storage cat')
        if [[ -f "$CLEANUP_FIXTURES/objects.json" ]] && jq -e --arg uri "$3" 'has($uri)' "$CLEANUP_FIXTURES/objects.json" >/dev/null; then
          jq -r --arg uri "$3" '.[$uri]' "$CLEANUP_FIXTURES/objects.json"
        elif [[ "$3" == *compute-cleanup.json && -f "$CLEANUP_FIXTURES/plan.json" ]]; then fixture plan
        else echo NOT_FOUND >&2; exit 1; fi ;;
      'storage cp')
        if [[ "$4" == *compute-cleanup.json ]]; then cp "$3" "$CLEANUP_FIXTURES/plan.json"; fi ;;
      'storage rm') [[ "${FAIL_STORAGE_RM:-false}" != true ]] ;;
      'storage buckets') if [[ "$3" == list ]]; then fixture buckets; fi ;;
      'storage ls')
        if [[ "$4" == *ci-cleanup.json ]]; then fixture owners; else fixture saved; fi ;;
      *) exit 2 ;;
    esac ;;
  *) exit 2 ;;
esac
`

type cleanupHarness struct {
	t       *testing.T
	dir     string
	scripts string
}

func newCleanupHarness(t *testing.T) *cleanupHarness {
	t.Helper()
	_, err := exec.LookPath("jq")
	require.NoError(t, err, "cleanup tests require jq")
	_, file, _, _ := runtime.Caller(0)
	h := &cleanupHarness{t: t, dir: t.TempDir(), scripts: filepath.Dir(file)}
	t.Setenv("CLEANUP_FIXTURES", h.dir)
	t.Setenv("PATH", h.dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("GCP_PROJECT_ID", "test-project")
	t.Setenv("GCP_CLEANUP_STATE_URI", "gs://test-bucket/cleanup")
	t.Setenv("GITHUB_REPOSITORY", "owner/repo")
	t.Setenv("AZURE_SUBSCRIPTION_ID", "test-subscription")
	t.Setenv("TMPDIR", h.dir)
	t.Setenv("AZURE_DEBUG_DIR", filepath.Join(h.dir, "azure-debug"))
	t.Setenv("OPENSHIFT_DEBUG_DIR", filepath.Join(h.dir, "openshift-debug"))
	t.Setenv("OPENSHIFT_DEBUG_GCS_URI", "")
	for _, name := range []string{"CLEANUP_DRY_RUN", "FAIL_API", "FAIL_DELETE", "FAIL_LIST", "FAIL_FAMILY", "FAIL_INSTALL", "FAIL_STORAGE_RM", "GCP_CLEANUP_OWNER_RUN_ID", "GCP_CLEANUP_EXPECTED_CLUSTERS_JSON", "GCP_CLEANUP_REUSE_CONTEXTS", "OPENSHIFT_CLEANUP_GCS_URI", "OPENSHIFT_INSTALL_DIRS", "OPENSHIFT_INSTALL_DIRS_ROOT", "GITHUB_EVENT_NAME", "CLEANUP_PROTECTED_RESOURCES"} {
		t.Setenv(name, "")
	}
	for _, name := range []string{"gcloud", "az", "gh", "kubectl", "openshift-install"} {
		require.NoError(t, os.WriteFile(filepath.Join(h.dir, name), []byte(cleanupFakeCLI), 0755))
	}
	h.write("commands", "")
	h.write("workflows.json", `{"workflow_runs":[]}`)
	return h
}

func (h *cleanupHarness) write(name, contents string) {
	h.t.Helper()
	require.NoError(h.t, os.WriteFile(filepath.Join(h.dir, name), []byte(contents), 0600))
}

func (h *cleanupHarness) run(script string, args ...string) (string, error) {
	h.t.Helper()
	out, err := exec.Command("bash", append([]string{filepath.Join(h.scripts, script)}, args...)...).CombinedOutput()
	return string(out), err
}

func (h *cleanupHarness) commands() string {
	h.t.Helper()
	out, err := os.ReadFile(filepath.Join(h.dir, "commands"))
	require.NoError(h.t, err)
	return string(out)
}

func TestCloudSweepAzureEligibility(t *testing.T) {
	for _, tc := range []struct {
		name, owner, repo, runStatus string
		age                          time.Duration
		dryRun, failAPI, failDelete  bool
		wantError, eligible          bool
	}{
		{name: "dry run", dryRun: true, eligible: true},
		{name: "apply", eligible: true},
		{name: "active run", runStatus: "in_progress"},
		{name: "young resource", age: time.Hour},
		{name: "other owner", owner: "someone-else"},
		{name: "other repository", repo: "other/repo"},
		{name: "API failure", failAPI: true, wantError: true},
		{name: "delete failure", failDelete: true, eligible: true, wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newCleanupHarness(t)
			owner, repo, runStatus, age := tc.owner, tc.repo, tc.runStatus, tc.age
			if owner == "" {
				owner = "helm-charts-e2e"
			}
			if repo == "" {
				repo = "owner/repo"
			}
			if runStatus == "" {
				runStatus = "completed"
			}
			if age == 0 {
				age = 24 * time.Hour
			}
			groups := []any{map[string]any{"name": "hc-e2e-rg-test", "tags": map[string]any{
				"ManagedBy": owner, "GitHubRepository": repo, "GitHubRunID": "123", "CreatedAtUnix": time.Now().Add(-age).Unix(),
			}}}
			data, err := json.Marshal(groups)
			require.NoError(t, err)
			h.write("azure.json", string(data))
			h.write("run-123.json", `{"status":"`+runStatus+`","repository":{"full_name":"owner/repo"}}`)
			if tc.failAPI {
				t.Setenv("FAIL_API", "true")
			}
			if tc.failDelete {
				t.Setenv("FAIL_DELETE", "true")
			}
			args := []string{"azure"}
			if tc.dryRun {
				args = append(args, "--dry-run")
			}
			out, err := h.run("sweep.sh", args...)
			if tc.wantError {
				require.Error(t, err, out)
			} else {
				require.NoError(t, err, out)
			}
			if tc.eligible {
				require.Contains(t, out, "hc-e2e-rg-test")
			}
			if tc.eligible && !tc.dryRun {
				require.Contains(t, h.commands(), "az group delete")
				if !tc.failDelete {
					require.Contains(t, h.commands(), "az group wait --deleted")
				}
			} else {
				require.NotContains(t, h.commands(), "az group delete")
			}
		})
	}
}

func (h *cleanupHarness) gcpGraph() {
	h.write("network.json", `{"name":"cockroachdb-vpc-test","id":"42","selfLink":"vpc","subnetworks":["subnet"]}`)
	h.write("clusters.json", `[{"name":"test-cluster","id":"100","location":"test-region","network":"vpc-path/cockroachdb-vpc-test","resourceLabels":{"managed-by":"helm-charts-e2e","github-run-id":"123","created-at":"1"}}]`)
	h.write("groups.json", `[{"name":"ig","selfLink":"group","network":"vpc","zone":"zones/test-zone"}]`)
	h.write("backend-services.json", `[{"name":"backend","selfLink":"backend","region":"regions/test-region","backends":[{"group":"group"}],"healthChecks":["hc"]}]`)
	h.write("forwarding-rules.json", `[{"name":"rule","region":"regions/test-region","backendService":"backend"}]`)
	h.write("health-checks.json", `[{"name":"check","selfLink":"hc","id":"7"},{"name":"unrelated","selfLink":"other","id":"8"}]`)
	h.write("services.json", `{"items":[{"metadata":{"name":"dns","namespace":"system"},"spec":{"type":"LoadBalancer"}},{"metadata":{"name":"kubernetes"},"spec":{"type":"ClusterIP"}}]}`)
}

func TestCloudGCPDependencyOrderAndRetry(t *testing.T) {
	h := newCleanupHarness(t)
	h.gcpGraph()
	t.Setenv("CLEANUP_DRY_RUN", "false")
	t.Setenv("GCP_CLEANUP_OWNER_RUN_ID", "123")
	t.Setenv("KUBECONFIG", filepath.Join(h.dir, "kubeconfig"))
	t.Setenv("FAIL_FAMILY", "health-checks")
	out, err := h.run("gcp.sh", "network", "cockroachdb-vpc-test", "42")
	require.Error(t, err, out)
	require.FileExists(t, filepath.Join(h.dir, "plan.json"))
	commands := h.commands()
	order := []string{"storage cp", "delete service dns", "container clusters delete", "forwarding-rules delete", "backend-services delete", "health-checks delete"}
	previous := -1
	for _, step := range order {
		index := strings.Index(commands, step)
		require.Greater(t, index, previous, "%s: %s", step, commands)
		previous = index
	}
	require.Contains(t, commands, "--wait=true --timeout=5m")
	for _, resource := range []string{"loadbalancer-service\tsystem/dns", "health-checks\tcheck"} {
		index := strings.Index(out, resource)
		require.GreaterOrEqual(t, index, 0, out)
		require.Less(t, index, strings.Index(out, "RUN gcloud storage cp"))
	}
	require.NotContains(t, commands, "delete service kubernetes")
	require.NotContains(t, commands, "networks delete")
	h.write("commands", "")
	t.Setenv("FAIL_FAMILY", "")
	out, err = h.run("gcp.sh", "network", "cockroachdb-vpc-test", "42")
	require.NoError(t, err, out)
	require.Contains(t, h.commands(), "health-checks delete check")
	require.NotContains(t, h.commands(), "delete unrelated")
	require.Contains(t, h.commands(), "networks delete cockroachdb-vpc-test")
}

func TestCloudGCPIdentityGuardsAndDryRun(t *testing.T) {
	for _, tc := range []struct {
		name, id, run string
		wantError     bool
	}{
		{"dry run", "42", "123", false},
		{"replaced network", "99", "123", true},
		{"other run cluster", "42", "456", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newCleanupHarness(t)
			h.gcpGraph()
			t.Setenv("GCP_CLEANUP_OWNER_RUN_ID", tc.run)
			out, err := h.run("gcp.sh", "network", "cockroachdb-vpc-test", tc.id, "--dry-run")
			if tc.wantError {
				require.Error(t, err, out)
			} else {
				require.NoError(t, err, out)
				require.Contains(t, out, "DRY RUN")
			}
			require.NotContains(t, h.commands(), " delete ")
			require.NotContains(t, h.commands(), "storage cp")
			require.NotContains(t, h.commands(), "kubectl --context=gke_test-project_test-region_test-cluster --request-timeout=30s delete")
		})
	}
}

func TestCloudOpenShiftCleanupReportsFailure(t *testing.T) {
	h := newCleanupHarness(t)
	root := t.TempDir()
	dir := filepath.Join(root, "ocp-test-abc")
	require.NoError(t, os.Mkdir(dir, 0700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "metadata.json"), []byte(`{"infraID":"ocp-test-abc"}`), 0600))
	t.Setenv("OPENSHIFT_INSTALL_DIRS_ROOT", root)
	out, err := h.run("openshift.sh", "recorded-installs", "--dry-run")
	require.NoError(t, err, out)
	require.DirExists(t, dir)
	require.NotContains(t, h.commands(), "openshift-install destroy")
	t.Setenv("CLEANUP_DRY_RUN", "false")
	t.Setenv("FAIL_INSTALL", "true")
	out, err = h.run("openshift.sh", "recorded-installs")
	require.Error(t, err, out)
	require.DirExists(t, dir)
	t.Setenv("FAIL_INSTALL", "")
	out, err = h.run("openshift.sh", "recorded-installs")
	require.NoError(t, err, out)
	require.NoDirExists(t, dir)
}

func TestCloudOpenShiftRetiresProvisioningState(t *testing.T) {
	for _, tc := range []struct {
		name                           string
		dryRun, failInstall, failState bool
	}{
		{name: "successful teardown"},
		{name: "dry run", dryRun: true},
		{name: "failed teardown", failInstall: true},
		{name: "failed state removal", failState: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newCleanupHarness(t)
			dir := filepath.Join(h.dir, "install")
			require.NoError(t, os.Mkdir(dir, 0700))
			require.NoError(t, os.WriteFile(filepath.Join(dir, "metadata.json"), []byte(`{"infraID":"ocp-test-abc"}`), 0600))
			require.NoError(t, os.WriteFile(filepath.Join(dir, "ci-cleanup.json"), []byte(`{"infra_id":"ocp-test-abc"}`), 0600))
			t.Setenv("OPENSHIFT_CLEANUP_GCS_URI", "gs://test-bucket/cleanup")
			if tc.failInstall {
				t.Setenv("FAIL_INSTALL", "true")
			}
			if tc.failState {
				t.Setenv("FAIL_STORAGE_RM", "true")
			}
			args := []string{"install", dir}
			if tc.dryRun {
				args = append(args, "--dry-run")
			}
			out, err := h.run("openshift.sh", args...)
			if tc.failInstall || tc.failState {
				require.Error(t, err, out)
			} else {
				require.NoError(t, err, out)
			}
			if tc.dryRun || tc.failInstall || tc.failState {
				require.DirExists(t, dir)
			} else {
				require.NoDirExists(t, dir)
				for _, file := range []string{"ci-cleanup.json", "metadata.json"} {
					require.Contains(t, h.commands(), "storage rm gs://test-bucket/cleanup/ocp-test-abc/"+file)
				}
			}
			if tc.dryRun || tc.failInstall {
				require.NotContains(t, h.commands(), "storage rm")
			} else {
				require.Less(t, strings.Index(h.commands(), "openshift-install destroy"), strings.Index(h.commands(), "storage rm"))
			}
		})
	}
}

func TestCloudSweepGCPAndIsolatedKubeconfig(t *testing.T) {
	h := newCleanupHarness(t)
	h.gcpGraph()
	owner := map[string]any{"managed_by": "helm-charts-e2e", "repository": "owner/repo", "run_id": "123", "created_at": time.Now().Add(-24 * time.Hour).Unix(), "provider": "gcp", "project": "test-project"}
	metadata, err := json.Marshal(owner)
	require.NoError(t, err)
	networks, err := json.Marshal([]any{map[string]any{"name": "cockroachdb-vpc-test", "id": "42", "description": string(metadata)}})
	require.NoError(t, err)
	h.write("networks.json", string(networks))
	h.write("run-123.json", `{"status":"completed","repository":{"full_name":"owner/repo"}}`)
	kubeconfig := filepath.Join(h.dir, "user-kubeconfig")
	h.write("user-kubeconfig", "original")
	t.Setenv("KUBECONFIG", kubeconfig)
	out, err := h.run("sweep.sh", "gcp")
	require.NoError(t, err, out)
	require.Contains(t, h.commands(), "networks delete cockroachdb-vpc-test")
	data, err := os.ReadFile(kubeconfig)
	require.NoError(t, err)
	require.Equal(t, "original", string(data))
}

func TestCloudAzurePrintsResourcesBeforeDefaultDeletion(t *testing.T) {
	h := newCleanupHarness(t)
	h.write("azure-resources.json", `[{"name":"test-vm","type":"Microsoft.Compute/virtualMachines","location":"test-region","id":"vm-id"}]`)
	h.write("azure-clusters.json", `[{"nodeResourceGroup":"test-node-group"}]`)
	out, err := h.run("azure.sh", "resource-group", "test-group")
	require.NoError(t, err, out)
	deleteIndex := strings.Index(out, "RUN az group delete")
	require.GreaterOrEqual(t, deleteIndex, 0, out)
	for _, resource := range []string{"resource-group\ttest-group", "Microsoft.Compute/virtualMachines\ttest-vm", "node-resource-group\ttest-node-group"} {
		index := strings.Index(out, resource)
		require.GreaterOrEqual(t, index, 0, out)
		require.Less(t, index, deleteIndex, out)
	}
	require.Contains(t, h.commands(), "az group delete")
}

func TestCloudAzureInventoryErrors(t *testing.T) {
	for _, failure := range []string{"missing", "permission"} {
		t.Run(failure, func(t *testing.T) {
			h := newCleanupHarness(t)
			t.Setenv("FAIL_LIST", failure)
			out, err := h.run("azure.sh", "resource-group", "test-group")
			if failure == "missing" {
				require.NoError(t, err, out)
			} else {
				require.Error(t, err, out)
				require.NotContains(t, h.commands(), "az group delete")
			}
		})
	}
}

func TestCloudOpenShiftPrintsBothInstallsBeforeDeleting(t *testing.T) {
	h := newCleanupHarness(t)
	root := t.TempDir()
	t.Setenv("OPENSHIFT_INSTALL_DIRS_ROOT", root)
	for _, infra := range []string{"ocp-first-abc", "ocp-second-def"} {
		dir := filepath.Join(root, infra)
		require.NoError(t, os.Mkdir(dir, 0700))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "metadata.json"), []byte(`{"infraID":"`+infra+`","gcp":{"projectID":"test-project","region":"test-region"}}`), 0600))
	}
	out, err := h.run("openshift.sh", "recorded-installs")
	require.NoError(t, err, out)
	deleteIndex := strings.Index(out, "RUN gcloud compute firewall-rules delete")
	require.GreaterOrEqual(t, deleteIndex, 0, out)
	for _, infra := range []string{"ocp-first-abc", "ocp-second-def"} {
		index := strings.Index(out, "openshift-install\t"+infra)
		require.GreaterOrEqual(t, index, 0, out)
		require.Less(t, index, deleteIndex, out)
	}
	require.Contains(t, h.commands(), "peerings delete ocp-first-to-ocp-second --network=ocp-first-abc-network")
	require.Contains(t, h.commands(), "peerings delete ocp-second-to-ocp-first --network=ocp-second-def-network")
}

func TestCloudDebugScriptsHaveNoCleanupAction(t *testing.T) {
	h := newCleanupHarness(t)
	for _, provider := range []string{"azure", "openshift"} {
		out, err := h.run("../debug/"+provider+".sh", "snapshot", "test")
		require.NoError(t, err, out)
		_, err = h.run("../debug/"+provider+".sh", "cleanup")
		require.Error(t, err)
	}
	require.NotContains(t, h.commands(), " delete ")
	require.NotContains(t, h.commands(), "openshift-install destroy")
}

func TestCloudShellSyntax(t *testing.T) {
	_, file, _, _ := runtime.Caller(0)
	for _, script := range []string{"common.sh", "sweep.sh", "discover-gcp.sh", "gcp.sh", "openshift.sh", "azure.sh", "../debug/azure.sh", "../debug/openshift.sh", "../lib/cloud-resources.sh"} {
		out, err := exec.Command("bash", "-n", filepath.Join(filepath.Dir(file), script)).CombinedOutput()
		require.NoError(t, err, string(out))
	}
}

func TestCloudLegacyGCPDiscovery(t *testing.T) {
	for _, tc := range []struct {
		name, workflows, protect, clusterName string
		age                                   time.Duration
		selected                              bool
	}{
		{name: "old unmarked resources", selected: true},
		{name: "active workflow", workflows: `{"workflow_runs":[{"id":999}]}`},
		{name: "young network", age: 12 * time.Hour},
		{name: "protected network", protect: "cockroachdb-vpc-abc123"},
		{name: "protected cluster", protect: "gcp-chart-testing-cluster-0-abc123"},
		{name: "unrecognized cluster", clusterName: "shared-cluster"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newCleanupHarness(t)
			age := tc.age
			if age == 0 {
				age = 48 * time.Hour
			}
			clusterName := tc.clusterName
			if clusterName == "" {
				clusterName = "gcp-chart-testing-cluster-0-abc123"
			}
			network := map[string]any{"name": "cockroachdb-vpc-abc123", "id": "42", "selfLink": "vpc", "creationTimestamp": time.Now().Add(-age).Format(time.RFC3339Nano)}
			data, err := json.Marshal(network)
			require.NoError(t, err)
			h.write("network.json", string(data))
			h.write("networks.json", "["+string(data)+"]")
			data, err = json.Marshal([]any{map[string]any{"name": clusterName, "id": "100", "location": "test-region", "network": "cockroachdb-vpc-abc123", "createTime": time.Now().Add(-48 * time.Hour).Format(time.RFC3339Nano)}})
			require.NoError(t, err)
			h.write("clusters.json", string(data))
			h.write("services.json", `{"items":[]}`)
			t.Setenv("CLEANUP_PROTECTED_RESOURCES", tc.protect)
			if tc.workflows != "" {
				h.write("workflows.json", tc.workflows)
			}
			out, err := h.run("sweep.sh", "gcp")
			require.NoError(t, err, out)
			if tc.selected {
				require.Contains(t, h.commands(), "container clusters delete "+clusterName)
				require.Contains(t, h.commands(), "networks delete cockroachdb-vpc-abc123")
			} else {
				require.NotContains(t, h.commands(), " delete ")
			}
		})
	}
}

func TestCloudLegacyAzureUsesCompletedRunAge(t *testing.T) {
	for _, tc := range []struct {
		name, status string
		age          time.Duration
		selected     bool
	}{
		{"old completed run", "completed", 48 * time.Hour, true},
		{"recently completed", "completed", time.Hour, false},
		{"still running", "in_progress", 48 * time.Hour, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newCleanupHarness(t)
			h.write("azure.json", `[{"name":"hc-e2e-rg-test","tags":{"ManagedBy":"helm-charts-e2e","GitHubRepository":"owner/repo","GitHubRunID":"123"}}]`)
			h.write("run-123.json", `{"status":"`+tc.status+`","repository":{"full_name":"owner/repo"},"updated_at":"`+time.Now().Add(-tc.age).Format(time.RFC3339)+`"}`)
			out, err := h.run("sweep.sh", "azure")
			require.NoError(t, err, out)
			require.Equal(t, tc.selected, strings.Contains(h.commands(), "az group delete"), out)
		})
	}
}

func (h *cleanupHarness) objects(objects map[string]string) {
	h.t.Helper()
	data, err := json.Marshal(objects)
	require.NoError(h.t, err)
	h.write("objects.json", string(data))
}

func TestCloudLegacyOpenShiftDiscovery(t *testing.T) {
	for _, tc := range []struct {
		name, protect     string
		dryRun, recovered bool
	}{
		{name: "apply saved metadata"},
		{name: "dry run", dryRun: true},
		{name: "protected install", protect: "ocp-0123456789-abc12"},
		{name: "retry saved recovered metadata", recovered: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newCleanupHarness(t)
			uri := "gs://test-bucket/123/1/single-region/helm-charts-ocp-ocp-0123456789-metadata.json"
			if tc.recovered {
				uri = "gs://test-bucket/cleanup/recovered-installs/ocp-0123456789-abc12/metadata.json"
			}
			h.write("accounts.json", `[{"displayName":"ocp-0123456789-abc12-worker-node","email":"test@test-project.iam.gserviceaccount.com","uniqueId":"99"}]`)
			h.write("saved.json", `[{"url":"`+uri+`#456","metadata":{"timeCreated":"2025-01-01T01:00:00.123456+00:00"}}]`)
			h.write("run-123.json", `{"status":"completed","repository":{"full_name":"owner/repo"}}`)
			h.objects(map[string]string{uri: `{"infraID":"ocp-0123456789-abc12","gcp":{"projectID":"test-project","region":"us-central1"}}`})
			t.Setenv("CLEANUP_PROTECTED_RESOURCES", tc.protect)
			args := []string{"gcp"}
			if tc.dryRun {
				args = append(args, "--dry-run")
			}
			out, err := h.run("sweep.sh", args...)
			require.NoError(t, err, out)
			if !tc.dryRun && tc.protect == "" {
				require.Contains(t, h.commands(), "openshift-install destroy cluster")
				require.Less(t, strings.Index(h.commands(), "storage cp"), strings.Index(h.commands(), "openshift-install destroy cluster"))
			} else {
				require.NotContains(t, h.commands(), "openshift-install destroy cluster")
			}
		})
	}
}

func TestCloudOrphanAccountObservation(t *testing.T) {
	for _, tc := range []struct {
		name, observation string
		dryRun, attached  bool
		selected          bool
	}{
		{name: "first discovery"},
		{name: "first discovery dry run", dryRun: true},
		{name: "known old orphan", observation: `{"id":"99","infra":"ocp-0123456789-abc12","created_at":1}`, selected: true},
		{name: "attached account", observation: `{"id":"99","infra":"ocp-0123456789-abc12","created_at":1}`, attached: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newCleanupHarness(t)
			const account = `{"displayName":"ocp-0123456789-abc12-worker-node","email":"test@test-project.iam.gserviceaccount.com","uniqueId":"99"}`
			h.write("accounts.json", "["+account+"]")
			h.write("account.json", account)
			if tc.observation != "" {
				h.objects(map[string]string{"gs://test-bucket/cleanup/orphan-accounts/99.json": tc.observation})
			}
			if tc.attached {
				h.write("instances.json", `[{"name":"unrelated-vm","serviceAccounts":[{"email":"test@test-project.iam.gserviceaccount.com"}]}]`)
			}
			args := []string{"gcp"}
			if tc.dryRun {
				args = append(args, "--dry-run")
			}
			out, err := h.run("sweep.sh", args...)
			if tc.attached {
				require.Error(t, err, out)
			} else {
				require.NoError(t, err, out)
			}
			require.Equal(t, tc.selected, strings.Contains(h.commands(), "iam service-accounts delete"), out)
			if tc.dryRun {
				require.NotContains(t, h.commands(), "storage cp")
			}
		})
	}
}

func TestCloudLeftoversPreserveLiveDependencies(t *testing.T) {
	h := newCleanupHarness(t)
	h.write("clusters.json", `[{"name":"gcp-chart-testing-cluster-0-live12"}]`)
	h.write("disks.json", `[
		{"name":"orphan","id":"1","zone":"zones/us-central1-a","creationTimestamp":"2025-01-01T00:00:00Z","labels":{"goog-k8s-cluster-name":"gcp-chart-testing-cluster-0-old123"}},
		{"name":"attached","creationTimestamp":"2025-01-01T00:00:00Z","users":["vm"],"labels":{"goog-k8s-cluster-name":"gcp-chart-testing-cluster-0-old123"}},
		{"name":"live-cluster","creationTimestamp":"2025-01-01T00:00:00Z","labels":{"goog-k8s-cluster-name":"gcp-chart-testing-cluster-0-live12"}},
		{"name":"unrelated","creationTimestamp":"2025-01-01T00:00:00Z"}]`)
	h.write("backend-services.json", `[{"name":"retained","healthChecks":["live-check"]}]`)
	h.write("health-checks.json", `[
		{"name":"k8s-orphan-node","id":"2","selfLink":"orphan-check","creationTimestamp":"2025-01-01T00:00:00Z"},
		{"name":"k8s-live-node","id":"3","selfLink":"live-check","creationTimestamp":"2025-01-01T00:00:00Z"}]`)
	out, err := h.run("gcp.sh", "leftovers", "17000000000")
	require.NoError(t, err, out)
	require.Contains(t, h.commands(), "disks delete orphan")
	require.Contains(t, h.commands(), "health-checks delete k8s-orphan-node")
	for _, resource := range []string{"disks delete attached", "disks delete live-cluster", "disks delete unrelated", "health-checks delete k8s-live-node"} {
		require.NotContains(t, h.commands(), resource)
	}
}

func TestCloudTimestampOffsetsAndMinimumAge(t *testing.T) {
	h := newCleanupHarness(t)
	cmd := exec.Command("jq", "-L", h.scripts, "-ne", `include "policy";
		["2026-11-01T23:14:36.303-08:00", "2026-11-02T07:14:36Z", "2026-11-02T07:14:36.000000+0000"] |
		map(epoch) | unique | length == 1`)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, string(out))
	sweepOut, err := h.run("sweep.sh", "azure", "--max-age-hours", "12")
	require.Error(t, err, sweepOut)
	require.NotContains(t, h.commands(), " delete ")
}
