package rotate

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
	"github.com/gruntwork-io/terratest/modules/random"
	"github.com/stretchr/testify/require"
	_ "k8s.io/client-go/plugin/pkg/client/auth"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var (
	cfg          = ctrl.GetConfigOrDie()
	k8sClient, _ = client.New(cfg, client.Options{})
	releaseName  = "crdb-test"
	imageTag     = os.Getenv("GITHUB_SHA")
)

func TestCockroachDbRotateCertificates(t *testing.T) {
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
		"tls.selfSigner.image.tag":                    imageTag,
		"storage.persistentVolume.size":               "1Gi",
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
		SetValues:      helmValues,
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

	t.Log("Rotate the Client and Node certificate for the CRDB")
	clientCert := k8s.GetSecret(t, kubectlOptions, crdbCluster.ClientSecret)
	nodeCert := k8s.GetSecret(t, kubectlOptions, crdbCluster.NodeSecret)
	testutil.RequireToRunRotateJob(t, crdbCluster, helmValues, false)
	testutil.RequireCertRotateJobToBeCompleted(t, "client-node-certificate-rotate", crdbCluster, 500*time.Second)
	testutil.RequireDatabaseToFunction(t, crdbCluster, true)

	newClientCert := k8s.GetSecret(t, kubectlOptions, crdbCluster.ClientSecret)
	newNodeCert := k8s.GetSecret(t, kubectlOptions, crdbCluster.NodeSecret)

	t.Log("Verify that client and node certificate duration is changed")
	require.NotEqual(t, clientCert.Annotations["certificate-valid-upto"], newClientCert.Annotations["certificate-valid-upto"])
	require.NotEqual(t, nodeCert.Annotations["certificate-valid-upto"], newNodeCert.Annotations["certificate-valid-upto"])
	t.Log("Client and Node Certificates rotated successfully")

	t.Log("Rotate the CA certificate for the CRDB")
	caCert := k8s.GetSecret(t, kubectlOptions, crdbCluster.CaSecret)
	testutil.RequireToRunRotateJob(t, crdbCluster, helmValues, true)
	testutil.RequireCertRotateJobToBeCompleted(t, "ca-certificate-rotate", crdbCluster, 500*time.Second)
	testutil.RequireDatabaseToFunction(t, crdbCluster, true)

	newCaCert := k8s.GetSecret(t, kubectlOptions, crdbCluster.ClientSecret)

	t.Log("Verify that CA certificate duration is changed")
	require.NotEqual(t, caCert.Annotations["certificate-valid-upto"], newCaCert.Annotations["certificate-valid-upto"])
	t.Log("CA Certificates rotated successfully")
}
