package migrate

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	api "github.com/cockroachdb/cockroach-operator/apis/v1alpha1"
	"github.com/cockroachdb/helm-charts/tests/e2e/operator"
	"github.com/cockroachdb/helm-charts/tests/testutil"
	"github.com/cockroachdb/helm-charts/tests/testutil/migration"
	"github.com/gruntwork-io/terratest/modules/helm"
	"github.com/gruntwork-io/terratest/modules/k8s"
	"github.com/gruntwork-io/terratest/modules/random"
	"github.com/gruntwork-io/terratest/modules/retry"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type PublicOperatorToCockroachDbOperator struct {
	migration.PublicOperator
}

func newPublicOperatorToCockroachDbOperator() *PublicOperatorToCockroachDbOperator {
	return &PublicOperatorToCockroachDbOperator{}
}

func TestPublicOperatorToCockroachDbOperator(t *testing.T) {
	h := newPublicOperatorToCockroachDbOperator()
	h.TestDefaultMigration(t)
}

func (o *PublicOperatorToCockroachDbOperator) TestDefaultMigration(t *testing.T) {
	o.Namespace = "cockroach" + strings.ToLower(random.UniqueId())
	kubectlOptions := k8s.NewKubectlOptions("", "", o.Namespace)
	k8s.CreateNamespace(t, k8s.NewKubectlOptions("", "", o.Namespace), o.Namespace)

	// Clean up any CRDs from previous test runs to avoid storedVersions conflicts
	t.Log("Cleaning up CRDs and instances from previous test runs")

	// Helper to patch finalizers and delete resources
	cleanupResources := func(resourceType string) {
		// Get all resources of this type
		output, err := k8s.RunKubectlAndGetOutputE(t, kubectlOptions, "get", resourceType, "--all-namespaces", "-o", "jsonpath={.items[*].metadata.name}")
		if err == nil && output != "" {
			resources := strings.Split(output, " ")
			for _, res := range resources {
				// Patch finalizers to empty to ensure deletion doesn't hang
				k8s.RunKubectl(t, kubectlOptions, "patch", resourceType, res, "-p", "{\"metadata\":{\"finalizers\":[]}}", "--type=merge")
				// Delete the resource
				k8s.RunKubectl(t, kubectlOptions, "delete", resourceType, res, "--ignore-not-found=true")
			}
		}
	}

	// Clean up instances first
	cleanupResources("crdbclusters.crdb.cockroachlabs.com")
	cleanupResources("crdbnodes.crdb.cockroachlabs.com")
	cleanupResources("crdbtenants.crdb.cockroachlabs.com")

	// Now delete CRDs
	k8s.RunKubectl(t, kubectlOptions, "delete", "crd", "crdbclusters.crdb.cockroachlabs.com", "--ignore-not-found=true", "--wait")
	k8s.RunKubectl(t, kubectlOptions, "delete", "crd", "crdbnodes.crdb.cockroachlabs.com", "--ignore-not-found=true", "--wait")
	k8s.RunKubectl(t, kubectlOptions, "delete", "crd", "crdbtenants.crdb.cockroachlabs.com", "--ignore-not-found=true", "--wait")

	testutil.InstallIngressAndMetalLB(t)
	defer func() {
		t.Log("uninstall ingress and metalLB")
		testutil.UninstallIngressAndMetalLB(t)
	}()

	k8s.RunKubectl(t, kubectlOptions, "create", "priorityclass", "operator-critical", "--value", "500000000")
	defer func() {
		t.Log("Delete the priority class operator-critical")
		k8s.RunKubectl(t, kubectlOptions, "delete", "priorityclass", "operator-critical")
	}()

	// Create cluster with different logging config than the default one.
	createLoggingConfig(t, k8sClient, "logging-config", o.Namespace)

	ingress := &api.IngressConfig{
		UI: &api.Ingress{
			IngressClassName: "nginx",
			Host:             "ui.local.com",
			Annotations: map[string]string{
				"nginx.ingress.kubernetes.io/backend-protocol": "HTTPS",
			},
		},
		SQL: &api.Ingress{
			IngressClassName: "nginx",
			Host:             "sql.local.com",
			Annotations: map[string]string{
				"nginx.ingress.kubernetes.io/backend-protocol": "HTTPS",
			},
		},
	}

	crdbClusterName := "crdb-test"
	o.CustomResourceBuilder = testutil.NewBuilder(crdbClusterName).
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
		}).WithClusterLogging("logging-config").
		WithIngress(ingress).
		WithPriorityClass("operator-critical")

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
	testutil.TestIngressRoutingDirect(t, "ui.local.com")

	prepareForMigration(t, o.CrdbCluster.StatefulSetName, o.Namespace, o.CrdbCluster.CaSecret, "operator")
	defer func() {
		_ = os.RemoveAll(manifestsDirPath)
	}()

	t.Log("Clearing kubectl cache to avoid discovery errors")
	homeDir, err := os.UserHomeDir()
	if err == nil {
		cmd := exec.Command("rm", "-rf", filepath.Join(homeDir, ".kube", "cache"))
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Logf("Failed to clear kubectl cache: %v, output: %s", err, string(out))
		}
	} else {
		t.Logf("Failed to get user home dir: %v", err)
	}

	t.Log("Annotating the CrdbCluster with cloudProvider and regionCode")
	retry.DoWithRetry(t, "Annotating CrdbCluster", 5, 2*time.Second, func() (string, error) {
		err = k8s.RunKubectlE(t, kubectlOptions, "annotate", "crdbcluster", o.CrdbCluster.StatefulSetName, "crdb.cockroachlabs.com/cloudProvider=k3d", "--overwrite")
		if err != nil {
			return "", err
		}
		err = k8s.RunKubectlE(t, kubectlOptions, "annotate", "crdbcluster", o.CrdbCluster.StatefulSetName, "crdb.cockroachlabs.com/regionCode=us-east-1", "--overwrite")
		if err != nil {
			return "", err
		}
		return "Successfully annotated CrdbCluster", nil
	})

	o.UninstallConflictingResources(t)
	t.Log("Create the priority class crdb-critical which will be owned by the cockroachdb CockroachDB operator")
	k8s.RunKubectl(t, kubectlOptions, "create", "priorityclass", "crdb-critical", "--value", "500000000")
	defer func() {
		t.Log("Delete the priority class crdb-critical")
		k8s.RunKubectl(t, kubectlOptions, "delete", "priorityclass", "crdb-critical")
	}()

	t.Log("Create the rbac permissions for the CockroachDB operator")
	k8s.KubectlApply(t, kubectlOptions, filepath.Join(manifestsDirPath, "rbac.yaml"))

	t.Log("Install the CockroachDB operator")
	operator.InstallCockroachDBOperator(t, kubectlOptions)
	defer func() {
		t.Log("Uninstall the CockroachDB operator")
		operator.UninstallCockroachDBOperator(t, kubectlOptions)
	}()

	// Label the public operator's pods with the following label
	// kubectl label po <pod-name> crdb.cockroachlabs.com/cluster=$CRDBCLUSTER svc=cockroachdb
	pods := k8s.ListPods(t, kubectlOptions, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("app.kubernetes.io/instance=%s", crdbClusterName),
	})
	for _, pod := range pods {
		k8s.RunKubectl(t, kubectlOptions, "label", "po", pod.Name, fmt.Sprintf("crdb.cockroachlabs.com/cluster=%s", crdbClusterName), "--overwrite")
		k8s.RunKubectl(t, kubectlOptions, "label", "po", pod.Name, "svc=cockroachdb", "--overwrite")
	}

	migratePodsToCrdbNodes(t, o.CrdbCluster, o.Namespace)

	k8s.RunKubectl(t, kubectlOptions, "delete", "poddisruptionbudget", o.CrdbCluster.StatefulSetName)

	var migrateObjsToHelm = make(map[string][]string)

	migrateObjsToHelm["service"] = []string{fmt.Sprintf("%s-public", o.CrdbCluster.StatefulSetName)}
	migrateObjsToHelm["crdbcluster"] = []string{o.CrdbCluster.StatefulSetName}
	migrateObjsToHelm["ingress"] = []string{fmt.Sprintf("ui-%s", o.CrdbCluster.StatefulSetName), fmt.Sprintf("sql-%s", o.CrdbCluster.StatefulSetName)}
	for resourceName, obj := range migrateObjsToHelm {
		for _, objName := range obj {
			k8s.RunKubectl(t, kubectlOptions, "annotate", resourceName, objName, fmt.Sprintf("meta.helm.sh/release-name=%s", o.CrdbCluster.StatefulSetName), "--overwrite")
			k8s.RunKubectl(t, kubectlOptions, "annotate", resourceName, objName, fmt.Sprintf("meta.helm.sh/release-namespace=%s", o.Namespace), "--overwrite")
			k8s.RunKubectl(t, kubectlOptions, "label", resourceName, objName, fmt.Sprintf("app.kubernetes.io/managed-by=%s", "Helm"), "--overwrite")
		}
	}

	o.HelmOptions = &helm.Options{
		KubectlOptions: kubectlOptions,
		ValuesFiles:    []string{filepath.Join(manifestsDirPath, "values.yaml")},
		ExtraArgs: map[string][]string{
			// Force recreation to transfer field ownership from public operator to Helm
			"install": {"--force"},
		},
	}
	t.Log("helm install the crdb cluster from the helm chart with --force flag")
	helmPath, _ := operator.HelmChartPaths()
	helm.Install(t, o.HelmOptions, helmPath, crdbClusterName)
	defer func() {
		t.Log("helm uninstall the crdb cluster")
		o.Uninstall(t)
	}()

	// Update the client and node secrets to the new release name
	o.CrdbCluster.ClientSecret = fmt.Sprintf("%s-client-secret", crdbClusterName)
	o.CrdbCluster.NodeSecret = fmt.Sprintf("%s-node-secret", crdbClusterName)

	o.ValidateExistingData = true
	o.ValidateCRDB(t)

	t.Log("Verify Operator Migration")
	// Verify priority class migration
	verifyOperatorMigration(t, kubectlOptions, o.CustomResourceBuilder.Cr(), crdbClusterName)

	testutil.TestIngressRoutingDirect(t, "ui.local.com")
	t.Log("Verify cluster is in MutableOnly mode and uninstall public operator")
	verifyMutableModeAndCleanupPublicOperator(t, kubectlOptions, crdbClusterName, &o.CrdbCluster)
}

// verifyOperatorMigration verifies that the operator has been correctly migrated
// from the original operator to the running pods
func verifyOperatorMigration(t *testing.T, kubectlOptions *k8s.KubectlOptions, crdbCluster *api.CrdbCluster, clusterName string) {
	// Get all pods for this cluster
	pods := k8s.ListPods(t, kubectlOptions, metav1.ListOptions{
		LabelSelector: operator.LabelSelector,
	})

	require.Equal(t, len(pods), int(crdbCluster.Spec.Nodes))
	require.Equal(t, crdbCluster.Spec.PriorityClassName, pods[0].Spec.PriorityClassName)
	require.Equal(t, crdbCluster.Spec.TerminationGracePeriodSecs, *pods[0].Spec.TerminationGracePeriodSeconds)
}

// verifyMutableModeAndCleanupPublicOperator verifies that the cluster is in MutableOnly mode
// after helm install, then uninstalls the public operator and verifies cluster stability
func verifyMutableModeAndCleanupPublicOperator(t *testing.T, kubectlOptions *k8s.KubectlOptions, clusterName string, crdbCluster *testutil.CockroachCluster) {
	// Verify the CrdbCluster is in MutableOnly mode
	t.Log("Verifying CrdbCluster reconciliation mode is MutableOnly")
	output, err := k8s.RunKubectlAndGetOutputE(t, kubectlOptions, "get", "crdbcluster", clusterName, "-o", "jsonpath={.spec.mode}")
	require.NoError(t, err)
	require.Equal(t, "MutableOnly", output, "CrdbCluster should be in MutableOnly mode after helm install")

	// Verify the CockroachDB operator has taken ownership of the service
	t.Log("Verifying CockroachDB operator owns the cockroachdb service")
	serviceOwner, err := k8s.RunKubectlAndGetOutputE(t, kubectlOptions, "get", "service", clusterName, "-o", "jsonpath={.metadata.ownerReferences[0].apiVersion}")
	require.NoError(t, err)
	require.Equal(t, "crdb.cockroachlabs.com/v1beta1", serviceOwner, "Service should be owned by v1beta1 CrdbCluster")

	// Uninstall the public operator
	t.Log("Uninstalling public operator deployment")
	publicOperatorKubectlOptions := k8s.NewKubectlOptions("", "", migration.OperatorNamespace)
	k8s.RunKubectl(t, publicOperatorKubectlOptions, "delete", "deployment", migration.OperatorDeploymentName, "--ignore-not-found=true")

	// Wait for public operator deployment to be deleted
	time.Sleep(10 * time.Second)

	// Verify cluster remains stable after public operator removal
	t.Log("Verifying cluster stability after public operator removal")
	testutil.RequireClusterToBeReadyEventuallyTimeout(t, *crdbCluster, 120*time.Second)

	// Verify CRDB continues to function
	t.Log("Verifying CRDB continues to function after public operator removal")
	testutil.RequireCRDBToFunction(t, *crdbCluster, false)

	// Verify all pods are still running
	pods := k8s.ListPods(t, kubectlOptions, metav1.ListOptions{
		LabelSelector: operator.LabelSelector,
	})
	require.Equal(t, len(pods), int(crdbCluster.DesiredNodes), "All pods should still be running after public operator removal")
	for _, pod := range pods {
		require.Equal(t, corev1.PodRunning, pod.Status.Phase, "Pod %s should be in Running state", pod.Name)
	}

	t.Log("Successfully verified cluster stability after public operator removal")
}
