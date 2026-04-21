package migration

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/cockroachdb/helm-charts/tests/testutil"
	"github.com/gruntwork-io/terratest/modules/k8s"
	"github.com/gruntwork-io/terratest/modules/retry"
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

	CustomResourceBuilder testutil.ClusterBuilder

	HelmInstall
}

func (o *PublicOperator) InstallOperator(t *testing.T) {
	kubectlOptions := k8s.NewKubectlOptions("", "", OperatorNamespace)

	if _, err := k8s.RunKubectlAndGetOutputE(t, kubectlOptions, "get", "crd", "crdbclusters.crdb.cockroachlabs.com"); err != nil {
		t.Logf("Installing CRDs for cockroach-operator")
		k8s.KubectlApply(t, kubectlOptions, "https://raw.githubusercontent.com/cockroachdb/cockroach-operator/master/install/crds.yaml")
	}
	for _, crd := range []string{
		"crdbclusters.crdb.cockroachlabs.com",
	} {
		_, err := retry.DoWithRetryE(t, "wait-for-public-operator-crd", 60, 5*time.Second, func() (string, error) {
			return k8s.RunKubectlAndGetOutputE(t, kubectlOptions, "get", "crd", crd)
		})
		require.NoError(t, err)
	}

	if _, err := k8s.RunKubectlAndGetOutputE(t, kubectlOptions, "get", "deployment", OperatorDeploymentName); err != nil {
		t.Logf("Installing cockroach-operator")
		k8s.KubectlApply(t, kubectlOptions, "https://raw.githubusercontent.com/cockroachdb/cockroach-operator/master/install/operator.yaml")
	}

	t.Log("Waiting for cockroach-operator to be ready")
	waitForOperatorToBeReady(t)

	k8s.WaitUntilServiceAvailable(t, kubectlOptions, "cockroach-operator-webhook-service", 10, 10*time.Second)
	testutil.RequireServiceEndpointsAvailable(t, kubectlOptions, "cockroach-operator-webhook-service", 2*time.Minute)
	t.Log("Installing crdbcluster custom resource")
	if _, err := k8s.GetNamespaceE(t, k8s.NewKubectlOptions("", "", o.Namespace), o.Namespace); err != nil && apierrors.IsNotFound(err) {
		k8s.CreateNamespace(t, k8s.NewKubectlOptions("", "", o.Namespace), o.Namespace)
	}

	crdbCluster := o.CustomResourceBuilder.Cr()
	crdbCluster.Namespace = o.Namespace
	require.NoError(t, o.CrdbCluster.K8sClient.Create(o.Ctx, crdbCluster))
}

func waitForOperatorToBeReady(t *testing.T) {
	kubectlOptions := k8s.NewKubectlOptions("", "", OperatorNamespace)
	// Use retry loop instead of WaitUntilDeploymentAvailable to avoid
	// terratest panic when deployment has zero conditions.
	retry.DoWithRetry(t, "Wait for deployment "+OperatorDeploymentName+" to be provisioned.", 30, 10*time.Second, func() (string, error) {
		deployment, err := k8s.GetDeploymentE(t, kubectlOptions, OperatorDeploymentName)
		if err != nil {
			return "", err
		}
		if deployment.Status.AvailableReplicas < 1 {
			return "", fmt.Errorf("deployment %s not available yet (availableReplicas=%d)", OperatorDeploymentName, deployment.Status.AvailableReplicas)
		}
		return "available", nil
	})
	pods, err := k8s.ListPodsE(t, kubectlOptions, metav1.ListOptions{LabelSelector: "app=cockroach-operator"})
	require.NoError(t, err)
	for _, pod := range pods {
		k8s.WaitUntilPodAvailable(t, kubectlOptions, pod.Name, 10, 10*time.Second)
	}
}
