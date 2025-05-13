package migrate

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gruntwork-io/terratest/modules/helm"
	"github.com/gruntwork-io/terratest/modules/k8s"
	"github.com/gruntwork-io/terratest/modules/random"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime"
	kscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	api "github.com/cockroachdb/cockroach-operator/apis/v1alpha1"
	"github.com/cockroachdb/helm-charts/tests/e2e/operator"
	"github.com/cockroachdb/helm-charts/tests/testutil"
	"github.com/cockroachdb/helm-charts/tests/testutil/migration"
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
			"operator.enabled":                         "false",
			"conf.cluster-name":                        "test",
			"init.provisioning.enabled":                "true",
			"init.provisioning.databases[0].name":      migration.TestDBName,
			"init.provisioning.databases[0].owners[0]": "root",
			"statefulset.labels.app":                   "cockroachdb",
		}),
	}

	kubectlOptions := k8s.NewKubectlOptions("", "", h.Namespace)

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

	t.Log("helm upgrade the cockroach enterprise operator")
	helmPath, _ := operator.HelmChartPaths()
	err := helm.UpgradeE(t, &helm.Options{
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
	defer func() {
		t.Log("helm uninstall the crdbcluster CR from the helm chart")
		h.Uninstall(t)
	}()

	h.ValidateExistingData = true
	h.ValidateCRDB(t)
}
