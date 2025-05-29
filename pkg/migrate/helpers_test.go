package migrate

import (
	"bytes"
	"context"
	"os"
	"testing"

	publicv1alpha1 "github.com/cockroachdb/cockroach-operator/apis/v1alpha1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	"sigs.k8s.io/yaml"
)

func TestParseInt32(t *testing.T) {
	tests := []struct {
		input    string
		expected int32
		wantErr  bool
	}{
		{"123", 123, false},
		{"-123", -123, false},
		{"abc", 0, true},
		{"", 0, true},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			result, err := parseInt32(tt.input)
			if tt.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
				assert.Equal(t, tt.expected, result)
			}
		})
	}
}

func TestConvertSecretToConfigMap(t *testing.T) {
	clientset := fake.NewSimpleClientset()
	ctx := context.TODO()
	namespace := "default"
	secretName := "test-secret"

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretName,
			Namespace: namespace,
		},
		Data: map[string][]byte{
			helmLogConfigKey: []byte("value1"),
		},
	}

	_, err := clientset.CoreV1().Secrets(namespace).Create(ctx, secret, metav1.CreateOptions{})
	require.NoError(t, err)

	err = ConvertSecretToConfigMap(ctx, clientset, namespace, secretName)
	require.NoError(t, err)

	configMap, err := clientset.CoreV1().ConfigMaps(namespace).Get(ctx, secretName, metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, "value1", configMap.Data[enterpriseOperatorLogConfigKey])
}

func TestMoveLoggingConfig(t *testing.T) {
	clientset := fake.NewSimpleClientset()
	ctx := context.TODO()
	namespace := "default"
	configMapName := "cockroachdb-log-config"

	logConfigMap := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      configMapName,
			Namespace: namespace,
		},
		Data: map[string]string{
			"logging.yaml": "value1",
		},
	}
	_, err := clientset.CoreV1().ConfigMaps(namespace).Create(ctx, logConfigMap, metav1.CreateOptions{})
	require.NoError(t, err)

	err = moveConfigMapKey(ctx, clientset, namespace, configMapName)
	require.NoError(t, err)

	updateLogCM, err := clientset.CoreV1().ConfigMaps(namespace).Get(ctx, configMapName, metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, "value1", updateLogCM.Data[enterpriseOperatorLogConfigKey])

}

func TestExtractJoinStringAndFlags(t *testing.T) {
	input := parsedMigrationInput{tlsEnabled: true}

	args := []string{
		"--join=${STATEFULSET_NAME}-0.${STATEFULSET_FQDN}:26257,${STATEFULSET_NAME}-1.${STATEFULSET_FQDN}:26257",
		"--advertise-host=$(hostname).${STATEFULSET_FQDN}",
		"--http-port=8080",
		"--port=26257",
		"--cache=25%",
		"--max-sql-memory=25%",
		"--logtostderr=INFO",
	}

	expectedJoinString := "${STATEFULSET_NAME}-0.${STATEFULSET_FQDN}:26257,${STATEFULSET_NAME}-1.${STATEFULSET_FQDN}:26257"
	expectedFlags := map[string]string{
		"--advertise-host": "$(hostname).${STATEFULSET_FQDN}",
		"--cache":          "25%",
		"--max-sql-memory": "25%",
	}

	err := extractJoinStringAndFlags(&input, args)

	assert.NoError(t, err)
	assert.Equal(t, int32(26257), input.sqlPort)
	assert.Equal(t, int32(26258), input.grpcPort)
	assert.Equal(t, int32(8080), input.httpPort)
	assert.Equal(t, expectedJoinString, input.joinCmd)
	assert.Equal(t, expectedFlags, input.flags)
}

func TestGenerateParsedMigrationInput(t *testing.T) {
	clientset := fake.NewSimpleClientset()
	ctx := context.TODO()
	namespace := "default"
	secretName := "cockroachdb-log-config"

	// Create a secret to be converted to a ConfigMap
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretName,
			Namespace: namespace,
		},
		Data: map[string][]byte{
			"key1": []byte("value1"),
		},
	}
	_, err := clientset.CoreV1().Secrets(namespace).Create(ctx, secret, metav1.CreateOptions{})
	require.NoError(t, err)

	sts := appsv1.StatefulSet{}
	manifestBytes, err := os.ReadFile("testdata/cockroachdb-statefulset.yaml")
	require.NoError(t, err)
	err = yaml.Unmarshal(manifestBytes, &sts)
	require.NoError(t, err)

	input, err := generateParsedMigrationInput(ctx, clientset, &sts)
	require.NoError(t, err)

	// Verify the parsed input
	assert.Equal(t, "${STATEFULSET_NAME}-0.${STATEFULSET_FQDN}:26257,${STATEFULSET_NAME}-1.${STATEFULSET_FQDN}:26257,${STATEFULSET_NAME}-2.${STATEFULSET_FQDN}:26257", input.joinCmd)
	assert.Equal(t, int32(26258), input.grpcPort)
	assert.Equal(t, int32(26257), input.sqlPort)
	assert.Equal(t, int32(8080), input.httpPort)
	assert.Equal(t, true, input.tlsEnabled)
	assert.Equal(t, secretName, input.loggingConfigMap)
	assert.Equal(t, []string{"country", "region"}, input.localityLabels)
	assert.Equal(t, input.flags["--cache"], "25%")
}

func TestUpdatePublicService(t *testing.T) {
	var grpcFound, sqlFound bool
	clientset := fake.NewSimpleClientset()
	ctx := context.TODO()
	namespace := "default"
	serviceName := "cockroachdb-public"

	svc := &corev1.Service{}
	manifestBytes, err := os.ReadFile("testdata/cockroachdb-public.yaml")
	require.NoError(t, err)
	err = yaml.Unmarshal(manifestBytes, svc)
	require.NoError(t, err)

	_, err = clientset.CoreV1().Services(namespace).Create(ctx, svc, metav1.CreateOptions{})
	require.NoError(t, err)

	err = generateUpdatedPublicServiceConfig(ctx, clientset, namespace, serviceName, ".")
	require.NoError(t, err)

	svcBytes, err := os.ReadFile(publicSvcYaml)
	require.NoError(t, err)

	// remove the public-service.yaml file generated as a part of this test.
	defer os.Remove(publicSvcYaml)

	err = yaml.Unmarshal(svcBytes, svc)
	require.NoError(t, err)

	for i := range svc.Spec.Ports {
		if svc.Spec.Ports[i].Name == "grpc" {
			grpcFound = true
		}
		if svc.Spec.Ports[i].Name == "sql" {
			sqlFound = true
		}
	}

	if !grpcFound || !sqlFound {
		t.FailNow()
	}
}

func TestBuildRBACFromPublicOperator(t *testing.T) {
	cluster := publicv1alpha1.CrdbCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "crdb-cluster",
			Namespace: "test-ns",
		},
	}

	err := buildRBACFromPublicOperator(cluster, ".")
	require.NoError(t, err)

	// Read and parse the generated RBAC yaml file
	manifestBytes, err := os.ReadFile("rbac.yaml")
	require.NoError(t, err)
	defer os.Remove("rbac.yaml")

	// Split the yaml into separate documents
	docs := bytes.Split(manifestBytes, []byte("---\n"))

	// Parse and validate ClusterRole
	var clusterRole rbacv1.ClusterRole
	err = yaml.Unmarshal(docs[0], &clusterRole)
	require.NoError(t, err)

	assert.Equal(t, "crdb-cluster", clusterRole.Name)
	assert.Equal(t, "crdb-cluster", clusterRole.Annotations["meta.helm.sh/release-name"])
	assert.Equal(t, "test-ns", clusterRole.Annotations["meta.helm.sh/release-namespace"])
	assert.Equal(t, "Helm", clusterRole.Labels["app.kubernetes.io/managed-by"])

	// Verify rules
	require.Len(t, clusterRole.Rules, 2)

	// Check nodes rule
	assert.Equal(t, []string{""}, clusterRole.Rules[0].APIGroups)
	assert.Equal(t, []string{"nodes"}, clusterRole.Rules[0].Resources)
	assert.Equal(t, []string{"get"}, clusterRole.Rules[0].Verbs)

	// Check CSR rule
	assert.Equal(t, []string{"certificates.k8s.io"}, clusterRole.Rules[1].APIGroups)
	assert.Equal(t, []string{"certificatesigningrequests"}, clusterRole.Rules[1].Resources)
	assert.Equal(t, []string{"create", "get", "watch"}, clusterRole.Rules[1].Verbs)

	// Parse and validate ClusterRoleBinding
	var clusterRoleBinding rbacv1.ClusterRoleBinding
	err = yaml.Unmarshal(docs[1], &clusterRoleBinding)
	require.NoError(t, err)

	assert.Equal(t, "crdb-cluster", clusterRoleBinding.Name)
	assert.Equal(t, "crdb-cluster", clusterRoleBinding.Annotations["meta.helm.sh/release-name"])
	assert.Equal(t, "test-ns", clusterRoleBinding.Annotations["meta.helm.sh/release-namespace"])
	assert.Equal(t, "Helm", clusterRoleBinding.Labels["app.kubernetes.io/managed-by"])

	// Verify RoleRef
	assert.Equal(t, "rbac.authorization.k8s.io", clusterRoleBinding.RoleRef.APIGroup)
	assert.Equal(t, "ClusterRole", clusterRoleBinding.RoleRef.Kind)
	assert.Equal(t, "crdb-cluster", clusterRoleBinding.RoleRef.Name)
}
