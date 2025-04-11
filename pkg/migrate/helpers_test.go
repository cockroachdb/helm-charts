package migrate

import (
	"context"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
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

func TestReplaceEnvVars(t *testing.T) {
	envVars := map[string]string{
		"VAR1": "value1",
		"VAR2": "value2",
	}

	tests := []struct {
		input    string
		expected string
	}{
		{"$VAR1", "value1"},
		{"${VAR2}", "value2"},
		{"${VAR3}", "${VAR3}"}, // No replacement found
		{"text $VAR1 text", "text value1 text"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			result := replaceEnvVars(tt.input, envVars)
			assert.Equal(t, tt.expected, result)
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
			"key1": []byte("value1"),
			"key2": []byte("value2"),
		},
	}

	_, err := clientset.CoreV1().Secrets(namespace).Create(ctx, secret, metav1.CreateOptions{})
	require.NoError(t, err)

	err = ConvertSecretToConfigMap(ctx, clientset, namespace, secretName)
	require.NoError(t, err)

	configMap, err := clientset.CoreV1().ConfigMaps(namespace).Get(ctx, secretName, metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, "value1", configMap.Data["key1"])
	assert.Equal(t, "value2", configMap.Data["key2"])
}

func TestExtractJoinStringAndFlags(t *testing.T) {
	input := parsedMigrationInput{tlsEnabled: true}
	envVars := map[string]string{
		"STATEFULSET_NAME": "cockroachdb",
		"STATEFULSET_FQDN": "cockroachdb.default.svc.cluster.local",
	}

	args := []string{
		"--join=${STATEFULSET_NAME}-0.${STATEFULSET_FQDN}:26257,${STATEFULSET_NAME}-1.${STATEFULSET_FQDN}:26257",
		"--advertise-host=$(hostname).${STATEFULSET_FQDN}",
		"--http-port=8080",
		"--port=26257",
		"--cache=25%",
		"--max-sql-memory=25%",
		"--logtostderr=INFO",
	}

	expectedJoinString := "cockroachdb-0.cockroachdb.default.svc.cluster.local:26257,cockroachdb-1.cockroachdb.default.svc.cluster.local:26257"
	expectedFlags := map[string]string{
		"--advertise-host": "$(hostname).cockroachdb.default.svc.cluster.local",
		"--cache":          "25%",
		"--max-sql-memory": "25%",
	}

	err := extractJoinStringAndFlags(&input, args, envVars)

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
	assert.Equal(t, "cockroachdb-0.cockroachdb.default.svc.cluster.local:26257,cockroachdb-1.cockroachdb.default.svc.cluster.local:26257,cockroachdb-2.cockroachdb.default.svc.cluster.local:26257", input.joinCmd)
	assert.Equal(t, int32(26258), input.grpcPort)
	assert.Equal(t, int32(26257), input.sqlPort)
	assert.Equal(t, int32(8080), input.httpPort)
	assert.Equal(t, true, input.tlsEnabled)
	assert.Equal(t, secretName, input.loggingConfigMap)
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

	err = updatePublicService(ctx, clientset, namespace, serviceName)
	require.NoError(t, err)

	svc, err = clientset.CoreV1().Services(namespace).Get(ctx, serviceName, metav1.GetOptions{})
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
