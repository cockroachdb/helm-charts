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
	operatorChartPath, initErr = filepath.Abs("../../cockroachdb-operator/charts/operator")
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
func renderOperatorResources(t *testing.T, options *helm.Options, extraHelmArgs ...string) operatorResources {
	t.Helper()
	output, renderErr := helm.RenderTemplateE(t, options, operatorChartPath, releaseName, []string{"templates/operator.yaml"}, extraHelmArgs...)
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
	t.Cleanup(func() {
		if err := os.Remove(valuesFile.Name()); err != nil {
			t.Logf("Failed to remove temporary values file: %v", err)
		}
	})
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

// TestOperatorPodAnnotationsAndLabels tests that podAnnotations and podLabels are added to
// the operator pod template without changing the deployment selector.
func TestOperatorPodAnnotationsAndLabels(t *testing.T) {
	t.Parallel()

	t.Run("unset", func(t *testing.T) {
		t.Parallel()
		options := &helm.Options{
			KubectlOptions: k8s.NewKubectlOptions("", "", namespaceName),
		}
		res := renderOperatorResources(t, options)

		require.Empty(t, res.Deployment.Spec.Template.Annotations)
		require.Equal(t, map[string]string{"app": "cockroach-operator"}, res.Deployment.Spec.Template.Labels)
	})

	t.Run("set", func(t *testing.T) {
		t.Parallel()
		options := &helm.Options{
			KubectlOptions: k8s.NewKubectlOptions("", "", namespaceName),
			SetStrValues: map[string]string{
				"podAnnotations.sidecar\\.istio\\.io/inject": "false",
				"podLabels.team": "platform",
			},
		}
		res := renderOperatorResources(t, options)

		require.Equal(t, map[string]string{"sidecar.istio.io/inject": "false"}, res.Deployment.Spec.Template.Annotations)
		require.Equal(t, "platform", res.Deployment.Spec.Template.Labels["team"])
		require.Equal(t, "cockroach-operator", res.Deployment.Spec.Template.Labels["app"])
		require.Equal(t, map[string]string{"app": "cockroach-operator"}, res.Deployment.Spec.Selector.MatchLabels)
	})
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

func TestOperatorResources(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name     string
		values   string
		upgrade  bool
		requests map[corev1.ResourceName]string
		limits   map[corev1.ResourceName]string
	}{
		{
			name:     "defaults",
			requests: map[corev1.ResourceName]string{corev1.ResourceCPU: "500m", corev1.ResourceMemory: "1000Mi"},
			limits:   map[corev1.ResourceName]string{corev1.ResourceCPU: "2", corev1.ResourceMemory: "4000Mi"},
		},
		{
			name:     "upgrade reusing old values without resources",
			values:   "image:\n  tag: v1.1.0\n",
			upgrade:  true,
			requests: map[corev1.ResourceName]string{corev1.ResourceCPU: "500m", corev1.ResourceMemory: "1000Mi"},
			limits:   map[corev1.ResourceName]string{corev1.ResourceCPU: "2", corev1.ResourceMemory: "4000Mi"},
		},
		{
			name:     "custom requests and limits",
			values:   "resources:\n  requests:\n    cpu: 125m\n    memory: 256Mi\n  limits:\n    cpu: 1\n    memory: 1Gi\n",
			requests: map[corev1.ResourceName]string{corev1.ResourceCPU: "125m", corev1.ResourceMemory: "256Mi"},
			limits:   map[corev1.ResourceName]string{corev1.ResourceCPU: "1", corev1.ResourceMemory: "1Gi"},
		},
		{
			name:     "partial override preserves other defaults",
			values:   "resources:\n  requests:\n    cpu: 125m\n",
			requests: map[corev1.ResourceName]string{corev1.ResourceCPU: "125m", corev1.ResourceMemory: "1000Mi"},
			limits:   map[corev1.ResourceName]string{corev1.ResourceCPU: "2", corev1.ResourceMemory: "4000Mi"},
		},
		{
			name:     "remove CPU limit",
			values:   "resources:\n  limits:\n    cpu: null\n",
			requests: map[corev1.ResourceName]string{corev1.ResourceCPU: "500m", corev1.ResourceMemory: "1000Mi"},
			limits:   map[corev1.ResourceName]string{corev1.ResourceMemory: "4000Mi"},
		},
		{
			name:   "empty resources override",
			values: "resources: {}\n",
		},
		{
			name:   "remove all resources",
			values: "resources: null\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			valuesFile := filepath.Join(t.TempDir(), "values.yaml")
			require.NoError(t, os.WriteFile(valuesFile, []byte(tc.values), 0600))
			options := &helm.Options{
				KubectlOptions: k8s.NewKubectlOptions("", "", namespaceName),
				ValuesFiles:    []string{valuesFile},
			}
			var extraArgs []string
			if tc.upgrade {
				extraArgs = []string{"--is-upgrade"}
			}
			res := renderOperatorResources(t, options, extraArgs...)
			resources := res.Deployment.Spec.Template.Spec.Containers[0].Resources
			require.Len(t, resources.Requests, len(tc.requests))
			require.Len(t, resources.Limits, len(tc.limits))
			for name, expected := range tc.requests {
				actual := resources.Requests[name]
				require.Equal(t, expected, actual.String(), "request %s", name)
			}
			for name, expected := range tc.limits {
				actual := resources.Limits[name]
				require.Equal(t, expected, actual.String(), "limit %s", name)
			}
		})
	}
}

func TestOperatorScheduling(t *testing.T) {
	t.Parallel()

	t.Run("defaults", func(t *testing.T) {
		t.Parallel()
		res := renderOperatorResources(t, &helm.Options{
			KubectlOptions: k8s.NewKubectlOptions("", "", namespaceName),
		})
		podSpec := res.Deployment.Spec.Template.Spec
		require.Empty(t, podSpec.NodeSelector)
		require.Empty(t, podSpec.Tolerations)
		require.Nil(t, podSpec.Affinity)
	})

	t.Run("custom scheduling", func(t *testing.T) {
		t.Parallel()
		valuesFile := filepath.Join(t.TempDir(), "values.yaml")
		require.NoError(t, os.WriteFile(valuesFile, []byte(`nodeSelector:
  workload: operators
tolerations:
  - key: workload
    operator: Equal
    value: operators
    effect: NoSchedule
affinity:
  podAntiAffinity:
    preferredDuringSchedulingIgnoredDuringExecution:
      - weight: 100
        podAffinityTerm:
          labelSelector:
            matchLabels:
              app: cockroach-operator
          topologyKey: kubernetes.io/hostname
`), 0600))
		res := renderOperatorResources(t, &helm.Options{
			KubectlOptions: k8s.NewKubectlOptions("", "", namespaceName),
			ValuesFiles:    []string{valuesFile},
		})
		podSpec := res.Deployment.Spec.Template.Spec
		require.Equal(t, map[string]string{"workload": "operators"}, podSpec.NodeSelector)
		require.Equal(t, []corev1.Toleration{{
			Key: "workload", Operator: corev1.TolerationOpEqual, Value: "operators", Effect: corev1.TaintEffectNoSchedule,
		}}, podSpec.Tolerations)
		require.NotNil(t, podSpec.Affinity)
		require.NotNil(t, podSpec.Affinity.PodAntiAffinity)
		terms := podSpec.Affinity.PodAntiAffinity.PreferredDuringSchedulingIgnoredDuringExecution
		require.Len(t, terms, 1)
		require.EqualValues(t, 100, terms[0].Weight)
		require.Equal(t, "kubernetes.io/hostname", terms[0].PodAffinityTerm.TopologyKey)
		require.NotNil(t, terms[0].PodAffinityTerm.LabelSelector)
		require.Equal(t, map[string]string{"app": "cockroach-operator"}, terms[0].PodAffinityTerm.LabelSelector.MatchLabels)
	})
}

func TestOperatorRelatedImages(t *testing.T) {
	t.Parallel()

	options := &helm.Options{
		KubectlOptions: k8s.NewKubectlOptions("", "", namespaceName),
		SetValues: map[string]string{
			"image.registry":                         "registry.example.com/operator",
			"image.tag":                              "v9.9.9",
			"relatedImages.initContainer.registry":   "registry.example.com/sidecars",
			"relatedImages.initContainer.repository": "custom-init",
			"relatedImages.initContainer.tag":        "init-tag",
			"relatedImages.inotifywait.registry":     "registry.example.com/sidecars",
			"relatedImages.inotifywait.repository":   "custom-inotify",
			"relatedImages.inotifywait.tag":          "inotify-tag",
		},
	}
	res := renderOperatorResources(t, options)

	env := res.Deployment.Spec.Template.Spec.Containers[0].Env
	require.Equal(t,
		"registry.example.com/sidecars/custom-init:init-tag",
		findEnvVar(env, "RELATED_IMAGE_INIT_CONTAINER").Value,
	)
	require.Equal(t,
		"registry.example.com/sidecars/custom-inotify:inotify-tag",
		findEnvVar(env, "RELATED_IMAGE_INOTIFYWAIT").Value,
	)
}

func TestOperatorNodeReaderRBAC(t *testing.T) {
	t.Parallel()

	options := &helm.Options{
		KubectlOptions: k8s.NewKubectlOptions("", "", namespaceName),
		SetValues: map[string]string{
			"nodeReader.enabled":                        "true",
			"nodeReader.name":                           "platform-node-reader",
			"nodeReader.subjects[0].namespace":          "tenant-a",
			"nodeReader.subjects[0].serviceAccountName": "crdb-cockroachdb",
			"nodeReader.subjects[1].namespace":          "tenant-b",
			"nodeReader.subjects[1].serviceAccountName": "custom-crdb-sa",
		},
	}

	output, err := helm.RenderTemplateE(t, options, operatorChartPath, releaseName, []string{"templates/node-reader-rbac.yaml"})
	require.NoError(t, err)

	decoder := yaml.NewYAMLOrJSONDecoder(strings.NewReader(output), 4096)
	var clusterRole rbacv1.ClusterRole
	require.NoError(t, decoder.Decode(&clusterRole))
	require.Equal(t, "ClusterRole", clusterRole.Kind)
	require.Equal(t, "platform-node-reader", clusterRole.Name)
	require.Contains(t, clusterRole.Rules, rbacv1.PolicyRule{
		APIGroups: []string{""},
		Resources: []string{"nodes"},
		Verbs:     []string{"get", "list", "watch"},
	})

	var clusterRoleBinding rbacv1.ClusterRoleBinding
	require.NoError(t, decoder.Decode(&clusterRoleBinding))
	require.Equal(t, "ClusterRoleBinding", clusterRoleBinding.Kind)
	require.Equal(t, "platform-node-reader", clusterRoleBinding.Name)
	require.Equal(t, "platform-node-reader", clusterRoleBinding.RoleRef.Name)
	require.ElementsMatch(t, []rbacv1.Subject{
		{
			Kind:      "ServiceAccount",
			Name:      "crdb-cockroachdb",
			Namespace: "tenant-a",
		},
		{
			Kind:      "ServiceAccount",
			Name:      "custom-crdb-sa",
			Namespace: "tenant-b",
		},
	}, clusterRoleBinding.Subjects)
}

func TestOperatorNodeReaderRBACDefaultName(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name            string
		releaseNS       string
		watchNamespaces string
		expectedName    string
	}{
		{
			name:         "global",
			releaseNS:    "ops-ns",
			expectedName: "cockroachdb-node-reader",
		},
		{
			name:            "scoped",
			releaseNS:       "ops-ns",
			watchNamespaces: "tenant-a",
			expectedName:    "cockroachdb-node-reader-ops-ns",
		},
	}

	for _, tc := range testCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			options := &helm.Options{
				KubectlOptions: k8s.NewKubectlOptions("", "", tc.releaseNS),
				SetValues: map[string]string{
					"nodeReader.enabled":                        "true",
					"nodeReader.subjects[0].namespace":          "tenant-a",
					"nodeReader.subjects[0].serviceAccountName": "crdb-cockroachdb",
				},
			}
			if tc.watchNamespaces != "" {
				options.SetValues["watchNamespaces"] = tc.watchNamespaces
			}

			output, err := helm.RenderTemplateE(t, options, operatorChartPath, releaseName, []string{"templates/node-reader-rbac.yaml"})
			require.NoError(t, err)

			decoder := yaml.NewYAMLOrJSONDecoder(strings.NewReader(output), 4096)
			var clusterRole rbacv1.ClusterRole
			require.NoError(t, decoder.Decode(&clusterRole))
			require.Equal(t, tc.expectedName, clusterRole.Name)

			var clusterRoleBinding rbacv1.ClusterRoleBinding
			require.NoError(t, decoder.Decode(&clusterRoleBinding))
			require.Equal(t, tc.expectedName, clusterRoleBinding.Name)
			require.Equal(t, tc.expectedName, clusterRoleBinding.RoleRef.Name)
		})
	}
}

func TestOperatorNodeReaderRBACRequiresSubjects(t *testing.T) {
	t.Parallel()

	options := &helm.Options{
		KubectlOptions: k8s.NewKubectlOptions("", "", namespaceName),
		SetValues: map[string]string{
			"nodeReader.enabled": "true",
		},
	}

	_, err := helm.RenderTemplateE(t, options, operatorChartPath, releaseName, []string{"templates/node-reader-rbac.yaml"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "nodeReader.subjects must contain at least one")
}

// TestOperatorPreUpgradeValidationRemoved verifies the operator pre-upgrade validation
// hook is no longer present. The operator image handles CRD version management at runtime.
func TestOperatorPreUpgradeValidationRemoved(t *testing.T) {
	t.Parallel()

	options := &helm.Options{
		KubectlOptions: k8s.NewKubectlOptions("", "", namespaceName),
	}
	_, err := helm.RenderTemplateE(t, options, operatorChartPath, releaseName, []string{"templates/pre-upgrade-validation.yaml"}, "--is-upgrade")
	require.Error(t, err, "pre-upgrade-validation.yaml should not exist in operator chart")
}

// TestOperatorHookRBACRemoved verifies the hook RBAC template is no longer present.
func TestOperatorHookRBACRemoved(t *testing.T) {
	t.Parallel()

	options := &helm.Options{
		KubectlOptions: k8s.NewKubectlOptions("", "", namespaceName),
	}
	_, err := helm.RenderTemplateE(t, options, operatorChartPath, releaseName, []string{"templates/clusterrole-hooks.yaml"})
	require.Error(t, err, "clusterrole-hooks.yaml should not exist in operator chart")
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
