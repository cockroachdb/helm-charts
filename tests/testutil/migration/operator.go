package migration

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cockroachdb/helm-charts/tests/testutil"
	"github.com/gruntwork-io/terratest/modules/k8s"
	"github.com/gruntwork-io/terratest/modules/retry"
	"github.com/gruntwork-io/terratest/modules/shell"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	publicOperatorVersion                  = "v2.18.3"
	OperatorDeploymentName                 = "cockroach-operator-manager"
	OperatorNamespace                      = "cockroach-operator-system"
	OperatorServiceName                    = "cockroach-operator-webhook-service"
	OperatorServiceAccountName             = "cockroach-operator-sa"
	OperatorClusterRoleName                = "cockroach-operator-role"
	OperatorClusterRoleBindingName         = "cockroach-operator-rolebinding"
	OperatorValidatingWebhookConfiguration = "cockroach-operator-validating-webhook-configuration"
	OperatorMutatingWebhookConfiguration   = "cockroach-operator-mutating-webhook-configuration"
)

var CockroachVersion = cockroachVersionFromChart()

type PublicOperator struct {
	Ctx context.Context

	CustomResourceBuilder testutil.ClusterBuilder

	HelmInstall
}

func cockroachVersionFromChart() string {
	valuesPath := filepath.Join(testutil.GetGitRoot(), "cockroachdb-parent/charts/cockroachdb/values.yaml")
	valuesBytes, err := os.ReadFile(valuesPath)
	if err != nil {
		panic(err)
	}

	var values struct {
		CockroachDB struct {
			CRDBCluster struct {
				Image struct {
					Name string `yaml:"name"`
				} `yaml:"image"`
			} `yaml:"crdbCluster"`
		} `yaml:"cockroachdb"`
	}
	if err := yaml.Unmarshal(valuesBytes, &values); err != nil {
		panic(err)
	}

	if values.CockroachDB.CRDBCluster.Image.Name == "" {
		panic("cockroachdb.crdbCluster.image.name must be set")
	}
	return values.CockroachDB.CRDBCluster.Image.Name
}

func (o *PublicOperator) InstallOperator(t *testing.T) {
	kubectlOptions := o.kubectlOptions(OperatorNamespace)

	if _, err := k8s.RunKubectlAndGetOutputE(t, kubectlOptions, "get", "crd", "crdbclusters.crdb.cockroachlabs.com"); err != nil {
		t.Logf("Installing CRDs for cockroach-operator")
		applyRemoteManifestWithRetry(t, kubectlOptions,
			"public operator CRDs",
			"https://api.github.com/repos/cockroachdb/cockroach-operator/contents/install/crds.yaml?ref="+publicOperatorVersion)
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
		applyRemoteManifestWithRetry(t, kubectlOptions,
			"public operator",
			"https://api.github.com/repos/cockroachdb/cockroach-operator/contents/install/operator.yaml?ref="+publicOperatorVersion)
	}

	t.Log("Waiting for cockroach-operator to be ready")
	waitForOperatorToBeReady(t, kubectlOptions)

	k8s.WaitUntilServiceAvailable(t, kubectlOptions, OperatorServiceName, 10, 10*time.Second)
	testutil.RequireServiceEndpointsAvailable(t, kubectlOptions, OperatorServiceName, 2*time.Minute)
	t.Log("Installing crdbcluster custom resource")
	clusterKubectlOptions := o.kubectlOptions(o.Namespace)
	if _, err := k8s.GetNamespaceE(t, clusterKubectlOptions, o.Namespace); err != nil && apierrors.IsNotFound(err) {
		k8s.CreateNamespace(t, clusterKubectlOptions, o.Namespace)
	}

	crdbCluster := o.CustomResourceBuilder.Cr()
	crdbCluster.Namespace = o.Namespace
	_, err := retry.DoWithRetryE(t, "wait for public operator admission webhook", 30, 2*time.Second, func() (string, error) {
		err := o.CrdbCluster.K8sClient.Create(o.Ctx, crdbCluster)
		if err == nil || apierrors.IsAlreadyExists(err) {
			return "public operator admission webhook is ready", nil
		}
		if !isTransientWebhookError(err) {
			return "", retry.FatalError{Underlying: err}
		}
		return "", err
	})
	require.NoError(t, err)
}

func isTransientWebhookError(err error) bool {
	if apierrors.IsInternalError(err) || apierrors.IsServiceUnavailable(err) ||
		apierrors.IsTimeout(err) || apierrors.IsServerTimeout(err) {
		return true
	}

	message := strings.ToLower(err.Error())
	return strings.Contains(message, "failed calling webhook") &&
		(strings.Contains(message, "connection refused") ||
			strings.Contains(message, "bad gateway") ||
			strings.Contains(message, "code 502") ||
			strings.Contains(message, "no endpoints available"))
}

func applyRemoteManifestWithRetry(
	t *testing.T, kubectlOptions *k8s.KubectlOptions, description, manifestURL string,
) {
	t.Helper()

	manifestFile, err := os.CreateTemp("", "public-operator-manifest-*.yaml")
	require.NoError(t, err)
	manifestPath := manifestFile.Name()
	require.NoError(t, manifestFile.Close())
	defer func() { _ = os.Remove(manifestPath) }()

	shell.RunCommand(t, shell.Command{
		Command: "curl",
		Args: []string{
			"--fail",
			"--silent",
			"--show-error",
			"--location",
			"--header", "Accept: application/vnd.github.raw+json",
			"--retry", "5",
			"--retry-all-errors",
			"--retry-delay", "2",
			"--connect-timeout", "15",
			"--max-time", "120",
			"--output", manifestPath,
			manifestURL,
		},
	})

	_, err = retry.DoWithRetryE(t, "apply "+description, 5, 5*time.Second, func() (string, error) {
		if err := k8s.KubectlApplyE(t, kubectlOptions, manifestPath); err != nil {
			return "", err
		}
		return description + " applied", nil
	})
	require.NoError(t, err)
}

func waitForOperatorToBeReady(t *testing.T, kubectlOptions *k8s.KubectlOptions) {
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
