package singleRegion

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
	t.Run("TestHelmInstall", r.TestHelmInstall)
	t.Run("TestHelmUpgrade", r.TestHelmUpgrade)
	t.Run("TestClusterRollingRestart", r.TestClusterRollingRestart)
	t.Run("TestKillingCockroachNode", r.TestKillingCockroachNode)
	t.Run("TestClusterScaleUp", r.TestClusterScaleUp)
	t.Run("TestInstallWithCertManager", r.TestInstallWithCertManager)
}

// TestHelmInstall will install Operator and CockroachDB charts
// and verifies if CockroachDB service is up and running.
func (r *singleRegion) TestHelmInstall(t *testing.T) {
	var corednsClusterOptions = make(map[string]coredns.CoreDNSClusterOption)
	cluster := operator.Clusters[0]
	r.Namespace[cluster] = fmt.Sprintf("%s-%s", operator.Namespace, strings.ToLower(random.UniqueId()))

	// Cleanup resources.
	defer r.CleanupResources(t)

	// Create CA certificate.
	err := r.CreateCACertificate(t)
	require.NoError(t, err)

	defer r.CleanUpCACertificate(t)

	// Setup Single region k3d infra.
	r.SetUpInfra(t, corednsClusterOptions)

	// Install Operator and CockroachDB charts.
	r.InstallCharts(t, cluster, 0, nil)

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
	var corednsClusterOptions = make(map[string]coredns.CoreDNSClusterOption)
	cluster := operator.Clusters[0]
	r.Namespace[cluster] = fmt.Sprintf("%s-%s", operator.Namespace, strings.ToLower(random.UniqueId()))

	// Cleanup resources.
	defer r.CleanupResources(t)

	// Create CA certificate.
	err := r.CreateCACertificate(t)
	require.NoError(t, err)

	defer r.CleanUpCACertificate(t)

	// Setup Single region k3d infra.
	r.SetUpInfra(t, corednsClusterOptions)

	// Install Operator and CockroachDB charts.
	r.InstallCharts(t, cluster, 0, nil)

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
	var corednsClusterOptions = make(map[string]coredns.CoreDNSClusterOption)
	cluster := operator.Clusters[0]
	r.Namespace[cluster] = fmt.Sprintf("%s-%s", operator.Namespace, strings.ToLower(random.UniqueId()))

	// Cleanup resources.
	defer r.CleanupResources(t)

	// Create CA certificate.
	err := r.CreateCACertificate(t)
	require.NoError(t, err)

	defer r.CleanUpCACertificate(t)

	//Setup Single region k3d infra.
	r.SetUpInfra(t, corednsClusterOptions)

	// Install Operator and CRDB charts.
	r.InstallCharts(t, cluster, 0, nil)

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
	var corednsClusterOptions = make(map[string]coredns.CoreDNSClusterOption)
	cluster := operator.Clusters[0]
	r.Namespace[cluster] = fmt.Sprintf("%s-%s", operator.Namespace, strings.ToLower(random.UniqueId()))

	// Cleanup resources.
	defer r.CleanupResources(t)

	//Setup Single region k3d infra.
	r.SetUpInfra(t, corednsClusterOptions)

	// Create CA certificate.
	err := r.CreateCACertificate(t)
	require.NoError(t, err)

	defer r.CleanUpCACertificate(t)

	// Install Operator and CRDB charts.
	r.InstallCharts(t, cluster, 0, nil)

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
	var corednsClusterOptions = make(map[string]coredns.CoreDNSClusterOption)
	cluster := operator.Clusters[0]
	r.Namespace[cluster] = fmt.Sprintf("%s-%s", operator.Namespace, strings.ToLower(random.UniqueId()))

	// Cleanup resources.
	defer r.CleanupResources(t)

	//Setup Single region k3d infra.
	r.SetUpInfra(t, corednsClusterOptions)

	// Create CA certificate.
	err := r.CreateCACertificate(t)
	require.NoError(t, err)

	defer r.CleanUpCACertificate(t)

	// Install Operator and CockroachDB charts.
	r.InstallCharts(t, cluster, 0, nil)

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
	r.NodeCount = 4
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

func (r *singleRegion) TestInstallWithCertManager(t *testing.T) {
	var corednsClusterOptions = make(map[string]coredns.CoreDNSClusterOption)
	cluster := operator.Clusters[0]
	r.Namespace[cluster] = fmt.Sprintf("%s-%s", operator.Namespace, strings.ToLower(random.UniqueId()))

	// Cleanup resources.
	defer r.CleanupResources(t)

	r.SetUpInfra(t, corednsClusterOptions)

	testutil.InstallCertManager(t)
	testutil.InstallTrustManager(t, r.Namespace[cluster])
	testutil.CreateSelfSignedIssuer(t, r.Namespace[cluster])
	testutil.CreateSelfSignedCertificate(t, r.Namespace[cluster])
	testutil.CreateCAIssuer(t, r.Namespace[cluster])
	testutil.CreateBundle(t, r.Namespace[cluster])
	defer func() {
		testutil.DeleteBundle(t, r.Namespace[cluster])
		testutil.DeleteCAIssuer(t, r.Namespace[cluster])
		testutil.DeleteSelfSignedCertificate(t, r.Namespace[cluster])
		testutil.DeleteSelfSignedIssuer(t, r.Namespace[cluster])
		testutil.DeleteTrustManager(t)
		testutil.DeleteCertManager(t)
	}()

	setValues := map[string]string{
		"cockroachdb.clusterDomain": operator.CustomDomains[cluster],
		"cockroachdb.tls.enabled": "true",
		"cockroachdb.tls.selfSigner.enabled": "false",
		"cockroachdb.tls.certManager.enabled": "true",
		"cockroachdb.tls.certManager.issuer.name": testutil.CAIssuerName,
		"cockroachdb.tls.certManager.caConfigMap": testutil.CAConfigMapName,
	}
	
	// Install Operator and CockroachDB charts.
	r.InstallCharts(t, cluster, 0, setValues)

	// Get current context name.
	_, rawConfig := r.GetCurrentContext(t)

	if _, ok := rawConfig.Contexts[cluster]; !ok {
		t.Fatal()
	}
	rawConfig.CurrentContext = cluster
	r.ValidateCRDB(t, cluster)

}