package template

import (
	"path/filepath"
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
// their values differently, so the TLS switch is spelled per chart.
type selfSignerChart struct {
	path string
	// tlsEnabled is the values key that turns TLS on and off.
	tlsEnabled string
	// cronJobRendersWithTLSOff records today's behavior: the client/node rotation CronJob of this
	// chart renders even when TLS is off, although its ServiceAccount, Role and RoleBinding do not.
	cronJobRendersWithTLSOff bool
}

var selfSignerCharts = []selfSignerChart{
	{path: "../../cockroachdb-legacy", tlsEnabled: "tls.enabled", cronJobRendersWithTLSOff: true},
	// The operator chart's validation rejects selfSigner.enabled with TLS off before any
	// template renders, so the gap does not surface there.
	{path: "../../cockroachdb-operator/charts/cockroachdb", tlsEnabled: "cockroachdb.tls.enabled"},
}

// renderSelfSignerPod renders one self-signer template and returns its pod spec, whether the
// template is the install Job or one of the rotation CronJobs.
func renderSelfSignerPod(t *testing.T, options *helm.Options, chartPath, template string) corev1.PodSpec {
	t.Helper()
	output := helm.RenderTemplate(t, options, chartPath, releaseName, []string{"templates/" + template})
	if strings.HasPrefix(template, "cronjob-") {
		var cronJob batchv1.CronJob
		helm.UnmarshalK8SYaml(t, output, &cronJob)
		return cronJob.Spec.JobTemplate.Spec.Template.Spec
	}
	var job batchv1.Job
	helm.UnmarshalK8SYaml(t, output, &job)
	return job.Spec.Template.Spec
}

// renderSelfSignerRoleRules renders one self-signer Role template and returns its rules.
func renderSelfSignerRoleRules(t *testing.T, options *helm.Options, chartPath, template string) []rbacv1.PolicyRule {
	t.Helper()
	output := helm.RenderTemplate(t, options, chartPath, releaseName, []string{"templates/" + template})
	var role rbacv1.Role
	helm.UnmarshalK8SYaml(t, output, &role)
	return role.Rules
}

// TestSelfSignerRotationMatchesInstall checks the certificate-rotation CronJobs against the install
// Job: pod and container securityContext, the TLS gate, and the Role granted to the rotation
// ServiceAccount. With the pre-delete cleaner Job, four templates copy one pod spec, and this test
// is what fails when a copy drifts. It currently records the drift; the fix flips each assertion
// to equality.
func TestSelfSignerRotationMatchesInstall(t *testing.T) {
	t.Parallel()
	for _, chart := range selfSignerCharts {
		t.Run(chart.path, func(t *testing.T) {
			t.Parallel()
			chartPath, err := filepath.Abs(chart.path)
			require.NoError(t, err)
			options := &helm.Options{KubectlOptions: k8s.NewKubectlOptions("", "", namespaceName)}

			job := renderSelfSignerPod(t, options, chartPath, "job-certSelfSigner.yaml")
			require.NotNil(t, job.SecurityContext, "install Job must set the pod securityContext")
			require.NotNil(t, job.Containers[0].SecurityContext, "install Job must set the container securityContext")

			for _, template := range []string{"cronjob-client-node-certSelfSigner.yaml", "cronjob-ca-certSelfSigner.yaml"} {
				t.Run(template, func(t *testing.T) {
					cronJob := renderSelfSignerPod(t, options, chartPath, template)
					// Today the CronJobs omit both securityContext blocks the Job sets.
					require.Nil(t, cronJob.SecurityContext)
					require.Nil(t, cronJob.Containers[0].SecurityContext)
				})
			}

			// The pre-delete cleaner Job is a fourth copy of the pod spec and already matches today.
			t.Run("job-cleaner.yaml", func(t *testing.T) {
				cleaner := renderSelfSignerPod(t, options, chartPath, "job-cleaner.yaml")
				require.Equal(t, job.SecurityContext, cleaner.SecurityContext, "cleaner Job pod securityContext must match the install Job")
				require.Equal(t, job.Containers[0].SecurityContext, cleaner.Containers[0].SecurityContext, "cleaner Job container securityContext must match the install Job")
			})

			t.Run("client-node CronJob gate", func(t *testing.T) {
				tlsOff := &helm.Options{SetValues: map[string]string{chart.tlsEnabled: "false"}}
				_, err := helm.RenderTemplateE(t, tlsOff, chartPath, releaseName, []string{"templates/cronjob-client-node-certSelfSigner.yaml"})
				if chart.cronJobRendersWithTLSOff {
					require.NoError(t, err)
				} else {
					require.Error(t, err)
				}
			})

			t.Run("rotate Role rules", func(t *testing.T) {
				install := renderSelfSignerRoleRules(t, options, chartPath, "role-certSelfSigner.yaml")
				rotate := renderSelfSignerRoleRules(t, options, chartPath, "role-certRotateSelfSigner.yaml")
				// Today the rotate Role is a narrower, separately maintained copy of the install Role.
				require.NotEqual(t, install, rotate)
			})
		})
	}
}
