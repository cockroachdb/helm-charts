package testutil

import (
	api "github.com/cockroachdb/cockroach-operator/apis/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apiresource "k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type ClusterBuilder struct {
	cluster api.CrdbCluster
}

func NewBuilder(name string) ClusterBuilder {
	b := ClusterBuilder{
		cluster: api.CrdbCluster{
			ObjectMeta: metav1.ObjectMeta{
				Name:        name,
				Labels:      make(map[string]string),
				Annotations: make(map[string]string),
			},
			Spec: api.CrdbClusterSpec{
				Image: &api.PodImage{},
			},
		},
	}

	return b
}

func (b ClusterBuilder) WithTerminationGracePeriodSeconds(s int64) ClusterBuilder {
	b.cluster.Spec.TerminationGracePeriodSecs = s
	return b
}

func (b ClusterBuilder) WithNodeCount(c int32) ClusterBuilder {
	b.cluster.Spec.Nodes = c
	return b
}

func (b ClusterBuilder) WithPVDataStore(size string) ClusterBuilder {
	quantity, _ := apiresource.ParseQuantity(size)

	volumeMode := corev1.PersistentVolumeFilesystem
	b.cluster.Spec.DataStore = api.Volume{
		VolumeClaim: &api.VolumeClaim{
			PersistentVolumeClaimSpec: corev1.PersistentVolumeClaimSpec{
				AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
				Resources: corev1.VolumeResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceStorage: quantity,
					},
				},
				VolumeMode: &volumeMode,
			},
		},
	}

	return b
}

func (b ClusterBuilder) WithTLS() ClusterBuilder {
	b.cluster.Spec.TLSEnabled = true
	return b
}

func (b ClusterBuilder) WithImage(image string) ClusterBuilder {
	b.cluster.Spec.Image.Name = image
	return b
}

func (b ClusterBuilder) WithLabels(labels map[string]string) ClusterBuilder {
	b.cluster.Spec.AdditionalLabels = labels
	return b
}

func (b ClusterBuilder) WithClusterLogging(logConfigMap string) ClusterBuilder {
	b.cluster.Spec.LogConfigMap = logConfigMap
	return b
}

func (b ClusterBuilder) WithAnnotations(annotations map[string]string) ClusterBuilder {
	b.cluster.Spec.AdditionalAnnotations = annotations
	return b
}

func (b ClusterBuilder) WithIngress(ingress *api.IngressConfig) ClusterBuilder {
	b.cluster.Spec.Ingress = ingress
	return b
}

func (b ClusterBuilder) WithResources(resources corev1.ResourceRequirements) ClusterBuilder {
	b.cluster.Spec.Resources = resources
	return b
}

func (b ClusterBuilder) WithPriorityClass(priorityClass string) ClusterBuilder {
	b.cluster.Spec.PriorityClassName = priorityClass
	return b
}

func (b ClusterBuilder) Cr() *api.CrdbCluster {
	cluster := b.cluster.DeepCopy()

	return cluster
}
