package compatibility

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cockroachdb/helm-charts/tests/e2e/operator"
	"github.com/cockroachdb/helm-charts/tests/e2e/operator/infra"
	"github.com/cockroachdb/helm-charts/tests/testutil"
	"github.com/gruntwork-io/terratest/modules/helm"
	"github.com/gruntwork-io/terratest/modules/k8s"
	"github.com/gruntwork-io/terratest/modules/random"
	"github.com/gruntwork-io/terratest/modules/shell"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	compatOperatorVersionEnv    = "COMPAT_OPERATOR_CHART_VERSION"
	compatCockroachDBVersionEnv = "COMPAT_COCKROACHDB_CHART_VERSION"
	compatHelmRepoNameEnv       = "COMPAT_HELM_REPO_NAME"
	compatHelmRepoURLEnv        = "COMPAT_HELM_REPO_URL"

	defaultCompatHelmRepoName = "cockroachdb-v2-compat"
	defaultCompatHelmRepoURL  = "https://charts.cockroachdb.com/v2"
)

type compatibilityRegion struct {
	operator.Region
}

func TestChartCompatibilityUpgrade(t *testing.T) {
	operatorVersion := strings.TrimSpace(os.Getenv(compatOperatorVersionEnv))
	cockroachDBVersion := strings.TrimSpace(os.Getenv(compatCockroachDBVersionEnv))
	if operatorVersion == "" || cockroachDBVersion == "" {
		t.Skipf("%s and %s must be set to run chart compatibility e2e tests; CI sets these from the PR base chart versions", compatOperatorVersionEnv, compatCockroachDBVersionEnv)
	}

	provider := compatProvider(t)
	r := newCompatibilityRegion(provider)
	cloudProvider := infra.ProviderFactory(provider, &r.Region)
	if cloudProvider == nil {
		t.Fatalf("unsupported provider: %s", provider)
	}

	t.Cleanup(func() {
		t.Logf("Starting infrastructure cleanup for provider: %s", provider)
		cloudProvider.TeardownInfra(t)
		t.Logf("Completed infrastructure cleanup for provider: %s", provider)
	})

	cloudProvider.SetUpInfra(t)

	cluster := r.Clusters[0]
	r.Namespace[cluster] = fmt.Sprintf("%s-%s", operator.Namespace, strings.ToLower(random.UniqueId()))

	defer r.CleanupResources(t)

	err := r.CreateCACertificate(t)
	require.NoError(t, err)
	defer r.CleanUpCACertificate(t)

	kubeConfig, rawConfig := r.GetCurrentContext(t)
	if _, ok := rawConfig.Contexts[cluster]; !ok {
		t.Fatalf("cluster context %q not found in kubeconfig", cluster)
	}
	rawConfig.CurrentContext = cluster

	kubectlOptions := k8s.NewKubectlOptions(cluster, kubeConfig, r.Namespace[cluster])
	k8s.CreateNamespace(t, kubectlOptions, r.Namespace[cluster])
	err = k8s.RunKubectlE(t, kubectlOptions, "create", "secret", "generic", "cockroachdb-ca-secret",
		"--from-file=ca.crt", "--from-file=ca.key")
	require.NoError(t, err)

	repoName := addCompatibilityHelmRepo(t)
	defer helm.RemoveRepo(t, &helm.Options{}, repoName)

	t.Logf("Installing published operator chart %s", operatorVersion)
	operator.InstallCockroachDBOperatorChart(t, kubectlOptions,
		fmt.Sprintf("%s/cockroachdb-operator-chart", repoName),
		operatorVersion,
		map[string]string{"numReplicas": "1"},
	)

	t.Logf("Installing published CockroachDB chart %s", cockroachDBVersion)
	installCockroachDBChart(t, &r.Region, kubectlOptions,
		fmt.Sprintf("%s/cockroachdb-chart", repoName),
		cockroachDBVersion,
	)
	r.ValidateCRDB(t, cluster)

	helmChartPath, operatorChartPath := operator.HelmChartPaths()

	t.Log("Upgrading operator chart from the local checkout")
	operator.UpgradeCockroachDBOperatorChart(t, kubectlOptions, operatorChartPath, compatibilityOperatorUpgradeValues(t, operatorChartPath))
	requireOperatorMigrationFlag(t, kubectlOptions, false)

	t.Log("Upgrading CockroachDB chart from the local checkout")
	upgradeCockroachDBChart(t, kubectlOptions, helmChartPath)

	testutil.RequireCRDBClusterToBeReadyEventuallyTimeout(t, kubectlOptions, testutil.CockroachCluster{
		DesiredNodes: r.NodeCount,
	}, 600*time.Second)
	r.ValidateCRDB(t, cluster)

	t.Log("Exercising release-specific migration and split-chart node-reader settings")
	nodeReaderName := fmt.Sprintf("compat-node-reader-%s", r.Namespace[cluster])
	operator.UpgradeCockroachDBOperatorChart(
		t,
		kubectlOptions,
		operatorChartPath,
		compatibilityOperatorMigrationAndNodeReaderValues(t, operatorChartPath, r.Namespace[cluster], nodeReaderName),
	)
	requireOperatorMigrationFlag(t, kubectlOptions, true)

	upgradeCockroachDBChart(t, kubectlOptions, helmChartPath, map[string]string{
		"cockroachdb.crdbCluster.rbac.nodeReader.create": "false",
	})
	requireNodeReaderDelegatedToOperatorChart(t, kubectlOptions, nodeReaderName)
	testutil.RequireCRDBClusterToBeReadyEventuallyTimeout(t, kubectlOptions, testutil.CockroachCluster{
		DesiredNodes: r.NodeCount,
	}, 600*time.Second)
	r.ValidateCRDB(t, cluster)
}

func newCompatibilityRegion(provider string) *compatibilityRegion {
	clusterName := fmt.Sprintf("%s-%s", provider, operator.Clusters[0])
	if provider != infra.ProviderK3D && provider != infra.ProviderKind {
		clusterName = fmt.Sprintf("%s-%s", clusterName, strings.ToLower(random.UniqueId()))
	}

	return &compatibilityRegion{
		Region: operator.Region{
			NodeCount:    3,
			ReusingInfra: false,
			Clients:      make(map[string]client.Client),
			Namespace:    make(map[string]string),
			Provider:     provider,
			Clusters:     []string{clusterName},
		},
	}
}

func compatProvider(t *testing.T) string {
	switch p := strings.TrimSpace(strings.ToLower(os.Getenv("PROVIDER"))); p {
	case "":
		return infra.ProviderK3D
	case infra.ProviderK3D, infra.ProviderKind, infra.ProviderGCP:
		return p
	default:
		t.Fatalf("unsupported provider override: %s", p)
		return ""
	}
}

func addCompatibilityHelmRepo(t *testing.T) string {
	repoName := strings.TrimSpace(os.Getenv(compatHelmRepoNameEnv))
	if repoName == "" {
		repoName = defaultCompatHelmRepoName
	}
	repoURL := strings.TrimSpace(os.Getenv(compatHelmRepoURLEnv))
	if repoURL == "" {
		repoURL = defaultCompatHelmRepoURL
	}

	options := &helm.Options{
		ExtraArgs: map[string][]string{
			"repoAdd": {"--force-update"},
		},
	}
	helm.AddRepo(t, options, repoName, repoURL)

	_, err := shell.RunCommandAndGetOutputE(t, shell.Command{
		Command: "helm",
		Args:    []string{"repo", "update", repoName},
	})
	require.NoError(t, err)

	return repoName
}

func compatibilityOperatorUpgradeValues(t *testing.T, operatorChartPath string) map[string]string {
	values := map[string]string{
		"numReplicas": "1",
	}
	for key, value := range localOperatorImageValues(t, operatorChartPath) {
		values[key] = value
	}
	return values
}

type operatorImageValues struct {
	Image struct {
		Registry   string `yaml:"registry"`
		Repository string `yaml:"repository"`
		Tag        string `yaml:"tag"`
	} `yaml:"image"`
}

func localOperatorImageValues(t *testing.T, operatorChartPath string) map[string]string {
	t.Helper()

	valuesBytes, err := os.ReadFile(filepath.Join(operatorChartPath, "values.yaml"))
	require.NoError(t, err)

	var values operatorImageValues
	require.NoError(t, yaml.Unmarshal(valuesBytes, &values))
	require.NotEmpty(t, values.Image.Registry)
	require.NotEmpty(t, values.Image.Repository)
	require.NotEmpty(t, values.Image.Tag)

	return map[string]string{
		"image.registry":   values.Image.Registry,
		"image.repository": values.Image.Repository,
		"image.tag":        values.Image.Tag,
	}
}

func compatibilityOperatorMigrationAndNodeReaderValues(
	t *testing.T, operatorChartPath, namespace, nodeReaderName string,
) map[string]string {
	values := compatibilityOperatorUpgradeValues(t, operatorChartPath)
	values["migration.enabled"] = "true"
	values["nodeReader.enabled"] = "true"
	values["nodeReader.name"] = nodeReaderName
	values["nodeReader.subjects[0].namespace"] = namespace
	values["nodeReader.subjects[0].serviceAccountName"] = operator.ReleaseName
	return values
}

func installCockroachDBChart(
	t *testing.T, r *operator.Region, kubectlOptions *k8s.KubectlOptions, chart, version string,
) {
	options := &helm.Options{
		KubectlOptions: kubectlOptions,
		Version:        version,
		SetValues:      cockroachDBCompatibilityValues(),
		SetJsonValues: map[string]string{
			"cockroachdb.crdbCluster.regions": operator.MustMarshalJSON(r.OperatorRegions(0, r.NodeCount)),
		},
		ExtraArgs: map[string][]string{
			"install": {
				"--wait",
				"--debug",
			},
		},
	}

	helm.Install(t, options, chart, operator.ReleaseName)
	k8s.WaitUntilServiceAvailable(t, kubectlOptions, "cockroachdb-public", 30, 5*time.Second)
}

func upgradeCockroachDBChart(
	t *testing.T, kubectlOptions *k8s.KubectlOptions, chart string, overrides ...map[string]string,
) {
	values := cockroachDBCompatibilityValues()
	for _, override := range overrides {
		for key, value := range override {
			values[key] = value
		}
	}

	options := &helm.Options{
		KubectlOptions: kubectlOptions,
		SetValues:      values,
		ExtraArgs: map[string][]string{
			"upgrade": {
				"--reuse-values",
				"--wait",
				"--debug",
			},
		},
	}

	helm.Upgrade(t, options, chart, operator.ReleaseName)
	k8s.WaitUntilServiceAvailable(t, kubectlOptions, "cockroachdb-public", 30, 5*time.Second)
}

func requireOperatorMigrationFlag(t *testing.T, kubectlOptions *k8s.KubectlOptions, expected bool) {
	t.Helper()

	args, err := k8s.RunKubectlAndGetOutputE(t, kubectlOptions,
		"get", "deployment", "cockroach-operator",
		"-o", `jsonpath={.spec.template.spec.containers[?(@.name=="cockroach-operator")].args}`,
	)
	require.NoError(t, err)
	if expected {
		require.Contains(t, args, "--enable-migration")
	} else {
		require.NotContains(t, args, "--enable-migration")
	}
}

func requireNodeReaderDelegatedToOperatorChart(
	t *testing.T, kubectlOptions *k8s.KubectlOptions, nodeReaderName string,
) {
	t.Helper()

	k8s.RunKubectl(t, kubectlOptions, "get", "clusterrole", nodeReaderName)
	k8s.RunKubectl(t, kubectlOptions, "get", "clusterrolebinding", nodeReaderName)

	crdbChartNodeReaderName := fmt.Sprintf("%s-%s-node-reader", operator.ReleaseName, kubectlOptions.Namespace)
	_, err := k8s.RunKubectlAndGetOutputE(t, kubectlOptions, "get", "clusterrole", crdbChartNodeReaderName)
	require.Error(t, err, "cockroachdb chart-owned node-reader ClusterRole should be removed")
	_, err = k8s.RunKubectlAndGetOutputE(t, kubectlOptions, "get", "clusterrolebinding", crdbChartNodeReaderName)
	require.Error(t, err, "cockroachdb chart-owned node-reader ClusterRoleBinding should be removed")

	canReadNodes, err := k8s.RunKubectlAndGetOutputE(t, kubectlOptions,
		"auth", "can-i", "get", "nodes",
		fmt.Sprintf("--as=system:serviceaccount:%s:%s", kubectlOptions.Namespace, operator.ReleaseName),
	)
	require.NoError(t, err)
	lines := strings.Split(strings.TrimSpace(canReadNodes), "\n")
	require.Equal(t, "yes", strings.TrimSpace(lines[len(lines)-1]))
}

func cockroachDBCompatibilityValues() map[string]string {
	return operator.PatchHelmValues(map[string]string{
		"cockroachdb.clusterDomain":             operator.CustomDomains[0],
		"cockroachdb.tls.enabled":               "true",
		"cockroachdb.tls.selfSigner.caProvided": "true",
		"cockroachdb.tls.selfSigner.caSecret":   "cockroachdb-ca-secret",
	})
}
