package helm_testing

import (
	"path/filepath"
	"testing"

	"github.com/gruntwork-io/terratest/modules/helm"
	"github.com/gruntwork-io/terratest/modules/logger"
	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/apps/v1"
	rbacv1 "k8s.io/api/rbac/v1"
)

func Test_Role(t *testing.T) {
	helmChartPath, err := filepath.Abs("../cockroachdb")
	require.NoError(t, err)
	require.NotNil(t,helmChartPath)

	t.Run("Test TLS Enabled but certs not provided", func(t *testing.T) {
		options := &helm.Options{
			SetValues: map[string]string{
				"tls.enabled": "True",
				"tls.certs.provided": "False",
				"tls.certs.certManager": "False",
			},
			Logger: logger.Discard,
		}

		releaseName := "cockroachdb-test"

		output := helm.RenderTemplate(t, options, helmChartPath, releaseName, []string{"templates/role.yaml"})
		require.NotNil(t, output)

		var role rbacv1.Role
		helm.UnmarshalK8SYaml(t, output, &role)

		resources := role.Rules[0].Resources
		require.Len(t, resources, 1)
		require.Contains(t, resources, "secrets")

		verbs := role.Rules[0].Verbs
		require.Len(t, verbs, 2)
		require.Contains(t, verbs, "get")
		require.Contains(t, verbs, "create")
	})

	t.Run("Test TLS Enabled and certs provided", func(t *testing.T) {
		options := &helm.Options{
			SetValues: map[string]string{
				"tls.enabled": "True",
				"tls.certs.provided": "True",
				"tls.certs.certManager": "False",
			},
			Logger: logger.Discard,
		}

		releaseName := "cockroachdb-test"

		output := helm.RenderTemplate(t, options, helmChartPath, releaseName, []string{"templates/role.yaml"})
		require.NotNil(t, output)

		var role rbacv1.Role
		helm.UnmarshalK8SYaml(t, output, &role)

		resources := role.Rules[0].Resources
		require.Len(t, resources, 1)
		require.Contains(t, resources, "secrets")

		verbs := role.Rules[0].Verbs
		require.Len(t, verbs, 1)
		require.Contains(t, verbs, "get")
	})

	t.Run("Test TLS Disabled", func(t *testing.T) {
		options := &helm.Options{
			SetValues: map[string]string{
				"tls.enabled": "False",
			},
			Logger: logger.Discard,
		}

		releaseName := "cockroachdb-test"

		_, err := helm.RenderTemplateE(t, options, helmChartPath, releaseName, []string{"templates/role.yaml"})
		require.Error(t, err) // Should error out because nothing gets rendered

	})
}

func Test_StatefulSet(t *testing.T) {
	helmChartPath, err := filepath.Abs("../cockroachdb")
	require.NoError(t, err)
	require.NotNil(t,helmChartPath)

	releaseName := "cockroachdb-stateful_set"

	t.Run("Test image credentials supported", func(t *testing.T) {
		options := &helm.Options{
			SetValues: map[string]string{
				"image.credentials.registry": "crdb_creds",
				"image.credentials.user": "user1",
				"image.credentials.password": "pw1234",

			},
			Logger: logger.Discard,
		}


		output := helm.RenderTemplate(t, options, helmChartPath, releaseName, []string{"templates/statefulset.yaml"})
		require.NotNil(t, output)

		statefulSet := v1.StatefulSet{}
		err = helm.UnmarshalK8SYamlE(t, output, &statefulSet)
		require.NoError(t, err)

		templateSpec := statefulSet.Spec.Template.Spec
		require.Equal(t,  "cockroachdb-stateful_set.db.registry", templateSpec.ImagePullSecrets[0].Name)


	})
}