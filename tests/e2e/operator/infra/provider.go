package infra

import (
	"testing"

	"github.com/gruntwork-io/terratest/modules/k8s"

	"github.com/cockroachdb/helm-charts/tests/e2e/operator"
)

// CloudProvider defines the interface that all cloud providers must implement
// Some methods are optional - providers that don't support certain operations
// can implement them as no-ops with appropriate logging.
type CloudProvider interface {
	// SetUpInfra creates the necessary infrastructure for the tests
	SetUpInfra(t *testing.T)

	// TeardownInfra cleans up all resources created by SetUpInfra
	TeardownInfra(t *testing.T)

	// ScaleNodePool scales the node pool in a cluster
	// Optional: providers that don't support scaling/ if auto-scaling is enabled can implement as no-op
	ScaleNodePool(t *testing.T, location string, nodeCount, index int)

	// CanScale checks if the provider supports scaling.
	CanScale() bool

	// ─── Encryption At Rest Methods ─────────────────────────────────────────────

	// SetupEncryptionInfrastructure creates cloud KMS resources (keys, roles, policies)
	// Returns a cleanup function that should be deferred to ensure proper resource cleanup
	// Called once during test setup, before any encryption secrets are created
	// For AWS: Creates KMS keys in each region, IAM roles with decrypt permissions
	// For GCP: Creates KMS keys, service accounts with permissions
	// For Local: No-op, returns empty cleanup function
	SetupEncryptionInfrastructure(t *testing.T) (cleanup func(), err error)

	// GetEncryptionPlatformConfig returns provider-specific encryption platform configuration
	// Defines platform type (AWS_KMS, GCP_CLOUD_KMS, UNKNOWN_KEY_TYPE), whether credentials are needed, etc.
	GetEncryptionPlatformConfig() operator.EncryptionPlatformConfig

	// EncryptStoreKey encrypts the plaintext store key using provider's KMS
	// Called for each cluster/region that needs encryption
	// For AWS: Encrypts with AWS KMS Encrypt API, returns base64-encoded ciphertext
	// For GCP: Encrypts with GCP KMS Encrypt API, returns base64-encoded ciphertext
	// For Local: Returns base64-encoded plaintext (no encryption)
	// clusterRegion identifies which cluster/region this key is for (maps to KMS key)
	EncryptStoreKey(t *testing.T, plaintextKey []byte, clusterRegion string) (encryptedKeyBase64 string, err error)

	// CreateEncryptionKeySecret creates Kubernetes secret with provider-specific fields
	// For AWS: StoreKeyData (encrypted), AuthPrincipal, URI, Region, Type, ExternalID
	// For GCP: StoreKeyData (encrypted), AuthPrincipal, URI, Region, Type
	// For Local: StoreKeyData (base64 plaintext)
	// encryptedKeyData is the base64-encoded result from EncryptStoreKey
	CreateEncryptionKeySecret(
		t *testing.T,
		kubectlOptions *k8s.KubectlOptions,
		secretName string,
		encryptedKeyData string,
		clusterRegion string,
	)

	// CreateEncryptionCredentialsSecret creates Kubernetes secret with KMS access credentials
	// For AWS: aws_access_key_id, aws_secret_access_key
	// For GCP: gcp_service_account_key (JSON)
	// For Local: No-op (returns "")
	// Returns secret name if created, empty string otherwise
	CreateEncryptionCredentialsSecret(t *testing.T, kubectlOptions *k8s.KubectlOptions) string
}

// ProviderFactory creates a CloudProvider instance for the given provider type.
func ProviderFactory(providerType string, region *operator.Region) CloudProvider {
	switch providerType {
	case ProviderK3D:
		provider := LocalRegion{Region: region, ProviderType: ProviderK3D}
		provider.RegionCodes = GetRegionCodes(providerType)
		return &provider
	case ProviderKind:
		provider := LocalRegion{Region: region, ProviderType: ProviderKind}
		provider.RegionCodes = GetRegionCodes(providerType)
		return &provider
	case ProviderGCP:
		provider := GcpRegion{Region: region}
		provider.RegionCodes = GetRegionCodes(providerType)
		return &provider
	case ProviderAWS:
		provider := AwsRegion{Region: region}
		provider.RegionCodes = GetRegionCodes(providerType)
		return &provider
	default:
		return nil
	}
}
