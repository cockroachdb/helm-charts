package infra

import (
	"context"
	"fmt"
	"os"
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
	ProviderType string // "k3d" or "kind"
}

// SetUpInfra Creates local clusters, deploy calico CNI, deploy coredns in each cluster.
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

		// For Kind, manually assign node IPs to LoadBalancer service (simulate ServiceLB)
		if r.ProviderType == ProviderKind {
			err = r.assignNodeIPsToLoadBalancer(t, kubectlOptions, k8sClient)
			require.NoError(t, err)
		}

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
				"tests/kind/dev-multi-cluster-kind.sh",
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
				"tests/kind/dev-multi-cluster-kind.sh",
				"up",
				"--name=chart-testing",
				fmt.Sprintf("--nodes=%d", nodeCount),
				fmt.Sprintf("--clusters=%d", len(r.Clusters)),
				"--disable-cni=true",
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

// assignNodeIPsToLoadBalancer manually assigns node IPs to LoadBalancer service status
// for Kind clusters to simulate ServiceLB behavior in K3d
func (r *LocalRegion) assignNodeIPsToLoadBalancer(t *testing.T, kubectlOptions *k8s.KubectlOptions, k8sClient client.Client) error {
	// Get the CoreDNS service
	svc := &corev1.Service{}
	err := k8sClient.Get(context.Background(), types.NamespacedName{
		Name:      coreDNSServiceName,
		Namespace: coreDNSNamespace,
	}, svc)
	if err != nil {
		return fmt.Errorf("failed to get CoreDNS service: %v", err)
	}

	// Get node IPs (similar to how setupNetworking gets them)
	nodes, err := k8s.GetNodesByFilterE(t, kubectlOptions, metav1.ListOptions{
		LabelSelector: "node-role.kubernetes.io/control-plane",
	})
	if err != nil {
		return fmt.Errorf("failed to get nodes: %v", err)
	}

	// Extract internal IPs from nodes
	var nodeIPs []string
	for _, node := range nodes {
		for _, addr := range node.Status.Addresses {
			if addr.Type == corev1.NodeInternalIP {
				nodeIPs = append(nodeIPs, addr.Address)
			}
		}
	}

	if len(nodeIPs) == 0 {
		return fmt.Errorf("no node IPs found")
	}

	// Patch the service status to include node IPs as LoadBalancer ingress
	var ingresses []corev1.LoadBalancerIngress
	for _, ip := range nodeIPs {
		ingresses = append(ingresses, corev1.LoadBalancerIngress{IP: ip})
	}

	// Update service status
	svc.Status.LoadBalancer.Ingress = ingresses
	err = k8sClient.Status().Update(context.Background(), svc)
	if err != nil {
		return fmt.Errorf("failed to update service status: %v", err)
	}

	t.Logf("[%s] Assigned node IPs %v to CoreDNS LoadBalancer service", r.ProviderType, nodeIPs)
	return nil
}
