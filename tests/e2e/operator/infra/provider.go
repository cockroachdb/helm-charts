package infra

import (
	"testing"

	"github.com/cockroachdb/helm-charts/tests/e2e/operator"
)

// CloudProvider defines the interface that all cloud providers must implement
// Some methods are optional - providers that don't support certain operations
// can implement them as no-ops with appropriate logging
type CloudProvider interface {
	// SetUpInfra creates the necessary infrastructure for the tests
	// This is the only required method for all providers
	SetUpInfra(t *testing.T)

	// TeardownInfra cleans up all resources created by SetUpInfra
	// Optional: providers that don't support teardown can implement as no-op
	TeardownInfra(t *testing.T)

	// ScaleNodePool scales the node pool in a cluster
	// Optional: providers that don't support scaling can implement as no-op
	ScaleNodePool(t *testing.T, location string, nodeCount, index int)
}

// ProviderFactory creates a CloudProvider instance for the given provider type
func ProviderFactory(providerType string, region *operator.Region) CloudProvider {
	switch providerType {
	case ProviderK3D:
		provider := K3dRegion{Region: region}
		provider.RegionCodes = GetRegionCodes(providerType)
		return &provider
	case ProviderKind:
		provider := KindRegion{Region: region}
		provider.RegionCodes = GetRegionCodes(providerType)
		return &provider
	case ProviderGCP:
		provider := GcpRegion{Region: region}
		provider.RegionCodes = GetRegionCodes(providerType)
		return &provider
	case ProviderAzure:
		provider := AzureRegion{Region: region}
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

// CanTeardown checks if the provider supports teardown
// This function is kept for backward compatibility
func CanTeardown(provider CloudProvider) (CloudProvider, bool) {
	// Check if the TeardownInfra method is a no-op implementation
	// Kind and K3D providers have no-op implementations
	switch provider.(type) {
	case *KindRegion:
		return nil, false
	default:
		return provider, true
	}
}

// CanScale checks if the provider supports scaling
// This function is kept for backward compatibility
func CanScale(provider CloudProvider) (CloudProvider, bool) {
	// Check if the ScaleNodePool method is a no-op implementation
	// GCP, Kind and K3D providers have no-op implementations
	switch provider.(type) {
	case *K3dRegion, *KindRegion, *GcpRegion:
		return nil, false
	default:
		return provider, true
	}
}
