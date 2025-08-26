package v1alpha1

import (
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type NodeConditionType string

// NodeDecommissionState enumerates the possible decommissioning states that a
// node can be in.
//
// There are three states to decommissioning:
// 1. CRDB is actively transferring range replicas to other nodes.
// 2. CRDB is finished transferring range replicas, but is awaiting a finalization step.
// 3. The finalization step has occurred.
type NodeDecommissionState string

const (
	// CrdbNodeKind is the CrdbNode CRD kind string.
	CrdbNodeKind = "CrdbNode"

	// CrdbNodeDecommissionAnnotation is an annotation indicating that the node
	// should be prioritized when picking a node to decommission.
	CrdbNodeDecommissionAnnotation = "crdb.cockroachlabs.com/decommission"

	CrdbNodeStartDecommissionAnnotation = "internal.crdb.cockroachlabs.com/decommission"

	// PodReady is a duplicate of Kubernetes' Pod's Ready Condition but
	// reported from the CrdbNode controller. If true, it implies that
	// PodRunning is also true.
	PodReady NodeConditionType = "PodReady"

	// PodReady is a duplicate of Kubernetes' Pod's Running Condition but
	// reported from the CrdbNode controller.
	PodRunning NodeConditionType = "PodRunning"

	// PodNeedsUpdate indicates that the nodectrl has determined that the
	// underlying Pod of this CrdbNode should be updated to a new PodSpec.
	PodNeedsUpdate NodeConditionType = "PodNeedsUpdate"
)

// Decommissioning related constants.
const (
	// Draining indicates the node has started to drain SQL connections and
	// leases.
	Draining NodeDecommissionState = "draining"

	// Drained indicates the node has successfully drained SQL connections
	// and leases.
	Drained NodeDecommissionState = "drained"

	// TransferringReplicas is the NodeDecommissionState where the CRDB node is
	// still transferring replicas to other nodes.
	TransferringReplicas NodeDecommissionState = "transferringReplicas"

	// ZeroReplicas is the NodeDecommissionState where the CRDB node is finished
	// transferring replicas to other nodes, but is awaiting a finalization
	// step.
	ZeroReplicas NodeDecommissionState = "zeroReplicas"

	// Decommissioned is the NodeDecommissionState where the CRDB node is fully
	// decommissioned.
	Decommissioned NodeDecommissionState = "decommissioned"
)

// CrdbNodeSpec defines the desired state of an individual
// CockroachDB node process.
type CrdbNodeSpec struct {
	// PodAnnotations are the annotations that should be applied to the
	// underlying CRDB pod.
	// Deprecated: use `PodTemplate` instead.
	PodAnnotations map[string]string `json:"podAnnotations,omitempty"`

	// PodLabels are the labels that should be applied to the underlying CRDB
	// pod.
	// Deprecated: use `PodTemplate` instead.
	PodLabels map[string]string `json:"podLabels,omitempty"`

	// Image is the location and name of the CockroachDB container image to
	// deploy (e.g. cockroachdb/cockroach:v20.1.1).
	// +kubebuilder:validation:Required
	Image string `json:"image,omitempty"`

	// InitContainerImage is the location and name of the CockroachDB init
	// container to deploy. Defaults to [manifests.defaultInitContainerImage].
	// +kubebuilder:validation:Optional
	InitContainerImage string `json:"initContainerImage,omitempty"`

	// Env is a list of environment variables to set in the container.
	// +kubebuilder:validation:Optional
	// Deprecated: use `PodTemplate` instead.
	Env []corev1.EnvVar `json:"env,omitempty"`

	// DataStore specifies the disk configuration for this CrdbNode.
	// +kubebuilder:validation:Required
	DataStore DataStore `json:"dataStore,omitempty"`

	// Certificates controls the TLS configuration of this CrdbNode.
	// +kubebuilder:validation:Required
	Certificates Certificates `json:"certificates,omitempty"`

	// ResourceRequirements is the resource requirements for the main
	// crdb container.
	// +kubebuilder:validation:Required
	// Deprecated: use `PodTemplate` instead.
	ResourceRequirements corev1.ResourceRequirements `json:"resourceRequirements,omitempty"`

	// ConditionalResourceRequirements describes resource requirements  that are
	// conditionally determined based on the node's capacity.
	// +kubebuilder:validation:Optional
	ConditionalResourceRequirements *ConditionalResourceRequirements `json:"conditionalResources,omitempty"`

	// ServiceAccountName is the name of a service account to use for
	// running pods.
	// +kubebuilder:validation:Required
	// Deprecated: use `PodTemplate` instead.
	ServiceAccountName string `json:"serviceAccountName,omitempty"`

	// SideCars will be run in the same pod as the crdb process.
	// This can be useful for running containers for fluentbit logging.
	// +kubebuilder:validation:Optional
	//  Deprecated: use `PodTemplate` instead.
	SideCars CrdbNodeSideCars `json:"sideCars,omitempty"`

	// TopologySpreadConstraints will be used to construct the crdb pod's topology spread
	// constraints.
	// Note: any label selectors will be discarded; the operator will instead set
	// the selector to match all pods belonging to the same cluster.
	// +kubebuilder:validation:Optional
	// Deprecated: use `PodTemplate` instead.
	TopologySpreadConstraints []corev1.TopologySpreadConstraint `json:"topologySpreadConstraints,omitempty"`

	// Join will be used for setting the join flag.
	// Defaults to <cluster-name>.<ns>.svc.cluster.local.
	// +kubebuilder:validation:Optional
	Join string `json:"join,omitempty"`

	// Domain is the cluster domain of the Kubernetes cluster this CrdbNode is
	// in. It is used to construct the address advertised by this CrdbNode.
	// Defaults to `cluster.local`.
	// +kubebuilder:validation:Optional
	Domain string `json:"domain,omitempty"`

	// LoggingConfigMapName is the name of a ConfigMap containing logging
	// configuration for CRDB nodes. If not specified, INFO logs will be
	// written to stderr for the health and dev channels.
	// +kubebuilder:validation:Optional
	LoggingConfigMapName string `json:"loggingConfigMapName,omitempty"`

	// LoggingConfigVars specifies a list of environment variable names
	// that will be expanded if present in the body of the
	// logging configuration.
	// Corresponding ENV values should be set via the `Env` field or `PodTemplate`.
	// +kubebuilder:validation:Optional
	LoggingConfigVars []string `json:"loggingConfigVars,omitempty"`

	// Flags specify the flags that will be used for starting the cluster.
	// Deprecated: use `StartFlags` instead.
	// +kubebuilder:validation:Optional
	Flags map[string]string `json:"flags,omitempty"`

	// StartFlags specify the flags that will be used for starting the cluster.
	// Any flag defined in the StartFlags will take precedence over the first-class
	// fields responsible for setting the same flags.
	// +kubebuilder:validation:Optional
	StartFlags *Flags `json:"startFlags,omitempty"`

	// EncryptionAtRest contains all the information that will be needed
	// for EAR setup at CrdbNode pod initialization. This value will be updated
	// given an update to the corresponding CrdbClusterRegion's EncryptionAtRest
	// attribute.
	// TODO(orchestration): currently this field is populated internally
	// through the CrdbClusterRegion struct. Given that both CrdbClusterRegion
	// and CrdbNode template can change this field, precedence needs to be
	// determined on which source of information to use.
	// +kubebuilder:validation:Optional
	EncryptionAtRest *EncryptionAtRest `json:"encryptionAtRest,omitempty"`

	// NoCloudPrefixedLocalities indicates whether this node's locality flags
	// are prefixed with its cloud infra's short code. See
	// IntrusionCrdbCluster.NoCloudPrefixedLocalities for more info. It is part
	// of CrdbNodeSpec as opposed to CrdbClusterSpec since the init container
	// is spawned from CrdbNodeSpec. This field is expected to be removed once
	// all clusters are using a value of `false`.
	NoCloudPrefixedLocalities bool `json:"noCloudPrefixedLocalities,omitempty"`

	// LocalityLabels specifies a list of kubernetes node labels
	// to read and add to the CockroachDB locality string.
	// Deprecated: use `LocalityMappings` instead.
	// +kubebuilder:validation:Optional
	LocalityLabels []string `json:"localityLabels,omitempty"`

	// LocalityMappings specifies a list of mappings from k8s node labels to crdb
	// locality labels. In order to set the `--locality` flag for the `cockroach
	// start` command, we may need information that's available at runtime, but
	// not available when the crdb pod is scheduled. We use an init continer to
	// look up node labels at runtime, then write the contents of the
	// `--locality` flag to disk. Then we `cat` the locality file as an argument
	// to `cockroach start`. By default, map the standard
	// "topology.kubernetes.io/region" and "topology.kubernetes.io/zone" node
	// labels to "region" and "zone", respectively. For details, see
	// https://www.cockroachlabs.com/docs/stable/cockroach-start#locality.
	// +kubebuilder:validation:Optional
	// +kubebuilder:default={{nodeLabel: "topology.kubernetes.io/region", localityLabel: "region"}, {nodeLabel: "topology.kubernetes.io/zone", localityLabel: "zone"}}
	LocalityMappings []LocalityMapping `json:"localityMappings,omitempty"`

	// TerminationGracePeriodSeconds determines the time available to CRDB for graceful drain.
	// It defaults to 5m.
	// See documentation for [manifests.CRDBTerminationGracePeriodSeconds].
	// +kubebuilder:validation:Optional
	// Deprecated: use `PodTemplate` instead.
	TerminationGracePeriod *metav1.Duration `json:"terminationGracePeriod,omitempty"`

	// NodeName is the name of the kubernetes corev1.Node the CrdbNode is
	// placed on. If "", then the CrdbNode is placed on a node by the
	// kubernetes scheduler.
	NodeName string `json:"nodeName,omitempty"`

	// WALFailover indicates whether we are attaching a new PVC to the node to be used
	// for WAL writes while the data dir disk encounters a stall or increased latency
	WALFailoverSpec *CrdbWalFailoverSpec `json:"walFailoverSpec,omitempty"`

	// Tolerations is the set of tolerations to apply to a node.
	// https://kubernetes.io/docs/concepts/scheduling-eviction/taint-and-toleration/
	// Deprecated: use `PodTemplate` instead.
	Tolerations []corev1.Toleration `json:"tolerations,omitempty"`

	// NodeSelector is the set of nodeSelector labels to apply to a node.
	// https://kubernetes.io/docs/concepts/scheduling-eviction/assign-pod-node/#nodeselector
	// Deprecated: use `PodTemplate` instead.
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`

	// Affinity is the affinity policy to apply to a node.
	// https://kubernetes.io/docs/concepts/scheduling-eviction/assign-pod-node/#affinity-and-anti-affinity
	// Deprecated: use `PodTemplate` instead.
	Affinity *corev1.Affinity `json:"affinity,omitempty"`

	// GRPCPort is the port used for the grpc service. This controls the
	// `--listen-addr` flag to `cockroach start`. If not set, default to 26258.
	// +kubebuilder:validation:Optional
	GRPCPort *int32 `json:"grpcPort,omitempty"`

	// HTTPPort is the port used for the http service. This controls the
	// `--http-addr` flag to `cockroach start`. If not set, default to 8080.
	// +kubebuilder:validation:Optional
	HTTPPort *int32 `json:"httpPort,omitempty"`

	// SQLPort is the port used for the sql service. This controls the
	// `--sql-addr` flag to `cockroach start`. If not set, default to 26257.
	// +kubebuilder:validation:Optional
	SQLPort *int32 `json:"sqlPort,omitempty"`

	// DropChownContainer drops the chown initContainer from the crdb pod. We
	// needed this initContainer during the rollout of the securityContext
	// feature previously, but now that this rollout is complete, we can drop the
	// container.
	// TODO(jmcarp): Drop this flag after it's fully rolled out.
	// +kubebuilder:validation:Optional
	DropChownContainer bool `json:"dropChownContainer,omitempty"`

	// PodTemplate is an optional pod specification that overrides the default pod specification configured by the
	// operator. If specified, PodTemplate is merged with the default pod specification, with settings in
	// PodTemplate taking precedence. This can be used to add or update containers, volumes, and
	// other settings of the cockroachdb pod. List fields are generally merged by name;
	// for details of merging behavior, see the PodTemplateBuilder type.
	// +kubebuilder:validation:Optional
	PodTemplate *PodTemplateSpec `json:"podTemplate,omitempty"`

	// PersistentVolumeClaimRetentionPolicy controls the retention of PVCs when the CrdbNode is deleted.
	// If not specified, defaults to "Delete" for backward compatibility.
	// +kubebuilder:validation:Optional
	PersistentVolumeClaimRetentionPolicy *CrdbNodePersistentVolumeClaimRetentionPolicy `json:"persistentVolumeClaimRetentionPolicy,omitempty"`
}

// PodTemplateSpec is a structure allowing the user to set a template for Pod
// generation.
type PodTemplateSpec struct {
	Metadata PodMeta        `json:"metadata,omitempty"`
	Spec     corev1.PodSpec `json:"spec,omitempty"`
}

type PodMeta struct {
	Labels      map[string]string `json:"labels,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`
}

// CrdbNodePersistentVolumeClaimRetentionPolicy describes the policy for PVCs created from CrdbNode.
type CrdbNodePersistentVolumeClaimRetentionPolicy struct {
	// WhenDeleted specifies what happens to PVCs when the CrdbNode is deleted.
	// +kubebuilder:validation:Optional
	// +kubebuilder:default=Delete
	// +kubebuilder:validation:Enum=Retain;Delete
	WhenDeleted appsv1.PersistentVolumeClaimRetentionPolicyType `json:"whenDeleted,omitempty"`
}

// Flags enable upserting and omitting default flags
// that are passed to the cockroach start command.
type Flags struct {
	// Upsert defines a set of flags that are given higher precedence
	// in the start command.
	// +kubebuilder:validation:Optional
	Upsert []string `json:"upsert"`
	// Omit defines a set of flags which will be omitted
	// from the start command.
	// +kubebuilder:validation:Optional
	Omit []string `json:"omit"`
}

// LocalityMapping represents a mapping between a k8s node label and a crdb
// locality label.
type LocalityMapping struct {
	NodeLabel     string `json:"nodeLabel,omitempty"`
	LocalityLabel string `json:"localityLabel,omitempty"`
}

// ConditionalResourceRequirements describes compute resource requirements for
// the crdb node that are determined based on the k8s node's capacity.
//
// The largest valid quantity is used for each resource. If there are no valid
// capacity requirements for a node's capacity, the traditional resource
// requirements are used.
type ConditionalResourceRequirements struct {
	// Limits contains conditional resource limits.
	Limits map[corev1.ResourceName][]ConditionalQuantity `json:"limits,omitempty"`
	// Requests contains conditional resource requests.
	Requests map[corev1.ResourceName][]ConditionalQuantity `json:"requests,omitempty"`
}

// ConditionalQuantity is a (capacity, quantity) pair. A ConditionalQuantity is
// valid if RequiredCapacity is less than or equal to the node's actual
// capacity.
type ConditionalQuantity struct {
	// RequiredCapacity is the amount of capacity the node must have for the
	// conditional quantity to be valid.
	RequiredCapacity resource.Quantity `json:"requiredCapacity"`
	// Quantity is the amount of resources that should be configured if the
	// conditional quantity is valid.
	Quantity resource.Quantity `json:"quantity"`
}

// CrdbNodeSideCars will be run in the same pod as the crdb process.
// This can be useful for running containers for fluentbit logging.
type CrdbNodeSideCars struct {
	// InitContainers will be run as init containers for the crdb pod.
	// This is mostly used for setting up sidecars, like FluentBit.
	// +kubebuilder:validation:Optional
	InitContainers []corev1.Container `json:"initContainers,omitempty"`

	// Containers will be run in the same pod as the the crdb container.
	// This is mostly used for running sidecars, like FluentBit.
	// +kubebuilder:validation:Optional
	Containers []corev1.Container `json:"containers,omitempty"`

	// Volumes will be requested in addition to the crdb volumes.
	// This is mostly used by the additional containers.
	// +kubebuilder:validation:Optional
	Volumes []corev1.Volume `json:"volumes,omitempty"`
}

// DataStore is the specification for the disk configuration of a CrdbNode.
// Exactly one of VolumeSource or VolumeClaimTemplate must be specified.
type DataStore struct {
	// VolumeSource specifies a pre-existing VolumeSource to be mounted to this
	// CrdbNode.
	// +kubebuilder:validation:Optional
	VolumeSource *corev1.VolumeSource `json:"dataStore,omitempty"`
	// VolumeClaimTemplate specifies a that a new PersistentVolumeClaim should
	// be created for this CrdbNode.
	// +kubebuilder:validation:Optional
	VolumeClaimTemplate *corev1.PersistentVolumeClaim `json:"volumeClaimTemplate,omitempty"`
}

// Certificates controls the TLS configuration of the various services run by
// CockroachDB. Only one of its members may be specified. See
// https://www.cockroachlabs.com/docs/stable/authentication.html for more
// details.
type Certificates struct {
	// ExternalCertificates allows end users to assume full control over the
	// certificates used by CockroachDB.
	ExternalCertificates *ExternalCertificates `json:"externalCertificates,omitempty"`
}

// ExternalCertificates is a certificate configuration that mounts the
// pre-existing and non-operator controlled Kubernetes Secrets into CrdbNode
// Pods for use as TLS on the various services run by CockroachDB.
//
// See https://www.cockroachlabs.com/docs/stable/authentication.html
type ExternalCertificates struct {
	// CAConfigMapName is the name of a Kubernetes ConfigMap containing a ca.crt
	// entry that was used to sign other external certificates. This is used to
	// validate the node and client certificates.
	// https://www.cockroachlabs.com/docs/stable/authentication.html#client-authentication
	// +kubebuilder:validation:Optional
	CAConfigMapName string `json:"caConfigMapName,omitempty"`

	// NodeCAConfigMapName is the name of a Kubernetes ConfigMap containing a
	// ca.crt entry that will be used as the CA for node authentication. Only set
	// if using split CA certificates, which is not recommended:
	// https://www.cockroachlabs.com/docs/stable/authentication.html#using-split-ca-certificates.
	// Exactly one of CAConfigMapName and NodeCAConfigMapName must be set.
	// +kubebuilder:validation:Optional
	NodeCAConfigMapName string `json:"nodeCaConfigMapName,omitempty"`

	// ClientCAConfigMapName is the name of a Kubernetes ConfigMap containing a
	// ca.crt entry that will be used as the CA for client authentication. This
	// is used to validate the client certificates. Only set if using split
	// CA certificates, which is not recommended:
	// https://www.cockroachlabs.com/docs/stable/authentication.html#using-split-ca-certificates.
	// https://www.cockroachlabs.com/docs/stable/authentication.html#client-authentication
	// +kubebuilder:validation:Optional
	ClientCAConfigMapName string `json:"clientCaConfigMapName,omitempty"`

	// HTTPSecretName is the name of a Kubernetes TLS Secret that will be used
	// for the HTTP service.
	// +kubebuilder:validation:Optional
	HTTPSecretName string `json:"httpSecretName,omitempty"`

	// NodeClientSecretName is the name of a Kubernetes TLS secret holding client
	// certificates used when establishing connections to other nodes in the cluster
	// (e.g. joining an existing cluster).
	//
	// The certificate must be signed with the CA identified by CAConfigMapName,
	// or ClientCASecretName if using split CA certificates.
	// +kubebuilder:validation:Optional
	NodeClientSecretName string `json:"nodeClientSecretName,omitempty"`

	// NodeSecretName is the name of a Kubernetes TLS Secret that will be used
	// when receiving incoming connections from other nodes for RPC and SQL calls.
	//
	// The certificate must be signed with the CA identified by CAConfigMapName,
	// or NodeCASecretName if using split CA certificates.
	// +kubebuilder:validation:Required
	NodeSecretName string `json:"nodeSecretName,omitempty"`

	// RootSQLClientSecretName is the name of a Kubernetes TLS secret holding SQL
	// client certificates for the root SQL user. It allows the operator to perform
	// various administrative actions (e.g. set cluster settings).
	//
	// The certificate must be signed with the CA identified by CAConfigMapName,
	// or ClientCASecretName if using split CA certificates.
	// +kubebuilder:validation:Required
	RootSQLClientSecretName string `json:"rootSqlClientSecretName,omitempty"`
}

// CrdbNodeStatus defines the observed state of an individual
// CockroachDB node process.
type CrdbNodeStatus struct {
	// ObservedGeneration is the value of the ObjectMeta.Generation last
	// reconciled by the controller.
	ObservedGeneration int64 `json:"observedGeneration"`

	// Conditions are the set of current status indicators for the node.
	Conditions []NodeCondition `json:"conditions,omitempty"`

	// Decommission indicates the current decommissioning state (if any).
	Decommission NodeDecommissionState `json:"decommission,omitempty"`

	// TopologyValues represents the topological zone that the node is in.
	// It is used to maintain zone balance when we scale down clusters.
	// Currently this slice contains at most 1 element. This is a
	// slice for future expansion.
	TopologyValues []string `json:"topologyValues,omitempty"`
}

type NodeCondition struct {
	// Type is the kind of this condition.
	// +kubebuilder:validation:Required
	Type NodeConditionType `json:"type"`
	// Status is the current state of the condition: True, False or Unknown.
	// +kubebuilder:validation:Required
	Status metav1.ConditionStatus `json:"status"`
	// LastTransitionTime is the time at which the condition was last updated.
	// +kubebuilder:validation:Required
	LastTransitionTime metav1.Time `json:"lastTransitionTime"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status

// CrdbNode is the Schema for the crdbnode API.
// NOTE: Don't add new fields to this struct. Instead, add fields describing
// the desired state of the CrdbNode to CrdbNodeSpec, and fields describing the
// observed state of the CrdbNode to CrdbNodeStatus.
type CrdbNode struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   CrdbNodeSpec   `json:"spec,omitempty"`
	Status CrdbNodeStatus `json:"status,omitempty"`
}
