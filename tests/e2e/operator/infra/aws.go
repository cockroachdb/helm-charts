// Package infra provides AWS infrastructure provisioning and management for e2e operator tests.
// It handles EKS cluster creation, networking setup across multiple regions, and resource cleanup
// using eksctl and the AWS SDK. The implementation supports multi-region testing with cross-region
// connectivity via VPC peering between region-specific VPCs.
package infra

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/eks"
	"github.com/aws/aws-sdk-go/service/sts"
	"github.com/go-logr/logr"
	"github.com/gruntwork-io/terratest/modules/k8s"
	"github.com/gruntwork-io/terratest/modules/random"
	"github.com/stretchr/testify/require"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/cockroachdb/helm-charts/tests/e2e/coredns"
	"github.com/cockroachdb/helm-charts/tests/e2e/operator"
)

// ─── AWS CONSTANTS ───────────────────────────────────────────────────────────────

const (
	awsVPCName             = "cockroachdb-vpc"
	awsWebhookSGName       = "allow-9443-port-for-webhook"
	awsInternalSGName      = "allow-internal"
	awsSubnetSuffix        = "subnet"
	awsIGWSuffix           = "igw"
	awsRouteTableSuffix    = "rtb"
	awsDefaultInstanceType = "m5.xlarge"
	awsDefaultDiskSize     = 30
	awsDefaultNodesPerAZ   = 1
	awsDefaultK8sVersion   = "1.35"

	// EnvKubectlInsecureSkipTLSVerify is the environment variable name used to control
	// whether TLS certificate verification should be skipped for kubectl and Kubernetes clients.
	// Set to "true" to skip TLS verification (useful for corporate proxy environments).
	EnvKubectlInsecureSkipTLSVerify = "KUBECTL_INSECURE_SKIP_TLS_VERIFY"
)

// ─── MULTI-REGION CONFIGURATION ─────────────────────────────────────────────────
// Centralized configuration for multi-region testing
// Modifies these constants to change regions/CIDRs across the entire test suite

const (
	// Primary region (matches GCP us-east1 geographically)
	awsRegionPrimary  = "us-east-1" // Virginia
	awsVPCCIDRPrimary = "172.28.112.0/24"
	awsPodCIDRPrimary = "172.28.0.0/20"

	// Secondary region (matches GCP us-central1 geographically)
	awsRegionSecondary  = "us-east-2" // Ohio - closer to primary, lower latency
	awsVPCCIDRSecondary = "172.28.144.0/24"
	awsPodCIDRSecondary = "172.28.48.0/20"

	// Optional tertiary region (matches GCP us-west1 geographically)
	awsRegionTertiary  = "us-west-2" // Oregon
	awsVPCCIDRTertiary = "172.28.176.0/24"
	awsPodCIDRTertiary = "172.28.96.0/20"
)

// AWSClusterSetupConfig holds per-cluster network & EKS settings
type AWSClusterSetupConfig struct {
	Region            string
	SubnetRanges      []string // One per AZ
	SubnetIDs         []string // Populated by createVPCAndSubnets after subnet creation
	ClusterName       string
	AvailabilityZones []string // Populated by createVPCAndSubnets based on available AZs in a region
}

// Pre-defined configs for each region; index order matches r.Clusters
// Uses centralized constants defined above for easy modification
var awsClusterConfigurations = []AWSClusterSetupConfig{
	{
		Region:       awsRegionPrimary,
		SubnetRanges: []string{"172.28.112.0/26", "172.28.112.64/26", "172.28.112.128/26"},
		ClusterName:  "cockroachdb-east",
	},
	{
		Region:       awsRegionSecondary,
		SubnetRanges: []string{"172.28.144.0/26", "172.28.144.64/26", "172.28.144.128/26"},
		ClusterName:  "cockroachdb-central",
	},
	{
		Region:       awsRegionTertiary,
		SubnetRanges: []string{"172.28.176.0/26", "172.28.176.64/26", "172.28.176.128/26"},
		ClusterName:  "cockroachdb-west",
	},
}

// kubeconfigMutex protects concurrent kubeconfig file updates to prevent corruption
var kubeconfigMutex sync.Mutex

// AwsRegion wraps operator.Region and manages AWS infrastructure for multi-region testing.
// Tracks VPCs, security groups, route tables, and VPC peering connections across regions.
type AwsRegion struct {
	*operator.Region
	vpcName               string
	securityGroupIDs      map[string]string // Map of security group names to IDs
	vpcPeeringConnections []string          // Tracked VPC peering connection IDs for cleanup
	routeTableIDs         map[string]string // Map of region-to-route table ID (used for VPC peering routes)
	clusterConfigs        []AWSClusterSetupConfig // Instance-level copy to avoid shared mutable state
}

// getResourceTags returns standard tags for AWS resources
func (r *AwsRegion) getResourceTags(resourceName string) []*ec2.Tag {
	return []*ec2.Tag{
		{Key: aws.String("Name"), Value: aws.String(resourceName)},
		{Key: aws.String("ManagedBy"), Value: aws.String("helm-charts-e2e")},
		{Key: aws.String("TestRunID"), Value: aws.String(r.TestRunID)},
	}
}

// SetUpInfra provisions complete AWS infrastructure for end-to-end testing.
// Performs a 7-step setup process (~25-30 minutes total):
// 1. Initialize AWS Sessions - Creates SDK sessions, EC2/EKS clients, logs TestRunID for concurrent test isolation
// 2. Create VPC and Internet Gateways - VPC with region-specific CIDRs, IGW, tagged with ManagedBy=helm-charts-e2e and TestRunID
// 3. Create Subnets and Route Tables - 3 subnets across AZs, route table with default route (0.0.0.0/0 → IGW)
// 3b. Create Security Groups - Webhook SG (TCP 9443 from VPC), Internal SG (all traffic within VPC and from peer VPCs/Pods)
// 4. Create VPC Peering - Mesh topology between regions, add VPC+Pod CIDR routes, update security groups for cross-region traffic
// 5. Create EKS Clusters (PARALLEL) - eksctl creates CloudFormation stacks (control plane and node group), verifies TLS connectivity (~20 min parallel vs. 60 min serial)
// 6. Deploy and Configure CoreDNS - Deploys ConfigMap/RBAC/Deployment, creates NLB service, updates cross-cluster DNS configuration
//
// If ReusingInfra is true, returns immediately without creating resources.
// All resources are tagged with ManagedBy and TestRunID for cleanup and concurrent test isolation.
func (r *AwsRegion) SetUpInfra(t *testing.T) {
	if r.ReusingInfra {
		t.Logf("[%s] Reusing existing infrastructure", ProviderAWS)
		return
	}

	// Initialize instance-level cluster configs to avoid shared mutable state
	r.clusterConfigs = make([]AWSClusterSetupConfig, len(awsClusterConfigurations))
	copy(r.clusterConfigs, awsClusterConfigurations)

	ctx := context.Background()

	// 1) Create AWS sessions for each region
	sessions := make(map[string]*session.Session)
	ec2Clients := make(map[string]*ec2.EC2)
	eksClients := make(map[string]*eks.EKS)

	for i := range r.Clusters {
		region := r.clusterConfigs[i].Region
		if _, exists := sessions[region]; !exists {
			sess, err := createAWSSession(region)
			require.NoError(t, err, "failed to create AWS session for region %s", region)
			sessions[region] = sess
			ec2Clients[region] = ec2.New(sess)
			eksClients[region] = eks.New(sess)
		}
	}

	// 2) Create VPC in each region
	r.vpcName = fmt.Sprintf("%s-%s", awsVPCName, strings.ToLower(random.UniqueId()))
	r.securityGroupIDs = make(map[string]string)
	r.routeTableIDs = make(map[string]string)
	r.vpcPeeringConnections = make([]string, 0)

	// Log the test run ID for troubleshooting
	t.Logf("[%s] Using Test Run ID: %s (for concurrent test isolation)", ProviderAWS, r.TestRunID)

	vpcIDs := make(map[string]string)
	igwIDs := make(map[string]string)

	for region, ec2Client := range ec2Clients {
		vpcID, err := r.createVPC(ctx, t, ec2Client, region)
		require.NoError(t, err, "failed to create VPC in region %s", region)
		vpcIDs[region] = vpcID

		// Create Internet Gateway for public access
		igwID, err := r.createInternetGateway(ctx, t, ec2Client, vpcID, region)
		require.NoError(t, err, "failed to create Internet Gateway in region %s", region)
		igwIDs[region] = igwID
	}

	// 3) Create subnets in each region
	for i, clusterName := range r.Clusters {
		setup := &r.clusterConfigs[i]
		setup.ClusterName = clusterName
		region := setup.Region
		ec2Client := ec2Clients[region]
		vpcID := vpcIDs[region]

		// Discover availability zones
		azs, err := r.discoverAvailabilityZones(ctx, ec2Client, region)
		require.NoError(t, err, "failed to discover AZs in region %s", region)

		// Use first 3 AZs
		if len(azs) > 3 {
			azs = azs[:3]
		}
		setup.AvailabilityZones = azs

		// Create subnets
		subnetIDs, err := r.createSubnets(ctx, t, ec2Client, vpcID, setup)
		require.NoError(t, err, "failed to create subnets in region %s", region)
		setup.SubnetIDs = subnetIDs

		// Create a route table and associate with subnets
		err = r.createRouteTable(ctx, t, ec2Client, vpcID, igwIDs[region], subnetIDs, region)
		require.NoError(t, err, "failed to create route table in region %s", region)
	}

	// 3b) Create security groups BEFORE VPC peering (peering setup updates SG rules for cross-region traffic)
	for region, ec2Client := range ec2Clients {
		vpcID := vpcIDs[region]
		err := r.createSecurityGroups(ctx, t, ec2Client, vpcID, region)
		require.NoError(t, err, "failed to create security groups in region %s", region)
	}

	// 4) Create VPC Peering Connections between regions (enables cross-region pod communication).
	// This must happen AFTER route tables and security groups are created, so routes and SG rules can be added.
	// Creates a mesh topology where all VPCs can communicate with each other.
	// Similar to GCP's single global VPC model.
	err := r.createVPCPeeringConnections(ctx, t, ec2Clients, vpcIDs)
	require.NoError(t, err, "failed to create VPC peering connections")

	// 5) Create EKS clusters in parallel
	err = r.createEKSClusters(ctx, t, eksClients)
	require.NoError(t, err, "failed to create EKS clusters")
	r.ReusingInfra = true

	// 6) Deploy CoreDNS with initial configuration, then update with complete cross-cluster info
	kubeConfigPath, err := r.EnsureKubeConfigPath()
	require.NoError(t, err)
	err = r.deployAndConfigureCoreDNS(t, kubeConfigPath)
	require.NoError(t, err, "failed to deploy and configure CoreDNS")
}

// TeardownInfra deletes all AWS resources created by SetUpInfra in reverse dependency order.
//
// Performs a cleanup process with retry logic (~8-10 minutes total):
//
// 1. Delete EKS Clusters (PARALLEL) - eksctl delete cluster --force --wait, deletes CloudFormation stacks (~10 min parallel)
// 1b. Delete IAM Roles - Detaches policies and deletes IAM roles created for EBS CSI driver
// 2. Wait 120 Seconds + Delete Cluster Resources (PARALLEL) - Waits for resource release, then deletes Load Balancers, NAT Gateways, ENIs, Elastic IPs, VPC Endpoints concurrently
// 3. Revoke Security Group Rules - Revokes all ingress/egress rules including self-referencing to break circular dependencies
// 4. Delete Security Groups (3 retries, 30s delay) - Handles AWS eventual consistency, retries if resources are still attached (up to 90s)
// 5. Delete Subnets and Route Tables (3 retries, 30s delay) - Disassociates and deletes, retries for ENI detachment (up to 90s)
// 6. Delete Internet Gateways - Detaches from VPCs and deletes, queries by ManagedBy and TestRunID tags
// 6b. Delete VPC Peering Connections - Deletes tracked peering connections and searches by TestRunID tag (must be before VPC deletion)
// 7. Delete VPCs (3 retries, 20s delay) - Final cleanup, retries for dependency clearing (up to 60s)
//
// Error Handling: All steps wrapped in safeExecute() which catches panics, logs errors, and continues cleanup.
// Retry Strategy: Security groups (3×30s), Subnets/route tables (3×30s), VPCs (3×20s) to handle AWS eventual consistency.
// Concurrent Test Isolation: All resource queries filter by TestRunID to delete only current test's resources.
// Designed to be called via t.Cleanup() for guaranteed execution even on test failure, timeout, or panic.
func (r *AwsRegion) TeardownInfra(t *testing.T) {
	defer func() {
		if rec := recover(); rec != nil {
			t.Logf("[%s] Panic during teardown (continuing cleanup): %v", ProviderAWS, rec)
		}
	}()

	t.Logf("[%s] Starting infrastructure teardown", ProviderAWS)

	// Initialize instance-level cluster configs if not already set
	if r.clusterConfigs == nil {
		r.clusterConfigs = make([]AWSClusterSetupConfig, len(awsClusterConfigurations))
		copy(r.clusterConfigs, awsClusterConfigurations)
	}

	ctx := context.Background()

	// Create AWS sessions for each region
	sessions := make(map[string]*session.Session)
	ec2Clients := make(map[string]*ec2.EC2)
	eksClients := make(map[string]*eks.EKS)

	for i := range r.Clusters {
		region := r.clusterConfigs[i].Region
		if _, exists := sessions[region]; !exists {
			sess, err := createAWSSession(region)
			if err != nil {
				t.Logf("[%s] Warning: failed to create AWS session for region %s: %v", ProviderAWS, region, err)
				continue
			}
			sessions[region] = sess
			ec2Clients[region] = ec2.New(sess)
			eksClients[region] = eks.New(sess)
		}
	}

	safeExecute := func(stepName string, fn func() error) {
		defer func() {
			if rec := recover(); rec != nil {
				t.Logf("[%s] Panic during %s (continuing teardown): %v", ProviderAWS, stepName, rec)
			}
		}()

		if err := fn(); err != nil {
			t.Logf("[%s] Warning: %s failed: %v", ProviderAWS, stepName, err)
		}
	}

	// 1) Delete EKS clusters
	//    - Deletes EBS CSI driver addons first (prevents orphaned addon resources)
	//    - Then deletes clusters via eksctl (handles nodegroups, CloudFormation stacks)
	safeExecute("EKS cluster deletion", func() error {
		return r.deleteEKSClusters(ctx, t)
	})

	// 1b) Delete IAM roles and OIDC providers
	//     - Deletes IAM roles created for EBS CSI driver
	//     - Deletes OIDC identity providers (persist after cluster deletion, accumulate over time)
	safeExecute("IAM role deletion", func() error {
		return r.deleteIAMRoles(t)
	})

	// Wait for cluster resources to be fully released
	// Cluster deletion is async - ENIs, load balancers, etc. take time to clean up
	t.Logf("[%s] Waiting for cluster resources to be released (this may take 2-3 minutes)...", ProviderAWS)
	time.Sleep(120 * time.Second)

	// 2) Delete cluster-related resources in parallel (Load Balancers, NAT Gateways, ENIs, Elastic IPs, VPC Endpoints)
	safeExecute("cluster resource cleanup", func() error {
		return r.deleteClusterResources(ctx, t, ec2Clients)
	})

	// 3) Revoke Security Group rules (self-referencing rules block deletion)
	safeExecute("Security Group rule revocation", func() error {
		return retryWithBackoff(t, "security group rules", 3, 30*time.Second, func() error {
			return r.revokeSecurityGroupRules(ctx, t, ec2Clients)
		})
	})

	// 4) Delete security groups (retry for AWS eventual consistency)
	safeExecute("security group deletion", func() error {
		return retryWithBackoff(t, "security group", 3, 30*time.Second, func() error {
			return r.deleteSecurityGroups(ctx, t, ec2Clients)
		})
	})

	// 5) Delete route tables and subnets (retry for ENI detachment)
	safeExecute("route table and subnet deletion", func() error {
		return retryWithBackoff(t, "subnet", 3, 30*time.Second, func() error {
			return r.deleteSubnetsAndRouteTables(ctx, t, ec2Clients)
		})
	})

	// 6) Delete Internet Gateways
	safeExecute("Internet Gateway deletion", func() error {
		return r.deleteInternetGateways(ctx, t, ec2Clients)
	})

	// 6b) Delete VPC Peering Connections (must be before VPC deletion)
	safeExecute("VPC peering deletion", func() error {
		return r.deleteVPCPeeringConnections(ctx, t, ec2Clients)
	})

	// 7) Delete VPCs (retry for dependency cleanup)
	safeExecute("VPC deletion", func() error {
		return retryWithBackoff(t, "VPC", 3, 20*time.Second, func() error {
			err := r.deleteVPCs(ctx, t, ec2Clients)
			if err != nil {
				t.Logf("[%s] VPC deletion failed, will retry if attempts remain", ProviderAWS)
			}
			return err
		})
	})

	t.Logf("[%s] Infrastructure teardown completed", ProviderAWS)
}

// ScaleNodePool scales the node pool for an EKS cluster
func (r *AwsRegion) ScaleNodePool(t *testing.T, region string, nodeCount, index int) {
	if index >= len(r.Clusters) {
		t.Fatalf("[%s] Invalid cluster index %d, only have %d clusters", ProviderAWS, index, len(r.Clusters))
	}

	clusterName := r.Clusters[index]
	nodegroupName := fmt.Sprintf("%s-nodegroup", clusterName)

	t.Logf("[%s] Scaling node pool '%s' in cluster '%s' (region: %s) to %d nodes",
		ProviderAWS, nodegroupName, clusterName, region, nodeCount)

	// Scale the nodegroup using eksctl
	// Also update max nodes to allow scaling beyond the initial maximum
	cmd := exec.Command("eksctl", "scale", "nodegroup",
		"--cluster", clusterName,
		"--name", nodegroupName,
		"--nodes", fmt.Sprint(nodeCount),
		"--nodes-max", fmt.Sprint(nodeCount+1), // Allow future scaling
		"--region", region,
	)

	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("[%s] Failed to scale nodegroup: %v\nOutput: %s", ProviderAWS, err, string(output))
	}

	t.Logf("[%s] Successfully scaled nodegroup. Output: %s", ProviderAWS, string(output))

	// Wait for nodes to become ready
	t.Logf("[%s] Waiting for nodes to become ready...", ProviderAWS)
	maxRetries := 30
	retryInterval := 10 * time.Second

	for i := 0; i < maxRetries; i++ {
		// Get kubeconfig and check node count
		kubeconfigCmd := exec.Command("kubectl", "get", "nodes", "--context", clusterName, "--no-headers")
		output, err := kubeconfigCmd.CombinedOutput()
		if err != nil {
			t.Logf("[%s] Retry %d/%d: Error checking nodes: %v", ProviderAWS, i+1, maxRetries, err)
			time.Sleep(retryInterval)
			continue
		}

		// Count ready nodes
		lines := strings.Split(strings.TrimSpace(string(output)), "\n")
		readyCount := 0
		for _, line := range lines {
			if strings.Contains(line, "Ready") && !strings.Contains(line, "NotReady") {
				readyCount++
			}
		}

		t.Logf("[%s] Ready nodes: %d/%d", ProviderAWS, readyCount, nodeCount)

		if readyCount >= nodeCount {
			t.Logf("[%s] All %d nodes are ready", ProviderAWS, nodeCount)
			return
		}

		time.Sleep(retryInterval)
	}

	t.Fatalf("[%s] Timed out waiting for nodes to become ready after scaling", ProviderAWS)
}

func (r *AwsRegion) CanScale() bool {
	return true
}

// ─── AWS CLIENT CREATORS ────────────────────────────────────────────────────────

func createAWSSession(region string) (*session.Session, error) {
	return session.NewSession(&aws.Config{
		Region: aws.String(region),
	})
}

// ─── RESOURCE CREATION ──────────────────────────────────────────────────────────

func (r *AwsRegion) createVPC(
	ctx context.Context, t *testing.T, client *ec2.EC2, region string,
) (string, error) {
	vpcName := fmt.Sprintf("%s-%s", r.vpcName, region)

	// Get region-specific VPC CIDR to avoid overlaps (required for VPC peering)
	vpcCIDR := r.getVPCCIDR(region)

	// Create VPC
	createVpcOutput, err := client.CreateVpcWithContext(ctx, &ec2.CreateVpcInput{
		CidrBlock: aws.String(vpcCIDR),
		TagSpecifications: []*ec2.TagSpecification{
			{
				ResourceType: aws.String("vpc"),
				Tags:         r.getResourceTags(vpcName),
			},
		},
	})
	if err != nil {
		return "", err
	}

	vpcID := *createVpcOutput.Vpc.VpcId
	t.Logf("[%s] Created VPC %s in region %s", ProviderAWS, vpcID, region)

	// Enable DNS support (required for VPC peering)
	_, err = client.ModifyVpcAttributeWithContext(ctx, &ec2.ModifyVpcAttributeInput{
		VpcId:            aws.String(vpcID),
		EnableDnsSupport: &ec2.AttributeBooleanValue{Value: aws.Bool(true)},
	})
	if err != nil {
		t.Logf("[%s] Warning: failed to enable DNS support for VPC %s: %v", ProviderAWS, vpcID, err)
	}

	// Enable DNS hostnames (required for VPC peering)
	_, err = client.ModifyVpcAttributeWithContext(ctx, &ec2.ModifyVpcAttributeInput{
		VpcId:              aws.String(vpcID),
		EnableDnsHostnames: &ec2.AttributeBooleanValue{Value: aws.Bool(true)},
	})
	if err != nil {
		t.Logf("[%s] Warning: failed to enable DNS hostnames for VPC %s: %v", ProviderAWS, vpcID, err)
	}

	return vpcID, nil
}

func (r *AwsRegion) createInternetGateway(
	ctx context.Context, t *testing.T, client *ec2.EC2, vpcID, region string,
) (string, error) {
	igwName := fmt.Sprintf("%s-%s-%s", r.vpcName, awsIGWSuffix, region)

	createIgwOutput, err := client.CreateInternetGatewayWithContext(ctx, &ec2.CreateInternetGatewayInput{
		TagSpecifications: []*ec2.TagSpecification{
			{
				ResourceType: aws.String("internet-gateway"),
				Tags:         r.getResourceTags(igwName),
			},
		},
	})
	if err != nil {
		return "", err
	}

	igwID := *createIgwOutput.InternetGateway.InternetGatewayId
	t.Logf("[%s] Created Internet Gateway %s in region %s", ProviderAWS, igwID, region)

	// Attach to VPC
	_, err = client.AttachInternetGatewayWithContext(ctx, &ec2.AttachInternetGatewayInput{
		InternetGatewayId: aws.String(igwID),
		VpcId:             aws.String(vpcID),
	})
	if err != nil {
		return "", err
	}

	return igwID, nil
}

func (r *AwsRegion) discoverAvailabilityZones(
	ctx context.Context, client *ec2.EC2, region string,
) ([]string, error) {
	output, err := client.DescribeAvailabilityZonesWithContext(ctx, &ec2.DescribeAvailabilityZonesInput{
		Filters: []*ec2.Filter{
			{
				Name:   aws.String("region-name"),
				Values: []*string{aws.String(region)},
			},
			{
				Name:   aws.String("state"),
				Values: []*string{aws.String("available")},
			},
		},
	})
	if err != nil {
		return nil, err
	}

	var azs []string
	for _, az := range output.AvailabilityZones {
		azs = append(azs, *az.ZoneName)
	}

	return azs, nil
}

func (r *AwsRegion) createSubnets(
	ctx context.Context, t *testing.T, client *ec2.EC2, vpcID string, setup *AWSClusterSetupConfig,
) ([]string, error) {
	var subnetIDs []string

	for i, az := range setup.AvailabilityZones {
		if i >= len(setup.SubnetRanges) {
			break
		}

		subnetName := fmt.Sprintf("%s-%s-%s", setup.ClusterName, awsSubnetSuffix, az)
		tags := r.getResourceTags(subnetName)
		tags = append(tags, &ec2.Tag{
			Key:   aws.String(fmt.Sprintf("kubernetes.io/cluster/%s", setup.ClusterName)),
			Value: aws.String("shared"),
		})
		createSubnetOutput, err := client.CreateSubnetWithContext(ctx, &ec2.CreateSubnetInput{
			VpcId:            aws.String(vpcID),
			CidrBlock:        aws.String(setup.SubnetRanges[i]),
			AvailabilityZone: aws.String(az),
			TagSpecifications: []*ec2.TagSpecification{
				{
					ResourceType: aws.String("subnet"),
					Tags:         tags,
				},
			},
		})
		if err != nil {
			return nil, err
		}

		subnetID := *createSubnetOutput.Subnet.SubnetId
		subnetIDs = append(subnetIDs, subnetID)
		t.Logf("[%s] Created subnet %s in AZ %s", ProviderAWS, subnetID, az)

		// Enable auto-assign public IP
		_, err = client.ModifySubnetAttributeWithContext(ctx, &ec2.ModifySubnetAttributeInput{
			SubnetId:            aws.String(subnetID),
			MapPublicIpOnLaunch: &ec2.AttributeBooleanValue{Value: aws.Bool(true)},
		})
		if err != nil {
			t.Logf("[%s] Warning: failed to enable auto-assign public IP for subnet %s: %v", ProviderAWS, subnetID, err)
		}
	}

	return subnetIDs, nil
}

func (r *AwsRegion) createRouteTable(
	ctx context.Context,
	t *testing.T,
	client *ec2.EC2,
	vpcID, igwID string,
	subnetIDs []string,
	region string,
) error {
	rtbName := fmt.Sprintf("%s-%s-%s", r.vpcName, awsRouteTableSuffix, region)

	createRtbOutput, err := client.CreateRouteTableWithContext(ctx, &ec2.CreateRouteTableInput{
		VpcId: aws.String(vpcID),
		TagSpecifications: []*ec2.TagSpecification{
			{
				ResourceType: aws.String("route-table"),
				Tags:         r.getResourceTags(rtbName),
			},
		},
	})
	if err != nil {
		return err
	}

	rtbID := *createRtbOutput.RouteTable.RouteTableId
	t.Logf("[%s] Created route table %s in region %s", ProviderAWS, rtbID, region)

	// Store route table ID for later use (VPC peering routes)
	r.routeTableIDs[region] = rtbID

	// Create a route to Internet Gateway
	_, err = client.CreateRouteWithContext(ctx, &ec2.CreateRouteInput{
		RouteTableId:         aws.String(rtbID),
		DestinationCidrBlock: aws.String("0.0.0.0/0"),
		GatewayId:            aws.String(igwID),
	})
	if err != nil {
		return err
	}

	// Associate route table with subnets
	for _, subnetID := range subnetIDs {
		_, err = client.AssociateRouteTableWithContext(ctx, &ec2.AssociateRouteTableInput{
			RouteTableId: aws.String(rtbID),
			SubnetId:     aws.String(subnetID),
		})
		if err != nil {
			return err
		}
	}

	return nil
}

func (r *AwsRegion) createSecurityGroups(
	ctx context.Context, t *testing.T, client *ec2.EC2, vpcID, region string,
) error {
	// Create a webhook security group (port 9443)
	webhookSGName := fmt.Sprintf("%s-%s-%s", r.vpcName, awsWebhookSGName, region)
	webhookSG, err := client.CreateSecurityGroupWithContext(ctx, &ec2.CreateSecurityGroupInput{
		GroupName:   aws.String(webhookSGName),
		Description: aws.String("Allow port 9443 for webhook"),
		VpcId:       aws.String(vpcID),
		TagSpecifications: []*ec2.TagSpecification{
			{
				ResourceType: aws.String("security-group"),
				Tags:         r.getResourceTags(webhookSGName),
			},
		},
	})
	if err != nil {
		return err
	}

	webhookSGID := *webhookSG.GroupId
	r.securityGroupIDs[webhookSGName] = webhookSGID
	t.Logf("[%s] Created security group %s for webhook", ProviderAWS, webhookSGID)

	// Add ingress rule for port 9443
	_, err = client.AuthorizeSecurityGroupIngressWithContext(ctx, &ec2.AuthorizeSecurityGroupIngressInput{
		GroupId: aws.String(webhookSGID),
		IpPermissions: []*ec2.IpPermission{
			{
				IpProtocol: aws.String("tcp"),
				FromPort:   aws.Int64(9443),
				ToPort:     aws.Int64(9443),
				IpRanges:   []*ec2.IpRange{{CidrIp: aws.String(DefaultVPCCIDR)}},
			},
		},
	})
	if err != nil {
		t.Logf("[%s] Warning: failed to add ingress rule to webhook security group: %v", ProviderAWS, err)
	}

	// Create an internal security group (all traffic within VPC)
	internalSGName := fmt.Sprintf("%s-%s-%s", r.vpcName, awsInternalSGName, region)
	internalSG, err := client.CreateSecurityGroupWithContext(ctx, &ec2.CreateSecurityGroupInput{
		GroupName:   aws.String(internalSGName),
		Description: aws.String("Allow all internal traffic"),
		VpcId:       aws.String(vpcID),
		TagSpecifications: []*ec2.TagSpecification{
			{
				ResourceType: aws.String("security-group"),
				Tags:         r.getResourceTags(internalSGName),
			},
		},
	})
	if err != nil {
		return err
	}

	internalSGID := *internalSG.GroupId
	r.securityGroupIDs[internalSGName] = internalSGID
	t.Logf("[%s] Created security group %s for internal traffic", ProviderAWS, internalSGID)

	// Add ingress rules for internal traffic
	_, err = client.AuthorizeSecurityGroupIngressWithContext(ctx, &ec2.AuthorizeSecurityGroupIngressInput{
		GroupId: aws.String(internalSGID),
		IpPermissions: []*ec2.IpPermission{
			{
				IpProtocol: aws.String("-1"), // All protocols
				IpRanges:   []*ec2.IpRange{{CidrIp: aws.String(DefaultVPCCIDR)}},
			},
		},
	})
	if err != nil {
		t.Logf("[%s] Warning: failed to add ingress rule to internal security group: %v", ProviderAWS, err)
	}

	return nil
}

// createVPCPeeringConnections creates VPC peering connections between all regions in a mesh topology.
// For each pair of regions, it:
// 1. Creates the peering connection from requester region
// 2. Accepts the connection in acceptor region
// 3. Waits for a connection to become active
// 4. Adds routes for both VPC CIDRs and Pod CIDRs through peering connection
// 5. Updates security groups to allow traffic from peer VPC and Pod CIDRs
//
// This enables cross-region pod communication, similar to GCP's single global VPC model.
// Requires route tables to be created before calling (uses r.routeTableIDs).
func (r *AwsRegion) createVPCPeeringConnections(
	ctx context.Context, t *testing.T, ec2Clients map[string]*ec2.EC2, vpcIDs map[string]string,
) error {
	// Get all unique regions
	regions := make([]string, 0, len(vpcIDs))
	for region := range vpcIDs {
		regions = append(regions, region)
	}

	// Create peering connections in a mesh topology
	// For 2 regions: A <-> B
	// For 3 regions: A <-> B, A <-> C, B <-> C
	for i := 0; i < len(regions); i++ {
		for j := i + 1; j < len(regions); j++ {
			regionA := regions[i]
			regionB := regions[j]
			vpcA := vpcIDs[regionA]
			vpcB := vpcIDs[regionB]

			// Create a peering connection from region A to region B
			peeringConnID, err := r.createVPCPeeringConnection(ctx, t, ec2Clients[regionA], vpcA, vpcB, regionA, regionB)
			if err != nil {
				return fmt.Errorf("failed to create VPC peering between %s and %s: %w", regionA, regionB, err)
			}
			r.vpcPeeringConnections = append(r.vpcPeeringConnections, peeringConnID)

			// Give AWS a moment to establish the peering connection before accepting
			time.Sleep(5 * time.Second)

			// Accept the peering connection in region B
			err = r.acceptVPCPeeringConnection(ctx, t, ec2Clients[regionB], peeringConnID, regionB)
			if err != nil {
				return fmt.Errorf("failed to accept VPC peering in %s: %w", regionB, err)
			}

			// Wait for the peering connection to be active
			err = r.waitForVPCPeeringActive(ctx, t, ec2Clients[regionA], peeringConnID)
			if err != nil {
				return fmt.Errorf("failed waiting for VPC peering to be active: %w", err)
			}

			// Update route tables in both regions to route through the peering connection
			err = r.addPeeringRoutes(ctx, t, ec2Clients, peeringConnID, regionA, regionB)
			if err != nil {
				return fmt.Errorf("failed to add peering routes: %w", err)
			}

			// Update security groups to allow cross-VPC traffic
			err = r.updateSecurityGroupsForPeering(ctx, t, ec2Clients, regionA, regionB)
			if err != nil {
				return fmt.Errorf("failed to update security groups for peering: %w", err)
			}

			t.Logf("[%s] Successfully established VPC peering between %s and %s (connection: %s)",
				ProviderAWS, regionA, regionB, peeringConnID)
		}
	}

	return nil
}

// getAWSAccountID retrieves the current AWS account ID using STS GetCallerIdentity.
// Required for cross-region VPC peering within the same account - PeerOwnerId must be specified.
// Uses the primary region for the STS call (STS is a global service).
func getAWSAccountID(ctx context.Context) (string, error) {
	sess, err := session.NewSession(&aws.Config{
		Region: aws.String(awsRegionPrimary), // STS is global, use any valid region
	})
	if err != nil {
		return "", fmt.Errorf("failed to create AWS session: %w", err)
	}

	stsClient := sts.New(sess)
	result, err := stsClient.GetCallerIdentityWithContext(ctx, &sts.GetCallerIdentityInput{})
	if err != nil {
		return "", fmt.Errorf("failed to get caller identity: %w", err)
	}

	return aws.StringValue(result.Account), nil
}

// createVPCPeeringConnection creates a VPC peering connection from regionA to regionB.
// The connection is created in the requester region (regionA) and must be accepted
// in the acceptor region (regionB) before it becomes active.
// Requires PeerOwnerId for cross-region peering within the same AWS account.
func (r *AwsRegion) createVPCPeeringConnection(
	ctx context.Context, t *testing.T, client *ec2.EC2, vpcA, vpcB, regionA, regionB string,
) (string, error) {
	peeringName := fmt.Sprintf("%s-peering-%s-%s", r.vpcName, regionA, regionB)

	// Get AWS account ID for cross-region peering
	accountID, err := getAWSAccountID(ctx)
	if err != nil {
		return "", fmt.Errorf("failed to get AWS account ID: %w", err)
	}

	output, err := client.CreateVpcPeeringConnectionWithContext(ctx, &ec2.CreateVpcPeeringConnectionInput{
		VpcId:       aws.String(vpcA),
		PeerVpcId:   aws.String(vpcB),
		PeerRegion:  aws.String(regionB),
		PeerOwnerId: aws.String(accountID), // Required for cross-region peering
		TagSpecifications: []*ec2.TagSpecification{
			{
				ResourceType: aws.String("vpc-peering-connection"),
				Tags:         r.getResourceTags(peeringName),
			},
		},
	})
	if err != nil {
		return "", err
	}

	peeringConnID := *output.VpcPeeringConnection.VpcPeeringConnectionId

	// Check the initial status of the peering connection
	if output.VpcPeeringConnection.Status != nil {
		statusCode := aws.StringValue(output.VpcPeeringConnection.Status.Code)
		statusMsg := aws.StringValue(output.VpcPeeringConnection.Status.Message)

		t.Logf("[%s] Created VPC peering connection %s from %s to %s (status: %s)", ProviderAWS, peeringConnID, regionA, regionB, statusCode)

		if statusMsg != "" {
			t.Logf("[%s] VPC peering status message: %s", ProviderAWS, statusMsg)
		}

		// If the peering connection immediately failed, return the error with details
		if statusCode == "failed" || statusCode == "rejected" {
			return "", fmt.Errorf("VPC peering connection failed immediately with status %s: %s", statusCode, statusMsg)
		}
	} else {
		t.Logf("[%s] Created VPC peering connection %s from %s to %s", ProviderAWS, peeringConnID, regionA, regionB)
	}

	return peeringConnID, nil
}

// acceptVPCPeeringConnection accepts a VPC peering connection request in the acceptor region.
// Retries for up to 2 minutes to handle propagation delays - cross-region peering connections
// take time to become visible in the acceptor region after being created in the requester region.
func (r *AwsRegion) acceptVPCPeeringConnection(
	ctx context.Context, t *testing.T, client *ec2.EC2, peeringConnID, region string,
) error {
	// Retry accepting the peering connection as it may take time to propagate to the acceptor region
	var lastErr error
	for i := 0; i < 12; i++ { // Retry for up to 2 minutes
		_, err := client.AcceptVpcPeeringConnectionWithContext(ctx, &ec2.AcceptVpcPeeringConnectionInput{
			VpcPeeringConnectionId: aws.String(peeringConnID),
		})
		if err == nil {
			t.Logf("[%s] Accepted VPC peering connection %s in region %s", ProviderAWS, peeringConnID, region)
			return nil
		}

		lastErr = err
		// If the peering connection is not found, wait and retry
		if strings.Contains(err.Error(), "InvalidVpcPeeringConnectionID.NotFound") {
			if i == 0 {
				t.Logf("[%s] VPC peering connection %s not yet visible in %s, waiting for propagation...", ProviderAWS, peeringConnID, region)
			}
			time.Sleep(10 * time.Second)
			continue
		}

		// For other errors, fail immediately
		return err
	}

	return fmt.Errorf("failed to accept VPC peering connection after retries: %w", lastErr)
}

// waitForVPCPeeringActive waits for a VPC peering connection to reach active status.
// After acceptance, the peering connection transitions through the provisioning state before
// becoming active. Waits up to 5 minutes, polling every 10 seconds.
func (r *AwsRegion) waitForVPCPeeringActive(
	ctx context.Context, t *testing.T, client *ec2.EC2, peeringConnID string,
) error {
	t.Logf("[%s] Waiting for VPC peering connection %s to become active...", ProviderAWS, peeringConnID)

	for i := 0; i < 30; i++ { // Wait up to 5 minutes
		output, err := client.DescribeVpcPeeringConnectionsWithContext(ctx, &ec2.DescribeVpcPeeringConnectionsInput{
			VpcPeeringConnectionIds: []*string{aws.String(peeringConnID)},
		})
		if err != nil {
			return err
		}

		if len(output.VpcPeeringConnections) == 0 {
			return fmt.Errorf("VPC peering connection %s not found", peeringConnID)
		}

		status := *output.VpcPeeringConnections[0].Status.Code
		if status == "active" {
			t.Logf("[%s] VPC peering connection %s is now active", ProviderAWS, peeringConnID)
			return nil
		}

		if status == "failed" || status == "rejected" || status == "deleted" {
			return fmt.Errorf("VPC peering connection %s failed with status: %s", peeringConnID, status)
		}

		time.Sleep(10 * time.Second)
	}

	return fmt.Errorf("timeout waiting for VPC peering connection %s to become active", peeringConnID)
}

// addPeeringRoutes adds routes to route tables for VPC peering between two regions.
// For each region, it adds two routes through the peering connection:
// 1. Route to peer VPC CIDR (e.g., 172.28.144.0/24) - for VPC infrastructure
// 2. Route to peer Pod CIDR (e.g., 172.28.48.0/20) - for cross-region pod communication
//
// Without Pod CIDR routes, pods in different regions cannot communicate even though
// VPC peering is active, causing CockroachDB cluster formation to fail.
//
// Requires r.routeTableIDs to be populated before calling.
func (r *AwsRegion) addPeeringRoutes(
	ctx context.Context,
	t *testing.T,
	ec2Clients map[string]*ec2.EC2,
	peeringConnID, regionA, regionB string,
) error {
	// Get VPC CIDRs and Pod CIDRs for routing
	vpcCIDRA := r.getVPCCIDR(regionA)
	vpcCIDRB := r.getVPCCIDR(regionB)
	podCIDRA := r.getPodCIDR(regionA)
	podCIDRB := r.getPodCIDR(regionB)

	routeTableA := r.routeTableIDs[regionA]
	routeTableB := r.routeTableIDs[regionB]

	// Add routes in region A to reach region B's VPC and Pods
	if routeTableA != "" {
		// Route for VPC CIDR
		_, err := ec2Clients[regionA].CreateRouteWithContext(ctx, &ec2.CreateRouteInput{
			RouteTableId:           aws.String(routeTableA),
			DestinationCidrBlock:   aws.String(vpcCIDRB),
			VpcPeeringConnectionId: aws.String(peeringConnID),
		})
		if err != nil && !strings.Contains(err.Error(), "already exists") {
			return fmt.Errorf("failed to add VPC route in %s: %w", regionA, err)
		}
		t.Logf("[%s] Added VPC route in %s: %s -> %s via %s", ProviderAWS, regionA, vpcCIDRB, regionB, peeringConnID)

		// Route for Pod CIDR (critical for cross-region pod communication)
		_, err = ec2Clients[regionA].CreateRouteWithContext(ctx, &ec2.CreateRouteInput{
			RouteTableId:           aws.String(routeTableA),
			DestinationCidrBlock:   aws.String(podCIDRB),
			VpcPeeringConnectionId: aws.String(peeringConnID),
		})
		if err != nil && !strings.Contains(err.Error(), "already exists") {
			return fmt.Errorf("failed to add Pod route in %s: %w", regionA, err)
		}
		t.Logf("[%s] Added Pod route in %s: %s -> %s via %s", ProviderAWS, regionA, podCIDRB, regionB, peeringConnID)
	}

	// Add routes in region B to reach region A's VPC and Pods
	if routeTableB != "" {
		// Route for VPC CIDR
		_, err := ec2Clients[regionB].CreateRouteWithContext(ctx, &ec2.CreateRouteInput{
			RouteTableId:           aws.String(routeTableB),
			DestinationCidrBlock:   aws.String(vpcCIDRA),
			VpcPeeringConnectionId: aws.String(peeringConnID),
		})
		if err != nil && !strings.Contains(err.Error(), "already exists") {
			return fmt.Errorf("failed to add VPC route in %s: %w", regionB, err)
		}
		t.Logf("[%s] Added VPC route in %s: %s -> %s via %s", ProviderAWS, regionB, vpcCIDRA, regionA, peeringConnID)

		// Route for Pod CIDR (critical for cross-region pod communication)
		_, err = ec2Clients[regionB].CreateRouteWithContext(ctx, &ec2.CreateRouteInput{
			RouteTableId:           aws.String(routeTableB),
			DestinationCidrBlock:   aws.String(podCIDRA),
			VpcPeeringConnectionId: aws.String(peeringConnID),
		})
		if err != nil && !strings.Contains(err.Error(), "already exists") {
			return fmt.Errorf("failed to add Pod route in %s: %w", regionB, err)
		}
		t.Logf("[%s] Added Pod route in %s: %s -> %s via %s", ProviderAWS, regionB, podCIDRA, regionA, peeringConnID)
	}

	return nil
}

// getVPCCIDR returns the VPC CIDR block for a given region.
// Each region has a unique non-overlapping VPC CIDR to enable VPC peering.
// Returns region-specific CIDR from centralized constants (e.g., 172.28.112.0/24 for us-east-1).
func (r *AwsRegion) getVPCCIDR(region string) string {
	switch region {
	case awsRegionPrimary:
		return awsVPCCIDRPrimary
	case awsRegionSecondary:
		return awsVPCCIDRSecondary
	case awsRegionTertiary:
		return awsVPCCIDRTertiary
	default:
		return "172.28.0.0/16" // Fallback
	}
}

// updateSecurityGroupsForPeering updates security groups to allow traffic from peered VPCs.
// For each region's security groups, it adds ingress rules to allow all traffic from:
// 1. Peer VPC CIDR (e.g., 172.28.144.0/24) - for VPC infrastructure traffic
// 2. Peer Pod CIDR (e.g., 172.28.48.0/20) - for cross-region pod-to-pod communication
//
// This is required in addition to VPC peering routes to allow CockroachDB pods
// in different regions to communicate with each other.
func (r *AwsRegion) updateSecurityGroupsForPeering(
	ctx context.Context, t *testing.T, ec2Clients map[string]*ec2.EC2, regionA, regionB string,
) error {
	vpcCIDRA := r.getVPCCIDR(regionA)
	vpcCIDRB := r.getVPCCIDR(regionB)

	// Also allow traffic from peer VPC's pod CIDRs (for CockroachDB pod-to-pod communication)
	podCIDRA := r.getPodCIDR(regionA)
	podCIDRB := r.getPodCIDR(regionB)

	// Update security groups in region A to allow traffic from region B
	for sgName, sgID := range r.securityGroupIDs {
		if !strings.Contains(sgName, regionA) {
			continue // Skip security groups from other regions
		}

		ec2Client := ec2Clients[regionA]

		// Allow VPC CIDR
		_, err := ec2Client.AuthorizeSecurityGroupIngressWithContext(ctx, &ec2.AuthorizeSecurityGroupIngressInput{
			GroupId: aws.String(sgID),
			IpPermissions: []*ec2.IpPermission{
				{
					IpProtocol: aws.String("-1"), // All protocols
					IpRanges:   []*ec2.IpRange{{CidrIp: aws.String(vpcCIDRB)}},
				},
			},
		})
		if err != nil && !strings.Contains(err.Error(), "already exists") {
			t.Logf("[%s] Warning: failed to add VPC CIDR ingress rule to SG %s: %v", ProviderAWS, sgID, err)
		}

		// Allow all traffic from Pod CIDR (needed for pod-to-pod communication across regions)
		_, err = ec2Client.AuthorizeSecurityGroupIngressWithContext(ctx, &ec2.AuthorizeSecurityGroupIngressInput{
			GroupId: aws.String(sgID),
			IpPermissions: []*ec2.IpPermission{
				{
					IpProtocol: aws.String("-1"), // All protocols for pod-to-pod communication
					IpRanges:   []*ec2.IpRange{{CidrIp: aws.String(podCIDRB)}},
				},
			},
		})
		if err != nil && !strings.Contains(err.Error(), "already exists") {
			t.Logf("[%s] Warning: failed to add pod CIDR ingress rule to SG %s: %v", ProviderAWS, sgID, err)
		}
	}

	// Update security groups in region B to allow traffic from region A
	for sgName, sgID := range r.securityGroupIDs {
		if !strings.Contains(sgName, regionB) {
			continue // Skip security groups from other regions
		}

		ec2Client := ec2Clients[regionB]

		// Allow VPC CIDR
		_, err := ec2Client.AuthorizeSecurityGroupIngressWithContext(ctx, &ec2.AuthorizeSecurityGroupIngressInput{
			GroupId: aws.String(sgID),
			IpPermissions: []*ec2.IpPermission{
				{
					IpProtocol: aws.String("-1"), // All protocols
					IpRanges:   []*ec2.IpRange{{CidrIp: aws.String(vpcCIDRA)}},
				},
			},
		})
		if err != nil && !strings.Contains(err.Error(), "already exists") {
			t.Logf("[%s] Warning: failed to add VPC CIDR ingress rule to SG %s: %v", ProviderAWS, sgID, err)
		}

		// Allow all traffic from Pod CIDR (needed for pod-to-pod communication across regions)
		_, err = ec2Client.AuthorizeSecurityGroupIngressWithContext(ctx, &ec2.AuthorizeSecurityGroupIngressInput{
			GroupId: aws.String(sgID),
			IpPermissions: []*ec2.IpPermission{
				{
					IpProtocol: aws.String("-1"), // All protocols for pod-to-pod communication
					IpRanges:   []*ec2.IpRange{{CidrIp: aws.String(podCIDRA)}},
				},
			},
		})
		if err != nil && !strings.Contains(err.Error(), "already exists") {
			t.Logf("[%s] Warning: failed to add pod CIDR ingress rule to SG %s: %v", ProviderAWS, sgID, err)
		}
	}

	t.Logf("[%s] Updated security groups to allow cross-VPC traffic between %s and %s", ProviderAWS, regionA, regionB)
	return nil
}

// getPodCIDR returns the Pod CIDR block for EKS pods in a given region.
// Each region has a unique non-overlapping Pod CIDR to enable cross-region pod communication.
// Pod CIDRs are larger than VPC CIDRs (e.g., /20 vs /24) to accommodate many pods.
// Returns region-specific CIDR from centralized constants (e.g., 172.28.0.0/20 for us-east-1).
func (r *AwsRegion) getPodCIDR(region string) string {
	switch region {
	case awsRegionPrimary:
		return awsPodCIDRPrimary
	case awsRegionSecondary:
		return awsPodCIDRSecondary
	case awsRegionTertiary:
		return awsPodCIDRTertiary
	default:
		return "172.28.0.0/16" // Fallback
	}
}

// deleteVPCPeeringConnections deletes all VPC peering connections created by the test.
// Uses a two-phase cleanup approach:
// 1. Delete tracked connections from r.vpcPeeringConnections
// 2. Search for and delete any connections tagged with TestRunID (fallback cleanup)
// Deletes connections in parallel across all regions for efficiency.
func (r *AwsRegion) deleteVPCPeeringConnections(
	ctx context.Context, t *testing.T, ec2Clients map[string]*ec2.EC2,
) error {
	// If we have tracked peering connections, delete them
	if len(r.vpcPeeringConnections) > 0 {
		t.Logf("[%s] Deleting %d VPC peering connections", ProviderAWS, len(r.vpcPeeringConnections))

		var wg sync.WaitGroup
		deletedCount := 0
		var mu sync.Mutex

		for _, peeringConnID := range r.vpcPeeringConnections {
			wg.Add(1)
			go func(connID string) {
				defer wg.Done()

				// Try to delete it from each region until we find the right one
				for region, ec2Client := range ec2Clients {
					_, err := ec2Client.DeleteVpcPeeringConnectionWithContext(ctx, &ec2.DeleteVpcPeeringConnectionInput{
						VpcPeeringConnectionId: aws.String(connID),
					})
					if err == nil {
						mu.Lock()
						deletedCount++
						mu.Unlock()
						t.Logf("[%s] Deleted VPC peering connection %s in region %s", ProviderAWS, connID, region)
						return
					}
					// If the error is "not found", try the next region
					if !strings.Contains(err.Error(), "InvalidVpcPeeringConnectionID.NotFound") {
						t.Logf("[%s] Warning: failed to delete VPC peering %s in region %s: %v", ProviderAWS, connID, region, err)
					}
				}
			}(peeringConnID)
		}

		wg.Wait()
		t.Logf("[%s] Successfully deleted %d/%d VPC peering connections", ProviderAWS, deletedCount, len(r.vpcPeeringConnections))
	}

	// Also search for any peering connections by tags (fallback cleanup)
	t.Logf("[%s] Searching for VPC peering connections by tags for additional cleanup", ProviderAWS)
	var wg sync.WaitGroup
	var taggedPeeringCount int
	var taggedDeletedCount int
	var mu sync.Mutex

	for region, ec2Client := range ec2Clients {
		wg.Add(1)
		go func(reg string, ec2Client *ec2.EC2) {
			defer wg.Done()

			// List peering connections with our tags
			result, err := ec2Client.DescribeVpcPeeringConnectionsWithContext(ctx, &ec2.DescribeVpcPeeringConnectionsInput{
				Filters: []*ec2.Filter{
					{
						Name:   aws.String("tag:ManagedBy"),
						Values: []*string{aws.String("helm-charts-e2e")},
					},
					{
						Name:   aws.String("tag:TestRunID"),
						Values: []*string{aws.String(r.TestRunID)},
					},
				},
			})
			if err != nil {
				t.Logf("[%s] Warning: failed to list VPC peering connections in region %s: %v", ProviderAWS, reg, err)
				return
			}

			for _, conn := range result.VpcPeeringConnections {
				mu.Lock()
				taggedPeeringCount++
				mu.Unlock()

				// Skip if already deleted or deleting
				if conn.Status != nil && (aws.StringValue(conn.Status.Code) == "deleted" || aws.StringValue(conn.Status.Code) == "deleting") {
					t.Logf("[%s] Skipping VPC peering %s (status: %s)", ProviderAWS, aws.StringValue(conn.VpcPeeringConnectionId), aws.StringValue(conn.Status.Code))
					continue
				}

				_, err := ec2Client.DeleteVpcPeeringConnectionWithContext(ctx, &ec2.DeleteVpcPeeringConnectionInput{
					VpcPeeringConnectionId: conn.VpcPeeringConnectionId,
				})
				if err != nil {
					t.Logf("[%s] ERROR: failed to delete VPC peering %s in region %s: %v", ProviderAWS, aws.StringValue(conn.VpcPeeringConnectionId), reg, err)
				} else {
					mu.Lock()
					taggedDeletedCount++
					mu.Unlock()
					t.Logf("[%s] Deleted VPC peering connection %s in region %s (tag-based cleanup)", ProviderAWS, aws.StringValue(conn.VpcPeeringConnectionId), reg)
				}
			}
		}(region, ec2Client)
	}

	wg.Wait()

	if taggedPeeringCount > 0 {
		t.Logf("[%s] Tag-based cleanup: found %d VPC peering connections, deleted %d/%d", ProviderAWS, taggedPeeringCount, taggedDeletedCount, taggedPeeringCount)
	} else {
		t.Logf("[%s] No additional VPC peering connections found by tags", ProviderAWS)
	}

	t.Logf("[%s] VPC peering connection deletion completed", ProviderAWS)
	return nil
}

func (r *AwsRegion) createEKSClusters(
	ctx context.Context, t *testing.T, eksClients map[string]*eks.EKS,
) error {
	// Suppress controller-runtime logging warning by setting a discard logger
	ctrllog.SetLogger(logr.Discard())

	var clients = make(map[string]client.Client)
	r.CorednsClusterOptions = make(map[string]coredns.CoreDNSClusterOption)

	type clusterResult struct {
		index  int
		client client.Client
		err    error
	}

	resultsChan := make(chan clusterResult, len(r.Clusters))

	for i, clusterName := range r.Clusters {
		go func(idx int, name string) {
			defer func() {
				if rec := recover(); rec != nil {
					resultsChan <- clusterResult{index: idx, err: fmt.Errorf("panic during cluster creation: %v", rec)}
				}
			}()

			cfg := r.clusterConfigs[idx]
			t.Logf("[%s] Starting parallel creation of EKS cluster '%s' in region %s", ProviderAWS, name, cfg.Region)

			eksClient := eksClients[cfg.Region]

			// Create the EKS cluster
			err := r.createEKSCluster(ctx, t, eksClient, &cfg)
			if err != nil {
				resultsChan <- clusterResult{index: idx, err: fmt.Errorf("failed to create cluster: %w", err)}
				return
			}

			// Update kubeconfig using AWS CLI
			err = UpdateKubeconfigAWS(t, cfg.Region, name, name)
			if err != nil {
				resultsChan <- clusterResult{index: idx, err: fmt.Errorf("failed to update kubeconfig: %w", err)}
				return
			}

			// Disable TLS verification for corporate proxy compatibility (e.g., Netskope)
			err = disableTLSVerificationInKubeconfig(t, name)
			if err != nil {
				t.Logf("[%s] Warning: Failed to disable TLS verification in kubeconfig: %v", ProviderAWS, err)
				// Don't fail - this is only needed in proxy environments
			}

			// Set gp2 StorageClass as default (required for PVCs without explicit storageClassName)
			t.Logf("[%s] Setting gp2 StorageClass as default for cluster %s (region: %s)", ProviderAWS, name, cfg.Region)
			patchCmd := exec.Command("kubectl",
				"--context", name,
				"patch", "storageclass", "gp2",
				"-p", `{"metadata": {"annotations":{"storageclass.kubernetes.io/is-default-class":"true"}}}`)

			if output, err := patchCmd.CombinedOutput(); err != nil {
				// Log warning but don't fail - the StorageClass might not exist yet
				t.Logf("[%s] Warning: Failed to set gp2 as default StorageClass: %v. Output: %s", ProviderAWS, err, string(output))
				t.Logf("[%s] PVCs may need explicit storageClassName specified", ProviderAWS)
			} else {
				t.Logf("[%s] Successfully set gp2 as default StorageClass", ProviderAWS)
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

			t.Logf("[%s] Successfully created EKS cluster '%s' in region %s", ProviderAWS, name, cfg.Region)
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

		// Store CoreDNS options with placeholder IPs (will be updated after service creation)
		r.CorednsClusterOptions[operator.CustomDomains[result.index]] = coredns.CoreDNSClusterOption{
			IPs:       []string{"127.0.0.1"}, // Placeholder
			Namespace: r.Namespace[r.Clusters[result.index]],
			Domain:    operator.CustomDomains[result.index],
		}
	}

	r.Clients = clients
	t.Logf("[%s] All EKS clusters created successfully", ProviderAWS)

	return nil
}

func (r *AwsRegion) createEKSCluster(
	ctx context.Context, t *testing.T, eksClient *eks.EKS, setup *AWSClusterSetupConfig,
) error {
	// Check if a cluster already exists
	_, err := eksClient.DescribeClusterWithContext(ctx, &eks.DescribeClusterInput{
		Name: aws.String(setup.ClusterName),
	})
	if err == nil {
		t.Logf("[%s] EKS cluster '%s' already exists", ProviderAWS, setup.ClusterName)
		return nil
	}

	// Use eksctl for cluster creation as it handles all the complexity
	args := []string{
		"create", "cluster",
		"--name", setup.ClusterName,
		"--region", setup.Region,
		"--version", awsDefaultK8sVersion,
		"--nodegroup-name", fmt.Sprintf("%s-nodegroup", setup.ClusterName),
		"--node-type", awsDefaultInstanceType,
		"--nodes", fmt.Sprint(len(setup.AvailabilityZones) * awsDefaultNodesPerAZ),
		"--nodes-min", fmt.Sprint(len(setup.AvailabilityZones) * awsDefaultNodesPerAZ),
		"--nodes-max", fmt.Sprint(len(setup.AvailabilityZones) * (awsDefaultNodesPerAZ + 1)),
		"--node-volume-size", fmt.Sprint(awsDefaultDiskSize),
		"--tags", fmt.Sprintf("ManagedBy=helm-charts-e2e,TestRunID=%s", r.TestRunID),
		// Enable OIDC provider (required for EBS CSI driver IAM role)
		"--with-oidc",
	}

	// Use existing VPC and subnets that we created
	if len(setup.SubnetIDs) > 0 {
		args = append(args, "--vpc-public-subnets", strings.Join(setup.SubnetIDs, ","))
	}

	// Attach our custom internal security group to nodes (allows cross-region traffic)
	// The internal SG has rules allowing traffic from 172.28.0.0/16 and peer VPC/Pod CIDRs
	internalSGName := fmt.Sprintf("%s-%s-%s", r.vpcName, awsInternalSGName, setup.Region)
	if internalSGID, ok := r.securityGroupIDs[internalSGName]; ok {
		args = append(args, "--node-security-groups", internalSGID)
		t.Logf("[%s] Attaching internal security group %s to nodes", ProviderAWS, internalSGID)
	} else {
		t.Logf("[%s] Warning: Internal security group not found for key %s", ProviderAWS, internalSGName)
	}

	cmd := exec.Command("eksctl", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	t.Logf("[%s] Running command: eksctl %s", ProviderAWS, strings.Join(args, " "))

	if err := cmd.Run(); err != nil {
		// Check if the cluster was actually created despite the eksctl error
		// (This can happen if eksctl fails on post-creation verification)
		time.Sleep(5 * time.Second)
		describeResp, describeErr := eksClient.DescribeClusterWithContext(ctx, &eks.DescribeClusterInput{
			Name: aws.String(setup.ClusterName),
		})
		if describeErr == nil && describeResp.Cluster != nil {
			t.Logf("[%s] Cluster '%s' was created successfully despite eksctl error (status: %s)",
				ProviderAWS, setup.ClusterName, *describeResp.Cluster.Status)
			// Wait for the cluster to be active
			if err := r.waitForClusterActive(ctx, t, eksClient, setup.ClusterName); err != nil {
				return err
			}
			// Disable TLS verification for proxy compatibility (eksctl already updated kubeconfig)
			if err := disableTLSVerificationInKubeconfig(t, setup.ClusterName); err != nil {
				t.Logf("[%s] Warning: Failed to disable TLS verification: %v", ProviderAWS, err)
			}
			// Enable OIDC and install EBS CSI driver
			return r.enableOIDCAndInstallCSIDriver(t, setup)
		}
		return fmt.Errorf("eksctl create command failed for cluster '%s': %w", setup.ClusterName, err)
	}

	// eksctl create cluster automatically updates kubeconfig
	// Optionally disable TLS verification if KUBECTL_INSECURE_SKIP_TLS_VERIFY=true
	if err := disableTLSVerificationInKubeconfig(t, setup.ClusterName); err != nil {
		t.Logf("[%s] Warning: Failed to disable TLS verification: %v", ProviderAWS, err)
		// Don't fail cluster creation - this is only needed in proxy environments
	}

	// Enable OIDC provider and install EBS CSI driver
	// (required for K8s 1.23+ to provision EBS volumes)
	if err := r.enableOIDCAndInstallCSIDriver(t, setup); err != nil {
		t.Logf("[%s] Warning: Failed to enable OIDC and install EBS CSI driver: %v", ProviderAWS, err)
		// Don't fail cluster creation if CSI driver installation fails
		// It can be installed manually if needed
	}

	return nil
}

// waitForClusterActive waits for the EKS cluster to reach ACTIVE status
func (r *AwsRegion) waitForClusterActive(
	ctx context.Context, t *testing.T, eksClient *eks.EKS, clusterName string,
) error {
	t.Logf("[%s] Waiting for cluster '%s' to become ACTIVE", ProviderAWS, clusterName)

	maxRetries := 60 // 10 minutes
	for i := 0; i < maxRetries; i++ {
		resp, err := eksClient.DescribeClusterWithContext(ctx, &eks.DescribeClusterInput{
			Name: aws.String(clusterName),
		})
		if err != nil {
			return fmt.Errorf("failed to describe cluster: %w", err)
		}

		status := *resp.Cluster.Status
		t.Logf("[%s] Cluster status: %s", ProviderAWS, status)

		if status == "ACTIVE" {
			t.Logf("[%s] Cluster '%s' is now ACTIVE", ProviderAWS, clusterName)
			return nil
		}

		if status == "FAILED" || status == "DELETING" {
			return fmt.Errorf("cluster is in %s state", status)
		}

		time.Sleep(10 * time.Second)
	}

	return fmt.Errorf("timeout waiting for cluster to become ACTIVE")
}

// enableOIDCAndInstallCSIDriver enables OIDC provider and installs the AWS EBS CSI driver addon
func (r *AwsRegion) enableOIDCAndInstallCSIDriver(
	t *testing.T, setup *AWSClusterSetupConfig,
) error {
	// First, explicitly enable OIDC provider (--with-oidc flag doesn't always work)
	t.Logf("[%s] Enabling OIDC provider for cluster '%s'", ProviderAWS, setup.ClusterName)
	oidcCmd := exec.Command("eksctl", "utils", "associate-iam-oidc-provider",
		"--cluster", setup.ClusterName,
		"--region", setup.Region,
		"--approve")

	if output, err := oidcCmd.CombinedOutput(); err != nil {
		t.Logf("[%s] OIDC association output: %s", ProviderAWS, string(output))
		// Check if OIDC is already associated
		if !strings.Contains(string(output), "already exists") && !strings.Contains(string(output), "already associated") {
			return fmt.Errorf("failed to associate OIDC provider: %w", err)
		}
		t.Logf("[%s] OIDC provider already associated", ProviderAWS)
	} else {
		t.Logf("[%s] Successfully enabled OIDC provider", ProviderAWS)
	}

	// Now install EBS CSI driver using AWS CLI instead of eksctl to avoid certificate issues
	t.Logf("[%s] Installing EBS CSI driver addon for cluster '%s'", ProviderAWS, setup.ClusterName)

	// Get AWS account ID
	accountCmd := exec.Command("aws", "sts", "get-caller-identity", "--query", "Account", "--output", "text")
	accountOutput, err := accountCmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("failed to get AWS account ID: %w", err)
	}
	accountID := strings.TrimSpace(string(accountOutput))

	// Create an IAM role for an EBS CSI driver using AWS IAM (bypasses kubectl/eksctl certificate issues)
	// Use TestRunID instead of cluster name to avoid exceeding AWS's 64-character limit for IAM role names
	roleName := fmt.Sprintf("ebs-csi-%s-%s", setup.Region, r.TestRunID)

	// Get OIDC provider URL
	oidcCmd = exec.Command("aws", "eks", "describe-cluster",
		"--name", setup.ClusterName,
		"--region", setup.Region,
		"--query", "cluster.identity.oidc.issuer",
		"--output", "text")

	oidcOutput, err := oidcCmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("failed to get OIDC provider: %w", err)
	}
	oidcProvider := strings.TrimSpace(strings.TrimPrefix(string(oidcOutput), "https://"))

	// Create a trust policy document
	trustPolicy := fmt.Sprintf(`{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Principal": {
        "Federated": "arn:aws:iam::%s:oidc-provider/%s"
      },
      "Action": "sts:AssumeRoleWithWebIdentity",
      "Condition": {
        "StringEquals": {
          "%s:sub": "system:serviceaccount:kube-system:ebs-csi-controller-sa",
          "%s:aud": "sts.amazonaws.com"
        }
      }
    }
  ]
}`, accountID, oidcProvider, oidcProvider, oidcProvider)

	// Create the IAM role
	t.Logf("[%s] Creating IAM role %s for EBS CSI driver", ProviderAWS, roleName)
	createRoleCmd := exec.Command("aws", "iam", "create-role",
		"--role-name", roleName,
		"--assume-role-policy-document", trustPolicy,
		"--description", "IAM role for EBS CSI driver",
		"--tags",
		"Key=ManagedBy,Value=helm-charts-e2e",
		fmt.Sprintf("Key=TestRunID,Value=%s", r.TestRunID))

	if output, err := createRoleCmd.CombinedOutput(); err != nil {
		// Check if a role already exists
		if !strings.Contains(string(output), "EntityAlreadyExists") {
			t.Logf("[%s] Failed to create IAM role: %s", ProviderAWS, string(output))
			return fmt.Errorf("failed to create IAM role: %w", err)
		}
		t.Logf("[%s] IAM role %s already exists", ProviderAWS, roleName)
	}

	// Attach the EBS CSI policy to the role
	attachPolicyCmd := exec.Command("aws", "iam", "attach-role-policy",
		"--role-name", roleName,
		"--policy-arn", "arn:aws:iam::aws:policy/service-role/AmazonEBSCSIDriverPolicy")

	if output, err := attachPolicyCmd.CombinedOutput(); err != nil {
		t.Logf("[%s] Warning: Failed to attach policy (may already be attached): %s", ProviderAWS, string(output))
	}

	// Install the EBS CSI driver addon using AWS CLI (no kubectl/certificate needed)
	roleArn := fmt.Sprintf("arn:aws:iam::%s:role/%s", accountID, roleName)

	t.Logf("[%s] Installing EBS CSI driver addon with role: %s", ProviderAWS, roleArn)
	addonCmd := exec.Command("aws", "eks", "create-addon",
		"--cluster-name", setup.ClusterName,
		"--region", setup.Region,
		"--addon-name", "aws-ebs-csi-driver",
		"--service-account-role-arn", roleArn,
		"--resolve-conflicts", "OVERWRITE")

	if output, err := addonCmd.CombinedOutput(); err != nil {
		// Check if addon already exists
		if strings.Contains(string(output), "ResourceInUseException") || strings.Contains(string(output), "already exists") {
			t.Logf("[%s] EBS CSI driver addon already exists", ProviderAWS)
		} else {
			t.Logf("[%s] EBS CSI driver addon installation output: %s", ProviderAWS, string(output))
			return fmt.Errorf("failed to install EBS CSI driver addon: %w", err)
		}
	} else {
		t.Logf("[%s] Successfully installed EBS CSI driver addon", ProviderAWS)
	}

	// Wait for the CSI driver addon to become active
	t.Logf("[%s] Waiting for EBS CSI driver addon to become active...", ProviderAWS)
	maxAttempts := 20 // 20 attempts * 15 seconds = 5 minutes max
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		checkCmd := exec.Command("aws", "eks", "describe-addon",
			"--cluster-name", setup.ClusterName,
			"--region", setup.Region,
			"--addon-name", "aws-ebs-csi-driver",
			"--query", "addon.status",
			"--output", "text")

		output, err := checkCmd.CombinedOutput()
		if err != nil {
			t.Logf("[%s] Attempt %d/%d: Failed to check addon status: %v", ProviderAWS, attempt, maxAttempts, err)
		} else {
			status := strings.TrimSpace(string(output))
			t.Logf("[%s] Attempt %d/%d: EBS CSI driver addon status: %s", ProviderAWS, attempt, maxAttempts, status)

			if status == "ACTIVE" {
				t.Logf("[%s] EBS CSI driver addon is now active", ProviderAWS)
				// Give it a bit more time for the CSI driver pods to fully start
				time.Sleep(15 * time.Second)
				return nil
			}

			if status == "CREATE_FAILED" || status == "DEGRADED" {
				return fmt.Errorf("EBS CSI driver addon failed with status: %s", status)
			}
		}

		if attempt < maxAttempts {
			t.Logf("[%s] Waiting 15 seconds before next check...", ProviderAWS)
			time.Sleep(15 * time.Second)
		}
	}

	return fmt.Errorf("timeout waiting for EBS CSI driver addon to become active after %d attempts", maxAttempts)
}

// deployAndConfigureCoreDNS handles the deployment and configuration of CoreDNS across all clusters
func (r *AwsRegion) deployAndConfigureCoreDNS(t *testing.T, kubeConfigPath string) error {
	// Deploy CoreDNS with initial configuration
	for i, clusterName := range r.Clusters {
		// Deploy CoreDNS (AWS uses NLB, which assigns hostnames, will be resolved to IPs)
		err := DeployCoreDNS(t, clusterName, kubeConfigPath, nil, ProviderAWS, operator.CustomDomains[i], r.CorednsClusterOptions)
		if err != nil {
			return fmt.Errorf("failed to deploy CoreDNS to cluster %s: %w", clusterName, err)
		}

		// Wait for the CoreDNS service to get IPs
		kubectlOpts := k8s.NewKubectlOptions(clusterName, kubeConfigPath, coreDNSNamespace)
		actualIPs, err := WaitForCoreDNSServiceIPs(t, kubectlOpts)
		if err != nil {
			return fmt.Errorf("failed to get CoreDNS service IPs for cluster %s: %w", clusterName, err)
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

// UpdateKubeconfigAWS updates kubeconfig for AWS EKS clusters and verifies connectivity
func UpdateKubeconfigAWS(t *testing.T, region, clusterName, alias string) error {
	t.Logf("[%s] Updating kubeconfig for cluster %s (region: %s) and verifying connectivity...", ProviderAWS, clusterName, region)

	// Verify cluster connectivity with retries, refreshing kubeconfig on each attempt
	maxRetries := 36 // 6 minutes total
	for i := 0; i < maxRetries; i++ {
		// Update kubeconfig on each retry to get fresh certificates
		// Use mutex to prevent concurrent kubeconfig file corruption
		kubeconfigMutex.Lock()
		updateCmd := exec.Command("aws", "eks", "update-kubeconfig",
			"--region", region,
			"--name", clusterName,
			"--alias", alias)
		_, err := updateCmd.CombinedOutput()
		kubeconfigMutex.Unlock()

		if err != nil {
			t.Logf("[%s] Warning: aws eks update-kubeconfig failed (attempt %d/%d): %v", ProviderAWS, i+1, maxRetries, err)
			time.Sleep(10 * time.Second)
			continue
		}

		// Use kubectl to verify connectivity (handles EKS certificates better than a Go client)
		testCmd := exec.Command("kubectl", "--context", alias, "get", "nodes", "--no-headers")
		testOutput, testErr := testCmd.CombinedOutput()

		if testErr == nil && len(testOutput) > 0 {
			t.Logf("[%s] Successfully connected to cluster %s in region %s (found nodes)", ProviderAWS, clusterName, region)
			// Give the cluster a bit more time to ensure all certificates are fully propagated
			time.Sleep(10 * time.Second)
			return nil
		}

		// Check if it's a TLS certificate error
		if testErr != nil && (strings.Contains(string(testOutput), "x509") ||
			strings.Contains(string(testOutput), "certificate") ||
			strings.Contains(testErr.Error(), "x509") ||
			strings.Contains(testErr.Error(), "certificate")) {
			t.Logf("[%s] Waiting for cluster certificates to be ready (attempt %d/%d)...", ProviderAWS, i+1, maxRetries)
			time.Sleep(10 * time.Second)
			continue
		}

		// If no nodes yet but no certificate error, the cluster might still be starting
		if testErr != nil {
			t.Logf("[%s] Cluster not ready yet (attempt %d/%d): %v", ProviderAWS, i+1, maxRetries, testErr)
			time.Sleep(10 * time.Second)
			continue
		}

		// No nodes found but no error - wait a bit more
		t.Logf("[%s] No nodes found yet (attempt %d/%d), waiting...", ProviderAWS, i+1, maxRetries)
		time.Sleep(10 * time.Second)
	}

	return fmt.Errorf("timeout waiting for cluster %s to be accessible", clusterName)
}

// ─── DELETE RESOURCES ──────────────────────────────────────────────────────────

// deleteEKSClusters deletes all EKS clusters in parallel.
// For each cluster:
//  1. Deletes the EBS CSI driver addon first (prevents orphaned addon resources)
//  2. Initiates cluster deletion via eksctl (async, no wait)
//  3. eksctl handles node group and CloudFormation stack deletion
func (r *AwsRegion) deleteEKSClusters(_ context.Context, t *testing.T) error {
	type deletionResult struct {
		clusterName string
		err         error
	}

	resultsChan := make(chan deletionResult, len(r.Clusters))

	for i, clusterName := range r.Clusters {
		go func(idx int, name string) {
			defer func() {
				if rec := recover(); rec != nil {
					resultsChan <- deletionResult{
						clusterName: name,
						err:         fmt.Errorf("panic during cluster deletion: %v", rec),
					}
				}
			}()

			cfg := r.clusterConfigs[idx]
			t.Logf("[%s] Deleting EKS cluster '%s' in region %s", ProviderAWS, name, cfg.Region)

			// Delete EBS CSI driver addon first (must be done before cluster deletion)
			// The addon can orphan resources if the cluster is deleted first
			t.Logf("[%s] Deleting EBS CSI driver addon for cluster %s", ProviderAWS, name)
			deleteAddonCmd := exec.Command("aws", "eks", "delete-addon",
				"--cluster-name", name,
				"--region", cfg.Region,
				"--addon-name", "aws-ebs-csi-driver")
			deleteAddonOutput, addonErr := deleteAddonCmd.CombinedOutput()
			if addonErr != nil {
				if strings.Contains(string(deleteAddonOutput), "ResourceNotFoundException") ||
					strings.Contains(string(deleteAddonOutput), "No addon") {
					t.Logf("[%s] EBS CSI addon does not exist for cluster %s, skipping", ProviderAWS, name)
				} else {
					t.Logf("[%s] Warning: failed to delete EBS CSI addon for %s: %v", ProviderAWS, name, addonErr)
				}
			} else {
				t.Logf("[%s] Successfully deleted EBS CSI addon for cluster %s", ProviderAWS, name)
			}

			// Use eksctl to delete the cluster with a force option (async, no wait)
			deleteCmd := exec.Command("eksctl", "delete", "cluster",
				"--name", name,
				"--region", cfg.Region,
				"--force",
				"--disable-nodegroup-eviction")
			deleteCmd.Stdout = os.Stdout
			deleteCmd.Stderr = os.Stderr

			err := deleteCmd.Run()
			if err != nil {
				t.Logf("[%s] Warning: cluster deletion command failed for %s: %v", ProviderAWS, name, err)
				t.Logf("[%s] Cluster may need manual cleanup", ProviderAWS)
				resultsChan <- deletionResult{clusterName: name, err: fmt.Errorf("deletion command failed: %w", err)}
				return
			}
			t.Logf("[%s] Initiated async deletion of cluster %s in region %s", ProviderAWS, name, cfg.Region)

			resultsChan <- deletionResult{clusterName: name, err: nil}
		}(i, clusterName)
	}

	// Create a map of the cluster name to a region for efficient lookup
	clusterRegions := make(map[string]string)
	for idx, name := range r.Clusters {
		clusterRegions[name] = r.clusterConfigs[idx].Region
	}

	// Wait for all deletions to complete
	for range r.Clusters {
		result := <-resultsChan
		regionStr := clusterRegions[result.clusterName]
		if result.err != nil {
			t.Logf("[%s] Warning: failed to delete cluster %s in region %s: %v", ProviderAWS, result.clusterName, regionStr, result.err)
		} else {
			t.Logf("[%s] Successfully deleted cluster %s in region %s", ProviderAWS, result.clusterName, regionStr)
		}
	}

	return nil
}

// deleteIAMRoles deletes IAM roles and OIDC providers for EKS clusters.
//  1. Deletes IAM roles: searches for roles with pattern ebs-csi-{region}-{testRunID}
//  2. Deletes OIDC providers: finds providers by querying cluster OIDC issuer URLs
//     OIDC providers persist after cluster deletion and accumulate over time, causing "already exists" errors.
func (r *AwsRegion) deleteIAMRoles(t *testing.T) error {
	t.Logf("[%s] Deleting IAM roles for TestRunID: %s", ProviderAWS, r.TestRunID)

	// Get a list of IAM roles matching our pattern
	for i := range r.Clusters {
		region := r.clusterConfigs[i].Region
		roleName := fmt.Sprintf("ebs-csi-%s-%s", region, r.TestRunID)

		t.Logf("[%s] Checking IAM role: %s", ProviderAWS, roleName)

		// Check if a role exists and has correct tags
		getCmd := exec.Command("aws", "iam", "get-role", "--role-name", roleName)
		if output, err := getCmd.CombinedOutput(); err != nil {
			if strings.Contains(string(output), "NoSuchEntity") {
				t.Logf("[%s] IAM role %s does not exist, skipping", ProviderAWS, roleName)
				continue
			}
			t.Logf("[%s] Warning: failed to get IAM role %s: %v", ProviderAWS, roleName, err)
			continue
		}

		// Detach all attached policies
		t.Logf("[%s] Detaching policies from IAM role: %s", ProviderAWS, roleName)
		listPoliciesCmd := exec.Command("aws", "iam", "list-attached-role-policies",
			"--role-name", roleName,
			"--query", "AttachedPolicies[].PolicyArn",
			"--output", "text")

		policiesOutput, err := listPoliciesCmd.CombinedOutput()
		if err != nil {
			t.Logf("[%s] Warning: failed to list policies for role %s: %v", ProviderAWS, roleName, err)
		} else {
			policies := strings.Fields(string(policiesOutput))
			for _, policyArn := range policies {
				if policyArn == "" {
					continue
				}
				detachCmd := exec.Command("aws", "iam", "detach-role-policy",
					"--role-name", roleName,
					"--policy-arn", policyArn)
				if output, err := detachCmd.CombinedOutput(); err != nil {
					t.Logf("[%s] Warning: failed to detach policy %s from role %s: %v", ProviderAWS, policyArn, roleName, err)
					t.Logf("[%s] Output: %s", ProviderAWS, string(output))
				} else {
					t.Logf("[%s] Detached policy %s from role %s", ProviderAWS, policyArn, roleName)
				}
			}
		}

		// Delete the role
		t.Logf("[%s] Deleting IAM role: %s", ProviderAWS, roleName)
		deleteCmd := exec.Command("aws", "iam", "delete-role", "--role-name", roleName)
		if output, err := deleteCmd.CombinedOutput(); err != nil {
			t.Logf("[%s] Warning: failed to delete IAM role %s: %v", ProviderAWS, roleName, err)
			t.Logf("[%s] Output: %s", ProviderAWS, string(output))
		} else {
			t.Logf("[%s] Successfully deleted IAM role: %s", ProviderAWS, roleName)
		}
	}

	// Delete OIDC providers associated with the EKS clusters
	// OIDC providers are created with --with-oidc flag during cluster creation but persist after deletion.
	// They don't have tags, so we identify them by extracting the OIDC issuer URL from each cluster
	// and matching it against the list of all OIDC providers in the account.
	t.Logf("[%s] Deleting OIDC providers for TestRunID: %s", ProviderAWS, r.TestRunID)
	for i := range r.Clusters {
		clusterName := r.Clusters[i]
		region := r.clusterConfigs[i].Region

		// Get the OIDC provider ARN for the cluster
		describeCmd := exec.Command("aws", "eks", "describe-cluster",
			"--name", clusterName,
			"--region", region,
			"--query", "cluster.identity.oidc.issuer",
			"--output", "text")
		issuerOutput, err := describeCmd.CombinedOutput()
		if err != nil {
			if strings.Contains(string(issuerOutput), "ResourceNotFoundException") ||
				strings.Contains(string(issuerOutput), "No cluster found") {
				t.Logf("[%s] Cluster %s does not exist, skipping OIDC provider deletion", ProviderAWS, clusterName)
				continue
			}
			t.Logf("[%s] Warning: failed to get OIDC issuer for cluster %s: %v", ProviderAWS, clusterName, err)
			continue
		}

		issuerURL := strings.TrimSpace(string(issuerOutput))
		if issuerURL == "" || issuerURL == "None" {
			t.Logf("[%s] No OIDC provider found for cluster %s", ProviderAWS, clusterName)
			continue
		}

		// Extract the OIDC provider ID from the issuer URL (last part after /)
		parts := strings.Split(issuerURL, "/")
		if len(parts) < 2 {
			t.Logf("[%s] Warning: invalid OIDC issuer URL format: %s", ProviderAWS, issuerURL)
			continue
		}
		oidcProviderID := parts[len(parts)-1]

		// List all OIDC providers and find the one matching our cluster
		listOIDCCmd := exec.Command("aws", "iam", "list-open-id-connect-providers",
			"--query", "OpenIDConnectProviderList[].Arn",
			"--output", "text")
		listOutput, err := listOIDCCmd.CombinedOutput()
		if err != nil {
			t.Logf("[%s] Warning: failed to list OIDC providers: %v", ProviderAWS, err)
			continue
		}

		// Find and delete the matching OIDC provider
		providerARNs := strings.Fields(string(listOutput))
		for _, providerARN := range providerARNs {
			if strings.Contains(providerARN, oidcProviderID) {
				t.Logf("[%s] Deleting OIDC provider: %s", ProviderAWS, providerARN)
				deleteOIDCCmd := exec.Command("aws", "iam", "delete-open-id-connect-provider",
					"--open-id-connect-provider-arn", providerARN)
				if output, err := deleteOIDCCmd.CombinedOutput(); err != nil {
					t.Logf("[%s] Warning: failed to delete OIDC provider %s: %v", ProviderAWS, providerARN, err)
					t.Logf("[%s] Output: %s", ProviderAWS, string(output))
				} else {
					t.Logf("[%s] Successfully deleted OIDC provider: %s", ProviderAWS, providerARN)
				}
				break
			}
		}
	}

	return nil
}

func (r *AwsRegion) deleteSecurityGroups(
	ctx context.Context, t *testing.T, ec2Clients map[string]*ec2.EC2,
) error {
	var wg sync.WaitGroup
	var mu sync.Mutex
	var deletionErrors []error

	for region, ec2Client := range ec2Clients {
		wg.Add(1)
		go func(reg string, ec2Client *ec2.EC2) {
			defer wg.Done()

			// List security groups by ManagedBy tag and TestRunID
			output, err := ec2Client.DescribeSecurityGroupsWithContext(ctx, &ec2.DescribeSecurityGroupsInput{
				Filters: []*ec2.Filter{
					{
						Name:   aws.String("tag:ManagedBy"),
						Values: []*string{aws.String("helm-charts-e2e")},
					},
					{
						Name:   aws.String("tag:TestRunID"),
						Values: []*string{aws.String(r.TestRunID)},
					},
				},
			})
			if err != nil {
				t.Logf("[%s] Warning: failed to list security groups in region %s: %v", ProviderAWS, reg, err)
				mu.Lock()
				deletionErrors = append(deletionErrors, fmt.Errorf("failed to list security groups in %s: %w", reg, err))
				mu.Unlock()
				return
			}

			for _, sg := range output.SecurityGroups {
				_, err := ec2Client.DeleteSecurityGroupWithContext(ctx, &ec2.DeleteSecurityGroupInput{
					GroupId: sg.GroupId,
				})
				if err != nil {
					t.Logf("[%s] Warning: failed to delete security group %s: %v", ProviderAWS, *sg.GroupId, err)
					mu.Lock()
					deletionErrors = append(deletionErrors, fmt.Errorf("failed to delete SG %s: %w", *sg.GroupId, err))
					mu.Unlock()
				} else {
					t.Logf("[%s] Deleted security group %s", ProviderAWS, *sg.GroupId)
				}
			}
		}(region, ec2Client)
	}

	wg.Wait()

	if len(deletionErrors) > 0 {
		return fmt.Errorf("failed to delete %d security group(s)", len(deletionErrors))
	}
	return nil
}

func (r *AwsRegion) deleteSubnetsAndRouteTables(
	ctx context.Context, t *testing.T, ec2Clients map[string]*ec2.EC2,
) error {
	var wg sync.WaitGroup

	for region, ec2Client := range ec2Clients {
		wg.Add(1)
		go func(reg string, ec2Client *ec2.EC2) {
			defer wg.Done()

			// Delete subnets
			subnets, err := ec2Client.DescribeSubnetsWithContext(ctx, &ec2.DescribeSubnetsInput{
				Filters: []*ec2.Filter{
					{
						Name:   aws.String("tag:ManagedBy"),
						Values: []*string{aws.String("helm-charts-e2e")},
					},
					{
						Name:   aws.String("tag:TestRunID"),
						Values: []*string{aws.String(r.TestRunID)},
					},
				},
			})
			if err != nil {
				t.Logf("[%s] Warning: failed to list subnets in region %s: %v", ProviderAWS, reg, err)
				return
			}

			for _, subnet := range subnets.Subnets {
				_, err := ec2Client.DeleteSubnetWithContext(ctx, &ec2.DeleteSubnetInput{
					SubnetId: subnet.SubnetId,
				})
				if err != nil {
					t.Logf("[%s] Warning: failed to delete subnet %s: %v", ProviderAWS, *subnet.SubnetId, err)
				} else {
					t.Logf("[%s] Deleted subnet %s", ProviderAWS, *subnet.SubnetId)
				}
			}

			// Delete route tables
			routeTables, err := ec2Client.DescribeRouteTablesWithContext(ctx, &ec2.DescribeRouteTablesInput{
				Filters: []*ec2.Filter{
					{
						Name:   aws.String("tag:ManagedBy"),
						Values: []*string{aws.String("helm-charts-e2e")},
					},
					{
						Name:   aws.String("tag:TestRunID"),
						Values: []*string{aws.String(r.TestRunID)},
					},
				},
			})
			if err != nil {
				t.Logf("[%s] Warning: failed to list route tables in region %s: %v", ProviderAWS, reg, err)
				return
			}

			for _, rtb := range routeTables.RouteTables {
				// Disassociate from subnets first
				for _, assoc := range rtb.Associations {
					if assoc.SubnetId != nil {
						_, err := ec2Client.DisassociateRouteTableWithContext(ctx, &ec2.DisassociateRouteTableInput{
							AssociationId: assoc.RouteTableAssociationId,
						})
						if err != nil {
							t.Logf("[%s] Warning: failed to disassociate route table %s: %v", ProviderAWS, *rtb.RouteTableId, err)
						}
					}
				}

				_, err := ec2Client.DeleteRouteTableWithContext(ctx, &ec2.DeleteRouteTableInput{
					RouteTableId: rtb.RouteTableId,
				})
				if err != nil {
					t.Logf("[%s] Warning: failed to delete route table %s: %v", ProviderAWS, *rtb.RouteTableId, err)
				} else {
					t.Logf("[%s] Deleted route table %s", ProviderAWS, *rtb.RouteTableId)
				}
			}
		}(region, ec2Client)
	}

	wg.Wait()
	return nil
}

func (r *AwsRegion) deleteInternetGateways(
	ctx context.Context, t *testing.T, ec2Clients map[string]*ec2.EC2,
) error {
	var wg sync.WaitGroup

	for region, ec2Client := range ec2Clients {
		wg.Add(1)
		go func(reg string, ec2Client *ec2.EC2) {
			defer wg.Done()

			igws, err := ec2Client.DescribeInternetGatewaysWithContext(ctx, &ec2.DescribeInternetGatewaysInput{
				Filters: []*ec2.Filter{
					{
						Name:   aws.String("tag:ManagedBy"),
						Values: []*string{aws.String("helm-charts-e2e")},
					},
					{
						Name:   aws.String("tag:TestRunID"),
						Values: []*string{aws.String(r.TestRunID)},
					},
				},
			})
			if err != nil {
				t.Logf("[%s] Warning: failed to list internet gateways in region %s: %v", ProviderAWS, reg, err)
				return
			}

			for _, igw := range igws.InternetGateways {
				// Detach from VPCs first
				for _, attachment := range igw.Attachments {
					_, err := ec2Client.DetachInternetGatewayWithContext(ctx, &ec2.DetachInternetGatewayInput{
						InternetGatewayId: igw.InternetGatewayId,
						VpcId:             attachment.VpcId,
					})
					if err != nil {
						t.Logf("[%s] Warning: failed to detach internet gateway %s: %v", ProviderAWS, *igw.InternetGatewayId, err)
					}
				}

				_, err := ec2Client.DeleteInternetGatewayWithContext(ctx, &ec2.DeleteInternetGatewayInput{
					InternetGatewayId: igw.InternetGatewayId,
				})
				if err != nil {
					t.Logf("[%s] Warning: failed to delete internet gateway %s: %v", ProviderAWS, *igw.InternetGatewayId, err)
				} else {
					t.Logf("[%s] Deleted internet gateway %s", ProviderAWS, *igw.InternetGatewayId)
				}
			}
		}(region, ec2Client)
	}

	wg.Wait()
	return nil
}

func (r *AwsRegion) deleteVPCs(
	ctx context.Context, t *testing.T, ec2Clients map[string]*ec2.EC2,
) error {
	var wg sync.WaitGroup

	for region, ec2Client := range ec2Clients {
		wg.Add(1)
		go func(reg string, ec2Client *ec2.EC2) {
			defer wg.Done()

			// Small delay to ensure all dependent resources are cleaned up
			time.Sleep(10 * time.Second)

			vpcs, err := ec2Client.DescribeVpcsWithContext(ctx, &ec2.DescribeVpcsInput{
				Filters: []*ec2.Filter{
					{
						Name:   aws.String("tag:ManagedBy"),
						Values: []*string{aws.String("helm-charts-e2e")},
					},
					{
						Name:   aws.String("tag:TestRunID"),
						Values: []*string{aws.String(r.TestRunID)},
					},
				},
			})
			if err != nil {
				t.Logf("[%s] Warning: failed to list VPCs in region %s: %v", ProviderAWS, reg, err)
				return
			}

			for _, vpc := range vpcs.Vpcs {
				_, err := ec2Client.DeleteVpcWithContext(ctx, &ec2.DeleteVpcInput{
					VpcId: vpc.VpcId,
				})
				if err != nil && !IsResourceNotFound(err) {
					t.Logf("[%s] Warning: failed to delete VPC %s: %v", ProviderAWS, *vpc.VpcId, err)
				} else if err == nil {
					t.Logf("[%s] Deleted VPC %s", ProviderAWS, *vpc.VpcId)
				}
			}
		}(region, ec2Client)
	}

	wg.Wait()
	return nil
}

// ─── ADDITIONAL CLEANUP FUNCTIONS FOR VPC DEPENDENCIES ─────────────────────────

// deleteLoadBalancers deletes all load balancers in VPCs managed by this test
func (r *AwsRegion) deleteLoadBalancers(
	ctx context.Context, t *testing.T, ec2Clients map[string]*ec2.EC2,
) error {
	// Need to use AWS CLI for ELBv2 operations
	for region := range ec2Clients {
		elbCmd := exec.Command("aws", "elbv2", "describe-load-balancers",
			"--region", region,
			"--query", "LoadBalancers[*].[LoadBalancerArn,VpcId]",
			"--output", "text")

		lbOutput, err := elbCmd.CombinedOutput()
		if err != nil {
			t.Logf("[%s] Warning: failed to list load balancers in region %s", ProviderAWS, region)
			continue
		}

		lines := strings.Split(strings.TrimSpace(string(lbOutput)), "\n")
		for _, line := range lines {
			if line == "" {
				continue
			}

			fields := strings.Fields(line)
			if len(fields) < 2 {
				continue
			}

			lbArn := fields[0]
			vpcID := fields[1]

			// Check if this VPC belongs to our test
			if r.isTestVPC(ctx, ec2Clients[region], vpcID) {
				t.Logf("[%s] Deleting load balancer in test VPC %s", ProviderAWS, vpcID)
				delCmd := exec.Command("aws", "elbv2", "delete-load-balancer",
					"--region", region,
					"--load-balancer-arn", lbArn)

				if err := delCmd.Run(); err != nil {
					t.Logf("[%s] Warning: failed to delete load balancer %s: %v", ProviderAWS, lbArn, err)
				} else {
					t.Logf("[%s] Deleted load balancer %s", ProviderAWS, lbArn)
				}
			}
		}
	}

	// Wait for load balancers to be fully deleted
	t.Logf("[%s] Waiting for load balancers to be deleted...", ProviderAWS)
	time.Sleep(30 * time.Second)

	return nil
}

// isTestVPC checks if a VPC has our test tags
func (r *AwsRegion) isTestVPC(ctx context.Context, ec2Client *ec2.EC2, vpcID string) bool {
	vpcs, err := ec2Client.DescribeVpcsWithContext(ctx, &ec2.DescribeVpcsInput{
		VpcIds: []*string{aws.String(vpcID)},
		Filters: []*ec2.Filter{
			{
				Name:   aws.String("tag:ManagedBy"),
				Values: []*string{aws.String("helm-charts-e2e")},
			},
			{
				Name:   aws.String("tag:TestRunID"),
				Values: []*string{aws.String(r.TestRunID)},
			},
		},
	})
	return err == nil && len(vpcs.Vpcs) > 0
}

// deleteNATGateways deletes NAT gateways in test VPCs
func (r *AwsRegion) deleteNATGateways(
	ctx context.Context, t *testing.T, ec2Clients map[string]*ec2.EC2,
) error {
	var wg sync.WaitGroup

	for region, ec2Client := range ec2Clients {
		wg.Add(1)
		go func(reg string, ec2Client *ec2.EC2) {
			defer wg.Done()

			// List NAT gateways with our tags
			natGWs, err := ec2Client.DescribeNatGatewaysWithContext(ctx, &ec2.DescribeNatGatewaysInput{
				Filter: []*ec2.Filter{
					{
						Name:   aws.String("tag:ManagedBy"),
						Values: []*string{aws.String("helm-charts-e2e")},
					},
					{
						Name:   aws.String("tag:TestRunID"),
						Values: []*string{aws.String(r.TestRunID)},
					},
				},
			})
			if err != nil {
				t.Logf("[%s] Warning: failed to list NAT gateways in region %s: %v", ProviderAWS, reg, err)
				return
			}

			for _, natGW := range natGWs.NatGateways {
				if *natGW.State == "deleted" || *natGW.State == "deleting" {
					continue
				}

				_, err := ec2Client.DeleteNatGatewayWithContext(ctx, &ec2.DeleteNatGatewayInput{
					NatGatewayId: natGW.NatGatewayId,
				})
				if err != nil {
					t.Logf("[%s] Warning: failed to delete NAT gateway %s: %v", ProviderAWS, *natGW.NatGatewayId, err)
				} else {
					t.Logf("[%s] Deleted NAT gateway %s", ProviderAWS, *natGW.NatGatewayId)
				}
			}
		}(region, ec2Client)
	}

	wg.Wait()

	// Wait for NAT gateways to be fully deleted
	if len(ec2Clients) > 0 {
		t.Logf("[%s] Waiting for NAT gateways to be deleted...", ProviderAWS)
		time.Sleep(30 * time.Second)
	}

	return nil
}

// deleteNetworkInterfaces deletes network interfaces in test VPCs
func (r *AwsRegion) deleteNetworkInterfaces(
	ctx context.Context, t *testing.T, ec2Clients map[string]*ec2.EC2,
) error {
	// ENIs created by EKS/load balancers take time to be released - retry a few times
	maxRetries := 3
	for retry := 0; retry < maxRetries; retry++ {
		if retry > 0 {
			t.Logf("[%s] Retry %d/%d for ENI deletion", ProviderAWS, retry, maxRetries-1)
			time.Sleep(30 * time.Second)
		}

		var wg sync.WaitGroup
		var mu sync.Mutex
		deletedAny := false

		for region, ec2Client := range ec2Clients {
			wg.Add(1)
			go func(reg string, ec2Client *ec2.EC2) {
				defer wg.Done()

				// Get all VPCs for this test
				vpcs, err := ec2Client.DescribeVpcsWithContext(ctx, &ec2.DescribeVpcsInput{
					Filters: []*ec2.Filter{
						{
							Name:   aws.String("tag:ManagedBy"),
							Values: []*string{aws.String("helm-charts-e2e")},
						},
						{
							Name:   aws.String("tag:TestRunID"),
							Values: []*string{aws.String(r.TestRunID)},
						},
					},
				})
				if err != nil {
					return
				}

				for _, vpc := range vpcs.Vpcs {
					// List network interfaces in this VPC
					enis, err := ec2Client.DescribeNetworkInterfacesWithContext(ctx, &ec2.DescribeNetworkInterfacesInput{
						Filters: []*ec2.Filter{
							{
								Name:   aws.String("vpc-id"),
								Values: []*string{vpc.VpcId},
							},
							{
								Name:   aws.String("status"),
								Values: []*string{aws.String("available")}, // Only try to delete available ENIs
							},
						},
					})
					if err != nil {
						continue
					}

					for _, eni := range enis.NetworkInterfaces {
						// Try to delete the ENI
						_, err := ec2Client.DeleteNetworkInterfaceWithContext(ctx, &ec2.DeleteNetworkInterfaceInput{
							NetworkInterfaceId: eni.NetworkInterfaceId,
						})
						if err == nil {
							mu.Lock()
							deletedAny = true
							mu.Unlock()
							t.Logf("[%s] Deleted ENI %s", ProviderAWS, *eni.NetworkInterfaceId)
						}
					}
				}
			}(region, ec2Client)
		}

		wg.Wait()

		// If we didn't delete anything this round, we're done
		if !deletedAny {
			break
		}
	}

	return nil
}

// releaseElasticIPs releases Elastic IPs associated with test resources
func (r *AwsRegion) releaseElasticIPs(
	ctx context.Context, t *testing.T, ec2Clients map[string]*ec2.EC2,
) error {
	var wg sync.WaitGroup

	for region, ec2Client := range ec2Clients {
		wg.Add(1)
		go func(reg string, ec2Client *ec2.EC2) {
			defer wg.Done()

			// List all Elastic IPs with our tags
			eips, err := ec2Client.DescribeAddressesWithContext(ctx, &ec2.DescribeAddressesInput{
				Filters: []*ec2.Filter{
					{
						Name:   aws.String("tag:ManagedBy"),
						Values: []*string{aws.String("helm-charts-e2e")},
					},
					{
						Name:   aws.String("tag:TestRunID"),
						Values: []*string{aws.String(r.TestRunID)},
					},
				},
			})
			if err != nil {
				return
			}

			for _, eip := range eips.Addresses {
				// Disassociate if associated
				if eip.AssociationId != nil {
					_, _ = ec2Client.DisassociateAddressWithContext(ctx, &ec2.DisassociateAddressInput{
						AssociationId: eip.AssociationId,
					})
					time.Sleep(2 * time.Second)
				}

				// Release the Elastic IP
				_, err := ec2Client.ReleaseAddressWithContext(ctx, &ec2.ReleaseAddressInput{
					AllocationId: eip.AllocationId,
				})
				if err != nil {
					t.Logf("[%s] Warning: failed to release EIP %s: %v", ProviderAWS, *eip.AllocationId, err)
				} else {
					t.Logf("[%s] Released Elastic IP %s", ProviderAWS, *eip.AllocationId)
				}
			}
		}(region, ec2Client)
	}

	wg.Wait()
	return nil
}

// deleteVPCEndpoints deletes VPC endpoints in test VPCs
func (r *AwsRegion) deleteVPCEndpoints(
	ctx context.Context, t *testing.T, ec2Clients map[string]*ec2.EC2,
) error {
	var wg sync.WaitGroup

	for region, ec2Client := range ec2Clients {
		wg.Add(1)
		go func(reg string, ec2Client *ec2.EC2) {
			defer wg.Done()

			// Get VPC IDs for this test
			vpcs, err := ec2Client.DescribeVpcsWithContext(ctx, &ec2.DescribeVpcsInput{
				Filters: []*ec2.Filter{
					{
						Name:   aws.String("tag:ManagedBy"),
						Values: []*string{aws.String("helm-charts-e2e")},
					},
					{
						Name:   aws.String("tag:TestRunID"),
						Values: []*string{aws.String(r.TestRunID)},
					},
				},
			})
			if err != nil {
				return
			}

			for _, vpc := range vpcs.Vpcs {
				// List VPC endpoints
				endpoints, err := ec2Client.DescribeVpcEndpointsWithContext(ctx, &ec2.DescribeVpcEndpointsInput{
					Filters: []*ec2.Filter{
						{
							Name:   aws.String("vpc-id"),
							Values: []*string{vpc.VpcId},
						},
					},
				})
				if err != nil {
					continue
				}

				var endpointIDs []*string
				for _, endpoint := range endpoints.VpcEndpoints {
					endpointIDs = append(endpointIDs, endpoint.VpcEndpointId)
				}

				if len(endpointIDs) > 0 {
					_, err := ec2Client.DeleteVpcEndpointsWithContext(ctx, &ec2.DeleteVpcEndpointsInput{
						VpcEndpointIds: endpointIDs,
					})
					if err != nil {
						t.Logf("[%s] Warning: failed to delete VPC endpoints: %v", ProviderAWS, err)
					} else {
						t.Logf("[%s] Deleted %d VPC endpoints in VPC %s", ProviderAWS, len(endpointIDs), *vpc.VpcId)
					}
				}
			}
		}(region, ec2Client)
	}

	wg.Wait()
	return nil
}

// deleteClusterResources deletes cluster-related resources in parallel
func (r *AwsRegion) deleteClusterResources(
	ctx context.Context, t *testing.T, ec2Clients map[string]*ec2.EC2,
) error {
	var wg sync.WaitGroup
	errors := make(chan error, 5)

	// Delete Load Balancers
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := r.deleteLoadBalancers(ctx, t, ec2Clients); err != nil {
			errors <- fmt.Errorf("load balancer deletion failed: %w", err)
		}
	}()

	// Delete NAT Gateways
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := r.deleteNATGateways(ctx, t, ec2Clients); err != nil {
			errors <- fmt.Errorf("NAT gateway deletion failed: %w", err)
		}
	}()

	// Delete Network Interfaces
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := r.deleteNetworkInterfaces(ctx, t, ec2Clients); err != nil {
			errors <- fmt.Errorf("network interface deletion failed: %w", err)
		}
	}()

	// Delete Elastic IPs
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := r.deleteElasticIPs(ctx, t, ec2Clients); err != nil {
			errors <- fmt.Errorf("elastic IP deletion failed: %w", err)
		}
	}()

	// Delete VPC Endpoints
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := r.deleteVPCEndpoints(ctx, t, ec2Clients); err != nil {
			errors <- fmt.Errorf("VPC endpoint deletion failed: %w", err)
		}
	}()

	wg.Wait()
	close(errors)

	// Collect any errors
	var errs []error
	for err := range errors {
		errs = append(errs, err)
	}

	if len(errs) > 0 {
		return fmt.Errorf("cluster resource deletion had %d errors: %v", len(errs), errs)
	}

	return nil
}

// deleteElasticIPs releases and deletes all Elastic IPs created by our test.
// This includes:
// 1. EIPs tagged with our TestRunID (if we created them explicitly)
// 2. EIPs associated with Load Balancers in our VPCs (created by Kubernetes services)
// 3. EIPs associated with NAT Gateways in our VPCs (if any exist)
func (r *AwsRegion) deleteElasticIPs(
	ctx context.Context, t *testing.T, ec2Clients map[string]*ec2.EC2,
) error {
	var wg sync.WaitGroup

	for region, ec2Client := range ec2Clients {
		wg.Add(1)
		go func(reg string, ec2Client *ec2.EC2) {
			defer wg.Done()

			// Collect all EIP allocation IDs to delete
			eipMap := make(map[string]bool) // Use a map to deduplicate

			// 1. Find EIPs with our tags
			taggedEIPs, err := ec2Client.DescribeAddressesWithContext(ctx, &ec2.DescribeAddressesInput{
				Filters: []*ec2.Filter{
					{
						Name:   aws.String("tag:ManagedBy"),
						Values: []*string{aws.String("helm-charts-e2e")},
					},
					{
						Name:   aws.String("tag:TestRunID"),
						Values: []*string{aws.String(r.TestRunID)},
					},
				},
			})
			if err == nil {
				for _, eip := range taggedEIPs.Addresses {
					if eip.AllocationId != nil {
						eipMap[*eip.AllocationId] = true
					}
				}
			} else if !IsResourceNotFound(err) {
				t.Logf("[%s] Warning: failed to describe tagged Elastic IPs in region %s: %v", ProviderAWS, reg, err)
			}

			// 2. Find our VPCs and get all EIPs associated with resources in those VPCs
			vpcs, err := ec2Client.DescribeVpcsWithContext(ctx, &ec2.DescribeVpcsInput{
				Filters: []*ec2.Filter{
					{
						Name:   aws.String("tag:ManagedBy"),
						Values: []*string{aws.String("helm-charts-e2e")},
					},
					{
						Name:   aws.String("tag:TestRunID"),
						Values: []*string{aws.String(r.TestRunID)},
					},
				},
			})
			if err == nil && vpcs.Vpcs != nil {
				// For each VPC, find all EIPs (these are typically from NLBs created by Kubernetes)
				for _, vpc := range vpcs.Vpcs {
					vpcID := aws.StringValue(vpc.VpcId)

					// Find all addresses and check if they're in this VPC via their network interface
					allEIPs, err := ec2Client.DescribeAddressesWithContext(ctx, &ec2.DescribeAddressesInput{})
					if err == nil {
						for _, eip := range allEIPs.Addresses {
							// Check if EIP is associated with a network interface in our VPC
							if eip.NetworkInterfaceId != nil {
								eni, err := ec2Client.DescribeNetworkInterfacesWithContext(ctx, &ec2.DescribeNetworkInterfacesInput{
									NetworkInterfaceIds: []*string{eip.NetworkInterfaceId},
								})
								if err == nil && len(eni.NetworkInterfaces) > 0 {
									if aws.StringValue(eni.NetworkInterfaces[0].VpcId) == vpcID {
										if eip.AllocationId != nil {
											eipMap[*eip.AllocationId] = true
											t.Logf("[%s] Found Elastic IP %s in VPC %s (likely from NLB)", ProviderAWS, *eip.AllocationId, vpcID)
										}
									}
								}
							}
						}
					}
				}
			}

			// Now delete all collected EIPs
			for allocationID := range eipMap {
				// Get EIP details to check if it's associated
				eipDetails, err := ec2Client.DescribeAddressesWithContext(ctx, &ec2.DescribeAddressesInput{
					AllocationIds: []*string{aws.String(allocationID)},
				})
				if err != nil {
					if !IsResourceNotFound(err) {
						t.Logf("[%s] Warning: failed to describe Elastic IP %s: %v", ProviderAWS, allocationID, err)
					}
					continue
				}

				if len(eipDetails.Addresses) == 0 {
					continue
				}

				eip := eipDetails.Addresses[0]

				// Disassociate if associated
				if eip.AssociationId != nil {
					_, err := ec2Client.DisassociateAddressWithContext(ctx, &ec2.DisassociateAddressInput{
						AssociationId: eip.AssociationId,
					})
					if err != nil && !IsResourceNotFound(err) {
						t.Logf("[%s] Warning: failed to disassociate Elastic IP %s: %v", ProviderAWS, allocationID, err)
					} else {
						t.Logf("[%s] Disassociated Elastic IP %s", ProviderAWS, allocationID)
					}
				}

				// Release the Elastic IP
				_, err = ec2Client.ReleaseAddressWithContext(ctx, &ec2.ReleaseAddressInput{
					AllocationId: aws.String(allocationID),
				})
				if err != nil {
					if !IsResourceNotFound(err) {
						t.Logf("[%s] Warning: failed to release Elastic IP %s: %v", ProviderAWS, allocationID, err)
					}
				} else {
					t.Logf("[%s] Released Elastic IP %s in region %s", ProviderAWS, allocationID, reg)
				}
			}
		}(region, ec2Client)
	}

	wg.Wait()
	return nil
}

// revokeSecurityGroupRules revokes all ingress and egress rules from security groups.
// This is necessary to break self-referencing and cross-referencing dependencies that prevent SG deletion.
// Called with retry logic (3 attempts × 30s) to handle AWS eventual consistency issues.
func (r *AwsRegion) revokeSecurityGroupRules(
	ctx context.Context, t *testing.T, ec2Clients map[string]*ec2.EC2,
) error {
	var wg sync.WaitGroup

	for region, ec2Client := range ec2Clients {
		wg.Add(1)
		go func(reg string, ec2Client *ec2.EC2) {
			defer wg.Done()

			// List security groups with our tags
			sgs, err := ec2Client.DescribeSecurityGroupsWithContext(ctx, &ec2.DescribeSecurityGroupsInput{
				Filters: []*ec2.Filter{
					{
						Name:   aws.String("tag:ManagedBy"),
						Values: []*string{aws.String("helm-charts-e2e")},
					},
					{
						Name:   aws.String("tag:TestRunID"),
						Values: []*string{aws.String(r.TestRunID)},
					},
				},
			})
			if err != nil {
				return
			}

			for _, sg := range sgs.SecurityGroups {
				// Revoke ingress rules
				if len(sg.IpPermissions) > 0 {
					_, err := ec2Client.RevokeSecurityGroupIngressWithContext(ctx, &ec2.RevokeSecurityGroupIngressInput{
						GroupId:       sg.GroupId,
						IpPermissions: sg.IpPermissions,
					})
					if err != nil {
						t.Logf("[%s] Warning: failed to revoke ingress rules for SG %s: %v", ProviderAWS, *sg.GroupId, err)
					} else {
						t.Logf("[%s] Revoked ingress rules for SG %s", ProviderAWS, *sg.GroupId)
					}
				}

				// Revoke egress rules
				if len(sg.IpPermissionsEgress) > 0 {
					_, err := ec2Client.RevokeSecurityGroupEgressWithContext(ctx, &ec2.RevokeSecurityGroupEgressInput{
						GroupId:       sg.GroupId,
						IpPermissions: sg.IpPermissionsEgress,
					})
					if err != nil {
						t.Logf("[%s] Warning: failed to revoke egress rules for SG %s: %v", ProviderAWS, *sg.GroupId, err)
					}
				}
			}
		}(region, ec2Client)
	}

	wg.Wait()
	return nil
}

// ─── HELPER FUNCTIONS ────────────────────────────────────────────────────────────

// disableTLSVerificationInKubeconfig modifies the kubeconfig to skip TLS verification for a specific cluster
// This is needed in corporate proxy environments (e.g., Netskope) that intercept SSL/TLS connections
func disableTLSVerificationInKubeconfig(t *testing.T, clusterName string) error {
	// Only disable TLS verification if explicitly enabled via environment variable
	if os.Getenv(EnvKubectlInsecureSkipTLSVerify) != "true" {
		return nil // Skip if the environment variable is not set
	}

	t.Logf("[%s] Disabling TLS verification in kubeconfig (%s=true)", ProviderAWS, EnvKubectlInsecureSkipTLSVerify)

	// Use mutex to prevent concurrent kubeconfig file corruption
	kubeconfigMutex.Lock()
	defer kubeconfigMutex.Unlock()

	// Get a kubeconfig path
	kubeconfigPath := os.Getenv("KUBECONFIG")
	if kubeconfigPath == "" {
		kubeconfigPath = os.ExpandEnv("$HOME/.kube/config")
	}

	// Load the kubeconfig
	kubeConfig, err := clientcmd.LoadFromFile(kubeconfigPath)
	if err != nil {
		return fmt.Errorf("failed to load kubeconfig: %w", err)
	}

	// Find ALL cluster entries that match (eksctl creates multiple entries)
	// e.g., both "arn:aws:eks:region:account:cluster/name" and "name.region.eksctl.io"
	modifiedCount := 0
	for name, cluster := range kubeConfig.Clusters {
		if name == clusterName || strings.Contains(name, clusterName) {
			// Disable TLS verification
			cluster.InsecureSkipTLSVerify = true
			// Clear certificate authority data since we're skipping verification
			cluster.CertificateAuthorityData = nil
			cluster.CertificateAuthority = ""
			kubeConfig.Clusters[name] = cluster
			modifiedCount++
			t.Logf("[%s] Disabled TLS verification for cluster entry: %s", ProviderAWS, name)
		}
	}

	if modifiedCount == 0 {
		return fmt.Errorf("cluster %s not found in kubeconfig", clusterName)
	}

	// Write the modified config back
	err = clientcmd.WriteToFile(*kubeConfig, kubeconfigPath)
	if err != nil {
		return fmt.Errorf("failed to write kubeconfig: %w", err)
	}

	t.Logf("[%s] Successfully disabled TLS verification for %d cluster entries", ProviderAWS, modifiedCount)
	return nil
}

// retryWithBackoff executes a function with retry logic for AWS eventual consistency.
// It retries the function up to maxAttempts times with a delay between attempts.
// Returns nil if any attempt succeeds, or the last error if all attempts fail.
func retryWithBackoff(
	t *testing.T, resourceType string, maxAttempts int, delay time.Duration, fn func() error,
) error {
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if attempt > 1 {
			t.Logf("[%s] Retry %d/%d for %s deletion after waiting...", ProviderAWS, attempt, maxAttempts, resourceType)
			time.Sleep(delay)
		}
		err := fn()
		if err == nil {
			return nil
		}
		if attempt == maxAttempts {
			return err
		}
	}
	return nil
}
