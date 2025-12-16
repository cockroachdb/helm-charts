package operator

import (
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
	rawConfig.CurrentContext = cluster

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
	kubeConfig, rawConfig := r.GetCurrentContext(t)
	rawConfig.CurrentContext = cluster
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
		kubeConfig, rawConfig := r.GetCurrentContext(t)
		rawConfig.CurrentContext = cluster
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
func VerifyInitCommandInOperatorLogs(t *testing.T, kubectlOptions *k8s.KubectlOptions, expected string) {
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
		SetValues: map[string]string{
			"numReplicas": "1",
		},
		ExtraArgs: helmExtraArgs,
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
