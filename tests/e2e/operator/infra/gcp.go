package infra

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gruntwork-io/terratest/modules/k8s"
	"github.com/gruntwork-io/terratest/modules/random"
	"github.com/stretchr/testify/require"
	cloudkms "google.golang.org/api/cloudkms/v1"
	"google.golang.org/api/compute/v1"
	"google.golang.org/api/container/v1"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"

	"github.com/cockroachdb/helm-charts/tests/e2e/coredns"
	"github.com/cockroachdb/helm-charts/tests/e2e/operator"
	"github.com/cockroachdb/helm-charts/tests/e2e/operator/encryption"
)

// ─── GCP CONSTANTS ───────────────────────────────────────────────────────────────

const (
	vpcName                  = "cockroachdb-vpc"
	defaultNodeTag           = "cockroachdb-node"
	autoprovisioningNodeTag  = "cockroach-node"
	webhookFirewallRuleName  = "allow-9443-port-for-webhook"
	internalFirewallRuleName = "allow-internal"
	subnetSuffix             = "subnet" // suffix for dynamically created subnets.

	gcpCreds              = "GOOGLE_APPLICATION_CREDENTIALS"
	githubActions         = "GITHUB_ACTIONS"
	serviceAccountKeyPath = "" // optional path to service-account JSON; if empty, ADC is used.
)

// Default project ID to use if not specified in the environment.
const defaultProjectID = "helm-testing"

func getNodeServiceAccount() string {
	return os.Getenv("GCP_NODE_SERVICE_ACCOUNT")
}

const (
	gcpKMSKeyRingID   = "cockroachdb-cmek-test"
	gcpKMSCryptoKeyID = "cockroachdb-cmek-key"
)

// getProjectID returns the GCP project ID from the environment variable or falls back to default.
func getProjectID() string {
	if projectID := os.Getenv("GCP_PROJECT_ID"); projectID != "" {
		return projectID
	}
	return defaultProjectID
}

// getGCPNetworkConfig retrieves a network configuration value for a region
func getGCPNetworkConfig(region, key, defaultValue string) string {
	if config, ok := NetworkConfigs[ProviderGCP][region]; ok {
		if value, ok := config.(map[string]string)[key]; ok {
			return value
		}
	}
	return defaultValue
}

func getGCPClusterCIDR(region string) string {
	return getGCPNetworkConfig(region, "ClusterCIDR", "172.28.0.0/20")
}

func getGCPServiceCIDR(region string) string {
	return getGCPNetworkConfig(region, "ServiceCIDR", "172.28.17.0/24")
}

func getGCPSubnetRange(region string) string {
	return getGCPNetworkConfig(region, "SubnetRange", "172.28.16.0/24")
}

func getGCPStaticIP(region string) string {
	return getGCPNetworkConfig(region, "StaticIP", "172.28.16.11")
}

// ClusterSetupConfig holds per‐cluster network & GKE settings.
type ClusterSetupConfig struct {
	Region          string
	SubnetRange     string
	SubnetName      string
	StaticIPAddress string // desired "reserved" IP for CoreDNS LB
	UseDynamicIP    bool   // if true, don't reserve static IP, let LoadBalancer allocate dynamically
	ClusterName     string
	ClusterIPV4CIDR string
	ServiceIPV4CIDR string
	NodeZones       []string // populated dynamically
}

// Pre‐defined configs for each region; index order matches r.Clusters.
var clusterConfigurations = []ClusterSetupConfig{
	{
		Region:          "us-central1",
		SubnetRange:     getGCPSubnetRange("us-central1"),
		StaticIPAddress: getGCPStaticIP("us-central1"),
		ClusterName:     "cockroachdb-central",
		ClusterIPV4CIDR: getGCPClusterCIDR("us-central1"),
		ServiceIPV4CIDR: getGCPServiceCIDR("us-central1"),
		UseDynamicIP:    true,
	},
	{
		Region:          "us-east1",
		SubnetRange:     getGCPSubnetRange("us-east1"),
		StaticIPAddress: getGCPStaticIP("us-east1"),
		ClusterName:     "cockroachdb-east",
		ClusterIPV4CIDR: getGCPClusterCIDR("us-east1"),
		ServiceIPV4CIDR: getGCPServiceCIDR("us-east1"),
		UseDynamicIP:    true,
	},
	{
		Region:          "us-west1",
		SubnetRange:     getGCPSubnetRange("us-west1"),
		StaticIPAddress: getGCPStaticIP("us-west1"),
		ClusterName:     "cockroachdb-west",
		ClusterIPV4CIDR: getGCPClusterCIDR("us-west1"),
		ServiceIPV4CIDR: getGCPServiceCIDR("us-west1"),
		UseDynamicIP:    true,
	},
}

// GcpRegion wraps operator.Region and manages GCP infra.
type GcpRegion struct {
	*operator.Region
	vpcName   string
	kmsKeyURI string
}

// SetUpInfra creates VPC, subnet, static IP, firewall rules, GKE clusters, and deploys CoreDNS
func (r *GcpRegion) SetUpInfra(t *testing.T) {
	if r.ReusingInfra {
		t.Logf("[%s] Reusing existing infrastructure", ProviderGCP)
		return
	}

	// 1) Initialize clients
	ctx := context.Background()

	gkeService, err := createGKEServiceClient(ctx)
	require.NoError(t, err, "failed to create GKE client")

	computeService, err := createComputeServiceClient(ctx)
	require.NoError(t, err, "failed to create Compute client")

	// 2) Create or reuse VPC (custom mode).
	r.vpcName = fmt.Sprintf("%s-%s", vpcName, strings.ToLower(random.UniqueId()))
	networkOp, err := createVPC(ctx, computeService, getProjectID(), r.vpcName)
	if err != nil && !IsResourceConflict(err) {
		t.Fatalf("VPC creation failed: %v", err)
	}
	if networkOp != nil {
		require.NoError(t, waitForGlobalComputeOperation(ctx, computeService, getProjectID(), networkOp.Name))
	}
	vpcSelfLink := fmt.Sprintf("projects/%s/global/networks/%s", getProjectID(), r.vpcName)

	// 3) Create subnets.
	var allSubnetRanges []string
	var allClusterIPV4CIDRs []string

	for i, clusterName := range r.Clusters {
		setup := &clusterConfigurations[i]
		setup.ClusterName = clusterName
		subnetName := fmt.Sprintf("%s-%s", clusterName, subnetSuffix)
		setup.SubnetName = subnetName
		// 3a) Create or reuse subnet
		subnetOp, err := createSubnet(ctx, computeService, getProjectID(), setup.Region, vpcSelfLink, subnetName, setup.SubnetRange)
		if err != nil && !IsResourceConflict(err) {
			t.Fatalf("subnet %s creation failed: %v", subnetName, err)
		}
		if subnetOp != nil {
			require.NoError(t, waitForRegionalComputeOperation(ctx, computeService, getProjectID(), setup.Region, subnetOp.Name))
		}
		allSubnetRanges = append(allSubnetRanges, setup.SubnetRange)
		allClusterIPV4CIDRs = append(allClusterIPV4CIDRs, setup.ClusterIPV4CIDR)

	}

	// 4) Create or reuse static IP addresses (if not using dynamic allocation).
	for i := range r.Clusters {
		setup := &clusterConfigurations[i]
		if setup.UseDynamicIP {
			// Skip static IP reservation - LoadBalancer will allocate dynamically
			t.Logf("Using dynamic IP allocation for cluster %s in region %s", setup.ClusterName, setup.Region)
			setup.StaticIPAddress = "" // Clear any predefined IP
			continue
		}

		// For parallel test isolation, append VPC name (which contains unique ID) to address name
		addressName := fmt.Sprintf("coredns-loadbalancer-%s-%s", setup.Region, r.vpcName)
		subnetSelfLink := fmt.Sprintf("projects/%s/regions/%s/subnetworks/%s", getProjectID(), setup.Region, setup.SubnetName)
		actualIP, err := ensureStaticIPAddress(ctx, computeService, getProjectID(), setup.Region, addressName, setup.StaticIPAddress, subnetSelfLink)
		require.NoError(t, err, "failed to ensure static IP")
		// Store the actual reserved IP address for later reference in CoreDNS service creation.
		setup.StaticIPAddress = actualIP
	}

	// 5) Create firewall rules (IDEMPOTENT)
	// Firewalls must be unique to a project, so create with unique names to avoid conflicts between parallel CI runs
	webhookFirewallName := fmt.Sprintf("%s-%s", webhookFirewallRuleName, r.vpcName)
	internalFirewallName := fmt.Sprintf("%s-%s", internalFirewallRuleName, r.vpcName)

	// 5a) Allow port 9443
	allow9443 := []*compute.FirewallAllowed{{IPProtocol: "tcp", Ports: []string{"9443"}}}
	_, err = createFirewallRuleIfNotExists(ctx, computeService, getProjectID(), vpcSelfLink, webhookFirewallName, allow9443, []string{defaultNodeTag}, allSubnetRanges)
	require.NoError(t, err, "failed to create webhook firewall rule")

	// 5b) Allow internal (TCP,UDP,ICMP)
	internalAllow := []*compute.FirewallAllowed{
		{IPProtocol: "tcp"}, {IPProtocol: "udp"}, {IPProtocol: "icmp"},
	}
	// Combine subnet ranges and cluster CIDRs
	allSources := make([]string, 0, len(allSubnetRanges)+len(allClusterIPV4CIDRs))
	allSources = append(allSources, allSubnetRanges...)
	allSources = append(allSources, allClusterIPV4CIDRs...)
	_, err = createFirewallRuleIfNotExists(ctx, computeService, getProjectID(), vpcSelfLink, internalFirewallName, internalAllow, []string{defaultNodeTag}, allSources)
	require.NoError(t, err, "failed to create internal firewall rule")

	// 6) Create GKE clusters in parallel.
	err = r.createGKEClusters(ctx, t, gkeService, computeService, vpcSelfLink)
	require.NoError(t, err, "failed to create GKE clusters")
	r.ReusingInfra = true

	// 7) Deploy CoreDNS with initial configuration, then update with complete cross-cluster info.
	kubeConfigPath, err := r.EnsureKubeConfigPath()
	require.NoError(t, err)
	err = r.deployAndConfigureCoreDNS(t, kubeConfigPath)
	require.NoError(t, err, "failed to deploy and configure CoreDNS")
}

func (r *GcpRegion) GetEncryptionProvider() encryption.Provider {
	return r
}

func (r *GcpRegion) SetupEncryptionInfrastructure(t *testing.T) (func(), error) {
	ctx := context.Background()
	svc, err := cloudkms.NewService(ctx, gcpKMSCredentialOptions()...)
	if err != nil {
		return nil, fmt.Errorf("failed to create GCP KMS client: %w", err)
	}

	project := getProjectID()
	location := gcpKMSLocation()
	parent := fmt.Sprintf("projects/%s/locations/%s", project, location)

	t.Logf("GCP provider: ensuring KMS key ring %s/%s", parent, gcpKMSKeyRingID)
	_, err = svc.Projects.Locations.KeyRings.Create(parent, &cloudkms.KeyRing{}).
		KeyRingId(gcpKMSKeyRingID).Context(ctx).Do()
	if err != nil && !IsResourceConflict(err) {
		return nil, fmt.Errorf("failed to create GCP KMS key ring: %w", err)
	}

	keyRingName := fmt.Sprintf("%s/keyRings/%s", parent, gcpKMSKeyRingID)

	t.Logf("GCP provider: ensuring KMS crypto key %s/%s", keyRingName, gcpKMSCryptoKeyID)
	_, err = svc.Projects.Locations.KeyRings.CryptoKeys.Create(keyRingName, &cloudkms.CryptoKey{
		Purpose: "ENCRYPT_DECRYPT",
		VersionTemplate: &cloudkms.CryptoKeyVersionTemplate{
			Algorithm: "GOOGLE_SYMMETRIC_ENCRYPTION",
		},
	}).CryptoKeyId(gcpKMSCryptoKeyID).Context(ctx).Do()
	if err != nil && !IsResourceConflict(err) {
		return nil, fmt.Errorf("failed to create GCP KMS crypto key: %w", err)
	}

	r.kmsKeyURI = fmt.Sprintf("%s/cryptoKeys/%s", keyRingName, gcpKMSCryptoKeyID)
	t.Logf("GCP provider: KMS crypto key ready at %s", r.kmsKeyURI)

	return func() {
		t.Logf("GCP provider: KMS key %s retained (GCP keys cannot be deleted)", r.kmsKeyURI)
	}, nil
}

func (r *GcpRegion) GetEncryptionPlatformConfig() *encryption.PlatformConfig {
	return &encryption.PlatformConfig{
		Platform:                     "GCP_CLOUD_KMS",
		RequiresCredentialsSecret:    true,
		DefaultCredentialsSecretName: "cmek-gcp-credentials",
	}
}

func (r *GcpRegion) EncryptKey(plaintextKey []byte, clusterRegion string) (string, error) {
	if r.kmsKeyURI == "" {
		return "", fmt.Errorf("GCP KMS key URI not set: call SetupEncryptionInfrastructure first")
	}

	ctx := context.Background()
	svc, err := cloudkms.NewService(ctx, gcpKMSCredentialOptions()...)
	if err != nil {
		return "", fmt.Errorf("failed to create GCP KMS client: %w", err)
	}

	req := &cloudkms.EncryptRequest{
		Plaintext: base64.StdEncoding.EncodeToString(plaintextKey),
	}
	resp, err := svc.Projects.Locations.KeyRings.CryptoKeys.Encrypt(r.kmsKeyURI, req).Context(ctx).Do()
	if err != nil {
		return "", fmt.Errorf("GCP KMS encrypt (%s): %w", r.kmsKeyURI, err)
	}
	return resp.Ciphertext, nil
}

func (r *GcpRegion) CreateKeySecret(kubectlOptions *k8s.KubectlOptions, secretName string, encryptedKeyData string, clusterRegion string) error {
	if r.kmsKeyURI == "" {
		return fmt.Errorf("GCP KMS key URI not set: call SetupEncryptionInfrastructure first")
	}
	authPrincipal := os.Getenv("GCP_KMS_AUTH_PRINCIPAL")
	if authPrincipal == "" {
		return fmt.Errorf("GCP_KMS_AUTH_PRINCIPAL environment variable not set")
	}
	kmsRegion := gcpKMSLocation()

	return gcpRunKubectl(kubectlOptions,
		"create", "secret", "generic", secretName,
		fmt.Sprintf("--from-literal=StoreKeyData=%s", encryptedKeyData),
		fmt.Sprintf("--from-literal=URI=%s", r.kmsKeyURI),
		fmt.Sprintf("--from-literal=AuthPrincipal=%s", authPrincipal),
		fmt.Sprintf("--from-literal=Region=%s", kmsRegion),
		"--from-literal=Type=GCP_CLOUD_KMS",
	)
}

func (r *GcpRegion) CreateCredentialsSecret(kubectlOptions *k8s.KubectlOptions) (string, error) {
	const credSecretName = "cmek-gcp-credentials"

	saKeyPath := os.Getenv("GCP_KMS_SA_KEY_PATH")
	if saKeyPath == "" {
		saKeyPath = getServiceAccountKeyPath()
	}
	if saKeyPath == "" {
		return "", fmt.Errorf("neither GCP_KMS_SA_KEY_PATH nor GOOGLE_APPLICATION_CREDENTIALS is set")
	}

	saKeyBytes, err := os.ReadFile(saKeyPath)
	if err != nil {
		return "", fmt.Errorf("failed to read GCP SA key file %s: %w", saKeyPath, err)
	}

	if err := gcpRunKubectl(kubectlOptions,
		"create", "secret", "generic", credSecretName,
		fmt.Sprintf("--from-literal=gcp_service_account_key=%s", base64.StdEncoding.EncodeToString(saKeyBytes)),
	); err != nil {
		return "", fmt.Errorf("failed to create GCP credentials secret: %w", err)
	}
	return credSecretName, nil
}

func gcpKMSLocation() string {
	if loc := os.Getenv("GCP_KMS_REGION"); loc != "" {
		return loc
	}
	return "us-central1"
}

func gcpKMSCredentialOptions() []option.ClientOption {
	if path := os.Getenv("GCP_KMS_SA_KEY_PATH"); path != "" {
		return []option.ClientOption{option.WithCredentialsFile(path)}
	}
	if path := getServiceAccountKeyPath(); path != "" {
		return []option.ClientOption{option.WithCredentialsFile(path)}
	}
	return nil
}

func gcpRunKubectl(kubectlOptions *k8s.KubectlOptions, args ...string) error {
	var cmdArgs []string
	if kubectlOptions.ContextName != "" {
		cmdArgs = append(cmdArgs, "--context", kubectlOptions.ContextName)
	}
	if kubectlOptions.ConfigPath != "" {
		cmdArgs = append(cmdArgs, "--kubeconfig", kubectlOptions.ConfigPath)
	}
	if kubectlOptions.Namespace != "" {
		cmdArgs = append(cmdArgs, "-n", kubectlOptions.Namespace)
	}
	cmdArgs = append(cmdArgs, args...)

	out, err := exec.Command("kubectl", cmdArgs...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("kubectl %s: %w\nOutput: %s", strings.Join(args, " "), err, string(out))
	}
	return nil
}

// TeardownInfra deletes all GCP resources created by SetUpInfra.
func (r *GcpRegion) TeardownInfra(t *testing.T) {
	if r.vpcName == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Minute)
	defer cancel()
	env := append(os.Environ(), "GCP_PROJECT_ID="+getProjectID(), "CLEANUP_DRY_RUN=false", "GCP_CLEANUP_REUSE_CONTEXTS=true")
	if os.Getenv(githubActions) == "true" {
		env = append(env, "GCP_CLEANUP_OWNER_RUN_ID="+os.Getenv("GITHUB_RUN_ID"))
	}
	if err := runCloudCleanup(ctx, env, "gcp.sh", "network", r.vpcName); err != nil {
		t.Errorf("[%s] infrastructure cleanup failed: %v", ProviderGCP, err)
	}
}

// ScaleNodePool scales the node pool for a GKE cluster.
func (r *GcpRegion) ScaleNodePool(t *testing.T, location string, nodeCount, index int) {
	t.Logf("[%s] Node pool scaling is not yet implemented for GCP", ProviderGCP)
	// TODO: Implement node pool scaling for GKE clusters
}

func (r *GcpRegion) CanScale() bool {
	return false
}

// createGKEClusters handles the complex logic of creating GKE clusters in parallel.
func (r *GcpRegion) createGKEClusters(ctx context.Context, t *testing.T, gkeService *container.Service, computeService *compute.Service, vpcSelfLink string) error {
	var clients = make(map[string]client.Client)
	r.CorednsClusterOptions = make(map[string]coredns.CoreDNSClusterOption)

	// Create all clusters in parallel
	type clusterResult struct {
		index  int
		client client.Client
		err    error
	}

	resultsChan := make(chan clusterResult, len(r.Clusters))

	for i, clusterName := range r.Clusters {
		go func(idx int, name string) {
			defer func() {
				if r := recover(); r != nil {
					resultsChan <- clusterResult{index: idx, err: fmt.Errorf("panic during cluster creation: %v", r)}
				}
			}()

			cfg := clusterConfigurations[idx]
			t.Logf("[%s] Starting parallel creation of GKE cluster '%s' in region %s", ProviderGCP, name, cfg.Region)

			// Discover zones for this region
			zones, err := discoverNodeLocations(ctx, computeService, getProjectID(), cfg.Region)
			if err != nil {
				resultsChan <- clusterResult{index: idx, err: fmt.Errorf("failed to discover zones: %w", err)}
				return
			}
			// Ensure we don't request more zones than available
			maxZones := len(zones)
			if r.NodeCount > maxZones {
				t.Logf("[%s] Requested %d zones but only %d available, using all available zones", ProviderGCP, r.NodeCount, maxZones)
				cfg.NodeZones = zones
			} else {
				cfg.NodeZones = zones[:r.NodeCount]
			}

			// Create the GKE cluster (regional)
			err = createGKERegionalCluster(ctx, gkeService, cfg, vpcSelfLink)
			if err != nil {
				resultsChan <- clusterResult{index: idx, err: fmt.Errorf("failed to create cluster: %w", err)}
				return
			}

			// Fetch credentials via gcloud (aliases context to cluster name)
			err = UpdateKubeconfigGCP(t, getProjectID(), cfg.Region, name, name)
			if err != nil {
				resultsChan <- clusterResult{index: idx, err: fmt.Errorf("failed to update kubeconfig: %w", err)}
				return
			}

			// Create a client for this context
			cfgRest, err := config.GetConfigWithContext(name)
			if err != nil {
				resultsChan <- clusterResult{index: idx, err: fmt.Errorf("failed to get config: %w", err)}
				return
			}
			k8sClient, err := client.New(cfgRest, client.Options{})
			if err != nil {
				resultsChan <- clusterResult{index: idx, err: fmt.Errorf("failed to create client: %w", err)}
				return
			}

			t.Logf("[%s] Successfully created GKE cluster '%s'", ProviderGCP, name)
			resultsChan <- clusterResult{index: idx, client: k8sClient, err: nil}
		}(i, clusterName)
	}

	// Wait for all cluster creations to complete
	for range r.Clusters {
		result := <-resultsChan
		if result.err != nil {
			return fmt.Errorf("failed to create cluster at index %d: %w", result.index, result.err)
		}
		clients[r.Clusters[result.index]] = result.client

		// Store CoreDNS options with initial IPs (placeholder for dynamic allocation)
		initialIP := clusterConfigurations[result.index].StaticIPAddress
		if initialIP == "" {
			// Use placeholder IP for dynamic allocation - will be updated with actual IP later
			initialIP = "127.0.0.1"
		}
		r.CorednsClusterOptions[operator.CustomDomains[result.index]] = coredns.CoreDNSClusterOption{
			IPs:       []string{initialIP},
			Namespace: r.Namespace[r.Clusters[result.index]],
			Domain:    operator.CustomDomains[result.index],
		}
	}

	r.Clients = clients
	t.Logf("[%s] All GKE clusters created successfully", ProviderGCP)

	return nil
}

// deployAndConfigureCoreDNS handles the deployment and configuration of CoreDNS across all clusters
func (r *GcpRegion) deployAndConfigureCoreDNS(t *testing.T, kubeConfigPath string) error {
	// Deploy CoreDNS with initial configuration
	for i, clusterName := range r.Clusters {
		staticIP := clusterConfigurations[i].StaticIPAddress

		// Deploy CoreDNS with initial (possibly incomplete) cluster options
		err := DeployCoreDNS(t, clusterName, kubeConfigPath, &staticIP, ProviderGCP, operator.CustomDomains[i], r.CorednsClusterOptions)
		if err != nil {
			return fmt.Errorf("failed to deploy CoreDNS to cluster %s: %w", clusterName, err)
		}

		// Wait for the CoreDNS service to get the static IP
		kubectlOpts := k8s.NewKubectlOptions(clusterName, kubeConfigPath, coreDNSNamespace)
		actualIPs, err := WaitForCoreDNSServiceIPs(t, kubectlOpts)
		if err != nil {
			return fmt.Errorf("failed to get CoreDNS service IPs for cluster %s: %w", clusterName, err)
		}

		// Update the StaticIPAddress in clusterConfigurations with the actual LoadBalancer IP
		if len(actualIPs) > 0 {
			clusterConfigurations[i].StaticIPAddress = actualIPs[0]
		}

		// Update the cluster options with the actual assigned IPs
		r.CorednsClusterOptions[operator.CustomDomains[i]] = coredns.CoreDNSClusterOption{
			IPs:       actualIPs,
			Namespace: r.Namespace[clusterName],
			Domain:    operator.CustomDomains[i],
		}
	}

	// Update CoreDNS configuration in all clusters with complete cross-cluster information
	UpdateCoreDNSConfiguration(t, r.Region, kubeConfigPath)

	return nil
}

// getServiceAccountKeyPath returns the path to the service account key file.
func getServiceAccountKeyPath() string {
	if serviceAccountKeyPath != "" {
		return serviceAccountKeyPath
	}
	if path := os.Getenv(gcpCreds); path != "" {
		return path
	}
	if os.Getenv(githubActions) == "true" {
		return "/tmp/gcp-key.json"
	}
	return ""
}

// ─── GCP CLIENT CREATORS ────────────────────────────────────────────────────────

func createGKEServiceClient(ctx context.Context) (*container.Service, error) {
	var opts []option.ClientOption
	if keyPath := getServiceAccountKeyPath(); keyPath != "" {
		opts = append(opts, option.WithCredentialsFile(keyPath))
	}
	return container.NewService(ctx, opts...)
}

func createComputeServiceClient(ctx context.Context) (*compute.Service, error) {
	var opts []option.ClientOption
	if keyPath := getServiceAccountKeyPath(); keyPath != "" {
		opts = append(opts, option.WithCredentialsFile(keyPath))
	}
	return compute.NewService(ctx, opts...)
}

// ─── RESOURCE CREATION ──────────────────────────────────────────────────────────

func gcpCleanupMetadata(provider string) (map[string]string, error) {
	if os.Getenv(githubActions) != "true" {
		return nil, nil
	}
	runID := os.Getenv("GITHUB_RUN_ID")
	if _, err := strconv.ParseUint(runID, 10, 64); err != nil || os.Getenv("GITHUB_REPOSITORY") == "" || os.Getenv("GCP_PROJECT_ID") == "" {
		return nil, fmt.Errorf("CI provisioning requires GITHUB_RUN_ID, GITHUB_REPOSITORY and GCP_PROJECT_ID")
	}
	return map[string]string{
		"managed_by": "helm-charts-e2e",
		"repository": os.Getenv("GITHUB_REPOSITORY"),
		"run_id":     runID,
		"created_at": strconv.FormatInt(time.Now().UTC().Unix(), 10),
		"provider":   provider,
		"project":    getProjectID(),
	}, nil
}

func createVPC(ctx context.Context, client *compute.Service, projectID, vpcName string) (*compute.Operation, error) {
	metadata, err := gcpCleanupMetadata(ProviderGCP)
	if err != nil {
		return nil, err
	}
	network := &compute.Network{
		Name:                  vpcName,
		AutoCreateSubnetworks: false, // This makes it a custom mode network
		RoutingConfig: &compute.NetworkRoutingConfig{
			RoutingMode: "REGIONAL",
		},
		ForceSendFields: []string{"AutoCreateSubnetworks"},
	}
	if metadata != nil {
		data, err := json.Marshal(metadata)
		if err != nil {
			return nil, err
		}
		network.Description = string(data)
	}
	return client.Networks.Insert(projectID, network).Context(ctx).Do()
}

func createSubnet(ctx context.Context, client *compute.Service, projectID, region, networkSelfLink, subnetName, ipCidrRange string) (*compute.Operation, error) {
	subnet := &compute.Subnetwork{
		Name:        subnetName,
		Network:     networkSelfLink,
		IpCidrRange: ipCidrRange,
		Region:      region,
	}
	return client.Subnetworks.Insert(projectID, region, subnet).Context(ctx).Do()
}

func createFirewallRule(ctx context.Context, client *compute.Service, projectID, networkSelfLink, name string,
	allowed []*compute.FirewallAllowed, targetTags, sourceRanges []string) (*compute.Operation, error) {

	fw := &compute.Firewall{
		Name:         name,
		Network:      networkSelfLink,
		Allowed:      allowed,
		TargetTags:   targetTags,
		SourceRanges: sourceRanges,
		Direction:    "INGRESS",
	}
	return client.Firewalls.Insert(projectID, fw).Context(ctx).Do()
}

// createFirewallRuleIfNotExists checks for existing, else calls createFirewallRule.
func createFirewallRuleIfNotExists(ctx context.Context, client *compute.Service, projectID, networkSelfLink, name string,
	allowed []*compute.FirewallAllowed, targetTags, sourceRanges []string) (*compute.Operation, error) {

	_, err := client.Firewalls.Get(projectID, name).Context(ctx).Do()
	if err == nil {
		return nil, nil // already exists
	}
	if !IsResourceNotFound(err) {
		return nil, err
	}
	return createFirewallRule(ctx, client, projectID, networkSelfLink, name, allowed, targetTags, sourceRanges)
}

// ensureStaticIPAddress tries to find an existing address by name, else reserves one.
func ensureStaticIPAddress(ctx context.Context, client *compute.Service, projectID, region, addressName, addressIP, subnetSelfLink string) (string, error) {
	// 1) Check if the addressName already exists.
	addr, err := client.Addresses.Get(projectID, region, addressName).Context(ctx).Do()
	if err == nil {
		// Already exists; return the actual IP
		return addr.Address, nil
	}
	if !IsResourceNotFound(err) {
		return "", err
	}

	// 2) Create a new reserved internal IP.
	address := &compute.Address{
		Name:        addressName,
		Address:     addressIP,
		AddressType: "INTERNAL",
		Purpose:     "GCE_ENDPOINT",
		Subnetwork:  subnetSelfLink,
		Region:      region,
	}
	op, err := client.Addresses.Insert(projectID, region, address).Context(ctx).Do()
	if err != nil {
		// If "alreadyExists" or "IP_ADDRESS_IN_USE", treat as ok and return addressIP
		if IsResourceConflict(err) || strings.Contains(err.Error(), "IP_ADDRESS_IN_USE") {
			return addressIP, nil
		}
		return "", err
	}
	return addressIP, waitForRegionalComputeOperation(ctx, client, projectID, region, op.Name)
}

// discoverNodeLocations lists zones in a given region.
func discoverNodeLocations(ctx context.Context, client *compute.Service, projectID, region string) ([]string, error) {
	reg, err := client.Regions.Get(projectID, region).Context(ctx).Do()
	if err != nil {
		return nil, err
	}
	var zones []string
	for _, zoneURL := range reg.Zones {
		parts := strings.Split(zoneURL, "/")
		zones = append(zones, parts[len(parts)-1])
	}
	if len(zones) == 0 {
		return nil, fmt.Errorf("no zones found for region %s", region)
	}
	return zones, nil
}

func createGKERegionalCluster(ctx context.Context, client *container.Service, setup ClusterSetupConfig,
	vpcSelfLink string) error {

	// Construct the full cluster name path for the Get request.
	clusterPath := fmt.Sprintf("projects/%s/locations/%s/clusters/%s", getProjectID(), setup.Region, setup.ClusterName)

	// See if the cluster already exists.
	_, err := client.Projects.Locations.Clusters.Get(clusterPath).Context(ctx).Do()
	if err == nil {
		// No error means the cluster was found and already exists.
		// Return nil, nil to indicate no operation was started and there was no error.
		return nil
	}

	// If there was an error, check if it was a "not found" (404) error.
	// If it's anything other than a 404, it's a real problem, so return the error.
	var gerr *googleapi.Error
	if !errors.As(err, &gerr) || gerr.Code != http.StatusNotFound {
		return fmt.Errorf("failed to get cluster details for '%s': %w", setup.ClusterName, err)
	}

	// If we get here, it means the error was a 404, so the cluster does not exist. Proceed with creation.
	fmt.Printf("Cluster '%s' not found, proceeding with creation...\n", setup.ClusterName)

	subnetSelfLink := fmt.Sprintf("projects/%s/regions/%s/subnetworks/%s", getProjectID(), setup.Region, setup.SubnetName)

	// Construct the arguments slice for the gcloud command
	args := []string{
		"container", "clusters", "create", setup.ClusterName,
		"--region", setup.Region,
		"--project", getProjectID(),
		"--network", vpcSelfLink,
		"--subnetwork", subnetSelfLink,
		"--cluster-ipv4-cidr", setup.ClusterIPV4CIDR,
		"--services-ipv4-cidr", setup.ServiceIPV4CIDR,
		"--enable-ip-alias",
		"--tags", strings.Join([]string{defaultNodeTag}, ","), // Join tags if there are multiple
		"--enable-master-authorized-networks",
		"--master-authorized-networks", strings.Join([]string{"0.0.0.0/0"}, ","),
		"--num-nodes", fmt.Sprint(defaultNodesPerZone),
		"--min-nodes", fmt.Sprint(defaultNodesPerZone),
		"--max-nodes", fmt.Sprint(defaultNodesPerZone + 1), // Needed for scaling cluster
		"--enable-autoscaling", // Enable autoscaling
		"--autoprovisioning-network-tags", strings.Join([]string{autoprovisioningNodeTag}, ","),
		"--machine-type", gcpDefaultMachineType,
		"--disk-size", "30GB", // Limit disk size to 30GB
		"--quiet", // Suppress interactive prompts
	}

	metadata, err := gcpCleanupMetadata(ProviderGCP)
	if err != nil {
		return err
	}
	if metadata != nil {
		labels := fmt.Sprintf("managed-by=helm-charts-e2e,github-run-id=%s,created-at=%s", metadata["run_id"], metadata["created_at"])
		args = append(args, "--labels", labels)
	}
	if sa := getNodeServiceAccount(); sa != "" {
		args = append(args, "--service-account", sa)
	}

	cmd := exec.Command("gcloud", args...)

	// Stream gcloud's stdout/stderr directly for real-time visibility into the long-running creation process.
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	fmt.Printf("Running command: gcloud %s\n", strings.Join(args, " "))

	// Run the command. This will block until the gcloud process completes.
	if err = cmd.Run(); err != nil {
		return fmt.Errorf("gcloud create command failed for cluster '%s': %w", setup.ClusterName, err)
	}

	return nil
}

// ─── WAIT‐FOR‐OPERATION HELPERS ──────────────────────────────────────────────────

func waitForGlobalComputeOperation(ctx context.Context, client *compute.Service, projectID, opName string) error {
	return waitForComputeOperation(ctx, func(ctx context.Context) (*compute.Operation, error) {
		return client.GlobalOperations.Get(projectID, opName).Context(ctx).Do()
	})
}

func waitForRegionalComputeOperation(ctx context.Context, client *compute.Service, projectID, region, opName string) error {
	return waitForComputeOperation(ctx, func(ctx context.Context) (*compute.Operation, error) {
		return client.RegionOperations.Get(projectID, region, opName).Context(ctx).Do()
	})
}

func waitForComputeOperation(ctx context.Context, get func(context.Context) (*compute.Operation, error)) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	for {
		op, err := get(ctx)
		if err != nil {
			return err
		}
		if op.Status == "DONE" {
			if op.Error != nil {
				return fmt.Errorf("compute operation %s failed: %v", op.Name, op.Error.Errors)
			}
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Second):
		}
	}
}
