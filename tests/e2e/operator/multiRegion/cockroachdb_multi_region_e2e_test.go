package multiRegion

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/cockroachdb/helm-charts/tests/e2e/coredns"
	"github.com/cockroachdb/helm-charts/tests/e2e/operator"
	"github.com/cockroachdb/helm-charts/tests/testutil"
	"github.com/gruntwork-io/terratest/modules/helm"
	"github.com/gruntwork-io/terratest/modules/k8s"
	"github.com/gruntwork-io/terratest/modules/random"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type multiRegion struct {
	operator.OperatorUseCases
	operator.Region
}

func newMultiRegion() *multiRegion {
	return &multiRegion{}
}
func TestOperatorInMultiRegion(t *testing.T) {
	r := newMultiRegion()
	r.Region = operator.Region{
		IsMultiRegion: true,
		NodeCount:     3,
		ReusingInfra:  false,
	}
	r.Clients = make(map[string]client.Client)
	r.Namespace = make(map[string]string)
	t.Run("TestHelmInstall", r.TestHelmInstall)
	t.Run("TestHelmUpgrade", r.TestHelmUpgrade)
	t.Run("TestClusterRollingRestart", r.TestClusterRollingRestart)
	t.Run("TestKillingCockroachNode", r.TestKillingCockroachNode)
	t.Run("TestClusterScaleUp", r.TestClusterScaleUp)
}

// TestHelmInstall will install Operator and CockroachDB charts in multiple regions,
// and verifies if CockroachDB has formed multi-region cluster.
func (r *multiRegion) TestHelmInstall(t *testing.T) {
	var corednsClusterOptions = make(map[string]coredns.CoreDNSClusterOption)

	// Set up multi-region k3d infra.
	r.SetUpInfra(t, corednsClusterOptions)

	var clusters = operator.Clusters[:len(r.Clients)]

	// Creating random namespace for each region.
	for _, cluster := range clusters {
		r.Namespace[cluster] = fmt.Sprintf("%s-%s", operator.Namespace, strings.ToLower(random.UniqueId()))
	}

	// Cleanup resources.
	defer r.CleanupResources(t)

	// Create CA certificate.
	err := r.CreateCACertificate(t)
	require.NoError(t, err)

	defer r.CleanUpCACertificate(t)

	// Apply operator, CockroachDB charts on each cluster.
	for i, cluster := range clusters {
		r.InstallCharts(t, cluster, i)
	}

	// Get current context name.
	_, rawConfig := r.GetCurrentContext(t)

	// Validate CockroachDB functionality in each cluster.
	for _, cluster := range clusters {
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
	var corednsClusterOptions = make(map[string]coredns.CoreDNSClusterOption)

	// Set up multi-region k3d infra.
	r.SetUpInfra(t, corednsClusterOptions)

	var clusters = operator.Clusters[:len(r.Clients)]

	// Creating random namespace for each region.
	for _, cluster := range clusters {
		r.Namespace[cluster] = fmt.Sprintf("%s-%s", operator.Namespace, strings.ToLower(random.UniqueId()))
	}

	// Cleanup resources.
	defer r.CleanupResources(t)

	// Create CA certificate.
	err := r.CreateCACertificate(t)
	require.NoError(t, err)

	defer r.CleanUpCACertificate(t)

	// Apply operator, CockroachDB charts on each cluster.
	for i, cluster := range clusters {
		r.InstallCharts(t, cluster, i)
	}

	// Get current context name.
	kubeConfig, rawConfig := r.GetCurrentContext(t)

	// Validate CockroachDB functionality in each cluster.
	for _, cluster := range clusters {
		if _, ok := rawConfig.Contexts[cluster]; !ok {
			t.Fatal()
		}
		rawConfig.CurrentContext = cluster
		// Validate CockroachDB cluster.
		r.ValidateCRDB(t, cluster)
	}

	// Get helm chart paths.
	helmChartPath, _ := operator.HelmChartPaths()
	for _, cluster := range clusters {
		kubectlOptions := k8s.NewKubectlOptions(cluster, kubeConfig, r.Namespace[cluster])
		// Get the initial timestamp of the pods before the upgrade.
		pods := k8s.ListPods(t, kubectlOptions, metav1.ListOptions{
			LabelSelector: operator.LabelSelector,
		})

		// Capture the creation timestamp of the first pod.
		initialTimestamp := pods[0].CreationTimestamp.Time
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
		pods = k8s.ListPods(t, kubectlOptions, metav1.ListOptions{
			LabelSelector: operator.LabelSelector,
		})
		for _, pod := range pods {
			containers := pod.Spec.Containers
			for _, container := range containers {
				if container.Name == operator.CockroachContainerName {
					require.Equal(t, resource.MustParse("100m"), container.Resources.Requests["cpu"])
				}
			}
		}
		r.ValidateCRDB(t, cluster)
	}
	r.ValidateMultiRegionSetup(t)
}

// TestClusterRollingRestart will do a rolling restart by updating
// timestamp of each cockroachdb pod in multi region through helm upgrade and verifies the same.
func (r *multiRegion) TestClusterRollingRestart(t *testing.T) {
	var corednsClusterOptions = make(map[string]coredns.CoreDNSClusterOption)

	// Set up multi-region k3d infra.
	r.SetUpInfra(t, corednsClusterOptions)

	var clusters = operator.Clusters[:len(r.Clients)]

	// Creating random namespace for each region.
	for _, cluster := range clusters {
		r.Namespace[cluster] = fmt.Sprintf("%s-%s", operator.Namespace, strings.ToLower(random.UniqueId()))
	}

	// Cleanup resources.
	defer r.CleanupResources(t)

	// Create CA certificate.
	err := r.CreateCACertificate(t)
	require.NoError(t, err)

	defer r.CleanUpCACertificate(t)

	// Apply operator, CockroachDB charts on each cluster.
	for i, cluster := range clusters {
		r.InstallCharts(t, cluster, i)
	}

	// Get current context name.
	kubeConfig, rawConfig := r.GetCurrentContext(t)

	// Validate CockroachDB functionality in each cluster.
	for _, cluster := range clusters {
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
	for _, cluster := range clusters {
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
	var corednsClusterOptions = make(map[string]coredns.CoreDNSClusterOption)

	// Set up multi-region k3d infra.
	r.SetUpInfra(t, corednsClusterOptions)

	var clusters = operator.Clusters[:len(r.Clients)]

	// Creating random namespace for each region.
	for _, cluster := range clusters {
		r.Namespace[cluster] = fmt.Sprintf("%s-%s", operator.Namespace, strings.ToLower(random.UniqueId()))
	}

	// Cleanup resources.
	defer r.CleanupResources(t)

	// Create CA certificate.
	err := r.CreateCACertificate(t)
	require.NoError(t, err)

	defer r.CleanUpCACertificate(t)

	// Apply operator, CockroachDB charts on each cluster.
	for i, cluster := range clusters {
		r.InstallCharts(t, cluster, i)
	}

	// Get current context name.
	kubeConfig, rawConfig := r.GetCurrentContext(t)

	// Validate CockroachDB functionality in each cluster.
	for _, cluster := range clusters {
		if _, ok := rawConfig.Contexts[cluster]; !ok {
			t.Fatal()
		}
		rawConfig.CurrentContext = cluster
		r.ValidateCRDB(t, cluster)
	}

	// Kill a pod in each cluster and verify the reconciliation
	for _, cluster := range clusters {
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
	var corednsClusterOptions = make(map[string]coredns.CoreDNSClusterOption)

	// Set up multi-region k3d infra.
	r.SetUpInfra(t, corednsClusterOptions)

	var clusters = operator.Clusters[:len(r.Clients)]

	// Creating random namespace for each region.
	for _, cluster := range clusters {
		r.Namespace[cluster] = fmt.Sprintf("%s-%s", operator.Namespace, strings.ToLower(random.UniqueId()))
	}

	// Cleanup resources.
	defer r.CleanupResources(t)

	// Create CA certificate.
	err := r.CreateCACertificate(t)
	require.NoError(t, err)

	defer r.CleanUpCACertificate(t)

	// Apply Operator, CockroachDB charts on each cluster.
	for i, cluster := range clusters {
		r.InstallCharts(t, cluster, i)
	}

	// Get current context name.
	kubeConfig, rawConfig := r.GetCurrentContext(t)

	// Validate CockroachDB functionality in each cluster.
	for _, cluster := range clusters {
		if _, ok := rawConfig.Contexts[cluster]; !ok {
			t.Fatal()
		}
		rawConfig.CurrentContext = cluster

		r.ValidateCRDB(t, cluster)
	}
	// Get helm chart paths.
	helmChartPath, _ := operator.HelmChartPaths()

	// Modify the nodes in each region and apply helm upgrade.
	for i, cluster := range clusters {
		kubectlOptions := k8s.NewKubectlOptions(cluster, kubeConfig, r.Namespace[cluster])
		r.NodeCount = 4
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
		testutil.RequireCRDBClusterToBeReadyEventuallyTimeout(t, kubectlOptions, crdbCluster, 600*time.Second)
		pods := k8s.ListPods(t, kubectlOptions, metav1.ListOptions{
			LabelSelector: operator.LabelSelector,
		})
		require.True(t, len(pods) == 4)
		// Validate CockroachDB cluster.
		r.ValidateCRDB(t, cluster)
	}
	r.ValidateMultiRegionSetup(t)
}
