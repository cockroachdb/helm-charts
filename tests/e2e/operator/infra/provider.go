package infra

import (
	"testing"

	"github.com/cockroachdb/helm-charts/tests/e2e/operator"
	"github.com/cockroachdb/helm-charts/tests/e2e/operator/encryption"
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

	// GetEncryptionProvider returns an encryption.Provider implementation for this cloud provider.
	// The provider can return itself since CloudProvider implementations also implement encryption.Provider.
	// All encryption-related operations are accessed through the returned encryption.Provider interface.
	GetEncryptionProvider() encryption.Provider
}

// ProviderFactory creates a CloudProvider instance for the given provider type.
// It also automatically sets the encryption provider on the region so it's available for advanced installs.
func ProviderFactory(providerType string, region *operator.Region) CloudProvider {
	var cloudProvider CloudProvider

	switch providerType {
	case ProviderK3D:
		provider := LocalRegion{Region: region, ProviderType: ProviderK3D}
		provider.RegionCodes = GetRegionCodes(providerType)
		cloudProvider = &provider
	case ProviderKind:
		provider := LocalRegion{Region: region, ProviderType: ProviderKind}
		provider.RegionCodes = GetRegionCodes(providerType)
		cloudProvider = &provider
	case ProviderGCP:
		provider := GcpRegion{Region: region}
		provider.RegionCodes = GetRegionCodes(providerType)
		cloudProvider = &provider
	default:
		return nil
	}

	// Automatically set the encryption provider on the region
	// This makes it available for encryption-enabled advanced installs
	region.SetEncryptionProvider(cloudProvider.GetEncryptionProvider())

	return cloudProvider
}
