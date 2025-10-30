package migrate

import (
	"context"
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
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type PublicOperatorToCockroachEnterpriseOperator struct {
	migration.PublicOperator
}

func newPublicOperatorToCockroachEnterpriseOperator() *PublicOperatorToCockroachEnterpriseOperator {
	return &PublicOperatorToCockroachEnterpriseOperator{}
}

func TestPublicOperatorToCockroachEnterpriseOperator(t *testing.T) {
	h := newPublicOperatorToCockroachEnterpriseOperator()
	h.TestDefaultMigration(t)
}

func (o *PublicOperatorToCockroachEnterpriseOperator) TestDefaultMigration(t *testing.T) {
	o.Namespace = "cockroach" + strings.ToLower(random.UniqueId())
	kubectlOptions := k8s.NewKubectlOptions("", "", o.Namespace)
	k8s.CreateNamespace(t, k8s.NewKubectlOptions("", "", o.Namespace), o.Namespace)
	testutil.InstallIngressAndMetalLB(t)
	defer func() {
		t.Log("uninstall ingress and metalLB")
		testutil.UninstallIngressAndMetalLB(t)
	}()

	k8s.RunKubectl(t, kubectlOptions, "create", "priorityclass", "operator-critical", "--value", "500000000")
	defer func() {
		t.Log("Delete the priority class operator-critical")
		k8s.RunKubectl(t, kubectlOptions, "delete", "priorityclass", "operator-critical")
	}()

	// Create cluster with different logging config than the default one.
	createLoggingConfig(t, k8sClient, "logging-config", o.Namespace)

	ingress := &api.IngressConfig{
		UI: &api.Ingress{
			IngressClassName: "nginx",
			Host:             "ui.local.com",
			Annotations: map[string]string{
				"nginx.ingress.kubernetes.io/backend-protocol": "HTTPS",
			},
		},
		SQL: &api.Ingress{
			IngressClassName: "nginx",
			Host:             "sql.local.com",
			Annotations: map[string]string{
				"nginx.ingress.kubernetes.io/backend-protocol": "HTTPS",
			},
		},
	}

	crdbClusterName := "crdb-test"
	o.CustomResourceBuilder = testutil.NewBuilder(crdbClusterName).
		WithNodeCount(3).
		WithTLS().
		WithImage(migration.CockroachVersion).
		WithPVDataStore("1Gi").
		WithLabels(map[string]string{"app": "cockroachdb"}).
		WithAnnotations(map[string]string{"crdb": "isCool"}).
		WithTerminationGracePeriodSeconds(int64(10)).
		WithResources(corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("200m"),
				corev1.ResourceMemory: resource.MustParse("256Mi"),
			},
		}).WithClusterLogging("logging-config").
		WithIngress(ingress).
		WithPriorityClass("operator-critical")

	o.Ctx = context.Background()
	o.CrdbCluster = testutil.CockroachCluster{
		Cfg:              cfg,
		K8sClient:        k8sClient,
		StatefulSetName:  crdbClusterName,
		Namespace:        o.Namespace,
		ClientSecret:     fmt.Sprintf("%s-root", crdbClusterName),
		NodeSecret:       fmt.Sprintf("%s-node", crdbClusterName),
		CaSecret:         fmt.Sprintf("%s-ca", crdbClusterName),
		IsCaUserProvided: true,
		Context:          k3dClusterName,
		DesiredNodes:     3,
	}

	o.InstallOperator(t)

	testutil.RequireClusterToBeReadyEventuallyTimeout(t, o.CrdbCluster, 600*time.Second)
	time.Sleep(20 * time.Second)
	testutil.RequireCRDBToFunction(t, o.CrdbCluster, false)
	testutil.TestIngressRoutingDirect(t, "ui.local.com")

	prepareForMigration(t, o.CrdbCluster.StatefulSetName, o.Namespace, o.CrdbCluster.CaSecret, "operator")
	defer func() {
		_ = os.RemoveAll(manifestsDirPath)
	}()

	o.UninstallOperator(t)
	t.Log("Create the priority class crdb-critical which will be owned by the cockroachdb enterprise operator")
	k8s.RunKubectl(t, kubectlOptions, "create", "priorityclass", "crdb-critical", "--value", "500000000")
	defer func() {
		t.Log("Delete the priority class crdb-critical")
		k8s.RunKubectl(t, kubectlOptions, "delete", "priorityclass", "crdb-critical")
	}()

	t.Log("Create the rbac permissions for the cockroachdb enterprise operator")
	k8s.KubectlApply(t, kubectlOptions, filepath.Join(manifestsDirPath, "rbac.yaml"))

	t.Log("Install the cockroachdb enterprise operator")
	operator.InstallCockroachDBEnterpriseOperator(t, kubectlOptions)
	defer func() {
		t.Log("Uninstall the cockroachdb enterprise operator")
		operator.UninstallCockroachDBEnterpriseOperator(t, kubectlOptions)
	}()

	migratePodsToCrdbNodes(t, o.CrdbCluster, o.Namespace)

	k8s.RunKubectl(t, kubectlOptions, "delete", "poddisruptionbudget", o.CrdbCluster.StatefulSetName)

	var migrateObjsToHelm = make(map[string][]string)

	migrateObjsToHelm["service"] = []string{fmt.Sprintf("%s-public", o.CrdbCluster.StatefulSetName)}
	migrateObjsToHelm["ingress"] = []string{fmt.Sprintf("ui-%s", o.CrdbCluster.StatefulSetName), fmt.Sprintf("sql-%s", o.CrdbCluster.StatefulSetName)}
	for resourceName, obj := range migrateObjsToHelm {
		for _, objName := range obj {
			k8s.RunKubectl(t, kubectlOptions, "annotate", resourceName, objName, fmt.Sprintf("meta.helm.sh/release-name=%s", o.CrdbCluster.StatefulSetName), "--overwrite")
			k8s.RunKubectl(t, kubectlOptions, "annotate", resourceName, objName, fmt.Sprintf("meta.helm.sh/release-namespace=%s", o.Namespace), "--overwrite")
			k8s.RunKubectl(t, kubectlOptions, "label", resourceName, objName, fmt.Sprintf("app.kubernetes.io/managed-by=%s", "Helm"), "--overwrite")
		}
	}

	o.HelmOptions = &helm.Options{
		KubectlOptions: kubectlOptions,
		ValuesFiles:    []string{filepath.Join(manifestsDirPath, "values.yaml")},
		SetValues:      map[string]string{},
	}
	t.Log("helm install the crdb cluster from the helm chart")
	helmPath, _ := operator.HelmChartPaths()
	helm.Install(t, o.HelmOptions, helmPath, crdbClusterName)
	defer func() {
		t.Log("helm uninstall the crdb cluster")
		o.Uninstall(t)
	}()

	// Update the client and node secrets to the new release name
	o.CrdbCluster.ClientSecret = fmt.Sprintf("%s-client-secret", crdbClusterName)
	o.CrdbCluster.NodeSecret = fmt.Sprintf("%s-node-secret", crdbClusterName)

	o.ValidateExistingData = true
	o.ValidateCRDB(t)

	t.Log("Verify Operator Migration")
	// Verify priority class migration
	verifyOperatorMigration(t, kubectlOptions, o.CustomResourceBuilder.Cr())

	testutil.TestIngressRoutingDirect(t, "ui.local.com")
}

// verifyOperatorMigration verifies that the operator has been correctly migrated
// from the original operator to the running pods
func verifyOperatorMigration(t *testing.T, kubectlOptions *k8s.KubectlOptions, crdbCluster *api.CrdbCluster) {
	// Get all pods for this cluster
	pods := k8s.ListPods(t, kubectlOptions, metav1.ListOptions{
		LabelSelector: operator.LabelSelector,
	})

	require.Equal(t, len(pods), int(crdbCluster.Spec.Nodes))
	require.Equal(t, crdbCluster.Spec.PriorityClassName, pods[0].Spec.PriorityClassName)
	require.Equal(t, crdbCluster.Spec.TerminationGracePeriodSecs, *pods[0].Spec.TerminationGracePeriodSeconds)
}
