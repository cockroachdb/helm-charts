package calico

import (
	"bufio"
	"embed"
	"fmt"
	"regexp"
	"strings"

	"github.com/cockroachdb/errors"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apiextv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apiextv1beta1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
)

// NOTE: This package contains all the necessary
// things to deploy calico objects on k8s clusters.

// This is particularly needed for our multi-region
// e2e tests where we set up multiple k3d clusters.

// By deploying calico CNI, we will enable cross cluster
// communication among k3d clusters.

// We decode all the k8s objects from yaml/calico-cni.yaml
// and apply on the cluster with other CR's.

// Todo(nishanth): We will directly import this code, once
// we move operator code into a separate repo.

// K3dClusterBGPConfig describes the bgp config of a cluster.
type K3dClusterBGPConfig struct {
	// AddressAllocation determines the pod cidr, service cidr, asNumber
	// as follows:
	// PodCIDR: 10.<alloc>.0.0/17
	// ServiceCIDR: 10.<alloc>.128.0/17
	// ASNumber: 64512 + <alloc>
	AddressAllocation int
	PeeringNodes      []string
}

//go:embed yaml/*
var yamls embed.FS
var scheme *runtime.Scheme
var codecFactory serializer.CodecFactory

func init() {
	scheme = runtime.NewScheme()
	_ = apiextv1.AddToScheme(scheme)
	_ = apiextv1beta1.AddToScheme(scheme)
	_ = clientgoscheme.AddToScheme(scheme)

	codecFactory = serializer.NewCodecFactory(scheme)
}

type Object interface {
	runtime.Object
	metav1.Object
}

// GetASNumber returns the AS number for the cluster.
func (c *K3dClusterBGPConfig) GetASNumber() uint32 {
	return uint32(64512 + c.AddressAllocation)
}

// GetPodCIDR returns the pod cidr for the cluster.
func (c *K3dClusterBGPConfig) GetPodCIDR() string {
	return fmt.Sprintf("10.%d.0.0/17", c.AddressAllocation)
}

// GetServiceCIDR returns the service cidr for the cluster.
func (c *K3dClusterBGPConfig) GetServiceCIDR() string {
	return fmt.Sprintf("10.%d.128.0/17", c.AddressAllocation)
}

// K3DCalicoCNI returns objects that can be used for installing calico
// as the CNI plugin for the k8s cluster.
// This is *only* intended for use with k3d environment for local development
// or testing.
func K3DCalicoCNI(opts K3dClusterBGPConfig) []Object {
	bgpConfig := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "crd.projectcalico.org/v1",
			"kind":       "BGPConfiguration",
			"metadata":   map[string]interface{}{"name": "default"},
			"spec": map[string]interface{}{
				"logSeverityScreen":     "Info",
				"nodeToNodeMeshEnabled": true,
				"asNumber":              opts.GetASNumber(),
				"serviceClusterIPs": []map[string]string{
					{
						"cidr": opts.GetServiceCIDR(),
					},
				},
			},
		},
	}
	// Force legacy iptables backend because the nftables backend does not play well with different versions of
	// iptables which could be present on the host and the k3s container.
	// Users should ensure that the host system is also using legacy iptables, you can generally do this with
	// `update-alternatives` or similar.
	felixConfig := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "crd.projectcalico.org/v1",
			"kind":       "FelixConfiguration",
			"metadata":   map[string]interface{}{"name": "default"},
			"spec": map[string]interface{}{
				"iptablesBackend": "Legacy",
			},
		},
	}
	objects := DecodeYAML("yaml/calico-cni.yaml")
	for _, obj := range objects {
		if dms, ok := obj.(*appsv1.DaemonSet); ok && dms.Name == "calico-node" {
			for i := range dms.Spec.Template.Spec.Containers {
				if dms.Spec.Template.Spec.Containers[i].Name == "calico-node" {
					dms.Spec.Template.Spec.Containers[i].Env = append(dms.Spec.Template.Spec.
						Containers[i].Env,
						corev1.EnvVar{
							Name:  "CALICO_IPV4POOL_CIDR",
							Value: opts.GetPodCIDR(),
						},
						corev1.EnvVar{
							// This setting allows traffic from any external IP address.
							// We don't technically need to allow the whole  IP space, but it's a little involved to get the set of peering IPs to this function call.
							// This is only intended for integration tests, or local development, so this should not be a security concern.
							// We could also potentially add networkpolicies to mitigate.
							Name:  "FELIX_EXTERNALNODESCIDRLIST",
							Value: "0.0.0.0/1,128.0.0.0/1",
						})
				}
			}
		}
	}
	return append(objects, felixConfig, bgpConfig)
}

// RegisterCalicoGVK is a helper method for adding calico objects returned
// by this package to the scheme.
func RegisterCalicoGVK(scheme *runtime.Scheme) {
	// We only register the kinds that we actually need here.
	scheme.AddKnownTypeWithName(schema.GroupVersionKind{
		Group:   "crd.projectcalico.org",
		Version: "v1",
		Kind:    "BGPConfiguration",
	}, &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(schema.GroupVersionKind{
		Group:   "crd.projectcalico.org",
		Version: "v1",
		Kind:    "IPPool",
	}, &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(schema.GroupVersionKind{
		Group:   "crd.projectcalico.org",
		Version: "v1",
		Kind:    "BGPPeer",
	}, &unstructured.Unstructured{})
}

// K3dCalicoBGPPeeringOptions determines how to set up bpg peering among
// several k3d clusters.
type K3dCalicoBGPPeeringOptions struct {
	ClusterConfig map[string]K3dClusterBGPConfig
}

// K3dCalicoBGPPeeringObjects returns the objects needed for connecting
// k3d clusters via BGP.
func K3dCalicoBGPPeeringObjects(opts K3dCalicoBGPPeeringOptions) map[string][]Object {

	objectsByRegion := make(map[string][]Object, len(opts.ClusterConfig))
	for region := range opts.ClusterConfig {
		var objects []Object
		for otherRegion, otherCluster := range opts.ClusterConfig {
			if otherRegion == region {
				continue
			}
			serviceIPPool := map[string]interface{}{
				"apiVersion": "crd.projectcalico.org/v1",
				"kind":       "IPPool",
				"metadata": map[string]interface{}{
					"name": fmt.Sprintf("%s-services", otherRegion),
				},
				"spec": map[string]interface{}{
					"cidr":        otherCluster.GetServiceCIDR(),
					"ipipMode":    "Always",
					"natOutgoing": false,
					"disabled":    true,
				},
			}
			podIPPool := map[string]interface{}{
				"apiVersion": "crd.projectcalico.org/v1",
				"kind":       "IPPool",
				"metadata": map[string]interface{}{
					"name": fmt.Sprintf("%s-pods", otherRegion),
				},
				"spec": map[string]interface{}{
					"cidr":        otherCluster.GetPodCIDR(),
					"ipipMode":    "Always",
					"natOutgoing": false,
					"disabled":    true,
				},
			}
			objects = append(objects,
				&unstructured.Unstructured{Object: serviceIPPool},
				&unstructured.Unstructured{Object: podIPPool})

			for i, addr := range otherCluster.PeeringNodes {
				bgpPeer := map[string]interface{}{
					"apiVersion": "crd.projectcalico.org/v1",
					"kind":       "BGPPeer",
					"metadata": map[string]interface{}{
						"name": fmt.Sprintf("edge-peer-%s-%d", otherRegion, i),
					},
					"spec": map[string]interface{}{
						"nodeSelector": "edge == 'true'",
						"peerIP":       addr,
						"asNumber":     otherCluster.GetASNumber(),
					},
				}
				objects = append(objects, &unstructured.Unstructured{Object: bgpPeer})
			}
		}
		objectsByRegion[region] = objects
	}
	return objectsByRegion
}

// DecodeYAML returns decoded Kubernetes YAML manifests from the internal/yaml
// directory. If asset cannot be found or if asset is invalid YAML, DecodeYAML
// panics.
func DecodeYAML(asset string) []Object {
	raw, err := yamls.ReadFile(asset)
	if err != nil {
		panic(fmt.Errorf("could not load YAML asset '%s': %w", asset, err))
	}

	objs, err := decode(raw)
	if err != nil {
		panic(fmt.Errorf("could not decode YAML asset '%s': %w", asset, err))
	}

	return objs
}

func decode(data []byte) ([]Object, error) {
	decode := codecFactory.UniversalDeserializer().Decode

	// NOTE: this could likely be implemented much more efficiently with
	// FindAllIndex or something of that nature.
	splitter := regexp.MustCompile("(^|\n)---($|\n)")

	i := 0
	spl := splitter.Split(string(data), -1)
	objects := make([]Object, len(spl))

	for _, doc := range spl {
		// Skip blocks that are only composed of blank lines and/or comments.
		hasContent := false
		scanner := bufio.NewScanner(strings.NewReader(doc))
		for scanner.Scan() {
			s := strings.TrimSpace(scanner.Text())
			if s != "" && !strings.HasPrefix(s, "#") {
				hasContent = true
				break
			}
		}
		if !hasContent {
			continue
		}

		object, _, err := decode([]byte(doc), nil, nil)
		if err != nil {
			return nil, err
		}

		objects[i], err = FromRuntime(object)
		if err != nil {
			return nil, err
		}
		i++
	}

	return objects[:i], nil
}

// FromRuntime converts a runtime.Object into an Object. This may fail if the
// input object is an ObjectList.
func FromRuntime(object runtime.Object) (Object, error) {
	result, ok := object.(Object)
	if ok {
		return result, nil
	}
	return nil, errors.AssertionFailedf("expected a kube.Object and found: %+v", object)
}
