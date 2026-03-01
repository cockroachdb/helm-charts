package migrate

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
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
)

type HelmChartToOperator struct {
	migration.HelmInstall
}

func newHelmChartToOperator() *HelmChartToOperator {
	return &HelmChartToOperator{}
}

func init() {
	var err error
	runtimeScheme := runtime.NewScheme()
	_ = kscheme.AddToScheme(runtimeScheme)
	_ = api.AddToScheme(runtimeScheme)
	cfg = ctrl.GetConfigOrDie()
	k8sClient, err = client.New(cfg, client.Options{
		Scheme: runtimeScheme,
	})
	if err != nil {
		panic(err)
	}
}

func TestHelmChartToOperatorMigration(t *testing.T) {
	h := newHelmChartToOperator()
	t.Run("helm chart to cockroach enterprise operator migration", h.TestDefaultMigration)
	t.Run("helm chart to cockroach enterprise operator migration with cert manager", h.TestCertManagerMigration)
	t.Run("helm chart to cockroach enterprise operator migration with PCR primary", h.TestPCRPrimaryMigration)
	// TODO: Re-enable once dedicated logs PVC e2e test is stable.
	// t.Run("helm chart to cockroach enterprise operator migration with dedicated logs PVC", h.TestDedicatedLogsPVCMigration)
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

	// Clean up any CRDs from previous test runs to avoid storedVersions conflicts
	t.Log("Cleaning up CRDs and instances from previous test runs")

	// Helper to patch finalizers and delete resources
	cleanupResources := func(resourceType string) {
		// Get all resources of this type
		output, err := k8s.RunKubectlAndGetOutputE(t, kubectlOptions, "get", resourceType, "--all-namespaces", "-o", "jsonpath={.items[*].metadata.name}")
		if err == nil && output != "" {
			resources := strings.Split(output, " ")
			for _, res := range resources {
				// Patch finalizers to empty to ensure deletion doesn't hang
				k8s.RunKubectl(t, kubectlOptions, "patch", resourceType, res, "-p", "{\"metadata\":{\"finalizers\":[]}}", "--type=merge")
				// Delete the resource
				k8s.RunKubectl(t, kubectlOptions, "delete", resourceType, res, "--ignore-not-found=true")
			}
		}
	}

	// Clean up instances first
	cleanupResources("crdbclusters.crdb.cockroachlabs.com")
	cleanupResources("crdbnodes.crdb.cockroachlabs.com")
	cleanupResources("crdbtenants.crdb.cockroachlabs.com")

	k8s.RunKubectl(t, kubectlOptions, "delete", "crd", "crdbclusters.crdb.cockroachlabs.com", "--ignore-not-found=true", "--wait")
	k8s.RunKubectl(t, kubectlOptions, "delete", "crd", "crdbnodes.crdb.cockroachlabs.com", "--ignore-not-found=true", "--wait")
	k8s.RunKubectl(t, kubectlOptions, "delete", "crd", "crdbtenants.crdb.cockroachlabs.com", "--ignore-not-found=true", "--wait")

	h.InstallHelm(t)
	h.ValidateCRDB(t)

	t.Log("Migrate the existing helm chart to Cockroach Enterprise Operator")

	prepareForMigration(t, h.CrdbCluster.StatefulSetName, h.Namespace, CASecret, "helm")
	defer func() {
		_ = os.RemoveAll(manifestsDirPath)
	}()

	t.Log("Install the cockroachdb enterprise operator")
	k8s.RunKubectl(t, kubectlOptions, "create", "priorityclass", "crdb-critical", "--value", "500000000")
	defer func() {
		t.Log("Delete the priority class crdb-critical")
		k8s.RunKubectl(t, kubectlOptions, "delete", "priorityclass", "crdb-critical")
	}()

	operator.InstallCockroachDBEnterpriseOperator(t, kubectlOptions)
	defer func() {
		t.Log("Uninstall the cockroachdb enterprise operator")
		operator.UninstallCockroachDBEnterpriseOperator(t, kubectlOptions)
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

	t.Log("helm upgrade the cockroach enterprise operator")
	helmPath, _ := operator.HelmChartPaths()
	err = helm.UpgradeE(t, &helm.Options{
		KubectlOptions: kubectlOptions,
		ValuesFiles:    []string{filepath.Join(manifestsDirPath, "values.yaml")},
	}, helmPath, releaseName)
	require.Contains(t, err.Error(), "You are attempting to upgrade from a StatefulSet-based CockroachDB Helm chart to the CockroachDB Enterprise Operator.")

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

func (h *HelmChartToOperator) TestCertManagerMigration(t *testing.T) {
	const caSecretName = "cockroach-ca"
	h.Namespace = "cockroach" + strings.ToLower(random.UniqueId())
	kubectlOptions := k8s.NewKubectlOptions("", "", h.Namespace)

	certManagerK8sOptions := k8s.NewKubectlOptions("", "", testutil.CertManagerNamespace)
	testutil.InstallCertManager(t, certManagerK8sOptions)
	// ... and make sure to delete the helm release at the end of the test.
	defer func() {
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

	t.Log("Migrate the existing helm chart to Cockroach Enterprise Operator")

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

	t.Log("Install the cockroachdb enterprise operator")
	k8s.RunKubectl(t, kubectlOptions, "create", "priorityclass", "crdb-critical", "--value", "500000000")
	defer func() {
		t.Log("Delete the priority class crdb-critical")
		k8s.RunKubectl(t, kubectlOptions, "delete", "priorityclass", "crdb-critical")
	}()

	operator.InstallCockroachDBEnterpriseOperator(t, kubectlOptions)
	defer func() {
		t.Log("Uninstall the cockroachdb enterprise operator")
		operator.UninstallCockroachDBEnterpriseOperator(t, kubectlOptions)
	}()

	migratePodsToCrdbNodes(t, h.CrdbCluster, h.Namespace)

	t.Log("All the statefulset pods are migrated to CrdbNodes")
	t.Log("Update the public service")
	k8s.RunKubectl(t, kubectlOptions, "apply", "-f", filepath.Join(manifestsDirPath, "public-service.yaml"))
	k8s.RunKubectl(t, kubectlOptions, "delete", "poddisruptionbudget", fmt.Sprintf("%s-budget", h.CrdbCluster.StatefulSetName))

	t.Log("helm upgrade the cockroach enterprise operator")
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

	k8s.RunKubectl(t, kubectlOptions, "create", "-f", filepath.Join(manifestsDirPath, fmt.Sprintf("%s-cockroachdb-ca-cert.yaml", releaseName)))
	k8s.RunKubectl(t, kubectlOptions, "create", "-f", filepath.Join(manifestsDirPath, fmt.Sprintf("%s-cockroachdb-ca-issuer.yaml", releaseName)))

	h.ValidateExistingData = true
	h.ValidateCRDB(t)
	h.ValidateCertManagerResources(t)
}

// TODO: Re-enable once dedicated logs PVC e2e test is stable.
// func (h *HelmChartToOperator) TestDedicatedLogsPVCMigration(t *testing.T) {
// 	h.Namespace = "cockroach" + strings.ToLower(random.UniqueId())
// 	h.CrdbCluster = testutil.CockroachCluster{
// 		Cfg:              cfg,
// 		K8sClient:        k8sClient,
// 		StatefulSetName:  fmt.Sprintf("%s-cockroachdb", releaseName),
// 		Namespace:        h.Namespace,
// 		ClientSecret:     ClientSecret,
// 		NodeSecret:       NodeSecret,
// 		CaSecret:         CASecret,
// 		IsCaUserProvided: isCaUserProvided,
// 		Context:          k3dClusterName,
// 		DesiredNodes:     3,
// 	}
// 	h.HelmOptions = &helm.Options{
// 		SetValues: testutil.PatchHelmValues(map[string]string{
// 			"operator.enabled":                           "false",
// 			"conf.cluster-name":                         "test",
// 			"init.provisioning.enabled":                 "true",
// 			"init.provisioning.databases[0].name":       migration.TestDBName,
// 			"init.provisioning.databases[0].owners[0]":  "root",
// 			"statefulset.labels.app":                    "cockroachdb",
// 			"conf.locality":                             "topology.kubernetes.io/region=us-east-1",
// 			"storage.PersistentVolume.enabled":          "true",
// 			"conf.log.enabled":                          "true",
// 			"conf.log.config.file-defaults.dir":         "/cockroach/cockroach-logs",
// 			"conf.log.persistentVolume.enabled":         "true",
// 			"conf.log.persistentVolume.size":            "5Gi",
// 		}),
// 	}
//
// 	kubectlOptions := k8s.NewKubectlOptions("", "", h.Namespace)
// 	h.ValidateExistingData = false
// 	h.InstallHelm(t)
// 	h.ValidateCRDB(t)
//
// 	t.Log("Verify logsdir PVCs are created by the public chart StatefulSet")
// 	for i := 0; i < h.CrdbCluster.DesiredNodes; i++ {
// 		pvcName := fmt.Sprintf("logsdir-%s-%d", h.CrdbCluster.StatefulSetName, i)
// 		k8s.RunKubectl(t, kubectlOptions, "get", "pvc", pvcName)
// 	}
//
// 	t.Log("Migrate the existing helm chart to Cockroach Enterprise Operator")
// 	prepareForMigration(t, h.CrdbCluster.StatefulSetName, h.Namespace, CASecret, "helm")
// 	defer func() {
// 		_ = os.RemoveAll(manifestsDirPath)
// 	}()
//
// 	t.Log("Verify dedicated log PVC configuration in migrated values.yaml")
// 	valuesFile := filepath.Join(manifestsDirPath, "values.yaml")
// 	valuesContent, err := os.ReadFile(valuesFile)
// 	require.NoError(t, err)
// 	require.Contains(t, string(valuesContent), "persistentVolume:")
// 	require.Contains(t, string(valuesContent), "enabled: true")
// 	require.Contains(t, string(valuesContent), "path: /cockroach/cockroach-logs")
// 	require.Contains(t, string(valuesContent), "size: 5Gi")
//
// 	t.Log("Verify CrdbNode manifests contain the logsStore configuration")
// 	for i := 0; i < h.CrdbCluster.DesiredNodes; i++ {
// 		nodeManifest, err := os.ReadFile(filepath.Join(manifestsDirPath, fmt.Sprintf("crdbnode-%d.yaml", i)))
// 		require.NoError(t, err)
// 		require.Contains(t, string(nodeManifest), "logsStore:")
// 		require.Contains(t, string(nodeManifest), "mountPath: /cockroach/cockroach-logs")
// 	}
//
// 	t.Log("Install the cockroachdb enterprise operator")
// 	k8s.RunKubectl(t, kubectlOptions, "create", "priorityclass", "crdb-critical", "--value", "500000000")
// 	defer func() {
// 		t.Log("Delete the priority class crdb-critical")
// 		k8s.RunKubectl(t, kubectlOptions, "delete", "priorityclass", "crdb-critical")
// 	}()
//
// 	operator.InstallCockroachDBEnterpriseOperator(t, kubectlOptions)
// 	defer func() {
// 		t.Log("Uninstall the cockroachdb enterprise operator")
// 		operator.UninstallCockroachDBEnterpriseOperator(t, kubectlOptions)
// 	}()
//
// 	migratePodsToCrdbNodes(t, h.CrdbCluster, h.Namespace)
//
// 	t.Log("All the statefulset pods are migrated to CrdbNodes")
// 	t.Log("Update the public service")
// 	k8s.RunKubectl(t, kubectlOptions, "apply", "-f", filepath.Join(manifestsDirPath, "public-service.yaml"))
// 	k8s.RunKubectl(t, kubectlOptions, "delete", "poddisruptionbudget", fmt.Sprintf("%s-budget", h.CrdbCluster.StatefulSetName))
//
// 	t.Log("Verify logsdir PVCs are retained after pod migration")
// 	for i := 0; i < h.CrdbCluster.DesiredNodes; i++ {
// 		pvcName := fmt.Sprintf("logsdir-%s-%d", h.CrdbCluster.StatefulSetName, i)
// 		k8s.RunKubectl(t, kubectlOptions, "get", "pvc", pvcName)
// 	}
//
// 	t.Log("helm upgrade the cockroach enterprise operator")
// 	helmPath, _ := operator.HelmChartPaths()
// 	err = helm.UpgradeE(t, &helm.Options{
// 		KubectlOptions: kubectlOptions,
// 		ValuesFiles:    []string{valuesFile},
// 	}, helmPath, releaseName)
// 	require.Contains(t, err.Error(), "You are attempting to upgrade from a StatefulSet-based CockroachDB Helm chart to the CockroachDB Enterprise Operator.")
//
// 	t.Log("Delete the StatefulSet as helm upgrade can proceed only if no StatefulSet is present")
// 	k8s.RunKubectl(t, kubectlOptions, "delete", "statefulset", h.CrdbCluster.StatefulSetName)
//
// 	helm.Upgrade(t, &helm.Options{
// 		KubectlOptions: kubectlOptions,
// 		ValuesFiles:    []string{valuesFile},
// 	}, helmPath, releaseName)
//
// 	for i := h.CrdbCluster.DesiredNodes - 1; i >= 0; i-- {
// 		podName := fmt.Sprintf("%s-%d", h.CrdbCluster.StatefulSetName, i)
// 		testutil.RequirePodToBeCreatedAndReady(t, kubectlOptions, podName, 300*time.Second)
// 	}
// 	defer func() {
// 		t.Log("helm uninstall the crdbcluster CR from the helm chart")
// 		h.Uninstall(t)
// 	}()
//
// 	h.ValidateExistingData = true
// 	h.ValidateCRDB(t)
// }

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

	t.Log("Migrate the existing helm chart to Cockroach Enterprise Operator with PCR primary configuration")

	prepareForMigration(t, h.CrdbCluster.StatefulSetName, h.Namespace, CASecret, "helm")
	defer func() {
		_ = os.RemoveAll(manifestsDirPath)
	}()

	t.Log("Install the cockroachdb enterprise operator")
	k8s.RunKubectl(t, kubectlOptions, "create", "priorityclass", "crdb-critical", "--value", "500000000")
	defer func() {
		t.Log("Delete the priority class crdb-critical")
		k8s.RunKubectl(t, kubectlOptions, "delete", "priorityclass", "crdb-critical")
	}()

	operator.InstallCockroachDBEnterpriseOperator(t, kubectlOptions)
	defer func() {
		t.Log("Uninstall the cockroachdb enterprise operator")
		operator.UninstallCockroachDBEnterpriseOperator(t, kubectlOptions)
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

	t.Log("helm upgrade the cockroach enterprise operator")
	helmPath, _ := operator.HelmChartPaths()
	err = helm.UpgradeE(t, &helm.Options{
		KubectlOptions: kubectlOptions,
		ValuesFiles:    []string{filepath.Join(manifestsDirPath, "values.yaml")},
	}, helmPath, releaseName)
	require.Contains(t, err.Error(), "You are attempting to upgrade from a StatefulSet-based CockroachDB Helm chart to the CockroachDB Enterprise Operator.")

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
