package infra

import (
	"fmt"
	"net"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/gruntwork-io/terratest/modules/k8s"
	"github.com/gruntwork-io/terratest/modules/retry"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"

	"github.com/cockroachdb/helm-charts/tests/e2e/coredns"
	"github.com/cockroachdb/helm-charts/tests/e2e/operator"
)

// Provider types.
const (
	ProviderK3D = "k3d"
	ProviderGCP = "gcp"
)

// Common constants.
const (
	defaultRetries        = 30
	defaultRetryInterval  = 10 * time.Second
	coreDNSDeploymentName = "coredns"
	coreDNSServiceName    = "crl-core-dns"
	coreDNSNamespace      = "kube-system"
	coreDNSReplicas       = 2
)

// Common network configuration constants for all providers.
const (
	// VPC CIDR blocks
	DefaultVPCCIDR = "172.28.0.0/16"

	// Instance types for different cloud providers.
	gcpDefaultMachineType = "e2-standard-4"

	// Default node counts
	defaultNodesPerZone = 1
)

// RegionCodes maps provider types to their region codes
var RegionCodes = map[string][]string{
	ProviderK3D: {"us-east1", "us-east2"},
	ProviderGCP: {"us-central1", "us-east1"},
}

// LoadBalancerAnnotations contains provider-specific service annotations.
var LoadBalancerAnnotations = map[string]map[string]string{
	ProviderGCP: {
		"networking.gke.io/internal-load-balancer-allow-global-access": "true",
		"networking.gke.io/load-balancer-type":                         "Internal",
		"cloud.google.com/load-balancer-type":                          "Internal",
	},
	ProviderK3D: {},
}

// NetworkConfigs defines standard network configurations for each provider and region.
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

// GetRegionCodes returns the region codes for a provider.
func GetRegionCodes(providerType string) []string {
	if codes, ok := RegionCodes[providerType]; ok {
		return codes
	}
	return []string{}
}

// GetLoadBalancerAnnotations returns load balancer annotations for a provider.
func GetLoadBalancerAnnotations(providerType string) map[string]string {
	if annotations, ok := LoadBalancerAnnotations[providerType]; ok {
		return annotations
	}
	return map[string]string{}
}

// DeployCoreDNS deploys CoreDNS to a cluster.
func DeployCoreDNS(t *testing.T, clusterName, kubeConfigPath string, staticIP *string, provider string, customDomain string, options map[string]coredns.CoreDNSClusterOption) error {
	kubectlOpts := k8s.NewKubectlOptions(clusterName, kubeConfigPath, coreDNSNamespace)

	// Deploy CoreDNS resources in order.
	if err := deployCoreDNSResources(t, kubectlOpts, customDomain, options); err != nil {
		return err
	}

	// Deploy and configure service.
	if err := deployCoreDNSService(t, kubectlOpts, staticIP, provider); err != nil {
		return err
	}

	// Scale down existing DNS and restart CoreDNS.
	return finalizeCoreDNSDeployment(t, kubectlOpts)
}

// deployCoreDNSResources deploys the core CoreDNS resources (ConfigMap, RBAC, Deployment)
func deployCoreDNSResources(t *testing.T, kubectlOpts *k8s.KubectlOptions, customDomain string, options map[string]coredns.CoreDNSClusterOption) error {
	// 1. Deploy ConfigMap
	cm := coredns.CoreDNSConfigMap(customDomain, options)
	cmYAML := coredns.ToYAML(t, cm)
	if err := k8s.KubectlApplyFromStringE(t, kubectlOpts, cmYAML); err != nil {
		return fmt.Errorf("failed to apply CoreDNS ConfigMap: %w", err)
	}

	// 2. Deploy RBAC resources
	if err := deployCoreDNSRBAC(t, kubectlOpts); err != nil {
		return err
	}

	// 3. Deploy Deployment
	deployment := coredns.CoreDNSDeployment(coreDNSReplicas)
	depYAML := coredns.ToYAML(t, deployment)
	if err := k8s.KubectlApplyFromStringE(t, kubectlOpts, depYAML); err != nil {
		return fmt.Errorf("failed to apply CoreDNS Deployment: %w", err)
	}

	// 4. Wait for deployment to be ready
	_, err := retry.DoWithRetryE(t, "wait for coredns deployment", defaultRetries, defaultRetryInterval,
		func() (string, error) {
			return k8s.RunKubectlAndGetOutputE(t, kubectlOpts, "wait", "--for=condition=Available", "deployment/coredns")
		})
	if err != nil {
		return fmt.Errorf("CoreDNS deployment failed to become ready: %w", err)
	}

	return nil
}

// deployCoreDNSRBAC deploys the RBAC resources for CoreDNS
func deployCoreDNSRBAC(t *testing.T, kubectlOpts *k8s.KubectlOptions) error {
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

	return nil
}

// deployCoreDNSService creates and applies the CoreDNS service
func deployCoreDNSService(t *testing.T, kubectlOpts *k8s.KubectlOptions, staticIP *string, provider string) error {
	// Get provider-specific annotations.
	annotations := GetLoadBalancerAnnotations(provider)

	// Add static IP annotation for providers that support it
	if staticIP != nil && provider == ProviderGCP {
		annotations["networking.gke.io/load-balancer-ip-addresses"] = *staticIP
	}

	service := coredns.CoreDNSService(staticIP, annotations)
	svcYAML := coredns.ToYAML(t, service)
	if err := k8s.KubectlApplyFromStringE(t, kubectlOpts, svcYAML); err != nil {
		return fmt.Errorf("failed to apply CoreDNS Service: %w", err)
	}

	return nil
}

// finalizeCoreDNSDeployment scales down existing DNS and restarts CoreDNS
func finalizeCoreDNSDeployment(t *testing.T, kubectlOpts *k8s.KubectlOptions) error {
	// Scale down existing kube-dns deployments.
	if err := k8s.RunKubectlE(t, kubectlOpts, "scale", "deployment", "kube-dns-autoscaler", "--replicas=0"); err != nil {
		// Log but don't fail if kube-dns-autoscaler doesn't exist.
		t.Logf("Warning: failed to scale down kube-dns-autoscaler (may not exist): %v", err)
	}
	if err := k8s.RunKubectlE(t, kubectlOpts, "scale", "deployment", "kube-dns", "--replicas=0"); err != nil {
		// Log but don't fail if kube-dns doesn't exist.
		t.Logf("Warning: failed to scale down kube-dns (may not exist): %v", err)
	}

	// Restart deployment to pick up the config.
	if err := k8s.RunKubectlE(t, kubectlOpts, "rollout", "restart", "deployment", coreDNSDeploymentName); err != nil {
		return fmt.Errorf("failed to restart CoreDNS deployment: %w", err)
	}

	return nil
}

// WaitForCoreDNSServiceIPs waits for and returns the CoreDNS service IPs.
func WaitForCoreDNSServiceIPs(t *testing.T, kubectlOpts *k8s.KubectlOptions) ([]string, error) {
	var ips []string

	_, err := retry.DoWithRetryE(t, "waiting for CoreDNS service IPs", defaultRetries, defaultRetryInterval,
		func() (string, error) {
			svc, err := k8s.GetServiceE(t, kubectlOpts, coreDNSServiceName)
			if err != nil {
				return "", err
			}

			if len(svc.Status.LoadBalancer.Ingress) == 0 {
				return "", fmt.Errorf("waiting for load balancer ingress")
			}

			// Extract IPs from service ingress.
			ips, err = extractServiceIPs(svc.Status.LoadBalancer.Ingress)
			if err != nil {
				return "", err
			}

			if len(ips) == 0 {
				return "", fmt.Errorf("no IPs found for CoreDNS service")
			}

			return "", nil
		})

	return ips, err
}

// extractServiceIPs extracts IP addresses from load balancer ingress.
func extractServiceIPs(ingresses []corev1.LoadBalancerIngress) ([]string, error) {
	var ips []string

	for _, ingress := range ingresses {
		if ingress.IP != "" {
			time.Sleep(5 * time.Second)
			ips = append(ips, ingress.IP)
		} else if ingress.Hostname != "" {
			// If the hostname is provided instead of IP, resolve it.
			resolvedIPs, err := net.LookupHost(ingress.Hostname)
			if err != nil {
				return nil, fmt.Errorf("failed to resolve hostname %s: %v", ingress.Hostname, err)
			}
			ips = append(ips, resolvedIPs...)
		}
	}

	return ips, nil
}

// UpdateKubeconfigGCP updates kubeconfig for GCP clusters.
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

// UpdateCoreDNSConfiguration updates CoreDNS configuration in all clusters.
func UpdateCoreDNSConfiguration(t *testing.T, r *operator.Region, kubeConfigPath string) {
	for i, clusterName := range r.Clusters {
		kubectlOpts := k8s.NewKubectlOptions(clusterName, kubeConfigPath, coreDNSNamespace)

		// Create and apply the updated CoreDNS ConfigMap with complete cluster information
		cm := coredns.CoreDNSConfigMap(operator.CustomDomains[i], r.CorednsClusterOptions)
		cmYAML := coredns.ToYAML(t, cm)
		if err := k8s.KubectlApplyFromStringE(t, kubectlOpts, cmYAML); err != nil {
			require.NoError(t, err, "failed to update CoreDNS ConfigMap for cluster %s", clusterName)
		}

		// Restart CoreDNS to pick up the updated configuration
		if err := k8s.RunKubectlE(t, kubectlOpts, "rollout", "restart", "deployment", coreDNSDeploymentName); err != nil {
			require.NoError(t, err, "failed to restart CoreDNS deployment for cluster %s", clusterName)
		}

		t.Logf("[%s] Updated CoreDNS configuration for cluster %s with namespace %s", r.Provider, clusterName, r.Namespace[clusterName])
	}
}

// UpdateCoreDNSWithNamespaces updates CoreDNS configuration with the current test namespaces.
// This is a generic function that works for all providers and should be called after namespaces are set in each test.
func UpdateCoreDNSWithNamespaces(t *testing.T, r *operator.Region) {
	require.True(t, len(r.Namespace) > 0, "no namespaces set - call this after setting test namespaces")

	kubeConfigPath, err := r.EnsureKubeConfigPath()
	require.NoError(t, err, "failed to get kubeconfig path")

	// Ensure CorednsClusterOptions is initialized
	require.NotNil(t, r.CorednsClusterOptions, "CorednsClusterOptions not initialized - infrastructure setup may have failed")

	// Update CoreDNS cluster options with current namespaces (keep existing IPs)
	for i, clusterName := range r.Clusters {
		if existing, ok := r.CorednsClusterOptions[operator.CustomDomains[i]]; ok {
			// Update only the namespace, keep existing IPs and domain.
			existing.Namespace = r.Namespace[clusterName]
			r.CorednsClusterOptions[operator.CustomDomains[i]] = existing
		} else {
			t.Logf("[%s] Warning: no existing CoreDNS options found for domain %s", r.Provider, operator.CustomDomains[i])
		}
	}

	// Apply the updated configuration to all clusters.
	UpdateCoreDNSConfiguration(t, r, kubeConfigPath)

	t.Logf("[%s] Updated CoreDNS configuration with test namespaces", r.Provider)
}
