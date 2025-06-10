package singleRegion

import (
	"fmt"
	"github.com/gruntwork-io/terratest/modules/random"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/cockroachdb/helm-charts/tests/e2e/operator/infra"

	"github.com/cockroachdb/helm-charts/tests/e2e/operator"
	"github.com/cockroachdb/helm-charts/tests/testutil"
	"github.com/gruntwork-io/terratest/modules/helm"
	"github.com/gruntwork-io/terratest/modules/k8s"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Todo: Have a var field for regions with all provider codes
type singleRegion struct {
	operator.OperatorUseCases
	operator.Region
}

func newSingleRegion() *singleRegion {
	return &singleRegion{}
}
func TestOperatorInSingleRegion(t *testing.T) {

	r := newSingleRegion()
	r.Region = operator.Region{
		IsMultiRegion: false,
		NodeCount:     3,
		ReusingInfra:  false,
	}
	r.Clients = make(map[string]client.Client)
	r.Namespace = make(map[string]string)

	var providers []string
	if os.Getenv("isNightly") == "false" {
		providers = []string{"k3d"}
	} else {
		providers = []string{"gcp"}
	}

	defer r.tearDownInfra(t, providers)

	for _, provider := range providers {
		provider := provider // Create new variable to avoid closure issues
		t.Run(provider, func(t *testing.T) {
			r.Provider = provider
			r.Clusters = append(r.Clusters, fmt.Sprintf("%s-%s", r.Provider, operator.Clusters[0]))

			//t.Run("TestHelmInstall", r.TestHelmInstall)
			//t.Run("TestHelmUpgrade", r.TestHelmUpgrade)
			//t.Run("TestClusterRollingRestart", r.TestClusterRollingRestart)
			//t.Run("TestKillingCockroachNode", r.TestKillingCockroachNode)
			t.Run("TestClusterScaleUp", r.TestClusterScaleUp)
			t.Run("TestInstallWithCertManager", r.TestInstallWithCertManager)
		})
	}
}

// TestHelmInstall will install Operator and CockroachDB charts
// and verifies if CockroachDB service is up and running.
func (r *singleRegion) TestHelmInstall(t *testing.T) {

	cluster := r.Clusters[0]
	r.Namespace[cluster] = fmt.Sprintf("%s-%s", operator.Namespace, strings.ToLower(random.UniqueId()))

	// Setup Single region infra.
	r.setupInfra(t)

	// Cleanup resources.
	defer r.CleanupResources(t)

	// Create CA certificate.
	err := r.CreateCACertificate(t)
	require.NoError(t, err)

	defer r.CleanUpCACertificate(t)

	// Install Operator and CockroachDB charts.
	r.InstallCharts(t, cluster, 0)

	// Get current context name.
	_, rawConfig := r.GetCurrentContext(t)

	if _, ok := rawConfig.Contexts[cluster]; !ok {
		t.Fatal()
	}
	rawConfig.CurrentContext = cluster
	r.ValidateCRDB(t, cluster)
}

// TestHelmUpgrade will upgrade the existing charts in a single region
// and verifies the CockroachDB health.
func (r *singleRegion) TestHelmUpgrade(t *testing.T) {
	cluster := r.Clusters[0]
	r.Namespace[cluster] = fmt.Sprintf("%s-%s", operator.Namespace, strings.ToLower(random.UniqueId()))

	// Setup Single region infra.
	r.setupInfra(t)

	// Cleanup resources.
	defer r.CleanupResources(t)

	// Create CA certificate.
	err := r.CreateCACertificate(t)
	require.NoError(t, err)

	defer r.CleanUpCACertificate(t)

	// Install Operator and CockroachDB charts.
	r.InstallCharts(t, cluster, 0)

	// Get current context name.
	kubeConfig, rawConfig := r.GetCurrentContext(t)

	if _, ok := rawConfig.Contexts[cluster]; !ok {
		t.Fatal()
	}
	rawConfig.CurrentContext = cluster

	// Validate CockroachDB cluster.
	r.ValidateCRDB(t, cluster)

	// Get helm chart paths.
	helmChartPath, _ := operator.HelmChartPaths()
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

// TestClusterRollingRestart will do a rolling restart by updating
// timestamp of each cockroachdb pod in single region through helm upgrade and verifies the same.
func (r *singleRegion) TestClusterRollingRestart(t *testing.T) {
	cluster := r.Clusters[0]
	r.Namespace[cluster] = fmt.Sprintf("%s-%s", operator.Namespace, strings.ToLower(random.UniqueId()))

	// Setup Single region infra.
	r.setupInfra(t)

	// Cleanup resources.
	defer r.CleanupResources(t)

	// Create CA certificate.
	err := r.CreateCACertificate(t)
	require.NoError(t, err)

	defer r.CleanUpCACertificate(t)

	// Install Operator and CRDB charts.
	r.InstallCharts(t, cluster, 0)

	// Get current context name.
	kubeConfig, rawConfig := r.GetCurrentContext(t)

	if _, ok := rawConfig.Contexts[cluster]; !ok {
		t.Fatal()
	}
	rawConfig.CurrentContext = cluster

	// Validate CockroachDB cluster.
	r.ValidateCRDB(t, cluster)

	// Get helm chart paths.
	helmChartPath, _ := operator.HelmChartPaths()

	var upgradeTime time.Time

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
	r.ValidateCRDB(t, cluster)
}

// TestKillingCockroachNode will manually kill one cockroachdb node to verify
// if the reconciliation is working as expected in single region and verifies the same.
func (r *singleRegion) TestKillingCockroachNode(t *testing.T) {
	cluster := r.Clusters[0]
	r.Namespace[cluster] = fmt.Sprintf("%s-%s", operator.Namespace, strings.ToLower(random.UniqueId()))

	// Setup Single region infra.
	r.setupInfra(t)

	// Cleanup resources.
	defer r.CleanupResources(t)

	// Create CA certificate.
	err := r.CreateCACertificate(t)
	require.NoError(t, err)

	defer r.CleanUpCACertificate(t)

	// Install Operator and CRDB charts.
	r.InstallCharts(t, cluster, 0)

	// Get current context name.
	kubeConfig, rawConfig := r.GetCurrentContext(t)

	// Validate CockroachDB cluster.
	if _, ok := rawConfig.Contexts[cluster]; !ok {
		t.Fatal()
	}
	rawConfig.CurrentContext = cluster

	// Validate CockroachDB cluster.
	r.ValidateCRDB(t, cluster)

	// List the pods.
	kubectlOptions := k8s.NewKubectlOptions(cluster, kubeConfig, r.Namespace[cluster])
	pods := k8s.ListPods(t, kubectlOptions, metav1.ListOptions{
		LabelSelector: operator.LabelSelector,
	})

	// Kill a node in the cluster.
	err = k8s.RunKubectlE(t, kubectlOptions, "delete", "pod", pods[0].Name)
	require.NoError(t, err)

	// Wait till the reconciliation is done and all the pods are up and running.
	crdbCluster := testutil.CockroachCluster{
		DesiredNodes: r.NodeCount,
	}
	testutil.RequireCRDBClusterToBeReadyEventuallyTimeout(t, kubectlOptions, crdbCluster, 600*time.Second)

	if _, ok := rawConfig.Contexts[cluster]; !ok {
		t.Fatal()
	}
	rawConfig.CurrentContext = cluster

	// Validate CockroachDB cluster.
	r.ValidateCRDB(t, cluster)
}

// TestClusterScaleUp will scale the CockroachDB nodes in the existing region
// and verifies the CockroachDB cluster health and replicas.
func (r *singleRegion) TestClusterScaleUp(t *testing.T) {
	cluster := r.Clusters[0]
	r.Namespace[cluster] = fmt.Sprintf("%s-%s", operator.Namespace, strings.ToLower(random.UniqueId()))

	// Setup Single region infra.
	r.setupInfra(t)

	// Cleanup resources.
	defer r.CleanupResources(t)

	// Create CA certificate.
	err := r.CreateCACertificate(t)
	require.NoError(t, err)

	defer r.CleanUpCACertificate(t)

	// Install Operator and CockroachDB charts.
	r.InstallCharts(t, cluster, 0)

	// Get current context name.
	kubeConfig, rawConfig := r.GetCurrentContext(t)
	if _, ok := rawConfig.Contexts[cluster]; !ok {
		t.Fatal()
	}
	rawConfig.CurrentContext = cluster

	// Validate CockroachDB cluster.
	r.ValidateCRDB(t, cluster)

	// Get helm chart paths.
	helmChartPath, _ := operator.HelmChartPaths()
	kubectlOptions := k8s.NewKubectlOptions(cluster, kubeConfig, r.Namespace[cluster])
	r.NodeCount += 1
	r.scaleNodePool(t)
	options := &helm.Options{
		KubectlOptions: kubectlOptions,
		SetJsonValues: map[string]string{
			"cockroachdb.crdbCluster.regions": operator.MustMarshalJSON(r.OperatorRegions(0, r.NodeCount)),
		},
		ExtraArgs: map[string][]string{
			"upgrade": {"--reuse-values", "--wait"},
		},
	}
	helm.Upgrade(t, options, helmChartPath, operator.ReleaseName)

	crdbCluster := testutil.CockroachCluster{
		DesiredNodes: r.NodeCount,
	}
	testutil.RequireCRDBClusterToBeReadyEventuallyTimeout(t, kubectlOptions, crdbCluster, 600*time.Second)
	r.ValidateCRDB(t, cluster)
}

// TestInstallWithCertManager will install the Operator and CockroachDB charts
// with cert-manager and trust-manager and verifies cockroachdb cluster is up and running.
func (r *singleRegion) TestInstallWithCertManager(t *testing.T) {
	cluster := r.Clusters[0]
	r.Namespace[cluster] = fmt.Sprintf("%s-%s", operator.Namespace, strings.ToLower(random.UniqueId()))
	r.IsCertManager = true

	// Cleanup resources.
	defer r.CleanupResources(t)

	// Setup Single region infra.
	r.setupInfra(t)

	// Install Operator and CockroachDB charts.
	r.InstallCharts(t, cluster, 0)

	// Get current context name.
	_, rawConfig := r.GetCurrentContext(t)

	if _, ok := rawConfig.Contexts[cluster]; !ok {
		t.Fatal()
	}
	rawConfig.CurrentContext = cluster
	r.ValidateCRDB(t, cluster)
}

func (r *singleRegion) setupInfra(t *testing.T) {
	// Create the appropriate provider using the factory
	provider := infra.ProviderFactory(r.Provider, &r.Region)

	// Set up the infrastructure
	if provider != nil {
		provider.SetUpInfra(t)
	} else {
		t.Fatalf("Unsupported provider: %s", r.Provider)
	}
}

func (r *singleRegion) tearDownInfra(t *testing.T, providers []string) {
	for _, providerType := range providers {
		// Create the appropriate provider using the factory
		r.Provider = providerType
		provider := infra.ProviderFactory(providerType, &r.Region)

		// Check if the provider supports teardown
		if teardownProvider, ok := infra.CanTeardown(provider); ok {
			// Tear down the infrastructure
			teardownProvider.TeardownInfra(t)
		} else {
			t.Logf("Provider %s does not support teardown or teardown is not implemented", providerType)
		}
	}
}

func (r *singleRegion) scaleNodePool(t *testing.T) {
	// Create the appropriate provider using the factory
	provider := infra.ProviderFactory(r.Provider, &r.Region)

	// Check if the provider supports scaling
	if scalableProvider, ok := infra.CanScale(provider); ok {
		// Scale the node pool
		scalableProvider.ScaleNodePool(t, r.RegionCodes[0], r.NodeCount, 0)
	} else {
		t.Logf("Provider %s does not support scaling", r.Provider)
	}
}
