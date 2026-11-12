package migrate

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/cockroachdb/helm-charts/pkg/generator"
	"github.com/cockroachdb/helm-charts/pkg/upstream/cockroach-operator/api/v1beta1"
	"github.com/cockroachdb/helm-charts/tests/e2e/operator"
	"github.com/cockroachdb/helm-charts/tests/testutil"
	"github.com/cockroachdb/helm-charts/tests/testutil/migration"
	"github.com/gruntwork-io/terratest/modules/helm"
	"github.com/gruntwork-io/terratest/modules/k8s"
	"github.com/gruntwork-io/terratest/modules/random"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

const helmPVCMetadataKey = "migration-test"

func snapshotSourceIdentity(t *testing.T, options *k8s.KubectlOptions, name string) *corev1.PodSecurityContext {
	t.Helper()
	pod := k8s.GetPod(t, options, name+"-0")
	require.NotNil(t, pod.Spec.SecurityContext, "source fixture must exercise a non-root identity")
	identity := pod.Spec.SecurityContext.DeepCopy()
	require.NotNil(t, identity.RunAsUser)
	require.NotZero(t, *identity.RunAsUser)
	require.NotNil(t, identity.FSGroup)
	// Migration aligns the primary group with the source volume group so
	// CockroachDB can read TLS keys mounted with group-read permissions.
	identity.RunAsGroup = new(*identity.FSGroup)
	return identity
}

func requirePodJoin(t *testing.T, options *k8s.KubectlOptions, name, expected string) {
	t.Helper()
	pod := k8s.GetPod(t, options, name+"-0")
	for _, container := range pod.Spec.Containers {
		if container.Name == "cockroachdb" {
			command := strings.Join(append(append([]string{}, container.Command...), container.Args...), " ")
			require.Regexp(t, `--join[= ]+`+regexp.QuoteMeta(expected)+`(?:\s|$)`, command)
			return
		}
	}
	t.Fatalf("pod %s has no database container", pod.Name)
}

// Check the persistent template as well as the running pods. A local volume may
// still be readable after losing the source UID, hiding a Linux rollout failure.
func requireSourceIdentity(
	t *testing.T, options *k8s.KubectlOptions, name string, expected *corev1.PodSecurityContext,
) {
	t.Helper()
	check := func(object string, actual *corev1.PodSecurityContext) {
		t.Helper()
		require.NotNil(t, actual, "%s lost the source security context", object)
		require.Equal(t, expected.RunAsUser, actual.RunAsUser, "%s changed runAsUser", object)
		require.Equal(t, expected.RunAsGroup, actual.RunAsGroup, "%s changed runAsGroup", object)
		require.Equal(t, expected.FSGroup, actual.FSGroup, "%s changed fsGroup", object)
	}
	raw, err := k8s.RunKubectlAndGetOutputE(t, options, "get", v1beta1CrdbClusterResource, name, "-o", "json")
	require.NoError(t, err)
	var cluster v1beta1.CrdbCluster
	require.NoError(t, json.Unmarshal([]byte(raw), &cluster))
	require.NotNil(t, cluster.Spec.Template.Spec.PodTemplate)
	check("CrdbCluster template", cluster.Spec.Template.Spec.PodTemplate.Spec.SecurityContext)

	checked := 0
	for _, pod := range k8s.ListPods(t, options, metav1.ListOptions{LabelSelector: operator.LabelSelector}) {
		owner := metav1.GetControllerOf(&pod)
		if owner == nil || owner.Kind != "CrdbNode" {
			continue
		}
		raw, err := k8s.RunKubectlAndGetOutputE(t, options, "get", "crdbnode", owner.Name, "-o", "json")
		require.NoError(t, err)
		var node v1beta1.CrdbNode
		require.NoError(t, json.Unmarshal([]byte(raw), &node))
		require.NotNil(t, node.Spec.PodTemplate)
		check("CrdbNode "+node.Name, node.Spec.PodTemplate.Spec.SecurityContext)
		check("Pod "+pod.Name, pod.Spec.SecurityContext)
		checked++
	}
	require.Positive(t, checked, "no migrated database pods were checked")
}

// Exercise the final cleanup while the adopted database is still running. Test
// teardown deletes CRDs, which cannot prove that conversion was retired safely.
func verifyLegacyOperatorRetirement(
	t *testing.T, options *k8s.KubectlOptions, name, valuesFile string,
	cluster testutil.CockroachCluster, service publicSQLService,
) {
	t.Helper()
	const crd = "crdbclusters.crdb.cockroachlabs.com"
	requireJSONPathEquals(t, options, "crd", crd, "{.spec.conversion.strategy}", "Webhook")
	requireJSONPathEquals(t, options, "crd", crd, "{.spec.versions[?(@.name=='v1alpha1')].served}", "true")
	crdUID, err := k8s.RunKubectlAndGetOutputE(t, options, "get", "crd", crd, "-o", "jsonpath={.metadata.uid}")
	require.NoError(t, err)
	clusterUID, err := k8s.RunKubectlAndGetOutputE(t, options, "get", v1beta1CrdbClusterResource, name,
		"-o", "jsonpath={.metadata.uid}")
	require.NoError(t, err)
	beforeSpec, err := k8s.RunKubectlAndGetOutputE(t, options, "get", v1beta1CrdbClusterResource, name,
		"-o", "jsonpath={.spec}")
	require.NoError(t, err)

	t.Log("Uninstalling the legacy operator after adoption, keeping the shared CRDs")
	cleanupPublicOperatorResources(t)
	_, err = k8s.GetDeploymentE(t, migrationKubectlOptions(migration.OperatorNamespace), migration.OperatorDeploymentName)
	require.True(t, apierrors.IsNotFound(err), "legacy operator deployment still exists: %v", err)
	k8s.RunKubectl(t, options, "label", v1beta1CrdbClusterResource, name,
		"crdb.io/skip-reconcile-", "crdb.io/migrate-")
	k8s.RunKubectl(t, options, "annotate", v1beta1CrdbClusterResource, name,
		"crdb.cockroachlabs.com/cloudProvider-", "crdb.cockroachlabs.com/regionCode-")
	requirePublicServiceSQL(t, options, cluster, service)

	// This fixture's only legacy source has completed migration and the fresh
	// coexistence cluster has already been removed. Never prune storedVersions
	// while another source still needs conversion.
	names, err := k8s.RunKubectlAndGetOutputE(t, options, "get", v1beta1CrdbClusterResource,
		"--all-namespaces", "-o", "jsonpath={.items[*].metadata.name}")
	require.NoError(t, err)
	require.Equal(t, name, names, "unexpected cluster still needs conversion")
	k8s.RunKubectl(t, options, "patch", "crd", crd, "--subresource=status", "--type=json", "-p",
		`[{"op":"replace","path":"/status/storedVersions","value":["v1beta1"]}]`)
	requireJSONPathEquals(t, options, "crd", crd, "{.status.storedVersions[*]}", "v1beta1")

	t.Log("Disabling migration and checking automatic conversion cleanup")
	_, operatorChart := operator.HelmChartPaths()
	helm.Upgrade(t, &helm.Options{
		KubectlOptions: options,
		SetValues:      map[string]string{"migration.enabled": "false"},
		ExtraArgs:      map[string][]string{"upgrade": {"--reuse-values", "--wait", "--timeout=5m"}},
	}, operatorChart, autoMigrationOperatorRelease)
	k8s.RunKubectl(t, options, "rollout", "status", "deployment/cockroach-operator", "--timeout=5m")

	// Restart once more to check that startup remains safe after conversion has
	// gone, rather than only checking the first migration-disabled process.
	k8s.RunKubectl(t, options, "rollout", "restart", "deployment/cockroach-operator")
	k8s.RunKubectl(t, options, "rollout", "status", "deployment/cockroach-operator", "--timeout=5m")
	requireJSONPathEquals(t, options, "crd", crd, "{.metadata.uid}", crdUID)
	requireJSONPathEquals(t, options, "crd", crd, "{.spec.conversion.strategy}", "None")
	requireJSONPathEquals(t, options, "crd", crd, "{.spec.conversion.webhook}", "")
	requireJSONPathEquals(t, options, "crd", crd, "{.spec.versions[?(@.name=='v1alpha1')].served}", "false")
	requireJSONPathEquals(t, options, "crd", crd, "{.spec.versions[?(@.name=='v1beta1')].served}", "true")
	requireJSONPathEquals(t, options, "crd", crd, "{.spec.versions[?(@.name=='v1beta1')].storage}", "true")
	requireJSONPathEquals(t, options, "crd", crd, "{.status.storedVersions[*]}", "v1beta1")
	requireJSONPathEquals(t, options, v1beta1CrdbClusterResource, name, "{.metadata.uid}", clusterUID)
	afterSpec, err := k8s.RunKubectlAndGetOutputE(t, options, "get", v1beta1CrdbClusterResource, name,
		"-o", "jsonpath={.spec}")
	require.NoError(t, err)
	// Admission omits empty sidecar lists when it serializes the typed spec.
	// Compare API semantics so [] and an omitted list do not look like a change.
	var before, after v1beta1.CrdbClusterSpec
	require.NoError(t, json.Unmarshal([]byte(beforeSpec), &before))
	require.NoError(t, json.Unmarshal([]byte(afterSpec), &after))
	require.True(t, apiequality.Semantic.DeepEqual(before, after),
		"retiring conversion changed the adopted spec: before=%s after=%s", beforeSpec, afterSpec)
	requireOperatorSourceFieldsPreserved(t, options, name)
	requirePublicServiceSQL(t, options, cluster, service)

	t.Log("Upgrading the adopted chart without force after retiring the legacy operator")
	helmChart, _ := operator.HelmChartPaths()
	verifyHelmAdoptionSupportsUpgrade(t, options, helmChart, name, name, valuesFile)
	requireOperatorSourceFieldsPreserved(t, options, name)
	testutil.RequireCRDBToFunction(t, cluster, true)
	requirePublicServiceSQL(t, options, cluster, service)
}

func migrationReplacementReady(pod *corev1.Pod) bool {
	if pod == nil || pod.DeletionTimestamp != nil || !k8s.IsPodAvailable(pod) {
		return false
	}
	owner := metav1.GetControllerOf(pod)
	if owner == nil || owner.Kind != "CrdbNode" || owner.Name != pod.Name ||
		!strings.HasPrefix(owner.APIVersion, "crdb.cockroachlabs.com/") {
		return false
	}
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodReady {
			return condition.Status == corev1.ConditionTrue
		}
	}
	return false
}

func requireHelmChartPVCMetadata(t *testing.T, claims []corev1.PersistentVolumeClaim, replicas int) {
	t.Helper()
	count := 0
	for _, claim := range claims {
		if !strings.HasPrefix(claim.Name, "datadir-") {
			continue
		}
		count++
		require.Equal(t, "keep", claim.Labels[helmPVCMetadataKey], "legacy chart did not set PVC label")
		require.Equal(t, "keep", claim.Annotations[helmPVCMetadataKey], "legacy chart did not set PVC annotation")
	}
	require.Equal(t, replicas, count)
}

// Do not require exactly one node: a fast reconciliation may create the next
// replacement before the poll observes the first one becoming Ready.
func waitForFirstHelmReplacement(t *testing.T, options *k8s.KubectlOptions, name string) {
	t.Helper()
	require.Eventually(t, func() bool {
		nodes, err := k8s.RunKubectlAndGetOutputE(t, options, "get", "crdbnodes",
			"-l", "crdb.cockroachlabs.com/cluster="+name, "-o", "jsonpath={.items[*].metadata.name}")
		if err != nil {
			return false
		}
		for _, node := range strings.Fields(nodes) {
			pod, err := k8s.GetPodE(t, options, node)
			if err == nil && migrationReplacementReady(pod) {
				return true
			}
		}
		return false
	}, 5*time.Minute, 2*time.Second, "no replacement became Ready")
}

// Shared-port migration must leave the existing client endpoint intact, including
// ports whose public number differs from the listener number.
func requirePublicServicePreserved(t *testing.T, options *k8s.KubectlOptions, before publicSQLService) {
	t.Helper()
	require.NotNil(t, before.original)
	after, err := k8s.GetServiceE(t, options, before.name)
	require.NoError(t, err)
	require.Equal(t, before.original.UID, after.UID)
	require.Equal(t, before.original.Spec, after.Spec, "migration changed public Service %s", before.name)
	require.Equal(t, before.original.Labels, after.Labels)
	require.Equal(t, before.original.Annotations, after.Annotations)
}

func requireSharedListenerReplacement(t *testing.T, options *k8s.KubectlOptions, name, clusterName string, disableNameVerification bool) {
	t.Helper()
	nodes, err := k8s.RunKubectlAndGetOutputE(t, options, "get", "crdbnodes",
		"-l", "crdb.cockroachlabs.com/cluster="+name, "-o", "jsonpath={.items[*].metadata.name}")
	require.NoError(t, err)
	require.NotEmpty(t, nodes)
	if clusterName == "" {
		clusterName = "test"
	}
	checked := 0
	for _, node := range strings.Fields(nodes) {
		pod, err := k8s.GetPodE(t, options, node)
		if apierrors.IsNotFound(err) {
			continue
		}
		require.NoError(t, err)
		if !migrationReplacementReady(pod) {
			continue
		}
		checked++
		found := false
		for _, container := range pod.Spec.Containers {
			if container.Name != "cockroachdb" {
				continue
			}
			found = true
			command := strings.Join(append(append([]string{}, container.Command...), container.Args...), " ")
			require.NotContains(t, command, "--sql-addr", "shared listener must not open a second SQL listener")
			require.Regexp(t, `--cluster-name[= ]+`+regexp.QuoteMeta(clusterName)+`(?:\s|$)`, command)
			require.Equal(t, disableNameVerification, strings.Contains(command, "--disable-cluster-name-verification"))
		}
		require.True(t, found, "replacement %s has no database container", node)
	}
	require.Positive(t, checked, "no Ready replacement was checked")
}

func requireMigrationSourceRBACPreserved(t *testing.T, options *k8s.KubectlOptions, before map[string]map[string]interface{}) {
	t.Helper()
	rbac := make(map[string]map[string]interface{})
	for resource, snapshot := range before {
		if strings.HasPrefix(resource, "role/") || strings.HasPrefix(resource, "rolebinding/") {
			rbac[resource] = snapshot
		}
	}
	require.NotEmpty(t, rbac)
	requireMigrationSourceResourcesPreserved(t, options, rbac)
}

// Completion cleanup is best effort and may finish just after the terminal
// status write. Query metadata only, including for Secrets.
func requireMigrationMarkersRemoved(t *testing.T, options *k8s.KubectlOptions, name string) {
	t.Helper()
	const marker = "crdb.cockroachlabs.com/migration-created"
	require.Eventually(t, func() bool {
		out, err := k8s.RunKubectlAndGetOutputE(t, options, "get",
			"pods,pvc,services,configmaps,secrets,serviceaccounts,roles,rolebindings,pdb,crdbnodes,"+v1beta1CrdbClusterResource,
			"-l", marker, "-o", "name")
		if err != nil || strings.TrimSpace(out) != "" {
			t.Logf("waiting for migration-created cleanup: %s (%v)", out, err)
			return false
		}
		for _, kind := range []string{"clusterrole", "clusterrolebinding"} {
			value, err := k8s.RunKubectlAndGetOutputE(t, options, "get", kind,
				"--field-selector", "metadata.name="+options.Namespace+"-"+name, "-l", marker, "-o", "name")
			if err != nil || value != "" {
				return false
			}
		}
		out, err = k8s.RunKubectlAndGetOutputE(t, options, "get", "crdbnodes", "-o",
			"jsonpath={range .items[*]}{.metadata.annotations.crdb\\.cockroachlabs\\.com/migration-storage}{end}")
		return err == nil && out == ""
	}, 2*time.Minute, 5*time.Second, "migration bookkeeping remains after handoff")
}

func requireNoOperatorSpecOwnership(t *testing.T, options *k8s.KubectlOptions, name string) {
	t.Helper()
	raw, err := k8s.RunKubectlAndGetOutputE(t, options, "get", v1beta1CrdbClusterResource, name,
		"-o", "json", "--show-managed-fields=true")
	require.NoError(t, err)
	var object struct {
		Metadata metav1.ObjectMeta `json:"metadata"`
	}
	require.NoError(t, json.Unmarshal([]byte(raw), &object))
	for _, entry := range object.Metadata.ManagedFields {
		if !strings.Contains(entry.Manager, "cockroach-operator") || entry.FieldsV1 == nil {
			continue
		}
		var fields map[string]interface{}
		require.NoError(t, json.Unmarshal(entry.FieldsV1.Raw, &fields))
		require.NotContains(t, fields, "f:spec", "operator reclaimed spec fields after chart adoption")
	}
}

func requireDiscoveryRejoinAfterRollback(t *testing.T, options *k8s.KubectlOptions, name string, cluster testutil.CockroachCluster) {
	t.Helper()
	podName := fmt.Sprintf("%s-%d", name, cluster.DesiredNodes-1)
	original, err := k8s.GetPodE(t, options, podName)
	require.NoError(t, err)
	k8s.RunKubectl(t, options, "delete", "pod", podName, "--wait=true", "--timeout=60s")
	require.Eventually(t, func() bool {
		pod, err := k8s.GetPodE(t, options, podName)
		return err == nil && pod.UID != original.UID && k8s.IsPodAvailable(pod)
	}, 5*time.Minute, 5*time.Second, "restored StatefulSet pod did not rejoin")
	// This connection must resolve the restarted pod through the original
	// discovery Service, and verify its wildcard certificate against pod DNS.
	requirePublicServiceSQL(t, options, cluster, publicSQLService{name: podName + "." + name, port: 26257})
	testutil.RequireCRDBClusterToBeReadyEventuallyTimeout(t, options, cluster, 3*time.Minute)
	testutil.RequireCRDBToFunction(t, cluster, true)
}

// The legacy chart signs leaves with the supplied CA, but controller migration
// also needs its conventional CA Secret. Neither copy has self-signer metadata:
// export must recover caProvided from the source Helm release instead.
func createHelmSuppliedCA(t *testing.T, options *k8s.KubectlOptions, name, suppliedName string) string {
	t.Helper()
	k8s.CreateNamespace(t, options, options.Namespace)
	g := generator.NewGenerateCert(k8sClient)
	g.DiscoveryServiceName = name
	g.RotateCACert = true
	require.NoError(t, g.CaCertConfig.SetConfig("17520h", "24h"))
	require.NoError(t, g.Do(context.Background(), options.Namespace))
	ca := k8s.GetSecret(t, options, name+"-ca-secret")
	ca.Annotations = nil
	require.NoError(t, k8sClient.Update(context.Background(), ca))
	ca.ObjectMeta = metav1.ObjectMeta{Name: suppliedName, Namespace: options.Namespace}
	require.NoError(t, k8sClient.Create(context.Background(), ca))
	return migrationSecretFingerprint(t, ca)
}

// Start with the source's CA so existing clients remain trusted. Custom external
// node certificates must already include the join names before migration starts.
func configureLegacyOperatorTLSSecrets(t *testing.T, options *k8s.KubectlOptions, cluster testutil.CockroachCluster, nodeName, clientName string) testutil.CockroachCluster {
	t.Helper()
	name := cluster.StatefulSetName
	// The legacy operator keeps only ca.key in its CA Secret; ca.crt is in
	// the node Secret. Assemble a temporary signer without changing the source.
	ca := k8s.GetSecret(t, options, cluster.CaSecret)
	node := k8s.GetSecret(t, options, cluster.NodeSecret)
	fixtureCA := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name + "-fixture-ca", Namespace: options.Namespace},
		Type:       corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			"ca.key": ca.Data["ca.key"],
			"ca.crt": node.Data["ca.crt"],
		},
	}
	require.NotEmpty(t, fixtureCA.Data["ca.key"])
	require.NotEmpty(t, fixtureCA.Data["ca.crt"])
	require.NoError(t, k8sClient.Create(context.Background(), fixtureCA))
	defer func() { require.NoError(t, k8sClient.Delete(context.Background(), fixtureCA)) }()
	g := generator.NewGenerateCert(k8sClient)
	g.CaSecret = fixtureCA.Name
	g.DiscoveryServiceName, g.PublicServiceName, g.ClusterDomain = name, name+"-public", "cluster.local"
	g.AdditionalSANs = []string{name + "-join", name + "-join." + options.Namespace,
		name + "-join." + options.Namespace + ".svc.cluster.local"}
	require.NoError(t, g.NodeCertConfig.SetConfig("8760h", "24h"))
	require.NoError(t, g.ClientCertConfig.SetConfig("8760h", "24h"))
	require.NoError(t, g.Do(context.Background(), options.Namespace))
	generatedNode := k8s.GetSecret(t, options, name+"-node-secret")
	if nodeName != "" {
		generatedNode.ObjectMeta = metav1.ObjectMeta{Name: nodeName, Namespace: options.Namespace}
		require.NoError(t, k8sClient.Create(context.Background(), generatedNode))
		cluster.NodeSecret = nodeName
	} else {
		// A client-only override keeps the node Secret that the source mounts.
		// Add join names there without changing its identity or signing CA.
		node := k8s.GetSecret(t, options, cluster.NodeSecret)
		node.Data = generatedNode.Data
		require.NoError(t, k8sClient.Update(context.Background(), node))
	}
	if clientName != "" {
		client := k8s.GetSecret(t, options, cluster.ClientSecret)
		client.ObjectMeta = metav1.ObjectMeta{Name: clientName, Namespace: options.Namespace}
		require.NoError(t, k8sClient.Create(context.Background(), client))
		cluster.ClientSecret = clientName
	}
	// Remove the temporary generated leaves so a wrong export cannot pass by
	// referencing unused Secrets that happen to exist.
	k8s.RunKubectl(t, options, "delete", "secret", name+"-node-secret", name+"-client-secret")
	patch, err := json.Marshal(map[string]interface{}{"spec": map[string]string{
		"nodeTLSSecret": nodeName, "clientTLSSecret": clientName,
	}})
	require.NoError(t, err)
	k8s.RunKubectl(t, options, "patch", v1alpha1CrdbClusterResource, name, "--type=merge", "-p", string(patch))
	require.Eventually(t, func() bool {
		for i := 0; i < cluster.DesiredNodes; i++ {
			pod, err := k8s.GetPodE(t, options, fmt.Sprintf("%s-%d", name, i))
			if err != nil || pod.DeletionTimestamp != nil || !k8s.IsPodAvailable(pod) {
				return false
			}
			nodeMounted, clientMounted := false, false
			for _, volume := range pod.Spec.Volumes {
				if volume.Name != "certs" || volume.Projected == nil {
					continue
				}
				for _, source := range volume.Projected.Sources {
					if source.Secret != nil {
						nodeMounted = nodeMounted || source.Secret.Name == cluster.NodeSecret
						clientMounted = clientMounted || source.Secret.Name == cluster.ClientSecret
					}
				}
			}
			if !nodeMounted || !clientMounted {
				return false
			}
		}
		return true
	}, 10*time.Minute, 5*time.Second, "legacy pods did not mount the configured TLS Secrets")
	return cluster
}

func requireExternalCertificateExport(t *testing.T, options *k8s.KubectlOptions, name, valuesFile string, cluster testutil.CockroachCluster) {
	t.Helper()
	data, err := os.ReadFile(valuesFile)
	require.NoError(t, err)
	var values struct {
		CockroachDB struct {
			TLS struct {
				SelfSigner struct{ Enabled bool } `yaml:"selfSigner"`
				External   struct {
					Enabled      bool
					Certificates map[string]string `yaml:"certificates"`
				} `yaml:"externalCertificates"`
			} `yaml:"tls"`
		} `yaml:"cockroachdb"`
	}
	require.NoError(t, yaml.Unmarshal(data, &values))
	require.False(t, values.CockroachDB.TLS.SelfSigner.Enabled)
	require.True(t, values.CockroachDB.TLS.External.Enabled)
	for field, expected := range externalCertificateNames(name, cluster) {
		require.Equal(t, expected, values.CockroachDB.TLS.External.Certificates[field], "exported %s", field)
	}
	requireExternalCertificateReferences(t, options, name, cluster)
}

func externalCertificateNames(name string, cluster testutil.CockroachCluster) map[string]string {
	return map[string]string{
		"caConfigMapName": name + "-ca-crt", "nodeSecretName": cluster.NodeSecret,
		"rootSqlClientSecretName": cluster.ClientSecret, "httpSecretName": cluster.ClientSecret,
	}
}

func requireExternalCertificateReferences(t *testing.T, options *k8s.KubectlOptions, name string, cluster testutil.CockroachCluster) {
	t.Helper()
	for field, expected := range externalCertificateNames(name, cluster) {
		requireJSONPathEquals(t, options, v1beta1CrdbClusterResource, name,
			"{.spec.template.spec.certificates.externalCertificates."+field+"}", expected)
	}
}

// Generate user-owned certificates without leaving their CA signing key in the
// cluster. Both layouts are valid for the legacy chart; only TLS keys work on
// the migration target. Include join SANs because external leaves are not reissued.
func createHelmProvidedCertificates(t *testing.T, options *k8s.KubectlOptions, name string, tlsLayout bool) map[string]corev1.Secret {
	t.Helper()
	k8s.CreateNamespace(t, options, options.Namespace)
	g := generator.NewGenerateCert(k8sClient)
	g.DiscoveryServiceName, g.PublicServiceName, g.ClusterDomain = name, name+"-public", "cluster.local"
	g.AdditionalSANs = []string{name + "-join", name + "-join." + options.Namespace,
		name + "-join." + options.Namespace + ".svc.cluster.local"}
	require.NoError(t, g.CaCertConfig.SetConfig("17520h", "24h"))
	require.NoError(t, g.NodeCertConfig.SetConfig("8760h", "24h"))
	require.NoError(t, g.ClientCertConfig.SetConfig("8760h", "24h"))
	require.NoError(t, g.Do(context.Background(), options.Namespace))
	secrets := make(map[string]corev1.Secret)
	for _, suffix := range []string{"-ca-secret", "-node-secret", "-client-secret"} {
		var secret corev1.Secret
		require.NoError(t, k8sClient.Get(context.Background(), types.NamespacedName{Namespace: options.Namespace, Name: name + suffix}, &secret))
		delete(secret.Data, "ca.key")
		secret.Annotations = nil
		if !tlsLayout && suffix != "-ca-secret" {
			certKey, privateKey := "node.crt", "node.key"
			if suffix == "-client-secret" {
				certKey, privateKey = "client.root.crt", "client.root.key"
			}
			secret.Data[certKey], secret.Data[privateKey] = secret.Data[corev1.TLSCertKey], secret.Data[corev1.TLSPrivateKeyKey]
			delete(secret.Data, corev1.TLSCertKey)
			delete(secret.Data, corev1.TLSPrivateKeyKey)
			// Secret types are immutable. Recreate the source fixture before
			// any pods use it, rather than trying to update its type.
			require.NoError(t, k8sClient.Delete(context.Background(), &secret))
			secret.ObjectMeta = metav1.ObjectMeta{Name: secret.Name, Namespace: secret.Namespace}
			secret.Type = corev1.SecretTypeOpaque
			require.NoError(t, k8sClient.Create(context.Background(), &secret))
		} else {
			require.NoError(t, k8sClient.Update(context.Background(), &secret))
		}
		secrets[secret.Name] = secret
	}
	return secrets
}

func testHelmAutoProvidedCertificates(t *testing.T, tlsLayout, sighup bool) {
	ns := "cockroach" + strings.ToLower(random.UniqueId())
	options := migrationKubectlOptions(ns)
	cleanupPublicOperatorResources(t)
	name := releaseName + "-cockroachdb"
	operatorInstalled := false
	defer func() {
		testutil.CaptureFailureArtifacts(t, options)
		_ = os.RemoveAll(manifestsDirPath)
		_ = helm.DeleteE(t, &helm.Options{KubectlOptions: options}, releaseName, true)
		if operatorInstalled {
			_ = k8s.RunKubectlE(t, options, "delete", v1beta1CrdbClusterResource, "--all", "--ignore-not-found", "--timeout=60s")
			waitForClusterResourcesGone(t, options)
			uninstallMigrationOperator(t, options)
		}
		k8s.DeleteNamespace(t, options, ns)
	}()
	secrets := createHelmProvidedCertificates(t, options, name, tlsLayout)
	_, cluster := installHelmSourceCluster(t, options, ns, map[string]string{
		"tls.certs.selfSigner.enabled": "false", "tls.certs.provided": "true",
		"tls.certs.tlsSecret": fmt.Sprint(tlsLayout), "tls.enableSighupRotation": fmt.Sprint(sighup),
		"tls.certs.caSecret": name + "-ca-secret", "tls.certs.nodeSecret": name + "-node-secret",
		"tls.certs.clientRootSecret": name + "-client-secret",
	})
	testutil.RequireCRDBClusterToBeReadyEventuallyTimeout(t, options, cluster, 10*time.Minute)
	service := snapshotPublicSQLService(t, options, name)
	// Seed through the public endpoint too: the generic database helper only
	// reads TLS-layout client Secrets, whereas the negative fixture uses raw keys.
	runPublicServiceSQL(t, options, cluster, service,
		"CREATE DATABASE IF NOT EXISTS test_db; CREATE TABLE IF NOT EXISTS test_db.accounts (id INT PRIMARY KEY, balance INT); "+
			"INSERT INTO test_db.accounts VALUES (1,1000),(2,250) ON CONFLICT DO NOTHING", "")
	requirePublicServiceSQL(t, options, cluster, service)
	claims := snapshotMigrationPVCs(t, options)
	requireHelmChartPVCMetadata(t, claims, cluster.DesiredNodes)
	var sourcePods []corev1.Pod
	for i := 0; i < cluster.DesiredNodes; i++ {
		pod, err := k8s.GetPodE(t, options, fmt.Sprintf("%s-%d", name, i))
		require.NoError(t, err)
		sourcePods = append(sourcePods, *pod)
	}
	operatorInstalled = true
	installMigrationOperator(t, options, ns)
	k8s.RunKubectl(t, options, "label", "sts", name, "crdb.io/migrate=start")
	if !tlsLayout {
		waitForMigrationPhase(t, options, name, phaseCertMigration, 5*time.Minute)
		require.Eventually(t, func() bool {
			lastError, err := k8s.RunKubectlAndGetOutputE(t, options, "get", v1beta1CrdbClusterResource, name,
				"-o", "jsonpath={.status.migration.lastError}")
			t.Logf("certificate migration diagnostic: %s (%v)", lastError, err)
			return err == nil && strings.Contains(lastError, "tls.crt/tls.key")
		}, 3*time.Minute, 5*time.Second, "missing diagnostic for unsupported Secret key layout")
		requireJSONPathEquals(t, options, v1beta1CrdbClusterResource, name, "{.status.migration.phase}", phaseCertMigration)
		waitForCrdbNodeCount(t, options, name, 0, time.Minute)
		requireJSONPathEquals(t, options, "sts", name, "{.spec.replicas}", "3")
		for _, original := range sourcePods {
			pod, err := k8s.GetPodE(t, options, original.Name)
			require.NoError(t, err)
			require.Equal(t, original.UID, pod.UID, "certificate failure replaced a source pod")
		}
		k8s.RunKubectl(t, options, "label", "sts", name, "crdb.io/migrate-")
		requireRollbackArtifactsRemoved(t, options, name, false)
	} else {
		waitForCrdbNodeCount(t, options, name, cluster.DesiredNodes, 20*time.Minute)
		waitForSettledFinalization(t, options, name)
		requirePublicServicePreserved(t, options, service)
		requirePublicServiceSQL(t, options, cluster, service)
		deleteStatefulSetAndWaitGone(t, options, name, 3*time.Minute)
		waitForMigrationPhase(t, options, name, phaseComplete, 3*time.Minute)
		requireMigrationComplete(t, options, name)
		valuesFile := exportValuesForHelmAdoption(t, name, ns)
		data, err := os.ReadFile(valuesFile)
		require.NoError(t, err)
		var values struct {
			CockroachDB struct {
				TLS struct {
					SelfSigner struct{ Enabled bool } `yaml:"selfSigner"`
					External   struct {
						Enabled      bool
						Certificates struct {
							NodeSecret   string `yaml:"nodeSecretName"`
							ClientSecret string `yaml:"rootSqlClientSecretName"`
						} `yaml:"certificates"`
					} `yaml:"externalCertificates"`
				} `yaml:"tls"`
			} `yaml:"cockroachdb"`
		}
		require.NoError(t, yaml.Unmarshal(data, &values))
		require.False(t, values.CockroachDB.TLS.SelfSigner.Enabled)
		require.True(t, values.CockroachDB.TLS.External.Enabled)
		require.Equal(t, cluster.NodeSecret, values.CockroachDB.TLS.External.Certificates.NodeSecret)
		require.Equal(t, cluster.ClientSecret, values.CockroachDB.TLS.External.Certificates.ClientSecret)
		adoptIntoCockroachDBHelmChart(t, options, name, releaseName, ns, valuesFile)
		jobs, err := k8s.RunKubectlAndGetOutputE(t, options, "get", "cronjobs", "-o", "name")
		require.NoError(t, err)
		require.Empty(t, jobs, "provided certificates must not gain self-signer rotation jobs")
	}
	for secretName, original := range secrets {
		secret, err := k8s.GetSecretE(t, options, secretName)
		require.NoError(t, err)
		require.Equal(t, original.UID, secret.UID)
		require.Equal(t, migrationSecretFingerprint(t, &original), migrationSecretFingerprint(t, secret), "provided certificate changed")
	}
	requireMigrationPVCsPreserved(t, options, claims)
	requirePublicServiceSQL(t, options, cluster, service)
}
