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
	"crypto/x509"
	"os"
	"path/filepath"
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

// setupTestCertEnv creates a test environment with temp dir, CA pair, and GenerateCert instance.
func setupTestCertEnv(t *testing.T, additionalSANs []string) (string, string, GenerateCert, func()) {
	certsDir, cleanup := tempDir(t)
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
		AdditionalSANs: additionalSANs,
	}

	return certsDir, caKey, genCert, cleanup
}

// buildStandardHosts creates the standard hosts list for node certificates.
func buildStandardHosts(genCert GenerateCert, namespace string) []string {
	return []string{
		"localhost",
		"127.0.0.1",
		genCert.PublicServiceName,
		genCert.PublicServiceName + "." + namespace,
		genCert.PublicServiceName + "." + namespace + ".svc." + genCert.ClusterDomain,
		"*." + genCert.DiscoveryServiceName,
		"*." + genCert.DiscoveryServiceName + "." + namespace,
		"*." + genCert.DiscoveryServiceName + "." + namespace + ".svc." + genCert.ClusterDomain,
	}
}

// createAndParseCert creates a node certificate and returns the parsed x509 certificate.
func createAndParseCert(t *testing.T, certsDir, caKey string, genCert GenerateCert, hosts []string) *x509.Certificate {
	// Create node certificate
	err := security.CreateNodePair(
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

	return cert
}

// getStandardSANs returns the list of standard SANs expected in all node certificates.
func getStandardSANs() []string {
	return []string{
		"localhost",
		"test-public",
		"test-public.default",
		"test-public.default.svc.cluster.local",
		"*.test-service",
		"*.test-service.default",
		"*.test-service.default.svc.cluster.local",
	}
}

// TestCreateNodeCertWithAdditionalSANs tests node certificate generation with various
// additional SAN configurations using a table-driven approach.
func TestCreateNodeCertWithAdditionalSANs(t *testing.T) {
	tests := []struct {
		name                string
		additionalSANs      []string
		expectedDNSSANs     []string
		expectedIPSANs      []string
		unexpectedDNSSANs   []string
		sanitizeAndValidate bool
	}{
		{
			name: "WithAdditionalSANs",
			additionalSANs: []string{
				"my-loadbalancer.example.com",
				"10.20.30.40",
				"backup-lb.example.com",
			},
			expectedDNSSANs: []string{
				"my-loadbalancer.example.com",
				"backup-lb.example.com",
			},
			expectedIPSANs:    []string{"10.20.30.40"},
			unexpectedDNSSANs: nil,
		},
		{
			name:              "NoAdditionalSANs",
			additionalSANs:    nil,
			expectedDNSSANs:   nil,
			expectedIPSANs:    nil,
			unexpectedDNSSANs: []string{"my-loadbalancer.example.com", "backup-lb.example.com"},
		},
		{
			name: "SanitizeAdditionalSANs",
			additionalSANs: []string{
				"valid-san.example.com",
				"  whitespace-before.example.com",
				"whitespace-after.example.com  ",
				"  whitespace-both.example.com  ",
				"",    // empty string
				"   ", // whitespace only
				"192.168.1.1",
				"  192.168.1.2  ",
			},
			expectedDNSSANs: []string{
				"valid-san.example.com",
				"whitespace-before.example.com",
				"whitespace-after.example.com",
				"whitespace-both.example.com",
			},
			expectedIPSANs:      []string{"192.168.1.1", "192.168.1.2"},
			sanitizeAndValidate: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			certsDir, caKey, genCert, cleanup := setupTestCertEnv(t, tt.additionalSANs)
			defer cleanup()

			namespace := "default"
			hosts := buildStandardHosts(genCert, namespace)

			// Apply sanitization and append additional SANs
			validSANs := SanitizeAdditionalSANs(genCert.AdditionalSANs)
			if tt.sanitizeAndValidate {
				// For sanitization test, verify the sanitization worked correctly
				expectedValidCount := len(tt.expectedDNSSANs) + len(tt.expectedIPSANs)
				require.Len(t, validSANs, expectedValidCount, "Should have correct number of valid SANs after filtering empty/whitespace")
			}
			if len(validSANs) > 0 {
				hosts = append(hosts, validSANs...)
			}

			// Create and parse certificate
			cert := createAndParseCert(t, certsDir, caKey, genCert, hosts)

			// Verify standard SANs are always present
			for _, san := range getStandardSANs() {
				assert.Contains(t, cert.DNSNames, san, "Standard SAN %s should be present", san)
			}

			// Verify expected additional DNS SANs
			for _, san := range tt.expectedDNSSANs {
				assert.Contains(t, cert.DNSNames, san, "Additional DNS SAN %s should be present", san)
			}

			// Verify expected IP SANs
			if len(tt.expectedIPSANs) > 0 {
				require.NotEmpty(t, cert.IPAddresses, "Certificate should contain IP addresses")
				ipStrings := make([]string, len(cert.IPAddresses))
				for i, ip := range cert.IPAddresses {
					ipStrings[i] = ip.String()
				}
				for _, expectedIP := range tt.expectedIPSANs {
					assert.Contains(t, ipStrings, expectedIP, "IP SAN %s should be present", expectedIP)
				}
			}

			// Verify unexpected SANs are not present
			for _, san := range tt.unexpectedDNSSANs {
				assert.NotContains(t, cert.DNSNames, san, "Should not contain unexpected SAN %s", san)
			}
		})
	}
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
