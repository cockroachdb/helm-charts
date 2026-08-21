package operator

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gruntwork-io/terratest/modules/k8s"
	"github.com/gruntwork-io/terratest/modules/retry"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

const defaultClusterDomain = "cluster.local"

// ProviderRuntime lets a provider adjust chart install and cleanup behavior
// without leaking provider-specific rules into the shared Region workflow.
type ProviderRuntime interface {
	ChartCloudProvider(provider string) string
	ClusterDomain(region *Region, index int) string
	PatchHelmValues(values map[string]string) map[string]string
	ConfigureNamespace(t *testing.T, kubectlOptions *k8s.KubectlOptions, namespace string)
	HelmDeleteArgs(args []string) []string
	CleanupNamespace(t *testing.T, kubectlOptions *k8s.KubectlOptions, namespace string)
}

type defaultProviderRuntime struct{}

func (defaultProviderRuntime) ChartCloudProvider(provider string) string {
	return provider
}

func (defaultProviderRuntime) ClusterDomain(_ *Region, index int) string {
	if domain, ok := CustomDomains[index]; ok {
		return domain
	}
	return defaultClusterDomain
}

func (defaultProviderRuntime) PatchHelmValues(values map[string]string) map[string]string {
	return values
}

func (defaultProviderRuntime) ConfigureNamespace(_ *testing.T, _ *k8s.KubectlOptions, _ string) {
}

func (defaultProviderRuntime) HelmDeleteArgs(args []string) []string {
	return args
}

func (defaultProviderRuntime) CleanupNamespace(_ *testing.T, _ *k8s.KubectlOptions, _ string) {
}

func (r *Region) runtime() ProviderRuntime {
	if r.providerRuntime != nil {
		return r.providerRuntime
	}
	return defaultProviderRuntime{}
}

func (r *Region) chartCloudProvider() string {
	return r.runtime().ChartCloudProvider(r.Provider)
}

func (r *Region) clusterDomain(index int) string {
	return r.runtime().ClusterDomain(r, index)
}

func (r *Region) prepareNamespace(t *testing.T, kubectlOptions *k8s.KubectlOptions, namespace string) {
	if !r.shouldRetryKubernetesAction(kubectlOptions) {
		k8s.CreateNamespace(t, kubectlOptions, namespace)
		r.runtime().ConfigureNamespace(t, kubectlOptions, namespace)
		return
	}

	_, err := retry.DoWithRetryE(t, "create namespace "+namespace, 12, 5*time.Second, func() (string, error) {
		err := k8s.CreateNamespaceE(t, kubectlOptions, namespace)
		if apierrors.IsAlreadyExists(err) {
			return "", nil
		}
		return "", err
	})
	require.NoError(t, err)
	r.runtime().ConfigureNamespace(t, kubectlOptions, namespace)
}

func (r *Region) shouldRetryKubernetesAction(opts *k8s.KubectlOptions) bool {
	if strings.EqualFold(strings.TrimSpace(r.Provider), "azure") {
		return true
	}
	if strings.EqualFold(strings.TrimSpace(os.Getenv("PROVIDER")), "azure") {
		return true
	}
	return opts != nil && strings.HasPrefix(opts.ContextName, "azure-")
}

func (r *Region) CockroachDBHelmValues(index int, values map[string]string) map[string]string {
	if values == nil {
		values = make(map[string]string)
	}
	if _, ok := values["cockroachdb.clusterDomain"]; !ok {
		values["cockroachdb.clusterDomain"] = r.clusterDomain(index)
	}
	return r.runtime().PatchHelmValues(PatchHelmValues(values))
}

func (r *Region) helmDeleteArgs(args ...string) []string {
	return r.runtime().HelmDeleteArgs(args)
}

func (r *Region) cleanupNamespace(t *testing.T, kubectlOptions *k8s.KubectlOptions, namespace string) {
	r.runtime().CleanupNamespace(t, kubectlOptions, namespace)
}
