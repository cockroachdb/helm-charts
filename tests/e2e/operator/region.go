package operator

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gruntwork-io/terratest/modules/helm"
	"github.com/gruntwork-io/terratest/modules/k8s"
	"github.com/gruntwork-io/terratest/modules/retry"
	"github.com/gruntwork-io/terratest/modules/shell"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apiextv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/clientcmd/api"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"

	"github.com/cockroachdb/errors"
	"github.com/cockroachdb/helm-charts/tests/e2e/calico"
	"github.com/cockroachdb/helm-charts/tests/e2e/coredns"
	"github.com/cockroachdb/helm-charts/tests/testutil"
)

const (
	CloudProvider = "k3d"
	TestDBName    = "testdb"
	Namespace     = "test-cockroach"
	LabelSelector = "app=cockroachdb"
)

var (
	RegionCodes   = []string{"us-east1", "us-east2"}
	Clusters      = []string{"k3d-chart-testing-cluster-0", "k3d-chart-testing-cluster-1"}
	CustomDomains = map[string]string{
		"k3d-chart-testing-cluster-0": "cluster1.local",
		"k3d-chart-testing-cluster-1": "cluster2.local",
	}

	operatorReleaseName = "cockroachdb-operator"
	customCASecret      = "cockroachdb-ca-secret"
	ReleaseName         = "cockroachdb"

	helmExtraArgs = map[string][]string{
		"install": {
			"--wait",
			"--debug",
		},
	}
)

type cockroachEnterpriseOperator interface {
	setUpInfra(t *testing.T, corednsClusterOptions map[string]coredns.CoreDNSClusterOption) map[string]client.Client
	installCharts(t *testing.T, cluster string, index int)
	validateCRDB(t *testing.T, cluster string, clients map[string]client.Client)
}

// OperatorUseCases defines use cases for the CockroachDB cluster.
type OperatorUseCases interface {
	TestHelmInstall(t *testing.T)
	TestHelmUpgrade(t *testing.T)
	TestClusterScaleUp(t *testing.T)
	TestClusterRollingRestart(t *testing.T)
	TestKillingCockroachNode(t *testing.T)
}

type Region struct {
	IsMultiRegion bool
	// NodeCount is the desired CockroachDB nodes in the region.
	NodeCount int
	// Namespace stores mapping between cluster name and namespace.
	Namespace    map[string]string
	ReusingInfra bool
	// Clients store the k8s client for each cluster
	// needed for performing k8s operations on k8s objects.
	Clients map[string]client.Client
	cockroachEnterpriseOperator
}

// SetUpInfra Creates K3d clusters, deploy calico CNI, deploy coredns in each cluster.
func (r *Region) SetUpInfra(t *testing.T, corednsClusterOptions map[string]coredns.CoreDNSClusterOption) {

	// If using existing infra return clients.
	if r.ReusingInfra {
		return
	}

	var clients = make(map[string]client.Client)
	// Get current context name.
	kubeConfig, rawConfig := r.GetCurrentContext(t)

	for i, cluster := range Clusters {
		if _, ok := rawConfig.Contexts[cluster]; !ok {
			// Create cluster using shell command.
			err := createK3DCluster(t)
			require.NoError(t, err)
		}

		cfg, err := config.GetConfigWithContext(cluster)
		require.NoError(t, err)
		k8sClient, err := client.New(cfg, client.Options{})
		require.NoError(t, err)
		clients[cluster] = k8sClient

		// Add the apiextensions scheme to the client's scheme.
		_ = apiextv1.AddToScheme(k8sClient.Scheme())

		kubectlOptions := k8s.NewKubectlOptions(cluster, kubeConfig, "kube-system")

		// Install Calico.
		calico.RegisterCalicoGVK(k8sClient.Scheme())
		objects := calico.K3DCalicoCNI(calico.K3dClusterBGPConfig{
			AddressAllocation: i,
		})

		for _, obj := range objects {
			err = k8sClient.Create(context.Background(), obj)
			require.NoError(t, err)
		}

		// Create or update CoreDNS deployment.
		deployment := coredns.CoreDNSDeployment(2)
		// Apply deployment.
		deploymentYaml := coredns.ToYAML(t, deployment)
		err = k8s.KubectlApplyFromStringE(t, kubectlOptions, deploymentYaml)
		require.NoError(t, err)

		// Wait for deployment to be ready.
		_, err = retry.DoWithRetryE(t, "waiting for coredns deployment",
			30, 10*time.Second,
			func() (string, error) {
				return k8s.RunKubectlAndGetOutputE(t, kubectlOptions,
					"wait", "--for=condition=Available", "deployment/coredns")
			})
		require.NoError(t, err)

		// Create CoreDNS service.
		service := coredns.CoreDNSService()
		serviceYaml := coredns.ToYAML(t, service)
		// Apply service.
		err = k8s.KubectlApplyFromStringE(t, kubectlOptions, serviceYaml)
		require.NoError(t, err)

		// Now get the DNS IPs.
		var ips []string
		_, err = retry.DoWithRetryE(t, "waiting for CoreDNS service IPs",
			30, 10*time.Second,
			func() (string, error) {
				svc, err := k8s.GetServiceE(t, kubectlOptions, "crl-core-dns")
				if err != nil {
					return "", err
				}

				if len(svc.Status.LoadBalancer.Ingress) == 0 {
					return "", fmt.Errorf("waiting for load balancer ingress")
				}

				// Collect IPs from ingress.
				for _, ingress := range svc.Status.LoadBalancer.Ingress {
					if ingress.IP != "" {
						time.Sleep(5 * time.Second)
						ips = append(ips, ingress.IP)
					} else if ingress.Hostname != "" {
						// If hostname is provided instead of IP, resolve it
						resolvedIPs, err := net.LookupHost(ingress.Hostname)
						if err != nil {
							return "", fmt.Errorf("failed to resolve hostname %s: %v", ingress.Hostname, err)
						}
						ips = append(ips, resolvedIPs...)
					}
				}
				return "", nil
			})

		require.NoError(t, err)

		corednsClusterOptions[CustomDomains[cluster]] = coredns.CoreDNSClusterOption{
			IPs:       ips,
			Namespace: r.Namespace[cluster],
			Domain:    CustomDomains[cluster],
		}
		if !r.IsMultiRegion {
			break
		}
	}

	// Update Coredns config.
	for _, cluster := range Clusters {
		// Create or update CoreDNS configmap.
		kubectlOptions := k8s.NewKubectlOptions(cluster, kubeConfig, "kube-system")
		cm := coredns.CoreDNSConfigMap(CustomDomains[cluster], corednsClusterOptions)

		// Apply the updated ConfigMap to Kubernetes.
		cmYaml := coredns.ToYAML(t, cm)
		err := k8s.KubectlApplyFromStringE(t, kubectlOptions, cmYaml)
		require.NoError(t, err)

		// restart coredns pods.
		err = k8s.RunKubectlE(t, kubectlOptions, "rollout", "restart", "deployment", "coredns")
		require.NoError(t, err)
		if !r.IsMultiRegion {
			r.Clients = clients
			r.ReusingInfra = true
			return
		}
	}
	r.Clients = clients
	r.ReusingInfra = true

	netConfig := calico.K3dCalicoBGPPeeringOptions{
		ClusterConfig: map[string]calico.K3dClusterBGPConfig{},
	}

	// Update network config for each region.
	for i, region := range RegionCodes {
		rawConfig.CurrentContext = Clusters[i]
		kubectlOptions := k8s.NewKubectlOptions(Clusters[i], kubeConfig, "kube-system")
		err := r.setupNetworking(t, context.TODO(), region, netConfig, kubectlOptions, i)
		if err != nil {
			t.Error(err)
		}
	}

	objectsByRegion := calico.K3dCalicoBGPPeeringObjects(netConfig)
	// Apply all the objects for each region on to the cluster.
	for i, region := range RegionCodes {
		ctl := clients[Clusters[i]]
		for _, obj := range objectsByRegion[region] {
			err := ctl.Create(context.Background(), obj)
			require.NoError(t, err)
		}
	}
}

// setupNetworking ensures there is cross-k3d-cluster network connectivity and
// service discovery.
func (r *Region) setupNetworking(t *testing.T, ctx context.Context, region string, netConfig calico.K3dCalicoBGPPeeringOptions, options *k8s.KubectlOptions, clusterId int) error {
	// Mark the master nodes as our bgp edge. These nodes will act as our bgp
	// peers.
	clusterConfig := netConfig.ClusterConfig[region]
	clusterConfig.AddressAllocation = clusterId

	ctl := r.Clients[options.ContextName]

	// Get master nodes.
	var nodes []corev1.Node
	nodes, err := k8s.GetNodesByFilterE(t, options, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("%s=%s", "node-role.kubernetes.io/master", "true"),
	})
	if err != nil {
		return errors.Wrapf(err, "list nodes in %s", region)
	}

	// Patch server nodes with new annotation.
	for _, node := range nodes {
		patch := []byte(`{"metadata": {"annotations": {"projectcalico.org/labels": "{\"edge\":\"true\"}"}}}`)
		if err := ctl.Patch(ctx, &node, client.RawPatch(types.StrategicMergePatchType, patch)); err != nil {
			return errors.Wrapf(err, "annotate node for calico edge")
		}

		time.Sleep(15 * time.Second)

		for _, nodeAddress := range node.Status.Addresses {
			if nodeAddress.Type == corev1.NodeInternalIP {
				clusterConfig.PeeringNodes = append(clusterConfig.PeeringNodes, nodeAddress.Address)
			}
		}
	}
	netConfig.ClusterConfig[region] = clusterConfig
	return nil
}

// InstallCharts Installs both Operator and CockroachDB charts by providing custom CA secret
// which is generated through cockroach binary, It also
// verifies whether relevant services are up and running.
func (r *Region) InstallCharts(t *testing.T, cluster string, index int) {
	// Get current context name.
	kubeConfig, rawConfig := r.GetCurrentContext(t)

	// Get helm chart paths.
	helmChartPath, _ := HelmChartPaths()

	// Verify if cluster exists in the contexts.
	if _, ok := rawConfig.Contexts[cluster]; !ok {
		t.Fatal()
	}
	rawConfig.CurrentContext = cluster

	// Setup kubectl options for this cluster.
	kubectlOptions := k8s.NewKubectlOptions(cluster, kubeConfig, r.Namespace[cluster])

	// Create namespace.
	k8s.CreateNamespace(t, kubectlOptions, r.Namespace[cluster])

	// create CA Secret.
	err := k8s.RunKubectlE(t, kubectlOptions, "create", "secret", "generic", customCASecret, "--from-file=ca.crt",
		"--from-file=ca.key")
	require.NoError(t, err)

	// Setup kubectl options for this cluster.
	kubectlOptions = k8s.NewKubectlOptions(cluster, kubeConfig, r.Namespace[cluster])
	InstallCockroachDBEnterpriseOperator(t, kubectlOptions)

	// Helm install cockroach CR with operator region config.
	crdbOptions := &helm.Options{
		KubectlOptions: kubectlOptions,
		SetValues: testutil.PatchHelmValues(map[string]string{
			"cockroachdb.clusterDomain":                                                             CustomDomains[cluster],
			"cockroachdb.crdbCluster.rollingRestartDelay":                                           "30s",
			"cockroachdb.tls.selfSigner.caProvided":                                                 "true",
			"cockroachdb.tls.selfSigner.caSecret":                                                   customCASecret,
			"cockroachdb.crdbCluster.dataStore.volumeClaimTemplate.spec.resources.requests.storage": "1Gi",
		}),
		SetJsonValues: map[string]string{
			"cockroachdb.crdbCluster.regions":        MustMarshalJSON(r.OperatorRegions(index, r.NodeCount)),
			"cockroachdb.crdbCluster.localityLabels": MustMarshalJSON([]string{"topology.kubernetes.io/region", "topology.kubernetes.io/zone"}),
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

	testutil.RequireCertificatesToBeValid(t, crdbCluster)
	testutil.RequireCRDBClusterToBeReadyEventuallyTimeout(t, kubectlOptions, crdbCluster, 900*time.Second)

	pods := k8s.ListPods(t, kubectlOptions, metav1.ListOptions{
		LabelSelector: LabelSelector,
	})
	require.True(t, len(pods) > 0)
	podName := fmt.Sprintf("%s.cockroachdb.%s", pods[0].Name, r.Namespace[cluster])
	testutil.RequireCRDBClusterToFunction(t, crdbCluster, false, podName)
	testutil.RequireCRDBDatabaseToFunction(t, crdbCluster, TestDBName, podName)
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
			// We are actually waiting for all pods to complete helm upgrade,
			// as just verifying one pod and cleaning up the resources will cause issues,
			// Since helm delete is happening while pods are still in upgrade process.
			for _, pod := range pods {
				if !pod.CreationTimestamp.Time.After(initialTimestamp) {
					return "", fmt.Errorf("pod %s has not been recreated with a new timestamp yet", pod.Name)
				}
			}
			return "", nil
		})
	return err
}

// ValidateMultiRegionSetup validates the multi-region setup.
func (r *Region) ValidateMultiRegionSetup(t *testing.T) {
	// Validate multi-region setup.
	for _, cluster := range Clusters {
		// Get current context name.
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

		// Verify regions output.
		expectedRegions := []string{
			"k3d-us-east1",
			"k3d-us-east2",
		}
		for _, clusterRegion := range expectedRegions {
			require.Contains(t, stdout, clusterRegion)
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
		for _, region := range expectedRegions {
			require.Equal(t, r.NodeCount, nodesPerRegion[region],
				"Region %s has %d nodes, expected %d",
				region, nodesPerRegion[region], r.NodeCount)
		}
	}
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

// GetCurrentContext gets current cluster context from KubeConfig.
func (r *Region) GetCurrentContext(t *testing.T) (string, api.Config) {
	kubeConfig, err := k8s.GetKubeConfigPathE(t)
	require.NoError(t, err)

	config := k8s.LoadConfigFromPath(kubeConfig)
	rawConfig, err := config.RawConfig()
	require.NoError(t, err)
	return kubeConfig, rawConfig
}

// CleanupResources will clean the resources installed by Operator, CockroachDB charts and deletes the namespace.
// Any failure in doing so might cause issues in other tests as some of the
// cluster resources are tied to the namespace.
func (r *Region) CleanupResources(t *testing.T) {
	for cluster, namespace := range r.Namespace {
		kubectlOptions := k8s.NewKubectlOptions(cluster, "", namespace)

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
		k8s.DeleteNamespace(t, kubectlOptions, namespace)
	}
}

// OperatorRegions returns the regions config based on the index
// which is referring to cluster index.
func (r *Region) OperatorRegions(index int, nodes int) []map[string]interface{} {
	return r.createOperatorRegions(index, nodes, CustomDomains, Clusters, RegionCodes)
}

func HelmChartPaths() (string, string) {
	rootPath := testutil.GetGitRoot()
	helmChartPath := filepath.Join(rootPath, "cockroachdb-parent/charts/cockroachdb")
	operatorChartPath := filepath.Join(rootPath, "cockroachdb-parent/charts/operator")

	return helmChartPath, operatorChartPath
}

// createK3DCluster creates a new k3d cluster
// by calling the make command which will create
// single k3d cluster.
func createK3DCluster(t *testing.T) error {
	cmd := shell.Command{
		Command: "make",
		Args: []string{
			"test/single-cluster/up",
		},
		WorkingDir: "../../../..",
	}

	output, err := shell.RunCommandAndGetOutputE(t, cmd)
	if err != nil {
		return fmt.Errorf("failed to create cluster: %v\nOutput: %s", err, output)
	}
	return nil
}

// createOperatorRegions returns the appropriate regions config
// required while installing CockroachDb charts.
func (r *Region) createOperatorRegions(index int, nodes int, customDomains map[string]string, clusters []string, regionCodes []string) []map[string]interface{} {
	regions := make([]map[string]interface{}, 0, len(regionCodes))

	for i, code := range regionCodes {
		if i > index {
			break
		}

		region := map[string]interface{}{
			"code":          code,
			"cloudProvider": CloudProvider,
			"nodes":         nodes,
			"namespace":     r.Namespace[clusters[i]],
		}

		if len(clusters) > i && clusters[i] != "" {
			if domain, ok := customDomains[clusters[i]]; ok {
				region["domain"] = domain
			}
		}

		regions = append(regions, region)
	}

	return regions
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
}

func UninstallCockroachDBEnterpriseOperator(t *testing.T, kubectlOptions *k8s.KubectlOptions) {
	operatorOpts := &helm.Options{
		KubectlOptions: kubectlOptions,
	}
	helm.Delete(t, operatorOpts, operatorReleaseName, true)
	k8s.RunKubectl(t, kubectlOptions, "delete", "service", "cockroach-webhook-service")
	k8s.RunKubectl(t, kubectlOptions, "delete", "validatingwebhookconfiguration", "cockroach-webhook-config")
	k8s.DeleteNamespace(t, kubectlOptions, kubectlOptions.Namespace)
}

func MustMarshalJSON(value interface{}) string {
	bytes, err := json.Marshal(value)
	if err != nil {
		panic(fmt.Sprintf("Failed to marshal JSON: %v", err))
	}
	return string(bytes)
}
