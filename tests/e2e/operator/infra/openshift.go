package infra

import (
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"text/template"
	"time"

	"github.com/cockroachdb/helm-charts/tests/e2e/coredns"
	"github.com/cockroachdb/helm-charts/tests/e2e/operator"
	"github.com/cockroachdb/helm-charts/tests/e2e/operator/encryption"
	"github.com/gruntwork-io/terratest/modules/k8s"
	"github.com/gruntwork-io/terratest/modules/retry"
	"github.com/stretchr/testify/require"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"
)

const (
	envOpenShiftPullSecret       = "OPENSHIFT_PULL_SECRET"
	envOpenShiftSSHPubKey        = "OPENSHIFT_SSH_PUB_KEY"
	envOpenShiftBaseDomain       = "OPENSHIFT_BASE_DOMAIN"
	envOpenShiftInstallLogLevel  = "OPENSHIFT_INSTALL_LOG_LEVEL"
	envOpenShiftReuseKubeconfig  = "OPENSHIFT_REUSE_KUBECONFIG"
	envOpenShiftReuseKubeconfigs = "OPENSHIFT_REUSE_KUBECONFIGS"
	envOpenShiftReuseContexts    = "OPENSHIFT_REUSE_CONTEXTS"
	envOpenShiftEnableSubmariner = "OPENSHIFT_ENABLE_SUBMARINER"
	envOpenShiftSkipTeardown     = "OPENSHIFT_SKIP_TEARDOWN"
	envOpenShiftStorageClass     = "OPENSHIFT_STORAGE_CLASS"
)

const (
	defaultOpenShiftInstallLogLevel = "info"
	defaultOpenShiftStorageClass    = "standard-csi"
	openShiftCockroachNofileLimit   = 1048576
	openShiftScaleUpExtraWorkers    = 1
)

const (
	openShiftCRIOMachineConfigName = "99-worker-cockroach-ulimits"
	openShiftCRIOUlimitsPath       = "/etc/crio/crio.conf.d/99-cockroach-ulimits.conf"
	openShiftSubmarinerNamespace   = "submariner-operator"
	openShiftSubmarinerBrokerFile  = "broker-info.subm"
	openShiftSubmarinerGatewayTag  = "submariner-io-gateway-node"
)

const (
	openShiftSubmarinerPublicFirewallRule   = "submariner-public-ports"
	openShiftSubmarinerInternalFirewallRule = "submariner-internal-ports"
	openShiftSubmarinerPublicPorts          = "udp:4500,udp:4490,esp,ah"
	openShiftSubmarinerInternalPorts        = "udp:4800"
)

type openShiftNetworkConfig struct {
	MachineCIDR    string
	ClusterNetwork string
	HostPrefix     int
	ServiceNetwork string
}

var openShiftNetworkConfigs = []openShiftNetworkConfig{
	{
		MachineCIDR:    "10.0.0.0/16",
		ClusterNetwork: "10.128.0.0/14",
		HostPrefix:     23,
		ServiceNetwork: "172.30.0.0/16",
	},
	{
		MachineCIDR:    "10.1.0.0/16",
		ClusterNetwork: "10.132.0.0/14",
		HostPrefix:     23,
		ServiceNetwork: "172.31.0.0/16",
	},
}

var openShiftInstallConfigTemplate = template.Must(template.New("install-config").Parse(`apiVersion: v1
baseDomain: {{ .BaseDomain }}
metadata:
  name: {{ .ClusterName }}
compute:
- architecture: amd64
  hyperthreading: Enabled
  name: worker
  platform: {}
  replicas: {{ .WorkerReplicas }}
controlPlane:
  architecture: amd64
  hyperthreading: Enabled
  name: master
  platform: {}
  replicas: 3
networking:
  networkType: OVNKubernetes
  machineNetwork:
  - cidr: {{ .MachineCIDR }}
  clusterNetwork:
  - cidr: {{ .ClusterNetwork }}
    hostPrefix: {{ .HostPrefix }}
  serviceNetwork:
  - {{ .ServiceNetwork }}
platform:
  gcp:
    projectID: {{ .ProjectID }}
    region: {{ .Region }}
pullSecret: '{{ .PullSecret }}'
sshKey: '{{ .SSHPubKey }}'
`))

type openShiftInstallConfigData struct {
	BaseDomain     string
	ClusterName    string
	ProjectID      string
	Region         string
	PullSecret     string
	SSHPubKey      string
	WorkerReplicas int
	MachineCIDR    string
	ClusterNetwork string
	HostPrefix     int
	ServiceNetwork string
}

type openShiftMetadata struct {
	InfraID string `json:"infraID"`
}

type openShiftSubmarinerGatewayNode struct {
	nodeName     string
	instanceName string
	zone         string
}

// OpenShiftRegion provisions OpenShift clusters on GCP using openshift-install.
type OpenShiftRegion struct {
	*operator.Region

	installDirs     map[string]string
	createdClusters map[string]bool
	infraIDs        map[string]string
}

func (r *OpenShiftRegion) SetUpInfra(t *testing.T) {
	if r.ReusingInfra {
		t.Logf("[%s] Reusing existing infrastructure", ProviderOpenShift)
		return
	}
	if len(r.Clusters) == 0 {
		t.Fatalf("[%s] no clusters configured", ProviderOpenShift)
	}
	if _, err := exec.LookPath("kubectl"); err != nil {
		t.Fatalf("[%s] kubectl not found in PATH: %v", ProviderOpenShift, err)
	}

	if len(r.RegionCodes) == 0 {
		r.RegionCodes = GetRegionCodes(ProviderOpenShift)
	}

	r.installDirs = make(map[string]string)
	r.createdClusters = make(map[string]bool)
	r.infraIDs = make(map[string]string)
	r.Clients = make(map[string]client.Client)

	if len(r.RegionCodes) < len(r.Clusters) {
		t.Fatalf("[%s] need %d region codes, got %d", ProviderOpenShift, len(r.Clusters), len(r.RegionCodes))
	}

	if reuseContexts := openShiftReuseContexts(t, len(r.Clusters)); len(reuseContexts) > 0 {
		r.Clusters = reuseContexts
		kubeConfigPath, err := r.EnsureKubeConfigPath()
		require.NoError(t, err)
		require.NoError(t, os.Setenv("KUBECONFIG", kubeConfigPath))
		for _, clusterName := range r.Clusters {
			r.validateOpenShiftCluster(t, kubeConfigPath, clusterName)
		}
		r.initializeOpenShiftClients(t)
		r.configureOpenShiftMultiCluster(t, kubeConfigPath)
		r.ReusingInfra = true
		t.Logf("[%s] Infrastructure setup complete using reused contexts", ProviderOpenShift)
		return
	}

	if reuseKubeconfigs := openShiftReuseKubeconfigs(t, len(r.Clusters)); len(reuseKubeconfigs) > 0 {
		for i, clusterName := range r.Clusters {
			r.reuseCluster(t, clusterName, reuseKubeconfigs[i])
		}
		r.initializeOpenShiftClients(t)
		kubeConfigPath, err := r.EnsureKubeConfigPath()
		require.NoError(t, err)
		r.configureOpenShiftMultiCluster(t, kubeConfigPath)
		r.ReusingInfra = true
		t.Logf("[%s] Infrastructure setup complete using reused kubeconfig contexts", ProviderOpenShift)
		return
	}

	if _, err := exec.LookPath("openshift-install"); err != nil {
		t.Fatalf("[%s] openshift-install not found in PATH: %v", ProviderOpenShift, err)
	}

	projectID := mustOpenShiftEnv(t, "GCP_PROJECT_ID")
	baseDomain := mustOpenShiftEnv(t, envOpenShiftBaseDomain)
	pullSecret := readOpenShiftPullSecret(t)
	sshPubKey := readOpenShiftSSHPubKey(t)

	kubeConfigPath, err := r.EnsureKubeConfigPath()
	require.NoError(t, err)

	for i, clusterName := range r.Clusters {
		regionCode := r.RegionCodes[i]
		workerReplicas := openShiftWorkerReplicasForNodeCount(r.NodeCount)
		netCfg := openShiftNetworkConfigForCluster(t, i)
		t.Logf("[%s] Provisioning cluster %q in region %s (project %s, workers %d, machine CIDR %s, pod CIDR %s, service CIDR %s)",
			ProviderOpenShift, clusterName, regionCode, projectID, workerReplicas,
			netCfg.MachineCIDR, netCfg.ClusterNetwork, netCfg.ServiceNetwork)

		installDir, infraID, err := r.provisionCluster(
			t,
			clusterName,
			regionCode,
			projectID,
			baseDomain,
			pullSecret,
			sshPubKey,
			workerReplicas,
			netCfg,
		)
		require.NoError(t, err, "[%s] failed to provision cluster %q", ProviderOpenShift, clusterName)

		r.installDirs[clusterName] = installDir
		r.createdClusters[clusterName] = true
		r.infraIDs[clusterName] = infraID
		t.Logf("[%s] Cluster %q created with infraID %q", ProviderOpenShift, clusterName, infraID)

		generatedKubeconfig := filepath.Join(installDir, "auth", "kubeconfig")
		require.NoError(t, mergeOpenShiftKubeconfig(generatedKubeconfig, kubeConfigPath, clusterName))
		r.validateOpenShiftCluster(t, kubeConfigPath, clusterName)
	}

	r.initializeOpenShiftClients(t)
	r.configureOpenShiftMultiCluster(t, kubeConfigPath)
	r.ReusingInfra = true
	t.Logf("[%s] Infrastructure setup complete", ProviderOpenShift)
}

func (r *OpenShiftRegion) reuseCluster(t *testing.T, clusterName, sourceKubeconfig string) {
	t.Helper()

	kubeConfigPath, err := r.EnsureKubeConfigPath()
	require.NoError(t, err)
	require.NoError(t, mergeOpenShiftKubeconfig(sourceKubeconfig, kubeConfigPath, clusterName),
		"[%s] failed to import reused kubeconfig %s", ProviderOpenShift, sourceKubeconfig)
	require.NoError(t, os.Setenv("KUBECONFIG", kubeConfigPath))

	r.validateOpenShiftCluster(t, kubeConfigPath, clusterName)
	t.Logf("[%s] Reusing cluster from %s as test context %q", ProviderOpenShift, sourceKubeconfig, clusterName)
}

func (r *OpenShiftRegion) TeardownInfra(t *testing.T) {
	if r.ReusingInfra && len(r.createdClusters) == 0 {
		t.Logf("[%s] Reused infrastructure; skipping cluster teardown", ProviderOpenShift)
		return
	}
	if parseBoolEnv(envOpenShiftSkipTeardown) {
		t.Logf("[%s] %s=true; skipping cluster teardown", ProviderOpenShift, envOpenShiftSkipTeardown)
		for clusterName, installDir := range r.installDirs {
			t.Logf("[%s] Preserved install dir for %q: %s", ProviderOpenShift, clusterName, installDir)
		}
		return
	}

	projectID := strings.TrimSpace(os.Getenv("GCP_PROJECT_ID"))
	if projectID != "" {
		r.cleanupOpenShiftMultiClusterResources(t, projectID)
	} else if len(r.Clusters) > 1 {
		t.Logf("[%s] GCP_PROJECT_ID is unset; skipping custom multi-cluster resource cleanup", ProviderOpenShift)
	}

	for clusterName, installDir := range r.installDirs {
		t.Logf("[%s] Destroying cluster %q", ProviderOpenShift, clusterName)
		cmd := exec.Command("openshift-install", "destroy", "cluster", "--dir", installDir, "--log-level", "info")
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		cmd.Env = openShiftInstallerEnv()

		if err := cmd.Run(); err != nil {
			t.Logf("[%s] Warning: failed to destroy cluster %q: %v", ProviderOpenShift, clusterName, err)
		}
		if err := os.RemoveAll(installDir); err != nil {
			t.Logf("[%s] Warning: failed to remove install dir %s: %v", ProviderOpenShift, installDir, err)
		}
	}
}

func (r *OpenShiftRegion) cleanupOpenShiftMultiClusterResources(t *testing.T, projectID string) {
	t.Helper()
	if len(r.Clusters) <= 1 {
		return
	}

	for _, clusterName := range r.Clusters {
		infraID := r.infraIDs[clusterName]
		if infraID == "" {
			t.Logf("[%s] Skipping Submariner cleanup for %q: missing infra ID", ProviderOpenShift, clusterName)
			continue
		}
		deleteOpenShiftFirewallRule(t, projectID, openShiftSubmarinerFirewallName(infraID, openShiftSubmarinerPublicFirewallRule))
		deleteOpenShiftFirewallRule(t, projectID, openShiftSubmarinerFirewallName(infraID, openShiftSubmarinerInternalFirewallRule))
	}

	for i := 0; i < len(r.Clusters); i++ {
		for j := i + 1; j < len(r.Clusters); j++ {
			clusterA := r.Clusters[i]
			clusterB := r.Clusters[j]
			infraA := r.infraIDs[clusterA]
			infraB := r.infraIDs[clusterB]
			if infraA == "" || infraB == "" {
				t.Logf("[%s] Skipping custom cleanup for %q/%q: missing infra IDs (%q, %q)",
					ProviderOpenShift, clusterA, clusterB, infraA, infraB)
				continue
			}

			t.Logf("[%s] Removing custom multi-cluster resources between %q and %q",
				ProviderOpenShift, infraA, infraB)
			deleteOpenShiftFirewallRule(t, projectID, openShiftPeerFirewallName(infraA, infraB))
			deleteOpenShiftFirewallRule(t, projectID, openShiftPeerFirewallName(infraB, infraA))
			deleteOpenShiftVPCPeering(t, projectID, openShiftNetworkName(infraA), openShiftPeeringName(infraA, infraB))
			deleteOpenShiftVPCPeering(t, projectID, openShiftNetworkName(infraB), openShiftPeeringName(infraB, infraA))
		}
	}
}

func (r *OpenShiftRegion) ScaleNodePool(t *testing.T, location string, nodeCount, index int) {
	if index >= len(r.Clusters) {
		t.Fatalf("[%s] invalid cluster index %d, only have %d clusters", ProviderOpenShift, index, len(r.Clusters))
	}

	clusterName := r.Clusters[index]
	kubeConfigPath, err := r.EnsureKubeConfigPath()
	require.NoError(t, err)

	t.Logf("[%s] validating pre-provisioned worker capacity for cluster %q: need %d ready workers",
		ProviderOpenShift, clusterName, nodeCount)
	WaitForReadyNodes(t, ProviderOpenShift, clusterName, kubeConfigPath, "node-role.kubernetes.io/worker", nodeCount)
}

func (r *OpenShiftRegion) CanScale() bool {
	return true
}

func (r *OpenShiftRegion) GetEncryptionProvider() encryption.Provider {
	return r
}

func (r *OpenShiftRegion) SetupEncryptionInfrastructure(t *testing.T) (func(), error) {
	t.Logf("[%s] No KMS infrastructure needed for file-based encryption (UNKNOWN_KEY_TYPE)", ProviderOpenShift)
	return func() {
		t.Logf("[%s] No KMS infrastructure to clean up", ProviderOpenShift)
	}, nil
}

func (r *OpenShiftRegion) GetEncryptionPlatformConfig() *encryption.PlatformConfig {
	return &encryption.PlatformConfig{
		Platform:                     "UNKNOWN_KEY_TYPE",
		RequiresCredentialsSecret:    false,
		DefaultCredentialsSecretName: "",
	}
}

func (r *OpenShiftRegion) EncryptKey(plaintextKey []byte, clusterRegion string) (string, error) {
	return "", fmt.Errorf("EncryptKey not supported for %s (file-based encryption / UNKNOWN_KEY_TYPE)", ProviderOpenShift)
}

func (r *OpenShiftRegion) CreateKeySecret(kubectlOptions *k8s.KubectlOptions, secretName string, encryptedKeyData string, clusterRegion string) error {
	return fmt.Errorf("CreateKeySecret not supported for %s (file-based encryption / UNKNOWN_KEY_TYPE)", ProviderOpenShift)
}

func (r *OpenShiftRegion) CreateCredentialsSecret(kubectlOptions *k8s.KubectlOptions) (string, error) {
	return "", fmt.Errorf("CreateCredentialsSecret not supported for %s (file-based encryption / UNKNOWN_KEY_TYPE)", ProviderOpenShift)
}

func (r *OpenShiftRegion) provisionCluster(
	t *testing.T,
	clusterName, region, projectID, baseDomain, pullSecret, sshPubKey string,
	workerReplicas int,
	netCfg openShiftNetworkConfig,
) (string, string, error) {
	installerName := shortOpenShiftInstallerName(clusterName)
	installDir, err := os.MkdirTemp("", fmt.Sprintf("helm-charts-ocp-%s-*", installerName))
	if err != nil {
		return "", "", fmt.Errorf("failed to create OpenShift install dir: %w", err)
	}

	configPath := filepath.Join(installDir, "install-config.yaml")
	configFile, err := os.Create(configPath)
	if err != nil {
		_ = os.RemoveAll(installDir)
		return "", "", fmt.Errorf("failed to create install-config.yaml: %w", err)
	}

	data := openShiftInstallConfigData{
		BaseDomain:     baseDomain,
		ClusterName:    installerName,
		ProjectID:      projectID,
		Region:         region,
		PullSecret:     pullSecret,
		SSHPubKey:      sshPubKey,
		WorkerReplicas: workerReplicas,
		MachineCIDR:    netCfg.MachineCIDR,
		ClusterNetwork: netCfg.ClusterNetwork,
		HostPrefix:     netCfg.HostPrefix,
		ServiceNetwork: netCfg.ServiceNetwork,
	}
	if err := openShiftInstallConfigTemplate.Execute(configFile, data); err != nil {
		_ = configFile.Close()
		_ = os.RemoveAll(installDir)
		return "", "", fmt.Errorf("failed to render install-config.yaml: %w", err)
	}
	if err := configFile.Close(); err != nil {
		_ = os.RemoveAll(installDir)
		return "", "", fmt.Errorf("failed to close install-config.yaml: %w", err)
	}

	logLevel := strings.TrimSpace(os.Getenv(envOpenShiftInstallLogLevel))
	if logLevel == "" {
		logLevel = defaultOpenShiftInstallLogLevel
	}

	if err := r.createInstallManifests(t, installDir, logLevel); err != nil {
		_ = os.RemoveAll(installDir)
		return "", "", err
	}

	t.Logf("[%s] Running openshift-install create cluster in %s", ProviderOpenShift, installDir)
	cmd := exec.Command("openshift-install", "create", "cluster", "--dir", installDir, "--log-level", logLevel)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = openShiftInstallerEnv()

	if err := cmd.Run(); err != nil {
		if parseBoolEnv(envOpenShiftSkipTeardown) {
			t.Logf("[%s] openshift-install failed; %s=true, preserving failed install dir and cluster resources: %s",
				ProviderOpenShift, envOpenShiftSkipTeardown, installDir)
			return "", "", fmt.Errorf("openshift-install create cluster failed: %w", err)
		}

		t.Logf("[%s] openshift-install failed, attempting cleanup", ProviderOpenShift)
		destroyCmd := exec.Command("openshift-install", "destroy", "cluster", "--dir", installDir, "--log-level", "warn")
		destroyCmd.Stdout = os.Stdout
		destroyCmd.Stderr = os.Stderr
		destroyCmd.Env = openShiftInstallerEnv()
		_ = destroyCmd.Run()
		_ = os.RemoveAll(installDir)
		return "", "", fmt.Errorf("openshift-install create cluster failed: %w", err)
	}

	infraID, err := readOpenShiftInfraID(installDir)
	if err != nil {
		t.Logf("[%s] Warning: could not read metadata infraID, using installer name %q: %v", ProviderOpenShift, installerName, err)
		infraID = installerName
	}

	return installDir, infraID, nil
}

func (r *OpenShiftRegion) createInstallManifests(t *testing.T, installDir, logLevel string) error {
	t.Helper()

	t.Logf("[%s] Running openshift-install create manifests in %s", ProviderOpenShift, installDir)
	cmd := exec.Command("openshift-install", "create", "manifests", "--dir", installDir, "--log-level", logLevel)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = openShiftInstallerEnv()
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("openshift-install create manifests failed: %w", err)
	}

	if err := writeOpenShiftCRIOUlimitsMachineConfig(installDir); err != nil {
		return fmt.Errorf("failed to write OpenShift CRI-O ulimit MachineConfig: %w", err)
	}
	return nil
}

func writeOpenShiftCRIOUlimitsMachineConfig(installDir string) error {
	crioConfig := fmt.Sprintf(`[crio.runtime]
default_ulimits = ["nofile=%d:%d"]
`, openShiftCockroachNofileLimit, openShiftCockroachNofileLimit)

	manifest := fmt.Sprintf(`apiVersion: machineconfiguration.openshift.io/v1
kind: MachineConfig
metadata:
  name: %s
  labels:
    machineconfiguration.openshift.io/role: worker
spec:
  config:
    ignition:
      version: 3.2.0
    storage:
      files:
      - path: %s
        mode: 420
        overwrite: true
        contents:
          source: data:text/plain;charset=utf-8;base64,%s
`, openShiftCRIOMachineConfigName, openShiftCRIOUlimitsPath, base64.StdEncoding.EncodeToString([]byte(crioConfig)))

	manifestDir := filepath.Join(installDir, "openshift")
	if err := os.MkdirAll(manifestDir, 0755); err != nil {
		return err
	}
	manifestPath := filepath.Join(manifestDir, openShiftCRIOMachineConfigName+".yaml")
	return os.WriteFile(manifestPath, []byte(manifest), 0644)
}

func (r *OpenShiftRegion) initializeOpenShiftClients(t *testing.T) {
	for _, clusterName := range r.Clusters {
		restCfg, err := config.GetConfigWithContext(clusterName)
		require.NoError(t, err, "[%s] failed to get kube config for %q", ProviderOpenShift, clusterName)

		k8sClient, err := client.New(restCfg, client.Options{})
		require.NoError(t, err, "[%s] failed to create k8s client for %q", ProviderOpenShift, clusterName)

		r.Clients[clusterName] = k8sClient
	}
}

func (r *OpenShiftRegion) validateOpenShiftCluster(t *testing.T, kubeConfigPath, clusterName string) {
	kubectlOptions := k8s.NewKubectlOptions(clusterName, kubeConfigPath, "")

	_, err := k8s.RunKubectlAndGetOutputE(t, kubectlOptions, "get", "nodes")
	require.NoError(t, err, "[%s] failed to list OpenShift nodes", ProviderOpenShift)

	_, err = k8s.RunKubectlAndGetOutputE(t, kubectlOptions,
		"wait", "--for=condition=Available", "clusteroperators.config.openshift.io", "--all", "--timeout=15m")
	require.NoError(t, err, "[%s] OpenShift cluster operators did not become Available", ProviderOpenShift)

	storageClassName := openShiftStorageClassName()
	_, err = k8s.RunKubectlAndGetOutputE(t, kubectlOptions, "get", "storageclass", storageClassName)
	require.NoError(t, err, "[%s] required StorageClass %q was not found", ProviderOpenShift, storageClassName)

	r.validateOpenShiftWorkloadPrereqs(t, kubeConfigPath, clusterName, storageClassName)
}

func (r *OpenShiftRegion) validateOpenShiftWorkloadPrereqs(
	t *testing.T, kubeConfigPath, clusterName, storageClassName string,
) {
	namespace := fmt.Sprintf("openshift-preflight-%s", shortOpenShiftInstallerName(clusterName))
	kubectlOptions := k8s.NewKubectlOptions(clusterName, kubeConfigPath, namespace)
	clusterOptions := k8s.NewKubectlOptions(clusterName, kubeConfigPath, "")

	t.Cleanup(func() {
		_ = k8s.RunKubectlE(t, clusterOptions, "delete", "namespace", namespace, "--ignore-not-found=true")
	})

	require.NoError(t, k8s.RunKubectlE(t, clusterOptions, "create", "namespace", namespace),
		"[%s] failed to create preflight namespace", ProviderOpenShift)
	require.NoError(t, k8s.RunKubectlE(t, kubectlOptions, "create", "serviceaccount", "preflight"),
		"[%s] failed to create preflight service account", ProviderOpenShift)

	manifest := fmt.Sprintf(`apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: preflight-pvc
spec:
  accessModes:
  - ReadWriteOnce
  storageClassName: %s
  resources:
    requests:
      storage: 1Gi
---
apiVersion: v1
kind: Pod
metadata:
  name: preflight-pod
spec:
  restartPolicy: Never
  serviceAccountName: preflight
  containers:
  - name: preflight
    image: registry.access.redhat.com/ubi9/ubi-minimal:latest
    command:
    - /bin/sh
    - -c
    - |
      hard="$(ulimit -Hn)"
      case "$hard" in
        unlimited) ;;
        ''|*[!0-9]*)
          echo "unexpected nofile hard limit: ${hard}" >&2
          exit 1
          ;;
        *)
          if [ "$hard" -lt %d ]; then
            echo "nofile hard limit ${hard} is below required %d" >&2
            exit 1
          fi
          ;;
      esac
      echo "nofile soft=$(ulimit -Sn) hard=${hard}" > /data/preflight
      sleep 30
    volumeMounts:
    - name: data
      mountPath: /data
  volumes:
  - name: data
    persistentVolumeClaim:
      claimName: preflight-pvc
`, storageClassName, openShiftCockroachNofileLimit, openShiftCockroachNofileLimit)

	require.NoError(t, k8s.KubectlApplyFromStringE(t, kubectlOptions, manifest),
		"[%s] failed to apply preflight PVC/pod", ProviderOpenShift)
	_, err := k8s.RunKubectlAndGetOutputE(t, kubectlOptions,
		"wait", "--for=condition=Ready", "pod/preflight-pod", "--timeout=5m")
	if err != nil {
		if logs, logErr := k8s.RunKubectlAndGetOutputE(t, kubectlOptions, "logs", "pod/preflight-pod"); logErr == nil {
			t.Logf("[%s] preflight pod logs:\n%s", ProviderOpenShift, logs)
		}
	}
	require.NoError(t, err, "[%s] preflight pod did not become ready", ProviderOpenShift)
}

func (r *OpenShiftRegion) configureOpenShiftMultiCluster(t *testing.T, kubeConfigPath string) {
	t.Helper()
	if len(r.Clusters) <= 1 {
		return
	}

	projectID := mustOpenShiftEnv(t, "GCP_PROJECT_ID")
	for _, clusterName := range r.Clusters {
		if r.infraIDs[clusterName] == "" {
			r.infraIDs[clusterName] = discoverOpenShiftInfraID(t, kubeConfigPath, clusterName)
		}
	}

	r.configureOpenShiftVPCPeering(t, projectID)
	r.configureOpenShiftSubmariner(t, kubeConfigPath, projectID)
	r.deployOpenShiftCoreDNS(t, kubeConfigPath)
	r.configureOpenShiftDNSForwarding(t, kubeConfigPath)
	r.validateOpenShiftDNSForwarding(t, kubeConfigPath)
	if OpenShiftSubmarinerEnabled() {
		t.Logf("[%s] OpenShift clusters paired with VPC peering, Submariner service/pod routing, and custom-domain DNS forwarding", ProviderOpenShift)
	} else {
		t.Logf("[%s] OpenShift clusters paired for node-network routing and custom-domain DNS forwarding; service/pod CIDR routing still requires a multi-cluster networking layer", ProviderOpenShift)
	}
}

func (r *OpenShiftRegion) validateOpenShiftDNSForwarding(t *testing.T, kubeConfigPath string) {
	t.Helper()
	for i, clusterName := range r.Clusters {
		kubectlOptions := k8s.NewKubectlOptions(clusterName, kubeConfigPath, "")
		for j := range r.Clusters {
			if i == j {
				continue
			}
			host := fmt.Sprintf("kubernetes.default.svc.%s", operator.CustomDomains[j])
			probeName := fmt.Sprintf("openshift-dns-probe-%d-%d", i, j)
			output, err := retry.DoWithRetryE(t, fmt.Sprintf("resolve %s from %s", host, clusterName), 12, 10*time.Second,
				func() (string, error) {
					_ = k8s.RunKubectlE(t, kubectlOptions, "delete", "pod", probeName, "--ignore-not-found=true", "--wait=false")
					return k8s.RunKubectlAndGetOutputE(t, kubectlOptions,
						"run", probeName,
						"--image=registry.access.redhat.com/ubi9/ubi-minimal:latest",
						"--restart=Never",
						"--rm",
						"-i",
						"--quiet",
						"--command",
						"--",
						"/bin/sh",
						"-c",
						fmt.Sprintf("getent hosts %s", host),
					)
				})
			require.NoError(t, err, "[%s] failed to resolve %s from cluster %q", ProviderOpenShift, host, clusterName)
			require.NotEmpty(t, strings.TrimSpace(output), "[%s] empty DNS result for %s from cluster %q", ProviderOpenShift, host, clusterName)
		}
	}
}

func (r *OpenShiftRegion) configureOpenShiftVPCPeering(t *testing.T, projectID string) {
	t.Helper()
	for i := 0; i < len(r.Clusters); i++ {
		for j := i + 1; j < len(r.Clusters); j++ {
			clusterA := r.Clusters[i]
			clusterB := r.Clusters[j]
			infraA := r.infraIDs[clusterA]
			infraB := r.infraIDs[clusterB]
			networkA := openShiftNetworkName(infraA)
			networkB := openShiftNetworkName(infraB)

			createOpenShiftVPCPeering(t, projectID, networkA, networkB, openShiftPeeringName(infraA, infraB))
			createOpenShiftVPCPeering(t, projectID, networkB, networkA, openShiftPeeringName(infraB, infraA))

			netCfgA := openShiftNetworkConfigForCluster(t, i)
			netCfgB := openShiftNetworkConfigForCluster(t, j)
			createOpenShiftPeerFirewallRule(t, projectID, networkA, openShiftPeerFirewallName(infraA, infraB), netCfgB)
			createOpenShiftPeerFirewallRule(t, projectID, networkB, openShiftPeerFirewallName(infraB, infraA), netCfgA)
		}
	}
}

func createOpenShiftVPCPeering(t *testing.T, projectID, network, peerNetwork, peeringName string) {
	t.Helper()
	_, err := runOpenShiftGCloud(t,
		"compute", "networks", "peerings", "create", peeringName,
		"--project", projectID,
		"--network", network,
		"--peer-network", peerNetwork,
		"--export-custom-routes",
		"--import-custom-routes",
	)
	if err == nil || isOpenShiftGCloudAlreadyExists(err) {
		return
	}
	require.NoError(t, err, "[%s] failed to create VPC peering %s", ProviderOpenShift, peeringName)
}

func deleteOpenShiftVPCPeering(t *testing.T, projectID, network, peeringName string) {
	t.Helper()
	_, err := runOpenShiftGCloud(t,
		"compute", "networks", "peerings", "delete", peeringName,
		"--project", projectID,
		"--network", network,
		"--quiet",
	)
	if err == nil || isOpenShiftGCloudNotFound(err) {
		return
	}
	t.Logf("[%s] Warning: failed to delete VPC peering %s from %s: %v",
		ProviderOpenShift, peeringName, network, err)
}

func createOpenShiftPeerFirewallRule(
	t *testing.T, projectID, network, firewallName string, peerNetCfg openShiftNetworkConfig,
) {
	t.Helper()
	sourceRanges := strings.Join([]string{
		peerNetCfg.MachineCIDR,
		peerNetCfg.ClusterNetwork,
		peerNetCfg.ServiceNetwork,
	}, ",")
	_, err := runOpenShiftGCloud(t,
		"compute", "firewall-rules", "create", firewallName,
		"--project", projectID,
		"--network", network,
		"--direction", "INGRESS",
		"--priority", "1000",
		"--source-ranges", sourceRanges,
		"--allow", "tcp,udp,icmp,esp",
	)
	if err == nil || isOpenShiftGCloudAlreadyExists(err) {
		return
	}
	require.NoError(t, err, "[%s] failed to create peer firewall rule %s", ProviderOpenShift, firewallName)
}

func deleteOpenShiftFirewallRule(t *testing.T, projectID, firewallName string) {
	t.Helper()
	_, err := runOpenShiftGCloud(t,
		"compute", "firewall-rules", "delete", firewallName,
		"--project", projectID,
		"--quiet",
	)
	if err == nil || isOpenShiftGCloudNotFound(err) {
		return
	}
	t.Logf("[%s] Warning: failed to delete firewall rule %s: %v",
		ProviderOpenShift, firewallName, err)
}

func (r *OpenShiftRegion) deployOpenShiftCoreDNS(t *testing.T, kubeConfigPath string) {
	t.Helper()
	r.CorednsClusterOptions = make(map[string]coredns.CoreDNSClusterOption)

	for i, clusterName := range r.Clusters {
		domain := operator.CustomDomains[i]
		localOptions := map[string]coredns.CoreDNSClusterOption{
			domain: {
				Domain:    domain,
				Namespace: r.Namespace[clusterName],
			},
		}

		kubectlOpts := k8s.NewKubectlOptions(clusterName, kubeConfigPath, coreDNSNamespace)
		require.NoError(t, deployCoreDNSResources(t, kubectlOpts, domain, localOptions),
			"[%s] failed to deploy CoreDNS resources to %s", ProviderOpenShift, clusterName)
		require.NoError(t, deployOpenShiftCoreDNSNodePortService(t, kubectlOpts),
			"[%s] failed to deploy CoreDNS NodePort service to %s", ProviderOpenShift, clusterName)

		service, err := k8s.GetServiceE(t, kubectlOpts, coreDNSServiceName)
		require.NoError(t, err, "[%s] failed to read CoreDNS service in %s", ProviderOpenShift, clusterName)
		require.NotEmpty(t, service.Spec.Ports, "[%s] CoreDNS service has no ports in %s", ProviderOpenShift, clusterName)

		nodePort := service.Spec.Ports[0].NodePort
		workerIPs := openShiftWorkerInternalIPs(t, kubeConfigPath, clusterName)
		upstreams := make([]string, 0, len(workerIPs))
		for _, workerIP := range workerIPs {
			upstreams = append(upstreams, fmt.Sprintf("%s:%d", workerIP, nodePort))
		}

		r.CorednsClusterOptions[domain] = coredns.CoreDNSClusterOption{
			IPs:       upstreams,
			Namespace: r.Namespace[clusterName],
			Domain:    domain,
		}
	}

	UpdateCoreDNSConfiguration(t, r.Region, kubeConfigPath)
}

func deployOpenShiftCoreDNSNodePortService(t *testing.T, kubectlOpts *k8s.KubectlOptions) error {
	t.Helper()
	_ = k8s.RunKubectlE(t, kubectlOpts,
		"patch", "service", coreDNSServiceName, "--type=merge", "-p", `{"metadata":{"finalizers":null}}`)
	_ = k8s.RunKubectlE(t, kubectlOpts,
		"delete", "service", coreDNSServiceName, "--ignore-not-found=true", "--wait=true")

	service := fmt.Sprintf(`apiVersion: v1
kind: Service
metadata:
  name: %s
  namespace: %s
  labels:
    k8s-app: kube-dns
spec:
  type: NodePort
  selector:
    k8s-app: kube-dns
  ports:
  - name: dns
    port: 53
    protocol: UDP
    targetPort: 53
`, coreDNSServiceName, coreDNSNamespace)
	return k8s.KubectlApplyFromStringE(t, kubectlOpts, service)
}

func (r *OpenShiftRegion) configureOpenShiftDNSForwarding(t *testing.T, kubeConfigPath string) {
	t.Helper()
	for _, clusterName := range r.Clusters {
		servers := make([]map[string]interface{}, 0, len(r.Clusters))
		for j := range r.Clusters {
			domain := operator.CustomDomains[j]
			option := r.CorednsClusterOptions[domain]
			require.NotEmpty(t, option.IPs, "[%s] no CoreDNS upstreams configured for domain %s", ProviderOpenShift, domain)
			servers = append(servers, map[string]interface{}{
				"name":  strings.ReplaceAll(domain, ".", "-"),
				"zones": []string{domain},
				"forwardPlugin": map[string]interface{}{
					"policy":    "Sequential",
					"upstreams": option.IPs,
				},
			})
		}

		patch, err := json.Marshal(map[string]interface{}{
			"spec": map[string]interface{}{
				"servers": servers,
			},
		})
		require.NoError(t, err)

		kubectlOpts := k8s.NewKubectlOptions(clusterName, kubeConfigPath, "")
		require.NoError(t, k8s.RunKubectlE(t, kubectlOpts,
			"patch", "dns.operator.openshift.io/default", "--type=merge", "-p", string(patch)),
			"[%s] failed to configure DNS forwarding in %s", ProviderOpenShift, clusterName)
		require.NoError(t, k8s.RunKubectlE(t, kubectlOpts,
			"-n", "openshift-dns", "rollout", "status", "daemonset/dns-default", "--timeout=3m"),
			"[%s] OpenShift DNS did not roll out in %s", ProviderOpenShift, clusterName)
	}
}

func (r *OpenShiftRegion) configureOpenShiftSubmariner(t *testing.T, kubeConfigPath, projectID string) {
	t.Helper()
	if !OpenShiftSubmarinerEnabled() {
		t.Logf("[%s] %s=false; skipping Submariner setup", ProviderOpenShift, envOpenShiftEnableSubmariner)
		return
	}
	if _, err := exec.LookPath("subctl"); err != nil {
		t.Fatalf("[%s] subctl not found in PATH: %v", ProviderOpenShift, err)
	}

	credentialsPath := openShiftGCPCredentialsPath(t)

	for i, clusterName := range r.Clusters {
		netCfg := openShiftNetworkConfigForCluster(t, i)
		r.prepareOpenShiftSubmarinerGCP(t, kubeConfigPath, projectID, clusterName, r.RegionCodes[i], r.infraIDs[clusterName], credentialsPath)
		t.Logf("[%s] Prepared GCP infrastructure for Submariner cluster %q (pod CIDR %s, service CIDR %s)",
			ProviderOpenShift, clusterName, netCfg.ClusterNetwork, netCfg.ServiceNetwork)
	}

	brokerDir := t.TempDir()
	brokerInfoPath := filepath.Join(brokerDir, openShiftSubmarinerBrokerFile)
	require.NoError(t, runOpenShiftSubctl(t, brokerDir,
		"deploy-broker",
		"--kubeconfig", kubeConfigPath,
		"--context", r.Clusters[0],
		"--globalnet=false",
	), "[%s] failed to deploy Submariner broker", ProviderOpenShift)
	require.FileExists(t, brokerInfoPath, "[%s] Submariner broker info file was not created", ProviderOpenShift)

	for i, clusterName := range r.Clusters {
		netCfg := openShiftNetworkConfigForCluster(t, i)
		gatewayNode := labelOpenShiftSubmarinerGatewayNode(t, kubeConfigPath, clusterName)
		tagOpenShiftSubmarinerGatewayInstance(t, projectID, gatewayNode)
		t.Logf("[%s] Using node %q / instance %q as the Submariner gateway for cluster %q",
			ProviderOpenShift, gatewayNode.nodeName, gatewayNode.instanceName, clusterName)
		require.NoError(t, runOpenShiftSubctl(t, "",
			"join", brokerInfoPath,
			"--kubeconfig", kubeConfigPath,
			"--context", clusterName,
			"--clusterid", openShiftSubmarinerClusterID(i),
			"--clustercidr", netCfg.ClusterNetwork,
			"--servicecidr", netCfg.ServiceNetwork,
			"--globalnet=false",
			"--load-balancer",
			"--label-gateway=false",
		), "[%s] failed to join %s to Submariner", ProviderOpenShift, clusterName)
		waitForOpenShiftSubmariner(t, kubeConfigPath, clusterName)
	}

	r.verifyOpenShiftSubmariner(t, kubeConfigPath)
}

func (r *OpenShiftRegion) prepareOpenShiftSubmarinerGCP(
	t *testing.T,
	kubeConfigPath, projectID, clusterName, regionCode, infraID, credentialsPath string,
) {
	t.Helper()
	if openShiftGCPServiceAccountCredentials(t, credentialsPath) {
		r.prepareOpenShiftSubmarinerGCPWithSubctl(t, kubeConfigPath, projectID, clusterName, regionCode, infraID, credentialsPath)
		return
	}

	t.Logf("[%s] Preparing GCP infrastructure for Submariner cluster %q with gcloud; subctl cloud prepare requires service_account JSON credentials",
		ProviderOpenShift, clusterName)
	r.prepareOpenShiftSubmarinerGCPWithGCloud(t, kubeConfigPath, projectID, clusterName, infraID)
}

func (r *OpenShiftRegion) prepareOpenShiftSubmarinerGCPWithSubctl(
	t *testing.T,
	kubeConfigPath, projectID, clusterName, regionCode, infraID, credentialsPath string,
) {
	t.Helper()
	args := []string{
		"cloud", "prepare", "gcp",
		"--kubeconfig", kubeConfigPath,
		"--context", clusterName,
		"--project-id", projectID,
		"--region", regionCode,
		"--infra-id", infraID,
		"--vpc-name", openShiftNetworkName(infraID),
		"--credentials", credentialsPath,
	}
	require.NoError(t, runOpenShiftSubctl(t, "", args...),
		"[%s] failed to prepare GCP infrastructure for Submariner cluster %s", ProviderOpenShift, clusterName)
}

func (r *OpenShiftRegion) prepareOpenShiftSubmarinerGCPWithGCloud(
	t *testing.T,
	kubeConfigPath, projectID, clusterName, infraID string,
) {
	t.Helper()
	network := openShiftNetworkName(infraID)
	upsertOpenShiftFirewallRule(t, projectID, openShiftSubmarinerFirewallName(infraID, openShiftSubmarinerPublicFirewallRule),
		[]string{
			"--network", network,
			"--direction", "INGRESS",
			"--priority", "1000",
			"--source-ranges", "0.0.0.0/0",
			"--target-tags", openShiftSubmarinerGatewayTag,
			"--allow", openShiftSubmarinerPublicPorts,
		},
		[]string{
			"--priority", "1000",
			"--source-ranges", "0.0.0.0/0",
			"--target-tags", openShiftSubmarinerGatewayTag,
			"--allow", openShiftSubmarinerPublicPorts,
		},
	)

	if openShiftSubmarinerNeedsInternalVXLAN(t, kubeConfigPath, clusterName) {
		submarinerTags := strings.Join([]string{infraID + "-worker", infraID + "-master"}, ",")
		upsertOpenShiftFirewallRule(t, projectID, openShiftSubmarinerFirewallName(infraID, openShiftSubmarinerInternalFirewallRule),
			[]string{
				"--network", network,
				"--direction", "INGRESS",
				"--priority", "1000",
				"--source-tags", submarinerTags,
				"--target-tags", submarinerTags,
				"--allow", openShiftSubmarinerInternalPorts,
			},
			[]string{
				"--priority", "1000",
				"--source-tags", submarinerTags,
				"--target-tags", submarinerTags,
				"--allow", openShiftSubmarinerInternalPorts,
			},
		)
	}
}

func waitForOpenShiftSubmariner(t *testing.T, kubeConfigPath, clusterName string) {
	t.Helper()
	kubectlOpts := k8s.NewKubectlOptions(clusterName, kubeConfigPath, openShiftSubmarinerNamespace)
	resources := []string{
		"deployment/submariner-operator",
		"deployment/submariner-lighthouse-agent",
		"deployment/submariner-lighthouse-coredns",
		"daemonset/submariner-gateway",
		"daemonset/submariner-metrics-proxy",
		"daemonset/submariner-routeagent",
	}
	for _, resource := range resources {
		_, err := retry.DoWithRetryE(t, fmt.Sprintf("wait for %s in %s", resource, clusterName), defaultRetries, defaultRetryInterval,
			func() (string, error) {
				return k8s.RunKubectlAndGetOutputE(t, kubectlOpts,
					"rollout", "status", resource, "--timeout=2m")
			})
		require.NoError(t, err, "[%s] Submariner resource %s did not roll out in %s", ProviderOpenShift, resource, clusterName)
	}
}

func labelOpenShiftSubmarinerGatewayNode(t *testing.T, kubeConfigPath, clusterName string) openShiftSubmarinerGatewayNode {
	t.Helper()
	kubectlOpts := k8s.NewKubectlOptions(clusterName, kubeConfigPath, "")
	nodeName, err := k8s.RunKubectlAndGetOutputE(t, kubectlOpts,
		"get", "nodes",
		"-l", "node-role.kubernetes.io/worker",
		"--sort-by=.metadata.name",
		"-o", "jsonpath={.items[0].metadata.name}")
	require.NoError(t, err, "[%s] failed to find a worker gateway node in %s", ProviderOpenShift, clusterName)
	nodeName = strings.TrimSpace(nodeName)
	require.NotEmpty(t, nodeName, "[%s] no worker gateway node found in %s", ProviderOpenShift, clusterName)

	require.NoError(t, k8s.RunKubectlE(t, kubectlOpts,
		"label", "node", nodeName, "submariner.io/gateway=true", "--overwrite"),
		"[%s] failed to label Submariner gateway node %s in %s", ProviderOpenShift, nodeName, clusterName)

	return openShiftSubmarinerGatewayNodeDetails(t, kubectlOpts, nodeName)
}

func openShiftSubmarinerGatewayNodeDetails(
	t *testing.T, kubectlOpts *k8s.KubectlOptions, nodeName string,
) openShiftSubmarinerGatewayNode {
	t.Helper()
	output, err := k8s.RunKubectlAndGetOutputE(t, kubectlOpts, "get", "node", nodeName, "-o", "json")
	require.NoError(t, err, "[%s] failed to read Submariner gateway node %s", ProviderOpenShift, nodeName)

	var node struct {
		Metadata struct {
			Annotations map[string]string `json:"annotations"`
			Labels      map[string]string `json:"labels"`
		} `json:"metadata"`
		Spec struct {
			ProviderID string `json:"providerID"`
		} `json:"spec"`
	}
	require.NoError(t, json.Unmarshal([]byte(output), &node),
		"[%s] failed to parse Submariner gateway node %s", ProviderOpenShift, nodeName)

	instanceName := openShiftInstanceNameFromMachineAnnotation(node.Metadata.Annotations["machine.openshift.io/machine"])
	zone := strings.TrimSpace(node.Metadata.Labels["topology.kubernetes.io/zone"])
	if instanceName == "" || zone == "" {
		providerZone, providerInstance := openShiftGCPProviderIDDetails(node.Spec.ProviderID)
		if instanceName == "" {
			instanceName = providerInstance
		}
		if zone == "" {
			zone = providerZone
		}
	}

	require.NotEmpty(t, instanceName, "[%s] failed to determine GCP instance for Submariner gateway node %s",
		ProviderOpenShift, nodeName)
	require.NotEmpty(t, zone, "[%s] failed to determine GCP zone for Submariner gateway node %s",
		ProviderOpenShift, nodeName)

	return openShiftSubmarinerGatewayNode{
		nodeName:     nodeName,
		instanceName: instanceName,
		zone:         zone,
	}
}

func tagOpenShiftSubmarinerGatewayInstance(t *testing.T, projectID string, gatewayNode openShiftSubmarinerGatewayNode) {
	t.Helper()
	_, err := runOpenShiftGCloud(t,
		"compute", "instances", "add-tags", gatewayNode.instanceName,
		"--project", projectID,
		"--zone", gatewayNode.zone,
		"--tags", openShiftSubmarinerGatewayTag,
		"--quiet",
	)
	require.NoError(t, err, "[%s] failed to tag GCP instance %s as a Submariner gateway",
		ProviderOpenShift, gatewayNode.instanceName)
}

func (r *OpenShiftRegion) verifyOpenShiftSubmariner(t *testing.T, kubeConfigPath string) {
	t.Helper()
	for i := 0; i < len(r.Clusters); i++ {
		for j := i + 1; j < len(r.Clusters); j++ {
			require.NoError(t, runOpenShiftSubctl(t, "",
				"verify",
				"--kubeconfig", kubeConfigPath,
				"--context", r.Clusters[i],
				"--toconfig", kubeConfigPath,
				"--tocontext", r.Clusters[j],
				"--only", "connectivity,basic-connectivity",
				"--skip-src-ip-check",
				"--connection-attempts", "3",
				"--connection-timeout", "60",
				"--operation-timeout", "300",
			), "[%s] Submariner connectivity verification failed between %s and %s", ProviderOpenShift, r.Clusters[i], r.Clusters[j])
		}
	}
}

func openShiftWorkerInternalIPs(t *testing.T, kubeConfigPath, clusterName string) []string {
	t.Helper()
	kubectlOpts := k8s.NewKubectlOptions(clusterName, kubeConfigPath, "")
	output, err := k8s.RunKubectlAndGetOutputE(t, kubectlOpts,
		"get", "nodes",
		"-l", "node-role.kubernetes.io/worker",
		"-o", "jsonpath={range .items[*]}{.status.addresses[?(@.type==\"InternalIP\")].address}{\"\\n\"}{end}")
	require.NoError(t, err, "[%s] failed to list OpenShift worker IPs for %s", ProviderOpenShift, clusterName)

	var workerIPs []string
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			workerIPs = append(workerIPs, line)
		}
	}
	require.NotEmpty(t, workerIPs, "[%s] no OpenShift worker IPs found for %s", ProviderOpenShift, clusterName)
	return workerIPs
}

func discoverOpenShiftInfraID(t *testing.T, kubeConfigPath, clusterName string) string {
	t.Helper()
	kubectlOpts := k8s.NewKubectlOptions(clusterName, kubeConfigPath, "")
	output, err := k8s.RunKubectlAndGetOutputE(t, kubectlOpts,
		"get", "nodes", "-o", "jsonpath={.items[0].metadata.name}")
	require.NoError(t, err, "[%s] failed to discover infraID from nodes for %s", ProviderOpenShift, clusterName)

	nodeName := strings.TrimSpace(strings.Split(output, ".")[0])
	for _, marker := range []string{"-master-", "-worker-"} {
		if idx := strings.Index(nodeName, marker); idx > 0 {
			return nodeName[:idx]
		}
	}
	t.Fatalf("[%s] could not derive infraID from node name %q", ProviderOpenShift, nodeName)
	return ""
}

func openShiftNetworkName(infraID string) string {
	return infraID + "-network"
}

func openShiftPeeringName(localInfraID, peerInfraID string) string {
	return fmt.Sprintf("%s-to-%s", openShiftShortInfraID(localInfraID), openShiftShortInfraID(peerInfraID))
}

func openShiftPeerFirewallName(localInfraID, peerInfraID string) string {
	return fmt.Sprintf("%s-allow-peer-%s", openShiftShortInfraID(localInfraID), openShiftShortInfraID(peerInfraID))
}

func openShiftSubmarinerFirewallName(infraID, ruleName string) string {
	return fmt.Sprintf("%s-%s-ingress", infraID, ruleName)
}

func openShiftShortInfraID(infraID string) string {
	parts := strings.Split(infraID, "-")
	if len(parts) >= 2 {
		return parts[0] + "-" + parts[1]
	}
	return infraID
}

func runOpenShiftGCloud(t *testing.T, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command("gcloud", args...)
	cmd.Env = openShiftGCloudEnv()
	output, err := cmd.CombinedOutput()
	if err != nil {
		return string(output), fmt.Errorf("%w: %s", err, strings.TrimSpace(string(output)))
	}
	return string(output), nil
}

func openShiftGCloudEnv() []string {
	env := openShiftInstallerEnv()
	env = append(env, "CLOUDSDK_CORE_DISABLE_PROMPTS=1")
	if creds := strings.TrimSpace(os.Getenv("GOOGLE_APPLICATION_CREDENTIALS")); creds != "" {
		env = appendWithoutEnvKey(env, "CLOUDSDK_AUTH_CREDENTIAL_FILE_OVERRIDE")
		env = append(env, "CLOUDSDK_AUTH_CREDENTIAL_FILE_OVERRIDE="+creds)
	}
	return env
}

func isOpenShiftGCloudAlreadyExists(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "already exists") ||
		strings.Contains(message, "alreadyexists")
}

func isOpenShiftGCloudNotFound(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "not found") ||
		strings.Contains(message, "notfound") ||
		strings.Contains(message, "was not found") ||
		strings.Contains(message, "does not exist")
}

func upsertOpenShiftFirewallRule(t *testing.T, projectID, firewallName string, createFlags, updateFlags []string) {
	t.Helper()
	createArgs := append([]string{"compute", "firewall-rules", "create", firewallName, "--project", projectID}, createFlags...)
	_, err := runOpenShiftGCloud(t, createArgs...)
	if err == nil {
		return
	}
	if !isOpenShiftGCloudAlreadyExists(err) {
		require.NoError(t, err, "[%s] failed to create firewall rule %s", ProviderOpenShift, firewallName)
		return
	}

	updateArgs := append([]string{"compute", "firewall-rules", "update", firewallName, "--project", projectID}, updateFlags...)
	_, err = runOpenShiftGCloud(t, updateArgs...)
	require.NoError(t, err, "[%s] failed to update firewall rule %s", ProviderOpenShift, firewallName)
}

func runOpenShiftSubctl(t *testing.T, dir string, args ...string) error {
	t.Helper()
	cmd := exec.Command("subctl", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	cmd.Env = openShiftInstallerEnv()
	output, err := cmd.CombinedOutput()
	t.Logf("[%s] subctl %s\n%s", ProviderOpenShift, strings.Join(args, " "), strings.TrimSpace(string(output)))
	if err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}

func openShiftGCPServiceAccountCredentials(t *testing.T, credentialsPath string) bool {
	t.Helper()
	if strings.TrimSpace(credentialsPath) == "" {
		return false
	}

	data, err := os.ReadFile(credentialsPath)
	if err != nil {
		t.Logf("[%s] Could not read credentials file %s for subctl cloud prepare, using gcloud fallback: %v",
			ProviderOpenShift, credentialsPath, err)
		return false
	}

	var credentials struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(data, &credentials); err != nil {
		t.Logf("[%s] Credentials file %s is not JSON, using gcloud fallback: %v", ProviderOpenShift, credentialsPath, err)
		return false
	}
	return credentials.Type == "service_account"
}

func openShiftGCPCredentialsPath(t *testing.T) string {
	t.Helper()
	if credentialsPath := strings.TrimSpace(os.Getenv("GOOGLE_APPLICATION_CREDENTIALS")); credentialsPath != "" {
		return credentialsPath
	}

	googleCredentials := strings.TrimSpace(os.Getenv("GOOGLE_CREDENTIALS"))
	if googleCredentials == "" {
		return getServiceAccountKeyPath()
	}
	if !json.Valid([]byte(googleCredentials)) {
		return googleCredentials
	}

	credentialsPath := filepath.Join(t.TempDir(), "gcp-credentials.json")
	require.NoError(t, os.WriteFile(credentialsPath, []byte(googleCredentials), 0600),
		"[%s] failed to write GOOGLE_CREDENTIALS for Submariner cloud prepare", ProviderOpenShift)
	return credentialsPath
}

func openShiftSubmarinerNeedsInternalVXLAN(t *testing.T, kubeConfigPath, clusterName string) bool {
	t.Helper()
	kubectlOpts := k8s.NewKubectlOptions(clusterName, kubeConfigPath, "")
	networkPlugin, err := k8s.RunKubectlAndGetOutputE(t, kubectlOpts,
		"get", "network.operator.openshift.io/cluster", "-o", "jsonpath={.spec.defaultNetwork.type}")
	if err != nil {
		t.Logf("[%s] Could not determine OpenShift network plugin for %s, opening Submariner internal VXLAN port: %v",
			ProviderOpenShift, clusterName, err)
		return true
	}
	networkPlugin = strings.TrimSpace(networkPlugin)
	if networkPlugin == "OVNKubernetes" {
		t.Logf("[%s] Cluster %q uses OVNKubernetes; Submariner internal VXLAN firewall rule is not required",
			ProviderOpenShift, clusterName)
		return false
	}
	return true
}

func openShiftInstanceNameFromMachineAnnotation(annotation string) string {
	annotation = strings.TrimSpace(annotation)
	if annotation == "" {
		return ""
	}
	parts := strings.Split(annotation, "/")
	return strings.TrimSpace(parts[len(parts)-1])
}

func openShiftGCPProviderIDDetails(providerID string) (string, string) {
	providerID = strings.TrimSpace(providerID)
	parts := strings.Split(providerID, "/")
	if len(parts) < 2 {
		return "", ""
	}
	instanceName := strings.TrimSpace(parts[len(parts)-1])
	zone := strings.TrimSpace(parts[len(parts)-2])
	return zone, instanceName
}

func readOpenShiftInfraID(installDir string) (string, error) {
	metadataPath := filepath.Join(installDir, "metadata.json")
	data, err := os.ReadFile(metadataPath)
	if err != nil {
		return "", fmt.Errorf("failed to read metadata.json: %w", err)
	}

	var metadata openShiftMetadata
	if err := json.Unmarshal(data, &metadata); err != nil {
		return "", fmt.Errorf("failed to parse metadata.json: %w", err)
	}
	if metadata.InfraID == "" {
		return "", fmt.Errorf("metadata.json has empty infraID")
	}
	return metadata.InfraID, nil
}

func mergeOpenShiftKubeconfig(generatedKubeconfig, destinationKubeconfig, clusterAlias string) error {
	sourceConfig, err := clientcmd.LoadFromFile(generatedKubeconfig)
	if err != nil {
		return fmt.Errorf("failed to load generated kubeconfig: %w", err)
	}

	destinationConfig, err := clientcmd.LoadFromFile(destinationKubeconfig)
	if err != nil {
		return fmt.Errorf("failed to load destination kubeconfig: %w", err)
	}
	if destinationConfig.Clusters == nil {
		destinationConfig.Clusters = make(map[string]*clientcmdapi.Cluster)
	}
	if destinationConfig.AuthInfos == nil {
		destinationConfig.AuthInfos = make(map[string]*clientcmdapi.AuthInfo)
	}
	if destinationConfig.Contexts == nil {
		destinationConfig.Contexts = make(map[string]*clientcmdapi.Context)
	}

	sourceContextName := sourceConfig.CurrentContext
	if sourceContextName == "" {
		for contextName := range sourceConfig.Contexts {
			sourceContextName = contextName
			break
		}
	}
	sourceContext := sourceConfig.Contexts[sourceContextName]
	if sourceContext == nil {
		return fmt.Errorf("generated kubeconfig has no usable context")
	}

	sourceCluster := sourceConfig.Clusters[sourceContext.Cluster]
	if sourceCluster == nil {
		return fmt.Errorf("generated kubeconfig context %q references missing cluster %q", sourceContextName, sourceContext.Cluster)
	}
	sourceAuth := sourceConfig.AuthInfos[sourceContext.AuthInfo]
	if sourceAuth == nil {
		return fmt.Errorf("generated kubeconfig context %q references missing user %q", sourceContextName, sourceContext.AuthInfo)
	}

	userAlias := "admin-" + clusterAlias
	destinationConfig.Clusters[clusterAlias] = sourceCluster
	destinationConfig.AuthInfos[userAlias] = sourceAuth
	destinationConfig.Contexts[clusterAlias] = &clientcmdapi.Context{
		Cluster:   clusterAlias,
		AuthInfo:  userAlias,
		Namespace: sourceContext.Namespace,
	}
	destinationConfig.CurrentContext = clusterAlias

	if err := clientcmd.WriteToFile(*destinationConfig, destinationKubeconfig); err != nil {
		return fmt.Errorf("failed to write merged kubeconfig: %w", err)
	}
	return nil
}

func readOpenShiftPullSecret(t *testing.T) string {
	value := mustOpenShiftEnv(t, envOpenShiftPullSecret)
	value = readEnvValueOrFile(t, value)
	if !json.Valid([]byte(value)) {
		t.Fatalf("[%s] %s must be valid pull-secret JSON or a path to a file containing it", ProviderOpenShift, envOpenShiftPullSecret)
	}
	return strings.TrimSpace(value)
}

func readOpenShiftSSHPubKey(t *testing.T) string {
	value := mustOpenShiftEnv(t, envOpenShiftSSHPubKey)
	return strings.TrimSpace(readEnvValueOrFile(t, value))
}

func readEnvValueOrFile(t *testing.T, value string) string {
	t.Helper()
	if data, err := os.ReadFile(value); err == nil {
		return strings.TrimSpace(string(data))
	}
	return value
}

func mustOpenShiftEnv(t *testing.T, envName string) string {
	t.Helper()
	value := strings.TrimSpace(os.Getenv(envName))
	if value == "" {
		t.Fatalf("[%s] required environment variable %s is not set", ProviderOpenShift, envName)
	}
	return value
}

func openShiftStorageClassName() string {
	if storageClassName := strings.TrimSpace(os.Getenv(envOpenShiftStorageClass)); storageClassName != "" {
		return storageClassName
	}
	return defaultOpenShiftStorageClass
}

func OpenShiftSubmarinerEnabled() bool {
	value := strings.TrimSpace(os.Getenv(envOpenShiftEnableSubmariner))
	if value == "" {
		return true
	}
	return parseBoolEnv(envOpenShiftEnableSubmariner)
}

func openShiftReuseKubeconfigs(t *testing.T, clusterCount int) []string {
	t.Helper()

	reuseKubeconfigs := strings.TrimSpace(os.Getenv(envOpenShiftReuseKubeconfigs))
	if reuseKubeconfigs != "" {
		paths := filepath.SplitList(reuseKubeconfigs)
		if len(paths) != clusterCount {
			t.Fatalf("[%s] %s must contain %d kubeconfig paths, got %d",
				ProviderOpenShift, envOpenShiftReuseKubeconfigs, clusterCount, len(paths))
		}
		return paths
	}

	reuseKubeconfig := strings.TrimSpace(os.Getenv(envOpenShiftReuseKubeconfig))
	if reuseKubeconfig == "" {
		return nil
	}
	if clusterCount != 1 {
		t.Fatalf("[%s] %s only supports single-cluster reuse; use %s for %d clusters",
			ProviderOpenShift, envOpenShiftReuseKubeconfig, envOpenShiftReuseKubeconfigs, clusterCount)
	}
	return []string{reuseKubeconfig}
}

func openShiftReuseContexts(t *testing.T, clusterCount int) []string {
	t.Helper()
	reuseContexts := strings.TrimSpace(os.Getenv(envOpenShiftReuseContexts))
	if reuseContexts == "" {
		return nil
	}
	contexts := filepath.SplitList(reuseContexts)
	if len(contexts) != clusterCount {
		t.Fatalf("[%s] %s must contain %d context names, got %d",
			ProviderOpenShift, envOpenShiftReuseContexts, clusterCount, len(contexts))
	}
	return contexts
}

func openShiftSubmarinerClusterID(clusterIndex int) string {
	return fmt.Sprintf("openshift-%d", clusterIndex+1)
}

func openShiftInstallerEnv() []string {
	env := os.Environ()
	env = appendWithoutEnvKey(env, "GODEBUG")
	env = append(env, "GODEBUG="+withOpenShiftInstallerGODEBUG(os.Getenv("GODEBUG")))

	googleCreds := os.Getenv("GOOGLE_CREDENTIALS")
	if googleCreds == "" || json.Valid([]byte(googleCreds)) {
		return env
	}

	env = appendWithoutEnvKey(env, "GOOGLE_APPLICATION_CREDENTIALS")
	return append(env, "GOOGLE_APPLICATION_CREDENTIALS="+googleCreds)
}

func withOpenShiftInstallerGODEBUG(value string) string {
	if strings.TrimSpace(value) == "" {
		return "netdns=go"
	}
	if strings.Contains(value, "netdns=") {
		return value
	}
	return value + ",netdns=go"
}

func appendWithoutEnvKey(env []string, key string) []string {
	prefix := key + "="
	filtered := make([]string, 0, len(env))
	for _, entry := range env {
		if strings.HasPrefix(entry, prefix) {
			continue
		}
		filtered = append(filtered, entry)
	}
	return filtered
}

func parseBoolEnv(envName string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(envName))) {
	case "1", "t", "true", "y", "yes":
		return true
	default:
		return false
	}
}

func openShiftWorkerReplicasForNodeCount(nodeCount int) int {
	if nodeCount < 1 {
		return 3 + openShiftScaleUpExtraWorkers
	}
	return nodeCount + openShiftScaleUpExtraWorkers
}

func openShiftNetworkConfigForCluster(t *testing.T, clusterIndex int) openShiftNetworkConfig {
	t.Helper()
	if clusterIndex >= len(openShiftNetworkConfigs) {
		t.Fatalf("[%s] no OpenShift network config defined for cluster index %d", ProviderOpenShift, clusterIndex)
	}
	return openShiftNetworkConfigs[clusterIndex]
}

func shortOpenShiftInstallerName(clusterName string) string {
	sum := sha1.Sum([]byte(clusterName + time.Now().UTC().Format(time.RFC3339Nano)))
	return "ocp-" + hex.EncodeToString(sum[:])[:10]
}

var _ CloudProvider = (*OpenShiftRegion)(nil)
var _ encryption.Provider = (*OpenShiftRegion)(nil)
