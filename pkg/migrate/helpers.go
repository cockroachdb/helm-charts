package migrate

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	certv1 "github.com/jetstack/cert-manager/pkg/apis/certmanager/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/yaml"

	publicv1 "github.com/cockroachdb/cockroach-operator/apis/v1alpha1"
	"github.com/cockroachdb/errors"
	"github.com/cockroachdb/helm-charts/pkg/upstream/cockroach-operator/api/v1alpha1"
)

const (
	logConfigVolumeName            = "log-config"
	crdbContainerName              = "db"
	joinStrPrefix                  = "--join="
	portPrefix                     = "--port="
	httpPortPrefix                 = "--http-port="
	insecureFlag                   = "--insecure"
	localityFlag                   = "--locality"
	logtostderrFlag                = "--logtostderr"
	logFlag                        = "--log"
	grpcName                       = "grpc"
	grpcPort                       = 26258
	sqlName                        = "sql"
	sqlPort                        = 26257
	ProtocolName                   = "TCP"
	publicSvcYaml                  = "public-service.yaml"
	helmLogConfigKey               = "log-config.yaml"
	publicOperatorLogConfigKey     = "logging.yaml"
	enterpriseOperatorLogConfigKey = "logs.yaml"
	certManagerGroup               = "cert-manager.io"
	certManagerVersion             = "v1"
	certificatesResource           = "certificates"
	issuersResource                = "issuers"
)

type parsedMigrationInput struct {
	sqlPort          int32
	grpcPort         int32
	httpPort         int32
	tlsEnabled       bool
	localityLabels   []string
	loggingConfigMap string
	startFlags       *v1alpha1.Flags
	certManagerInput *certManagerInput
	caConfigMap      string
	nodeSecretName   string
	clientSecretName string
}

type certManagerInput struct {
	issuerName string
	issuerKind string
}

func To[T any](v T) *T {
	return &v
}

// yamlToDisk marshals the given data to YAML and writes it to the given path.
func yamlToDisk(path string, data []any) error {
	file, err := os.Create(path)
	if err != nil {
		return errors.Wrap(err, "creating file")
	}

	for i := range data {
		bytes, err := yaml.Marshal(data[i])
		if err != nil {
			return errors.Wrap(err, "marshalling yaml")
		}
		// Hack: drop creationTimestamp: null lines. See
		// https://github.com/kubernetes/kubernetes/issues/67610 for details.
		lines := strings.Split(string(bytes), "\n")
		filteredLines := []string{}
		timestampRE := regexp.MustCompile(`\s*creationTimestamp: null`)
		for _, line := range lines {
			if !timestampRE.MatchString(line) {
				filteredLines = append(filteredLines, line)
			}
		}
		if i > 0 {
			_, _ = file.WriteString("---\n")
		}
		if _, err := file.Write([]byte(strings.Join(filteredLines, "\n"))); err != nil {
			return errors.Wrap(err, "writing yaml")
		}
	}

	return nil
}

// buildNodeSpecFromOperator builds a CrdbNodeSpec from a publicv1.CrdbCluster and a StatefulSet created by the public operator.
func buildNodeSpecFromOperator(cluster publicv1.CrdbCluster, sts *appsv1.StatefulSet, nodeName string, startFlags *v1alpha1.Flags) v1alpha1.CrdbNodeSpec {

	return v1alpha1.CrdbNodeSpec{
		NodeName:       nodeName,
		PodLabels:      sts.Spec.Template.Labels,
		PodAnnotations: sts.Spec.Template.Annotations,
		StartFlags:     startFlags,
		DataStore: v1alpha1.DataStore{
			VolumeClaimTemplate: &corev1.PersistentVolumeClaim{
				ObjectMeta: metav1.ObjectMeta{
					Name: "datadir",
				},
				Spec: sts.Spec.VolumeClaimTemplates[0].Spec,
			},
		},
		Domain:               "",
		LoggingConfigMapName: cluster.Spec.LogConfigMap,
		Env: append(sts.Spec.Template.Spec.Containers[0].Env, []corev1.EnvVar{
			{
				Name: "HOST_IP",
				ValueFrom: &corev1.EnvVarSource{
					FieldRef: &corev1.ObjectFieldSelector{
						APIVersion: "v1",
						FieldPath:  "status.hostIP",
					},
				},
			},
		}...),
		ResourceRequirements: sts.Spec.Template.Spec.Containers[0].Resources,
		Image:                sts.Spec.Template.Spec.Containers[0].Image,
		ServiceAccountName:   cluster.Name,
		GRPCPort:             cluster.Spec.GRPCPort,
		SQLPort:              cluster.Spec.SQLPort,
		HTTPPort:             cluster.Spec.HTTPPort,
		Certificates: v1alpha1.Certificates{
			ExternalCertificates: &v1alpha1.ExternalCertificates{
				CAConfigMapName:         cluster.Name + "-ca-crt",
				NodeSecretName:          cluster.Name + "-node-secret",
				RootSQLClientSecretName: cluster.Name + "-client-secret",
			},
		},
		Affinity:                  sts.Spec.Template.Spec.Affinity,
		NodeSelector:              sts.Spec.Template.Spec.NodeSelector,
		Tolerations:               sts.Spec.Template.Spec.Tolerations,
		TerminationGracePeriod:    &metav1.Duration{Duration: time.Duration(cluster.Spec.TerminationGracePeriodSecs) * time.Second},
		TopologySpreadConstraints: sts.Spec.Template.Spec.TopologySpreadConstraints,
	}
}

// buildHelmValuesFromOperator builds a map of values for the CockroachDB Helm chart from a publicv1.CrdbCluster and a StatefulSet created by the public operator.
func buildHelmValuesFromOperator(
	cluster publicv1.CrdbCluster,
	sts *appsv1.StatefulSet,
	cloudProvider string,
	cloudRegion string,
	namespace string,
	flags *v1alpha1.Flags) map[string]interface{} {

	ingressValue := buildIngressValue(cluster)

	return map[string]interface{}{
		"cockroachdb": map[string]interface{}{
			"tls": map[string]interface{}{
				"enabled": cluster.Spec.TLSEnabled,
				"selfSigner": map[string]interface{}{
					"enabled": false,
				},
				"externalCertificates": map[string]interface{}{
					"enabled": true,
					"certificates": map[string]interface{}{
						"caConfigMapName":         cluster.Name + "-ca-crt",
						"nodeSecretName":          cluster.Name + "-node-secret",
						"rootSqlClientSecretName": cluster.Name + "-client-secret",
					},
				},
			},
			"crdbCluster": map[string]interface{}{
				"image": map[string]interface{}{
					"name": cluster.Spec.Image.Name,
				},
				"podLabels":      sts.Spec.Template.Labels,
				"podAnnotations": sts.Spec.Template.Annotations,
				"resources":      cluster.Spec.Resources,
				"startFlags":     flags,
				"regions": []map[string]interface{}{
					{
						"namespace":     namespace,
						"cloudProvider": cloudProvider,
						"code":          cloudRegion,
						"nodes":         cluster.Spec.Nodes,
						"domain":        "",
					},
				},
				"dataStore": map[string]interface{}{
					"volumeClaimTemplate": map[string]interface{}{
						"metadata": map[string]interface{}{
							"name": "datadir",
						},
						"spec": sts.Spec.VolumeClaimTemplates[0].Spec,
					},
				},
				"service": map[string]interface{}{
					"ports": map[string]interface{}{
						"grpc": map[string]interface{}{
							"port": cluster.Spec.GRPCPort,
						},
						"http": map[string]interface{}{
							"port": cluster.Spec.HTTPPort,
						},
						"sql": map[string]interface{}{
							"port": cluster.Spec.SQLPort,
						},
					},
					"public": map[string]interface{}{
						"name": cluster.Name + "-public",
					},
					"ingress": ingressValue,
				},
				"affinity":                  sts.Spec.Template.Spec.Affinity,
				"nodeSelector":              sts.Spec.Template.Spec.NodeSelector,
				"tolerations":               sts.Spec.Template.Spec.Tolerations,
				"terminationGracePeriod":    fmt.Sprintf("%ds", cluster.Spec.TerminationGracePeriodSecs),
				"loggingConfigMapName":      cluster.Spec.LogConfigMap,
				"env":                       sts.Spec.Template.Spec.Containers[0].Env,
				"topologySpreadConstraints": sts.Spec.Template.Spec.TopologySpreadConstraints,
			},
		},
		"k8s": map[string]interface{}{
			"fullnameOverride": cluster.Name,
		},
	}
}

// buildIngressValue constructs the ingress section of the Helm values
func buildIngressValue(cluster publicv1.CrdbCluster) map[string]interface{} {
	spec := cluster.Spec.Ingress
	if spec == nil {
		return map[string]interface{}{"enabled": false}
	}

	result := map[string]interface{}{"enabled": true}

	if spec.UI != nil {
		result["ui"] = map[string]interface{}{
			"ingressClassName": spec.UI.IngressClassName,
			"annotations":      spec.UI.Annotations,
			"host":             spec.UI.Host,
		}
	}
	if spec.SQL != nil {
		result["sql"] = map[string]interface{}{
			"ingressClassName": spec.SQL.IngressClassName,
			"annotations":      spec.SQL.Annotations,
			"host":             spec.SQL.Host,
		}
	}
	return result
}

// buildNodeSpecFromHelm builds a CrdbNodeSpec from a StatefulSet created by the CockroachDB Helm chart.
func buildNodeSpecFromHelm(
	sts *appsv1.StatefulSet,
	nodeName string,
	input parsedMigrationInput) v1alpha1.CrdbNodeSpec {

	return v1alpha1.CrdbNodeSpec{
		NodeName:       nodeName,
		PodLabels:      sts.Spec.Template.Labels,
		PodAnnotations: sts.Spec.Template.Annotations,
		StartFlags:     input.startFlags,
		DataStore: v1alpha1.DataStore{
			VolumeClaimTemplate: &corev1.PersistentVolumeClaim{
				ObjectMeta: metav1.ObjectMeta{
					Name: "datadir",
				},
				Spec: sts.Spec.VolumeClaimTemplates[0].Spec,
			},
		},
		Domain:               "",
		LocalityLabels:       input.localityLabels,
		LoggingConfigMapName: input.loggingConfigMap,
		Env: append(sts.Spec.Template.Spec.Containers[0].Env, []corev1.EnvVar{
			{
				Name: "HOST_IP",
				ValueFrom: &corev1.EnvVarSource{
					FieldRef: &corev1.ObjectFieldSelector{
						APIVersion: "v1",
						FieldPath:  "status.hostIP",
					},
				},
			},
		}...),
		ResourceRequirements: sts.Spec.Template.Spec.Containers[0].Resources,
		Image:                sts.Spec.Template.Spec.Containers[0].Image,
		ServiceAccountName:   sts.Name,
		GRPCPort:             &input.grpcPort,
		SQLPort:              &input.sqlPort,
		HTTPPort:             &input.httpPort,
		Certificates: v1alpha1.Certificates{
			ExternalCertificates: &v1alpha1.ExternalCertificates{
				CAConfigMapName:         input.caConfigMap,
				NodeSecretName:          input.nodeSecretName,
				RootSQLClientSecretName: input.clientSecretName,
				HTTPSecretName:          input.clientSecretName,
			},
		},
		Affinity:                  sts.Spec.Template.Spec.Affinity,
		NodeSelector:              sts.Spec.Template.Spec.NodeSelector,
		Tolerations:               sts.Spec.Template.Spec.Tolerations,
		TerminationGracePeriod:    &metav1.Duration{Duration: time.Duration(*sts.Spec.Template.Spec.TerminationGracePeriodSeconds) * time.Second},
		TopologySpreadConstraints: sts.Spec.Template.Spec.TopologySpreadConstraints,
	}
}

// buildHelmValuesFromHelm builds a values.yaml for the CockroachDB Enterprise Operator Helm chart from a StatefulSet created by the CockroachDB Helm chart.
func buildHelmValuesFromHelm(
	sts *appsv1.StatefulSet,
	cloudProvider string,
	cloudRegion string,
	namespace string,
	input parsedMigrationInput) map[string]interface{} {

	tls := map[string]interface{}{
		"enabled": input.tlsEnabled,
		"selfSigner": map[string]interface{}{
			"enabled": false,
		},
		"externalCertificates": map[string]interface{}{
			"enabled": true,
			"certificates": map[string]interface{}{
				"caConfigMapName":         input.caConfigMap,
				"nodeSecretName":          input.nodeSecretName,
				"rootSqlClientSecretName": input.clientSecretName,
				"httpSecretName":          input.clientSecretName,
			},
		},
	}
	if input.certManagerInput != nil {
		tls = map[string]interface{}{
			"enabled": input.tlsEnabled,
			"selfSigner": map[string]interface{}{
				"enabled": false,
			},
			"certManager": map[string]interface{}{
				"enabled":          true,
				"caConfigMap":      input.caConfigMap,
				"nodeSecret":       input.nodeSecretName,
				"clientRootSecret": input.clientSecretName,
				"issuer": map[string]interface{}{
					"name": input.certManagerInput.issuerName,
					"kind": input.certManagerInput.issuerKind,
				},
			},
		}
	}

	return map[string]interface{}{
		"cockroachdb": map[string]interface{}{
			"tls": tls,
			"crdbCluster": map[string]interface{}{
				"image": map[string]interface{}{
					"name": sts.Spec.Template.Spec.Containers[0].Image,
				},
				"localityLabels": input.localityLabels,
				"podLabels":      sts.Spec.Template.Labels,
				"podAnnotations": sts.Spec.Template.Annotations,
				"resources":      sts.Spec.Template.Spec.Containers[0].Resources,
				"startFlags":     input.startFlags,
				"regions": []map[string]interface{}{
					{
						"namespace":     namespace,
						"cloudProvider": cloudProvider,
						"code":          cloudRegion,
						"nodes":         sts.Spec.Replicas,
						"domain":        "",
					},
				},
				"dataStore": map[string]interface{}{
					"volumeClaimTemplate": map[string]interface{}{
						"metadata": map[string]interface{}{
							"name": "datadir",
						},
						"spec": sts.Spec.VolumeClaimTemplates[0].Spec,
					},
				},
				"service": map[string]interface{}{
					"ports": map[string]interface{}{
						"grpc": map[string]interface{}{
							"port": input.grpcPort,
						},
						"http": map[string]interface{}{
							"port": input.httpPort,
						},
						"sql": map[string]interface{}{
							"port": input.sqlPort,
						},
					},
				},
				"affinity":                  sts.Spec.Template.Spec.Affinity,
				"nodeSelector":              sts.Spec.Template.Spec.NodeSelector,
				"tolerations":               sts.Spec.Template.Spec.Tolerations,
				"terminationGracePeriod":    fmt.Sprintf("%ds", *sts.Spec.Template.Spec.TerminationGracePeriodSeconds),
				"loggingConfigMapName":      input.loggingConfigMap,
				"env":                       sts.Spec.Template.Spec.Containers[0].Env,
				"topologySpreadConstraints": sts.Spec.Template.Spec.TopologySpreadConstraints,
			},
		},
	}
}

// generateParsedMigrationInput parses the command arguments, extracts the --join string, and replaces env variables.
func generateParsedMigrationInput(
	ctx context.Context,
	clientset kubernetes.Interface,
	sts *appsv1.StatefulSet) (parsedMigrationInput, error) {
	var startCmd string
	var parsedInput = parsedMigrationInput{
		tlsEnabled: true,
	}

	// In the public Helm chart, logging configuration is provided as a secret to the StatefulSet.
	// However, in the Cockroach Enterprise Operator, it is supplied as a ConfigMap.
	for _, vol := range sts.Spec.Template.Spec.Volumes {
		if vol.Name == logConfigVolumeName {
			if vol.Secret != nil {
				parsedInput.loggingConfigMap = vol.Secret.SecretName
				if err := ConvertSecretToConfigMap(ctx, clientset, sts.Namespace, parsedInput.loggingConfigMap); err != nil {
					return parsedInput, err
				}
			}
		}
	}

	for _, c := range sts.Spec.Template.Spec.Containers {
		if c.Name == crdbContainerName {
			startCmd = c.Args[2]
		}
	}

	if err := extractJoinStringAndFlags(&parsedInput, strings.Fields(startCmd)); err != nil {
		return parsedInput, err
	}

	return parsedInput, nil
}

// certificatesInput checks if the node certificate exists in the cluster and adds the certificate input based on
// the presence of the node certificate. If the node certificate is present, it means the cert-manager manages the certificates.
// If not, then certs are managed by self-signer utility
func certificatesInput(ctx context.Context, dynamicClient dynamic.Interface, parsedInput *parsedMigrationInput, sts *appsv1.StatefulSet) error {
	var (
		err error
	)

	nodeCertificateName := fmt.Sprintf("%s-node", sts.Name)
	// if the node certificate is not present, then we assume that the cert-manager is not managing the certificates
	// and we will use the self-signer utility to generate the certificates.
	nodeCertificate, err := getCertificate(ctx, dynamicClient, nodeCertificateName, sts.Namespace)
	if err != nil {
		parsedInput.caConfigMap = fmt.Sprintf("%s-ca-secret-crt", sts.Name)
		parsedInput.nodeSecretName = fmt.Sprintf("%s-node-secret", sts.Name)
		parsedInput.clientSecretName = fmt.Sprintf("%s-client-secret", sts.Name)
		return nil
	}

	parsedInput.caConfigMap = fmt.Sprintf("%s-ca-crt", sts.Name)
	parsedInput.certManagerInput = &certManagerInput{
		issuerName: nodeCertificate.Spec.IssuerRef.Name,
		issuerKind: nodeCertificate.Spec.IssuerRef.Kind,
	}
	parsedInput.nodeSecretName = nodeCertificate.Spec.SecretName

	clientCertificateName := fmt.Sprintf("%s-root-client", sts.Name)
	clientCertificate, err := getCertificate(ctx, dynamicClient, clientCertificateName, sts.Namespace)
	if err != nil {
		return err
	}
	parsedInput.clientSecretName = clientCertificate.Spec.SecretName

	return nil
}

// extractJoinStringAndFlags parses the command arguments, extracts the --join string, and replaces env variables.
func extractJoinStringAndFlags(
	parsedInput *parsedMigrationInput,
	args []string) error {

	flags := &v1alpha1.Flags{}
	// Regular expression to match flags (e.g., --advertise-host=something)
	flagRegex := regexp.MustCompile(`--([\w-]+)=(.*)`)

	for _, arg := range args {
		switch {
		case strings.HasPrefix(arg, joinStrPrefix):
			flags.Upsert = append(flags.Upsert, fmt.Sprintf("%s", arg))
		case strings.HasPrefix(arg, portPrefix):
			num, err := parseInt32(strings.TrimPrefix(arg, portPrefix))
			if err != nil {
				return fmt.Errorf("invalid --port value: %w", err)
			}
			parsedInput.sqlPort = num

		case strings.HasPrefix(arg, httpPortPrefix):
			num, err := parseInt32(strings.TrimPrefix(arg, httpPortPrefix))
			if err != nil {
				return fmt.Errorf("invalid --http-port value: %w", err)
			}
			parsedInput.httpPort = num

		case strings.HasPrefix(arg, insecureFlag):
			parsedInput.tlsEnabled = false

		case strings.HasPrefix(arg, localityFlag):
			value := strings.TrimPrefix(arg, localityFlag+"=")
			labels := strings.Split(value, ",")
			for i := range labels {
				parsedInput.localityLabels = append(parsedInput.localityLabels, strings.Split(labels[i], "=")[0])
			}

		// CockroachDB Enterprise Operator automatically adds "--logs" flag if it is not present.
		case strings.HasPrefix(arg, logtostderrFlag):
			continue

		case strings.HasPrefix(arg, logFlag):
			continue

		default:
			if matches := flagRegex.FindStringSubmatch(arg); len(matches) == 3 {
				flags.Upsert = append(flags.Upsert, fmt.Sprintf("--%s=%s", matches[1], matches[2]))
			}
			parsedInput.startFlags = flags
		}
	}

	// The helm chart configures crdb to listen for grpc and sql on one port and for http on another.
	// The cloud operator uses three distinct ports for grpc, sql, and http.
	// Default port for grpc is 26258
	parsedInput.grpcPort = 26258

	return nil
}

// parseInt32 safely converts a string to int32
func parseInt32(value string) (int32, error) {
	num, err := strconv.ParseInt(value, 10, 32) // Base 10, 32-bit size
	if err != nil {
		return 0, err
	}
	return int32(num), nil
}

// ConvertSecretToConfigMap retrieves a secret and creates a ConfigMap with the same data.
func ConvertSecretToConfigMap(ctx context.Context, clientset kubernetes.Interface, namespace, secretName string) error {
	// Get the Secret
	secret, err := clientset.CoreV1().Secrets(namespace).Get(ctx, secretName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("failed to get secret %s: %w", secretName, err)
	}

	// Convert Secret data to ConfigMap data
	configMapData := make(map[string]string)
	for key, value := range secret.Data {
		if key == helmLogConfigKey {
			configMapData[enterpriseOperatorLogConfigKey] = string(value)
		}
	}

	// Create a new ConfigMap
	configMap := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretName,
			Namespace: namespace,
		},
		Data: configMapData,
	}

	// Create the ConfigMap in Kubernetes
	_, err = clientset.CoreV1().ConfigMaps(namespace).Create(ctx, configMap, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("failed to create ConfigMap %s: %w", secretName, err)
	}

	fmt.Println("ConfigMap created successfully:", secretName)
	return nil
}

// moveConfigMapKey moves the "logging.yaml" key to "logs.yaml" in the ConfigMap.
// This is a solution to support the migration from the public operator to the Cockroach Enterprise Operator.
// The public operator uses "logging.yaml" and the Cockroach Enterprise Operator uses "logs.yaml".
func moveConfigMapKey(ctx context.Context, clientset kubernetes.Interface, namespace, configMapName string) error {
	configMap, err := clientset.CoreV1().ConfigMaps(namespace).Get(ctx, configMapName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("failed to get ConfigMap %s: %w", configMapName, err)
	}

	for key, val := range configMap.Data {
		if key == publicOperatorLogConfigKey {
			configMap.Data[enterpriseOperatorLogConfigKey] = val
		}
	}

	// Update the ConfigMap
	_, err = clientset.CoreV1().ConfigMaps(namespace).Update(ctx, configMap, metav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("failed to update ConfigMap %s: %w", configMapName, err)
	}

	return nil
}

// generateUpdatedPublicServiceConfig updates the "cockroachdb-public" service with separate sql and grpc ports.
func generateUpdatedPublicServiceConfig(ctx context.Context, clientset kubernetes.Interface, namespace, name, outputDir string) error {
	var (
		grpcFound, sqlFound bool
	)

	svc, err := clientset.CoreV1().Services(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("failed to get service %s: %w", name, err)
	}
	grpcSvcPort := corev1.ServicePort{
		Name:       grpcName,
		Protocol:   ProtocolName,
		Port:       grpcPort,
		TargetPort: intstr.IntOrString{Type: 1, StrVal: grpcName},
	}

	sqlSvcPort := corev1.ServicePort{
		Name:       sqlName,
		Protocol:   ProtocolName,
		Port:       sqlPort,
		TargetPort: intstr.IntOrString{Type: 1, StrVal: sqlName},
	}

	for i := range svc.Spec.Ports {
		if svc.Spec.Ports[i].Name == grpcName {
			grpcFound = true
			svc.Spec.Ports[i] = grpcSvcPort
		} else if svc.Spec.Ports[i].Name == sqlName {
			sqlFound = true
			svc.Spec.Ports[i] = sqlSvcPort
		}
	}

	if !grpcFound {
		svc.Spec.Ports = append(svc.Spec.Ports, grpcSvcPort)
	}

	if !sqlFound {
		svc.Spec.Ports = append(svc.Spec.Ports, sqlSvcPort)
	}

	svc.TypeMeta = metav1.TypeMeta{
		APIVersion: "v1",
		Kind:       "Service",
	}

	err = yamlToDisk(filepath.Join(outputDir, publicSvcYaml), []any{svc})
	if err != nil {
		panic(err)
	}

	return nil
}

// buildRBACFromPublicOperator builds the RBAC resources from the public operator which is used by the cockroachdb enterprise operator.
func buildRBACFromPublicOperator(cluster publicv1.CrdbCluster, outputDir string) error {
	clusterRole := &rbacv1.ClusterRole{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "rbac.authorization.k8s.io/v1",
			Kind:       "ClusterRole",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: cluster.Name,
			Annotations: map[string]string{
				"meta.helm.sh/release-name":      cluster.Name,
				"meta.helm.sh/release-namespace": cluster.Namespace,
			},
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "Helm",
			},
		},
		Rules: []rbacv1.PolicyRule{
			{
				APIGroups: []string{""},
				Resources: []string{"nodes"},
				Verbs:     []string{"get"},
			},
			{
				APIGroups: []string{"certificates.k8s.io"},
				Resources: []string{"certificatesigningrequests"},
				Verbs:     []string{"create", "get", "watch"},
			},
		},
	}

	clusterRoleBinding := &rbacv1.ClusterRoleBinding{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "rbac.authorization.k8s.io/v1",
			Kind:       "ClusterRoleBinding",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: cluster.Name,
			Annotations: map[string]string{
				"meta.helm.sh/release-name":      cluster.Name,
				"meta.helm.sh/release-namespace": cluster.Namespace,
			},
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "Helm",
			},
		},
		RoleRef: rbacv1.RoleRef{
			APIGroup: "rbac.authorization.k8s.io",
			Kind:     "ClusterRole",
			Name:     cluster.Name,
		},
		Subjects: []rbacv1.Subject{
			{
				Kind:      "ServiceAccount",
				Name:      cluster.Name,
				Namespace: cluster.Namespace,
			},
		},
	}

	role := &rbacv1.Role{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "rbac.authorization.k8s.io/v1",
			Kind:       "Role",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      cluster.Name,
			Namespace: cluster.Namespace,
			Annotations: map[string]string{
				"meta.helm.sh/release-name":      cluster.Name,
				"meta.helm.sh/release-namespace": cluster.Namespace,
			},
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "Helm",
			},
		},
		Rules: []rbacv1.PolicyRule{
			{
				APIGroups: []string{""},
				Resources: []string{"secrets"},
				Verbs:     []string{"create", "get"},
			},
		},
	}

	roleBinding := &rbacv1.RoleBinding{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "rbac.authorization.k8s.io/v1",
			Kind:       "RoleBinding",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      cluster.Name,
			Namespace: cluster.Namespace,
			Annotations: map[string]string{
				"meta.helm.sh/release-name":      cluster.Name,
				"meta.helm.sh/release-namespace": cluster.Namespace,
			},
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "Helm",
			},
		},
		RoleRef: rbacv1.RoleRef{
			APIGroup: "rbac.authorization.k8s.io",
			Kind:     "Role",
			Name:     cluster.Name,
		},
		Subjects: []rbacv1.Subject{
			{
				Kind:      "ServiceAccount",
				Name:      cluster.Name,
				Namespace: cluster.Namespace,
			},
		},
	}

	serviceAccount := &corev1.ServiceAccount{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "v1",
			Kind:       "ServiceAccount",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      cluster.Name,
			Namespace: cluster.Namespace,
			Annotations: map[string]string{
				"meta.helm.sh/release-name":      cluster.Name,
				"meta.helm.sh/release-namespace": cluster.Namespace,
			},
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "Helm",
			},
		},
	}

	return yamlToDisk(filepath.Join(outputDir, "rbac.yaml"), []any{clusterRole, clusterRoleBinding, role, roleBinding, serviceAccount})
}

// backupCAIssuerAndCert if generated by the previous helm chart.
func backupCAIssuerAndCert(ctx context.Context, dynamicClient dynamic.Interface, sts *appsv1.StatefulSet, outputDir string) error {
	certManagerResources := []struct {
		name     string
		resource string
	}{
		{
			name:     fmt.Sprintf("%s-ca-issuer", sts.Name),
			resource: issuersResource,
		},
		{
			name:     fmt.Sprintf("%s-ca-cert", sts.Name),
			resource: certificatesResource,
		},
	}

	for i := range certManagerResources {
		resourceName := certManagerResources[i].name
		resourceType := certManagerResources[i].resource

		gvr := schema.GroupVersionResource{
			Group:    certManagerGroup,
			Version:  certManagerVersion,
			Resource: resourceType,
		}

		resource, err := dynamicClient.Resource(gvr).Namespace(sts.Namespace).Get(ctx, resourceName, metav1.GetOptions{})
		if err != nil {
			if apierrors.IsNotFound(err) {
				fmt.Printf("Resource %s %s not found, skipping backup.\n", resourceType, resourceName)
				continue
			}
			return fmt.Errorf("failed to get %s %s: %w", resourceType, resourceName, err)
		}

		annotations := resource.GetAnnotations()
		if _, ok := annotations["meta.helm.sh/release-name"]; !ok {
			// helm release annotation is not present, no need to backup as resource is not managed by helm.
			continue
		}

		if err := yamlToDisk(filepath.Join(outputDir, resourceName+".yaml"), []any{resource}); err != nil {
			return fmt.Errorf("failed to write %s to disk: %w", resourceType, err)
		}
		fmt.Printf("📁Backed up %s %s to %s\n", resourceType, resourceName, filepath.Join(outputDir, resourceName+".yaml"))
	}

	fmt.Println("⚙️ After helm upgrade, the backed up resources will be removed. Please recreate these resource after upgrade.")

	return nil
}

// getCertificate retrieves a certificate by name and namespace using the dynamic client.
func getCertificate(ctx context.Context, dynamicClient dynamic.Interface, name, namespace string) (*certv1.Certificate, error) {
	cert := &certv1.Certificate{}
	certGVR := schema.GroupVersionResource{
		Group:    certManagerGroup,
		Version:  certManagerVersion,
		Resource: certificatesResource,
	}

	certUnstructured, err := dynamicClient.Resource(certGVR).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}

	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(certUnstructured.Object, &cert); err != nil {
		return nil, errors.Wrap(err, "unmarshalling public crdbcluster objectName")
	}

	return cert, nil
}
