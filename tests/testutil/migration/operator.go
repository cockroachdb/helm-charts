package migration

import (
	"context"
	"testing"
	"time"

	optestutil "github.com/cockroachdb/cockroach-operator/pkg/testutil"
	"github.com/gruntwork-io/terratest/modules/k8s"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	CockroachVersion       = "cockroachdb/cockroach:v25.1.2"
	OperatorDeploymentName = "cockroach-operator-manager"
	OperatorNamespace      = "cockroach-operator-system"
)

type PublicOperator struct {
	Ctx context.Context

	CustomResourceBuilder optestutil.ClusterBuilder

	HelmInstall
}

func (o *PublicOperator) InstallOperator(t *testing.T) {
	kubectlOptions := k8s.NewKubectlOptions("", "", OperatorNamespace)
	t.Logf("Installing CRDs for cockroach-operator")
	k8s.KubectlApply(t, kubectlOptions, "https://raw.githubusercontent.com/cockroachdb/cockroach-operator/master/install/crds.yaml")

	t.Logf("Installing cockroach-operator")
	k8s.KubectlApply(t, kubectlOptions, "https://raw.githubusercontent.com/cockroachdb/cockroach-operator/master/install/operator.yaml")

	// Sleep for 10 seconds to check for the deployment to be ready
	time.Sleep(10 * time.Second)
	waitForOperatorToBeReady(t)

	t.Log("Waiting for cockroach-operator to be ready")
	t.Log("Installing crdbcluster custom resource")
	if _, err := k8s.GetNamespaceE(t, k8s.NewKubectlOptions("", "", o.Namespace), o.Namespace); err != nil && apierrors.IsNotFound(err) {
		k8s.CreateNamespace(t, k8s.NewKubectlOptions("", "", o.Namespace), o.Namespace)
	}

	crdbCluster := o.CustomResourceBuilder.Cr()
	crdbCluster.Namespace = o.Namespace
	require.NoError(t, o.CrdbCluster.K8sClient.Create(o.Ctx, crdbCluster))
}

func (o *PublicOperator) UninstallOperator(t *testing.T) {
	kubectlOptions := k8s.NewKubectlOptions("", "", OperatorNamespace)
	crdbCluster := o.CustomResourceBuilder.Cr()
	crdbCluster.Namespace = o.Namespace
	k8s.RunKubectl(t, kubectlOptions, "delete", "clusterrolebinding", "cockroach-operator-rolebinding")
	k8s.RunKubectl(t, k8s.NewKubectlOptions("", "", o.Namespace), "delete", "crdbcluster", crdbCluster.Name, "--cascade=orphan")

	t.Logf("Uninstalling cockroach-operator")
	k8s.KubectlDelete(t, kubectlOptions, "https://raw.githubusercontent.com/cockroachdb/cockroach-operator/master/install/crds.yaml")
	k8s.RunKubectl(t, kubectlOptions, "delete", "clusterrole", "cockroach-operator-role")
	k8s.RunKubectl(t, kubectlOptions, "delete", "serviceaccount", "cockroach-operator-sa")
	k8s.RunKubectl(t, kubectlOptions, "delete", "deployment", "cockroach-operator-manager")
	k8s.RunKubectl(t, kubectlOptions, "delete", "service", "cockroach-operator-webhook-service")
	k8s.RunKubectl(t, kubectlOptions, "delete", "mutatingwebhookconfigurations", "cockroach-operator-mutating-webhook-configuration")
	k8s.RunKubectl(t, kubectlOptions, "delete", "validatingwebhookconfigurations", "cockroach-operator-validating-webhook-configuration")
}

func waitForOperatorToBeReady(t *testing.T) {
	kubectlOptions := k8s.NewKubectlOptions("", "", OperatorNamespace)
	k8s.WaitUntilDeploymentAvailable(t, kubectlOptions, OperatorDeploymentName, 10, 10*time.Second)
	pods, err := k8s.ListPodsE(t, kubectlOptions, metav1.ListOptions{LabelSelector: "app=cockroach-operator"})
	require.NoError(t, err)
	for _, pod := range pods {
		k8s.WaitUntilPodAvailable(t, kubectlOptions, pod.Name, 10, 10*time.Second)
	}

	time.Sleep(10 * time.Second)
}
