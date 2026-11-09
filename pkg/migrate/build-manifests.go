package migrate

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	publicv1 "github.com/cockroachdb/cockroach-operator/apis/v1alpha1"
	"github.com/cockroachdb/errors"
	"github.com/cockroachdb/helm-charts/pkg/upstream/cockroach-operator/api/v1beta1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

type Manifest struct {
	cloudProvider string
	cloudRegion   string
	objectName    string
	namespace     string
	outputDir     string
	clientset     kubernetes.Interface
	dynamicClient dynamic.Interface
}

// NewManifest constructs a Manifest with required fields and functional options
func NewManifest(
	cloudProvider, cloudRegion, kubeconfig, objectName, namespace, outputDir string,
) (*Manifest, error) {
	// Ensure required fields are set
	if cloudProvider == "" || cloudRegion == "" {
		return nil, errors.New("cloudProvider and cloudRegion are required")
	}

	config, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		return nil, errors.Wrap(err, "building k8s config")
	}
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, errors.Wrap(err, "building k8s clientset")
	}
	dynamicClient, err := dynamic.NewForConfig(config)
	if err != nil {
		return nil, errors.Wrap(err, "building k8s dynamic client")
	}

	return &Manifest{
		cloudProvider: cloudProvider,
		cloudRegion:   cloudRegion,
		namespace:     namespace,
		objectName:    objectName,
		outputDir:     outputDir,
		clientset:     clientset,
		dynamicClient: dynamicClient,
	}, nil
}

func NewValuesExporter(kubeconfig, objectName, namespace, outputDir string) (*Manifest, error) {
	config, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		return nil, errors.Wrap(err, "building k8s config")
	}
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, errors.Wrap(err, "building k8s clientset")
	}
	dynamicClient, err := dynamic.NewForConfig(config)
	if err != nil {
		return nil, errors.Wrap(err, "building k8s dynamic client")
	}

	return &Manifest{
		namespace:     namespace,
		objectName:    objectName,
		outputDir:     outputDir,
		clientset:     clientset,
		dynamicClient: dynamicClient,
	}, nil
}

func (m *Manifest) FromPublicOperator() error {
	var crdbCluster string
	ctx := context.TODO()

	publicCluster := publicv1.CrdbCluster{}
	gvr := schema.GroupVersionResource{
		Group:    "crdb.cockroachlabs.com",
		Version:  "v1alpha1",
		Resource: "crdbclusters",
	}
	cr, err := m.dynamicClient.Resource(gvr).Namespace(m.namespace).Get(ctx, m.objectName, metav1.GetOptions{})
	if err != nil {
		return errors.Wrap(err, "fetching public crdbcluster objectName")
	}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(cr.Object, &publicCluster); err != nil {
		return errors.Wrap(err, "unmarshalling public crdbcluster objectName")
	}

	crdbCluster = publicCluster.Name
	sts, err := m.clientset.AppsV1().StatefulSets(m.namespace).Get(ctx, crdbCluster, metav1.GetOptions{})
	if err != nil {
		return errors.Wrap(err, "fetching statefulset")
	}
	if err = validateStatefulSetForPublicOperatorMigration(sts); err != nil {
		return errors.Wrap(err, "validating public operator statefulset")
	}

	if publicCluster.Spec.LogConfigMap != "" {
		if err := moveConfigMapKey(ctx, m.clientset, m.namespace, publicCluster.Spec.LogConfigMap); err != nil {
			return errors.Wrap(err, "moving config map key")
		}
	}
	input := parsedMigrationInput{tlsEnabled: publicCluster.Spec.TLSEnabled}
	if err := extractJoinStringAndFlags(&input, strings.Fields(sts.Spec.Template.Spec.Containers[0].Command[2])); err != nil {
		return errors.Wrap(err, "extracting join string and flags")
	}

	for nodeIdx := int32(0); nodeIdx < publicCluster.Spec.Nodes; nodeIdx++ {
		podName := fmt.Sprintf("%s-%d", crdbCluster, nodeIdx)
		pod, err := m.clientset.CoreV1().Pods(m.namespace).Get(ctx, podName, metav1.GetOptions{})
		if err != nil {
			return errors.Newf("couldn't find crdb pod %s", podName)
		}

		if pod.Spec.NodeName == "" {
			return errors.Newf("pod %s isn't scheduled to a node", podName)
		}

		nodeSpec := buildNodeSpecFromOperator(publicCluster, sts, pod.Spec.NodeName, input.startFlags)
		crdbNode := v1beta1.CrdbNode{
			TypeMeta: metav1.TypeMeta{
				Kind:       "CrdbNode",
				APIVersion: "crdb.cockroachlabs.com/v1beta1",
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

	helmValues := buildHelmValuesFromOperator(publicCluster, sts, m.cloudProvider, m.cloudRegion, m.namespace, input.startFlags)
	certs := operatorExternalCertificates(publicCluster, sts)
	if err := m.restoreSelfSignerValues(ctx, helmValues, publicCluster.Name, certs.NodeSecretName, certs.RootSQLClientSecretName); err != nil {
		return err
	}

	if err := yamlToDisk(filepath.Join(m.outputDir, "values.yaml"), []any{helmValues}); err != nil {
		return errors.Wrap(err, "writing helm values to disk")
	}

	if err := buildRBACFromPublicOperator(publicCluster, sts, m.outputDir); err != nil {
		return errors.Wrap(err, "building rbac from public operator")
	}

	return nil
}

// ExportValuesFromV1beta1 writes a values.yaml from the live migrated v1beta1
// resources so Helm adoption can start from the current cluster state.
func (m *Manifest) ExportValuesFromV1beta1() error {
	ctx := context.TODO()

	cluster := v1beta1.CrdbCluster{}
	gvr := schema.GroupVersionResource{
		Group:    "crdb.cockroachlabs.com",
		Version:  "v1beta1",
		Resource: "crdbclusters",
	}
	cr, err := m.dynamicClient.Resource(gvr).Namespace(m.namespace).Get(ctx, m.objectName, metav1.GetOptions{})
	if err != nil {
		return errors.Wrap(err, "fetching migrated v1beta1 crdbcluster")
	}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(cr.Object, &cluster); err != nil {
		return errors.Wrap(err, "unmarshalling migrated v1beta1 crdbcluster")
	}

	ingresses, err := m.loadIngressesForCluster(ctx, cluster.Name)
	if err != nil {
		return errors.Wrap(err, "loading ingress resources")
	}

	if err := os.MkdirAll(m.outputDir, 0755); err != nil {
		return errors.Wrap(err, "creating output directory")
	}

	values := buildHelmValuesFromV1beta1(cluster, ingresses)
	if err := m.preservePublicServiceValues(ctx, values, cluster.Name); err != nil {
		return err
	}
	if certs := cluster.Spec.Template.Spec.Certificates.ExternalCertificates; certs != nil && (certs.HTTPSecretName == "" || certs.HTTPSecretName == certs.RootSQLClientSecretName) {
		if err := m.restoreSelfSignerValues(ctx, values, cluster.Name, certs.NodeSecretName, certs.RootSQLClientSecretName); err != nil {
			return err
		}
	}
	if err := yamlToDisk(filepath.Join(m.outputDir, "values.yaml"), []any{values}); err != nil {
		return errors.Wrap(err, "writing helm values to disk")
	}
	return nil
}

// preservePublicServiceValues carries chart-supported client Service settings
// into adoption. Try <name>-public first, then <name> if it is a non-headless
// client Service. The legacy operator's bare-name headless Service is skipped.
func (m *Manifest) preservePublicServiceValues(ctx context.Context, values map[string]interface{}, clusterName string) error {
	for _, name := range []string{clusterName + "-public", clusterName} {
		svc, err := m.clientset.CoreV1().Services(m.namespace).Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return errors.Wrapf(err, "fetching public Service %s", name)
		}
		if svc.Spec.ClusterIP == "None" {
			continue // Never adopt the operator's discovery Service as the client Service.
		}
		service := values["cockroachdb"].(map[string]interface{})["crdbCluster"].(map[string]interface{})["service"].(map[string]interface{})
		public := map[string]interface{}{"name": svc.Name, "type": string(svc.Spec.Type)}
		labels := make(map[string]string)
		for key, value := range svc.Labels {
			switch key {
			case "helm.sh/chart", "app.kubernetes.io/name", "app.kubernetes.io/instance", "app.kubernetes.io/managed-by":
				continue
			}
			labels[key] = value
		}
		annotations := make(map[string]string)
		for key, value := range svc.Annotations {
			switch key {
			case "meta.helm.sh/release-name", "meta.helm.sh/release-namespace", "kubectl.kubernetes.io/last-applied-configuration", "service.alpha.kubernetes.io/app-protocols":
				continue
			}
			annotations[key] = value
		}
		public["labels"] = labels
		public["annotations"] = annotations
		service["public"] = public
		ports := service["ports"].(map[string]interface{})
		for _, port := range svc.Spec.Ports {
			if entry, ok := ports[port.TargetPort.String()].(map[string]interface{}); ok && port.Name != "" {
				entry["name"] = port.Name
			}
		}
		return nil
	}
	return nil
}

// loadIngressesForCluster fetches the optional UI and SQL ingress resources
// that should be carried into Helm adoption when present. Legacy Helm ingress
// names are not translated; users must review those routes before adoption.
func (m *Manifest) loadIngressesForCluster(
	ctx context.Context, clusterName string,
) ([]networkingv1.Ingress, error) {
	var ingresses []networkingv1.Ingress
	for _, name := range []string{
		fmt.Sprintf("ui-%s", clusterName),
		fmt.Sprintf("sql-%s", clusterName),
	} {
		ingress, err := m.clientset.NetworkingV1().Ingresses(m.namespace).Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return nil, errors.Wrapf(err, "fetching ingress %s", name)
		}
		ingresses = append(ingresses, *ingress)
	}
	return ingresses, nil
}

func (m *Manifest) FromHelmChart() error {
	ctx := context.TODO()

	sts, err := m.clientset.AppsV1().StatefulSets(m.namespace).Get(ctx, m.objectName, metav1.GetOptions{})
	if err != nil {
		return errors.Wrap(err, "fetching statefulset")
	}
	if err = validateStatefulSetForHelmMigration(sts); err != nil {
		return errors.Wrap(err, "validating helm statefulset")
	}
	if sts.Spec.Replicas == nil {
		return errors.Newf("statefulset %s has nil replicas", sts.Name)
	}

	input, err := generateParsedMigrationInput(ctx, m.clientset, sts)
	if err != nil {
		return err
	}

	if err := certificatesInput(ctx, m.dynamicClient, &input, sts); err != nil {
		return err
	}

	if input.certManagerInput != nil {
		if err := backupCAIssuerAndCert(ctx, m.dynamicClient, sts, m.outputDir); err != nil {
			return errors.Wrap(err, "backing up CA issuer and cert")
		}
	}

	if err := generateUpdatedPublicServiceConfig(ctx, m.clientset, sts.Namespace, fmt.Sprintf("%s-public", sts.Name), m.outputDir); err != nil {
		return err
	}

	input.pcrSpec = detectPCRFromInitJob(m.clientset, sts.Name, m.namespace)
	for nodeIdx := int32(0); nodeIdx < *sts.Spec.Replicas; nodeIdx++ {
		podName := fmt.Sprintf("%s-%d", sts.Name, nodeIdx)
		pod, err := m.clientset.CoreV1().Pods(m.namespace).Get(ctx, podName, metav1.GetOptions{})
		if err != nil {
			return errors.Newf("couldn't find crdb pod %s", podName)
		}

		if pod.Spec.NodeName == "" {
			return errors.Newf("pod %s isn't scheduled to a node", podName)
		}

		// Build the walFailoverSpec if applicable
		if err = buildWalFailoverSpec(ctx, m.clientset, sts, pod.Spec.NodeName, nodeIdx, &input); err != nil {
			return errors.Wrap(err, "building WAL failover spec")
		}
		nodeSpec := buildNodeSpecFromHelm(sts, pod.Spec.NodeName, input)
		crdbNode := v1beta1.CrdbNode{
			TypeMeta: metav1.TypeMeta{
				Kind:       "CrdbNode",
				APIVersion: "crdb.cockroachlabs.com/v1beta1",
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

	newHelmValues := buildHelmValuesFromHelm(sts, m.cloudProvider, m.cloudRegion, m.namespace, input)
	if err := m.restoreSelfSignerValues(ctx, newHelmValues, sts.Name, input.nodeSecretName, input.clientSecretName); err != nil {
		return err
	}

	if err := yamlToDisk(filepath.Join(m.outputDir, "values.yaml"), []any{newHelmValues}); err != nil {
		return errors.Wrap(err, "writing helm values to disk")
	}

	if len(input.localityLabels) > 0 {
		fmt.Println("⚠️  Locality labels detected on CockroachDB cluster.")
		fmt.Println("CockroachDB uses locality labels to distribute pods across failure domains (e.g., zones or regions).")
		fmt.Println("These labels must be present on the Kubernetes nodes before upgrading to new operator.")
		fmt.Printf("Following locality label keys are supplied to cockroachdb nodes: %v\n", input.localityLabels)
		fmt.Println("\n❗ If the required locality labels are missing from kubernetes nodes, the CockroachDB pods will not start.")
	}

	return nil
}
