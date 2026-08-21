package infra

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/cockroachdb/helm-charts/tests/e2e/coredns"
	"github.com/cockroachdb/helm-charts/tests/e2e/operator"
	"github.com/cockroachdb/helm-charts/tests/e2e/operator/encryption"
	"github.com/gruntwork-io/terratest/modules/k8s"
	"github.com/gruntwork-io/terratest/modules/random"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"
)

const (
	azureDefaultResourcePrefix = "helm-charts-e2e"
	azureDefaultNodeVMSize     = "Standard_D4s_v3"
	azureDefaultMaxPods        = 30
	azureCLICommandTimeout     = 10 * time.Minute
	azureAKSCreateTimeout      = 45 * time.Minute
	azureAKSCreateAttempts     = 3
	azureAKSCreatePollAttempts = 30
	azureAKSCreatePollInterval = 20 * time.Second
	azureDeleteAttempts        = 5
	azureDeleteRetrySleep      = 20 * time.Second
	azureKubernetesAttempts    = 5
	azureKubernetesRetrySleep  = 10 * time.Second
	azureMaxNodeRGNameLength   = 80
)

const (
	envAzureSubscriptionID     = "AZURE_SUBSCRIPTION_ID"
	envAzureClientID           = "AZURE_CLIENT_ID"
	envAzureClientSecret       = "AZURE_CLIENT_SECRET" // #nosec G101 - env var name, not a credential
	envAzureTenantID           = "AZURE_TENANT_ID"
	envAzureResourceGroup      = "AZURE_RESOURCE_GROUP"
	envAzureResourceGroupsFile = "AZURE_RESOURCE_GROUPS_FILE"
	envAzureResourcePrefix     = "AZURE_RESOURCE_PREFIX"
	envAzureReuseContexts      = "AZURE_REUSE_CONTEXTS"
	envAzureNodeVMSize         = "AZURE_NODE_VM_SIZE"
	envAzureSkipTeardown       = "AZURE_SKIP_TEARDOWN"
	envAzureTicket             = "AZURE_TICKET"
)

type azureClusterConfig struct {
	Region       string
	ClusterName  string
	VNetName     string
	VNetCIDR     string
	SubnetName   string
	SubnetCIDR   string
	ServiceCIDR  string
	DNSServiceIP string
}

var azureClusterConfigTemplates = []azureClusterConfig{
	{
		Region:       "eastus",
		VNetCIDR:     "10.10.0.0/16",
		SubnetCIDR:   "10.10.0.0/24",
		ServiceCIDR:  "172.28.17.0/24",
		DNSServiceIP: "172.28.17.10",
	},
	{
		Region:       "westus2",
		VNetCIDR:     "10.20.0.0/16",
		SubnetCIDR:   "10.20.0.0/24",
		ServiceCIDR:  "172.28.49.0/24",
		DNSServiceIP: "172.28.49.10",
	},
}

var azureCLIEnvOverrides = captureAzureCLIEnv()

func captureAzureCLIEnv() map[string]string {
	env := make(map[string]string)
	for _, key := range []string{
		"HTTP_PROXY",
		"HTTPS_PROXY",
		"NO_PROXY",
		"http_proxy",
		"https_proxy",
		"no_proxy",
		"REQUESTS_CA_BUNDLE",
		"AZURE_CLI_CA_CERTS",
		"SSL_CERT_FILE",
	} {
		if value, ok := os.LookupEnv(key); ok {
			env[key] = value
		}
	}
	return env
}

func azureCommand(args ...string) *exec.Cmd {
	return azureCommandContext(context.Background(), args...)
}

func azureCommandContext(ctx context.Context, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "az", args...)
	cmd.Env = withEnvOverrides(os.Environ(), azureCLIEnvOverrides)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	cmd.WaitDelay = 30 * time.Second
	return cmd
}

func azureCommandWithTimeout(timeout time.Duration, args ...string) (*exec.Cmd, context.CancelFunc) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	return azureCommandContext(ctx, args...), cancel
}

func withEnvOverrides(base []string, overrides map[string]string) []string {
	if len(overrides) == 0 {
		return base
	}

	keys := make(map[string]struct{}, len(overrides))
	for key := range overrides {
		keys[key] = struct{}{}
	}

	env := make([]string, 0, len(base)+len(overrides))
	for _, entry := range base {
		key, _, found := strings.Cut(entry, "=")
		if found {
			if _, overridden := keys[key]; overridden {
				continue
			}
		}
		env = append(env, entry)
	}
	for key, value := range overrides {
		env = append(env, key+"="+value)
	}
	return env
}

func disableKubernetesProxyEnv(t *testing.T) {
	t.Helper()

	previous := make(map[string]*string)
	disabled := false
	for _, key := range []string{"HTTP_PROXY", "HTTPS_PROXY", "http_proxy", "https_proxy"} {
		if value, ok := os.LookupEnv(key); ok {
			valueCopy := value
			previous[key] = &valueCopy
			_ = os.Unsetenv(key)
			disabled = true
		} else {
			previous[key] = nil
		}
	}
	if disabled {
		t.Logf("[%s] Disabled proxy environment for Kubernetes clients; az CLI calls preserve original proxy settings",
			ProviderAzure)
	}
	t.Cleanup(func() {
		for key, value := range previous {
			if value == nil {
				_ = os.Unsetenv(key)
				continue
			}
			_ = os.Setenv(key, *value)
		}
	})
}

// AzureRegion provisions AKS clusters for Helm chart E2E runs.
type AzureRegion struct {
	*operator.Region

	resourceGroupName string
	clusterConfigs    []azureClusterConfig
	kubeConfigPath    string
}

func (r *AzureRegion) SetUpInfra(t *testing.T) {
	if len(r.Clusters) == 0 {
		t.Fatalf("[%s] no clusters configured", ProviderAzure)
	}
	if len(r.Clusters) > len(azureClusterConfigTemplates) {
		t.Fatalf("[%s] need %d Azure cluster configs, only have %d",
			ProviderAzure, len(r.Clusters), len(azureClusterConfigTemplates))
	}
	if _, err := exec.LookPath("az"); err != nil {
		t.Fatalf("[%s] az CLI not found in PATH: %v", ProviderAzure, err)
	}
	if _, err := exec.LookPath("kubectl"); err != nil {
		t.Fatalf("[%s] kubectl not found in PATH: %v", ProviderAzure, err)
	}
	if len(r.RegionCodes) == 0 {
		r.RegionCodes = GetRegionCodes(ProviderAzure)
	}

	r.configureIsolatedKubeconfig(t)
	disableKubernetesProxyEnv(t)

	if reuseContexts := azureReuseContexts(t, len(r.Clusters)); len(reuseContexts) > 0 {
		r.Clusters = reuseContexts
		r.ReusingInfra = true
	}

	if r.ReusingInfra {
		r.reuseInfra(t)
		return
	}

	subscriptionID := mustAzureEnv(t, envAzureSubscriptionID)
	require.NoError(t, ensureAzureLogin(t, subscriptionID), "[%s] Azure authentication failed", ProviderAzure)

	uid := strings.ToLower(random.UniqueId())
	prefix := azureResourcePrefix()
	r.resourceGroupName = fmt.Sprintf("%s-rg-%s", prefix, uid)
	r.clusterConfigs = r.buildClusterConfigs(prefix, uid)
	require.NoError(t, r.validateGeneratedNames(), "[%s] invalid generated Azure resource names", ProviderAzure)
	require.NoError(t, r.recordResourceGroupForCI(), "[%s] failed to record Azure resource group for CI cleanup",
		ProviderAzure)
	r.Clients = make(map[string]client.Client)
	r.CorednsClusterOptions = make(map[string]coredns.CoreDNSClusterOption)

	rgLocation := r.clusterConfigs[0].Region
	t.Logf("[%s] Creating resource group %s in %s", ProviderAzure, r.resourceGroupName, rgLocation)
	require.NoError(t, r.createResourceGroup(rgLocation, uid), "[%s] failed to create resource group", ProviderAzure)

	for i := range r.clusterConfigs {
		cfg := &r.clusterConfigs[i]
		t.Logf("[%s] Creating VNet %s in %s", ProviderAzure, cfg.VNetName, cfg.Region)
		require.NoError(t, r.createVNetAndSubnet(cfg), "[%s] failed to create VNet/subnet for %s",
			ProviderAzure, cfg.ClusterName)
	}

	require.NoError(t, r.createAKSClusters(t), "[%s] failed to create AKS clusters", ProviderAzure)

	if len(r.Clusters) > 1 {
		require.NoError(t, r.setupVNetPeering(t), "[%s] failed to set up VNet peering", ProviderAzure)
	}

	require.NoError(t, r.deployAndConfigureCoreDNS(t, r.kubeConfigPath), "[%s] failed to configure AKS DNS",
		ProviderAzure)

	r.ReusingInfra = true
	t.Logf("[%s] Infrastructure setup complete; cleanup resource group with: az group delete --name %s --subscription %s --yes",
		ProviderAzure, r.resourceGroupName, subscriptionID)
}

func (r *AzureRegion) TeardownInfra(t *testing.T) {
	if parseBoolEnv(envAzureSkipTeardown) {
		t.Logf("[%s] %s=true; skipping resource group teardown", ProviderAzure, envAzureSkipTeardown)
		return
	}

	resourceGroup := r.resourceGroupName
	if resourceGroup == "" {
		resourceGroup = strings.TrimSpace(os.Getenv(envAzureResourceGroup))
	}
	if resourceGroup == "" {
		t.Logf("[%s] no resource group recorded; set %s to clean up manually", ProviderAzure, envAzureResourceGroup)
		return
	}

	subscriptionID := strings.TrimSpace(os.Getenv(envAzureSubscriptionID))
	if subscriptionID == "" {
		t.Logf("[%s] %s is not set; clean up manually with: az group delete --name %s --yes",
			ProviderAzure, envAzureSubscriptionID, resourceGroup)
		return
	}

	if err := ensureAzureLogin(t, subscriptionID); err != nil {
		t.Logf("[%s] warning: Azure auth failed during teardown: %v", ProviderAzure, err)
	}

	t.Logf("[%s] Deleting resource group %s", ProviderAzure, resourceGroup)
	for attempt := 1; attempt <= azureDeleteAttempts; attempt++ {
		cmd, cancel := azureCommandWithTimeout(azureCLICommandTimeout, "group", "delete",
			"--name", resourceGroup,
			"--subscription", subscriptionID,
			"--yes",
			"--no-wait",
			"--output", "none",
		)
		out, err := cmd.CombinedOutput()
		cancel()
		if err == nil {
			t.Logf("[%s] Initiated async deletion for resource group %s", ProviderAzure, resourceGroup)
			return
		}
		if isAzureResourceNotFoundError(err) {
			t.Logf("[%s] resource group %s is already deleted", ProviderAzure, resourceGroup)
			return
		}
		if !isTransientAzureCLIError(err, out) || attempt == azureDeleteAttempts {
			t.Logf("[%s] warning: failed to initiate resource group deletion for %s: %v\n%s",
				ProviderAzure, resourceGroup, err, strings.TrimSpace(string(out)))
			return
		}
		t.Logf("[%s] az group delete %s hit a transient Azure CLI transport error on attempt %d/%d: %v\n%s",
			ProviderAzure, resourceGroup, attempt, azureDeleteAttempts, err, strings.TrimSpace(string(out)))
		time.Sleep(azureDeleteRetrySleep)
	}
}

func (r *AzureRegion) ScaleNodePool(t *testing.T, location string, nodeCount, index int) {
	t.Logf("[%s] Node scaling delegated to AKS cluster autoscaler for cluster index %d in %s; desired CRDB nodes=%d",
		ProviderAzure, index, location, nodeCount)
}

func (r *AzureRegion) CanScale() bool {
	return true
}

func (r *AzureRegion) GetEncryptionProvider() encryption.Provider {
	return r
}

func (r *AzureRegion) SetupEncryptionInfrastructure(t *testing.T) (func(), error) {
	t.Logf("[%s] No KMS infrastructure needed for file-based encryption (UNKNOWN_KEY_TYPE)", ProviderAzure)
	return func() {
		t.Logf("[%s] No KMS infrastructure to clean up", ProviderAzure)
	}, nil
}

func (r *AzureRegion) GetEncryptionPlatformConfig() *encryption.PlatformConfig {
	return &encryption.PlatformConfig{
		Platform:                     "UNKNOWN_KEY_TYPE",
		RequiresCredentialsSecret:    false,
		DefaultCredentialsSecretName: "",
	}
}

func (r *AzureRegion) EncryptKey(plaintextKey []byte, clusterRegion string) (string, error) {
	return "", fmt.Errorf("EncryptKey not supported for %s (file-based encryption / UNKNOWN_KEY_TYPE)", ProviderAzure)
}

func (r *AzureRegion) CreateKeySecret(
	kubectlOptions *k8s.KubectlOptions, secretName string, encryptedKeyData string, clusterRegion string,
) error {
	return fmt.Errorf("CreateKeySecret not supported for %s (file-based encryption / UNKNOWN_KEY_TYPE)", ProviderAzure)
}

func (r *AzureRegion) CreateCredentialsSecret(kubectlOptions *k8s.KubectlOptions) (string, error) {
	return "", fmt.Errorf("CreateCredentialsSecret not supported for %s (file-based encryption / UNKNOWN_KEY_TYPE)",
		ProviderAzure)
}

func (r *AzureRegion) configureIsolatedKubeconfig(t *testing.T) {
	t.Helper()

	kubeConfigPath := r.kubeConfigPath
	if kubeConfigPath == "" {
		kubeConfigPath = fmt.Sprintf("%s/azure-kubeconfig.yaml", t.TempDir())
	}

	emptyConfig := []byte("apiVersion: v1\nkind: Config\nclusters: []\ncontexts: []\nusers: []\n")
	require.NoError(t, os.WriteFile(kubeConfigPath, emptyConfig, 0600),
		"[%s] failed to initialize isolated kubeconfig", ProviderAzure)

	previousKubeconfig := os.Getenv("KUBECONFIG")
	require.NoError(t, os.Setenv("KUBECONFIG", kubeConfigPath))
	r.kubeConfigPath = kubeConfigPath
	t.Cleanup(func() {
		if previousKubeconfig != "" {
			_ = os.Setenv("KUBECONFIG", previousKubeconfig)
			return
		}
		_ = os.Unsetenv("KUBECONFIG")
	})

	t.Logf("[%s] Using isolated kubeconfig %s", ProviderAzure, kubeConfigPath)
}

func (r *AzureRegion) reuseInfra(t *testing.T) {
	resourceGroup := strings.TrimSpace(os.Getenv(envAzureResourceGroup))
	require.NotEmpty(t, resourceGroup, "[%s] %s must be set when reusing Azure infrastructure",
		ProviderAzure, envAzureResourceGroup)

	r.resourceGroupName = resourceGroup
	r.Clients = make(map[string]client.Client)
	r.CorednsClusterOptions = make(map[string]coredns.CoreDNSClusterOption)

	subscriptionID := mustAzureEnv(t, envAzureSubscriptionID)
	require.NoError(t, ensureAzureLogin(t, subscriptionID), "[%s] Azure authentication failed", ProviderAzure)

	for i, clusterName := range r.Clusters {
		require.NoError(t, UpdateKubeconfigAzure(t, resourceGroup, clusterName, clusterName),
			"[%s] failed to get credentials for reused AKS cluster %s", ProviderAzure, clusterName)
		k8sClient, err := clientForContext(clusterName)
		require.NoError(t, err, "[%s] failed to initialize Kubernetes client for %s", ProviderAzure, clusterName)
		r.Clients[clusterName] = k8sClient

		r.CorednsClusterOptions[operator.CustomDomains[i]] = coredns.CoreDNSClusterOption{
			IPs:       []string{"127.0.0.1"},
			Namespace: r.Namespace[clusterName],
			Domain:    operator.CustomDomains[i],
		}
	}

	require.NoError(t, r.deployAndConfigureCoreDNS(t, r.kubeConfigPath),
		"[%s] failed to configure DNS on reused AKS infrastructure", ProviderAzure)
}

func (r *AzureRegion) buildClusterConfigs(prefix, uid string) []azureClusterConfig {
	configs := make([]azureClusterConfig, len(r.Clusters))
	for i, clusterName := range r.Clusters {
		configs[i] = azureClusterConfigTemplates[i]
		if len(r.RegionCodes) > i && r.RegionCodes[i] != "" {
			configs[i].Region = r.RegionCodes[i]
		}
		configs[i].ClusterName = clusterName
		configs[i].VNetName = fmt.Sprintf("%s-vnet-%d-%s", prefix, i, uid)
		configs[i].SubnetName = fmt.Sprintf("%s-subnet", clusterName)
	}
	return configs
}

func (r *AzureRegion) createResourceGroup(location, testRun string) error {
	args := []string{
		"group", "create",
		"--name", r.resourceGroupName,
		"--location", location,
		"--subscription", azureSubscriptionID(),
		"--tags",
		"ManagedBy=helm-charts-e2e",
		"TestRun=" + testRun,
	}
	if ticket := strings.TrimSpace(os.Getenv(envAzureTicket)); ticket != "" {
		args = append(args, "Ticket="+ticket)
	}
	for _, tag := range []struct {
		key string
		env string
	}{
		{key: "GitHubRunID", env: "GITHUB_RUN_ID"},
		{key: "GitHubRunAttempt", env: "GITHUB_RUN_ATTEMPT"},
		{key: "GitHubRepository", env: "GITHUB_REPOSITORY"},
		{key: "GitHubWorkflow", env: "GITHUB_WORKFLOW"},
		{key: "GitHubJob", env: "GITHUB_JOB"},
		{key: "GitHubRefName", env: "GITHUB_REF_NAME"},
	} {
		if value := strings.TrimSpace(os.Getenv(tag.env)); value != "" {
			args = append(args, tag.key+"="+value)
		}
	}
	args = append(args, "--output", "none")

	cmd, cancel := azureCommandWithTimeout(azureCLICommandTimeout, args...)
	defer cancel()
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("az group create %s: %w", r.resourceGroupName, err)
	}
	return nil
}

func (r *AzureRegion) recordResourceGroupForCI() error {
	recordFile := strings.TrimSpace(os.Getenv(envAzureResourceGroupsFile))
	if recordFile == "" || r.resourceGroupName == "" {
		return nil
	}

	if err := os.MkdirAll(filepath.Dir(recordFile), 0755); err != nil {
		return fmt.Errorf("create parent directory for %s: %w", recordFile, err)
	}
	file, err := os.OpenFile(recordFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		return fmt.Errorf("open %s: %w", recordFile, err)
	}
	defer file.Close()

	if _, err := fmt.Fprintln(file, r.resourceGroupName); err != nil {
		return fmt.Errorf("write %s: %w", recordFile, err)
	}
	return nil
}

func (r *AzureRegion) validateGeneratedNames() error {
	for _, cfg := range r.clusterConfigs {
		nodeResourceGroup := fmt.Sprintf("MC_%s_%s_%s", r.resourceGroupName, cfg.ClusterName, cfg.Region)
		if len(nodeResourceGroup) > azureMaxNodeRGNameLength {
			return fmt.Errorf("generated AKS node resource group name %q is %d characters; max is %d; lower %s",
				nodeResourceGroup, len(nodeResourceGroup), azureMaxNodeRGNameLength, envAzureResourcePrefix)
		}
	}
	return nil
}

func (r *AzureRegion) createVNetAndSubnet(cfg *azureClusterConfig) error {
	cmd, cancel := azureCommandWithTimeout(azureCLICommandTimeout, "network", "vnet", "create",
		"--resource-group", r.resourceGroupName,
		"--name", cfg.VNetName,
		"--location", cfg.Region,
		"--address-prefix", cfg.VNetCIDR,
		"--subnet-name", cfg.SubnetName,
		"--subnet-prefix", cfg.SubnetCIDR,
		"--subscription", azureSubscriptionID(),
		"--output", "none",
	)
	defer cancel()
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("az network vnet create %s: %w", cfg.VNetName, err)
	}
	return nil
}

func (r *AzureRegion) createAKSClusters(t *testing.T) error {
	for i, clusterName := range r.Clusters {
		cfg := r.clusterConfigs[i]
		t.Logf("[%s] Creating AKS cluster %s in %s", ProviderAzure, clusterName, cfg.Region)
		if err := createAKSCluster(t, r.resourceGroupName, cfg, r.NodeCount); err != nil {
			return err
		}
		if err := UpdateKubeconfigAzure(t, r.resourceGroupName, clusterName, clusterName); err != nil {
			return err
		}
		k8sClient, err := clientForContext(clusterName)
		if err != nil {
			return err
		}
		r.Clients[clusterName] = k8sClient
		r.CorednsClusterOptions[operator.CustomDomains[i]] = coredns.CoreDNSClusterOption{
			IPs:       []string{"127.0.0.1"},
			Namespace: r.Namespace[clusterName],
			Domain:    operator.CustomDomains[i],
		}
	}
	return nil
}

func createAKSCluster(t *testing.T, resourceGroup string, cfg azureClusterConfig, nodeCount int) error {
	subnetID, err := azureSubnetID(resourceGroup, cfg.VNetName, cfg.SubnetName)
	if err != nil {
		return fmt.Errorf("get subnet ID for %s: %w", cfg.SubnetName, err)
	}

	maxCount := nodeCount + 1
	if maxCount < 2 {
		maxCount = 2
	}

	args := []string{
		"aks", "create",
		"--resource-group", resourceGroup,
		"--name", cfg.ClusterName,
		"--location", cfg.Region,
		"--subscription", azureSubscriptionID(),
		"--node-count", fmt.Sprint(nodeCount),
		"--node-vm-size", azureNodeVMSize(),
		"--network-plugin", "azure",
		"--vnet-subnet-id", subnetID,
		"--service-cidr", cfg.ServiceCIDR,
		"--dns-service-ip", cfg.DNSServiceIP,
		"--max-pods", fmt.Sprint(azureDefaultMaxPods),
		"--enable-cluster-autoscaler",
		"--min-count", fmt.Sprint(nodeCount),
		"--max-count", fmt.Sprint(maxCount),
		"--generate-ssh-keys",
		"--output", "none",
	}

	t.Logf("[%s] Running az %s", ProviderAzure, strings.Join(args, " "))
	var lastErr error
	for attempt := 1; attempt <= azureAKSCreateAttempts; attempt++ {
		cmd, cancel := azureCommandWithTimeout(azureAKSCreateTimeout, args...)
		out, err := cmd.CombinedOutput()
		cancel()
		if len(out) > 0 {
			t.Logf("[%s] az aks create %s output:\n%s", ProviderAzure, cfg.ClusterName, string(out))
		}
		if err == nil {
			return nil
		}

		lastErr = fmt.Errorf("az aks create %s: %w", cfg.ClusterName, err)
		if !isTransientAzureCLIError(err, out) {
			return lastErr
		}

		t.Logf("[%s] az aks create %s hit a transient Azure CLI transport error on attempt %d/%d: %v",
			ProviderAzure, cfg.ClusterName, attempt, azureAKSCreateAttempts, err)
		if waitErr := waitForAKSClusterReadyAfterCreateError(t, resourceGroup, cfg.ClusterName); waitErr == nil {
			t.Logf("[%s] AKS cluster %s reached Succeeded after transient create error",
				ProviderAzure, cfg.ClusterName)
			return nil
		} else {
			t.Logf("[%s] AKS cluster %s was not ready after transient create error: %v",
				ProviderAzure, cfg.ClusterName, waitErr)
		}

		if attempt < azureAKSCreateAttempts {
			time.Sleep(30 * time.Second)
		}
	}
	if lastErr != nil {
		return lastErr
	}
	return nil
}

func waitForAKSClusterReadyAfterCreateError(t *testing.T, resourceGroup, clusterName string) error {
	var lastErr error
	for attempt := 1; attempt <= azureAKSCreatePollAttempts; attempt++ {
		state, err := azureAKSProvisioningState(resourceGroup, clusterName)
		if err == nil {
			switch state {
			case "Succeeded":
				return nil
			case "Failed", "Canceled":
				return fmt.Errorf("AKS cluster %s provisioning state is %s", clusterName, state)
			case "":
				lastErr = fmt.Errorf("AKS cluster %s returned empty provisioning state", clusterName)
			default:
				lastErr = fmt.Errorf("AKS cluster %s provisioning state is %s", clusterName, state)
				t.Logf("[%s] Waiting for AKS cluster %s after transient create error; state=%s",
					ProviderAzure, clusterName, state)
			}
		} else {
			lastErr = err
			if isAzureResourceNotFoundError(err) {
				return err
			}
			t.Logf("[%s] Waiting for AKS cluster %s after transient create error; show failed: %v",
				ProviderAzure, clusterName, err)
		}

		time.Sleep(azureAKSCreatePollInterval)
	}
	if lastErr == nil {
		return fmt.Errorf("AKS cluster %s did not become visible", clusterName)
	}
	return lastErr
}

func azureAKSProvisioningState(resourceGroup, clusterName string) (string, error) {
	cmd, cancel := azureCommandWithTimeout(azureCLICommandTimeout, "aks", "show",
		"--resource-group", resourceGroup,
		"--name", clusterName,
		"--subscription", azureSubscriptionID(),
		"--query", "provisioningState",
		"--output", "tsv",
	)
	out, err := cmd.CombinedOutput()
	cancel()
	if err != nil {
		return "", fmt.Errorf("az aks show %s: %w: %s", clusterName, err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}

func isTransientAzureCLIError(err error, output []byte) bool {
	if err == nil {
		return false
	}
	errText := err.Error() + "\n" + string(output)
	for _, marker := range []string{
		"UNEXPECTED_EOF_WHILE_READING",
		"EOF occurred in violation of protocol",
		"Certificate verification failed",
		"Connection aborted",
		"connection reset",
		"context deadline exceeded",
		"timed out",
		"TLS/SSL connection has been closed",
		"unexpected EOF",
	} {
		if strings.Contains(errText, marker) {
			return true
		}
	}
	return false
}

func isAzureResourceNotFoundError(err error) bool {
	if err == nil {
		return false
	}
	errText := err.Error()
	return strings.Contains(errText, "ResourceNotFound") ||
		strings.Contains(errText, "could not be found") ||
		strings.Contains(errText, "was not found")
}

func (r *AzureRegion) setupVNetPeering(t *testing.T) error {
	if len(r.clusterConfigs) < 2 {
		return nil
	}

	var wg sync.WaitGroup
	errs := make(chan error, len(r.clusterConfigs)*len(r.clusterConfigs))

	for i := range r.clusterConfigs {
		for j := range r.clusterConfigs {
			if i == j {
				continue
			}
			sourceIndex, remoteIndex := i, j
			source := r.clusterConfigs[i]
			remote := r.clusterConfigs[j]
			wg.Add(1)
			go func() {
				defer wg.Done()
				remoteVNetID, err := azureVNetID(r.resourceGroupName, remote.VNetName)
				if err != nil {
					errs <- fmt.Errorf("get remote VNet ID for %s: %w", remote.VNetName, err)
					return
				}
				peeringName := fmt.Sprintf("peer-%d-to-%d", sourceIndex, remoteIndex)
				if err := createAzureVNetPeering(r.resourceGroupName, source.VNetName, peeringName, remoteVNetID); err != nil {
					errs <- fmt.Errorf("create VNet peering %s/%s: %w", source.VNetName, peeringName, err)
				}
			}()
		}
	}

	wg.Wait()
	close(errs)

	for err := range errs {
		if err != nil {
			return err
		}
	}
	t.Logf("[%s] VNet peering setup complete", ProviderAzure)
	return nil
}

func (r *AzureRegion) deployAndConfigureCoreDNS(t *testing.T, kubeConfigPath string) error {
	if len(r.Clusters) > 1 {
		for i, clusterName := range r.Clusters {
			kubectlOpts := k8s.NewKubectlOptions(clusterName, kubeConfigPath, coreDNSNamespace)
			if err := applyAzureCoreDNSService(t, kubectlOpts); err != nil {
				return fmt.Errorf("apply CoreDNS service for %s: %w", clusterName, err)
			}
			actualIPs, err := WaitForCoreDNSServiceIPs(t, kubectlOpts)
			if err != nil {
				return fmt.Errorf("get CoreDNS service IPs for %s: %w", clusterName, err)
			}
			r.CorednsClusterOptions[operator.CustomDomains[i]] = coredns.CoreDNSClusterOption{
				IPs:       actualIPs,
				Namespace: r.Namespace[clusterName],
				Domain:    operator.CustomDomains[i],
			}
		}

		UpdateCoreDNSConfiguration(t, r.Region, kubeConfigPath)
		return nil
	}

	for i, clusterName := range r.Clusters {
		kubectlOpts := k8s.NewKubectlOptions(clusterName, kubeConfigPath, coreDNSNamespace)
		if err := applyAzureCoreDNSCustom(t, kubectlOpts, operator.CustomDomains[i], r.CorednsClusterOptions); err != nil {
			return fmt.Errorf("apply coredns-custom for %s: %w", clusterName, err)
		}
		if err := restartAzureCoreDNS(t, kubectlOpts, clusterName); err != nil {
			return err
		}
	}

	return nil
}

func restartAzureCoreDNS(t *testing.T, kubectlOpts *k8s.KubectlOptions, clusterName string) error {
	if err := runAzureKubernetesActionWithRetry(t, fmt.Sprintf("restart AKS CoreDNS for %s", clusterName), func() error {
		return k8s.RunKubectlE(t, kubectlOpts, "rollout", "restart", "deployment", coreDNSDeploymentName)
	}); err != nil {
		return fmt.Errorf("restart AKS CoreDNS for %s: %w", clusterName, err)
	}
	if err := runAzureKubernetesActionWithRetry(t, fmt.Sprintf("wait for AKS CoreDNS rollout for %s", clusterName), func() error {
		return k8s.RunKubectlE(t, kubectlOpts, "rollout", "status", "deployment", coreDNSDeploymentName, "--timeout=3m")
	}); err != nil {
		return fmt.Errorf("wait for AKS CoreDNS rollout for %s: %w", clusterName, err)
	}
	return nil
}

func applyAzureCoreDNSCustom(
	t *testing.T, kubectlOpts *k8s.KubectlOptions, thisDomain string,
	allClusters map[string]coredns.CoreDNSClusterOption,
) error {
	cm := &corev1.ConfigMap{
		TypeMeta: metav1.TypeMeta{
			Kind:       "ConfigMap",
			APIVersion: "v1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "coredns-custom",
			Namespace: coreDNSNamespace,
		},
		Data: buildAzureCoreDNSCustomData(thisDomain, allClusters),
	}
	if err := kubectlApplyAzureManifest(t, kubectlOpts, coredns.ToYAML(t, cm)); err != nil {
		return fmt.Errorf("kubectl apply coredns-custom: %w", err)
	}
	return nil
}

func buildAzureCoreDNSCustomData(
	thisDomain string, allClusters map[string]coredns.CoreDNSClusterOption,
) map[string]string {
	data := map[string]string{}

	quotedDomain := regexp.QuoteMeta(thisDomain)
	data["rewrite.override"] = fmt.Sprintf(`rewrite continue {
    name regex ^(.+)\.%s\.?$ {1}.cluster.local
    answer name ^(.+)\.cluster\.local\.?$ {1}.%s
    answer value ^(.+)\.cluster\.local\.?$ {1}.%s
}`, quotedDomain, thisDomain, thisDomain)

	domains := make([]string, 0, len(allClusters))
	for domain := range allClusters {
		domains = append(domains, domain)
	}
	sort.Strings(domains)

	for _, clusterDomain := range domains {
		if clusterDomain == thisDomain {
			continue
		}
		cluster := allClusters[clusterDomain]
		if len(cluster.IPs) == 0 {
			continue
		}

		ips := append([]string{}, cluster.IPs...)
		sort.Strings(ips)
		ipList := strings.Join(ips, " ")
		serverBlock := func(host string) string {
			return fmt.Sprintf(`%s:53 {
    errors
    ready
    cache 30
    forward . %s {
        force_tcp
    }
}
`, host, ipList)
		}

		data[fmt.Sprintf("%s.server", clusterDomain)] = serverBlock(clusterDomain)
		if cluster.Namespace != "" {
			data[fmt.Sprintf("%s.svc.%s.server", cluster.Namespace, clusterDomain)] =
				serverBlock(fmt.Sprintf("%s.svc.%s", cluster.Namespace, clusterDomain))
			data[fmt.Sprintf("%s.pod.%s.server", cluster.Namespace, clusterDomain)] =
				serverBlock(fmt.Sprintf("%s.pod.%s", cluster.Namespace, clusterDomain))
		}
	}

	return data
}

func applyAzureCoreDNSService(t *testing.T, kubectlOpts *k8s.KubectlOptions) error {
	annotations := GetLoadBalancerAnnotations(ProviderAzure)
	annotationCopy := make(map[string]string, len(annotations))
	for key, value := range annotations {
		annotationCopy[key] = value
	}

	svc := &corev1.Service{
		TypeMeta: metav1.TypeMeta{
			Kind:       "Service",
			APIVersion: "v1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:        coreDNSServiceName,
			Namespace:   coreDNSNamespace,
			Annotations: annotationCopy,
		},
		Spec: corev1.ServiceSpec{
			Type: corev1.ServiceTypeLoadBalancer,
			Ports: []corev1.ServicePort{
				{
					Name:       "dns-tcp",
					Port:       53,
					Protocol:   corev1.ProtocolTCP,
					TargetPort: intstr.FromInt(53),
				},
			},
			Selector: detectAzureCoreDNSPodLabel(t, kubectlOpts),
		},
	}

	if err := kubectlApplyAzureManifest(t, kubectlOpts, coredns.ToYAML(t, svc)); err != nil {
		return fmt.Errorf("kubectl apply %s: %w", coreDNSServiceName, err)
	}
	return nil
}

func kubectlApplyAzureManifest(t *testing.T, kubectlOpts *k8s.KubectlOptions, manifest string) error {
	t.Helper()

	manifestFile := filepath.Join(t.TempDir(), "manifest.yaml")
	if err := os.WriteFile(manifestFile, []byte(manifest), 0600); err != nil {
		return err
	}

	return runAzureKubernetesActionWithRetry(t, "kubectl apply Azure manifest", func() error {
		return k8s.RunKubectlE(t, kubectlOpts, "apply", "--validate=false", "-f", manifestFile)
	})
}

func runAzureKubernetesActionWithRetry(t *testing.T, description string, action func() error) error {
	t.Helper()

	var err error
	for attempt := 1; attempt <= azureKubernetesAttempts; attempt++ {
		err = action()
		if err == nil {
			return nil
		}
		if !isTransientAzureKubernetesError(err) || attempt == azureKubernetesAttempts {
			return err
		}
		t.Logf("[%s] %s hit a transient Kubernetes API error on attempt %d/%d: %v",
			ProviderAzure, description, attempt, azureKubernetesAttempts, err)
		time.Sleep(azureKubernetesRetrySleep)
	}
	return err
}

func isTransientAzureKubernetesError(err error) bool {
	if err == nil {
		return false
	}
	errText := err.Error()
	for _, marker := range []string{
		"Bad Gateway",
		"Client.Timeout",
		"EOF",
		"failed to download openapi",
		"http2: client connection lost",
		"IncompleteCertChain",
		"net/http: request canceled",
		"request canceled",
		"SSL_INCOMPLETE_CHAIN",
		"TLS handshake timeout",
		"connection reset",
		"context deadline exceeded",
		"i/o timeout",
		"server was unable to return a response",
		"unexpected EOF",
	} {
		if strings.Contains(errText, marker) {
			return true
		}
	}
	return false
}

func detectAzureCoreDNSPodLabel(t *testing.T, kubectlOpts *k8s.KubectlOptions) map[string]string {
	for _, label := range []string{"kube-dns", "coredns"} {
		output, err := k8s.RunKubectlAndGetOutputE(t, kubectlOpts,
			"get", "pods", "-l", fmt.Sprintf("k8s-app=%s", label), "--no-headers", "-o", "name")
		if err != nil {
			t.Logf("[%s] warning: failed to detect AKS CoreDNS pods with k8s-app=%s: %v",
				ProviderAzure, label, err)
			continue
		}
		if strings.TrimSpace(output) != "" {
			t.Logf("[%s] Detected AKS CoreDNS pod label k8s-app=%s", ProviderAzure, label)
			return map[string]string{"k8s-app": label}
		}
	}
	t.Logf("[%s] warning: defaulting AKS CoreDNS selector to k8s-app=kube-dns", ProviderAzure)
	return map[string]string{"k8s-app": "kube-dns"}
}

func UpdateKubeconfigAzure(t *testing.T, resourceGroup, clusterName, alias string) error {
	args := []string{
		"aks", "get-credentials",
		"--resource-group", resourceGroup,
		"--name", clusterName,
		"--subscription", azureSubscriptionID(),
		"--context", alias,
		"--overwrite-existing",
	}
	if kubeconfig := os.Getenv("KUBECONFIG"); kubeconfig != "" {
		args = append(args, "--file", kubeconfig)
	}

	cmd, cancel := azureCommandWithTimeout(azureCLICommandTimeout, args...)
	defer cancel()
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Logf("[%s] az aks get-credentials output:\n%s", ProviderAzure, string(output))
		return fmt.Errorf("get credentials for AKS cluster %s: %w", clusterName, err)
	}

	if err := EnsureKubeconfigTLSVerificationDisabled(t, []string{alias, clusterName}); err != nil {
		return fmt.Errorf("disable kubeconfig TLS verification for %s: %w", clusterName, err)
	}
	if err := addKubeAPIServerToNoProxy(t, alias, clusterName); err != nil {
		return fmt.Errorf("add AKS API server to NO_PROXY for %s: %w", clusterName, err)
	}
	return nil
}

func addKubeAPIServerToNoProxy(t *testing.T, contextName, clusterName string) error {
	t.Helper()

	kubeConfigPath := strings.TrimSpace(os.Getenv("KUBECONFIG"))
	if kubeConfigPath == "" {
		var err error
		kubeConfigPath, err = k8s.KubeConfigPathFromHomeDirE()
		if err != nil {
			return fmt.Errorf("resolve kubeconfig path: %w", err)
		}
	}
	rawConfig, err := clientcmd.LoadFromFile(kubeConfigPath)
	if err != nil {
		return fmt.Errorf("load kubeconfig %s: %w", kubeConfigPath, err)
	}

	clusterRef := clusterName
	if context, ok := rawConfig.Contexts[contextName]; ok && context.Cluster != "" {
		clusterRef = context.Cluster
	}
	cluster, ok := rawConfig.Clusters[clusterRef]
	if !ok {
		cluster, ok = rawConfig.Clusters[contextName]
	}
	if !ok {
		cluster, ok = rawConfig.Clusters[clusterName]
	}
	if !ok {
		return fmt.Errorf("cluster entry not found for context %s", contextName)
	}

	serverURL, err := url.Parse(strings.TrimSpace(cluster.Server))
	if err != nil {
		return fmt.Errorf("parse AKS API server URL %q: %w", cluster.Server, err)
	}
	host := serverURL.Hostname()
	if host == "" {
		return fmt.Errorf("AKS API server URL %q has no host", cluster.Server)
	}

	addHostToNoProxy(host)
	t.Logf("[%s] Added AKS API server %s to NO_PROXY", ProviderAzure, host)
	return nil
}

func addHostToNoProxy(host string) {
	for _, envName := range []string{"NO_PROXY", "no_proxy"} {
		current := strings.TrimSpace(os.Getenv(envName))
		if current == "*" {
			continue
		}

		entries := make([]string, 0)
		found := false
		for _, entry := range strings.Split(current, ",") {
			entry = strings.TrimSpace(entry)
			if entry == "" {
				continue
			}
			if entry == host {
				found = true
			}
			entries = append(entries, entry)
		}
		if !found {
			entries = append(entries, host)
		}
		_ = os.Setenv(envName, strings.Join(entries, ","))
	}
}

func azureSubnetID(resourceGroup, vnetName, subnetName string) (string, error) {
	cmd, cancel := azureCommandWithTimeout(azureCLICommandTimeout, "network", "vnet", "subnet", "show",
		"--resource-group", resourceGroup,
		"--vnet-name", vnetName,
		"--name", subnetName,
		"--subscription", azureSubscriptionID(),
		"--query", "id",
		"--output", "tsv",
	)
	defer cancel()
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("az network vnet subnet show %s/%s: %w", vnetName, subnetName, err)
	}
	return strings.TrimSpace(string(out)), nil
}

func azureVNetID(resourceGroup, vnetName string) (string, error) {
	cmd, cancel := azureCommandWithTimeout(azureCLICommandTimeout, "network", "vnet", "show",
		"--resource-group", resourceGroup,
		"--name", vnetName,
		"--subscription", azureSubscriptionID(),
		"--query", "id",
		"--output", "tsv",
	)
	defer cancel()
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("az network vnet show %s: %w", vnetName, err)
	}
	return strings.TrimSpace(string(out)), nil
}

func createAzureVNetPeering(resourceGroup, vnetName, peeringName, remoteVNetID string) error {
	cmd, cancel := azureCommandWithTimeout(azureCLICommandTimeout, "network", "vnet", "peering", "create",
		"--resource-group", resourceGroup,
		"--name", peeringName,
		"--vnet-name", vnetName,
		"--remote-vnet", remoteVNetID,
		"--subscription", azureSubscriptionID(),
		"--allow-vnet-access",
		"--allow-forwarded-traffic",
		"--output", "none",
	)
	defer cancel()
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func clientForContext(contextName string) (client.Client, error) {
	cfg, err := config.GetConfigWithContext(contextName)
	if err != nil {
		return nil, fmt.Errorf("get REST config for %s: %w", contextName, err)
	}
	k8sClient, err := client.New(cfg, client.Options{})
	if err != nil {
		return nil, fmt.Errorf("create Kubernetes client for %s: %w", contextName, err)
	}
	return k8sClient, nil
}

func ensureAzureLogin(t *testing.T, subscriptionID string) error {
	clientID := strings.TrimSpace(os.Getenv(envAzureClientID))
	clientSecret := strings.TrimSpace(os.Getenv(envAzureClientSecret))
	tenantID := strings.TrimSpace(os.Getenv(envAzureTenantID))

	if clientID != "" || clientSecret != "" || tenantID != "" {
		if clientID == "" || clientSecret == "" || tenantID == "" {
			return fmt.Errorf("%s, %s, and %s must be set together for service principal auth",
				envAzureClientID, envAzureClientSecret, envAzureTenantID)
		}
		t.Logf("[%s] Authenticating az CLI with service principal in tenant %s", ProviderAzure, tenantID)
		cmd, cancel := azureCommandWithTimeout(azureCLICommandTimeout, "login",
			"--service-principal",
			"--username", clientID,
			"--password", clientSecret,
			"--tenant", tenantID,
		)
		output, err := cmd.CombinedOutput()
		cancel()
		if err != nil {
			t.Logf("[%s] az login output:\n%s", ProviderAzure, string(output))
			return fmt.Errorf("az login failed: %w", err)
		}
	} else {
		t.Logf("[%s] Using existing az CLI login session", ProviderAzure)
	}

	cmd, cancel := azureCommandWithTimeout(azureCLICommandTimeout, "account", "set",
		"--subscription", subscriptionID)
	out, err := cmd.CombinedOutput()
	cancel()
	if err != nil {
		t.Logf("[%s] az account set output:\n%s", ProviderAzure, string(out))
		return fmt.Errorf("az account set %s: %w", subscriptionID, err)
	}
	cmd, cancel = azureCommandWithTimeout(azureCLICommandTimeout, "account", "show",
		"--subscription", subscriptionID, "--output", "none")
	out, err = cmd.CombinedOutput()
	cancel()
	if err != nil {
		t.Logf("[%s] az account show output:\n%s", ProviderAzure, string(out))
		return fmt.Errorf("az account show %s: %w", subscriptionID, err)
	}
	return nil
}

func mustAzureEnv(t *testing.T, envName string) string {
	t.Helper()
	value := strings.TrimSpace(os.Getenv(envName))
	if value == "" {
		t.Fatalf("[%s] required environment variable %s is not set", ProviderAzure, envName)
	}
	return value
}

func azureSubscriptionID() string {
	return strings.TrimSpace(os.Getenv(envAzureSubscriptionID))
}

func azureResourcePrefix() string {
	if prefix := strings.TrimSpace(os.Getenv(envAzureResourcePrefix)); prefix != "" {
		return prefix
	}
	return azureDefaultResourcePrefix
}

func azureNodeVMSize() string {
	if size := strings.TrimSpace(os.Getenv(envAzureNodeVMSize)); size != "" {
		return size
	}
	return azureDefaultNodeVMSize
}

func azureReuseContexts(t *testing.T, clusterCount int) []string {
	t.Helper()
	reuseContexts := strings.TrimSpace(os.Getenv(envAzureReuseContexts))
	if reuseContexts == "" {
		return nil
	}
	contexts := filepath.SplitList(reuseContexts)
	if len(contexts) != clusterCount {
		t.Fatalf("[%s] %s must contain %d context names, got %d",
			ProviderAzure, envAzureReuseContexts, clusterCount, len(contexts))
	}
	return contexts
}

var _ CloudProvider = (*AzureRegion)(nil)
var _ encryption.Provider = (*AzureRegion)(nil)
