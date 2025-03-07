package template

import (
	"path/filepath"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	"k8s.io/api/batch/v1beta1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"

	"github.com/gruntwork-io/terratest/modules/helm"
	"github.com/gruntwork-io/terratest/modules/k8s"
	"github.com/gruntwork-io/terratest/modules/random"
	monitoring "github.com/prometheus-operator/prometheus-operator/pkg/apis/monitoring/v1"
	"github.com/stretchr/testify/require"
)

var (
	err           error
	helmChartPath string
	releaseName   = "helm-basic"
	namespaceName = "crdb-" + strings.ToLower(random.UniqueId())
)

func init() {
	helmChartPath, err = filepath.Abs("../../cockroachdb")
	if err != nil {
		panic(err)
	}
}

// TestTLSEnable tests the enabling the TLS, you have to enable only one method of TLS certs
func TestTLSEnable(t *testing.T) {
	t.Parallel()
	testCases := []struct {
		name   string
		values map[string]string
		expect string
	}{
		{
			"Self Signer and cert manager set to false",
			map[string]string{
				"tls.certs.selfSigner.enabled": "false",
				"tls.certs.certManager":        "false",
				"operator.enabled":             "false",
			},
			"You have to enable either self signed certificates or certificate manager, if you have enabled tls",
		},
		{
			"Self Signer and cert manager set to true",
			map[string]string{
				"tls.certs.selfSigner.enabled": "true",
				"tls.certs.certManager":        "true",
				"operator.enabled":             "false",
			},
			"Can not enable the self signed certificates and certificate manager at the same time",
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
			require.Contains(t, err.Error(), testCase.expect)
		})
	}
}

// TestHelmSelfCertSignerServiceAccount contains the tests around the service account of self signer utility
func TestHelmSelfCertSignerServiceAccount(t *testing.T) {
	t.Parallel()

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
			"tls.certs.certManager":        "true",
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
			"tls.certs.certManager":        "true",
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
			"tls.certs.certManager":        "true",
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
			"tls.certs.certManager":        "true",
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
	output = helm.RenderTemplate(t, options, helmChartPath, releaseName, []string{"templates/cronjob-client-node-certSelfSigner.yaml"})
	helm.UnmarshalK8SYaml(t, output, &cronjob)
	require.Equal(t, namespaceName, cronjob.Namespace)

	testCases := []struct {
		name   string
		values map[string]string
	}{
		{
			"Self Signer disable",
			map[string]string{
				"tls.certs.selfSigner.enabled": "false",
				"tls.certs.certManager":        "true",
			},
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

			_, err = helm.RenderTemplateE(t, options, helmChartPath, releaseName, []string{"templates/cronjob-client-node-certSelfSigner.yaml"})
			require.Error(t, err)
			require.Contains(t, err.Error(), "Error: could not find template templates/cronjob-client-node-certSelfSigner.yaml in chart")
		})
	}
}

// TestHelmSelfCertSignerCronJobSchedule contains the tests around the cronjob schedule of self signer utility
func TestHelmSelfCertSignerCronJobSchedule(t *testing.T) {
	t.Parallel()

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
	output = helm.RenderTemplate(t, options, helmChartPath, releaseName, []string{"templates/cronjob-client-node-certSelfSigner.yaml"})
	helm.UnmarshalK8SYaml(t, output, &cronjob)
	require.Equal(t, namespaceName, cronjob.Namespace)

	testCases := []struct {
		name               string
		values             map[string]string
		caExpectedCron     string
		clientExpectedCron string
	}{
		{
			"Validate cron schedule of Self Signer cert rotate jobs",
			map[string]string{},
			"0 0 1 */11 *",
			"0 0 */26 * *",
		},
		{
			"Validate cron schedule of Self Signer cert rotate jobs with a different schedule than default schedule",
			map[string]string{
				"tls.certs.selfSigner.minimumCertDuration":    "24h",
				"tls.certs.selfSigner.caCertDuration":         "720h",
				"tls.certs.selfSigner.caCertExpiryWindow":     "48h",
				"tls.certs.selfSigner.clientCertDuration":     "240h",
				"tls.certs.selfSigner.clientCertExpiryWindow": "24h",
				"tls.certs.selfSigner.nodeCertDuration":       "440h",
				"tls.certs.selfSigner.nodeCertExpiryWindow":   "36h",
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

			output = helm.RenderTemplate(t, options, helmChartPath, releaseName, []string{"templates/cronjob-client-node-certSelfSigner.yaml"})
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

	testCases := []struct {
		name   string
		values map[string]string
		expect string
	}{
		{
			"Self Signer enable",
			map[string]string{
				"tls.certs.selfSigner.enabled": "true",
				"operator.enabled":             "false",
			},
			"copy-certs",
		},
		{
			"Self Signer disable",
			map[string]string{
				"tls.certs.selfSigner.enabled": "false",
				"tls.certs.certManager":        "true",
				"operator.enabled":             "false",
			},
			"copy-certs",
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

// TestHelmLogConfigFileStatefulSet contains the tests around the new logging configuration
func TestHelmLogConfigFileStatefulSet(t *testing.T) {
	t.Parallel()

	type expect struct {
		statefulsetArgument     string
		logConfig               string
		secretExists            bool
		renderErr               string
		persistentVolumeCreated bool
	}

	testCases := []struct {
		name   string
		values map[string]string
		expect expect
	}{
		{
			"New logging configuration enabled",
			map[string]string{
				"conf.log.enabled": "true",
				"operator.enabled": "false",
			},
			expect{
				"--log-config-file=/cockroach/log-config/log-config.yaml",
				"",
				true,
				"",
				false,
			},
		},
		{
			"New logging configuration overridden",
			map[string]string{
				"conf.log.enabled":                  "true",
				"conf.log.config.file-defaults.dir": "/cockroach/cockroach-logs",
				"operator.enabled":                  "false",
			},
			expect{
				"--log-config-file=/cockroach/log-config/log-config.yaml",
				"file-defaults:\n  dir: /cockroach/cockroach-logs",
				true,
				"",
				false,
			},
		},
		{
			"New logging configuration disabled",
			map[string]string{
				"conf.log.enabled": "false",
				"conf.logtostderr": "INFO",
				"operator.enabled": "false",
			},
			expect{
				"--logtostderr=INFO",
				"",
				false,
				"",
				false,
			},
		},
		{
			"New logging configuration disabled, but persistent volume enabled",
			map[string]string{
				"conf.log.enabled":                  "false",
				"conf.logtostderr":                  "INFO",
				"conf.log.persistentVolume.enabled": "true",
				"operator.enabled":                  "false",
			},
			expect{
				"--logtostderr=INFO",
				"",
				false,
				"Persistent volume for logs can only be enabled if logging is enabled",
				false,
			},
		},
		{
			"New logging configuration not using persistent volume when enabled",
			map[string]string{
				"conf.log.enabled":                  "true",
				"conf.log.config.file-defaults.dir": "/wrong/path",
				"conf.log.persistentVolume.enabled": "true",
				"operator.enabled":                  "false",
			},
			expect{
				"",
				"",
				false,
				"Log configuration should use the persistent volume if enabled",
				false,
			},
		},
		{
			"New logging configuration using the persistent volume",
			map[string]string{
				"conf.log.enabled":                  "true",
				"conf.log.config.file-defaults.dir": "/cockroach/cockroach-logs",
				"conf.log.persistentVolume.enabled": "true",
				"operator.enabled":                  "false",
			},
			expect{
				"--log-config-file=/cockroach/log-config/log-config.yaml",
				"file-defaults:\n  dir: /cockroach/cockroach-logs",
				true,
				"",
				true,
			},
		},
	}

	for _, testCase := range testCases {
		var statefulset appsv1.StatefulSet
		var secret corev1.Secret

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

			output, err := helm.RenderTemplateE(t, options, helmChartPath, releaseName, []string{"templates/statefulset.yaml"})
			if err != nil {
				require.ErrorContains(subT, err, testCase.expect.renderErr)
				return
			} else {
				require.Empty(subT, testCase.expect.renderErr)
			}

			helm.UnmarshalK8SYaml(t, output, &statefulset)

			require.Equal(subT, namespaceName, statefulset.Namespace)
			require.Contains(t, statefulset.Spec.Template.Spec.Containers[0].Args[2], testCase.expect.statefulsetArgument)

			if testCase.expect.persistentVolumeCreated {
				// Expect 2 persistent volumes: data, logs
				require.Equal(subT, 2, len(statefulset.Spec.VolumeClaimTemplates))
				require.Equal(subT, "logsdir", statefulset.Spec.VolumeClaimTemplates[1].Name)
			} else {
				// Expect 1 persistent volume: data
				require.Equal(subT, 1, len(statefulset.Spec.VolumeClaimTemplates))
			}

			output, err = helm.RenderTemplateE(t, options, helmChartPath, releaseName, []string{"templates/secret.logconfig.yaml"})
			require.Equal(subT, testCase.expect.secretExists, err == nil)

			if testCase.expect.secretExists {
				helm.UnmarshalK8SYaml(t, output, &secret)
				require.Equal(subT, namespaceName, secret.Namespace)
				require.Contains(subT, secret.StringData["log-config.yaml"], testCase.expect.logConfig)
			}
		})
	}
}

// TestHelmDatabaseProvisioning contains the tests around the cluster init and provisioning
func TestHelmDatabaseProvisioning(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name   string
		values map[string]string
		expect struct {
			job struct {
				exists           bool
				hookDeletePolicy string
				initCluster      bool
				provisionCluster bool
				sql              string
			}
			secret struct {
				exists          bool
				users           map[string]string
				clusterSettings map[string]string
			}
		}
	}{
		{
			"Disabled provisioning",
			map[string]string{
				"init.provisioning.enabled": "false",
				"operator.enabled":          "false",
			},
			struct {
				job struct {
					exists           bool
					hookDeletePolicy string
					initCluster      bool
					provisionCluster bool
					sql              string
				}
				secret struct {
					exists          bool
					users           map[string]string
					clusterSettings map[string]string
				}
			}{
				struct {
					exists           bool
					hookDeletePolicy string
					initCluster      bool
					provisionCluster bool
					sql              string
				}{
					true,
					"before-hook-creation",
					true,
					false,
					"",
				},
				struct {
					exists          bool
					users           map[string]string
					clusterSettings map[string]string
				}{
					false,
					nil,
					nil,
				},
			},
		},
		{
			"Enabled empty provisioning",
			map[string]string{
				"init.provisioning.enabled": "true",
				"operator.enabled":          "false",
			},
			struct {
				job struct {
					exists           bool
					hookDeletePolicy string
					initCluster      bool
					provisionCluster bool
					sql              string
				}
				secret struct {
					exists          bool
					users           map[string]string
					clusterSettings map[string]string
				}
			}{
				struct {
					exists           bool
					hookDeletePolicy string
					initCluster      bool
					provisionCluster bool
					sql              string
				}{
					true,
					"before-hook-creation",
					true,
					true,
					"",
				},
				struct {
					exists          bool
					users           map[string]string
					clusterSettings map[string]string
				}{
					true,
					nil,
					nil,
				},
			},
		},
		{
			"Users provisioning",
			map[string]string{
				"init.provisioning.enabled":             "true",
				"init.provisioning.users[0].name":       "testUser",
				"init.provisioning.users[0].password":   "testPassword",
				"init.provisioning.users[0].options[0]": "CREATEROLE",
				"operator.enabled":                      "false",
			},
			struct {
				job struct {
					exists           bool
					hookDeletePolicy string
					initCluster      bool
					provisionCluster bool
					sql              string
				}
				secret struct {
					exists          bool
					users           map[string]string
					clusterSettings map[string]string
				}
			}{
				struct {
					exists           bool
					hookDeletePolicy string
					initCluster      bool
					provisionCluster bool
					sql              string
				}{
					true,
					"before-hook-creation",
					true,
					true,
					"CREATE USER IF NOT EXISTS testUser WITH PASSWORD '$testUser_PASSWORD' CREATEROLE;",
				},
				struct {
					exists          bool
					users           map[string]string
					clusterSettings map[string]string
				}{true,
					map[string]string{
						"testUser": "testPassword",
					},
					nil,
				},
			},
		},
		{
			"Database provisioning",
			map[string]string{
				"init.provisioning.enabled":                 "true",
				"init.provisioning.databases[0].name":       "testDatabase",
				"init.provisioning.databases[0].options[0]": "encoding='utf-8'",
				"operator.enabled":                          "false",
			},
			struct {
				job struct {
					exists           bool
					hookDeletePolicy string
					initCluster      bool
					provisionCluster bool
					sql              string
				}
				secret struct {
					exists          bool
					users           map[string]string
					clusterSettings map[string]string
				}
			}{
				struct {
					exists           bool
					hookDeletePolicy string
					initCluster      bool
					provisionCluster bool
					sql              string
				}{
					true,
					"before-hook-creation",
					true,
					true,
					"CREATE DATABASE IF NOT EXISTS testDatabase encoding='utf-8';",
				},
				struct {
					exists          bool
					users           map[string]string
					clusterSettings map[string]string
				}{
					true,
					nil,
					nil,
				},
			},
		},
		{
			"Users with database granted provisioning",
			map[string]string{
				"init.provisioning.enabled":                "true",
				"init.provisioning.users[0].name":          "testUser",
				"init.provisioning.users[0].password":      "testPassword",
				"init.provisioning.databases[0].name":      "testDatabase",
				"init.provisioning.databases[0].owners[0]": "testUser",
				"operator.enabled":                         "false",
			},
			struct {
				job struct {
					exists           bool
					hookDeletePolicy string
					initCluster      bool
					provisionCluster bool
					sql              string
				}
				secret struct {
					exists          bool
					users           map[string]string
					clusterSettings map[string]string
				}
			}{
				struct {
					exists           bool
					hookDeletePolicy string
					initCluster      bool
					provisionCluster bool
					sql              string
				}{
					true,
					"before-hook-creation",
					true,
					true,
					"CREATE USER IF NOT EXISTS testUser WITH PASSWORD '$testUser_PASSWORD';" +
						"CREATE DATABASE IF NOT EXISTS testDatabase;" +
						"GRANT ALL ON DATABASE testDatabase TO testUser;",
				},
				struct {
					exists          bool
					users           map[string]string
					clusterSettings map[string]string
				}{
					true,
					map[string]string{
						"testUser": "testPassword",
					},
					nil,
				},
			},
		},
		{
			"Cluster settings provisioning",
			map[string]string{
				"init.provisioning.enabled":                                "true",
				"init.provisioning.clusterSettings.cluster\\.organization": "testOrganization",
				"init.provisioning.clusterSettings.enterprise\\.license":   "testLicense",
				"operator.enabled":                                         "false",
			},
			struct {
				job struct {
					exists           bool
					hookDeletePolicy string
					initCluster      bool
					provisionCluster bool
					sql              string
				}
				secret struct {
					exists          bool
					users           map[string]string
					clusterSettings map[string]string
				}
			}{
				struct {
					exists           bool
					hookDeletePolicy string
					initCluster      bool
					provisionCluster bool
					sql              string
				}{
					true,
					"before-hook-creation",
					true,
					true,
					"SET CLUSTER SETTING cluster.organization = '$cluster_organization_CLUSTER_SETTING';" +
						"SET CLUSTER SETTING enterprise.license = '$enterprise_license_CLUSTER_SETTING';",
				},
				struct {
					exists          bool
					users           map[string]string
					clusterSettings map[string]string
				}{
					true,
					nil,
					map[string]string{
						"cluster-organization": "testOrganization",
						"enterprise-license":   "testLicense",
					},
				},
			},
		},
		{
			"Database backup provisioning",
			map[string]string{
				"init.provisioning.enabled":                                 "true",
				"init.provisioning.databases[0].name":                       "testDatabase",
				"init.provisioning.databases[0].backup.into":                "s3://backups/testDatabase?AWS_ACCESS_KEY_ID=minioadmin&AWS_ENDPOINT=http://minio.minio:80&AWS_REGION=us-east-1&AWS_SECRET_ACCESS_KEY=minioadmin",
				"init.provisioning.databases[0].backup.options[0]":          "revision_history",
				"init.provisioning.databases[0].backup.recurring":           "@always",
				"init.provisioning.databases[0].backup.fullBackup":          "@daily",
				"init.provisioning.databases[0].backup.schedule.options[0]": "first_run = 'now'",
				"operator.enabled":                                          "false",
			},
			struct {
				job struct {
					exists           bool
					hookDeletePolicy string
					initCluster      bool
					provisionCluster bool
					sql              string
				}
				secret struct {
					exists          bool
					users           map[string]string
					clusterSettings map[string]string
				}
			}{
				struct {
					exists           bool
					hookDeletePolicy string
					initCluster      bool
					provisionCluster bool
					sql              string
				}{
					true,
					"before-hook-creation",
					true,
					true,
					"CREATE DATABASE IF NOT EXISTS testDatabase;" +
						"CREATE SCHEDULE IF NOT EXISTS testDatabase_scheduled_backup" +
						"FOR BACKUP DATABASE testDatabase INTO 's3://backups/testDatabase?AWS_ACCESS_KEY_ID=minioadmin&AWS_ENDPOINT=http://minio.minio:80&AWS_REGION=us-east-1&AWS_SECRET_ACCESS_KEY=minioadmin'" +
						"WITH revision_history" +
						"RECURRING '@always'" +
						"FULL BACKUP '@daily'" +
						"WITH SCHEDULE OPTIONS first_run = 'now';",
				},
				struct {
					exists          bool
					users           map[string]string
					clusterSettings map[string]string
				}{
					true,
					nil,
					nil,
				},
			},
		},
	}

	for _, testCase := range testCases {
		var job batchv1.Job
		var secret corev1.Secret

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
			output, err := helm.RenderTemplateE(t, options, helmChartPath, releaseName, []string{"templates/job.init.yaml"})

			require.Equal(subT, testCase.expect.job.exists, err == nil)

			if testCase.expect.job.exists {
				helm.UnmarshalK8SYaml(t, output, &job)
				require.Equal(subT, job.Namespace, namespaceName)

				require.Equal(subT, job.Annotations["helm.sh/hook-delete-policy"], testCase.expect.job.hookDeletePolicy)

				initJobCommand := job.Spec.Template.Spec.Containers[0].Command[2]

				if testCase.expect.job.initCluster {
					require.Contains(subT, initJobCommand, "initCluster()")
					require.Contains(subT, initJobCommand, "initCluster;")
				} else {
					require.NotContains(subT, initJobCommand, "initCluster()")
					require.NotContains(subT, initJobCommand, "initCluster;")
				}

				if testCase.expect.job.provisionCluster {
					require.Contains(subT, initJobCommand, "provisionCluster()")
					require.Contains(subT, initJobCommand, "provisionCluster;")

					// Stripping all whitespaces and new lines
					preparedSql := strings.ReplaceAll(strings.ReplaceAll(initJobCommand, " ", ""), "\n", "")
					expectedSql := strings.ReplaceAll(strings.ReplaceAll(testCase.expect.job.sql, " ", ""), "\n", "")

					require.Contains(subT, preparedSql, expectedSql)
				} else {
					require.NotContains(subT, initJobCommand, "provisionCluster()")
					require.NotContains(subT, initJobCommand, "provisionCluster;")
				}
			}

			output, err = helm.RenderTemplateE(t, options, helmChartPath, releaseName, []string{"templates/secrets.init.yaml"})

			require.Equal(subT, testCase.expect.secret.exists, err == nil)

			if testCase.expect.secret.exists {
				helm.UnmarshalK8SYaml(t, output, &secret)

				require.Equal(subT, secret.Namespace, namespaceName)

				for username, password := range testCase.expect.secret.users {
					require.Equal(subT, secret.StringData[username+"-password"], password)
				}

				for name, value := range testCase.expect.secret.clusterSettings {
					require.Equal(subT, secret.StringData[name+"-cluster-setting"], value)
				}
			}
		})
	}
}

func TestHelmServiceMonitor(t *testing.T) {
	t.Parallel()
	testCases := []struct {
		name       string
		values     map[string]string
		namespaced bool
	}{
		{
			"All namespaces are selected",
			map[string]string{
				"serviceMonitor.enabled":    "true",
				"serviceMonitor.namespaced": "false",
			},
			false,
		},
		{
			"Current namespace is selected",
			map[string]string{
				"serviceMonitor.enabled":    "true",
				"serviceMonitor.namespaced": "true",
			},
			true,
		},
	}

	for _, testCase := range testCases {
		// Here, we capture the range variable and force it into the scope of this block. If we don't do this, when the
		// subtest switches contexts (because of t.Parallel), the testCase value will have been updated by the for loop
		// and will be the next testCase!
		testCase := testCase
		t.Run(testCase.name, func(subT *testing.T) {
			subT.Parallel()

			options := &helm.Options{
				KubectlOptions: k8s.NewKubectlOptions("", "", namespaceName),
				SetValues:      testCase.values,
			}
			output := helm.RenderTemplate(t, options, helmChartPath, releaseName, []string{"templates/serviceMonitor.yaml"})

			var monitor monitoring.ServiceMonitor
			helm.UnmarshalK8SYaml(t, output, &monitor)

			require.Equal(t, monitor.Spec.NamespaceSelector.Any, !testCase.namespaced)
			if testCase.namespaced {
				require.Len(t, monitor.Spec.NamespaceSelector.MatchNames, 1)
				require.Contains(t, monitor.Spec.NamespaceSelector.MatchNames, namespaceName)
			} else {
				require.Empty(t, monitor.Spec.NamespaceSelector.MatchNames)
			}
		})
	}
}

// TestHelmSecretBackendConfig tests the secret.backendconfig template
func TestHelmSecretBackendConfig(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name   string
		values map[string]string
		expect string
	}{
		{
			"IAP enabled and clientId empty",
			map[string]string{
				"iap.enabled":      "true",
				"iap.clientId":     "",
				"iap.clientSecret": "notempty",
			},
			"iap.clientID can't be empty if iap.enabled is set to true",
		},
		{
			"IAP enabled and clientSecret empty",
			map[string]string{
				"iap.enabled":      "true",
				"iap.clientId":     "notempty",
				"iap.clientSecret": "",
			},
			"iap.clientSecret can't be empty if iap.enabled is set to true",
		},
		{
			"IAP enabled and both clientId and clientSecret set",
			map[string]string{
				"iap.enabled":      "true",
				"iap.clientId":     "myclientid",
				"iap.clientSecret": "myclientsecret",
			},
			"",
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
			output, err := helm.RenderTemplateE(subT, options, helmChartPath, releaseName, []string{"templates/secret.backendconfig.yaml"})

			if testCase.expect != "" {
				require.Error(subT, err)
				require.Contains(subT, err.Error(), testCase.expect)
			} else {

				require.Nil(t, err)

				var secret corev1.Secret
				helm.UnmarshalK8SYaml(t, output, &secret)

				require.Equal(t, string(secret.Data["client_id"]), testCase.values["iap.clientId"])
				require.Equal(t, string(secret.Data["client_secret"]), testCase.values["iap.clientSecret"])
			}
		})
	}
}

// TestHelmBackendConfig tests the backendconfig template
func TestHelmBackendConfig(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name   string
		values map[string]string
	}{
		{
			"IAP enabled",
			map[string]string{
				"iap.enabled":      "true",
				"iap.clientId":     "notempty",
				"iap.clientSecret": "notempty",
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
			_, err := helm.RenderTemplateE(subT, options, helmChartPath, releaseName, []string{"templates/backendconfig.yaml"})
			require.Nil(subT, err)
		})
	}
}

// TestHelmIngress tests the ingress template
func TestHelmIngress(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name             string
		values           map[string]string
		expectedPathType networkingv1.PathType
	}{
		{
			"Ingress enabled",
			map[string]string{
				"ingress.enabled":  "true",
				"iap.clientId":     "notempty",
				"iap.clientSecret": "notempty",
			},
			networkingv1.PathTypePrefix,
		},
		{
			"Ingress and IAP enabled",
			map[string]string{
				"ingress.enabled":  "true",
				"ingress.paths":    "{/*}",
				"iap.enabled":      "true",
				"iap.clientId":     "notempty",
				"iap.clientSecret": "notempty",
			},
			networkingv1.PathTypeImplementationSpecific,
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
			output, err := helm.RenderTemplateE(t, options, helmChartPath, releaseName, []string{"templates/ingress.yaml"}, "--api-versions", "networking.k8s.io/v1/Ingress")

			require.Nil(t, err)

			var ingress networkingv1.Ingress
			helm.UnmarshalK8SYaml(t, output, &ingress)

			require.Equal(t, ingress.APIVersion, "networking.k8s.io/v1")

			for _, rule := range ingress.Spec.Rules {
				for _, path := range rule.HTTP.Paths {
					require.NotNil(t, path.PathType)
					require.Equal(t, *path.PathType, testCase.expectedPathType)
				}
			}
		})
	}
}

// TestHelmInitJobAnnotations contains the tests for the annotations of the Init Job
func TestHelmInitJobAnnotations(t *testing.T) {
	t.Parallel()
	testCases := []struct {
		name        string
		values      map[string]string
		annotations map[string]string
	}{
		{
			"No extra job annotations were supplied",
			map[string]string{
				"operator.enabled": "false",
			},
			map[string]string{
				"helm.sh/hook":               "post-install,post-upgrade",
				"helm.sh/hook-delete-policy": "before-hook-creation",
			},
		},
		{
			"Extra job annotations were supplied",
			map[string]string{
				"init.jobAnnotations.test-key-1": "test-value-1",
				"init.jobAnnotations.test-key-2": "test-value-2",
				"operator.enabled":               "false",
			},
			map[string]string{
				"helm.sh/hook":               "post-install,post-upgrade",
				"helm.sh/hook-delete-policy": "before-hook-creation",
				"test-key-1":                 "test-value-1",
				"test-key-2":                 "test-value-2",
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

			options := &helm.Options{
				KubectlOptions: k8s.NewKubectlOptions("", "", namespaceName),
				SetValues:      testCase.values,
			}
			output, err := helm.RenderTemplateE(t, options, helmChartPath, releaseName, []string{"templates/job.init.yaml"})

			require.Equal(subT, err, nil)

			var job batchv1.Job
			helm.UnmarshalK8SYaml(t, output, &job)

			require.Equal(t, testCase.annotations, job.Annotations)
		})
	}
}

func TestStatefulSetInitContainers(t *testing.T) {
	t.Parallel()
	testCases := []struct {
		name          string
		initContainer bool
		volume        bool
		values        map[string]string
	}{
		{
			"Add extra init container",
			true,
			true,
			map[string]string{
				"statefulset.initContainers[0].name":       "fetch-metadata",
				"statefulset.initContainers[0].image":      "busybox",
				"statefulset.initContainers[0].command[0]": "/bin/bash",
				"statefulset.initContainers[0].command[1]": "-c",
				"statefulset.initContainers[0].command[2]": "echo 'Fetching metadata'",
				"statefulset.volumeMounts[0].name":         "metadata",
				"statefulset.volumeMounts[0].mountPath":    "/metadata",
				"statefulset.volumes[0].name":              "metadata",
				"statefulset.volumes[0].configMap.name":    "log-config",
				"operator.enabled":                         "false",
			},
		},
		{
			"Add extra volume without init container",
			false,
			true,
			map[string]string{
				"statefulset.volumeMounts[0].name":      "metadata",
				"statefulset.volumeMounts[0].mountPath": "/metadata",
				"statefulset.volumes[0].name":           "metadata",
				"statefulset.volumes[0].configMap.name": "log-config",
				"operator.enabled":                      "false",
			},
		},
		{
			"Add extra init container without volume",
			true,
			false,
			map[string]string{
				"statefulset.initContainers[0].name":       "fetch-metadata",
				"statefulset.initContainers[0].image":      "busybox",
				"statefulset.initContainers[0].command[0]": "/bin/bash",
				"statefulset.initContainers[0].command[1]": "-c",
				"statefulset.initContainers[0].command[2]": "echo 'Fetching metadata'",
				"operator.enabled":                         "false",
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

			options := &helm.Options{
				KubectlOptions: k8s.NewKubectlOptions("", "", namespaceName),
				SetValues:      testCase.values,
			}
			output, err := helm.RenderTemplateE(t, options, helmChartPath, releaseName, []string{"templates/statefulset.yaml"})

			require.Equal(subT, err, nil)

			var sts appsv1.StatefulSet
			helm.UnmarshalK8SYaml(t, output, &sts)

			if testCase.initContainer {
				var fetchMetadata bool
				for _, c := range sts.Spec.Template.Spec.InitContainers {
					if c.Name == "fetch-metadata" {
						fetchMetadata = true
						require.Equal(subT, "busybox", c.Image)
						require.Equal(subT, []string{"/bin/bash", "-c", "echo 'Fetching metadata'"}, c.Command)
						if testCase.volume {
							require.Equal(subT, []corev1.VolumeMount{{Name: "metadata", MountPath: "/metadata"}}, c.VolumeMounts)
						}
						break
					}
				}

				if !fetchMetadata {
					require.Fail(subT, "Init container fetch-metadata not found")
				}
			}

			if testCase.volume {
				var metadataVolume bool
				for _, v := range sts.Spec.Template.Spec.Volumes {
					if v.Name == "metadata" {
						metadataVolume = true
						require.Equal(subT, v.ConfigMap.Name, "log-config")
						break
					}
				}

				if !metadataVolume {
					require.Fail(subT, "Volume metadata not found")
				}
			}
		})
	}

}

// TestHelmCockroachStartCmd tests the arguments to the cockroach start command.
func TestHelmCockroachStartCmd(t *testing.T) {
	t.Parallel()

	type expect struct {
		startCmd string
	}

	testCases := []struct {
		name   string
		values map[string]string
		expect expect
	}{
		{
			"start single node with default args",
			map[string]string{
				"conf.single-node": "true",
				"operator.enabled": "false",
			},
			expect{
				"exec /cockroach/cockroach start-single-node " +
					"--advertise-host=$(hostname).${STATEFULSET_FQDN} " +
					"--certs-dir=/cockroach/cockroach-certs/ " +
					"--http-port=8080 " +
					"--port=26257 " +
					"--cache=25% " +
					"--max-sql-memory=25% " +
					"--logtostderr=INFO",
			},
		},
		{
			"start single node with custom args",
			map[string]string{
				"conf.single-node":                 "true",
				"tls.enabled":                      "false",
				"conf.attrs":                       "gpu",
				"service.ports.http.port":          "8081",
				"service.ports.grpc.internal.port": "26258",
				"conf.cache":                       "10%",
				"conf.max-disk-temp-storage":       "1GB",
				"conf.max-offset":                  "100ms",
				"conf.max-sql-memory":              "10%",
				"conf.locality":                    "region=us-west-1",
				"conf.sql-audit-dir":               "/audit",
				"conf.log.enabled":                 "true",
				"operator.enabled":                 "false",
			},
			expect{
				"exec /cockroach/cockroach start-single-node " +
					"--advertise-host=$(hostname).${STATEFULSET_FQDN} " +
					"--insecure " +
					"--attrs=gpu " +
					"--http-port=8081 " +
					"--port=26258 " +
					"--cache=10% " +
					"--max-disk-temp-storage=1GB " +
					"--max-offset=100ms " +
					"--max-sql-memory=10% " +
					"--locality=region=us-west-1 " +
					"--sql-audit-dir=/audit " +
					"--log-config-file=/cockroach/log-config/log-config.yaml",
			},
		},
		{
			"start multiple node cluster with default args",
			map[string]string{
				"conf.join":        "1.1.1.1",
				"operator.enabled": "false",
			},
			expect{
				"exec /cockroach/cockroach start --join=1.1.1.1 " +
					"--advertise-host=$(hostname).${STATEFULSET_FQDN} " +
					"--certs-dir=/cockroach/cockroach-certs/ " +
					"--http-port=8080 " +
					"--port=26257 " +
					"--cache=25% " +
					"--max-sql-memory=25% " +
					"--logtostderr=INFO",
			},
		},
		{
			"start multiple node cluster with custom args",
			map[string]string{
				"conf.join":                              "1.1.1.1",
				"conf.cluster-name":                      "test",
				"conf.disable-cluster-name-verification": "true",
				"tls.enabled":                            "false",
				"conf.attrs":                             "gpu",
				"service.ports.http.port":                "8081",
				"service.ports.grpc.internal.port":       "26258",
				"conf.cache":                             "10%",
				"conf.max-disk-temp-storage":             "1GB",
				"conf.max-offset":                        "100ms",
				"conf.max-sql-memory":                    "10%",
				"conf.locality":                          "region=us-west-1",
				"conf.sql-audit-dir":                     "/audit",
				"conf.log.enabled":                       "true",
				"operator.enabled":                       "false",
			},
			expect{
				"exec /cockroach/cockroach start --join=1.1.1.1 " +
					"--cluster-name=test " +
					"--disable-cluster-name-verification " +
					"--advertise-host=$(hostname).${STATEFULSET_FQDN} " +
					"--insecure " +
					"--attrs=gpu " +
					"--http-port=8081 " +
					"--port=26258 " +
					"--cache=10% " +
					"--max-disk-temp-storage=1GB " +
					"--max-offset=100ms " +
					"--max-sql-memory=10% " +
					"--locality=region=us-west-1 " +
					"--sql-audit-dir=/audit " +
					"--log-config-file=/cockroach/log-config/log-config.yaml",
			},
		},
	}

	for _, testCase := range testCases {
		var statefulset appsv1.StatefulSet

		// Here, we capture the range variable and force it into the scope of this block.
		// If we don't do this, when the subtest switches contexts (because of t.Parallel),
		// the testCase value will have been updated by the for loop and will be the next testCase!
		testCase := testCase

		t.Run(testCase.name, func(subT *testing.T) {
			subT.Parallel()

			options := &helm.Options{
				KubectlOptions: k8s.NewKubectlOptions("", "", namespaceName),
				SetValues:      testCase.values,
			}

			output, err := helm.RenderTemplateE(
				t,
				options,
				helmChartPath,
				releaseName,
				[]string{"templates/statefulset.yaml"},
			)
			require.NoError(subT, err)

			helm.UnmarshalK8SYaml(t, output, &statefulset)

			require.Equal(subT, namespaceName, statefulset.Namespace)
			cmdString := statefulset.Spec.Template.Spec.Containers[0].Args[2]
			require.Equal(subT, testCase.expect.startCmd, cmdString)
			// Validate that there is no newline in the command due to improper template formatting.
			require.NotContains(subT, cmdString, "\n")
		})
	}
}

// TestHelmWALFailoverConfiguration contains the tests around WAL failover configuration.
func TestHelmWALFailoverConfiguration(t *testing.T) {
	t.Parallel()
	t.Logf("helm chart path: %s", helmChartPath)

	type expect struct {
		statefulsetArgument   string
		renderErr             string
		persistentVolumeNames []string
	}

	testCases := []struct {
		name   string
		values map[string]string
		expect expect
	}{
		{
			"WAL failover invalid configuration",
			map[string]string{
				"conf.wal-failover.value": "invalid",
				"operator.enabled":        "false",
			},
			expect{
				"",
				"Invalid WAL failover configuration value. Expected either of '', 'disabled', 'among-stores' or 'path=<path>'",
				[]string{"datadir"},
			},
		},
		{
			"WAL failover not configured",
			map[string]string{
				"conf.wal-failover.value": "",
				"conf.store.enabled":      "true",
				"conf.store.count":        "1",
				"operator.enabled":        "false",
			},
			expect{
				"--store=path=cockroach-data,size=100Gi",
				"",
				[]string{"datadir"},
			},
		},
		{
			"WAL failover among multiple stores",
			map[string]string{
				"conf.wal-failover.value": "among-stores",
				"conf.store.enabled":      "true",
				"conf.store.count":        "2",
				"operator.enabled":        "false",
			},
			expect{
				"--store=path=cockroach-data,size=100Gi " +
					"--store=path=cockroach-data-2,size=100Gi " +
					"--wal-failover=among-stores",
				"",
				[]string{"datadir", "datadir-2"},
			},
		},
		{
			"WAL failover disabled with multiple stores",
			map[string]string{
				"conf.wal-failover.value": "disabled",
				"conf.store.enabled":      "true",
				"conf.store.count":        "2",
				"operator.enabled":        "false",
			},
			expect{
				"--store=path=cockroach-data,size=100Gi " +
					"--store=path=cockroach-data-2,size=100Gi " +
					"--wal-failover=disabled",
				"",
				[]string{"datadir", "datadir-2"},
			},
		},
		{
			"WAL failover among stores but store disabled",
			map[string]string{
				"conf.wal-failover.value": "among-stores",
				"conf.store.enabled":      "false",
				"operator.enabled":        "false",
			},
			expect{
				"",
				"WAL failover among stores requires store enabled with count greater than 1",
				[]string{"datadir"},
			},
		},
		{
			"WAL failover among stores but single store",
			map[string]string{
				"conf.wal-failover.value": "among-stores",
				"conf.store.enabled":      "true",
				"conf.store.count":        "1",
				"operator.enabled":        "false",
			},
			expect{
				"",
				"WAL failover among stores requires store enabled with count greater than 1",
				[]string{"datadir"},
			},
		},
		{
			"WAL failover through side disk (absolute path)",
			map[string]string{
				"conf.wal-failover.value":                    "path=/cockroach/cockroach-failover/abc",
				"conf.wal-failover.persistentVolume.enabled": "true",
				"operator.enabled":                           "false",
			},
			expect{
				"--wal-failover=path=/cockroach/cockroach-failover/abc",
				"",
				[]string{"datadir", "failoverdir"},
			},
		},
		{
			"WAL failover through side disk (relative path)",
			map[string]string{
				"conf.wal-failover.value":                    "path=cockroach-failover/abc",
				"conf.wal-failover.persistentVolume.enabled": "true",
				"operator.enabled":                           "false",
			},
			expect{
				"--wal-failover=path=cockroach-failover/abc",
				"",
				[]string{"datadir", "failoverdir"},
			},
		},
		{
			"WAL failover disabled through side disk",
			map[string]string{
				"conf.wal-failover.value":                    "disabled",
				"conf.wal-failover.persistentVolume.enabled": "true",
				"operator.enabled":                           "false",
			},
			expect{
				"--wal-failover=disabled",
				"",
				[]string{"datadir", "failoverdir"},
			},
		},
		{
			"WAL failover through side disk but no pvc",
			map[string]string{
				"conf.wal-failover.value":                    "path=/cockroach/cockroach-failover",
				"conf.wal-failover.persistentVolume.enabled": "false",
				"operator.enabled":                           "false",
			},
			expect{
				"",
				"WAL failover to a side disk requires a persistent volume",
				[]string{"datadir"},
			},
		},
		{
			"WAL failover through side disk but invalid path",
			map[string]string{
				"conf.wal-failover.value":                    "path=/invalid",
				"conf.wal-failover.persistentVolume.enabled": "true",
				"operator.enabled":                           "false",
			},
			expect{
				"",
				"WAL failover to a side disk requires a path to the mounted persistent volume",
				[]string{"datadir", "failoverdir"},
			},
		},
	}

	for _, testCase := range testCases {
		var statefulset appsv1.StatefulSet

		// Here, we capture the range variable and force it into the scope of this block.
		// If we don't do this, when the subtest switches contexts (because of t.Parallel),
		// the testCase value will have been updated by the for loop and will be the next testCase.
		testCase := testCase

		t.Run(testCase.name, func(subT *testing.T) {
			subT.Parallel()

			options := &helm.Options{
				KubectlOptions: k8s.NewKubectlOptions("", "", namespaceName),
				SetValues:      testCase.values,
			}

			output, err := helm.RenderTemplateE(
				t, options, helmChartPath, releaseName, []string{"templates/statefulset.yaml"},
			)
			if err != nil {
				require.ErrorContains(subT, err, testCase.expect.renderErr)
				return
			} else {
				require.Empty(subT, testCase.expect.renderErr)
			}

			helm.UnmarshalK8SYaml(t, output, &statefulset)

			require.Equal(subT, namespaceName, statefulset.Namespace)
			require.Contains(
				t,
				statefulset.Spec.Template.Spec.Containers[0].Args[2],
				testCase.expect.statefulsetArgument,
			)

			require.Equal(
				subT,
				len(testCase.expect.persistentVolumeNames),
				len(statefulset.Spec.VolumeClaimTemplates),
			)
			var actualPersistentVolumeNames []string
			for _, pvc := range statefulset.Spec.VolumeClaimTemplates {
				actualPersistentVolumeNames = append(actualPersistentVolumeNames, pvc.Name)
			}
			require.EqualValues(
				subT,
				testCase.expect.persistentVolumeNames,
				actualPersistentVolumeNames,
			)
		})
	}
}
