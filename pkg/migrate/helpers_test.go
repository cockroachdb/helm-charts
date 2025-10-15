package migrate

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	publicv1alpha1 "github.com/cockroachdb/cockroach-operator/apis/v1alpha1"
	"github.com/cockroachdb/helm-charts/pkg/upstream/cockroach-operator/api/v1alpha1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/resource"
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
	expectedFlags := []string{
		fmt.Sprintf("--join=%s", expectedJoinString),
		"--advertise-host=$(hostname).${STATEFULSET_FQDN}",
		"--cache=25%",
		"--max-sql-memory=25%",
	}

	err := extractJoinStringAndFlags(&input, args)

	assert.NoError(t, err)
	assert.Equal(t, int32(26257), input.sqlPort)
	assert.Equal(t, int32(26258), input.grpcPort)
	assert.Equal(t, int32(8080), input.httpPort)
	assert.Equal(t, expectedFlags, input.startFlags.Upsert)
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
	assert.Equal(t, int32(26258), input.grpcPort)
	assert.Equal(t, int32(26257), input.sqlPort)
	assert.Equal(t, int32(8080), input.httpPort)
	assert.Equal(t, true, input.tlsEnabled)
	assert.Equal(t, secretName, input.loggingConfigMap)
	assert.Equal(t, []string{"country", "region"}, input.localityLabels)
	expectedJoinString := "${STATEFULSET_NAME}-0.${STATEFULSET_FQDN}:26257,${STATEFULSET_NAME}-1.${STATEFULSET_FQDN}:26257,${STATEFULSET_NAME}-2.${STATEFULSET_FQDN}:26257"
	expectedFlags := []string{
		fmt.Sprintf("--join=%s", expectedJoinString),
		"--advertise-host=$(hostname).${STATEFULSET_FQDN}",
		"--certs-dir=/cockroach/cockroach-certs/",
		"--cache=25%",
		"--max-sql-memory=25%",
		"--wal-failover=path=/cockroach/wal-failover",
	}
	assert.Equal(t, expectedFlags, input.startFlags.Upsert)

	// Note: WAL failover spec is not built by generateParsedMigrationInput
	// It's built separately by buildWalFailoverSpec function when needed
	assert.Nil(t, input.walFailoverSpec, "WAL failover spec should not be built by generateParsedMigrationInput")

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

func TestDetectPCRFromInitJob(t *testing.T) {
	tests := []struct {
		name         string
		jobCommand   []string
		expectedMode *v1alpha1.CrdbVirtualClusterSpec
		jobExists    bool
	}{
		{
			name:         "Job does not exist",
			jobCommand:   nil,
			expectedMode: nil,
			jobExists:    false,
		},
		{
			name:         "Job with --virtualized flag (primary)",
			jobCommand:   []string{"/bin/bash", "-c", "cockroach init --virtualized --host=test:26257"},
			expectedMode: &v1alpha1.CrdbVirtualClusterSpec{Mode: v1alpha1.VirtualClusterPrimary},
			jobExists:    true,
		},
		{
			name:         "Job with --virtualized-empty flag (standby)",
			jobCommand:   []string{"/bin/bash", "-c", "cockroach init --virtualized-empty --host=test:26257"},
			expectedMode: &v1alpha1.CrdbVirtualClusterSpec{Mode: v1alpha1.VirtualClusterStandby},
			jobExists:    true,
		},
		{
			name:         "Job without PCR flags",
			jobCommand:   []string{"/bin/bash", "-c", "cockroach init --host=test:26257"},
			expectedMode: nil,
			jobExists:    true,
		},
		{
			name:         "Job with empty containers",
			jobCommand:   nil,
			expectedMode: nil,
			jobExists:    true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create a fake clientset
			clientset := fake.NewSimpleClientset()
			ctx := context.TODO()
			namespace := "test-namespace"
			stsName := "test-cockroachdb"

			if tt.jobExists {
				// Create a job with the specified command
				job := &batchv1.Job{
					ObjectMeta: metav1.ObjectMeta{
						Name:      stsName + "-init",
						Namespace: namespace,
					},
					Spec: batchv1.JobSpec{
						Template: corev1.PodTemplateSpec{
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{
									{
										Command: tt.jobCommand,
									},
								},
							},
						},
					},
				}
				_, err := clientset.BatchV1().Jobs(namespace).Create(ctx, job, metav1.CreateOptions{})
				require.NoError(t, err)
			}

			// Test the function
			result := detectPCRFromInitJob(clientset, stsName, namespace)
			if tt.expectedMode == nil {
				require.Nil(t, result)
			} else {
				require.NotNil(t, result)
				require.Equal(t, tt.expectedMode.Mode, result.Mode)
			}
		})
	}
}

func TestBuildHelmValuesFromHelm_WALFailover(t *testing.T) {
					namespace := "default"

					// Create base StatefulSet for testing (using realistic values)
					terminationGracePeriod := int64(60)
					replicaCount := int32(3) // Standard CockroachDB cluster size

					sts := &appsv1.StatefulSet{
						ObjectMeta: metav1.ObjectMeta{
							Name:      "cockroachdb",
							Namespace: namespace,
						},
						Spec: appsv1.StatefulSetSpec{
							Replicas: &replicaCount,
							Template: corev1.PodTemplateSpec{
								ObjectMeta: metav1.ObjectMeta{
									Labels:      map[string]string{"app": "cockroachdb"},
									Annotations: map[string]string{"annotation": "value"},
								},
								Spec: corev1.PodSpec{
									TerminationGracePeriodSeconds: &terminationGracePeriod,
									Containers: []corev1.Container{
										{
											Name:      "cockroachdb",
											Image:     "cockroachdb/cockroach:v25.3.1",
											Resources: corev1.ResourceRequirements{},
											Env:       []corev1.EnvVar{},
										},
									},
								},
							},
							VolumeClaimTemplates: []corev1.PersistentVolumeClaim{
								{
									ObjectMeta: metav1.ObjectMeta{Name: "datadir"},
									Spec: corev1.PersistentVolumeClaimSpec{
										AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
									},
								},
							},
						},
					}

					testCases := []struct {
						name           string
						walFailoverArg string
						expectedPath   string
						expectedStatus string
						expectWarning  bool
					}{
						{
							name:           "WAL failover with absolute path",
							walFailoverArg: "--wal-failover=path=/cockroach/custom-failover",
							expectedPath:   "/cockroach/custom-failover",
							expectedStatus: "enable",
							expectWarning:  false,
						},
						{
							name:           "WAL failover with relative path",
							walFailoverArg: "--wal-failover=path=custom-failover",
							expectedPath:   "custom-failover",
							expectedStatus: "enable",
							expectWarning:  true,
						},
						{
							name:           "WAL failover disabled",
							walFailoverArg: "--wal-failover=disabled",
							expectedPath:   "",
							expectedStatus: "disable",
							expectWarning:  false,
						},
						{
							name:           "WAL failover among stores",
							walFailoverArg: "--wal-failover=among-stores",
							expectedPath:   "",
							expectedStatus: "", // No WAL failover spec should be created for among-stores
							expectWarning:  false,
						},
					}

					for _, tc := range testCases {
						t.Run(tc.name, func(t *testing.T) {
							// Create input with WAL failover flag
							input := parsedMigrationInput{
								tlsEnabled:     true,
								sqlPort:        26257,
								grpcPort:       26258,
								httpPort:       8080,
								localityLabels: []string{"region", "zone"},
								startFlags: &v1alpha1.Flags{
									Upsert: []string{
										"--join=cockroachdb-0.cockroachdb:26257",
										"--advertise-host=$(hostname).cockroachdb",
										tc.walFailoverArg,
									},
								},
							}

							// Build WAL failover spec if the input contains WAL failover flags
							// Mock the necessary dependencies since we can't actually connect to k8s in unit tests
							if tc.walFailoverArg != "" && strings.Contains(tc.walFailoverArg, "path=") {
								// Manually set the walFailoverSpec for path-based tests since we can't mock k8s calls
								path := strings.TrimPrefix(tc.walFailoverArg, "--wal-failover=path=")
								input.walFailoverSpec = &v1alpha1.CrdbWalFailoverSpec{
									Name:             "failoverdir",
									Status:           "enable",
									Path:             path,
									Size:             "10Gi",
									StorageClassName: "fast-ssd",
								}
							} else if tc.walFailoverArg == "--wal-failover=disabled" {
								input.walFailoverSpec = &v1alpha1.CrdbWalFailoverSpec{
									Status: "disable",
								}
							}
							// Note: among-stores case doesn't set walFailoverSpec as it's not supported

							result := buildHelmValuesFromHelm(
								sts, "gcp", "us-central1", namespace, input,
							)

							// Verify the result structure
							require.NotNil(t, result)
							cockroachdbConfig, ok := result["cockroachdb"].(map[string]interface{})
							require.True(t, ok)

							crdbCluster, ok := cockroachdbConfig["crdbCluster"].(map[string]interface{})
							require.True(t, ok)

							if tc.expectedStatus != "" {
								walFailoverSpec, ok := crdbCluster["walFailoverSpec"].(map[string]interface{})
								require.True(t, ok, "Expected walFailoverSpec to be present")

								assert.Equal(t, tc.expectedStatus, string(walFailoverSpec["status"].(v1alpha1.CrdbWalFailoverStatus)))

								if tc.expectedPath != "" {
									assert.Equal(t, tc.expectedPath, walFailoverSpec["path"])
								} else {
									// Path should not exist or be empty for disabled/among-stores
									path, exists := walFailoverSpec["path"]
									if exists {
										assert.Empty(t, path)
									}
								}
							} else {
								// No WAL failover spec should be present
								_, exists := crdbCluster["walFailoverSpec"]
								assert.False(t, exists)
							}
						})
					}
				}

func TestExtractJoinStringAndFlags_WALFailover(t *testing.T) {
					testCases := []struct {
						name              string
						args              []string
						expectedWALFlag   string
						shouldContainFlag bool
					}{
						{
							name: "Args with WAL failover path",
							args: []string{
								"--join=host1:26257,host2:26257",
								"--advertise-host=$(hostname)",
								"--wal-failover=path=/custom/wal",
								"--cache=25%",
								"--http-port=8080",
								"--port=26257",
							},
							expectedWALFlag:   "--wal-failover=path=/custom/wal",
							shouldContainFlag: true,
						},
						{
							name: "Args with WAL failover disabled",
							args: []string{
								"--join=host1:26257",
								"--wal-failover=disabled",
								"--max-sql-memory=25%",
								"--http-port=8080",
								"--port=26257",
							},
							expectedWALFlag:   "--wal-failover=disabled",
							shouldContainFlag: true,
						},
						{
							name: "Args without WAL failover",
							args: []string{
								"--join=host1:26257",
								"--cache=25%",
								"--max-sql-memory=25%",
								"--http-port=8080",
								"--port=26257",
							},
							shouldContainFlag: false,
						},
					}

					for _, tc := range testCases {
						t.Run(tc.name, func(t *testing.T) {
							input := parsedMigrationInput{tlsEnabled: true}

							err := extractJoinStringAndFlags(&input, tc.args)
							assert.NoError(t, err)

							if tc.shouldContainFlag {
								assert.Contains(t, input.startFlags.Upsert, tc.expectedWALFlag)
							} else {
								// Verify no WAL failover flag exists
								for _, flag := range input.startFlags.Upsert {
									assert.NotContains(t, flag, "--wal-failover")
								}
							}
						})
					}
				}

func TestGetWalFailoverPVCDetails(t *testing.T) {
					clientset := fake.NewSimpleClientset()
					ctx := context.TODO()
					namespace := "default"
					nodeName := "test-node"

					// Create a StatefulSet with a failover PVC template
					sts := &appsv1.StatefulSet{
						ObjectMeta: metav1.ObjectMeta{
							Name:      "cockroachdb",
							Namespace: namespace,
						},
						Spec: appsv1.StatefulSetSpec{
							VolumeClaimTemplates: []corev1.PersistentVolumeClaim{
								{
									ObjectMeta: metav1.ObjectMeta{Name: "datadir"},
									Spec: corev1.PersistentVolumeClaimSpec{
										Resources: corev1.VolumeResourceRequirements{
											Requests: corev1.ResourceList{
												corev1.ResourceStorage: resource.MustParse("100Gi"),
											},
										},
									},
								},
								{
									ObjectMeta: metav1.ObjectMeta{Name: "failoverdir"},
									Spec: corev1.PersistentVolumeClaimSpec{
										Resources: corev1.VolumeResourceRequirements{
											Requests: corev1.ResourceList{
												corev1.ResourceStorage: resource.MustParse("50Gi"),
											},
										},
									},
								},
							},
						},
					}

					// Create a Pod on the specified node
					pod := &corev1.Pod{
						ObjectMeta: metav1.ObjectMeta{
							Name:      "cockroachdb-0",
							Namespace: namespace,
						},
						Spec: corev1.PodSpec{
							NodeName: nodeName,
							Volumes: []corev1.Volume{
								{
									Name: "failoverdir",
									VolumeSource: corev1.VolumeSource{
										PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
											ClaimName: "failoverdir-cockroachdb-0",
										},
									},
								},
							},
						},
					}

					// Create the actual PVC with storage class
					storageClassName := "fast-ssd"
					pvc := &corev1.PersistentVolumeClaim{
						ObjectMeta: metav1.ObjectMeta{
							Name:      "failoverdir-cockroachdb-0",
							Namespace: namespace,
						},
						Spec: corev1.PersistentVolumeClaimSpec{
							StorageClassName: &storageClassName,
							Resources: corev1.VolumeResourceRequirements{
								Requests: corev1.ResourceList{
									corev1.ResourceStorage: resource.MustParse("50Gi"),
								},
							},
						},
					}

					// Create resources in fake clientset
					_, err := clientset.CoreV1().Pods(namespace).Create(ctx, pod, metav1.CreateOptions{})
					require.NoError(t, err)
					_, err = clientset.CoreV1().PersistentVolumeClaims(namespace).Create(ctx, pvc, metav1.CreateOptions{})
					require.NoError(t, err)

					// Test successful case
					result, err := getWalFailoverPVCDetails(ctx, clientset, sts, nodeName, 0)
					require.NoError(t, err)
					assert.Equal(t, "failoverdir", result.name)
					assert.Equal(t, "50Gi", result.size)
					assert.Equal(t, "fast-ssd", result.storageClassName)

					// Test error case - wrong node
					_, err = getWalFailoverPVCDetails(ctx, clientset, sts, "wrong-node", 0)
					assert.Error(t, err)
					assert.Contains(t, err.Error(), "is not on node")

					// Test error case - no failover PVC
					stsNoFailover := &appsv1.StatefulSet{
						ObjectMeta: metav1.ObjectMeta{
							Name:      "cockroachdb",
							Namespace: namespace,
						},
						Spec: appsv1.StatefulSetSpec{
							VolumeClaimTemplates: []corev1.PersistentVolumeClaim{
								{
									ObjectMeta: metav1.ObjectMeta{Name: "datadir"},
								},
							},
						},
					}
					_, err = getWalFailoverPVCDetails(ctx, clientset, stsNoFailover, nodeName, 0)
					assert.Error(t, err)
					assert.Contains(t, err.Error(), "no failoverdir PVC found")
				}

func TestBuildWalFailoverSpec(t *testing.T) {
					clientset := fake.NewSimpleClientset()
					ctx := context.TODO()
					namespace := "default"
					nodeName := "test-node"

					// Create StatefulSet with failover PVC
					sts := &appsv1.StatefulSet{
						ObjectMeta: metav1.ObjectMeta{
							Name:      "cockroachdb",
							Namespace: namespace,
						},
						Spec: appsv1.StatefulSetSpec{
							VolumeClaimTemplates: []corev1.PersistentVolumeClaim{
								{
									ObjectMeta: metav1.ObjectMeta{Name: "failoverdir"},
									Spec: corev1.PersistentVolumeClaimSpec{
										Resources: corev1.VolumeResourceRequirements{
											Requests: corev1.ResourceList{
												corev1.ResourceStorage: resource.MustParse("50Gi"),
											},
										},
									},
								},
							},
						},
					}

					// Create pod and PVC
					pod := &corev1.Pod{
						ObjectMeta: metav1.ObjectMeta{
							Name:      "cockroachdb-0",
							Namespace: namespace,
						},
						Spec: corev1.PodSpec{
							NodeName: nodeName,
							Volumes: []corev1.Volume{
								{
									Name: "failoverdir",
									VolumeSource: corev1.VolumeSource{
										PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
											ClaimName: "failoverdir-cockroachdb-0",
										},
									},
								},
							},
						},
					}

					storageClassName := "fast-ssd"
					pvc := &corev1.PersistentVolumeClaim{
						ObjectMeta: metav1.ObjectMeta{
							Name:      "failoverdir-cockroachdb-0",
							Namespace: namespace,
						},
						Spec: corev1.PersistentVolumeClaimSpec{
							StorageClassName: &storageClassName,
						},
					}

					_, err := clientset.CoreV1().Pods(namespace).Create(ctx, pod, metav1.CreateOptions{})
					require.NoError(t, err)
					_, err = clientset.CoreV1().PersistentVolumeClaims(namespace).Create(ctx, pvc, metav1.CreateOptions{})
					require.NoError(t, err)

					testCases := []struct {
						name              string
						walFailoverFlag   string
						expectedStatus    v1alpha1.CrdbWalFailoverStatus
						expectedPath      string
						shouldHaveWalSpec bool
					}{
						{
							name:              "WAL failover enabled with path",
							walFailoverFlag:   "--wal-failover=path=/custom/wal",
							expectedStatus:    "enable",
							expectedPath:      "/custom/wal",
							shouldHaveWalSpec: true,
						},
						{
							name:              "WAL failover disabled",
							walFailoverFlag:   "--wal-failover=disabled",
							expectedStatus:    "disable",
							expectedPath:      "",
							shouldHaveWalSpec: true,
						},
						{
							name:              "WAL failover among-stores (not supported)",
							walFailoverFlag:   "--wal-failover=among-stores",
							shouldHaveWalSpec: false,
						},
						{
							name:              "No WAL failover flag",
							walFailoverFlag:   "",
							shouldHaveWalSpec: false,
						},
					}

					for _, tc := range testCases {
						t.Run(tc.name, func(t *testing.T) {
							input := &parsedMigrationInput{}
							if tc.walFailoverFlag != "" {
								input.startFlags = &v1alpha1.Flags{
									Upsert: []string{tc.walFailoverFlag},
								}
							}

							buildWalFailoverSpec(ctx, clientset, sts, nodeName, 0, input)

							if tc.shouldHaveWalSpec {
								require.NotNil(t, input.walFailoverSpec)
								assert.Equal(t, tc.expectedStatus, input.walFailoverSpec.Status)
								assert.Equal(t, tc.expectedPath, input.walFailoverSpec.Path)

								if tc.expectedStatus == "enable" {
									assert.Equal(t, "failoverdir", input.walFailoverSpec.Name)
									assert.Equal(t, "50Gi", input.walFailoverSpec.Size)
									assert.Equal(t, "fast-ssd", input.walFailoverSpec.StorageClassName)
								}
							} else {
								assert.Nil(t, input.walFailoverSpec)
							}
						})
					}
				}

func TestBuildNodeSpecFromHelm_WithWalFailover(t *testing.T) {
					namespace := "default"
					nodeName := "test-node"

					sts := &appsv1.StatefulSet{
						ObjectMeta: metav1.ObjectMeta{
							Name:      "cockroachdb",
							Namespace: namespace,
						},
						Spec: appsv1.StatefulSetSpec{
							Template: corev1.PodTemplateSpec{
								ObjectMeta: metav1.ObjectMeta{
									Labels:      map[string]string{"app": "cockroachdb"},
									Annotations: map[string]string{"annotation": "value"},
								},
								Spec: corev1.PodSpec{
									TerminationGracePeriodSeconds: To(int64(60)),
									Containers: []corev1.Container{
										{
											Name:      "cockroachdb",
											Image:     "cockroachdb/cockroach:v25.3.1",
											Resources: corev1.ResourceRequirements{},
											Env:       []corev1.EnvVar{},
										},
									},
								},
							},
							VolumeClaimTemplates: []corev1.PersistentVolumeClaim{
								{
									ObjectMeta: metav1.ObjectMeta{Name: "datadir"},
									Spec: corev1.PersistentVolumeClaimSpec{
										AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
									},
								},
							},
						},
					}

					testCases := []struct {
						name            string
						hasWalFailover  bool
						walFailoverSpec *v1alpha1.CrdbWalFailoverSpec
					}{
						{
							name:            "Node spec without WAL failover",
							hasWalFailover:  false,
							walFailoverSpec: nil,
						},
						{
							name:           "Node spec with WAL failover enabled",
							hasWalFailover: true,
							walFailoverSpec: &v1alpha1.CrdbWalFailoverSpec{
								Name:             "failoverdir",
								Status:           "enable",
								Path:             "/custom/wal",
								Size:             "50Gi",
								StorageClassName: "fast-ssd",
							},
						},
						{
							name:           "Node spec with WAL failover disabled",
							hasWalFailover: true,
							walFailoverSpec: &v1alpha1.CrdbWalFailoverSpec{
								Status: "disable",
							},
						},
					}

					for _, tc := range testCases {
						t.Run(tc.name, func(t *testing.T) {
							input := parsedMigrationInput{
								tlsEnabled:       true,
								sqlPort:          26257,
								grpcPort:         26258,
								httpPort:         8080,
								localityLabels:   []string{"region", "zone"},
								walFailoverSpec:  tc.walFailoverSpec,
								caConfigMap:      "ca-config",
								nodeSecretName:   "node-secret",
								clientSecretName: "client-secret",
								startFlags: &v1alpha1.Flags{
									Upsert: []string{"--join=host:26257"},
								},
							}

							nodeSpec := buildNodeSpecFromHelm(sts, nodeName, input)

							// Verify basic fields
							assert.Equal(t, nodeName, nodeSpec.NodeName)
							assert.Equal(t, sts.Spec.Template.Labels, nodeSpec.PodLabels)
							assert.Equal(t, sts.Spec.Template.Annotations, nodeSpec.PodAnnotations)

							// Verify WAL failover spec
							if tc.hasWalFailover {
								require.NotNil(t, nodeSpec.WALFailoverSpec)
								assert.Equal(t, tc.walFailoverSpec.Status, nodeSpec.WALFailoverSpec.Status)
								assert.Equal(t, tc.walFailoverSpec.Path, nodeSpec.WALFailoverSpec.Path)
								assert.Equal(t, tc.walFailoverSpec.Name, nodeSpec.WALFailoverSpec.Name)
								assert.Equal(t, tc.walFailoverSpec.Size, nodeSpec.WALFailoverSpec.Size)
								assert.Equal(t, tc.walFailoverSpec.StorageClassName, nodeSpec.WALFailoverSpec.StorageClassName)
							} else {
								assert.Nil(t, nodeSpec.WALFailoverSpec)
							}
						})
					}
				}
