package infra

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/eks"
	ekstypes "github.com/aws/aws-sdk-go-v2/service/eks/types"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	iamtypes "github.com/aws/aws-sdk-go-v2/service/iam/types"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/gruntwork-io/terratest/modules/k8s"
	"github.com/gruntwork-io/terratest/modules/random"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/cockroachdb/helm-charts/tests/e2e/coredns"
	"github.com/cockroachdb/helm-charts/tests/e2e/operator"
)

// ─── AWS GLOBAL CONSTANTS ───────────────────────────────────────────────────────

const (
	awsVpcName       = "nishanth-cockroachdb-vpc"
	subnetNamePrefix = "helm-charts"
	ClusterRoleARN   = "arn:aws:iam::541263489771:role/EKSClusterRole"
	NodeRoleARN      = "arn:aws:iam::541263489771:role/EKSNodegroupRole"
)

// AwsClusterSetupConfig holds per‐cluster parameters
type AwsClusterSetupConfig struct {
	Region          string
	SubnetRange     []string // e.g. {"172.28.12.0/24", "172.28.24.0/24", "172.28.36.0/24"}
	ElasticAllocIDs []string // will be populated with eipalloc- IDs
	ClusterName     string   // e.g. "cockroachdb-east-1"
	InstanceTypes   []string // e.g. {"m5.large"}
	DesiredSize     int32    // e.g. 3
	MinSize         int32    // e.g. 3
	MaxSize         int32    // e.g. 4
	ServiceCIDR     string   // e.g. "10.200.0.0/16"
	UseElasticIP    bool     // if true, reuse or allocate EIPs for CoreDNS NLB
}

// Fill in your cluster list here; index order must match r.Clusters
var awsClusterSetups = []AwsClusterSetupConfig{
	{
		Region:          "us-east-1",
		SubnetRange:     getAwsSubnetRanges("us-east-1"),
		ElasticAllocIDs: []string{},
		ClusterName:     "cockroachdb-east-1",
		InstanceTypes:   []string{AWSDefaultInstanceType},
		DesiredSize:     DefaultNodeCount,
		MinSize:         DefaultMinNodeCount,
		MaxSize:         DefaultMaxNodeCount,
		ServiceCIDR:     getAwsServiceCIDR("us-east-1"),
		UseElasticIP:    true,
	},
	{
		Region:          "us-east-2",
		SubnetRange:     getAwsSubnetRanges("us-east-2"),
		ElasticAllocIDs: []string{},
		ClusterName:     "cockroachdb-east-2",
		InstanceTypes:   []string{AWSDefaultInstanceType},
		DesiredSize:     DefaultNodeCount,
		MinSize:         DefaultMinNodeCount,
		MaxSize:         DefaultMaxNodeCount,
		ServiceCIDR:     getAwsServiceCIDR("us-east-2"),
		UseElasticIP:    true,
	},
	{
		Region:          "us-west-1",
		SubnetRange:     getAwsSubnetRanges("us-west-1"),
		ElasticAllocIDs: []string{},
		ClusterName:     "cockroachdb-west-1",
		InstanceTypes:   []string{AWSDefaultInstanceType},
		DesiredSize:     DefaultNodeCount,
		MinSize:         DefaultMinNodeCount,
		MaxSize:         DefaultMaxNodeCount,
		ServiceCIDR:     getAwsServiceCIDR("us-west-1"),
		UseElasticIP:    true,
	},
}

// Helper functions to get network configuration from common.go
func getAwsSubnetRanges(region string) []string {
	if config, ok := NetworkConfigs[ProviderAWS][region]; ok {
		if ranges, ok := config.(map[string]interface{})["SubnetRanges"].([]string); ok {
			return ranges
		}
	}
	// Fallback defaults if config not found
	return []string{"172.28.12.0/24", "172.28.24.0/24", "172.28.36.0/24"}
}

func getAwsServiceCIDR(region string) string {
	if config, ok := NetworkConfigs[ProviderAWS][region]; ok {
		if cidr, ok := config.(map[string]interface{})["ServiceCIDR"].(string); ok {
			return cidr
		}
	}
	// Fallback default if config not found
	return "10.200.0.0/16"
}

// AwsRegion implements CloudProvider for AWS
type AwsRegion struct {
	*operator.Region
}

// ScaleNodePool scales the node pool in an EKS cluster
func (r *AwsRegion) ScaleNodePool(t *testing.T, location string, nodeCount, index int) {
	t.Logf("[%s] Scaling node pool in cluster %s to %d nodes", ProviderAWS, awsClusterSetups[index].ClusterName, nodeCount)

	// In a real implementation, this would use the AWS SDK to scale the node pool
	// This would include getting the node pool and updating its count
	t.Logf("[%s] Node pool scaling not fully implemented for AWS", ProviderAWS)

	// Uncomment and complete this code when implementing actual scaling:
	/*
		ctx := context.Background()
		cfg, err := awsconfig.LoadDefaultConfig(ctx)
		if err != nil {
			t.Logf("[%s] Failed to load AWS config: %v", ProviderAWS, err)
			return
		}

		setup := awsClusterSetups[index]
		eksClient := eks.NewFromConfig(cfg, func(o *eks.Options) { o.Region = setup.Region })

		// Actual implementation would go here
	*/
}

func (r *AwsRegion) SetUpInfra(t *testing.T) {
	if r.ReusingInfra {
		t.Logf("[%s] Reusing existing infrastructure", ProviderAWS)
		return
	}

	t.Logf("[%s] Setting up infrastructure", ProviderAWS)
	ctx := context.Background()
	cfg, err := awsconfig.LoadDefaultConfig(ctx)
	require.NoError(t, err)

	iamClient := iam.NewFromConfig(cfg)
	stsClient := sts.NewFromConfig(cfg)

	// 1) Ensure IAM Roles exist
	t.Logf("[%s] Ensuring IAM roles exist", ProviderAWS)
	_, err = ensureClusterServiceRole(ctx, iamClient, ClusterRoleARN)
	require.NoError(t, err)
	_, err = ensureNodeInstanceRole(ctx, iamClient, stsClient, NodeRoleARN)
	require.NoError(t, err)

	var clients = make(map[string]client.Client)
	r.CorednsClusterOptions = make(map[string]coredns.CoreDNSClusterOption)
	clusterSubnets := make([][]string, len(r.Clusters))

	for i, clusterName := range r.Clusters {
		setup := awsClusterSetups[i]
		setup.ClusterName = clusterName
		t.Logf("[%s] Setting up cluster %s in region %s", ProviderAWS, clusterName, setup.Region)

		// EC2 & EKS clients scoped to region
		ec2Client := ec2.NewFromConfig(cfg, func(o *ec2.Options) { o.Region = setup.Region })
		eksClient := eks.NewFromConfig(cfg, func(o *eks.Options) { o.Region = setup.Region })

		// 2) Create or get VPC
		vpcID, err := createAwsVPC(ctx, ec2Client, awsVpcName, DefaultVPCCIDR)
		require.NoError(t, err)

		// 2a) Ensure IGW + Public Route Table
		igwID, err := ensureInternetGateway(ctx, ec2Client, vpcID)
		require.NoError(t, err)
		rtID, err := ensurePublicRouteTable(ctx, ec2Client, vpcID, igwID)
		require.NoError(t, err)

		// 3a) Create or reuse 3 public subnets
		subnetIDs, err := createAwsSubnets(ctx, ec2Client, vpcID, setup.SubnetRange, setup.Region)
		require.NoError(t, err)
		// Associate each subnet with the public RT
		for _, snID := range subnetIDs {
			_, err := ec2Client.AssociateRouteTable(ctx, &ec2.AssociateRouteTableInput{
				RouteTableId: aws.String(rtID),
				SubnetId:     aws.String(snID),
			})
			if err != nil && !IsResourceConflict(err) {
				t.Fatalf("[%s] Failed to associate subnet %q with RT %q: %v", ProviderAWS, snID, rtID, err)
			}
		}
		clusterSubnets[i] = subnetIDs

		// 3b) Create control‐plane & worker‐node SGs + firewall rules
		controlSG, workerSG, err := createEKSClusterSecurityGroups(ctx, ec2Client, vpcID, clusterName)
		require.NoError(t, err)

		// 3c) Create EKS control plane (public + private endpoint)
		t.Logf("[%s] Creating EKS cluster %s", ProviderAWS, clusterName)
		err = createEKSCluster(ctx, eksClient, setup, subnetIDs, controlSG)
		require.NoError(t, err)

		// 3d) Update kubeconfig to point to this cluster
		err = UpdateKubeconfigAWS(t, setup.Region, clusterName)
		require.NoError(t, err)

		// 3e) Create managed NodeGroup
		t.Logf("[%s] Creating EKS node group for cluster %s", ProviderAWS, clusterName)
		err = createManagedNodeGroup(ctx, eksClient, setup, subnetIDs, workerSG)
		require.NoError(t, err)

		// 3f) Prepare CoreDNS options
		r.Namespace[clusterName] = fmt.Sprintf("%s-%s", operator.Namespace, strings.ToLower(random.UniqueId()))
		clients[clusterName] = MustNewClientForContext(t, clusterName)
		r.CorednsClusterOptions[operator.CustomDomains[i]] = coredns.CoreDNSClusterOption{
			IPs:       setup.ElasticAllocIDs,
			Namespace: r.Namespace[clusterName],
			Domain:    operator.CustomDomains[i],
		}
	}

	// 4) Deploy CoreDNS ConfigMap + Service (with NLB & EIPs), then rollout restart
	for i, clusterName := range r.Clusters {
		setup := awsClusterSetups[i]
		subnetIDs := clusterSubnets[i]
		kubeConfig, err := k8s.KubeConfigPathFromHomeDirE()
		require.NoError(t, err)

		// 4a) Reuse or allocate Elastic IPs if needed
		if setup.UseElasticIP {
			t.Logf("[%s] Ensuring Elastic IPs for cluster %s", ProviderAWS, clusterName)
			allocationIDs, err := ensureElasticIPs(ctx, ec2.NewFromConfig(cfg, func(o *ec2.Options) {
				o.Region = setup.Region
			}), len(subnetIDs))
			require.NoError(t, err)
			setup.ElasticAllocIDs = allocationIDs
			awsClusterSetups[i].ElasticAllocIDs = allocationIDs
			r.CorednsClusterOptions[operator.CustomDomains[i]] = coredns.CoreDNSClusterOption{
				IPs:       allocationIDs,
				Namespace: r.Namespace[clusterName],
				Domain:    operator.CustomDomains[i],
			}
		}

		// 4b) Deploy CoreDNS
		annotations := GetLoadBalancerAnnotations(ProviderAWS)
		// Add AWS-specific annotations
		annotations["service.beta.kubernetes.io/aws-load-balancer-eip-allocations"] = strings.Join(setup.ElasticAllocIDs, ",")
		annotations["service.beta.kubernetes.io/aws-load-balancer-subnets"] = strings.Join(subnetIDs, ",")

		var staticIP *string // AWS uses EIP allocations instead of direct IPs

		err = DeployCoreDNS(t, clusterName, kubeConfig, staticIP, ProviderAWS, operator.CustomDomains[i], r.CorednsClusterOptions)
		require.NoError(t, err, "failed to deploy CoreDNS to cluster %s", clusterName)
	}

	r.Clients = clients
	r.ReusingInfra = true
	t.Logf("[%s] Infrastructure setup completed", ProviderAWS)
}

// TeardownInfra deletes all AWS resources created by SetUpInfra
func (r *AwsRegion) TeardownInfra(t *testing.T) {
	t.Logf("[%s] Starting infrastructure teardown", ProviderAWS)
	ctx := context.Background()
	cfg, err := awsconfig.LoadDefaultConfig(ctx)
	require.NoError(t, err)

	for _, setup := range awsClusterSetups[:len(r.Clusters)] {
		region := setup.Region
		clusterName := setup.ClusterName

		ec2Client := ec2.NewFromConfig(cfg, func(o *ec2.Options) { o.Region = region })
		eksClient := eks.NewFromConfig(cfg, func(o *eks.Options) { o.Region = region })

		// a) Delete NodeGroup
		ngName := clusterName + "-ng"
		t.Logf("[%s] Deleting node group '%s' from cluster '%s'", ProviderAWS, ngName, clusterName)
		_, _ = eksClient.DeleteNodegroup(ctx, &eks.DeleteNodegroupInput{
			ClusterName:   aws.String(clusterName),
			NodegroupName: aws.String(ngName),
		})
		// Wait until gone
		for {
			_, err := eksClient.DescribeNodegroup(ctx, &eks.DescribeNodegroupInput{
				ClusterName:   aws.String(clusterName),
				NodegroupName: aws.String(ngName),
			})
			if err != nil {
				break
			}
			time.Sleep(15 * time.Second)
		}

		// b) Delete Cluster
		t.Logf("[%s] Deleting EKS cluster '%s'", ProviderAWS, clusterName)
		_, _ = eksClient.DeleteCluster(ctx, &eks.DeleteClusterInput{Name: aws.String(clusterName)})
		// Wait until gone
		for {
			_, err = eksClient.DescribeCluster(ctx, &eks.DescribeClusterInput{Name: aws.String(clusterName)})
			if err != nil {
				break
			}
			time.Sleep(15 * time.Second)
		}

		// c) Release Elastic IPs
		t.Logf("[%s] Releasing Elastic IPs for cluster '%s'", ProviderAWS, clusterName)
		out, _ := ec2Client.DescribeAddresses(ctx, &ec2.DescribeAddressesInput{
			Filters: []ec2types.Filter{
				{
					Name: aws.String("tag:Name"),
					// EC2 tag filters accept '*' and '?' wild-cards, so this matches every prefix variant.
					Values: []string{"self-hosted-testing-eip-*"},
				},
			},
		})

		// Release only the unassociated ones that matched the filter
		for _, addr := range out.Addresses {
			if addr.AssociationId == nil { // still free
				_, _ = ec2Client.ReleaseAddress(ctx, &ec2.ReleaseAddressInput{
					AllocationId: addr.AllocationId,
				})
			}
		}

		// d) Delete Security Groups (by tag Name = <clusterName>-control-sg and <clusterName>-worker-sg)
		t.Logf("[%s] Deleting security groups for cluster '%s'", ProviderAWS, clusterName)
		sgFilters := []ec2types.Filter{
			{Name: aws.String("vpc-id"), Values: []string{awsVpcName}}, // using VPC ID tag works too
			{Name: aws.String("tag:Name"), Values: []string{clusterName + "-control-sg", clusterName + "-worker-sg"}},
		}
		sgOut, _ := ec2Client.DescribeSecurityGroups(ctx, &ec2.DescribeSecurityGroupsInput{Filters: sgFilters})
		for _, sg := range sgOut.SecurityGroups {
			_, _ = ec2Client.DeleteSecurityGroup(ctx, &ec2.DeleteSecurityGroupInput{GroupId: sg.GroupId})
		}

		// e) Delete subnets
		t.Logf("[%s] Deleting subnets for cluster '%s'", ProviderAWS, clusterName)
		subnetFilters := []ec2types.Filter{{Name: aws.String("tag:Name"), Values: []string{subnetNamePrefix + "-*"}}}
		subnetsOut, _ := ec2Client.DescribeSubnets(ctx, &ec2.DescribeSubnetsInput{Filters: subnetFilters})
		for _, sn := range subnetsOut.Subnets {
			_, _ = ec2Client.DeleteSubnet(ctx, &ec2.DeleteSubnetInput{SubnetId: sn.SubnetId})
		}

		// f) Delete Route Table
		t.Logf("[%s] Cleaning up networking resources", ProviderAWS)
		vpcsOut, _ := ec2Client.DescribeVpcs(ctx, &ec2.DescribeVpcsInput{Filters: []ec2types.Filter{{Name: aws.String("tag:Name"), Values: []string{awsVpcName}}}})
		if len(vpcsOut.Vpcs) > 0 {
			vpcID := *vpcsOut.Vpcs[0].VpcId
			rtFilters := []ec2types.Filter{{Name: aws.String("tag:Name"), Values: []string{vpcID + "-public-rt"}}}
			rtOut, _ := ec2Client.DescribeRouteTables(ctx, &ec2.DescribeRouteTablesInput{Filters: rtFilters})
			for _, rt := range rtOut.RouteTables {
				_, _ = ec2Client.DeleteRouteTable(ctx, &ec2.DeleteRouteTableInput{RouteTableId: rt.RouteTableId})
			}

			// g) Detach & Delete Internet Gateway
			igwOut, _ := ec2Client.DescribeInternetGateways(ctx, &ec2.DescribeInternetGatewaysInput{
				Filters: []ec2types.Filter{{Name: aws.String("attachment.vpc-id"), Values: []string{vpcID}}},
			})
			for _, igw := range igwOut.InternetGateways {
				_, _ = ec2Client.DetachInternetGateway(ctx, &ec2.DetachInternetGatewayInput{
					InternetGatewayId: igw.InternetGatewayId,
					VpcId:             aws.String(vpcID),
				})
				_, _ = ec2Client.DeleteInternetGateway(ctx, &ec2.DeleteInternetGatewayInput{
					InternetGatewayId: igw.InternetGatewayId,
				})
			}

			// h) Finally, delete the VPC
			t.Logf("[%s] Deleting VPC '%s'", ProviderAWS, vpcID)
			_, _ = ec2Client.DeleteVpc(ctx, &ec2.DeleteVpcInput{VpcId: aws.String(vpcID)})
		}
	}

	t.Logf("[%s] Infrastructure teardown completed", ProviderAWS)
}

// ─── ENSURE IAM ROLES EXIST ──────────────────────────────────────────────────────

func ensureClusterServiceRole(ctx context.Context, iamClient *iam.Client, roleARN string) (string, error) {
	roleName, err := extractRoleName(roleARN)
	if err != nil {
		return "", fmt.Errorf("invalid ClusterRoleARN %q: %w", roleARN, err)
	}
	out, err := iamClient.GetRole(ctx, &iam.GetRoleInput{RoleName: aws.String(roleName)})
	if err == nil {
		return aws.ToString(out.Role.Arn), nil
	}
	var noEnt *iamtypes.NoSuchEntityException
	if !errors.As(err, &noEnt) {
		return "", fmt.Errorf("error fetching IAM role %q: %w", roleName, err)
	}

	trust := map[string]interface{}{
		"Version": "2012-10-17",
		"Statement": []map[string]interface{}{
			{"Effect": "Allow", "Principal": map[string]interface{}{"Service": "eks.amazonaws.com"}, "Action": "sts:AssumeRole"},
		},
	}
	trustBytes, _ := json.Marshal(trust)
	created, err := iamClient.CreateRole(ctx, &iam.CreateRoleInput{
		RoleName:                 aws.String(roleName),
		AssumeRolePolicyDocument: aws.String(string(trustBytes)),
		Description:              aws.String("EKS Cluster Service Role"),
		Tags:                     []iamtypes.Tag{{Key: aws.String("Name"), Value: aws.String(roleName)}},
	})
	if err != nil {
		return "", fmt.Errorf("failed to create role %q: %w", roleName, err)
	}
	for _, pol := range []string{
		"arn:aws:iam::aws:policy/AmazonEKSClusterPolicy",
		"arn:aws:iam::aws:policy/AmazonEKSVPCResourceController",
	} {
		_, _ = iamClient.AttachRolePolicy(ctx, &iam.AttachRolePolicyInput{
			RoleName:  aws.String(roleName),
			PolicyArn: aws.String(pol),
		})
	}
	return aws.ToString(created.Role.Arn), nil
}

func ensureNodeInstanceRole(
	ctx context.Context,
	iamClient *iam.Client,
	stsClient *sts.Client,
	roleARN string,
) (string, error) {
	roleName, err := extractRoleName(roleARN)
	if err != nil {
		return "", fmt.Errorf("invalid NodeRoleARN %q: %w", roleARN, err)
	}
	out, err := iamClient.GetRole(ctx, &iam.GetRoleInput{RoleName: aws.String(roleName)})
	if err == nil {
		return aws.ToString(out.Role.Arn), nil
	}
	var noEnt *iamtypes.NoSuchEntityException
	if !errors.As(err, &noEnt) {
		return "", fmt.Errorf("error fetching IAM role %q: %w", roleName, err)
	}

	trust := map[string]interface{}{
		"Version": "2012-10-17",
		"Statement": []map[string]interface{}{
			{"Effect": "Allow", "Principal": map[string]interface{}{"Service": "ec2.amazonaws.com"}, "Action": "sts:AssumeRole"},
		},
	}
	trustBytes, _ := json.Marshal(trust)
	created, err := iamClient.CreateRole(ctx, &iam.CreateRoleInput{
		RoleName:                 aws.String(roleName),
		AssumeRolePolicyDocument: aws.String(string(trustBytes)),
		Description:              aws.String("EKS Worker Node Instance Role"),
		Tags:                     []iamtypes.Tag{{Key: aws.String("Name"), Value: aws.String(roleName)}},
	})
	if err != nil {
		return "", fmt.Errorf("failed to create NodeInstanceRole %q: %w", roleName, err)
	}
	nodeRoleARN := aws.ToString(created.Role.Arn)

	for _, pol := range []string{
		"arn:aws:iam::aws:policy/AmazonEKSWorkerNodePolicy",
		"arn:aws:iam::aws:policy/AmazonEKS_CNI_Policy",
		"arn:aws:iam::aws:policy/AmazonEC2ContainerRegistryReadOnly",
	} {
		_, _ = iamClient.AttachRolePolicy(ctx, &iam.AttachRolePolicyInput{
			RoleName:  aws.String(roleName),
			PolicyArn: aws.String(pol),
		})
	}

	if err = ensureCallerCanPassRole(ctx, iamClient, stsClient, nodeRoleARN); err != nil {
		return "", err
	}
	return nodeRoleARN, nil
}

func ensureCallerCanPassRole(
	ctx context.Context,
	iamClient *iam.Client,
	stsClient *sts.Client,
	roleARN string,
) error {
	caller, err := stsClient.GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	if err != nil {
		return fmt.Errorf("cannot get caller identity: %w", err)
	}
	callerARN := aws.ToString(caller.Arn)

	var callerName string
	var isUser bool
	if strings.Contains(callerARN, ":user/") {
		parts := strings.Split(callerARN, "/")
		callerName = parts[len(parts)-1]
		isUser = true
	} else if strings.Contains(callerARN, ":role/") {
		parts := strings.Split(callerARN, "/")
		callerName = parts[len(parts)-1]
		isUser = false
	} else {
		return fmt.Errorf("caller ARN %q is neither user nor role", callerARN)
	}

	sim, err := iamClient.SimulatePrincipalPolicy(ctx, &iam.SimulatePrincipalPolicyInput{
		PolicySourceArn: aws.String(callerARN),
		ActionNames:     []string{"iam:PassRole"},
		ResourceArns:    []string{roleARN},
	})
	if err != nil {
		return fmt.Errorf("simulate policy error: %w", err)
	}
	for _, r := range sim.EvaluationResults {
		if r.EvalDecision != iamtypes.PolicyEvaluationDecisionTypeAllowed {
			inline := map[string]interface{}{
				"Version": "2012-10-17",
				"Statement": []map[string]interface{}{
					{"Effect": "Allow", "Action": "iam:PassRole", "Resource": roleARN},
				},
			}
			inlineBytes, _ := json.Marshal(inline)
			if isUser {
				_, _ = iamClient.PutUserPolicy(ctx, &iam.PutUserPolicyInput{
					UserName:       aws.String(callerName),
					PolicyName:     aws.String("AllowPassRoleToNodeRole"),
					PolicyDocument: aws.String(string(inlineBytes)),
				})
			} else {
				_, _ = iamClient.PutRolePolicy(ctx, &iam.PutRolePolicyInput{
					RoleName:       aws.String(callerName),
					PolicyName:     aws.String("AllowPassRoleToNodeRole"),
					PolicyDocument: aws.String(string(inlineBytes)),
				})
			}
			break
		}
	}
	return nil
}

func extractRoleName(roleARN string) (string, error) {
	parts := strings.Split(roleARN, "/")
	if len(parts) < 2 {
		return "", fmt.Errorf("cannot parse role ARN: %q", roleARN)
	}
	return parts[len(parts)-1], nil
}

// ─── VPC, SUBNET, INTERNET GATEWAY, ROUTE TABLE, SECURITY GROUPS ──────────────

func createAwsVPC(ctx context.Context, client *ec2.Client, name, cidr string) (string, error) {
	vpcs, err := client.DescribeVpcs(ctx, &ec2.DescribeVpcsInput{
		Filters: []ec2types.Filter{{Name: aws.String("tag:Name"), Values: []string{name}}},
	})
	if err != nil {
		return "", err
	}
	if len(vpcs.Vpcs) > 0 {
		return aws.ToString(vpcs.Vpcs[0].VpcId), nil
	}
	out, err := client.CreateVpc(ctx, &ec2.CreateVpcInput{
		CidrBlock: aws.String(cidr),
		TagSpecifications: []ec2types.TagSpecification{{
			ResourceType: ec2types.ResourceTypeVpc,
			Tags:         []ec2types.Tag{{Key: aws.String("Name"), Value: aws.String(name)}},
		}},
	})
	if err != nil {
		return "", err
	}
	vpcID := aws.ToString(out.Vpc.VpcId)
	// Enable DNS support & hostnames
	client.ModifyVpcAttribute(ctx, &ec2.ModifyVpcAttributeInput{
		VpcId:            out.Vpc.VpcId,
		EnableDnsSupport: &ec2types.AttributeBooleanValue{Value: aws.Bool(true)},
	})
	client.ModifyVpcAttribute(ctx, &ec2.ModifyVpcAttributeInput{
		VpcId:              out.Vpc.VpcId,
		EnableDnsHostnames: &ec2types.AttributeBooleanValue{Value: aws.Bool(true)},
	})
	return vpcID, nil
}

func createAwsSubnets(
	ctx context.Context,
	client *ec2.Client,
	vpcID string,
	cidrBlocks []string,
	region string,
) ([]string, error) {
	if len(cidrBlocks) < 3 {
		return nil, fmt.Errorf("need at least 3 CIDR blocks, got %d", len(cidrBlocks))
	}
	// 1) List first 3 AZs
	azResp, err := client.DescribeAvailabilityZones(ctx, &ec2.DescribeAvailabilityZonesInput{
		Filters: []ec2types.Filter{
			{Name: aws.String("region-name"), Values: []string{region}},
			{Name: aws.String("state"), Values: []string{"available"}},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("list AZs in %s: %w", region, err)
	}
	if len(azResp.AvailabilityZones) < 3 {
		return nil, fmt.Errorf("fewer than 3 AZs in %q", region)
	}
	azNames := []string{
		*azResp.AvailabilityZones[0].ZoneName,
		*azResp.AvailabilityZones[1].ZoneName,
		*azResp.AvailabilityZones[2].ZoneName,
	}

	var subnetIDs []string
	for i, az := range azNames {
		desiredTag := fmt.Sprintf("%s-%s", subnetNamePrefix, az)
		desc, err := client.DescribeSubnets(ctx, &ec2.DescribeSubnetsInput{
			Filters: []ec2types.Filter{
				{Name: aws.String("vpc-id"), Values: []string{vpcID}},
				{Name: aws.String("tag:Name"), Values: []string{desiredTag}},
				{Name: aws.String("availability-zone"), Values: []string{az}},
			},
		})
		if err != nil {
			return nil, fmt.Errorf("describe subnets %q: %w", desiredTag, err)
		}
		var thisSubnetID string
		if len(desc.Subnets) > 0 {
			thisSubnetID = aws.ToString(desc.Subnets[0].SubnetId)
		} else {
			out, err := client.CreateSubnet(ctx, &ec2.CreateSubnetInput{
				VpcId:            aws.String(vpcID),
				CidrBlock:        aws.String(cidrBlocks[i]),
				AvailabilityZone: aws.String(az),
				TagSpecifications: []ec2types.TagSpecification{{
					ResourceType: ec2types.ResourceTypeSubnet,
					Tags:         []ec2types.Tag{{Key: aws.String("Name"), Value: aws.String(desiredTag)}},
				}},
			})
			if err != nil {
				return nil, fmt.Errorf("create subnet %q: %w", desiredTag, err)
			}
			thisSubnetID = aws.ToString(out.Subnet.SubnetId)
		}
		// Enable Auto-assign Public IP
		client.ModifySubnetAttribute(ctx, &ec2.ModifySubnetAttributeInput{
			SubnetId:            aws.String(thisSubnetID),
			MapPublicIpOnLaunch: &ec2types.AttributeBooleanValue{Value: aws.Bool(true)},
		})
		subnetIDs = append(subnetIDs, thisSubnetID)
	}
	return subnetIDs, nil
}

func ensureInternetGateway(ctx context.Context, client *ec2.Client, vpcID string) (string, error) {
	igws, err := client.DescribeInternetGateways(ctx, &ec2.DescribeInternetGatewaysInput{
		Filters: []ec2types.Filter{{Name: aws.String("attachment.vpc-id"), Values: []string{vpcID}}},
	})
	if err != nil {
		return "", err
	}
	if len(igws.InternetGateways) > 0 {
		return aws.ToString(igws.InternetGateways[0].InternetGatewayId), nil
	}
	createOut, err := client.CreateInternetGateway(ctx, &ec2.CreateInternetGatewayInput{
		TagSpecifications: []ec2types.TagSpecification{{
			ResourceType: ec2types.ResourceTypeInternetGateway,
			Tags:         []ec2types.Tag{{Key: aws.String("Name"), Value: aws.String(vpcID + "-igw")}},
		}},
	})
	if err != nil {
		return "", err
	}
	igwID := aws.ToString(createOut.InternetGateway.InternetGatewayId)
	_, err = client.AttachInternetGateway(ctx, &ec2.AttachInternetGatewayInput{
		InternetGatewayId: aws.String(igwID),
		VpcId:             aws.String(vpcID),
	})
	return igwID, err
}

func ensurePublicRouteTable(ctx context.Context, client *ec2.Client, vpcID, igwID string) (string, error) {
	rtResp, err := client.DescribeRouteTables(ctx, &ec2.DescribeRouteTablesInput{
		Filters: []ec2types.Filter{
			{Name: aws.String("vpc-id"), Values: []string{vpcID}},
			{Name: aws.String("tag:Name"), Values: []string{vpcID + "-public-rt"}},
		},
	})
	if err != nil {
		return "", err
	}
	if len(rtResp.RouteTables) > 0 {
		return aws.ToString(rtResp.RouteTables[0].RouteTableId), nil
	}
	createOut, err := client.CreateRouteTable(ctx, &ec2.CreateRouteTableInput{
		VpcId: aws.String(vpcID),
		TagSpecifications: []ec2types.TagSpecification{{
			ResourceType: ec2types.ResourceTypeRouteTable,
			Tags:         []ec2types.Tag{{Key: aws.String("Name"), Value: aws.String(vpcID + "-public-rt")}}},
		},
	})
	if err != nil {
		return "", err
	}
	rtID := aws.ToString(createOut.RouteTable.RouteTableId)
	_, err = client.CreateRoute(ctx, &ec2.CreateRouteInput{
		RouteTableId:         aws.String(rtID),
		DestinationCidrBlock: aws.String("0.0.0.0/0"),
		GatewayId:            aws.String(igwID),
	})
	return rtID, err
}

func createEKSClusterSecurityGroups(
	ctx context.Context,
	client *ec2.Client,
	vpcID, clusterName string,
) (controlSG string, workerSG string, _ error) {
	controlName := clusterName + "-control-sg"
	workerName := clusterName + "-worker-sg"

	controlID, err := findOrCreateSG(ctx, client, vpcID, controlName, "EKS control-plane SG")
	if err != nil {
		return "", "", err
	}
	workerID, err := findOrCreateSG(ctx, client, vpcID, workerName, "EKS worker-node SG")
	if err != nil {
		return "", "", err
	}

	// Collect all subnet CIDRs
	subnetsOut, err := client.DescribeSubnets(ctx, &ec2.DescribeSubnetsInput{
		Filters: []ec2types.Filter{{Name: aws.String("vpc-id"), Values: []string{vpcID}}},
	})
	if err != nil {
		return "", "", err
	}
	var allSubnetCIDRs []string
	for _, sn := range subnetsOut.Subnets {
		allSubnetCIDRs = append(allSubnetCIDRs, aws.ToString(sn.CidrBlock))
	}

	// controlSG: allow inbound TCP:443 from workerSG
	_ = revokeIfExists(ctx, client, controlID, workerID, 443, 443, "tcp")
	_, err = client.AuthorizeSecurityGroupIngress(ctx, &ec2.AuthorizeSecurityGroupIngressInput{
		GroupId: aws.String(controlID),
		IpPermissions: []ec2types.IpPermission{{
			FromPort:         aws.Int32(443),
			ToPort:           aws.Int32(443),
			IpProtocol:       aws.String("tcp"),
			UserIdGroupPairs: []ec2types.UserIdGroupPair{{GroupId: aws.String(workerID)}},
		}},
	})
	if err != nil {
		return "", "", err
	}

	// workerSG: ingress TCP:9443 from all subnets
	_ = revokeCIDRIngress(ctx, client, workerID, 9443, 9443, "tcp", allSubnetCIDRs)
	_, err = client.AuthorizeSecurityGroupIngress(ctx, &ec2.AuthorizeSecurityGroupIngressInput{
		GroupId: aws.String(workerID),
		IpPermissions: []ec2types.IpPermission{{
			FromPort:   aws.Int32(9443),
			ToPort:     aws.Int32(9443),
			IpProtocol: aws.String("tcp"),
			IpRanges:   toIPRanges(allSubnetCIDRs),
		}},
	})
	if err != nil {
		return "", "", err
	}

	// workerSG: internal traffic (TCP 0–65535, UDP 0–65535, ICMP all) from all subnets
	_ = revokeCIDRIngress(ctx, client, workerID, 0, 65535, "tcp", allSubnetCIDRs)
	_ = revokeCIDRIngress(ctx, client, workerID, 0, 65535, "udp", allSubnetCIDRs)
	_ = revokeCIDRIngress(ctx, client, workerID, -1, -1, "icmp", allSubnetCIDRs)
	_, err = client.AuthorizeSecurityGroupIngress(ctx, &ec2.AuthorizeSecurityGroupIngressInput{
		GroupId: aws.String(workerID),
		IpPermissions: []ec2types.IpPermission{
			{FromPort: aws.Int32(0), ToPort: aws.Int32(65535), IpProtocol: aws.String("tcp"), IpRanges: toIPRanges(allSubnetCIDRs)},
			{FromPort: aws.Int32(0), ToPort: aws.Int32(65535), IpProtocol: aws.String("udp"), IpRanges: toIPRanges(allSubnetCIDRs)},
			{FromPort: aws.Int32(-1), ToPort: aws.Int32(-1), IpProtocol: aws.String("icmp"), IpRanges: toIPRanges(allSubnetCIDRs)},
		},
	})
	if err != nil {
		return "", "", err
	}

	// workerSG: allow controlSG on TCP:443 & TCP:1025–65535
	_ = revokeIfExists(ctx, client, workerID, controlID, 443, 443, "tcp")
	_ = revokeIfExists(ctx, client, workerID, controlID, 1025, 65535, "tcp")
	_, err = client.AuthorizeSecurityGroupIngress(ctx, &ec2.AuthorizeSecurityGroupIngressInput{
		GroupId: aws.String(workerID),
		IpPermissions: []ec2types.IpPermission{
			{FromPort: aws.Int32(443), ToPort: aws.Int32(443), IpProtocol: aws.String("tcp"), UserIdGroupPairs: []ec2types.UserIdGroupPair{{GroupId: aws.String(controlID)}}},
			{FromPort: aws.Int32(1025), ToPort: aws.Int32(65535), IpProtocol: aws.String("tcp"), UserIdGroupPairs: []ec2types.UserIdGroupPair{{GroupId: aws.String(controlID)}}},
		},
	})
	if err != nil {
		return "", "", err
	}

	// workerSG: egress all
	_ = revokeAllEgress(ctx, client, workerID)
	_, err = client.AuthorizeSecurityGroupEgress(ctx, &ec2.AuthorizeSecurityGroupEgressInput{
		GroupId: aws.String(workerID),
		IpPermissions: []ec2types.IpPermission{{
			IpProtocol: aws.String("-1"),
			IpRanges:   []ec2types.IpRange{{CidrIp: aws.String("0.0.0.0/0")}},
		}},
	})
	if err != nil {
		return "", "", err
	}

	return controlID, workerID, nil
}

func findOrCreateSG(ctx context.Context, client *ec2.Client, vpcID, name, desc string) (string, error) {
	out, err := client.DescribeSecurityGroups(ctx, &ec2.DescribeSecurityGroupsInput{
		Filters: []ec2types.Filter{
			{Name: aws.String("vpc-id"), Values: []string{vpcID}},
			{Name: aws.String("tag:Name"), Values: []string{name}},
		},
	})
	if err != nil {
		return "", err
	}
	if len(out.SecurityGroups) > 0 {
		return aws.ToString(out.SecurityGroups[0].GroupId), nil
	}
	created, err := client.CreateSecurityGroup(ctx, &ec2.CreateSecurityGroupInput{
		GroupName:   aws.String(name),
		Description: aws.String(desc),
		VpcId:       aws.String(vpcID),
		TagSpecifications: []ec2types.TagSpecification{{
			ResourceType: ec2types.ResourceTypeSecurityGroup,
			Tags:         []ec2types.Tag{{Key: aws.String("Name"), Value: aws.String(name)}},
		}},
	})
	if err != nil {
		return "", err
	}
	return aws.ToString(created.GroupId), nil
}

func revokeIfExists(ctx context.Context, client *ec2.Client, toSG, fromSG string, fromPort, toPort int32, ipProto string) error {
	_, _ = client.RevokeSecurityGroupIngress(ctx, &ec2.RevokeSecurityGroupIngressInput{
		GroupId: aws.String(toSG),
		IpPermissions: []ec2types.IpPermission{{
			FromPort:   aws.Int32(fromPort),
			ToPort:     aws.Int32(toPort),
			IpProtocol: aws.String(ipProto),
			UserIdGroupPairs: []ec2types.UserIdGroupPair{
				{GroupId: aws.String(fromSG)},
			},
		}},
	})
	return nil
}

func revokeCIDRIngress(ctx context.Context, client *ec2.Client, toSG string, fromPort, toPort int32, ipProto string, cidrs []string) error {
	var ipRanges []ec2types.IpRange
	for _, cidr := range cidrs {
		ipRanges = append(ipRanges, ec2types.IpRange{CidrIp: aws.String(cidr)})
	}
	_, _ = client.RevokeSecurityGroupIngress(ctx, &ec2.RevokeSecurityGroupIngressInput{
		GroupId: aws.String(toSG),
		IpPermissions: []ec2types.IpPermission{{
			FromPort:   aws.Int32(fromPort),
			ToPort:     aws.Int32(toPort),
			IpProtocol: aws.String(ipProto),
			IpRanges:   ipRanges,
		}},
	})
	return nil
}

func revokeAllEgress(ctx context.Context, client *ec2.Client, sgID string) error {
	_, _ = client.RevokeSecurityGroupEgress(ctx, &ec2.RevokeSecurityGroupEgressInput{
		GroupId: aws.String(sgID),
		IpPermissions: []ec2types.IpPermission{{
			IpProtocol: aws.String("-1"),
			IpRanges:   []ec2types.IpRange{{CidrIp: aws.String("0.0.0.0/0")}},
		}},
	})
	return nil
}

func toIPRanges(cidrs []string) []ec2types.IpRange {
	var ranges []ec2types.IpRange
	for _, c := range cidrs {
		ranges = append(ranges, ec2types.IpRange{CidrIp: aws.String(c)})
	}
	return ranges
}

// ─── EKS CLUSTER + NODEGROUP CREATION ──────────────────────────────────────────

func createEKSCluster(
	ctx context.Context,
	eksClient *eks.Client,
	cfg AwsClusterSetupConfig,
	subnetIDs []string,
	controlSG string,
) error {
	_, err := eksClient.DescribeCluster(ctx, &eks.DescribeClusterInput{Name: aws.String(cfg.ClusterName)})
	if err == nil {
		return nil // already exists
	}
	var notFound *ekstypes.ResourceNotFoundException
	if !errors.As(err, &notFound) {
		return err
	}
	_, err = eksClient.CreateCluster(ctx, &eks.CreateClusterInput{
		Name:    aws.String(cfg.ClusterName),
		RoleArn: aws.String(ClusterRoleARN),
		ResourcesVpcConfig: &ekstypes.VpcConfigRequest{
			SubnetIds:             subnetIDs,
			SecurityGroupIds:      []string{controlSG},
			EndpointPublicAccess:  aws.Bool(true),
			EndpointPrivateAccess: aws.Bool(true),
		},
		KubernetesNetworkConfig: &ekstypes.KubernetesNetworkConfigRequest{
			ServiceIpv4Cidr: aws.String(cfg.ServiceCIDR),
		},
	})
	if err != nil {
		return err
	}
	// Wait for ACTIVE
	for {
		out, err := eksClient.DescribeCluster(ctx, &eks.DescribeClusterInput{Name: aws.String(cfg.ClusterName)})
		if err != nil {
			return err
		}
		if out.Cluster.Status == ekstypes.ClusterStatusActive {
			break
		}
		time.Sleep(15 * time.Second)
	}
	return nil
}

func createManagedNodeGroup(
	ctx context.Context,
	eksClient *eks.Client,
	cfg AwsClusterSetupConfig,
	subnetIDs []string,
	workerSG string,
) error {
	ngName := cfg.ClusterName + "-ng"
	_, err := eksClient.DescribeNodegroup(ctx, &eks.DescribeNodegroupInput{
		ClusterName:   aws.String(cfg.ClusterName),
		NodegroupName: aws.String(ngName),
	})
	if err == nil {
		return nil // exists
	}
	var notFound *ekstypes.ResourceNotFoundException
	if !errors.As(err, &notFound) {
		return err
	}
	_, err = eksClient.CreateNodegroup(ctx, &eks.CreateNodegroupInput{
		ClusterName:   aws.String(cfg.ClusterName),
		NodegroupName: aws.String(ngName),
		ScalingConfig: &ekstypes.NodegroupScalingConfig{
			DesiredSize: aws.Int32(cfg.DesiredSize),
			MinSize:     aws.Int32(cfg.MinSize),
			MaxSize:     aws.Int32(cfg.MaxSize),
		},
		Subnets:       subnetIDs,
		InstanceTypes: cfg.InstanceTypes,
		NodeRole:      aws.String(NodeRoleARN),
		DiskSize:      aws.Int32(20),
	})
	if err != nil {
		return err
	}
	for {
		out, err := eksClient.DescribeNodegroup(ctx, &eks.DescribeNodegroupInput{
			ClusterName:   aws.String(cfg.ClusterName),
			NodegroupName: aws.String(ngName),
		})
		if err != nil {
			return err
		}
		if out.Nodegroup.Status == ekstypes.NodegroupStatusActive {
			break
		}
		time.Sleep(15 * time.Second)
	}
	return nil
}

// ─── ELASTIC IP ALLOCATION ─────────────────────────────────────────────────────

// ensureElasticIPs reuses unassociated EIPs by tag or allocates new ones
func ensureElasticIPs(ctx context.Context, client *ec2.Client, count int) ([]string, error) {
	var result []string

	// 1) Find existing unassociated EIPs tagged "cockroachdb-eip-..."
	addrsOut, err := client.DescribeAddresses(ctx, &ec2.DescribeAddressesInput{})
	if err != nil {
		return nil, err
	}
	for _, addr := range addrsOut.Addresses {
		if addr.AssociationId == nil {
			for _, tag := range addr.Tags {
				if tag.Key != nil && *tag.Key == "Name" && strings.HasPrefix(*tag.Value, "self-hosted-testing-eip-") {
					result = append(result, aws.ToString(addr.AllocationId))
					break
				}
			}
		}
		if len(result) >= count {
			return result[:count], nil
		}
	}

	// 2) Allocate remaining count
	toAllocate := count - len(result)
	for i := 0; i < toAllocate; i++ {
		out, err := client.AllocateAddress(ctx, &ec2.AllocateAddressInput{Domain: ec2types.DomainTypeVpc})
		if err != nil {
			return nil, err
		}
		allocID := aws.ToString(out.AllocationId)
		// Tag it for future reuse
		_, _ = client.CreateTags(ctx, &ec2.CreateTagsInput{
			Resources: []string{allocID},
			Tags:      []ec2types.Tag{{Key: aws.String("Name"), Value: aws.String("self-hosted-testing-eip-" + allocID)}},
		})
		result = append(result, allocID)
	}
	return result, nil
}
