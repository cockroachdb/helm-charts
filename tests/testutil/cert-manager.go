package testutil

import (
	"fmt"
	"os"
	"path/filepath"
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

	certManagerInstall := []string{"cert-manager", "jetstack/cert-manager", "--install", "--create-namespace", "--set", "installCRDs=true", "--wait"}
	output, err := helm.RunHelmCommandAndGetOutputE(t, certManagerHelmOptions, "upgrade", certManagerInstall...)
	require.NoError(t, err)
	t.Log(output)

	// Wait for the cert-manager to be ready.
	pods := ListPodsWithRetry(t, kubectlOptions, metav1.ListOptions{LabelSelector: "app=cert-manager"})
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
	trustManagerInstall := []string{"trust-manager", "jetstack/trust-manager", "--install", "--create-namespace",
		"--set", fmt.Sprintf("app.trust.namespace=%s", trustNamespace), "--wait"}
	output, err := helm.RunHelmCommandAndGetOutputE(t, trustManagerHelmOptions, "upgrade", trustManagerInstall...)
	require.NoError(t, err)
	t.Log(output)

	// Wait for the trust-manager to be ready.
	pods := ListPodsWithRetry(t, kubectlOptions, metav1.ListOptions{LabelSelector: "app=trust-manager"})
	require.NoError(t, err)
	for _, pod := range pods {
		k8s.IsPodAvailable(&pod)
	}

	// Wait for Bundle CRD to be installed (CRDs are cluster-scoped, so use default namespace)
	t.Log("Waiting for Bundle CRD to be available...")
	defaultKubectlOptions := k8s.NewKubectlOptions(kubectlOptions.ContextName, kubectlOptions.ConfigPath, "default")
	maxRetries := 60
	crdFound := false
	for i := 0; i < maxRetries; i++ {
		_, err := k8s.RunKubectlAndGetOutputE(t, defaultKubectlOptions, "get", "crd", "bundles.trust.cert-manager.io")
		if err == nil {
			t.Log("Bundle CRD is now available")
			crdFound = true
			break
		}
		t.Logf("Waiting for Bundle CRD... (%d/%d)", i+1, maxRetries)
		time.Sleep(2 * time.Second)
	}
	if !crdFound {
		require.FailNow(t, "Bundle CRD not found after waiting 120 seconds")
	}

	// Additional wait for CRD to be fully established and ready
	time.Sleep(10 * time.Second)
}

// DeleteCertManager deletes the cert-manager release.
func DeleteCertManager(t *testing.T, kubectlOptions *k8s.KubectlOptions) {
	certManagerHelmOptions := &helm.Options{
		KubectlOptions: kubectlOptions,
	}
	if err := helm.DeleteE(t, certManagerHelmOptions, "cert-manager", true); err != nil {
		t.Logf("Warning: failed to delete cert-manager release: %v", err)
	}
}

// DeleteTrustManager deletes the trust-manager release.
func DeleteTrustManager(t *testing.T, kubectlOptions *k8s.KubectlOptions) {
	certManagerHelmOptions := &helm.Options{
		KubectlOptions: kubectlOptions,
	}
	if err := helm.DeleteE(t, certManagerHelmOptions, "trust-manager", true); err != nil {
		t.Logf("Warning: failed to delete trust-manager release: %v", err)
	}

	// Clean up Bundle CRD if it still exists
	k8s.RunKubectl(t, kubectlOptions, "delete", "crd", "bundles.trust.cert-manager.io", "--ignore-not-found=true")
}

// CreateSelfSignedIssuer creates a self-signed issuer which is used to sign the self signed CA certificate.
func CreateSelfSignedIssuer(
	t *testing.T, kubectlOptions *k8s.KubectlOptions, issuerNamespace string,
) {
	issuerCreateData := fmt.Sprintf(`
apiVersion: cert-manager.io/v1
kind: Issuer
metadata:
  name: %s
  namespace: %s
spec:
  selfSigned: {}
`, SelfSignedIssuerName, issuerNamespace)

	applyCertManagerManifest(t, kubectlOptions, selfSignedIssuerYaml, issuerCreateData)
}

// DeleteSelfSignedIssuer deletes the self-signed issuer.
func DeleteSelfSignedIssuer(
	t *testing.T, kubectlOptions *k8s.KubectlOptions, issuerNamespace string,
) {
	k8s.RunKubectl(t, kubectlOptions, "delete", "issuers.cert-manager.io", SelfSignedIssuerName, "--namespace", issuerNamespace, "--ignore-not-found=true")
}

// CreateSelfSignedCertificate creates a self-signed certificate which is stored in a secret.
func CreateSelfSignedCertificate(
	t *testing.T, kubectlOptions *k8s.KubectlOptions, issuerNamespace string,
) {
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

	applyCertManagerManifest(t, kubectlOptions, caCertYaml, certCreateData)
}

func DeleteSelfSignedCertificate(
	t *testing.T, kubectlOptions *k8s.KubectlOptions, issuerNamespace string,
) {
	k8s.RunKubectl(t, kubectlOptions, "delete", "certificates.cert-manager.io", SelfSignedCertName, "--namespace", issuerNamespace, "--ignore-not-found=true")
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

	applyCertManagerManifest(t, kubectlOptions, caIssuerYaml, issuerCreateData)
}

func DeleteCAIssuer(t *testing.T, kubectlOptions *k8s.KubectlOptions, issuerNamespace string) {
	k8s.RunKubectl(t, kubectlOptions, "delete", "issuers.cert-manager.io", CAIssuerName, "--namespace", issuerNamespace, "--ignore-not-found=true")
}

// CreateBundle creates a bundle which transfers the CA certificate from secret to configmap in the target namespace.
func CreateBundle(
	t *testing.T, kubectlOptions *k8s.KubectlOptions, caSecretName, caConfigMapName string,
) {
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
       kubernetes.io/metadata.name: %s`, caConfigMapName, caSecretName, kubectlOptions.Namespace)

	applyCertManagerManifest(t, kubectlOptions, bundleYaml, bundleCreateData)
}

// DeleteBundle deletes the bundle.
func DeleteBundle(t *testing.T, kubectlOptions *k8s.KubectlOptions, names ...string) {
	if len(names) == 0 {
		names = []string{CAConfigMapName}
	}
	for _, name := range names {
		k8s.RunKubectl(t, kubectlOptions, "delete", "bundles.trust.cert-manager.io", name, "--ignore-not-found=true")
	}
}

// Keep generated manifests out of the checkout and isolate fixtures per test.
func applyCertManagerManifest(t *testing.T, options *k8s.KubectlOptions, name, manifest string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	require.NoError(t, os.WriteFile(path, []byte(manifest), 0600))
	k8s.RunKubectl(t, options, "apply", "-f", path)
}
