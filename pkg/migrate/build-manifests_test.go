package migrate

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"time"

	"flag"
	certresource "github.com/cockroachdb/helm-charts/pkg/resource"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	publicv1 "github.com/cockroachdb/cockroach-operator/apis/v1alpha1"
	"github.com/cockroachdb/helm-charts/pkg/upstream/cockroach-operator/api/v1beta1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/intstr"
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

func TestManualOperatorCertificateNames(t *testing.T) {
	const name, namespace = "cockroachdb", "default"
	sts := &appsv1.StatefulSet{}
	data, err := os.ReadFile("testdata/operator/allInput/cockroachdb-statefulset.yaml")
	require.NoError(t, err)
	require.NoError(t, yaml.Unmarshal(data, sts))
	for _, tc := range []struct {
		name, node, client, wantNode, wantClient string
	}{
		{"built-in signer", "", "", name + "-node-secret", name + "-client-secret"},
		{"both overrides", "custom-node", "custom-client", "custom-node", "custom-client"},
		{"node override", "custom-node", "", "custom-node", name + "-root"},
		{"client override", "", "custom-client", name + "-node", "custom-client"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cluster := publicv1.CrdbCluster{ObjectMeta: sts.ObjectMeta, Spec: publicv1.CrdbClusterSpec{
				TLSEnabled: true, NodeTLSSecret: tc.node, ClientTLSSecret: tc.client, Nodes: 3,
				Image: &publicv1.PodImage{Name: "cockroachdb/cockroach:v26.3.5"},
			}}
			node := buildNodeSpecFromOperator(cluster, sts, "worker", &v1beta1.Flags{})
			require.Equal(t, tc.wantNode, node.Certificates.ExternalCertificates.NodeSecretName)
			require.Equal(t, tc.wantClient, node.Certificates.ExternalCertificates.RootSQLClientSecretName)
			values := buildHelmValuesFromOperator(cluster, sts, "gcp", "us-east1", namespace, &v1beta1.Flags{})
			m := &Manifest{namespace: namespace, clientset: fake.NewSimpleClientset()}
			require.NoError(t, m.restoreSelfSignerValues(context.Background(), values, name, tc.wantNode, tc.wantClient))
			valuesFile := filepath.Join(t.TempDir(), "values.yaml")
			require.NoError(t, yamlToDisk(valuesFile, []any{values}))
			chart, err := filepath.Abs("../../cockroachdb-operator/charts/cockroachdb")
			require.NoError(t, err)
			rendered, err := exec.Command("helm", "template", name, chart, "--namespace", namespace, "--values", valuesFile).CombinedOutput()
			require.NoError(t, err, "%s", rendered)
			var found bool
			for _, doc := range strings.Split(string(rendered), "\n---") {
				var obj map[string]interface{}
				require.NoError(t, yaml.Unmarshal([]byte(doc), &obj))
				require.NotEqual(t, "CronJob", obj["kind"], "external source must not gain rotation jobs")
				if obj["kind"] != "CrdbCluster" {
					continue
				}
				found = true
				refs := obj["spec"].(map[string]interface{})["template"].(map[string]interface{})["spec"].(map[string]interface{})["certificates"].(map[string]interface{})["externalCertificates"].(map[string]interface{})
				require.Equal(t, name+"-ca-crt", refs["caConfigMapName"])
				require.Equal(t, tc.wantNode, refs["nodeSecretName"])
				require.Equal(t, tc.wantClient, refs["rootSqlClientSecretName"])
				if tc.node != "" || tc.client != "" {
					require.Equal(t, tc.wantClient, refs["httpSecretName"])
				}
			}
			require.True(t, found)
		})
	}
}

func TestExportValuesFromV1beta1(t *testing.T) {
	clientset := fake.NewSimpleClientset()
	dynamicClient := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme())
	ctx := context.TODO()
	namespace := "default"
	outputDir := t.TempDir()

	cluster := v1beta1.CrdbCluster{
		TypeMeta: metav1.TypeMeta{
			Kind:       "CrdbCluster",
			APIVersion: "crdb.cockroachlabs.com/v1beta1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "cockroachdb",
			Namespace: namespace,
		},
		Spec: v1beta1.CrdbClusterSpec{
			TLSEnabled: true,
			Mode:       new(v1beta1.MutableOnly),
			Regions: []v1beta1.CrdbClusterRegion{
				{
					Code:          "us-east-1",
					CloudProvider: "k3d",
					Namespace:     namespace,
					Nodes:         3,
				},
			},
			PostInitSQL: &v1beta1.PostInitSQL{
				Inline: []string{
					"CREATE DATABASE IF NOT EXISTS smoke",
					"CREATE USER IF NOT EXISTS app_user",
				},
				ConfigMapRef: &corev1.ConfigMapKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: "bootstrap-sql"},
					Key:                  "init.sql",
				},
				SecretRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: "license-sql"},
					Key:                  "license.sql",
				},
			},
			Template: v1beta1.CrdbNodeTemplate{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{"template-scope": "ignore-me"},
					Labels:      map[string]string{"template-scope": "ignore-me"},
				},
				Spec: v1beta1.CrdbNodeSpec{
					Image: "cockroachdb/cockroach:v25.3.1",
					Certificates: v1beta1.Certificates{
						ExternalCertificates: &v1beta1.ExternalCertificates{
							CAConfigMapName:         "cockroachdb-ca-crt",
							NodeSecretName:          "cockroachdb-node-secret",
							RootSQLClientSecretName: "cockroachdb-client-secret",
							HTTPSecretName:          "cockroachdb-client-secret",
						},
					},
					DataStore: v1beta1.DataStore{
						VolumeClaimTemplate: &corev1.PersistentVolumeClaim{
							ObjectMeta: metav1.ObjectMeta{Name: "datadir"},
							Spec: corev1.PersistentVolumeClaimSpec{
								Resources: corev1.VolumeResourceRequirements{
									Requests: corev1.ResourceList{
										corev1.ResourceStorage: resource.MustParse("10Gi"),
									},
								},
							},
						},
					},
					WALFailoverSpec: &v1beta1.CrdbWalFailoverSpec{
						Name:   "failoverdir",
						Path:   "/cockroach/wal-failover",
						Size:   "1Gi",
						Status: v1beta1.WalEnable,
					},
					LoggingConfigMapName: "logging-config",
					LocalityLabels:       []string{"topology.kubernetes.io/region"},
					GRPCPort:             new(int32(26258)),
					HTTPPort:             new(int32(8080)),
					SQLPort:              new(int32(26257)),
					PodTemplate: &v1beta1.PodTemplateSpec{
						Metadata: v1beta1.PodMeta{
							Annotations: map[string]string{"crdb": "isCool"},
							Labels:      map[string]string{"app": "cockroachdb"},
						},
						Spec: corev1.PodSpec{
							ServiceAccountName: "crdb-test-sa",
							PriorityClassName:  "operator-critical",
							Containers: []corev1.Container{
								{
									Name:  "cockroachdb",
									Image: "cockroachdb/cockroach:v25.3.1",
									Resources: corev1.ResourceRequirements{
										Requests: corev1.ResourceList{
											corev1.ResourceCPU: resource.MustParse("200m"),
										},
									},
								},
							},
						},
					},
				},
			},
		},
	}

	clusterObj, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&cluster)
	require.NoError(t, err)
	gvr := schema.GroupVersionResource{
		Group:    "crdb.cockroachlabs.com",
		Version:  "v1beta1",
		Resource: "crdbclusters",
	}
	_, err = dynamicClient.Resource(gvr).Namespace(namespace).Create(ctx, &unstructured.Unstructured{Object: clusterObj}, metav1.CreateOptions{})
	require.NoError(t, err)

	ingressClassName := "nginx"
	_, err = clientset.NetworkingV1().Ingresses(namespace).Create(ctx, &networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "ui-cockroachdb",
			Namespace:   namespace,
			Annotations: map[string]string{"nginx.ingress.kubernetes.io/backend-protocol": "HTTPS"},
		},
		Spec: networkingv1.IngressSpec{
			IngressClassName: &ingressClassName,
			Rules: []networkingv1.IngressRule{
				{Host: "ui.local.com"},
			},
		},
	}, metav1.CreateOptions{})
	require.NoError(t, err)

	m := &Manifest{
		objectName:    cluster.Name,
		namespace:     namespace,
		outputDir:     outputDir,
		clientset:     clientset,
		dynamicClient: dynamicClient,
	}

	err = m.ExportValuesFromV1beta1()
	require.NoError(t, err)

	valuesBytes, err := os.ReadFile(filepath.Join(outputDir, "values.yaml"))
	require.NoError(t, err)

	var valuesData map[string]interface{}
	err = yaml.Unmarshal(valuesBytes, &valuesData)
	require.NoError(t, err)

	k8sValues, ok := valuesData["k8s"].(map[string]interface{})
	require.True(t, ok)
	assert.Equal(t, "cockroachdb", k8sValues["fullnameOverride"])

	cockroachdbValues, ok := valuesData["cockroachdb"].(map[string]interface{})
	require.True(t, ok)

	tlsValues, ok := cockroachdbValues["tls"].(map[string]interface{})
	require.True(t, ok)
	assert.Equal(t, true, tlsValues["enabled"])
	externalCertificates, ok := tlsValues["externalCertificates"].(map[string]interface{})
	require.True(t, ok)
	assert.Equal(t, true, externalCertificates["enabled"])

	crdbClusterValues, ok := cockroachdbValues["crdbCluster"].(map[string]interface{})
	require.True(t, ok)
	assert.Equal(t, "logging-config", crdbClusterValues["loggingConfigMapName"])
	postInitSQL, ok := crdbClusterValues["postInitSQL"].(map[string]interface{})
	require.True(t, ok, "postInitSQL should be present in generated values.yaml")
	assert.Equal(t, []interface{}{
		"CREATE DATABASE IF NOT EXISTS smoke",
		"CREATE USER IF NOT EXISTS app_user",
	}, postInitSQL["inline"])
	configMapRef, ok := postInitSQL["configMapRef"].(map[string]interface{})
	require.True(t, ok)
	assert.Equal(t, "bootstrap-sql", configMapRef["name"])
	assert.Equal(t, "init.sql", configMapRef["key"])
	secretRef, ok := postInitSQL["secretRef"].(map[string]interface{})
	require.True(t, ok)
	assert.Equal(t, "license-sql", secretRef["name"])
	assert.Equal(t, "license.sql", secretRef["key"])
	rbacValues, ok := crdbClusterValues["rbac"].(map[string]interface{})
	require.True(t, ok)
	serviceAccountValues, ok := rbacValues["serviceAccount"].(map[string]interface{})
	require.True(t, ok)
	assert.Equal(t, false, serviceAccountValues["create"])
	assert.Equal(t, "crdb-test-sa", serviceAccountValues["name"])

	podTemplateValues, ok := crdbClusterValues["podTemplate"].(map[string]interface{})
	require.True(t, ok)
	podTemplateMetadata, ok := podTemplateValues["metadata"].(map[string]interface{})
	require.True(t, ok)
	podTemplateAnnotations, ok := podTemplateMetadata["annotations"].(map[string]interface{})
	require.True(t, ok)
	assert.Equal(t, "isCool", podTemplateAnnotations["crdb"])
	_, templateScopeAnnotationExists := podTemplateAnnotations["template-scope"]
	assert.False(t, templateScopeAnnotationExists)

	walFailoverSpec, ok := crdbClusterValues["walFailoverSpec"].(map[string]interface{})
	require.True(t, ok)
	assert.Equal(t, "/cockroach/wal-failover", walFailoverSpec["path"])

	serviceValues, ok := crdbClusterValues["service"].(map[string]interface{})
	require.True(t, ok)
	ingressValues, ok := serviceValues["ingress"].(map[string]interface{})
	require.True(t, ok)
	assert.Equal(t, true, ingressValues["enabled"])
	uiValues, ok := ingressValues["ui"].(map[string]interface{})
	require.True(t, ok)
	assert.Equal(t, "ui.local.com", uiValues["host"])
}

func TestExportPublicServiceValues(t *testing.T) {
	for _, serviceName := range []string{"cockroachdb-public", "cockroachdb"} {
		t.Run(serviceName, func(t *testing.T) {
			const name, namespace = "cockroachdb", "default"
			cluster := v1beta1.CrdbCluster{
				TypeMeta:   metav1.TypeMeta{APIVersion: "crdb.cockroachlabs.com/v1beta1", Kind: "CrdbCluster"},
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
				Spec: v1beta1.CrdbClusterSpec{Template: v1beta1.CrdbNodeTemplate{Spec: v1beta1.CrdbNodeSpec{
					GRPCPort: new(int32(26257)), SQLPort: new(int32(26257)),
				}}},
			}
			obj, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&cluster)
			require.NoError(t, err)
			service := &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{Name: serviceName, Namespace: namespace,
					Labels:      map[string]string{"app.kubernetes.io/instance": "old-release", "monitoring": "enabled"},
					Annotations: map[string]string{"meta.helm.sh/release-name": "old-release", "example.com/load-balancer": "internal"}},
				Spec: corev1.ServiceSpec{Type: corev1.ServiceTypeLoadBalancer, Ports: []corev1.ServicePort{
					{Name: "database", Port: 26257, TargetPort: intstr.FromString("grpc")},
					{Name: "console", Port: 8080, TargetPort: intstr.FromString("http")},
				}},
			}
			m := &Manifest{namespace: namespace, objectName: name, outputDir: t.TempDir(),
				clientset:     fake.NewSimpleClientset(service),
				dynamicClient: dynamicfake.NewSimpleDynamicClient(runtime.NewScheme(), &unstructured.Unstructured{Object: obj})}
			require.NoError(t, m.ExportValuesFromV1beta1())
			valuesFile := filepath.Join(m.outputDir, "values.yaml")
			chart, err := filepath.Abs("../../cockroachdb-operator/charts/cockroachdb")
			require.NoError(t, err)
			rendered, err := exec.Command("helm", "template", "adopted", chart, "--namespace", namespace,
				"--values", valuesFile, "--show-only", "templates/service.public.yaml").CombinedOutput()
			require.NoError(t, err, "%s", rendered)
			var adopted corev1.Service
			require.NoError(t, yaml.Unmarshal(rendered, &adopted))
			require.Equal(t, serviceName, adopted.Name)
			require.Equal(t, corev1.ServiceTypeLoadBalancer, adopted.Spec.Type)
			require.Equal(t, "enabled", adopted.Labels["monitoring"])
			require.Equal(t, "adopted", adopted.Labels["app.kubernetes.io/instance"])
			require.Equal(t, map[string]string{"example.com/load-balancer": "internal"}, adopted.Annotations)
			require.Len(t, adopted.Spec.Ports, 2)
			require.Equal(t, service.Spec.Ports[0], adopted.Spec.Ports[0])
			require.Equal(t, service.Spec.Ports[1], adopted.Spec.Ports[1])
		})
	}
	t.Run("discovery Service is not adopted", func(t *testing.T) {
		m := &Manifest{namespace: "default", clientset: fake.NewSimpleClientset(&corev1.Service{
			ObjectMeta: metav1.ObjectMeta{Name: "cockroachdb", Namespace: "default"},
			Spec:       corev1.ServiceSpec{ClusterIP: corev1.ClusterIPNone},
		})}
		values := buildHelmValuesFromV1beta1(v1beta1.CrdbCluster{}, nil)
		require.NoError(t, m.preservePublicServiceValues(context.Background(), values, "cockroachdb"))
		service := values["cockroachdb"].(map[string]interface{})["crdbCluster"].(map[string]interface{})["service"].(map[string]interface{})
		require.NotContains(t, service, "public")
	})
}

func TestInsecureMigrationValues(t *testing.T) {
	const name, namespace = "cockroachdb", "default"
	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: appsv1.StatefulSetSpec{
			Replicas: new(int32(3)),
			Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{
				Name: "db", Image: "cockroachdb/cockroach:v26.3.5",
			}}}},
			VolumeClaimTemplates: []corev1.PersistentVolumeClaim{{ObjectMeta: metav1.ObjectMeta{Name: "datadir"}}},
		},
	}
	legacy := publicv1.CrdbCluster{ObjectMeta: sts.ObjectMeta, Spec: publicv1.CrdbClusterSpec{
		Nodes: 3, TLSEnabled: false, Image: &publicv1.PodImage{Name: sts.Spec.Template.Spec.Containers[0].Image},
	}}
	for _, source := range []string{"controller export", "manual Helm", "manual legacy operator"} {
		t.Run(source, func(t *testing.T) {
			m := &Manifest{namespace: namespace, objectName: name, outputDir: t.TempDir(), clientset: fake.NewSimpleClientset()}
			var values map[string]interface{}
			switch source {
			case "controller export":
				cluster := v1beta1.CrdbCluster{TypeMeta: metav1.TypeMeta{APIVersion: "crdb.cockroachlabs.com/v1beta1", Kind: "CrdbCluster"},
					ObjectMeta: sts.ObjectMeta, Spec: v1beta1.CrdbClusterSpec{TLSEnabled: false,
						Template: v1beta1.CrdbNodeTemplate{Spec: v1beta1.CrdbNodeSpec{Image: legacy.Spec.Image.Name}}}}
				obj, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&cluster)
				require.NoError(t, err)
				m.dynamicClient = dynamicfake.NewSimpleDynamicClient(runtime.NewScheme(), &unstructured.Unstructured{Object: obj})
				require.NoError(t, m.ExportValuesFromV1beta1())
				data, err := os.ReadFile(filepath.Join(m.outputDir, "values.yaml"))
				require.NoError(t, err)
				require.NoError(t, yaml.Unmarshal(data, &values))
			case "manual Helm":
				values = buildHelmValuesFromHelm(sts, "gcp", "us-east1", namespace,
					parsedMigrationInput{tlsEnabled: false, grpcPort: 26258, sqlPort: 26257, httpPort: 8080})
			case "manual legacy operator":
				values = buildHelmValuesFromOperator(legacy, sts, "gcp", "us-east1", namespace, &v1beta1.Flags{})
			}
			// Certificate restoration must leave TLS-off values unchanged.
			require.NoError(t, m.restoreSelfSignerValues(context.Background(), values, name, name+"-node-secret", name+"-client-secret"))
			tls := values["cockroachdb"].(map[string]interface{})["tls"].(map[string]interface{})
			require.Equal(t, false, tls["enabled"])
			require.Equal(t, false, tls["selfSigner"].(map[string]interface{})["enabled"])
			for _, mode := range []string{"certManager", "externalCertificates"} {
				if settings, ok := tls[mode].(map[string]interface{}); ok {
					require.Equal(t, false, settings["enabled"], mode)
				}
			}
			valuesFile := filepath.Join(m.outputDir, "values.yaml")
			require.NoError(t, yamlToDisk(valuesFile, []any{values}))
			chart, err := filepath.Abs("../../cockroachdb-operator/charts/cockroachdb")
			require.NoError(t, err)
			rendered, err := exec.Command("helm", "template", name, chart, "--values", valuesFile).CombinedOutput()
			require.NoError(t, err, "%s", rendered)
			var found bool
			for _, doc := range strings.Split(string(rendered), "\n---") {
				var obj map[string]interface{}
				require.NoError(t, yaml.Unmarshal([]byte(doc), &obj))
				switch obj["kind"] {
				case "CrdbCluster":
					found = true
					spec := obj["spec"].(map[string]interface{})
					require.Equal(t, false, spec["tlsEnabled"])
					require.NotContains(t, spec["template"].(map[string]interface{})["spec"], "certificates")
				case "Certificate", "Issuer", "CronJob":
					t.Fatalf("insecure values rendered certificate resource %s", obj["kind"])
				}
			}
			require.True(t, found)
		})
	}
}

func TestExportSelfSignerValues(t *testing.T) {
	const name, namespace = "cockroachdb", "default"
	// Build real pairs so incorrect keys, SANs or stale certificate metadata
	// cannot accidentally pass as a successful export.
	now := time.Now().UTC().Truncate(time.Second)
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	caCert := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test CA"},
		NotBefore: now, NotAfter: now.Add(43800 * time.Hour), IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature}
	makeSecret := func(secretName string, template *x509.Certificate, key *ecdsa.PrivateKey, ca bool) *corev1.Secret {
		t.Helper()
		der, err := x509.CreateCertificate(rand.Reader, template, caCert, &key.PublicKey, caKey)
		require.NoError(t, err)
		keyDER, err := x509.MarshalECPrivateKey(key)
		require.NoError(t, err)
		certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
		keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
		data := map[string][]byte{corev1.TLSCertKey: certPEM, corev1.TLSPrivateKeyKey: keyPEM}
		if ca {
			data = map[string][]byte{certresource.CaCert: certPEM, certresource.CaKey: keyPEM}
		}
		annotations := certresource.GetSecretAnnotations(template.NotBefore.Format(time.RFC3339), template.NotAfter.Format(time.RFC3339), template.NotAfter.Sub(template.NotBefore).String())
		annotations[certresource.SecretDataHash] = "existing-self-signer-hash"
		return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: namespace, Annotations: annotations}, Data: data}
	}
	ca := makeSecret(name+"-ca-secret", caCert, caKey, true)
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	leaf := &x509.Certificate{SerialNumber: big.NewInt(2), NotBefore: now, NotAfter: now.Add(8760 * time.Hour),
		DNSNames: []string{"sql.example.com", name + "-join.default.svc.cluster.local"}, IPAddresses: []net.IP{net.ParseIP("10.0.0.10")},
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth}}
	node := makeSecret(name+"-node-secret", leaf, leafKey, false)
	clientCert := *leaf
	clientCert.SerialNumber = big.NewInt(3)
	clientCert.NotAfter = now.Add(672 * time.Hour)
	client := makeSecret(name+"-client-secret", &clientCert, leafKey, false)
	for _, tc := range []struct {
		name           string
		mutate         func(*corev1.Secret, *corev1.Secret, *corev1.Secret)
		enabled        bool
		wantError      bool
		clientDuration string
		providedCA     bool
		rotationOff    bool
		otherRelease   bool
		signerDisabled bool
		uncompressed   bool
	}{
		{name: "legacy self-signer", enabled: true, clientDuration: "672h"},
		{name: "provided custom CA without signer annotations", enabled: true, clientDuration: "672h", providedCA: true,
			mutate: func(ca, node, client *corev1.Secret) { ca.Name, ca.Annotations = "supplied-ca", nil }},
		{name: "provided default CA without signer annotations", enabled: true, clientDuration: "672h", providedCA: true,
			mutate: func(ca, node, client *corev1.Secret) { ca.Annotations = nil }},
		{name: "provided CA with rotation disabled", enabled: true, clientDuration: "672h", providedCA: true, rotationOff: true,
			mutate: func(ca, node, client *corev1.Secret) { ca.Name, ca.Annotations = "supplied-ca", nil }},
		{name: "provided CA in an uncompressed Helm record", enabled: true, clientDuration: "672h", providedCA: true, uncompressed: true,
			mutate: func(ca, node, client *corev1.Secret) { ca.Name, ca.Annotations = "supplied-ca", nil }},
		{name: "disabled source signer does not enable rotation", providedCA: true, signerDisabled: true,
			mutate: func(ca, node, client *corev1.Secret) { ca.Annotations = nil }},
		{name: "provided CA without signing key fails", providedCA: true, wantError: true,
			mutate: func(ca, node, client *corev1.Secret) { ca.Annotations = nil; delete(ca.Data, certresource.CaKey) }},
		{name: "unrelated release does not establish CA ownership", providedCA: true, otherRelease: true,
			mutate: func(ca, node, client *corev1.Secret) { ca.Annotations = nil }},
		{name: "controller reissued client with stale annotations", enabled: true, clientDuration: "672h", mutate: func(ca, node, client *corev1.Secret) {
			client.Annotations[certresource.CertDuration] = "48h"
			client.Annotations[certresource.CertValidUpto] = now.Add(48 * time.Hour).Format(time.RFC3339)
		}},
		{name: "external CA with signing key", mutate: func(ca, node, client *corev1.Secret) { ca.Annotations = nil }},
		{name: "custom node Secret name", mutate: func(ca, node, client *corev1.Secret) { node.Name = "custom-node" }},
		{name: "custom client Secret name", mutate: func(ca, node, client *corev1.Secret) { client.Name = "custom-client" }},
		{name: "legacy operator controller-reissued certificates", mutate: func(ca, node, client *corev1.Secret) {
			// Controller migration creates standard leaf Secret names, but keeps
			// the legacy operator's CA name and does not add signer metadata.
			ca.Name = name + "-ca"
			ca.Annotations, node.Annotations, client.Annotations = nil, nil, nil
		}},
		{name: "legacy operator source certificates", mutate: func(ca, node, client *corev1.Secret) {
			ca.Name, node.Name, client.Name = name+"-ca", name+"-node", name+"-root"
			ca.Annotations, node.Annotations, client.Annotations = nil, nil, nil
		}},
		{name: "cert-manager Secret", mutate: func(ca, node, client *corev1.Secret) {
			node.Annotations["cert-manager.io/certificate-name"] = "custom-node"
		}},
		{name: "incomplete CA metadata", wantError: true, mutate: func(ca, node, client *corev1.Secret) { delete(ca.Annotations, certresource.SecretDataHash) }},
		{name: "mismatched node key", wantError: true, mutate: func(ca, node, client *corev1.Secret) {
			node.Data[corev1.TLSPrivateKeyKey] = ca.Data[certresource.CaKey]
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ca, node, client := ca.DeepCopy(), node.DeepCopy(), client.DeepCopy()
			if tc.mutate != nil {
				tc.mutate(ca, node, client)
			}
			cluster := v1beta1.CrdbCluster{
				TypeMeta:   metav1.TypeMeta{APIVersion: "crdb.cockroachlabs.com/v1beta1", Kind: "CrdbCluster"},
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
				Spec: v1beta1.CrdbClusterSpec{
					TLSEnabled: true,
					Mode:       new(v1beta1.MutableOnly),
					Regions:    []v1beta1.CrdbClusterRegion{{Code: "local", Nodes: 3, Namespace: namespace, Domain: "example.test"}},
					Template: v1beta1.CrdbNodeTemplate{Spec: v1beta1.CrdbNodeSpec{
						Image:      "cockroachdb/cockroach:v26.4.0",
						Join:       name + "-join.default.svc.example.test",
						Domain:     "obsolete.test",
						StartFlags: &v1beta1.Flags{Upsert: []string{"--join=seed.other.svc.example.test:26257"}},
						Certificates: v1beta1.Certificates{ExternalCertificates: &v1beta1.ExternalCertificates{
							CAConfigMapName: name + "-ca-crt", NodeSecretName: node.Name, RootSQLClientSecretName: client.Name,
						}},
					}},
				},
				Status: v1beta1.CrdbClusterStatus{Region: "local"},
			}
			obj, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&cluster)
			require.NoError(t, err)
			m := &Manifest{namespace: namespace, objectName: name, outputDir: t.TempDir(), clientset: fake.NewSimpleClientset(ca, node, client), dynamicClient: dynamicfake.NewSimpleDynamicClient(runtime.NewScheme(), &unstructured.Unstructured{Object: obj})}
			if tc.providedCA {
				stsName := name
				if tc.otherRelease {
					stsName = "unrelated-cluster"
				}
				release, err := json.Marshal(map[string]interface{}{
					"namespace": namespace,
					"manifest":  "apiVersion: apps/v1\nkind: StatefulSet\nmetadata:\n  name: " + stsName + "\n",
					"config": map[string]interface{}{"tls": map[string]interface{}{"certs": map[string]interface{}{
						"selfSigner": map[string]interface{}{"enabled": !tc.signerDisabled, "caProvided": true, "caSecret": ca.Name, "rotateCerts": !tc.rotationOff},
					}}},
				})
				require.NoError(t, err)
				var compressed bytes.Buffer
				writer := gzip.NewWriter(&compressed)
				_, err = writer.Write(release)
				require.NoError(t, err)
				require.NoError(t, writer.Close())
				storedRelease := compressed.Bytes()
				if tc.uncompressed {
					storedRelease = release
				}
				_, err = m.clientset.CoreV1().Secrets(namespace).Create(context.Background(), &corev1.Secret{
					ObjectMeta: metav1.ObjectMeta{Name: "sh.helm.release.v1.source.v1", Namespace: namespace,
						Labels: map[string]string{"owner": "helm", "status": "deployed"}},
					Type: "helm.sh/release.v1", Data: map[string][]byte{"release": []byte(base64.StdEncoding.EncodeToString(storedRelease))},
				}, metav1.CreateOptions{})
				require.NoError(t, err)
			}
			err = m.ExportValuesFromV1beta1()
			if tc.wantError {
				require.Error(t, err)
				if tc.name == "incomplete CA metadata" {
					require.ErrorContains(t, err, "self-signer CA is missing secret-data-hash; repair its metadata before exporting values")
				}
				_, statErr := os.Stat(filepath.Join(m.outputDir, "values.yaml"))
				require.True(t, os.IsNotExist(statErr))
				return
			}
			require.NoError(t, err)
			bytes, err := os.ReadFile(filepath.Join(m.outputDir, "values.yaml"))
			require.NoError(t, err)
			var values map[string]interface{}
			require.NoError(t, yaml.Unmarshal(bytes, &values))
			crdb := values["cockroachdb"].(map[string]interface{})
			require.Equal(t, "example.test", crdb["clusterDomain"])
			require.NotContains(t, string(bytes), "PRIVATE KEY")
			tls := crdb["tls"].(map[string]interface{})
			signer := tls["selfSigner"].(map[string]interface{})
			require.Equal(t, tc.enabled, signer["enabled"])
			if !tc.enabled {
				require.NotEqual(t, true, signer["rotateCerts"])
				external := tls["externalCertificates"].(map[string]interface{})
				require.Equal(t, true, external["enabled"])
				references := external["certificates"].(map[string]interface{})
				require.Equal(t, name+"-ca-crt", references["caConfigMapName"])
				require.Equal(t, node.Name, references["nodeSecretName"])
				require.Equal(t, client.Name, references["rootSqlClientSecretName"])
			} else {
				require.Equal(t, !tc.rotationOff, signer["rotateCerts"])
				require.Equal(t, tc.providedCA, signer["caProvided"])
				if tc.providedCA {
					require.Equal(t, ca.Name, signer["caSecret"])
				}
				require.Equal(t, tc.clientDuration, signer["clientCertDuration"])
				require.ElementsMatch(t, []interface{}{"sql.example.com", name + "-join.default.svc.cluster.local", "10.0.0.10"}, signer["additionalSANs"])
			}
			if _, err := exec.LookPath("helm"); err != nil {
				t.Log("Helm unavailable; chart round-trip unverified")
				return
			}
			chart, err := filepath.Abs("../../cockroachdb-operator/charts/cockroachdb")
			require.NoError(t, err)
			args := []string{"template", name, chart, "--namespace", namespace, "--values", filepath.Join(m.outputDir, "values.yaml")}
			if tc.providedCA && tc.enabled {
				// Provided-CA validation uses lookup. Let Helm query a small API
				// server instead of bypassing the chart's real Secret check.
				resources := map[string][]metav1.APIResource{
					"v1": {{Name: "secrets", Kind: "Secret", Namespaced: true}, {Name: "configmaps", Kind: "ConfigMap", Namespaced: true},
						{Name: "services", Kind: "Service", Namespaced: true}, {Name: "serviceaccounts", Kind: "ServiceAccount", Namespaced: true}},
					"rbac.authorization.k8s.io/v1": {{Name: "roles", Kind: "Role", Namespaced: true}, {Name: "rolebindings", Kind: "RoleBinding", Namespaced: true},
						{Name: "clusterroles", Kind: "ClusterRole"}, {Name: "clusterrolebindings", Kind: "ClusterRoleBinding"}},
					"batch/v1":                       {{Name: "jobs", Kind: "Job", Namespaced: true}, {Name: "cronjobs", Kind: "CronJob", Namespaced: true}},
					"policy/v1":                      {{Name: "poddisruptionbudgets", Kind: "PodDisruptionBudget", Namespaced: true}},
					"crdb.cockroachlabs.com/v1beta1": {{Name: "crdbclusters", Kind: "CrdbCluster", Namespaced: true}},
				}
				groups := metav1.APIGroupList{}
				for version := range resources {
					if version == "v1" {
						continue
					}
					gv := schema.FromAPIVersionAndKind(version, "").GroupVersion()
					v := metav1.GroupVersionForDiscovery{GroupVersion: version, Version: gv.Version}
					groups.Groups = append(groups.Groups, metav1.APIGroup{Name: gv.Group, Versions: []metav1.GroupVersionForDiscovery{v}, PreferredVersion: v})
				}
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					assert.Equal(t, http.MethodGet, r.Method)
					var obj interface{}
					switch r.URL.Path {
					case "/version":
						obj = map[string]string{"major": "1", "minor": "34", "gitVersion": "v1.34.0"}
					case "/api":
						obj = metav1.APIVersions{Versions: []string{"v1"}}
					case "/apis":
						obj = groups
					case "/api/v1":
						obj = metav1.APIResourceList{GroupVersion: "v1", APIResources: resources["v1"]}
					case "/api/v1/namespaces/" + namespace + "/secrets/" + ca.Name:
						obj = ca.DeepCopy()
						obj.(*corev1.Secret).TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"}
					default:
						version := strings.TrimPrefix(r.URL.Path, "/apis/")
						if list, ok := resources[version]; ok {
							obj = metav1.APIResourceList{GroupVersion: version, APIResources: list}
						} else {
							w.WriteHeader(http.StatusNotFound)
							obj = metav1.Status{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Status"}, Reason: metav1.StatusReasonNotFound, Code: 404}
						}
					}
					if err := json.NewEncoder(w).Encode(obj); err != nil {
						t.Errorf("encoding API response: %v", err)
					}
				}))
				defer server.Close()
				configFile := filepath.Join(m.outputDir, "kubeconfig")
				config := "apiVersion: v1\nkind: Config\nclusters:\n- name: test\n  cluster:\n    server: " + server.URL + "\ncontexts:\n- name: test\n  context:\n    cluster: test\ncurrent-context: test\n"
				require.NoError(t, os.WriteFile(configFile, []byte(config), 0600))
				args = append(args, "--dry-run=server", "--disable-openapi-validation", "--kubeconfig", configFile)
			}
			rendered, err := exec.Command("helm", args...).CombinedOutput()
			require.NoError(t, err, "%s", rendered)
			objects := strings.Split(string(rendered), "\n---")
			cronjobs := 0
			for _, object := range objects {
				var res map[string]interface{}
				require.NoError(t, yaml.Unmarshal([]byte(object), &res))
				if res["kind"] == "CronJob" {
					cronjobs++
					if tc.providedCA {
						require.NotContains(t, object, "--rotate-ca")
						require.Contains(t, object, "--ca-secret="+ca.Name)
					}
					if strings.Contains(object, "--rotate-node") {
						require.Contains(t, object, "sql.example.com")
						require.Contains(t, object, "10.0.0.10")
					}
				}
				if res["kind"] == "Job" && tc.providedCA && tc.enabled && strings.Contains(object, "- generate") {
					require.Contains(t, object, "--ca-secret="+ca.Name)
				}
				if res["kind"] == "CrdbCluster" {
					spec := res["spec"].(map[string]interface{})["template"].(map[string]interface{})["spec"].(map[string]interface{})
					require.NotContains(t, spec, "join")
					require.NotContains(t, spec, "domain")
					flags := spec["startFlags"].(map[string]interface{})
					require.Equal(t, []interface{}{"--join=seed.other.svc.example.test:26257"}, flags["upsert"])
					regions := res["spec"].(map[string]interface{})["regions"].([]interface{})
					require.Equal(t, "example.test", regions[0].(map[string]interface{})["domain"])
					if !tc.enabled {
						references := spec["certificates"].(map[string]interface{})["externalCertificates"].(map[string]interface{})
						require.Equal(t, node.Name, references["nodeSecretName"])
						require.Equal(t, client.Name, references["rootSqlClientSecretName"])
					}
				}
			}
			if tc.enabled && !tc.rotationOff {
				wantJobs := 2
				if tc.providedCA {
					wantJobs = 1 // Only the combined node/client job; the supplied CA is not rotated.
				}
				require.Equal(t, wantJobs, cronjobs)
			} else {
				require.Zero(t, cronjobs, "external sources must not gain chart rotation jobs")
			}
		})
	}
}

// validateGoldenFile compares the generated file with the golden file
func validateGoldenFile(t *testing.T, generatedFile, goldenFile string) {
	generated, err := os.ReadFile(generatedFile)
	require.NoError(t, err)

	golden, err := os.ReadFile(goldenFile)
	require.NoError(t, err)

	if *updateGolden {
		err = os.WriteFile(goldenFile, generated, 0644)
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

		var nodeData v1beta1.CrdbNode
		err = yaml.Unmarshal(nodeBytes, &nodeData)
		require.NoError(t, err)

		require.NotNil(t, nodeData.Spec.WALFailoverSpec, "WAL failover spec should be present in node %d", i)
		assert.Equal(t, v1beta1.CrdbWalFailoverStatus("enable"), nodeData.Spec.WALFailoverSpec.Status)
		assert.Equal(t, "/custom/wal", nodeData.Spec.WALFailoverSpec.Path)
		assert.Equal(t, "failoverdir", nodeData.Spec.WALFailoverSpec.Name)
		assert.Equal(t, "50Gi", nodeData.Spec.WALFailoverSpec.Size)
		assert.Equal(t, "fast-ssd", nodeData.Spec.WALFailoverSpec.StorageClassName)
	}
}

func TestFromHelmChart_WithDedicatedLogsPVC(t *testing.T) {
	clientset := fake.NewSimpleClientset()
	dynamicClient := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme())
	ctx := context.TODO()
	namespace := "default"
	outputDir := t.TempDir()

	storageClassName := "standard"
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
								"--join=cockroachdb-0.cockroachdb:26257",
							},
							VolumeMounts: []corev1.VolumeMount{
								{Name: "datadir", MountPath: "/cockroach/cockroach-data"},
								{Name: "logsdir", MountPath: "/cockroach/logs"},
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
					ObjectMeta: metav1.ObjectMeta{Name: "logsdir"},
					Spec: corev1.PersistentVolumeClaimSpec{
						StorageClassName: &storageClassName,
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

	// Create pods
	for i := 0; i < 3; i++ {
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      sts.Name + "-" + strconv.Itoa(i),
				Namespace: namespace,
			},
			Spec: corev1.PodSpec{
				NodeName:   "node" + strconv.Itoa(i),
				Containers: sts.Spec.Template.Spec.Containers,
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

	m := &Manifest{
		cloudProvider: "gcp",
		cloudRegion:   "us-central1",
		objectName:    sts.Name,
		namespace:     sts.Namespace,
		outputDir:     outputDir,
		clientset:     clientset,
		dynamicClient: dynamicClient,
	}

	err = m.FromHelmChart()
	require.NoError(t, err)

	// Verify values.yaml contains log.logsStore
	valuesBytes, err := os.ReadFile(filepath.Join(outputDir, "values.yaml"))
	require.NoError(t, err)

	var valuesData map[string]interface{}
	err = yaml.Unmarshal(valuesBytes, &valuesData)
	require.NoError(t, err)

	cockroachdb, ok := valuesData["cockroachdb"].(map[string]interface{})
	require.True(t, ok)
	crdbCluster, ok := cockroachdb["crdbCluster"].(map[string]interface{})
	require.True(t, ok)
	log, ok := crdbCluster["log"].(map[string]interface{})
	require.True(t, ok, "log section should be present in values.yaml")
	logsStore, ok := log["logsStore"].(map[string]interface{})
	require.True(t, ok, "log.logsStore should be present in values.yaml")

	assert.Equal(t, "/cockroach/logs", logsStore["mountPath"])
	vct, ok := logsStore["volumeClaimTemplate"].(map[string]interface{})
	require.True(t, ok, "volumeClaimTemplate should be present in logsStore")
	spec, ok := vct["spec"].(map[string]interface{})
	require.True(t, ok, "spec should be present in volumeClaimTemplate")
	resources, ok := spec["resources"].(map[string]interface{})
	require.True(t, ok, "resources should be present in spec")
	requests, ok := resources["requests"].(map[string]interface{})
	require.True(t, ok, "requests should be present in resources")
	assert.Equal(t, "50Gi", requests["storage"])
	assert.Equal(t, "standard", spec["storageClassName"])

	// Verify each CrdbNode has LogsStore set correctly
	for i := 0; i < 3; i++ {
		nodeBytes, err := os.ReadFile(filepath.Join(outputDir, "crdbnode-"+strconv.Itoa(i)+".yaml"))
		require.NoError(t, err)

		var nodeData v1beta1.CrdbNode
		err = yaml.Unmarshal(nodeBytes, &nodeData)
		require.NoError(t, err)

		require.NotNil(t, nodeData.Spec.LogsStore, "LogsStore should be present in node %d", i)
		assert.Equal(t, "/cockroach/logs", nodeData.Spec.LogsStore.MountPath)
		require.NotNil(t, nodeData.Spec.LogsStore.VolumeClaimTemplate)
		assert.Equal(t, "logsdir", nodeData.Spec.LogsStore.VolumeClaimTemplate.Name)
		storage := nodeData.Spec.LogsStore.VolumeClaimTemplate.Spec.Resources.Requests[corev1.ResourceStorage]
		assert.Equal(t, "50Gi", storage.String())
		require.NotNil(t, nodeData.Spec.LogsStore.VolumeClaimTemplate.Spec.StorageClassName)
		assert.Equal(t, "standard", *nodeData.Spec.LogsStore.VolumeClaimTemplate.Spec.StorageClassName)
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

	// When WAL failover is disabled, walFailoverSpec should not be present at all
	_, walFailoverExists := crdbCluster["walFailoverSpec"]
	require.False(t, walFailoverExists, "walFailoverSpec should not be present when WAL failover is disabled")

	// Verify that each CrdbNode does NOT contain walFailoverSpec when disabled
	for i := 0; i < 2; i++ {
		nodeBytes, err := os.ReadFile(filepath.Join(outputDir, "crdbnode-"+strconv.Itoa(i)+".yaml"))
		require.NoError(t, err)

		var nodeData v1beta1.CrdbNode
		err = yaml.Unmarshal(nodeBytes, &nodeData)
		require.NoError(t, err)

		require.Nil(t, nodeData.Spec.WALFailoverSpec, "WAL failover spec should not be present in node %d when disabled", i)
	}
}

// A namespace can contain unrelated charts with incompatible values or old,
// unreadable release records. Only the source StatefulSet establishes CA intent.
func TestSourceProvidedCAReleaseSelection(t *testing.T) {
	const namespace, name = "default", "source"
	provided := `{"tls":{"certs":{"selfSigner":{"caProvided":true,"caSecret":"supplied-ca"}}}}`
	for _, tc := range []struct {
		name, otherConfig, otherManifest, sourceConfig string
		unreadable, duplicate                          bool
		wantError                                      string
	}{
		{name: "boolean TLS on another chart", otherConfig: `{"tls":true}`},
		{name: "list certs on another chart", otherConfig: `{"tls":{"certs":[]}}`},
		{name: "unreadable release record", unreadable: true},
		{name: "invalid unrelated manifest", otherConfig: `{}`, otherManifest: "metadata: ["},
		{name: "same name in another namespace", otherConfig: `{"tls":true}`,
			otherManifest: "kind: StatefulSet\nmetadata:\n  name: source\n  namespace: another\n"},
		{name: "incompatible source settings", sourceConfig: `{"tls":true}`, wantError: "cannot read source Helm release settings"},
		{name: "missing source CA Secret", sourceConfig: `{"tls":{"certs":{"selfSigner":{"caProvided":true}}}}`, wantError: "without caSecret"},
		{name: "ambiguous source", duplicate: true, wantError: "multiple deployed Helm releases"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := fake.NewSimpleClientset()
			addRelease := func(secretName, manifest, config string, unreadable bool) {
				data, err := json.Marshal(map[string]interface{}{
					"namespace": namespace, "manifest": manifest, "config": json.RawMessage(config),
				})
				require.NoError(t, err)
				encoded := base64.StdEncoding.EncodeToString(data)
				if unreadable {
					encoded = "not base64!"
				}
				_, err = client.CoreV1().Secrets(namespace).Create(context.Background(), &corev1.Secret{
					ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: namespace,
						Labels: map[string]string{"owner": "helm", "status": "deployed"}},
					Type: "helm.sh/release.v1", Data: map[string][]byte{"release": []byte(encoded)},
				}, metav1.CreateOptions{})
				require.NoError(t, err)
			}
			config := tc.otherConfig
			if config == "" {
				config = `{}`
			}
			manifest := tc.otherManifest
			if manifest == "" {
				manifest = "kind: StatefulSet\nmetadata:\n  name: unrelated\n"
			}
			addRelease("a-unrelated", manifest, config, tc.unreadable)
			config = tc.sourceConfig
			if config == "" {
				config = provided
			}
			manifest = "kind: StatefulSet\nmetadata:\n  name: source\n"
			addRelease("b-source", manifest, config, false)
			if tc.duplicate {
				addRelease("c-duplicate", manifest, provided, false)
			}
			m := &Manifest{namespace: namespace, clientset: client}
			settings, err := m.sourceProvidedCA(context.Background(), name)
			if tc.wantError != "" {
				require.ErrorContains(t, err, tc.wantError)
				return
			}
			require.NoError(t, err)
			require.Equal(t, &providedCASettings{secretName: "supplied-ca", rotateCerts: true}, settings)
		})
	}
}
