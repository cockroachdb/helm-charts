package encryption

import (
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gruntwork-io/terratest/modules/k8s"
	"github.com/gruntwork-io/terratest/modules/shell"
	"github.com/stretchr/testify/require"
)

type PlatformConfig struct {
	// Platform is the KMS platform type: "AWS_KMS", "GCP_CLOUD_KMS", or "UNKNOWN_KEY_TYPE"
	Platform string

	// RequiresCredentialsSecret indicates if cmekCredentialsSecretName is required
	// True for AWS_KMS and GCP_KMS, false for UNKNOWN_KEY_TYPE (file-based)
	RequiresCredentialsSecret bool

	// DefaultCredentialsSecretName is the default name for the credentials secret
	// Used when RequiresCredentialsSecret is true
	DefaultCredentialsSecretName string
}

// Provider defines the encryption-related methods that cloud providers must implement
// for encryption-at-rest testing. Providers return configuration, not implementation.
type Provider interface {
	// SetupEncryptionInfrastructure creates cloud KMS resources (keys, roles, policies)
	// Returns a cleanup function that should be deferred to ensure proper resource cleanup
	// Called once during test setup, before any encryption secrets are created
	SetupEncryptionInfrastructure(t *testing.T) (cleanup func(), err error)

	// GetEncryptionPlatformConfig returns provider-specific encryption platform configuration
	GetEncryptionPlatformConfig() *PlatformConfig

	// ─── CMEK Encryption Methods ─────────────────────────────────────────────────
	// The following methods are only called for cloud KMS providers (AWS_KMS, GCP_CLOUD_KMS).
	// File-based providers (UNKNOWN_KEY_TYPE) should return errors indicating lack of support.

	// EncryptKey encrypts a plaintext key using the provider's KMS
	// Takes raw key bytes, returns base64-encoded encrypted data
	EncryptKey(plaintextKey []byte, clusterRegion string) (encryptedKeyBase64 string, err error)

	// CreateKeySecret creates the Kubernetes secret with encrypted key data and provider metadata
	// (AuthPrincipal, URI, Region, Type, ExternalID, etc.)
	CreateKeySecret(kubectlOptions *k8s.KubectlOptions, secretName string, encryptedKeyData string, clusterRegion string) error

	// CreateCredentialsSecret creates the Kubernetes secret with cloud credentials
	// Returns the secret name and any error
	CreateCredentialsSecret(kubectlOptions *k8s.KubectlOptions) (string, error)
}

// GenerateAndEncodeEncryptionKey generates a 256-bit AES encryption key and returns both
// the raw bytes and base64-encoded string. This is a utility function for providers using
// UNKNOWN_KEY_TYPE (file-based encryption) to generate and encode keys consistently.
func GenerateAndEncodeEncryptionKey(t *testing.T) (rawKey []byte, base64Key string) {
	tempDir := t.TempDir()
	keyPath := filepath.Join(tempDir, "store.key")

	cmd := shell.Command{
		Command:    "cockroach",
		Args:       []string{"gen", "encryption-key", "--size", "256", "store.key"},
		WorkingDir: tempDir,
	}

	_, err := shell.RunCommandAndGetOutputE(t, cmd)
	require.NoError(t, err, "Failed to generate encryption key")

	keyBytes, err := os.ReadFile(keyPath)
	require.NoError(t, err, "Failed to read encryption key file")

	base64Encoded := base64.StdEncoding.EncodeToString(keyBytes)
	base64Encoded = strings.ReplaceAll(base64Encoded, "\n", "")

	return keyBytes, base64Encoded
}

// SetupEncryptionSecretsWithName dispatches to the appropriate setup path based on platform type.
// CMEK providers (GCP_CLOUD_KMS, AWS_KMS) go through KMS encryption; file-based (UNKNOWN_KEY_TYPE)
// writes raw key bytes directly.
func SetupEncryptionSecretsWithName(t *testing.T, provider Provider, kubectlOptions *k8s.KubectlOptions, clusterRegion string, secretName string) error {
	platformConfig := provider.GetEncryptionPlatformConfig()

	if platformConfig.Platform == "UNKNOWN_KEY_TYPE" {
		return setupFileBasedEncryptionSecretsWithName(t, kubectlOptions, platformConfig.Platform, secretName)
	}

	return setupCMEKEncryptionSecretsWithName(t, provider, kubectlOptions, clusterRegion, secretName)
}

func setupCMEKEncryptionSecretsWithName(
	t *testing.T,
	provider Provider,
	kubectlOptions *k8s.KubectlOptions,
	clusterRegion string,
	secretName string,
) error {
	platformConfig := provider.GetEncryptionPlatformConfig()
	t.Logf("Setting up KMS envelope encryption for cluster %s (platform: %s, secret: %s)", clusterRegion, platformConfig.Platform, secretName)

	plaintextKey, _ := GenerateAndEncodeEncryptionKey(t)

	encryptedKey, err := provider.EncryptKey(plaintextKey, clusterRegion)
	if err != nil {
		return fmt.Errorf("failed to encrypt store key with KMS: %w", err)
	}

	if err := provider.CreateKeySecret(kubectlOptions, secretName, encryptedKey, clusterRegion); err != nil {
		return fmt.Errorf("failed to create encryption key secret %s: %w", secretName, err)
	}

	credentialsSecretName, err := provider.CreateCredentialsSecret(kubectlOptions)
	if err != nil {
		if !strings.Contains(err.Error(), "AlreadyExists") && !strings.Contains(err.Error(), "already exists") {
			return fmt.Errorf("failed to create credentials secret: %w", err)
		}
		t.Logf("Credentials secret already exists, reusing existing secret for KMS authentication")
	} else {
		t.Logf("Created credentials secret %s for KMS authentication", credentialsSecretName)
	}

	return nil
}

func setupFileBasedEncryptionSecretsWithName(t *testing.T, kubectlOptions *k8s.KubectlOptions, providerName string, secretName string) error {
	t.Logf("%s provider: Setting up file-based encryption secrets (secret: %s)", providerName, secretName)

	rawKey, _ := GenerateAndEncodeEncryptionKey(t)
	t.Logf("Generated encryption key (%d bytes)", len(rawKey))

	tempDir := t.TempDir()
	keyPath := filepath.Join(tempDir, "StoreKeyData")
	if err := os.WriteFile(keyPath, rawKey, 0600); err != nil {
		return fmt.Errorf("failed to write key to temp file: %w", err)
	}

	err := k8s.RunKubectlE(t, kubectlOptions, "create", "secret", "generic", secretName,
		fmt.Sprintf("--from-file=StoreKeyData=%s", keyPath))
	if err != nil {
		return fmt.Errorf("failed to create encryption key secret: %w", err)
	}

	secretSize, err := k8s.RunKubectlAndGetOutputE(t, kubectlOptions,
		"get", "secret", secretName,
		"-o", "jsonpath={.data.StoreKeyData}")
	if err != nil {
		return fmt.Errorf("failed to verify encryption secret: %w", err)
	}
	if len(secretSize) == 0 {
		return fmt.Errorf("encryption secret StoreKeyData is empty")
	}

	t.Logf("Created encryption secret %s with key data (%d bytes)", secretName, len(secretSize))

	return nil
}
