package multiRegion

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/cockroachdb/helm-charts/tests/e2e/operator"
	"github.com/cockroachdb/helm-charts/tests/e2e/operator/infra"
	"github.com/cockroachdb/helm-charts/tests/testutil"
	"github.com/gruntwork-io/terratest/modules/helm"
	"github.com/gruntwork-io/terratest/modules/k8s"
	"github.com/gruntwork-io/terratest/modules/random"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Region codes for each provider are now centralized in infra.RegionCodes
type multiRegion struct {
	operator.OperatorUseCases
	operator.Region
}

func newMultiRegion() *multiRegion {
	return &multiRegion{}
}

// TestOperatorInMultiRegion tests CockroachDB operator functionality across multiple regions
func TestOperatorInMultiRegion(t *testing.T) {
	// Fetch provider from env
	var provider string
	if p := strings.TrimSpace(strings.ToLower(os.Getenv("PROVIDER"))); p != "" {
		switch p {
		case "kind":
			provider = infra.ProviderKind
		case "gcp":
			provider = infra.ProviderGCP
		default:
			t.Fatalf("Unsupported provider override: %s", p)
		}
	} else {
		provider = infra.ProviderK3D
	}

	t.Run(provider, func(t *testing.T) {
		t.Parallel()

		// Create a provider-specific instance to avoid race conditions.
		providerRegion := newMultiRegion()
		providerRegion.Region = operator.Region{
			IsMultiRegion: true,
			NodeCount:     3,
			ReusingInfra:  false,
		}
		providerRegion.Clients = make(map[string]client.Client)
		providerRegion.Namespace = make(map[string]string)

		providerRegion.Provider = provider
		for _, cluster := range operator.Clusters {
			clusterName := fmt.Sprintf("%s-%s", providerRegion.Provider, cluster)
			if providerRegion.Provider != infra.ProviderK3D && provider != infra.ProviderKind {
				clusterName = fmt.Sprintf("%s-%s", clusterName, strings.ToLower(random.UniqueId()))
			}
			providerRegion.Clusters = append(providerRegion.Clusters, clusterName)
		}

		// Create and reuse the same provider instance for both setup and teardown.
		cloudProvider := infra.ProviderFactory(providerRegion.Provider, &providerRegion.Region)
		if cloudProvider == nil {
			t.Fatalf("Unsupported provider: %s", provider)
		}

		// Use t.Cleanup for guaranteed cleanup even on test timeout/panic.
		t.Cleanup(func() {
			t.Logf("Starting infrastructure cleanup for provider: %s", provider)
			cloudProvider.TeardownInfra(t)
			t.Logf("Completed infrastructure cleanup for provider: %s", provider)
		})

		// Set up infrastructure for this provider once.
		cloudProvider.SetUpInfra(t)

		testCases := map[string]func(*testing.T){
			"TestHelmInstall":           providerRegion.TestHelmInstall,
			"TestHelmUpgrade":           providerRegion.TestHelmUpgrade,
			"TestClusterRollingRestart": providerRegion.TestClusterRollingRestart,
			"TestKillingCockroachNode":  providerRegion.TestKillingCockroachNode,
			"TestClusterScaleUp":        func(t *testing.T) { providerRegion.TestClusterScaleUp(t, cloudProvider) },
		}

		// Run tests sequentially within a provider.
		var testFailed bool
		for name, method := range testCases {
			// Skip remaining tests if a previous test failed to save time
			if testFailed {
				t.Logf("Skipping test %s due to previous test failure", name)
				continue
			}

			t.Run(name, func(t *testing.T) {
				// Add immediate cleanup trigger if this individual test fails
				defer func() {
					if t.Failed() {
						testFailed = true
						t.Logf("Test %s failed, triggering immediate infrastructure cleanup", name)
						cloudProvider.TeardownInfra(t)
						t.Logf("Infrastructure cleanup completed due to test failure")
					}
				}()

				method(t)
			})
		}
	})
}

// TestHelmInstall will install Operator and CockroachDB charts in multiple regions,
// and verifies if CockroachDB has formed a multi-region cluster.
func (r *multiRegion) TestHelmInstall(t *testing.T) {

	// Creating random namespace for each region.
	for _, cluster := range r.Clusters {
		r.Namespace[cluster] = fmt.Sprintf("%s-%s", operator.Namespace, strings.ToLower(random.UniqueId()))
	}

	// Update CoreDNS configuration with the test namespaces.
	infra.UpdateCoreDNSWithNamespaces(t, &r.Region)

	// Cleanup resources.
	defer r.CleanupResources(t)

	// Create CA certificate.
	err := r.CreateCACertificate(t)
	require.NoError(t, err)

	defer r.CleanUpCACertificate(t)

	// Apply operator, CockroachDB charts on each cluster.
	for i, cluster := range r.Clusters {
		r.InstallCharts(t, cluster, i)
	}

	// Get the current context name.
	_, rawConfig := r.GetCurrentContext(t)

	// Validate CockroachDB functionality in each cluster.
	for _, cluster := range r.Clusters {
		if _, ok := rawConfig.Contexts[cluster]; !ok {
			t.Fatalf("cluster context '%s' not found in kubeconfig", cluster)
		}
		rawConfig.CurrentContext = cluster
		// Validate CockroachDB cluster.
		r.ValidateCRDB(t, cluster)
	}
	// Validate Multi-region setup.
	r.ValidateMultiRegionSetup(t)
}

// TestHelmUpgrade will upgrade the existing charts in multiple regions,
// and verifies the CockroachDB health in a multi-region.
func (r *multiRegion) TestHelmUpgrade(t *testing.T) {

	// Creating random namespace for each region.
	for _, cluster := range r.Clusters {
		r.Namespace[cluster] = fmt.Sprintf("%s-%s", operator.Namespace, strings.ToLower(random.UniqueId()))
	}

	// Update CoreDNS configuration with the test namespaces.
	infra.UpdateCoreDNSWithNamespaces(t, &r.Region)

	// Cleanup resources.
	defer r.CleanupResources(t)

	// Create CA certificate.
	err := r.CreateCACertificate(t)
	require.NoError(t, err)

	defer r.CleanUpCACertificate(t)

	// Apply operator, CockroachDB charts on each cluster.
	for i, cluster := range r.Clusters {
		r.InstallCharts(t, cluster, i)
	}

	// Get the current context name.
	kubeConfig, rawConfig := r.GetCurrentContext(t)

	// Validate CockroachDB functionality in each cluster.
	for _, cluster := range r.Clusters {
		if _, ok := rawConfig.Contexts[cluster]; !ok {
			t.Fatalf("cluster context '%s' not found in kubeconfig", cluster)
		}
		rawConfig.CurrentContext = cluster
		// Validate CockroachDB cluster.
		r.ValidateCRDB(t, cluster)
	}

	// Get helm chart paths.
	helmChartPath, _ := operator.HelmChartPaths()
	for _, cluster := range r.Clusters {
		kubectlOptions := k8s.NewKubectlOptions(cluster, kubeConfig, r.Namespace[cluster])
		// Get the initial timestamp of the pods before the upgrade.
		pods := k8s.ListPods(t, kubectlOptions, metav1.ListOptions{
			LabelSelector: operator.LabelSelector,
		})

		// Capture the creation timestamp of the last pod.
		// Capture the creation timestamp of the last pod.
		if len(pods) == 0 {
			require.Fail(t, "No pods found for deployment")
		}
		initialTimestamp := pods[len(pods)-1].CreationTimestamp.Time
		options := &helm.Options{
			KubectlOptions: kubectlOptions,
			ExtraArgs: map[string][]string{
				"upgrade": {"--reuse-values", "--set", fmt.Sprintf("cockroachdb.crdbCluster.resources.requests.cpu=%s", "100m")},
			},
		}
		// Apply Helm upgrade with updated values.
		helm.Upgrade(t, options, helmChartPath, operator.ReleaseName)

		// Verify if the pods are restarted after helm upgrade.
		err = r.VerifyHelmUpgrade(t, initialTimestamp, kubectlOptions)
		require.NoError(t, err)

		crdbCluster := testutil.CockroachCluster{
			DesiredNodes: r.NodeCount,
		}
		testutil.RequireCRDBClusterToBeReadyEventuallyTimeout(t, kubectlOptions, crdbCluster, 600*time.Second)
		r.ValidateCRDBContainerResources(t, kubectlOptions)
		r.ValidateCRDB(t, cluster)
	}
	r.ValidateMultiRegionSetup(t)
}

// TestClusterRollingRestart will do a rolling restart by updating
// the timestamp of each cockroachdb pod in a multi region through helm upgrade and verifies the same.
func (r *multiRegion) TestClusterRollingRestart(t *testing.T) {

	// Creating random namespace for each region.
	for _, cluster := range r.Clusters {
		r.Namespace[cluster] = fmt.Sprintf("%s-%s", operator.Namespace, strings.ToLower(random.UniqueId()))
	}

	// Update CoreDNS configuration with the test namespaces.
	infra.UpdateCoreDNSWithNamespaces(t, &r.Region)

	// Cleanup resources.
	defer r.CleanupResources(t)

	// Create CA certificate.
	err := r.CreateCACertificate(t)
	require.NoError(t, err)

	defer r.CleanUpCACertificate(t)

	// Apply operator, CockroachDB charts on each cluster.
	for i, cluster := range r.Clusters {
		r.InstallCharts(t, cluster, i)
	}

	// Get the current context name.
	kubeConfig, rawConfig := r.GetCurrentContext(t)

	// Validate CockroachDB functionality in each cluster.
	for _, cluster := range r.Clusters {
		if _, ok := rawConfig.Contexts[cluster]; !ok {
			t.Fatalf("cluster context '%s' not found in kubeconfig", cluster)
		}
		rawConfig.CurrentContext = cluster
		r.ValidateCRDB(t, cluster)
	}

	// Get helm chart paths.
	helmChartPath, _ := operator.HelmChartPaths()

	// Modify the timestamp value and apply helm upgrade.
	var upgradeTime time.Time
	for _, cluster := range r.Clusters {
		// Helm upgrade with timestamp annotation.
		kubectlOptions := k8s.NewKubectlOptions(cluster, kubeConfig, r.Namespace[cluster])
		upgradeTime = time.Now().UTC()

		options := &helm.Options{
			KubectlOptions: kubectlOptions,
			ExtraArgs: map[string][]string{
				"upgrade": {"--reuse-values", "--set", fmt.Sprintf("cockroachdb.crdbCluster.timestamp=%s", upgradeTime.Format(time.RFC3339))},
			},
		}
		helm.Upgrade(t, options, helmChartPath, operator.ReleaseName)

		// Get the initial timestamp of the pods before the upgrade.
		pods := k8s.ListPods(t, kubectlOptions, metav1.ListOptions{
			LabelSelector: operator.LabelSelector,
		})
		if len(pods) == 0 {
			require.Fail(t, "No initial pods found for deployment")
		}

		// Capture the creation timestamp of the last pod.
		if len(pods) < 3 {
			require.Fail(t, "Expected at least 3 pods but found %d", len(pods))
		}
		initialTimestamp := pods[len(pods)-1].CreationTimestamp.Time

		// Verify if the pods are restarted after helm upgrade.
		err = r.VerifyHelmUpgrade(t, initialTimestamp, kubectlOptions)
		require.NoError(t, err)

		crdbCluster := testutil.CockroachCluster{
			DesiredNodes: r.NodeCount,
		}
		testutil.RequireCRDBClusterToBeReadyEventuallyTimeout(t, kubectlOptions, crdbCluster, 600*time.Second)
		pods = k8s.ListPods(t, kubectlOptions, metav1.ListOptions{
			LabelSelector: operator.LabelSelector,
		})
		// Verify that each pod's creation timestamp is after the upgradeTime.
		for _, pod := range pods {
			require.False(t, pod.CreationTimestamp.Time.Before(upgradeTime), "pod %s was not restarted after %v", pod.Name, upgradeTime)
		}
	}
	r.ValidateMultiRegionSetup(t)
}

// TestKillingCockroachNode will manually kill one cockroachdb node to verify
// if the reconciliation is working as expected in multi region and verifies the same.
func (r *multiRegion) TestKillingCockroachNode(t *testing.T) {

	// Creating random namespace for each region.
	for _, cluster := range r.Clusters {
		r.Namespace[cluster] = fmt.Sprintf("%s-%s", operator.Namespace, strings.ToLower(random.UniqueId()))
	}

	// Update CoreDNS configuration with the test namespaces.
	infra.UpdateCoreDNSWithNamespaces(t, &r.Region)

	// Cleanup resources.
	defer r.CleanupResources(t)

	// Create CA certificate.
	err := r.CreateCACertificate(t)
	require.NoError(t, err)

	defer r.CleanUpCACertificate(t)

	// Apply operator, CockroachDB charts on each cluster.
	for i, cluster := range r.Clusters {
		r.InstallCharts(t, cluster, i)
	}

	// Get the current context name.
	kubeConfig, rawConfig := r.GetCurrentContext(t)

	// Validate CockroachDB functionality in each cluster.
	for _, cluster := range r.Clusters {
		if _, ok := rawConfig.Contexts[cluster]; !ok {
			t.Fatalf("cluster context '%s' not found in kubeconfig", cluster)
		}
		rawConfig.CurrentContext = cluster
		r.ValidateCRDB(t, cluster)
	}

	// Kill a pod in each cluster and verify the reconciliation.
	for _, cluster := range r.Clusters {
		kubectlOptions := k8s.NewKubectlOptions(cluster, kubeConfig, r.Namespace[cluster])
		pods := k8s.ListPods(t, kubectlOptions, metav1.ListOptions{
			LabelSelector: operator.LabelSelector,
		})

		// Kill a node in the cluster.
		err := k8s.RunKubectlE(t, kubectlOptions, "delete", "pod", pods[0].Name)
		require.NoError(t, err)

		// Wait till the reconciliation is done and all the pods are up and running.
		crdbCluster := testutil.CockroachCluster{
			DesiredNodes: r.NodeCount,
		}
		testutil.RequireCRDBClusterToBeReadyEventuallyTimeout(t, kubectlOptions, crdbCluster, 600*time.Second)
		// Validate CockroachDB cluster.
		if _, ok := rawConfig.Contexts[cluster]; !ok {
			t.Fatalf("cluster context '%s' not found in kubeconfig", cluster)
		}
		rawConfig.CurrentContext = cluster
		r.ValidateCRDB(t, cluster)
	}
	r.ValidateMultiRegionSetup(t)
}

// TestClusterScaleUp will scale the CockroachDB nodes in multiple regions
// and verifies the CockroachDB cluster health and replicas using the provided cloudProvider.
func (r *multiRegion) TestClusterScaleUp(t *testing.T, cloudProvider infra.CloudProvider) {

	// Creating random namespace for each region.
	for _, cluster := range r.Clusters {
		r.Namespace[cluster] = fmt.Sprintf("%s-%s", operator.Namespace, strings.ToLower(random.UniqueId()))
	}

	// Update CoreDNS configuration with the test namespaces.
	infra.UpdateCoreDNSWithNamespaces(t, &r.Region)

	// Cleanup resources.
	defer r.CleanupResources(t)

	// Create CA certificate.
	err := r.CreateCACertificate(t)
	require.NoError(t, err)

	defer r.CleanUpCACertificate(t)

	// Apply Operator, CockroachDB charts on each cluster.
	for i, cluster := range r.Clusters {
		r.InstallCharts(t, cluster, i)
	}

	// Get the current context name.
	kubeConfig, rawConfig := r.GetCurrentContext(t)

	// Validate CockroachDB functionality in each cluster.
	for _, cluster := range r.Clusters {
		if _, ok := rawConfig.Contexts[cluster]; !ok {
			t.Fatalf("cluster context '%s' not found in kubeconfig", cluster)
		}
		rawConfig.CurrentContext = cluster

		r.ValidateCRDB(t, cluster)
	}
	// Get helm chart paths.
	helmChartPath, _ := operator.HelmChartPaths()

	r.NodeCount += 1
	// Modify the nodes in each region and apply helm upgrade.
	for i, cluster := range r.Clusters {
		kubectlOptions := k8s.NewKubectlOptions(cluster, kubeConfig, r.Namespace[cluster])

		// Check if scaling is supported by the cloud provider.
		if cloudProvider.CanScale() {
			t.Logf("Scaling node pool for provider: %s in region: %s", r.Provider, r.RegionCodes[i])
			cloudProvider.ScaleNodePool(t, r.RegionCodes[i], r.NodeCount, i)
		} else {
			t.Logf("Provider %s does not support scaling", r.Provider)
		}

		options := &helm.Options{
			KubectlOptions: kubectlOptions,
			SetJsonValues: map[string]string{
				"cockroachdb.crdbCluster.regions": operator.MustMarshalJSON(r.OperatorRegions(i, r.NodeCount)),
			},
			ExtraArgs: map[string][]string{
				"upgrade": {"--reuse-values", "--wait"},
			},
		}
		// Apply Helm upgrade with updated values.
		helm.Upgrade(t, options, helmChartPath, operator.ReleaseName)

		crdbCluster := testutil.CockroachCluster{
			DesiredNodes: r.NodeCount,
		}
		testutil.RequireCRDBClusterToBeReadyEventuallyTimeout(t, kubectlOptions, crdbCluster, 1200*time.Second)
		// Validate CockroachDB cluster.
		r.ValidateCRDB(t, cluster)
	}
	r.ValidateMultiRegionSetup(t)
}
