package template

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gruntwork-io/terratest/modules/helm"
	"github.com/gruntwork-io/terratest/modules/k8s"
	"github.com/stretchr/testify/require"
	batchv1 "k8s.io/api/batch/v1"
	"k8s.io/api/batch/v1beta1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
)

func TestCockroachDBPublicServicePorts(t *testing.T) {
	t.Parallel()
	chartPath, err := filepath.Abs("../../cockroachdb-operator/charts/cockroachdb")
	require.NoError(t, err)
	for _, tc := range []struct {
		name string
		grpc int32
		sql  int32
	}{
		{name: "separate listeners", grpc: 26258, sql: 26257},
		{name: "shared legacy listener", grpc: 26257, sql: 26257},
		{name: "custom shared listener", grpc: 30000, sql: 30000},
		{name: "custom separate listeners", grpc: 30000, sql: 30001},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			output := helm.RenderTemplate(t, &helm.Options{
				SetValues: map[string]string{
					"cockroachdb.crdbCluster.service.ports.grpc.port": fmt.Sprint(tc.grpc),
					"cockroachdb.crdbCluster.service.ports.sql.port":  fmt.Sprint(tc.sql),
				},
			}, chartPath, releaseName, []string{"templates/service.public.yaml"})
			var service corev1.Service
			helm.UnmarshalK8SYaml(t, output, &service)
			wantPorts := map[string]int32{"grpc": tc.grpc, "http": 8080}
			if tc.grpc != tc.sql {
				wantPorts["sql"] = tc.sql
			}
			require.Len(t, service.Spec.Ports, len(wantPorts))
			for _, port := range service.Spec.Ports {
				require.Contains(t, wantPorts, port.Name)
				require.Equal(t, wantPorts[port.Name], port.Port)
				require.Equal(t, port.Name, port.TargetPort.String())
				delete(wantPorts, port.Name)
			}
			require.Empty(t, wantPorts)
			// Ingress must use the same port name that the public Service exposes.
			ingressOutput := helm.RenderTemplate(t, &helm.Options{SetValues: map[string]string{
				"cockroachdb.crdbCluster.service.ports.grpc.port":  fmt.Sprint(tc.grpc),
				"cockroachdb.crdbCluster.service.ports.sql.port":   fmt.Sprint(tc.sql),
				"cockroachdb.crdbCluster.service.ingress.enabled":  "true",
				"cockroachdb.crdbCluster.service.ingress.sql.host": "sql.example.test",
			}}, chartPath, releaseName, []string{"templates/ingress.yaml"})
			var ingress networkingv1.Ingress
			helm.UnmarshalK8SYaml(t, ingressOutput, &ingress)
			portName := "sql"
			if tc.grpc == tc.sql {
				portName = "grpc"
			}
			require.Equal(t, portName, ingress.Spec.Rules[0].HTTP.Paths[0].Backend.Service.Port.Name)
		})
	}
}

func TestCockroachDBIngressPortNames(t *testing.T) {
	t.Parallel()
	chartPath, err := filepath.Abs("../../cockroachdb-operator/charts/cockroachdb")
	require.NoError(t, err)
	for _, grpcPort := range []int{26257, 26258} {
		t.Run(fmt.Sprint(grpcPort), func(t *testing.T) {
			t.Parallel()
			values := map[string]string{
				"cockroachdb.crdbCluster.service.ports.grpc.port":  fmt.Sprint(grpcPort),
				"cockroachdb.crdbCluster.service.ports.grpc.name":  "db-internal",
				"cockroachdb.crdbCluster.service.ports.sql.name":   "db-sql",
				"cockroachdb.crdbCluster.service.ports.http.name":  "db-ui",
				"cockroachdb.crdbCluster.service.ingress.enabled":  "true",
				"cockroachdb.crdbCluster.service.ingress.ui.host":  "ui.example.test",
				"cockroachdb.crdbCluster.service.ingress.sql.host": "sql.example.test",
			}
			output := helm.RenderTemplate(t, &helm.Options{SetValues: values}, chartPath, releaseName, []string{"templates/service.public.yaml"})
			var service corev1.Service
			helm.UnmarshalK8SYaml(t, output, &service)
			output = helm.RenderTemplate(t, &helm.Options{SetValues: values}, chartPath, releaseName, []string{"templates/ingress.yaml"})
			count := 0
			for _, document := range strings.Split(output, "\n---") {
				if !strings.Contains(document, "kind: Ingress") {
					continue
				}
				var ingress networkingv1.Ingress
				helm.UnmarshalK8SYaml(t, document, &ingress)
				backend := ingress.Spec.Rules[0].HTTP.Paths[0].Backend.Service
				require.Equal(t, service.Name, backend.Name)
				var port int32
				for _, candidate := range service.Spec.Ports {
					if candidate.Name == backend.Port.Name {
						port = candidate.Port
					}
				}
				if strings.HasPrefix(ingress.Name, "ui-") {
					require.EqualValues(t, 8080, port)
				} else {
					require.EqualValues(t, 26257, port)
				}
				count++
			}
			require.Equal(t, 2, count)
		})
	}
}

func TestCockroachDBInsecureTemplate(t *testing.T) {
	t.Parallel()
	chartPath, err := filepath.Abs("../../cockroachdb-operator/charts/cockroachdb")
	require.NoError(t, err)
	values := map[string]string{"cockroachdb.tls.enabled": "false"}
	_, err = helm.RenderTemplateE(t, &helm.Options{SetValues: values}, chartPath, releaseName, nil)
	require.ErrorContains(t, err, "must all be disabled when TLS is off")
	values["cockroachdb.tls.selfSigner.enabled"] = "false"
	output := helm.RenderTemplate(t, &helm.Options{SetValues: values}, chartPath, releaseName, nil)
	var clusterFound bool
	for _, document := range strings.Split(output, "\n---") {
		var object struct {
			Kind string `json:"kind"`
			Spec struct {
				TLSEnabled bool `json:"tlsEnabled"`
				Template   struct {
					Spec map[string]interface{} `json:"spec"`
				} `json:"template"`
			} `json:"spec"`
		}
		helm.UnmarshalK8SYaml(t, document, &object)
		switch object.Kind {
		case "CrdbCluster":
			clusterFound = true
			require.False(t, object.Spec.TLSEnabled)
			require.NotContains(t, object.Spec.Template.Spec, "certificates")
		case "Certificate", "Issuer", "CronJob":
			t.Fatalf("insecure chart rendered certificate resource %s", object.Kind)
		}
	}
	require.True(t, clusterFound)
}

// TestCockroachDBSelfSignerAdditionalSANs checks the cluster chart signer Job and CronJob.
func TestCockroachDBSelfSignerAdditionalSANs(t *testing.T) {
	t.Parallel()

	chartPath, err := filepath.Abs("../../cockroachdb-operator/charts/cockroachdb")
	require.NoError(t, err)

	testCases := []struct {
		name                 string
		setValues            map[string]string
		expectAdditionalSANs bool
		expectedValue        string
	}{
		{
			name: "additionalSANs with multiple values",
			setValues: map[string]string{
				"cockroachdb.tls.selfSigner.enabled":           "true",
				"cockroachdb.tls.selfSigner.additionalSANs[0]": "my-loadbalancer.example.com",
				"cockroachdb.tls.selfSigner.additionalSANs[1]": "10.20.30.40",
				"cockroachdb.tls.selfSigner.additionalSANs[2]": "backup-lb.example.com",
			},
			expectAdditionalSANs: true,
			expectedValue:        "my-loadbalancer.example.com,10.20.30.40,backup-lb.example.com",
		},
		{
			name: "additionalSANs with single value",
			setValues: map[string]string{
				"cockroachdb.tls.selfSigner.enabled":           "true",
				"cockroachdb.tls.selfSigner.additionalSANs[0]": "my-lb.example.com",
			},
			expectAdditionalSANs: true,
			expectedValue:        "my-lb.example.com",
		},
		{
			name: "no additionalSANs configured",
			setValues: map[string]string{
				"cockroachdb.tls.selfSigner.enabled": "true",
			},
			expectAdditionalSANs: false,
		},
	}

	for _, tc := range testCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			options := &helm.Options{
				KubectlOptions: k8s.NewKubectlOptions("", "", namespaceName),
				SetValues:      tc.setValues,
			}

			// Test Job template
			jobOutput := helm.RenderTemplate(t, options, chartPath, releaseName, []string{"templates/job-certSelfSigner.yaml"})
			var job batchv1.Job
			helm.UnmarshalK8SYaml(t, jobOutput, &job)

			additionalSANsEnv := findEnvVar(job.Spec.Template.Spec.Containers[0].Env, "ADDITIONAL_SANS")

			if tc.expectAdditionalSANs {
				require.NotNil(t, additionalSANsEnv, "ADDITIONAL_SANS env var should be present in Job")
				require.Equal(t, tc.expectedValue, additionalSANsEnv.Value, "ADDITIONAL_SANS value should match")
			} else {
				require.Nil(t, additionalSANsEnv, "ADDITIONAL_SANS env var should not be present when not configured")
			}

			// Test CronJob template
			cronJobOutput := helm.RenderTemplate(t, options, chartPath, releaseName, []string{"templates/cronjob-client-node-certSelfSigner.yaml"})
			var cronJob v1beta1.CronJob
			helm.UnmarshalK8SYaml(t, cronJobOutput, &cronJob)

			additionalSANsEnvCron := findEnvVar(cronJob.Spec.JobTemplate.Spec.Template.Spec.Containers[0].Env, "ADDITIONAL_SANS")

			if tc.expectAdditionalSANs {
				require.NotNil(t, additionalSANsEnvCron, "ADDITIONAL_SANS env var should be present in CronJob")
				require.Equal(t, tc.expectedValue, additionalSANsEnvCron.Value, "ADDITIONAL_SANS value should match")
			} else {
				require.Nil(t, additionalSANsEnvCron, "ADDITIONAL_SANS env var should not be present when not configured")
			}
		})
	}
}

// TestCockroachDBTestPodAnnotationsAndLabels tests that tests.podAnnotations and
// tests.podLabels are added to the helm test pod alongside the helm hook annotation.
func TestCockroachDBTestPodAnnotationsAndLabels(t *testing.T) {
	t.Parallel()

	chartPath, err := filepath.Abs("../../cockroachdb-operator/charts/cockroachdb")
	require.NoError(t, err)

	render := func(t *testing.T, setStrValues map[string]string) corev1.Pod {
		t.Helper()
		options := &helm.Options{
			KubectlOptions: k8s.NewKubectlOptions("", "", namespaceName),
			SetStrValues:   setStrValues,
		}
		output := helm.RenderTemplate(t, options, chartPath, releaseName, []string{"templates/tests/client.yaml"})
		var pod corev1.Pod
		helm.UnmarshalK8SYaml(t, output, &pod)
		return pod
	}

	t.Run("unset", func(t *testing.T) {
		t.Parallel()
		pod := render(t, nil)

		require.Equal(t, map[string]string{"helm.sh/hook": "test-success"}, pod.Annotations)
		require.Empty(t, pod.Labels)
	})

	t.Run("set", func(t *testing.T) {
		t.Parallel()
		pod := render(t, map[string]string{
			"cockroachdb.tests.podAnnotations.sidecar\\.istio\\.io/inject": "false",
			"cockroachdb.tests.podLabels.team":                             "platform",
		})

		require.Equal(t, "test-success", pod.Annotations["helm.sh/hook"])
		require.Equal(t, "false", pod.Annotations["sidecar.istio.io/inject"])
		require.Equal(t, map[string]string{"team": "platform"}, pod.Labels)
	})
}

// TestCockroachDBTestPodClientCerts tests that the helm test pod mounts the root client
// certificate and CA used by the CrdbCluster for each TLS mode, and connects to the SQL port.
func TestCockroachDBTestPodClientCerts(t *testing.T) {
	t.Parallel()

	chartPath, err := filepath.Abs("../../cockroachdb-operator/charts/cockroachdb")
	require.NoError(t, err)

	testCases := []struct {
		name         string
		setValues    map[string]string
		expectSecret string
		expectCA     string
	}{
		{
			name:         "self-signer",
			setValues:    map[string]string{},
			expectSecret: releaseName + "-cockroachdb-client-secret",
			expectCA:     releaseName + "-cockroachdb-ca-secret-crt",
		},
		{
			name: "cert-manager",
			setValues: map[string]string{
				"cockroachdb.tls.selfSigner.enabled":           "false",
				"cockroachdb.tls.certManager.enabled":          "true",
				"cockroachdb.tls.certManager.issuer.name":      "issuer",
				"cockroachdb.tls.certManager.clientRootSecret": "my-root",
				"cockroachdb.tls.certManager.caConfigMap":      "my-ca",
			},
			expectSecret: "my-root",
			expectCA:     "my-ca",
		},
		{
			name: "external certificates",
			setValues: map[string]string{
				"cockroachdb.tls.selfSigner.enabled":                                        "false",
				"cockroachdb.tls.externalCertificates.enabled":                              "true",
				"cockroachdb.tls.externalCertificates.certificates.caConfigMapName":         "ext-ca",
				"cockroachdb.tls.externalCertificates.certificates.nodeSecretName":          "ext-node",
				"cockroachdb.tls.externalCertificates.certificates.httpSecretName":          "ext-root",
				"cockroachdb.tls.externalCertificates.certificates.rootSqlClientSecretName": "ext-root",
			},
			expectSecret: "ext-root",
			expectCA:     "ext-ca",
		},
		{
			// Split CA: caConfigMapName is left empty and the node CA is set
			// instead. The test pod validates the server it connects to, so it
			// must fall back to the node CA. See the external-certificates docs
			// on split CA certificates in values.yaml.
			name: "external certificates split CA",
			setValues: map[string]string{
				"cockroachdb.tls.selfSigner.enabled":                                        "false",
				"cockroachdb.tls.externalCertificates.enabled":                              "true",
				"cockroachdb.tls.externalCertificates.certificates.nodeCaConfigMapName":     "ext-node-ca",
				"cockroachdb.tls.externalCertificates.certificates.clientCaConfigMapName":   "ext-client-ca",
				"cockroachdb.tls.externalCertificates.certificates.nodeSecretName":          "ext-node",
				"cockroachdb.tls.externalCertificates.certificates.httpSecretName":          "ext-root",
				"cockroachdb.tls.externalCertificates.certificates.rootSqlClientSecretName": "ext-root",
			},
			expectSecret: "ext-root",
			expectCA:     "ext-node-ca",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			options := &helm.Options{
				KubectlOptions: k8s.NewKubectlOptions("", "", namespaceName),
				SetValues:      tc.setValues,
			}
			output := helm.RenderTemplate(t, options, chartPath, releaseName, []string{"templates/tests/client.yaml"})
			var pod corev1.Pod
			helm.UnmarshalK8SYaml(t, output, &pod)

			require.Len(t, pod.Spec.Volumes, 1)
			projected := pod.Spec.Volumes[0].Projected
			require.NotNil(t, projected)
			require.Len(t, projected.Sources, 2)

			secret := projected.Sources[0].Secret
			require.NotNil(t, secret)
			require.Equal(t, tc.expectSecret, secret.Name)
			require.Equal(t, []corev1.KeyToPath{
				{Key: "tls.crt", Path: "client.root.crt"},
				{Key: "tls.key", Path: "client.root.key"},
			}, secret.Items)

			ca := projected.Sources[1].ConfigMap
			require.NotNil(t, ca)
			require.Equal(t, tc.expectCA, ca.Name)
			require.Equal(t, []corev1.KeyToPath{{Key: "ca.crt", Path: "ca.crt"}}, ca.Items)

			require.Contains(t, strings.Join(pod.Spec.Containers[0].Command, " "), "--port 26257")
		})
	}

	t.Run("insecure", func(t *testing.T) {
		t.Parallel()
		options := &helm.Options{
			KubectlOptions: k8s.NewKubectlOptions("", "", namespaceName),
			SetValues: map[string]string{
				"cockroachdb.tls.enabled":            "false",
				"cockroachdb.tls.selfSigner.enabled": "false",
			},
		}
		output := helm.RenderTemplate(t, options, chartPath, releaseName, []string{"templates/tests/client.yaml"})
		var pod corev1.Pod
		helm.UnmarshalK8SYaml(t, output, &pod)

		require.Empty(t, pod.Spec.Volumes)
		require.Contains(t, pod.Spec.Containers[0].Command, "--insecure")
	})
}
