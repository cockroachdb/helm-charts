package singleRegion

import (
	"fmt"
	"github.com/cockroachdb/helm-charts/tests/e2e/operator"
	"github.com/gruntwork-io/terratest/modules/helm"
	"github.com/gruntwork-io/terratest/modules/k8s"
	"github.com/gruntwork-io/terratest/modules/random"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"strings"
	"testing"
)

// TestWALFailover tests the WAL failover functionality by:
// 1. Installing CockroachDB with WAL failover enabled with custom path
// 2. Verifying the cluster is healthy
// 3. Verifying --wal-failover flag with custom path and PVC mounting
func (r *singleRegion) TestWALFailover(t *testing.T) {
	cluster := r.Clusters[0]
	cleanup := r.SetupSingleClusterWithCA(t, cluster)
	defer cleanup()

	// Install CockroachDB with WAL failover enabled using common method
	config := operator.AdvancedInstallConfig{
		WALFailoverEnabled: true,
		WALFailoverSize:    "5Gi",
	}
	r.InstallChartsWithAdvancedConfig(t, cluster, 0, config)

	// Validate CockroachDB cluster is healthy
	r.ValidateCRDB(t, cluster)

	// Validate WAL failover with workload and metrics monitoring
	r.ValidateWALFailover(t, cluster, nil)

	t.Logf("WAL failover test completed successfully")
}

// TestWALFailoverDisable tests disabling WAL failover via helm upgrade by:
// 1. Installing CockroachDB with WAL failover enabled with custom path
// 2. Verifying WAL failover is configured with custom path
// 3. Upgrading to disable WAL failover
// 4. Verifying --wal-failover flag contains disable and prev_path
func (r *singleRegion) TestWALFailoverDisable(t *testing.T) {
	cluster := r.Clusters[0]
	cleanup := r.SetupSingleClusterWithCA(t, cluster)
	defer cleanup()

	// Step 1: Install CockroachDB with WAL failover enabled
	t.Log("Installing CockroachDB with WAL failover enabled...")
	config := operator.AdvancedInstallConfig{
		WALFailoverEnabled: true,
		WALFailoverSize:    "5Gi",
	}
	r.InstallChartsWithAdvancedConfig(t, cluster, 0, config)

	// Validate CockroachDB cluster is healthy
	r.ValidateCRDB(t, cluster)

	// Validate WAL failover is enabled
	r.ValidateWALFailover(t, cluster, nil)

	// Step 2: Upgrade to disable WAL failover
	t.Log("Upgrading to disable WAL failover...")
	kubeConfig, _ := r.GetCurrentContext(t)
	kubectlOptions := k8s.NewKubectlOptions(cluster, kubeConfig, r.Namespace[cluster])

	// Get initial pod timestamps before upgrade
	pods := k8s.ListPods(t, kubectlOptions, metav1.ListOptions{
		LabelSelector: operator.LabelSelector,
	})
	require.True(t, len(pods) > 0, "No CockroachDB pods found")
	initialTimestamp := pods[0].CreationTimestamp.Time

	// Get helm chart paths
	helmChartPath, _ := operator.HelmChartPaths()

	// Upgrade with WAL failover disabled
	disableConfig := operator.PatchHelmValues(map[string]string{
		"cockroachdb.clusterDomain":                      operator.CustomDomains[0],
		"cockroachdb.tls.selfSigner.caProvided":          "true",
		"cockroachdb.tls.selfSigner.caSecret":            "cockroachdb-ca-secret",
		"cockroachdb.crdbCluster.walFailoverSpec.status": "disable",
		"cockroachdb.crdbCluster.walFailoverSpec.name":   "datadir-wal-failover",
		"cockroachdb.crdbCluster.walFailoverSpec.path":   "/cockroach/cockroach-wal-failover",
	})

	upgradeOptions := &helm.Options{
		KubectlOptions: kubectlOptions,
		SetValues:      disableConfig,
		SetJsonValues: map[string]string{
			"cockroachdb.crdbCluster.regions": operator.MustMarshalJSON(r.OperatorRegions(0, r.NodeCount)),
		},
		ExtraArgs: map[string][]string{
			"upgrade": {"--reuse-values", "--wait"},
		},
	}

	helm.Upgrade(t, upgradeOptions, helmChartPath, operator.ReleaseName)

	// Wait for upgrade to complete using VerifyHelmUpgrade helper
	err := r.VerifyHelmUpgrade(t, initialTimestamp, kubectlOptions)
	require.NoError(t, err)

	// Step 3: Verify WAL failover is disabled
	t.Log("Verifying WAL failover is disabled after upgrade...")
	pods = k8s.ListPods(t, kubectlOptions, metav1.ListOptions{
		LabelSelector: operator.LabelSelector,
	})
	require.True(t, len(pods) > 0, "No CockroachDB pods found")

	podName := pods[0].Name

	// Verify --wal-failover flag now contains "disable" and "prev_path"
	podCommand, err := k8s.RunKubectlAndGetOutputE(t, kubectlOptions,
		"get", "pod", podName, "-o", "jsonpath={.spec.containers[?(@.name=='cockroachdb')].command}")
	require.NoError(t, err)
	require.Contains(t, podCommand, "--wal-failover=disable", "Pod command should contain --wal-failover=disable after disabling")
	require.Contains(t, podCommand, "prev_path=/cockroach/cockroach-wal-failover", "Pod command should contain prev_path with custom path after disabling")

	// Verify WAL failover PVC still exists (not deleted on disable)
	pvcs, err := k8s.RunKubectlAndGetOutputE(t, kubectlOptions,
		"get", "pvc", "-o", "jsonpath={.items[*].metadata.name}")
	require.NoError(t, err)
	require.Contains(t, pvcs, "datadir-wal-failover", "WAL failover PVC should still exist after disable")
	t.Logf("PVCs after disable: %s", pvcs)

	t.Log("WAL failover successfully disabled")
	t.Logf("WAL failover disable test completed successfully")
}

// TestEncryptionAtRestEnable tests encryption at rest functionality by:
// 1. Generating a proper 256-bit AES encryption key
// 2. Creating encryption key secret
// 3. Installing CockroachDB with encryption at rest enabled
// 4. Verifying the cluster is healthy and encryption is active
func (r *singleRegion) TestEncryptionAtRestEnable(t *testing.T) {
	cluster := r.Clusters[0]
	cleanup := r.SetupSingleClusterWithCA(t, cluster)
	defer cleanup()

	// Generate proper 256-bit AES encryption key
	t.Log("Generating 256-bit AES encryption key...")
	encryptionKeyB64 := r.GenerateEncryptionKey(t)
	t.Logf("Generated encryption key (base64 length: %d)", len(encryptionKeyB64))

	// Configure encryption at rest regions
	encryptionRegions := r.BuildEncryptionRegions(cluster, 0, nil)

	// Install CockroachDB with encryption at rest enabled using common method
	config := operator.AdvancedInstallConfig{
		EncryptionEnabled:   true,
		EncryptionKeySecret: encryptionKeyB64,
		CustomRegions:       encryptionRegions,
	}
	r.InstallChartsWithAdvancedConfig(t, cluster, 0, config)

	// Validate CockroachDB cluster is healthy
	r.ValidateCRDB(t, cluster)

	// Validate encryption at rest is active
	r.ValidateEncryptionAtRest(t, cluster, nil)

	// Additional validation: Verify key and old-key in the encryption flag
	kubeConfig, _ := r.GetCurrentContext(t)
	kubectlOptions := k8s.NewKubectlOptions(cluster, kubeConfig, r.Namespace[cluster])

	pods := k8s.ListPods(t, kubectlOptions, metav1.ListOptions{
		LabelSelector: operator.LabelSelector,
	})
	require.True(t, len(pods) > 0, "No CockroachDB pods found")
	podName := pods[0].Name

	podCommand, err := k8s.RunKubectlAndGetOutputE(t, kubectlOptions,
		"get", "pod", podName, "-o", "jsonpath={.spec.containers[?(@.name=='cockroachdb')].command}")
	require.NoError(t, err)

	// Verify encryption flag format: key should point to the secret, old-key should be "plain" for initial setup
	require.Contains(t, podCommand, "key=/etc/cockroach-key/", "Encryption flag should contain key path")
	require.Contains(t, podCommand, "old-key=plain", "Encryption flag should have old-key=plain for initial setup")
	t.Logf("Verified encryption flag format with key and old-key=plain")

	t.Logf("Encryption at rest enable test completed successfully")
}

// TestEncryptionAtRestDisable tests transitioning from encrypted to plaintext by:
// 1. Installing CockroachDB with encryption at rest enabled
// 2. Verifying encryption is active
// 3. Upgrading to use plaintext (setting keySecretName to nil and oldKeySecretName to existing secret)
// 4. Verifying --enterprise-encryption flag still exists but now points to "plain" (plaintext)
// Note: Once encryption is enabled, you must always include the --enterprise-encryption flag.
// To disable encryption, keep encryptionAtRest enabled but
// set keySecretName to nil/empty and oldKeySecretName to the existing secret.
func (r *singleRegion) TestEncryptionAtRestDisable(t *testing.T) {
	cluster := r.Clusters[0]
	cleanup := r.SetupSingleClusterWithCA(t, cluster)
	defer cleanup()

	// Step 1: Install CockroachDB with encryption at rest enabled
	t.Log("Installing CockroachDB with encryption at rest enabled...")
	encryptionKeyB64 := r.GenerateEncryptionKey(t)
	t.Logf("Generated encryption key (base64 length: %d)", len(encryptionKeyB64))

	// Configure encryption at rest regions
	encryptionRegions := r.BuildEncryptionRegions(cluster, 0, nil)

	config := operator.AdvancedInstallConfig{
		EncryptionEnabled:   true,
		EncryptionKeySecret: encryptionKeyB64,
		CustomRegions:       encryptionRegions,
	}
	r.InstallChartsWithAdvancedConfig(t, cluster, 0, config)

	// Validate CockroachDB cluster is healthy
	r.ValidateCRDB(t, cluster)

	// Validate encryption at rest is active
	r.ValidateEncryptionAtRest(t, cluster, nil)

	// Step 2: Transition to plaintext by setting keySecretName to nil and oldKeySecretName to existing secret
	t.Log("Upgrading to transition from encrypted to plaintext...")
	kubeConfig, _ := r.GetCurrentContext(t)
	kubectlOptions := k8s.NewKubectlOptions(cluster, kubeConfig, r.Namespace[cluster])

	// Get initial pod timestamps before upgrade
	pods := k8s.ListPods(t, kubectlOptions, metav1.ListOptions{
		LabelSelector: operator.LabelSelector,
	})
	require.True(t, len(pods) > 0, "No CockroachDB pods found")
	initialTimestamp := pods[0].CreationTimestamp.Time

	// Get helm chart paths
	helmChartPath, _ := operator.HelmChartPaths()

	// Configure regions with encryption at rest still enabled but keySecretName set to empty and oldKeySecretName set to existing secret
	// This tells CockroachDB to transition to plaintext mode while keeping the flag
	regions := r.BuildEncryptionRegions(cluster, 0, map[string]interface{}{
		"keySecretName":    nil,
		"oldKeySecretName": "cmek-key-secret",
	})

	upgradeOptions := &helm.Options{
		KubectlOptions: kubectlOptions,
		SetJsonValues: map[string]string{
			"cockroachdb.crdbCluster.regions": operator.MustMarshalJSON(regions),
		},
		ExtraArgs: map[string][]string{
			"upgrade": {"--reuse-values", "--wait"},
		},
	}

	helm.Upgrade(t, upgradeOptions, helmChartPath, operator.ReleaseName)

	// Wait for upgrade to complete using VerifyHelmUpgrade helper
	err := r.VerifyHelmUpgrade(t, initialTimestamp, kubectlOptions)
	require.NoError(t, err)

	// Step 3: Verify encryption flag still exists but now uses "plain"
	t.Log("Verifying transition to plaintext after upgrade...")
	pods = k8s.ListPods(t, kubectlOptions, metav1.ListOptions{
		LabelSelector: operator.LabelSelector,
	})
	require.True(t, len(pods) > 0, "No CockroachDB pods found")

	podName := pods[0].Name

	// According to CockroachDB docs, once encryption is enabled, the --enterprise-encryption
	// flag must always be present. When transitioning to plaintext, it should show "plain"
	podCommand, err := k8s.RunKubectlAndGetOutputE(t, kubectlOptions,
		"get", "pod", podName, "-o", "jsonpath={.spec.containers[?(@.name=='cockroachdb')].command}")
	require.NoError(t, err)
	require.Contains(t, podCommand, "--enterprise-encryption", "Pod command should still contain --enterprise-encryption flag (required once encryption is enabled)")

	// Verify encryption flag format: key should be "plain" (plaintext), old-key should reference the encrypted key
	require.Contains(t, podCommand, "key=plain", "Encryption flag should have key=plain for plaintext mode")
	require.Contains(t, podCommand, "old-key=/etc/cockroach-key/", "Encryption flag should have old-key pointing to previous encrypted key")
	t.Logf("Verified encryption flag format with key=plain and old-key pointing to encrypted key")

	// Verify encryption status shows plaintext
	//encryptionStatus, err := k8s.RunKubectlAndGetOutputE(t, kubectlOptions,
	//	"exec", podName, "-c", "cockroachdb", "--",
	//	"/cockroach/cockroach", "debug", "encryption-status", "/cockroach/cockroach-data")
	//require.NoError(t, err)
	// In plaintext mode, encryption status should not show "Active encryption" or should show plaintext
	//t.Logf("Encryption status after transitioning to plaintext: %s", encryptionStatus)

	t.Log("Successfully transitioned from encrypted to plaintext")
	t.Logf("Encryption at rest disable test completed successfully")
}

// TestEncryptionAtRestModifySecret tests rotating the encryption key by:
// 1. Installing CockroachDB with encryption at rest enabled
// 2. Verifying encryption is active
// 3. Generating a new encryption key and creating a new secret
// 4. Upgrading with keySecretName pointing to new secret and oldKeySecretName to existing secret
// 5. Verifying encryption still works with rotated key
// Note: Key rotation requires setting keySecretName to the new key and oldKeySecretName to the old key.
func (r *singleRegion) TestEncryptionAtRestModifySecret(t *testing.T) {
	cluster := r.Clusters[0]
	cleanup := r.SetupSingleClusterWithCA(t, cluster)
	defer cleanup()

	// Step 1: Install CockroachDB with encryption at rest enabled
	t.Log("Installing CockroachDB with encryption at rest enabled...")
	encryptionKeyB64 := r.GenerateEncryptionKey(t)
	t.Logf("Generated initial encryption key (base64 length: %d)", len(encryptionKeyB64))

	// Configure encryption at rest regions
	encryptionRegions := r.BuildEncryptionRegions(cluster, 0, nil)

	config := operator.AdvancedInstallConfig{
		EncryptionEnabled:   true,
		EncryptionKeySecret: encryptionKeyB64,
		CustomRegions:       encryptionRegions,
	}
	r.InstallChartsWithAdvancedConfig(t, cluster, 0, config)

	// Validate CockroachDB cluster is healthy
	r.ValidateCRDB(t, cluster)

	// Validate encryption at rest is active
	r.ValidateEncryptionAtRest(t, cluster, nil)

	// Step 2: Generate new encryption key and create new secret
	t.Log("Generating new encryption key and creating new secret...")
	kubeConfig, _ := r.GetCurrentContext(t)
	kubectlOptions := k8s.NewKubectlOptions(cluster, kubeConfig, r.Namespace[cluster])

	newEncryptionKeyB64 := r.GenerateEncryptionKey(t)
	t.Logf("Generated new encryption key (base64 length: %d)", len(newEncryptionKeyB64))

	// Create new secret with a different name
	k8s.RunKubectl(t, kubectlOptions, "create", "secret", "generic", "cmek-key-secret-new",
		"--from-literal=StoreKeyData="+newEncryptionKeyB64)

	t.Log("Created new encryption key secret: cmek-key-secret-new")

	// Get initial pod timestamps before upgrade
	pods := k8s.ListPods(t, kubectlOptions, metav1.ListOptions{
		LabelSelector: operator.LabelSelector,
	})
	require.True(t, len(pods) > 0, "No CockroachDB pods found")
	initialTimestamp := pods[0].CreationTimestamp.Time

	// Step 3: Configure regions with key rotation - new key in keySecretName, old key in oldKeySecretName
	t.Log("Upgrading with key rotation configuration...")
	helmChartPath, _ := operator.HelmChartPaths()

	// Configure regions with both new and old keys for rotation
	rotationRegions := r.BuildEncryptionRegions(cluster, 0, map[string]interface{}{
		"keySecretName":    "cmek-key-secret-new",
		"oldKeySecretName": "cmek-key-secret",
	})

	upgradeOptions := &helm.Options{
		KubectlOptions: kubectlOptions,
		SetJsonValues: map[string]string{
			"cockroachdb.crdbCluster.regions": operator.MustMarshalJSON(rotationRegions),
		},
		ExtraArgs: map[string][]string{
			"upgrade": {"--reuse-values", "--wait"},
		},
	}

	helm.Upgrade(t, upgradeOptions, helmChartPath, operator.ReleaseName)

	// Wait for pods to restart and key rotation to complete using VerifyHelmUpgrade helper
	err := r.VerifyHelmUpgrade(t, initialTimestamp, kubectlOptions)
	require.NoError(t, err)

	// Step 4: Verify encryption is still active with new key
	t.Log("Verifying encryption at rest is still active with new key...")

	// Validate CockroachDB cluster is healthy
	r.ValidateCRDB(t, cluster)

	// Validate encryption at rest is still active
	r.ValidateEncryptionAtRest(t, cluster, nil)

	// Verify new secret exists and is being used
	newSecretData, err := k8s.RunKubectlAndGetOutputE(t, kubectlOptions,
		"get", "secret", "cmek-key-secret-new", "-o", "jsonpath={.data.StoreKeyData}")
	require.NoError(t, err)
	t.Logf("New secret key length: %d", len(newSecretData))

	// Verify old secret still exists (referenced in oldKeySecretName)
	oldSecretData, err := k8s.RunKubectlAndGetOutputE(t, kubectlOptions,
		"get", "secret", "cmek-key-secret", "-o", "jsonpath={.data.StoreKeyData}")
	require.NoError(t, err)
	t.Logf("Old secret key length: %d", len(oldSecretData))

	// Verify pod command contains encryption flag with both key references
	pods = k8s.ListPods(t, kubectlOptions, metav1.ListOptions{
		LabelSelector: operator.LabelSelector,
	})
	require.True(t, len(pods) > 0, "No CockroachDB pods found")
	podName := pods[0].Name

	podCommand, err := k8s.RunKubectlAndGetOutputE(t, kubectlOptions,
		"get", "pod", podName, "-o", "jsonpath={.spec.containers[?(@.name=='cockroachdb')].command}")
	require.NoError(t, err)
	require.Contains(t, podCommand, "--enterprise-encryption", "Pod command should contain --enterprise-encryption flag")

	// Verify encryption flag format: key should point to new secret, old-key should point to old secret
	require.Contains(t, podCommand, "key=/etc/cockroach-key/", "Encryption flag should contain new key path")
	require.Contains(t, podCommand, "old-key=/etc/cockroach-key/", "Encryption flag should contain old key path for rotation")
	t.Logf("Verified encryption flag format with both new key and old-key during rotation")
	t.Logf("Encryption flag after key rotation: %s", podCommand)

	t.Log("Encryption at rest successfully working with rotated key")
	t.Logf("Encryption at rest modify secret test completed successfully")
}

// TestWALFailoverWithEncryption tests WAL failover with encryption at rest by:
// 1. Installing CockroachDB with both WAL failover and encryption at rest enabled
// 2. Verifying the cluster is healthy
// 3. Verifying --wal-failover flag with custom path
// 4. Verifying --enterprise-encryption flag includes WAL path encryption
// Note: When WAL failover is enabled with encryption at rest, the WAL path must also be encrypted.
func (r *singleRegion) TestWALFailoverWithEncryption(t *testing.T) {
	cluster := r.Clusters[0]
	cleanup := r.SetupSingleClusterWithCA(t, cluster)
	defer cleanup()

	// Step 1: Generate encryption key
	t.Log("Generating 256-bit AES encryption key...")
	encryptionKeyB64 := r.GenerateEncryptionKey(t)
	t.Logf("Generated encryption key (base64 length: %d)", len(encryptionKeyB64))

	// Configure encryption at rest regions
	encryptionRegions := r.BuildEncryptionRegions(cluster, 0, nil)

	// Install CockroachDB with both WAL failover and encryption enabled
	config := operator.AdvancedInstallConfig{
		WALFailoverEnabled:  true,
		WALFailoverSize:     "5Gi",
		EncryptionEnabled:   true,
		EncryptionKeySecret: encryptionKeyB64,
		CustomRegions:       encryptionRegions,
	}
	r.InstallChartsWithAdvancedConfig(t, cluster, 0, config)

	// Validate CockroachDB cluster is healthy
	r.ValidateCRDB(t, cluster)

	// Step 2: Verify WAL failover is configured
	t.Log("Verifying WAL failover configuration...")
	kubeConfig, _ := r.GetCurrentContext(t)
	kubectlOptions := k8s.NewKubectlOptions(cluster, kubeConfig, r.Namespace[cluster])

	pods := k8s.ListPods(t, kubectlOptions, metav1.ListOptions{
		LabelSelector: operator.LabelSelector,
	})
	require.True(t, len(pods) > 0, "No CockroachDB pods found")
	podName := pods[0].Name

	// Verify --wal-failover flag exists
	podCommand, err := k8s.RunKubectlAndGetOutputE(t, kubectlOptions,
		"get", "pod", podName, "-o", "jsonpath={.spec.containers[?(@.name=='cockroachdb')].command}")
	require.NoError(t, err)
	require.Contains(t, podCommand, "--wal-failover=path=/cockroach/cockroach-wal-failover",
		"Pod command should contain --wal-failover flag with custom path")

	// Step 3: Verify encryption is configured for both data store and WAL path
	t.Log("Verifying encryption is configured for data store and WAL path...")
	// Encryption flag is part of the command
	require.Contains(t, podCommand, "--enterprise-encryption",
		"Pod command should contain --enterprise-encryption flag")

	// The encryption flag should reference the WAL failover path
	// Format: --enterprise-encryption=path=cockroach-data,key=...,old-key=plain;path=/cockroach/cockroach-wal-failover,key=...,old-key=plain
	require.Contains(t, podCommand, "cockroach-data",
		"Encryption flag should include data store path")
	require.Contains(t, podCommand, "/cockroach/cockroach-wal-failover",
		"Encryption flag should include WAL failover path for encryption")

	// Verify encryption flag format: both data and WAL paths should have key=/etc/cockroach-key/ and old-key=plain
	require.Contains(t, podCommand, "key=/etc/cockroach-key/", "Encryption flag should contain key path for both data and WAL")
	require.Contains(t, podCommand, "old-key=plain", "Encryption flag should have old-key=plain for initial setup")
	t.Logf("Verified encryption flag format with key and old-key=plain for both data store and WAL path")

	// Commented out cockroach debug verification commands
	//t.Log("Verifying encryption status for both paths...")
	//// Verify data store encryption
	//dataEncryptionStatus, err := k8s.RunKubectlAndGetOutputE(t, kubectlOptions,
	//	"exec", podName, "-c", "cockroachdb", "--",
	//	"/cockroach/cockroach", "debug", "encryption-status", "/cockroach/cockroach-data")
	//require.NoError(t, err)
	//require.Contains(t, dataEncryptionStatus, "Active", "Data store should show active encryption")
	//
	//// Verify WAL path encryption
	//walEncryptionStatus, err := k8s.RunKubectlAndGetOutputE(t, kubectlOptions,
	//	"exec", podName, "-c", "cockroachdb", "--",
	//	"/cockroach/cockroach", "debug", "encryption-status", "/cockroach/cockroach-wal-failover")
	//require.NoError(t, err)
	//require.Contains(t, walEncryptionStatus, "Active", "WAL failover path should show active encryption")

	// Verify both PVCs exist
	pvcs, err := k8s.RunKubectlAndGetOutputE(t, kubectlOptions,
		"get", "pvc", "-o", "jsonpath={.items[*].metadata.name}")
	require.NoError(t, err)
	require.Contains(t, pvcs, "datadir", "Data PVC should exist")
	require.Contains(t, pvcs, "datadir-wal-failover", "WAL failover PVC should exist")

	t.Log("WAL failover with encryption at rest test completed successfully")
	t.Logf("Verified both data store and WAL path are encrypted")
}

// TestPCR tests Physical Cluster Replication by:
// 1. Installing a primary virtual cluster
// 2. Installing a standby virtual cluster in separate namespace
// 3. Creating replication stream between primary and standby
// 4. Running workload on primary
// 5. Verifying cutover to standby
func (r *singleRegion) TestPCR(t *testing.T) {
	cluster := r.Clusters[0]

	// Create CA certificate once for both clusters
	cleanupCA := r.RequireCACertificate(t)
	defer cleanupCA()

	var (
		primaryNamespace string
		standbyNamespace string
	)

	// Step 1: Install primary virtual cluster
	t.Log("Installing primary virtual cluster...")
	primaryNamespace = fmt.Sprintf("%s-primary-%s", operator.Namespace, strings.ToLower(random.UniqueId()))
	r.Namespace[cluster] = primaryNamespace

	primaryConfig := operator.AdvancedInstallConfig{
		VirtualClusterMode: "primary",
	}
	r.InstallChartsWithAdvancedConfig(t, cluster, 0, primaryConfig)

	r.VirtualClusterModePrimary = true
	r.ValidateCRDB(t, cluster)
	r.VirtualClusterModePrimary = false

	// Step 2: Install standby virtual cluster in separate namespace
	t.Log("Installing standby virtual cluster...")
	standbyNamespace = fmt.Sprintf("%s-standby-%s", operator.Namespace, strings.ToLower(random.UniqueId()))
	r.Namespace[cluster] = standbyNamespace

	standbyConfig := operator.AdvancedInstallConfig{
		VirtualClusterMode:  "standby",
		SkipOperatorInstall: true,
	}
	r.InstallChartsWithAdvancedConfig(t, cluster, 0, standbyConfig)

	r.VirtualClusterModeStandby = true
	r.ValidateCRDB(t, cluster)
	r.VirtualClusterModeStandby = false

	// Step 3: Set up replication and test failover/failback
	r.ValidatePCR(t, &operator.AdvancedValidationConfig{
		PCR: operator.PCRValidation{
			Cluster:          cluster,
			PrimaryNamespace: primaryNamespace,
			StandbyNamespace: standbyNamespace,
		},
	})

	// Cleanup both namespaces
	defer func() {
		r.Namespace[cluster] = primaryNamespace
		r.CleanupResources(t)
	}()
	defer func() {
		kubectlOptions := k8s.NewKubectlOptions("", "", standbyNamespace)
		k8s.DeleteNamespace(t, kubectlOptions, standbyNamespace)
	}()

	t.Logf("PCR (Physical Cluster Replication) test completed successfully")
}
