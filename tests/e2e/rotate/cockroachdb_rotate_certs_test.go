package rotate

import (
	"fmt"
	"github.com/gruntwork-io/terratest/modules/shell"
	"path"
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

	cmd := shell.Command{
		Command:    "yq",
		Args:       []string{".tls.selfSigner.image.tag", path.Join(helmChartPath, "values.yaml")},
		WorkingDir: ".",
	}

	tagOutput := shell.RunCommandAndGetOutput(t, cmd)
	t.Log(tagOutput)

	k8s.CreateNamespace(t, kubectlOptions, namespaceName)
	// ... and make sure to delete the namespace at the end of the test
	defer k8s.DeleteNamespace(t, kubectlOptions, namespaceName)

	// Setup the args. For this test, we will set the following input values:
	helmValues := map[string]string{
		"tls.selfSigner.image.tag":                    tagOutput,
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

	// Deploy the cockroachdb helm chart and checks installation should succeed.
	err = helm.InstallE(t, options, helmChartPath, releaseName)
	require.NoError(t, err)

	// ... and make sure to delete the helm release at the end of the test.
	defer helm.Delete(t, options, releaseName, true)
	// Print the debug logs in case of test failure.
	defer func() {
		if t.Failed() {
			testutil.PrintDebugLogs(t, kubectlOptions)
		}
	}()

	// Next we wait for the service endpoint
	serviceName := fmt.Sprintf("%s-cockroachdb-public", releaseName)
	k8s.WaitUntilServiceAvailable(t, kubectlOptions, serviceName, 30, 2*time.Second)

	testutil.RequireCertificatesToBeValid(t, crdbCluster)
	testutil.RequireClusterToBeReadyEventuallyTimeout(t, crdbCluster, 500*time.Second)
	time.Sleep(20 * time.Second)
	// This will create a database, a table and insert two rows into that table.
	testutil.RequireCRDBToFunction(t, crdbCluster, false)

	t.Log("Rotating the Client and Node certificate for the CRDB")

	clientCert := k8s.GetSecret(t, kubectlOptions, crdbCluster.ClientSecret)
	nodeCert := k8s.GetSecret(t, kubectlOptions, crdbCluster.NodeSecret)

	// RequireToRunRotateJob will initiate the client and node certificate rotation using a cron schedule to trigger the
	// actual rotation. The cron schedule provide should be greater than the life of the certificate, then only rotation
	// will be triggered. Here the client and node certificates are valid for 10 days and the next cron is scheduled in
	// 26 days, hence it will trigger the client and node certificate rotation.
	testutil.RequireToRunRotateJob(t, crdbCluster, helmValues, "0 0 */26 * *", false)

	// This will wait for the certificate rotation to complete, which will do a rolling restart of the CRDB cluster.
	testutil.RequireCertRotateJobToBeCompleted(t, "client-node-certificate-rotate", crdbCluster, 500*time.Second)

	time.Sleep(20 * time.Second)
	// This will check after rotation the database is working properly.
	testutil.RequireCRDBToFunction(t, crdbCluster, true)

	t.Log("Verify that client and node certificate duration is changed")
	newClientCert := k8s.GetSecret(t, kubectlOptions, crdbCluster.ClientSecret)
	newNodeCert := k8s.GetSecret(t, kubectlOptions, crdbCluster.NodeSecret)
	require.NotEqual(t, clientCert.Annotations["certificate-valid-upto"], newClientCert.Annotations["certificate-valid-upto"])
	require.NotEqual(t, nodeCert.Annotations["certificate-valid-upto"], newNodeCert.Annotations["certificate-valid-upto"])
	t.Log("Client and Node Certificates rotated successfully")

	t.Log("Rotating the CA certificate for the CRDB")
	caCert := k8s.GetSecret(t, kubectlOptions, crdbCluster.CaSecret)

	// RequireToRunRotateJob will initiate the CA certificate rotation using a cron schedule to trigger the actual
	// rotation. The cron schedule provide should be greater than the life of the certificate, then only rotation will
	// be triggered. Here the CA certificates are valid for 28 days and the next cron is scheduled in 29 days, hence it
	// will trigger the CA certificate rotation.
	testutil.RequireToRunRotateJob(t, crdbCluster, helmValues, "0 0 */29 * *", true)

	// This will wait for the certificate rotation to complete, which will do a rolling restart of the CRDB cluster.
	testutil.RequireCertRotateJobToBeCompleted(t, "ca-certificate-rotate", crdbCluster, 500*time.Second)

	time.Sleep(20 * time.Second)
	// This will check after rotation the database is working properly.
	testutil.RequireCRDBToFunction(t, crdbCluster, true)

	t.Log("Verify that CA certificate duration is changed")
	newCaCert := k8s.GetSecret(t, kubectlOptions, crdbCluster.ClientSecret)
	require.NotEqual(t, caCert.Annotations["certificate-valid-upto"], newCaCert.Annotations["certificate-valid-upto"])
	t.Log("CA Certificates rotated successfully")
}
