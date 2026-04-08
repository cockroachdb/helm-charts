/*
Copyright 2026 The Cockroach Authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    https://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package generator

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/cockroachdb/helm-charts/pkg/security"
)

const testCALifetime = 5 * 366 * 24 * time.Hour   // five years
const testCertLifetime = 1 * 366 * 24 * time.Hour // one year

// tempDir creates a temporary directory for testing.
func tempDir(t *testing.T) (string, func()) {
	certsDir, err := os.MkdirTemp("", "generator_test")
	if err != nil {
		t.Fatal(err)
	}
	return certsDir, func() {
		if err := os.RemoveAll(certsDir); err != nil {
			t.Fatal(err)
		}
	}
}

// TestCreateNodeCertWithAdditionalSANs tests node certificate generation with various
// additional SAN configurations.
func TestCreateNodeCertWithAdditionalSANs(t *testing.T) {
	t.Run("WithAdditionalSANs", func(t *testing.T) {
		certsDir, cleanup := tempDir(t)
		defer cleanup()
		caKey := filepath.Join(certsDir, "ca.key")

		// Create CA first
		err := security.CreateCAPair(certsDir, caKey, defaultKeySize, testCALifetime, true, true)
		require.NoError(t, err, "Failed to create CA pair")

		// Create GenerateCert instance with additional SANs
		genCert := GenerateCert{
			CertsDir:             certsDir,
			CAKey:                caKey,
			PublicServiceName:    "test-public",
			DiscoveryServiceName: "test-service",
			ClusterDomain:        "cluster.local",
			NodeCertConfig: &certConfig{
				Duration:     testCertLifetime,
				ExpiryWindow: 7 * 24 * time.Hour,
			},
			AdditionalSANs: []string{
				"my-loadbalancer.example.com",
				"10.20.30.40",
				"backup-lb.example.com",
			},
		}

		// Generate node certificate (simplified version without K8s client)
		namespace := "default"
		hosts := []string{
			"localhost",
			"127.0.0.1",
			genCert.PublicServiceName,
			genCert.PublicServiceName + "." + namespace,
			genCert.PublicServiceName + "." + namespace + ".svc." + genCert.ClusterDomain,
			"*." + genCert.DiscoveryServiceName,
			"*." + genCert.DiscoveryServiceName + "." + namespace,
			"*." + genCert.DiscoveryServiceName + "." + namespace + ".svc." + genCert.ClusterDomain,
		}

		// Append additional SANs using the same sanitization as production code
		validSANs := SanitizeAdditionalSANs(genCert.AdditionalSANs)
		if len(validSANs) > 0 {
			hosts = append(hosts, validSANs...)
		}

		// Create node certificate with all hosts
		err = security.CreateNodePair(
			certsDir,
			caKey,
			defaultKeySize,
			genCert.NodeCertConfig.Duration,
			true,
			hosts,
		)
		require.NoError(t, err, "Failed to create node certificate")

		// Read and parse the generated certificate
		pemCert, err := os.ReadFile(filepath.Join(certsDir, "node.crt"))
		require.NoError(t, err, "Failed to read node certificate")

		cert, err := security.GetCertObj(pemCert)
		require.NoError(t, err, "Failed to parse certificate")

		// Verify standard SANs are present
		standardSANs := []string{
			"localhost",
			"test-public",
			"test-public.default",
			"test-public.default.svc.cluster.local",
			"*.test-service",
			"*.test-service.default",
			"*.test-service.default.svc.cluster.local",
		}

		for _, san := range standardSANs {
			assert.Contains(t, cert.DNSNames, san, "Standard SAN %s should be present", san)
		}

		// Verify additional SANs are present
		additionalDNSSANs := []string{
			"my-loadbalancer.example.com",
			"backup-lb.example.com",
		}

		for _, san := range additionalDNSSANs {
			assert.Contains(t, cert.DNSNames, san, "Additional DNS SAN %s should be present", san)
		}

		// Verify IP SANs are present in IPAddresses field
		// The cockroach cert command parses numeric IPs into the IPAddresses field
		require.NotEmpty(t, cert.IPAddresses, "Certificate should contain IP addresses")
		foundIP := false
		for _, ip := range cert.IPAddresses {
			if ip.String() == "10.20.30.40" {
				foundIP = true
				break
			}
		}
		assert.True(t, foundIP, "Additional IP SAN 10.20.30.40 should be present in IPAddresses")
	})

	t.Run("NoAdditionalSANs", func(t *testing.T) {
		certsDir, cleanup := tempDir(t)
		defer cleanup()
		caKey := filepath.Join(certsDir, "ca.key")

		// Create CA first
		err := security.CreateCAPair(certsDir, caKey, defaultKeySize, testCALifetime, true, true)
		require.NoError(t, err, "Failed to create CA pair")

		// Create GenerateCert instance WITHOUT additional SANs
		genCert := GenerateCert{
			CertsDir:             certsDir,
			CAKey:                caKey,
			PublicServiceName:    "test-public",
			DiscoveryServiceName: "test-service",
			ClusterDomain:        "cluster.local",
			NodeCertConfig: &certConfig{
				Duration:     testCertLifetime,
				ExpiryWindow: 7 * 24 * time.Hour,
			},
			AdditionalSANs: nil, // No additional SANs
		}

		namespace := "default"
		hosts := []string{
			"localhost",
			"127.0.0.1",
			genCert.PublicServiceName,
			genCert.PublicServiceName + "." + namespace,
			genCert.PublicServiceName + "." + namespace + ".svc." + genCert.ClusterDomain,
			"*." + genCert.DiscoveryServiceName,
			"*." + genCert.DiscoveryServiceName + "." + namespace,
			"*." + genCert.DiscoveryServiceName + "." + namespace + ".svc." + genCert.ClusterDomain,
		}

		// Append additional SANs using sanitization (should be skipped)
		validSANs := SanitizeAdditionalSANs(genCert.AdditionalSANs)
		if len(validSANs) > 0 {
			hosts = append(hosts, validSANs...)
		}

		// Create node certificate
		err = security.CreateNodePair(certsDir, caKey, defaultKeySize, genCert.NodeCertConfig.Duration, true, hosts)
		require.NoError(t, err, "Failed to create node certificate")

		// Read and parse the generated certificate
		pemCert, err := os.ReadFile(filepath.Join(certsDir, "node.crt"))
		require.NoError(t, err, "Failed to read node certificate")

		cert, err := security.GetCertObj(pemCert)
		require.NoError(t, err, "Failed to parse certificate")

		// Verify only standard SANs are present
		standardSANs := []string{
			"localhost",
			"test-public",
			"test-public.default",
			"test-public.default.svc.cluster.local",
			"*.test-service",
			"*.test-service.default",
			"*.test-service.default.svc.cluster.local",
		}

		for _, san := range standardSANs {
			assert.Contains(t, cert.DNSNames, san, "Standard SAN %s should be present", san)
		}

		// Verify no unexpected additional SANs
		assert.NotContains(t, cert.DNSNames, "my-loadbalancer.example.com", "Should not contain additional SANs")
		assert.NotContains(t, cert.DNSNames, "backup-lb.example.com", "Should not contain additional SANs")
	})

	t.Run("EmptyAdditionalSANs", func(t *testing.T) {
		certsDir, cleanup := tempDir(t)
		defer cleanup()
		caKey := filepath.Join(certsDir, "ca.key")

		// Create CA first
		err := security.CreateCAPair(certsDir, caKey, defaultKeySize, testCALifetime, true, true)
		require.NoError(t, err, "Failed to create CA pair")

		genCert := GenerateCert{
			CertsDir:             certsDir,
			CAKey:                caKey,
			PublicServiceName:    "test-public",
			DiscoveryServiceName: "test-service",
			ClusterDomain:        "cluster.local",
			NodeCertConfig: &certConfig{
				Duration:     testCertLifetime,
				ExpiryWindow: 7 * 24 * time.Hour,
			},
			AdditionalSANs: make([]string, 0), // Empty slice
		}

		hosts := []string{
			"localhost",
			genCert.PublicServiceName,
		}

		// This should not append anything
		validSANs := SanitizeAdditionalSANs(genCert.AdditionalSANs)
		if len(validSANs) > 0 {
			hosts = append(hosts, validSANs...)
		}

		// Verify hosts slice only has standard entries
		assert.Equal(t, 2, len(hosts), "Should only have standard hosts")
		assert.NotContains(t, hosts, "extra-san.example.com", "Should not have additional SANs")
	})

	t.Run("SanitizeAdditionalSANs", func(t *testing.T) {
		certsDir, cleanup := tempDir(t)
		defer cleanup()
		caKey := filepath.Join(certsDir, "ca.key")

		// Create CA first
		err := security.CreateCAPair(certsDir, caKey, defaultKeySize, testCALifetime, true, true)
		require.NoError(t, err, "Failed to create CA pair")

		// Test with SANs that have whitespace, empty strings, and valid entries
		genCert := GenerateCert{
			CertsDir:             certsDir,
			CAKey:                caKey,
			PublicServiceName:    "test-public",
			DiscoveryServiceName: "test-service",
			ClusterDomain:        "cluster.local",
			NodeCertConfig: &certConfig{
				Duration:     testCertLifetime,
				ExpiryWindow: 7 * 24 * time.Hour,
			},
			AdditionalSANs: []string{
				"valid-san.example.com",
				"  whitespace-before.example.com",
				"whitespace-after.example.com  ",
				"  whitespace-both.example.com  ",
				"",    // empty string
				"   ", // whitespace only
				"192.168.1.1",
				"  192.168.1.2  ",
			},
		}

		namespace := "default"
		hosts := []string{
			"localhost",
			"127.0.0.1",
			genCert.PublicServiceName,
			genCert.PublicServiceName + "." + namespace,
			genCert.PublicServiceName + "." + namespace + ".svc." + genCert.ClusterDomain,
		}

		// Apply sanitization like production code
		validSANs := SanitizeAdditionalSANs(genCert.AdditionalSANs)
		require.Len(t, validSANs, 6, "Should have 6 valid SANs after filtering empty/whitespace")

		// Verify sanitized SANs are trimmed
		assert.Contains(t, validSANs, "valid-san.example.com")
		assert.Contains(t, validSANs, "whitespace-before.example.com")
		assert.Contains(t, validSANs, "whitespace-after.example.com")
		assert.Contains(t, validSANs, "whitespace-both.example.com")
		assert.Contains(t, validSANs, "192.168.1.1")
		assert.Contains(t, validSANs, "192.168.1.2")

		// Verify no entries with leading/trailing whitespace
		for _, san := range validSANs {
			assert.Equal(t, strings.TrimSpace(san), san, "SAN should not have leading/trailing whitespace: %q", san)
		}

		hosts = append(hosts, validSANs...)

		// Create node certificate with sanitized hosts
		err = security.CreateNodePair(
			certsDir,
			caKey,
			defaultKeySize,
			genCert.NodeCertConfig.Duration,
			true,
			hosts,
		)
		require.NoError(t, err, "Failed to create node certificate")

		// Read and parse the generated certificate
		pemCert, err := os.ReadFile(filepath.Join(certsDir, "node.crt"))
		require.NoError(t, err, "Failed to read node certificate")

		cert, err := security.GetCertObj(pemCert)
		require.NoError(t, err, "Failed to parse certificate")

		// Verify sanitized DNS SANs are present
		assert.Contains(t, cert.DNSNames, "valid-san.example.com")
		assert.Contains(t, cert.DNSNames, "whitespace-before.example.com")
		assert.Contains(t, cert.DNSNames, "whitespace-after.example.com")
		assert.Contains(t, cert.DNSNames, "whitespace-both.example.com")

		// Verify IP SANs are present
		ipStrings := make([]string, len(cert.IPAddresses))
		for i, ip := range cert.IPAddresses {
			ipStrings[i] = ip.String()
		}
		assert.Contains(t, ipStrings, "192.168.1.1")
		assert.Contains(t, ipStrings, "192.168.1.2")
	})
}

// TestSanitizeAdditionalSANs tests the SAN sanitization helper function directly.
func TestSanitizeAdditionalSANs(t *testing.T) {
	tests := []struct {
		name     string
		input    []string
		expected []string
	}{
		{
			name:     "nil input",
			input:    nil,
			expected: nil,
		},
		{
			name:     "empty slice",
			input:    []string{},
			expected: nil,
		},
		{
			name:     "valid SANs",
			input:    []string{"host1.example.com", "192.168.1.1"},
			expected: []string{"host1.example.com", "192.168.1.1"},
		},
		{
			name:     "SANs with leading whitespace",
			input:    []string{"  host1.example.com", "  192.168.1.1"},
			expected: []string{"host1.example.com", "192.168.1.1"},
		},
		{
			name:     "SANs with trailing whitespace",
			input:    []string{"host1.example.com  ", "192.168.1.1  "},
			expected: []string{"host1.example.com", "192.168.1.1"},
		},
		{
			name:     "SANs with both leading and trailing whitespace",
			input:    []string{"  host1.example.com  ", "  192.168.1.1  "},
			expected: []string{"host1.example.com", "192.168.1.1"},
		},
		{
			name:     "empty strings filtered out",
			input:    []string{"host1.example.com", "", "192.168.1.1"},
			expected: []string{"host1.example.com", "192.168.1.1"},
		},
		{
			name:     "whitespace-only strings filtered out",
			input:    []string{"host1.example.com", "   ", "192.168.1.1"},
			expected: []string{"host1.example.com", "192.168.1.1"},
		},
		{
			name:     "mixed valid, empty, and whitespace",
			input:    []string{"  host1.example.com", "", "   ", "192.168.1.1  ", "host2.example.com"},
			expected: []string{"host1.example.com", "192.168.1.1", "host2.example.com"},
		},
		{
			name:     "all empty or whitespace",
			input:    []string{"", "   ", "\t", "  \n  "},
			expected: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := SanitizeAdditionalSANs(tt.input)
			if tt.expected == nil {
				assert.Nil(t, result, "Expected nil result")
			} else {
				assert.Equal(t, tt.expected, result, "Sanitized SANs should match expected")
			}
		})
	}
}
