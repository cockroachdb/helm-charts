package migrate

import (
	"context"
	"flag"
	"os"
	"path/filepath"
	"sigs.k8s.io/yaml"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
)

var updateGolden = flag.Bool("update", false, "update golden files")

func TestFromHelmChart(t *testing.T) {
	// Setup fake Kubernetes client
	clientset := fake.NewSimpleClientset()
	dynamicClient := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme())
	ctx := context.TODO()
	namespace := "default"
	outputDir := t.TempDir()

	sts := appsv1.StatefulSet{}
	manifestBytes, err := os.ReadFile("testdata/helm/allInput/cockroachdb-statefulset.yaml")
	require.NoError(t, err)
	err = yaml.Unmarshal(manifestBytes, &sts)
	require.NoError(t, err)
	// Create a StatefulSet
	_, err = clientset.AppsV1().StatefulSets(namespace).Create(ctx, &sts, metav1.CreateOptions{})
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

	assert.Equal(t, string(golden), string(generated), "Generated file does not match golden file")
}
