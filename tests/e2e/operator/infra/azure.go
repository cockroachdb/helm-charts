package infra

import (
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/cockroachdb/helm-charts/tests/e2e/coredns"
	"github.com/cockroachdb/helm-charts/tests/e2e/operator"
	"github.com/gruntwork-io/terratest/modules/k8s"
	"github.com/gruntwork-io/terratest/modules/random"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"
)

const (
	azureDefaultNodeVMSize = "Standard_D4s_v3"
	azureDefaultMaxPods    = 30

	envAzureSubscriptionID  = "AZURE_SUBSCRIPTION_ID"
	envAzureClientID        = "AZURE_CLIENT_ID"
	envAzureClientSecret    = "AZURE_CLIENT_SECRET"
	envAzureTenantID        = "AZURE_TENANT_ID"
	envAzureResourcePrefix  = "AZURE_RESOURCE_PREFIX"
)

// AzureClusterConfig holds per-cluster AKS and network settings for a single run.
type AzureClusterConfig struct {
	Region       string
	ClusterName  string
	VNetName     string
	VNetCIDR     string
	SubnetName   string
	SubnetCIDR   string
	ServiceCIDR  string
	DNSServiceIP string
}

// azureClusterConfigTemplates defines the region-specific network configuration templates.
// These are copied into AzureRegion.clusterConfigs at setup time to avoid shared-state mutations.
var azureClusterConfigTemplates = []AzureClusterConfig{
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

// AzureRegion implements CloudProvider for AKS clusters on Azure.
type AzureRegion struct {
	*operator.Region
	// resourceGroupName is the shared Azure resource group for all clusters in this run.
	resourceGroupName string
	// clusterConfigs holds the per-cluster network/AKS config for this run (copied from templates).
	clusterConfigs []AzureClusterConfig
	// kubeConfigPath is the path to the isolated kubeconfig file used by this test run.
	// Using an isolated file prevents external tools (e.g. Rancher Desktop) that write
	// to ~/.kube/config from removing our AKS context during long-running tests.
	kubeConfigPath string
}

// SetUpInfra creates Azure infrastructure: resource group, VNets, subnets, AKS clusters,
// VNet peering (for multi-region), and deploys CoreDNS.
func (r *AzureRegion) SetUpInfra(t *testing.T) {
	if !r.ReusingInfra {
		// Create an isolated kubeconfig file for this test run.
		// This prevents external tools that modify ~/.kube/config (e.g. Rancher Desktop on
		// developer workstations, which periodically rewrites the file) from removing our
		// AKS context during long-running test operations like cert-manager installation.
		// By writing to an isolated temp file and pointing KUBECONFIG at it, all kubectl,
		// helm, and Go k8s-client operations use our controlled file exclusively.
		tmpFile, err := os.CreateTemp("", "azure-kubeconfig-*.yaml")
		if err != nil {
			t.Fatalf("[%s] Failed to create isolated kubeconfig: %v", ProviderAzure, err)
		}
		emptyConfig := "apiVersion: v1\nkind: Config\nclusters: []\ncontexts: []\nusers: []\n"
		if _, werr := tmpFile.WriteString(emptyConfig); werr != nil {
			t.Fatalf("[%s] Failed to initialize isolated kubeconfig: %v", ProviderAzure, werr)
		}
		_ = tmpFile.Close()
		r.kubeConfigPath = tmpFile.Name()
		prevKubeconfig := os.Getenv("KUBECONFIG")
		os.Setenv("KUBECONFIG", r.kubeConfigPath)
		t.Logf("[%s] Using isolated kubeconfig: %s", ProviderAzure, r.kubeConfigPath)
		t.Cleanup(func() {
			if prevKubeconfig != "" {
				os.Setenv("KUBECONFIG", prevKubeconfig)
			} else {
				os.Unsetenv("KUBECONFIG")
			}
			os.Remove(r.kubeConfigPath)
		})
	}

	if r.ReusingInfra {
		t.Logf("[%s] Reusing existing infrastructure — rediscovering cluster state", ProviderAzure)
		// Populate resourceGroupName from AZURE_RESOURCE_GROUP so TeardownInfra can
		// clean up the resource group at the end of the run if needed.
		if rg := os.Getenv("AZURE_RESOURCE_GROUP"); rg != "" {
			r.resourceGroupName = rg
			t.Logf("[%s] Resource group set from AZURE_RESOURCE_GROUP: %s", ProviderAzure, rg)
		}
		// Re-fetch credentials and set insecure-skip-tls-verify for each cluster.
		// This is required on corporate networks with Netskope TLS inspection: the proxy
		// intercepts HTTPS to the AKS API server and presents its own certificate, which
		// the Go k8s client rejects. UpdateKubeconfigAzure runs az aks get-credentials
		// and then sets insecure-skip-tls-verify=true on the kubeconfig cluster entry.
		for _, clusterName := range r.Clusters {
			if err := UpdateKubeconfigAzure(t, r.resourceGroupName, clusterName, clusterName); err != nil {
				t.Logf("[%s] Warning: could not refresh credentials for cluster %s: %v", ProviderAzure, clusterName, err)
			}
		}
		if err := ReinitFromExistingClusters(t, r.Region); err != nil {
			t.Fatalf("[%s] Failed to reinitialize from existing clusters: %v", ProviderAzure, err)
		}
		return
	}

	// Authenticate via service principal if credentials are present in the environment.
	// On local dev, az login is assumed to have been run already.
	if err := ensureAzureLogin(t); err != nil {
		t.Fatalf("[%s] Azure authentication failed: %v", ProviderAzure, err)
	}

	// Build per-run configs from templates so that parallel test runs don't share state.
	uid := strings.ToLower(random.UniqueId())
	prefix := getResourcePrefix()
	r.resourceGroupName = fmt.Sprintf("%s-rg-%s", prefix, uid)
	r.clusterConfigs = make([]AzureClusterConfig, len(r.Clusters))
	for i, clusterName := range r.Clusters {
		r.clusterConfigs[i] = azureClusterConfigTemplates[i]
		r.clusterConfigs[i].ClusterName = clusterName
		r.clusterConfigs[i].VNetName = fmt.Sprintf("%s-vnet-%d-%s", prefix, i, uid)
		r.clusterConfigs[i].SubnetName = fmt.Sprintf("%s-subnet", clusterName)
	}

	// 1) Create resource group.
	rgLocation := r.clusterConfigs[0].Region
	t.Logf("[%s] Creating resource group %s in %s (subscription %s)", ProviderAzure, r.resourceGroupName, rgLocation, getAzureSubscriptionID())
	if err := r.createResourceGroup(t, rgLocation); err != nil {
		t.Fatalf("[%s] Failed to create resource group: %v", ProviderAzure, err)
	}

	// 2) Create VNet + subnet for each cluster.
	for i := range r.Clusters {
		cfg := &r.clusterConfigs[i]
		t.Logf("[%s] Creating VNet %s (CIDR %s) in region %s", ProviderAzure, cfg.VNetName, cfg.VNetCIDR, cfg.Region)
		if err := r.createVNetAndSubnet(t, cfg); err != nil {
			t.Fatalf("[%s] Failed to create VNet/subnet for cluster %s: %v", ProviderAzure, cfg.ClusterName, err)
		}
	}

	// 3) Create AKS clusters in parallel.
	r.CorednsClusterOptions = make(map[string]coredns.CoreDNSClusterOption)
	if err := r.createAKSClusters(t); err != nil {
		t.Fatalf("[%s] Failed to create AKS clusters: %v", ProviderAzure, err)
	}
	r.ReusingInfra = true

	// 4) For multi-region: set up bidirectional VNet peering.
	if r.IsMultiRegion && len(r.Clusters) >= 2 {
		if err := r.setupVNetPeering(t); err != nil {
			t.Fatalf("[%s] Failed to set up VNet peering: %v", ProviderAzure, err)
		}
	}

	// 5) Deploy and configure CoreDNS on all clusters.
	kubeConfigPath, err := r.EnsureKubeConfigPath()
	require.NoError(t, err)
	if err := r.deployAndConfigureCoreDNS(t, kubeConfigPath); err != nil {
		t.Fatalf("[%s] Failed to deploy CoreDNS: %v", ProviderAzure, err)
	}

	// Install a kubeconfig refresh hook so that cert-manager/trust-manager installs
	// don't leave the context in a broken state. The hook re-runs UpdateKubeconfigAzure
	// for each cluster, restoring insecure-skip-tls-verify and the context entry.
	r.Region.KubeconfigRefreshHook = func(t *testing.T) {
		for i, clusterName := range r.Clusters {
			cfg := r.clusterConfigs[i]
			t.Logf("[%s] Refreshing kubeconfig for cluster %s after cert-manager install", ProviderAzure, clusterName)
			if err := UpdateKubeconfigAzure(t, r.resourceGroupName, cfg.ClusterName, clusterName); err != nil {
				t.Logf("[%s] Warning: failed to refresh kubeconfig for %s: %v", ProviderAzure, clusterName, err)
			}
		}
	}

	t.Logf("[%s] Infrastructure setup complete", ProviderAzure)
	t.Logf("[%s] To clean up or reuse this run, export: AZURE_RESOURCE_GROUP=%s", ProviderAzure, r.resourceGroupName)
}

// TeardownInfra deletes the Azure resource group which removes all resources inside it.
func (r *AzureRegion) TeardownInfra(t *testing.T) {
	t.Logf("[%s] Starting infrastructure teardown", ProviderAzure)

	// Ensure we're authenticated. This is normally done by SetUpInfra, but when
	// TeardownInfra is called standalone (e.g. CLEANUP_ONLY=true), we need to log in here.
	if err := ensureAzureLogin(t); err != nil {
		t.Logf("[%s] Warning: Azure authentication failed during teardown: %v", ProviderAzure, err)
	}

	if r.resourceGroupName == "" {
		// Fall back to AZURE_RESOURCE_GROUP env var. This allows explicit cleanup after a
		// REUSE_INFRA or SKIP_CLEANUP run by setting the resource group from the prior run.
		if rg := os.Getenv("AZURE_RESOURCE_GROUP"); rg != "" {
			r.resourceGroupName = rg
			t.Logf("[%s] Using resource group from AZURE_RESOURCE_GROUP: %s", ProviderAzure, rg)
		} else {
			t.Logf("[%s] No resource group to delete (AZURE_RESOURCE_GROUP not set)", ProviderAzure)
			return
		}
	}

	// --no-wait initiates async deletion; this avoids blocking the test runner.
	cmd := exec.Command("az", "group", "delete",
		"--name", r.resourceGroupName,
		"--subscription", getAzureSubscriptionID(),
		"--yes",
		"--no-wait",
	)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		t.Logf("[%s] Warning: failed to initiate deletion of resource group %s: %v", ProviderAzure, r.resourceGroupName, err)
	} else {
		t.Logf("[%s] Initiated async deletion of resource group %s", ProviderAzure, r.resourceGroupName)
	}

	t.Logf("[%s] Infrastructure teardown complete", ProviderAzure)
}

// ScaleNodePool is a no-op for Azure: the cluster autoscaler set up during
// SetUpInfra handles node scaling automatically when pods are Pending.
func (r *AzureRegion) ScaleNodePool(t *testing.T, location string, nodeCount, index int) {
	t.Logf("[%s] Node pool scaling delegated to cluster autoscaler (no explicit action needed)", ProviderAzure)
}

// CanScale reports that Azure supports autoscaling via the cluster autoscaler.
func (r *AzureRegion) CanScale() bool {
	return true
}

// createResourceGroup creates an Azure resource group.
func (r *AzureRegion) createResourceGroup(t *testing.T, location string) error {
	cmd := exec.Command("az", "group", "create",
		"--name", r.resourceGroupName,
		"--location", location,
		"--subscription", getAzureSubscriptionID(),
	)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("az group create %s: %w", r.resourceGroupName, err)
	}
	t.Logf("[%s] Resource group %s created", ProviderAzure, r.resourceGroupName)
	return nil
}

// createVNetAndSubnet creates an Azure VNet with a single subnet for an AKS cluster.
func (r *AzureRegion) createVNetAndSubnet(t *testing.T, cfg *AzureClusterConfig) error {
	cmd := exec.Command("az", "network", "vnet", "create",
		"--resource-group", r.resourceGroupName,
		"--name", cfg.VNetName,
		"--location", cfg.Region,
		"--address-prefix", cfg.VNetCIDR,
		"--subnet-name", cfg.SubnetName,
		"--subnet-prefix", cfg.SubnetCIDR,
		"--subscription", getAzureSubscriptionID(),
	)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("az network vnet create %s: %w", cfg.VNetName, err)
	}
	t.Logf("[%s] VNet %s created with subnet %s", ProviderAzure, cfg.VNetName, cfg.SubnetName)
	return nil
}

// createAKSClusters creates all AKS clusters in parallel.
func (r *AzureRegion) createAKSClusters(t *testing.T) error {
	type clusterResult struct {
		index  int
		client client.Client
		err    error
	}

	resultsChan := make(chan clusterResult, len(r.Clusters))
	clients := make(map[string]client.Client)

	for i, clusterName := range r.Clusters {
		go func(idx int, name string) {
			defer func() {
				if rec := recover(); rec != nil {
					resultsChan <- clusterResult{index: idx, err: fmt.Errorf("panic during cluster creation: %v", rec)}
				}
			}()

			cfg := r.clusterConfigs[idx]
			t.Logf("[%s] Creating AKS cluster %s in region %s", ProviderAzure, name, cfg.Region)

			if err := createAKSCluster(t, r.resourceGroupName, name, cfg, r.NodeCount); err != nil {
				resultsChan <- clusterResult{index: idx, err: err}
				return
			}

			// Fetch credentials and merge into kubeconfig with the cluster name as context alias.
			if err := UpdateKubeconfigAzure(t, r.resourceGroupName, name, name); err != nil {
				resultsChan <- clusterResult{index: idx, err: err}
				return
			}

			// Create a controller-runtime Kubernetes client for this context.
			restCfg, err := config.GetConfigWithContext(name)
			if err != nil {
				resultsChan <- clusterResult{index: idx, err: fmt.Errorf("get rest config for %s: %w", name, err)}
				return
			}
			k8sClient, err := client.New(restCfg, client.Options{})
			if err != nil {
				resultsChan <- clusterResult{index: idx, err: fmt.Errorf("create k8s client for %s: %w", name, err)}
				return
			}

			t.Logf("[%s] Successfully created AKS cluster %s", ProviderAzure, name)
			resultsChan <- clusterResult{index: idx, client: k8sClient}
		}(i, clusterName)
	}

	// Collect results.
	for range r.Clusters {
		result := <-resultsChan
		if result.err != nil {
			return fmt.Errorf("cluster at index %d: %w", result.index, result.err)
		}
		clusterName := r.Clusters[result.index]
		clients[clusterName] = result.client

		// Seed CoreDNS options with a placeholder IP; updated with real IPs after LB assignment.
		r.CorednsClusterOptions[operator.CustomDomains[result.index]] = coredns.CoreDNSClusterOption{
			IPs:       []string{"127.0.0.1"},
			Namespace: r.Namespace[clusterName],
			Domain:    operator.CustomDomains[result.index],
		}
	}

	r.Clients = clients
	t.Logf("[%s] All AKS clusters created successfully", ProviderAzure)
	return nil
}

// createAKSCluster creates a single AKS cluster using the az CLI.
// Uses Azure CNI (--network-plugin azure) so pod IPs come from the subnet and
// are automatically routable across VNet peering for multi-region setups.
func createAKSCluster(t *testing.T, resourceGroup, clusterName string, cfg AzureClusterConfig, nodeCount int) error {
	subnetID, err := getSubnetID(resourceGroup, cfg.VNetName, cfg.SubnetName)
	if err != nil {
		return fmt.Errorf("get subnet ID for %s: %w", cfg.SubnetName, err)
	}

	maxCount := nodeCount + 1

	args := []string{
		"aks", "create",
		"--resource-group", resourceGroup,
		"--name", clusterName,
		"--location", cfg.Region,
		"--subscription", getAzureSubscriptionID(),
		"--node-count", fmt.Sprint(nodeCount),
		"--node-vm-size", azureDefaultNodeVMSize,
		"--network-plugin", "azure",
		"--vnet-subnet-id", subnetID,
		"--service-cidr", cfg.ServiceCIDR,
		"--dns-service-ip", cfg.DNSServiceIP,
		"--max-pods", fmt.Sprint(azureDefaultMaxPods),
		"--enable-cluster-autoscaler",
		"--min-count", fmt.Sprint(nodeCount),
		"--max-count", fmt.Sprint(maxCount),
		"--generate-ssh-keys",
	}

	cmd := exec.Command("az", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	t.Logf("[%s] Running: az %s", ProviderAzure, strings.Join(args, " "))
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("az aks create %s: %w", clusterName, err)
	}
	return nil
}

// setupVNetPeering creates bidirectional VNet peering between the two cluster VNets
// so that pods and services (including CoreDNS LB IPs) can communicate cross-cluster.
func (r *AzureRegion) setupVNetPeering(t *testing.T) error {
	if len(r.Clusters) < 2 {
		return nil
	}

	cfg0 := r.clusterConfigs[0]
	cfg1 := r.clusterConfigs[1]

	t.Logf("[%s] Setting up VNet peering: %s ↔ %s", ProviderAzure, cfg0.VNetName, cfg1.VNetName)

	// Retrieve resource IDs for both VNets.
	vnet0ID, err := getVNetID(r.resourceGroupName, cfg0.VNetName)
	if err != nil {
		return fmt.Errorf("get VNet ID for %s: %w", cfg0.VNetName, err)
	}
	vnet1ID, err := getVNetID(r.resourceGroupName, cfg1.VNetName)
	if err != nil {
		return fmt.Errorf("get VNet ID for %s: %w", cfg1.VNetName, err)
	}

	// Create both peering directions in parallel.
	var wg sync.WaitGroup
	errs := make(chan error, 2)

	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := createVNetPeering(r.resourceGroupName, cfg0.VNetName, "peer-0-to-1", vnet1ID); err != nil {
			errs <- fmt.Errorf("peering %s→%s: %w", cfg0.VNetName, cfg1.VNetName, err)
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := createVNetPeering(r.resourceGroupName, cfg1.VNetName, "peer-1-to-0", vnet0ID); err != nil {
			errs <- fmt.Errorf("peering %s→%s: %w", cfg1.VNetName, cfg0.VNetName, err)
		}
	}()

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

// deployAndConfigureCoreDNS sets up cross-cluster DNS on AKS using the coredns-custom
// ConfigMap and a crl-core-dns internal LoadBalancer Service targeting AKS CoreDNS pods.
func (r *AzureRegion) deployAndConfigureCoreDNS(t *testing.T, kubeConfigPath string) error {
	for i, clusterName := range r.Clusters {
		kubectlOpts := k8s.NewKubectlOptions(clusterName, kubeConfigPath, coreDNSNamespace)

		// Apply coredns-custom ConfigMap (placeholder IPs at this stage).
		if err := applyAzureCoreDNSCustom(t, kubectlOpts, operator.CustomDomains[i], r.CorednsClusterOptions); err != nil {
			return fmt.Errorf("apply coredns-custom for cluster %s: %w", clusterName, err)
		}

		// Create the crl-core-dns internal LB service targeting AKS CoreDNS pods.
		if err := applyAzureCoreDNSService(t, kubectlOpts); err != nil {
			return fmt.Errorf("apply crl-core-dns service for cluster %s: %w", clusterName, err)
		}

		// Restart AKS CoreDNS to pick up the coredns-custom ConfigMap.
		if err := k8s.RunKubectlE(t, kubectlOpts, "rollout", "restart", "deployment", coreDNSDeploymentName); err != nil {
			return fmt.Errorf("restart CoreDNS for cluster %s: %w", clusterName, err)
		}

		// Wait for the Azure internal LB to assign an IP from the subnet.
		actualIPs, err := WaitForCoreDNSServiceIPs(t, kubectlOpts)
		if err != nil {
			return fmt.Errorf("get CoreDNS LB IPs for cluster %s: %w", clusterName, err)
		}

		// Log endpoint count to verify the LB has healthy backends.
		// If endpoints is 0, the service selector doesn't match any pods (misconfigured).
		endpointsOutput, epErr := k8s.RunKubectlAndGetOutputE(t, kubectlOpts, "get", "endpoints", coreDNSServiceName)
		if epErr == nil {
			t.Logf("[%s] crl-core-dns endpoints for cluster %s: %s", ProviderAzure, clusterName, endpointsOutput)
		}

		r.CorednsClusterOptions[operator.CustomDomains[i]] = coredns.CoreDNSClusterOption{
			IPs:       actualIPs,
			Namespace: r.Namespace[clusterName],
			Domain:    operator.CustomDomains[i],
		}
		t.Logf("[%s] CoreDNS LB IPs for cluster %s: %v", ProviderAzure, clusterName, actualIPs)
	}

	// Final pass: update all clusters' coredns-custom ConfigMaps with the real cross-cluster IPs.
	// Uses UpdateCoreDNSConfiguration which has Azure-specific handling for coredns-custom.
	UpdateCoreDNSConfiguration(t, r.Region, kubeConfigPath)
	return nil
}

// applyAzureCoreDNSCustom creates or updates the coredns-custom ConfigMap that
// AKS CoreDNS watches for custom forwarding and rewrite rules.
func applyAzureCoreDNSCustom(t *testing.T, kubectlOpts *k8s.KubectlOptions, thisDomain string, allClusters map[string]coredns.CoreDNSClusterOption) error {
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
	cmYAML := coredns.ToYAML(t, cm)
	if err := k8s.KubectlApplyFromStringE(t, kubectlOpts, cmYAML); err != nil {
		return fmt.Errorf("kubectl apply coredns-custom: %w", err)
	}
	return nil
}

// buildAzureCoreDNSCustomData builds the data map for the coredns-custom ConfigMap.
// Keys ending in .override are applied to the default zone; keys ending in .server
// add custom server blocks for forwarding to remote clusters.
func buildAzureCoreDNSCustomData(thisDomain string, allClusters map[string]coredns.CoreDNSClusterOption) map[string]string {
	data := make(map[string]string)

	// Rewrite rule: translate custom domain names to cluster.local and back.
	quotedDomain := regexp.QuoteMeta(thisDomain)
	data["rewrite.override"] = fmt.Sprintf(`rewrite continue {
    name regex ^(.+)\.%s\.?$ {1}.cluster.local
    answer name ^(.+)\.cluster\.local\.?$ {1}.%s
    answer value ^(.+)\.cluster\.local\.?$ {1}.%s
}`, quotedDomain, thisDomain, thisDomain)

	// Collect and sort domains for deterministic output.
	domains := make([]string, 0, len(allClusters))
	for d := range allClusters {
		domains = append(domains, d)
	}
	sort.Strings(domains)

	// One forwarding server block per remote cluster.
	for _, clusterDomain := range domains {
		if clusterDomain == thisDomain {
			continue
		}
		cluster := allClusters[clusterDomain]
		if len(cluster.IPs) == 0 {
			continue
		}
		sortedIPs := make([]string, len(cluster.IPs))
		copy(sortedIPs, cluster.IPs)
		sort.Strings(sortedIPs)
		ipList := strings.Join(sortedIPs, " ")

		serverBlock := func(host string) string {
			return fmt.Sprintf("%s:53 {\n    log\n    errors\n    ready\n    cache 30\n    forward . %s {\n        force_tcp\n    }\n}\n",
				host, ipList)
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

// detectCoreDNSPodLabel returns the k8s-app label used by AKS CoreDNS pods (either
// "kube-dns" or "coredns" depending on AKS version). Uses kubectl instead of the Go
// k8s client to avoid Netskope proxy "Bad Gateway" errors on corporate networks.
func detectCoreDNSPodLabel(t *testing.T, kubectlOpts *k8s.KubectlOptions) map[string]string {
	for _, label := range []string{"kube-dns", "coredns"} {
		output, err := k8s.RunKubectlAndGetOutputE(t, kubectlOpts,
			"get", "pods", "-l", fmt.Sprintf("k8s-app=%s", label),
			"--no-headers", "-o", "name")
		if err != nil {
			t.Logf("[%s] Warning: kubectl get pods for k8s-app=%s failed: %v", ProviderAzure, label, err)
			continue
		}
		if strings.TrimSpace(output) != "" {
			count := len(strings.Split(strings.TrimSpace(output), "\n"))
			t.Logf("[%s] Detected CoreDNS pod label: k8s-app=%s (%d pod(s) found)", ProviderAzure, label, count)
			return map[string]string{"k8s-app": label}
		}
	}
	// Default to kube-dns (standard Kubernetes DNS label) if neither found or on error.
	t.Logf("[%s] Warning: no CoreDNS pods found with k8s-app=kube-dns or k8s-app=coredns; defaulting to k8s-app=kube-dns", ProviderAzure)
	return map[string]string{"k8s-app": "kube-dns"}
}

// applyAzureCoreDNSService creates the crl-core-dns internal LoadBalancer Service.
// It dynamically detects the label used by AKS CoreDNS pods (either k8s-app: kube-dns
// or k8s-app: coredns depending on the AKS version) to ensure the LB has healthy backends.
func applyAzureCoreDNSService(t *testing.T, kubectlOpts *k8s.KubectlOptions) error {
	// Detect the selector label actually used by AKS CoreDNS pods.
	// Using the wrong label results in an LB with no endpoints and silent DNS failures.
	coreDNSSelector := detectCoreDNSPodLabel(t, kubectlOpts)

	annotations := GetLoadBalancerAnnotations(ProviderAzure)
	annCopy := make(map[string]string, len(annotations))
	for k, v := range annotations {
		annCopy[k] = v
	}

	svc := &corev1.Service{
		TypeMeta: metav1.TypeMeta{
			Kind:       "Service",
			APIVersion: "v1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:        coreDNSServiceName,
			Namespace:   coreDNSNamespace,
			Annotations: annCopy,
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
			Selector: coreDNSSelector,
		},
	}

	svcYAML := coredns.ToYAML(t, svc)
	if err := k8s.KubectlApplyFromStringE(t, kubectlOpts, svcYAML); err != nil {
		return fmt.Errorf("kubectl apply crl-core-dns: %w", err)
	}
	return nil
}

// getResourcePrefix returns the prefix used for all Azure resource names (resource groups,
// VNets, clusters). Set AZURE_RESOURCE_PREFIX to override the default prefix.
// Defaults to "shreyaskm" to make resources easy to identify and clean up in a shared subscription.
func getResourcePrefix() string {
	if prefix := os.Getenv(envAzureResourcePrefix); prefix != "" {
		return prefix
	}
	return "shreyaskm"
}

// getAzureSubscriptionID returns the Azure subscription ID from the environment variable.
// It mirrors GCP's getProjectID() pattern: a required env var with no hard-coded fallback,
// since subscriptions are account-specific unlike GCP project IDs.
func getAzureSubscriptionID() string {
	return os.Getenv(envAzureSubscriptionID)
}

// ensureAzureLogin authenticates the az CLI using a service principal when
// AZURE_CLIENT_ID, AZURE_CLIENT_SECRET, and AZURE_TENANT_ID are all set.
// This mirrors GCP's getServiceAccountKeyPath() / ADC pattern:
//   - In CI: set all three env vars to log in as a service principal.
//   - Locally: run `az login` beforehand; this function is a no-op in that case.
func ensureAzureLogin(t *testing.T) error {
	clientID := os.Getenv(envAzureClientID)
	clientSecret := os.Getenv(envAzureClientSecret)
	tenantID := os.Getenv(envAzureTenantID)

	if clientID == "" || clientSecret == "" || tenantID == "" {
		// Credentials not set — rely on an existing interactive az login session.
		t.Logf("[%s] Service principal env vars not set; relying on existing az login session", ProviderAzure)
		return nil
	}

	t.Logf("[%s] Authenticating via service principal (tenant %s)", ProviderAzure, tenantID)
	cmd := exec.Command("az", "login",
		"--service-principal",
		"--username", clientID,
		"--password", clientSecret,
		"--tenant", tenantID,
	)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Logf("[%s] az login --service-principal output:\n%s", ProviderAzure, string(output))
		return fmt.Errorf("az login failed: %w", err)
	}

	// Set the active subscription so all subsequent commands target the right account.
	if subID := getAzureSubscriptionID(); subID != "" {
		setCmd := exec.Command("az", "account", "set", "--subscription", subID)
		if out, err := setCmd.CombinedOutput(); err != nil {
			t.Logf("[%s] az account set output:\n%s", ProviderAzure, string(out))
			return fmt.Errorf("az account set %s: %w", subID, err)
		}
		t.Logf("[%s] Active subscription set to %s", ProviderAzure, subID)
	}

	return nil
}

// UpdateKubeconfigAzure fetches AKS credentials into the kubeconfig under the given
// context alias and sets insecure-skip-tls-verify to handle Netskope TLS inspection.
func UpdateKubeconfigAzure(t *testing.T, resourceGroup, clusterName, alias string) error {
	args := []string{
		"aks", "get-credentials",
		"--resource-group", resourceGroup,
		"--name", clusterName,
		"--subscription", getAzureSubscriptionID(),
		"--context", alias,
		"--overwrite-existing",
	}
	// If KUBECONFIG points to our isolated temp file, write credentials there explicitly
	// using --file so az ignores the default ~/.kube/config path.
	if kc := os.Getenv("KUBECONFIG"); kc != "" {
		args = append(args, "--file", kc)
	}
	cmd := exec.Command("az", args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Logf("[%s] az aks get-credentials failed. Output:\n%s", ProviderAzure, string(output))
		return fmt.Errorf("get credentials for cluster %s: %w", clusterName, err)
	}

	// Set insecure-skip-tls-verify on the cluster entry so the Go k8s client skips
	// server certificate verification. Without this the Go TLS layer rejects the
	// Netskope-intercepted certificate with "x509: certificate signed by unknown authority".
	skipCmd := exec.Command("kubectl", "config", "set-cluster", alias,
		"--insecure-skip-tls-verify=true",
	)
	if skipOut, skipErr := skipCmd.CombinedOutput(); skipErr != nil {
		t.Logf("[%s] kubectl config set-cluster --insecure-skip-tls-verify failed. Output:\n%s", ProviderAzure, string(skipOut))
		return fmt.Errorf("set insecure-skip-tls-verify for cluster %s: %w", alias, skipErr)
	}
	t.Logf("[%s] Set insecure-skip-tls-verify=true for cluster %s (Netskope proxy workaround)", ProviderAzure, alias)

	return nil
}

// getSubnetID returns the Azure resource ID of a VNet subnet via the az CLI.
func getSubnetID(resourceGroup, vnetName, subnetName string) (string, error) {
	out, err := exec.Command("az", "network", "vnet", "subnet", "show",
		"--resource-group", resourceGroup,
		"--vnet-name", vnetName,
		"--name", subnetName,
		"--subscription", getAzureSubscriptionID(),
		"--query", "id",
		"--output", "tsv",
	).Output()
	if err != nil {
		return "", fmt.Errorf("az network vnet subnet show %s/%s: %w", vnetName, subnetName, err)
	}
	return strings.TrimSpace(string(out)), nil
}

// getVNetID returns the Azure resource ID of a VNet via the az CLI.
func getVNetID(resourceGroup, vnetName string) (string, error) {
	out, err := exec.Command("az", "network", "vnet", "show",
		"--resource-group", resourceGroup,
		"--name", vnetName,
		"--subscription", getAzureSubscriptionID(),
		"--query", "id",
		"--output", "tsv",
	).Output()
	if err != nil {
		return "", fmt.Errorf("az network vnet show %s: %w", vnetName, err)
	}
	return strings.TrimSpace(string(out)), nil
}

// createVNetPeering creates a one-directional VNet peering link via the az CLI.
// The reverse direction must be created separately by calling this function again
// with the source and remote VNets swapped.
func createVNetPeering(resourceGroup, vnetName, peeringName, remoteVNetID string) error {
	cmd := exec.Command("az", "network", "vnet", "peering", "create",
		"--resource-group", resourceGroup,
		"--name", peeringName,
		"--vnet-name", vnetName,
		"--remote-vnet", remoteVNetID,
		"--subscription", getAzureSubscriptionID(),
		"--allow-vnet-access",
		"--allow-forwarded-traffic",
	)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// Ensure AzureRegion satisfies the CloudProvider interface at compile time.
var _ CloudProvider = (*AzureRegion)(nil)
