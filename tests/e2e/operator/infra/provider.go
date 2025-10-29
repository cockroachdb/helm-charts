package infra

import (
	"testing"

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
	default:
		return nil
	}
}
