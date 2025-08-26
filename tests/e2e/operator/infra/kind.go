package infra

import (
	"testing"

	"github.com/cockroachdb/helm-charts/tests/e2e/operator"
)

type Provider interface {
	SetUpInfra(t *testing.T)
	TeardownInfra(t *testing.T)
	ScaleNodePool(t *testing.T, location string, nodeCount, index int)
}

// KindRegion implements CloudProvider for Kind
type KindRegion struct {
	*operator.Region
}

// SetUpInfra Creates Kind clusters, deploy calico CNI, deploy coredns in each cluster.
func (r *KindRegion) SetUpInfra(t *testing.T) {
	t.Logf("[%s] Kind setup not fully implemented", ProviderKind)
}

// TeardownInfra cleans up all resources created by SetUpInfra
func (r *KindRegion) TeardownInfra(t *testing.T) {
	t.Logf("[%s] Kind teardown not implemented - clusters will be cleaned up by the test framework", ProviderKind)
}

// ScaleNodePool scales the node pool in a Kind cluster
func (r *KindRegion) ScaleNodePool(t *testing.T, location string, nodeCount, index int) {
	t.Logf("[%s] Kind scaling not implemented - Kind doesn't support scaling node pools", ProviderKind)
}

/*
	Try running everything in a different go routine.
	Follow the same API's, struct for each infra.
	Each infra should just have the additional details needed.
*/
