package migrate

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/gruntwork-io/terratest/modules/k8s"
	"github.com/gruntwork-io/terratest/modules/shell"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/cockroachdb/helm-charts/tests/testutil"
)

func prepareForMigration(t *testing.T, stsName, namespace, caSecret, crdbDeploymentType string) {
	t.Log("Updating the existing certs")
	certGeneration := shell.Command{
		Command: migrationHelperPath,
		Args: []string{
			"migrate-certs",
			"--statefulset-name", stsName,
			"--namespace", namespace,
			"--ca-secret", caSecret,
		},
	}
	t.Log(shell.RunCommandAndGetOutput(t, certGeneration))

	require.NoError(t, os.Mkdir(manifestsDirPath, 0700))

	t.Log("Generate manifests to migrate")

	cmdArg := "--statefulset-name"
	if crdbDeploymentType == "operator" {
		cmdArg = "--crdb-cluster"
	}

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
	t.Log(shell.RunCommandAndGetOutput(t, generateManifestsCmd))
}

func migratePodsToCrdbNodes(t *testing.T, stsName, namespace string) {
	t.Log("Migrating the pods to CrdbNodes")

	kubectlOptions := k8s.NewKubectlOptions("", "", namespace)
	var crdbSts = appsv1.StatefulSet{}
	err := k8sClient.Get(context.TODO(), types.NamespacedName{Name: stsName, Namespace: namespace}, &crdbSts)
	require.NoError(t, err)

	crdbPodCount := int(*crdbSts.Spec.Replicas)
	for idx := crdbPodCount - 1; idx >= 0; idx-- {
		t.Logf("Scaling statefulset %s to %d", stsName, idx)
		k8s.RunKubectl(t, kubectlOptions, "scale", "statefulset", stsName, "--replicas", strconv.Itoa(idx))

		podName := fmt.Sprintf("%s-%d", stsName, idx)
		testutil.WaitUntilPodDeleted(t, kubectlOptions, podName, 30, 2*time.Second)
		k8s.RunKubectl(t, kubectlOptions, "apply", "-f", filepath.Join(manifestsDirPath, fmt.Sprintf("crdbnode-%d.yaml", idx)))
		testutil.RequirePodToBeCreatedAndReady(t, kubectlOptions, podName, 300*time.Second)
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
