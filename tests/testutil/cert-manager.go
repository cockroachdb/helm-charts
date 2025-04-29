package testutil

import (
	"fmt"
	"io/fs"
	"os"
	"testing"

	"github.com/gruntwork-io/terratest/modules/helm"
	"github.com/gruntwork-io/terratest/modules/k8s"
	"github.com/stretchr/testify/require"
)

const (
	SelfSignedIssuerName = "cockroachdb-selfsigned"
	SelfSignedCertName = "cockroachdb-ca"
	CAIssuerName = "cockroachdb"
	CASecretName = "cockroachdb-ca-secret"
	CAConfigMapName = "cockroachdb-ca"
	certManagerNamespace = "cert-manager"
	selfSignedIssuerYaml = "selfsign-ca-issuer.yaml"
	caIssuerYaml = "ca-issuer.yaml"
	caCertYaml = "ca-cert.yaml"
	bundleYaml = "bundle.yaml"
)

func InstallCertManager(t *testing.T) {
	certManagerHelmOptions := &helm.Options{
		KubectlOptions: k8s.NewKubectlOptions("", "", certManagerNamespace),
	}
	jetStackRepoAdd := []string{"add", "jetstack", "https://charts.jetstack.io", "--force-update"}
	_, err := helm.RunHelmCommandAndGetOutputE(t, &helm.Options{}, "repo", jetStackRepoAdd...)
	require.NoError(t, err)

	certManagerInstall := []string{"cert-manager", "jetstack/cert-manager", "--create-namespace", "--set", "installCRDs=true"}
	output, err := helm.RunHelmCommandAndGetOutputE(t, certManagerHelmOptions, "install", certManagerInstall...)
	require.NoError(t, err)
	t.Log(output)
}

func InstallTrustManager(t *testing.T, trustNamespace string) {
	trustManagerHelmOptions := &helm.Options{
		KubectlOptions: k8s.NewKubectlOptions("", "", certManagerNamespace),
	}
	trustManagerInstall := []string{"trust-manager", "jetstack/trust-manager", "--create-namespace", 
	"--set", fmt.Sprintf("app.trust.namespace=%s", trustNamespace)}
	output, err := helm.RunHelmCommandAndGetOutputE(t, trustManagerHelmOptions, "install", trustManagerInstall...)
	require.NoError(t, err)
	t.Log(output)
}


func DeleteCertManager(t *testing.T) {
	certManagerHelmOptions := &helm.Options{
		KubectlOptions: k8s.NewKubectlOptions("", "", certManagerNamespace),
	}
	helm.Delete(t, certManagerHelmOptions, "cert-manager", true)
}

func DeleteTrustManager(t *testing.T) {
	certManagerHelmOptions := &helm.Options{
		KubectlOptions: k8s.NewKubectlOptions("", "", certManagerNamespace),
	}
	helm.Delete(t, certManagerHelmOptions, "trust-manager", true)
}

func CreateSelfSignedIssuer(t *testing.T, issuerNamespace string) {
	issuerCreateData := fmt.Sprintf(`
apiVersion: cert-manager.io/v1
kind: Issuer
metadata:
  name: %s
  namespace: %s
spec:
  selfSigned: {}
`, SelfSignedIssuerName, issuerNamespace)

	err := os.WriteFile(selfSignedIssuerYaml, []byte(issuerCreateData), fs.ModePerm)
	require.NoError(t, err)

	k8s.RunKubectl(t, k8s.NewKubectlOptions("", "", issuerNamespace), "apply", "-f", selfSignedIssuerYaml)
}

func DeleteSelfSignedIssuer(t *testing.T, issuerNamespace string) {
	k8s.RunKubectl(t, k8s.NewKubectlOptions("", "", issuerNamespace), "delete", "-f", selfSignedIssuerYaml)
	_ = os.Remove(selfSignedIssuerYaml)
}

func CreateSelfSignedCertificate(t *testing.T, issuerNamespace string) {
	certCreateData := fmt.Sprintf(`
apiVersion: cert-manager.io/v1
kind: Certificate
metadata:
  name: %s
  namespace: %s
spec:
  isCA: true
  commonName: root
  subject:
    organizations:
      - cockroachdb
  privateKey:
    algorithm: ECDSA
    size: 256
  secretName: %s
  issuerRef:
    name: %s
    kind: Issuer
`, SelfSignedCertName, issuerNamespace, CASecretName, SelfSignedIssuerName)

	err := os.WriteFile(caCertYaml, []byte(certCreateData), fs.ModePerm)
	require.NoError(t, err)

	k8s.RunKubectl(t, k8s.NewKubectlOptions("", "", issuerNamespace), "apply", "-f", caCertYaml)
}

func DeleteSelfSignedCertificate(t *testing.T, issuerNamespace string) {
	k8s.RunKubectl(t, k8s.NewKubectlOptions("", "", issuerNamespace), "delete", "-f", caCertYaml)
	_ = os.Remove(caCertYaml)
}

func CreateCAIssuer(t *testing.T, issuerNamespace string) {
	issuerCreateData := fmt.Sprintf(`
apiVersion: cert-manager.io/v1
kind: Issuer
metadata:
  name: %s
  namespace: %s
spec:
  ca:
    secretName: %s
`, CAIssuerName, issuerNamespace, CASecretName)

	err := os.WriteFile(caIssuerYaml, []byte(issuerCreateData), fs.ModePerm)
	require.NoError(t, err)

	k8s.RunKubectl(t, k8s.NewKubectlOptions("", "", issuerNamespace), "apply", "-f", caIssuerYaml)
}

func DeleteCAIssuer(t *testing.T, issuerNamespace string) {
	k8s.RunKubectl(t, k8s.NewKubectlOptions("", "", issuerNamespace), "delete", "-f", caIssuerYaml)
	_ = os.Remove(caIssuerYaml)
}

func CreateBundle(t *testing.T, targetNamespace string) {
	bundleCreateData := fmt.Sprintf(`
apiVersion: trust.cert-manager.io/v1alpha1
kind: Bundle
metadata:
  name: %s
spec:
  sources:
    - secret:
        name: %s
        key: ca.crt
  target:
    configMap:
      key: ca.crt
    namespaceSelector:
      matchLabels:
       kubernetes.io/metadata.name: %s`, CAConfigMapName, CASecretName, targetNamespace)

	err := os.WriteFile(bundleYaml, []byte(bundleCreateData), fs.ModePerm)
	require.NoError(t, err)

	k8s.RunKubectl(t, k8s.NewKubectlOptions("", "", targetNamespace), "apply", "-f", bundleYaml)
}

func DeleteBundle(t *testing.T, targetNamespace string) {
	k8s.RunKubectl(t, k8s.NewKubectlOptions("", "", targetNamespace), "delete", "-f", bundleYaml)
	_ = os.Remove(bundleYaml)
}
