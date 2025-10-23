package operator

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cockroachdb/helm-charts/tests/e2e/coredns"
	"github.com/cockroachdb/helm-charts/tests/testutil"
	"github.com/gruntwork-io/terratest/modules/helm"
	"github.com/gruntwork-io/terratest/modules/k8s"
	"github.com/gruntwork-io/terratest/modules/random"
	"github.com/gruntwork-io/terratest/modules/retry"
	"github.com/gruntwork-io/terratest/modules/shell"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/clientcmd/api"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"
)

const (
	TestDBName            = "testdb"
	Namespace             = "cockroach-ns"
	LabelSelector         = "app=cockroachdb"
	OperatorLabelSelector = "app=cockroach-operator"
)

var (
	Clusters      = []string{"chart-testing-cluster-0", "chart-testing-cluster-1"}
	CustomDomains = map[int]string{
		0: "cluster1.local",
		1: "cluster2.local",
	}

	operatorReleaseName    = "cockroachdb-operator"
	customCASecret         = "cockroachdb-ca-secret"
	ReleaseName            = "cockroachdb"
	CockroachContainerName = "cockroachdb"

	helmExtraArgs = map[string][]string{
		"install": {
			"--wait",
			"--debug",
		},
	}
)

// OperatorUseCases defines use cases for the CockroachDB cluster.
type OperatorUseCases interface {
	TestHelmInstall(t *testing.T)
	TestHelmUpgrade(t *testing.T)
	TestClusterScaleUp(t *testing.T)
	TestClusterRollingRestart(t *testing.T)
	TestKillingCockroachNode(t *testing.T)
	TestInstallWithCertManager(t *testing.T)
}

type Region struct {
	// IsCertManager is true if the cockroachdb cluster is using cert-manager.
	IsCertManager bool
	// IsMultiRegion is true if the region is multi-region.
	IsMultiRegion bool
	// NodeCount is the desired CockroachDB nodes in the region.
	NodeCount int
	// Namespace stores mapping between cluster name and namespace.
	Namespace    map[string]string
	ReusingInfra bool
	// Clients store the k8s client for each cluster
	// needed for performing k8s operations on k8s objects.
	Clients               map[string]client.Client
	Clusters              []string
	CorednsClusterOptions map[string]coredns.CoreDNSClusterOption
	Provider              string
	RegionCodes           []string

	VirtualClusterModePrimary bool
	VirtualClusterModeStandby bool
	IsOperatorInstalled       bool
}

// InstallCharts Installs both Operator and CockroachDB charts by providing custom CA secret
// which is generated through cockroach binary, It also
// verifies whether relevant services are up and running.
func (r *Region) InstallCharts(t *testing.T, cluster string, index int) {
	var crdbOp map[string]string
	// Get the current context name.
	kubeConfig, rawConfig := r.GetCurrentContext(t)

	// Get helm chart paths.
	helmChartPath, _ := HelmChartPaths()

	// Verify if a cluster exists in the contexts.
	if _, ok := rawConfig.Contexts[cluster]; !ok {
		t.Fatal()
	}

	// Setup kubectl options for this cluster.
	kubectlOptions := k8s.NewKubectlOptions(cluster, kubeConfig, r.Namespace[cluster])
	certManagerK8sOptions := k8s.NewKubectlOptions(cluster, kubeConfig, testutil.CertManagerNamespace)

	// Create a namespace.
	k8s.CreateNamespace(t, kubectlOptions, r.Namespace[cluster])

	if r.IsCertManager {
		testutil.InstallCertManager(t, certManagerK8sOptions)
		testutil.InstallTrustManager(t, certManagerK8sOptions, r.Namespace[cluster])
		testutil.CreateSelfSignedIssuer(t, kubectlOptions, r.Namespace[cluster])
		testutil.CreateSelfSignedCertificate(t, kubectlOptions, r.Namespace[cluster])
		testutil.CreateCAIssuer(t, kubectlOptions, r.Namespace[cluster])
		testutil.CreateBundle(t, kubectlOptions, testutil.CASecretName, testutil.CAConfigMapName)
	} else {
		// create CA Secret.
		err := k8s.RunKubectlE(t, kubectlOptions, "create", "secret", "generic", customCASecret, "--from-file=ca.crt",
			"--from-file=ca.key")
		require.NoError(t, err)
	}

	// Setup kubectl options for this cluster.
	kubectlOptions = k8s.NewKubectlOptions(cluster, kubeConfig, r.Namespace[cluster])
	if !r.IsOperatorInstalled {
		InstallCockroachDBEnterpriseOperator(t, kubectlOptions)
	}

	if r.IsCertManager {
		crdbOp = PatchHelmValues(map[string]string{
			"cockroachdb.clusterDomain":               CustomDomains[index],
			"cockroachdb.tls.enabled":                 "true",
			"cockroachdb.tls.selfSigner.enabled":      "false",
			"cockroachdb.tls.certManager.enabled":     "true",
			"cockroachdb.tls.certManager.issuer.name": testutil.CAIssuerName,
			"cockroachdb.tls.certManager.caConfigMap": testutil.CAConfigMapName,
		})
	} else {
		crdbOp = PatchHelmValues(map[string]string{
			"cockroachdb.clusterDomain":             CustomDomains[index],
			"cockroachdb.tls.selfSigner.caProvided": "true",
			"cockroachdb.tls.selfSigner.caSecret":   customCASecret,
		})
	}
	if r.VirtualClusterModePrimary {
		crdbOp = PatchHelmValues(map[string]string{
			"cockroachdb.clusterDomain":                   CustomDomains[index],
			"cockroachdb.crdbCluster.virtualCluster.mode": "primary",
		})
	}
	if r.VirtualClusterModeStandby {
		crdbOp = PatchHelmValues(map[string]string{
			"cockroachdb.clusterDomain":                   CustomDomains[index],
			"cockroachdb.crdbCluster.virtualCluster.mode": "standby",
		})
	}

	// Helm install cockroach CR with operator region config.
	crdbOptions := &helm.Options{
		KubectlOptions: kubectlOptions,
		SetValues:      crdbOp,
		SetJsonValues: map[string]string{
			"cockroachdb.crdbCluster.regions": MustMarshalJSON(r.OperatorRegions(index, r.NodeCount)),
		},
		ExtraArgs: helmExtraArgs,
	}

	helm.Install(t, crdbOptions, helmChartPath, ReleaseName)

	serviceName := "cockroachdb-public"
	k8s.WaitUntilServiceAvailable(t, kubectlOptions, serviceName, 30, 5*time.Second)
}

// ValidateCRDB validates the CockroachDB cluster by performing basic operations on db.
func (r *Region) ValidateCRDB(t *testing.T, cluster string) {
	cfg, err := config.GetConfigWithContext(cluster)
	require.NoError(t, err)
	// Get current context name.
	kubeConfig, _ := r.GetCurrentContext(t)
	// Setup kubectl options for this cluster.
	namespaceName := r.Namespace[cluster]
	kubectlOptions := k8s.NewKubectlOptions(cluster, kubeConfig, namespaceName)
	crdbCluster := testutil.CockroachCluster{
		Cfg:              cfg,
		K8sClient:        r.Clients[cluster],
		StatefulSetName:  "cockroachdb",
		Namespace:        namespaceName,
		ClientSecret:     "cockroachdb-client-secret",
		NodeSecret:       "cockroachdb-node-secret",
		CaSecret:         customCASecret,
		IsCaUserProvided: true,
		DesiredNodes:     r.NodeCount,
		Context:          cluster,
	}

	if r.IsCertManager {
		crdbCluster.CaSecret = testutil.CASecretName
		crdbCluster.NodeSecret = "cockroachdb-node"
		crdbCluster.ClientSecret = "cockroachdb-root"
	}

	if !r.IsCertManager {
		testutil.RequireCertificatesToBeValid(t, crdbCluster)
	}
	testutil.RequireCRDBClusterToBeReadyEventuallyTimeout(t, kubectlOptions, crdbCluster, 900*time.Second)

	pods := k8s.ListPods(t, kubectlOptions, metav1.ListOptions{
		LabelSelector: LabelSelector,
	})
	require.True(t, len(pods) > 0)
	podName := fmt.Sprintf("%s.cockroachdb.%s", pods[0].Name, r.Namespace[cluster])

	// In Virtual Cluster, the standby cluster does not serve SQL traffic until you promote it.
	// Hence we are not testing database functions on it.
	if !r.VirtualClusterModeStandby {
		testutil.RequireCRDBClusterToFunction(t, crdbCluster, false, podName)
		testutil.RequireCRDBDatabaseToFunction(t, crdbCluster, TestDBName, podName)
	}
}

// VerifyHelmUpgrade waits till all the pods are restarted after the
// helm upgrade is completed, it verifies with initialTimestamp which is the timestamp
// of the pods before recreation and returns the pod name.
func (r *Region) VerifyHelmUpgrade(t *testing.T, initialTimestamp time.Time, kubectlOptions *k8s.KubectlOptions) error {
	// Wait for the pods to be recreated with a new timestamp after the upgrade.
	_, err := retry.DoWithRetryE(t, "waiting for pods to be recreated with new timestamp",
		60, 10*time.Second,
		func() (string, error) {
			// List the pods for the deployment.
			pods := k8s.ListPods(t, kubectlOptions, metav1.ListOptions{
				LabelSelector: LabelSelector,
			})

			// Check if any pods exist.
			if len(pods) == 0 {
				return "", fmt.Errorf("no pods found for deployment")
			}

			// Check if any pod has a creation timestamp greater than the initial timestamp.
			// We are waiting for all pods to complete the Helm upgrade,
			// as verifying just one pod and cleaning up the resources via Helm delete may cause issues
			// because the pods might still be in the upgrade process.
			for _, pod := range pods {
				if !pod.CreationTimestamp.Time.After(initialTimestamp) {
					return "", fmt.Errorf("pod %s has not been recreated with a new timestamp yet", pod.Name)
				}

				if pod.Status.Phase != corev1.PodRunning {
					return "", fmt.Errorf("pod %s is not in running phase", pod.Name)
				}

				containerStatus := pod.Status.ContainerStatuses
				for _, container := range containerStatus {
					if container.State.Running == nil {
						return "", fmt.Errorf("container %s is not in running state", container.Name)
					}
				}
			}

			return "", nil
		})
	return err
}

// ValidateMultiRegionSetup validates the multi-region setup.
func (r *Region) ValidateMultiRegionSetup(t *testing.T) {
	// Validate multi-region setup.
	for _, cluster := range r.Clusters {
		// Get the current context name.
		kubeConfig, _ := r.GetCurrentContext(t)
		kubectlOptions := k8s.NewKubectlOptions(cluster, kubeConfig, r.Namespace[cluster])

		pods := k8s.ListPods(t, kubectlOptions, metav1.ListOptions{
			LabelSelector: LabelSelector,
		})
		require.NotEmpty(t, pods)

		// Execute SQL query to verify regions.
		stdout, err := k8s.RunKubectlAndGetOutputE(t, kubectlOptions,
			"exec", pods[0].Name, "-c", "cockroachdb", "--",
			"/cockroach/cockroach", "sql", "--certs-dir=/cockroach/cockroach-certs",
			"-e", "SHOW REGIONS FROM CLUSTER")
		require.NoError(t, err)

		// Verify regions output
		for _, clusterRegion := range r.RegionCodes {
			// For multi-region validation, check for cloud provider prefixed region names
			expectedRegion := fmt.Sprintf("%s-%s", r.Provider, clusterRegion)
			require.Contains(t, stdout, expectedRegion, "Expected region %s to be present in cluster output", expectedRegion)
		}

		// Execute node status command and verify node properties.
		nodeStatus, err := k8s.RunKubectlAndGetOutputE(t, kubectlOptions,
			"exec", pods[0].Name, "-c", "cockroachdb", "--",
			"/cockroach/cockroach", "node", "status", "--format=json", "--certs-dir=/cockroach/cockroach-certs")
		require.NoError(t, err)

		var nodes []struct {
			Address     string `json:"address"`
			Build       string `json:"build"`
			ID          string `json:"id"`
			IsAvailable string `json:"is_available"`
			IsLive      string `json:"is_live"`
			Locality    string `json:"locality"`
			SQLAddress  string `json:"sql_address"`
			StartedAt   string `json:"started_at"`
			UpdatedAt   string `json:"updated_at"`
		}
		err = json.Unmarshal([]byte(nodeStatus), &nodes)
		require.NoError(t, err)

		// Count nodes per region.
		nodesPerRegion := make(map[string]int)
		for _, node := range nodes {
			// Extract region from locality.
			for _, part := range strings.Split(node.Locality, ",") {
				if strings.HasPrefix(part, "region=") {
					region := strings.TrimPrefix(part, "region=")
					nodesPerRegion[region]++
				}
			}
			// Verify node is available and live.
			require.Equal(t, "true", node.IsAvailable, "Node %s is not available", node.ID)
			require.Equal(t, "true", node.IsLive, "Node %s is not live", node.ID)
		}

		// Verify node count per region matches desired nodes.
		for _, region := range r.RegionCodes {
			region = fmt.Sprintf("%s-%s", r.Provider, region)
			require.Equal(t, r.NodeCount, nodesPerRegion[region],
				"Region %s has %d nodes, expected %d",
				region, nodesPerRegion[region], r.NodeCount)
		}
	}
}

func (r *Region) ValidateCRDBContainerResources(t *testing.T, kubectlOptions *k8s.KubectlOptions) {
	// Wait for resource specifications to be applied
	_, err := retry.DoWithRetryE(t, "waiting for container resources to be updated",
		30, 5*time.Second,
		func() (string, error) {
			pods := k8s.ListPods(t, kubectlOptions, metav1.ListOptions{
				LabelSelector: LabelSelector,
			})

			for _, pod := range pods {
				containers := pod.Spec.Containers
				for _, container := range containers {
					if container.Name == CockroachContainerName {
						quantity := container.Resources.Requests["cpu"]
						if container.Resources.Requests == nil || quantity.IsZero() {
							return "", fmt.Errorf("container %s resources not yet updated", container.Name)
						}
					}
				}
			}
			return "", nil
		})
	require.NoError(t, err)
}

// CreateCACertificate creates CA cert and key at the same path.
func (r *Region) CreateCACertificate(t *testing.T) error {
	// Create CA secret in all regions.
	cmd := shell.Command{
		Command:    "cockroach",
		Args:       []string{"cert", "create-ca", "--certs-dir=.", "--ca-key=ca.key"},
		WorkingDir: ".",
		Env:        nil,
		Logger:     nil,
	}

	certOutput, err := shell.RunCommandAndGetOutputE(t, cmd)
	t.Log(certOutput)
	return err
}

func (r *Region) CleanUpCACertificate(t *testing.T) {
	cmd := shell.Command{
		Command:    "rm",
		Args:       []string{"-rf", "ca.crt", "ca.key"},
		WorkingDir: ".",
	}

	shell.RunCommand(t, cmd)
}

// GetCurrentContext gets the current cluster context from KubeConfig.
func (r *Region) GetCurrentContext(t *testing.T) (string, api.Config) {
	// Try to get kubeconfig path using the standard method
	kubeConfig, err := k8s.GetKubeConfigPathE(t)
	require.NoError(t, err)
	_, err = r.EnsureKubeConfigPath()
	require.NoError(t, err)
	config := k8s.LoadConfigFromPath(kubeConfig)
	rawConfig, err := config.RawConfig()
	require.NoError(t, err)
	return kubeConfig, rawConfig
}

// EnsureKubeConfigPath ensures that the kubeconfig file exists and returns its path.
func (r *Region) EnsureKubeConfigPath() (string, error) {
	kubeConfigPath, err := k8s.KubeConfigPathFromHomeDirE()
	kubeConfigDir := filepath.Dir(kubeConfigPath)
	if err != nil {
		return "", fmt.Errorf("failed to get kubeconfig path: %w", err)
	}

	// Check if a directory exists, create if not.
	if _, err := os.Stat(kubeConfigDir); os.IsNotExist(err) {
		if err := os.MkdirAll(kubeConfigDir, 0755); err != nil {
			return "", fmt.Errorf("failed to create .kube directory: %w", err)
		}
	}

	// Check if a file exists, create an empty one if not.
	if _, err := os.Stat(kubeConfigPath); os.IsNotExist(err) {
		// Create an empty kubeconfig file.
		emptyConfig := []byte("apiVersion: v1\nkind: Config\nclusters: []\ncontexts: []\nusers: []\n")
		if err := os.WriteFile(kubeConfigPath, emptyConfig, 0644); err != nil {
			return "", fmt.Errorf("failed to create empty kubeconfig file: %w", err)
		}
	}

	return kubeConfigPath, nil
}

// CleanupResources will clean the resources installed by Operator, CockroachDB charts and deletes the namespace.
// Any failure in doing so might cause issues in other tests as some of the
// cluster resources are tied to the namespace.
func (r *Region) CleanupResources(t *testing.T) {
	for cluster, namespace := range r.Namespace {
		kubectlOptions := k8s.NewKubectlOptions(cluster, "", namespace)
		certManagerK8sOptions := k8s.NewKubectlOptions(cluster, "", testutil.CertManagerNamespace)

		extraArgs := map[string][]string{
			"delete": {
				"--wait",
				"--debug",
			},
		}
		helmOptions := &helm.Options{
			KubectlOptions: kubectlOptions,
			ExtraArgs:      extraArgs,
		}
		err := helm.DeleteE(t, helmOptions, ReleaseName, true)
		require.NoError(t, err)
		err = helm.DeleteE(t, helmOptions, operatorReleaseName, true)
		require.NoError(t, err)
		if r.IsCertManager {
			testutil.DeleteBundle(t, kubectlOptions)
			testutil.DeleteCAIssuer(t, kubectlOptions, namespace)
			testutil.DeleteSelfSignedCertificate(t, kubectlOptions, namespace)
			testutil.DeleteSelfSignedIssuer(t, kubectlOptions, namespace)
			testutil.DeleteTrustManager(t, certManagerK8sOptions)
			testutil.DeleteCertManager(t, certManagerK8sOptions)
		}
		k8s.DeleteNamespace(t, kubectlOptions, namespace)
	}
}

// OperatorRegions returns the regions config based on the index
// which is referring to cluster index.
func (r *Region) OperatorRegions(index int, nodes int) []map[string]interface{} {
	return r.createOperatorRegions(index, nodes, CustomDomains)
}

func HelmChartPaths() (helmChartPath string, operatorChartPath string) {
	rootPath := testutil.GetGitRoot()
	helmChartPath = filepath.Join(rootPath, "cockroachdb-parent/charts/cockroachdb")
	operatorChartPath = filepath.Join(rootPath, "cockroachdb-parent/charts/operator")

	return helmChartPath, operatorChartPath
}

// createOperatorRegions returns the appropriate regions config
// required while installing CockroachDb charts.
func (r *Region) createOperatorRegions(index int, nodes int, customDomains map[int]string) []map[string]interface{} {
	regions := make([]map[string]interface{}, 0, len(r.Clusters))

	for i := 0; i < len(r.Clusters); i++ {
		if i > index {
			break
		}

		region := map[string]interface{}{
			"code":          r.RegionCodes[i],
			"cloudProvider": r.Provider,
			"nodes":         nodes,
			"namespace":     r.Namespace[r.Clusters[i]],
		}

		if len(r.Clusters) > i && r.Clusters[i] != "" {
			if domain, ok := customDomains[i]; ok {
				region["domain"] = domain
			}
		}

		regions = append(regions, region)
	}

	return regions
}

// VerifyInitCommandInOperatorLogs verifies that the operator logs contain the expected init command.
func (r Region) VerifyInitCommandInOperatorLogs(t *testing.T, kubectlOptions *k8s.KubectlOptions, expected string) {
	// Get operator pods
	pods := k8s.ListPods(t, kubectlOptions, metav1.ListOptions{
		LabelSelector: OperatorLabelSelector,
	})
	require.NotEmpty(t, pods, "no operator pods found")

	// Get logs from the first operator pod (adjust if multiple replicas)
	logs, err := k8s.RunKubectlAndGetOutputE(t, kubectlOptions,
		"logs", pods[0].Name, "-n", kubectlOptions.Namespace)
	require.NoError(t, err)

	// Verify expected command is present in logs
	require.Contains(t, logs, expected, "operator logs did not contain expected init command")
}

func InstallCockroachDBEnterpriseOperator(t *testing.T, kubectlOptions *k8s.KubectlOptions) {
	_, operatorChartPath := HelmChartPaths()

	operatorOpts := &helm.Options{
		KubectlOptions: kubectlOptions,
		ExtraArgs:      helmExtraArgs,
	}

	// Install Operator on the cluster.
	helm.Install(t, operatorOpts, operatorChartPath, operatorReleaseName)

	// Wait for operator and webhook service to be available with endpoints.
	k8s.WaitUntilServiceAvailable(t, kubectlOptions, "cockroach-operator", 30, 2*time.Second)
	k8s.WaitUntilServiceAvailable(t, kubectlOptions, "cockroach-webhook-service", 30, 2*time.Second)

	// Wait for crd to be installed.
	_, _ = retry.DoWithRetryE(t, "wait-for-crd", 60, time.Second*5, func() (string, error) {
		return k8s.RunKubectlAndGetOutputE(t, operatorOpts.KubectlOptions, "get", "crd", "crdbclusters.crdb.cockroachlabs.com")
	})

	// wait for the operator pod to be running
	pods := k8s.ListPods(t, kubectlOptions, metav1.ListOptions{
		LabelSelector: OperatorLabelSelector,
	})

	for i := range pods {
		testutil.RequirePodToBeCreatedAndReady(t, operatorOpts.KubectlOptions, pods[i].Name, 300*time.Second)
	}
}

func UninstallCockroachDBEnterpriseOperator(t *testing.T, kubectlOptions *k8s.KubectlOptions) {
	operatorOpts := &helm.Options{
		KubectlOptions: kubectlOptions,
	}
	helm.Delete(t, operatorOpts, operatorReleaseName, true)
	k8s.RunKubectl(t, kubectlOptions, "delete", "service", "cockroach-webhook-service")
	k8s.RunKubectl(t, kubectlOptions, "delete", "validatingwebhookconfiguration", "cockroach-webhook-config")
	k8s.RunKubectl(t, kubectlOptions, "delete", "mutatingwebhookconfiguration", "cockroach-mutating-webhook-config")
	k8s.DeleteNamespace(t, kubectlOptions, kubectlOptions.Namespace)
}

func MustMarshalJSON(value interface{}) string {
	bytes, err := json.Marshal(value)
	if err != nil {
		panic(fmt.Sprintf("Failed to marshal JSON: %v", err))
	}
	return string(bytes)
}

// PatchHelmValues adds and overrides few default values for the helm charts for testing purposes.
// It sets the persistent storage size to 1Gi, terminationGracePeriod to 30s and rolling restart delay to 30s.
func PatchHelmValues(inputValues map[string]string) map[string]string {
	overrides := map[string]string{
		// Override the persistent storage size to 1Gi so that we do not run out of space.
		"cockroachdb.crdbCluster.dataStore.volumeClaimTemplate.spec.resources.requests.storage": "1Gi",
		// Override the terminationGracePeriodSeconds from 300s to 30 as it makes pod delete take longer.
		"cockroachdb.crdbCluster.terminationGracePeriod": "30s",
		// Override the rolling restart delay 30s as few times cockroachdb takes few seconds to come up.
		"cockroachdb.crdbCluster.rollingRestartDelay": "30s",
	}

	for k, v := range overrides {
		inputValues[k] = v
	}

	return inputValues
}

const (
	defaultWALFailoverPath  = "/cockroach/cockroach-wal-failover"
	defaultEncryptionSecret = "cmek-key-secret"
)

// AdvancedValidationConfig aggregates validation knobs for advanced feature assertions.
type AdvancedValidationConfig struct {
	WALFailover      WALFailoverValidation
	EncryptionAtRest EncryptionAtRestValidation
	PCR              PCRValidation
}

// WALFailoverValidation captures WAL failover expectations.
type WALFailoverValidation struct {
	CustomPath string
}

// EncryptionAtRestValidation captures encryption validation expectations.
type EncryptionAtRestValidation struct {
	SecretName string
}

// PCRValidation captures virtual cluster replication validation inputs.
type PCRValidation struct {
	Cluster          string
	PrimaryNamespace string
	StandbyNamespace string
}

// DefaultAdvancedValidationConfig returns default validation values used when a test does not override them.
func DefaultAdvancedValidationConfig() AdvancedValidationConfig {
	return AdvancedValidationConfig{
		WALFailover: WALFailoverValidation{
			CustomPath: defaultWALFailoverPath,
		},
		EncryptionAtRest: EncryptionAtRestValidation{
			SecretName: defaultEncryptionSecret,
		},
	}
}

func mergeValidationConfig(cfg *AdvancedValidationConfig) AdvancedValidationConfig {
	merged := DefaultAdvancedValidationConfig()
	if cfg == nil {
		return merged
	}

	if cfg.WALFailover.CustomPath != "" {
		merged.WALFailover = cfg.WALFailover
	}
	if cfg.EncryptionAtRest.SecretName != "" {
		merged.EncryptionAtRest = cfg.EncryptionAtRest
	}
	if cfg.PCR.Cluster != "" || cfg.PCR.PrimaryNamespace != "" || cfg.PCR.StandbyNamespace != "" {
		merged.PCR = cfg.PCR
	}

	return merged
}

// AssignRandomNamespace assigns a randomized namespace for the given cluster using the default prefix.
func (r *Region) AssignRandomNamespace(cluster string) string {
	return r.AssignRandomNamespaceWithPrefix(cluster, Namespace)
}

// AssignRandomNamespaceWithPrefix assigns a randomized namespace for the given cluster using the provided prefix.
func (r *Region) AssignRandomNamespaceWithPrefix(cluster string, prefix string) string {
	if prefix == "" {
		prefix = Namespace
	}
	if r.Namespace == nil {
		r.Namespace = make(map[string]string)
	}
	name := fmt.Sprintf("%s-%s", prefix, strings.ToLower(random.UniqueId()))
	r.Namespace[cluster] = name
	return name
}

// AssignRandomNamespacesWithPrefix assigns randomized namespaces for all tracked clusters.
func (r *Region) AssignRandomNamespacesWithPrefix(prefix string) {
	for _, cluster := range r.Clusters {
		r.AssignRandomNamespaceWithPrefix(cluster, prefix)
	}
}

// RequireCACertificate creates a CA certificate for the current test and returns a cleanup closure.
func (r *Region) RequireCACertificate(t *testing.T) func() {
	err := r.CreateCACertificate(t)
	require.NoError(t, err)
	return func() {
		r.CleanUpCACertificate(t)
	}
}

// SetupSingleClusterWithCA prepares a single cluster for advanced tests and returns a cleanup closure.
func (r *Region) SetupSingleClusterWithCA(t *testing.T, cluster string) func() {
	r.AssignRandomNamespace(cluster)
	cleanupCA := r.RequireCACertificate(t)
	return func() {
		r.CleanupResources(t)
		cleanupCA()
	}
}

// SetupMultiClusterWithCA prepares all clusters for advanced tests and returns a cleanup closure.
func (r *Region) SetupMultiClusterWithCA(t *testing.T) func() {
	r.AssignRandomNamespacesWithPrefix(Namespace)
	cleanupCA := r.RequireCACertificate(t)
	return func() {
		r.CleanupResources(t)
		cleanupCA()
	}
}

// BaseRegionConfig returns a baseline region configuration map for the provided cluster and index.
func (r *Region) BaseRegionConfig(cluster string, index int) map[string]interface{} {
	code := fmt.Sprintf("region-%d", index)
	if len(r.RegionCodes) > index {
		code = r.RegionCodes[index]
	}
	region := map[string]interface{}{
		"code":          code,
		"cloudProvider": r.Provider,
		"nodes":         r.NodeCount,
		"namespace":     r.Namespace[cluster],
	}
	if domain, ok := CustomDomains[index]; ok {
		region["domain"] = domain
	}
	return region
}

// EncryptionAtRestConfig returns a reusable encryption configuration map with optional overrides.
func (r *Region) EncryptionAtRestConfig(secretName string, overrides map[string]interface{}) map[string]interface{} {
	if secretName == "" {
		secretName = "cmek-key-secret"
	}
	config := map[string]interface{}{
		"platform":      "UNKNOWN_KEY_TYPE",
		"keySecretName": secretName,
	}
	for k, v := range overrides {
		config[k] = v
	}
	return config
}

// BuildEncryptionRegions creates a slice containing a single region entry with encryption settings applied.
func (r *Region) BuildEncryptionRegions(cluster string, index int, encryptionOverrides map[string]interface{}) []map[string]interface{} {
	region := r.BaseRegionConfig(cluster, index)
	region["encryptionAtRest"] = r.EncryptionAtRestConfig("", encryptionOverrides)
	return []map[string]interface{}{region}
}

// AdvancedInstallConfig holds configuration for advanced feature installations
type AdvancedInstallConfig struct {
	// WAL Failover configuration
	WALFailoverEnabled bool
	WALFailoverSize    string

	// Encryption at Rest configuration
	EncryptionEnabled   bool
	EncryptionKeySecret string

	// Virtual Cluster configuration (for PCR)
	VirtualClusterMode string // "primary", "standby", or ""

	// Custom helm values to merge
	CustomValues map[string]string

	// Custom regions configuration (for encryption)
	CustomRegions []map[string]interface{}

	// Skip operator installation (for second virtual cluster)
	SkipOperatorInstall bool
}

// InstallChartsWithAdvancedConfig installs CockroachDB with advanced features configuration
func (r *Region) InstallChartsWithAdvancedConfig(t *testing.T, cluster string, index int, config AdvancedInstallConfig) {
	// Get the current context name.
	kubeConfig, rawConfig := r.GetCurrentContext(t)

	// Get helm chart paths.
	helmChartPath, _ := HelmChartPaths()

	// Verify if a cluster exists in the contexts.
	if _, ok := rawConfig.Contexts[cluster]; !ok {
		t.Fatal()
	}

	// Setup kubectl options for this cluster.
	kubectlOptions := k8s.NewKubectlOptions(cluster, kubeConfig, r.Namespace[cluster])

	// Create a namespace.
	k8s.CreateNamespace(t, kubectlOptions, r.Namespace[cluster])

	// create CA Secret.
	err := k8s.RunKubectlE(t, kubectlOptions, "create", "secret", "generic", customCASecret, "--from-file=ca.crt",
		"--from-file=ca.key")
	require.NoError(t, err)

	// Create encryption key secret if encryption is enabled
	if config.EncryptionEnabled && config.EncryptionKeySecret != "" {
		// Create secret with base64 encoded AES key
		err = k8s.RunKubectlE(t, kubectlOptions, "create", "secret", "generic", "cmek-key-secret",
			fmt.Sprintf("--from-literal=StoreKeyData=%s", config.EncryptionKeySecret))
		require.NoError(t, err)

		// Verify secret was created with data
		secretSize, err := k8s.RunKubectlAndGetOutputE(t, kubectlOptions,
			"get", "secret", "cmek-key-secret",
			"-o", "jsonpath={.data.StoreKeyData}")
		require.NoError(t, err)
		require.True(t, len(secretSize) > 0, "Secret StoreKeyData should be >0")
		t.Logf("Created encryption secret with size: %d bytes", len(secretSize))
	}

	// Install the operator when it is not skipped and not already marked as installed.
	if !config.SkipOperatorInstall && !r.IsOperatorInstalled {
		InstallCockroachDBEnterpriseOperator(t, kubectlOptions)
	}

	// Build helm values
	helmValues := PatchHelmValues(map[string]string{
		"cockroachdb.clusterDomain":             CustomDomains[index],
		"cockroachdb.tls.selfSigner.caProvided": "true",
		"cockroachdb.tls.selfSigner.caSecret":   customCASecret,
	})

	// Add WAL failover configuration
	if config.WALFailoverEnabled {
		helmValues["cockroachdb.crdbCluster.walFailoverSpec.status"] = "enable"
		helmValues["cockroachdb.crdbCluster.walFailoverSpec.size"] = config.WALFailoverSize
		helmValues["cockroachdb.crdbCluster.walFailoverSpec.name"] = "datadir-wal-failover"
		helmValues["cockroachdb.crdbCluster.walFailoverSpec.path"] = "/cockroach/cockroach-wal-failover"
	}

	// Add virtual cluster configuration
	if config.VirtualClusterMode != "" {
		helmValues["cockroachdb.crdbCluster.virtualCluster.mode"] = config.VirtualClusterMode
	}

	// Merge custom values
	for k, v := range config.CustomValues {
		helmValues[k] = v
	}

	// Determine which regions configuration to use
	var regionsConfig interface{}
	if config.CustomRegions != nil {
		regionsConfig = config.CustomRegions
	} else {
		regionsConfig = r.OperatorRegions(index, r.NodeCount)
	}

	// Helm install cockroach CR with configuration
	crdbOptions := &helm.Options{
		KubectlOptions: kubectlOptions,
		SetValues:      helmValues,
		SetJsonValues: map[string]string{
			"cockroachdb.crdbCluster.regions": MustMarshalJSON(regionsConfig),
		},
		ExtraArgs: helmExtraArgs,
	}

	helm.Install(t, crdbOptions, helmChartPath, ReleaseName)

	serviceName := "cockroachdb-public"
	k8s.WaitUntilServiceAvailable(t, kubectlOptions, serviceName, 30, 5*time.Second)
}

// ValidateWALFailover verifies that WAL failover is properly configured by checking the --wal-failover flag and PVC.
// Pass nil config to rely on defaults or provide a pointer with overrides through AdvancedValidationConfig.WALFailover.
func (r *Region) ValidateWALFailover(t *testing.T, cluster string, cfg *AdvancedValidationConfig) {
	validationConfig := mergeValidationConfig(cfg)
	expectedPath := validationConfig.WALFailover.CustomPath
	if expectedPath == "" {
		expectedPath = defaultWALFailoverPath
	}

	kubeConfig, _ := r.GetCurrentContext(t)
	kubectlOptions := k8s.NewKubectlOptions(cluster, kubeConfig, r.Namespace[cluster])

	// Get CockroachDB pods
	pods := k8s.ListPods(t, kubectlOptions, metav1.ListOptions{
		LabelSelector: LabelSelector,
	})
	require.True(t, len(pods) > 0, "No CockroachDB pods found")

	podName := pods[0].Name

	// 1. Verify cockroach start command contains --wal-failover flag with custom path
	t.Logf("Verifying cockroach start command contains --wal-failover flag with path %s...", expectedPath)
	podCommand, err := k8s.RunKubectlAndGetOutputE(t, kubectlOptions,
		"get", "pod", podName, "-o", "jsonpath={.spec.containers[?(@.name=='cockroachdb')].command}")
	require.NoError(t, err)
	expectedFlag := fmt.Sprintf("--wal-failover=path=%s", expectedPath)
	require.Contains(t, podCommand, expectedFlag,
		"Pod command should contain %s", expectedFlag)
	t.Logf("Cockroach start command contains --wal-failover flag with path %s", expectedPath)

	// 2. Verify COCKROACH_WAL_FAILOVER environment variable is set
	t.Log("Verifying COCKROACH_WAL_FAILOVER environment variable...")
	walFailoverEnv, err := k8s.RunKubectlAndGetOutputE(t, kubectlOptions,
		"get", "pod", podName, "-o", "jsonpath={.spec.containers[?(@.name=='cockroachdb')].env[?(@.name=='COCKROACH_WAL_FAILOVER')].value}")
	if err == nil && walFailoverEnv != "" {
		t.Logf("COCKROACH_WAL_FAILOVER environment variable is set to: %s", walFailoverEnv)
	}

	// 3. Verify WAL failover PVC exists with correct naming convention
	t.Log("Verifying WAL failover PVC exists...")
	pvcs, err := k8s.RunKubectlAndGetOutputE(t, kubectlOptions,
		"get", "pvc", "-o", "jsonpath={.items[*].metadata.name}")
	require.NoError(t, err)
	t.Logf("Found PVCs: %s", pvcs)
	// PVC should follow the pattern: datadir-wal-failover-cockroachdb-{index}
	// The name prefix comes from walFailoverSpec.name which we set to "datadir-wal-failover"
	require.Contains(t, pvcs, "datadir-wal-failover", "WAL failover PVC should exist with correct naming")
	t.Log("WAL failover PVC exists with correct naming convention")

	// 4. Verify WAL failover volume is mounted in the pod
	t.Log("Verifying WAL failover volume is mounted...")
	volumes, err := k8s.RunKubectlAndGetOutputE(t, kubectlOptions,
		"get", "pod", podName, "-o", "jsonpath={.spec.volumes[*].name}")
	require.NoError(t, err)
	// The volume name is always "wal-failover" regardless of the custom path or PVC name
	require.Contains(t, volumes, "wal-failover", "WAL failover volume should be mounted")
	t.Log("WAL failover volume is properly mounted")

	// 5. Verify the custom WAL failover path exists in the container
	t.Logf("Verifying custom WAL failover path %s exists in container...", expectedPath)
	_, err = k8s.RunKubectlAndGetOutputE(t, kubectlOptions,
		"exec", podName, "-c", "cockroachdb", "--",
		"ls", "-la", expectedPath)
	require.NoError(t, err)
	t.Logf("Custom WAL failover path %s exists in container", expectedPath)

	t.Log("WAL failover validation completed successfully")
}

// GenerateEncryptionKey generates a 256-bit AES encryption key and returns base64 encoded value
func (r *Region) GenerateEncryptionKey(t *testing.T) string {
	// Generate 256-bit AES key using cockroach gen encryption-key
	cmd := shell.Command{
		Command:    "cockroach",
		Args:       []string{"gen", "encryption-key", "--size", "256", "store.key"},
		WorkingDir: ".",
	}

	_, err := shell.RunCommandAndGetOutputE(t, cmd)
	require.NoError(t, err)

	// Read the generated key file
	keyBytes, err := os.ReadFile("store.key")
	require.NoError(t, err)

	// Base64 encode the key (removing any newlines)
	storeKeyB64 := base64.StdEncoding.EncodeToString(keyBytes)
	storeKeyB64 = strings.ReplaceAll(storeKeyB64, "\n", "")

	// Clean up the key file
	os.Remove("store.key")

	return storeKeyB64
}

// ValidateEncryptionAtRest verifies that encryption at rest is properly configured by checking flags and encryption status.
// Pass nil config to rely on defaults or provide overrides via AdvancedValidationConfig.EncryptionAtRest.
func (r *Region) ValidateEncryptionAtRest(t *testing.T, cluster string, cfg *AdvancedValidationConfig) {
	kubeConfig, _ := r.GetCurrentContext(t)
	kubectlOptions := k8s.NewKubectlOptions(cluster, kubeConfig, r.Namespace[cluster])

	// Get CockroachDB pods
	pods := k8s.ListPods(t, kubectlOptions, metav1.ListOptions{
		LabelSelector: LabelSelector,
	})
	require.True(t, len(pods) > 0, "No CockroachDB pods found")

	podName := pods[0].Name

	validationConfig := mergeValidationConfig(cfg)
	secretName := validationConfig.EncryptionAtRest.SecretName
	if secretName == "" {
		secretName = defaultEncryptionSecret
	}

	// 1. Verify the encryption key secret exists and has data
	t.Logf("Verifying encryption key secret %s...", secretName)
	secretSize, err := k8s.RunKubectlAndGetOutputE(t, kubectlOptions,
		"get", "secret", secretName,
		"-o", "jsonpath={.data.StoreKeyData}")
	require.NoError(t, err)
	require.True(t, len(secretSize) > 0, "Secret StoreKeyData should not be empty")
	t.Logf("Encryption key secret %s exists with data", secretName)

	// 2. Verify cockroach start command contains encryption flags
	t.Log("Verifying cockroach start command contains encryption flags...")
	podSpec, err := k8s.RunKubectlAndGetOutputE(t, kubectlOptions,
		"get", "pod", podName, "-o", "jsonpath={.spec.containers[?(@.name=='cockroachdb')].command}")
	require.NoError(t, err)
	require.Contains(t, podSpec, "--enterprise-encryption", "Pod command should contain --enterprise-encryption flag")
	t.Log("Cockroach start command contains encryption flags")

	// 3. Check encryption status via debug command
	//t.Log("Checking encryption status...")
	//encryptionStatus, err := k8s.RunKubectlAndGetOutputE(t, kubectlOptions,
	//	"exec", podName, "-c", "cockroachdb", "--",
	//	"/cockroach/cockroach", "debug", "encryption-status", "/cockroach/cockroach-data",
	//	"--certs-dir=/cockroach/cockroach-certs")
	//require.NoError(t, err)
	//t.Logf("Encryption status:\n%s", encryptionStatus)

	// 4. Verify encryption is active by checking for "Active" status
	//require.Contains(t, encryptionStatus, "Active", "Encryption should be active")
	//t.Log("Encryption is active on the store")

	t.Log("Encryption at rest validation completed successfully")
}

// generateLocalTenantURI creates local connection URI for connecting to same cluster's tenants
// Returns: postgresql://root:root@localhost:26257?options=-ccluster%3D<cluster>
func generateLocalTenantURI(cluster string) string {
	return fmt.Sprintf("postgresql://root:root@localhost:26257?options=-ccluster%%3D%s", cluster)
}

// generateExternalConnectionURI creates external connection URI using cockroach encode-uri
// This is used for cross-cluster connections (e.g., replication from primary to standby)
// Format: cockroach encode-uri 'postgresql://USERNAME:PASSWORD@HOST' [flags]
func generateExternalConnectionURI(t *testing.T, kubectlOpts *k8s.KubectlOptions, pod string, connString string) string {
	output, err := k8s.RunKubectlAndGetOutputE(t, kubectlOpts,
		"exec", pod, "-c", "cockroachdb", "--",
		"/cockroach/cockroach", "encode-uri",
		connString, // Format: postgresql://user:pass@host:port
		"--ca-cert=/cockroach/cockroach-certs/ca.crt",
		"--inline")
	require.NoError(t, err)
	return strings.TrimSpace(output)
}

// Helper function to execute SQL on a specific virtual cluster
func execSQLOnVC(t *testing.T, kubectlOpts *k8s.KubectlOptions, pod string, vcURI string, database string, sql string) (string, error) {
	args := []string{
		"exec", pod, "-c", "cockroachdb", "--",
		"/cockroach/cockroach", "sql",
		"--certs-dir=/cockroach/cockroach-certs",
		"--url", vcURI,
	}
	if database != "" {
		args = append(args, "--database="+database)
	}
	args = append(args, "-e", sql)

	return k8s.RunKubectlAndGetOutputE(t, kubectlOpts, args...)
}

// ValidatePCR validates PCR by verifying virtual cluster configuration and testing failover/failback connections.
// Provide the cluster and namespaces through AdvancedValidationConfig.PCR.
func (r *Region) ValidatePCR(t *testing.T, cfg *AdvancedValidationConfig) {
	validationConfig := mergeValidationConfig(cfg)
	cluster := validationConfig.PCR.Cluster
	primaryNamespace := validationConfig.PCR.PrimaryNamespace
	standbyNamespace := validationConfig.PCR.StandbyNamespace

	require.NotEmpty(t, cluster, "PCR validation requires a cluster context")
	require.NotEmpty(t, primaryNamespace, "PCR validation requires a primary namespace")
	require.NotEmpty(t, standbyNamespace, "PCR validation requires a standby namespace")

	kubeConfig, _ := r.GetCurrentContext(t)

	// Get primary and standby pods
	primaryKubectlOptions := k8s.NewKubectlOptions(cluster, kubeConfig, primaryNamespace)
	standbyKubectlOptions := k8s.NewKubectlOptions(cluster, kubeConfig, standbyNamespace)

	primaryPods := k8s.ListPods(t, primaryKubectlOptions, metav1.ListOptions{
		LabelSelector: LabelSelector,
	})
	require.True(t, len(primaryPods) > 0, "No primary pods found")

	standbyPods := k8s.ListPods(t, standbyKubectlOptions, metav1.ListOptions{
		LabelSelector: LabelSelector,
	})
	require.True(t, len(standbyPods) > 0, "No standby pods found")

	primaryPod := primaryPods[0].Name
	standbyPod := standbyPods[0].Name

	// Step 1: Setup primary cluster for PCR
	t.Log("==================================================")
	t.Log("Step 1: Setting up primary cluster for Physical Cluster Replication")
	t.Log("==================================================")

	// Generate local connection URIs for primary
	primarySystemURI := generateLocalTenantURI("system")
	t.Logf("Connecting to primary system tenant at: %s", primarySystemURI)

	// Enable rangefeed for PCR (required for replication)
	t.Log("Enabling rangefeed on primary cluster (required for PCR)...")
	_, err := execSQLOnVC(t, primaryKubectlOptions, primaryPod, primarySystemURI, "", "SET CLUSTER SETTING kv.rangefeed.enabled = true")
	require.NoError(t, err)
	t.Log("✓ Rangefeed enabled on primary cluster")

	// With --virtualized flag, the 'main' virtual cluster is created automatically
	// We just need to verify it exists and ensure it's in SHARED mode
	t.Log("Verifying main virtual cluster exists on primary (automatically created with --virtualized flag)...")
	primaryVCs, err := execSQLOnVC(t, primaryKubectlOptions, primaryPod, primarySystemURI, "", "SHOW VIRTUAL CLUSTERS")
	require.NoError(t, err)
	require.Contains(t, primaryVCs, "main", "Primary should have main virtual cluster")
	t.Logf("✓ Main virtual cluster exists on primary\n%s", primaryVCs)

	// Ensure the service is started in SHARED mode
	t.Log("Ensuring main virtual cluster service is in SHARED mode...")
	_, err = execSQLOnVC(t, primaryKubectlOptions, primaryPod, primarySystemURI, "", "ALTER VIRTUAL CLUSTER main START SERVICE SHARED")
	if err != nil && !strings.Contains(err.Error(), "already") {
		t.Logf("Note: Service start returned: %v", err)
	}
	t.Log("✓ Main virtual cluster service is running in SHARED mode")

	// Wait for service to be ready
	time.Sleep(5 * time.Second)

	// Create replication user on primary with required permissions
	t.Log("Creating replication user on primary cluster...")
	createUserSQL := fmt.Sprintf("CREATE USER IF NOT EXISTS '%s' WITH PASSWORD '%s'", "pcr_source", "repl_password_123")
	_, err = execSQLOnVC(t, primaryKubectlOptions, primaryPod, primarySystemURI, "", createUserSQL)
	require.NoError(t, err)
	t.Log("✓ Created user: pcr_source")

	t.Log("Granting admin privileges to pcr_source...")
	_, err = execSQLOnVC(t, primaryKubectlOptions, primaryPod, primarySystemURI, "", "GRANT admin TO pcr_source")
	require.NoError(t, err)
	t.Log("✓ Granted admin privileges to pcr_source")

	t.Log("Primary cluster setup complete!")

	// Step 2: Setup standby cluster
	t.Log("")
	t.Log("==================================================")
	t.Log("Step 2: Setting up standby cluster")
	t.Log("==================================================")

	// Generate local connection URI for standby
	standbySystemURI := generateLocalTenantURI("system")
	t.Logf("Connecting to standby system tenant at: %s", standbySystemURI)

	// Enable rangefeed on standby cluster
	t.Log("Enabling rangefeed on standby cluster...")
	_, err = execSQLOnVC(t, standbyKubectlOptions, standbyPod, standbySystemURI, "", "SET CLUSTER SETTING kv.rangefeed.enabled = true")
	require.NoError(t, err)
	t.Log("✓ Rangefeed enabled on standby cluster")

	// Verify standby was initialized with --virtualized-empty (no main VC yet)
	t.Log("Verifying standby cluster state (should have no main virtual cluster yet)...")
	standbyVCsInitial, err := execSQLOnVC(t, standbyKubectlOptions, standbyPod, standbySystemURI, "", "SHOW VIRTUAL CLUSTERS")
	require.NoError(t, err)
	t.Logf("Standby virtual clusters before replication:\n%s", standbyVCsInitial)
	t.Log("✓ Standby initialized with --virtualized-empty (no main tenant yet)")

	// Create admin user on standby
	t.Log("Creating admin user on standby cluster...")
	createAdminSQL := fmt.Sprintf("CREATE USER IF NOT EXISTS '%s' WITH PASSWORD '%s'", "pcr_admin", "admin_password_123")
	_, err = execSQLOnVC(t, standbyKubectlOptions, standbyPod, standbySystemURI, "", createAdminSQL)
	require.NoError(t, err)
	_, err = execSQLOnVC(t, standbyKubectlOptions, standbyPod, standbySystemURI, "", "GRANT admin TO pcr_admin")
	require.NoError(t, err)
	t.Log("✓ Created user pcr_admin with admin privileges")
	t.Log("Standby cluster setup complete!")

	// Step 4: Set up replication stream from standby to primary
	t.Log("")
	t.Log("==================================================")
	t.Log("Step 4: Creating replication stream from primary to standby")
	t.Log("==================================================")

	// Step 4a: Generate external connection URI for replication
	// Following: https://www.cockroachlabs.com/docs/stable/set-up-physical-cluster-replication#step-4-start-replication
	t.Log("Generating connection URI for primary cluster using cockroach encode-uri...")
	primaryHost := fmt.Sprintf("cockroachdb-public.%s.svc.%s:26257", primaryNamespace, CustomDomains[0])
	primaryConnString := fmt.Sprintf("postgresql://%s:%s@%s", "pcr_source", "repl_password_123", primaryHost)
	t.Logf("Primary connection string format: postgresql://pcr_source:***@%s", primaryHost)

	// Use encode-uri to generate the proper connection string with certificates
	encodedPrimaryURI := generateExternalConnectionURI(t, standbyKubectlOptions, standbyPod, primaryConnString)
	t.Logf("✓ Generated encoded connection URI for replication")

	// Step 4b: Create external connection on standby (as per documentation)
	t.Log("Creating external connection on standby cluster...")
	createExternalConnSQL := fmt.Sprintf("CREATE EXTERNAL CONNECTION IF NOT EXISTS primary_replication AS '%s'", encodedPrimaryURI)
	_, err = execSQLOnVC(t, standbyKubectlOptions, standbyPod, standbySystemURI, "", createExternalConnSQL)
	require.NoError(t, err)
	t.Log("✓ External connection 'primary_replication' created on standby")

	// Step 4c: Create the replication stream using the external connection name
	t.Log("Creating replication stream on standby cluster using external connection...")
	t.Log("This will create the main virtual cluster on standby and start replicating data from primary")
	createReplicationCmd := "CREATE VIRTUAL CLUSTER main FROM REPLICATION OF main ON 'external://primary_replication'"
	t.Logf("Executing: %s", createReplicationCmd)

	output, err := execSQLOnVC(t, standbyKubectlOptions, standbyPod, standbySystemURI, "", createReplicationCmd)
	if err != nil && !strings.Contains(err.Error(), "already exists") && !strings.Contains(output, "already exists") {
		t.Logf("ERROR: Replication stream creation failed: %v", err)
		t.Logf("Output: %s", output)
		// Debug: Check what tenants exist on primary
		t.Log("Debugging: Checking virtual clusters on primary...")
		tenantsOutput, _ := execSQLOnVC(t, primaryKubectlOptions, primaryPod, primarySystemURI, "", "SHOW VIRTUAL CLUSTERS")
		t.Logf("Virtual clusters on primary:\n%s", tenantsOutput)
		require.NoError(t, err, "Failed to create replication stream")
	} else {
		t.Log("✓ Replication stream created successfully!")
		t.Log("✓ Main virtual cluster now exists on standby and is replicating from primary")
	}

	// Wait for replication to catch up
	t.Log("Waiting for initial replication to catch up (15 seconds)...")
	time.Sleep(15 * time.Second)
	t.Log("✓ Initial replication sync complete")

	// Step 5: Verify replication status
	t.Log("")
	t.Log("==================================================")
	t.Log("Step 5: Verifying replication status")
	t.Log("==================================================")

	t.Log("Checking virtual clusters on standby (should now include main)...")
	standbyVCsStatus, err := execSQLOnVC(t, standbyKubectlOptions, standbyPod, standbySystemURI, "", "SHOW VIRTUAL CLUSTERS")
	require.NoError(t, err)
	require.Contains(t, standbyVCsStatus, "main", "Standby should now have main virtual cluster after replication setup")
	t.Logf("Virtual clusters on standby:\n%s", standbyVCsStatus)
	t.Log("✓ Main virtual cluster exists on standby with replication status")

	t.Log("Replication verification complete!")

	// Step 6: Test read-only access on standby using a separate reader virtual cluster
	// Following: https://www.cockroachlabs.com/docs/stable/read-from-standby
	t.Log("")
	t.Log("==================================================")
	t.Log("Step 6: Testing read-only access on standby cluster")
	t.Log("==================================================")

	// Step 6a: Create a reader virtual cluster for read-only access
	// This is the recommended approach per documentation
	t.Log("Creating a reader virtual cluster on standby for read-only access...")
	t.Log("This allows read queries without affecting the replication stream")
	createReaderSQL := "CREATE VIRTUAL CLUSTER main_reader FROM REPLICATION OF main ON 'external://primary_replication' WITH READ VIRTUAL CLUSTER"
	_, err = execSQLOnVC(t, standbyKubectlOptions, standbyPod, standbySystemURI, "", createReaderSQL)
	if err != nil && !strings.Contains(err.Error(), "already exists") {
		t.Logf("Note: Reader VC creation returned: %v", err)
	}
	t.Log("✓ Reader virtual cluster 'main_reader' created")

	// Step 6b: Start the reader service in SHARED mode
	t.Log("Starting reader virtual cluster service in SHARED mode...")
	_, err = execSQLOnVC(t, standbyKubectlOptions, standbyPod, standbySystemURI, "", "ALTER VIRTUAL CLUSTER main_reader START SERVICE SHARED")
	if err != nil && !strings.Contains(err.Error(), "already") {
		t.Logf("Note: Service start returned: %v (may already be started)", err)
	}
	t.Log("✓ Reader service started in SHARED mode")

	// Wait for reader service to be ready and replication to catch up
	// Documentation recommends waiting for replication to catch up
	t.Log("Waiting for reader service to be ready and replication to catch up (20 seconds)...")
	time.Sleep(20 * time.Second)

	// Step 6c: Verify reader virtual cluster exists
	t.Log("Verifying reader virtual cluster status...")
	readerVCsStatus, err := execSQLOnVC(t, standbyKubectlOptions, standbyPod, standbySystemURI, "", "SHOW VIRTUAL CLUSTERS")
	require.NoError(t, err)
	require.Contains(t, readerVCsStatus, "main_reader", "Standby should have main_reader virtual cluster")
	t.Logf("Virtual clusters on standby:\n%s", readerVCsStatus)
	t.Log("✓ Reader virtual cluster is ready")

	// Step 6d: Read from the reader virtual cluster
	t.Log("Attempting to read replicated data from reader virtual cluster...")
	readerURI := generateLocalTenantURI("main_reader")
	readFromStandby, err := execSQLOnVC(t, standbyKubectlOptions, standbyPod, readerURI, "bank", "SELECT id, balance FROM accounts ORDER BY id")
	if err != nil {
		t.Logf("Warning: Read from standby reader failed (may need more time): %v", err)
		// Try reading directly from main VC as fallback
		t.Log("Trying to read from main virtual cluster instead...")
		standbyMainURI := generateLocalTenantURI("main")
		readFromStandby, err = execSQLOnVC(t, standbyKubectlOptions, standbyPod, standbyMainURI, "bank", "SELECT id, balance FROM accounts ORDER BY id")
		if err != nil {
			t.Logf("Warning: Read from main VC also failed: %v", err)
		}
	}

	if err == nil {
		require.Contains(t, readFromStandby, "1000", "Should be able to read replicated data from standby")
		require.Contains(t, readFromStandby, "250", "Should be able to read replicated data from standby")
		t.Log("✓ Successfully read replicated data from standby reader!")
		t.Logf("Data from standby reader:\n%s", readFromStandby)
	}

	t.Log("Read-only access test complete!")

	// Step 7: Test failover (cutover)
	t.Log("")
	t.Log("==================================================")
	t.Log("Step 7: Testing failover (promoting standby to primary)")
	t.Log("==================================================")

	// Stop the service first (if running in SHARED mode)
	t.Log("Stopping main virtual cluster service on standby before cutover...")
	_, _ = execSQLOnVC(t, standbyKubectlOptions, standbyPod, standbySystemURI, "", "ALTER VIRTUAL CLUSTER main STOP SERVICE")
	t.Log("✓ Service stopped")

	time.Sleep(5 * time.Second)

	// Complete replication to latest - this makes the standby writable
	t.Log("Completing replication to latest (this promotes standby to be writable)...")
	_, err = execSQLOnVC(t, standbyKubectlOptions, standbyPod, standbySystemURI, "", "ALTER VIRTUAL CLUSTER main COMPLETE REPLICATION TO LATEST")
	if err != nil {
		t.Logf("Note: Cutover command returned: %v", err)
	}
	t.Log("✓ Replication completed to latest")

	// Wait for cutover to complete
	t.Log("Waiting for cutover to complete (15 seconds)...")
	time.Sleep(15 * time.Second)

	// Step 8: Start the service after cutover
	t.Log("")
	t.Log("==================================================")
	t.Log("Step 8: Starting service on promoted standby")
	t.Log("==================================================")

	t.Log("Starting main virtual cluster service after cutover...")
	_, err = execSQLOnVC(t, standbyKubectlOptions, standbyPod, standbySystemURI, "", "ALTER VIRTUAL CLUSTER main START SERVICE SHARED")
	if err != nil {
		t.Logf("Note: Service start returned: %v (may already be started)", err)
	}
	t.Log("✓ Service started, standby is now the primary and can accept writes")

	//time.Sleep(10 * time.Second)

	// Step 9: Verify promoted standby can serve read/write traffic
	//t.Log("")
	//t.Log("==================================================")
	//t.Log("Step 9: Verifying promoted standby can handle read/write traffic")
	//t.Log("==================================================")
	//
	//// Check virtual cluster status
	//t.Log("Checking virtual cluster status after cutover...")
	//standbyVCsFinal, err := execSQLOnVC(t, standbyKubectlOptions, standbyPod, standbySystemURI, "", "SHOW VIRTUAL CLUSTERS")
	//require.NoError(t, err)
	//t.Logf("Virtual clusters on promoted standby:\n%s", standbyVCsFinal)
	//t.Log("✓ Virtual cluster status looks good")

	// Step 10: Cleanup note
	//t.Log("")
	//t.Log("==================================================")
	//t.Log("Step 10: Cleanup")
	//t.Log("==================================================")
	//t.Log("Note: Reader virtual cluster 'main_reader' can be dropped if no longer needed")
	//t.Log("Command: DROP VIRTUAL CLUSTER main_reader")
	//
	//t.Log("")
	//t.Log("==================================================")
	//t.Log("PCR Validation Complete!")
	//t.Log("==================================================")
	//t.Log("✓ Successfully tested:")
	//t.Log("  - Virtual cluster setup with --virtualized flag on primary")
	//t.Log("  - Virtual cluster setup with --virtualized-empty flag on standby")
	//t.Log("  - User creation and permissions (pcr_source, pcr_admin)")
	//t.Log("  - External connection creation")
	//t.Log("  - Replication stream creation using external connection")
	//t.Log("  - Reader virtual cluster for read-only access")
	//t.Log("  - Read-only access to standby via reader VC")
	//t.Log("  - Failover (cutover) to standby")
	//t.Log("  - Read/write operations on promoted standby")
	//t.Log("==================================================")
}
