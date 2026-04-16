package infra

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"text/template"
	"time"

	"github.com/cockroachdb/helm-charts/tests/e2e/coredns"
	"github.com/cockroachdb/helm-charts/tests/e2e/operator"
	"github.com/gruntwork-io/terratest/modules/k8s"
	"github.com/gruntwork-io/terratest/modules/retry"
	"github.com/stretchr/testify/require"
	"google.golang.org/api/compute/v1"
	"google.golang.org/api/option"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"
)

// ─── OPENSHIFT CONSTANTS ──────────────────────────────────────────────────────

const (
	// env vars consumed by openshift.go
	envOpenShiftPullSecret = "OPENSHIFT_PULL_SECRET"
	envOpenShiftSSHPubKey  = "OPENSHIFT_SSH_PUB_KEY"
	envOpenShiftBaseDomain = "OPENSHIFT_BASE_DOMAIN"
	// envOpenShiftInstallDir points to an existing openshift-install state directory.
	// When set, SetUpInfra skips provisioning and reuses the existing cluster.
	// Example: OPENSHIFT_INSTALL_DIR=/var/folders/.../ocp-ocp-0qaif4-2872850045
	envOpenShiftInstallDir = "OPENSHIFT_INSTALL_DIR"
	// envOpenShiftInstallDirs is a comma-separated list of existing openshift-install
	// state directories, one per cluster in order. When set, SetUpInfra skips
	// provisioning for all clusters and reuses the existing ones. VPC peering,
	// CoreDNS, and DNS operator setup still run (they are idempotent).
	// Example: OPENSHIFT_INSTALL_DIRS=/path/to/cluster-0,/path/to/cluster-1
	envOpenShiftInstallDirs = "OPENSHIFT_INSTALL_DIRS"

	// defaultOpenShiftProjectID is the GCP project used when GCP_PROJECT_ID is not set.
	// TODO: Change this to "helm-testing" to match the GCP provider default and avoid
	// accidental provisioning into a personal project when GCP_PROJECT_ID is unset.
	defaultOpenShiftProjectID = "cockroach-shreyaskm"

	// maxInstallerNameLen is the maximum length of the cluster name in install-config.yaml.
	// OpenShift enforces a 14-character limit on metadata.name.
	maxInstallerNameLen = 14
)

// openShiftNetworkConfig holds per-cluster network CIDR configuration.
// Two clusters must use non-overlapping ranges so VPC peering routes are unambiguous.
type openShiftNetworkConfig struct {
	MachineCIDR    string
	ClusterNetwork string
	HostPrefix     int
	ServiceNetwork string
}

// openShiftNetworkConfigs defines non-overlapping CIDRs for each cluster index.
// Index 0 → us-central1 cluster, index 1 → us-east1 cluster.
var openShiftNetworkConfigs = []openShiftNetworkConfig{
	{
		MachineCIDR:    "10.0.0.0/16",
		ClusterNetwork: "10.128.0.0/14",
		HostPrefix:     23,
		ServiceNetwork: "172.30.0.0/16",
	},
	{
		MachineCIDR:    "10.1.0.0/16",
		ClusterNetwork: "10.132.0.0/14",
		HostPrefix:     23,
		ServiceNetwork: "172.31.0.0/16",
	},
}

// installConfigTemplate is the install-config.yaml template for openshift-install.
// It includes explicit networking ranges so that two clusters in the same GCP project
// use non-overlapping pod/service CIDRs, enabling VPC peering.
var installConfigTemplate = template.Must(template.New("install-config").Parse(`apiVersion: v1
baseDomain: {{ .BaseDomain }}
metadata:
  name: {{ .ClusterName }}
compute:
  - architecture: amd64
    hyperthreading: Enabled
    name: worker
    platform: {}
    replicas: 3
controlPlane:
  architecture: amd64
  hyperthreading: Enabled
  name: master
  platform: {}
  replicas: 3
networking:
  networkType: OVNKubernetes
  machineNetwork:
    - cidr: {{ .MachineCIDR }}
  clusterNetwork:
    - cidr: {{ .ClusterNetwork }}
      hostPrefix: {{ .HostPrefix }}
  serviceNetwork:
    - {{ .ServiceNetwork }}
platform:
  gcp:
    projectID: {{ .ProjectID }}
    region: {{ .Region }}
pullSecret: '{{ .PullSecret }}'
sshKey: {{ .SSHPubKey }}
`))

type installConfigData struct {
	BaseDomain     string
	ClusterName    string
	ProjectID      string
	Region         string
	PullSecret     string
	SSHPubKey      string
	MachineCIDR    string
	ClusterNetwork string
	HostPrefix     int
	ServiceNetwork string
}

// ─── OPENSHIFT REGION ─────────────────────────────────────────────────────────

// openShiftMetadata mirrors the fields we need from the metadata.json file
// that openshift-install writes to the install directory after provisioning.
type openShiftMetadata struct {
	InfraID string `json:"infraID"`
}

// OpenShiftRegion implements CloudProvider for OpenShift clusters on GCP,
// provisioned via openshift-install.
type OpenShiftRegion struct {
	*operator.Region
	// installDirs maps cluster name → temp directory that holds install-config.yaml
	// and all state produced by openshift-install (auth/kubeconfig, terraform state, etc.)
	installDirs map[string]string
	// provisionedInstallDirs is the subset of installDirs that this test actually
	// provisioned (i.e. not provided via OPENSHIFT_INSTALL_DIR/OPENSHIFT_INSTALL_DIRS).
	// TeardownInfra only destroys and removes directories in this set, leaving
	// user-provided reuse directories untouched.
	provisionedInstallDirs map[string]bool
	// infraIDs maps cluster name → the infraID written to metadata.json by openshift-install.
	// OpenShift creates the VPC as "<infraID>-network" — this differs from the
	// metadata.name (installer name) which is the shorter user-supplied cluster name.
	infraIDs map[string]string
}

// SetUpInfra provisions one OpenShift cluster per entry in r.Clusters.
// For single-region tests there is exactly one cluster.
//
// Cluster reuse modes (skip provisioning):
//   - OPENSHIFT_INSTALL_DIRS: comma-separated list of existing openshift-install
//     state directories, one per cluster in order. VPC peering, CoreDNS, and DNS
//     operator setup still run (they are idempotent). Use this for multi-region.
//   - OPENSHIFT_INSTALL_DIR: single directory for single-region reuse.
func (r *OpenShiftRegion) SetUpInfra(t *testing.T) {
	if r.ReusingInfra {
		t.Logf("[%s] Reusing existing infrastructure, skipping setup", ProviderOpenShift)
		return
	}
	r.installDirs = make(map[string]string)
	r.provisionedInstallDirs = make(map[string]bool)
	r.infraIDs = make(map[string]string)
	r.Clients = make(map[string]client.Client)
	r.CorednsClusterOptions = make(map[string]coredns.CoreDNSClusterOption)

	reuseDirsEnv := os.Getenv(envOpenShiftInstallDirs)
	reuseDir := os.Getenv(envOpenShiftInstallDir)

	switch {
	case reuseDirsEnv != "":
		// Multi-region reuse: OPENSHIFT_INSTALL_DIRS is a comma-separated list of
		// existing install directories, one per cluster in order.
		// Provisioning is skipped; infraIDs are read from each metadata.json.
		// VPC peering, CoreDNS, and DNS operator setup still run (idempotent).
		dirs := splitTrimmed(reuseDirsEnv)
		if len(dirs) != len(r.Clusters) {
			t.Fatalf("[%s] OPENSHIFT_INSTALL_DIRS has %d entries but %d clusters expected",
				ProviderOpenShift, len(dirs), len(r.Clusters))
		}
		t.Logf("[%s] Reusing %d existing clusters from OPENSHIFT_INSTALL_DIRS", ProviderOpenShift, len(dirs))
		for i, clusterName := range r.Clusters {
			installDir := dirs[i]
			r.installDirs[clusterName] = installDir

			infraID, err := readOpenShiftInfraID(installDir)
			if err != nil {
				t.Fatalf("[%s] Failed to read infraID for cluster %q from %s: %v",
					ProviderOpenShift, clusterName, installDir, err)
			}
			r.infraIDs[clusterName] = infraID
			t.Logf("[%s] Cluster %q infraID: %q (reused)", ProviderOpenShift, clusterName, infraID)

			generatedKubeconfig := filepath.Join(installDir, "auth", "kubeconfig")
			if err := mergeOpenShiftKubeconfig(t, generatedKubeconfig, clusterName); err != nil {
				t.Fatalf("[%s] Failed to merge kubeconfig for %q: %v", ProviderOpenShift, clusterName, err)
			}
			restCfg, err := config.GetConfigWithContext(clusterName)
			if err != nil {
				t.Fatalf("[%s] Failed to get rest config for %q: %v", ProviderOpenShift, clusterName, err)
			}
			k8sClient, err := client.New(restCfg, client.Options{})
			if err != nil {
				t.Fatalf("[%s] Failed to create k8s client for %q: %v", ProviderOpenShift, clusterName, err)
			}
			r.Clients[clusterName] = k8sClient
			t.Logf("[%s] Cluster %q client ready (reused)", ProviderOpenShift, clusterName)
		}
		// Fall through to VPC peering + CoreDNS + DNS operator setup below.

	case reuseDir != "":
		// Single-region reuse: existing OPENSHIFT_INSTALL_DIR behavior.
		t.Logf("[%s] Reusing existing cluster from OPENSHIFT_INSTALL_DIR=%s", ProviderOpenShift, reuseDir)
		clusterName := r.Clusters[0]
		r.installDirs[clusterName] = reuseDir

		generatedKubeconfig := filepath.Join(reuseDir, "auth", "kubeconfig")
		if err := mergeOpenShiftKubeconfig(t, generatedKubeconfig, clusterName); err != nil {
			t.Fatalf("[%s] Failed to merge kubeconfig for %q: %v", ProviderOpenShift, clusterName, err)
		}
		restCfg, err := config.GetConfigWithContext(clusterName)
		if err != nil {
			t.Fatalf("[%s] Failed to get rest config for %q: %v", ProviderOpenShift, clusterName, err)
		}
		k8sClient, err := client.New(restCfg, client.Options{})
		if err != nil {
			t.Fatalf("[%s] Failed to create k8s client for %q: %v", ProviderOpenShift, clusterName, err)
		}
		r.Clients[clusterName] = k8sClient
		t.Logf("[%s] Cluster %q client ready (reused)", ProviderOpenShift, clusterName)
		// Fall through to the common post-setup block below (DNS patching, autoscaler)
		// so single-cluster reuse behaves identically to fresh provisioning.

	default:
		// Fresh provisioning.
		pullSecret := mustEnv(t, envOpenShiftPullSecret)
		sshPubKey := mustEnv(t, envOpenShiftSSHPubKey)
		baseDomain := mustEnv(t, envOpenShiftBaseDomain)
		projectID := getOpenShiftProjectID()

		for i, clusterName := range r.Clusters {
			regionCode := r.RegionCodes[i]
			t.Logf("[%s] Provisioning cluster %q in region %s (project %s)", ProviderOpenShift, clusterName, regionCode, projectID)

			netCfg := openShiftNetworkConfigs[0]
			if i < len(openShiftNetworkConfigs) {
				netCfg = openShiftNetworkConfigs[i]
			}

			installDir, infraID, err := r.provisionCluster(t, clusterName, regionCode, projectID, baseDomain, pullSecret, sshPubKey, netCfg)
			if err != nil {
				t.Fatalf("[%s] Failed to provision cluster %q: %v", ProviderOpenShift, clusterName, err)
			}
			r.installDirs[clusterName] = installDir
			r.provisionedInstallDirs[clusterName] = true
			r.infraIDs[clusterName] = infraID

			generatedKubeconfig := filepath.Join(installDir, "auth", "kubeconfig")
			if err := mergeOpenShiftKubeconfig(t, generatedKubeconfig, clusterName); err != nil {
				t.Fatalf("[%s] Failed to merge kubeconfig for %q: %v", ProviderOpenShift, clusterName, err)
			}
			restCfg, err := config.GetConfigWithContext(clusterName)
			if err != nil {
				t.Fatalf("[%s] Failed to get rest config for %q: %v", ProviderOpenShift, clusterName, err)
			}
			k8sClient, err := client.New(restCfg, client.Options{})
			if err != nil {
				t.Fatalf("[%s] Failed to create k8s client for %q: %v", ProviderOpenShift, clusterName, err)
			}
			r.Clients[clusterName] = k8sClient
			t.Logf("[%s] Cluster %q is ready", ProviderOpenShift, clusterName)

			if !r.IsMultiRegion {
				break
			}
		}
	}

	// Common: VPC peering, CoreDNS deployment, and DNS operator patching.
	kubeConfigPath, err := r.EnsureKubeConfigPath()
	require.NoError(t, err)

	if r.IsMultiRegion {
		if err := r.setupOpenShiftVPCPeering(t, getOpenShiftProjectID()); err != nil {
			t.Fatalf("[%s] Failed to set up VPC peering: %v", ProviderOpenShift, err)
		}

		if err := r.deployAndConfigureOpenShiftCoreDNS(t, kubeConfigPath); err != nil {
			t.Fatalf("[%s] Failed to deploy CoreDNS: %v", ProviderOpenShift, err)
		}

		// Patch each cluster's DNS operator to forward ALL custom cluster domains
		// to the local custom CoreDNS ClusterIP.
		var allDomains []string
		for j := range r.Clusters {
			allDomains = append(allDomains, operator.CustomDomains[j])
		}

		for _, clusterName := range r.Clusters {
			clusterIP, err := r.getCoreDNSClusterIP(t, clusterName, kubeConfigPath)
			if err != nil {
				t.Fatalf("[%s] Failed to get CoreDNS ClusterIP for cluster %q: %v", ProviderOpenShift, clusterName, err)
			}
			if err := r.patchOpenShiftDNSAllDomains(t, clusterName, kubeConfigPath, allDomains, clusterIP); err != nil {
				t.Fatalf("[%s] Failed to patch DNS operator on cluster %q: %v", ProviderOpenShift, clusterName, err)
			}
		}

		// Set up Submariner for cross-cluster pod networking.
		// OVN-K overlay pod IPs are not routable across VPC peering without this.
		if err := r.setupSubmariner(t, kubeConfigPath); err != nil {
			t.Fatalf("[%s] Failed to set up Submariner: %v", ProviderOpenShift, err)
		}

		// Enable cluster autoscaler so TestClusterScaleUp can schedule the 4th
		// CockroachDB pod on a new node (mirrors GKE's --enable-autoscaling).
		if err := r.setupClusterAutoscaler(t, kubeConfigPath); err != nil {
			t.Fatalf("[%s] Failed to set up cluster autoscaler: %v", ProviderOpenShift, err)
		}
	} else {
		// Single-region: the charts use the cluster's default domain (cluster.local),
		// which the OpenShift DNS operator already handles natively. Forwarding
		// cluster1.local → dns-default would create a self-referential loop, so we
		// skip DNS patching entirely here.

		// Enable cluster autoscaler so TestClusterScaleUp can schedule the 4th
		// CockroachDB pod on a new node (mirrors GKE's --enable-autoscaling).
		if err := r.setupClusterAutoscaler(t, kubeConfigPath); err != nil {
			t.Fatalf("[%s] Failed to set up cluster autoscaler: %v", ProviderOpenShift, err)
		}
	}

	r.ReusingInfra = true
	t.Logf("[%s] Infrastructure setup complete", ProviderOpenShift)
}

// TeardownInfra destroys all provisioned OpenShift clusters.
// openshift-install destroy cluster handles all GCP resource cleanup automatically.
// Clusters that were reused via OPENSHIFT_INSTALL_DIR/OPENSHIFT_INSTALL_DIRS are
// left untouched — only clusters this test provisioned itself are destroyed.
func (r *OpenShiftRegion) TeardownInfra(t *testing.T) {
	t.Logf("[%s] Starting infrastructure teardown", ProviderOpenShift)

	for clusterName, installDir := range r.installDirs {
		if !r.provisionedInstallDirs[clusterName] {
			t.Logf("[%s] Skipping teardown for reused cluster %q (not provisioned by this test)", ProviderOpenShift, clusterName)
			continue
		}

		t.Logf("[%s] Destroying cluster %q", ProviderOpenShift, clusterName)

		cmd := exec.Command("openshift-install", "destroy", "cluster",
			"--dir", installDir,
			"--log-level", "info",
		)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr

		if err := cmd.Run(); err != nil {
			// Log but don't fatal — best-effort cleanup.
			t.Logf("[%s] Warning: destroy cluster %q failed: %v", ProviderOpenShift, clusterName, err)
		} else {
			t.Logf("[%s] Cluster %q destroyed successfully", ProviderOpenShift, clusterName)
		}

		// Remove the temp install directory regardless of destroy outcome.
		if err := os.RemoveAll(installDir); err != nil {
			t.Logf("[%s] Warning: failed to remove install dir %s: %v", ProviderOpenShift, installDir, err)
		}
	}

	t.Logf("[%s] Infrastructure teardown complete", ProviderOpenShift)
}

// ScaleNodePool is a no-op for OpenShift: the ClusterAutoscaler provisioned
// in SetUpInfra handles node scaling transparently when pods are Pending.
func (r *OpenShiftRegion) ScaleNodePool(t *testing.T, location string, nodeCount, index int) {
	t.Logf("[%s] Node pool scaling delegated to ClusterAutoscaler (no explicit action needed)", ProviderOpenShift)
}

// CanScale reports whether this provider supports scaling.
// OpenShift uses the ClusterAutoscaler + MachineAutoscaler set up in SetUpInfra
// to provision new nodes automatically when pods are Pending.
func (r *OpenShiftRegion) CanScale() bool {
	return true
}

// ─── CLUSTER PROVISIONING ────────────────────────────────────────────────────

// shortInstallerName derives a ≤14-character name for install-config.yaml metadata.name.
// OpenShift enforces this limit; our internal cluster names are much longer.
// The last component of the cluster name is the random UniqueId (e.g. "ab12cd"),
// so we prefix it with "ocp-" to get a unique, short, valid name.
func shortInstallerName(clusterName string) string {
	parts := strings.Split(clusterName, "-")
	suffix := parts[len(parts)-1]
	name := "ocp-" + suffix
	if len(name) > maxInstallerNameLen {
		name = name[:maxInstallerNameLen]
	}
	return strings.TrimRight(name, "-")
}

// provisionCluster writes an install-config.yaml and runs openshift-install create cluster.
// Returns the install directory path and the infraID (read from metadata.json) on success.
// The infraID is used to construct GCP resource names such as "<infraID>-network".
func (r *OpenShiftRegion) provisionCluster(
	t *testing.T,
	clusterName, region, projectID, baseDomain, pullSecret, sshPubKey string,
	netCfg openShiftNetworkConfig,
) (string, string, error) {
	// OpenShift metadata.name must be ≤14 chars; derive a short name from the
	// random suffix of the full cluster name.
	installerName := shortInstallerName(clusterName)
	t.Logf("[%s] Using installer name %q for cluster %q", ProviderOpenShift, installerName, clusterName)
	// Create a temp directory to hold all installer state.
	installDir, err := os.MkdirTemp("", fmt.Sprintf("ocp-%s-*", installerName))
	if err != nil {
		return "", "", fmt.Errorf("failed to create install dir: %w", err)
	}

	// Write install-config.yaml.
	configPath := filepath.Join(installDir, "install-config.yaml")
	f, err := os.Create(configPath)
	if err != nil {
		_ = os.RemoveAll(installDir)
		return "", "", fmt.Errorf("failed to create install-config.yaml: %w", err)
	}

	data := installConfigData{
		BaseDomain:     baseDomain,
		ClusterName:    installerName,
		ProjectID:      projectID,
		Region:         region,
		PullSecret:     pullSecret,
		SSHPubKey:      sshPubKey,
		MachineCIDR:    netCfg.MachineCIDR,
		ClusterNetwork: netCfg.ClusterNetwork,
		HostPrefix:     netCfg.HostPrefix,
		ServiceNetwork: netCfg.ServiceNetwork,
	}
	if err := installConfigTemplate.Execute(f, data); err != nil {
		f.Close()
		_ = os.RemoveAll(installDir)
		return "", "", fmt.Errorf("failed to render install-config.yaml: %w", err)
	}
	f.Close()

	// Use debug log level to capture full bootstrap diagnostics in the test
	// output (bootstrap serial console, ignition status, container pull progress).
	logLevel := "debug"
	if os.Getenv("OPENSHIFT_INSTALL_LOG_LEVEL") != "" {
		logLevel = os.Getenv("OPENSHIFT_INSTALL_LOG_LEVEL")
	}
	t.Logf("[%s] Running: openshift-install create cluster --dir=%s --log-level=%s", ProviderOpenShift, installDir, logLevel)

	cmd := exec.Command("openshift-install", "create", "cluster",
		"--dir", installDir,
		"--log-level", logLevel,
	)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = installerEnv()

	if err := cmd.Run(); err != nil {
		// Attempt cleanup before returning the error.
		t.Logf("[%s] openshift-install failed, attempting destroy cluster for cleanup", ProviderOpenShift)
		destroyCmd := exec.Command("openshift-install", "destroy", "cluster",
			"--dir", installDir, "--log-level", "warn")
		destroyCmd.Stdout = os.Stdout
		destroyCmd.Stderr = os.Stderr
		destroyCmd.Env = installerEnv()
		_ = destroyCmd.Run()
		// Preserve the install directory (which contains the log bundle and
		// .openshift_install.log) when SKIP_CLEANUP is set so the operator can
		// inspect bootstrap diagnostics after a failure.
		if os.Getenv("SKIP_CLEANUP") == "true" || os.Getenv("PRESERVE_INFRA_ON_FAILURE") == "true" {
			t.Logf("[%s] Install dir preserved for debugging: %s", ProviderOpenShift, installDir)
			t.Logf("[%s] Check for log bundle: %s/log-bundle-*.tar.gz", ProviderOpenShift, installDir)
		} else {
			_ = os.RemoveAll(installDir)
		}
		return "", "", fmt.Errorf("openshift-install create cluster failed: %w", err)
	}

	// Read the infraID from metadata.json that openshift-install writes.
	// This is the authoritative ID used by OpenShift to name GCP resources
	// (e.g., the VPC is "<infraID>-network", not "<installerName>-network").
	infraID, err := readOpenShiftInfraID(installDir)
	if err != nil {
		// Non-fatal: fall back to the installer name so VPC peering can still
		// be attempted (it may fail gracefully if the name is wrong).
		t.Logf("[%s] Warning: could not read infraID from metadata.json, using installer name %q: %v",
			ProviderOpenShift, installerName, err)
		infraID = installerName
	}
	t.Logf("[%s] Cluster %q infraID: %q", ProviderOpenShift, clusterName, infraID)

	return installDir, infraID, nil
}

// readOpenShiftInfraID reads the infraID field from the metadata.json file
// that openshift-install writes to the install directory after provisioning.
func readOpenShiftInfraID(installDir string) (string, error) {
	metadataPath := filepath.Join(installDir, "metadata.json")
	data, err := os.ReadFile(metadataPath)
	if err != nil {
		return "", fmt.Errorf("failed to read metadata.json: %w", err)
	}
	var meta openShiftMetadata
	if err := json.Unmarshal(data, &meta); err != nil {
		return "", fmt.Errorf("failed to parse metadata.json: %w", err)
	}
	if meta.InfraID == "" {
		return "", fmt.Errorf("infraID is empty in metadata.json")
	}
	return meta.InfraID, nil
}

// ─── VPC PEERING ─────────────────────────────────────────────────────────────

// setupOpenShiftVPCPeering creates bidirectional GCP VPC peering between the
// two OpenShift cluster VPCs so that pods can reach each other cross-cluster.
// OpenShift names each cluster's VPC "<infraID>-network" where infraID is read
// from metadata.json after provisioning.
func (r *OpenShiftRegion) setupOpenShiftVPCPeering(t *testing.T, projectID string) error {
	if len(r.Clusters) < 2 {
		return nil
	}

	infraID0 := r.infraIDs[r.Clusters[0]]
	infraID1 := r.infraIDs[r.Clusters[1]]
	vpc0Name := infraID0 + "-network"
	vpc1Name := infraID1 + "-network"

	t.Logf("[%s] Setting up VPC peering: %q ↔ %q", ProviderOpenShift, vpc0Name, vpc1Name)

	ctx := context.Background()
	computeService, err := createOpenShiftComputeClient(ctx)
	if err != nil {
		return fmt.Errorf("failed to create compute client: %w", err)
	}

	vpc0SelfLink := fmt.Sprintf("https://www.googleapis.com/compute/v1/projects/%s/global/networks/%s", projectID, vpc0Name)
	vpc1SelfLink := fmt.Sprintf("https://www.googleapis.com/compute/v1/projects/%s/global/networks/%s", projectID, vpc1Name)

	// Peer vpc0 → vpc1.
	peerName01 := fmt.Sprintf("peer-%s-to-%s", infraID0, infraID1)
	if len(peerName01) > 63 {
		peerName01 = peerName01[:63]
	}
	peering01 := &compute.NetworkPeering{
		Name:                 peerName01,
		Network:              vpc1SelfLink,
		ExchangeSubnetRoutes: true,
		ImportCustomRoutes:   true,
		ExportCustomRoutes:   true,
	}
	op01, err := computeService.Networks.AddPeering(projectID, vpc0Name, &compute.NetworksAddPeeringRequest{
		NetworkPeering: peering01,
	}).Context(ctx).Do()
	if err != nil && IsResourceConflict(err) {
		// Peering already exists — update it to ensure custom routes are enabled.
		op01, err = computeService.Networks.UpdatePeering(projectID, vpc0Name, &compute.NetworksUpdatePeeringRequest{
			NetworkPeering: peering01,
		}).Context(ctx).Do()
	}
	if err != nil {
		return fmt.Errorf("failed to create/update peering %s→%s: %w", vpc0Name, vpc1Name, err)
	}
	if op01 != nil {
		waitForGlobalComputeOperation(ctx, computeService, projectID, op01.Name)
	}
	t.Logf("[%s] Peering %s→%s created/updated (ImportCustomRoutes=true, ExportCustomRoutes=true)", ProviderOpenShift, vpc0Name, vpc1Name)

	// Peer vpc1 → vpc0.
	peerName10 := fmt.Sprintf("peer-%s-to-%s", infraID1, infraID0)
	if len(peerName10) > 63 {
		peerName10 = peerName10[:63]
	}
	peering10 := &compute.NetworkPeering{
		Name:                 peerName10,
		Network:              vpc0SelfLink,
		ExchangeSubnetRoutes: true,
		ImportCustomRoutes:   true,
		ExportCustomRoutes:   true,
	}
	op10, err := computeService.Networks.AddPeering(projectID, vpc1Name, &compute.NetworksAddPeeringRequest{
		NetworkPeering: peering10,
	}).Context(ctx).Do()
	if err != nil && IsResourceConflict(err) {
		// Peering already exists — update it to ensure custom routes are enabled.
		op10, err = computeService.Networks.UpdatePeering(projectID, vpc1Name, &compute.NetworksUpdatePeeringRequest{
			NetworkPeering: peering10,
		}).Context(ctx).Do()
	}
	if err != nil {
		return fmt.Errorf("failed to create/update peering %s→%s: %w", vpc1Name, vpc0Name, err)
	}
	if op10 != nil {
		waitForGlobalComputeOperation(ctx, computeService, projectID, op10.Name)
	}
	t.Logf("[%s] Peering %s→%s created/updated (ImportCustomRoutes=true, ExportCustomRoutes=true)", ProviderOpenShift, vpc1Name, vpc0Name)

	// Add firewall rules on each VPC to allow traffic from the other cluster's
	// machine and pod CIDRs on all TCP/UDP ports (CockroachDB needs 26257).
	for i, clusterName := range r.Clusters {
		if i >= len(openShiftNetworkConfigs) {
			break
		}
		// The "other" cluster index and its network config.
		other := 1 - i
		if other >= len(openShiftNetworkConfigs) {
			break
		}
		otherNet := openShiftNetworkConfigs[other]
		infraID := r.infraIDs[clusterName]
		vpcName := infraID + "-network"
		vpcSelfLink := fmt.Sprintf("https://www.googleapis.com/compute/v1/projects/%s/global/networks/%s", projectID, vpcName)

		ruleName := fmt.Sprintf("%s-allow-peer", infraID)
		if len(ruleName) > 63 {
			ruleName = ruleName[:63]
		}

		// Source ranges: other cluster's machine CIDR + pod CIDR.
		sources := []string{otherNet.MachineCIDR, otherNet.ClusterNetwork}
		fw := &compute.Firewall{
			Name:    ruleName,
			Network: vpcSelfLink,
			Allowed: []*compute.FirewallAllowed{
				{IPProtocol: "tcp"},
				{IPProtocol: "udp"},
				{IPProtocol: "icmp"},
				// ESP (IP protocol 50) is required for Submariner's IPsec data plane tunnel.
				// GCP drops ESP by default; without this, cross-cluster pod networking fails.
				{IPProtocol: "esp"},
			},
			SourceRanges: sources,
			Direction:    "INGRESS",
		}
		_, fwErr := computeService.Firewalls.Insert(projectID, fw).Context(ctx).Do()
		if fwErr != nil && IsResourceConflict(fwErr) {
			// Rule already exists — update it to ensure ESP is included.
			_, fwErr = computeService.Firewalls.Update(projectID, ruleName, fw).Context(ctx).Do()
		}
		if fwErr != nil {
			t.Logf("[%s] Warning: failed to add/update cross-cluster firewall rule on %s: %v", ProviderOpenShift, vpcName, fwErr)
		} else {
			t.Logf("[%s] Cross-cluster firewall rule added/updated on %s (sources: %s, ESP enabled)", ProviderOpenShift, vpcName, strings.Join(sources, ", "))
		}
	}

	t.Logf("[%s] VPC peering setup complete", ProviderOpenShift)
	return nil
}

// ─── SUBMARINER ──────────────────────────────────────────────────────────────

// setupSubmariner installs Submariner on both clusters to provide cross-cluster
// pod-to-pod networking. OpenShift uses OVN-Kubernetes overlay networking whose
// pod IPs are not routable across VPC-peered networks by default. Submariner
// creates IPsec tunnels between gateway nodes (which use routable machine IPs)
// and programs OVN-K routing policies for remote pod CIDRs.
//
// Prerequisites (handled by setupOpenShiftVPCPeering):
//   - VPC peering with ImportCustomRoutes/ExportCustomRoutes enabled
//   - Firewall rule allowing ESP (IP protocol 50) for IPsec data plane
//
// This function is idempotent: if the submariner-operator namespace already
// exists on cluster-0, setup is skipped.
func (r *OpenShiftRegion) setupSubmariner(t *testing.T, kubeConfigPath string) error {
	if len(r.Clusters) < 2 {
		return nil
	}

	// Verify subctl is available.
	if _, err := exec.LookPath("subctl"); err != nil {
		return fmt.Errorf("subctl not found in PATH — install from https://github.com/submariner-io/subctl/releases: %w", err)
	}

	// Idempotency: skip if Submariner is already installed on cluster-0.
	kubectlOpts0 := k8s.NewKubectlOptions(r.Clusters[0], kubeConfigPath, "")
	if _, err := k8s.RunKubectlAndGetOutputE(t, kubectlOpts0,
		"get", "namespace", "submariner-operator"); err == nil {
		t.Logf("[%s] Submariner already installed (submariner-operator namespace exists), skipping", ProviderOpenShift)
		return nil
	}

	// Label one gateway node per cluster before joining.
	// subctl join requires a gateway-labeled node to avoid interactive prompts.
	for _, clusterName := range r.Clusters {
		kubectlOpts := k8s.NewKubectlOptions(clusterName, kubeConfigPath, "")
		output, err := k8s.RunKubectlAndGetOutputE(t, kubectlOpts,
			"get", "nodes",
			"-l", "node-role.kubernetes.io/worker",
			"--no-headers",
			"-o", "custom-columns=NAME:.metadata.name")
		if err != nil {
			return fmt.Errorf("failed to list worker nodes for cluster %s: %w", clusterName, err)
		}
		nodes := strings.Fields(strings.TrimSpace(output))
		if len(nodes) == 0 {
			return fmt.Errorf("no worker nodes found for cluster %s", clusterName)
		}
		gatewayNode := nodes[0]
		t.Logf("[%s] Labeling gateway node %s on cluster %s", ProviderOpenShift, gatewayNode, clusterName)
		if err := k8s.RunKubectlE(t, kubectlOpts,
			"label", "node", gatewayNode, "submariner.io/gateway=true", "--overwrite"); err != nil {
			return fmt.Errorf("failed to label gateway node on cluster %s: %w", clusterName, err)
		}
	}

	// Use a temp directory so broker-info.subm is written to a known location.
	brokerDir, err := os.MkdirTemp("", "submariner-broker-*")
	if err != nil {
		return fmt.Errorf("failed to create temp dir for Submariner broker info: %w", err)
	}
	defer os.RemoveAll(brokerDir)

	// Deploy Submariner broker on cluster-0. This writes broker-info.subm to brokerDir.
	cluster0 := r.Clusters[0]
	t.Logf("[%s] Deploying Submariner broker on cluster %s", ProviderOpenShift, cluster0)
	brokerCmd := exec.Command("subctl", "deploy-broker",
		"--context", cluster0,
		"--kubeconfig", kubeConfigPath,
	)
	brokerCmd.Dir = brokerDir
	brokerCmd.Stdout = os.Stdout
	brokerCmd.Stderr = os.Stderr
	if err := brokerCmd.Run(); err != nil {
		return fmt.Errorf("subctl deploy-broker failed on cluster %s: %w", cluster0, err)
	}

	brokerInfoPath := filepath.Join(brokerDir, "broker-info.subm")

	// Join each cluster to the broker.
	// --natt=false: both clusters are in the same GCP project; no NAT traversal needed.
	// --globalnet=false: pod CIDRs are non-overlapping; no global IP remapping needed.
	for _, clusterName := range r.Clusters {
		t.Logf("[%s] Joining cluster %s to Submariner", ProviderOpenShift, clusterName)
		joinCmd := exec.Command("subctl", "join", brokerInfoPath,
			"--context", clusterName,
			"--kubeconfig", kubeConfigPath,
			"--natt=false",
			"--globalnet=false",
		)
		joinCmd.Dir = brokerDir
		joinCmd.Stdout = os.Stdout
		joinCmd.Stderr = os.Stderr
		if err := joinCmd.Run(); err != nil {
			return fmt.Errorf("subctl join failed for cluster %s: %w", clusterName, err)
		}
	}

	// Wait for the Submariner gateway DaemonSet to be ready on both clusters.
	// This confirms the IPsec tunnels are up and cross-cluster pod routing is active.
	for _, clusterName := range r.Clusters {
		kubectlOpts := k8s.NewKubectlOptions(clusterName, kubeConfigPath, "submariner-operator")
		t.Logf("[%s] Waiting for Submariner gateway to be ready on cluster %s", ProviderOpenShift, clusterName)
		_, err := retry.DoWithRetryE(t,
			fmt.Sprintf("wait for submariner-gateway DaemonSet on %s", clusterName),
			30, 20*time.Second,
			func() (string, error) {
				return k8s.RunKubectlAndGetOutputE(t, kubectlOpts,
					"rollout", "status", "daemonset/submariner-gateway", "--timeout=15s")
			})
		if err != nil {
			return fmt.Errorf("Submariner gateway not ready on cluster %s: %w", clusterName, err)
		}
		t.Logf("[%s] Submariner gateway ready on cluster %s", ProviderOpenShift, clusterName)
	}

	t.Logf("[%s] Submariner setup complete — cross-cluster pod networking active", ProviderOpenShift)
	return nil
}

// ─── CLUSTER AUTOSCALER ──────────────────────────────────────────────────────

// setupClusterAutoscaler enables the OpenShift cluster autoscaler on each cluster,
// mirroring GKE's --enable-autoscaling flag. This is required for TestClusterScaleUp:
// when CockroachDB scales from 3 → 4 replicas, the 4th pod goes Pending (anti-affinity
// prevents two pods per node), the autoscaler detects it and provisions a new node via
// MachineSet — exactly the same flow as GKE node pool autoscaling.
//
// Two resources are created per cluster:
//   - ClusterAutoscaler: watches for Pending pods and triggers scale-up/down
//   - MachineAutoscaler: per MachineSet, sets min/max replicas (equivalent to
//     GKE's --min-nodes/--max-nodes)
//
// This function is idempotent: kubectl apply is used throughout.
func (r *OpenShiftRegion) setupClusterAutoscaler(t *testing.T, kubeConfigPath string) error {
	const clusterAutoscalerYAML = `apiVersion: autoscaling.openshift.io/v1
kind: ClusterAutoscaler
metadata:
  name: default
spec:
  resourceLimits:
    maxNodesTotal: 10
  scaleDown:
    enabled: true
    delayAfterAdd: 10m
    unneededTime: 5m`

	for _, clusterName := range r.Clusters {
		t.Logf("[%s] Setting up cluster autoscaler on cluster %s", ProviderOpenShift, clusterName)

		// Apply the ClusterAutoscaler resource.
		clusterOpts := k8s.NewKubectlOptions(clusterName, kubeConfigPath, "")
		if err := k8s.KubectlApplyFromStringE(t, clusterOpts, clusterAutoscalerYAML); err != nil {
			return fmt.Errorf("failed to apply ClusterAutoscaler on cluster %s: %w", clusterName, err)
		}
		t.Logf("[%s] ClusterAutoscaler applied on cluster %s", ProviderOpenShift, clusterName)

		// List MachineSets to create a MachineAutoscaler for each active one.
		machineOpts := k8s.NewKubectlOptions(clusterName, kubeConfigPath, "openshift-machine-api")
		output, err := k8s.RunKubectlAndGetOutputE(t, machineOpts,
			"get", "machinesets",
			"--no-headers",
			"-o", "custom-columns=NAME:.metadata.name,REPLICAS:.spec.replicas")
		if err != nil {
			return fmt.Errorf("failed to list MachineSets for cluster %s: %w", clusterName, err)
		}

		for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
			fields := strings.Fields(line)
			if len(fields) < 2 {
				continue
			}
			msName := fields[0]
			msReplicas := fields[1]
			// Skip MachineSets with 0 replicas — creating a MachineAutoscaler
			// with minReplicas=1 on a 0-replica set would cause unwanted scale-up.
			if msReplicas == "0" || msReplicas == "<none>" {
				t.Logf("[%s] Skipping MachineAutoscaler for %s (replicas=%s)", ProviderOpenShift, msName, msReplicas)
				continue
			}

			maYAML := fmt.Sprintf(`apiVersion: autoscaling.openshift.io/v1beta1
kind: MachineAutoscaler
metadata:
  name: %s
  namespace: openshift-machine-api
spec:
  minReplicas: 1
  maxReplicas: 2
  scaleTargetRef:
    apiVersion: machine.openshift.io/v1beta1
    kind: MachineSet
    name: %s`, msName, msName)

			if err := k8s.KubectlApplyFromStringE(t, machineOpts, maYAML); err != nil {
				return fmt.Errorf("failed to apply MachineAutoscaler for %s on cluster %s: %w", msName, clusterName, err)
			}
			t.Logf("[%s] MachineAutoscaler applied for %s on cluster %s (min=1, max=2)", ProviderOpenShift, msName, clusterName)
		}
	}

	t.Logf("[%s] Cluster autoscaler setup complete", ProviderOpenShift)
	return nil
}

// ─── COREDNS DEPLOYMENT ───────────────────────────────────────────────────────

// deployAndConfigureOpenShiftCoreDNS deploys a custom CoreDNS instance to
// kube-system on each cluster with an external LoadBalancer service. It then:
//  1. Grants SCC permissions for the CoreDNS pods (OpenShift security).
//  2. Waits for the external LB IP.
//  3. Populates r.CorednsClusterOptions with those IPs.
//  4. Applies the full cross-cluster Corefile to every cluster.
func (r *OpenShiftRegion) deployAndConfigureOpenShiftCoreDNS(t *testing.T, kubeConfigPath string) error {
	// Phase 1: deploy CoreDNS on each cluster and collect external LB IPs.
	for i, clusterName := range r.Clusters {
		t.Logf("[%s] Deploying custom CoreDNS on cluster %q", ProviderOpenShift, clusterName)

		kubectlOpts := k8s.NewKubectlOptions(clusterName, kubeConfigPath, coreDNSNamespace)

		// Deploy RBAC first so the coredns ServiceAccount exists before we bind SCC.
		if err := deployCoreDNSRBAC(t, kubectlOpts); err != nil {
			return fmt.Errorf("failed to deploy CoreDNS RBAC on cluster %s: %w", clusterName, err)
		}

		// Grant anyuid SCC to the CoreDNS SA in kube-system so the pods can run.
		// Must come after RBAC deployment so the SA exists.
		// Use "kubectl create clusterrolebinding" instead of "oc adm policy add-scc-to-user":
		// k8s.RunKubectlE prepends --context before the subcommand, which the oc adm
		// plugin rejects with "flags cannot be placed before plugin name: --context".
		if err := k8s.RunKubectlE(t, kubectlOpts,
			"create", "clusterrolebinding", "crl-coredns-anyuid",
			"--clusterrole=system:openshift:scc:anyuid",
			"--serviceaccount=kube-system:coredns"); err != nil {
			t.Logf("[%s] Warning: create clusterrolebinding for coredns SA on %s: %v", ProviderOpenShift, clusterName, err)
		}

		// Deploy ConfigMap + Deployment + wait. RBAC is already applied above.
		if err := deployCoreDNSResources(t, kubectlOpts, operator.CustomDomains[i], r.CorednsClusterOptions); err != nil {
			return fmt.Errorf("failed to deploy CoreDNS resources on cluster %s: %w", clusterName, err)
		}
		if err := deployCoreDNSService(t, kubectlOpts, nil, ProviderOpenShift); err != nil {
			return fmt.Errorf("failed to deploy CoreDNS service on cluster %s: %w", clusterName, err)
		}

		// Wait for the external LoadBalancer IP.
		ips, err := WaitForCoreDNSServiceIPs(t, kubectlOpts)
		if err != nil {
			return fmt.Errorf("failed to get CoreDNS external IPs for cluster %s: %w", clusterName, err)
		}

		r.CorednsClusterOptions[operator.CustomDomains[i]] = coredns.CoreDNSClusterOption{
			IPs:       ips,
			Namespace: r.Namespace[clusterName],
			Domain:    operator.CustomDomains[i],
		}
		t.Logf("[%s] CoreDNS LB IPs for cluster %q: %v", ProviderOpenShift, clusterName, ips)
	}

	// Phase 2: update the Corefile on every cluster with the full cross-cluster config.
	UpdateCoreDNSConfiguration(t, r.Region, kubeConfigPath)

	return nil
}

// getCoreDNSClusterIP returns the ClusterIP of the crl-core-dns-internal service in kube-system.
// This is a ClusterIP-only service exposing both UDP/53 and TCP/53 to the CoreDNS pods.
// The DNS operator patch uses this IP so that the built-in OpenShift DNS can forward
// queries via UDP (its default) to our custom CoreDNS. The LoadBalancer service
// (crl-core-dns) is TCP-only due to GCP LB constraints and cannot be used here.
func (r *OpenShiftRegion) getCoreDNSClusterIP(t *testing.T, clusterName, kubeConfigPath string) (string, error) {
	kubectlOpts := k8s.NewKubectlOptions(clusterName, kubeConfigPath, coreDNSNamespace)

	var clusterIP string
	_, err := retry.DoWithRetryE(t,
		fmt.Sprintf("get crl-core-dns-internal ClusterIP for %s", clusterName),
		defaultRetries, defaultRetryInterval,
		func() (string, error) {
			ip, err := k8s.RunKubectlAndGetOutputE(t, kubectlOpts,
				"get", "service", coreDNSInternalServiceName,
				"-o", "jsonpath={.spec.clusterIP}")
			if err != nil || ip == "" {
				return "", fmt.Errorf("crl-core-dns-internal ClusterIP not ready yet")
			}
			clusterIP = ip
			return ip, nil
		})
	if err != nil {
		return "", fmt.Errorf("failed to get crl-core-dns-internal ClusterIP on cluster %q: %w", clusterName, err)
	}
	return clusterIP, nil
}

// ─── DNS ─────────────────────────────────────────────────────────────────────

// patchOpenShiftDNSAllDomains patches the OpenShift DNS operator to forward ALL
// custom cluster domains (cluster1.local, cluster2.local, …) to the local custom
// CoreDNS ClusterIP. The custom CoreDNS Corefile handles cross-cluster forwarding.
// Used for multi-region tests.
func (r *OpenShiftRegion) patchOpenShiftDNSAllDomains(
	t *testing.T,
	clusterName, kubeConfigPath string,
	allDomains []string,
	coreDNSClusterIP string,
) error {
	t.Logf("[%s] Patching DNS operator on %q for all domains → %s",
		ProviderOpenShift, clusterName, coreDNSClusterIP)

	// Build the servers array for all custom domains.
	var serverEntries []string
	for _, domain := range allDomains {
		entry := fmt.Sprintf(
			`{"name":"%s-forwarder","zones":["%s"],"forwardPlugin":{"upstreams":["%s"]}}`,
			strings.ReplaceAll(domain, ".", "-"),
			domain,
			coreDNSClusterIP,
		)
		serverEntries = append(serverEntries, entry)
	}

	// --type=merge intentionally replaces the entire spec.servers list. This is safe
	// because spec.servers only holds user-defined forwarder entries for custom zones;
	// the operator's built-in cluster DNS (cluster.local, <cluster>.<base-domain>) is
	// managed separately and is unaffected. Replacing the list also ensures no stale
	// entries from previous test runs accumulate.
	patch := fmt.Sprintf(`{"spec":{"servers":[%s]}}`, strings.Join(serverEntries, ","))

	kubectlOpts := k8s.NewKubectlOptions(clusterName, kubeConfigPath, "")
	_, err := k8s.RunKubectlAndGetOutputE(t, kubectlOpts,
		"patch", "dns.operator.openshift.io", "default",
		"--type=merge",
		fmt.Sprintf("--patch=%s", patch))
	if err != nil {
		return fmt.Errorf("failed to patch DNS operator on cluster %q: %w", clusterName, err)
	}

	t.Logf("[%s] DNS operator patched on %q: forwarding %v → %s",
		ProviderOpenShift, clusterName, allDomains, coreDNSClusterIP)
	return nil
}

// ─── GCP COMPUTE CLIENT ──────────────────────────────────────────────────────

// createOpenShiftComputeClient creates a GCP Compute Service client using
// the same credential resolution as the GCP provider.
func createOpenShiftComputeClient(ctx context.Context) (*compute.Service, error) {
	var opts []option.ClientOption
	if keyPath := getServiceAccountKeyPath(); keyPath != "" {
		opts = append(opts, option.WithCredentialsFile(keyPath))
	}
	return compute.NewService(ctx, opts...)
}

// ─── KUBECONFIG ──────────────────────────────────────────────────────────────

// mergeOpenShiftKubeconfig merges the installer-generated kubeconfig into the
// current user kubeconfig and renames the context to clusterAlias so the rest
// of the test framework can reference it by name.
//
// Every OpenShift cluster generates a kubeconfig whose user is named "admin".
// When multiple clusters are merged sequentially, the second cluster's "admin"
// entry would silently overwrite the first cluster's credentials, breaking
// authentication to the first cluster. To prevent this, we rename the user to
// "admin-<clusterAlias>" in a temporary copy before merging so each cluster's
// credentials coexist under distinct names.
func mergeOpenShiftKubeconfig(t *testing.T, generatedKubeconfig, clusterAlias string) error {
	// Determine destination kubeconfig path.
	// KUBECONFIG may legally be a colon-separated list of paths; normalize to a
	// single writable file (the first entry) before using filesystem APIs.
	destKubeconfig := os.Getenv("KUBECONFIG")
	if destKubeconfig == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return fmt.Errorf("failed to determine home dir: %w", err)
		}
		destKubeconfig = filepath.Join(home, ".kube", "config")
	} else if paths := filepath.SplitList(destKubeconfig); len(paths) > 1 {
		destKubeconfig = strings.TrimSpace(paths[0])
	}
	if destKubeconfig == "" {
		return fmt.Errorf("destination kubeconfig path is empty")
	}

	// Rename the "admin" user to "admin-<clusterAlias>" in a temp copy so that
	// merging multiple clusters does not cause one cluster's credentials to
	// overwrite another's (they all use "admin" by default).
	renamedKubeconfig, err := renamedKubeconfigUser(generatedKubeconfig, "admin", "admin-"+clusterAlias)
	if err != nil {
		return fmt.Errorf("failed to rename user in generated kubeconfig: %w", err)
	}
	defer os.Remove(renamedKubeconfig)

	// Merge by setting KUBECONFIG to both files and running kubectl config view --flatten.
	// Put renamedKubeconfig first so the cluster-specific user entry takes
	// precedence over any same-named entry already present in destKubeconfig.
	merged := fmt.Sprintf("%s:%s", renamedKubeconfig, destKubeconfig)
	flattenCmd := exec.Command("kubectl", "config", "view", "--flatten")
	flattenCmd.Env = append(os.Environ(), fmt.Sprintf("KUBECONFIG=%s", merged))
	flattenOut, err := flattenCmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("kubectl config view --flatten failed: %w: %s", err, strings.TrimSpace(string(flattenOut)))
	}

	// Ensure the destination directory exists (may be absent in a clean CI environment).
	if err := os.MkdirAll(filepath.Dir(destKubeconfig), 0700); err != nil {
		return fmt.Errorf("failed to create kubeconfig directory: %w", err)
	}

	// Write merged config back to the destination file.
	if err := os.WriteFile(destKubeconfig, flattenOut, 0600); err != nil {
		return fmt.Errorf("failed to write merged kubeconfig: %w", err)
	}

	// Find the context name that openshift-install created and rename it to
	// clusterAlias. We read from the renamed copy because the context name in
	// the original generated kubeconfig is typically "admin".
	getContextsCmd := exec.Command("kubectl", "config", "get-contexts",
		"--no-headers", "-o", "name",
		"--kubeconfig", renamedKubeconfig)
	out, err := getContextsCmd.Output()
	if err != nil {
		return fmt.Errorf("failed to get contexts from generated kubeconfig: %w", err)
	}

	generatedContext := strings.TrimSpace(string(out))
	if generatedContext == "" {
		return fmt.Errorf("no context found in generated kubeconfig %s", generatedKubeconfig)
	}
	// Take only the first context if multiple lines.
	generatedContext = strings.SplitN(generatedContext, "\n", 2)[0]

	t.Logf("[%s] Renaming kubeconfig context %q → %q", ProviderOpenShift, generatedContext, clusterAlias)

	renameCmd := exec.Command("kubectl", "config", "rename-context", generatedContext, clusterAlias)
	if out, err := renameCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("failed to rename context %q to %q: %w\n%s", generatedContext, clusterAlias, err, string(out))
	}

	return nil
}

// renamedKubeconfigUser loads the kubeconfig at src, renames the user entry
// named oldName to newName (in both the users list and all context references),
// writes the result to a temporary file, and returns the temp file path.
// The caller is responsible for removing the temp file when done.
func renamedKubeconfigUser(src, oldName, newName string) (string, error) {
	cfg, err := clientcmd.LoadFromFile(src)
	if err != nil {
		return "", fmt.Errorf("failed to load kubeconfig %s: %w", src, err)
	}

	// Rename the AuthInfo (user) entry.
	if authInfo, ok := cfg.AuthInfos[oldName]; ok {
		cfg.AuthInfos[newName] = authInfo
		delete(cfg.AuthInfos, oldName)
	}

	// Update any context that references the old user name.
	for _, ctx := range cfg.Contexts {
		if ctx.AuthInfo == oldName {
			ctx.AuthInfo = newName
		}
	}

	tmpFile, err := os.CreateTemp("", "kubeconfig-renamed-*")
	if err != nil {
		return "", fmt.Errorf("failed to create temp kubeconfig file: %w", err)
	}
	tmpFile.Close()

	if err := clientcmd.WriteToFile(*cfg, tmpFile.Name()); err != nil {
		_ = os.Remove(tmpFile.Name())
		return "", fmt.Errorf("failed to write renamed kubeconfig: %w", err)
	}

	return tmpFile.Name(), nil
}

// ─── HELPERS ─────────────────────────────────────────────────────────────────

// getOpenShiftProjectID returns the GCP project ID for OpenShift provisioning.
func getOpenShiftProjectID() string {
	if id := os.Getenv("GCP_PROJECT_ID"); id != "" {
		return id
	}
	return defaultOpenShiftProjectID
}

// installerEnv returns the environment slice for openshift-install commands.
// It propagates the current environment and ensures the Go GCP client inside
// openshift-install uses the correct service-account credentials.
//
// Credential handling:
//   - If GOOGLE_CREDENTIALS is JSON content (CI "Mint mode"), strip
//     GOOGLE_APPLICATION_CREDENTIALS so that ADC does not override it.
//   - If GOOGLE_CREDENTIALS is a file path (local dev), replace
//     GOOGLE_APPLICATION_CREDENTIALS with that path so that both Terraform
//     and the Go GCP client (used for GCS bucket creation, etc.) use the
//     same SA key.  Without this, the Go client falls back to ADC user
//     credentials which may lack storage.buckets.create in the target project.
func installerEnv() []string {
	googleCreds := os.Getenv("GOOGLE_CREDENTIALS")
	// isFilePath is true when GOOGLE_CREDENTIALS holds a file path rather than
	// raw JSON (i.e. not valid JSON and not empty).
	isFilePath := googleCreds != "" && !json.Valid([]byte(googleCreds))

	env := make([]string, 0, len(os.Environ()))
	for _, e := range os.Environ() {
		if strings.HasPrefix(e, "GOOGLE_APPLICATION_CREDENTIALS=") {
			if isFilePath {
				// Forward the SA key file so the Go GCP client can find it.
				env = append(env, "GOOGLE_APPLICATION_CREDENTIALS="+googleCreds)
			}
			// Always skip the original entry (replaced above or stripped for CI).
			continue
		}
		env = append(env, e)
	}
	return env
}

// splitTrimmed splits s by comma, trims whitespace from each element, and
// returns only non-empty elements.
func splitTrimmed(s string) []string {
	var result []string
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part != "" {
			result = append(result, part)
		}
	}
	return result
}

// mustEnv fatals the test if the named environment variable is not set.
func mustEnv(t *testing.T, name string) string {
	t.Helper()
	val := os.Getenv(name)
	if val == "" {
		t.Fatalf("[%s] Required environment variable %q is not set", ProviderOpenShift, name)
	}
	return val
}

// Ensure OpenShiftRegion satisfies the CloudProvider interface at compile time.
var _ CloudProvider = (*OpenShiftRegion)(nil)
