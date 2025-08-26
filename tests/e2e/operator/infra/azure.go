package infra

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/containerservice/armcontainerservice/v4"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v4"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/resources/armresources"
	"github.com/cockroachdb/helm-charts/tests/e2e/coredns"
	"github.com/cockroachdb/helm-charts/tests/e2e/operator"
	"github.com/gruntwork-io/terratest/modules/k8s"
	"github.com/gruntwork-io/terratest/modules/random"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"
)

// --- Azure Constants ---
const (
	azureSubscriptionID = "<YOUR_AZURE_SUBSCRIPTION_ID>" // replace with your subscription ID
	commonResourceGroup = "cockroachdb-infra-rg"
	defaultNodeTagKey   = "app"
	defaultNodeTagValue = "cockroachdb-node"
	defaultK8sVersion   = "" // empty = use AKS default
)

// AzureClusterSetupConfig holds per-cluster parameters
type AzureClusterSetupConfig struct {
	Region            string   // e.g. "centralus", "eastus", "westus"
	ResourceGroup     string   // = commonResourceGroup
	VNetName          string   // = "vnet-<region>"
	VNetPrefix        string   // e.g. "172.28.16.0/16" (unique per-region block)
	SubnetName        string   // = "subnet-<region>"
	SubnetPrefix      string   // e.g. "172.28.16.0/24"
	ClusterName       string   // e.g. "cockroachdb-central"
	DNSPrefix         string   // e.g. "crdb-central"
	PodCIDR           string   // e.g. "10.244.0.0/16"
	ServiceCIDR       string   // e.g. "10.96.0.0/16"
	AvailabilityZones []string // e.g. {"1","2","3"}
}

// Helper functions to get network configuration from common.go
func getAzureVNetPrefix(region string) string {
	if config, ok := NetworkConfigs[ProviderAzure][region]; ok {
		if prefix, ok := config.(map[string]interface{})["VNetPrefix"].(string); ok {
			return prefix
		}
	}
	// Fallback defaults if config not found
	return "172.28.0.0/16"
}

func getAzureSubnetPrefix(region string) string {
	if config, ok := NetworkConfigs[ProviderAzure][region]; ok {
		if prefix, ok := config.(map[string]interface{})["SubnetPrefix"].(string); ok {
			return prefix
		}
	}
	// Fallback defaults if config not found
	return "172.28.0.0/24"
}

func getAzurePodCIDR(region string) string {
	if config, ok := NetworkConfigs[ProviderAzure][region]; ok {
		if cidr, ok := config.(map[string]interface{})["PodCIDR"].(string); ok {
			return cidr
		}
	}
	// Fallback defaults if config not found
	return "10.244.0.0/16"
}

func getAzureServiceCIDR(region string) string {
	if config, ok := NetworkConfigs[ProviderAzure][region]; ok {
		if cidr, ok := config.(map[string]interface{})["ServiceCIDR"].(string); ok {
			return cidr
		}
	}
	// Fallback defaults if config not found
	return "10.96.0.0/16"
}

// One entry per cluster/region
var azureClusterSetups = []AzureClusterSetupConfig{
	{
		Region:            "centralus",
		ResourceGroup:     commonResourceGroup,
		VNetName:          "vnet-centralus",
		VNetPrefix:        getAzureVNetPrefix("centralus"),
		SubnetName:        "subnet-centralus",
		SubnetPrefix:      getAzureSubnetPrefix("centralus"),
		ClusterName:       "cockroachdb-central",
		DNSPrefix:         "crdb-central",
		PodCIDR:           getAzurePodCIDR("centralus"),
		ServiceCIDR:       getAzureServiceCIDR("centralus"),
		AvailabilityZones: []string{"1", "2", "3"},
	},
	{
		Region:            "eastus",
		ResourceGroup:     commonResourceGroup,
		VNetName:          "vnet-eastus",
		VNetPrefix:        getAzureVNetPrefix("eastus"),
		SubnetName:        "subnet-eastus",
		SubnetPrefix:      getAzureSubnetPrefix("eastus"),
		ClusterName:       "cockroachdb-east",
		DNSPrefix:         "crdb-east",
		PodCIDR:           getAzurePodCIDR("eastus"),
		ServiceCIDR:       getAzureServiceCIDR("eastus"),
		AvailabilityZones: []string{"1", "2", "3"},
	},
	{
		Region:            "westus",
		ResourceGroup:     commonResourceGroup,
		VNetName:          "vnet-westus",
		VNetPrefix:        getAzureVNetPrefix("westus"),
		SubnetName:        "subnet-westus",
		SubnetPrefix:      getAzureSubnetPrefix("westus"),
		ClusterName:       "cockroachdb-west",
		DNSPrefix:         "crdb-west",
		PodCIDR:           getAzurePodCIDR("westus"),
		ServiceCIDR:       getAzureServiceCIDR("westus"),
		AvailabilityZones: []string{"1", "2", "3"},
	},
}

// AzureRegion implements CloudProvider for Azure
type AzureRegion struct {
	*operator.Region
}

// ScaleNodePool scales the node pool in an AKS cluster
func (r *AzureRegion) ScaleNodePool(t *testing.T, location string, nodeCount, index int) {
	t.Logf("[%s] Scaling node pool in cluster %s to %d nodes", ProviderAzure, azureClusterSetups[index].ClusterName, nodeCount)

	// In a real implementation, this would use the Azure SDK to scale the node pool
	// This would include getting the node pool and updating its count
	t.Logf("[%s] Node pool scaling not fully implemented for Azure", ProviderAzure)

	// Uncomment and complete this code when implementing actual scaling:
	/*
		ctx := context.Background()
		cred, err := azidentity.NewDefaultAzureCredential(nil)
		if err != nil {
			t.Logf("[%s] Failed to get Azure credentials: %v", ProviderAzure, err)
			return
		}

		aksClient, err := armcontainerservice.NewManagedClustersClient(azureSubscriptionID, cred, nil)
		if err != nil {
			t.Logf("[%s] Failed to create AKS client: %v", ProviderAzure, err)
			return
		}

		// Actual implementation would go here
	*/
}

func (r *AzureRegion) SetUpInfra(t *testing.T) {
	if r.ReusingInfra {
		t.Logf("[%s] Reusing existing infrastructure", ProviderAzure)
		return
	}

	t.Logf("[%s] Setting up infrastructure", ProviderAzure)
	ctx := context.Background()
	cred, err := azidentity.NewDefaultAzureCredential(nil)
	require.NoError(t, err)

	// Clients
	rgClient, err := armresources.NewResourceGroupsClient(azureSubscriptionID, cred, nil)
	require.NoError(t, err)
	vnetClient, err := armnetwork.NewVirtualNetworksClient(azureSubscriptionID, cred, nil)
	require.NoError(t, err)
	subnetClient, err := armnetwork.NewSubnetsClient(azureSubscriptionID, cred, nil)
	require.NoError(t, err)
	nsgClient, err := armnetwork.NewSecurityGroupsClient(azureSubscriptionID, cred, nil)
	require.NoError(t, err)
	pipClient, err := armnetwork.NewPublicIPAddressesClient(azureSubscriptionID, cred, nil)
	require.NoError(t, err)
	aksClient, err := armcontainerservice.NewManagedClustersClient(azureSubscriptionID, cred, nil)
	require.NoError(t, err)

	// 1) Ensure Resource Group exists
	_, err = createOrUpdateRG(ctx, rgClient, commonResourceGroup, azureClusterSetups[0].Region)
	require.NoError(t, err)

	// Prepare map for CoreDNS options
	r.CorednsClusterOptions = make(map[string]coredns.CoreDNSClusterOption)

	// 2) Loop over each cluster config
	for i, cfg := range azureClusterSetups[:len(r.Clusters)] {
		// Update cluster name to match the one in r.Clusters
		cfg.ClusterName = r.Clusters[i]

		t.Logf("[%s] Setting up cluster %s in region %s", ProviderAzure, cfg.ClusterName, cfg.Region)

		// 2a) Create or reuse VNet in this region
		_, err := createOrUpdateVNet(ctx, vnetClient, cfg.ResourceGroup, cfg.VNetName, cfg.Region, cfg.VNetPrefix)
		require.NoError(t, err)

		// 2b) Create or reuse Subnet inside that VNet
		subnetResp, err := createOrUpdateSubnet(ctx, subnetClient, cfg.ResourceGroup, cfg.VNetName, cfg.SubnetName, cfg.SubnetPrefix)
		require.NoError(t, err)

		// 2c) Create or reuse NSG and associate it to the Subnet
		nsgName := cfg.SubnetName + "-nsg"
		nsgRules := []armnetwork.SecurityRule{
			{
				Name: ToPtr("AllowWebhook9443"),
				Properties: &armnetwork.SecurityRulePropertiesFormat{
					Protocol:                 ToPtr(armnetwork.SecurityRuleProtocolTCP),
					SourceAddressPrefix:      ToPtr("VirtualNetwork"),
					SourcePortRange:          ToPtr("*"),
					DestinationAddressPrefix: ToPtr("*"),
					DestinationPortRange:     ToPtr("9443"),
					Access:                   ToPtr(armnetwork.SecurityRuleAccessAllow),
					Priority:                 ToPtr[int32](100),
					Direction:                ToPtr(armnetwork.SecurityRuleDirectionInbound),
				},
			},
			{
				Name: ToPtr("AllowVNetInternal"),
				Properties: &armnetwork.SecurityRulePropertiesFormat{
					Protocol:                 ToPtr(armnetwork.SecurityRuleProtocolAsterisk),
					SourceAddressPrefix:      ToPtr("VirtualNetwork"),
					SourcePortRange:          ToPtr("*"),
					DestinationAddressPrefix: ToPtr("VirtualNetwork"),
					DestinationPortRange:     ToPtr("*"),
					Access:                   ToPtr(armnetwork.SecurityRuleAccessAllow),
					Priority:                 ToPtr[int32](110),
					Direction:                ToPtr(armnetwork.SecurityRuleDirectionInbound),
				},
			},
		}
		nsgResp, err := createOrUpdateNSG(ctx, nsgClient, cfg.ResourceGroup, nsgName, cfg.Region, nsgRules)
		require.NoError(t, err)
		err = associateNSG(ctx, subnetClient, cfg.ResourceGroup, cfg.VNetName, cfg.SubnetName, *nsgResp.ID)
		require.NoError(t, err)

		// 2d) Create or reuse a Public IP for CoreDNS LB in this region
		pipName := fmt.Sprintf("coredns-pip-%s", cfg.Region)
		pipResp, err := ensurePublicIP(ctx, pipClient, cfg.ResourceGroup, cfg.Region, pipName)
		require.NoError(t, err)

		// 2e) Create or reuse AKS cluster
		_, err = createOrUpdateAKS(ctx, aksClient, cfg, *subnetResp.ID)
		require.NoError(t, err)

		// 2f) Update kubeconfig via Azure CLI
		err = UpdateKubeconfigAzure(t, cfg.ResourceGroup, cfg.ClusterName)
		require.NoError(t, err, "failed to get-credentials for %s", cfg.ClusterName)

		// 2g) Prepare CoreDNS options (store static IP)
		r.CorednsClusterOptions[operator.CustomDomains[i]] = coredns.CoreDNSClusterOption{
			IPs:       []string{*pipResp.Properties.IPAddress},
			Namespace: fmt.Sprintf("%s-%s", operator.Namespace, strings.ToLower(random.UniqueId())),
			Domain:    operator.CustomDomains[i],
		}
	}

	// 3) Deploy CoreDNS to each AKS cluster
	kubeConfigPath, err := k8s.KubeConfigPathFromHomeDirE()
	require.NoError(t, err)

	for i, cfg := range azureClusterSetups[:len(r.Clusters)] {
		// 3a) Deploy CoreDNS with proper configuration
		pipName := fmt.Sprintf("coredns-pip-%s", cfg.Region)
		annotations := GetLoadBalancerAnnotations(ProviderAzure)

		// Add Azure-specific annotations
		annotations["service.beta.kubernetes.io/azure-pip-name"] = pipName
		annotations["service.beta.kubernetes.io/azure-load-balancer-resource-group"] = cfg.ResourceGroup

		ips := r.CorednsClusterOptions[operator.CustomDomains[i]].IPs
		var staticIP *string
		if len(ips) > 0 {
			staticIP = ToPtr(ips[0])
		}

		err := DeployCoreDNS(t, cfg.ClusterName, kubeConfigPath, staticIP, ProviderAzure, operator.CustomDomains[i], r.CorednsClusterOptions)
		require.NoError(t, err, "failed to deploy CoreDNS to cluster %s", cfg.ClusterName)
	}

	// 4) Build client map for operator
	r.Clients = make(map[string]client.Client)
	for _, cfg := range azureClusterSetups[:len(r.Clusters)] {
		cfgRest, err := config.GetConfigWithContext(cfg.ClusterName)
		require.NoError(t, err)
		c, err := client.New(cfgRest, client.Options{})
		require.NoError(t, err)
		r.Clients[cfg.ClusterName] = c
	}

	r.ReusingInfra = true
	t.Logf("[%s] Infrastructure setup completed", ProviderAzure)
}

// TeardownInfra deletes AKS clusters, Public IPs, Subnets, VNets, and Resource Group
func (r *AzureRegion) TeardownInfra(t *testing.T) {
	t.Logf("[%s] Starting infrastructure teardown", ProviderAzure)
	ctx := context.Background()
	cred, err := azidentity.NewDefaultAzureCredential(nil)
	require.NoError(t, err)

	rgClient, _ := armresources.NewResourceGroupsClient(azureSubscriptionID, cred, nil)
	vnetClient, _ := armnetwork.NewVirtualNetworksClient(azureSubscriptionID, cred, nil)
	subnetClient, _ := armnetwork.NewSubnetsClient(azureSubscriptionID, cred, nil)
	nsgClient, _ := armnetwork.NewSecurityGroupsClient(azureSubscriptionID, cred, nil)
	pipClient, _ := armnetwork.NewPublicIPAddressesClient(azureSubscriptionID, cred, nil)
	aksClient, _ := armcontainerservice.NewManagedClustersClient(azureSubscriptionID, cred, nil)

	// 1) Delete AKS clusters
	for _, cfg := range azureClusterSetups[:len(r.Clusters)] {
		t.Logf("[%s] Deleting AKS cluster '%s'", ProviderAzure, cfg.ClusterName)
		_, err := aksClient.BeginDelete(ctx, cfg.ResourceGroup, cfg.ClusterName, nil)
		if err != nil && !IsResourceNotFound(err) {
			t.Logf("[%s] Warning: error deleting AKS %s: %v", ProviderAzure, cfg.ClusterName, err)
		}
	}

	// 2) Delete Public IPs
	for _, cfg := range azureClusterSetups[:len(r.Clusters)] {
		pipName := fmt.Sprintf("coredns-pip-%s", cfg.Region)
		t.Logf("[%s] Deleting Public IP '%s'", ProviderAzure, pipName)
		_, err := pipClient.BeginDelete(ctx, cfg.ResourceGroup, pipName, nil)
		if err != nil && !IsResourceNotFound(err) {
			t.Logf("[%s] Warning: error deleting PublicIP %s: %v", ProviderAzure, pipName, err)
		}
	}

	// 3) Delete NSGs
	for _, cfg := range azureClusterSetups[:len(r.Clusters)] {
		nsgName := cfg.SubnetName + "-nsg"
		t.Logf("[%s] Deleting NSG '%s'", ProviderAzure, nsgName)
		_, err := nsgClient.BeginDelete(ctx, cfg.ResourceGroup, nsgName, nil)
		if err != nil && !IsResourceNotFound(err) {
			t.Logf("[%s] Warning: error deleting NSG %s: %v", ProviderAzure, nsgName, err)
		}
	}

	// 4) Delete Subnets
	for _, cfg := range azureClusterSetups[:len(r.Clusters)] {
		t.Logf("[%s] Deleting Subnet '%s'", ProviderAzure, cfg.SubnetName)
		_, err := subnetClient.BeginDelete(ctx, cfg.ResourceGroup, cfg.VNetName, cfg.SubnetName, nil)
		if err != nil && !IsResourceNotFound(err) {
			t.Logf("[%s] Warning: error deleting Subnet %s: %v", ProviderAzure, cfg.SubnetName, err)
		}
	}

	// 5) Delete VNets
	for _, cfg := range azureClusterSetups[:len(r.Clusters)] {
		t.Logf("[%s] Deleting VNet '%s'", ProviderAzure, cfg.VNetName)
		_, err := vnetClient.BeginDelete(ctx, cfg.ResourceGroup, cfg.VNetName, nil)
		if err != nil && !IsResourceNotFound(err) {
			t.Logf("[%s] Warning: error deleting VNet %s: %v", ProviderAzure, cfg.VNetName, err)
		}
	}

	// 6) Delete Resource Group (and everything inside)
	t.Logf("[%s] Deleting Resource Group '%s'", ProviderAzure, commonResourceGroup)
	_, err = rgClient.BeginDelete(ctx, commonResourceGroup, nil)
	if err != nil && !IsResourceNotFound(err) {
		t.Logf("[%s] Warning: error deleting ResourceGroup %s: %v", ProviderAzure, commonResourceGroup, err)
	}

	t.Logf("[%s] Infrastructure teardown completed", ProviderAzure)
}

// --- Helper Functions ---

func createOrUpdateRG(ctx context.Context, client *armresources.ResourceGroupsClient, name, location string) (*armresources.ResourceGroup, error) {
	_, err := client.Get(ctx, name, nil)
	if err == nil {
		return nil, nil
	}
	var respErr *azcore.ResponseError
	if !strings.Contains(strings.ToLower(err.Error()), "notfound") && !errors.As(err, &respErr) {
		return nil, err
	}
	resp, err := client.CreateOrUpdate(ctx, name, armresources.ResourceGroup{Location: ToPtr(location)}, nil)
	if err != nil {
		return nil, err
	}
	return &resp.ResourceGroup, nil
}

func createOrUpdateVNet(ctx context.Context, client *armnetwork.VirtualNetworksClient, rg, name, location, prefix string) (*armnetwork.VirtualNetwork, error) {
	_, err := client.Get(ctx, rg, name, nil)
	if err == nil {
		return nil, nil
	}
	var respErr *azcore.ResponseError
	if !strings.Contains(strings.ToLower(err.Error()), "notfound") && !errors.As(err, &respErr) {
		return nil, err
	}
	poller, err := client.BeginCreateOrUpdate(ctx, rg, name, armnetwork.VirtualNetwork{
		Location: ToPtr(location),
		Properties: &armnetwork.VirtualNetworkPropertiesFormat{
			AddressSpace: &armnetwork.AddressSpace{AddressPrefixes: []*string{ToPtr(prefix)}},
		},
	}, nil)
	if err != nil {
		return nil, err
	}
	resp, err := poller.PollUntilDone(ctx, nil)
	if err != nil {
		return nil, err
	}
	return &resp.VirtualNetwork, nil
}

func createOrUpdateSubnet(ctx context.Context, client *armnetwork.SubnetsClient, rg, vnet, name, prefix string) (*armnetwork.Subnet, error) {
	respOld, err := client.Get(ctx, rg, vnet, name, nil)
	if err == nil {
		return &respOld.Subnet, nil
	}
	var respErr *azcore.ResponseError
	if !strings.Contains(strings.ToLower(err.Error()), "notfound") && !errors.As(err, &respErr) {
		return nil, err
	}
	poller, err := client.BeginCreateOrUpdate(ctx, rg, vnet, name, armnetwork.Subnet{
		Properties: &armnetwork.SubnetPropertiesFormat{
			AddressPrefix: ToPtr(prefix),
		},
	}, nil)
	if err != nil {
		return nil, err
	}
	resp, err := poller.PollUntilDone(ctx, nil)
	if err != nil {
		return nil, err
	}
	return &resp.Subnet, nil
}

func createOrUpdateNSG(ctx context.Context, client *armnetwork.SecurityGroupsClient, rg, name, location string, rules []armnetwork.SecurityRule) (*armnetwork.SecurityGroup, error) {
	respOld, err := client.Get(ctx, rg, name, nil)
	if err == nil {
		return &respOld.SecurityGroup, nil
	}
	var respErr *azcore.ResponseError
	if !strings.Contains(strings.ToLower(err.Error()), "notfound") && !errors.As(err, &respErr) {
		return nil, err
	}
	poller, err := client.BeginCreateOrUpdate(ctx, rg, name, armnetwork.SecurityGroup{
		Location:   ToPtr(location),
		Properties: &armnetwork.SecurityGroupPropertiesFormat{SecurityRules: Pointers(rules)},
	}, nil)
	if err != nil {
		return nil, err
	}
	resp, err := poller.PollUntilDone(ctx, nil)
	if err != nil {
		return nil, err
	}
	return &resp.SecurityGroup, nil
}

func associateNSG(ctx context.Context, client *armnetwork.SubnetsClient, rg, vnet, subnetName, nsgID string) error {
	subnetResp, err := client.Get(ctx, rg, vnet, subnetName, nil)
	if err != nil {
		return err
	}
	subnet := subnetResp.Subnet
	subnet.Properties.NetworkSecurityGroup = &armnetwork.SecurityGroup{ID: ToPtr(nsgID)}
	poller, err := client.BeginCreateOrUpdate(ctx, rg, vnet, subnetName, armnetwork.Subnet{
		Properties: subnet.Properties,
	}, nil)
	if err != nil {
		return err
	}
	_, err = poller.PollUntilDone(ctx, nil)
	return err
}

func ensurePublicIP(ctx context.Context, client *armnetwork.PublicIPAddressesClient, rg, region, name string) (*armnetwork.PublicIPAddress, error) {
	respOld, err := client.Get(ctx, rg, name, nil)
	if err == nil {
		return &respOld.PublicIPAddress, nil
	}
	var respErr *azcore.ResponseError
	if !strings.Contains(strings.ToLower(err.Error()), "notfound") && !errors.As(err, &respErr) {
		return nil, err
	}
	poller, err := client.BeginCreateOrUpdate(ctx, rg, name, armnetwork.PublicIPAddress{
		Location: ToPtr(region),
		Properties: &armnetwork.PublicIPAddressPropertiesFormat{
			PublicIPAllocationMethod: ToPtr(armnetwork.IPAllocationMethodStatic),
		},
		SKU:  &armnetwork.PublicIPAddressSKU{Name: ToPtr(armnetwork.PublicIPAddressSKUNameStandard)},
		Tags: map[string]*string{defaultNodeTagKey: ToPtr(defaultNodeTagValue)},
	}, nil)
	if err != nil {
		return nil, err
	}
	resp, err := poller.PollUntilDone(ctx, nil)
	if err != nil {
		return nil, err
	}
	return &resp.PublicIPAddress, nil
}

func createOrUpdateAKS(ctx context.Context, client *armcontainerservice.ManagedClustersClient, cfg AzureClusterSetupConfig, subnetID string) (*armcontainerservice.ManagedCluster, error) {
	_, err := client.Get(ctx, cfg.ResourceGroup, cfg.ClusterName, nil)
	if err == nil {
		return nil, nil
	}
	var respErr *azcore.ResponseError
	if !strings.Contains(strings.ToLower(err.Error()), "notfound") && !errors.As(err, &respErr) {
		return nil, err
	}
	k8sVer := cfg.ServiceCIDR // placeholder; we let AKS pick default if left empty
	if defaultK8sVersion != "" {
		k8sVer = defaultK8sVersion
	}
	agentPool := armcontainerservice.ManagedClusterAgentPoolProfile{
		Name:               ToPtr("agentpool"),
		Count:              ToPtr[int32](DefaultNodeCount),
		VMSize:             ToPtr(AzureDefaultVMSize),
		OSDiskSizeGB:       ToPtr[int32](128),
		OSType:             ToPtr(armcontainerservice.OSTypeLinux),
		Mode:               ToPtr(armcontainerservice.AgentPoolModeSystem),
		Type:               ToPtr(armcontainerservice.AgentPoolTypeVirtualMachineScaleSets),
		VnetSubnetID:       ToPtr(subnetID),
		AvailabilityZones:  Pointers(cfg.AvailabilityZones),
		EnableNodePublicIP: ToPtr(false),
	}
	networkProfile := &armcontainerservice.NetworkProfile{
		NetworkPlugin: ToPtr(armcontainerservice.NetworkPluginAzure),
		PodCidr:       ToPtr(cfg.PodCIDR),
		ServiceCidr:   ToPtr(cfg.ServiceCIDR),
		DNSServiceIP:  calcDNSIP(cfg.ServiceCIDR),
	}
	managedCluster := armcontainerservice.ManagedCluster{
		Location: ToPtr(cfg.Region),
		Properties: &armcontainerservice.ManagedClusterProperties{
			DNSPrefix:              ToPtr(cfg.DNSPrefix),
			KubernetesVersion:      ToPtr(k8sVer),
			AgentPoolProfiles:      []*armcontainerservice.ManagedClusterAgentPoolProfile{&agentPool},
			NetworkProfile:         networkProfile,
			APIServerAccessProfile: &armcontainerservice.ManagedClusterAPIServerAccessProfile{AuthorizedIPRanges: []*string{ToPtr("0.0.0.0/0")}},
			EnableRBAC:             ToPtr(true),
			AutoUpgradeProfile:     &armcontainerservice.ManagedClusterAutoUpgradeProfile{UpgradeChannel: ToPtr(armcontainerservice.UpgradeChannelPatch)},
		},
		Tags: map[string]*string{defaultNodeTagKey: ToPtr(defaultNodeTagValue)},
	}
	poller, err := client.BeginCreateOrUpdate(ctx, cfg.ResourceGroup, cfg.ClusterName, managedCluster, nil)
	if err != nil {
		return nil, err
	}
	resp, err := poller.PollUntilDone(ctx, nil)
	if err != nil {
		return nil, err
	}
	return &resp.ManagedCluster, nil
}

func calcDNSIP(serviceCIDR string) *string {
	octets := strings.Split(strings.Split(serviceCIDR, "/")[0], ".")
	if len(octets) == 4 {
		return ToPtr(fmt.Sprintf("%s.%s.0.10", octets[0], octets[1]))
	}
	return nil
}
