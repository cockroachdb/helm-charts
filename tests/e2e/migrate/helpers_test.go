package migrate

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"os"
	"os/exec"
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
	"gopkg.in/yaml.v3"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	k8sretry "k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type publicSQLService struct {
	name     string
	port     int32
	original *corev1.Service
}

// Keep source permissions and disruption protection intact until handoff, or
// throughout rollback. Exclude status and ownership metadata that can change.
func snapshotMigrationSourceResources(t *testing.T, options *k8s.KubectlOptions, resources ...string) map[string]map[string]interface{} {
	t.Helper()
	snapshot := make(map[string]map[string]interface{}, len(resources))
	for _, resource := range resources {
		raw, err := k8s.RunKubectlAndGetOutputE(t, options, "get", resource, "-o", "json")
		require.NoError(t, err)
		var object map[string]interface{}
		require.NoError(t, json.Unmarshal([]byte(raw), &object))
		metadata, ok := object["metadata"].(map[string]interface{})
		require.True(t, ok)
		snapshot[resource] = map[string]interface{}{
			"uid": metadata["uid"], "spec": object["spec"], "rules": object["rules"],
			"roleRef": object["roleRef"], "subjects": object["subjects"],
			"labels": metadata["labels"],
		}
		if annotations, ok := metadata["annotations"].(map[string]interface{}); ok {
			snapshot[resource]["releaseName"] = annotations["meta.helm.sh/release-name"]
			snapshot[resource]["releaseNamespace"] = annotations["meta.helm.sh/release-namespace"]
		}
	}
	return snapshot
}

func requireMigrationSourceResourcesPreserved(t *testing.T, options *k8s.KubectlOptions, before map[string]map[string]interface{}) {
	t.Helper()
	for resource, original := range before {
		current := snapshotMigrationSourceResources(t, options, resource)
		require.Equal(t, original, current[resource], "migration changed source resource %s", resource)
	}
}

// snapshotPublicSQLService pins the client endpoint before migration. Following
// a changed port after migration would hide a broken endpoint for existing clients.
func snapshotPublicSQLService(t *testing.T, options *k8s.KubectlOptions, clusterName string) publicSQLService {
	t.Helper()
	svc, err := k8s.GetServiceE(t, options, clusterName+"-public")
	require.NoError(t, err)
	require.NotEmpty(t, svc.Spec.ClusterIP)
	require.NotEqual(t, corev1.ClusterIPNone, svc.Spec.ClusterIP)
	// Legacy Helm sources multiplex SQL on the grpc port. The legacy operator
	// has a separate named SQL port.
	for _, name := range []string{"sql", "grpc"} {
		for _, port := range svc.Spec.Ports {
			if port.Name == name {
				return publicSQLService{name: svc.Name, port: port.Port, original: svc.DeepCopy()}
			}
		}
	}
	t.Fatalf("public Service %s has neither a sql nor grpc port", svc.Name)
	return publicSQLService{}
}

// requirePublicServiceSQL runs inside a database pod but connects through Service
// DNS and ClusterIP, rather than localhost or the test runner's pod port-forward.
// Stream the test's root client credentials over stdin because legacy pods do
// not mount them. Keep them out of command arguments, logs and local files, and
// remove the temporary directory in the pod when SQL exits.
func requirePublicServiceSQL(
	t *testing.T, options *k8s.KubectlOptions, cluster testutil.CockroachCluster, service publicSQLService,
) {
	t.Helper()
	runPublicServiceSQL(t, options, cluster, service,
		"SELECT id, balance FROM test_db.accounts ORDER BY id", "id,balance\n1,1000\n2,250")
}

func runPublicServiceSQL(
	t *testing.T, options *k8s.KubectlOptions, cluster testutil.CockroachCluster, service publicSQLService, statement, expected string,
) {
	t.Helper()
	host := fmt.Sprintf("%s.%s.svc.cluster.local:%d", service.name, options.Namespace, service.port)
	var credentials bytes.Buffer
	sqlCommand := `/cockroach/cockroach sql --format=csv \
  --url="postgresql://root@$1/defaultdb?sslmode=disable&connect_timeout=5" \
  --execute="$2"`
	if cluster.CaSecret != "" {
		clientSecret, err := k8s.GetSecretE(t, options, cluster.ClientSecret)
		require.NoError(t, err)
		nodeSecret, err := k8s.GetSecretE(t, options, cluster.NodeSecret)
		require.NoError(t, err)
		archive := tar.NewWriter(&credentials)
		clientCert, clientKey := clientSecret.Data[corev1.TLSCertKey], clientSecret.Data[corev1.TLSPrivateKeyKey]
		if len(clientCert) == 0 {
			clientCert, clientKey = clientSecret.Data["client.root.crt"], clientSecret.Data["client.root.key"]
		}
		for name, data := range map[string][]byte{
			"ca.crt":          nodeSecret.Data["ca.crt"],
			"client.root.crt": clientCert,
			"client.root.key": clientKey,
		} {
			require.NotEmpty(t, data, "missing certificate file %s", name)
			require.NoError(t, archive.WriteHeader(&tar.Header{Name: name, Mode: 0600, Size: int64(len(data))}))
			_, err := archive.Write(data)
			require.NoError(t, err)
		}
		require.NoError(t, archive.Close())
		// The fixtures have a read-only root filesystem. Use their writable data
		// volume for the short-lived client files and always remove the directory.
		sqlCommand = `certs=$(mktemp -d /cockroach/cockroach-data/.service-sql.XXXXXX)
trap 'rm -rf "$certs"' EXIT
tar -xf - -C "$certs"
/cockroach/cockroach sql --format=csv \
  --url="postgresql://root@$1/defaultdb?sslmode=verify-full&sslrootcert=$certs/ca.crt&sslcert=$certs/client.root.crt&sslkey=$certs/client.root.key&connect_timeout=5" \
  --execute="$2"`
	}
	args := []string{}
	if options.ContextName != "" {
		args = append(args, "--context", options.ContextName)
	}
	if options.ConfigPath != "" {
		args = append(args, "--kubeconfig", options.ConfigPath)
	}
	args = append(args, "--namespace", options.Namespace, "--request-timeout=20s")

	_, err := retry.DoWithRetryE(t, "verify SQL through "+host, 12, 5*time.Second, func() (string, error) {
		// Pod 0 can be replaced between samples during migration. Resolve its
		// container each time instead of retrying forever against the old name.
		pod, err := k8s.GetPodE(t, options, cluster.StatefulSetName+"-0")
		if err != nil {
			return "", err
		}
		var containerName string
		for _, container := range pod.Spec.Containers {
			if container.Name == "cockroachdb" || container.Name == "db" {
				containerName = container.Name
				break
			}
		}
		if containerName == "" {
			return "", fmt.Errorf("database container missing from %s", pod.Name)
		}
		execArgs := append(append([]string{}, args...), "exec", "-i", pod.Name, "-c", containerName, "--",
			"sh", "-ec", sqlCommand, "service-sql-check", host, statement)
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, "kubectl", execArgs...)
		cmd.Stdin = bytes.NewReader(credentials.Bytes())
		out, err := cmd.CombinedOutput()
		if err != nil {
			return "", fmt.Errorf("SQL through %s: %w: %s", host, err, out)
		}
		if expected != "" && strings.TrimSpace(string(out)) != expected {
			return "", fmt.Errorf("unexpected data through %s: %s", host, out)
		}
		return string(out), nil
	})
	require.NoError(t, err)
}

func requireInsecureMigrationValues(t *testing.T, valuesFile string) {
	t.Helper()
	data, err := os.ReadFile(valuesFile)
	require.NoError(t, err)
	var values struct {
		CockroachDB struct {
			TLS map[string]interface{} `yaml:"tls"`
		} `yaml:"cockroachdb"`
	}
	require.NoError(t, yaml.Unmarshal(data, &values))
	require.Equal(t, false, values.CockroachDB.TLS["enabled"])
	for _, mode := range []string{"selfSigner", "certManager", "externalCertificates"} {
		settings, ok := values.CockroachDB.TLS[mode].(map[string]interface{})
		require.True(t, ok, "export must explicitly disable %s", mode)
		require.Equal(t, false, settings["enabled"], mode)
	}
}

func prepareForMigration(t *testing.T, stsName, namespace, caSecret, crdbDeploymentType string) {
	t.Log("Updating the existing certs")
	retry.DoWithRetry(t, "Running migrate-certs", 5, 2*time.Second, func() (string, error) {
		certGeneration := shell.Command{
			Command: migrationHelperPath,
			Env:     map[string]string{"KUBECONFIG": migrationKubeConfig},
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
				"--kubeconfig", migrationKubeConfig,
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

// migrationChartValues uses the self-signer image built and loaded by the e2e
// setup. Keep exported certificate settings unchanged during chart adoption.
func migrationChartValues(values map[string]string) map[string]string {
	if values == nil {
		values = make(map[string]string)
	}
	values["cockroachdb.tls.selfSigner.image.registry"] = testutil.TestSelfSignerImageRegistry
	values["cockroachdb.tls.selfSigner.image.repository"] = testutil.TestSelfSignerImageRepository
	return values
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

	options := &helm.Options{KubectlOptions: kubectlOptions, SetValues: migrationChartValues(nil)}
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

// waitForSettledFinalization prevents deleting the source before the controller's
// one-time final spec rewrite and health confirmation have completed.
func waitForSettledFinalization(t *testing.T, options *k8s.KubectlOptions, name string) {
	t.Helper()
	waitForMigrationPhase(t, options, name, phaseFinalization, phaseFinalizationTimeout)
	requireJSONPathEqualsEventually(t, options, v1beta1CrdbClusterResource, name,
		"{.status.migration.settledPhase}", phaseFinalization, 3*time.Minute)
	requireJSONPathEquals(t, options, "sts", name, "{.spec.replicas}", "0")
}

func requireNodeCertificateNames(t *testing.T, options *k8s.KubectlOptions, secretName string, names ...string) {
	t.Helper()
	var secret corev1.Secret
	require.NoError(t, k8sClient.Get(context.Background(), types.NamespacedName{
		Namespace: options.Namespace, Name: secretName,
	}, &secret))
	certificate := secret.Data["node.crt"]
	if len(certificate) == 0 {
		certificate = secret.Data["tls.crt"]
	}
	block, _ := pem.Decode(certificate)
	require.NotNil(t, block, "node certificate must be PEM encoded")
	parsed, err := x509.ParseCertificate(block.Bytes)
	require.NoError(t, err)
	for _, name := range names {
		require.NoError(t, parsed.VerifyHostname(name), "node certificate lost SAN %s", name)
	}
}

const migrationMetadataKey = "test.cockroachlabs.com/migration-metadata"

// Mark source claims to distinguish preserved user metadata from operator-owned
// labels and owner references, which legitimately change during adoption.
func snapshotMigrationPVCs(t *testing.T, options *k8s.KubectlOptions) []corev1.PersistentVolumeClaim {
	t.Helper()
	var claims corev1.PersistentVolumeClaimList
	require.NoError(t, k8sClient.List(context.Background(), &claims, client.InNamespace(options.Namespace)))
	require.NotEmpty(t, claims.Items)
	for i := range claims.Items {
		claim := &claims.Items[i]
		require.Equal(t, corev1.ClaimBound, claim.Status.Phase)
		require.NoError(t, k8sretry.RetryOnConflict(k8sretry.DefaultRetry, func() error {
			if err := k8sClient.Get(context.Background(), types.NamespacedName{Namespace: claim.Namespace, Name: claim.Name}, claim); err != nil {
				return err
			}
			if claim.Labels == nil {
				claim.Labels = map[string]string{}
			}
			if claim.Annotations == nil {
				claim.Annotations = map[string]string{}
			}
			claim.Labels[migrationMetadataKey] = "keep"
			claim.Annotations[migrationMetadataKey] = "keep"
			return k8sClient.Update(context.Background(), claim)
		}))
	}
	return claims.Items
}

func requireMigrationPVCsPreserved(t *testing.T, options *k8s.KubectlOptions, originals []corev1.PersistentVolumeClaim) {
	t.Helper()
	for _, original := range originals {
		var claim corev1.PersistentVolumeClaim
		require.NoError(t, k8sClient.Get(context.Background(), types.NamespacedName{
			Namespace: options.Namespace, Name: original.Name,
		}, &claim))
		require.Equal(t, original.UID, claim.UID, "PVC %s was recreated", original.Name)
		require.Equal(t, original.Spec.VolumeName, claim.Spec.VolumeName, "PVC %s changed volume", original.Name)
		require.Equal(t, original.Spec.DataSource, claim.Spec.DataSource)
		require.Equal(t, corev1.ClaimBound, claim.Status.Phase)
		require.Equal(t, "keep", claim.Labels[migrationMetadataKey])
		require.Equal(t, "keep", claim.Annotations[migrationMetadataKey])
		if value, ok := original.Labels[helmPVCMetadataKey]; ok {
			require.Equal(t, value, claim.Labels[helmPVCMetadataKey], "PVC %s lost chart label", claim.Name)
			require.Equal(t, original.Annotations[helmPVCMetadataKey], claim.Annotations[helmPVCMetadataKey],
				"PVC %s lost chart annotation", claim.Name)
		}
	}
}

func requireRollbackArtifactsRemoved(t *testing.T, options *k8s.KubectlOptions, name string, operatorSource bool) {
	t.Helper()
	key := types.NamespacedName{Namespace: options.Namespace, Name: name + "-ca-crt"}
	require.Eventually(t, func() bool {
		var ca corev1.ConfigMap
		return apierrors.IsNotFound(k8sClient.Get(context.Background(), key, &ca))
	}, 3*time.Minute, 5*time.Second, "rollback retained migration CA ConfigMap")
	key.Name = name + "-join"
	require.Eventually(t, func() bool {
		var join corev1.Service
		return apierrors.IsNotFound(k8sClient.Get(context.Background(), key, &join))
	}, time.Minute, 5*time.Second, "rollback retained join service")
	for _, kind := range []string{"clusterrole", "clusterrolebinding"} {
		require.Eventually(t, func() bool {
			out, err := k8s.RunKubectlAndGetOutputE(t, options, "get", kind, options.Namespace+"-"+name,
				"--ignore-not-found", "-o", "name")
			return err == nil && strings.TrimSpace(out) == ""
		}, time.Minute, 5*time.Second, "rollback retained migration %s", kind)
	}
	if operatorSource {
		for _, suffix := range []string{"-node-secret", "-client-secret"} {
			key.Name = name + suffix
			var secret corev1.Secret
			require.True(t, apierrors.IsNotFound(k8sClient.Get(context.Background(), key, &secret)),
				"rollback retained generated Secret %s", key.Name)
		}
	} else {
		// Helm rollback deletes the target it created, rather than preserving an
		// operator-source audit status on an existing public CrdbCluster.
		retry.DoWithRetry(t, "wait for migration target removal", 36, 5*time.Second, func() (string, error) {
			out, err := k8s.RunKubectlAndGetOutputE(t, options, "get", v1beta1CrdbClusterResource, name,
				"--ignore-not-found", "-o", "name")
			if err != nil {
				return "", err
			}
			if strings.TrimSpace(out) != "" {
				return "", fmt.Errorf("migration target still present")
			}
			return "removed", nil
		})
	}
	requireMigrationMarkersRemoved(t, options, name)
}

func getMigrationCertificateSpec(t *testing.T, options *k8s.KubectlOptions, name string) map[string]interface{} {
	t.Helper()
	raw, err := k8s.RunKubectlAndGetOutputE(t, options, "get", "certificates.cert-manager.io", name, "-o", "json")
	require.NoError(t, err)
	var certificate struct {
		Spec map[string]interface{} `json:"spec"`
	}
	require.NoError(t, json.Unmarshal([]byte(raw), &certificate))
	return certificate.Spec
}

func requireMigrationCertificatePreserved(t *testing.T, options *k8s.KubectlOptions, name string, original map[string]interface{}) {
	t.Helper()
	current := getMigrationCertificateSpec(t, options, name)
	require.Equal(t, original["issuerRef"], current["issuerRef"])
	require.Equal(t, original["secretName"], current["secretName"])
	for _, field := range []string{"dnsNames", "ipAddresses"} {
		names, ok := original[field].([]interface{})
		require.True(t, ok, "source Certificate must exercise %s preservation", field)
		for _, name := range names {
			require.Contains(t, current[field], name, "Certificate lost custom %s", field)
		}
	}
	require.Contains(t, current["dnsNames"], strings.TrimSuffix(name, "-node")+"-join."+options.Namespace+".svc.cluster.local")
}

// A same-release chart switch removes resources absent from the new chart.
// Retain the legacy CA chain in the recorded manifest, as the migration guide
// requires; annotating only the live resources would not exercise that workflow.
func legacyChartWithRetainedCertManagerCA(t *testing.T) string {
	t.Helper()
	chart := t.TempDir()
	require.NoError(t, os.CopyFS(chart, os.DirFS(filepath.Join(testutil.GetGitRoot(), "cockroachdb-legacy"))))
	for _, name := range []string{"certificate.ca.yaml", "certificate.issuer.yaml"} {
		path := filepath.Join(chart, "templates", name)
		data, err := os.ReadFile(path)
		require.NoError(t, err)
		require.Equal(t, 1, bytes.Count(data, []byte("metadata:\n")))
		data = bytes.Replace(data, []byte("metadata:\n"),
			[]byte("metadata:\n  annotations:\n    helm.sh/resource-policy: keep\n"), 1)
		require.NoError(t, os.WriteFile(path, data, 0600))
	}
	return chart
}

// Controller export retains external Secret references. Opt back into chart
// certificate management explicitly, preserving the source issuer and lifetimes.
func configureMigrationCertManagerAdoption(
	t *testing.T, valuesFile, caConfigMap string,
	node, root map[string]interface{}, dnsNames, ipAddresses []string,
) {
	t.Helper()
	data, err := os.ReadFile(valuesFile)
	require.NoError(t, err)
	var values map[string]interface{}
	require.NoError(t, yaml.Unmarshal(data, &values))
	cockroachDB, ok := values["cockroachdb"].(map[string]interface{})
	require.True(t, ok)
	tls, ok := cockroachDB["tls"].(map[string]interface{})
	require.True(t, ok)
	require.Equal(t, map[string]interface{}{"enabled": false}, tls["selfSigner"])
	require.Equal(t, map[string]interface{}{"enabled": false}, tls["certManager"])
	external, ok := tls["externalCertificates"].(map[string]interface{})
	require.True(t, ok)
	require.Equal(t, true, external["enabled"])
	refs, ok := external["certificates"].(map[string]interface{})
	require.True(t, ok)
	require.Equal(t, caConfigMap, refs["caConfigMapName"])
	require.Equal(t, node["secretName"], refs["nodeSecretName"])
	require.Equal(t, root["secretName"], refs["rootSqlClientSecretName"])
	issuerRef, ok := node["issuerRef"].(map[string]interface{})
	require.True(t, ok)
	require.Equal(t, issuerRef, root["issuerRef"])
	issuer := make(map[string]interface{}, len(issuerRef)+4)
	for key, value := range issuerRef {
		issuer[key] = value
	}
	for key, value := range map[string]interface{}{
		"nodeCertDuration": node["duration"], "nodeCertExpiryWindow": node["renewBefore"],
		"clientCertDuration": root["duration"], "clientCertExpiryWindow": root["renewBefore"],
	} {
		require.NotEmpty(t, value, "source must specify %s", key)
		issuer[key] = value
	}
	external["enabled"] = false
	tls["certManager"] = map[string]interface{}{
		"enabled": true, "caConfigMap": caConfigMap, "issuer": issuer,
		"nodeSecret": node["secretName"], "clientRootSecret": root["secretName"],
		"additionalDnsNames": dnsNames, "additionalIpAddresses": ipAddresses,
	}
	data, err = yaml.Marshal(values)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(valuesFile, data, 0600))
}

func requireMigrationCertManagerCertificates(t *testing.T, options *k8s.KubectlOptions, name string, node, root map[string]interface{}) {
	t.Helper()
	requireMigrationCertificatePreserved(t, options, name+"-node", node)
	for suffix, original := range map[string]map[string]interface{}{"-node": node, "-root-client": root} {
		current := getMigrationCertificateSpec(t, options, name+suffix)
		for _, field := range []string{"issuerRef", "secretName", "duration", "renewBefore"} {
			require.Equal(t, original[field], current[field], "Certificate %s%s changed %s", name, suffix, field)
		}
		requireJSONPathEqualsEventually(t, options, "certificates.cert-manager.io", name+suffix,
			"{.status.conditions[?(@.type==\"Ready\")].status}", "True", 5*time.Minute)
	}
	requireJSONPathEqualsEventually(t, options, "issuers.cert-manager.io", name+"-ca-issuer",
		"{.status.conditions[?(@.type==\"Ready\")].status}", "True", time.Minute)
	requireJSONPathEqualsEventually(t, options, "certificates.cert-manager.io", name+"-ca-cert",
		"{.status.conditions[?(@.type==\"Ready\")].status}", "True", time.Minute)
}

func requireMigrationCertManagerReissue(t *testing.T, options *k8s.KubectlOptions, nodeSecret, caSecret string, names ...string) {
	t.Helper()
	previous := k8s.GetSecret(t, options, nodeSecret)
	ca := k8s.GetSecret(t, options, caSecret)
	roots := x509.NewCertPool()
	require.True(t, roots.AppendCertsFromPEM(ca.Data["tls.crt"]))
	k8s.RunKubectl(t, options, "delete", "secret", nodeSecret)
	retry.DoWithRetry(t, "wait for cert-manager to reissue node certificate with migrated SANs", 60, 5*time.Second, func() (string, error) {
		var secret corev1.Secret
		if err := k8sClient.Get(context.Background(), types.NamespacedName{Namespace: options.Namespace, Name: nodeSecret}, &secret); err != nil {
			return "", err
		}
		if bytes.Equal(previous.Data["tls.crt"], secret.Data["tls.crt"]) {
			return "", fmt.Errorf("node certificate has not changed")
		}
		block, _ := pem.Decode(secret.Data["tls.crt"])
		if block == nil {
			return "", fmt.Errorf("reissued node certificate is missing or not PEM encoded")
		}
		certificate, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return "", err
		}
		if _, err := certificate.Verify(x509.VerifyOptions{Roots: roots}); err != nil {
			return "", err
		}
		for _, name := range names {
			if err := certificate.VerifyHostname(name); err != nil {
				return "", err
			}
		}
		return "node certificate reissued with the same CA and migrated SANs", nil
	})
}

func requireMigrationWaitsForSkipReconcile(t *testing.T, options *k8s.KubectlOptions, name string, replicas int) {
	t.Helper()
	retry.DoWithRetry(t, "wait for explicit skip-reconcile requirement", 36, 5*time.Second, func() (string, error) {
		lastError, err := k8s.RunKubectlAndGetOutputE(t, options, "get", v1beta1CrdbClusterResource, name,
			"-o", "jsonpath={.status.migration.lastError}")
		if err != nil {
			return "", err
		}
		if !strings.Contains(lastError, "missing required label: crdb.io/skip-reconcile") {
			return "", fmt.Errorf("waiting for skip-reconcile diagnostic, got %q", lastError)
		}
		return lastError, nil
	})
	requireJSONPathEquals(t, options, v1beta1CrdbClusterResource, name, "{.metadata.labels.crdb\\.io/skip-reconcile}", "")
	requireJSONPathEquals(t, options, "sts", name, "{.spec.replicas}", strconv.Itoa(replicas))
	waitForCrdbNodeCount(t, options, name, 0, time.Minute)
}

type migrationSelfSignerSnapshot struct {
	providedCA  string
	caConfigMap corev1.ConfigMap
	secrets     map[string]string
}

// Check the export before adoption, without overriding its choice of TLS provider.
// The migrated CR references external certificates even when self-signer owns them.
func requireMigrationSelfSignerExport(t *testing.T, options *k8s.KubectlOptions, name, valuesFile, providedCA string, additionalSANs ...string) migrationSelfSignerSnapshot {
	t.Helper()
	data, err := os.ReadFile(valuesFile)
	require.NoError(t, err)
	var values struct {
		CockroachDB struct {
			TLS struct {
				SelfSigner           map[string]interface{} `yaml:"selfSigner"`
				ExternalCertificates map[string]interface{} `yaml:"externalCertificates"`
			} `yaml:"tls"`
		} `yaml:"cockroachdb"`
	}
	require.NoError(t, yaml.Unmarshal(data, &values))
	signer := values.CockroachDB.TLS.SelfSigner
	require.Equal(t, true, signer["enabled"], "export must restore self-signer ownership")
	require.Equal(t, true, signer["rotateCerts"], "export must restore rotation")
	require.Equal(t, providedCA != "", signer["caProvided"], "export must preserve who owns the signing CA")
	if providedCA != "" {
		require.Equal(t, providedCA, signer["caSecret"], "export must retain the supplied CA Secret name")
	}
	require.Equal(t, false, values.CockroachDB.TLS.ExternalCertificates["enabled"], "Helm TLS providers must be mutually exclusive")
	for _, san := range append(additionalSANs, name+"-join."+options.Namespace+".svc.cluster.local") {
		require.Contains(t, signer["additionalSANs"], san, "export must retain custom and join SANs")
	}

	snapshot := migrationSelfSignerSnapshot{providedCA: providedCA, secrets: map[string]string{}}
	for field, suffix := range map[string]string{"caCertDuration": "ca-secret", "nodeCertDuration": "node-secret", "clientCertDuration": "client-secret"} {
		secret := k8s.GetSecret(t, options, name+"-"+suffix)
		snapshot.secrets[secret.Name] = migrationSecretFingerprint(t, secret)
		durationValue, ok := signer[field].(string)
		require.True(t, ok, "export must include %s", field)
		duration, err := time.ParseDuration(durationValue)
		require.NoError(t, err)
		require.Positive(t, duration, "export must include a usable %s", field)
	}

	if providedCA != "" {
		snapshot.secrets[providedCA] = migrationSecretFingerprint(t, k8s.GetSecret(t, options, providedCA))
	}

	// Manual migration creates CrdbNodes before chart adoption creates the
	// CrdbCluster. Check the CA actually mounted by the migrated nodes in both paths.
	caConfigMapName, err := k8s.RunKubectlAndGetOutputE(t, options, "get", "crdbnode", name+"-0",
		"-o", "jsonpath={.spec.certificates.externalCertificates.caConfigMapName}")
	require.NoError(t, err)
	require.NotEmpty(t, caConfigMapName)
	require.NoError(t, k8sClient.Get(context.Background(), types.NamespacedName{Namespace: options.Namespace, Name: caConfigMapName}, &snapshot.caConfigMap))
	require.Equal(t, string(k8s.GetSecret(t, options, name+"-ca-secret").Data["ca.crt"]), snapshot.caConfigMap.Data["ca.crt"])
	clusterResource, err := k8s.RunKubectlAndGetOutputE(t, options, "get", v1beta1CrdbClusterResource, name,
		"--ignore-not-found", "-o", "name")
	require.NoError(t, err)
	if clusterResource != "" {
		requireJSONPathEquals(t, options, v1beta1CrdbClusterResource, name,
			"{.spec.template.spec.certificates.externalCertificates.caConfigMapName}", snapshot.caConfigMap.Name)
	}
	if snapshot.caConfigMap.Name != name+"-ca-secret-crt" {
		var chartCA corev1.ConfigMap
		err = k8sClient.Get(context.Background(), types.NamespacedName{Namespace: options.Namespace, Name: name + "-ca-secret-crt"}, &chartCA)
		require.True(t, apierrors.IsNotFound(err), "controller fixture must exercise creation of the chart CA ConfigMap, got %v", err)
	}
	return snapshot
}

// Fingerprint Secret data and identity so a failure never prints private keys.
func migrationSecretFingerprint(t *testing.T, secret *corev1.Secret) string {
	t.Helper()
	data, err := json.Marshal(secret.Data)
	require.NoError(t, err)
	return fmt.Sprintf("%s:%x", secret.UID, sha256.Sum256(data))
}

func requireMigrationSelfSignerAdoption(t *testing.T, options *k8s.KubectlOptions, name string, nodeCount int, before migrationSelfSignerSnapshot) {
	t.Helper()
	for secretName, fingerprint := range before.secrets {
		require.Equal(t, fingerprint, migrationSecretFingerprint(t, k8s.GetSecret(t, options, secretName)), "adoption must reuse %s without changing its data", secretName)
	}
	var migrationCA, chartCA corev1.ConfigMap
	require.NoError(t, k8sClient.Get(context.Background(), types.NamespacedName{Namespace: options.Namespace, Name: before.caConfigMap.Name}, &migrationCA))
	require.Equal(t, before.caConfigMap.UID, migrationCA.UID, "adoption must not replace the migration CA ConfigMap")
	require.Equal(t, before.caConfigMap.Data, migrationCA.Data, "adoption must retain the migration CA bundle")
	require.NoError(t, k8sClient.Get(context.Background(), types.NamespacedName{Namespace: options.Namespace, Name: name + "-ca-secret-crt"}, &chartCA))
	require.Equal(t, before.caConfigMap.Data["ca.crt"], chartCA.Data["ca.crt"], "chart CA ConfigMap must contain the existing CA")
	for field, expected := range map[string]string{
		"caConfigMapName": chartCA.Name, "nodeSecretName": name + "-node-secret",
		"rootSqlClientSecretName": name + "-client-secret", "httpSecretName": name + "-client-secret",
	} {
		requireJSONPathEquals(t, options, v1beta1CrdbClusterResource, name,
			"{.spec.template.spec.certificates.externalCertificates."+field+"}", expected)
	}
	if before.providedCA == "" {
		k8s.RunKubectl(t, options, "get", "cronjob", name+"-rotate-self-signer")
	} else {
		jobs, err := k8s.RunKubectlAndGetOutputE(t, options, "get", "cronjob", name+"-rotate-self-signer", "--ignore-not-found", "-o", "name")
		require.NoError(t, err)
		require.Empty(t, jobs, "the chart must not rotate a supplied CA")
	}
	k8s.RunKubectl(t, options, "get", "cronjob", name+"-rotate-self-signer-client")
	// The parent update must reach the live pod mounts before rotation can be safe.
	require.Eventually(t, func() bool {
		for i := 0; i < nodeCount; i++ {
			var pod corev1.Pod
			if err := k8sClient.Get(context.Background(), types.NamespacedName{Namespace: options.Namespace, Name: fmt.Sprintf("%s-%d", name, i)}, &pod); err != nil || pod.DeletionTimestamp != nil || !k8s.IsPodAvailable(&pod) {
				return false
			}
			found := false
			for _, volume := range pod.Spec.Volumes {
				if volume.Projected == nil {
					continue
				}
				for _, source := range volume.Projected.Sources {
					if source.ConfigMap != nil {
						if source.ConfigMap.Name == before.caConfigMap.Name && before.caConfigMap.Name != chartCA.Name {
							return false
						}
						found = found || source.ConfigMap.Name == chartCA.Name
					}
				}
			}
			if !found {
				return false
			}
		}
		return true
	}, 10*time.Minute, 5*time.Second, "waiting for all migrated pods to mount the chart CA ConfigMap")
	for i := 0; i < nodeCount; i++ {
		requireJSONPathEqualsEventually(t, options, "crdbnodes", fmt.Sprintf("%s-%d", name, i),
			"{.status.conditions[?(@.type==\"PodReady\")].status}", "True", time.Minute)
	}
}

// Reuse the forward fixture to verify that the exported values restore rotation
// without losing the migrated join name or the source's custom DNS/IP SANs.
func requireMigrationSelfSignerRotation(t *testing.T, options *k8s.KubectlOptions, name string, cluster testutil.CockroachCluster, before migrationSelfSignerSnapshot, additionalSANs ...string) {
	t.Helper()
	caName := name + "-ca-secret"
	if before.providedCA != "" {
		caName = before.providedCA
	}
	ca := k8s.GetSecret(t, options, caName)
	node := k8s.GetSecret(t, options, name+"-node-secret")
	client := k8s.GetSecret(t, options, name+"-client-secret")
	var bundle corev1.ConfigMap
	require.NoError(t, k8sClient.Get(context.Background(), types.NamespacedName{Namespace: options.Namespace, Name: name + "-ca-secret-crt"}, &bundle))
	require.Equal(t, string(ca.Data["ca.crt"]), bundle.Data["ca.crt"])
	for _, secret := range []string{node.Name, client.Name} {
		k8s.RunKubectl(t, options, "annotate", "secret", secret, "certificate-valid-upto=2000-01-01T00:00:00Z", "--overwrite")
	}
	job := "migration-certificate-rotate"
	k8s.RunKubectl(t, options, "create", "job", job, "--from=cronjob/"+name+"-rotate-self-signer-client")
	testutil.RequireCertRotateJobToBeCompleted(t, job, cluster, 10*time.Minute)
	require.NotEqual(t, node.Data["tls.crt"], k8s.GetSecret(t, options, node.Name).Data["tls.crt"])
	require.NotEqual(t, client.Data["tls.crt"], k8s.GetSecret(t, options, client.Name).Data["tls.crt"])
	require.Equal(t, migrationSecretFingerprint(t, ca), migrationSecretFingerprint(t, k8s.GetSecret(t, options, ca.Name)), "leaf rotation must preserve the CA")
	requireNodeCertificateNames(t, options, node.Name, append(additionalSANs, name+"-join."+options.Namespace+".svc.cluster.local")...)
	for i := 0; i < cluster.DesiredNodes; i++ {
		testutil.RequirePodToBeCreatedAndReady(t, options, fmt.Sprintf("%s-%d", name, i), 3*time.Minute)
		requireJSONPathEqualsEventually(t, options, "crdbnodes", fmt.Sprintf("%s-%d", name, i),
			"{.status.conditions[?(@.type==\"PodReady\")].status}", "True", time.Minute)
	}
	testutil.RequireCRDBToFunction(t, cluster, true)
}

// Render the real chart and change a parent value before any forced ownership
// transfer. Applying only the CrdbCluster isolates migration SSA behavior from
// the separate first adoption of Services, Certificates and RBAC.
func verifyMigrationChartApply(t *testing.T, options *k8s.KubectlOptions, chartPath, release, name, valuesFile string) {
	t.Helper()
	previous, err := k8s.RunKubectlAndGetOutputE(t, options, "get", v1beta1CrdbClusterResource, name,
		"-o", "jsonpath={.spec.rollingRestartDelay}")
	require.NoError(t, err)
	restartDelay := "10s"
	if previous == restartDelay {
		restartDelay = "11s"
	}
	rendered := helm.RenderTemplate(t, &helm.Options{
		KubectlOptions: options, ValuesFiles: []string{valuesFile},
		SetValues: map[string]string{"cockroachdb.crdbCluster.rollingRestartDelay": restartDelay},
	}, chartPath, release, []string{"templates/crdb.yaml"}, "--dry-run=server")
	path := filepath.Join(manifestsDirPath, "helm-adoption-crdb.yaml")
	require.NoError(t, os.WriteFile(path, []byte(rendered), 0600))
	defer func() { _ = os.Remove(path) }()
	k8s.RunKubectl(t, options, "apply", "--server-side", "--field-manager=helm", "-f", path)
	requireJSONPathEquals(t, options, v1beta1CrdbClusterResource, name, "{.spec.rollingRestartDelay}", restartDelay)
	requireHelmSSAOwnership(t, options, name)
	requireNoOperatorSpecOwnership(t, options, name)
}
