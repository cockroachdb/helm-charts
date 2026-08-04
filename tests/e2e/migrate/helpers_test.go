package migrate

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cockroachdb/helm-charts/tests/testutil"
	"github.com/gruntwork-io/terratest/modules/helm"
	"github.com/gruntwork-io/terratest/modules/k8s"
	"github.com/gruntwork-io/terratest/modules/retry"
	"github.com/gruntwork-io/terratest/modules/shell"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func prepareForMigration(t *testing.T, stsName, namespace, caSecret, crdbDeploymentType string) {
	t.Log("Updating the existing certs")
	retry.DoWithRetry(t, "Running migrate-certs", 5, 2*time.Second, func() (string, error) {
		certGeneration := shell.Command{
			Command: migrationHelperPath,
			Args: []string{
				"migrate-certs",
				"--statefulset-name", stsName,
				"--namespace", namespace,
				"--ca-secret", caSecret,
			},
		}
		return shell.RunCommandAndGetOutputE(t, certGeneration)
	})

	require.NoError(t, os.Mkdir(manifestsDirPath, 0700))

	t.Log("Generate manifests to migrate")

	cmdArg := "--statefulset-name"
	if crdbDeploymentType == "operator" {
		cmdArg = "--crdb-cluster"
	}

	retry.DoWithRetry(t, "Running build-manifest", 5, 2*time.Second, func() (string, error) {
		generateManifestsCmd := shell.Command{
			Command: migrationHelperPath,
			Args: []string{
				"build-manifest",
				crdbDeploymentType,
				fmt.Sprintf("%s=%s", cmdArg, stsName),
				fmt.Sprintf("--namespace=%s", namespace),
				"--cloud-provider=k3d",
				"--cloud-region=us-east-1",
				fmt.Sprintf("--output-dir=%s", manifestsDirPath),
			},
		}
		return shell.RunCommandAndGetOutputE(t, generateManifestsCmd)
	})
}

func migratePodsToCrdbNodes(t *testing.T, crdbCluster testutil.CockroachCluster, namespace string) {
	t.Log("Migrating the pods to CrdbNodes")

	kubectlOptions := migrationKubectlOptions(namespace)

	// Add the crdb.io/skip-reconcile label to prevent the public operator from scaling up
	// This is only needed if a CrdbCluster CR exists (i.e., migrating from public operator)
	// For Helm chart migrations, there is no CrdbCluster CR initially
	t.Log("Checking if CrdbCluster exists to add crdb.io/skip-reconcile label")
	retry.DoWithRetry(t, "Adding crdb.io/skip-reconcile label", 5, 2*time.Second, func() (string, error) {
		_, err := k8s.RunKubectlAndGetOutputE(t, kubectlOptions, "get", v1alpha1CrdbClusterResource, crdbCluster.StatefulSetName)
		if err == nil {
			// CrdbCluster exists, add the migration label
			t.Log("Adding crdb.io/skip-reconcile label to CrdbCluster")
			return "Successfully labeled CrdbCluster", k8s.RunKubectlE(t, kubectlOptions, "label", v1alpha1CrdbClusterResource, crdbCluster.StatefulSetName, "crdb.io/skip-reconcile=true", "--overwrite")
		}
		t.Log("No CrdbCluster found (normal for Helm chart migrations)")
		return "No CrdbCluster found, skipping label", nil
	})

	var crdbSts = appsv1.StatefulSet{}
	err := k8sClient.Get(context.TODO(), types.NamespacedName{Name: crdbCluster.StatefulSetName, Namespace: namespace}, &crdbSts)
	require.NoError(t, err)

	crdbPodCount := int(*crdbSts.Spec.Replicas)
	for idx := crdbPodCount - 1; idx >= 0; idx-- {
		t.Logf("Scaling statefulset %s to %d", crdbCluster.StatefulSetName, idx)
		k8s.RunKubectl(t, kubectlOptions, "scale", "statefulset", crdbCluster.StatefulSetName, "--replicas", strconv.Itoa(idx))

		podName := fmt.Sprintf("%s-%d", crdbCluster.StatefulSetName, idx)
		testutil.WaitUntilPodDeleted(t, kubectlOptions, podName, 30, 2*time.Second)
		k8s.RunKubectl(t, kubectlOptions, "apply", "-f", filepath.Join(manifestsDirPath, fmt.Sprintf("crdbnode-%d.yaml", idx)))
		testutil.RequirePodToBeCreatedAndReady(t, kubectlOptions, podName, 300*time.Second)
		testutil.RequireCRDBClusterToBeReadyEventuallyTimeout(t, kubectlOptions, crdbCluster, 600*time.Second)
		testutil.RequireCRDBToFunction(t, crdbCluster, true)
	}

	t.Log("All the statefulset pods are migrated to CrdbNodes")

}

func createLoggingConfig(t *testing.T, cl client.Client, name, namespace string) {
	// Create cluster with different logging config than the default one.
	logJson := []byte(`{"sinks": {"file-groups": {"dev": {"channels": "DEV", "filter": "WARNING"}}}}`)
	logConfig := make(map[string]interface{})
	require.NoError(t, json.Unmarshal(logJson, &logConfig))

	var loggingConfigMap = corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Data: map[string]string{
			"logging.yaml": string(logJson),
		},
	}

	require.NoError(t, cl.Create(context.TODO(), &loggingConfigMap))
}

// verifyHelmSSAUpgrade performs a normal Helm upgrade and verifies that Helm
// used server-side apply to manage the CrdbCluster. The command intentionally
// does not use --force-conflicts: an ownership conflict must fail the test.
func verifyHelmSSAUpgrade(
	t *testing.T,
	kubectlOptions *k8s.KubectlOptions,
	helmPath, releaseName, clusterName, valuesFile string,
) {
	t.Helper()
	t.Logf("Verifying Helm 4 SSA upgrade for CrdbCluster %s", clusterName)

	options := &helm.Options{KubectlOptions: kubectlOptions}
	if valuesFile != "" {
		options.ValuesFiles = []string{valuesFile}
	}
	helm.Upgrade(t, options, helmPath, releaseName)
	requireHelmSSAOwnership(t, kubectlOptions, clusterName)
}

// requireHelmSSAOwnership proves that the test is exercising Helm 4 SSA rather
// than only checking that a client-side Helm upgrade happened to succeed.
func requireHelmSSAOwnership(
	t *testing.T, kubectlOptions *k8s.KubectlOptions, clusterName string,
) {
	t.Helper()

	raw, err := k8s.RunKubectlAndGetOutputE(
		t,
		kubectlOptions,
		"get", v1beta1CrdbClusterResource, clusterName,
		"-o", "json",
		"--show-managed-fields=true",
	)
	require.NoError(t, err)

	var object struct {
		Metadata struct {
			ManagedFields []struct {
				Manager   string `json:"manager"`
				Operation string `json:"operation"`
			} `json:"managedFields"`
		} `json:"metadata"`
	}
	require.NoError(t, json.Unmarshal([]byte(raw), &object))

	for _, field := range object.Metadata.ManagedFields {
		if strings.Contains(strings.ToLower(field.Manager), "helm") && field.Operation == "Apply" {
			return
		}
	}

	require.Failf(
		t,
		"Helm SSA ownership was not recorded",
		"CrdbCluster %s has no Helm managedFields entry with operation Apply: %+v",
		clusterName,
		object.Metadata.ManagedFields,
	)
}

// requireNoMigrationAnnotations verifies that conversion-only metadata was
// cleaned before Helm adoption and was not injected into a native v1beta1
// cluster during v1alpha1/v1beta1 coexistence.
func requireNoMigrationAnnotations(
	t *testing.T, kubectlOptions *k8s.KubectlOptions, clusterName string,
) {
	t.Helper()

	raw, err := k8s.RunKubectlAndGetOutputE(
		t,
		kubectlOptions,
		"get", v1beta1CrdbClusterResource, clusterName,
		"-o", "json",
	)
	require.NoError(t, err)

	var object struct {
		Metadata struct {
			Annotations map[string]string `json:"annotations"`
		} `json:"metadata"`
	}
	require.NoError(t, json.Unmarshal([]byte(raw), &object))
	for key := range object.Metadata.Annotations {
		require.NotContains(t, strings.ToLower(key), "migration",
			"CrdbCluster %s retained conversion-only annotation %s", clusterName, key)
	}
}
