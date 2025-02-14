package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/cockroachdb/errors"
	"github.com/spf13/cobra"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/homedir"
	"sigs.k8s.io/yaml"

	publicv1 "github.com/cockroachdb/cockroach-operator/apis/v1alpha1"
	"github.com/cockroachdb/helm-charts/pkg/upstream/cockroach-operator/api/v1alpha1"
)

func main() {
	var (
		cloudProvider string
		cloudRegion   string
		crdbCluster   string
		namespace     string
		kubeconfig    string
		outputDir     string
	)

	cmd := &cobra.Command{
		Use:   "build-manifests",
		Short: "Generate migration manifests.",
		Long:  "Generate manifests for migrating from the public operator to the cloud operator.",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := context.TODO()

			config, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
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

			gvr := schema.GroupVersionResource{
				Group:    "crdb.cockroachlabs.com",
				Version:  "v1alpha1",
				Resource: "crdbclusters",
			}
			cr, err := dynamicClient.Resource(gvr).Namespace(namespace).Get(ctx, crdbCluster, metav1.GetOptions{})
			if err != nil {
				return errors.Wrap(err, "fetching public crdbcluster object")
			}

			publicCluster := publicv1.CrdbCluster{}
			if err := runtime.DefaultUnstructuredConverter.FromUnstructured(cr.Object, &publicCluster); err != nil {
				return errors.Wrap(err, "unmarshalling public crdbcluster object")
			}

			sts, err := clientset.AppsV1().StatefulSets(namespace).Get(ctx, crdbCluster, metav1.GetOptions{})
			if err != nil {
				return errors.Wrap(err, "fetching statefulset")
			}

			grpcPort := publicCluster.Spec.GRPCPort
			joinAddrs := []string{}
			for nodeIdx := range publicCluster.Spec.Nodes {
				joinAddrs = append(joinAddrs, fmt.Sprintf("%s-%d.%s.%s:%d", crdbCluster, nodeIdx, crdbCluster, namespace, grpcPort))
			}
			joinString := strings.Join(joinAddrs, ",")

			flags := map[string]string{}
			if publicCluster.Spec.Cache != "" {
				flags["--cache"] = publicCluster.Spec.Cache
			}
			if publicCluster.Spec.MaxSQLMemory != "" {
				flags["--max-sql-memory"] = publicCluster.Spec.MaxSQLMemory
			}

			for nodeIdx := range publicCluster.Spec.Nodes {
				podName := fmt.Sprintf("%s-%d", crdbCluster, nodeIdx)
				pod, err := clientset.CoreV1().Pods(namespace).Get(ctx, podName, metav1.GetOptions{})
				if err != nil {
					return errors.Newf("couldn't find crdb pod %s", podName)
				}

				if pod.Spec.NodeName == "" {
					return errors.Newf("pod %s isn't scheduled to a node", podName)
				}

				nodeSpec := buildNodeSpec(sts, pod.Spec.NodeName, joinString, flags)
				crdbNode := v1alpha1.CrdbNode{
					TypeMeta: metav1.TypeMeta{
						Kind:       "CrdbNode",
						APIVersion: "crdb.cockroachlabs.com/v1alpha1",
					},
					ObjectMeta: metav1.ObjectMeta{
						Name:         fmt.Sprintf("%s-%d", crdbCluster, nodeIdx),
						Namespace:    namespace,
						GenerateName: "",
						Labels: map[string]string{
							"app":                            "cockroachdb",
							"svc":                            "cockroachdb",
							"crdb.cockroachlabs.com/cluster": crdbCluster,
						},
						Annotations: map[string]string{
							"crdb.cockroachlabs.com/cloudProvider": cloudProvider,
						},
						Finalizers: []string{"crdbnode.crdb.cockroachlabs.com/finalizer"},
					},
					Spec: nodeSpec,
				}
				yamlToDisk(filepath.Join(outputDir, fmt.Sprintf("crdbnode-%d.yaml", nodeIdx)), crdbNode)
			}

			helmValues := buildHelmValues(publicCluster, cloudProvider, cloudRegion, namespace)

			yamlToDisk(filepath.Join(outputDir, "values.yaml"), helmValues)

			return nil
		},
	}

	cmd.PersistentFlags().StringVar(&cloudProvider, "cloud-provider", "", "name of cloud provider")
	cmd.PersistentFlags().StringVar(&cloudRegion, "cloud-region", "", "name of cloud provider region")
	cmd.PersistentFlags().StringVar(&crdbCluster, "crdb-cluster", "", "name of crdbcluster resource")
	cmd.PersistentFlags().StringVar(&namespace, "namespace", "default", "namespace of crdbcluster resource")
	cmd.PersistentFlags().StringVar(&kubeconfig, "kubeconfig", filepath.Join(homedir.HomeDir(), ".kube", "config"), "path to kubeconfig file")
	cmd.PersistentFlags().StringVar(&outputDir, "output-dir", "./manifests", "manifest output directory")
	cmd.MarkPersistentFlagRequired("cloud-provider")
	cmd.MarkPersistentFlagRequired("cloud-region")
	cmd.MarkPersistentFlagRequired("crdb-cluster")

	if err := cmd.Execute(); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
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
	if _, err := file.Write(bytes); err != nil {
		return errors.Wrap(err, "writing yaml")
	}
	return nil
}

func buildNodeSpec(cluster publicv1.CrdbCluster, sts *appsv1.StatefulSet, nodeName string, joinString string, flags map[string]string) v1alpha1.CrdbNodeSpec {
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
				Spec: corev1.PersistentVolumeClaimSpec{
					AccessModes: []corev1.PersistentVolumeAccessMode{
						corev1.ReadWriteOnce,
					},
					Resources:        sts.Spec.VolumeClaimTemplates[0].Spec.Resources,
					StorageClassName: sts.Spec.VolumeClaimTemplates[0].Spec.StorageClassName,
				},
			},
		},
		Domain: "",
		Env: []corev1.EnvVar{
			{
				Name: "HostIP",
				ValueFrom: &corev1.EnvVarSource{
					FieldRef: &corev1.ObjectFieldSelector{
						APIVersion: "v1",
						FieldPath:  "status.hostIP",
					},
				},
			},
		},
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
		Affinity:     cluster.Spec.Affinity,
		NodeSelector: cluster.Spec.NodeSelector,
		Tolerations:  cluster.Spec.Tolerations,
	}
}

func buildHelmValues(cluster publicv1.CrdbCluster, cloudProvider string, cloudRegion string, namespace string, flags map[string]string) map[string]interface{} {
	return map[string]interface{}{
		"operator": map[string]interface{}{
			"enabled":        true,
			"tlsEnabled":     cluster.Spec.TLSEnabled,
			"podLabels":      cluster.Spec.AdditionalLabels,
			"podAnnotations": cluster.Spec.AdditionalAnnotations,
			"resources":      cluster.Spec.Resources,
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
			"affinity":     cluster.Spec.Affinity,
			"nodeSelector": cluster.Spec.NodeSelector,
			"tolerations":  cluster.Spec.Tolerations,
		},
	}
}
