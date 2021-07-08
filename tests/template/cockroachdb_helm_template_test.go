package template

import (
	appsv1 "k8s.io/api/apps/v1"
	"path/filepath"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	"k8s.io/api/batch/v1beta1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"

	"github.com/gruntwork-io/terratest/modules/helm"
	"github.com/gruntwork-io/terratest/modules/k8s"
	"github.com/gruntwork-io/terratest/modules/random"
	"github.com/stretchr/testify/require"
)

var (
	releaseName   = "helm-basic"
	namespaceName = "medieval-" + strings.ToLower(random.UniqueId())
)

// TestHelmSelfCertSignerServiceAccount contains the tests around the service account of self signer utility
func TestHelmSelfCertSignerServiceAccount(t *testing.T) {
	t.Parallel()

	// Path to the helm chart we will test
	helmChartPath, err := filepath.Abs("../../cockroachdb")
	require.NoError(t, err)

	// Setup the args. For this test, we will set the following input values:
	options := &helm.Options{
		KubectlOptions: k8s.NewKubectlOptions("", "", namespaceName),
	}

	// Rendering the template of self signer service account
	output := helm.RenderTemplate(t, options, helmChartPath, releaseName, []string{"templates/serviceaccount-certSelfSigner.yaml"})

	// Now we use kubernetes/client-go library to render the template output into the ServiceAccount struct. This will
	// ensure the ServiceAccount resource is rendered correctly.
	var serviceAccount corev1.ServiceAccount
	helm.UnmarshalK8SYaml(t, output, &serviceAccount)

	// Verify the namespace matches the expected supplied namespace.
	require.Equal(t, namespaceName, serviceAccount.Namespace)

	// Setup the args. For this test, we will set the following input values:
	options = &helm.Options{
		KubectlOptions: k8s.NewKubectlOptions("", "", namespaceName),
		SetValues: map[string]string{
			"tls.certs.selfSigner.enabled": "false",
		},
	}

	// Service account will error out as it could not find the template due to if condition is failing
	// inside which template resides.
	_, err = helm.RenderTemplateE(t, options, helmChartPath, releaseName, []string{"templates/serviceaccount-certSelfSigner.yaml"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "Error: could not find template templates/serviceaccount-certSelfSigner.yaml in chart")
}

// TestHelmSelfCertSignerRole contains the tests around the Role of self signer utility
func TestHelmSelfCertSignerRole(t *testing.T) {
	t.Parallel()

	// Path to the helm chart we will test
	helmChartPath, err := filepath.Abs("../../cockroachdb")
	require.NoError(t, err)

	// Setup the args. For this test, we will set the following input values:
	options := &helm.Options{
		KubectlOptions: k8s.NewKubectlOptions("", "", namespaceName),
	}

	// Rendering the template of self signer service account
	output := helm.RenderTemplate(t, options, helmChartPath, releaseName, []string{"templates/role-certSelfSigner.yaml"})

	var role rbacv1.Role
	helm.UnmarshalK8SYaml(t, output, &role)
	require.Equal(t, namespaceName, role.Namespace)

	// Setup the args. For this test, we will set the following input values:
	options = &helm.Options{
		KubectlOptions: k8s.NewKubectlOptions("", "", namespaceName),
		SetValues: map[string]string{
			"tls.certs.selfSigner.enabled": "false",
		},
	}

	// Role will error out as it could not find the template due to if condition failing inside which template resides.
	_, err = helm.RenderTemplateE(t, options, helmChartPath, releaseName, []string{"templates/role-certSelfSigner.yaml"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "Error: could not find template templates/role-certSelfSigner.yaml in chart")
}

// TestHelmSelfCertSignerRoleBinding contains the tests around the rolebinding of self signer utility
func TestHelmSelfCertSignerRoleBinding(t *testing.T) {
	t.Parallel()

	// Path to the helm chart we will test
	helmChartPath, err := filepath.Abs("../../cockroachdb")
	require.NoError(t, err)

	// Setup the args. For this test, we will set the following input values:
	options := &helm.Options{
		KubectlOptions: k8s.NewKubectlOptions("", "", namespaceName),
	}

	// Rendering the template of self signer service account
	output := helm.RenderTemplate(t, options, helmChartPath, releaseName, []string{"templates/rolebinding-certSelfSigner.yaml"})

	var rolebinding rbacv1.RoleBinding
	helm.UnmarshalK8SYaml(t, output, &rolebinding)
	require.Equal(t, namespaceName, rolebinding.Namespace)

	// Setup the args. For this test, we will set the following input values:
	options = &helm.Options{
		KubectlOptions: k8s.NewKubectlOptions("", "", namespaceName),
		SetValues: map[string]string{
			"tls.certs.selfSigner.enabled": "false",
		},
	}

	// RoleBinding will error out as it could not find the template due to if condition failing inside which template resides.
	_, err = helm.RenderTemplateE(t, options, helmChartPath, releaseName, []string{"templates/rolebinding-certSelfSigner.yaml"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "Error: could not find template templates/rolebinding-certSelfSigner.yaml in chart")
}

// TestHelmSelfCertSignerJob contains the tests around the job of self signer utility
func TestHelmSelfCertSignerJob(t *testing.T) {
	t.Parallel()

	// Path to the helm chart we will test
	helmChartPath, err := filepath.Abs("../../cockroachdb")
	require.NoError(t, err)

	// Setup the args. For this test, we will set the following input values:
	options := &helm.Options{
		KubectlOptions: k8s.NewKubectlOptions("", "", namespaceName),
	}

	// Rendering the template of self signer service account
	output := helm.RenderTemplate(t, options, helmChartPath, releaseName, []string{"templates/job-certSelfSigner.yaml"})

	var job batchv1.Job
	helm.UnmarshalK8SYaml(t, output, &job)
	require.Equal(t, namespaceName, job.Namespace)

	// Setup the args. For this test, we will set the following input values:
	options = &helm.Options{
		KubectlOptions: k8s.NewKubectlOptions("", "", namespaceName),
		SetValues: map[string]string{
			"tls.certs.selfSigner.enabled": "false",
		},
	}

	// Service account will error out as it could not find the template due to if condition failing inside which template resides.
	_, err = helm.RenderTemplateE(t, options, helmChartPath, releaseName, []string{"templates/job-certSelfSigner.yaml"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "Error: could not find template templates/job-certSelfSigner.yaml in chart")
}

// TestHelmSelfCertSignerCronJob contains the tests around the cronjob of self signer utility
func TestHelmSelfCertSignerCronJob(t *testing.T) {
	t.Parallel()

	// Path to the helm chart we will test
	helmChartPath, err := filepath.Abs("../../cockroachdb")
	require.NoError(t, err)

	// Setup the args. For this test, we will set the following input values:
	options := &helm.Options{
		KubectlOptions: k8s.NewKubectlOptions("", "", namespaceName),
	}

	// Rendering the template of self signer service account
	output := helm.RenderTemplate(t, options, helmChartPath, releaseName, []string{"templates/cronjob-ca-certSelfSigner.yaml"})

	var cronjob v1beta1.CronJob
	helm.UnmarshalK8SYaml(t, output, &cronjob)
	require.Equal(t, namespaceName, cronjob.Namespace)

	// Rendering the template of self signer cert rotation job
	output = helm.RenderTemplate(t, options, helmChartPath, releaseName, []string{"templates/cronjob-client-certSelfSigner.yaml"})
	helm.UnmarshalK8SYaml(t, output, &cronjob)
	require.Equal(t, namespaceName, cronjob.Namespace)

	testCases := []struct {
		name   string
		values map[string]string
	}{
		{
			"Self Signer disable",
			map[string]string{"tls.certs.selfSigner.enabled": "false"},
		},
		{
			"Cert rotate disable",
			map[string]string{
				"tls.certs.selfSigner.enabled":     "true",
				"tls.certs.selfSigner.rotateCerts": "false",
			},
		},
	}

	for _, testCase := range testCases {
		// Here, we capture the range variable and force it into the scope of this block. If we don't do this, when the
		// subtest switches contexts (because of t.Parallel), the testCase value will have been updated by the for loop
		// and will be the next testCase!
		testCase := testCase
		t.Run(testCase.name, func(subT *testing.T) {
			subT.Parallel()

			// Now we try rendering the template, but verify we get an error
			options := &helm.Options{SetValues: testCase.values}
			_, err := helm.RenderTemplateE(t, options, helmChartPath, releaseName, []string{"templates/cronjob-ca-certSelfSigner.yaml"})
			require.Error(t, err)
			require.Contains(t, err.Error(), "Error: could not find template templates/cronjob-ca-certSelfSigner.yaml in chart")

			_, err = helm.RenderTemplateE(t, options, helmChartPath, releaseName, []string{"templates/cronjob-client-certSelfSigner.yaml"})
			require.Error(t, err)
			require.Contains(t, err.Error(), "Error: could not find template templates/cronjob-client-certSelfSigner.yaml in chart")
		})
	}
}

// TestHelmSelfCertSignerCronJobSchedule contains the tests around the cronjob schedule of self signer utility
func TestHelmSelfCertSignerCronJobSchedule(t *testing.T) {
	t.Parallel()

	// Path to the helm chart we will test
	helmChartPath, err := filepath.Abs("../../cockroachdb")
	require.NoError(t, err)

	// Setup the args. For this test, we will set the following input values:
	options := &helm.Options{
		KubectlOptions: k8s.NewKubectlOptions("", "", namespaceName),
	}

	// Rendering the template of self signer service account
	output := helm.RenderTemplate(t, options, helmChartPath, releaseName, []string{"templates/cronjob-ca-certSelfSigner.yaml"})

	var cronjob v1beta1.CronJob
	helm.UnmarshalK8SYaml(t, output, &cronjob)
	require.Equal(t, namespaceName, cronjob.Namespace)

	// Rendering the template of self signer cert rotation job
	output = helm.RenderTemplate(t, options, helmChartPath, releaseName, []string{"templates/cronjob-client-certSelfSigner.yaml"})
	helm.UnmarshalK8SYaml(t, output, &cronjob)
	require.Equal(t, namespaceName, cronjob.Namespace)

	testCases := []struct {
		name   string
		values map[string]string
		caExpectedCron string
		clientExpectedCron string
	}{
		{
			"Validate cron schedule of Self Signer cert rotate jobs",
			map[string]string{},
			"0 0 0 */10 */4",
			"0 0 */26 * *",
		},
		{
			"Validate cron schedule of Self Signer cert rotate jobs with a different schedule than default schedule",
			map[string]string{
				"tls.certs.selfSigner.minimumCertDuration": "24h",
				"tls.certs.selfSigner.caCertDuration": "720h",
				"tls.certs.selfSigner.caCertExpiryWindow": "48h",
				"tls.certs.selfSigner.clientCertDuration": "240h",
				"tls.certs.selfSigner.clientCertExpiryWindow": "24h",
				"tls.certs.selfSigner.nodeCertDuration": "440h",
				"tls.certs.selfSigner.nodeCertExpiryWindow": "36h",
			},
			"0 0 */28 * *",
			"0 0 */1 * *",
		},
	}

	for _, testCase := range testCases {
		// Here, we capture the range variable and force it into the scope of this block. If we don't do this, when the
		// subtest switches contexts (because of t.Parallel), the testCase value will have been updated by the for loop
		// and will be the next testCase!
		testCase := testCase
		t.Run(testCase.name, func(subT *testing.T) {
			subT.Parallel()

			// Now we try rendering the template, but verify we get an error
			options := &helm.Options{SetValues: testCase.values}
			output := helm.RenderTemplate(t, options, helmChartPath, releaseName, []string{"templates/cronjob-ca-certSelfSigner.yaml"})

			var cronjob v1beta1.CronJob
			helm.UnmarshalK8SYaml(t, output, &cronjob)

			require.Equal(subT, cronjob.Spec.Schedule, testCase.caExpectedCron)

			output = helm.RenderTemplate(t, options, helmChartPath, releaseName, []string{"templates/cronjob-client-certSelfSigner.yaml"})
			helm.UnmarshalK8SYaml(t, output, &cronjob)

			require.Equal(subT, cronjob.Spec.Schedule, testCase.clientExpectedCron)
		})
	}
}

// TestHelmSelfCertSignerStatefulSet contains the tests around the statefulset of self signer utility
func TestHelmSelfCertSignerStatefulSet(t *testing.T) {
	t.Parallel()

	var statefulset appsv1.StatefulSet
	var job batchv1.Job
	// Path to the helm chart we will test
	helmChartPath, err := filepath.Abs("../../cockroachdb")
	require.NoError(t, err)

	testCases := []struct {
		name   string
		values map[string]string
		expect string
	}{
		{
			"Self Signer enable",
			map[string]string{"tls.certs.selfSigner.enabled": "true"},
			"copy-certs",
		},
		{
			"Self Signer disable",
			map[string]string{"tls.certs.selfSigner.enabled": "false"},
			"init-certs",
		},
	}

	for _, testCase := range testCases {
		// Here, we capture the range variable and force it into the scope of this block. If we don't do this, when the
		// subtest switches contexts (because of t.Parallel), the testCase value will have been updated by the for loop
		// and will be the next testCase!
		testCase := testCase
		t.Run(testCase.name, func(subT *testing.T) {
			subT.Parallel()

			// Now we try rendering the template, but verify we get an error
			options := &helm.Options{
				KubectlOptions: k8s.NewKubectlOptions("", "", namespaceName),
				SetValues:      testCase.values,
			}
			output := helm.RenderTemplate(t, options, helmChartPath, releaseName, []string{"templates/statefulset.yaml"})

			helm.UnmarshalK8SYaml(t, output, &statefulset)
			require.Equal(t, namespaceName, statefulset.Namespace)
			require.Equal(t, 1, len(statefulset.Spec.Template.Spec.InitContainers))
			require.Equal(t, testCase.expect, statefulset.Spec.Template.Spec.InitContainers[0].Name)

			output = helm.RenderTemplate(t, options, helmChartPath, releaseName, []string{"templates/job.init.yaml"})

			helm.UnmarshalK8SYaml(t, output, &job)
			require.Equal(t, namespaceName, job.Namespace)
			require.Equal(t, 1, len(job.Spec.Template.Spec.InitContainers))
			require.Equal(t, testCase.expect, job.Spec.Template.Spec.InitContainers[0].Name)
		})
	}
}

// TestSelfSignerHelmValidation contains the validations around the self-signer utility inputs
func TestSelfSignerHelmValidation(t *testing.T) {
	t.Parallel()

	// Path to the helm chart we will test
	helmChartPath, err := filepath.Abs("../../cockroachdb")
	require.NoError(t, err)

	testCases := []struct {
		name   string
		values map[string]string
		expect string
	}{
		{
			"Input validations for taking duration input as hours only",
			map[string]string{"tls.certs.selfSigner.caCertDuration": "1d"},
			"tls.certs.selfSigner.caCertDuration: Does not match pattern '^[0-9]*h$'",
		},
		{
			"CA cert duration is empty",
			map[string]string{"tls.certs.selfSigner.caCertDuration": ""},
			"values don't meet the specifications of the schema",
		},
		{
			"CA cert expiration window less than minimumCertDuration",
			map[string]string{
				"tls.certs.selfSigner.caCertDuration":      "20h",
				"tls.certs.selfSigner.caCertExpiryWindow":  "5h",
				"tls.certs.selfSigner.minimumCertDuration": "6h",
			},
			"CA cert expiration window should not be less than minimum Cert duration",
		},
		{
			"Node cert duration is empty",
			map[string]string{"tls.certs.selfSigner.nodeCertDuration": ""},
			"values don't meet the specifications of the schema",
		},
		{
			"Node cert duration minus Node cert expiry is less than the minimumCertDuration",
			map[string]string{
				"tls.certs.selfSigner.nodeCertDuration":     "20h",
				"tls.certs.selfSigner.nodeCertExpiryWindow": "5h",
				"tls.certs.selfSigner.minimumCertDuration":  "16h",
			},
			"Node cert duration minus node cert expiry window should not be less than minimum Cert duration",
		},
		{
			"Client cert duration is empty",
			map[string]string{"tls.certs.selfSigner.clientCertDuration": ""},
			"values don't meet the specifications of the schema",
		},
		{
			"Client cert duration minus client expiry is less than minimumCertDuration",
			map[string]string{
				"tls.certs.selfSigner.clientCertDuration":     "20h",
				"tls.certs.selfSigner.clientCertExpiryWindow": "5h",
				"tls.certs.selfSigner.minimumCertDuration":    "21h",
			},
			"Client cert duration minus client cert expiry window should not be less than minimum Cert duration",
		},
		{
			"caProvided is enabled",
			map[string]string{"tls.certs.selfSigner.caProvided": "true"},
			"CA secret can't be empty if caProvided is set to true",
		},
		{
			"caProvided is enabled with secret name",
			map[string]string{
				"tls.certs.selfSigner.caProvided":     "true",
				"tls.certs.selfSigner.caSecret":       "test-secret",
				"tls.certs.selfSigner.caCertDuration": "",
			},
			"CA secret is not present in the release namespace",
		},
	}

	for _, testCase := range testCases {
		// Here, we capture the range variable and force it into the scope of this block. If we don't do this, when the
		// subtest switches contexts (because of t.Parallel), the testCase value will have been updated by the for loop
		// and will be the next testCase!
		testCase := testCase
		t.Run(testCase.name, func(subT *testing.T) {
			subT.Parallel()

			// Now we try rendering the template, but verify we get an error
			options := &helm.Options{
				KubectlOptions: k8s.NewKubectlOptions("", "", namespaceName),
				SetValues:      testCase.values,
			}
			_, err = helm.RenderTemplateE(t, options, helmChartPath, releaseName, []string{"templates/serviceaccount-certSelfSigner.yaml"})

			require.Error(t, err)
			require.Contains(t, err.Error(), testCase.expect)
		})
	}
}
