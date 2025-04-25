package migrate

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	optestutil "github.com/cockroachdb/cockroach-operator/pkg/testutil"
	"github.com/cockroachdb/helm-charts/tests/e2e/operator"
	"github.com/cockroachdb/helm-charts/tests/testutil"
	"github.com/cockroachdb/helm-charts/tests/testutil/migration"
	"github.com/gruntwork-io/terratest/modules/helm"
	"github.com/gruntwork-io/terratest/modules/k8s"
	"github.com/gruntwork-io/terratest/modules/random"
	corev1 "k8s.io/api/core/v1"
	resource "k8s.io/apimachinery/pkg/api/resource"
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

	// Create cluster with different logging config than the default one.
	createLoggingConfig(t, k8sClient, "logging-config", o.Namespace)

	crdbClusterName := "crdb-test"
	o.CustomResourceBuilder = optestutil.NewBuilder(crdbClusterName).
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
		}).WithClusterLogging("logging-config")

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
	testutil.RequireCRDBDatabaseToFunction(t, o.CrdbCluster, migration.TestDBName, "")

	prepareForMigration(t, o.CrdbCluster.StatefulSetName, o.Namespace, o.CrdbCluster.CaSecret, "operator")
	defer func() {
		_ = os.RemoveAll(manifestsDirPath)
	}()

	o.UninstallOperator(t)
	t.Log("Create the priority class crdb-critical which will be owned by the cockroachdb enterprise operator")
	k8s.RunKubectl(t, kubectlOptions, "create", "priorityclass", "crdb-critical", "--value", "500000000")
	defer k8s.RunKubectl(t, kubectlOptions, "delete", "priorityclass", "crdb-critical")

	t.Log("Create the rbac permissions for the cockroachdb enterprise operator")
	k8s.KubectlApply(t, kubectlOptions, filepath.Join(manifestsDirPath, "rbac.yaml"))

	t.Log("Install the cockroachdb enterprise operator")
	operator.InstallCockroachDBEnterpriseOperator(t, kubectlOptions)
	defer func() {
		t.Log("Uninstall the cockroachdb enterprise operator")
		operator.UninstallCockroachDBEnterpriseOperator(t, kubectlOptions)
	}()

	migratePodsToCrdbNodes(t, o.CrdbCluster.StatefulSetName, o.Namespace)

	k8s.RunKubectl(t, kubectlOptions, "delete", "poddisruptionbudget", o.CrdbCluster.StatefulSetName)
	k8s.RunKubectl(t, kubectlOptions, "annotate", "service", fmt.Sprintf("%s-public", o.CrdbCluster.StatefulSetName), fmt.Sprintf("meta.helm.sh/release-name=%s", o.CrdbCluster.StatefulSetName), "--overwrite")
	k8s.RunKubectl(t, kubectlOptions, "annotate", "service", fmt.Sprintf("%s-public", o.CrdbCluster.StatefulSetName), fmt.Sprintf("meta.helm.sh/release-namespace=%s", o.Namespace), "--overwrite")
	k8s.RunKubectl(t, kubectlOptions, "label", "service", fmt.Sprintf("%s-public", o.CrdbCluster.StatefulSetName), fmt.Sprintf("app.kubernetes.io/managed-by=%s", "Helm"), "--overwrite")

	o.HelmOptions = &helm.Options{
		KubectlOptions: kubectlOptions,
		ValuesFiles:    []string{filepath.Join(manifestsDirPath, "values.yaml")},
		SetValues:      map[string]string{},
	}
	t.Log("helm install the cockroach enterprise operator")
	helmPath, _ := operator.HelmChartPaths()
	helm.Install(t, o.HelmOptions, helmPath, crdbClusterName)
	defer func() {
		t.Log("helm uninstall the cockroach enterprise operator")
		o.Uninstall(t)
	}()

	// Update the client and node secrets to the new release name
	o.CrdbCluster.ClientSecret = fmt.Sprintf("%s-client-secret", crdbClusterName)
	o.CrdbCluster.NodeSecret = fmt.Sprintf("%s-node-secret", crdbClusterName)

	o.ValidateCRDB(t)
}
