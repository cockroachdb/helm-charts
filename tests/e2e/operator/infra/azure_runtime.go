package infra

import (
	"testing"

	"github.com/cockroachdb/helm-charts/tests/e2e/operator"
	"github.com/gruntwork-io/terratest/modules/k8s"
)

const azureDefaultClusterDomain = "cluster.local"

type azureRuntime struct{}

func (azureRuntime) ChartCloudProvider(provider string) string {
	return provider
}

func (azureRuntime) ClusterDomain(_ *operator.Region, index int) string {
	if domain, ok := operator.CustomDomains[index]; ok {
		return domain
	}
	return azureDefaultClusterDomain
}

func (azureRuntime) PatchHelmValues(values map[string]string) map[string]string {
	if values == nil {
		values = make(map[string]string)
	}
	values["cockroachdb.crdbCluster.podTemplate.spec.terminationGracePeriodSeconds"] = "30"
	return values
}

func (azureRuntime) ConfigureNamespace(_ *testing.T, _ *k8s.KubectlOptions, _ string) {
}

func (azureRuntime) HelmDeleteArgs(args []string) []string {
	return args
}

func (azureRuntime) CleanupNamespace(_ *testing.T, _ *k8s.KubectlOptions, _ string) {
}

var _ operator.ProviderRuntime = azureRuntime{}
