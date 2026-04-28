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

// EncryptionPlatformConfig holds provider-specific encryption configuration defaults
type EncryptionPlatformConfig struct {
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
// for encryption-at-rest testing. Providers handle all encryption details internally.
type Provider interface {
	// SetupEncryptionInfrastructure creates cloud KMS resources (keys, roles, policies)
	// Returns a cleanup function that should be deferred to ensure proper resource cleanup
	// Called once during test setup, before any encryption secrets are created
	SetupEncryptionInfrastructure(t *testing.T) (cleanup func(), err error)

	// SetupEncryptionSecrets creates all necessary Kubernetes secrets for encryption.
	// This includes:
	//   - Key secret with encrypted/encoded key data and provider-specific metadata
	//   - Credentials secret (if required by the provider)
	// The provider manages key generation, encryption, secret names, and all other details internally.
	// clusterRegion identifies which cluster/region this is for (used for KMS key selection in multi-region)
	SetupEncryptionSecrets(t *testing.T, kubectlOptions *k8s.KubectlOptions, clusterRegion string) error

	// GetEncryptionPlatformConfig returns provider-specific encryption platform configuration
	GetEncryptionPlatformConfig() *EncryptionPlatformConfig
}

// GenerateAndEncodeEncryptionKey generates a 256-bit AES encryption key and returns both
// the raw bytes and base64-encoded string. This is a utility function for providers using
// UNKNOWN_KEY_TYPE (file-based encryption) to generate and encode keys consistently.
func GenerateAndEncodeEncryptionKey(t *testing.T) (rawKey []byte, base64Key string) {
	tempDir := t.TempDir()
	keyPath := filepath.Join(tempDir, "store.key")

	// Generate 256-bit AES key using cockroach gen encryption-key
	cmd := shell.Command{
		Command:    "cockroach",
		Args:       []string{"gen", "encryption-key", "--size", "256", "store.key"},
		WorkingDir: tempDir,
	}

	_, err := shell.RunCommandAndGetOutputE(t, cmd)
	require.NoError(t, err, "Failed to generate encryption key")

	// Read the generated key file
	keyBytes, err := os.ReadFile(keyPath)
	require.NoError(t, err, "Failed to read encryption key file")

	// Base64 encode the key (removing any newlines)
	base64Encoded := base64.StdEncoding.EncodeToString(keyBytes)
	base64Encoded = strings.ReplaceAll(base64Encoded, "\n", "")

	return keyBytes, base64Encoded
}

// SetupFileBasedEncryptionSecrets is a utility function for providers using UNKNOWN_KEY_TYPE
// (file-based encryption). It generates a key, creates the Kubernetes secret, and verifies it.
// This is the common implementation for Local and GCP providers (when not using cloud KMS).
func SetupFileBasedEncryptionSecrets(t *testing.T, kubectlOptions *k8s.KubectlOptions, providerName string) error {
	t.Logf("%s provider: Setting up file-based encryption secrets", providerName)

	// Generate and encode encryption key
	_, base64Key := GenerateAndEncodeEncryptionKey(t)
	t.Logf("Generated encryption key (base64 length: %d)", len(base64Key))

	// Create Kubernetes secret with base64-encoded key
	// Use default secret name for file-based encryption
	secretName := "cmek-key-secret"

	err := k8s.RunKubectlE(t, kubectlOptions, "create", "secret", "generic", secretName,
		fmt.Sprintf("--from-literal=StoreKeyData=%s", base64Key))
	if err != nil {
		return fmt.Errorf("failed to create encryption key secret: %w", err)
	}

	// Verify secret was created with data
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

	// No credentials secret needed for file-based encryption (UNKNOWN_KEY_TYPE)
	return nil
}
