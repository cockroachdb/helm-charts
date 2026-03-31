package template

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gruntwork-io/terratest/modules/helm"
	"github.com/gruntwork-io/terratest/modules/k8s"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	schedulingv1 "k8s.io/api/scheduling/v1"
	"k8s.io/apimachinery/pkg/util/yaml"
)

var (
	operatorChartPath string
)

func init() {
	var initErr error
	operatorChartPath, initErr = filepath.Abs("../../cockroachdb-parent/charts/operator")
	if initErr != nil {
		panic(initErr)
	}
}

// operatorResources holds all resources rendered from operator.yaml.
// Helm renders them in this order: priority class, service account, cluster role,
// cluster role binding, service, deployment.
type operatorResources struct {
	PriorityClass      schedulingv1.PriorityClass
	ServiceAccount     corev1.ServiceAccount
	ClusterRole        rbacv1.ClusterRole
	ClusterRoleBinding rbacv1.ClusterRoleBinding
	Service            corev1.Service
	Deployment         appsv1.Deployment
}

type operatorValues struct {
	Image struct {
		Registry   string `yaml:"registry"`
		Repository string `yaml:"repository"`
		Tag        string `yaml:"tag"`
	} `yaml:"image"`
}

// renderOperatorResources renders operator.yaml and returns each parsed resource.
// It uses a YAML stream decoder so that --- separators and Helm's # Source headers
// are handled correctly regardless of whitespace or extra blank documents.
func renderOperatorResources(t *testing.T, options *helm.Options) operatorResources {
	t.Helper()
	output, renderErr := helm.RenderTemplateE(t, options, operatorChartPath, releaseName, []string{"templates/operator.yaml"})
	require.NoError(t, renderErr)

	decoder := yaml.NewYAMLOrJSONDecoder(strings.NewReader(output), 4096)
	var res operatorResources
	require.NoError(t, decoder.Decode(&res.PriorityClass))
	require.NoError(t, decoder.Decode(&res.ServiceAccount))
	require.NoError(t, decoder.Decode(&res.ClusterRole))
	require.NoError(t, decoder.Decode(&res.ClusterRoleBinding))
	require.NoError(t, decoder.Decode(&res.Service))
	require.NoError(t, decoder.Decode(&res.Deployment))
	return res
}

func expectedOperatorImage(t *testing.T) string {
	t.Helper()

	valuesPath := filepath.Join(operatorChartPath, "values.yaml")
	valuesFile, err := os.ReadFile(valuesPath)
	require.NoError(t, err)

	var values operatorValues
	decoder := yaml.NewYAMLOrJSONDecoder(strings.NewReader(string(valuesFile)), 4096)
	require.NoError(t, decoder.Decode(&values))

	return values.Image.Registry + "/" + values.Image.Repository + ":" + values.Image.Tag
}

// webhookResourceNamesRule returns the cluster role rule that scopes get, patch and delete
// on webhook configurations to specific resource names, or nil if not found.
func webhookResourceNamesRule(rules []rbacv1.PolicyRule) *rbacv1.PolicyRule {
	for i, rule := range rules {
		for _, resource := range rule.Resources {
			if resource == "validatingwebhookconfigurations" && len(rule.ResourceNames) > 0 {
				return &rules[i]
			}
		}
	}
	return nil
}

// watchNamespaceEnvVar returns the WATCH_NAMESPACE env var from the operator container, or nil.
func watchNamespaceEnvVar(containers []corev1.Container) *corev1.EnvVar {
	for _, c := range containers {
		for i, env := range c.Env {
			if env.Name == "WATCH_NAMESPACE" {
				return &c.Env[i]
			}
		}
	}
	return nil
}

// TestOperatorClusterScopedMode tests that with no watchNamespaces set, cluster scoped resources
// use standard names without a namespace suffix, WATCH_NAMESPACE is not set,
// and the webhook rule covers only the global webhook names.
func TestOperatorClusterScopedMode(t *testing.T) {
	t.Parallel()

	options := &helm.Options{
		KubectlOptions: k8s.NewKubectlOptions("", "", "cockroach-ns"),
	}
	res := renderOperatorResources(t, options)

	require.Equal(t, "cockroachdb-operator", res.PriorityClass.Name)
	require.Equal(t, "cockroachdb-operator-role", res.ClusterRole.Name)
	require.Equal(t, "cockroachdb-operator", res.ClusterRoleBinding.Name)
	require.Equal(t, "cockroachdb-operator-role", res.ClusterRoleBinding.RoleRef.Name)
	require.Equal(t, res.PriorityClass.Name, res.Deployment.Spec.Template.Spec.PriorityClassName)

	// WATCH_NAMESPACE should not be set in cluster-scoped mode.
	require.Nil(t, watchNamespaceEnvVar(res.Deployment.Spec.Template.Spec.Containers))

	// Webhook rule should cover only the two global names.
	rule := webhookResourceNamesRule(res.ClusterRole.Rules)
	require.NotNil(t, rule, "expected a scoped webhook rule in cluster role")
	require.ElementsMatch(t, []string{
		"cockroach-webhook-config",
		"cockroach-mutating-webhook-config",
	}, rule.ResourceNames)
	require.ElementsMatch(t, []string{"get", "patch", "delete"}, rule.Verbs)
}

// TestOperatorNamespaceScopedMode tests that with watchNamespaces set, cluster scoped resources
// include the release namespace suffix, WATCH_NAMESPACE is set, and the webhook rule
// covers both the global and namespace suffixed webhook names.
func TestOperatorNamespaceScopedMode(t *testing.T) {
	t.Parallel()

	releaseNamespace := "ops-ns"
	options := &helm.Options{
		KubectlOptions: k8s.NewKubectlOptions("", "", releaseNamespace),
		SetValues: map[string]string{
			"watchNamespaces": "cockroach-ns",
		},
	}
	res := renderOperatorResources(t, options)

	require.Equal(t, "cockroachdb-operator-"+releaseNamespace, res.PriorityClass.Name)
	require.Equal(t, "cockroachdb-operator-role-"+releaseNamespace, res.ClusterRole.Name)
	require.Equal(t, "cockroachdb-operator-"+releaseNamespace, res.ClusterRoleBinding.Name)
	require.Equal(t, "cockroachdb-operator-role-"+releaseNamespace, res.ClusterRoleBinding.RoleRef.Name)
	require.Equal(t, res.PriorityClass.Name, res.Deployment.Spec.Template.Spec.PriorityClassName)

	// WATCH_NAMESPACE should reflect the provided value.
	envVar := watchNamespaceEnvVar(res.Deployment.Spec.Template.Spec.Containers)
	require.NotNil(t, envVar)
	require.Equal(t, "cockroach-ns", envVar.Value)

	// Webhook rule should cover global names and namespace suffixed names.
	rule := webhookResourceNamesRule(res.ClusterRole.Rules)
	require.NotNil(t, rule)
	require.ElementsMatch(t, []string{
		"cockroach-webhook-config",
		"cockroach-mutating-webhook-config",
		"cockroach-webhook-config-" + releaseNamespace,
		"cockroach-mutating-webhook-config-" + releaseNamespace,
	}, rule.ResourceNames)
}

// TestOperatorWatchNamespacesWhitespaceTrimmed tests that whitespace around watchNamespaces
// is trimmed before being used in resource names and WATCH_NAMESPACE.
func TestOperatorWatchNamespacesWhitespaceTrimmed(t *testing.T) {
	t.Parallel()

	releaseNamespace := "ops-ns"
	options := &helm.Options{
		KubectlOptions: k8s.NewKubectlOptions("", "", releaseNamespace),
		SetValues: map[string]string{
			"watchNamespaces": "  cockroach-ns  ",
		},
	}
	res := renderOperatorResources(t, options)

	// Resource names should not contain stray whitespace.
	require.Equal(t, "cockroachdb-operator-"+releaseNamespace, res.PriorityClass.Name)
	require.Equal(t, "cockroachdb-operator-role-"+releaseNamespace, res.ClusterRole.Name)
	require.Equal(t, "cockroachdb-operator-"+releaseNamespace, res.ClusterRoleBinding.Name)

	// WATCH_NAMESPACE value should be trimmed.
	envVar := watchNamespaceEnvVar(res.Deployment.Spec.Template.Spec.Containers)
	require.NotNil(t, envVar)
	require.Equal(t, "cockroach-ns", envVar.Value)
}

// TestOperatorMultipleWatchNamespaces tests that a list of namespaces separated by commas
// is passed through unchanged to WATCH_NAMESPACE.
func TestOperatorMultipleWatchNamespaces(t *testing.T) {
	t.Parallel()

	// Both --set and --set-string treat commas as array separators, so a
	// comma-separated namespace list must be supplied via a values file.
	valuesFile, err := os.CreateTemp("", "operator-values-*.yaml")
	require.NoError(t, err)
	defer os.Remove(valuesFile.Name())
	_, err = valuesFile.WriteString("watchNamespaces: \"ns-a,ns-b,ns-c\"\n")
	require.NoError(t, err)
	require.NoError(t, valuesFile.Close())

	options := &helm.Options{
		KubectlOptions: k8s.NewKubectlOptions("", "", "ops-ns"),
		ValuesFiles:    []string{valuesFile.Name()},
	}
	res := renderOperatorResources(t, options)

	envVar := watchNamespaceEnvVar(res.Deployment.Spec.Template.Spec.Containers)
	require.NotNil(t, envVar)
	require.Equal(t, "ns-a,ns-b,ns-c", envVar.Value)
}

// TestOperatorDefaultAppLabel tests that the default app label is applied to the deployment,
// its pod template, the service selector and the service account.
func TestOperatorDefaultAppLabel(t *testing.T) {
	t.Parallel()

	options := &helm.Options{
		KubectlOptions: k8s.NewKubectlOptions("", "", namespaceName),
	}
	res := renderOperatorResources(t, options)

	defaultLabel := "cockroach-operator"
	require.Equal(t, defaultLabel, res.Deployment.Labels["app"])
	require.Equal(t, defaultLabel, res.Deployment.Spec.Selector.MatchLabels["app"])
	require.Equal(t, defaultLabel, res.Deployment.Spec.Template.Labels["app"])
	require.Equal(t, defaultLabel, res.Service.Labels["app"])
	require.Equal(t, defaultLabel, res.Service.Spec.Selector["app"])
	require.Equal(t, defaultLabel, res.ServiceAccount.Labels["app"])
	require.Equal(t, defaultLabel, res.ClusterRoleBinding.Labels["app"])
}

// TestOperatorCustomAppLabel tests that a custom app label is applied consistently across
// the deployment, pod template, service selector and service account.
func TestOperatorCustomAppLabel(t *testing.T) {
	t.Parallel()

	options := &helm.Options{
		KubectlOptions: k8s.NewKubectlOptions("", "", namespaceName),
		SetValues: map[string]string{
			"appLabel": "my-operator",
		},
	}
	res := renderOperatorResources(t, options)

	customLabel := "my-operator"
	require.Equal(t, customLabel, res.Deployment.Labels["app"])
	require.Equal(t, customLabel, res.Deployment.Spec.Selector.MatchLabels["app"])
	require.Equal(t, customLabel, res.Deployment.Spec.Template.Labels["app"])
	require.Equal(t, customLabel, res.Service.Labels["app"])
	require.Equal(t, customLabel, res.Service.Spec.Selector["app"])
	require.Equal(t, customLabel, res.ServiceAccount.Labels["app"])
	require.Equal(t, customLabel, res.ClusterRoleBinding.Labels["app"])
}

// TestOperatorImageDigestUpdated keeps the expected image aligned with chart defaults.
func TestOperatorImageDigestUpdated(t *testing.T) {
	t.Parallel()

	options := &helm.Options{
		KubectlOptions: k8s.NewKubectlOptions("", "", namespaceName),
	}
	res := renderOperatorResources(t, options)

	require.Equal(t, expectedOperatorImage(t), res.Deployment.Spec.Template.Spec.Containers[0].Image)
}

// TestOperatorPreUpgradeValidationRequiresV1beta1OnlyState checks the phase 3 upgrade guardrails.
func TestOperatorPreUpgradeValidationRequiresV1beta1OnlyState(t *testing.T) {
	t.Parallel()

	options := &helm.Options{
		KubectlOptions: k8s.NewKubectlOptions("", "", namespaceName),
	}
	output, err := helm.RenderTemplateE(t, options, operatorChartPath, releaseName, []string{"templates/pre-upgrade-validation.yaml"}, "--is-upgrade")
	require.NoError(t, err)

	require.Contains(t, output, `V1BETA1_STORAGE=$(kubectl get crd crdbclusters.crdb.cockroachlabs.com`)
	require.Contains(t, output, `v1alpha1 served: $V1ALPHA1_SERVED`)
	require.Contains(t, output, `UPGRADE BLOCKED - storedVersions must be [\"v1beta1\"]`)
	require.NotContains(t, output, `Cannot skip Phase 1`)
	require.NotContains(t, output, `helm get manifest`)
}

// TestOperatorHookRBACIsPreUpgradeOnly keeps the hook permissions scoped to upgrade validation.
func TestOperatorHookRBACIsPreUpgradeOnly(t *testing.T) {
	t.Parallel()

	options := &helm.Options{
		KubectlOptions: k8s.NewKubectlOptions("", "", namespaceName),
	}
	output, err := helm.RenderTemplateE(t, options, operatorChartPath, releaseName, []string{"templates/clusterrole-hooks.yaml"})
	require.NoError(t, err)

	decoder := yaml.NewYAMLOrJSONDecoder(strings.NewReader(output), 4096)
	var role rbacv1.ClusterRole
	var binding rbacv1.ClusterRoleBinding
	require.NoError(t, decoder.Decode(&role))
	require.NoError(t, decoder.Decode(&binding))

	require.Equal(t, "pre-upgrade", role.Annotations["helm.sh/hook"])
	require.Equal(t, "pre-upgrade", binding.Annotations["helm.sh/hook"])

	for _, rule := range role.Rules {
		require.NotContains(t, rule.Resources, "customresourcedefinitions/status")
		require.NotContains(t, rule.Resources, "secrets")
		if len(rule.APIGroups) == 1 && rule.APIGroups[0] == "crdb.cockroachlabs.com" {
			require.ElementsMatch(t, []string{"get", "list"}, rule.Verbs)
		}
	}
}

// TestOperatorStorageMigrationTemplateRemoved verifies the old storage migration hook is gone in phase 3.
func TestOperatorStorageMigrationTemplateRemoved(t *testing.T) {
	t.Parallel()

	options := &helm.Options{
		KubectlOptions: k8s.NewKubectlOptions("", "", namespaceName),
	}
	_, err := helm.RenderTemplateE(t, options, operatorChartPath, releaseName, []string{"templates/storage-migration.yaml"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "Could not resolve template file templates/storage-migration.yaml")
}
