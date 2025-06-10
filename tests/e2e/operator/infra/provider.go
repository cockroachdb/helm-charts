package infra

import (
	"testing"

	"github.com/cockroachdb/helm-charts/tests/e2e/operator"
)

// CloudProvider defines the interface that all cloud providers must implement
type CloudProvider interface {
	// SetUpInfra creates the necessary infrastructure for the tests
	SetUpInfra(t *testing.T)
}

// CloudProviderWithTeardown extends CloudProvider with teardown capability
type CloudProviderWithTeardown interface {
	CloudProvider
	// TeardownInfra cleans up all resources created by SetUpInfra
	TeardownInfra(t *testing.T)
}

// CloudProviderWithScaling extends CloudProvider with scaling capability
type CloudProviderWithScaling interface {
	CloudProvider
	// ScaleNodePool scales the node pool in a cluster
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
func CanTeardown(provider CloudProvider) (CloudProviderWithTeardown, bool) {
	if p, ok := provider.(CloudProviderWithTeardown); ok {
		return p, true
	}
	return nil, false
}

// CanScale checks if the provider supports scaling
func CanScale(provider CloudProvider) (CloudProviderWithScaling, bool) {
	if p, ok := provider.(CloudProviderWithScaling); ok {
		return p, true
	}
	return nil, false
}
