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
			},
			"You have to enable either self signed certificates or certificate manager, if you have enabled tls",
		},
		{
			"Self Signer and cert manager set to true",
			map[string]string{
				"tls.certs.selfSigner.enabled": "true",
				"tls.certs.certManager":        "true",
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
			map[string]string{"tls.certs.selfSigner.enabled": "true"},
			"copy-certs",
		},
		{
			"Self Signer disable",
			map[string]string{
				"tls.certs.selfSigner.enabled": "false",
				"tls.certs.certManager":        "true",
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

	testCases := []struct {
		name   string
		values map[string]string
		expect struct {
			statefulsetArgument string
			logConfig           string
			secretExists        bool
		}
	}{
		{
			"New logging configuration enabled",
			map[string]string{"conf.log.enabled": "true"},
			struct {
				statefulsetArgument string
				logConfig           string
				secretExists        bool
			}{
				"--log-config-file=/cockroach/log-config/log-config.yaml",
				"{}",
				true,
			},
		},
		{
			"New logging configuration overridden",
			map[string]string{
				"conf.log.enabled": "true",
				"conf.log.config":  "file-defaults:\ndir: /custom/dir/path/",
			},
			struct {
				statefulsetArgument string
				logConfig           string
				secretExists        bool
			}{
				"--log-config-file=/cockroach/log-config/log-config.yaml",
				"file-defaults:\n  dir: /custom/dir/path/",
				true,
			},
		},
		{
			"New logging configuration disabled",
			map[string]string{
				"conf.log.enabled": "false",
				"conf.logtostderr": "INFO",
			},
			struct {
				statefulsetArgument string
				logConfig           string
				secretExists        bool
			}{
				"--logtostderr=INFO",
				"",
				false,
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
			output := helm.RenderTemplate(t, options, helmChartPath, releaseName, []string{"templates/statefulset.yaml"})

			helm.UnmarshalK8SYaml(t, output, &statefulset)

			require.Equal(subT, namespaceName, statefulset.Namespace)
			require.Contains(t, statefulset.Spec.Template.Spec.Containers[0].Args[2], testCase.expect.statefulsetArgument)

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
			map[string]string{"init.provisioning.enabled": "false"},
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
			map[string]string{"init.provisioning.enabled": "true"},
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
