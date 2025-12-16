package singleRegion

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

// Environment variable name to check if running in nightly mode
const isNightlyEnvVar = "isNightly"

type singleRegion struct {
	operator.OperatorUseCases
	operator.Region
}

func newSingleRegion() *singleRegion {
	return &singleRegion{}
}
func TestOperatorInSingleRegion(t *testing.T) {
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
		// Run tests for different providers in parallel.
		t.Parallel()

		// Create a provider-specific instance to avoid race conditions.
		providerRegion := newSingleRegion()
		providerRegion.Region = operator.Region{
			IsMultiRegion: false,
			NodeCount:     3,
			ReusingInfra:  false,
		}
		providerRegion.Clients = make(map[string]client.Client)
		providerRegion.Namespace = make(map[string]string)

		providerRegion.Provider = provider
		clusterName := fmt.Sprintf("%s-%s", providerRegion.Provider, operator.Clusters[0])
		if provider != infra.ProviderK3D && provider != infra.ProviderKind {
			clusterName = fmt.Sprintf("%s-%s", clusterName, strings.ToLower(random.UniqueId()))
		}
		providerRegion.Clusters = append(providerRegion.Clusters, clusterName)

		// Create and reuse the same provider instance for both setup and teardown.
		cloudProvider := infra.ProviderFactory(providerRegion.Provider, &providerRegion.Region)
		if cloudProvider == nil {
			t.Fatalf("Unsupported provider: %s", provider)
		}

		// Use t.Cleanup for guaranteed cleanup even on test timeout/panic
		t.Cleanup(func() {
			t.Logf("Starting infrastructure cleanup for provider: %s", provider)
			cloudProvider.TeardownInfra(t)
			t.Logf("Completed infrastructure cleanup for provider: %s", provider)
		})

		// Set up infrastructure for this provider once.
		cloudProvider.SetUpInfra(t)

		testCases := map[string]func(*testing.T){
			"TestHelmInstall":               providerRegion.TestHelmInstall,
			"TestHelmInstallVirtualCluster": providerRegion.TestHelmInstallVirtualCluster,
			"TestHelmUpgrade":               providerRegion.TestHelmUpgrade,
			"TestClusterRollingRestart":     providerRegion.TestClusterRollingRestart,
			"TestKillingCockroachNode":      providerRegion.TestKillingCockroachNode,
			"TestClusterScaleUp":            func(t *testing.T) { providerRegion.TestClusterScaleUp(t, cloudProvider) },
			"TestInstallWithCertManager":    providerRegion.TestInstallWithCertManager,
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

// TestHelmInstall will install Operator and CockroachDB charts
// and verifies if CockroachDB service is up and running.
func (r *singleRegion) TestHelmInstall(t *testing.T) {
	cluster := r.Clusters[0]
	r.Namespace[cluster] = fmt.Sprintf("%s-%s", operator.Namespace, strings.ToLower(random.UniqueId()))

	// Cleanup resources.
	defer r.CleanupResources(t)

	// Create CA certificate.
	err := r.CreateCACertificate(t)
	require.NoError(t, err)

	defer r.CleanUpCACertificate(t)

	// Install Operator and CockroachDB charts.
	r.InstallCharts(t, cluster, 0)

	// Get the current context name.
	_, rawConfig := r.GetCurrentContext(t)

	if _, ok := rawConfig.Contexts[cluster]; !ok {
		t.Fatalf("cluster context '%s' not found in kubeconfig", cluster)
	}
	rawConfig.CurrentContext = cluster
	r.ValidateCRDB(t, cluster)
}

// TestHelmInstallVirtualCluster installs Operator and CockroachDB charts
// for both primary and standby virtual clusters and verifies if
// CockroachDB service is up and running.
func (r *singleRegion) TestHelmInstallVirtualCluster(t *testing.T) {
	// Create CA certificate.
	err := r.CreateCACertificate(t)
	require.NoError(t, err)
	var (
		operatorNamespace, standByNamespace string
	)

	tests := []struct {
		name                string
		isPrimary           bool
		initCommand         string
		IsOperatorInstalled bool
	}{
		{
			name:                "primary",
			isPrimary:           true,
			initCommand:         "/cockroach/cockroach init --host :26258 --virtualized",
			IsOperatorInstalled: false,
		},
		{
			name:                "standby",
			isPrimary:           false,
			initCommand:         "/cockroach/cockroach init --host :26258 --virtualized-empty",
			IsOperatorInstalled: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r.IsOperatorInstalled = tt.IsOperatorInstalled
			cluster := r.Clusters[0]
			namespace := fmt.Sprintf("%s-%s", operator.Namespace, strings.ToLower(random.UniqueId()))
			r.Namespace[cluster] = namespace

			if tt.isPrimary {
				r.VirtualClusterModePrimary = true
				operatorNamespace = namespace
				defer func() { r.VirtualClusterModePrimary = false }()
			} else {
				r.VirtualClusterModeStandby = true
				standByNamespace = r.Namespace[cluster]
				defer func() {
					r.VirtualClusterModeStandby = false
					r.IsOperatorInstalled = false
				}()
			}

			// Install Operator and CockroachDB charts.
			r.InstallCharts(t, cluster, 0)

			// Get the current context name.
			kubeConfig, rawConfig := r.GetCurrentContext(t)

			if _, ok := rawConfig.Contexts[cluster]; !ok {
				t.Fatalf("cluster context '%s' not found in kubeconfig", cluster)
			}
			rawConfig.CurrentContext = cluster

			r.ValidateCRDB(t, cluster)

			kubectlOptions := k8s.NewKubectlOptions(cluster, kubeConfig, operatorNamespace)

			// Verify init command in logs
			operator.VerifyInitCommandInOperatorLogs(t, kubectlOptions, tt.initCommand)
			r.Namespace[cluster] = operatorNamespace
		})
	}
	defer r.CleanupResources(t)
	defer func() {
		kubectlOptions := k8s.NewKubectlOptions("", "", standByNamespace)
		k8s.DeleteNamespace(t, kubectlOptions, standByNamespace)
	}()
	defer r.CleanUpCACertificate(t)

}

// TestHelmUpgrade will upgrade the existing charts in a single region
// and verifies the CockroachDB health.
func (r *singleRegion) TestHelmUpgrade(t *testing.T) {
	cluster := r.Clusters[0]
	r.Namespace[cluster] = fmt.Sprintf("%s-%s", operator.Namespace, strings.ToLower(random.UniqueId()))

	// Cleanup resources.
	defer r.CleanupResources(t)

	// Create CA certificate.
	err := r.CreateCACertificate(t)
	require.NoError(t, err)

	defer r.CleanUpCACertificate(t)

	// Install Operator and CockroachDB charts.
	r.InstallCharts(t, cluster, 0)

	// Get the current context name.
	kubeConfig, rawConfig := r.GetCurrentContext(t)

	if _, ok := rawConfig.Contexts[cluster]; !ok {
		t.Fatalf("cluster context '%s' not found in kubeconfig", cluster)
	}
	rawConfig.CurrentContext = cluster

	// Validate CockroachDB cluster.
	r.ValidateCRDB(t, cluster)

	kubectlOptions := k8s.NewKubectlOptions(cluster, kubeConfig, r.Namespace[cluster])
	// Get the initial timestamp of the pods before the upgrade.
	pods := k8s.ListPods(t, kubectlOptions, metav1.ListOptions{
		LabelSelector: operator.LabelSelector,
	})

	// Capture the creation timestamp of the last pod.
	if len(pods) == 0 {
		require.Fail(t, "No pods found for deployment")
	}
	initialTimestamp := pods[len(pods)-1].CreationTimestamp.Time

	// Get helm chart paths.
	helmChartPath, _ := operator.HelmChartPaths()
	options := &helm.Options{
		KubectlOptions: kubectlOptions,
		ExtraArgs: map[string][]string{
			"upgrade": {
				"--reuse-values",
				"--set", "cockroachdb.crdbCluster.podTemplate.spec.containers[0].name=cockroachdb",
				"--set", fmt.Sprintf("cockroachdb.crdbCluster.podTemplate.spec.containers[0].resources.requests.cpu=%s", "100m"),
			},
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

// TestClusterRollingRestart will do a rolling restart by updating
// the timestamp of each cockroachdb pod in a single region through helm upgrade and verifies the same.
func (r *singleRegion) TestClusterRollingRestart(t *testing.T) {
	cluster := r.Clusters[0]
	r.Namespace[cluster] = fmt.Sprintf("%s-%s", operator.Namespace, strings.ToLower(random.UniqueId()))

	// Cleanup resources.
	defer r.CleanupResources(t)

	// Create CA certificate.
	err := r.CreateCACertificate(t)
	require.NoError(t, err)

	defer r.CleanUpCACertificate(t)

	// Install Operator and CRDB charts.
	r.InstallCharts(t, cluster, 0)

	// Get the current context name.
	kubeConfig, rawConfig := r.GetCurrentContext(t)

	if _, ok := rawConfig.Contexts[cluster]; !ok {
		t.Fatalf("cluster context '%s' not found in kubeconfig", cluster)
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
	if len(pods) < 3 {
		require.Fail(t, "Expected at least 3 pods but found %d", len(pods))
	}
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
		require.False(t, pod.CreationTimestamp.Time.Before(upgradeTime), "pod %s was not restarted after %v", pod.Name, upgradeTime)
	}
	r.ValidateCRDB(t, cluster)
}

// TestKillingCockroachNode will manually kill one cockroachdb node to verify
// if the reconciliation is working as expected in a single region and verifies the same.
func (r *singleRegion) TestKillingCockroachNode(t *testing.T) {
	cluster := r.Clusters[0]
	r.Namespace[cluster] = fmt.Sprintf("%s-%s", operator.Namespace, strings.ToLower(random.UniqueId()))

	// Cleanup resources.
	defer r.CleanupResources(t)

	// Create CA certificate.
	err := r.CreateCACertificate(t)
	require.NoError(t, err)

	defer r.CleanUpCACertificate(t)

	// Install Operator and CRDB charts.
	r.InstallCharts(t, cluster, 0)

	// Get the current context name.
	kubeConfig, rawConfig := r.GetCurrentContext(t)

	// Validate CockroachDB cluster.
	if _, ok := rawConfig.Contexts[cluster]; !ok {
		t.Fatalf("cluster context '%s' not found in kubeconfig", cluster)
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
		t.Fatalf("cluster context '%s' not found in kubeconfig", cluster)
	}
	rawConfig.CurrentContext = cluster

	// Validate CockroachDB cluster.
	r.ValidateCRDB(t, cluster)
}

// TestClusterScaleUp will scale the CockroachDB nodes in the existing region
// and verifies the CockroachDB cluster health and replicas using the provided cloudProvider.
func (r *singleRegion) TestClusterScaleUp(t *testing.T, cloudProvider infra.CloudProvider) {
	cluster := r.Clusters[0]
	r.Namespace[cluster] = fmt.Sprintf("%s-%s", operator.Namespace, strings.ToLower(random.UniqueId()))

	// Cleanup resources.
	defer r.CleanupResources(t)

	// Create CA certificate.
	err := r.CreateCACertificate(t)
	require.NoError(t, err)

	defer r.CleanUpCACertificate(t)

	// Install Operator and CockroachDB charts.
	r.InstallCharts(t, cluster, 0)

	// Get the current context name.
	kubeConfig, rawConfig := r.GetCurrentContext(t)
	if _, ok := rawConfig.Contexts[cluster]; !ok {
		t.Fatalf("cluster context '%s' not found in kubeconfig", cluster)
	}
	rawConfig.CurrentContext = cluster

	// Validate CockroachDB cluster.
	r.ValidateCRDB(t, cluster)

	// Get helm chart paths.
	helmChartPath, _ := operator.HelmChartPaths()
	kubectlOptions := k8s.NewKubectlOptions(cluster, kubeConfig, r.Namespace[cluster])
	r.NodeCount += 1

	// Check if scaling is supported by the cloud provider.
	if cloudProvider.CanScale() {
		t.Logf("Scaling node pool for provider: %s", r.Provider)
		cloudProvider.ScaleNodePool(t, r.RegionCodes[0], r.NodeCount, 0)
	} else {
		t.Logf("Provider %s does not support scaling", r.Provider)
	}

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
	defer func() {
		r.IsCertManager = false
	}()

	// Cleanup resources.
	defer r.CleanupResources(t)

	// Install Operator and CockroachDB charts.
	r.InstallCharts(t, cluster, 0)

	// Get the current context name.
	_, rawConfig := r.GetCurrentContext(t)

	if _, ok := rawConfig.Contexts[cluster]; !ok {
		t.Fatalf("cluster context '%s' not found in kubeconfig", cluster)
	}
	rawConfig.CurrentContext = cluster
	r.ValidateCRDB(t, cluster)

}
