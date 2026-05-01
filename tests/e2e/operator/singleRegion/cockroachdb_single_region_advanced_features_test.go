package singleRegion

import (
	"fmt"
	"strings"
	"testing"

	"github.com/cockroachdb/helm-charts/tests/e2e/operator"
	"github.com/gruntwork-io/terratest/modules/helm"
	"github.com/gruntwork-io/terratest/modules/k8s"
	"github.com/gruntwork-io/terratest/modules/random"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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

	t.Log("WAL failover test completed successfully")
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
	t.Log("Installing CockroachDB with WAL failover enabled")
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
	t.Log("Upgrading to disable WAL failover")
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
	t.Log("Verifying WAL failover is disabled after upgrade")
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

	t.Log("WAL failover disable test completed successfully")
}

// TestEncryptionAtRestEnable tests encryption at rest functionality by:
// 1. Installing CockroachDB with encryption at rest enabled (key generation handled by provider)
// 2. Verifying the cluster is healthy and encryption is active
func (r *singleRegion) TestEncryptionAtRestEnable(t *testing.T) {
	cluster := r.Clusters[0]
	cleanup := r.SetupSingleClusterWithCA(t, cluster)
	defer cleanup()

	encryptionRegions := r.BuildEncryptionRegions(cluster, 0, r.EncryptionOverridesFromProvider())
	r.InstallChartsWithAdvancedConfig(t, cluster, 0, operator.AdvancedInstallConfig{
		EncryptionEnabled: true,
		CustomRegions:     encryptionRegions,
	})

	// Validate CockroachDB cluster is healthy
	r.ValidateCRDB(t, cluster)

	r.ValidateEncryptionAtRest(t, cluster, nil)

	t.Log("Encryption at rest enable test completed successfully")
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
	t.Log("Installing CockroachDB with encryption at rest enabled")
	r.InstallChartsWithAdvancedConfig(t, cluster, 0, operator.AdvancedInstallConfig{
		EncryptionEnabled: true,
		CustomRegions:     r.BuildEncryptionRegions(cluster, 0, r.EncryptionOverridesFromProvider()),
	})

	// Validate CockroachDB cluster is healthy
	r.ValidateCRDB(t, cluster)

	// Validate encryption at rest is active
	r.ValidateEncryptionAtRest(t, cluster, nil)

	// Step 2: Trigger plaintext transition via helm upgrade.
	// Setting keySecretName absent (nil in operator = "plain") and oldKeySecretName to the
	// current key secret tells the operator to transition to plaintext mode.
	t.Log("Upgrading CrdbCluster to configure plaintext transition")
	kubeConfig, _ := r.GetCurrentContext(t)
	kubectlOptions := k8s.NewKubectlOptions(cluster, kubeConfig, r.Namespace[cluster])
	helmChartPath, _ := operator.HelmChartPaths()

	// Get initial pod timestamps before upgrade
	pods := k8s.ListPods(t, kubectlOptions, metav1.ListOptions{
		LabelSelector: operator.LabelSelector,
	})
	require.True(t, len(pods) > 0, "No CockroachDB pods found")
	initialTimestamp := pods[0].CreationTimestamp.Time

	// Absent keySecretName = operator uses "plain". oldKeySecretName references the
	// previous encrypted key so the transition can decrypt it.
	disableOverrides := r.EncryptionOverridesFromProvider()
	disableOverrides["keySecretName"] = ""
	disableOverrides["oldKeySecretName"] = operator.DefaultEncryptionSecret
	// oldCmekCredentialsSecretName is required for the operator to set OLD_CMEK_PLATFORM.
	if credSecretName, ok := disableOverrides["cmekCredentialsSecretName"]; ok {
		disableOverrides["oldCmekCredentialsSecretName"] = credSecretName
	}
	regions := r.BuildEncryptionRegions(cluster, 0, disableOverrides)

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

	err := r.VerifyHelmUpgrade(t, initialTimestamp, kubectlOptions)
	require.NoError(t, err)

	// Step 3: Verify the CrdbCluster CR spec was correctly updated.
	// The authoritative signal for "disable EAR" is the CR spec:
	//   - keySecretName must be absent (operator interprets nil as "plain")
	//   - oldKeySecretName must reference the previous key secret
	// This validation works regardless of the pod-level rolling restart state.
	t.Log("Verifying CrdbCluster CR spec reflects plaintext transition")
	crEncryption, err := k8s.RunKubectlAndGetOutputE(t, kubectlOptions,
		"get", "crdbcluster", "cockroachdb",
		"-o", "jsonpath={.spec.regions[*].encryptionAtRest}")
	require.NoError(t, err)
	require.Contains(t, crEncryption, operator.DefaultEncryptionSecret,
		"CrdbCluster CR should reference the old key secret for the plaintext transition")
	require.NotContains(t, crEncryption, `"keySecretName"`,
		"CrdbCluster CR should not have keySecretName set (absent = operator uses plain)")
	t.Logf("CrdbCluster CR spec correctly updated for plaintext transition: %s", crEncryption)

	// Step 4: Verify pod command reflects plaintext transition.
	t.Log("Verifying transition to plaintext after upgrade")
	pods = k8s.ListPods(t, kubectlOptions, metav1.ListOptions{LabelSelector: operator.LabelSelector})
	require.True(t, len(pods) > 0, "No CockroachDB pods found")
	podName := pods[0].Name

	podCommand, err := k8s.RunKubectlAndGetOutputE(t, kubectlOptions,
		"get", "pod", podName, "-o", "jsonpath={.spec.containers[?(@.name=='cockroachdb')].command}")
	require.NoError(t, err)
	require.Contains(t, podCommand, "--enterprise-encryption",
		"Pod command should still contain --enterprise-encryption flag (required once encryption is enabled)")
	require.Contains(t, podCommand, "key=plain",
		"Encryption flag should have key=plain for plaintext mode")
	require.Contains(t, podCommand, "old-key=/etc/cockroach-key/",
		"Encryption flag should have old-key pointing to the previous encrypted key")
	t.Logf("Verified encryption flag format with key=plain and old-key pointing to encrypted key")

	t.Log("Encryption at rest disable test completed successfully")
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
	t.Log("Installing CockroachDB with encryption at rest enabled")
	r.InstallChartsWithAdvancedConfig(t, cluster, 0, operator.AdvancedInstallConfig{
		EncryptionEnabled: true,
		CustomRegions:     r.BuildEncryptionRegions(cluster, 0, r.EncryptionOverridesFromProvider()),
	})

	// Validate CockroachDB cluster is healthy
	r.ValidateCRDB(t, cluster)

	// Validate encryption at rest is active
	r.ValidateEncryptionAtRest(t, cluster, nil)

	// Step 2: Create new encryption key secret for rotation
	t.Log("Creating new encryption key secret for rotation...")
	kubeConfig, _ := r.GetCurrentContext(t)
	kubectlOptions := k8s.NewKubectlOptions(cluster, kubeConfig, r.Namespace[cluster])

	err := r.CreateEncryptionKeySecret(t, kubectlOptions, operator.DefaultRotatedEncryptionSecret, r.RegionCodes[0])
	require.NoError(t, err, "failed to create new encryption key secret")

	t.Logf("Created new encryption key secret: %s", operator.DefaultRotatedEncryptionSecret)

	// Get initial pod timestamps before upgrade
	pods := k8s.ListPods(t, kubectlOptions, metav1.ListOptions{
		LabelSelector: operator.LabelSelector,
	})
	require.True(t, len(pods) > 0, "No CockroachDB pods found")
	initialTimestamp := pods[0].CreationTimestamp.Time

	// Step 3: Configure regions with key rotation - new key in keySecretName, old key in oldKeySecretName
	t.Log("Upgrading with key rotation configuration...")
	helmChartPath, _ := operator.HelmChartPaths()

	rotationOverrides := r.EncryptionOverridesFromProvider()
	rotationOverrides["keySecretName"] = operator.DefaultRotatedEncryptionSecret
	rotationOverrides["oldKeySecretName"] = operator.DefaultEncryptionSecret
	if credSecretName, ok := rotationOverrides["cmekCredentialsSecretName"]; ok {
		rotationOverrides["oldCmekCredentialsSecretName"] = credSecretName
	}
	rotationRegions := r.BuildEncryptionRegions(cluster, 0, rotationOverrides)

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
	err = r.VerifyHelmUpgrade(t, initialTimestamp, kubectlOptions)
	require.NoError(t, err)

	// Step 4: Verify encryption is still active with new key
	t.Log("Verifying encryption at rest is still active with new key")

	// Validate CockroachDB cluster is healthy
	r.ValidateCRDB(t, cluster)

	// Validate encryption at rest is still active. After key rotation old-key points to
	// the previous key path rather than "plain".
	r.ValidateEncryptionAtRest(t, cluster, &operator.AdvancedValidationConfig{
		EncryptionAtRest: operator.EncryptionAtRestValidation{
			SecretName:     operator.DefaultRotatedEncryptionSecret,
			OldKeyExpected: "old-key=/etc/cockroach-key/",
		},
	})

	// Verify both secrets exist after rotation
	_, err = k8s.RunKubectlAndGetOutputE(t, kubectlOptions,
		"get", "secret", operator.DefaultRotatedEncryptionSecret, "-o", "jsonpath={.data.StoreKeyData}")
	require.NoError(t, err, "new encryption key secret should exist")

	_, err = k8s.RunKubectlAndGetOutputE(t, kubectlOptions,
		"get", "secret", operator.DefaultEncryptionSecret, "-o", "jsonpath={.data.StoreKeyData}")
	require.NoError(t, err, "old encryption key secret should still exist")

	t.Log("Encryption at rest modify secret test completed successfully")
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

	r.InstallChartsWithAdvancedConfig(t, cluster, 0, operator.AdvancedInstallConfig{
		WALFailoverEnabled: true,
		WALFailoverSize:    "5Gi",
		EncryptionEnabled:  true,
		CustomRegions:      r.BuildEncryptionRegions(cluster, 0, r.EncryptionOverridesFromProvider()),
	})

	// Validate CockroachDB cluster is healthy
	r.ValidateCRDB(t, cluster)

	r.ValidateWALFailover(t, cluster, nil)
	r.ValidateEncryptionAtRest(t, cluster, nil)

	// Unique to WAL+encryption: verify the WAL path is also covered by the encryption flag
	kubeConfig, _ := r.GetCurrentContext(t)
	kubectlOptions := k8s.NewKubectlOptions(cluster, kubeConfig, r.Namespace[cluster])
	pods := k8s.ListPods(t, kubectlOptions, metav1.ListOptions{
		LabelSelector: operator.LabelSelector,
	})
	require.True(t, len(pods) > 0, "No CockroachDB pods found")
	podCommand, err := k8s.RunKubectlAndGetOutputE(t, kubectlOptions,
		"get", "pod", pods[0].Name, "-o", "jsonpath={.spec.containers[?(@.name=='cockroachdb')].command}")
	require.NoError(t, err)
	require.Contains(t, podCommand, "/cockroach/cockroach-wal-failover",
		"Encryption flag should include WAL failover path")

	t.Log("WAL failover with encryption at rest test completed successfully")
}

// TestPCR tests Physical Cluster Replication by:
// 1. Installing a primary virtual cluster
// 2. Installing a standby virtual cluster in separate namespace
// 3. Creating replication stream between primary and standby
// 4. Verifying replication is active on standby
// 5. Testing read-only access via a reader virtual cluster
// 6. Verifying cutover to standby
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
	t.Log("Installing primary virtual cluster")
	primaryNamespace = fmt.Sprintf("%s-primary-%s", operator.Namespace, strings.ToLower(random.UniqueId()))
	r.Namespace[cluster] = primaryNamespace

	primaryConfig := operator.AdvancedInstallConfig{
		VirtualClusterMode: "primary",
	}
	r.InstallChartsWithAdvancedConfig(t, cluster, 0, primaryConfig)

	r.ValidateCRDB(t, cluster)

	// Step 2: Install standby virtual cluster in separate namespace
	t.Log("Installing standby virtual cluster")
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

	// Register cleanup for both namespaces before ValidatePCR so namespaces are
	// always removed even if ValidatePCR fails early via require.* / t.FailNow.
	kubeConfig, _ := r.GetCurrentContext(t)
	defer func() {
		r.Namespace[cluster] = primaryNamespace
		r.CleanupResources(t)
	}()
	defer func() {
		kubectlOptions := k8s.NewKubectlOptions(cluster, kubeConfig, standbyNamespace)
		if err := k8s.DeleteNamespaceE(t, kubectlOptions, standbyNamespace); err != nil {
			t.Logf("Warning: failed to delete standby namespace %s (cluster may be unreachable): %v", standbyNamespace, err)
		}
	}()

	// Step 3: Set up replication and test failover/failback.
	r.ValidatePCR(t, &operator.AdvancedValidationConfig{
		PCR: operator.PCRValidation{
			Cluster:          cluster,
			PrimaryNamespace: primaryNamespace,
			StandbyNamespace: standbyNamespace,
		},
	})

	t.Logf("PCR (Physical Cluster Replication) test completed successfully")
}
