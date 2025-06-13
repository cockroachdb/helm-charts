package infra

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gruntwork-io/terratest/modules/k8s"
	"github.com/gruntwork-io/terratest/modules/random"
	"github.com/stretchr/testify/require"
	"google.golang.org/api/compute/v1"
	"google.golang.org/api/container/v1"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"

	"github.com/cockroachdb/helm-charts/tests/e2e/coredns"
	"github.com/cockroachdb/helm-charts/tests/e2e/operator"
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
	vpcName string
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
		waitForGlobalComputeOperation(ctx, computeService, getProjectID(), networkOp.Name)
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
			waitForRegionalComputeOperation(ctx, computeService, getProjectID(), setup.Region, subnetOp.Name)
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
	// 5a) Allow port 9443
	allow9443 := []*compute.FirewallAllowed{{IPProtocol: "tcp", Ports: []string{"9443"}}}
	_, err = createFirewallRuleIfNotExists(ctx, computeService, getProjectID(), vpcSelfLink, webhookFirewallRuleName, allow9443, []string{defaultNodeTag}, allSubnetRanges)
	require.NoError(t, err, "failed to create webhook firewall rule")

	// 5b) Allow internal (TCP,UDP,ICMP)
	internalAllow := []*compute.FirewallAllowed{
		{IPProtocol: "tcp"}, {IPProtocol: "udp"}, {IPProtocol: "icmp"},
	}
	// Combine subnet ranges and cluster CIDRs
	allSources := make([]string, 0, len(allSubnetRanges)+len(allClusterIPV4CIDRs))
	allSources = append(allSources, allSubnetRanges...)
	allSources = append(allSources, allClusterIPV4CIDRs...)
	_, err = createFirewallRuleIfNotExists(ctx, computeService, getProjectID(), vpcSelfLink, internalFirewallRuleName, internalAllow, []string{defaultNodeTag}, allSources)
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

// TeardownInfra deletes all GCP resources created by SetUpInfra.
func (r *GcpRegion) TeardownInfra(t *testing.T) {
	// Ensure teardown continues even if individual steps fail
	defer func() {
		if r := recover(); r != nil {
			t.Logf("[%s] Panic during teardown (continuing cleanup): %v", ProviderGCP, r)
		}
	}()

	t.Logf("[%s] Starting infrastructure teardown", ProviderGCP)

	// Initialize clients
	ctx := context.Background()

	computeService, err := createComputeServiceClient(ctx)
	if err != nil {
		t.Logf("[%s] Warning: failed to create compute client for teardown: %v", ProviderGCP, err)
		return
	}

	gkeService, err := createGKEServiceClient(ctx)
	if err != nil {
		t.Logf("[%s] Warning: failed to create GKE client for teardown: %v", ProviderGCP, err)
		return
	}

	// safely executes teardown steps
	safeExecute := func(stepName string, fn func() error) {
		defer func() {
			if r := recover(); r != nil {
				t.Logf("[%s] Panic during %s (continuing teardown): %v", ProviderGCP, stepName, r)
			}
		}()

		if err := fn(); err != nil {
			t.Logf("[%s] Warning: %s failed: %v", ProviderGCP, stepName, err)
		}
	}

	// 1) Delete GKE clusters
	safeExecute("cluster deletion", func() error {
		return r.deleteGKEClusters(ctx, t, gkeService)
	})

	// 2) Delete static IPs and firewall rules in parallel
	safeExecute("static IPs and firewall rules deletion", func() error {
		return r.deleteStaticIPsAndFirewallRules(ctx, t, computeService)
	})

	// 3) Delete subnets (must be after static IPs since static IPs reference subnets)
	safeExecute("subnet deletion", func() error {
		return r.deleteSubnets(ctx, t, computeService)
	})

	// 5) Delete VPC
	safeExecute("VPC deletion", func() error {
		if r.vpcName != "" {
			// Add a small delay to ensure all dependent resources are fully cleaned up
			time.Sleep(10 * time.Second)

			op, err := computeService.Networks.Delete(getProjectID(), r.vpcName).Context(ctx).Do()
			if err != nil && !IsResourceNotFound(err) {
				t.Logf("[%s] Warning: failed to delete VPC %s: %v", ProviderGCP, r.vpcName, err)
				return err
			} else if op != nil {
				t.Logf("[%s] Initiated deletion of VPC %s", ProviderGCP, r.vpcName)
				// Wait for VPC deletion to complete
				waitForGlobalComputeOperation(ctx, computeService, getProjectID(), op.Name)
				t.Logf("[%s] Successfully deleted VPC %s", ProviderGCP, r.vpcName)
			} else {
				t.Logf("[%s] VPC %s was already deleted", ProviderGCP, r.vpcName)
			}
		}
		return nil
	})

	t.Logf("[%s] Infrastructure teardown completed", ProviderGCP)
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

func createVPC(ctx context.Context, client *compute.Service, projectID, vpcName string) (*compute.Operation, error) {
	network := &compute.Network{
		Name:                  vpcName,
		AutoCreateSubnetworks: false, // This makes it a custom mode network
		RoutingConfig: &compute.NetworkRoutingConfig{
			RoutingMode: "REGIONAL",
		},
		ForceSendFields: []string{"AutoCreateSubnetworks"},
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
	waitForRegionalComputeOperation(ctx, client, projectID, region, op.Name)
	return addressIP, nil
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

// scaleNodePool scales the node count in a GKE cluster's node pool
func scaleNodePool(ctx context.Context, service *container.Service, projectID, location, clusterID, nodePoolID string, nodeCount int64) (*container.Operation, error) {
	req := &container.SetNodePoolSizeRequest{NodeCount: nodeCount}
	name := fmt.Sprintf("projects/%s/locations/%s/clusters/%s/nodePools/%s", projectID, location, clusterID, nodePoolID)
	return service.Projects.Locations.Clusters.NodePools.SetSize(name, req).Context(ctx).Do()
}

// ─── DELETE RESOURCES ──────────────────────────────────────────────────────────

// deleteStaticIPsAndFirewallRules handles deletion of static IPs and firewall rules in parallel
func (r *GcpRegion) deleteStaticIPsAndFirewallRules(ctx context.Context, t *testing.T, computeService *compute.Service) error {
	type operationResult struct {
		resourceType string
		resourceName string
		operation    *compute.Operation
		err          error
		isGlobal     bool
		region       string
	}

	resultsChan := make(chan operationResult, 100) // Buffer for all possible operations
	var wg sync.WaitGroup

	// Static IP deletions (parallel by region)
	for _, cfg := range clusterConfigurations[:len(r.Clusters)] {
		if cfg.UseDynamicIP {
			continue // Skip if using dynamic IP
		}
		wg.Add(1)
		go func(config ClusterSetupConfig) {
			defer wg.Done()
			addressName := fmt.Sprintf("coredns-loadbalancer-%s-%s", config.Region, r.vpcName)
			op, err := computeService.Addresses.Delete(getProjectID(), config.Region, addressName).Context(ctx).Do()
			result := operationResult{
				resourceType: "static IP",
				resourceName: addressName,
				operation:    op,
				err:          err,
				isGlobal:     false,
				region:       config.Region,
			}
			if err != nil && !IsResourceNotFound(err) {
				t.Logf("[%s] Warning: failed to delete static IP %s: %v", ProviderGCP, addressName, err)
			} else if op != nil {
				t.Logf("[%s] Initiated deletion of static IP %s", ProviderGCP, addressName)
			}
			resultsChan <- result
		}(cfg)
	}

	// Firewall rule deletions (parallel)
	firewallRules := []string{webhookFirewallRuleName, internalFirewallRuleName}
	for _, rule := range firewallRules {
		wg.Add(1)
		go func(ruleName string) {
			defer wg.Done()
			op, err := computeService.Firewalls.Delete(getProjectID(), ruleName).Context(ctx).Do()
			result := operationResult{
				resourceType: "firewall rule",
				resourceName: ruleName,
				operation:    op,
				err:          err,
				isGlobal:     true,
			}
			if err != nil && !IsResourceNotFound(err) {
				t.Logf("[%s] Warning: failed to delete firewall rule %s: %v", ProviderGCP, ruleName, err)
			} else if op != nil {
				t.Logf("[%s] Initiated deletion of firewall rule %s", ProviderGCP, ruleName)
			}
			resultsChan <- result
		}(rule)
	}

	// Close the channel when all goroutines complete
	go func() {
		wg.Wait()
		close(resultsChan)
	}()

	// Collect all operations and wait for them in parallel
	var globalOps []*compute.Operation
	var regionalOps []struct {
		op     *compute.Operation
		region string
	}

	for result := range resultsChan {
		if result.err == nil && result.operation != nil {
			if result.isGlobal {
				globalOps = append(globalOps, result.operation)
			} else {
				regionalOps = append(regionalOps, struct {
					op     *compute.Operation
					region string
				}{result.operation, result.region})
			}
		}
	}

	// Wait for all operations in parallel
	var waitWg sync.WaitGroup

	// Wait for global operations
	for _, op := range globalOps {
		waitWg.Add(1)
		go func(operation *compute.Operation) {
			defer waitWg.Done()
			waitForGlobalComputeOperation(ctx, computeService, getProjectID(), operation.Name)
		}(op)
	}

	// Wait for regional operations
	for _, regOp := range regionalOps {
		waitWg.Add(1)
		go func(operation *compute.Operation, region string) {
			defer waitWg.Done()
			waitForRegionalComputeOperation(ctx, computeService, getProjectID(), region, operation.Name)
		}(regOp.op, regOp.region)
	}

	waitWg.Wait()
	return nil
}

// deleteSubnets handles deletion of subnets in parallel (must be called after static IPs are deleted)
func (r *GcpRegion) deleteSubnets(ctx context.Context, t *testing.T, computeService *compute.Service) error {
	type operationResult struct {
		resourceName string
		operation    *compute.Operation
		err          error
		region       string
	}

	resultsChan := make(chan operationResult, len(r.Clusters))
	var wg sync.WaitGroup

	// Subnet deletions (parallel by region)
	for _, cfg := range clusterConfigurations[:len(r.Clusters)] {
		wg.Add(1)
		go func(config ClusterSetupConfig) {
			defer wg.Done()
			op, err := computeService.Subnetworks.Delete(getProjectID(), config.Region, config.SubnetName).Context(ctx).Do()
			result := operationResult{
				resourceName: config.SubnetName,
				operation:    op,
				err:          err,
				region:       config.Region,
			}
			if err != nil && !IsResourceNotFound(err) {
				t.Logf("[%s] Warning: failed to delete subnet %s: %v", ProviderGCP, config.SubnetName, err)
			} else if op != nil {
				t.Logf("[%s] Initiated deletion of subnet %s", ProviderGCP, config.SubnetName)
			}
			resultsChan <- result
		}(cfg)
	}

	// Close the channel when all goroutines complete
	go func() {
		wg.Wait()
		close(resultsChan)
	}()

	// Collect all operations and wait for them in parallel
	var operations []struct {
		op     *compute.Operation
		region string
	}

	for result := range resultsChan {
		if result.err == nil && result.operation != nil {
			operations = append(operations, struct {
				op     *compute.Operation
				region string
			}{result.operation, result.region})
		}
	}

	// Wait for all subnet deletion operations in parallel
	var waitWg sync.WaitGroup
	for _, opInfo := range operations {
		waitWg.Add(1)
		go func(operation *compute.Operation, region string) {
			defer waitWg.Done()
			waitForRegionalComputeOperation(ctx, computeService, getProjectID(), region, operation.Name)
		}(opInfo.op, opInfo.region)
	}

	waitWg.Wait()
	return nil
}

// deleteGKEClusters handles the complex logic of deleting GKE clusters in parallel
func (r *GcpRegion) deleteGKEClusters(ctx context.Context, t *testing.T, gkeService *container.Service) error {
	const maxRetries = 20
	const retryDelay = 30 * time.Second

	// Delete all clusters in parallel
	type deletionResult struct {
		clusterName string
		err         error
	}

	resultsChan := make(chan deletionResult, len(r.Clusters))

	for _, cfg := range clusterConfigurations[:len(r.Clusters)] {
		go func(config ClusterSetupConfig) {
			defer func() {
				if rec := recover(); rec != nil {
					resultsChan <- deletionResult{
						clusterName: config.ClusterName,
						err:         fmt.Errorf("panic during cluster deletion: %v", rec),
					}
				}
			}()

			// Initiate deletion
			if err := r.deleteGKECluster(ctx, t, gkeService, config); err != nil {
				resultsChan <- deletionResult{clusterName: config.ClusterName, err: err}
				return
			}

			// Wait for deletion to complete
			r.waitForClusterDeletion(ctx, t, gkeService, config.ClusterName, config.Region, maxRetries, retryDelay)
			resultsChan <- deletionResult{clusterName: config.ClusterName, err: nil}
		}(cfg)
	}

	// Wait for all deletions to complete
	for i := 0; i < len(r.Clusters); i++ {
		result := <-resultsChan
		if result.err != nil {
			t.Logf("[%s] Warning: failed to delete cluster %s: %v", ProviderGCP, result.clusterName, result.err)
		}
	}

	return nil
}

// deleteGKECluster handles the deletion of a single GKE cluster
func (r *GcpRegion) deleteGKECluster(ctx context.Context, t *testing.T, gkeService *container.Service, cfg ClusterSetupConfig) error {
	t.Logf("[%s] Deleting GKE cluster '%s'", ProviderGCP, cfg.ClusterName)

	clusterPath := fmt.Sprintf("projects/%s/locations/%s/clusters/%s", getProjectID(), cfg.Region, cfg.ClusterName)
	cluster, err := gkeService.Projects.Locations.Clusters.Get(clusterPath).Context(ctx).Do()
	if err != nil {
		if IsResourceNotFound(err) {
			t.Logf("[%s] Cluster %s already deleted", ProviderGCP, cfg.ClusterName)
			return nil
		}
		return fmt.Errorf("error checking cluster %s status: %w", cfg.ClusterName, err)
	}

	// Wait for any ongoing operations to complete
	if err := r.waitForOngoingOperations(ctx, t, gkeService, cluster, cfg); err != nil {
		t.Logf("[%s] Warning: error waiting for ongoing operations on cluster %s: %v", ProviderGCP, cfg.ClusterName, err)
	}

	// Initiate cluster deletion
	delCmd := exec.Command("gcloud", "container", "clusters", "delete", cfg.ClusterName,
		"--region", cfg.Region, "--project", getProjectID(), "--quiet", "--async")
	delCmd.Stdout = os.Stdout
	delCmd.Stderr = os.Stderr
	return delCmd.Run()
}

// ─── WAIT‐FOR‐OPERATION HELPERS ──────────────────────────────────────────────────

func waitForGlobalComputeOperation(ctx context.Context, client *compute.Service, projectID, opName string) {
	for {
		op, err := client.GlobalOperations.Get(projectID, opName).Context(ctx).Do()
		if err != nil {
			break
		}
		if op.Status == "DONE" {
			break
		}
		time.Sleep(10 * time.Second)
	}
}

func waitForRegionalComputeOperation(ctx context.Context, client *compute.Service, projectID, region, opName string) {
	for {
		op, err := client.RegionOperations.Get(projectID, region, opName).Context(ctx).Do()
		if err != nil {
			break
		}
		if op.Status == "DONE" {
			break
		}
		time.Sleep(10 * time.Second)
	}
}

// waitForOngoingOperations waits for any ongoing operations on a cluster
func (r *GcpRegion) waitForOngoingOperations(ctx context.Context, t *testing.T, gkeService *container.Service, cluster *container.Cluster, cfg ClusterSetupConfig) error {
	if cluster.Status == "RUNNING" || cluster.Status == "ERROR" {
		return nil
	}

	t.Logf("[%s] Cluster %s is in %s state, waiting for operation to complete...", ProviderGCP, cfg.ClusterName, cluster.Status)

	if cluster.SelfLink == "" {
		return nil
	}

	operationsPath := fmt.Sprintf("projects/%s/locations/%s/operations", getProjectID(), cfg.Region)
	ops, err := gkeService.Projects.Locations.Operations.List(operationsPath).Context(ctx).Do()
	if err != nil {
		return err
	}

	for _, op := range ops.Operations {
		if strings.Contains(op.TargetLink, cfg.ClusterName) && op.Status != "DONE" {
			return r.waitForGKEOperation(gkeService, op.Name, cfg.Region, "")
		}
	}
	return nil
}

// waitForClusterDeletion waits for a cluster deletion to complete
func (r *GcpRegion) waitForClusterDeletion(ctx context.Context, t *testing.T, gkeService *container.Service, clusterName, region string, maxRetries int, retryDelay time.Duration) {
	clusterPath := fmt.Sprintf("projects/%s/locations/%s/clusters/%s", getProjectID(), region, clusterName)

	for retries := 0; retries < maxRetries; retries++ {
		_, err := gkeService.Projects.Locations.Clusters.Get(clusterPath).Context(ctx).Do()
		if IsResourceNotFound(err) {
			t.Logf("[%s] Confirmed deletion of cluster %s", ProviderGCP, clusterName)
			return
		}
		if retries == maxRetries-1 {
			t.Logf("[%s] Warning: timed out waiting for cluster %s deletion", ProviderGCP, clusterName)
			return
		}
		time.Sleep(retryDelay)
	}
}

// waitForGKEOperation waits for a GKE operation to complete
func (r *GcpRegion) waitForGKEOperation(service *container.Service, operationName, location, zone string) error {
	opID := parseOperationID(operationName)
	for {
		var op *container.Operation
		var err error
		if zone != "" {
			op, err = service.Projects.Zones.Operations.Get(getProjectID(), zone, opID).Context(context.Background()).Do()
		} else {
			name := fmt.Sprintf("projects/%s/locations/%s/operations/%s", getProjectID(), location, opID)
			op, err = service.Projects.Locations.Operations.Get(name).Context(context.Background()).Do()
		}
		if err != nil {
			return fmt.Errorf("failed to get GKE operation status: %w", err)
		}
		if op.Status == "DONE" {
			if op.Error != nil {
				return fmt.Errorf("GKE operation failed: %s", op.Error.Message)
			}
			break
		}
		time.Sleep(15 * time.Second)
	}
	return nil
}

func parseOperationID(fullName string) string {
	parts := strings.Split(fullName, "/")
	return parts[len(parts)-1]
}
