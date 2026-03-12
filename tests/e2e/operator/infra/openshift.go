package infra

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"text/template"

	"github.com/cockroachdb/helm-charts/tests/e2e/operator"
	"github.com/gruntwork-io/terratest/modules/k8s"
	"github.com/gruntwork-io/terratest/modules/retry"
	"github.com/stretchr/testify/require"
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

	// defaultOpenShiftProjectID is the GCP project used when GCP_PROJECT_ID is not set.
	defaultOpenShiftProjectID = "cockroach-shreyaskm"

	// openshift-dns namespace where the built-in DNS service lives.
	openshiftDNSNamespace = "openshift-dns"
	openshiftDNSService   = "dns-default"

	// maxInstallerNameLen is the maximum length of the cluster name in install-config.yaml.
	// OpenShift enforces a 14-character limit on metadata.name.
	maxInstallerNameLen = 14
)

// installConfigTemplate is the install-config.yaml template for openshift-install.
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
platform:
  gcp:
    projectID: {{ .ProjectID }}
    region: {{ .Region }}
pullSecret: '{{ .PullSecret }}'
sshKey: {{ .SSHPubKey }}
`))

type installConfigData struct {
	BaseDomain  string
	ClusterName string
	ProjectID   string
	Region      string
	PullSecret  string
	SSHPubKey   string
}

// ─── OPENSHIFT REGION ─────────────────────────────────────────────────────────

// OpenShiftRegion implements CloudProvider for OpenShift clusters on GCP,
// provisioned via openshift-install.
type OpenShiftRegion struct {
	*operator.Region
	// installDirs maps cluster name → temp directory that holds install-config.yaml
	// and all state produced by openshift-install (auth/kubeconfig, terraform state, etc.)
	installDirs map[string]string
}

// SetUpInfra provisions one OpenShift cluster per entry in r.Clusters.
// For single-region tests there is exactly one cluster.
// If OPENSHIFT_INSTALL_DIR is set, provisioning is skipped and the existing
// cluster at that path is reused (kubeconfig merged, client created).
func (r *OpenShiftRegion) SetUpInfra(t *testing.T) {
	r.installDirs = make(map[string]string)
	r.Clients = make(map[string]client.Client)

	reuseDir := os.Getenv(envOpenShiftInstallDir)

	if reuseDir != "" {
		t.Logf("[%s] Reusing existing cluster from OPENSHIFT_INSTALL_DIR=%s", ProviderOpenShift, reuseDir)
		// Single-region: map the one cluster name to the provided install dir.
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
		r.ReusingInfra = true
		t.Logf("[%s] Reuse complete — cluster %q is ready", ProviderOpenShift, clusterName)
		return
	}

	pullSecret := mustEnv(t, envOpenShiftPullSecret)
	sshPubKey := mustEnv(t, envOpenShiftSSHPubKey)
	baseDomain := mustEnv(t, envOpenShiftBaseDomain)
	projectID := getOpenShiftProjectID()

	// For single-region there is one cluster; the loop generalises cleanly.
	for i, clusterName := range r.Clusters {
		regionCode := r.RegionCodes[i]
		t.Logf("[%s] Provisioning cluster %q in region %s (project %s)", ProviderOpenShift, clusterName, regionCode, projectID)

		installDir, err := r.provisionCluster(t, clusterName, regionCode, projectID, baseDomain, pullSecret, sshPubKey)
		if err != nil {
			t.Fatalf("[%s] Failed to provision cluster %q: %v", ProviderOpenShift, clusterName, err)
		}
		r.installDirs[clusterName] = installDir

		// Merge the generated kubeconfig and alias the context to the short cluster name.
		generatedKubeconfig := filepath.Join(installDir, "auth", "kubeconfig")
		if err := mergeOpenShiftKubeconfig(t, generatedKubeconfig, clusterName); err != nil {
			t.Fatalf("[%s] Failed to merge kubeconfig for %q: %v", ProviderOpenShift, clusterName, err)
		}

		// Create a controller-runtime client for this cluster context.
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
	}

	// Configure DNS: patch each cluster's DNS operator to serve cluster<N>.local
	// by forwarding queries to that cluster's own built-in DNS service.
	kubeConfigPath, err := r.EnsureKubeConfigPath()
	require.NoError(t, err)

	for i, clusterName := range r.Clusters {
		if err := r.patchOpenShiftDNS(t, clusterName, kubeConfigPath, i); err != nil {
			t.Fatalf("[%s] Failed to configure DNS for cluster %q: %v", ProviderOpenShift, clusterName, err)
		}
	}

	r.ReusingInfra = true
	t.Logf("[%s] Infrastructure setup complete", ProviderOpenShift)
}

// TeardownInfra destroys all provisioned OpenShift clusters.
// openshift-install destroy cluster handles all GCP resource cleanup automatically.
func (r *OpenShiftRegion) TeardownInfra(t *testing.T) {
	t.Logf("[%s] Starting infrastructure teardown", ProviderOpenShift)

	for clusterName, installDir := range r.installDirs {
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

// ScaleNodePool is a no-op for OpenShift (not yet implemented).
func (r *OpenShiftRegion) ScaleNodePool(t *testing.T, location string, nodeCount, index int) {
	t.Logf("[%s] Node pool scaling is not yet implemented for OpenShift", ProviderOpenShift)
}

// CanScale reports whether this provider supports scaling.
func (r *OpenShiftRegion) CanScale() bool {
	return false
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
// Returns the install directory path on success.
func (r *OpenShiftRegion) provisionCluster(
	t *testing.T,
	clusterName, region, projectID, baseDomain, pullSecret, sshPubKey string,
) (string, error) {
	// OpenShift metadata.name must be ≤14 chars; derive a short name from the
	// random suffix of the full cluster name.
	installerName := shortInstallerName(clusterName)
	t.Logf("[%s] Using installer name %q for cluster %q", ProviderOpenShift, installerName, clusterName)
	// Create a temp directory to hold all installer state.
	installDir, err := os.MkdirTemp("", fmt.Sprintf("ocp-%s-*", installerName))
	if err != nil {
		return "", fmt.Errorf("failed to create install dir: %w", err)
	}

	// Write install-config.yaml.
	configPath := filepath.Join(installDir, "install-config.yaml")
	f, err := os.Create(configPath)
	if err != nil {
		_ = os.RemoveAll(installDir)
		return "", fmt.Errorf("failed to create install-config.yaml: %w", err)
	}

	data := installConfigData{
		BaseDomain:  baseDomain,
		ClusterName: installerName,
		ProjectID:   projectID,
		Region:      region,
		PullSecret:  pullSecret,
		SSHPubKey:   sshPubKey,
	}
	if err := installConfigTemplate.Execute(f, data); err != nil {
		f.Close()
		_ = os.RemoveAll(installDir)
		return "", fmt.Errorf("failed to render install-config.yaml: %w", err)
	}
	f.Close()

	t.Logf("[%s] Running: openshift-install create cluster --dir=%s", ProviderOpenShift, installDir)

	cmd := exec.Command("openshift-install", "create", "cluster",
		"--dir", installDir,
		"--log-level", "info",
	)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	// Explicitly set GOOGLE_CREDENTIALS and clear GOOGLE_APPLICATION_CREDENTIALS
	// so the installer uses the service account key (Mint mode) regardless of
	// any ADC credentials set in the shell environment.
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
		_ = os.RemoveAll(installDir)
		return "", fmt.Errorf("openshift-install create cluster failed: %w", err)
	}

	return installDir, nil
}

// ─── KUBECONFIG ──────────────────────────────────────────────────────────────

// mergeOpenShiftKubeconfig merges the installer-generated kubeconfig into the
// current user kubeconfig and renames the context to clusterAlias so the rest
// of the test framework can reference it by name.
func mergeOpenShiftKubeconfig(t *testing.T, generatedKubeconfig, clusterAlias string) error {
	// Determine destination kubeconfig path.
	destKubeconfig := os.Getenv("KUBECONFIG")
	if destKubeconfig == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return fmt.Errorf("failed to determine home dir: %w", err)
		}
		destKubeconfig = filepath.Join(home, ".kube", "config")
	}

	// Merge by setting KUBECONFIG to both files and running kubectl config view --flatten.
	merged := fmt.Sprintf("%s:%s", destKubeconfig, generatedKubeconfig)
	flattenCmd := exec.Command("kubectl", "config", "view", "--flatten")
	flattenCmd.Env = append(os.Environ(), fmt.Sprintf("KUBECONFIG=%s", merged))
	flattenOut, err := flattenCmd.Output()
	if err != nil {
		return fmt.Errorf("kubectl config view --flatten failed: %w", err)
	}

	// Write merged config back to the destination file.
	if err := os.WriteFile(destKubeconfig, flattenOut, 0600); err != nil {
		return fmt.Errorf("failed to write merged kubeconfig: %w", err)
	}

	// Find the context name that openshift-install created and rename it.
	// The generated context is typically "admin" in the installer kubeconfig.
	// After merge it becomes the first context from the generated file, which
	// we can find by parsing the generated kubeconfig directly.
	getContextsCmd := exec.Command("kubectl", "config", "get-contexts",
		"--no-headers", "-o", "name",
		"--kubeconfig", generatedKubeconfig)
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

// ─── DNS ─────────────────────────────────────────────────────────────────────

// patchOpenShiftDNS patches the OpenShift DNS operator so that queries for
// cluster<N>.local are forwarded to that cluster's own built-in DNS service IP.
// This makes CockroachDB pod FQDNs like
//   cockroachdb-0.cockroachdb.<ns>.svc.cluster1.local
// resolvable within the cluster even though OpenShift natively only serves cluster.local.
func (r *OpenShiftRegion) patchOpenShiftDNS(t *testing.T, clusterName, kubeConfigPath string, index int) error {
	domain := operator.CustomDomains[index] // e.g. "cluster1.local"

	// Resolve the built-in DNS service ClusterIP for this cluster.
	kubectlOpts := k8s.NewKubectlOptions(clusterName, kubeConfigPath, openshiftDNSNamespace)

	var dnsIP string
	_, err := retry.DoWithRetryE(t,
		fmt.Sprintf("get dns-default service IP for %s", clusterName),
		defaultRetries, defaultRetryInterval,
		func() (string, error) {
			ip, err := k8s.RunKubectlAndGetOutputE(t, kubectlOpts,
				"get", "service", openshiftDNSService,
				"-o", "jsonpath={.spec.clusterIP}")
			if err != nil || ip == "" {
				return "", fmt.Errorf("dns-default ClusterIP not ready yet")
			}
			dnsIP = ip
			return ip, nil
		})
	if err != nil {
		return fmt.Errorf("failed to get dns-default ClusterIP for cluster %q: %w", clusterName, err)
	}

	t.Logf("[%s] Patching DNS operator on %q: %s → %s", ProviderOpenShift, clusterName, domain, dnsIP)

	// Build the patch that tells the OpenShift DNS operator to forward
	// <domain> queries to the cluster's own DNS IP (which already knows how
	// to answer *.svc.cluster.local queries — we just alias cluster<N>.local
	// to cluster.local by pointing it at the same resolver).
	patch := fmt.Sprintf(`{"spec":{"servers":[{"name":"%s-forwarder","zones":["%s"],"forwardPlugin":{"upstreams":["%s"]}}]}}`,
		strings.ReplaceAll(domain, ".", "-"), domain, dnsIP)

	kubectlCluster := k8s.NewKubectlOptions(clusterName, kubeConfigPath, "")

	_, err = k8s.RunKubectlAndGetOutputE(t, kubectlCluster,
		"patch", "dns.operator.openshift.io", "default",
		"--type=merge",
		fmt.Sprintf("--patch=%s", patch))
	if err != nil {
		return fmt.Errorf("failed to patch DNS operator on cluster %q: %w", clusterName, err)
	}

	t.Logf("[%s] DNS operator patched on %q: %s → %s", ProviderOpenShift, clusterName, domain, dnsIP)
	return nil
}

// ─── SCC ─────────────────────────────────────────────────────────────────────

// ApplyOpenShiftSCCBindings grants the anyuid SCC to all service accounts
// in the namespace using a group binding. This covers every SA the helm charts
// create (cockroachdb, cockroach-operator, cockroachdb-rotate-self-signer, etc.)
// without needing to enumerate them individually.
// Called from region.go InstallCharts immediately after namespace creation.
func ApplyOpenShiftSCCBindings(t *testing.T, kubectlOptions *k8s.KubectlOptions) {
	group := fmt.Sprintf("system:serviceaccounts:%s", kubectlOptions.Namespace)
	t.Logf("[%s] Granting anyuid SCC to group %s", ProviderOpenShift, group)
	if err := k8s.RunKubectlE(t, kubectlOptions,
		"adm", "policy", "add-scc-to-group", "anyuid", group); err != nil {
		t.Logf("[%s] Warning: add-scc-to-group for %s: %v", ProviderOpenShift, group, err)
	}
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
// It propagates the current environment but explicitly removes
// GOOGLE_APPLICATION_CREDENTIALS so that ADC does not take precedence over
// the service-account key supplied via GOOGLE_CREDENTIALS (Mint mode).
func installerEnv() []string {
	env := make([]string, 0, len(os.Environ()))
	for _, e := range os.Environ() {
		if strings.HasPrefix(e, "GOOGLE_APPLICATION_CREDENTIALS=") {
			continue
		}
		env = append(env, e)
	}
	return env
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

// certManagerInstall installs cert-manager on the given cluster using the
// existing testutil helper and waits for it to be ready.
func certManagerInstall(t *testing.T, clusterName, kubeConfigPath string) {
	// Reuse the cert-manager namespace from testutil constants.
	const certManagerNamespace = "cert-manager"
	opts := k8s.NewKubectlOptions(clusterName, kubeConfigPath, certManagerNamespace)

	t.Logf("[%s] Installing cert-manager on cluster %q", ProviderOpenShift, clusterName)

	// Apply the upstream cert-manager manifest.
	_, err := retry.DoWithRetryE(t, "install cert-manager", defaultRetries, defaultRetryInterval,
		func() (string, error) {
			return k8s.RunKubectlAndGetOutputE(t, opts,
				"apply", "-f",
				"https://github.com/cert-manager/cert-manager/releases/latest/download/cert-manager.yaml")
		})
	if err != nil {
		t.Fatalf("[%s] Failed to install cert-manager on %q: %v", ProviderOpenShift, clusterName, err)
	}

	// Wait for the cert-manager webhook deployment to be available.
	_, err = retry.DoWithRetryE(t, "wait for cert-manager webhook", 60, defaultRetryInterval,
		func() (string, error) {
			return k8s.RunKubectlAndGetOutputE(t, opts,
				"wait", "--for=condition=Available", "deployment/cert-manager-webhook",
				"--timeout=30s")
		})
	if err != nil {
		t.Fatalf("[%s] cert-manager webhook not ready on %q: %v", ProviderOpenShift, clusterName, err)
	}

	t.Logf("[%s] cert-manager ready on cluster %q", ProviderOpenShift, clusterName)
}

// Ensure OpenShiftRegion satisfies the CloudProvider interface at compile time.
var _ CloudProvider = (*OpenShiftRegion)(nil)
