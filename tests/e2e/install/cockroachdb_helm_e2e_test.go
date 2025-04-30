package install

import (
	"fmt"
	"log"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cockroachdb/helm-charts/pkg/security"
	util "github.com/cockroachdb/helm-charts/pkg/utils"
	"github.com/cockroachdb/helm-charts/tests/testutil"
	"github.com/cockroachdb/helm-charts/tests/testutil/migration"
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
	k3dClusterName   = "k3d-chart-testing-cluster"
	ClientSecret     = fmt.Sprintf("%s-cockroachdb-client-secret", releaseName)
	NodeSecret       = fmt.Sprintf("%s-cockroachdb-node-secret", releaseName)
	CASecret         = fmt.Sprintf("%s-cockroachdb-ca-secret", releaseName)
)

type CockroachDBHelm struct {
	migration.HelmInstall
}

func newCockroachDBHelm() *CockroachDBHelm {
	return &CockroachDBHelm{}
}

func TestHelmChartInstall(t *testing.T) {
	h := newCockroachDBHelm()
	t.Run("CockroachDB Helm Installl", h.TestHelmInstall)
	t.Run("CockroachDB Helm Install with user provided CA", h.TestHelmInstallWithCAProvided)
	t.Run("CockroachDB Helm Chart with cert migration", h.TestHelmMigration)
	t.Run("CockroachDB Helm Chart with insecure mode", h.TestHelmWithInsecureMode)
	t.Run("CockroachDB Helm Chart with cert manager", h.TestWithCertManager)
	t.Run("WAL Failover among stores", h.TestWALFailoverAmongStoresExistingCluster)
	t.Run("WAL Failover on Side disks", h.TestWALFailoverSideDiskExistingCluster)
}

func (h *CockroachDBHelm) TestHelmInstall(t *testing.T) {
	isCaUserProvided := false
	h.Namespace = "cockroach" + strings.ToLower(random.UniqueId())
	h.CrdbCluster = testutil.CockroachCluster{
		Cfg:              cfg,
		K8sClient:        k8sClient,
		StatefulSetName:  fmt.Sprintf("%s-cockroachdb", releaseName),
		Namespace:        h.Namespace,
		ClientSecret:     ClientSecret,
		NodeSecret:       NodeSecret,
		CaSecret:         CASecret,
		IsCaUserProvided: isCaUserProvided,
		Context:          k3dClusterName,
		DesiredNodes:     3,
	}
	h.HelmOptions = &helm.Options{
		SetValues: testutil.PatchHelmValues(map[string]string{
			"operator.enabled":                         "false",
			"conf.cluster-name":                        "test",
			"init.provisioning.enabled":                "true",
			"init.provisioning.databases[0].name":      migration.TestDBName,
			"init.provisioning.databases[0].owners[0]": "root",
		}),
	}

	h.InstallHelm(t)
	defer func() {
		h.Uninstall(t)
		k8s.DeleteNamespace(t, k8s.NewKubectlOptions("", "", h.Namespace), h.Namespace)
	}()
	h.ValidateCRDB(t)
}

func (h *CockroachDBHelm) TestHelmInstallWithCAProvided(t *testing.T) {
	h.Namespace = "cockroach" + strings.ToLower(random.UniqueId())
	isCaUserProvided := true

	kubectlOptions := k8s.NewKubectlOptions("", "", h.Namespace)
	cmd := shell.Command{
		Command:    "cockroach",
		Args:       []string{"cert", "create-ca", "--certs-dir=.", "--ca-key=ca.key"},
		WorkingDir: ".",
		Env:        nil,
		Logger:     nil,
	}
	certOutput, err := shell.RunCommandAndGetOutputE(t, cmd)
	t.Log(certOutput)
	require.NoError(t, err)

	k8s.CreateNamespace(t, kubectlOptions, h.Namespace)
	err = k8s.RunKubectlE(t, kubectlOptions, "create", "secret", "generic", customCASecret, "--from-file=ca.crt",
		"--from-file=ca.key")
	require.NoError(t, err)

	cmd = shell.Command{
		Command:    "rm",
		Args:       []string{"-rf", "ca.crt", "ca.key"},
		WorkingDir: ".",
	}

	defer shell.RunCommand(t, cmd)

	h.CrdbCluster = testutil.CockroachCluster{
		Cfg:              cfg,
		K8sClient:        k8sClient,
		StatefulSetName:  fmt.Sprintf("%s-cockroachdb", releaseName),
		Namespace:        h.Namespace,
		ClientSecret:     ClientSecret,
		NodeSecret:       NodeSecret,
		CaSecret:         customCASecret,
		IsCaUserProvided: isCaUserProvided,
		Context:          k3dClusterName,
		DesiredNodes:     3,
	}

	// Setup the args. For this test, we will set the following input values:
	h.HelmOptions = &helm.Options{
		SetValues: testutil.PatchHelmValues(map[string]string{
			"operator.enabled":                "false",
			"tls.certs.selfSigner.caProvided": "true",
			"tls.certs.selfSigner.caSecret":   customCASecret,
		}),
	}

	h.InstallHelm(t)
	defer func() {
		h.Uninstall(t)
		k8s.DeleteNamespace(t, kubectlOptions, h.Namespace)
	}()
	h.ValidateCRDB(t)
}

func (h *CockroachDBHelm) TestHelmMigration(t *testing.T) {
	h.Namespace = "cockroach" + strings.ToLower(random.UniqueId())
	kubectlOptions := k8s.NewKubectlOptions("", "", h.Namespace)

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

	publicService := fmt.Sprintf("%s-cockroachdb-public", migration.ReleaseName)

	hosts := []string{
		"localhost",
		"127.0.0.1",
		publicService,
		fmt.Sprintf("%s.%s", publicService, h.Namespace),
		fmt.Sprintf("%s.%s.svc.%s", publicService, h.Namespace, "cluster.local"),
		fmt.Sprintf("*.%s", fmt.Sprintf("%s-cockroachdb", migration.ReleaseName)),
		fmt.Sprintf("*.%s.%s", fmt.Sprintf("%s-cockroachdb", migration.ReleaseName), h.Namespace),
		fmt.Sprintf("*.%s.%s.svc.%s", fmt.Sprintf("%s-cockroachdb", migration.ReleaseName), h.Namespace, "cluster.local"),
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

	cmds := []shell.Command{cmdCa, cmdNode, cmdClient}
	for i := range cmds {
		certOutput, err := shell.RunCommandAndGetOutputE(t, cmds[i])
		t.Log(certOutput)
		require.NoError(t, err)
	}

	k8s.CreateNamespace(t, kubectlOptions, h.Namespace)
	err := k8s.RunKubectlE(t, kubectlOptions, "create", "secret", "generic", NodeSecret,
		fmt.Sprintf("--from-file=%s/node.crt", certsDir), fmt.Sprintf("--from-file=%s/node.key", certsDir),
		fmt.Sprintf("--from-file=%s/ca.crt", certsDir))
	require.NoError(t, err)

	err = k8s.RunKubectlE(t, kubectlOptions, "create", "secret", "generic", ClientSecret,
		fmt.Sprintf("--from-file=%s/client.root.crt", certsDir), fmt.Sprintf("--from-file=%s/client.root.key", certsDir),
		fmt.Sprintf("--from-file=%s/ca.crt", certsDir))
	require.NoError(t, err)

	h.CrdbCluster = testutil.CockroachCluster{
		Cfg:              cfg,
		K8sClient:        k8sClient,
		StatefulSetName:  fmt.Sprintf("%s-cockroachdb", releaseName),
		Namespace:        h.Namespace,
		ClientSecret:     ClientSecret,
		NodeSecret:       NodeSecret,
		CaSecret:         "",
		IsCaUserProvided: false,
		Context:          k3dClusterName,
		DesiredNodes:     3,
	}

	// Setup the args.
	h.HelmOptions = &helm.Options{
		SetValues: testutil.PatchHelmValues(map[string]string{
			"operator.enabled":             "false",
			"tls.certs.provided":           "true",
			"tls.certs.selfSigner.enabled": "false",
			"tls.certs.clientRootSecret":   ClientSecret,
			"tls.certs.nodeSecret":         NodeSecret,
		}),
	}

	h.InstallHelm(t)
	defer func() {
		h.Uninstall(t)
		k8s.DeleteNamespace(t, kubectlOptions, h.Namespace)
	}()

	// Setup the args for upgrade.
	h.CrdbCluster.NodeSecret = fmt.Sprintf("%s-cockroachdb-node-secret", releaseName)
	h.CrdbCluster.ClientSecret = fmt.Sprintf("%s-cockroachdb-client-secret", releaseName)
	h.CrdbCluster.CaSecret = fmt.Sprintf("%s-cockroachdb-ca-secret", releaseName)

	// Default method is self-signer so no need to set explicitly.
	h.HelmOptions = &helm.Options{
		KubectlOptions: k8s.NewKubectlOptions("", "", h.Namespace),
		SetValues: testutil.PatchHelmValues(map[string]string{
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
	// Upgrade is done in goRoutine to unblock the code flow.
	// While upgrading statefulset pods need to be deleted manually to consume the new certificate chain.
	wg := sync.WaitGroup{}
	wg.Add(1)

	go func() {
		defer wg.Done()
		err = helm.UpgradeE(t, h.HelmOptions, helmChartPath, releaseName)
		require.NoError(t, err)
	}()

	time.Sleep(30 * time.Second)

	// Delete the pods manually
	err = k8s.RunKubectlE(t, kubectlOptions, "delete", "pods", "-l", "app.kubernetes.io/component=cockroachdb")
	require.NoError(t, err)

	wg.Wait()

	// Wait for the service endpoint.
	k8s.WaitUntilServiceAvailable(t, kubectlOptions, publicService, 30, 2*time.Second)

	h.ValidateCRDB(t)
}

func (h *CockroachDBHelm) TestHelmWithInsecureMode(t *testing.T) {
	h.Namespace = "cockroach" + strings.ToLower(random.UniqueId())
	h.CrdbCluster = testutil.CockroachCluster{
		Cfg:             cfg,
		K8sClient:       k8sClient,
		StatefulSetName: fmt.Sprintf("%s-cockroachdb", releaseName),
		Namespace:       h.Namespace,
		Context:         k3dClusterName,
		DesiredNodes:    3,
	}
	// Setup the args. For this test, we will set the following input values:
	h.HelmOptions = &helm.Options{
		SetValues: testutil.PatchHelmValues(map[string]string{
			"operator.enabled": "false",
			"tls.enabled":      "false",
		}),
	}

	h.InstallHelm(t)
	defer func() {
		h.Uninstall(t)
		k8s.DeleteNamespace(t, k8s.NewKubectlOptions("", "", h.Namespace), h.Namespace)
	}()
	h.ValidateCRDB(t)
}

func (h *CockroachDBHelm) TestWithCertManager(t *testing.T) {
	h.Namespace = "cockroach" + strings.ToLower(random.UniqueId())

	k8sOptions := k8s.NewKubectlOptions("", "", h.Namespace)
	certManagerK8sOptions := k8s.NewKubectlOptions("", "", testutil.CertManagerNamespace)
	k8s.CreateNamespace(t, k8sOptions, h.Namespace)
	testutil.InstallCertManager(t, certManagerK8sOptions)
	//... and make sure to delete the helm release at the end of the test.
	defer func() {
		testutil.DeleteCertManager(t, certManagerK8sOptions)
		k8s.DeleteNamespace(t, certManagerK8sOptions, testutil.CertManagerNamespace)
	}()

	testutil.CreateSelfSignedIssuer(t, k8sOptions, h.Namespace)

	h.CrdbCluster = testutil.CockroachCluster{
		Cfg:              cfg,
		K8sClient:        k8sClient,
		StatefulSetName:  fmt.Sprintf("%s-cockroachdb", releaseName),
		Namespace:        h.Namespace,
		ClientSecret:     "cockroachdb-root",
		NodeSecret:       "cockroachdb-node",
		CaSecret:         "cockroach-ca",
		IsCaUserProvided: false,
		Context:          k3dClusterName,
		DesiredNodes:     3,
	}

	// Setup the args. For this test, we will set the following input values:
	h.HelmOptions = &helm.Options{
		SetValues: testutil.PatchHelmValues(map[string]string{
			"operator.enabled":                 "false",
			"tls.enabled":                      "true",
			"tls.certs.selfSigner.enabled":     "false",
			"tls.certs.certManager":            "true",
			"tls.certs.certManagerIssuer.kind": "Issuer",
			"tls.certs.certManagerIssuer.name": testutil.SelfSignedIssuerName,
		}),
	}

	h.InstallHelm(t)
	defer func() {
		h.Uninstall(t)
		k8s.DeleteNamespace(t, k8sOptions, h.Namespace)
	}()
	h.ValidateCRDB(t)
}

func (h *CockroachDBHelm) TestWALFailoverSideDiskExistingCluster(t *testing.T) {
	testWALFailoverExistingCluster(
		t,
		h,
		testutil.PatchHelmValues(map[string]string{
			"operator.enabled":                           "false",
			"conf.wal-failover.value":                    "path=cockroach-failover",
			"conf.wal-failover.persistentVolume.enabled": "true",
			"conf.wal-failover.persistentVolume.size":    "1Gi",
		}),
	)
}

func (h *CockroachDBHelm) TestWALFailoverAmongStoresExistingCluster(t *testing.T) {
	testWALFailoverExistingCluster(
		t,
		h,
		testutil.PatchHelmValues(map[string]string{
			"operator.enabled":        "false",
			"conf.wal-failover.value": "among-stores",
			"conf.store.count":        "2",
		}),
	)
}

func testWALFailoverExistingCluster(t *testing.T, h *CockroachDBHelm, additionalValues map[string]string) {
	h.Namespace = "cockroach" + strings.ToLower(random.UniqueId())
	numReplicas := 3
	kubectlOptions := k8s.NewKubectlOptions("", "", h.Namespace)

	h.CrdbCluster = testutil.CockroachCluster{
		Cfg:              cfg,
		K8sClient:        k8sClient,
		StatefulSetName:  fmt.Sprintf("%s-cockroachdb", releaseName),
		Namespace:        h.Namespace,
		ClientSecret:     fmt.Sprintf("%s-cockroachdb-client-secret", releaseName),
		NodeSecret:       fmt.Sprintf("%s-cockroachdb-node-secret", releaseName),
		CaSecret:         fmt.Sprintf("%s-cockroachdb-ca-secret", releaseName),
		IsCaUserProvided: false,
		Context:          k3dClusterName,
		DesiredNodes:     numReplicas,
	}

	// Configure options for the initial deployment.
	initialValues := testutil.PatchHelmValues(map[string]string{
		"operator.enabled":     "false",
		"conf.cluster-name":    "test",
		"conf.store.enabled":   "true",
		"statefulset.replicas": strconv.Itoa(numReplicas),
	})
	h.HelmOptions = &helm.Options{
		KubectlOptions: k8s.NewKubectlOptions("", "", h.Namespace),
		SetValues:      initialValues,
	}

	h.InstallHelm(t)
	defer func() {
		h.Uninstall(t)
		k8s.DeleteNamespace(t, kubectlOptions, h.Namespace)
	}()
	h.ValidateCRDB(t)

	serviceName := fmt.Sprintf("%s-cockroachdb-public", releaseName)

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
	h.HelmOptions = &helm.Options{
		KubectlOptions: k8s.NewKubectlOptions("", "", h.Namespace),
		SetValues:      updatedValues,
	}

	updateSinglePod := func(idx int) {
		podName := fmt.Sprintf("%s-%d", h.CrdbCluster.StatefulSetName, idx)
		log.Printf("Request received to update pod %s\n", podName)

		// Delete the statefulset with --cascade=orphan
		log.Println("Deleting the statefulset with --cascade=orphan")
		k8s.RunKubectl(
			t,
			kubectlOptions,
			"delete",
			"statefulset",
			h.CrdbCluster.StatefulSetName,
			"--cascade=orphan",
		)

		// Delete the pod
		log.Printf("Deleting the pod %s\n", podName)
		k8s.RunKubectl(t, kubectlOptions, "delete", "pod", podName)
		testutil.WaitUntilPodDeleted(t, kubectlOptions, podName, 30, 2*time.Second)

		// Upgrade the Helm release
		log.Println("Upgrading the Helm release")
		helm.Upgrade(t, h.HelmOptions, helmChartPath, releaseName)
	}

	// Iterate over all pods in the statefulset.
	for idx := 0; idx < numReplicas; idx++ {
		updateSinglePod(idx)

		k8s.WaitUntilServiceAvailable(t, kubectlOptions, serviceName, 30, 2*time.Second)
		testutil.RequireClusterToBeReadyEventuallyTimeout(t, h.CrdbCluster, 600*time.Second)
	}
}
