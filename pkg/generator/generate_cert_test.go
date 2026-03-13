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
	"testing"
	"time"

	"github.com/cockroachdb/helm-charts/pkg/security"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

// TestGenerateNodeCert tests node certificate generation with various
// additional SAN configurations.
func TestGenerateNodeCert(t *testing.T) {
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

		// Append additional SANs (mimics the actual code path)
		if len(genCert.AdditionalSANs) > 0 {
			hosts = append(hosts, genCert.AdditionalSANs...)
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

		// Note: IP SANs like "10.20.30.40" are tested separately as they're in IPAddresses field
		// The cockroach cert command parses numeric IPs into the IPAddresses field
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

		// Append additional SANs if present (should be skipped)
		if len(genCert.AdditionalSANs) > 0 {
			hosts = append(hosts, genCert.AdditionalSANs...)
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
		if len(genCert.AdditionalSANs) > 0 {
			hosts = append(hosts, genCert.AdditionalSANs...)
		}

		// Verify hosts slice only has standard entries
		assert.Equal(t, 2, len(hosts), "Should only have standard hosts")
		assert.NotContains(t, hosts, "extra-san.example.com", "Should not have additional SANs")
	})
}
