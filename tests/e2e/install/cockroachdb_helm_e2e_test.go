package integration

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cockroachdb/helm-charts/tests/testutil"
	"github.com/gruntwork-io/terratest/modules/helm"
	"github.com/gruntwork-io/terratest/modules/k8s"
	"github.com/gruntwork-io/terratest/modules/shell"
	_ "k8s.io/client-go/plugin/pkg/client/auth"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/gruntwork-io/terratest/modules/random"
	"github.com/stretchr/testify/require"
)

var (
	cfg            = ctrl.GetConfigOrDie()
	k8sClient, _   = client.New(cfg, client.Options{})
	releaseName    = "crdb-test"
	customCASecret = "custom-ca-secret"
	imageTag       = os.Getenv("GITHUB_SHA")
)

func TestCockroachDbHelmInstall(t *testing.T) {
	// Path to the helm chart we will test
	helmChartPath, err := filepath.Abs("../../../cockroachdb")
	require.NoError(t, err)

	namespaceName := "cockroach" + strings.ToLower(random.UniqueId())

	kubectlOptions := k8s.NewKubectlOptions("", "", namespaceName)

	crdbCluster := testutil.CockroachCluster{
		Cfg:              cfg,
		K8sClient:        k8sClient,
		StatefulSetName:  fmt.Sprintf("%s-cockroachdb", releaseName),
		Namespace:        namespaceName,
		ClientSecret:     fmt.Sprintf("%s-cockroachdb-client-secret", releaseName),
		NodeSecret:       fmt.Sprintf("%s-cockroachdb-node-secret", releaseName),
		CaSecret:         fmt.Sprintf("%s-cockroachdb-ca-secret", releaseName),
		IsCaUserProvided: false,
	}

	k8s.CreateNamespace(t, kubectlOptions, namespaceName)
	// ... and make sure to delete the namespace at the end of the test
	defer k8s.DeleteNamespace(t, kubectlOptions, namespaceName)

	// Setup the args. For this test, we will set the following input values:
	options := &helm.Options{
		KubectlOptions: k8s.NewKubectlOptions("", "", namespaceName),
		SetValues: map[string]string{
			"tls.selfSigner.image.tag":      imageTag,
			"storage.persistentVolume.size": "1Gi",
		},
	}

	defer helm.Delete(t, options, releaseName, true)
	defer func() {
		if t.Failed() {
			testutil.PrintDebugLogs(t, kubectlOptions)
		}
	}()

	// Deploy the chart using `helm install`. Note that we use the version without `E`, since we want to assert the
	// install succeeds without any errors.
	helm.Install(t, options, helmChartPath, releaseName)

	// Next we wait for the service endpoint
	serviceName := fmt.Sprintf("%s-cockroachdb-public", releaseName)

	k8s.WaitUntilServiceAvailable(t, kubectlOptions, serviceName, 30, 2*time.Second)

	testutil.RequireCertificatesToBeValid(t, crdbCluster)
	testutil.RequireClusterToBeReadyEventuallyTimeout(t, crdbCluster, 500*time.Second)
	time.Sleep(20 * time.Second)
	testutil.RequireDatabaseToFunction(t, crdbCluster, false)
}

func TestCockroachDbHelmInstallWithCAProvided(t *testing.T) {
	// Path to the helm chart we will test
	helmChartPath, err := filepath.Abs("../../../cockroachdb")
	require.NoError(t, err)

	namespaceName := "cockroach" + strings.ToLower(random.UniqueId())
	kubectlOptions := k8s.NewKubectlOptions("", "", namespaceName)

	crdbCluster := testutil.CockroachCluster{
		Cfg:              cfg,
		K8sClient:        k8sClient,
		StatefulSetName:  fmt.Sprintf("%s-cockroachdb", releaseName),
		Namespace:        namespaceName,
		ClientSecret:     fmt.Sprintf("%s-cockroachdb-client-secret", releaseName),
		NodeSecret:       fmt.Sprintf("%s-cockroachdb-node-secret", releaseName),
		CaSecret:         customCASecret,
		IsCaUserProvided: true,
	}

	cmd := shell.Command{
		Command:    "cockroach",
		Args:       []string{"cert", "create-ca", "--certs-dir=.", "--ca-key=ca.key"},
		WorkingDir: ".",
		Env:        nil,
		Logger:     nil,
	}

	k8s.CreateNamespace(t, kubectlOptions, namespaceName)
	// ... and make sure to delete the namespace at the end of the test
	defer k8s.DeleteNamespace(t, kubectlOptions, namespaceName)

	certOutput, err := shell.RunCommandAndGetOutputE(t, cmd)
	t.Log(certOutput)
	require.NoError(t, err)

	err = k8s.RunKubectlE(t, kubectlOptions, "create", "secret", "generic", customCASecret, "--from-file=ca.crt",
		"--from-file=ca.key")
	require.NoError(t, err)

	cmd = shell.Command{
		Command:    "rm",
		Args:       []string{"-rf", "ca.crt", "ca.key"},
		WorkingDir: ".",
	}

	defer shell.RunCommand(t, cmd)

	// Setup the args. For this test, we will set the following input values:
	options := &helm.Options{
		KubectlOptions: k8s.NewKubectlOptions("", "", namespaceName),
		SetValues: map[string]string{
			"tls.selfSigner.image.tag":        imageTag,
			"tls.certs.selfSigner.caProvided": "true",
			"tls.certs.selfSigner.caSecret":   customCASecret,
			"storage.persistentVolume.size":   "1Gi",
		},
	}

	defer helm.Delete(t, options, releaseName, true)
	defer func() {
		if t.Failed() {
			testutil.PrintDebugLogs(t, kubectlOptions)
		}
	}()

	// Deploy the chart using `helm install`. Note that we use the version without `E`, since we want to assert the
	// install succeeds without any errors.
	helm.Install(t, options, helmChartPath, releaseName)

	// Next we wait for the service endpoint
	serviceName := fmt.Sprintf("%s-cockroachdb-public", releaseName)

	k8s.WaitUntilServiceAvailable(t, kubectlOptions, serviceName, 30, 2*time.Second)
	testutil.RequireCertificatesToBeValid(t, crdbCluster)
	testutil.RequireClusterToBeReadyEventuallyTimeout(t, crdbCluster, 500*time.Second)
	time.Sleep(20 * time.Second)
	testutil.RequireDatabaseToFunction(t, crdbCluster, false)
}
