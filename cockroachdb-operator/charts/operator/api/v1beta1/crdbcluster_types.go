package v1beta1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ReconciliationMode describes the modus operandi of the CrdbCluster
// controller when reconciling CrdbNodes.
type ReconciliationMode string

// This file defines a CrdbCluster custom resource definition (CRD) object.
// NOTE: json tags are required.  Any new fields you add must have json tags for
// the fields to be serialized.
const (
	// CrdbClusterKind is the CrdbCluster CRD kind string.
	CrdbClusterKind = "CrdbCluster"

	// LocalPathStorageClass refers to the Rancher local-path-provisioner that
	// creates persistent volumes that utilize the local storage in each node.
	// See https://github.com/rancher/local-path-provisioner.
	// This is used for testing only.
	LocalPathStorageClass = "local-path"

	// MutableOnly ReconciliationMode reconciles mutable fields of all
	// CrdbNodes. New CrdbNodes will be created and CrdbNodes will be
	// decommissioned. MutableOnly is the default ReconciliationMode if one is
	// not specified.
	MutableOnly ReconciliationMode = "MutableOnly"

	// CreateOnly ReconciliationMode disables reconciliation for existing
	// CrdbNodes. New CrbdNodes nodes will be created inline with
	// CrdbNodeTemplate. CrdbNodes will not be decommissioned.
	CreateOnly ReconciliationMode = "CreateOnly"

	// Disabled ReconciliationMode disables reconciliation of CrdbNodes.
	// Changes to the CrdbNodeTemplate will not be propagated, new CrdbNodes
	// will not be created, and CrdbNodes will not be decommissioned.
	Disabled ReconciliationMode = "Disabled"
)

// CrdbNodeTemplate is the template from which CrdbNodes will be created or
// reconciled towards.
type CrdbNodeTemplate struct {
	// ObjectMeta is a set of metadata that will be propagated to CrdbNodes.
	// Only Labels and Annotations will be respected.
	metav1.ObjectMeta `json:"metadata,omitempty"`
	// Spec is used as the template to construct or reconcile the Spec of
	// CrdbNodes, depending on the ReconciliationMode of the CrdbCluster.
	// +kubebuilder:validation:Required
	Spec CrdbNodeSpec `json:"spec,omitempty"`
}

// PostInitSQL specifies SQL statements to execute once after the cluster has
// been initialized. Statements run in the order: secretRef contents, then
// configMapRef contents, then inline. Any failure flips the
// PostInitSQLApplied condition to False with the error in Reason/Message.
// Users are responsible for writing idempotent SQL (IF NOT EXISTS,
// ON CONFLICT DO NOTHING) because all statements replay from the beginning
// after a fix.
type PostInitSQL struct {
	// Inline is an ordered list of SQL statements to execute.
	Inline []string `json:"inline,omitempty"`

	// ConfigMapRef references a ConfigMap key whose value is a SQL script.
	ConfigMapRef *corev1.ConfigMapKeySelector `json:"configMapRef,omitempty"`

	// SecretRef references a Secret key whose value is a SQL script.
	SecretRef *corev1.SecretKeySelector `json:"secretRef,omitempty"`
}

// CrdbClusterSpec defines the desired state of CrdbCluster.
// NOTE: Run "make" to regenerate code after modifying this file.
// TODO(chrisseto): Add backward compatibility for all betaclusterctrl fields
// by installing a mutating webhook.
type CrdbClusterSpec struct {
	// Mode sets the modus operandi of the CrdbCluster controller when
	// reconciling CrdbNodes.
	// NOTE: Mode is only respected by the betaclusterctrl and is therefore
	// optional.
	// +kubebuilder:validation:Optional
	// +kubebuilder:default=MutableOnly
	// +kubebuilder:validation:Enum=MutableOnly;CreateOnly;Disabled
	Mode *ReconciliationMode `json:"mode,omitempty"`

	// Template is the object that describes the CrdbNodes that will be created
	// and reconciled by the CrdbCluster controller, depending on the
	// ReconciliationMode.
	// NOTE: Template is only respected by the betaclusterctrl and is therefore
	// optional.
	// +kubebuilder:validation:Optional
	Template CrdbNodeTemplate `json:"template"`

	// ClusterSettings is a set of CockroachDB CLUSTER SETTINGS that will be
	// set by the CrdbCluster controller via executing SET CLUSTER SETTING.
	// NOTE: ClusterSettings is only respected by the betaclusterctrl and is
	// therefore optional.
	// +kubebuilder:validation:Optional
	ClusterSettings map[string]string `json:"clusterSettings,omitempty"`

	// PostInitSQL specifies SQL statements to run once after cluster
	// initialization. See PostInitSQL for execution semantics.
	PostInitSQL *PostInitSQL `json:"postInitSQL,omitempty"`

	// Regions specifies the regions in which this cluster is deployed, along
	// with information about how each region is configured.
	// +kubebuilder:validation:Required
	Regions []CrdbClusterRegion `json:"regions"`

	// TLSEnabled indicates whether the cluster is running in secure mode.
	// API. See https://github.com/cockroachdb/cockroach-operator/issues/291.
	// +kubebuilder:validation:Optional
	TLSEnabled bool `json:"tlsEnabled,omitempty"`

	// Features specifies the enabled ClusterFeatures for this cluster.
	// +kubebuilder:validation:Optional
	//Features []ClusterFeature `json:"features,omitempty"`

	// RollingRestartDelay is the delay between node restarts during a rolling
	// update. Defaults to 1 minute.
	// +kubebuilder:validation:Optional
	// +kubebuilder:default="1m"
	RollingRestartDelay *metav1.Duration `json:"rollingRestartDelay,omitempty"`

	// IsClusterDisrupted specifies if this cluster is intentionally crippled
	// or not. Functionality is handled by Intrusion and this value is
	// informative more than anything.
	// +kubebuilder:validation:Optional
	IsClusterDisrupted bool `json:"isClusterDisrupted,omitempty"`
}

// CrdbClusterRegion describes a region in which CRDB cluster nodes operate. It
// is used to generate the --join flag passed to each CrdbNode within the
// cluster.
type CrdbClusterRegion struct {
	// Code corresponds to the cloud provider's identifier of this region (e.g.
	// "us-east-1" for AWS, "us-east1" for GCP). This value is used to detect
	// which CrdbClusterRegion will be reconciled and must match the
	// "topology.kubernetes.io/region" label on Kubernetes Nodes in this
	// cluster.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength:=1
	Code string `json:"code"`

	// Nodes is the number of CRDB nodes that are in the region.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Minimum:=0
	Nodes int32 `json:"nodes"`

	// CloudProvider sets the cloud provider for this region. When set, this value
	// is used to prefix the locality flag for all nodes in the region.
	// +kubebuilder:validation:Optional
	CloudProvider string `json:"cloudProvider,omitempty"`

	// Namespace is the name of the Kubernetes namespace that this
	// CrdbClusterRegion is deployed within. It is used to compute the --join
	// flag for this region. Defaults to the .Code of this region and then the
	// Namespace of this CrdbCluster, if not provided.
	// +kubebuilder:validation:Optional
	Namespace string `json:"namespace,omitempty"`

	// Domain is the domain of the CrdbClusterRegion.
	// Other regions need to reach this region by connecting to
	// <cluster-name>.<namespace>.svc.<domain>.
	// It defaults an empty string, but this will not work
	// in a multi-region setup, where CrdbCluster objects are potentially
	// in different namespaces.
	// It will also not work if the k8s cluster has a custom domain.
	Domain string `json:"domain,omitempty"`

	// EncryptionAtRest contains all secret names and keys for EAR encryption.
	// +kubebuilder:validation:Optional
	EncryptionAtRest *EncryptionAtRest `json:"encryptionAtRest,omitempty"`
}

// EncryptionAtRest contains secrets for managing Encryption at Rest.
type EncryptionAtRest struct {
	// KeySecretName is the name of the k8s secret containing the (new)
	// store key. If nil, this will be interpreted as "plain" i.e.
	// unencrypted.
	// +kubebuilder:validation:Optional
	KeySecretName *string `json:"keySecretName,omitempty"`

	// Platform is the cloud platform whose KMS is used to gate the
	// new Customer-Managed Encryption Key (CMEK). This string value can
	// be mapped to CMEKKeyType with the CMEKKeyType_value map.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Enum=UNKNOWN_KEY_TYPE;AWS_KMS;GCP_CLOUD_KMS;AZURE_KEY_VAULT
	Platform EncryptionPlatform `json:"platform"`

	// CMEKCredentialsSecretName is the name of the k8s secret containing
	// our credentials that are needed to authenticate into the customer's
	// KMS. This value is required if Platform is non-zero.
	// +kubebuilder:validation:Optional
	CMEKCredentialsSecretName *string `json:"cmekCredentialsSecretName,omitempty"`

	// OldKeySecretName is the name of the k8s secret containing the old
	// store key. If nil, this will be interpreted as "plain" i.e. unencrypted.
	// +kubebuilder:validation:Optional
	OldKeySecretName *string `json:"oldKeySecretName,omitempty"`

	// OldPlatform is the cloud platform whose KMS was used to encrypt the
	// old store key. Only needed when the old key uses a different KMS
	// provider than the current Platform or when disabling EAR entirely. If
	// unset, defaults to Platform.
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:Enum=UNKNOWN_KEY_TYPE;AWS_KMS;GCP_CLOUD_KMS;AZURE_KEY_VAULT
	OldPlatform EncryptionPlatform `json:"oldPlatform,omitempty"`

	// OldCMEKCredentialsSecretName is the name of the k8s secret containing
	// credentials for the old KMS. Only needed when switching cloud KMS
	// providers or disabling EAR. If unset, defaults to
	// CMEKCredentialsSecretName.
	// +kubebuilder:validation:Optional
	OldCMEKCredentialsSecretName *string `json:"oldCmekCredentialsSecretName,omitempty"`
}

type EncryptionPlatform string

const (
	EncryptionPlatformUnknown       = EncryptionPlatform("UNKNOWN_KEY_TYPE")
	EncryptionPlatformAwsKms        = EncryptionPlatform("AWS_KMS")
	EncryptionPlatformGcpCloudKms   = EncryptionPlatform("GCP_CLOUD_KMS")
	EncryptionPlatformAzureKeyVault = EncryptionPlatform("AZURE_KEY_VAULT")
)

// ResolvedOldPlatform returns the KMS platform for the old store key. If
// OldPlatform is explicitly set, it is used. Otherwise it falls back to
// Platform so same-provider key rotations do not need extra fields.
func (e *EncryptionAtRest) ResolvedOldPlatform() EncryptionPlatform {
	if e.OldPlatform != "" {
		return e.OldPlatform
	}
	return e.Platform
}

// ResolvedOldCMEKCredentialsSecretName returns the credentials secret for the
// old KMS. It falls back to CMEKCredentialsSecretName when not explicitly set.
func (e *EncryptionAtRest) ResolvedOldCMEKCredentialsSecretName() *string {
	if e.OldCMEKCredentialsSecretName != nil {
		return e.OldCMEKCredentialsSecretName
	}
	return e.CMEKCredentialsSecretName
}

func ParseEncryptionPlatform(name string) EncryptionPlatform {
	switch name {
	case string(EncryptionPlatformAwsKms):
		return EncryptionPlatformAwsKms
	case string(EncryptionPlatformGcpCloudKms):
		return EncryptionPlatformGcpCloudKms
	case string(EncryptionPlatformAzureKeyVault):
		return EncryptionPlatformAzureKeyVault
	default:
		return EncryptionPlatformUnknown
	}
}

func (p EncryptionPlatform) String() string {
	return string(p)
}

// CrdbClusterStatus defines the observed state of CrdbCluster.
// NOTE: Run "make" to regenerate code after modifying this file
type CrdbClusterStatus struct {
	// ObservedGeneration is the value of the ObjectMeta.Generation last
	// reconciled by the controller.
	// Note(alyshan): ObjectMeta.Generation uses int64, so we match the type.
	ObservedGeneration int64 `json:"observedGeneration"`

	// Actions are the set of operations taken on this cluster.
	Actions []ClusterAction `json:"actions,omitempty"`

	// Conditions are the set of current status indicators for the cluster.
	Conditions []ClusterCondition `json:"conditions,omitempty"`

	// Settings contains the cluster settings for the CRDB cluster.
	Settings map[string]string `json:"settings,omitempty"`

	// ReadyNodes is the number of nodes that are ready in this region.
	ReadyNodes int32 `json:"readyNodes,omitempty"`

	// Reconciled indicates whether the spec of ObservedGeneration is reconciled.
	Reconciled bool `json:"reconciled,omitempty"`

	// ReconciledByBetaController is true if the cluster is reconciled by
	// the beta cluster controller.
	ReconciledByBetaController bool `json:"reconciledByBetaController,omitempty"`

	// Provider is the name of the cloud provider that this object's k8s server is in.
	Provider string `json:"provider,omitempty"`

	// Region is the name of the region that this crdbcluster object's k8s server is in.
	// This is useful for consumers to determine if this region's crdb pods
	// are ready, etc..
	Region string `json:"region,omitempty"`

	// CurrentRevision is the fingerprint of the revision this CrdbCluster
	// believes to be current.
	CurrentRevision string `json:"currentRevision,omitempty"`

	// PreviousRevision is the fingerprint of the last revision that was
	// successfully rolled out to this CrdbCluster's CrdbNodes.
	PreviousRevision string `json:"previousRevision,omitempty"`

	// Image is the CockroachDB image currently running in this cluster.
	// +kubebuilder:validation:Optional
	Image string `json:"image,omitempty"`

	// Version is the version of CockroachDB currently running in this cluster.
	// This is populated by specifing the version where version is the output of executing
	// `cockroach version` command on running pods.
	// +kubebuilder:validation:Optional
	Version string `json:"version,omitempty"`
}

// ClusterCondition describes the current state of some aspect of the cluster's
// status. The operator will add these to the Conditions list as it completes
// work.
// NOTE: Conditions should always be able to be reconstructed by the operator
// based on observations it makes about the state of the cluster; these
// conditions only exist to speed up reconciliation and to provide additional
// observability to the user.
type ClusterCondition struct {
	// Type is the kind of this condition.
	// +kubebuilder:validation:Required
	//Type ClusterConditionType `json:"type"`
	// Status is the current state of the condition: True, False or Unknown.
	// +kubebuilder:validation:Required
	Status metav1.ConditionStatus `json:"status"`
	// LastTransitionTime is the time at which the condition was last updated.
	// +kubebuilder:validation:Required
	LastTransitionTime metav1.Time `json:"lastTransitionTime"`
}

// ClusterAction describes an operation performed by the operator on the
// CrdbCluster.
type ClusterAction struct {
	// Type is the kind of this action.
	// +kubebuilder:validation:Required
	Type ActionType `json:"type"`
	// Status is the current state of the action: Starting, Failed, Finished or Unknown.
	// +kubebuilder:validation:Required
	Status ActionStatus `json:"status"`
	// LastTransitionTime is the time at which the condition was last updated.
	// +kubebuilder:validation:Required
	LastTransitionTime metav1.Time `json:"lastTransitionTime"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:storageversion

// CrdbCluster is the Schema for the crdbclusters API.
// NOTE: Don't add new fields to this struct. Instead, add fields describing
// the desired state of the CrdbCluster to CrdbClusterSpec, and fields
// describing the observed state of the CrdbCluster to CrdbClusterStatus.
type CrdbCluster struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   CrdbClusterSpec   `json:"spec,omitempty"`
	Status CrdbClusterStatus `json:"status,omitempty"`
}

// CrdbClusterList contains a list of CrdbCluster.
type CrdbClusterList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []CrdbCluster `json:"items"`
}

type CrdbWalFailoverStatus string

const (
	WalEnable  CrdbWalFailoverStatus = "enable"
	WalDisable CrdbWalFailoverStatus = "disable"
	WalNotSet  CrdbWalFailoverStatus = ""
)

type CrdbWalFailoverSpec struct {
	Name             string                `json:"name,omitempty"`
	Size             string                `json:"size"`
	StorageClassName string                `json:"storageClassName,omitempty"`
	Status           CrdbWalFailoverStatus `json:"status"`
	Path             string                `json:"path,omitempty"`
}
