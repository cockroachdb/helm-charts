package infra

import (
	"fmt"
	"net"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/cockroachdb/helm-charts/tests/e2e/coredns"
	"github.com/gruntwork-io/terratest/modules/k8s"
	"github.com/gruntwork-io/terratest/modules/retry"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client"
	k8sconfig "sigs.k8s.io/controller-runtime/pkg/client/config"
)

// Provider types
const (
	ProviderK3D   = "k3d"
	ProviderKind  = "kind"
	ProviderGCP   = "gcp"
	ProviderAzure = "azure"
	ProviderAWS   = "aws"
)

// Common constants
const (
	DefaultRetries        = 30
	DefaultRetryInterval  = 10 * time.Second
	CoreDNSDeploymentName = "coredns"
	CoreDNSServiceName    = "crl-core-dns"
	CoreDNSNamespace      = "kube-system"
	CoreDNSReplicas       = 2
)

// Common network configuration constants for all providers
const (
	// VPC CIDR blocks
	DefaultVPCCIDR = "172.28.0.0/16"

	// Instance types for different cloud providers
	GCPDefaultMachineType  = "e2-standard-4"
	AWSDefaultInstanceType = "m5.large"
	AzureDefaultVMSize     = "Standard_A4_v2"

	// Default node counts
	DefaultNodeCount       = 3
	DefaultNodesPerZone    = 1
	DefaultMinNodeCount    = 3
	DefaultMaxNodeCount    = 4
	DefaultCoreDNSReplicas = 2
)

// RegionCodes maps provider types to their region codes
var RegionCodes = map[string][]string{
	ProviderK3D:   {"us-east1", "us-east2"},
	ProviderKind:  {"us-east1", "us-east2"},
	ProviderGCP:   {"us-central1", "us-east1", "us-west1"},
	ProviderAzure: {"centralus", "eastus"},
	ProviderAWS:   {"us-east-1", "us-east-2"},
}

// LoadBalancerAnnotations contains provider-specific service annotations
var LoadBalancerAnnotations = map[string]map[string]string{
	ProviderGCP: {
		"networking.gke.io/internal-load-balancer-allow-global-access": "true",
		"networking.gke.io/load-balancer-type":                         "Internal",
		"cloud.google.com/load-balancer-type":                          "Internal",
	},
	ProviderAzure: {
		"service.beta.kubernetes.io/azure-load-balancer-internal": "true",
	},
	ProviderAWS: {
		"service.beta.kubernetes.io/aws-load-balancer-type":     "nlb",
		"service.beta.kubernetes.io/aws-load-balancer-internal": "true",
	},
	ProviderK3D:  {},
	ProviderKind: {},
}

// NetworkConfigs defines standard network configurations for each provider and region
var NetworkConfigs = map[string]map[string]interface{}{
	ProviderGCP: {
		"us-central1": map[string]string{
			"ClusterCIDR": "172.28.0.0/20",
			"ServiceCIDR": "172.28.17.0/24",
			"SubnetRange": "172.28.16.0/24",
			"StaticIP":    "172.28.16.11",
		},
		"us-east1": map[string]string{
			"ClusterCIDR": "172.28.32.0/20",
			"ServiceCIDR": "172.28.49.0/24",
			"SubnetRange": "172.28.48.0/24",
			"StaticIP":    "172.28.48.11",
		},
		"us-west1": map[string]string{
			"ClusterCIDR": "172.28.64.0/20",
			"ServiceCIDR": "172.28.81.0/24",
			"SubnetRange": "172.28.80.0/24",
			"StaticIP":    "172.28.80.11",
		},
	},
	ProviderAWS: {
		"us-east-1": map[string]interface{}{
			"SubnetRanges": []string{"172.28.12.0/24", "172.28.24.0/24", "172.28.36.0/24"},
			"ServiceCIDR":  "10.200.0.0/16",
		},
		"us-east-2": map[string]interface{}{
			"SubnetRanges": []string{"172.28.48.0/24", "172.28.60.0/24", "172.28.72.0/24"},
			"ServiceCIDR":  "10.201.0.0/16",
		},
		"us-west-1": map[string]interface{}{
			"SubnetRanges": []string{"172.28.84.0/24", "172.28.96.0/24", "172.28.108.0/24"},
			"ServiceCIDR":  "10.202.0.0/16",
		},
	},
	ProviderAzure: {
		"centralus": map[string]interface{}{
			"VNetPrefix":   "172.28.0.0/16",
			"SubnetPrefix": "172.28.0.0/24",
			"PodCIDR":      "10.244.0.0/16",
			"ServiceCIDR":  "10.96.0.0/16",
		},
		"eastus": map[string]interface{}{
			"VNetPrefix":   "172.29.0.0/16",
			"SubnetPrefix": "172.29.0.0/24",
			"PodCIDR":      "10.100.0.0/16",
			"ServiceCIDR":  "10.101.0.0/16",
		},
		"westus": map[string]interface{}{
			"VNetPrefix":   "172.30.0.0/16",
			"SubnetPrefix": "172.30.0.0/24",
			"PodCIDR":      "10.200.0.0/16",
			"ServiceCIDR":  "10.201.0.0/16",
		},
	},
}

// ToPtr is a generic helper function that returns a pointer to the given value.
// This is useful for APIs that require pointers to primitive types.
func ToPtr[T any](v T) *T {
	return &v
}

// Pointers converts a slice of values to a slice of pointers to those values.
// This is useful for APIs that require slices of pointers.
func Pointers[T any](slice []T) []*T {
	out := make([]*T, len(slice))
	for i := range slice {
		out[i] = &slice[i]
	}
	return out
}

// MustNewClientForContext creates a new Kubernetes client for a specific context
// and panics if there is an error
func MustNewClientForContext(t *testing.T, ctxName string) client.Client {
	cfg, err := k8sconfig.GetConfigWithContext(ctxName)
	require.NoError(t, err)
	c, err := client.New(cfg, client.Options{})
	require.NoError(t, err)
	return c
}

// IsResourceConflict checks if error is a 409 conflict (resource already exists)
func IsResourceConflict(err error) bool {
	if err == nil {
		return false
	}
	errStr := strings.ToLower(err.Error())
	return strings.Contains(errStr, "409") ||
		strings.Contains(errStr, "alreadyexists") ||
		strings.Contains(errStr, "already exists") ||
		strings.Contains(errStr, "conflict")
}

// IsResourceNotFound checks if error indicates resource not found
func IsResourceNotFound(err error) bool {
	if err == nil {
		return false
	}
	errStr := strings.ToLower(err.Error())
	return strings.Contains(errStr, "404") ||
		strings.Contains(errStr, "notfound") ||
		strings.Contains(errStr, "not found")
}

// GetRegionCodes returns the region codes for a provider
func GetRegionCodes(providerType string) []string {
	if codes, ok := RegionCodes[providerType]; ok {
		return codes
	}
	return []string{}
}

// GetLoadBalancerAnnotations returns load balancer annotations for a provider
func GetLoadBalancerAnnotations(providerType string) map[string]string {
	if annotations, ok := LoadBalancerAnnotations[providerType]; ok {
		return annotations
	}
	return map[string]string{}
}

// DeployCoreDNS deploys CoreDNS to a cluster
func DeployCoreDNS(t *testing.T, clusterName, kubeConfigPath string, staticIP *string, provider string, customDomain string, options map[string]coredns.CoreDNSClusterOption) error {
	kubectlOpts := k8s.NewKubectlOptions(clusterName, kubeConfigPath, CoreDNSNamespace)

	// 1. Deploy ConfigMap
	cm := coredns.CoreDNSConfigMap(customDomain, options)
	cmYAML := coredns.ToYAML(t, cm)
	if err := k8s.KubectlApplyFromStringE(t, kubectlOpts, cmYAML); err != nil {
		return fmt.Errorf("failed to apply CoreDNS ConfigMap: %w", err)
	}

	// 2. Deploy RBAC
	sa := coredns.CoreDNSServiceAccount()
	if err := k8s.KubectlApplyFromStringE(t, kubectlOpts, coredns.ToYAML(t, sa)); err != nil {
		return fmt.Errorf("failed to apply ServiceAccount: %w", err)
	}

	cr := coredns.CoreDNSClusterRole()
	if err := k8s.KubectlApplyFromStringE(t, kubectlOpts, coredns.ToYAML(t, cr)); err != nil {
		return fmt.Errorf("failed to apply ClusterRole: %w", err)
	}

	crb := coredns.CoreDNSClusterRoleBinding()
	if err := k8s.KubectlApplyFromStringE(t, kubectlOpts, coredns.ToYAML(t, crb)); err != nil {
		return fmt.Errorf("failed to apply ClusterRoleBinding: %w", err)
	}

	// 3. Deploy Deployment
	deployment := coredns.CoreDNSDeployment(CoreDNSReplicas)
	depYAML := coredns.ToYAML(t, deployment)
	if err := k8s.KubectlApplyFromStringE(t, kubectlOpts, depYAML); err != nil {
		return fmt.Errorf("failed to apply CoreDNS Deployment: %w", err)
	}

	// 4. Wait for deployment to be ready
	_, err := retry.DoWithRetryE(t, "wait for coredns deployment", DefaultRetries, DefaultRetryInterval,
		func() (string, error) {
			return k8s.RunKubectlAndGetOutputE(t, kubectlOpts, "wait", "--for=condition=Available", "deployment/coredns")
		})
	if err != nil {
		return fmt.Errorf("CoreDNS deployment failed to become ready: %w", err)
	}

	// 5. Deploy Service with provider-specific annotations
	annotations := GetLoadBalancerAnnotations(provider)
	service := coredns.CoreDNSService(staticIP, annotations)
	svcYAML := coredns.ToYAML(t, service)
	if err := k8s.KubectlApplyFromStringE(t, kubectlOpts, svcYAML); err != nil {
		return fmt.Errorf("failed to apply CoreDNS Service: %w", err)
	}

	// 6. Restart deployment to pick up new config
	if err := k8s.RunKubectlE(t, kubectlOpts, "rollout", "restart", "deployment", CoreDNSDeploymentName); err != nil {
		return fmt.Errorf("failed to restart CoreDNS deployment: %w", err)
	}

	return nil
}

// WaitForCoreDNSServiceIPs waits for and returns the CoreDNS service IPs
func WaitForCoreDNSServiceIPs(t *testing.T, kubectlOpts *k8s.KubectlOptions) ([]string, error) {
	var ips []string

	_, err := retry.DoWithRetryE(t, "waiting for CoreDNS service IPs", DefaultRetries, DefaultRetryInterval,
		func() (string, error) {
			svc, err := k8s.GetServiceE(t, kubectlOpts, CoreDNSServiceName)
			if err != nil {
				return "", err
			}

			if len(svc.Status.LoadBalancer.Ingress) == 0 {
				return "", fmt.Errorf("waiting for load balancer ingress")
			}

			// Reset IPs on each retry to avoid duplicates
			ips = []string{}

			for _, ingress := range svc.Status.LoadBalancer.Ingress {
				if ingress.IP != "" {
					time.Sleep(5 * time.Second)
					ips = append(ips, ingress.IP)
				} else if ingress.Hostname != "" {
					// If hostname is provided instead of IP, resolve it
					resolvedIPs, err := net.LookupHost(ingress.Hostname)
					if err != nil {
						return "", fmt.Errorf("failed to resolve hostname %s: %v", ingress.Hostname, err)
					}
					ips = append(ips, resolvedIPs...)
				}
			}

			if len(ips) == 0 {
				return "", fmt.Errorf("no IPs found for CoreDNS service")
			}

			return "", nil
		})

	return ips, err
}

// UpdateKubeconfigGCP updates kubeconfig for GCP clusters
func UpdateKubeconfigGCP(t *testing.T, projectID, region, clusterName, alias string) error {
	// Step 1: Get credentials
	getCredsCmd := exec.Command("gcloud", "container", "clusters", "get-credentials",
		clusterName, "--region", region, "--project", projectID)
	output, err := getCredsCmd.CombinedOutput()
	if err != nil {
		t.Logf("gcloud get-credentials command failed. Output:\n%s\n", string(output))
		return fmt.Errorf("failed to get GCP credentials for cluster %s: %w", clusterName, err)
	}

	// Step 2: Rename context
	longContextName := fmt.Sprintf("gke_%s_%s_%s", projectID, region, clusterName)
	renameCmd := exec.Command("kubectl", "config", "rename-context", longContextName, alias)
	output, err = renameCmd.CombinedOutput()
	if err != nil {
		t.Logf("kubectl rename-context command failed. Output:\n%s\n", string(output))
		return fmt.Errorf("failed to rename context for cluster %s: %w", clusterName, err)
	}

	return nil
}

// UpdateKubeconfigAWS updates kubeconfig for AWS EKS clusters
func UpdateKubeconfigAWS(t *testing.T, region, clusterName string) error {
	cmd := exec.Command("aws", "eks", "update-kubeconfig",
		"--region", region,
		"--name", clusterName)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Logf("aws eks update-kubeconfig command failed. Output:\n%s\n", string(output))
		return fmt.Errorf("failed to update kubeconfig for EKS cluster %s: %w", clusterName, err)
	}
	return nil
}

// UpdateKubeconfigAzure updates kubeconfig for Azure AKS clusters
func UpdateKubeconfigAzure(t *testing.T, resourceGroup, clusterName string) error {
	cmd := exec.Command("az", "aks", "get-credentials",
		"--resource-group", resourceGroup,
		"--name", clusterName,
		"--admin",
		"--overwrite-existing")
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Logf("az aks get-credentials command failed. Output:\n%s\n", string(output))
		return fmt.Errorf("failed to get Azure credentials for cluster %s: %w", clusterName, err)
	}
	return nil
}
