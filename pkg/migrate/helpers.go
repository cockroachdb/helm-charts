package migrate

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sigs.k8s.io/yaml"
	"strconv"
	"strings"
	"time"

	publicv1 "github.com/cockroachdb/cockroach-operator/apis/v1alpha1"
	"github.com/cockroachdb/errors"
	"github.com/cockroachdb/helm-charts/pkg/upstream/cockroach-operator/api/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes"
)

const (
	logConfigVolumeName = "log-config"
	crdbContainerName   = "db"
	joinStrPrefix       = "--join="
	portPrefix          = "--port="
	httpPortPrefix      = "--http-port="
	insecureFlag        = "--insecure"
	logtostderrFlag     = "--logtostderr"
	grpcName            = "grpc"
	grpcPort            = 26258
	sqlName             = "sql"
	sqlPort             = 26257
	ProtocolName        = "TCP"
	publicSvcYaml       = "public-service.yaml"
)

type parsedMigrationInput struct {
	sqlPort          int32
	grpcPort         int32
	httpPort         int32
	joinCmd          string
	tlsEnabled       bool
	loggingConfigMap string
	flags            map[string]string
}

func To[T any](v T) *T {
	return &v
}

func yamlToDisk(path string, data any) error {
	file, err := os.Create(path)
	if err != nil {
		return errors.Wrap(err, "creating file")
	}
	bytes, err := yaml.Marshal(data)
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
	if _, err := file.Write([]byte(strings.Join(filteredLines, "\n"))); err != nil {
		return errors.Wrap(err, "writing yaml")
	}
	return nil
}

func buildNodeSpecFromOperator(cluster publicv1.CrdbCluster, sts *appsv1.StatefulSet, nodeName string, joinString string, flags map[string]string) v1alpha1.CrdbNodeSpec {
	return v1alpha1.CrdbNodeSpec{
		NodeName:  nodeName,
		Join:      joinString,
		PodLabels: sts.Spec.Template.Labels,
		Flags:     flags,
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
		Env: append(cluster.Spec.PodEnvVariables, []corev1.EnvVar{
			{
				Name: "HostIP",
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
		ServiceAccountName:   "cockroachdb",
		GRPCPort:             cluster.Spec.GRPCPort,
		SQLPort:              cluster.Spec.SQLPort,
		HTTPPort:             cluster.Spec.HTTPPort,
		Certificates: v1alpha1.Certificates{
			ExternalCertificates: &v1alpha1.ExternalCertificates{
				CAConfigMapName:         cluster.Name + "-ca",
				NodeSecretName:          cluster.Name + "-node-certs",
				RootSQLClientSecretName: cluster.Name + "-client-certs",
			},
		},
		Affinity:               cluster.Spec.Affinity,
		NodeSelector:           cluster.Spec.NodeSelector,
		Tolerations:            cluster.Spec.Tolerations,
		TerminationGracePeriod: &metav1.Duration{Duration: time.Duration(cluster.Spec.TerminationGracePeriodSecs)},
	}
}

func buildHelmValuesFromOperator(cluster publicv1.CrdbCluster, cloudProvider string, cloudRegion string, namespace string, flags map[string]string) map[string]interface{} {
	return map[string]interface{}{
		"operator": map[string]interface{}{
			"enabled":        true,
			"tlsEnabled":     cluster.Spec.TLSEnabled,
			"podLabels":      cluster.Spec.AdditionalLabels,
			"podAnnotations": cluster.Spec.AdditionalAnnotations,
			"resources":      cluster.Spec.Resources,
			"flags":          flags,
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
				},
			},
			"ports": map[string]interface{}{
				"grpcPort": cluster.Spec.GRPCPort,
				"httpPort": cluster.Spec.HTTPPort,
				"sqlPort":  cluster.Spec.SQLPort,
			},
			"certificates": map[string]interface{}{
				"externalCertificates": map[string]interface{}{
					"caConfigMapName":         cluster.Name + "-ca",
					"nodeSecretName":          cluster.Name + "-node-certs",
					"rootSqlClientSecretName": cluster.Name + "-client-certs",
				},
			},
			"affinity":                      cluster.Spec.Affinity,
			"nodeSelector":                  cluster.Spec.NodeSelector,
			"tolerations":                   cluster.Spec.Tolerations,
			"terminationGracePeriodSeconds": cluster.Spec.TerminationGracePeriodSecs,
			"loggingConfigMapName":          cluster.Spec.LogConfigMap,
			"env":                           cluster.Spec.PodEnvVariables,
		},
	}
}

func buildNodeSpecFromHelm(sts *appsv1.StatefulSet, nodeName string, input parsedMigrationInput) v1alpha1.CrdbNodeSpec {
	return v1alpha1.CrdbNodeSpec{
		NodeName:  nodeName,
		Join:      input.joinCmd,
		PodLabels: sts.Spec.Template.Labels,
		Flags:     input.flags,
		DataStore: v1alpha1.DataStore{
			VolumeClaimTemplate: &corev1.PersistentVolumeClaim{
				ObjectMeta: metav1.ObjectMeta{
					Name: "datadir",
				},
				Spec: sts.Spec.VolumeClaimTemplates[0].Spec,
			},
		},
		Domain:               "",
		LoggingConfigMapName: input.loggingConfigMap,
		Env: append(sts.Spec.Template.Spec.Containers[0].Env, []corev1.EnvVar{
			{
				Name: "HostIP",
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
		ServiceAccountName:   "cockroachdb",
		GRPCPort:             &input.grpcPort,
		SQLPort:              &input.sqlPort,
		HTTPPort:             &input.httpPort,
		Certificates: v1alpha1.Certificates{
			ExternalCertificates: &v1alpha1.ExternalCertificates{
				CAConfigMapName:         sts.Name + "-ca",
				NodeSecretName:          sts.Name + "-node-certs",
				RootSQLClientSecretName: sts.Name + "-client-certs",
			},
		},
		Affinity:               sts.Spec.Template.Spec.Affinity,
		NodeSelector:           sts.Spec.Template.Spec.NodeSelector,
		Tolerations:            sts.Spec.Template.Spec.Tolerations,
		TerminationGracePeriod: &metav1.Duration{Duration: time.Duration(*sts.Spec.Template.Spec.TerminationGracePeriodSeconds) * time.Second},
	}
}

func buildHelmValuesFromHelm(
	sts *appsv1.StatefulSet,
	cloudProvider string,
	cloudRegion string,
	namespace string,
	input parsedMigrationInput) map[string]interface{} {

	return map[string]interface{}{
		"operator": map[string]interface{}{
			"enabled":        true,
			"tlsEnabled":     input.tlsEnabled,
			"podLabels":      sts.Spec.Template.Labels,
			"podAnnotations": sts.Spec.Template.Annotations,
			"resources":      sts.Spec.Template.Spec.Containers[0].Resources,
			"flags":          input.flags,
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
				},
			},
			"ports": map[string]interface{}{
				"grpcPort": input.grpcPort,
				"httpPort": input.httpPort,
				"sqlPort":  input.sqlPort,
			},
			"certificates": map[string]interface{}{
				"externalCertificates": map[string]interface{}{
					"caConfigMapName":         sts.Name + "-ca",
					"nodeSecretName":          sts.Name + "-node-certs",
					"rootSqlClientSecretName": sts.Name + "-client-certs",
				},
			},
			"affinity":                      sts.Spec.Template.Spec.Affinity,
			"nodeSelector":                  sts.Spec.Template.Spec.NodeSelector,
			"tolerations":                   sts.Spec.Template.Spec.Tolerations,
			"terminationGracePeriodSeconds": sts.Spec.Template.Spec.TerminationGracePeriodSeconds,
			"loggingConfigMapName":          input.loggingConfigMap,
			"env":                           sts.Spec.Template.Spec.Containers[0].Env,
		},
	}
}

func generateParsedMigrationInput(
	ctx context.Context,
	clientset kubernetes.Interface,
	sts *appsv1.StatefulSet) (parsedMigrationInput, error) {
	var startCmd string
	var envVars = make(map[string]string)
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
			for i := range c.Env {
				envVars[c.Env[i].Name] = c.Env[i].Value
			}
		}
	}

	if err := extractJoinStringAndFlags(&parsedInput, strings.Fields(startCmd), envVars); err != nil {
		return parsedInput, err
	}

	return parsedInput, nil
}

// extractJoinStringAndFlags parses the command arguments, extracts the --join string, and replaces env variables.
func extractJoinStringAndFlags(
	parsedInput *parsedMigrationInput,
	args []string,
	envVars map[string]string) error {

	flags := make(map[string]string)
	// Regular expression to match flags (e.g., --advertise-host=something)
	flagRegex := regexp.MustCompile(`--([\w-]+)=(.*)`)

	for _, arg := range args {
		switch {
		case strings.HasPrefix(arg, joinStrPrefix):
			parsedInput.joinCmd = strings.TrimPrefix(arg, joinStrPrefix)

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

		// CockroachDB Enterprise Operator automatically adds "--logs" flag if it is not present.
		case strings.HasPrefix(arg, logtostderrFlag):
			continue

		default:
			if matches := flagRegex.FindStringSubmatch(arg); len(matches) == 3 {
				flags[fmt.Sprintf("--%s", matches[1])] = matches[2]
			}
			parsedInput.flags = flags
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
		configMapData[key] = string(value) // If encoding needed
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

	err = yamlToDisk(filepath.Join(outputDir, publicSvcYaml), svc)
	if err != nil {
		panic(err)
	}

	return nil
}
