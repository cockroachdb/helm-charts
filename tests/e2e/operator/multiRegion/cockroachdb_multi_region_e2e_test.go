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
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Environment variable name to check if running in nightly mode
const isNightlyEnvVar = "isNightly"

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
	var providers []string
	if os.Getenv(isNightlyEnvVar) == "true" {
		providers = []string{infra.ProviderGCP}
	} else {
		providers = []string{infra.ProviderK3D}
	}

	for _, provider := range providers {
		provider := provider // Create a new variable to avoid closure issues
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
				if providerRegion.Provider != infra.ProviderK3D {
					clusterName = fmt.Sprintf("%s-%s", clusterName, strings.ToLower(random.UniqueId()))
				}
				providerRegion.Clusters = append(providerRegion.Clusters, clusterName)
			}

			// Setup infrastructure for this provider.
			providerRegion.setupInfra(t)

			// Teardown infra for this provider.
			defer func() {
				providerRegion.teardownInfra(t, provider)
			}()

			t.Run("TestHelmInstall", providerRegion.TestHelmInstall)
			t.Run("TestHelmUpgrade", providerRegion.TestHelmUpgrade)
			t.Run("TestClusterRollingRestart", providerRegion.TestClusterRollingRestart)
			t.Run("TestKillingCockroachNode", providerRegion.TestKillingCockroachNode)
			t.Run("TestClusterScaleUp", providerRegion.TestClusterScaleUp)
		})
	}
}

// TestHelmInstall will install Operator and CockroachDB charts in multiple regions,
// and verifies if CockroachDB has formed multi-region cluster.
func (r *multiRegion) TestHelmInstall(t *testing.T) {

	for _, cluster := range r.Clusters {
		r.Namespace[cluster] = fmt.Sprintf("%s-%s", operator.Namespace, strings.ToLower(random.UniqueId()))
	}

	// Set up multi-region k3d infra.
	r.setupInfra(t)

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

	// Get current context name.
	_, rawConfig := r.GetCurrentContext(t)

	// Validate CockroachDB functionality in each cluster.
	for _, cluster := range r.Clusters {
		if _, ok := rawConfig.Contexts[cluster]; !ok {
			t.Fatal()
		}
		rawConfig.CurrentContext = cluster
		// Validate CockroachDB cluster.
		r.ValidateCRDB(t, cluster)
	}
	// Validate Multi-region setup.
	r.ValidateMultiRegionSetup(t)
}

// TestHelmUpgrade will upgrade the existing charts in multiple regions,
// and verifies the CockroachDB health in multi-region.
func (r *multiRegion) TestHelmUpgrade(t *testing.T) {

	for _, cluster := range r.Clusters {
		r.Namespace[cluster] = fmt.Sprintf("%s-%s", operator.Namespace, strings.ToLower(random.UniqueId()))
	}

	// Set up multi-region infra.
	r.setupInfra(t)

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

	// Get current context name.
	kubeConfig, rawConfig := r.GetCurrentContext(t)

	// Validate CockroachDB functionality in each cluster.
	for _, cluster := range r.Clusters {
		if _, ok := rawConfig.Contexts[cluster]; !ok {
			t.Fatal()
		}
		rawConfig.CurrentContext = cluster
		// Validate CockroachDB cluster.
		r.ValidateCRDB(t, cluster)
	}

	// Get helm chart paths.
	helmChartPath, _ := operator.HelmChartPaths()
	for _, cluster := range r.Clusters {
		kubectlOptions := k8s.NewKubectlOptions(cluster, kubeConfig, r.Namespace[cluster])
		options := &helm.Options{
			KubectlOptions: kubectlOptions,
			ExtraArgs: map[string][]string{
				"upgrade": {"--reuse-values", "--set", fmt.Sprintf("cockroachdb.crdbCluster.resources.requests.cpu=%s", "100m")},
			},
		}
		// Apply Helm upgrade with updated values.
		helm.Upgrade(t, options, helmChartPath, operator.ReleaseName)

		// Get the initial timestamp of the pods before the upgrade.
		pods := k8s.ListPods(t, kubectlOptions, metav1.ListOptions{
			LabelSelector: operator.LabelSelector,
		})

		// Capture the creation timestamp of the last pod.
		initialTimestamp := pods[0].CreationTimestamp.Time

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
		for _, pod := range pods {
			require.Equal(t, resource.MustParse("100m"), pod.Spec.Containers[0].Resources.Requests["cpu"])
		}
		r.ValidateCRDB(t, cluster)
	}
	r.ValidateMultiRegionSetup(t)
}

// TestClusterRollingRestart will do a rolling restart by updating
// timestamp of each cockroachdb pod in multi region through helm upgrade and verifies the same.
func (r *multiRegion) TestClusterRollingRestart(t *testing.T) {

	for _, cluster := range r.Clusters {
		r.Namespace[cluster] = fmt.Sprintf("%s-%s", operator.Namespace, strings.ToLower(random.UniqueId()))
	}

	// Set up multi-region infra.
	r.setupInfra(t)

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

	// Get current context name.
	kubeConfig, rawConfig := r.GetCurrentContext(t)

	// Validate CockroachDB functionality in each cluster.
	for _, cluster := range r.Clusters {
		if _, ok := rawConfig.Contexts[cluster]; !ok {
			t.Fatal()
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
		initialTimestamp := pods[2].CreationTimestamp.Time

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
			require.False(t, pod.CreationTimestamp.Time.Before(upgradeTime), fmt.Errorf("pod %s was not restarted before %v", pod.Name, upgradeTime))
		}
	}
	r.ValidateMultiRegionSetup(t)
}

// TestKillingCockroachNode will manually kill one cockroachdb node to verify
// if the reconciliation is working as expected in multi region and verifies the same.
func (r *multiRegion) TestKillingCockroachNode(t *testing.T) {

	for _, cluster := range r.Clusters {
		r.Namespace[cluster] = fmt.Sprintf("%s-%s", operator.Namespace, strings.ToLower(random.UniqueId()))
	}

	// Set up multi-region infra.
	r.setupInfra(t)

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

	// Get current context name.
	kubeConfig, rawConfig := r.GetCurrentContext(t)

	// Validate CockroachDB functionality in each cluster.
	for _, cluster := range r.Clusters {
		if _, ok := rawConfig.Contexts[cluster]; !ok {
			t.Fatal()
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
			t.Fatal()
		}
		rawConfig.CurrentContext = cluster
		r.ValidateCRDB(t, cluster)
	}
	r.ValidateMultiRegionSetup(t)
}

// TestClusterScaleUp will scale the CockroachDB nodes in multiple regions
// and verifies the CockroachDB cluster health and replicas in each region.
func (r *multiRegion) TestClusterScaleUp(t *testing.T) {

	for _, cluster := range r.Clusters {
		r.Namespace[cluster] = fmt.Sprintf("%s-%s", operator.Namespace, strings.ToLower(random.UniqueId()))
	}

	// Set up multi-region infra.
	r.setupInfra(t)

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
			t.Fatal()
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

		// Scale the node pool in the cloud infrastructure
		r.scaleNodePool(t, r.RegionCodes[i], r.NodeCount, i)

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

func (r *multiRegion) setupInfra(t *testing.T) {
	// Use the provider factory to create the appropriate provider.
	provider := infra.ProviderFactory(r.Provider, &r.Region)
	if provider == nil {
		t.Fatalf("Unsupported provider: %s", r.Provider)
	}

	// Set up the infrastructure.
	provider.SetUpInfra(t)
}

func (r *multiRegion) teardownInfra(t *testing.T, provider string) {
	t.Logf("Tearing down infrastructure for provider: %s", provider)
	// Create provider using factory.
	providerType := infra.ProviderFactory(provider, &r.Region)
	if providerType == nil {
		t.Logf("Unsupported provider: %s", provider)
		return
	}

	// Check if the provider supports teardown.
	if teardownProvider, ok := infra.CanTeardown(providerType); ok {
		t.Logf("Running teardown for provider: %s", provider)
		teardownProvider.TeardownInfra(t)
	} else {
		t.Logf("Provider %s does not support teardown", provider)
	}
}

// scaleNodePool scales a node pool in a multi-region infrastructure.
func (r *multiRegion) scaleNodePool(t *testing.T, location string, nodeCount, index int) {
	// Create provider using factory
	provider := infra.ProviderFactory(r.Provider, &r.Region)
	if provider == nil {
		t.Fatalf("Unsupported provider: %s", r.Provider)
	}

	// Check if the provider supports scaling.
	if scaleProvider, ok := infra.CanScale(provider); ok {
		t.Logf("Scaling node pool for provider: %s", r.Provider)
		scaleProvider.ScaleNodePool(t, location, nodeCount, index)
	} else {
		t.Logf("Provider %s does not support scaling node pools", r.Provider)
	}
}
