package migrate

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	api "github.com/cockroachdb/cockroach-operator/apis/v1alpha1"
	"github.com/cockroachdb/helm-charts/tests/e2e/operator"
	"github.com/cockroachdb/helm-charts/tests/testutil"
	"github.com/cockroachdb/helm-charts/tests/testutil/migration"
	"github.com/gruntwork-io/terratest/modules/helm"
	"github.com/gruntwork-io/terratest/modules/k8s"
	"github.com/gruntwork-io/terratest/modules/random"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime"
	kscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var (
	cfg                 *rest.Config
	k8sClient           client.Client
	releaseName         = "crdb-test"
	k3dClusterName      = "k3d-chart-testing-cluster"
	rootPath            = testutil.GetGitRoot()
	manifestsDirPath    = filepath.Join(rootPath, "manifests")
	migrationHelperPath = filepath.Join(rootPath, "bin", "migration-helper")
	ClientSecret        = fmt.Sprintf("%s-cockroachdb-client-secret", releaseName)
	NodeSecret          = fmt.Sprintf("%s-cockroachdb-node-secret", releaseName)
	CASecret            = fmt.Sprintf("%s-cockroachdb-ca-secret", releaseName)
	isCaUserProvided    = false
	migrationEnvOnce    sync.Once
	migrationEnvErr     error
)

var (
	migrationSuiteResources = []string{
		"crdbclusters.v1alpha1.crdb.cockroachlabs.com",
		"crdbclusters.v1beta1.crdb.cockroachlabs.com",
		"crdbnodes.v1beta1.crdb.cockroachlabs.com",
		"crdbtenants.v1beta1.crdb.cockroachlabs.com",
	}
	migrationSuiteCRDs = []string{
		"crdbclusters.crdb.cockroachlabs.com",
		"crdbnodes.crdb.cockroachlabs.com",
		"crdbtenants.crdb.cockroachlabs.com",
	}
)

type HelmChartToOperator struct {
	migration.HelmInstall
}

func newHelmChartToOperator() *HelmChartToOperator {
	return &HelmChartToOperator{}
}

func ensureMigrationTestEnv(t *testing.T) {
	t.Helper()

	migrationEnvOnce.Do(func() {
		migrationEnvErr = ensureMigrationHelperBinary()
		if migrationEnvErr != nil {
			return
		}
		migrationEnvErr = ensureK3DCluster()
		if migrationEnvErr != nil {
			return
		}
		migrationEnvErr = cleanupMigrationSuiteState(t)
		if migrationEnvErr != nil {
			return
		}
		migrationEnvErr = initMigrationClient()
	})

	require.NoError(t, migrationEnvErr)
}

func ensureMigrationHelperBinary() error {
	cmd := exec.Command("make", "bin/migration-helper")
	cmd.Dir = rootPath
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func cleanupMigrationSuiteState(t *testing.T) error {
	t.Helper()
	t.Log("Cleaning migrate CRDs and custom resources")

	kubectlPath := filepath.Join(rootPath, "bin", "kubectl")
	if _, err := os.Stat(kubectlPath); err != nil {
		return err
	}

	for _, resource := range migrationSuiteResources {
		listCmd := exec.Command(kubectlPath, "get", resource, "--all-namespaces",
			"-o", "jsonpath={range .items[*]}{.metadata.namespace}{\"\\t\"}{.metadata.name}{\"\\n\"}{end}")
		listCmd.Dir = rootPath
		output, err := listCmd.Output()
		if err != nil {
			continue
		}

		for _, line := range strings.Split(strings.TrimSpace(string(output)), "\n") {
			if line == "" {
				continue
			}
			parts := strings.Split(line, "\t")
			if len(parts) != 2 {
				continue
			}

			patchCmd := exec.Command(kubectlPath, "patch", resource, parts[1], "-n", parts[0],
				"--type=merge", "-p", "{\"metadata\":{\"finalizers\":[]}}")
			patchCmd.Dir = rootPath
			_, _ = patchCmd.CombinedOutput()

			deleteCmd := exec.Command(kubectlPath, "delete", resource, parts[1], "-n", parts[0],
				"--ignore-not-found=true", "--wait=true")
			deleteCmd.Dir = rootPath
			_, _ = deleteCmd.CombinedOutput()
		}
	}

	for _, crd := range migrationSuiteCRDs {
		deleteCmd := exec.Command(kubectlPath, "delete", "crd", crd, "--ignore-not-found=true", "--wait=true")
		deleteCmd.Dir = rootPath
		_, _ = deleteCmd.CombinedOutput()
	}

	return nil
}

func ensureK3DCluster() error {
	k3dPath := filepath.Join(rootPath, "bin", "k3d")
	expectedClusterName := strings.TrimPrefix(k3dClusterName, "k3d-")

	if _, err := os.Stat(k3dPath); err == nil {
		cmd := exec.Command(k3dPath, "cluster", "list", "--output", "name")
		cmd.Dir = rootPath
		output, err := cmd.Output()
		if err == nil && strings.Contains(string(output), expectedClusterName) {
			return nil
		}
	}

	cmd := exec.Command("make", "test/cluster/up/3")
	cmd.Dir = rootPath
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func initMigrationClient() error {
	var err error
	runtimeScheme := runtime.NewScheme()
	_ = kscheme.AddToScheme(runtimeScheme)
	_ = api.AddToScheme(runtimeScheme)
	cfg, err = ctrl.GetConfig()
	if err != nil {
		return err
	}
	k8sClient, err = client.New(cfg, client.Options{
		Scheme: runtimeScheme,
	})
	return err
}

func TestHelmChartToOperatorMigration(t *testing.T) {
	ensureMigrationTestEnv(t)

	h := newHelmChartToOperator()
	t.Run("helm chart to CockroachDB operator migration", h.TestDefaultMigration)
	t.Run("helm chart to CockroachDB operator migration with cert manager", h.TestCertManagerMigration)
	t.Run("helm chart to CockroachDB operator migration with PCR primary", h.TestPCRPrimaryMigration)
	t.Run("helm chart to CockroachDB operator migration with dedicated logs PVC", h.TestDedicatedLogsPVCMigration)
}

func (h *HelmChartToOperator) TestDefaultMigration(t *testing.T) {
	h.Namespace = "cockroach" + strings.ToLower(random.UniqueId())
	h.CrdbCluster = testutil.CockroachCluster{
		Cfg:              cfg,
		K8sClient:        k8sClient,
		StatefulSetName:  fmt.Sprintf("%s-cockroachdb", releaseName),
		Namespace:        h.Namespace,
		ClientSecret:     ClientSecret,
		NodeSecret:       NodeSecret,
		CaSecret:         CASecret,
		IsCaUserProvided: isCaUserProvided,
		Context:          k3dClusterName,
		DesiredNodes:     3,
	}
	h.HelmOptions = &helm.Options{
		SetValues: testutil.PatchHelmValues(map[string]string{
			"operator.enabled":                           "false",
			"conf.cluster-name":                          "test",
			"init.provisioning.enabled":                  "true",
			"init.provisioning.databases[0].name":        migration.TestDBName,
			"init.provisioning.databases[0].owners[0]":   "root",
			"statefulset.labels.app":                     "cockroachdb",
			"conf.locality":                              "topology.kubernetes.io/region=us-east-1",
			"storage.PersistentVolume.enabled":           "true",
			"conf.log.persistentVolume.enabled":          "false",
			"conf.wal-failover.value":                    "path=/cockroach/wal-failover",
			"conf.wal-failover.persistentVolume.enabled": "true",
			"conf.wal-failover.persistentVolume.path":    "wal-failover",
			"conf.wal-failover.persistentVolume.size":    "1Gi",
		}),
	}

	kubectlOptions := k8s.NewKubectlOptions("", "", h.Namespace)

	h.InstallHelm(t)
	h.ValidateCRDB(t)

	t.Log("Migrate the existing helm chart to CockroachDB Operator")

	prepareForMigration(t, h.CrdbCluster.StatefulSetName, h.Namespace, CASecret, "helm")
	defer func() {
		_ = os.RemoveAll(manifestsDirPath)
	}()

	t.Log("Install the CockroachDB operator")
	k8s.RunKubectl(t, kubectlOptions, "create", "priorityclass", "crdb-critical", "--value", "500000000")
	defer func() {
		t.Log("Delete the priority class crdb-critical")
		k8s.RunKubectl(t, kubectlOptions, "delete", "priorityclass", "crdb-critical")
	}()

	operator.InstallCockroachDBOperatorScopedForMigration(t, kubectlOptions, h.Namespace)
	defer func() {
		t.Log("Uninstall the CockroachDB operator")
		operator.UninstallCockroachDBOperator(t, kubectlOptions)
	}()

	migratePodsToCrdbNodes(t, h.CrdbCluster, h.Namespace)

	t.Log("All the statefulset pods are migrated to CrdbNodes")
	t.Log("Update the public service")
	k8s.RunKubectl(t, kubectlOptions, "apply", "-f", filepath.Join(manifestsDirPath, "public-service.yaml"))
	k8s.RunKubectl(t, kubectlOptions, "delete", "poddisruptionbudget", fmt.Sprintf("%s-budget", h.CrdbCluster.StatefulSetName))

	t.Log("Verify WAL failover configuration in migrated values.yaml")
	valuesFile := filepath.Join(manifestsDirPath, "values.yaml")
	valuesContent, err := os.ReadFile(valuesFile)
	require.NoError(t, err)
	require.Contains(t, string(valuesContent), "walFailoverSpec:")
	require.Contains(t, string(valuesContent), "path: /cockroach/wal-failover")
	require.Contains(t, string(valuesContent), "size: 1Gi")
	require.Contains(t, string(valuesContent), "status: enable")

	t.Log("helm upgrade the CockroachDB operator")
	helmPath, _ := operator.HelmChartPaths()
	err = helm.UpgradeE(t, &helm.Options{
		KubectlOptions: kubectlOptions,
		ValuesFiles:    []string{filepath.Join(manifestsDirPath, "values.yaml")},
	}, helmPath, releaseName)
	require.ErrorContains(t, err, "You are attempting to upgrade from a StatefulSet-based CockroachDB Helm chart to the CockroachDB Operator.")

	t.Log("Delete the StatefulSet as helm upgrade can proceed only if no StatefulSet is present")
	k8s.RunKubectl(t, kubectlOptions, "delete", "statefulset", h.CrdbCluster.StatefulSetName)

	helm.Upgrade(t, &helm.Options{
		KubectlOptions: kubectlOptions,
		ValuesFiles:    []string{filepath.Join(manifestsDirPath, "values.yaml")},
	}, helmPath, releaseName)

	for i := h.CrdbCluster.DesiredNodes - 1; i >= 0; i-- {
		podName := fmt.Sprintf("%s-%d", h.CrdbCluster.StatefulSetName, i)
		testutil.RequirePodToBeCreatedAndReady(t, kubectlOptions, podName, 300*time.Second)
	}
	defer func() {
		t.Log("helm uninstall the crdbcluster CR from the helm chart")
		h.Uninstall(t)
	}()

	k8s.RunKubectl(t, kubectlOptions, "create", "-f", filepath.Join(manifestsDirPath, fmt.Sprintf("%s-cockroachdb-ca-cert.yaml", releaseName)))
	k8s.RunKubectl(t, kubectlOptions, "create", "-f", filepath.Join(manifestsDirPath, fmt.Sprintf("%s-cockroachdb-ca-issuer.yaml", releaseName)))

	h.ValidateExistingData = true
	h.ValidateCRDB(t)
	h.ValidateCertManagerResources(t)
}

func (h *HelmChartToOperator) TestCertManagerMigration(t *testing.T) {
	const caSecretName = "cockroach-ca"
	h.Namespace = "cockroach" + strings.ToLower(random.UniqueId())
	kubectlOptions := k8s.NewKubectlOptions("", "", h.Namespace)

	certManagerK8sOptions := k8s.NewKubectlOptions("", "", testutil.CertManagerNamespace)
	testutil.InstallCertManager(t, certManagerK8sOptions)
	// ... and make sure to delete the helm release at the end of the test.
	defer func() {
		testutil.DeleteTrustManager(t, certManagerK8sOptions)
		testutil.DeleteCertManager(t, certManagerK8sOptions)
		k8s.DeleteNamespace(t, certManagerK8sOptions, testutil.CertManagerNamespace)
	}()

	k8s.CreateNamespace(t, kubectlOptions, h.Namespace)
	testutil.CreateSelfSignedIssuer(t, kubectlOptions, h.Namespace)

	h.CrdbCluster = testutil.CockroachCluster{
		Cfg:              cfg,
		K8sClient:        k8sClient,
		StatefulSetName:  fmt.Sprintf("%s-cockroachdb", releaseName),
		Namespace:        h.Namespace,
		ClientSecret:     "cockroachdb-root",
		NodeSecret:       "cockroachdb-node",
		CaSecret:         caSecretName,
		IsCaUserProvided: isCaUserProvided,
		Context:          k3dClusterName,
		DesiredNodes:     3,
	}
	h.HelmOptions = &helm.Options{
		SetValues: testutil.PatchHelmValues(map[string]string{
			"operator.enabled":                         "false",
			"conf.cluster-name":                        "test",
			"init.provisioning.enabled":                "true",
			"init.provisioning.databases[0].name":      migration.TestDBName,
			"init.provisioning.databases[0].owners[0]": "root",
			"statefulset.labels.app":                   "cockroachdb",
			"tls.certs.selfSigner.enabled":             "false",
			"tls.certs.certManager":                    "true",
			"tls.certs.certManagerIssuer.name":         testutil.SelfSignedIssuerName,
		}),
	}

	h.ValidateExistingData = false
	h.InstallHelm(t)
	h.ValidateCRDB(t)

	t.Log("Migrate the existing helm chart to CockroachDB Operator")

	prepareForMigration(t, h.CrdbCluster.StatefulSetName, h.Namespace, CASecret, "helm")
	defer func() {
		_ = os.RemoveAll(manifestsDirPath)
	}()

	caConfigMapName := fmt.Sprintf("%s-ca-crt", h.CrdbCluster.StatefulSetName)
	testutil.InstallTrustManager(t, certManagerK8sOptions, h.Namespace)
	testutil.CreateBundle(t, kubectlOptions, caSecretName, caConfigMapName)
	defer func() {
		testutil.DeleteBundle(t, kubectlOptions)
	}()

	t.Log("Install the CockroachDB operator")
	k8s.RunKubectl(t, kubectlOptions, "create", "priorityclass", "crdb-critical", "--value", "500000000")
	defer func() {
		t.Log("Delete the priority class crdb-critical")
		k8s.RunKubectl(t, kubectlOptions, "delete", "priorityclass", "crdb-critical")
	}()

	operator.InstallCockroachDBOperatorScopedForMigration(t, kubectlOptions, h.Namespace)
	defer func() {
		t.Log("Uninstall the CockroachDB operator")
		operator.UninstallCockroachDBOperator(t, kubectlOptions)
	}()

	migratePodsToCrdbNodes(t, h.CrdbCluster, h.Namespace)

	t.Log("All the statefulset pods are migrated to CrdbNodes")
	t.Log("Update the public service")
	k8s.RunKubectl(t, kubectlOptions, "apply", "-f", filepath.Join(manifestsDirPath, "public-service.yaml"))
	k8s.RunKubectl(t, kubectlOptions, "delete", "poddisruptionbudget", fmt.Sprintf("%s-budget", h.CrdbCluster.StatefulSetName))

	t.Log("helm upgrade the cockroach CockroachDB operator")
	helmPath, _ := operator.HelmChartPaths()
	t.Log("Delete the StatefulSet as helm upgrade can proceed only if no StatefulSet is present")
	k8s.RunKubectl(t, kubectlOptions, "delete", "statefulset", h.CrdbCluster.StatefulSetName)

	helm.Upgrade(t, &helm.Options{
		KubectlOptions: kubectlOptions,
		ValuesFiles:    []string{filepath.Join(manifestsDirPath, "values.yaml")},
	}, helmPath, releaseName)

	for i := h.CrdbCluster.DesiredNodes - 1; i >= 0; i-- {
		podName := fmt.Sprintf("%s-%d", h.CrdbCluster.StatefulSetName, i)
		testutil.RequirePodToBeCreatedAndReady(t, kubectlOptions, podName, 300*time.Second)
	}
	defer func() {
		t.Log("helm uninstall the crdbcluster CR from the helm chart")
		h.Uninstall(t)
	}()

	h.ValidateExistingData = true
	h.ValidateCRDB(t)
}

func (h *HelmChartToOperator) TestDedicatedLogsPVCMigration(t *testing.T) {
	h.Namespace = "cockroach" + strings.ToLower(random.UniqueId())
	h.CrdbCluster = testutil.CockroachCluster{
		Cfg:              cfg,
		K8sClient:        k8sClient,
		StatefulSetName:  fmt.Sprintf("%s-cockroachdb", releaseName),
		Namespace:        h.Namespace,
		ClientSecret:     ClientSecret,
		NodeSecret:       NodeSecret,
		CaSecret:         CASecret,
		IsCaUserProvided: isCaUserProvided,
		Context:          k3dClusterName,
		DesiredNodes:     3,
	}
	h.HelmOptions = &helm.Options{
		SetValues: testutil.PatchHelmValues(map[string]string{
			"operator.enabled":                         "false",
			"conf.cluster-name":                        "test",
			"init.provisioning.enabled":                "true",
			"init.provisioning.databases[0].name":      migration.TestDBName,
			"init.provisioning.databases[0].owners[0]": "root",
			"statefulset.labels.app":                   "cockroachdb",
			"conf.locality":                            "topology.kubernetes.io/region=us-east-1",
			"storage.PersistentVolume.enabled":         "true",
			"conf.log.enabled":                         "true",
			"conf.log.config.file-defaults.dir":        "/cockroach/test-logs",
			"conf.log.persistentVolume.enabled":        "true",
			"conf.log.persistentVolume.path":           "test-logs",
			"conf.log.persistentVolume.size":           "1Gi",
		}),
	}

	kubectlOptions := k8s.NewKubectlOptions("", "", h.Namespace)
	h.ValidateExistingData = false
	h.InstallHelm(t)
	h.ValidateCRDB(t)

	t.Log("Verify logsdir PVCs are created by the public chart StatefulSet")
	for i := 0; i < h.CrdbCluster.DesiredNodes; i++ {
		pvcName := fmt.Sprintf("logsdir-%s-%d", h.CrdbCluster.StatefulSetName, i)
		k8s.RunKubectl(t, kubectlOptions, "get", "pvc", pvcName)
	}

	t.Log("Migrate the existing helm chart to CockroachDB Operator")
	prepareForMigration(t, h.CrdbCluster.StatefulSetName, h.Namespace, CASecret, "helm")
	defer func() {
		_ = os.RemoveAll(manifestsDirPath)
	}()

	t.Log("Verify dedicated log PVC configuration in migrated values.yaml")
	valuesFile := filepath.Join(manifestsDirPath, "values.yaml")
	valuesContent, err := os.ReadFile(valuesFile)
	require.NoError(t, err)
	require.Contains(t, string(valuesContent), "logsStore:")
	require.Contains(t, string(valuesContent), "mountPath: /cockroach/test-logs")
	require.Contains(t, string(valuesContent), "storage: 1Gi")

	t.Log("Verify log ConfigMap was created from the STS log secret during migration")
	logConfigMapName := fmt.Sprintf("%s-log-config", h.CrdbCluster.StatefulSetName)
	k8s.RunKubectl(t, kubectlOptions, "get", "configmap", logConfigMapName)
	require.Contains(t, string(valuesContent), "loggingConfigMapName:")
	require.Contains(t, string(valuesContent), logConfigMapName)

	t.Log("Verify CrdbNode manifests contain the logsStore configuration")
	for i := 0; i < h.CrdbCluster.DesiredNodes; i++ {
		nodeManifest, err := os.ReadFile(filepath.Join(manifestsDirPath, fmt.Sprintf("crdbnode-%d.yaml", i)))
		require.NoError(t, err)
		require.Contains(t, string(nodeManifest), "logsStore:")
		require.Contains(t, string(nodeManifest), "mountPath: /cockroach/test-logs")
	}

	t.Log("Install the CockroachDB Operator")
	k8s.RunKubectl(t, kubectlOptions, "create", "priorityclass", "crdb-critical", "--value", "500000000")
	defer func() {
		t.Log("Delete the priority class crdb-critical")
		k8s.RunKubectl(t, kubectlOptions, "delete", "priorityclass", "crdb-critical")
	}()

	operator.InstallCockroachDBOperatorScopedForMigration(t, kubectlOptions, h.Namespace)
	defer func() {
		t.Log("Uninstall the CockroachDB Operator")
		operator.UninstallCockroachDBOperator(t, kubectlOptions)
	}()

	migratePodsToCrdbNodes(t, h.CrdbCluster, h.Namespace)

	t.Log("All the statefulset pods are migrated to CrdbNodes")
	t.Log("Update the public service")
	k8s.RunKubectl(t, kubectlOptions, "apply", "-f", filepath.Join(manifestsDirPath, "public-service.yaml"))
	k8s.RunKubectl(t, kubectlOptions, "delete", "poddisruptionbudget", fmt.Sprintf("%s-budget", h.CrdbCluster.StatefulSetName))

	t.Log("Verify logsdir PVCs are retained after pod migration")
	for i := 0; i < h.CrdbCluster.DesiredNodes; i++ {
		pvcName := fmt.Sprintf("logsdir-%s-%d", h.CrdbCluster.StatefulSetName, i)
		k8s.RunKubectl(t, kubectlOptions, "get", "pvc", pvcName)
	}

	t.Log("helm upgrade the CockroachDB Operator")
	helmPath, _ := operator.HelmChartPaths()
	err = helm.UpgradeE(t, &helm.Options{
		KubectlOptions: kubectlOptions,
		ValuesFiles:    []string{valuesFile},
	}, helmPath, releaseName)
	require.ErrorContains(t, err, "You are attempting to upgrade from a StatefulSet-based CockroachDB Helm chart to the CockroachDB Operator.")

	t.Log("Delete the StatefulSet as helm upgrade can proceed only if no StatefulSet is present")
	k8s.RunKubectl(t, kubectlOptions, "delete", "statefulset", h.CrdbCluster.StatefulSetName)

	helm.Upgrade(t, &helm.Options{
		KubectlOptions: kubectlOptions,
		ValuesFiles:    []string{valuesFile},
	}, helmPath, releaseName)

	for i := h.CrdbCluster.DesiredNodes - 1; i >= 0; i-- {
		podName := fmt.Sprintf("%s-%d", h.CrdbCluster.StatefulSetName, i)
		testutil.RequirePodToBeCreatedAndReady(t, kubectlOptions, podName, 300*time.Second)
	}
	defer func() {
		t.Log("helm uninstall the crdbcluster CR from the helm chart")
		h.Uninstall(t)
	}()

	h.ValidateExistingData = true
	h.ValidateCRDB(t)
}

func (h *HelmChartToOperator) TestPCRPrimaryMigration(t *testing.T) {
	h.Namespace = "cockroach" + strings.ToLower(random.UniqueId())
	h.CrdbCluster = testutil.CockroachCluster{
		Cfg:              cfg,
		K8sClient:        k8sClient,
		StatefulSetName:  fmt.Sprintf("%s-cockroachdb", releaseName),
		Namespace:        h.Namespace,
		ClientSecret:     ClientSecret,
		NodeSecret:       NodeSecret,
		CaSecret:         CASecret,
		IsCaUserProvided: isCaUserProvided,
		Context:          k3dClusterName,
		DesiredNodes:     3,
	}
	h.HelmOptions = &helm.Options{
		SetValues: testutil.PatchHelmValues(map[string]string{
			"operator.enabled":                         "false",
			"conf.cluster-name":                        "test",
			"init.provisioning.enabled":                "true",
			"init.provisioning.databases[0].name":      migration.TestDBName,
			"init.provisioning.databases[0].owners[0]": "root",
			"statefulset.labels.app":                   "cockroachdb",
			"conf.locality":                            "topology.kubernetes.io/region=us-east-1",
			"init.pcr.enabled":                         "true",
			"init.pcr.isPrimary":                       "true",
		}),
	}

	kubectlOptions := k8s.NewKubectlOptions("", "", h.Namespace)
	h.ValidateExistingData = false
	h.InstallHelm(t)
	h.ValidateCRDB(t)

	t.Log("Migrate the existing helm chart to Cockroach CockroachDB Operator with PCR primary configuration")

	prepareForMigration(t, h.CrdbCluster.StatefulSetName, h.Namespace, CASecret, "helm")
	defer func() {
		_ = os.RemoveAll(manifestsDirPath)
	}()

	t.Log("Install the CockroachDB operator")
	k8s.RunKubectl(t, kubectlOptions, "create", "priorityclass", "crdb-critical", "--value", "500000000")
	defer func() {
		t.Log("Delete the priority class crdb-critical")
		k8s.RunKubectl(t, kubectlOptions, "delete", "priorityclass", "crdb-critical")
	}()

	operator.InstallCockroachDBOperatorScopedForMigration(t, kubectlOptions, h.Namespace)
	defer func() {
		t.Log("Uninstall the CockroachDB operator")
		operator.UninstallCockroachDBOperator(t, kubectlOptions)
	}()

	migratePodsToCrdbNodes(t, h.CrdbCluster, h.Namespace)

	t.Log("All the statefulset pods are migrated to CrdbNodes")
	t.Log("Update the public service")
	k8s.RunKubectl(t, kubectlOptions, "apply", "-f", filepath.Join(manifestsDirPath, "public-service.yaml"))
	k8s.RunKubectl(t, kubectlOptions, "delete", "poddisruptionbudget", fmt.Sprintf("%s-budget", h.CrdbCluster.StatefulSetName))

	t.Log("Verify PCR primary configuration in migrated values.yaml")
	valuesFile := filepath.Join(manifestsDirPath, "values.yaml")
	valuesContent, err := os.ReadFile(valuesFile)
	require.NoError(t, err)
	require.Contains(t, string(valuesContent), "virtualCluster:")
	require.Contains(t, string(valuesContent), "mode: primary")

	t.Log("helm upgrade the cockroach CockroachDB operator")
	helmPath, _ := operator.HelmChartPaths()
	err = helm.UpgradeE(t, &helm.Options{
		KubectlOptions: kubectlOptions,
		ValuesFiles:    []string{filepath.Join(manifestsDirPath, "values.yaml")},
	}, helmPath, releaseName)
	require.ErrorContains(t, err, "You are attempting to upgrade from a StatefulSet-based CockroachDB Helm chart to the CockroachDB Operator.")

	t.Log("Delete the StatefulSet as helm upgrade can proceed only if no StatefulSet is present")
	k8s.RunKubectl(t, kubectlOptions, "delete", "statefulset", h.CrdbCluster.StatefulSetName)

	helm.Upgrade(t, &helm.Options{
		KubectlOptions: kubectlOptions,
		ValuesFiles:    []string{filepath.Join(manifestsDirPath, "values.yaml")},
	}, helmPath, releaseName)

	for i := h.CrdbCluster.DesiredNodes - 1; i >= 0; i-- {
		podName := fmt.Sprintf("%s-%d", h.CrdbCluster.StatefulSetName, i)
		testutil.RequirePodToBeCreatedAndReady(t, kubectlOptions, podName, 300*time.Second)
	}
	defer func() {
		t.Log("helm uninstall the crdbcluster CR from the helm chart")
		h.Uninstall(t)
	}()

	h.ValidateExistingData = true
	h.ValidateCRDB(t)
}
