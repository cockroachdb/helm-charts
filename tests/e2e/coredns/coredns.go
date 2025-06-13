package coredns

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	v1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/yaml"
)

// NOTE: This package contains all the necessary
// things to deploy coredns objects on k8s clusters.

// A reference yaml is placed under `yaml/coredns.yaml`.

// Instead of decoding `yaml/coredns.yaml` objects and applying
// on k8s cluster, we modify coredns config to whitelist
// service requests with custom domain and also forward
// requests to other clusters.

// Todo(nishanth): We will directly import this code, once
// we move operator code into a separate repo.

// CoreDNSServiceAccount returns a ServiceAccount for CoreDNS.
func CoreDNSServiceAccount() *corev1.ServiceAccount {
	return &corev1.ServiceAccount{
		TypeMeta: metav1.TypeMeta{
			Kind:       "ServiceAccount",
			APIVersion: "v1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "coredns",
			Namespace: "kube-system",
		},
	}
}

// CoreDNSClusterRole returns a ClusterRole for CoreDNS with necessary permissions.
func CoreDNSClusterRole() *v1.ClusterRole {
	return &v1.ClusterRole{
		TypeMeta: metav1.TypeMeta{
			Kind:       "ClusterRole",
			APIVersion: "rbac.authorization.k8s.io/v1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: "system:coredns",
			Labels: map[string]string{
				"kubernetes.io/bootstrapping": "rbac-defaults",
			},
		},
		Rules: []v1.PolicyRule{
			{
				APIGroups: []string{""},
				Resources: []string{"services", "endpoints", "pods", "namespaces", "nodes"},
				Verbs:     []string{"get", "list", "watch"},
			},
			{
				APIGroups: []string{"discovery.k8s.io"},
				Resources: []string{"endpointslices"},
				Verbs:     []string{"get", "list", "watch"},
			},
		},
	}
}

// CoreDNSClusterRoleBinding returns a ClusterRoleBinding that binds the CoreDNS ServiceAccount to the CoreDNS ClusterRole.
func CoreDNSClusterRoleBinding() *v1.ClusterRoleBinding {
	return &v1.ClusterRoleBinding{
		TypeMeta: metav1.TypeMeta{
			Kind:       "ClusterRoleBinding",
			APIVersion: "rbac.authorization.k8s.io/v1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: "system:coredns",
			Labels: map[string]string{
				"kubernetes.io/bootstrapping": "rbac-defaults",
			},
		},
		Subjects: []v1.Subject{
			{
				Kind:      "ServiceAccount",
				Name:      "coredns",
				Namespace: "kube-system",
			},
		},
		RoleRef: v1.RoleRef{
			APIGroup: "rbac.authorization.k8s.io",
			Kind:     "ClusterRole",
			Name:     "system:coredns",
		},
	}
}

// CoreDNSService returns coredns service object.
func CoreDNSService(IpAddress *string, annotations map[string]string) *corev1.Service {
	// Create a copy of the annotations to avoid modifying the original map.
	serviceAnnotations := make(map[string]string)
	for k, v := range annotations {
		serviceAnnotations[k] = v
	}

	svc := &corev1.Service{
		TypeMeta: metav1.TypeMeta{
			Kind:       "Service",
			APIVersion: "v1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Labels: map[string]string{
				"k8s-app": "kube-dns",
			},
			Name:        "crl-core-dns",
			Namespace:   "kube-system",
			Annotations: serviceAnnotations,
		},
		Spec: corev1.ServiceSpec{
			Type: corev1.ServiceTypeLoadBalancer,
			Ports: []corev1.ServicePort{
				{
					Name:       "dns-tcp",
					Port:       53,
					Protocol:   "TCP",
					TargetPort: intstr.Parse("53"),
				},
			},
			Selector: map[string]string{
				"k8s-app": "kube-dns",
			},
		},
	}

	// Set LoadBalancerIP field for backward compatibility and as fallback.
	// For GCP internal LoadBalancers, we rely on annotations, but still set LoadBalancerIP as fallback.
	// For other providers or external LoadBalancers, LoadBalancerIP might be the primary method.
	if IpAddress != nil {
		svc.Spec.LoadBalancerIP = *IpAddress
	}

	return svc
}

// CoreDNSDeployment returns coredns deployment object.
func CoreDNSDeployment(replicas int32) *appsv1.Deployment {
	healthCheckPort := intstr.FromInt32(8080)
	maxSurge := intstr.FromInt32(1)
	readinessPort := intstr.FromInt32(8181)

	memoryRequest := resource.MustParse("70Mi")
	cpuRequest := resource.MustParse("100m")

	// templateLables are the labels applied to the created pods and the labels
	// used to target pods by the deployment.
	templateLabels := map[string]string{
		"k8s-app": "kube-dns",
	}

	return &appsv1.Deployment{
		TypeMeta: metav1.TypeMeta{
			Kind:       "Deployment",
			APIVersion: "apps/v1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: "coredns",
			Labels: map[string]string{
				// The label `k8s-app: kube-dns` is intentional - this allows CoreDNS
				// to be picked up by the existing kube-dns service, which has a
				// selector on this particular label.
				"k8s-app":            "kube-dns",
				"kubernetes.io/name": "CoreDNS",
			},
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: proto.Int32(replicas),
			Strategy: appsv1.DeploymentStrategy{
				Type: appsv1.RollingUpdateDeploymentStrategyType,
				RollingUpdate: &appsv1.RollingUpdateDeployment{
					MaxSurge: &maxSurge,
				},
			},
			Selector: &metav1.LabelSelector{
				MatchLabels: templateLabels,
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels:      templateLabels,
					Annotations: map[string]string{},
				},
				Spec: corev1.PodSpec{
					SecurityContext: &corev1.PodSecurityContext{
						SeccompProfile: &corev1.SeccompProfile{Type: "RuntimeDefault"},
					},
					ServiceAccountName: "coredns",
					Affinity: &corev1.Affinity{
						PodAntiAffinity: &corev1.PodAntiAffinity{
							PreferredDuringSchedulingIgnoredDuringExecution: []corev1.WeightedPodAffinityTerm{
								{
									Weight: 100,
									PodAffinityTerm: corev1.PodAffinityTerm{
										LabelSelector: &metav1.LabelSelector{
											MatchLabels: templateLabels,
										},
										TopologyKey: "kubernetes.io/hostname",
									},
								},
							},
						},
					},
					Tolerations: []corev1.Toleration{
						{
							Key:      "CriticalAddonsOnly",
							Operator: "Exists",
						},
					},
					Containers: []corev1.Container{
						{
							Name:            "coredns",
							Image:           "coredns/coredns:1.9.2",
							ImagePullPolicy: corev1.PullIfNotPresent,
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{
									corev1.ResourceMemory: memoryRequest,
									corev1.ResourceCPU:    cpuRequest,
								},
							},
							Args: []string{
								"-conf", "/etc/coredns/Corefile",
							},
							VolumeMounts: []corev1.VolumeMount{
								{
									Name:      "config-volume",
									MountPath: "/etc/coredns",
									ReadOnly:  true,
								},
							},
							Ports: []corev1.ContainerPort{
								{
									ContainerPort: 53,
									Name:          "dns",
									Protocol:      corev1.ProtocolUDP,
								},
								{
									ContainerPort: 53,
									Name:          "dns-tcp",
									Protocol:      corev1.ProtocolTCP,
								},
								{
									ContainerPort: 9153,
									Name:          "metrics",
									Protocol:      corev1.ProtocolTCP,
								},
							},
							SecurityContext: &corev1.SecurityContext{
								AllowPrivilegeEscalation: proto.Bool(false),
								Capabilities: &corev1.Capabilities{
									Add: []corev1.Capability{
										"NET_BIND_SERVICE",
									},
									Drop: []corev1.Capability{
										"all",
									},
								},
								ReadOnlyRootFilesystem: proto.Bool(true),
							},
							LivenessProbe: &corev1.Probe{
								ProbeHandler: corev1.ProbeHandler{
									HTTPGet: &corev1.HTTPGetAction{
										Path:   "/health",
										Port:   healthCheckPort,
										Scheme: corev1.URISchemeHTTP,
									},
								},
								InitialDelaySeconds: 60,
								TimeoutSeconds:      3,
								SuccessThreshold:    1,
								FailureThreshold:    5,
							},
							ReadinessProbe: &corev1.Probe{
								ProbeHandler: corev1.ProbeHandler{
									HTTPGet: &corev1.HTTPGetAction{
										Path:   "/ready",
										Port:   readinessPort,
										Scheme: corev1.URISchemeHTTP,
									},
								},
							},
						},
					},
					DNSPolicy: corev1.DNSDefault,
					Volumes: []corev1.Volume{
						{
							Name: "config-volume",
							VolumeSource: corev1.VolumeSource{
								ConfigMap: &corev1.ConfigMapVolumeSource{
									LocalObjectReference: corev1.LocalObjectReference{
										Name: "coredns",
									},
									Items: []corev1.KeyToPath{
										{
											Key:  "Corefile",
											Path: "Corefile",
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
}

// CoreDNSClusterOption defines configuration options for CoreDNS in a multi-cluster environment.
// It contains information needed for cross-cluster DNS resolution.
type CoreDNSClusterOption struct {
	// Namespace is the namespace where other clusters can resolve
	// services by querying <service-name>.<namespace>.<svc>.cluster.local.
	Namespace string
	// Domain is the cluster domain where other clusters can resolve
	// by querying <service-name>.<any-namespace>.<svc>.<domain>,
	// as if the cluster has a custom domain, even though
	// technically, the cluster is provisioned with default cluster domain
	// cluster.local.
	// // to the new domain.
	Domain string
	// IPs is a list of routable IPs pointing to CoreDNS load balancers. This
	// will be used to configure the Corefile to forward matching requests
	// based on the suffix of DNS names to the specified instances. The result
	// is called DNS Chaining, which allows pods in cluster A to resolve the
	// DNS names matching the defined rules to IPs in cluster B.
	//
	// NOTE: Hostnames MUST be resolved to IPs as CoreDNS does not support
	// resolving the hostnames of chained instances as of Feb 2021. This is a
	// CoreDNS limitation, and while issue has been raised many times, the
	// developers always say no as it would introduce a circular dependency on
	// CoreDNS itself.
	IPs []string
}

// CoreDNSConfigMap will have all the rules required to forward
// service requests of other cluster domains.
func CoreDNSConfigMap(thisDomain string, allClusters map[string]CoreDNSClusterOption) *corev1.ConfigMap {
	var data strings.Builder
	dataMap := map[string]string{}

	// Our addition to the typical default server configuration.
	quotedDomain := regexp.QuoteMeta(thisDomain)
	rewriteStr := fmt.Sprintf(`rewrite continue {
		name regex ^(.+)\.%s\.?$ {1}.cluster.local
		answer name ^(.+)\.cluster\.local\.?$ {1}.%s
		answer value ^(.+)\.cluster\.local\.?$ {1}.%s
	}`, quotedDomain, thisDomain, thisDomain)

	// We provide the rewrite string to Azure as a ".override" map entry.
	// We provide it to AWS and GCP by directly constructing the entire
	// default server string.
	dataMap["rewrite.override"] = rewriteStr
	// "It returns the length of s and a nil error, or else it gets the
	// hose again"
	// - https://golang.org/pkg/strings/#Builder.WriteString
	_, _ = data.WriteString(`.:53 {
	errors
	ready
	health
	` + rewriteStr + `
	kubernetes cluster.local ` + thisDomain + ` in-addr.arpa ip6.arpa {
		pods insecure
		fallthrough in-addr.arpa ip6.arpa
	}
	prometheus :9153
	forward . /etc/resolv.conf
	cache 30
	loop
	reload
	loadbalance
}
`)

	i := 0
	clusters := make([]string, len(allClusters))
	for clusterDomain := range allClusters {
		clusters[i] = clusterDomain
		i++
	}

	// Sort our cluster domains to ensure our output is stable and deterministic.
	sort.Strings(clusters)

	// Construct a list of forwarding rules for remote CoreDNS servers.
	for _, clusterDomain := range clusters {
		// Skip the current kubernetes cluster.
		if clusterDomain == thisDomain {
			continue
		}

		cluster := allClusters[clusterDomain]
		if len(cluster.IPs) == 0 {
			panic(fmt.Errorf("cluster %s has no IPs", clusterDomain))
		}

		// Sort IPs to ensure determinism.
		sort.Strings(cluster.IPs)

		// For a given remote cluster, configure the local CoreDNS with the IP
		// address(es) that point to the remote cluster's CoreDNS load balancer.
		// The local CoreDNS service will then forward any requests matching the
		// format *.<remote-cluster>.svc.cluster.local or
		// *.monitoring-<remote-cluster>.svc.cluster.local to the remote
		// cluster's DNS service, which will resolve the DNS name to an IP
		// address and return it.
		//
		// force_tcp was originally included because AWS NLBs did
		// not support UDP traffic. Now that they support UDP, experiment with
		// removing this flag.
		ipAddresses := strings.Join(cluster.IPs, " ")

		serverBlockStr := fmt.Sprintf(`{
	log
	errors
	ready
	cache 30
	forward . %s {
		force_tcp
	}
}`, ipAddresses)

		// The server values which will eventually find their way into the
		// Corefile.
		namespace := cluster.Namespace
		var servers []string
		if namespace != "" {
			servers = append(
				servers,
				fmt.Sprintf("%s.svc.%s", namespace, clusterDomain),
				fmt.Sprintf("%s.pod.%s", namespace, clusterDomain),
			)
		}
		servers = append(servers, clusterDomain)

		for _, server := range servers {
			serverStr := fmt.Sprintf("%s:53 %s\n", server, serverBlockStr)
			// We provide the server configuration to Azure as a ".server" map
			// entry.
			// We provide the server configuration to AWS and GCP as a direct
			// entry in the Corefile string.
			dataMap[fmt.Sprintf("%s.server", server)] = serverStr
			// "It returns the length of s and a nil error, or else it gets the
			// hose again"
			// - https://golang.org/pkg/strings/#Builder.WriteString
			_, _ = data.WriteString(serverStr)
		}
	}

	configMapData := map[string]string{
		"Corefile": data.String(),
	}

	return &corev1.ConfigMap{
		TypeMeta: metav1.TypeMeta{
			Kind:       "ConfigMap",
			APIVersion: "v1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "coredns",
			Namespace: "kube-system",
		},
		Data: configMapData,
	}
}

// ToYAML converts a Kubernetes runtime.Object (Service, Deployment, or ConfigMap) to YAML string.
func ToYAML(t *testing.T, obj runtime.Object) string {
	// Create a new scheme.
	scheme := runtime.NewScheme()

	// Add known types based on the object type.
	switch obj.(type) {
	case *corev1.Service:
		scheme.AddKnownTypes(corev1.SchemeGroupVersion, &corev1.Service{})
	case *appsv1.Deployment:
		scheme.AddKnownTypes(corev1.SchemeGroupVersion, &appsv1.Deployment{})
	case *corev1.ConfigMap:
		scheme.AddKnownTypes(corev1.SchemeGroupVersion, &corev1.ConfigMap{})
	case *corev1.ServiceAccount:
		scheme.AddKnownTypes(corev1.SchemeGroupVersion, &corev1.ServiceAccount{})
	case *v1.ClusterRole:
		scheme.AddKnownTypes(v1.SchemeGroupVersion, &v1.ClusterRole{})
	case *v1.ClusterRoleBinding:
		scheme.AddKnownTypes(v1.SchemeGroupVersion, &v1.ClusterRoleBinding{})
	default:
		t.Fatalf("Unsupported object type: %T", obj)
	}

	// Convert the object to bytes
	yamlBytes, err := yaml.Marshal(obj)
	if err != nil {
		t.Fatalf("Failed to marshal object to YAML: %v", err)
	}

	return string(yamlBytes)
}
