package infra

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/cockroachdb/errors"
	"github.com/cockroachdb/helm-charts/tests/e2e/calico"
	"github.com/cockroachdb/helm-charts/tests/e2e/coredns"
	"github.com/cockroachdb/helm-charts/tests/e2e/operator"
	"github.com/cockroachdb/helm-charts/tests/testutil"
	"github.com/gruntwork-io/terratest/modules/k8s"
	"github.com/gruntwork-io/terratest/modules/retry"
	"github.com/gruntwork-io/terratest/modules/shell"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apiextv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"
)

// LocalRegion implements CloudProvider for local Kubernetes providers (K3d and Kind)
type LocalRegion struct {
	*operator.Region
	// "k3d" or "kind"
	ProviderType string
}

// SetUpInfra Creates local k3d and kind clusters, deploy CNI, deploy coredns in each cluster.
//
// Multi-region networking approach:
//   - K3D: Calico CNI with BGP for cross-cluster pod routing, built-in ServiceLB for LBs.
//   - Kind: default kindnet for in-cluster, Calico objects + BGP peering to advertise
//     pod/service CIDRs between clusters. MetalLB for CoreDNS LBs.
//   - CoreDNS instances forward requests for other cluster domains; endpoints can be
//     ClusterIP/pod IPs (with Calico) or LB IPs (with MetalLB).
func (r *LocalRegion) SetUpInfra(t *testing.T) {
	// If using existing infra return clients.
	if r.ReusingInfra {
		t.Logf("[%s] Reusing existing infrastructure", r.ProviderType)
		return
	}

	t.Logf("[%s] Setting up infrastructure", r.ProviderType)

	var clients = make(map[string]client.Client)
	r.CorednsClusterOptions = make(map[string]coredns.CoreDNSClusterOption)

	// Get the current context name.
	kubeConfig, rawConfig := r.GetCurrentContext(t)

	for i, cluster := range r.Clusters {
		if _, ok := rawConfig.Contexts[cluster]; !ok {
			// Create a cluster using shell command.
			err := r.createLocalCluster(t, cluster, r.NodeCount)
			require.NoError(t, err)
		}

		cfg, err := config.GetConfigWithContext(cluster)
		require.NoError(t, err)
		k8sClient, err := client.New(cfg, client.Options{})
		require.NoError(t, err)
		clients[cluster] = k8sClient

		// Add the apiextensions scheme to the client's scheme.
		_ = apiextv1.AddToScheme(k8sClient.Scheme())

		kubectlOptions := k8s.NewKubectlOptions(cluster, kubeConfig, coreDNSNamespace)

		// Install Calico.
		calico.RegisterCalicoGVK(k8sClient.Scheme())
		objects := calico.K3DCalicoCNI(calico.K3dClusterBGPConfig{
			AddressAllocation: i,
		})

		for _, obj := range objects {
			err = k8sClient.Create(context.Background(), obj)
			require.NoError(t, err)
		}

		// For Kind, install MetalLB before creating LoadBalancer services
		// Calico+BGP provides pod routing; MetalLB provides external LB IPs when needed
		if r.ProviderType == ProviderKind {
			// Install MetalLB with Docker network IPs
			err = r.installMetalLBWithDockerIPs(t, kubectlOptions, i)
			require.NoError(t, err)
		}

		// Create or update CoreDNS deployment.
		deployment := coredns.CoreDNSDeployment(coreDNSReplicas)
		// Apply deployment.
		deploymentYaml := coredns.ToYAML(t, deployment)
		err = k8s.KubectlApplyFromStringE(t, kubectlOptions, deploymentYaml)
		require.NoError(t, err)

		// Wait for deployment to be ready.
		_, err = retry.DoWithRetryE(t, "waiting for coredns deployment",
			defaultRetries, defaultRetryInterval,
			func() (string, error) {
				return k8s.RunKubectlAndGetOutputE(t, kubectlOptions,
					"wait", "--for=condition=Available", fmt.Sprintf("deployment/%s", coreDNSDeploymentName))
			})
		require.NoError(t, err)

		// Create a CoreDNS service.
		service := coredns.CoreDNSService(nil, GetLoadBalancerAnnotations(r.ProviderType))
		serviceYaml := coredns.ToYAML(t, service)
		// Apply service.
		err = k8s.KubectlApplyFromStringE(t, kubectlOptions, serviceYaml)
		require.NoError(t, err)

		// Now get the DNS IPs.
		ips, err := WaitForCoreDNSServiceIPs(t, kubectlOptions)
		require.NoError(t, err)

		r.CorednsClusterOptions[operator.CustomDomains[i]] = coredns.CoreDNSClusterOption{
			IPs:       ips,
			Namespace: r.Namespace[cluster],
			Domain:    operator.CustomDomains[i],
		}
		if !r.IsMultiRegion {
			break
		}
	}

	// Update Coredns config.
	for i, cluster := range r.Clusters {
		// Create or update CoreDNS configmap.
		kubectlOptions := k8s.NewKubectlOptions(cluster, kubeConfig, coreDNSNamespace)
		cm := coredns.CoreDNSConfigMap(operator.CustomDomains[i], r.CorednsClusterOptions)

		// Apply the updated ConfigMap to Kubernetes.
		cmYaml := coredns.ToYAML(t, cm)
		err := k8s.KubectlApplyFromStringE(t, kubectlOptions, cmYaml)
		require.NoError(t, err)

		// restart coredns pods.
		err = k8s.RunKubectlE(t, kubectlOptions, "rollout", "restart", "deployment", coreDNSDeploymentName)
		require.NoError(t, err)
		if !r.IsMultiRegion {
			r.Clients = clients
			r.ReusingInfra = true
			return
		}
	}
	r.Clients = clients
	r.ReusingInfra = true

	netConfig := calico.K3dCalicoBGPPeeringOptions{
		ClusterConfig: map[string]calico.K3dClusterBGPConfig{},
	}

	// Update network config for each region.
	for i, region := range r.RegionCodes {
		rawConfig.CurrentContext = r.Clusters[i]
		kubectlOptions := k8s.NewKubectlOptions(r.Clusters[i], kubeConfig, coreDNSNamespace)
		err := r.setupNetworking(t, context.TODO(), region, netConfig, kubectlOptions, i)
		if err != nil {
			t.Logf("[%s] Failed to setup networking for region %s: %v", r.ProviderType, region, err)
		}
	}

	objectsByRegion := calico.K3dCalicoBGPPeeringObjects(netConfig)
	// Apply all the objects for each region on to the cluster.
	for i, region := range r.RegionCodes {
		ctl := clients[r.Clusters[i]]
		for _, obj := range objectsByRegion[region] {
			err := ctl.Create(context.Background(), obj)
			require.NoError(t, err)
		}
	}
}

// TeardownInfra cleans up all resources created by SetUpInfra
func (r *LocalRegion) TeardownInfra(t *testing.T) {
	t.Logf("[%s] Tearing down %s infrastructure", r.ProviderType, r.ProviderType)

	var cmd shell.Command
	switch r.ProviderType {
	case ProviderK3D:
		cmd = shell.Command{
			Command: "make",
			Args: []string{
				"test/multi-cluster/down",
			},
			WorkingDir: testutil.GetGitRoot(),
		}
	case ProviderKind:
		cmd = shell.Command{
			Command: "bash",
			Args: []string{
				"tests/kind/dev-multi-cluster.sh",
				"down",
			},
			WorkingDir: testutil.GetGitRoot(),
		}
	default:
		t.Logf("[%s] Unknown provider type for teardown", r.ProviderType)
		return
	}

	output, err := shell.RunCommandAndGetOutputE(t, cmd)
	if err != nil {
		t.Logf("[%s] Warning: Failed to tear down %s clusters: %v\nOutput: %s",
			r.ProviderType, r.ProviderType, err, output)
	} else {
		t.Logf("[%s] Successfully tore down %s clusters", r.ProviderType, r.ProviderType)
	}
}

// ScaleNodePool scales the node pool in a local cluster
func (r *LocalRegion) ScaleNodePool(t *testing.T, location string, nodeCount, index int) {
	t.Logf("[%s] %s scaling not implemented - %s doesn't support scaling node pools", r.ProviderType, r.ProviderType, r.ProviderType)
}

func (r *LocalRegion) CanScale() bool {
	return false
}

// setupNetworking ensures there is cross-cluster network connectivity and
// service discovery.
func (r *LocalRegion) setupNetworking(t *testing.T, ctx context.Context, region string, netConfig calico.K3dCalicoBGPPeeringOptions, options *k8s.KubectlOptions, clusterId int) error {
	// Mark the control-plane/master nodes as our bgp edge. These nodes will act as our bgp
	// peers.
	clusterConfig := netConfig.ClusterConfig[region]
	clusterConfig.AddressAllocation = clusterId

	ctl := r.Clients[options.ContextName]

	// Get control-plane/master nodes based on provider type.
	var nodes []corev1.Node
	var labelSelector string
	switch r.ProviderType {
	case ProviderK3D:
		labelSelector = fmt.Sprintf("%s=%s", "node-role.kubernetes.io/master", "true")
	case ProviderKind:
		labelSelector = fmt.Sprintf("%s=%s", "node-role.kubernetes.io/control-plane", "")
	default:
		return fmt.Errorf("unknown provider type: %s", r.ProviderType)
	}

	nodes, err := k8s.GetNodesByFilterE(t, options, metav1.ListOptions{
		LabelSelector: labelSelector,
	})
	if err != nil {
		return errors.Wrapf(err, "list nodes in %s", region)
	}

	// Patch control-plane/master nodes with new annotation.
	for _, node := range nodes {
		patch := []byte(`{"metadata": {"annotations": {"projectcalico.org/labels": "{\"edge\":\"true\"}"}}}`)
		if err := ctl.Patch(ctx, &node, client.RawPatch(types.StrategicMergePatchType, patch)); err != nil {
			return errors.Wrapf(err, "annotate node for calico edge")
		}

		time.Sleep(15 * time.Second)

		for _, nodeAddress := range node.Status.Addresses {
			if nodeAddress.Type == corev1.NodeInternalIP {
				clusterConfig.PeeringNodes = append(clusterConfig.PeeringNodes, nodeAddress.Address)
			}
		}
	}
	netConfig.ClusterConfig[region] = clusterConfig
	return nil
}

// createLocalCluster creates a new local cluster (k3d or kind)
// by calling the appropriate shell command.
func (r *LocalRegion) createLocalCluster(t *testing.T, clusterName string, nodeCount int) error {
	t.Logf("[%s] Creating new %s cluster: %s with %d nodes", r.ProviderType, r.ProviderType, clusterName, nodeCount)

	var cmd shell.Command
	switch r.ProviderType {
	case ProviderK3D:
		cmd = shell.Command{
			Command: "make",
			Args: []string{
				"test/single-cluster/up",
			},
			WorkingDir: testutil.GetGitRoot(),
		}
	case ProviderKind:
		cmd = shell.Command{
			Command: "bash",
			Args: []string{
				"tests/kind/dev-multi-cluster.sh",
				"up",
				"--name=chart-testing",
				fmt.Sprintf("--nodes=%d", nodeCount),
				fmt.Sprintf("--clusters=%d", len(r.Clusters)),
			},
			WorkingDir: testutil.GetGitRoot(),
		}
	default:
		return fmt.Errorf("unknown provider type: %s", r.ProviderType)
	}
	if version := os.Getenv("K3DVersion"); version != "" {
		cmd.Args = append(cmd.Args, fmt.Sprintf("version=%s", version))
	}

	output, err := shell.RunCommandAndGetOutputE(t, cmd)
	if err != nil {
		t.Logf("[%s] Failed to create cluster: %v", r.ProviderType, err)
		return fmt.Errorf("failed to create cluster: %v\nOutput: %s", err, output)
	}

	t.Logf("[%s] Successfully created new %s cluster: %s", r.ProviderType, r.ProviderType, clusterName)
	return nil
}

// installMetalLBWithDockerIPs installs MetalLB in a Kind cluster and configures it with
// auto-detected Docker network IPs.
func (r *LocalRegion) installMetalLBWithDockerIPs(t *testing.T, kubectlOptions *k8s.KubectlOptions, clusterIndex int) error {
	t.Logf("Installing MetalLB for cluster %s with Docker network IPs", kubectlOptions.ContextName)

	// Create kubectl options for MetalLB namespace
	kubectlOptionsMetallb := k8s.NewKubectlOptions(kubectlOptions.ContextName, kubectlOptions.ConfigPath, "metallb-system")

	// 1. Install MetalLB using official manifests
	metallbManifest := "https://raw.githubusercontent.com/metallb/metallb/v0.15.2/config/manifests/metallb-native.yaml"

	t.Logf("Applying MetalLB manifests from %s", metallbManifest)
	err := k8s.RunKubectlE(t, kubectlOptionsMetallb, "apply", "-f", metallbManifest)
	if err != nil {
		return fmt.Errorf("failed to apply MetalLB manifests: %w", err)
	}

	// 2. Wait for MetalLB controller and speaker to be ready
	t.Log("Waiting for MetalLB controller deployment to be ready")
	_, err = retry.DoWithRetryE(t, "wait for metallb controller", defaultRetries, defaultRetryInterval,
		func() (string, error) {
			return k8s.RunKubectlAndGetOutputE(t, kubectlOptionsMetallb,
				"wait", "--for=condition=Available", "deployment/controller", "--timeout=120s")
		})
	if err != nil {
		return fmt.Errorf("MetalLB controller failed to become ready: %w", err)
	}

	t.Log("Waiting for MetalLB speaker daemonset to be ready")
	_, err = retry.DoWithRetryE(t, "wait for metallb speaker", defaultRetries, defaultRetryInterval,
		func() (string, error) {
			return k8s.RunKubectlAndGetOutputE(t, kubectlOptionsMetallb,
				"wait", "--for=condition=Ready", "pod", "-l", "app=metallb,component=speaker", "--timeout=120s")
		})
	if err != nil {
		return fmt.Errorf("MetalLB speaker failed to become ready: %w", err)
	}

	// Note: strictARP (needed for kube-proxy IPVS) is not required for Kind's default iptables mode.
	// 3. Auto-detect Docker network subnet for the shared multi-cluster network
	networkName := "kind-chart-testing"
	t.Logf("Detecting Docker network subnet for %s", networkName)

	cmd := shell.Command{
		Command: "docker",
		Args: []string{
			"network", "inspect", networkName,
			"--format", "{{(index .IPAM.Config 0).Subnet}}",
		},
	}

	output, err := shell.RunCommandAndGetOutputE(t, cmd)
	if err != nil {
		return fmt.Errorf("failed to detect Docker network subnet for %s: %w", networkName, err)
	}

	subnet := strings.TrimSpace(output)
	t.Logf("Detected Docker subnet for cluster %d: %s", clusterIndex, subnet)

	// 4. Parse subnet and create a unique per-cluster IP range from the high end
	// Example: 172.20.0.0/16 -> cluster 0: 172.20.255.200-172.20.255.214, cluster 1: 215-229, cluster 2: 230-244
	parts := strings.Split(subnet, ".")
	if len(parts) != 4 {
		return fmt.Errorf("invalid subnet format: %s", subnet)
	}

	// Compute a non-overlapping range per cluster index within the .255.x space
	rangeStart := 200 + (clusterIndex * 15)
	if rangeStart > 254 {
		rangeStart = 240
	}
	rangeEnd := rangeStart + 14
	if rangeEnd > 254 {
		rangeEnd = 254
	}
	ipRange := fmt.Sprintf("%s.%s.%s.%d-%s.%s.%s.%d", parts[0], parts[1], "255", rangeStart, parts[0], parts[1], "255", rangeEnd)
	t.Logf("MetalLB IP address pool: %s", ipRange)

	// 5. Apply MetalLB IPAddressPool and L2Advertisement
	ipPoolYAML := fmt.Sprintf(`
apiVersion: metallb.io/v1beta1
kind: IPAddressPool
metadata:
  name: kind-pool-%d
  namespace: metallb-system
spec:
  addresses:
  - %s
---
apiVersion: metallb.io/v1beta1
kind: L2Advertisement
metadata:
  name: kind-l2adv-%d
  namespace: metallb-system
spec:
  ipAddressPools:
  - kind-pool-%d
`, clusterIndex, ipRange, clusterIndex, clusterIndex)

	t.Log("Applying MetalLB IPAddressPool and L2Advertisement config")
	err = k8s.KubectlApplyFromStringE(t, kubectlOptionsMetallb, ipPoolYAML)
	if err != nil {
		return fmt.Errorf("failed to apply MetalLB IP pool configuration: %w", err)
	}

	t.Logf("Successfully installed and configured MetalLB for cluster %s", kubectlOptions.ContextName)
	t.Logf("MetalLB installed with IP pool: %s", ipRange)
	return nil
}
