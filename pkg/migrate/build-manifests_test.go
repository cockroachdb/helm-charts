package migrate

import (
	"context"
	"flag"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/cockroachdb/helm-charts/pkg/upstream/cockroach-operator/api/v1alpha1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	batchv1 "k8s.io/api/batch/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
	"sigs.k8s.io/yaml"
)

var updateGolden = flag.Bool("update", false, "update golden files")

func TestFromHelmChart(t *testing.T) {
	// Setup fake Kubernetes client
	clientset := fake.NewSimpleClientset()
	dynamicClient := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme())
	ctx := context.TODO()
	namespace := "default"
	outputDir := t.TempDir()

	job := &batchv1.Job{}
	manifestBytes, err := os.ReadFile("testdata/helm/allInput/cockroachdb-init.yaml")
	require.NoError(t, err)
	err = yaml.Unmarshal(manifestBytes, &job)
	require.NoError(t, err)
	_, err = clientset.BatchV1().Jobs(namespace).Create(ctx, job, metav1.CreateOptions{})
	require.NoError(t, err)

	sts := appsv1.StatefulSet{}
	manifestBytes, err = os.ReadFile("testdata/helm/allInput/cockroachdb-statefulset.yaml")
	require.NoError(t, err)
	err = yaml.Unmarshal(manifestBytes, &sts)
	require.NoError(t, err)
	// Create a StatefulSet
	_, err = clientset.AppsV1().StatefulSets(namespace).Create(ctx, &sts, metav1.CreateOptions{})
	require.NoError(t, err)
	// Create Pods
	for i := 0; i < 3; i++ {
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      sts.Name + "-" + strconv.Itoa(i),
				Namespace: namespace,
			},

			Spec: sts.Spec.Template.Spec,
		}
		pod.Spec.NodeName = "node" + strconv.Itoa(i)
		_, err := clientset.CoreV1().Pods(namespace).Create(ctx, pod, metav1.CreateOptions{})
		require.NoError(t, err)
	}
	// Create the public service
	svc := &corev1.Service{}
	manifestBytes, err = os.ReadFile("testdata/helm/allInput/cockroachdb-public.yaml")
	require.NoError(t, err)
	err = yaml.Unmarshal(manifestBytes, &svc)
	require.NoError(t, err)
	_, err = clientset.CoreV1().Services(namespace).Create(ctx, svc, metav1.CreateOptions{})
	require.NoError(t, err)
	// Create the Logging Config Secret
	loggingConfigSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "cockroachdb-log-config",
			Namespace: sts.Namespace,
		},
		Immutable: nil,
		Data:      map[string][]byte{"log-config.yaml": []byte("testdata")},
	}

	_, err = clientset.CoreV1().Secrets(namespace).Create(ctx, loggingConfigSecret, metav1.CreateOptions{})
	require.NoError(t, err)
	// Create a Manifest instance
	m := &Manifest{
		cloudProvider: "gcp",
		cloudRegion:   "us-central1",
		objectName:    sts.Name,
		namespace:     sts.Namespace,
		outputDir:     outputDir,
		clientset:     clientset,
		dynamicClient: dynamicClient,
	}

	// Run the FromHelmChart function
	err = m.FromHelmChart()
	require.NoError(t, err)

	// Validate generated files against golden files
	validateGoldenFile(t, filepath.Join(outputDir, "values.yaml"), "testdata/helm/allInput/values.yaml.golden")
	for i := 0; i < 3; i++ {
		validateGoldenFile(t, filepath.Join(outputDir, "crdbnode-"+strconv.Itoa(i)+".yaml"), "testdata/helm/allInput/crdbnode-"+strconv.Itoa(i)+".yaml.golden")
	}
}

func TestFromOperator(t *testing.T) {
	// setup fake Kubernetes client
	clientset := fake.NewSimpleClientset()
	dynamicClient := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme())
	ctx := context.TODO()
	namespace := "default"
	outputDir := t.TempDir()

	// Load and create CrdbCluster (as unstructured)
	crdbClusterBytes, err := os.ReadFile("testdata/operator/allInput/crdbcluster.yaml")
	require.NoError(t, err)
	var unstructuredCrdbCluster map[string]interface{}
	require.NoError(t, yaml.Unmarshal(crdbClusterBytes, &unstructuredCrdbCluster))
	gvr := schema.GroupVersionResource{
		Group:    "crdb.cockroachlabs.com",
		Version:  "v1alpha1",
		Resource: "crdbclusters",
	}
	_, err = dynamicClient.Resource(gvr).Namespace(namespace).Create(
		ctx,
		&unstructured.Unstructured{Object: unstructuredCrdbCluster},
		metav1.CreateOptions{},
	)
	require.NoError(t, err)

	// Load and create StatefulSet
	sts := appsv1.StatefulSet{}
	stsBytes, err := os.ReadFile("testdata/operator/allInput/cockroachdb-statefulset.yaml")
	require.NoError(t, err)
	require.NoError(t, yaml.Unmarshal(stsBytes, &sts))
	_, err = clientset.AppsV1().StatefulSets(namespace).Create(ctx, &sts, metav1.CreateOptions{})
	require.NoError(t, err)

	// Create Pods
	for i := 0; i < 3; i++ {
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      sts.Name + "-" + strconv.Itoa(i),
				Namespace: namespace,
			},
			Spec: sts.Spec.Template.Spec,
		}
		pod.Spec.NodeName = "node" + strconv.Itoa(i)
		_, err := clientset.CoreV1().Pods(namespace).Create(ctx, pod, metav1.CreateOptions{})
		require.NoError(t, err)
	}

	// Create the Logging Config Secret
	loggingConfigMap := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "cockroachdb-log-config",
			Namespace: sts.Namespace,
		},
		Data: map[string]string{"logging.yaml": "testdata"},
	}
	_, err = clientset.CoreV1().ConfigMaps(namespace).Create(ctx, loggingConfigMap, metav1.CreateOptions{})
	require.NoError(t, err)

	// Create Manifest
	m := &Manifest{
		cloudProvider: "gcp",
		cloudRegion:   "us-central1",
		objectName:    sts.Name,
		namespace:     sts.Namespace,
		outputDir:     outputDir,
		clientset:     clientset,
		dynamicClient: dynamicClient,
	}

	// Run FromPublicOperator
	err = m.FromPublicOperator()
	require.NoError(t, err)

	// Validate generated files against golden files
	validateGoldenFile(t, filepath.Join(outputDir, "values.yaml"), "testdata/operator/allInput/values.yaml.golden")
	for i := 0; i < 3; i++ {
		validateGoldenFile(t, filepath.Join(outputDir, "crdbnode-"+strconv.Itoa(i)+".yaml"), "testdata/operator/allInput/crdbnode-"+strconv.Itoa(i)+".yaml.golden")
	}

}

// validateGoldenFile compares the generated file with the golden file
func validateGoldenFile(t *testing.T, generatedFile, goldenFile string) {
	generated, err := os.ReadFile(generatedFile)
	require.NoError(t, err)

	golden, err := os.ReadFile(goldenFile)
	require.NoError(t, err)

	if *updateGolden {
		generatedData, err := os.ReadFile(generatedFile)
		require.NoError(t, err)
		err = os.WriteFile(goldenFile, generatedData, 0644)
		require.NoError(t, err)
		t.Logf("Updated golden file: %s", goldenFile)
	}

	assert.Equal(t, string(golden), string(generated), "Generated file does not match golden file. Please run the test with the -update flag to update the golden files.")
}

func TestFromHelmChart_WithWalFailover(t *testing.T) {
	clientset := fake.NewSimpleClientset()
	dynamicClient := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme())
	ctx := context.TODO()
	namespace := "default"
	outputDir := t.TempDir()

	// Create a StatefulSet with WAL failover configuration
	replicas := int32(3)
	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "cockroachdb",
			Namespace: namespace,
		},
		Spec: appsv1.StatefulSetSpec{
			Replicas: &replicas,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{"app": "cockroachdb"},
				},
				Spec: corev1.PodSpec{
					TerminationGracePeriodSeconds: &[]int64{60}[0],
					Containers: []corev1.Container{
						{
							Name:  "db",
							Image: "cockroachdb/cockroach:v25.3.1",
							Args: []string{
								"/cockroach/cockroach.sh",
								"start",
								"--join=cockroachdb-0.cockroachdb:26257 --advertise-host=$(hostname).cockroachdb --wal-failover=path=/custom/wal",
							},
						},
					},
				},
			},
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

	_, err := clientset.AppsV1().StatefulSets(namespace).Create(ctx, sts, metav1.CreateOptions{})
	require.NoError(t, err)

	// First create the PVCs, then create pods with failover PVCs
	for i := 0; i < 3; i++ {
		// Create the corresponding PVC for each pod FIRST
		storageClassName := "fast-ssd"
		pvc := &corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "failoverdir-cockroachdb-" + strconv.Itoa(i),
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
		_, err = clientset.CoreV1().PersistentVolumeClaims(namespace).Create(ctx, pvc, metav1.CreateOptions{})
		require.NoError(t, err)
	}

	// Now create pods that reference the PVCs
	for i := 0; i < 3; i++ {
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      sts.Name + "-" + strconv.Itoa(i),
				Namespace: namespace,
			},
			Spec: corev1.PodSpec{
				NodeName: "node" + strconv.Itoa(i),
				Containers: []corev1.Container{
					{
						Name:  "db",
						Image: "cockroachdb/cockroach:v25.3.1",
						Args: []string{
							"/cockroach/cockroach.sh",
							"start",
							"--join=cockroachdb-0.cockroachdb:26257 --advertise-host=$(hostname).cockroachdb --wal-failover=path=/custom/wal",
						},
					},
				},
				Volumes: []corev1.Volume{
					{
						Name: "failoverdir",
						VolumeSource: corev1.VolumeSource{
							PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
								ClaimName: "failoverdir-cockroachdb-" + strconv.Itoa(i),
							},
						},
					},
				},
			},
		}
		_, err := clientset.CoreV1().Pods(namespace).Create(ctx, pod, metav1.CreateOptions{})
		require.NoError(t, err)
	}

	// Create the public service
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "cockroachdb-public",
			Namespace: namespace,
		},
		Spec: corev1.ServiceSpec{
			Ports: []corev1.ServicePort{
				{Name: "http", Port: 8080},
			},
		},
	}
	_, err = clientset.CoreV1().Services(namespace).Create(ctx, svc, metav1.CreateOptions{})
	require.NoError(t, err)

	// Create Manifest
	m := &Manifest{
		cloudProvider: "gcp",
		cloudRegion:   "us-central1",
		objectName:    sts.Name,
		namespace:     sts.Namespace,
		outputDir:     outputDir,
		clientset:     clientset,
		dynamicClient: dynamicClient,
	}

	// Run FromHelmChart
	err = m.FromHelmChart()
	require.NoError(t, err)

	// Verify that values.yaml contains walFailoverSpec
	valuesBytes, err := os.ReadFile(filepath.Join(outputDir, "values.yaml"))
	require.NoError(t, err)

	var valuesData map[string]interface{}
	err = yaml.Unmarshal(valuesBytes, &valuesData)
	require.NoError(t, err)

	cockroachdb, ok := valuesData["cockroachdb"].(map[string]interface{})
	require.True(t, ok)

	crdbCluster, ok := cockroachdb["crdbCluster"].(map[string]interface{})
	require.True(t, ok)

	walFailoverSpec, ok := crdbCluster["walFailoverSpec"].(map[string]interface{})
	require.True(t, ok, "walFailoverSpec should be present in generated values.yaml")

	assert.Equal(t, "enable", walFailoverSpec["status"])
	assert.Equal(t, "/custom/wal", walFailoverSpec["path"])
	assert.Equal(t, "failoverdir", walFailoverSpec["name"])
	assert.Equal(t, "50Gi", walFailoverSpec["size"])
	assert.Equal(t, "fast-ssd", walFailoverSpec["storageClassName"])

	// Verify that each CrdbNode contains walFailoverSpec
	for i := 0; i < 3; i++ {
		nodeBytes, err := os.ReadFile(filepath.Join(outputDir, "crdbnode-"+strconv.Itoa(i)+".yaml"))
		require.NoError(t, err)

		var nodeData v1alpha1.CrdbNode
		err = yaml.Unmarshal(nodeBytes, &nodeData)
		require.NoError(t, err)

		require.NotNil(t, nodeData.Spec.WALFailoverSpec, "WAL failover spec should be present in node %d", i)
		assert.Equal(t, v1alpha1.CrdbWalFailoverStatus("enable"), nodeData.Spec.WALFailoverSpec.Status)
		assert.Equal(t, "/custom/wal", nodeData.Spec.WALFailoverSpec.Path)
		assert.Equal(t, "failoverdir", nodeData.Spec.WALFailoverSpec.Name)
		assert.Equal(t, "50Gi", nodeData.Spec.WALFailoverSpec.Size)
		assert.Equal(t, "fast-ssd", nodeData.Spec.WALFailoverSpec.StorageClassName)
	}
}

func TestFromHelmChart_WithWalFailoverDisabled(t *testing.T) {
	clientset := fake.NewSimpleClientset()
	dynamicClient := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme())
	ctx := context.TODO()
	namespace := "default"
	outputDir := t.TempDir()

	// Create a StatefulSet with WAL failover disabled
	replicas := int32(2)
	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "cockroachdb",
			Namespace: namespace,
		},
		Spec: appsv1.StatefulSetSpec{
			Replicas: &replicas,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{"app": "cockroachdb"},
				},
				Spec: corev1.PodSpec{
					TerminationGracePeriodSeconds: &[]int64{60}[0],
					Containers: []corev1.Container{
						{
							Name:  "db",
							Image: "cockroachdb/cockroach:v25.3.1",
							Args: []string{
								"/cockroach/cockroach.sh",
								"start",
								"--join=cockroachdb-0.cockroachdb:26257 --advertise-host=$(hostname).cockroachdb --wal-failover=disabled",
							},
						},
					},
				},
			},
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
			},
		},
	}

	_, err := clientset.AppsV1().StatefulSets(namespace).Create(ctx, sts, metav1.CreateOptions{})
	require.NoError(t, err)

	// Create pods
	for i := 0; i < 2; i++ {
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      sts.Name + "-" + strconv.Itoa(i),
				Namespace: namespace,
			},
			Spec: corev1.PodSpec{
				NodeName: "node" + strconv.Itoa(i),
				Containers: []corev1.Container{
					{
						Name:  "db",
						Image: "cockroachdb/cockroach:v25.3.1",
						Args: []string{
							"/cockroach/cockroach.sh",
							"start",
							"--join=cockroachdb-0.cockroachdb:26257 --advertise-host=$(hostname).cockroachdb --wal-failover=disabled",
						},
					},
				},
			},
		}
		_, err := clientset.CoreV1().Pods(namespace).Create(ctx, pod, metav1.CreateOptions{})
		require.NoError(t, err)
	}

	// Create the public service
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "cockroachdb-public",
			Namespace: namespace,
		},
		Spec: corev1.ServiceSpec{
			Ports: []corev1.ServicePort{
				{Name: "http", Port: 8080},
			},
		},
	}
	_, err = clientset.CoreV1().Services(namespace).Create(ctx, svc, metav1.CreateOptions{})
	require.NoError(t, err)

	// Create Manifest
	m := &Manifest{
		cloudProvider: "gcp",
		cloudRegion:   "us-central1",
		objectName:    sts.Name,
		namespace:     sts.Namespace,
		outputDir:     outputDir,
		clientset:     clientset,
		dynamicClient: dynamicClient,
	}

	// Run FromHelmChart
	err = m.FromHelmChart()
	require.NoError(t, err)

	// Verify that values.yaml contains walFailoverSpec with disabled status
	valuesBytes, err := os.ReadFile(filepath.Join(outputDir, "values.yaml"))
	require.NoError(t, err)

	var valuesData map[string]interface{}
	err = yaml.Unmarshal(valuesBytes, &valuesData)
	require.NoError(t, err)

	cockroachdb, ok := valuesData["cockroachdb"].(map[string]interface{})
	require.True(t, ok)

	crdbCluster, ok := cockroachdb["crdbCluster"].(map[string]interface{})
	require.True(t, ok)

	walFailoverSpec, ok := crdbCluster["walFailoverSpec"].(map[string]interface{})
	require.True(t, ok, "walFailoverSpec should be present in generated values.yaml")

	assert.Equal(t, "disable", walFailoverSpec["status"])

	// Verify that each CrdbNode contains walFailoverSpec with disabled status
	for i := 0; i < 2; i++ {
		nodeBytes, err := os.ReadFile(filepath.Join(outputDir, "crdbnode-"+strconv.Itoa(i)+".yaml"))
		require.NoError(t, err)

		var nodeData v1alpha1.CrdbNode
		err = yaml.Unmarshal(nodeBytes, &nodeData)
		require.NoError(t, err)

		require.NotNil(t, nodeData.Spec.WALFailoverSpec, "WAL failover spec should be present in node %d", i)
		assert.Equal(t, v1alpha1.CrdbWalFailoverStatus("disable"), nodeData.Spec.WALFailoverSpec.Status)
	}
}
