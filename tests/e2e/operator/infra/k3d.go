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

// K3dRegion implements CloudProvider for K3D
type K3dRegion struct {
	*operator.Region
}

// SetUpInfra Creates K3d clusters, deploy calico CNI, deploy coredns in each cluster.
func (r *K3dRegion) SetUpInfra(t *testing.T) {
	// If using existing infra return clients.
	if r.ReusingInfra {
		t.Logf("[%s] Reusing existing infrastructure", ProviderK3D)
		return
	}

	t.Logf("[%s] Setting up infrastructure", ProviderK3D)

	var clients = make(map[string]client.Client)
	r.CorednsClusterOptions = make(map[string]coredns.CoreDNSClusterOption)

	// Get the current context name.
	kubeConfig, rawConfig := r.GetCurrentContext(t)

	for i, cluster := range r.Clusters {
		if _, ok := rawConfig.Contexts[cluster]; !ok {
			// Create a cluster using shell command.
			err := createK3DCluster(t, cluster, r.NodeCount)
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
		service := coredns.CoreDNSService(nil, GetLoadBalancerAnnotations(ProviderK3D))
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
			t.Logf("[%s] Failed to setup networking for region %s: %v", ProviderK3D, region, err)
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
func (r *K3dRegion) TeardownInfra(t *testing.T) {
	t.Logf("[%s] Tearing down K3D infrastructure", ProviderK3D)

	cmd := shell.Command{
		Command: "make",
		Args: []string{
			"test/multi-cluster/down",
		},
		WorkingDir: testutil.GetGitRoot(),
	}

	output, err := shell.RunCommandAndGetOutputE(t, cmd)
	if err != nil {
		t.Logf("[%s] Warning: Failed to tear down K3D clusters: %v\nOutput: %s",
			ProviderK3D, err, output)
	} else {
		t.Logf("[%s] Successfully tore down K3D clusters", ProviderK3D)
	}
}

// ScaleNodePool scales the node pool in a K3D cluster
func (r *K3dRegion) ScaleNodePool(t *testing.T, location string, nodeCount, index int) {
	t.Logf("[%s] K3D scaling not implemented - K3D doesn't support scaling node pools", ProviderK3D)
}

func (r *K3dRegion) CanScale() bool {
	return false
}

// setupNetworking ensures there is cross-k3d-cluster network connectivity and
// service discovery.
func (r *K3dRegion) setupNetworking(t *testing.T, ctx context.Context, region string, netConfig calico.K3dCalicoBGPPeeringOptions, options *k8s.KubectlOptions, clusterId int) error {
	// Mark the master nodes as our bgp edge. These nodes will act as our bgp
	// peers.
	clusterConfig := netConfig.ClusterConfig[region]
	clusterConfig.AddressAllocation = clusterId

	ctl := r.Clients[options.ContextName]

	// Get master nodes.
	var nodes []corev1.Node
	nodes, err := k8s.GetNodesByFilterE(t, options, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("%s=%s", "node-role.kubernetes.io/master", "true"),
	})
	if err != nil {
		return errors.Wrapf(err, "list nodes in %s", region)
	}

	// Patch server nodes with new annotation.
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

// createK3DCluster creates a new k3d cluster
// by calling the make command which will create
// a single k3d cluster.
func createK3DCluster(t *testing.T, clusterName string, nodeCount int) error {
	t.Logf("[%s] Creating new K3D cluster: %s with %d nodes", ProviderK3D, clusterName, nodeCount)
	cmd := shell.Command{
		Command: "make",
		Args: []string{
			"test/single-cluster/up",
			fmt.Sprintf("name=%s", strings.TrimLeft(clusterName, "k3d-")),
			fmt.Sprintf("nodes=%d", nodeCount),
		},
		WorkingDir: testutil.GetGitRoot(),
	}
	if version := os.Getenv("K3DVersion"); version != "" {
		cmd.Args = append(cmd.Args, fmt.Sprintf("version=%s", version))
	}

	output, err := shell.RunCommandAndGetOutputE(t, cmd)
	if err != nil {
		t.Logf("[%s] Failed to create cluster: %v", ProviderK3D, err)
		return fmt.Errorf("failed to create cluster: %v\nOutput: %s", err, output)
	}

	t.Logf("[%s] Successfully created new K3D cluster: %s", ProviderK3D, clusterName)
	return nil
}
