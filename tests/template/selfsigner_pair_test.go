package template

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/gruntwork-io/terratest/modules/helm"
	"github.com/gruntwork-io/terratest/modules/k8s"
	"github.com/stretchr/testify/require"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
)

// selfSignerChart names one chart's self-signer templates for the pair test. The two charts nest
// their values differently, so each values key is spelled per chart.
type selfSignerChart struct {
	path string
	// tlsEnabled is the values key that turns TLS on and off.
	tlsEnabled string
	// tlsOffError is the error rendering the client/node CronJob must return when TLS is off.
	tlsOffError string
	// securityContextEnabled is the values key that turns the self-signer securityContext on and off.
	securityContextEnabled string
	// selfSignerOff turns the self-signer off while keeping the chart's values valid.
	selfSignerOff map[string]string
	// grantsOpenShiftSCC is whether the chart's self-signer Role grants use of OpenShift
	// SecurityContextConstraints.
	grantsOpenShiftSCC bool
}

var selfSignerCharts = []selfSignerChart{
	{
		path:       "../../cockroachdb-legacy",
		tlsEnabled: "tls.enabled",
		// Helm reports a template that rendered nothing as missing.
		tlsOffError:            "could not find template templates/cronjob-client-node-certSelfSigner.yaml",
		securityContextEnabled: "tls.certs.selfSigner.securityContext.enabled",
		selfSignerOff: map[string]string{
			"tls.certs.selfSigner.enabled": "false",
			"tls.certs.certManager":        "true",
		},
	},
	{
		path:       "../../cockroachdb-operator/charts/cockroachdb",
		tlsEnabled: "cockroachdb.tls.enabled",
		// The chart validation rejects selfSigner with TLS off before any template renders.
		tlsOffError:            "must all be disabled when TLS is off",
		securityContextEnabled: "cockroachdb.tls.selfSigner.securityContext.enabled",
		selfSignerOff: map[string]string{
			"cockroachdb.tls.selfSigner.enabled":  "false",
			"cockroachdb.tls.certManager.enabled": "true",
		},
		grantsOpenShiftSCC: true,
	},
}

// renderSelfSignerPod renders one self-signer template and returns its pod spec, whether the
// template is the install Job or one of the rotation CronJobs.
func renderSelfSignerPod(t *testing.T, options *helm.Options, chartPath, template string) corev1.PodSpec {
	t.Helper()
	output := helm.RenderTemplate(t, options, chartPath, releaseName, []string{"templates/" + template})
	var spec corev1.PodSpec
	if strings.HasPrefix(template, "cronjob-") {
		var cronJob batchv1.CronJob
		helm.UnmarshalK8SYaml(t, output, &cronJob)
		spec = cronJob.Spec.JobTemplate.Spec.Template.Spec
	} else {
		var job batchv1.Job
		helm.UnmarshalK8SYaml(t, output, &job)
		spec = job.Spec.Template.Spec
	}
	require.NotEmpty(t, spec.Containers, "%s renders no containers", template)
	return spec
}

// renderSelfSignerRoleRules renders one self-signer Role template and returns its rules.
func renderSelfSignerRoleRules(t *testing.T, options *helm.Options, chartPath, template string) []rbacv1.PolicyRule {
	t.Helper()
	output := helm.RenderTemplate(t, options, chartPath, releaseName, []string{"templates/" + template})
	var role rbacv1.Role
	helm.UnmarshalK8SYaml(t, output, &role)
	return role.Rules
}

// hasRule reports whether some rule grants verb on resource in apiGroup.
func hasRule(rules []rbacv1.PolicyRule, apiGroup, resource, verb string) bool {
	for _, rule := range rules {
		if slices.Contains(rule.APIGroups, apiGroup) && slices.Contains(rule.Resources, resource) &&
			slices.Contains(rule.Verbs, verb) {
			return true
		}
	}
	return false
}

// TestSelfSignerRotationMatchesInstall checks the certificate-rotation CronJobs and the pre-delete
// cleaner Job against the install Job: pod and container securityContext, the TLS gate, and the
// Role granted to the rotation ServiceAccount. The four templates share their securityContext,
// gate and Role rules through the chart's `selfcerts.*` helpers; this test fails if one of them
// stops including a helper or reintroduces an inline copy.
func TestSelfSignerRotationMatchesInstall(t *testing.T) {
	t.Parallel()
	installJob := "job-certSelfSigner.yaml"
	others := []string{"job-cleaner.yaml", "cronjob-client-node-certSelfSigner.yaml", "cronjob-ca-certSelfSigner.yaml"}
	for _, chart := range selfSignerCharts {
		t.Run(chart.path, func(t *testing.T) {
			t.Parallel()
			chartPath, err := filepath.Abs(chart.path)
			require.NoError(t, err)
			options := &helm.Options{KubectlOptions: k8s.NewKubectlOptions("", "", namespaceName)}

			job := renderSelfSignerPod(t, options, chartPath, installJob)
			require.NotNil(t, job.SecurityContext, "install Job must set the pod securityContext")
			require.NotNil(t, job.Containers[0].SecurityContext, "install Job must set the container securityContext")

			for _, template := range others {
				t.Run(template, func(t *testing.T) {
					pod := renderSelfSignerPod(t, options, chartPath, template)
					require.Equal(t, job.SecurityContext, pod.SecurityContext,
						"%s pod securityContext differs from the install Job", template)
					require.Equal(t, job.Containers[0].SecurityContext, pod.Containers[0].SecurityContext,
						"%s container securityContext differs from the install Job", template)
				})
			}

			t.Run("securityContext disabled", func(t *testing.T) {
				scOff := &helm.Options{
					KubectlOptions: k8s.NewKubectlOptions("", "", namespaceName),
					SetValues:      map[string]string{chart.securityContextEnabled: "false"},
				}
				for _, template := range append([]string{installJob}, others...) {
					pod := renderSelfSignerPod(t, scOff, chartPath, template)
					require.Nil(t, pod.SecurityContext, "%s sets a pod securityContext while disabled", template)
					require.Nil(t, pod.Containers[0].SecurityContext,
						"%s sets a container securityContext while disabled", template)
				}
			})

			t.Run("client-node CronJob gate", func(t *testing.T) {
				tlsOff := &helm.Options{SetValues: map[string]string{chart.tlsEnabled: "false"}}
				_, err := helm.RenderTemplateE(t, tlsOff, chartPath, releaseName, []string{"templates/cronjob-client-node-certSelfSigner.yaml"})
				require.ErrorContains(t, err, chart.tlsOffError)
			})

			t.Run("client-node CronJob gate without self-signer", func(t *testing.T) {
				selfSignerOff := &helm.Options{SetValues: chart.selfSignerOff}
				_, err := helm.RenderTemplateE(t, selfSignerOff, chartPath, releaseName, []string{"templates/cronjob-client-node-certSelfSigner.yaml"})
				require.ErrorContains(t, err, "could not find template")
			})

			t.Run("rotate Role rules", func(t *testing.T) {
				install := renderSelfSignerRoleRules(t, options, chartPath, "role-certSelfSigner.yaml")
				rotate := renderSelfSignerRoleRules(t, options, chartPath, "role-certRotateSelfSigner.yaml")
				require.NotEmpty(t, install)
				require.Equal(t, install, rotate)
				// These are the rules the rotate Role lacked before the Role templates shared selfcerts.roleRules.
				require.True(t, hasRule(rotate, "", "configmaps", "delete"), "rotate Role must delete configmaps")
				if chart.grantsOpenShiftSCC {
					require.True(t, hasRule(rotate, "security.openshift.io", "securitycontextconstraints", "use"),
						"rotate Role must use securitycontextconstraints")
				}
			})
		})
	}
}
