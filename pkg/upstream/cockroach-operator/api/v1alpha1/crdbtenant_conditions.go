package v1alpha1

// TenantConditionType is an enumeration of CrdbTenant conditions, listed below.
// Each condition should start with the "Tenant" prefix and be phrased as a
// state adjective, if possible.
type TenantConditionType string

const (
	// TenantInitialized is set to True once a newly created tenant has been
	// fully initialized and is ready for connections (e.g. the tenant's
	// keyspace has been allocated in the host cluster).
	TenantInitialized TenantConditionType = "Initialized"
)
