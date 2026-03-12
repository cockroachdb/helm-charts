package multiRegion

import (
	"fmt"
	"strings"
	"testing"

	"github.com/cockroachdb/helm-charts/tests/e2e/operator"
	"github.com/gruntwork-io/terratest/modules/k8s"
	"github.com/gruntwork-io/terratest/modules/random"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TestWALFailoverMultiRegion tests WAL failover with different paths in each region
// Region 0: WAL failover enabled with custom path
// Region 1: WAL failover disabled
func (r *multiRegion) TestWALFailoverMultiRegion(t *testing.T) {
	// Setup namespaces and CA for each region
	cleanup := r.SetupMultiClusterWithCA(t)
	defer cleanup()

	// Region 0: Install with WAL failover enabled
	cluster0 := r.Clusters[0]
	walPath0 := "/cockroach/wal-region-0"

	t.Logf("Installing region 0 (%s) with WAL failover enabled at path %s", cluster0, walPath0)
	config0 := operator.AdvancedInstallConfig{
		WALFailoverEnabled: true,
		WALFailoverSize:    "5Gi",
		CustomValues: map[string]string{
			"cockroachdb.crdbCluster.walFailoverSpec.path": walPath0,
		},
	}
	r.InstallChartsWithAdvancedConfig(t, cluster0, 0, config0)

	// Region 1: Install without WAL failover
	cluster1 := r.Clusters[1]
	t.Logf("Installing region 1 (%s) without WAL failover", cluster1)
	config1 := operator.AdvancedInstallConfig{}
	r.InstallChartsWithAdvancedConfig(t, cluster1, 1, config1)

	// Validate CockroachDB cluster health in both regions
	for _, cluster := range r.Clusters {
		r.ValidateCRDB(t, cluster)
	}

	// Validate multi-region setup
	r.ValidateMultiRegionSetup(t)

	// Validate WAL failover in region 0
	t.Log("Validating WAL failover in region 0...")
	r.ValidateWALFailover(t, cluster0, &operator.AdvancedValidationConfig{
		WALFailover: operator.WALFailoverValidation{
			CustomPath: walPath0,
		},
	})

	// Validate NO WAL failover in region 1
	t.Log("Validating NO WAL failover in region 1...")
	kubeConfig, _ := r.GetCurrentContext(t)
	kubectlOptions1 := k8s.NewKubectlOptions(cluster1, kubeConfig, r.Namespace[cluster1])

	pods := k8s.ListPods(t, kubectlOptions1, metav1.ListOptions{
		LabelSelector: operator.LabelSelector,
	})
	require.True(t, len(pods) > 0, "No CockroachDB pods found in region 1")

	podCommand, err := k8s.RunKubectlAndGetOutputE(t, kubectlOptions1,
		"get", "pod", pods[0].Name, "-o", "jsonpath={.spec.containers[?(@.name=='cockroachdb')].command}")
	require.NoError(t, err)
	require.NotContains(t, podCommand, "--wal-failover", "Region 1 should not have WAL failover enabled")
	t.Log("Confirmed region 1 does not have WAL failover")

	t.Logf("WAL failover multi-region test completed successfully")
}

// TestEncryptionAtRestMultiRegion tests encryption at rest with different secrets per region
// Region 0: Encryption enabled with secret "cmek-key-secret-region-0"
// Region 1: Encryption disabled (no encryption)
func (r *multiRegion) TestEncryptionAtRestMultiRegion(t *testing.T) {
	// Setup namespaces and CA for each region
	cleanup := r.SetupMultiClusterWithCA(t)
	defer cleanup()

	// Generate encryption key for region 0
	encryptionKeyB64 := r.GenerateEncryptionKey(t)
	t.Logf("Generated encryption key for region 0 (base64 length: %d)", len(encryptionKeyB64))

	// Region 0: Install with encryption at rest enabled
	cluster0 := r.Clusters[0]
	secretName0 := "cmek-key-secret-region-0"

	encryptionRegions0 := []map[string]interface{}{
		{
			"code":          r.RegionCodes[0],
			"cloudProvider": r.Provider,
			"nodes":         r.NodeCount,
			"namespace":     r.Namespace[cluster0],
			"domain":        operator.CustomDomains[0],
			"encryptionAtRest": map[string]interface{}{
				"platform":      "UNKNOWN_KEY_TYPE",
				"keySecretName": secretName0,
			},
		},
	}

	t.Logf("Installing region 0 (%s) with encryption at rest enabled", cluster0)
	config0 := operator.AdvancedInstallConfig{
		EncryptionEnabled:   true,
		EncryptionKeySecret: encryptionKeyB64,
		CustomRegions:       encryptionRegions0,
		CustomValues: map[string]string{
			// Override secret name to use custom name
			"cockroachdb.crdbCluster.encryptionKeySecretName": secretName0,
		},
	}
	r.InstallChartsWithAdvancedConfig(t, cluster0, 0, config0)

	// Manually create the secret with custom name in region 0
	kubeConfig, _ := r.GetCurrentContext(t)
	kubectlOptions0 := k8s.NewKubectlOptions(cluster0, kubeConfig, r.Namespace[cluster0])

	// Delete the default secret if it exists
	_ = k8s.RunKubectlE(t, kubectlOptions0, "delete", "secret", "cmek-key-secret", "--ignore-not-found")

	// Create the custom named secret
	err := k8s.RunKubectlE(t, kubectlOptions0, "create", "secret", "generic", secretName0,
		fmt.Sprintf("--from-literal=StoreKeyData=%s", encryptionKeyB64))
	require.NoError(t, err)
	t.Logf("Created encryption secret %s in region 0", secretName0)

	// Region 1: Install without encryption
	cluster1 := r.Clusters[1]
	t.Logf("Installing region 1 (%s) without encryption at rest", cluster1)
	config1 := operator.AdvancedInstallConfig{}
	r.InstallChartsWithAdvancedConfig(t, cluster1, 1, config1)

	// Validate CockroachDB cluster health in both regions
	for _, cluster := range r.Clusters {
		r.ValidateCRDB(t, cluster)
	}

	// Validate multi-region setup
	r.ValidateMultiRegionSetup(t)

	// Validate encryption in region 0
	t.Log("Validating encryption at rest in region 0...")
	r.ValidateEncryptionAtRest(t, cluster0, &operator.AdvancedValidationConfig{
		EncryptionAtRest: operator.EncryptionAtRestValidation{
			SecretName: secretName0,
		},
	})

	// Validate NO encryption in region 1
	t.Log("Validating NO encryption at rest in region 1...")
	kubectlOptions1 := k8s.NewKubectlOptions(cluster1, kubeConfig, r.Namespace[cluster1])

	pods := k8s.ListPods(t, kubectlOptions1, metav1.ListOptions{
		LabelSelector: operator.LabelSelector,
	})
	require.True(t, len(pods) > 0, "No CockroachDB pods found in region 1")

	podCommand, err := k8s.RunKubectlAndGetOutputE(t, kubectlOptions1,
		"get", "pod", pods[0].Name, "-o", "jsonpath={.spec.containers[?(@.name=='cockroachdb')].command}")
	require.NoError(t, err)
	require.NotContains(t, podCommand, "--enterprise-encryption", "Region 1 should not have encryption enabled")
	t.Log("Confirmed region 1 does not have encryption at rest")

	t.Logf("Encryption at rest multi-region test completed successfully")
}

// TestPCRMultiRegion tests Physical Cluster Replication with multi-region setup
// Creates a multi-region primary cluster, then creates a standby cluster and tests failover/failback
func (r *multiRegion) TestPCRMultiRegion(t *testing.T) {
	// Creating random namespace for primary multi-region cluster
	for _, cluster := range r.Clusters {
		r.Namespace[cluster] = fmt.Sprintf("%s-primary-%s", operator.Namespace, strings.ToLower(random.UniqueId()))
	}

	// Create CA certificate once for all clusters
	cleanupCA := r.RequireCACertificate(t)
	defer cleanupCA()

	var standbyNamespace string

	// Cleanup both primary and standby clusters
	defer r.CleanupResources(t)
	defer func() {
		if standbyNamespace != "" {
			kubectlOptions := k8s.NewKubectlOptions("", "", standbyNamespace)
			k8s.DeleteNamespace(t, kubectlOptions, standbyNamespace)
		}
	}()

	// Step 1: Install primary multi-region cluster
	t.Log("Installing primary multi-region cluster...")
	for i, cluster := range r.Clusters {
		primaryConfig := operator.AdvancedInstallConfig{
			VirtualClusterMode: "primary",
		}
		if i != 0 {
			// Subsequent regions skip operator installation
			primaryConfig.SkipOperatorInstall = true
		}
		r.InstallChartsWithAdvancedConfig(t, cluster, i, primaryConfig)
	}

	// Validate primary cluster health in all regions
	r.VirtualClusterModePrimary = true
	for _, cluster := range r.Clusters {
		r.ValidateCRDB(t, cluster)
	}
	r.VirtualClusterModePrimary = false

	// Validate multi-region setup
	r.ValidateMultiRegionSetup(t)
	t.Log("Primary multi-region cluster is healthy")

	// Step 2: Install standby cluster (single region for simplicity)
	t.Log("Installing standby cluster...")
	standbyCluster := r.Clusters[0] // Use first cluster for standby
	standbyNamespace = fmt.Sprintf("%s-standby-%s", operator.Namespace, strings.ToLower(random.UniqueId()))

	// Temporarily update namespace for standby installation
	originalNamespace := r.Namespace[standbyCluster]
	r.Namespace[standbyCluster] = standbyNamespace

	standbyConfig := operator.AdvancedInstallConfig{
		VirtualClusterMode:  "standby",
		SkipOperatorInstall: true, // Operator already installed
	}
	r.InstallChartsWithAdvancedConfig(t, standbyCluster, 0, standbyConfig)

	// Validate standby cluster
	r.VirtualClusterModeStandby = true
	r.ValidateCRDB(t, standbyCluster)
	r.VirtualClusterModeStandby = false
	t.Log("Standby cluster is healthy")

	// Step 3: Set up replication and test failover/failback
	t.Log("Testing PCR failover and failback...")
	r.ValidatePCR(t, &operator.AdvancedValidationConfig{
		PCR: operator.PCRValidation{
			Cluster:          standbyCluster,
			PrimaryNamespace: originalNamespace,
			StandbyNamespace: standbyNamespace,
		},
	})

	// Restore original namespace
	r.Namespace[standbyCluster] = originalNamespace

	t.Logf("PCR multi-region test completed successfully")
}
