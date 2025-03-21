package v1alpha1

import (
	"github.com/cockroachdb/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	// TenantClusterNameLabel is the name of the label on the CrdbTenant
	// resource that stores the tenant cluster name.
	// NOTE: The cluster name should not be used as the CrdbTenant name, since
	// K8s resource names cannot be changed (and we want to be able to rename
	// clusters in the future). We should consider making this a strongly-typed
	// field on CrdbTenantSpec, but it's unclear if that's what we want in the
	// longer-term, so keep it a label for now.
	TenantClusterNameLabel = "cluster-name"
	// DefaultTenantPoolName is the name that tenants' .Spec.TenantPool
	// will default to.
	DefaultTenantPoolName = "default"
)

// This file defines a CrdbTenant custom resource definition (CRD) object.
// NOTE: json tags are required.  Any new fields you add must have json tags for
// the fields to be serialized.

// CrdbTenantKind is the CrdbTenant CRD kind string.
const CrdbTenantKind = "CrdbTenant"

// CrdbTenantSpec defines the desired state of CrdbTenant.
type CrdbTenantSpec struct {
	// INSERT ADDITIONAL SPEC FIELDS - desired state of cluster
	// Important: Run "make" to regenerate code after modifying this file

	// TenantID is the unique identifier of the tenant within its host cluster.
	// The caller is responsible for ensuring that different tenants within the
	// host clusters each have a different TenantID.
	// NOTE: Two tenants from different host clusters may have the same ID.
	// +kubebuilder:validation:Required
	TenantID TenantID `json:"tenantID"`

	// Pods is the number of SQL tenant pods that should be stamped and running
	// for this tenant. Pods must always be <= MaxPods.
	// +kubebuilder:validation:Required
	Pods int32 `json:"pods"`

	// MaxPods is the maximum number of SQL tenant pods that can be stamped and
	// running for this tenant. If nil, then the value of TenantPool.MaxPods is
	// used. If the tenant pool is not available, or does not define MaxPods,
	// then no max limit will be enforced.
	// NOTE: The autoscaler will never set Pods > MaxPods, even if load is high.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Optional
	MaxPods *int32 `json:"maxPods,omitempty"`

	// TenantPool is the name of the TenantPool, defined by the host cluster,
	// that tenant will run in. Defaults to "default".
	// +kubebuilder:default=default
	// +kubebuilder:validation:Required
	TenantPool *string `json:"tenantPool,omitempty"`

	// DisableAutoscale prohibits the autoscaler from updating the Pods field.
	// This is useful to set in tests or when the autoscaler is not wanted for
	// this tenant.
	// +kubebuilder:validation:Optional
	DisableAutoscale bool `json:"disableAutoscale,omitempty"`

	// AllowedCIDRRanges specifies which IP address ranges are allowed to access
	// a cluster. An empty list means no public connections are allowed.
	// We purposefully choose to leave off the omitempty to ensure that empty
	// slices can be treated differently from nil slices by the tenant directory.
	// +kubebuilder:validation:Optional
	AllowedCIDRRanges []string `json:"allowedCIDRRanges"`

	// AllowedPrivateEndpoints specifies the list of private endpoints
	// identifiers for private connectivity. An empty list means no private
	// connections are allowed.
	// +kubebuilder:validation:Optional
	AllowedPrivateEndpoints []string `json:"allowedPrivateEndpoints,omitempty"`

	// OtelCollector, if defined, specifies the configuration for the
	// OpenTelemetry Collector process that runs as a sidecar within each SQL
	// pod.
	// +kubebuilder:validation:Optional
	OtelCollector *OtelCollector `json:"otelCollector,omitempty"`

	// Fluentbit, if defined, specifies the configuration for the Fluentbit
	// process that runs as a sidecar within each SQL pod. When not defined,
	// the operator will use a default noop configuration for the Fluentbit
	// process.
	// +kubebuilder:validation:Optional
	Fluentbit *Fluentbit `json:"fluentbit,omitempty"`
}

// OtelCollector defines the desired runtime attributes for the OpenTelemetry
// Collector sidecar.
type OtelCollector struct {
	// ConfigMapName is the name of the ConfigMap object containing config for
	// the OpenTelemetry Collector process.
	// +kubebuilder:validation:Required
	ConfigMapName string `json:"configMapName"`

	// SecretName is the name of the Secret object containing secrets for the
	// OpenTelemetry Collector process. Key-value pairs in the secret will be
	// sent to the agent sidecar container as files ending with .env
	// (e.g. DD_API_KEY.env), and injected as environment variables.
	// +kubebuilder:validation:Optional
	SecretName string `json:"secretName,omitempty"`

	// AWSMetricsRoleARN is the name of the IAM resource that the operator will need
	// to generate credentials for and supply to the OpenTelemetry Collector
	// process when exporting metrics to a tenant's AWS CloudWatch sink.
	// +kubebuilder:validation:Optional
	AWSMetricsRoleARN string `json:"awsMetricsRoleArn,omitempty"`

	// AWSLogsRoleARN is the name of the IAM resource that the operator will need
	// to generate credentials for and supply to the OpenTelemetry Collector
	// process when exporting logs to a tenant's AWS CloudWatch sink.
	// +kubebuilder:validation:Optional
	AWSLogsRoleARN string `json:"awsLogsRoleArn,omitempty"`

	// AWSExternalID is an identifier that the operator will use when generating
	// credentials for AWSRoleARN. This field must be set for IAM roles that
	// enforce who can assume such a role via external IDs. One benefit to doing
	// so is that this addresses the confused deputy problem in the case of a
	// multi-tenant AWS account.
	// See: https://docs.aws.amazon.com/IAM/latest/UserGuide/confused-deputy.html
	// +kubebuilder:validation:Optional
	AWSExternalID string `json:"awsExternalID,omitempty"`

	// GCPLogsServiceAccount is the name of the GCP service account that the operator
	// will need to generate credentials for and supply to the OpenTelemetry Collector
	// process when exporting logs to a tenant's GCP cloud logging sink.
	// +kubebuilder:validation:Optional
	GCPLogsServiceAccount string `json:"gcpLogsServiceAccount,omitempty"`
}

// Fluentbit defines the desired runtime attributes for the Fluentbit sidecar.
type Fluentbit struct {
	// ConfigMapName is the name of the ConfigMap object containing config for
	// the FluentBit process.
	// +kubebuilder:validation:Required
	ConfigMapName string `json:"configMapName"`
}

// CrdbTenantStatus defines the observed state of CrdbTenant.
type CrdbTenantStatus struct {
	// INSERT ADDITIONAL STATUS FIELD - define observed state of cluster
	// Important: Run "make" to regenerate code after modifying this file

	// Pods is the number of SQL tenant pods that are currently stamped and
	// running for this tenant. Note that pods in this list may be unhealthy;
	// there is a delay between a pod failing and that being reflected in this
	// status.
	Pods int32 `json:"pods"`

	// Conditions are the set of current status indicators for the tenant. For
	// example, once a tenant has been fully initialized, a TenantInitialized
	// condition will be added to the list and set to True.
	Conditions []TenantCondition `json:"conditions,omitempty"`
}

// HasCondition returns true if a condition of the given type exists in the
// tenant condition list and has the given status value.
func (s *CrdbTenantStatus) HasCondition(typ TenantConditionType, st metav1.ConditionStatus) bool {
	for i := range s.Conditions {
		if s.Conditions[i].Type == typ {
			return s.Conditions[i].Status == st
		}
	}
	return false
}

// TenantCondition describes the current state of some aspect of the tenant's
// status. The operator will add these to the Conditions list as it completes
// work.
// NOTE: Conditions should always be able to be reconstructed by the operator
// based on observations it makes about the state of the tenant; these
// conditions only exist to speed up reconciliation and to provide additional
// observability to the user.
type TenantCondition struct {
	// Type is the kind of this condition.
	// +kubebuilder:validation:Required
	Type TenantConditionType `json:"type"`
	// Status is the current state of the condition: True, False or Unknown.
	// +kubebuilder:validation:Required
	Status metav1.ConditionStatus `json:"status"`
	// LastTransitionTime is the time at which the condition was last updated.
	// +kubebuilder:validation:Required
	LastTransitionTime metav1.Time `json:"lastTransitionTime"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status

// CrdbTenant is the schema for the crdbtenants API.
// NOTE: Don't add new fields to this struct. Instead, add fields describing
// the desired state of the CrdbTenant to CrdbTenantSpec, and fields describing
// the observed state of the CrdbTenant to CrdbTenantStatus.
type CrdbTenant struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   CrdbTenantSpec   `json:"spec,omitempty"`
	Status CrdbTenantStatus `json:"status,omitempty"`
}

// IsReady returns nil once it has been initialized and the number of
// running pods associated with the tenant equals the desired number of pods.
func (tenant *CrdbTenant) IsReady() error {
	if tenant.Spec.Pods != tenant.Status.Pods {
		return errors.Newf("only %d/%d pods are ready", tenant.Status.Pods, tenant.Spec.Pods)
	}
	if !tenant.Status.HasCondition(TenantInitialized, metav1.ConditionTrue) {
		return errors.Newf("tenant is not initialized")
	}
	return nil
}

// +kubebuilder:object:root=true

// CrdbTenantList contains a list of CrdbTenant.
type CrdbTenantList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []CrdbTenant `json:"items"`
}
