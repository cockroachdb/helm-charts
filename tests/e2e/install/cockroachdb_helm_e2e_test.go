package integration

import (
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cockroachdb/cockroach-operator/pkg/kube"
	"github.com/cockroachdb/helm-charts/pkg/security"
	util "github.com/cockroachdb/helm-charts/pkg/utils"
	"github.com/cockroachdb/helm-charts/tests/testutil"
	"github.com/gruntwork-io/terratest/modules/helm"
	"github.com/gruntwork-io/terratest/modules/k8s"
	"github.com/gruntwork-io/terratest/modules/random"
	"github.com/gruntwork-io/terratest/modules/shell"
	"github.com/stretchr/testify/require"
	_ "k8s.io/client-go/plugin/pkg/client/auth"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var (
	cfg              = ctrl.GetConfigOrDie()
	k8sClient, _     = client.New(cfg, client.Options{})
	releaseName      = "crdb-test"
	customCASecret   = "custom-ca-secret"
	helmChartPath, _ = filepath.Abs("../../../cockroachdb")
)

func TestCockroachDbHelmInstall(t *testing.T) {
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
	defer testutil.DeleteNamespace(t, k8sClient, namespaceName)

	const testDBName = "testdb"

	// Setup the args. For this test, we will set the following input values:
	options := &helm.Options{
		KubectlOptions: k8s.NewKubectlOptions("", "", namespaceName),
		SetValues: patchHelmValues(map[string]string{
			"operator.enabled":                         "false",
			"conf.cluster-name":                        "test",
			"init.provisioning.enabled":                "true",
			"init.provisioning.databases[0].name":      testDBName,
			"init.provisioning.databases[0].owners[0]": "root",
		}),
	}

	// Deploy the cockroachdb helm chart and checks installation should succeed.
	helm.Install(t, options, helmChartPath, releaseName)
	defer cleanupResources(
		t,
		releaseName,
		kubectlOptions,
		options,
		[]string{
			crdbCluster.CaSecret,
			crdbCluster.ClientSecret,
			crdbCluster.NodeSecret,
		},
	)

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
	testutil.RequireClusterToBeReadyEventuallyTimeout(t, crdbCluster, 600*time.Second)
	time.Sleep(20 * time.Second)
	testutil.RequireCRDBToFunction(t, crdbCluster, false)
	testutil.RequireDatabaseToFunction(t, crdbCluster, testDBName)
}

func TestCockroachDbHelmInstallWithCAProvided(t *testing.T) {
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
	defer testutil.DeleteNamespace(t, k8sClient, namespaceName)

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
		SetValues: patchHelmValues(map[string]string{
			"operator.enabled":                "false",
			"tls.certs.selfSigner.caProvided": "true",
			"tls.certs.selfSigner.caSecret":   customCASecret,
		}),
	}

	// Deploy the cockroachdb helm chart and checks installation should succeed.
	helm.Install(t, options, helmChartPath, releaseName)

	//... and make sure to delete the helm release at the end of the test.
	defer func() {
		cleanupResources(
			t,
			releaseName,
			kubectlOptions,
			options,
			[]string{
				crdbCluster.ClientSecret,
				crdbCluster.NodeSecret,
			},
		)

		// custom user CA certificate secret should not be deleted by pre-delete job
		_, err = k8s.GetSecretE(t, kubectlOptions, crdbCluster.CaSecret)
		require.NoError(t, err)
	}()

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
	testutil.RequireClusterToBeReadyEventuallyTimeout(t, crdbCluster, 600*time.Second)
	time.Sleep(20 * time.Second)
	testutil.RequireCRDBToFunction(t, crdbCluster, false)
}

// Test to check migration from Bring your own certificate method to self-sginer cert utility
func TestCockroachDbHelmMigration(t *testing.T) {
	namespaceName := "cockroach" + strings.ToLower(random.UniqueId())
	kubectlOptions := k8s.NewKubectlOptions("", "", namespaceName)

	crdbCluster := testutil.CockroachCluster{
		Cfg:              cfg,
		K8sClient:        k8sClient,
		StatefulSetName:  fmt.Sprintf("%s-cockroachdb", releaseName),
		Namespace:        namespaceName,
		ClientSecret:     fmt.Sprintf("%s-cockroachdb-root", releaseName),
		NodeSecret:       fmt.Sprintf("%s-cockroachdb-node", releaseName),
		CaSecret:         customCASecret,
		IsCaUserProvided: false,
	}

	certsDir, cleanup := util.CreateTempDir("certsDir")
	defer cleanup()

	cmdCa := shell.Command{
		Command: "cockroach",
		Args: []string{"cert", "create-ca", fmt.Sprintf("--certs-dir=%s", certsDir),
			fmt.Sprintf("--ca-key=%s/ca.key", certsDir)},
		WorkingDir: ".",
		Env:        nil,
		Logger:     nil,
	}

	publicService := crdbCluster.StatefulSetName + "-public"

	hosts := []string{
		"localhost",
		"127.0.0.1",
		publicService,
		fmt.Sprintf("%s.%s", publicService, namespaceName),
		fmt.Sprintf("%s.%s.svc.%s", publicService, namespaceName, "cluster.local"),
		fmt.Sprintf("*.%s", crdbCluster.StatefulSetName),
		fmt.Sprintf("*.%s.%s", crdbCluster.StatefulSetName, namespaceName),
		fmt.Sprintf("*.%s.%s.svc.%s", crdbCluster.StatefulSetName, namespaceName, "cluster.local"),
	}

	cmdNode := shell.Command{
		Command: "cockroach",
		Args: append(append([]string{"cert", "create-node"}, hosts...), fmt.Sprintf("--certs-dir=%s", certsDir),
			fmt.Sprintf("--ca-key=%s/ca.key", certsDir)),
		WorkingDir: ".",
		Env:        nil,
		Logger:     nil,
	}

	cmdClient := shell.Command{
		Command: "cockroach",
		Args: []string{"cert", "create-client", security.RootUser, fmt.Sprintf("--certs-dir=%s", certsDir),
			fmt.Sprintf("--ca-key=%s/ca.key", certsDir)},
		WorkingDir: ".",
		Env:        nil,
		Logger:     nil,
	}

	k8s.CreateNamespace(t, kubectlOptions, namespaceName)
	// Make sure to delete the namespace at the end of the test
	defer testutil.DeleteNamespace(t, k8sClient, namespaceName)

	cmds := []shell.Command{cmdCa, cmdNode, cmdClient}
	for i := range cmds {
		certOutput, err := shell.RunCommandAndGetOutputE(t, cmds[i])
		t.Log(certOutput)
		require.NoError(t, err)
	}

	err := k8s.RunKubectlE(t, kubectlOptions, "create", "secret", "generic", crdbCluster.NodeSecret,
		fmt.Sprintf("--from-file=%s/node.crt", certsDir), fmt.Sprintf("--from-file=%s/node.key", certsDir),
		fmt.Sprintf("--from-file=%s/ca.crt", certsDir))
	require.NoError(t, err)

	err = k8s.RunKubectlE(t, kubectlOptions, "create", "secret", "generic", crdbCluster.ClientSecret,
		fmt.Sprintf("--from-file=%s/client.root.crt", certsDir), fmt.Sprintf("--from-file=%s/client.root.key", certsDir),
		fmt.Sprintf("--from-file=%s/ca.crt", certsDir))
	require.NoError(t, err)

	// Setup the args
	options := &helm.Options{
		KubectlOptions: k8s.NewKubectlOptions("", "", namespaceName),
		SetValues: patchHelmValues(map[string]string{
			"operator.enabled":             "false",
			"tls.certs.provided":           "true",
			"tls.certs.selfSigner.enabled": "false",
			"tls.certs.clientRootSecret":   crdbCluster.ClientSecret,
			"tls.certs.nodeSecret":         crdbCluster.NodeSecret,
		}),
	}

	// Deploy the cockroachdb helm chart and checks installation should succeed.
	helm.Install(t, options, helmChartPath, releaseName)
	defer cleanupResources(t, releaseName, kubectlOptions, options, []string{})

	// Print the debug logs in case of test failure.
	defer func() {
		if t.Failed() {
			testutil.PrintDebugLogs(t, kubectlOptions)
		}
	}()

	// Wait for the service endpoint
	k8s.WaitUntilServiceAvailable(t, kubectlOptions, publicService, 30, 2*time.Second)

	testutil.RequireClusterToBeReadyEventuallyTimeout(t, crdbCluster, 600*time.Second)
	time.Sleep(20 * time.Second)

	// Setup the args for upgrade
	crdbCluster.NodeSecret = fmt.Sprintf("%s-cockroachdb-node-secret", releaseName)
	crdbCluster.ClientSecret = fmt.Sprintf("%s-cockroachdb-client-secret", releaseName)
	crdbCluster.CaSecret = fmt.Sprintf("%s-cockroachdb-ca-secret", releaseName)

	// Default method is self-signer so no need to set explicitly
	options = &helm.Options{
		KubectlOptions: k8s.NewKubectlOptions("", "", namespaceName),
		SetValues: patchHelmValues(map[string]string{
			"operator.enabled":                "false",
			"statefulset.updateStrategy.type": "OnDelete",
		}),
		ExtraArgs: map[string][]string{
			"upgrade": {
				"--timeout=20m",
			},
		},
	}

	// Upgrade the cockroachdb helm chart and checks installation should succeed.
	// Upgrade is done in goRoutine to unblock the code flow
	// While upgrading statefulset pods need to be deleted manually to consume the new certificate chain
	wg := sync.WaitGroup{}
	wg.Add(1)

	go func() {
		defer wg.Done()
		err = helm.UpgradeE(t, options, helmChartPath, releaseName)
		require.NoError(t, err)
	}()

	time.Sleep(30 * time.Second)

	// Delete the pods manually
	err = k8s.RunKubectlE(t, kubectlOptions, "delete", "pods", "-l", "app.kubernetes.io/component=cockroachdb")
	require.NoError(t, err)

	wg.Wait()

	// Wait for the service endpoint
	k8s.WaitUntilServiceAvailable(t, kubectlOptions, publicService, 30, 2*time.Second)

	testutil.RequireCertificatesToBeValid(t, crdbCluster)
	testutil.RequireClusterToBeReadyEventuallyTimeout(t, crdbCluster, 600*time.Second)
	time.Sleep(20 * time.Second)
	testutil.RequireCRDBToFunction(t, crdbCluster, false)
}

func TestCockroachDbWithInsecureMode(t *testing.T) {
	namespaceName := "cockroach" + strings.ToLower(random.UniqueId())
	kubectlOptions := k8s.NewKubectlOptions("", "", namespaceName)

	crdbCluster := testutil.CockroachCluster{
		Cfg:             cfg,
		K8sClient:       k8sClient,
		StatefulSetName: fmt.Sprintf("%s-cockroachdb", releaseName),
		Namespace:       namespaceName,
	}

	k8s.CreateNamespace(t, kubectlOptions, namespaceName)
	// ... and make sure to delete the namespace at the end of the test
	defer testutil.DeleteNamespace(t, k8sClient, namespaceName)

	// Setup the args. For this test, we will set the following input values:
	options := &helm.Options{
		KubectlOptions: k8s.NewKubectlOptions("", "", namespaceName),
		SetValues: patchHelmValues(map[string]string{
			"operator.enabled": "false",
			"tls.enabled":      "false",
		}),
	}

	// Deploy the cockroachdb helm chart and checks installation should succeed.
	helm.Install(t, options, helmChartPath, releaseName)
	defer cleanupResources(t, releaseName, kubectlOptions, options, []string{})

	// Print the debug logs in case of test failure.
	defer func() {
		if t.Failed() {
			testutil.PrintDebugLogs(t, kubectlOptions)
		}
	}()

	// Next we wait for the service endpoint
	serviceName := fmt.Sprintf("%s-cockroachdb-public", releaseName)
	k8s.WaitUntilServiceAvailable(t, kubectlOptions, serviceName, 30, 2*time.Second)

	testutil.RequireClusterToBeReadyEventuallyTimeout(t, crdbCluster, 600*time.Second)
	time.Sleep(20 * time.Second)
	testutil.RequireCRDBToFunction(t, crdbCluster, false)
}

func TestCockroachDbWithCertManager(t *testing.T) {
	namespaceName := "cockroach" + strings.ToLower(random.UniqueId())
	kubectlOptions := k8s.NewKubectlOptions("", "", namespaceName)

	k8s.CreateNamespace(t, kubectlOptions, namespaceName)
	// ... and make sure to delete the namespace at the end of the test
	defer testutil.DeleteNamespace(t, k8sClient, namespaceName)

	certManagerHelmOptions := &helm.Options{
		KubectlOptions: k8s.NewKubectlOptions("", "", "cert-manager"),
	}

	jetStackRepoAdd := []string{"add", "jetstack", "https://charts.jetstack.io", "--force-update"}
	_, err := helm.RunHelmCommandAndGetOutputE(t, &helm.Options{}, "repo", jetStackRepoAdd...)
	require.NoError(t, err)

	certManagerInstall := []string{"cert-manager", "jetstack/cert-manager", "--create-namespace", "--set", "installCRDs=true", "--version", "v1.11.0"}
	output, err := helm.RunHelmCommandAndGetOutputE(t, certManagerHelmOptions, "install", certManagerInstall...)

	require.NoError(t, err)

	//... and make sure to delete the helm release at the end of the test.
	defer func() {
		if t.Failed() {
			t.Log(output)
		}
		helm.Delete(t, certManagerHelmOptions, "cert-manager", true)
		k8s.DeleteNamespace(t, &k8s.KubectlOptions{}, "cert-manager")
	}()

	issuerFile := "ca-issuer.yaml"
	issuerCreateData := fmt.Sprintf(`
apiVersion: cert-manager.io/v1
kind: Issuer
metadata:
  name: cockroachdb
  namespace: %s
spec:
  selfSigned: {}
`, namespaceName)

	err = os.WriteFile(issuerFile, []byte(issuerCreateData), fs.ModePerm)
	require.NoError(t, err)

	defer func() {
		_ = os.Remove(issuerFile)
	}()

	err = k8s.KubectlApplyE(t, &k8s.KubectlOptions{}, issuerFile)
	require.NoError(t, err)

	crdbCluster := testutil.CockroachCluster{
		Cfg:              cfg,
		K8sClient:        k8sClient,
		StatefulSetName:  fmt.Sprintf("%s-cockroachdb", releaseName),
		Namespace:        namespaceName,
		ClientSecret:     "cockroachdb-root",
		NodeSecret:       "cockroachdb-node",
		CaSecret:         "cockroachdb-ca",
		IsCaUserProvided: false,
	}

	// Setup the args. For this test, we will set the following input values:
	options := &helm.Options{
		KubectlOptions: k8s.NewKubectlOptions("", "", namespaceName),
		SetValues: patchHelmValues(map[string]string{
			"operator.enabled":                 "false",
			"tls.enabled":                      "true",
			"tls.certs.selfSigner.enabled":     "false",
			"tls.certs.certManager":            "true",
			"tls.certs.certManagerIssuer.kind": "Issuer",
			"tls.certs.certManagerIssuer.name": "cockroachdb",
		}),
	}

	// Deploy the cockroachdb helm chart and checks installation should succeed.
	helm.Install(t, options, helmChartPath, releaseName)
	defer cleanupResources(t, releaseName, kubectlOptions, options, []string{})

	// Print the debug logs in case of test failure.
	defer func() {
		if t.Failed() {
			testutil.PrintDebugLogs(t, kubectlOptions)
		}
	}()

	// Next we wait for the service endpoint
	serviceName := fmt.Sprintf("%s-cockroachdb-public", releaseName)
	k8s.WaitUntilServiceAvailable(t, kubectlOptions, serviceName, 30, 2*time.Second)

	testutil.RequireClusterToBeReadyEventuallyTimeout(t, crdbCluster, 600*time.Second)
	time.Sleep(20 * time.Second)
	testutil.RequireCRDBToFunction(t, crdbCluster, false)
}

func TestWALFailoverSideDiskExistingCluster(t *testing.T) {
	testWALFailoverExistingCluster(
		t,
		patchHelmValues(map[string]string{
			"operator.enabled":                           "false",
			"conf.wal-failover.value":                    "path=cockroach-failover",
			"conf.wal-failover.persistentVolume.enabled": "true",
			"conf.wal-failover.persistentVolume.size":    "1Gi",
		}),
	)
}

func TestWALFailoverAmongStoresExistingCluster(t *testing.T) {
	testWALFailoverExistingCluster(
		t,
		patchHelmValues(map[string]string{
			"operator.enabled":        "false",
			"conf.wal-failover.value": "among-stores",
			"conf.store.count":        "2",
		}),
	)
}

func testWALFailoverExistingCluster(t *testing.T, additionalValues map[string]string) {
	namespaceName := "cockroach" + strings.ToLower(random.UniqueId())
	numReplicas := 3
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
	defer testutil.DeleteNamespace(t, k8sClient, namespaceName)

	// Print the debug logs in case of test failure.
	defer func() {
		if t.Failed() {
			testutil.PrintDebugLogs(t, kubectlOptions)
		}
	}()

	// Configure options for the initial deployment.
	initialValues := patchHelmValues(map[string]string{
		"operator.enabled":     "false",
		"conf.cluster-name":    "test",
		"conf.store.enabled":   "true",
		"statefulset.replicas": strconv.Itoa(numReplicas),
	})
	options := &helm.Options{
		KubectlOptions: k8s.NewKubectlOptions("", "", namespaceName),
		SetValues:      initialValues,
	}

	// Deploy the helm chart and confirm the installation is successful.
	helm.Install(t, options, helmChartPath, releaseName)
	defer cleanupResources(t, releaseName, kubectlOptions, options, []string{})

	// Wait for the service endpoint to be available.
	serviceName := fmt.Sprintf("%s-cockroachdb-public", releaseName)
	k8s.WaitUntilServiceAvailable(t, kubectlOptions, serviceName, 30, 2*time.Second)
	testutil.RequireClusterToBeReadyEventuallyTimeout(t, crdbCluster, 600*time.Second)

	// Enable WAL Failover and upgrade the cluster.
	// In order to prevent any downtime, we need to follow the below steps for each pod:
	// - delete the statefulset with --cascade=orphan
	// - delete the pod
	// - upgrade the Helm chart

	// Configure options for the updated deployment.
	updatedValues := map[string]string{}
	for k, v := range initialValues {
		updatedValues[k] = v
	}
	for k, v := range additionalValues {
		updatedValues[k] = v
	}
	options = &helm.Options{
		KubectlOptions: k8s.NewKubectlOptions("", "", namespaceName),
		SetValues:      updatedValues,
	}

	updateSinglePod := func(idx int) {
		podName := fmt.Sprintf("%s-%d", crdbCluster.StatefulSetName, idx)
		log.Printf("Request received to update pod %s\n", podName)

		// Delete the statefulset with --cascade=orphan
		log.Println("Deleting the statefulset with --cascade=orphan")
		k8s.RunKubectl(
			t,
			kubectlOptions,
			"delete",
			"statefulset",
			crdbCluster.StatefulSetName,
			"--cascade=orphan",
		)

		// Delete the pod
		log.Printf("Deleting the pod %s\n", podName)
		k8s.RunKubectl(t, kubectlOptions, "delete", "pod", podName)
		testutil.WaitUntilPodDeleted(t, kubectlOptions, podName, 30, 2*time.Second)

		// Upgrade the Helm release
		log.Println("Upgrading the Helm release")
		helm.Upgrade(t, options, helmChartPath, releaseName)
	}

	// Iterate over all pods in the statefulset.
	for idx := 0; idx < numReplicas; idx++ {
		updateSinglePod(idx)

		k8s.WaitUntilServiceAvailable(t, kubectlOptions, serviceName, 30, 2*time.Second)
		testutil.RequireClusterToBeReadyEventuallyTimeout(t, crdbCluster, 600*time.Second)
	}
}

func patchHelmValues(inputValues map[string]string) map[string]string {
	overrides := map[string]string{
		// Override the persistent storage size to 1Gi so that we do not run out of space.
		"storage.persistentVolume.size": "1Gi",
	}

	for k, v := range overrides {
		inputValues[k] = v
	}

	return inputValues
}

func cleanupResources(
	t *testing.T,
	releaseName string,
	kubectlOptions *k8s.KubectlOptions,
	options *helm.Options,
	danglingSecrets []string,
) {
	err := helm.DeleteE(t, options, releaseName, true)
	// Ignore the error if the operation timed out.
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		require.NoError(t, err)
	} else {
		t.Logf("Error while deleting helm release: %v", err)
	}

	for i := range danglingSecrets {
		_, err = k8s.GetSecretE(t, kubectlOptions, danglingSecrets[i])
		require.Equal(t, true, kube.IsNotFound(err))
		t.Logf("Secret %s deleted by helm uninstall", danglingSecrets[i])
	}
}
