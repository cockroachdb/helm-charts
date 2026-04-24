// Package utils provides common utilities for E2E tests to avoid duplication
// between single-region and multi-region test suites.
package utils

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/gruntwork-io/terratest/modules/random"
)

const (
	// Provider constants (duplicated from infra package to avoid import cycles)

	ProviderK3D  = "k3d"
	ProviderKind = "kind"
	ProviderGCP  = "gcp"
	ProviderAWS  = "aws"
)

// GetProviderFromEnv reads the PROVIDER environment variable and returns the provider string.
// Returns ProviderK3D as default if no provider is specified.
func GetProviderFromEnv() (string, error) {
	p := strings.TrimSpace(strings.ToLower(os.Getenv("PROVIDER")))
	if p == "" {
		return ProviderK3D, nil
	}

	switch p {
	case "k3d":
		return ProviderK3D, nil
	case "kind":
		return ProviderKind, nil
	case "gcp":
		return ProviderGCP, nil
	case "aws":
		return ProviderAWS, nil
	default:
		return "", fmt.Errorf("unsupported provider: %s", p)
	}
}

// GenerateTestRunID creates a unique test run ID for resource isolation.
// Format: <random-id>-<unix-timestamp>
func GenerateTestRunID() string {
	return fmt.Sprintf("%s-%d", strings.ToLower(random.UniqueId()), time.Now().Unix())
}

// GenerateClusterNames creates cluster names with context about who is running the test.
// For GitHub CI: includes PR number (e.g., aws-chart-testing-pr602-cluster-0-abc123)
// For local runs: includes username (e.g., aws-chart-testing-bhaskar-cluster-0-abc123)
// For K3D/Kind: includes provider prefix to match kubeconfig context names
//
//	(e.g., k3d-chart-testing-cluster-0 matches the context created by k3d)
//
// Parameters:
//   - provider: The cloud provider (aws, gcp, k3d, kind)
//   - clusterCount: Number of clusters to create names for
//
// Returns: Array of cluster names
func GenerateClusterNames(provider string, clusterCount int) []string {
	// Start with provider prefix for all providers
	// For k3d/kind, this matches the kubeconfig context name (k3d adds "k3d-" prefix to contexts)
	var clusterPrefix string
	clusterPrefix = fmt.Sprintf("%s-%s", provider, "chart-testing")

	// For cloud providers (AWS, GCP), add GitHub PR number or username for isolation
	// For local providers (k3d, kind), keep simple names without PR/username
	if provider != ProviderK3D && provider != ProviderKind {
		// Add GitHub PR number if running in CI
		if prNumber := os.Getenv("GITHUB_PR_NUMBER"); prNumber != "" {
			clusterPrefix = fmt.Sprintf("%s-pr%s", clusterPrefix, prNumber)
		} else if ghRef := os.Getenv("GITHUB_REF"); strings.Contains(ghRef, "/pull/") {
			// Extract PR number from GITHUB_REF (e.g., refs/pull/123/merge)
			parts := strings.Split(ghRef, "/")
			if len(parts) >= 3 {
				clusterPrefix = fmt.Sprintf("%s-pr%s", clusterPrefix, parts[2])
			}
		} else {
			// For local runs, add username
			if username := os.Getenv("USER"); username != "" {
				// Sanitize username for Kubernetes naming (lowercase, alphanumeric and dash only)
				username = strings.ToLower(username)
				username = strings.Map(func(r rune) rune {
					if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
						return r
					}
					return '-'
				}, username)
				// Truncate username to max 10 characters to avoid exceeding 63-char label limit
				if len(username) > 10 {
					username = username[:10]
				}
				clusterPrefix = fmt.Sprintf("%s-%s", clusterPrefix, username)
			}
		}
	}

	clusterNames := make([]string, 0, clusterCount)
	for i := 0; i < clusterCount; i++ {
		clusterName := fmt.Sprintf("%s-cluster-%d", clusterPrefix, i)
		// Add random suffix for cloud providers (not for local K3D/Kind)
		if provider != ProviderK3D && provider != ProviderKind {
			clusterName = fmt.Sprintf("%s-%s", clusterName, strings.ToLower(random.UniqueId()))
		}
		clusterNames = append(clusterNames, clusterName)
	}

	return clusterNames
}
