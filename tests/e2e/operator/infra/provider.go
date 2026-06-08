package infra

import (
	"os"
	"strings"
	"testing"

	"github.com/cockroachdb/helm-charts/tests/e2e/operator"
	"github.com/cockroachdb/helm-charts/tests/e2e/operator/encryption"
)

// CloudProvider defines the interface that all cloud providers must implement.
// Some methods are optional - providers that don't support certain operations
// can implement them as no-ops with appropriate logging.
type CloudProvider interface {
	// SetUpInfra creates the necessary infrastructure for the tests.
	SetUpInfra(t *testing.T)

	// TeardownInfra cleans up all resources created by SetUpInfra.
	TeardownInfra(t *testing.T)

	// ScaleNodePool scales the node pool in a cluster.
	// Optional: providers that don't support scaling can implement as a no-op.
	ScaleNodePool(t *testing.T, location string, nodeCount, index int)

	// CanScale checks if the provider supports scaling.
	CanScale() bool

	GetEncryptionProvider() encryption.Provider
}

// ResolveProvider reads the PROVIDER env var and returns the matching provider
// constant. Defaults to ProviderK3D if not set.
func ResolveProvider(t *testing.T) string {
	if p := strings.TrimSpace(strings.ToLower(os.Getenv("PROVIDER"))); p != "" {
		switch p {
		case ProviderK3D, ProviderKind, ProviderGCP:
			return p
		default:
			t.Fatalf("Unsupported provider override: %s", p)
		}
	}
	return ProviderK3D
}

// ProviderFactory creates a CloudProvider instance for the given provider type
// and automatically wires the encryption provider into the region so that
// InstallChartsWithAdvancedConfig can use it without any manual setup.
func ProviderFactory(providerType string, region *operator.Region) CloudProvider {
	var cp CloudProvider

	switch providerType {
	case ProviderK3D:
		p := LocalRegion{Region: region, ProviderType: ProviderK3D}
		p.RegionCodes = GetRegionCodes(providerType)
		cp = &p
	case ProviderKind:
		p := LocalRegion{Region: region, ProviderType: ProviderKind}
		p.RegionCodes = GetRegionCodes(providerType)
		cp = &p
	case ProviderGCP:
		p := GcpRegion{Region: region}
		p.RegionCodes = GetRegionCodes(providerType)
		cp = &p
	default:
		return nil
	}

	region.SetEncryptionProvider(cp.GetEncryptionProvider())
	return cp
}
