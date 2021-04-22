package helm_testing

import (
	"path/filepath"
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"

	"github.com/stretchr/testify/require"
	"github.com/gruntwork-io/terratest/modules/helm"
	"github.com/gruntwork-io/terratest/modules/k8s"
)

func Test_Role(t *testing.T) {
	helmChartPath, err := filepath.Abs("../cockroachdb")
	require.NoError(t, err)
	require.NotNil(t,helmChartPath)

	options := &helm.Options{
		SetValues: map[string]string{
			"tls.enabled": "True",
		},
		KubectlOptions: k8s.NewKubectlOptions("", "", "test-deployment"),
	}

	releaseName := "cockroachdb-test"

	output := helm.RenderTemplate(t, options, helmChartPath, releaseName, []string{"templates/role.yaml"})
	require.NotNil(t, output)

	var role rbacv1.Role
	helm.UnmarshalK8SYaml(t, output, &role)

	verbs := role.Rules[0].Verbs

	require.Len(t, verbs, 2)
	require.Contains(t, verbs, "get")
	require.Contains(t, verbs, "create")
}
