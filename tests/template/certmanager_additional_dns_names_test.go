package template

import (
	"path/filepath"
	"testing"

	"github.com/gruntwork-io/terratest/modules/helm"
	"github.com/gruntwork-io/terratest/modules/k8s"
	"github.com/stretchr/testify/require"
)

// nodeCertificate is a minimal projection of the cert-manager Certificate resource,
// covering only the fields these tests assert on.
type nodeCertificate struct {
	Spec struct {
		DNSNames    []string `json:"dnsNames"`
		IPAddresses []string `json:"ipAddresses"`
	} `json:"spec"`
}

// TestCertManagerAdditionalDnsNames verifies that operator-supplied DNS names and IP
// addresses are added to the cert-manager issued node certificate, that the built-in
// names are preserved, and that leaving the fields unset changes nothing.
func TestCertManagerAdditionalDnsNames(t *testing.T) {
	t.Parallel()

	operatorChartPath, pathErr := filepath.Abs("../../cockroachdb-operator/charts/cockroachdb")
	require.NoError(t, pathErr)

	charts := []struct {
		name       string
		chartPath  string
		baseValues map[string]string
		valuesKey  string
		ipKey      string
		// builtInName is a default SAN that must survive the change.
		builtInName string
	}{
		{
			name:      "operator chart",
			chartPath: operatorChartPath,
			baseValues: map[string]string{
				"cockroachdb.tls.certManager.enabled": "true",
				"cockroachdb.tls.selfSigner.enabled":  "false",
			},
			valuesKey:   "cockroachdb.tls.certManager.additionalDnsNames",
			ipKey:       "cockroachdb.tls.certManager.additionalIpAddresses",
			builtInName: "localhost",
		},
		{
			name:      "legacy chart",
			chartPath: helmChartPath,
			baseValues: map[string]string{
				"tls.certs.certManager":        "true",
				"tls.certs.selfSigner.enabled": "false",
			},
			valuesKey:   "tls.certs.certManagerIssuer.additionalDnsNames",
			ipKey:       "tls.certs.certManagerIssuer.additionalIpAddresses",
			builtInName: "localhost",
		},
	}

	for _, chart := range charts {
		chart := chart
		t.Run(chart.name, func(t *testing.T) {
			t.Parallel()

			render := func(t *testing.T, extra map[string]string) nodeCertificate {
				values := map[string]string{}
				for k, v := range chart.baseValues {
					values[k] = v
				}
				for k, v := range extra {
					values[k] = v
				}
				options := &helm.Options{
					KubectlOptions: k8s.NewKubectlOptions("", "", namespaceName),
					SetValues:      values,
				}
				output := helm.RenderTemplate(t, options, chart.chartPath, releaseName,
					[]string{"templates/certificate.node.yaml"})
				var cert nodeCertificate
				helm.UnmarshalK8SYaml(t, output, &cert)
				return cert
			}

			t.Run("appends additional names", func(t *testing.T) {
				cert := render(t, map[string]string{
					chart.valuesKey + "[0]": "crdb.example.com",
					chart.valuesKey + "[1]": "*.crdb.example.com",
				})
				require.Contains(t, cert.Spec.DNSNames, "crdb.example.com")
				require.Contains(t, cert.Spec.DNSNames, "*.crdb.example.com")
				require.Contains(t, cert.Spec.DNSNames, chart.builtInName,
					"built-in DNS names must be preserved")
			})

			t.Run("adds IP addresses as ipAddresses, not dnsNames", func(t *testing.T) {
				cert := render(t, map[string]string{
					chart.ipKey + "[0]": "10.0.0.10",
					chart.ipKey + "[1]": "192.168.1.100",
				})
				require.Equal(t, []string{"10.0.0.10", "192.168.1.100"}, cert.Spec.IPAddresses)
				require.NotContains(t, cert.Spec.DNSNames, "10.0.0.10")
				require.Contains(t, cert.Spec.DNSNames, chart.builtInName,
					"built-in DNS names must be preserved")
			})

			t.Run("unset adds nothing", func(t *testing.T) {
				cert := render(t, nil)
				require.Contains(t, cert.Spec.DNSNames, chart.builtInName)
				require.NotContains(t, cert.Spec.DNSNames, "crdb.example.com")
				require.NotContains(t, cert.Spec.DNSNames, "",
					"an unset additionalDnsNames must not emit an empty SAN")
				require.Empty(t, cert.Spec.IPAddresses,
					"an unset additionalIpAddresses must not emit an ipAddresses field")
			})
		})
	}
}
