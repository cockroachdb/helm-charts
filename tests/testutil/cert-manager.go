package testutil

import (
	"fmt"
	"io/fs"
	"os"
	"testing"
	"time"

	"github.com/gruntwork-io/terratest/modules/helm"
	"github.com/gruntwork-io/terratest/modules/k8s"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	SelfSignedIssuerName = "cockroachdb-selfsigned"
	SelfSignedCertName   = "cockroachdb-ca"
	CAIssuerName         = "cockroachdb"
	CASecretName         = "cockroachdb-ca-secret"
	CAConfigMapName      = "cockroachdb-ca"
	CertManagerNamespace = "cert-manager"
	selfSignedIssuerYaml = "selfsign-ca-issuer.yaml"
	caIssuerYaml         = "ca-issuer.yaml"
	caCertYaml           = "ca-cert.yaml"
	bundleYaml           = "bundle.yaml"
)

// InstallCertManager installs the cert-manager in cert-manager namespace with helm: https://cert-manager.io/docs/installation/helm/
func InstallCertManager(t *testing.T, kubectlOptions *k8s.KubectlOptions) {
	certManagerHelmOptions := &helm.Options{
		KubectlOptions: kubectlOptions,
	}
	jetStackRepoAdd := []string{"add", "jetstack", "https://charts.jetstack.io", "--force-update"}
	_, err := helm.RunHelmCommandAndGetOutputE(t, &helm.Options{}, "repo", jetStackRepoAdd...)
	require.NoError(t, err)

	certManagerInstall := []string{"cert-manager", "jetstack/cert-manager", "--create-namespace", "--set", "installCRDs=true", "--wait"}
	output, err := helm.RunHelmCommandAndGetOutputE(t, certManagerHelmOptions, "install", certManagerInstall...)
	require.NoError(t, err)
	t.Log(output)

	// Wait for the cert-manager to be ready.
	pods := k8s.ListPods(t, kubectlOptions, metav1.ListOptions{LabelSelector: "app=cert-manager"})
	require.NoError(t, err)
	for _, pod := range pods {
		k8s.IsPodAvailable(&pod)
	}
}

// InstallTrustManager installs the trust-manager in cert-manager namespace with helm: https://cert-manager.io/docs/trust/trust-manager/installation/
func InstallTrustManager(t *testing.T, kubectlOptions *k8s.KubectlOptions, trustNamespace string) {
	trustManagerHelmOptions := &helm.Options{
		KubectlOptions: kubectlOptions,
	}
	trustManagerInstall := []string{"trust-manager", "jetstack/trust-manager", "--create-namespace",
		"--set", fmt.Sprintf("app.trust.namespace=%s", trustNamespace), "--wait"}
	output, err := helm.RunHelmCommandAndGetOutputE(t, trustManagerHelmOptions, "install", trustManagerInstall...)
	require.NoError(t, err)
	t.Log(output)

	// Wait for the trust-manager to be ready.
	pods := k8s.ListPods(t, kubectlOptions, metav1.ListOptions{LabelSelector: "app=trust-manager"})
	require.NoError(t, err)
	for _, pod := range pods {
		k8s.IsPodAvailable(&pod)
	}

	// Sleeping for 10 seconds to ensure that the trust-manager is ready.
	time.Sleep(10 * time.Second)
}

// DeleteCertManager deletes the cert-manager release.
func DeleteCertManager(t *testing.T, kubectlOptions *k8s.KubectlOptions) {
	certManagerHelmOptions := &helm.Options{
		KubectlOptions: kubectlOptions,
	}
	helm.Delete(t, certManagerHelmOptions, "cert-manager", true)
}

// DeleteTrustManager deletes the trust-manager release.
func DeleteTrustManager(t *testing.T, kubectlOptions *k8s.KubectlOptions) {
	certManagerHelmOptions := &helm.Options{
		KubectlOptions: kubectlOptions,
	}
	helm.Delete(t, certManagerHelmOptions, "trust-manager", true)
}

// CreateSelfSignedIssuer creates a self-signed issuer which is used to sign the self signed CA certificate.
func CreateSelfSignedIssuer(t *testing.T, kubectlOptions *k8s.KubectlOptions, issuerNamespace string) {
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

	k8s.RunKubectl(t, kubectlOptions, "apply", "-f", selfSignedIssuerYaml)
}

// DeleteSelfSignedIssuer deletes the self-signed issuer.
func DeleteSelfSignedIssuer(t *testing.T, kubectlOptions *k8s.KubectlOptions, issuerNamespace string) {
	k8s.RunKubectl(t, kubectlOptions, "delete", "-f", selfSignedIssuerYaml)
	_ = os.Remove(selfSignedIssuerYaml)
}

// CreateSelfSignedCertificate creates a self-signed certificate which is stored in a secret.
func CreateSelfSignedCertificate(t *testing.T, kubectlOptions *k8s.KubectlOptions, issuerNamespace string) {
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

	k8s.RunKubectl(t, kubectlOptions, "apply", "-f", caCertYaml)
}

func DeleteSelfSignedCertificate(t *testing.T, kubectlOptions *k8s.KubectlOptions, issuerNamespace string) {
	k8s.RunKubectl(t, kubectlOptions, "delete", "-f", caCertYaml)
	_ = os.Remove(caCertYaml)
}

// CreateCAIssuer creates a CA issuer using the CA certificate stored in a secret.
// The CA issuer is used to sign the certificates for the cockroachdb cluster.
func CreateCAIssuer(t *testing.T, kubectlOptions *k8s.KubectlOptions, issuerNamespace string) {
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

	k8s.RunKubectl(t, kubectlOptions, "apply", "-f", caIssuerYaml)
}

func DeleteCAIssuer(t *testing.T, kubectlOptions *k8s.KubectlOptions, issuerNamespace string) {
	k8s.RunKubectl(t, kubectlOptions, "delete", "-f", caIssuerYaml)
	_ = os.Remove(caIssuerYaml)
}

// CreateBundle creates a bundle which transfers the CA certificate from secret to configmap in the target namespace.
func CreateBundle(t *testing.T, kubectlOptions *k8s.KubectlOptions, targetNamespace string) {
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

	k8s.RunKubectl(t, kubectlOptions, "apply", "-f", bundleYaml)
}

// DeleteBundle deletes the bundle.
func DeleteBundle(t *testing.T, kubectlOptions *k8s.KubectlOptions, targetNamespace string) {
	k8s.RunKubectl(t, kubectlOptions, "delete", "-f", bundleYaml)
	_ = os.Remove(bundleYaml)
}
