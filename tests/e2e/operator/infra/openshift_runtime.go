package infra

import (
	"fmt"
	"testing"

	"github.com/cockroachdb/helm-charts/tests/e2e/operator"
	"github.com/gruntwork-io/terratest/modules/k8s"
	"github.com/stretchr/testify/require"
)

const (
	openShiftChartCloudProvider   = ProviderGCP
	openShiftDefaultClusterDomain = "cluster.local"
)

type openShiftRuntime struct{}

func (openShiftRuntime) ChartCloudProvider(_ string) string {
	return openShiftChartCloudProvider
}

func (openShiftRuntime) ClusterDomain(region *operator.Region, index int) string {
	if region != nil && len(region.Clusters) > 1 {
		if domain, ok := operator.CustomDomains[index]; ok {
			return domain
		}
	}
	return openShiftDefaultClusterDomain
}

func (openShiftRuntime) PatchHelmValues(values map[string]string) map[string]string {
	if values == nil {
		values = make(map[string]string)
	}
	values["cockroachdb.crdbCluster.dataStore.volumeClaimTemplate.spec.storageClassName"] = openShiftStorageClassName()
	return values
}

func (openShiftRuntime) ConfigureNamespace(
	t *testing.T, kubectlOptions *k8s.KubectlOptions, namespace string,
) {
	bindingName := openShiftAnyUIDBindingName(namespace)
	group := fmt.Sprintf("system:serviceaccounts:%s", namespace)
	manifest := fmt.Sprintf(`apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: %s
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: system:openshift:scc:anyuid
subjects:
- apiGroup: rbac.authorization.k8s.io
  kind: Group
  name: %s
`, bindingName, group)

	require.NoError(t, k8s.KubectlApplyFromStringE(t, kubectlOptions, manifest),
		"failed to apply OpenShift SCC binding %s", bindingName)
}

func (openShiftRuntime) HelmDeleteArgs(args []string) []string {
	deleteArgs := append([]string{}, args...)
	return append(deleteArgs, "--no-hooks")
}

func (openShiftRuntime) CleanupNamespace(
	t *testing.T, kubectlOptions *k8s.KubectlOptions, namespace string,
) {
	bindingName := openShiftAnyUIDBindingName(namespace)
	_ = k8s.RunKubectlE(t, kubectlOptions, "delete", "clusterrolebinding", bindingName, "--ignore-not-found=true")
}

func openShiftAnyUIDBindingName(namespace string) string {
	return fmt.Sprintf("cockroach-anyuid-%s", namespace)
}

var _ operator.ProviderRuntime = openShiftRuntime{}
