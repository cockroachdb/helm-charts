package rotate

import (
	"fmt"
	"github.com/cockroachdb/helm-charts/tests/testutil"
	"github.com/gruntwork-io/terratest/modules/helm"
	"github.com/gruntwork-io/terratest/modules/k8s"
	"github.com/gruntwork-io/terratest/modules/random"
	"github.com/stretchr/testify/require"
	"os"
	"path/filepath"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"strings"
	"testing"
	"time"
)

var (
	cfg            = ctrl.GetConfigOrDie()
	k8sClient, _   = client.New(cfg, client.Options{})
	releaseName    = "crdb-test"
	customCASecret = "custom-ca-secret"
	imageTag       = os.Getenv("TAG")
)

func TestCockroachDbNodeAndClientCert(t *testing.T) {
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
	//defer k8s.DeleteNamespace(t, kubectlOptions, namespaceName)

	// Setup the args. For this test, we will set the following input values:
	helmValues := map[string]string{
		"tls.selfSigner.image.tag": imageTag,
		"tls.certs.selfSigner.minimumCertDuration":    "24h",
		"tls.certs.selfSigner.caCertDuration":         "720h",
		"tls.certs.selfSigner.caCertExpiryWindow":     "48h",
		"tls.certs.selfSigner.clientCertDuration":     "240h",
		"tls.certs.selfSigner.clientCertExpiryWindow": "24h",
		"tls.certs.selfSigner.nodeCertDuration":       "440h",
		"tls.certs.selfSigner.nodeCertExpiryWindow":   "36h",
	}
	options := &helm.Options{
		KubectlOptions: k8s.NewKubectlOptions("", "", namespaceName),
		SetValues: helmValues,
	}

	//defer helm.Delete(t, options, releaseName, true)

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

	clientCert := k8s.GetSecret(t, kubectlOptions, crdbCluster.ClientSecret)
	nodeCert := k8s.GetSecret(t, kubectlOptions, crdbCluster.NodeSecret)
	testutil.RequireToRunRotateJob(t, crdbCluster, helmValues)
	testutil.RequireCertRotateJobToBeCompleted(t, "rotate-cert-job", crdbCluster, 500*time.Second)
	time.Sleep(20 * time.Second)
	testutil.RequireDatabaseToFunction(t, crdbCluster, true)

	newClientCert := k8s.GetSecret(t, kubectlOptions, crdbCluster.ClientSecret)
	newNodeCert := k8s.GetSecret(t, kubectlOptions, crdbCluster.NodeSecret)

	t.Log("Verify that certificate duration is changed")
	require.NotEqual(t, clientCert.Annotations["certificate-valid-upto"], newClientCert.Annotations["certificate-valid-upto"])
	require.NotEqual(t, nodeCert.Annotations["certificate-valid-upto"], newNodeCert.Annotations["certificate-valid-upto"])
}