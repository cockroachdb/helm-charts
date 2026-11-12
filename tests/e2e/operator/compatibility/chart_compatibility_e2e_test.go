package compatibility

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cockroachdb/helm-charts/pkg/security"
	"github.com/cockroachdb/helm-charts/tests/e2e/operator"
	"github.com/cockroachdb/helm-charts/tests/e2e/operator/infra"
	"github.com/cockroachdb/helm-charts/tests/testutil"
	"github.com/gruntwork-io/terratest/modules/helm"
	"github.com/gruntwork-io/terratest/modules/k8s"
	"github.com/gruntwork-io/terratest/modules/random"
	"github.com/gruntwork-io/terratest/modules/retry"
	"github.com/gruntwork-io/terratest/modules/shell"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	compatOperatorVersionEnv    = "COMPAT_OPERATOR_CHART_VERSION"
	compatCockroachDBVersionEnv = "COMPAT_COCKROACHDB_CHART_VERSION"
	compatHelmRepoNameEnv       = "COMPAT_HELM_REPO_NAME"
	compatHelmRepoURLEnv        = "COMPAT_HELM_REPO_URL"

	defaultCompatHelmRepoName = "cockroachdb-v2-compat"
	defaultCompatHelmRepoURL  = "https://charts.cockroachdb.com/v2"
)

type compatibilityRegion struct {
	operator.Region
}

func TestChartCompatibilityUpgrade(t *testing.T) {
	operatorVersion := strings.TrimSpace(os.Getenv(compatOperatorVersionEnv))
	cockroachDBVersion := strings.TrimSpace(os.Getenv(compatCockroachDBVersionEnv))
	if operatorVersion == "" || cockroachDBVersion == "" {
		t.Skipf("%s and %s must be set to run chart compatibility e2e tests; CI sets these from the PR base chart versions", compatOperatorVersionEnv, compatCockroachDBVersionEnv)
	}

	provider := compatProvider(t)
	r := newCompatibilityRegion(provider)
	cloudProvider := infra.ProviderFactory(provider, &r.Region)
	if cloudProvider == nil {
		t.Fatalf("unsupported provider: %s", provider)
	}

	t.Cleanup(func() {
		t.Logf("Starting infrastructure cleanup for provider: %s", provider)
		cloudProvider.TeardownInfra(t)
		t.Logf("Completed infrastructure cleanup for provider: %s", provider)
	})

	cloudProvider.SetUpInfra(t)

	cluster := r.Clusters[0]
	r.Namespace[cluster] = fmt.Sprintf("%s-%s", operator.Namespace, strings.ToLower(random.UniqueId()))

	defer r.CleanupResources(t)

	err := r.CreateCACertificate(t)
	require.NoError(t, err)
	defer r.CleanUpCACertificate(t)

	kubeConfig, rawConfig := r.GetCurrentContext(t)
	if _, ok := rawConfig.Contexts[cluster]; !ok {
		t.Fatalf("cluster context %q not found in kubeconfig", cluster)
	}
	rawConfig.CurrentContext = cluster

	kubectlOptions := k8s.NewKubectlOptions(cluster, kubeConfig, r.Namespace[cluster])
	k8s.CreateNamespace(t, kubectlOptions, r.Namespace[cluster])
	err = k8s.RunKubectlE(t, kubectlOptions, "create", "secret", "generic", "cockroachdb-ca-secret",
		"--from-file=ca.crt", "--from-file=ca.key")
	require.NoError(t, err)

	repoName := addCompatibilityHelmRepo(t)
	defer helm.RemoveRepo(t, &helm.Options{}, repoName)

	t.Logf("Installing published operator chart %s", operatorVersion)
	operator.InstallCockroachDBOperatorChart(t, kubectlOptions,
		fmt.Sprintf("%s/cockroachdb-operator-chart", repoName),
		operatorVersion,
		map[string]string{"numReplicas": "1"},
	)

	t.Logf("Installing published CockroachDB chart %s", cockroachDBVersion)
	installCockroachDBChart(t, &r.Region, kubectlOptions,
		fmt.Sprintf("%s/cockroachdb-chart", repoName),
		cockroachDBVersion,
	)
	r.ValidateCRDB(t, cluster)
	brokenNodeCert := requireOldSelfSignerRotation(t, r, cluster, kubectlOptions)

	helmChartPath, operatorChartPath := operator.HelmChartPaths()

	t.Log("Upgrading operator chart from the local checkout")
	operatorValues := compatibilityOperatorUpgradeValues(t, operatorChartPath)
	operator.UpgradeCockroachDBOperatorChart(t, kubectlOptions, operatorChartPath, operatorValues)
	requireUpgradedOperator(t, kubectlOptions, operatorValues)
	requireOperatorMigrationFlag(t, kubectlOptions, false)

	t.Log("Upgrading CockroachDB chart from the local checkout")
	postInitSQLDatabase := "compat_post_init"
	upgradeCockroachDBChart(t, kubectlOptions, helmChartPath, map[string]string{
		"cockroachdb.crdbCluster.postInitSQL.inline[0]": fmt.Sprintf("CREATE DATABASE IF NOT EXISTS %s", postInitSQLDatabase),
	})
	nodeAfterUpgrade := k8s.GetSecret(t, kubectlOptions, "cockroachdb-node-secret")
	require.Equal(t, brokenNodeCert, nodeAfterUpgrade.Data["tls.crt"],
		"upgrading the chart and image alone should not repair the broken certificate")
	t.Run("recover join SANs after upgrade", func(t *testing.T) {
		requireSelfSignerRotation(t, r, cluster, kubectlOptions, helmChartPath)
	})

	testutil.RequireCRDBClusterToBeReadyEventuallyTimeout(t, kubectlOptions, testutil.CockroachCluster{
		DesiredNodes: r.NodeCount,
	}, 600*time.Second)
	r.ValidateCRDB(t, cluster)
	requireRunningRelatedImages(t, kubectlOptions, operatorValues)
	requirePostInitSQLDatabase(t, kubectlOptions, postInitSQLDatabase)
	t.Run("secure helm test after certificate rotation", func(t *testing.T) {
		requireSecureHelmTest(t, kubectlOptions)
	})

	t.Log("Checking migration settings and node-reader RBAC transfer after upgrade")
	nodeReaderName := fmt.Sprintf("compat-node-reader-%s", r.Namespace[cluster])
	operator.UpgradeCockroachDBOperatorChart(
		t,
		kubectlOptions,
		operatorChartPath,
		compatibilityOperatorMigrationAndNodeReaderValues(t, operatorChartPath, r.Namespace[cluster], nodeReaderName),
	)
	requireOperatorMigrationFlag(t, kubectlOptions, true)

	upgradeCockroachDBChart(t, kubectlOptions, helmChartPath, map[string]string{
		"cockroachdb.crdbCluster.rbac.nodeReader.create": "false",
	})
	requireNodeReaderDelegatedToOperatorChart(t, kubectlOptions, nodeReaderName)
	testutil.RequireCRDBClusterToBeReadyEventuallyTimeout(t, kubectlOptions, testutil.CockroachCluster{
		DesiredNodes: r.NodeCount,
	}, 600*time.Second)
	r.ValidateCRDB(t, cluster)
}

func newCompatibilityRegion(provider string) *compatibilityRegion {
	clusterName := fmt.Sprintf("%s-%s", provider, operator.Clusters[0])
	if provider != infra.ProviderK3D && provider != infra.ProviderKind {
		clusterName = fmt.Sprintf("%s-%s", clusterName, strings.ToLower(random.UniqueId()))
	}

	return &compatibilityRegion{
		Region: operator.Region{
			NodeCount:    3,
			ReusingInfra: false,
			Clients:      make(map[string]client.Client),
			Namespace:    make(map[string]string),
			Provider:     provider,
			Clusters:     []string{clusterName},
		},
	}
}

func compatProvider(t *testing.T) string {
	switch p := strings.TrimSpace(strings.ToLower(os.Getenv("PROVIDER"))); p {
	case "":
		return infra.ProviderK3D
	case infra.ProviderK3D, infra.ProviderKind, infra.ProviderGCP:
		return p
	default:
		t.Fatalf("unsupported provider override: %s", p)
		return ""
	}
}

func addCompatibilityHelmRepo(t *testing.T) string {
	repoName := strings.TrimSpace(os.Getenv(compatHelmRepoNameEnv))
	if repoName == "" {
		repoName = defaultCompatHelmRepoName
	}
	repoURL := strings.TrimSpace(os.Getenv(compatHelmRepoURLEnv))
	if repoURL == "" {
		repoURL = defaultCompatHelmRepoURL
	}

	options := &helm.Options{
		ExtraArgs: map[string][]string{
			"repoAdd": {"--force-update"},
		},
	}
	helm.AddRepo(t, options, repoName, repoURL)

	_, err := shell.RunCommandAndGetOutputE(t, shell.Command{
		Command: "helm",
		Args:    []string{"repo", "update", repoName},
	})
	require.NoError(t, err)

	return repoName
}

func compatibilityOperatorUpgradeValues(t *testing.T, operatorChartPath string) map[string]string {
	// TODO: For the next release, verify that upgrading the published chart applies
	// podAnnotations and podLabels to the running operator pods. Once that release
	// is validated, remove these markers and the metadata assertions in
	// requireUpgradedOperator for later releases and keep the image upgrade checks.
	values := map[string]string{
		"numReplicas":                       "1",
		"podAnnotations.compatibility-test": "operator-upgrade",
		"podLabels.compatibility-test":      "operator-upgrade",
		"resources.requests.cpu":            "250m",
		"resources.requests.memory":         "512Mi",
		"resources.limits.cpu":              "1500m",
		"resources.limits.memory":           "2Gi",
		"nodeSelector.kubernetes\\.io/os":   "linux",
		"tolerations[0].key":                "compatibility-test",
		"tolerations[0].operator":           "Equal",
		"tolerations[0].value":              "operator-upgrade",
		"tolerations[0].effect":             "NoSchedule",
	}
	// Match the test nodes without requiring dedicated nodes or extra taints.
	affinityPrefix := "affinity.nodeAffinity.requiredDuringSchedulingIgnoredDuringExecution.nodeSelectorTerms[0].matchExpressions[0]."
	values[affinityPrefix+"key"] = "kubernetes.io/os"
	values[affinityPrefix+"operator"] = "In"
	values[affinityPrefix+"values[0]"] = "linux"
	for key, value := range localOperatorImageValues(t, operatorChartPath) {
		values[key] = value
	}
	return values
}

type chartImageValues struct {
	Registry   string `yaml:"registry"`
	Repository string `yaml:"repository"`
	Tag        string `yaml:"tag"`
}

type operatorImageValues struct {
	Image         chartImageValues `yaml:"image"`
	RelatedImages struct {
		InitContainer chartImageValues `yaml:"initContainer"`
		Inotifywait   chartImageValues `yaml:"inotifywait"`
	} `yaml:"relatedImages"`
}

func localOperatorImageValues(t *testing.T, operatorChartPath string) map[string]string {
	t.Helper()

	valuesBytes, err := os.ReadFile(filepath.Join(operatorChartPath, "values.yaml"))
	require.NoError(t, err)

	var values operatorImageValues
	require.NoError(t, yaml.Unmarshal(valuesBytes, &values))
	require.NotEmpty(t, values.Image.Registry)
	require.NotEmpty(t, values.Image.Repository)
	require.NotEmpty(t, values.Image.Tag)

	result := map[string]string{
		"image.registry":   values.Image.Registry,
		"image.repository": values.Image.Repository,
		"image.tag":        values.Image.Tag,
	}
	for name, image := range map[string]chartImageValues{
		"initContainer": values.RelatedImages.InitContainer,
		"inotifywait":   values.RelatedImages.Inotifywait,
	} {
		if image.Registry == "" {
			image.Registry = values.Image.Registry
		}
		if image.Tag == "" {
			image.Tag = values.Image.Tag
		}
		require.NotEmpty(t, image.Repository)
		result["relatedImages."+name+".registry"] = image.Registry
		result["relatedImages."+name+".repository"] = image.Repository
		result["relatedImages."+name+".tag"] = image.Tag
	}
	return result
}

func requireUpgradedOperator(t *testing.T, kubectlOptions *k8s.KubectlOptions, values map[string]string) {
	t.Helper()

	pods := testutil.ListPodsWithRetry(t, kubectlOptions, metav1.ListOptions{
		LabelSelector: operator.OperatorLabelSelector,
	})
	checked := 0
	for _, pod := range pods {
		if pod.DeletionTimestamp != nil {
			continue
		}
		checked++
		require.Equal(t, "operator-upgrade", pod.Annotations["compatibility-test"])
		require.Equal(t, "operator-upgrade", pod.Labels["compatibility-test"])
		var container *corev1.Container
		for i := range pod.Spec.Containers {
			if pod.Spec.Containers[i].Name == "cockroach-operator" {
				container = &pod.Spec.Containers[i]
				break
			}
		}
		require.NotNil(t, container)
		require.Equal(t, configuredImage(values, "image"), container.Image)
		require.Equal(t, values["resources.requests.cpu"], container.Resources.Requests.Cpu().String())
		require.Equal(t, values["resources.requests.memory"], container.Resources.Requests.Memory().String())
		require.Equal(t, values["resources.limits.cpu"], container.Resources.Limits.Cpu().String())
		require.Equal(t, values["resources.limits.memory"], container.Resources.Limits.Memory().String())
		require.Equal(t, "linux", pod.Spec.NodeSelector["kubernetes.io/os"])
		require.Contains(t, pod.Spec.Tolerations, corev1.Toleration{
			Key: "compatibility-test", Operator: corev1.TolerationOpEqual, Value: "operator-upgrade", Effect: corev1.TaintEffectNoSchedule,
		})
		require.NotNil(t, pod.Spec.Affinity)
		require.NotNil(t, pod.Spec.Affinity.NodeAffinity)
		requiredAffinity := pod.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution
		require.NotNil(t, requiredAffinity)
		require.Equal(t, []corev1.NodeSelectorTerm{{
			MatchExpressions: []corev1.NodeSelectorRequirement{{
				Key: "kubernetes.io/os", Operator: corev1.NodeSelectorOpIn, Values: []string{"linux"},
			}},
		}}, requiredAffinity.NodeSelectorTerms)
		for envName, imageName := range map[string]string{
			"RELATED_IMAGE_INIT_CONTAINER": "initContainer",
			"RELATED_IMAGE_INOTIFYWAIT":    "inotifywait",
		} {
			var actual string
			for _, env := range container.Env {
				if env.Name == envName {
					actual = env.Value
				}
			}
			require.Equal(t, configuredImage(values, "relatedImages."+imageName), actual, envName)
		}
	}
	require.Positive(t, checked, "no upgraded operator pods found")
}

// Check what replacement pods actually pulled, in addition to the operator's
// RELATED_IMAGE environment variables. This catches stale sidecar tags or pins.
func requireRunningRelatedImages(t *testing.T, options *k8s.KubectlOptions, values map[string]string) {
	t.Helper()
	require.Eventually(t, func() bool {
		pods, err := k8s.ListPodsE(t, options, metav1.ListOptions{LabelSelector: operator.LabelSelector})
		if err != nil || len(pods) == 0 {
			return false
		}
		checked := 0
		for _, pod := range pods {
			if pod.DeletionTimestamp != nil {
				continue
			}
			checked++
			for containerName, imageName := range map[string]string{
				"cockroachdb-init": "initContainer", "cert-reloader": "inotifywait",
			} {
				statuses := append(append([]corev1.ContainerStatus{}, pod.Status.InitContainerStatuses...), pod.Status.ContainerStatuses...)
				found := false
				for _, status := range statuses {
					if status.Name != containerName {
						continue
					}
					found = status.Image == configuredImage(values, "relatedImages."+imageName) && strings.Contains(status.ImageID, "sha256:")
				}
				if !found {
					return false
				}
			}
		}
		return checked > 0
	}, 10*time.Minute, 5*time.Second, "waiting for all related containers to use the upgraded images and resolved digests")
}

func configuredImage(values map[string]string, prefix string) string {
	return fmt.Sprintf("%s/%s:%s", values[prefix+".registry"], values[prefix+".repository"], values[prefix+".tag"])
}

func compatibilityOperatorMigrationAndNodeReaderValues(
	t *testing.T, operatorChartPath, namespace, nodeReaderName string,
) map[string]string {
	values := compatibilityOperatorUpgradeValues(t, operatorChartPath)
	values["migration.enabled"] = "true"
	values["nodeReader.enabled"] = "true"
	values["nodeReader.name"] = nodeReaderName
	values["nodeReader.subjects[0].namespace"] = namespace
	values["nodeReader.subjects[0].serviceAccountName"] = operator.ReleaseName
	return values
}

func installCockroachDBChart(
	t *testing.T, r *operator.Region, kubectlOptions *k8s.KubectlOptions, chart, version string,
) {
	options := &helm.Options{
		KubectlOptions: kubectlOptions,
		Version:        version,
		SetValues:      cockroachDBCompatibilityValues(),
		SetJsonValues: map[string]string{
			"cockroachdb.crdbCluster.regions": operator.MustMarshalJSON(r.OperatorRegions(0, r.NodeCount)),
		},
		ExtraArgs: map[string][]string{
			"install": {
				"--wait",
				"--debug",
			},
		},
	}

	operator.HelmInstallWithRetry(t, options, chart, operator.ReleaseName)
	k8s.WaitUntilServiceAvailable(t, kubectlOptions, "cockroachdb-public", 30, 5*time.Second)
}

func upgradeCockroachDBChart(
	t *testing.T, kubectlOptions *k8s.KubectlOptions, chart string, overrides ...map[string]string,
) {
	values := cockroachDBCompatibilityValues()
	// TODO: For the next release, verify that tests.podAnnotations and tests.podLabels
	// reach the Helm test pod after chart upgrade and certificate rotation. Once
	// that release is validated, remove these markers and the metadata assertions
	// in requireSecureHelmTest for later releases and keep the secure connection test.
	values["cockroachdb.tests.podAnnotations.compatibility-test"] = "secure-helm-test"
	values["cockroachdb.tests.podLabels.compatibility-test"] = "secure-helm-test"
	// Reused values can retain the published chart's older self-signer image.
	valuesBytes, err := os.ReadFile(filepath.Join(chart, "values.yaml"))
	require.NoError(t, err)
	var localValues struct {
		CockroachDB struct {
			TLS struct {
				SelfSigner operatorImageValues `yaml:"selfSigner"`
			} `yaml:"tls"`
		} `yaml:"cockroachdb"`
	}
	require.NoError(t, yaml.Unmarshal(valuesBytes, &localValues))
	require.NotEmpty(t, localValues.CockroachDB.TLS.SelfSigner.Image.Tag)
	values["cockroachdb.tls.selfSigner.image.tag"] = localValues.CockroachDB.TLS.SelfSigner.Image.Tag
	for _, override := range overrides {
		for key, value := range override {
			values[key] = value
		}
	}

	options := &helm.Options{
		KubectlOptions: kubectlOptions,
		SetValues:      values,
		ExtraArgs: map[string][]string{
			"upgrade": {
				"--reuse-values",
				"--wait",
				"--debug",
			},
		},
	}

	operator.HelmUpgradeWithRetry(t, options, chart, operator.ReleaseName)
	k8s.WaitUntilServiceAvailable(t, kubectlOptions, "cockroachdb-public", 30, 5*time.Second)

	cronJobJSON, err := k8s.RunKubectlAndGetOutputE(t, kubectlOptions,
		"get", "cronjob", "cockroachdb-rotate-self-signer-client", "-o", "json")
	require.NoError(t, err)
	var cronJob batchv1.CronJob
	require.NoError(t, json.Unmarshal([]byte(cronJobJSON), &cronJob))
	require.Len(t, cronJob.Spec.JobTemplate.Spec.Template.Spec.Containers, 1)
	container := cronJob.Spec.JobTemplate.Spec.Template.Spec.Containers[0]
	require.Equal(t, configuredImage(values, "cockroachdb.tls.selfSigner.image"), container.Image,
		"upgrades must replace the published self-signer image with the local chart image")
	require.Contains(t, container.Args, "--operator-managed=true")
}

func requireSecureHelmTest(t *testing.T, kubectlOptions *k8s.KubectlOptions) {
	t.Helper()

	_, err := helm.RunHelmCommandAndGetOutputE(t, &helm.Options{KubectlOptions: kubectlOptions},
		"test", operator.ReleaseName, "--logs", "--timeout", "5m")
	require.NoError(t, err)
	pod := k8s.GetPod(t, kubectlOptions, "cockroachdb-test")
	require.Equal(t, corev1.PodSucceeded, pod.Status.Phase)
	require.Equal(t, "secure-helm-test", pod.Annotations["compatibility-test"])
	require.Equal(t, "secure-helm-test", pod.Labels["compatibility-test"])
}

func requireOldSelfSignerRotation(
	t *testing.T, r *compatibilityRegion, cluster string, kubectlOptions *k8s.KubectlOptions,
) []byte {
	t.Helper()
	ctx := context.Background()
	nodeSecret := k8s.GetSecret(t, kubectlOptions, "cockroachdb-node-secret")
	joinHostname := fmt.Sprintf("cockroachdb-join.%s.svc.%s", kubectlOptions.Namespace, operator.CustomDomains[0])
	cert, err := security.GetCertObj(nodeSecret.Data["tls.crt"])
	require.NoError(t, err)
	require.NoError(t, cert.VerifyHostname(joinHostname))

	k8s.RunKubectl(t, kubectlOptions, "annotate", "secret", nodeSecret.Name,
		"certificate-valid-upto=2000-01-01T00:00:00Z", "--overwrite")
	var cronJob batchv1.CronJob
	require.NoError(t, r.Clients[cluster].Get(ctx, client.ObjectKey{
		Namespace: kubectlOptions.Namespace, Name: "cockroachdb-rotate-self-signer-client",
	}, &cronJob))

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "compat-old-certificate-rotate", Namespace: kubectlOptions.Namespace},
		Spec:       *cronJob.Spec.JobTemplate.Spec.DeepCopy(),
	}
	// Pin the broken signer and arguments so this still reproduces after later releases.
	container := &job.Spec.Template.Spec.Containers[0]
	container.Image = fmt.Sprintf("%s/%s:1.10", testutil.TestSelfSignerImageRegistry, testutil.TestSelfSignerImageRepository)
	args := make([]string, 0, len(container.Args))
	for _, arg := range container.Args {
		if !strings.HasPrefix(arg, "--operator-managed") {
			args = append(args, arg)
		}
	}
	container.Args = args
	require.NoError(t, r.Clients[cluster].Create(ctx, job))

	// The old job updates the Secret, then its retry skips rotation and succeeds.
	testutil.RequireCertRotateJobToBeCompleted(t, job.Name, testutil.CockroachCluster{
		K8sClient: r.Clients[cluster], Namespace: kubectlOptions.Namespace,
	}, 600*time.Second)
	rotatedNode := k8s.GetSecret(t, kubectlOptions, nodeSecret.Name)
	require.NotEqual(t, nodeSecret.Data["tls.crt"], rotatedNode.Data["tls.crt"])
	cert, err = security.GetCertObj(rotatedNode.Data["tls.crt"])
	require.NoError(t, err)
	for _, hostname := range []string{"cockroachdb-join", "cockroachdb-join." + kubectlOptions.Namespace, joinHostname} {
		require.Error(t, cert.VerifyHostname(hostname), "old rotation should drop join SAN %s", hostname)
	}
	return rotatedNode.Data["tls.crt"]
}

func requireSelfSignerRotation(
	t *testing.T, r *compatibilityRegion, cluster string, kubectlOptions *k8s.KubectlOptions, chart string,
) {
	t.Helper()

	nodeSecret := k8s.GetSecret(t, kubectlOptions, "cockroachdb-node-secret")
	caSecret := k8s.GetSecret(t, kubectlOptions, "cockroachdb-ca-secret")
	upgradeCockroachDBChart(t, kubectlOptions, chart, map[string]string{
		"cockroachdb.tls.selfSigner.additionalSANs[0]":                       "compat.example.com",
		"cockroachdb.tls.selfSigner.additionalSANs[1]":                       "192.0.2.20",
		"cockroachdb.crdbCluster.clusterSettings.sql\\.defaults\\.vectorize": "on",
	})

	nodeAfterUpgrade := k8s.GetSecret(t, kubectlOptions, nodeSecret.Name)
	require.Equal(t, nodeSecret.Data["tls.crt"], nodeAfterUpgrade.Data["tls.crt"],
		"upgrading the chart alone should not regenerate the node certificate")

	clientSecret := k8s.GetSecret(t, kubectlOptions, "cockroachdb-client-secret")
	for _, name := range []string{nodeSecret.Name, clientSecret.Name} {
		k8s.RunKubectl(t, kubectlOptions, "annotate", "secret", name,
			"certificate-valid-upto=2000-01-01T00:00:00Z", "--overwrite")
	}
	jobName := "compat-certificate-rotate"
	k8s.RunKubectl(t, kubectlOptions, "create", "job", jobName,
		"--from=cronjob/cockroachdb-rotate-self-signer-client")
	testutil.RequireCertRotateJobToBeCompleted(t, jobName, testutil.CockroachCluster{
		K8sClient: r.Clients[cluster], Namespace: kubectlOptions.Namespace,
	}, 600*time.Second)

	rotatedNode := k8s.GetSecret(t, kubectlOptions, nodeSecret.Name)
	rotatedClient := k8s.GetSecret(t, kubectlOptions, clientSecret.Name)
	require.NotEqual(t, nodeSecret.Data["tls.crt"], rotatedNode.Data["tls.crt"])
	require.NotEqual(t, clientSecret.Data["tls.crt"], rotatedClient.Data["tls.crt"])

	cert, err := security.GetCertObj(rotatedNode.Data["tls.crt"])
	require.NoError(t, err)
	for _, hostname := range []string{
		"cockroachdb-join",
		"cockroachdb-join." + kubectlOptions.Namespace,
		fmt.Sprintf("cockroachdb-join.%s.svc.%s", kubectlOptions.Namespace, operator.CustomDomains[0]),
		"compat.example.com",
		"192.0.2.20",
	} {
		require.NoError(t, cert.VerifyHostname(hostname))
	}

	caAfterRotation := k8s.GetSecret(t, kubectlOptions, caSecret.Name)
	require.Equal(t, caSecret.Data["ca.crt"], caAfterRotation.Data["ca.crt"],
		"node regeneration should keep the existing CA")
	caBundle, err := k8s.RunKubectlAndGetOutputE(t, kubectlOptions,
		"get", "configmap", "cockroachdb-ca-secret-crt", "-o", "jsonpath={.data.ca\\.crt}")
	require.NoError(t, err)
	// Terratest trims kubectl output, including the PEM's trailing newline.
	require.Equal(t, strings.TrimSpace(string(caSecret.Data["ca.crt"])), strings.TrimSpace(caBundle))

	// Apply SQL settings with the recovered join certificate and request a rolling
	// restart. Existing pods keep the old auxiliary images until they are recreated.
	upgradeCockroachDBChart(t, kubectlOptions, chart, map[string]string{
		"cockroachdb.crdbCluster.clusterSettings.sql\\.defaults\\.vectorize": "off",
		"cockroachdb.crdbCluster.timestamp":                                  time.Now().UTC().Format(time.RFC3339Nano),
	})
	_, err = retry.DoWithRetryE(t, "waiting for cluster setting after certificate rotation", 60, 10*time.Second, func() (string, error) {
		pods, err := k8s.ListPodsE(t, kubectlOptions, metav1.ListOptions{LabelSelector: operator.LabelSelector})
		if err != nil {
			return "", err
		}
		if len(pods) == 0 {
			return "", fmt.Errorf("no CockroachDB pods found")
		}
		output, err := operator.ExecSQL(t, kubectlOptions, pods[0].Name, "SHOW CLUSTER SETTING sql.defaults.vectorize")
		if err != nil {
			return output, err
		}
		if !strings.Contains(output, "off") {
			return output, fmt.Errorf("cluster setting has not been applied: %s", output)
		}
		return output, nil
	})
	require.NoError(t, err)
}

func requireOperatorMigrationFlag(t *testing.T, kubectlOptions *k8s.KubectlOptions, expected bool) {
	t.Helper()

	args, err := k8s.RunKubectlAndGetOutputE(t, kubectlOptions,
		"get", "deployment", "cockroach-operator",
		"-o", `jsonpath={.spec.template.spec.containers[?(@.name=="cockroach-operator")].args}`,
	)
	require.NoError(t, err)
	if expected {
		require.Contains(t, args, "--enable-migration")
	} else {
		require.NotContains(t, args, "--enable-migration")
	}
}

func requireNodeReaderDelegatedToOperatorChart(
	t *testing.T, kubectlOptions *k8s.KubectlOptions, nodeReaderName string,
) {
	t.Helper()

	k8s.RunKubectl(t, kubectlOptions, "get", "clusterrole", nodeReaderName)
	k8s.RunKubectl(t, kubectlOptions, "get", "clusterrolebinding", nodeReaderName)

	crdbChartNodeReaderName := fmt.Sprintf("%s-%s-node-reader", operator.ReleaseName, kubectlOptions.Namespace)
	_, err := k8s.RunKubectlAndGetOutputE(t, kubectlOptions, "get", "clusterrole", crdbChartNodeReaderName)
	require.Error(t, err, "cockroachdb chart-owned node-reader ClusterRole should be removed")
	_, err = k8s.RunKubectlAndGetOutputE(t, kubectlOptions, "get", "clusterrolebinding", crdbChartNodeReaderName)
	require.Error(t, err, "cockroachdb chart-owned node-reader ClusterRoleBinding should be removed")

	canReadNodes, err := k8s.RunKubectlAndGetOutputE(t, kubectlOptions,
		"auth", "can-i", "get", "nodes",
		fmt.Sprintf("--as=system:serviceaccount:%s:%s", kubectlOptions.Namespace, operator.ReleaseName),
	)
	require.NoError(t, err)
	lines := strings.Split(strings.TrimSpace(canReadNodes), "\n")
	require.Equal(t, "yes", strings.TrimSpace(lines[len(lines)-1]))
}

func requirePostInitSQLDatabase(t *testing.T, kubectlOptions *k8s.KubectlOptions, databaseName string) {
	t.Helper()

	_, err := retry.DoWithRetryE(t, fmt.Sprintf("waiting for postInitSQL database %s", databaseName), 60, 10*time.Second, func() (string, error) {
		pods, err := k8s.ListPodsE(t, kubectlOptions, metav1.ListOptions{
			LabelSelector: operator.LabelSelector,
		})
		if err != nil {
			return "", err
		}
		if len(pods) == 0 {
			return "", fmt.Errorf("no pods found with label %s", operator.LabelSelector)
		}

		output, err := operator.ExecSQL(t, kubectlOptions, pods[0].Name,
			fmt.Sprintf("SELECT database_name FROM [SHOW DATABASES] WHERE database_name = '%s'", databaseName),
		)
		if err != nil {
			return output, err
		}
		if !strings.Contains(output, databaseName) {
			return output, fmt.Errorf("database %s not found", databaseName)
		}
		return output, nil
	})
	require.NoError(t, err)
}

func cockroachDBCompatibilityValues() map[string]string {
	return operator.PatchHelmValues(map[string]string{
		"cockroachdb.clusterDomain":             operator.CustomDomains[0],
		"cockroachdb.tls.enabled":               "true",
		"cockroachdb.tls.selfSigner.caProvided": "true",
		"cockroachdb.tls.selfSigner.caSecret":   "cockroachdb-ca-secret",
	})
}
