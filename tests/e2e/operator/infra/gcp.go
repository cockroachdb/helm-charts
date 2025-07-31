package infra

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"
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
	serviceAccountKeyPath    = "" // optional path to service-account JSON; if empty, ADC is used.
	webhookFirewallRuleName  = "allow-9443-port-for-webhook"
	internalFirewallRuleName = "allow-internal"
	subnetSuffix             = "subnet" // suffix for dynamically created subnets.
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

func getGCPClusterCIDR(region string) string {
	if config, ok := NetworkConfigs[ProviderGCP][region]; ok {
		if cidr, ok := config.(map[string]string)["ClusterCIDR"]; ok {
			return cidr
		}
	}
	return "172.28.0.0/20"
}

func getGCPServiceCIDR(region string) string {
	if config, ok := NetworkConfigs[ProviderGCP][region]; ok {
		if cidr, ok := config.(map[string]string)["ServiceCIDR"]; ok {
			return cidr
		}
	}
	return "172.28.17.0/24"
}

func getGCPSubnetRange(region string) string {
	if config, ok := NetworkConfigs[ProviderGCP][region]; ok {
		if subnet, ok := config.(map[string]string)["SubnetRange"]; ok {
			return subnet
		}
	}
	return "172.28.16.0/24"
}

func getGCPStaticIP(region string) string {
	if config, ok := NetworkConfigs[ProviderGCP][region]; ok {
		if ip, ok := config.(map[string]string)["StaticIP"]; ok {
			return ip
		}
	}
	return "172.28.16.11"
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
	},
	{
		Region:          "us-east1",
		SubnetRange:     getGCPSubnetRange("us-east1"),
		StaticIPAddress: getGCPStaticIP("us-east1"),
		ClusterName:     "cockroachdb-east",
		ClusterIPV4CIDR: getGCPClusterCIDR("us-east1"),
		ServiceIPV4CIDR: getGCPServiceCIDR("us-east1"),
	},
	{
		Region:          "us-west1",
		SubnetRange:     getGCPSubnetRange("us-west1"),
		StaticIPAddress: getGCPStaticIP("us-west1"),
		ClusterName:     "cockroachdb-west",
		ClusterIPV4CIDR: getGCPClusterCIDR("us-west1"),
		ServiceIPV4CIDR: getGCPServiceCIDR("us-west1"),
	},
}

// GcpRegion wraps operator.Region and manages GCP infra.
type GcpRegion struct {
	*operator.Region
	vpcName string // Store the VPC name for cleanup
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

	// Only iterate as many as r.Clusters
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

		addressName := fmt.Sprintf("coredns-loadbalancer-%s", setup.Region)
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
	// Combine subnet ranges and cluster CIDRs as specified in documentation
	allSources := make([]string, 0, len(allSubnetRanges)+len(allClusterIPV4CIDRs))
	allSources = append(allSources, allSubnetRanges...)
	allSources = append(allSources, allClusterIPV4CIDRs...)
	_, err = createFirewallRuleIfNotExists(ctx, computeService, getProjectID(), vpcSelfLink, internalFirewallRuleName, internalAllow, []string{defaultNodeTag}, allSources)
	require.NoError(t, err, "failed to create internal firewall rule")

	// 6) Create GKE clusters in parallel for better performance.
	kubeConfigPath, err := r.EnsureKubeConfigPath()
	require.NoError(t, err)

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

			// 6a) Discover zones for this region.
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

			// 6b) Create the GKE cluster (regional)
			err = createGKERegionalCluster(ctx, gkeService, cfg, vpcSelfLink)
			if err != nil {
				resultsChan <- clusterResult{index: idx, err: fmt.Errorf("failed to create cluster: %w", err)}
				return
			}

			// 6c) Fetch credentials via gcloud (aliases context to cluster name)
			err = UpdateKubeconfigGCP(t, getProjectID(), cfg.Region, name, name)
			if err != nil {
				resultsChan <- clusterResult{index: idx, err: fmt.Errorf("failed to update kubeconfig: %w", err)}
				return
			}

			// 6d) Prepare a controller-runtime client for this context.
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
	for i := 0; i < len(r.Clusters); i++ {
		result := <-resultsChan
		if result.err != nil {
			t.Fatalf("Failed to create cluster at index %d: %v", result.index, result.err)
		}
		clients[r.Clusters[result.index]] = result.client

		// 6e) Store CoreDNS options (use actual reserved IP, not alloc ID)
		r.CorednsClusterOptions[operator.CustomDomains[result.index]] = coredns.CoreDNSClusterOption{
			IPs:       []string{clusterConfigurations[result.index].StaticIPAddress},
			Namespace: r.Namespace[r.Clusters[result.index]],
			Domain:    operator.CustomDomains[result.index],
		}
	}

	r.Clients = clients
	r.ReusingInfra = true
	t.Logf("[%s] All GKE clusters created successfully", ProviderGCP)

	// 7) Deploy CoreDNS with initial configuration, then update with complete cross-cluster info
	for i, clusterName := range r.Clusters {
		staticIP := clusterConfigurations[i].StaticIPAddress

		// Deploy CoreDNS with initial (possibly incomplete) cluster options
		err := DeployCoreDNS(t, clusterName, kubeConfigPath, &staticIP, ProviderGCP, operator.CustomDomains[i], r.CorednsClusterOptions)
		require.NoError(t, err, "failed to deploy CoreDNS to cluster %s", clusterName)

		// Wait for the CoreDNS service to get the static IP
		kubectlOpts := k8s.NewKubectlOptions(clusterName, kubeConfigPath, CoreDNSNamespace)
		actualIPs, err := WaitForCoreDNSServiceIPs(t, kubectlOpts)
		require.NoError(t, err, "failed to get CoreDNS service IPs for cluster %s", clusterName)

		// Update the cluster options with the actual assigned IPs
		r.CorednsClusterOptions[operator.CustomDomains[i]] = coredns.CoreDNSClusterOption{
			IPs:       actualIPs,
			Namespace: r.Namespace[clusterName],
			Domain:    operator.CustomDomains[i],
		}
	}

	// 8) Update CoreDNS configuration in all clusters with complete cross-cluster information
	for i, clusterName := range r.Clusters {
		kubectlOpts := k8s.NewKubectlOptions(clusterName, kubeConfigPath, CoreDNSNamespace)

		// Create and apply the updated CoreDNS ConfigMap with complete cluster information
		cm := coredns.CoreDNSConfigMap(operator.CustomDomains[i], r.CorednsClusterOptions)
		cmYAML := coredns.ToYAML(t, cm)
		if err := k8s.KubectlApplyFromStringE(t, kubectlOpts, cmYAML); err != nil {
			require.NoError(t, err, "failed to update CoreDNS ConfigMap for cluster %s", clusterName)
		}

		// Restart CoreDNS to pick up the updated configuration
		if err := k8s.RunKubectlE(t, kubectlOpts, "rollout", "restart", "deployment", CoreDNSDeploymentName); err != nil {
			require.NoError(t, err, "failed to restart CoreDNS deployment for cluster %s", clusterName)
		}
	}
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

	// Helper function to safely execute teardown steps
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

	// 1) Delete GKE clusters with timeout protection.
	safeExecute("cluster deletion", func() error {
		for _, cfg := range clusterConfigurations[:len(r.Clusters)] {
			t.Logf("[%s] Deleting GKE cluster '%s'", ProviderGCP, cfg.ClusterName)

			// Check for ongoing operations.
			clusterPath := fmt.Sprintf("projects/%s/locations/%s/clusters/%s", getProjectID(), cfg.Region, cfg.ClusterName)
			cluster, err := gkeService.Projects.Locations.Clusters.Get(clusterPath).Context(ctx).Do()
			if err != nil {
				if IsResourceNotFound(err) {
					t.Logf("[%s] Cluster %s already deleted", ProviderGCP, cfg.ClusterName)
					continue
				}
				t.Logf("[%s] Warning: error checking cluster %s status: %v", ProviderGCP, cfg.ClusterName, err)
				continue
			}

			// If the cluster is in a transitioning state, wait for the current operation to complete.
			if cluster.Status != "RUNNING" && cluster.Status != "ERROR" {
				t.Logf("[%s] Cluster %s is in %s state, waiting for operation to complete...", ProviderGCP, cfg.ClusterName, cluster.Status)
				if cluster.SelfLink != "" { // Use SelfLink to get ongoing operations
					// List operations for this cluster to find the current one
					operationsPath := fmt.Sprintf("projects/%s/locations/%s/operations", getProjectID(), cfg.Region)
					ops, err := gkeService.Projects.Locations.Operations.List(operationsPath).Context(ctx).Do()
					if err == nil {
						for _, op := range ops.Operations {
							if strings.Contains(op.TargetLink, cfg.ClusterName) && op.Status != "DONE" {
								err = waitForGKEOperation(gkeService, op.Name, cfg.Region, "")
								if err != nil {
									t.Logf("[%s] Warning: error waiting for operation on cluster %s: %v", ProviderGCP, cfg.ClusterName, err)
								}
								break
							}
						}
					}
				}
			}

			// Now try to delete the cluster.
			delCmd := exec.Command("gcloud", "container", "clusters", "delete", cfg.ClusterName,
				"--region", cfg.Region, "--project", getProjectID(), "--quiet", "--async")
			delCmd.Stdout = os.Stdout
			delCmd.Stderr = os.Stderr
			if err := delCmd.Run(); err != nil {
				t.Logf("[%s] Warning: error initiating deletion of cluster %s: %v", ProviderGCP, cfg.ClusterName, err)
			}
		}

		// Wait for all cluster deletions to complete with timeout.
		for _, cfg := range clusterConfigurations[:len(r.Clusters)] {
			clusterPath := fmt.Sprintf("projects/%s/locations/%s/clusters/%s", getProjectID(), cfg.Region, cfg.ClusterName)
			for retries := 0; retries < 20; retries++ {
				_, err := gkeService.Projects.Locations.Clusters.Get(clusterPath).Context(ctx).Do()
				if IsResourceNotFound(err) {
					t.Logf("[%s] Confirmed deletion of cluster %s", ProviderGCP, cfg.ClusterName)
					break
				}
				if retries == 19 {
					t.Logf("[%s] Warning: timed out waiting for cluster %s deletion", ProviderGCP, cfg.ClusterName)
				}
				time.Sleep(30 * time.Second)
			}
		}
		return nil
	})

	// 2) Delete static IPs (unreserve).
	safeExecute("static IP deletion", func() error {
		for _, cfg := range clusterConfigurations[:len(r.Clusters)] {
			if cfg.UseDynamicIP {
				continue // Skip if using dynamic IP
			}
			addressName := fmt.Sprintf("coredns-loadbalancer-%s", cfg.Region)
			_, err := computeService.Addresses.Delete(getProjectID(), cfg.Region, addressName).Context(ctx).Do()
			if err != nil && !IsResourceNotFound(err) {
				t.Logf("[%s] Warning: failed to delete static IP %s: %v", ProviderGCP, addressName, err)
			} else {
				t.Logf("[%s] Deleted static IP %s", ProviderGCP, addressName)
			}
		}
		return nil
	})

	// 3) Delete firewall rules.
	safeExecute("firewall rule deletion", func() error {
		for _, rule := range []string{webhookFirewallRuleName, internalFirewallRuleName} {
			_, err = computeService.Firewalls.Delete(getProjectID(), rule).Context(ctx).Do()
			if err != nil && !IsResourceNotFound(err) {
				t.Logf("[%s] Warning: failed to delete firewall rule %s: %v", ProviderGCP, rule, err)
			} else {
				t.Logf("[%s] Deleted firewall rule %s", ProviderGCP, rule)
			}
		}
		return nil
	})

	// 4) Delete subnets.
	safeExecute("subnet deletion", func() error {
		for _, cfg := range clusterConfigurations[:len(r.Clusters)] {
			_, err = computeService.Subnetworks.Delete(getProjectID(), cfg.Region, cfg.SubnetName).Context(ctx).Do()
			if err != nil && !IsResourceNotFound(err) {
				t.Logf("[%s] Warning: failed to delete subnet %s: %v", ProviderGCP, cfg.SubnetName, err)
			} else {
				t.Logf("[%s] Deleted subnet %s", ProviderGCP, cfg.SubnetName)
			}
		}
		return nil
	})

	// 5) Delete VPC
	safeExecute("VPC deletion", func() error {
		if r.vpcName != "" {
			_, err = computeService.Networks.Delete(getProjectID(), r.vpcName).Context(ctx).Do()
			if err != nil && !IsResourceNotFound(err) {
				t.Logf("[%s] Warning: failed to delete VPC %s: %v", ProviderGCP, r.vpcName, err)
			} else {
				t.Logf("[%s] Deleted VPC %s", ProviderGCP, r.vpcName)
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

// getServiceAccountKeyPath returns the path to the service account key file.
func getServiceAccountKeyPath() string {
	if serviceAccountKeyPath != "" {
		return serviceAccountKeyPath
	}
	if path := os.Getenv("GOOGLE_APPLICATION_CREDENTIALS"); path != "" {
		return path
	}
	if os.Getenv("GITHUB_ACTIONS") == "true" {
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
	// 1) Check if addressName already exists.
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
		// If "alreadyExists" or "IP_ADDRESS_IN_USE", treat as ok and return addressName
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

	//See if the cluster already exists.
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
		"--num-nodes", fmt.Sprint(DefaultNodesPerZone),
		"--min-nodes", fmt.Sprint(DefaultNodesPerZone),
		"--max-nodes", fmt.Sprint(DefaultNodesPerZone + 1), // Needed for scaling cluster
		"--enable-autoscaling", // Enable autoscaling
		"--autoprovisioning-network-tags", strings.Join([]string{autoprovisioningNodeTag}, ","),
		"--machine-type", GCPDefaultMachineType,
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

func waitForGKEOperation(service *container.Service, operationName, location, zone string) error {
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
