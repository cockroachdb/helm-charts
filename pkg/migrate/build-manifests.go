package migrate

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"

	publicv1 "github.com/cockroachdb/cockroach-operator/apis/v1alpha1"
	"github.com/cockroachdb/errors"
	"github.com/cockroachdb/helm-charts/pkg/upstream/cockroach-operator/api/v1alpha1"
)

type Manifest struct {
	cloudProvider string
	cloudRegion   string
	objectName    string
	namespace     string
	kubeconfig    string
	outputDir     string
}

// NewManifest constructs a Manifest with required fields and functional options
func NewManifest(cloudProvider, cloudRegion, kubeconfig, objectName, namespace, outputDir string) (*Manifest, error) {
	// Ensure required fields are set
	if cloudProvider == "" || cloudRegion == "" {
		return nil, errors.New("cloudProvider and cloudRegion are required")
	}

	return &Manifest{
		cloudProvider: cloudProvider,
		cloudRegion:   cloudRegion,
		kubeconfig:    kubeconfig,
		namespace:     namespace,
		objectName:    objectName,
		outputDir:     outputDir,
	}, nil
}

func (m *Manifest) FromPublicOperator() error {
	var crdbCluster string
	ctx := context.TODO()

	config, err := clientcmd.BuildConfigFromFlags("", m.kubeconfig)
	if err != nil {
		return errors.Wrap(err, "building k8s config")
	}
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return errors.Wrap(err, "building k8s clientset")
	}
	dynamicClient, err := dynamic.NewForConfig(config)
	if err != nil {
		return errors.Wrap(err, "building k8s dynamic client")
	}

	publicCluster := publicv1.CrdbCluster{}
	gvr := schema.GroupVersionResource{
		Group:    "crdb.cockroachlabs.com",
		Version:  "v1alpha1",
		Resource: "crdbclusters",
	}
	cr, err := dynamicClient.Resource(gvr).Namespace(m.namespace).Get(ctx, m.objectName, metav1.GetOptions{})
	if err != nil {
		return errors.Wrap(err, "fetching public crdbcluster objectName")
	}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(cr.Object, &publicCluster); err != nil {
		return errors.Wrap(err, "unmarshalling public crdbcluster objectName")
	}

	crdbCluster = publicCluster.Name
	sts, err := clientset.AppsV1().StatefulSets(m.namespace).Get(ctx, crdbCluster, metav1.GetOptions{})
	if err != nil {
		return errors.Wrap(err, "fetching statefulset")
	}

	grpcPort := publicCluster.Spec.GRPCPort
	joinAddrs := []string{}
	for nodeIdx := int32(0); nodeIdx < publicCluster.Spec.Nodes; nodeIdx++ {
		joinAddrs = append(joinAddrs, fmt.Sprintf("%s-%d.%s.%s:%d", crdbCluster, nodeIdx, crdbCluster, m.namespace, grpcPort))
	}
	joinString := strings.Join(joinAddrs, ",")

	flags := map[string]string{}
	if publicCluster.Spec.Cache != "" {
		flags["--cache"] = publicCluster.Spec.Cache
	}
	if publicCluster.Spec.MaxSQLMemory != "" {
		flags["--max-sql-memory"] = publicCluster.Spec.MaxSQLMemory
	}
	if publicCluster.Spec.LogConfigMap != "" {
		if err := moveConfigMapKey(ctx, clientset, m.namespace, publicCluster.Spec.LogConfigMap); err != nil {
			return errors.Wrap(err, "moving config map key")
		}
	}
	for nodeIdx := int32(0); nodeIdx < publicCluster.Spec.Nodes; nodeIdx++ {
		podName := fmt.Sprintf("%s-%d", crdbCluster, nodeIdx)
		pod, err := clientset.CoreV1().Pods(m.namespace).Get(ctx, podName, metav1.GetOptions{})
		if err != nil {
			return errors.Newf("couldn't find crdb pod %s", podName)
		}

		if pod.Spec.NodeName == "" {
			return errors.Newf("pod %s isn't scheduled to a node", podName)
		}

		nodeSpec := buildNodeSpecFromOperator(publicCluster, sts, pod.Spec.NodeName, joinString, flags)
		crdbNode := v1alpha1.CrdbNode{
			TypeMeta: metav1.TypeMeta{
				Kind:       "CrdbNode",
				APIVersion: "crdb.cockroachlabs.com/v1alpha1",
			},
			ObjectMeta: metav1.ObjectMeta{
				Name:         fmt.Sprintf("%s-%d", crdbCluster, nodeIdx),
				Namespace:    m.namespace,
				GenerateName: "",
				Labels: map[string]string{
					"app":                            "cockroachdb",
					"svc":                            "cockroachdb",
					"crdb.cockroachlabs.com/cluster": crdbCluster,
				},
				Annotations: map[string]string{
					"crdb.cockroachlabs.com/cloudProvider": m.cloudProvider,
				},
				Finalizers: []string{"crdbnode.crdb.cockroachlabs.com/finalizer"},
			},
			Spec: nodeSpec,
		}
		if err := yamlToDisk(filepath.Join(m.outputDir, fmt.Sprintf("crdbnode-%d.yaml", nodeIdx)), []any{crdbNode}); err != nil {
			return errors.Wrap(err, "writing crdbnode manifest to disk")
		}
	}

	helmValues := buildHelmValuesFromOperator(publicCluster, m.cloudProvider, m.cloudRegion, m.namespace, joinString, flags)

	if err := yamlToDisk(filepath.Join(m.outputDir, "values.yaml"), []any{helmValues}); err != nil {
		return errors.Wrap(err, "writing helm values to disk")
	}

	if err := buildRBACFromPublicOperator(publicCluster, m.outputDir); err != nil {
		return errors.Wrap(err, "building rbac from public operator")
	}

	return nil
}

func (m *Manifest) FromHelmChart() error {
	ctx := context.TODO()
	config, err := clientcmd.BuildConfigFromFlags("", m.kubeconfig)
	if err != nil {
		return errors.Wrap(err, "building k8s config")
	}
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return errors.Wrap(err, "building k8s clientset")
	}

	sts, err := clientset.AppsV1().StatefulSets(m.namespace).Get(ctx, m.objectName, metav1.GetOptions{})
	if err != nil {
		return errors.Wrap(err, "fetching statefulset")
	}

	input, err := generateParsedMigrationInput(ctx, clientset, sts)
	if err != nil {
		return err
	}

	if err := generateUpdatedPublicServiceConfig(ctx, clientset, sts.Namespace, fmt.Sprintf("%s-public", sts.Name), m.outputDir); err != nil {
		return err
	}

	for nodeIdx := int32(0); nodeIdx < *sts.Spec.Replicas; nodeIdx++ {
		podName := fmt.Sprintf("%s-%d", sts.Name, nodeIdx)
		pod, err := clientset.CoreV1().Pods(m.namespace).Get(ctx, podName, metav1.GetOptions{})
		if err != nil {
			return errors.Newf("couldn't find crdb pod %s", podName)
		}

		if pod.Spec.NodeName == "" {
			return errors.Newf("pod %s isn't scheduled to a node", podName)
		}

		nodeSpec := buildNodeSpecFromHelm(sts, pod.Spec.NodeName, input)
		crdbNode := v1alpha1.CrdbNode{
			TypeMeta: metav1.TypeMeta{
				Kind:       "CrdbNode",
				APIVersion: "crdb.cockroachlabs.com/v1alpha1",
			},
			ObjectMeta: metav1.ObjectMeta{
				Name:         fmt.Sprintf("%s-%d", sts.Name, nodeIdx),
				Namespace:    m.namespace,
				GenerateName: "",
				Labels: map[string]string{
					"app":                            "cockroachdb",
					"svc":                            "cockroachdb",
					"crdb.cockroachlabs.com/cluster": sts.Name,
				},
				Annotations: map[string]string{
					"crdb.cockroachlabs.com/cloudProvider": m.cloudProvider,
				},
				Finalizers: []string{"crdbnode.crdb.cockroachlabs.com/finalizer"},
			},
			Spec: nodeSpec,
		}
		if err := yamlToDisk(filepath.Join(m.outputDir, fmt.Sprintf("crdbnode-%d.yaml", nodeIdx)), []any{crdbNode}); err != nil {
			return errors.Wrap(err, "writing crdbnode manifest to disk")
		}
	}

	helmValues := buildHelmValuesFromHelm(sts, m.cloudProvider, m.cloudRegion, m.namespace, input)

	if err := yamlToDisk(filepath.Join(m.outputDir, "values.yaml"), []any{helmValues}); err != nil {
		return errors.Wrap(err, "writing helm values to disk")
	}

	return nil
}
