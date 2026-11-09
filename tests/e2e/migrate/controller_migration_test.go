package migrate

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/cockroachdb/helm-charts/pkg/upstream/cockroach-operator/api/v1beta1"
	"github.com/cockroachdb/helm-charts/tests/e2e/operator"
	"github.com/cockroachdb/helm-charts/tests/testutil"
	"github.com/cockroachdb/helm-charts/tests/testutil/migration"
	"github.com/gruntwork-io/terratest/modules/helm"
	"github.com/gruntwork-io/terratest/modules/k8s"
	"github.com/gruntwork-io/terratest/modules/random"
	"github.com/gruntwork-io/terratest/modules/retry"
	"github.com/gruntwork-io/terratest/modules/shell"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
)

const (
	autoMigrationOperatorRelease = "crdb-migration-operator"
	v1alpha1CrdbClusterResource  = "crdbclusters.v1alpha1.crdb.cockroachlabs.com"
	v1beta1CrdbClusterResource   = "crdbclusters.v1beta1.crdb.cockroachlabs.com"

	// Finalization starts after the controller has completed the PodMigration
	// health checks. Those checks allow up to 10 minutes for a migrated CrdbNode
	// to become healthy before the controller auto-pauses the migration.
	phaseFinalizationTimeout = 15 * time.Minute

	// Migration phases as reported in status.migration.phase.
	phaseCertMigration = "CertMigration"
	phasePodMigration  = "PodMigration"
	phaseFinalization  = "Finalization"
	phaseComplete      = "Complete"
	phaseStopped       = "Stopped"

	// Migration label values set by the controller on the source resource.
	migrateLabelStopped  = "stopped"
	migrateLabelComplete = "complete"

	cloudProvider = "k3d"
	cloudRegion   = "us-east-1"
)

var migrationPhaseOrder = map[string]int{
	phaseCertMigration: 1,
	phasePodMigration:  2,
	phaseFinalization:  3,
	phaseComplete:      4,
}

// TestAutoMigration tests the automatic migration controller for both Helm and
// public-operator source clusters. Each subtest runs in its own namespace with
// its own migration operator scoped via watchNamespaces.
func TestAutoMigration(t *testing.T) {
	ensureMigrationTestEnv(t)

	t.Run("helm forward migration", testHelmAutoForwardMigration)
	t.Run("helm forward with shell cluster name", func(t *testing.T) {
		testHelmAutoForwardMigrationWithValues(t, map[string]string{"conf.cluster-name": "$(printf test)"}, true)
	})
	t.Run("helm rollback with shell cluster name", func(t *testing.T) {
		testHelmAutoRollbackFromPhase(t, false, map[string]string{"conf.cluster-name": "$(printf test)"})
	})
	t.Run("helm provided self-signer CA", func(t *testing.T) {
		testHelmAutoForwardMigrationWithValues(t, map[string]string{
			"tls.certs.selfSigner.caProvided": "true", "tls.certs.selfSigner.caSecret": "supplied-ca",
		}, true)
	})
	t.Run("helm stop and resume", testHelmAutoStopResume)
	t.Run("helm rollback", testHelmAutoRollback)
	t.Run("helm forward without cluster name verification", func(t *testing.T) {
		testHelmAutoForwardMigrationWithValues(t, map[string]string{"conf.disable-cluster-name-verification": "true"}, true)
	})
	t.Run("helm rollback without cluster name verification", func(t *testing.T) {
		testHelmAutoRollbackFromPhase(t, false, map[string]string{"conf.disable-cluster-name-verification": "true"})
	})
	t.Run("helm rollback during finalization", func(t *testing.T) {
		testHelmAutoRollbackFromPhase(t, true, nil)
	})
	t.Run("helm shared external port migration", func(t *testing.T) {
		// A distinct client-facing port is supported by migration. Adoption needs
		// separate Service management, as documented; do not silently normalize it.
		testHelmAutoForwardMigrationWithValues(t, map[string]string{"service.ports.grpc.external.port": "30057"}, false)
	})
	t.Run("helm provided TLS certificates", func(t *testing.T) {
		testHelmAutoProvidedCertificates(t, true, false)
	})
	t.Run("helm provided TLS certificates with SIGHUP", func(t *testing.T) {
		testHelmAutoProvidedCertificates(t, true, true)
	})
	t.Run("helm provided non-TLS Secret layout blocked", func(t *testing.T) {
		testHelmAutoProvidedCertificates(t, false, false)
	})
	t.Run("helm insecure forward migration", func(t *testing.T) { testHelmAutoInsecureMigration(t, false) })
	t.Run("helm insecure rollback", func(t *testing.T) { testHelmAutoInsecureMigration(t, true) })
	t.Run("operator forward migration", testOperatorAutoForwardMigration)
	for _, mode := range []string{"node", "client", "both"} {
		t.Run("operator custom TLS Secrets "+mode, func(t *testing.T) {
			node, client := "", ""
			if mode != "client" {
				node = "custom-node"
			}
			if mode != "node" {
				client = "custom-client"
			}
			testOperatorAutoForwardMigrationWithTLSSecrets(t, node, client)
		})
	}
	t.Run("operator stop and resume", testOperatorAutoStopResume)
	t.Run("operator rollback", testOperatorAutoRollback)
	t.Run("helm cert-manager migration", testHelmAutoCertManagerMigration)
}

// testHelmAutoForwardMigration installs a Helm StatefulSet cluster, runs the full
// automatic migration to completion, validates data integrity, then adopts it via
// the Helm chart.
func testHelmAutoForwardMigration(t *testing.T) {
	testHelmAutoForwardMigrationWithValues(t, nil, true)
}

func testHelmAutoForwardMigrationWithValues(t *testing.T, sourceValues map[string]string, adopt bool) {
	ns := "cockroach" + strings.ToLower(random.UniqueId())
	kubectlOptions := migrationKubectlOptions(ns)

	cleanupPublicOperatorResources(t)
	operatorInstalled := false
	defer func() {
		_ = os.RemoveAll(manifestsDirPath)
	}()
	defer func() {
		testutil.CaptureFailureArtifacts(t, kubectlOptions)
		_ = helm.DeleteE(t, &helm.Options{KubectlOptions: kubectlOptions}, releaseName, true)
		if operatorInstalled {
			_ = k8s.RunKubectlE(t, kubectlOptions, "delete", v1beta1CrdbClusterResource, "--all", "--ignore-not-found=true", "--timeout=60s")
			waitForClusterResourcesGone(t, kubectlOptions)
			uninstallMigrationOperator(t, kubectlOptions)
		}
		k8s.DeleteNamespace(t, kubectlOptions, ns)
	}()

	var suppliedCAFingerprint string
	if sourceValues["tls.certs.selfSigner.caProvided"] == "true" {
		suppliedCAFingerprint = createHelmSuppliedCA(t, kubectlOptions, releaseName+"-cockroachdb", sourceValues["tls.certs.selfSigner.caSecret"])
	}
	stsName, crdbCluster := installHelmSourceCluster(t, kubectlOptions, ns, sourceValues)

	testutil.RequireCRDBClusterToBeReadyEventuallyTimeout(t, kubectlOptions, crdbCluster, 600*time.Second)
	testutil.RequireCRDBToFunction(t, crdbCluster, false)
	sourceIdentity := snapshotSourceIdentity(t, kubectlOptions, stsName)
	publicService := snapshotPublicSQLService(t, kubectlOptions, stsName)
	if sourceValues["service.ports.grpc.external.port"] == "" {
		require.EqualValues(t, 26257, publicService.port, "fixture must exercise the legacy shared SQL/gRPC listener")
	}
	requirePublicServiceSQL(t, kubectlOptions, crdbCluster, publicService)
	originalPVCs := snapshotMigrationPVCs(t, kubectlOptions)
	requireHelmChartPVCMetadata(t, originalPVCs, crdbCluster.DesiredNodes)
	sourceResources := snapshotMigrationSourceResources(t, kubectlOptions,
		"poddisruptionbudget/"+stsName+"-budget", "role/"+stsName, "rolebinding/"+stsName)

	operatorInstalled = true
	installMigrationOperator(t, kubectlOptions, ns)

	// Start migration.
	k8s.RunKubectl(t, kubectlOptions, "label", "sts", stsName, "crdb.io/migrate=start")

	// Wait for migration controller to create CrdbCluster and advance through phases.
	waitForMigrationPhase(t, kubectlOptions, stsName, phaseCertMigration, 5*time.Minute)
	waitForMigrationPhase(t, kubectlOptions, stsName, phasePodMigration, 10*time.Minute)
	waitForFirstHelmReplacement(t, kubectlOptions, stsName)
	requireSharedListenerReplacement(t, kubectlOptions, stsName, sourceValues["conf.cluster-name"], sourceValues["conf.disable-cluster-name-verification"] == "true")
	requirePublicServicePreserved(t, kubectlOptions, publicService)
	requirePublicServiceSQL(t, kubectlOptions, crdbCluster, publicService)

	// Verify CrdbNodes are being created one by one and cluster stays healthy.
	waitForCrdbNodeCount(t, kubectlOptions, stsName, crdbCluster.DesiredNodes, 20*time.Minute)

	waitForSettledFinalization(t, kubectlOptions, stsName)
	requireMigrationSourceResourcesPreserved(t, kubectlOptions, sourceResources)
	requireSharedListenerReplacement(t, kubectlOptions, stsName, sourceValues["conf.cluster-name"], sourceValues["conf.disable-cluster-name-verification"] == "true")
	requirePublicServicePreserved(t, kubectlOptions, publicService)
	requireMigrationPVCsPreserved(t, kubectlOptions, originalPVCs)
	requirePublicServiceSQL(t, kubectlOptions, crdbCluster, publicService)

	// Finalization complete: delete the StatefulSet to trigger Phase=Complete.
	t.Log("Deleting StatefulSet to trigger migration completion")
	deleteStatefulSetAndWaitGone(t, kubectlOptions, stsName, 3*time.Minute)

	waitForMigrationPhase(t, kubectlOptions, stsName, phaseComplete, 3*time.Minute)

	// Assert final state.
	requireMigrationComplete(t, kubectlOptions, stsName)
	requirePublicServicePreserved(t, kubectlOptions, publicService)
	requireMigrationSourceRBACPreserved(t, kubectlOptions, sourceResources)
	requireHelmSourceFieldsPreserved(t, kubectlOptions, stsName)
	requireSourceIdentity(t, kubectlOptions, stsName, sourceIdentity)

	// Validate data survived migration.
	crdbCluster.ClientSecret = fmt.Sprintf("%s-client-secret", stsName)
	crdbCluster.NodeSecret = fmt.Sprintf("%s-node-secret", stsName)
	testutil.RequireCRDBToFunction(t, crdbCluster, true)
	requirePublicServiceSQL(t, kubectlOptions, crdbCluster, publicService)
	if !adopt {
		return
	}

	valuesFile := exportValuesForHelmAdoption(t, stsName, ns)
	customSAN := "migration-client." + ns + ".svc.cluster.local"
	certificatesBeforeAdoption := requireMigrationSelfSignerExport(t, kubectlOptions, stsName, valuesFile, sourceValues["tls.certs.selfSigner.caSecret"],
		"migration.example.test", "192.0.2.10", customSAN)

	// Adopt into Helm chart with values generated from the live migrated v1beta1
	// cluster so takeover reflects the actual controller output.
	adoptIntoCockroachDBHelmChart(t, kubectlOptions, stsName, releaseName, ns, valuesFile)
	requireMigrationSelfSignerAdoption(t, kubectlOptions, stsName, crdbCluster.DesiredNodes, certificatesBeforeAdoption)
	service, err := k8s.GetServiceE(t, kubectlOptions, publicService.name)
	require.NoError(t, err)
	var sharedPorts []corev1.ServicePort
	for _, port := range service.Spec.Ports {
		if port.Port == publicService.port {
			sharedPorts = append(sharedPorts, port)
		}
	}
	require.Len(t, sharedPorts, 1, "adoption must render one entry for the shared listener")
	require.Equal(t, "grpc", sharedPorts[0].TargetPort.String())
	requireHelmSourceFieldsPreserved(t, kubectlOptions, stsName)
	requireSourceIdentity(t, kubectlOptions, stsName, sourceIdentity)
	testutil.RequireCRDBToFunction(t, crdbCluster, true)
	requirePublicServiceSQL(t, kubectlOptions, crdbCluster, publicService)
	requireMigrationPVCsPreserved(t, kubectlOptions, originalPVCs)
	customEndpoint := publicSQLService{name: "migration-client", port: publicService.port}
	requirePublicServiceSQL(t, kubectlOptions, crdbCluster, customEndpoint)
	requireMigrationSelfSignerRotation(t, kubectlOptions, stsName, crdbCluster, certificatesBeforeAdoption,
		"migration.example.test", "192.0.2.10", customSAN)
	requirePublicServiceSQL(t, kubectlOptions, crdbCluster, publicService)
	requirePublicServiceSQL(t, kubectlOptions, crdbCluster, customEndpoint)
	requireSourceIdentity(t, kubectlOptions, stsName, sourceIdentity)
	if suppliedCAFingerprint != "" {
		require.Equal(t, suppliedCAFingerprint, migrationSecretFingerprint(t,
			k8s.GetSecret(t, kubectlOptions, sourceValues["tls.certs.selfSigner.caSecret"])), "migration or adoption replaced the supplied CA")
	}
}

// testHelmAutoStopResume migrates a Helm cluster, stops mid-way, verifies the cluster
// stays functional during the pause, then resumes to completion.
func testHelmAutoStopResume(t *testing.T) {
	ns := "cockroach" + strings.ToLower(random.UniqueId())
	kubectlOptions := migrationKubectlOptions(ns)

	cleanupPublicOperatorResources(t)
	stsName, crdbCluster := installHelmSourceCluster(t, kubectlOptions, ns)
	defer func() {
		_ = os.RemoveAll(manifestsDirPath)
	}()
	defer func() {
		testutil.CaptureFailureArtifacts(t, kubectlOptions)
		_ = helm.DeleteE(t, &helm.Options{KubectlOptions: kubectlOptions}, releaseName, true)
		_ = k8s.RunKubectlE(t, kubectlOptions, "delete", v1beta1CrdbClusterResource, "--all", "--ignore-not-found=true", "--timeout=60s")
		waitForClusterResourcesGone(t, kubectlOptions)
		uninstallMigrationOperator(t, kubectlOptions)
		k8s.DeleteNamespace(t, kubectlOptions, ns)
	}()

	testutil.RequireCRDBClusterToBeReadyEventuallyTimeout(t, kubectlOptions, crdbCluster, 600*time.Second)
	testutil.RequireCRDBToFunction(t, crdbCluster, false)
	publicService := snapshotPublicSQLService(t, kubectlOptions, stsName)
	requirePublicServiceSQL(t, kubectlOptions, crdbCluster, publicService)

	installMigrationOperator(t, kubectlOptions, ns)

	k8s.RunKubectl(t, kubectlOptions, "label", "sts", stsName, "crdb.io/migrate=start")
	waitForMigrationPhase(t, kubectlOptions, stsName, phasePodMigration, 10*time.Minute)

	// Wait for at least 1 CrdbNode before stopping.
	waitForCrdbNodeCount(t, kubectlOptions, stsName, 1, 5*time.Minute)

	// Stop migration.
	k8s.RunKubectl(t, kubectlOptions, "label", "sts", stsName, "crdb.io/migrate=stop", "--overwrite")
	waitForMigrationPhase(t, kubectlOptions, stsName, phaseStopped, 2*time.Minute)
	waitForSTSMigrationLabel(t, kubectlOptions, stsName, migrateLabelStopped, 2*time.Minute)

	// Cluster must remain functional while paused.
	crdbCluster.ClientSecret = fmt.Sprintf("%s-client-secret", stsName)
	crdbCluster.NodeSecret = fmt.Sprintf("%s-node-secret", stsName)
	testutil.RequireCRDBToFunction(t, crdbCluster, true)
	requirePublicServiceSQL(t, kubectlOptions, crdbCluster, publicService)

	// Resume migration.
	k8s.RunKubectl(t, kubectlOptions, "label", "sts", stsName, "crdb.io/migrate=start", "--overwrite")
	waitForMigrationResume(t, kubectlOptions, stsName)
	waitForCrdbNodeCount(t, kubectlOptions, stsName, crdbCluster.DesiredNodes, 20*time.Minute)
	waitForSettledFinalization(t, kubectlOptions, stsName)

	// Per the migration docs, the source StatefulSet must be deleted to move
	// from Finalization to Complete.
	deleteStatefulSetAndWaitGone(t, kubectlOptions, stsName, 3*time.Minute)
	waitForMigrationPhase(t, kubectlOptions, stsName, phaseComplete, 3*time.Minute)
	requireMigrationComplete(t, kubectlOptions, stsName)
	requireHelmSourceFieldsPreserved(t, kubectlOptions, stsName)
	testutil.RequireCRDBToFunction(t, crdbCluster, true)
	requirePublicServiceSQL(t, kubectlOptions, crdbCluster, publicService)

	valuesFile := exportValuesForHelmAdoption(t, stsName, ns)
	adoptIntoCockroachDBHelmChart(t, kubectlOptions, stsName, releaseName, ns, valuesFile)
	requireHelmSourceFieldsPreserved(t, kubectlOptions, stsName)
	testutil.RequireCRDBToFunction(t, crdbCluster, true)
	requirePublicServiceSQL(t, kubectlOptions, crdbCluster, publicService)
}

// testHelmAutoRollback starts migration, waits until at least one node is replaced,
// then triggers rollback and verifies the StatefulSet is fully restored.
func testHelmAutoRollback(t *testing.T) {
	testHelmAutoRollbackFromPhase(t, false, nil)
}

func testHelmAutoRollbackFromPhase(t *testing.T, finalization bool, sourceValues map[string]string) {
	ns := "cockroach" + strings.ToLower(random.UniqueId())
	kubectlOptions := migrationKubectlOptions(ns)

	cleanupPublicOperatorResources(t)
	stsName, crdbCluster := installHelmSourceCluster(t, kubectlOptions, ns, sourceValues)
	defer func() {
		_ = os.RemoveAll(manifestsDirPath)
	}()
	defer func() {
		testutil.CaptureFailureArtifacts(t, kubectlOptions)
		_ = helm.DeleteE(t, &helm.Options{KubectlOptions: kubectlOptions}, releaseName, true)
		// Rollback does not adopt into Helm, so explicitly delete CRs before
		// waiting. The operator is still running and will process finalizers.
		_ = k8s.RunKubectlE(t, kubectlOptions, "delete", "crdbclusters.crdb.cockroachlabs.com", "--all", "--ignore-not-found=true")
		waitForClusterResourcesGone(t, kubectlOptions)
		uninstallMigrationOperator(t, kubectlOptions)
		k8s.DeleteNamespace(t, kubectlOptions, ns)
	}()

	testutil.RequireCRDBClusterToBeReadyEventuallyTimeout(t, kubectlOptions, crdbCluster, 600*time.Second)
	testutil.RequireCRDBToFunction(t, crdbCluster, false)
	publicService := snapshotPublicSQLService(t, kubectlOptions, stsName)
	requirePublicServiceSQL(t, kubectlOptions, crdbCluster, publicService)
	originalPVCs := snapshotMigrationPVCs(t, kubectlOptions)
	requireHelmChartPVCMetadata(t, originalPVCs, crdbCluster.DesiredNodes)
	sourceResources := snapshotMigrationSourceResources(t, kubectlOptions,
		"poddisruptionbudget/"+stsName+"-budget", "role/"+stsName, "rolebinding/"+stsName,
		"service/"+stsName)

	installMigrationOperator(t, kubectlOptions, ns)

	k8s.RunKubectl(t, kubectlOptions, "label", "sts", stsName, "crdb.io/migrate=start")
	waitForMigrationPhase(t, kubectlOptions, stsName, phasePodMigration, 10*time.Minute)
	// At least 1 CrdbNode must exist before rolling back.
	waitForFirstHelmReplacement(t, kubectlOptions, stsName)
	requireSharedListenerReplacement(t, kubectlOptions, stsName, sourceValues["conf.cluster-name"], sourceValues["conf.disable-cluster-name-verification"] == "true")
	requirePublicServicePreserved(t, kubectlOptions, publicService)
	requirePublicServiceSQL(t, kubectlOptions, crdbCluster, publicService)
	if finalization {
		waitForCrdbNodeCount(t, kubectlOptions, stsName, crdbCluster.DesiredNodes, 20*time.Minute)
		waitForSettledFinalization(t, kubectlOptions, stsName)
		requireSharedListenerReplacement(t, kubectlOptions, stsName, sourceValues["conf.cluster-name"], sourceValues["conf.disable-cluster-name-verification"] == "true")
		requirePublicServiceSQL(t, kubectlOptions, crdbCluster, publicService)
	}
	requireMigrationSourceResourcesPreserved(t, kubectlOptions, sourceResources)

	// Remove the label to trigger rollback.
	k8s.RunKubectl(t, kubectlOptions, "label", "sts", stsName, "crdb.io/migrate-")

	// Verify rollback: STS restored to original replicas, all CrdbNodes deleted.
	waitForSTSReplicas(t, kubectlOptions, stsName, crdbCluster.DesiredNodes, 10*time.Minute)
	waitForCrdbNodeCount(t, kubectlOptions, stsName, 0, 5*time.Minute)

	requireRollbackArtifactsRemoved(t, kubectlOptions, stsName, false)
	requireMigrationSourceResourcesPreserved(t, kubectlOptions, sourceResources)
	requireJSONPathEquals(t, kubectlOptions, "sts", stsName, "{.metadata.labels.crdb\\.io/migrate}", "")

	// Cluster must be healthy after rollback.
	testutil.RequireCRDBClusterToBeReadyEventuallyTimeout(t, kubectlOptions, crdbCluster, 300*time.Second)
	testutil.RequireCRDBToFunction(t, crdbCluster, true)
	requirePublicServiceSQL(t, kubectlOptions, crdbCluster, publicService)
	requireMigrationPVCsPreserved(t, kubectlOptions, originalPVCs)
	requirePublicServicePreserved(t, kubectlOptions, publicService)
	requireDiscoveryRejoinAfterRollback(t, kubectlOptions, stsName, crdbCluster)
}

// testHelmAutoInsecureMigration exercises both outcomes without introducing TLS
// credentials: migrate and adopt the exported values, or restore the source.
func testHelmAutoInsecureMigration(t *testing.T, rollback bool) {
	ns := "cockroach" + strings.ToLower(random.UniqueId())
	options := migrationKubectlOptions(ns)
	cleanupPublicOperatorResources(t)
	name, cluster := installHelmSourceCluster(t, options, ns, map[string]string{
		"tls.enabled": "false", "tls.certs.selfSigner.enabled": "false",
	})
	defer func() {
		testutil.CaptureFailureArtifacts(t, options)
		_ = os.RemoveAll(manifestsDirPath)
		_ = helm.DeleteE(t, &helm.Options{KubectlOptions: options}, releaseName, true)
		_ = k8s.RunKubectlE(t, options, "delete", v1beta1CrdbClusterResource, "--all", "--ignore-not-found=true")
		waitForClusterResourcesGone(t, options)
		uninstallMigrationOperator(t, options)
		k8s.DeleteNamespace(t, options, ns)
	}()
	testutil.RequireCRDBClusterToBeReadyEventuallyTimeout(t, options, cluster, 600*time.Second)
	testutil.RequireCRDBToFunction(t, cluster, false)
	service := snapshotPublicSQLService(t, options, name)
	requirePublicServiceSQL(t, options, cluster, service)
	claims := snapshotMigrationPVCs(t, options)
	requireHelmChartPVCMetadata(t, claims, cluster.DesiredNodes)
	installMigrationOperator(t, options, ns)
	k8s.RunKubectl(t, options, "label", "sts", name, "crdb.io/migrate=start")
	waitForMigrationPhase(t, options, name, phasePodMigration, 10*time.Minute)
	wantNodes := cluster.DesiredNodes
	if rollback {
		wantNodes = 1
	}
	waitForCrdbNodeCount(t, options, name, wantNodes, 20*time.Minute)
	requireInsecureMigrationState(t, options, name)
	requirePublicServiceSQL(t, options, cluster, service)
	events, err := k8s.RunKubectlAndGetOutputE(t, options, "get", "events",
		"--field-selector", "involvedObject.name="+name, "-o", "jsonpath={.items[*].reason}")
	require.NoError(t, err)
	require.Contains(t, events, "InsecureClusterMigration")
	if rollback {
		k8s.RunKubectl(t, options, "label", "sts", name, "crdb.io/migrate=rollback", "--overwrite")
		waitForSTSReplicas(t, options, name, cluster.DesiredNodes, 10*time.Minute)
		waitForCrdbNodeCount(t, options, name, 0, 5*time.Minute)
		requireRollbackArtifactsRemoved(t, options, name, false)
		requireJSONPathEquals(t, options, "sts", name, "{.metadata.labels.crdb\\.io/migrate}", "")
		testutil.RequireCRDBClusterToBeReadyEventuallyTimeout(t, options, cluster, 300*time.Second)
	} else {
		waitForSettledFinalization(t, options, name)
		deleteStatefulSetAndWaitGone(t, options, name, 3*time.Minute)
		waitForMigrationPhase(t, options, name, phaseComplete, 3*time.Minute)
		requireMigrationComplete(t, options, name)
		valuesFile := exportValuesForHelmAdoption(t, name, ns)
		requireInsecureMigrationValues(t, valuesFile)
		adoptIntoCockroachDBHelmChart(t, options, name, releaseName, ns, valuesFile)
		waitForClusterPodCount(t, options, name, cluster.DesiredNodes, 5*time.Minute)
		requireInsecureMigrationState(t, options, name)
	}
	testutil.RequireCRDBToFunction(t, cluster, true)
	requirePublicServiceSQL(t, options, cluster, service)
	requireMigrationPVCsPreserved(t, options, claims)
}

func requireInsecureMigrationState(t *testing.T, options *k8s.KubectlOptions, name string) {
	t.Helper()
	raw, err := k8s.RunKubectlAndGetOutputE(t, options, "get", v1beta1CrdbClusterResource, name, "-o", "json")
	require.NoError(t, err)
	var cluster struct {
		Spec struct {
			TLSEnabled bool `json:"tlsEnabled"`
		} `json:"spec"`
	}
	require.NoError(t, json.Unmarshal([]byte(raw), &cluster))
	// The API omits this field when false, so decode it with the same zero value.
	require.False(t, cluster.Spec.TLSEnabled, "migration must keep TLS disabled")
	requireJSONPathEquals(t, options, v1beta1CrdbClusterResource, name,
		"{.spec.template.spec.certificates.externalCertificates}", "")
	for _, suffix := range []string{"-ca-secret", "-node-secret", "-client-secret"} {
		_, err := k8s.GetSecretE(t, options, name+suffix)
		require.True(t, apierrors.IsNotFound(err), "insecure migration created Secret %s: %v", name+suffix, err)
	}
	for _, suffix := range []string{"-ca-crt", "-ca-secret-crt"} {
		_, err := k8s.GetConfigMapE(t, options, name+suffix)
		require.True(t, apierrors.IsNotFound(err), "insecure migration created CA ConfigMap %s: %v", name+suffix, err)
	}
	cronjobs, err := k8s.RunKubectlAndGetOutputE(t, options, "get", "cronjobs", "-o", "name")
	require.NoError(t, err)
	require.Empty(t, strings.TrimSpace(cronjobs), "insecure migration must not install rotation jobs")
}

// testOperatorAutoForwardMigration migrates a public-operator cluster to the CockroachDB
// Operator, validates data integrity, then cleans up public operator resources.
func testOperatorAutoForwardMigration(t *testing.T) {
	testOperatorAutoForwardMigrationWithTLSSecrets(t, "", "")
}

func testOperatorAutoForwardMigrationWithTLSSecrets(t *testing.T, nodeSecret, clientSecret string) {
	ns := "cockroach" + strings.ToLower(random.UniqueId())
	kubectlOptions := migrationKubectlOptions(ns)

	clusterName, crdbCluster := installPublicOperatorSourceCluster(t, kubectlOptions, ns)
	operatorInstalled := false
	defer func() {
		_ = os.RemoveAll(manifestsDirPath)
	}()
	defer func() {
		testutil.CaptureFailureArtifacts(t, kubectlOptions)
		_ = helm.DeleteE(t, &helm.Options{KubectlOptions: kubectlOptions}, releaseName, true)
		clusterResource := v1alpha1CrdbClusterResource
		if operatorInstalled {
			clusterResource = v1beta1CrdbClusterResource
		}
		_ = k8s.RunKubectlE(t, kubectlOptions, "delete", clusterResource, "--all", "--ignore-not-found=true", "--timeout=60s")
		if operatorInstalled {
			waitForClusterResourcesGone(t, kubectlOptions)
		}
		uninstallMigrationOperator(t, kubectlOptions)
		k8s.RunKubectl(t, kubectlOptions, "delete", "priorityclass", "operator-critical", "--ignore-not-found")
		cleanupPublicOperatorResources(t)
		k8s.DeleteNamespace(t, kubectlOptions, ns)
	}()

	testutil.RequireClusterToBeReadyEventuallyTimeout(t, crdbCluster, 600*time.Second)
	testutil.RequireCRDBToFunction(t, crdbCluster, false)
	customTLS := nodeSecret != "" || clientSecret != ""
	if customTLS {
		crdbCluster = configureLegacyOperatorTLSSecrets(t, kubectlOptions, crdbCluster, nodeSecret, clientSecret)
	}
	sourceIdentity := snapshotSourceIdentity(t, kubectlOptions, clusterName)
	publicService := snapshotPublicSQLService(t, kubectlOptions, clusterName)
	requirePublicServiceSQL(t, kubectlOptions, crdbCluster, publicService)
	originalPVCs := snapshotMigrationPVCs(t, kubectlOptions)

	// Leave the public source active initially to verify the explicit opt-in gate.

	// Step 2: Annotate CrdbCluster with cloud region and provider for conversion.
	retry.DoWithRetry(t, "annotate crdbcluster", 5, 2*time.Second, func() (string, error) {
		if err := k8s.RunKubectlE(t, kubectlOptions, "annotate", v1alpha1CrdbClusterResource, clusterName,
			fmt.Sprintf("crdb.cockroachlabs.com/cloudProvider=%s", cloudProvider), "--overwrite"); err != nil {
			return "", err
		}
		return k8s.RunKubectlAndGetOutputE(t, kubectlOptions, "annotate", v1alpha1CrdbClusterResource, clusterName,
			fmt.Sprintf("crdb.cockroachlabs.com/regionCode=%s", cloudRegion), "--overwrite")
	})

	// Step 3: Install CockroachDB Operator scoped to this namespace.
	operatorInstalled = true
	installMigrationOperator(t, kubectlOptions, ns)

	// While the active v1alpha1 source still exists, install and upgrade an
	// unrelated native v1beta1 cluster with Helm 4 SSA. This exercises the
	// conversion webhook coexistence path without adding another suite case.
	if !customTLS {
		verifyFreshV1Beta1HelmClusterDuringCoexistence(t, kubectlOptions, ns, clusterName, crdbCluster)
	}

	// Step 4: Start migration.
	k8s.RunKubectl(t, kubectlOptions, "label", v1beta1CrdbClusterResource, clusterName, "crdb.io/migrate=start")
	requireMigrationWaitsForSkipReconcile(t, kubectlOptions, clusterName, crdbCluster.DesiredNodes)
	k8s.RunKubectl(t, kubectlOptions, "label", v1alpha1CrdbClusterResource, clusterName, "crdb.io/skip-reconcile=true", "--overwrite")

	waitForMigrationPhase(t, kubectlOptions, clusterName, phaseCertMigration, 5*time.Minute)
	waitForMigrationPhase(t, kubectlOptions, clusterName, phasePodMigration, 10*time.Minute)
	waitForCrdbNodeCount(t, kubectlOptions, clusterName, crdbCluster.DesiredNodes, 20*time.Minute)
	waitForSettledFinalization(t, kubectlOptions, clusterName)

	// Per the migration docs, the source StatefulSet must be deleted to move
	// from Finalization to Complete.
	deleteStatefulSetAndWaitGone(t, kubectlOptions, clusterName, 3*time.Minute)
	waitForMigrationPhase(t, kubectlOptions, clusterName, phaseComplete, 3*time.Minute)
	requireMigrationComplete(t, kubectlOptions, clusterName)
	requireNoLegacyV1Alpha1ManagedFields(t, kubectlOptions, clusterName)
	requireOperatorSourceFieldsPreserved(t, kubectlOptions, clusterName)
	requireSourceIdentity(t, kubectlOptions, clusterName, sourceIdentity)

	// Validate data survived migration.
	if !customTLS {
		crdbCluster.ClientSecret = fmt.Sprintf("%s-client-secret", clusterName)
		crdbCluster.NodeSecret = fmt.Sprintf("%s-node-secret", clusterName)
	}
	testutil.RequireCRDBToFunction(t, crdbCluster, true)
	requirePublicServiceSQL(t, kubectlOptions, crdbCluster, publicService)

	// Keep skip-reconcile on the source v1alpha1 resource after successful
	// migration so the public operator cannot resume reconciling it.
	skipReconcile, _ := k8s.RunKubectlAndGetOutputE(t, kubectlOptions, "get", v1alpha1CrdbClusterResource, clusterName,
		"-o", "jsonpath={.metadata.labels.crdb\\.io/skip-reconcile}")
	require.Equal(t, "true", skipReconcile, "skip-reconcile label should remain after migration complete")

	valuesFile := exportValuesForHelmAdoption(t, clusterName, ns)
	var certificateFingerprints map[string]string
	if customTLS {
		requireExternalCertificateExport(t, kubectlOptions, clusterName, valuesFile, crdbCluster)
		certificateFingerprints = map[string]string{}
		for _, secret := range []string{crdbCluster.CaSecret, crdbCluster.NodeSecret, crdbCluster.ClientSecret} {
			certificateFingerprints[secret] = migrationSecretFingerprint(t, k8s.GetSecret(t, kubectlOptions, secret))
		}
	}

	// Adopt the migrated cluster into the CockroachDB Helm chart with values
	// generated from the live migrated v1beta1 resources.
	adoptIntoCockroachDBHelmChart(t, kubectlOptions, clusterName, clusterName, ns, valuesFile)
	requireOperatorSourceFieldsPreserved(t, kubectlOptions, clusterName)
	requireSourceIdentity(t, kubectlOptions, clusterName, sourceIdentity)
	testutil.RequireCRDBToFunction(t, crdbCluster, true)
	requirePublicServiceSQL(t, kubectlOptions, crdbCluster, publicService)
	requireMigrationPVCsPreserved(t, kubectlOptions, originalPVCs)
	if customTLS {
		requireExternalCertificateReferences(t, kubectlOptions, clusterName, crdbCluster)
		for secret, fingerprint := range certificateFingerprints {
			require.Equal(t, fingerprint, migrationSecretFingerprint(t, k8s.GetSecret(t, kubectlOptions, secret)), "adoption replaced an external certificate")
		}
		jobs, err := k8s.RunKubectlAndGetOutputE(t, kubectlOptions, "get", "cronjobs", "-o", "name")
		require.NoError(t, err)
		require.Empty(t, jobs, "custom certificate names must remain external, without self-signer rotation")
	}
	if !customTLS {
		verifyLegacyOperatorRetirement(t, kubectlOptions, clusterName, valuesFile, crdbCluster, publicService)
		requireSourceIdentity(t, kubectlOptions, clusterName, sourceIdentity)
	}
}

// testOperatorAutoStopResume pauses a public-operator migration and then resumes it.
func testOperatorAutoStopResume(t *testing.T) {
	ns := "cockroach" + strings.ToLower(random.UniqueId())
	kubectlOptions := migrationKubectlOptions(ns)

	clusterName, crdbCluster := installPublicOperatorSourceCluster(t, kubectlOptions, ns)
	defer func() {
		_ = os.RemoveAll(manifestsDirPath)
	}()
	defer func() {
		testutil.CaptureFailureArtifacts(t, kubectlOptions)
		_ = helm.DeleteE(t, &helm.Options{KubectlOptions: kubectlOptions}, releaseName, true)
		_ = k8s.RunKubectlE(t, kubectlOptions, "delete", v1beta1CrdbClusterResource, "--all", "--ignore-not-found=true", "--timeout=60s")
		waitForClusterResourcesGone(t, kubectlOptions)
		uninstallMigrationOperator(t, kubectlOptions)
		k8s.RunKubectl(t, kubectlOptions, "delete", "priorityclass", "operator-critical", "--ignore-not-found")
		cleanupPublicOperatorResources(t)
		k8s.DeleteNamespace(t, kubectlOptions, ns)
	}()

	testutil.RequireClusterToBeReadyEventuallyTimeout(t, crdbCluster, 600*time.Second)
	testutil.RequireCRDBToFunction(t, crdbCluster, false)
	publicService := snapshotPublicSQLService(t, kubectlOptions, clusterName)
	requirePublicServiceSQL(t, kubectlOptions, crdbCluster, publicService)

	k8s.RunKubectl(t, kubectlOptions, "label", v1alpha1CrdbClusterResource, clusterName, "crdb.io/skip-reconcile=true", "--overwrite")
	retry.DoWithRetry(t, "annotate crdbcluster", 5, 2*time.Second, func() (string, error) {
		if err := k8s.RunKubectlE(t, kubectlOptions, "annotate", v1alpha1CrdbClusterResource, clusterName,
			fmt.Sprintf("crdb.cockroachlabs.com/cloudProvider=%s", cloudProvider), "--overwrite"); err != nil {
			return "", err
		}
		return k8s.RunKubectlAndGetOutputE(t, kubectlOptions, "annotate", v1alpha1CrdbClusterResource, clusterName,
			fmt.Sprintf("crdb.cockroachlabs.com/regionCode=%s", cloudRegion), "--overwrite")
	})

	installMigrationOperator(t, kubectlOptions, ns)

	k8s.RunKubectl(t, kubectlOptions, "label", v1alpha1CrdbClusterResource, clusterName, "crdb.io/migrate=start")
	waitForMigrationPhase(t, kubectlOptions, clusterName, phasePodMigration, 10*time.Minute)
	waitForCrdbNodeCount(t, kubectlOptions, clusterName, 1, 5*time.Minute)

	// Stop.
	k8s.RunKubectl(t, kubectlOptions, "label", v1alpha1CrdbClusterResource, clusterName, "crdb.io/migrate=stop", "--overwrite")
	waitForMigrationPhase(t, kubectlOptions, clusterName, phaseStopped, 2*time.Minute)

	// Cluster must be functional while stopped.
	crdbCluster.ClientSecret = fmt.Sprintf("%s-client-secret", clusterName)
	crdbCluster.NodeSecret = fmt.Sprintf("%s-node-secret", clusterName)
	testutil.RequireCRDBToFunction(t, crdbCluster, true)
	requirePublicServiceSQL(t, kubectlOptions, crdbCluster, publicService)

	// Resume.
	k8s.RunKubectl(t, kubectlOptions, "label", v1alpha1CrdbClusterResource, clusterName, "crdb.io/migrate=start", "--overwrite")
	waitForMigrationResume(t, kubectlOptions, clusterName)
	waitForCrdbNodeCount(t, kubectlOptions, clusterName, crdbCluster.DesiredNodes, 20*time.Minute)
	waitForSettledFinalization(t, kubectlOptions, clusterName)

	// Per the migration docs, the source StatefulSet must be deleted to move
	// from Finalization to Complete.
	deleteStatefulSetAndWaitGone(t, kubectlOptions, clusterName, 3*time.Minute)
	waitForMigrationPhase(t, kubectlOptions, clusterName, phaseComplete, 3*time.Minute)
	requireMigrationComplete(t, kubectlOptions, clusterName)
	requireNoLegacyV1Alpha1ManagedFields(t, kubectlOptions, clusterName)
	requireOperatorSourceFieldsPreserved(t, kubectlOptions, clusterName)

	skipReconcile, _ := k8s.RunKubectlAndGetOutputE(t, kubectlOptions, "get", v1alpha1CrdbClusterResource, clusterName,
		"-o", "jsonpath={.metadata.labels.crdb\\.io/skip-reconcile}")
	require.Equal(t, "true", skipReconcile, "skip-reconcile label should remain after migration complete")

	valuesFile := exportValuesForHelmAdoption(t, clusterName, ns)
	adoptIntoCockroachDBHelmChart(t, kubectlOptions, clusterName, clusterName, ns, valuesFile)
	requireOperatorSourceFieldsPreserved(t, kubectlOptions, clusterName)
	testutil.RequireCRDBToFunction(t, crdbCluster, true)
	requirePublicServiceSQL(t, kubectlOptions, crdbCluster, publicService)
}

// testOperatorAutoRollback triggers rollback mid-migration and verifies the public
// operator can resume control cleanly.
func testOperatorAutoRollback(t *testing.T) {
	ns := "cockroach" + strings.ToLower(random.UniqueId())
	kubectlOptions := migrationKubectlOptions(ns)

	clusterName, crdbCluster := installPublicOperatorSourceCluster(t, kubectlOptions, ns)
	defer func() {
		testutil.CaptureFailureArtifacts(t, kubectlOptions)
		// Rollback does not adopt into Helm, so explicitly delete CRs before
		// waiting. The operator is still running and will process finalizers.
		_ = k8s.RunKubectlE(t, kubectlOptions, "delete", "crdbclusters.crdb.cockroachlabs.com", "--all", "--ignore-not-found=true")
		waitForClusterResourcesGone(t, kubectlOptions)
		uninstallMigrationOperator(t, kubectlOptions)
		k8s.RunKubectl(t, kubectlOptions, "delete", "priorityclass", "operator-critical", "--ignore-not-found")
		cleanupPublicOperatorResources(t)
		k8s.DeleteNamespace(t, kubectlOptions, ns)
	}()

	testutil.RequireClusterToBeReadyEventuallyTimeout(t, crdbCluster, 600*time.Second)
	testutil.RequireCRDBToFunction(t, crdbCluster, false)
	publicService := snapshotPublicSQLService(t, kubectlOptions, clusterName)
	requirePublicServiceSQL(t, kubectlOptions, crdbCluster, publicService)
	originalPVCs := snapshotMigrationPVCs(t, kubectlOptions)
	sourceResources := snapshotMigrationSourceResources(t, kubectlOptions,
		"poddisruptionbudget/"+clusterName, "role/"+clusterName+"-role",
		"rolebinding/"+clusterName+"-role-binding", "service/"+clusterName)

	k8s.RunKubectl(t, kubectlOptions, "label", v1alpha1CrdbClusterResource, clusterName, "crdb.io/skip-reconcile=true", "--overwrite")
	retry.DoWithRetry(t, "annotate crdbcluster", 5, 2*time.Second, func() (string, error) {
		if err := k8s.RunKubectlE(t, kubectlOptions, "annotate", v1alpha1CrdbClusterResource, clusterName,
			fmt.Sprintf("crdb.cockroachlabs.com/cloudProvider=%s", cloudProvider), "--overwrite"); err != nil {
			return "", err
		}
		return k8s.RunKubectlAndGetOutputE(t, kubectlOptions, "annotate", v1alpha1CrdbClusterResource, clusterName,
			fmt.Sprintf("crdb.cockroachlabs.com/regionCode=%s", cloudRegion), "--overwrite")
	})

	installMigrationOperator(t, kubectlOptions, ns)

	k8s.RunKubectl(t, kubectlOptions, "label", v1alpha1CrdbClusterResource, clusterName, "crdb.io/migrate=start")
	waitForMigrationPhase(t, kubectlOptions, clusterName, phasePodMigration, 10*time.Minute)
	waitForCrdbNodeCount(t, kubectlOptions, clusterName, 1, 5*time.Minute)

	requireMigrationSourceResourcesPreserved(t, kubectlOptions, sourceResources)

	// Remove label to trigger rollback.
	k8s.RunKubectl(t, kubectlOptions, "label", v1alpha1CrdbClusterResource, clusterName, "crdb.io/migrate-")

	// After rollback: CrdbNodes deleted, STS restored, skip-reconcile removed.
	waitForCrdbNodeCount(t, kubectlOptions, clusterName, 0, 5*time.Minute)
	waitForSTSReplicas(t, kubectlOptions, clusterName, crdbCluster.DesiredNodes, 10*time.Minute)

	// skip-reconcile must be removed last so the public operator can resume.
	retry.DoWithRetry(t, "wait for skip-reconcile removal", 30, 5*time.Second, func() (string, error) {
		out, err := k8s.RunKubectlAndGetOutputE(t, kubectlOptions, "get", v1alpha1CrdbClusterResource, clusterName,
			"-o", "jsonpath={.metadata.labels.crdb\\.io/skip-reconcile}")
		if err != nil {
			return "", err
		}
		if out != "" {
			return "", fmt.Errorf("skip-reconcile label still present: %s", out)
		}
		return "removed", nil
	})

	requireJSONPathEqualsEventually(t, kubectlOptions, v1beta1CrdbClusterResource, clusterName,
		"{.status.migration.settledPhase}", "RollbackComplete", 3*time.Minute)
	requireRollbackArtifactsRemoved(t, kubectlOptions, clusterName, true)
	for _, secret := range []string{crdbCluster.CaSecret, crdbCluster.NodeSecret, crdbCluster.ClientSecret} {
		k8s.RunKubectl(t, kubectlOptions, "get", "secret", secret)
	}
	// Exercise beta admission after handback: it must not pause the public operator again.
	k8s.RunKubectl(t, kubectlOptions, "annotate", v1beta1CrdbClusterResource, clusterName,
		"test.cockroachlabs.com/after-rollback=true", "--overwrite")
	requireJSONPathEquals(t, kubectlOptions, v1beta1CrdbClusterResource, clusterName,
		"{.spec.mode}", "Disabled")
	for _, label := range []string{"skip-reconcile", "migrate"} {
		requireJSONPathEquals(t, kubectlOptions, v1beta1CrdbClusterResource, clusterName,
			"{.metadata.labels.crdb\\.io/"+label+"}", "")
	}

	// Assert logging ConfigMap key was reverted to logging.yaml on rollback.
	logConfigData, err := k8s.RunKubectlAndGetOutputE(t, kubectlOptions,
		"get", "configmap", "logging-config", "-o", "jsonpath={.data}")
	require.NoError(t, err)
	require.Contains(t, logConfigData, "logging.yaml",
		"logging ConfigMap key should revert to logging.yaml after rollback")

	// Cluster must be healthy after rollback with public operator in control.
	testutil.RequireClusterToBeReadyEventuallyTimeout(t, crdbCluster, 300*time.Second)
	testutil.RequireCRDBToFunction(t, crdbCluster, true)
	requirePublicServiceSQL(t, kubectlOptions, crdbCluster, publicService)
	requireMigrationPVCsPreserved(t, kubectlOptions, originalPVCs)
	requireMigrationSourceResourcesPreserved(t, kubectlOptions, sourceResources)
	requirePublicServicePreserved(t, kubectlOptions, publicService)
	requireDiscoveryRejoinAfterRollback(t, kubectlOptions, clusterName, crdbCluster)
}

// testHelmAutoCertManagerMigration installs a Helm cluster with cert-manager TLS,
// verifies custom SANs through migration and chart adoption, then reissues a leaf.
func testHelmAutoCertManagerMigration(t *testing.T) {
	const (
		caSecretName = "cockroach-ca"
		customDNS    = "migration.example.test"
		customIP     = "192.0.2.10"
	)
	ns := "cockroach" + strings.ToLower(random.UniqueId())
	kubectlOptions := migrationKubectlOptions(ns)
	certManagerK8sOptions := migrationKubectlOptions(testutil.CertManagerNamespace)

	cleanupPublicOperatorResources(t)
	testutil.InstallCertManager(t, certManagerK8sOptions)
	defer func() {
		testutil.DeleteTrustManager(t, certManagerK8sOptions)
		testutil.DeleteCertManager(t, certManagerK8sOptions)
		k8s.DeleteNamespace(t, certManagerK8sOptions, testutil.CertManagerNamespace)
	}()

	k8s.CreateNamespace(t, kubectlOptions, ns)
	testutil.CreateSelfSignedIssuer(t, kubectlOptions, ns)

	stsName := fmt.Sprintf("%s-cockroachdb", releaseName)
	crdbCluster := testutil.CockroachCluster{
		Cfg:              cfg,
		K8sClient:        k8sClient,
		StatefulSetName:  stsName,
		Namespace:        ns,
		ClientSecret:     "cockroachdb-root",
		NodeSecret:       "cockroachdb-node",
		CaSecret:         caSecretName,
		IsCaUserProvided: isCaUserProvided,
		Context:          k3dClusterName,
		DesiredNodes:     3,
	}

	helmOptions := &helm.Options{
		KubectlOptions: kubectlOptions,
		SetValues: testutil.PatchHelmValues(map[string]string{
			"operator.enabled":                                     "false",
			"conf.cluster-name":                                    "test",
			"init.provisioning.enabled":                            "true",
			"init.provisioning.databases[0].name":                  migration.TestDBName,
			"init.provisioning.databases[0].owners[0]":             "root",
			"conf.locality":                                        fmt.Sprintf("topology.kubernetes.io/region=%s", cloudRegion),
			"tls.certs.selfSigner.enabled":                         "false",
			"tls.certs.certManager":                                "true",
			"tls.certs.certManagerIssuer.name":                     testutil.SelfSignedIssuerName,
			"tls.certs.certManagerIssuer.additionalDnsNames[0]":    customDNS,
			"tls.certs.certManagerIssuer.additionalIpAddresses[0]": customIP,
		}),
	}
	// Record retention in the source release manifest before migration. The new
	// chart manages the leaves but does not render the legacy CA Certificate/Issuer.
	helmChartPath := legacyChartWithRetainedCertManagerCA(t)
	helm.Install(t, helmOptions, helmChartPath, releaseName)
	defer func() {
		testutil.CaptureFailureArtifacts(t, kubectlOptions)
		_ = helm.DeleteE(t, &helm.Options{KubectlOptions: kubectlOptions}, releaseName, true)
		// Also clean up if the test fails before chart adoption.
		_ = k8s.RunKubectlE(t, kubectlOptions, "delete", "crdbclusters.crdb.cockroachlabs.com", "--all", "--ignore-not-found=true")
		waitForClusterResourcesGone(t, kubectlOptions)
		uninstallMigrationOperator(t, kubectlOptions)
		k8s.DeleteNamespace(t, kubectlOptions, ns)
		_ = os.RemoveAll(manifestsDirPath)
	}()

	k8s.WaitUntilServiceAvailable(t, kubectlOptions, fmt.Sprintf("%s-public", stsName), 30, 2*time.Second)

	caConfigMapName := fmt.Sprintf("%s-ca-crt", stsName)
	testutil.InstallTrustManager(t, certManagerK8sOptions, ns)
	testutil.CreateBundle(t, kubectlOptions, caSecretName, caConfigMapName)
	defer testutil.DeleteBundle(t, kubectlOptions, caConfigMapName)

	testutil.RequireCRDBClusterToBeReadyEventuallyTimeout(t, kubectlOptions, crdbCluster, 600*time.Second)
	testutil.RequireCRDBToFunction(t, crdbCluster, false)
	publicService := snapshotPublicSQLService(t, kubectlOptions, stsName)
	requirePublicServiceSQL(t, kubectlOptions, crdbCluster, publicService)

	originalCertificate := getMigrationCertificateSpec(t, kubectlOptions, stsName+"-node")
	originalClientCertificate := getMigrationCertificateSpec(t, kubectlOptions, stsName+"-root-client")
	issuerChain := snapshotMigrationSourceResources(t, kubectlOptions,
		"issuers.cert-manager.io/"+stsName+"-ca-issuer",
		"certificates.cert-manager.io/"+stsName+"-ca-cert")
	originalCA := migrationSecretFingerprint(t, k8s.GetSecret(t, kubectlOptions, caSecretName))

	installMigrationOperator(t, kubectlOptions, ns)

	k8s.RunKubectl(t, kubectlOptions, "label", "sts", stsName, "crdb.io/migrate=start")

	waitForMigrationPhase(t, kubectlOptions, stsName, phaseCertMigration, 5*time.Minute)

	// Verify cert-manager-managed secret names are preserved on the migrated cluster.
	requireJSONPathEquals(t, kubectlOptions, v1beta1CrdbClusterResource, stsName,
		"{.spec.template.spec.certificates.externalCertificates.nodeSecretName}", "cockroachdb-node")
	requireJSONPathEquals(t, kubectlOptions, v1beta1CrdbClusterResource, stsName,
		"{.spec.template.spec.certificates.externalCertificates.rootSqlClientSecretName}", "cockroachdb-root")

	retry.DoWithRetry(t, "verify cert-manager node secret exists", 10, 5*time.Second, func() (string, error) {
		return k8s.RunKubectlAndGetOutputE(t, kubectlOptions, "get", "secret", "cockroachdb-node")
	})

	waitForCrdbNodeCount(t, kubectlOptions, stsName, crdbCluster.DesiredNodes, 20*time.Minute)
	waitForSettledFinalization(t, kubectlOptions, stsName)

	// Per the migration docs, the source StatefulSet must be deleted to move
	// from Finalization to Complete.
	deleteStatefulSetAndWaitGone(t, kubectlOptions, stsName, 3*time.Minute)
	waitForMigrationPhase(t, kubectlOptions, stsName, phaseComplete, 3*time.Minute)
	requireMigrationComplete(t, kubectlOptions, stsName)
	requirePublicServiceSQL(t, kubectlOptions, crdbCluster, publicService)

	requireJSONPathContains(t, kubectlOptions, v1beta1CrdbClusterResource, stsName,
		"{.spec.template.spec.startFlags}", "topology.kubernetes.io/region="+cloudRegion)
	requireMigrationCertificatePreserved(t, kubectlOptions, stsName+"-node", originalCertificate)
	joinDNS := stsName + "-join." + ns + ".svc.cluster.local"
	requireNodeCertificateNames(t, kubectlOptions, crdbCluster.NodeSecret, customDNS, customIP, joinDNS)

	valuesFile := exportValuesForHelmAdoption(t, stsName, ns)
	configureMigrationCertManagerAdoption(t, valuesFile, caConfigMapName,
		originalCertificate, originalClientCertificate, []string{customDNS}, []string{customIP})
	adoptIntoCockroachDBHelmChart(t, kubectlOptions, stsName, releaseName, ns, valuesFile)

	// Adoption includes a second Helm upgrade and rolling restart. Keep the
	// existing issuer chain and certificate settings through both operations.
	requireMigrationSourceResourcesPreserved(t, kubectlOptions, issuerChain)
	requireMigrationCertManagerCertificates(t, kubectlOptions, stsName, originalCertificate, originalClientCertificate)
	requireNodeCertificateNames(t, kubectlOptions, crdbCluster.NodeSecret, customDNS, customIP, joinDNS)
	for path, expected := range map[string]string{
		"caConfigMapName":         caConfigMapName,
		"nodeSecretName":          crdbCluster.NodeSecret,
		"rootSqlClientSecretName": crdbCluster.ClientSecret,
	} {
		requireJSONPathEquals(t, kubectlOptions, v1beta1CrdbClusterResource, stsName,
			"{.spec.template.spec.certificates.externalCertificates."+path+"}", expected)
	}
	requirePublicServiceSQL(t, kubectlOptions, crdbCluster, publicService)

	// An existing leaf can mask a broken issuer chain. Recreate the node Secret
	// after adoption and verify the new certificate's SANs and trust against the CA.
	requireMigrationCertManagerReissue(t, kubectlOptions, crdbCluster.NodeSecret, caSecretName, customDNS, customIP, joinDNS)
	requireMigrationCertManagerCertificates(t, kubectlOptions, stsName, originalCertificate, originalClientCertificate)
	require.Equal(t, originalCA, migrationSecretFingerprint(t, k8s.GetSecret(t, kubectlOptions, caSecretName)),
		"chart adoption and leaf reissuance must preserve the CA")
	testutil.RequireClusterToBeReadyEventuallyTimeout(t, crdbCluster, 300*time.Second)
	testutil.RequireCRDBToFunction(t, crdbCluster, true)
	requirePublicServiceSQL(t, kubectlOptions, crdbCluster, publicService)
}

// --- Helpers ---

// installMigrationOperator installs the CockroachDB Operator with migration enabled and
// watchNamespaces scoped to the given namespace.
func installMigrationOperator(t *testing.T, kubectlOptions *k8s.KubectlOptions, namespace string) {
	t.Logf("Installing migration operator scoped to namespace %s", namespace)
	_, operatorChartPath := operator.HelmChartPaths()
	if _, err := k8s.GetNamespaceE(t, kubectlOptions, namespace); err != nil && apierrors.IsNotFound(err) {
		k8s.CreateNamespace(t, kubectlOptions, namespace)
	}
	opts := &helm.Options{
		KubectlOptions: kubectlOptions,
		SetValues: map[string]string{
			"numReplicas":       "1",
			"migration.enabled": "true",
			"watchNamespaces":   namespace,
			"cloudRegion":       cloudRegion,
		},
		ExtraArgs: map[string][]string{"install": {"--wait", "--debug"}},
	}
	helm.Install(t, opts, operatorChartPath, autoMigrationOperatorRelease)

	for _, crd := range []string{
		"crdbclusters.crdb.cockroachlabs.com",
		"crdbnodes.crdb.cockroachlabs.com",
		"crdbtenants.crdb.cockroachlabs.com",
	} {
		retry.DoWithRetry(t, fmt.Sprintf("wait-for-%s", crd), 60, 5*time.Second, func() (string, error) {
			return k8s.RunKubectlAndGetOutputE(t, kubectlOptions, "get", "crd", crd)
		})
	}

	pods := k8s.ListPods(t, kubectlOptions, metav1.ListOptions{LabelSelector: operator.OperatorLabelSelector})
	for i := range pods {
		testutil.RequirePodToBeCreatedAndReady(t, kubectlOptions, pods[i].Name, 300*time.Second)
	}

	k8s.WaitUntilServiceAvailable(t, kubectlOptions, "cockroach-operator", 30, 5*time.Second)
	k8s.WaitUntilServiceAvailable(t, kubectlOptions, "cockroach-webhook-service", 30, 5*time.Second)
	testutil.RequireServiceEndpointsAvailable(t, kubectlOptions, "cockroach-webhook-service", 2*time.Minute)
}

func uninstallMigrationOperator(t *testing.T, kubectlOptions *k8s.KubectlOptions) {
	opts := &helm.Options{KubectlOptions: kubectlOptions}
	_ = helm.DeleteE(t, opts, autoMigrationOperatorRelease, true)

	// Delete CRDs to fully reset state between tests. The operator registers
	// a conversion webhook on the CRD at runtime that isn't cleaned up on
	// shutdown. Deleting the CRDs avoids stale conversion webhooks, storedVersions,
	// etc. They get recreated by the next operator or public operator install.
	clusterKubectl := migrationKubectlOptions("")
	for _, crd := range []string{
		"crdbclusters.crdb.cockroachlabs.com",
		"crdbnodes.crdb.cockroachlabs.com",
		"crdbtenants.crdb.cockroachlabs.com",
	} {
		// A failed migration can leave finalizers behind. Bound this wait so
		// teardown reports that failure instead of hanging until runner shutdown.
		if err := k8s.RunKubectlE(t, clusterKubectl, "delete", "crd", crd,
			"--ignore-not-found=true", "--timeout=60s", "--request-timeout=20s"); err != nil {
			t.Errorf("deleting migration CRD %s: %v", crd, err)
		}
	}
}

func cleanupPublicOperatorResources(t *testing.T) {
	kubectlOptions := migrationKubectlOptions(migration.OperatorNamespace)
	k8s.RunKubectl(t, kubectlOptions, "delete", "deployment", migration.OperatorDeploymentName, "--ignore-not-found=true")
	k8s.RunKubectl(t, kubectlOptions, "delete", "service", migration.OperatorServiceName, "--ignore-not-found=true")
	k8s.RunKubectl(t, kubectlOptions, "delete", "serviceaccount", migration.OperatorServiceAccountName, "--ignore-not-found=true")
	k8s.RunKubectl(t, kubectlOptions, "delete", "clusterrole", migration.OperatorClusterRoleName, "--ignore-not-found=true")
	k8s.RunKubectl(t, kubectlOptions, "delete", "clusterrolebinding", migration.OperatorClusterRoleBindingName, "--ignore-not-found=true")
	k8s.RunKubectl(t, kubectlOptions, "delete", "validatingwebhookconfiguration", migration.OperatorValidatingWebhookConfiguration, "--ignore-not-found=true")
	k8s.RunKubectl(t, kubectlOptions, "delete", "mutatingwebhookconfiguration", migration.OperatorMutatingWebhookConfiguration, "--ignore-not-found=true")
}

func patchPublicOperatorWebhooksForCoexistence(t *testing.T) {
	t.Helper()
	kubectlOptions := migrationKubectlOptions("")
	for _, resource := range []string{
		"validatingwebhookconfiguration/" + migration.OperatorValidatingWebhookConfiguration,
		"mutatingwebhookconfiguration/" + migration.OperatorMutatingWebhookConfiguration,
	} {
		k8s.RunKubectl(t, kubectlOptions, "patch", resource,
			"--type=json",
			"-p", `[{"op":"add","path":"/webhooks/0/matchPolicy","value":"Exact"}]`,
		)
		matchPolicy, err := k8s.RunKubectlAndGetOutputE(t, kubectlOptions,
			"get", resource, "-o", "jsonpath={.webhooks[0].matchPolicy}")
		require.NoError(t, err)
		require.Equal(t, "Exact", matchPolicy)
	}
}

// waitForClusterResourcesGone waits until CrdbClusters and CrdbNodes are fully
// removed from the namespace. After helm adoption, helm delete handles
// CrdbCluster/CrdbNode deletion. For rollback tests where no helm adoption
// occurs, the CrdbCluster must be explicitly deleted before calling this.
func waitForClusterResourcesGone(t *testing.T, kubectlOptions *k8s.KubectlOptions) {
	if err := waitForClusterResourcesGoneE(t, kubectlOptions); err != nil {
		t.Errorf("waiting for cluster resources to be gone: %v", err)
	}
}

func waitForClusterResourcesGoneE(t *testing.T, kubectlOptions *k8s.KubectlOptions) error {
	resources := []string{
		"crdbclusters.crdb.cockroachlabs.com",
		"crdbnodes.crdb.cockroachlabs.com",
	}
	for _, res := range resources {
		if _, err := retry.DoWithRetryE(t, fmt.Sprintf("wait for %s to be gone", res), 60, 5*time.Second, func() (string, error) {
			out, err := k8s.RunKubectlAndGetOutputE(t, kubectlOptions, "get", res, "-o", "name")
			if err != nil {
				// CRD might not exist, that's fine.
				return "gone", nil
			}
			if strings.TrimSpace(out) != "" {
				return "", fmt.Errorf("%s still present: %s", res, out)
			}
			return "gone", nil
		}); err != nil {
			return err
		}
	}
	return nil
}

// installHelmSourceCluster installs a Helm StatefulSet-based CockroachDB cluster with
// WAL failover and a logging config to maximize migration coverage.
// Returns the StatefulSet name and a CockroachCluster descriptor.
func installHelmSourceCluster(
	t *testing.T, kubectlOptions *k8s.KubectlOptions, namespace string, overrides ...map[string]string,
) (string, testutil.CockroachCluster) {
	stsName := fmt.Sprintf("%s-cockroachdb", releaseName)
	crdbCluster := testutil.CockroachCluster{
		Cfg:              cfg,
		K8sClient:        k8sClient,
		StatefulSetName:  stsName,
		Namespace:        namespace,
		ClientSecret:     ClientSecret,
		NodeSecret:       NodeSecret,
		CaSecret:         CASecret,
		IsCaUserProvided: isCaUserProvided,
		Context:          k3dClusterName,
		DesiredNodes:     3,
	}

	helmOptions := &helm.Options{
		KubectlOptions: kubectlOptions,
		SetValues: testutil.PatchHelmValues(map[string]string{
			"operator.enabled":                         "false",
			"conf.cluster-name":                        "test",
			"init.provisioning.enabled":                "true",
			"init.provisioning.databases[0].name":      migration.TestDBName,
			"init.provisioning.databases[0].owners[0]": "root",
			"statefulset.labels.app":                   "cockroachdb",
			"conf.locality":                            fmt.Sprintf("region=%s\\,zone=$(printf zone-a)", cloudRegion),
			"tls.certs.selfSigner.additionalSANs[0]":   "migration.example.test",
			"tls.certs.selfSigner.additionalSANs[1]":   "192.0.2.10",
			"tls.certs.selfSigner.additionalSANs[2]":   "migration-client." + namespace + ".svc.cluster.local",
			// This source cluster uses dedicated volumes for WAL failover and logs.
			// Migration should preserve both on the v1beta1 target.
			"storage.persistentVolume.enabled":                    "true",
			"storage.persistentVolume.labels.migration-test":      "keep",
			"storage.persistentVolume.annotations.migration-test": "keep",
			"conf.wal-failover.value":                             "path=/cockroach/wal-failover",
			"conf.wal-failover.persistentVolume.enabled":          "true",
			"conf.wal-failover.persistentVolume.path":             "wal-failover",
			"conf.wal-failover.persistentVolume.size":             "1Gi",
			"conf.log.enabled":                                    "true",
			"conf.log.config.file-defaults.dir":                   "/cockroach/test-logs",
			"conf.log.persistentVolume.enabled":                   "true",
			"conf.log.persistentVolume.path":                      "test-logs",
			"conf.log.persistentVolume.size":                      "1Gi",
		}),
	}

	for _, values := range overrides {
		for key, value := range values {
			helmOptions.SetValues[key] = value
		}
	}
	if helmOptions.SetValues["tls.enabled"] == "false" {
		crdbCluster.CaSecret, crdbCluster.NodeSecret, crdbCluster.ClientSecret = "", "", ""
	}

	h := &migration.HelmInstall{
		Namespace:   namespace,
		HelmOptions: helmOptions,
		CrdbCluster: crdbCluster,
	}
	h.InstallHelm(t)
	// Resolve a custom certificate SAN to the same public Service. SQL through
	// this alias must verify that name, rather than only the default chart DNS.
	require.NoError(t, k8sClient.Create(context.Background(), &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "migration-client", Namespace: namespace},
		Spec: corev1.ServiceSpec{
			Type:         corev1.ServiceTypeExternalName,
			ExternalName: stsName + "-public." + namespace + ".svc.cluster.local",
		},
	}))
	return stsName, crdbCluster
}

// installPublicOperatorSourceCluster installs the public operator and creates a v1alpha1
// CrdbCluster with logging config, annotations, terminationGracePeriod, and priority class
// to maximize migration coverage. Returns the cluster name and a CockroachCluster descriptor.
func installPublicOperatorSourceCluster(
	t *testing.T, kubectlOptions *k8s.KubectlOptions, namespace string,
) (string, testutil.CockroachCluster) {
	const (
		clusterName   = "crdb-test"
		priorityClass = "operator-critical"
		logConfigName = "logging-config"
	)

	if _, err := k8s.GetNamespaceE(t, kubectlOptions, namespace); err != nil && apierrors.IsNotFound(err) {
		k8s.CreateNamespace(t, kubectlOptions, namespace)
	}

	// Priority class is cluster-scoped; ignore the error if it already exists.
	_ = k8s.RunKubectlE(t, kubectlOptions, "create", "priorityclass", priorityClass, "--value", "500000000")

	// Logging config: migration must rename key logging.yaml → logs.yaml.
	createLoggingConfig(t, k8sClient, logConfigName, namespace)

	crdbCluster := testutil.CockroachCluster{
		Cfg:              cfg,
		K8sClient:        k8sClient,
		StatefulSetName:  clusterName,
		Namespace:        namespace,
		ClientSecret:     fmt.Sprintf("%s-root", clusterName),
		NodeSecret:       fmt.Sprintf("%s-node", clusterName),
		CaSecret:         fmt.Sprintf("%s-ca", clusterName),
		IsCaUserProvided: true,
		Context:          k3dClusterName,
		DesiredNodes:     3,
	}

	o := &migration.PublicOperator{}
	o.Namespace = namespace
	o.Ctx = context.Background()
	o.CrdbCluster = crdbCluster
	o.CustomResourceBuilder = testutil.NewBuilder(clusterName).
		WithNodeCount(int32(3)).
		WithTLS().
		WithImage(migration.CockroachVersion).
		WithPVDataStore("1Gi").
		WithLabels(map[string]string{"app": "cockroachdb"}).
		WithAnnotations(map[string]string{"crdb": "isCool"}).
		WithTerminationGracePeriodSeconds(int64(60)).
		WithPriorityClass(priorityClass).
		WithClusterLogging(logConfigName).
		WithResources(corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("200m"),
				corev1.ResourceMemory: resource.MustParse("256Mi"),
			},
		})

	o.InstallOperator(t)
	patchPublicOperatorWebhooksForCoexistence(t)
	return clusterName, crdbCluster
}

// waitForMigrationPhase polls status.migration.phase on the migrated v1beta1
// CrdbCluster until it matches expectedPhase or the timeout is exceeded.
func waitForMigrationPhase(
	t *testing.T,
	kubectlOptions *k8s.KubectlOptions,
	clusterName, expectedPhase string,
	timeout time.Duration,
) {
	t.Logf("Waiting for migration phase %q on v1beta1 CrdbCluster %s", expectedPhase, clusterName)
	ctx := context.Background()
	err := wait.PollUntilContextTimeout(ctx, 10*time.Second, timeout, true,
		func(ctx context.Context) (bool, error) {
			phase, err := k8s.RunKubectlAndGetOutputE(t, kubectlOptions,
				"get", v1beta1CrdbClusterResource, clusterName,
				"-o", "jsonpath={.status.migration.phase}")
			if err != nil {
				return false, nil
			}
			t.Logf("Current migration phase: %q", phase)
			if phase == "Failed" || (phase == phaseStopped && expectedPhase != phaseStopped) {
				return false, fmt.Errorf("migration entered %s while waiting for %s", phase, expectedPhase)
			}
			return migrationPhaseReached(phase, expectedPhase), nil
		},
	)
	if err != nil {
		// Log last error for debugging.
		lastErr, _ := k8s.RunKubectlAndGetOutputE(t, kubectlOptions,
			"get", v1beta1CrdbClusterResource, clusterName,
			"-o", "jsonpath={.status.migration.lastError}")
		t.Logf("Migration lastError: %s", lastErr)
		logMigrationResources(t, kubectlOptions, clusterName)
		logMigrationEvents(t, kubectlOptions, clusterName)
	}
	require.NoError(t, err, "timed out waiting for migration phase %q", expectedPhase)
}

// Allow an intentional resume to be observed before the forward waits start
// treating Stopped as a failure again.
func waitForMigrationResume(t *testing.T, options *k8s.KubectlOptions, name string) {
	t.Helper()
	err := wait.PollUntilContextTimeout(context.Background(), 2*time.Second, 2*time.Minute, true,
		func(context.Context) (bool, error) {
			phase, err := k8s.RunKubectlAndGetOutputE(t, options, "get", v1beta1CrdbClusterResource,
				name, "-o", "jsonpath={.status.migration.phase}")
			if err != nil {
				return false, err
			}
			if phase == "Failed" {
				return false, fmt.Errorf("migration failed while resuming")
			}
			return phase != "" && phase != phaseStopped, nil
		})
	require.NoError(t, err, "waiting for migration resume")
}

func migrationPhaseReached(currentPhase, expectedPhase string) bool {
	if currentPhase == expectedPhase {
		return true
	}

	expectedOrder, expectedOrdered := migrationPhaseOrder[expectedPhase]
	currentOrder, currentOrdered := migrationPhaseOrder[currentPhase]
	if expectedOrdered && currentOrdered && currentOrder >= expectedOrder {
		return true
	}

	return false
}

func logMigrationEvents(t *testing.T, kubectlOptions *k8s.KubectlOptions, clusterName string) {
	t.Helper()

	events, err := k8s.RunKubectlAndGetOutputE(
		t,
		kubectlOptions,
		"get", "events",
		"--field-selector", fmt.Sprintf("involvedObject.name=%s", clusterName),
		"--sort-by=.lastTimestamp",
	)
	if err != nil {
		t.Logf("Failed to fetch migration events for %s: %v", clusterName, err)
		return
	}

	t.Logf("Migration events for %s:\n%s", clusterName, events)
}

func logMigrationResources(t *testing.T, kubectlOptions *k8s.KubectlOptions, clusterName string) {
	t.Helper()

	for _, args := range [][]string{
		{"get", v1beta1CrdbClusterResource, clusterName, "-o", "yaml"},
		{"get", "crdbnodes.crdb.cockroachlabs.com", "-l", fmt.Sprintf("crdb.cockroachlabs.com/cluster=%s", clusterName), "-o", "wide"},
		{"get", "pods", "-l", fmt.Sprintf("crdb.cockroachlabs.com/cluster=%s", clusterName), "-o", "wide"},
		{"get", "poddisruptionbudgets", "-o", "yaml"},
	} {
		out, err := k8s.RunKubectlAndGetOutputE(t, kubectlOptions, args...)
		if err != nil {
			t.Logf("Failed to fetch migration resource state with kubectl %v: %v", args, err)
			continue
		}
		t.Logf("Migration resource state from kubectl %v:\n%s", args, out)
	}
}

func deleteStatefulSetAndWaitGone(
	t *testing.T, kubectlOptions *k8s.KubectlOptions, statefulSetName string, timeout time.Duration,
) {
	t.Helper()

	uidBeforeDelete, err := k8s.RunKubectlAndGetOutputE(
		t,
		kubectlOptions,
		"get", "sts", statefulSetName,
		"-o", "jsonpath={.metadata.uid}",
	)
	require.NoError(t, err)

	t.Logf("Deleting StatefulSet %s to trigger migration completion", statefulSetName)
	k8s.RunKubectl(t, kubectlOptions, "delete", "sts", statefulSetName, "--wait=true")

	ctx := context.Background()
	err = wait.PollUntilContextTimeout(ctx, 5*time.Second, timeout, true, func(ctx context.Context) (bool, error) {
		uidAfterDelete, err := k8s.RunKubectlAndGetOutputE(
			t,
			kubectlOptions,
			"get", "sts", statefulSetName,
			"-o", "jsonpath={.metadata.uid}",
		)
		if err != nil {
			return true, nil
		}

		t.Logf("StatefulSet %s still exists after delete. original UID=%q current UID=%q", statefulSetName, uidBeforeDelete, uidAfterDelete)
		return false, nil
	})
	require.NoError(t, err, "timed out waiting for StatefulSet %s to be fully deleted", statefulSetName)
}

// waitForSTSMigrationLabel polls the crdb.io/migrate label on the StatefulSet.
func waitForSTSMigrationLabel(
	t *testing.T,
	kubectlOptions *k8s.KubectlOptions,
	stsName, expectedValue string,
	timeout time.Duration,
) {
	ctx := context.Background()
	err := wait.PollUntilContextTimeout(ctx, 5*time.Second, timeout, true,
		func(ctx context.Context) (bool, error) {
			label, err := k8s.RunKubectlAndGetOutputE(t, kubectlOptions,
				"get", "sts", stsName,
				"-o", "jsonpath={.metadata.labels.crdb\\.io/migrate}")
			if err != nil {
				return false, nil
			}
			return label == expectedValue, nil
		},
	)
	require.NoError(t, err, "timed out waiting for STS migration label %q", expectedValue)
}

// waitForCrdbNodeCount polls until the number of CrdbNodes labelled for the cluster
// equals expectedCount and each replacement pod is Ready. Pass 0 to wait for all
// CrdbNodes to be deleted during rollback.
func waitForCrdbNodeCount(
	t *testing.T,
	kubectlOptions *k8s.KubectlOptions,
	clusterName string,
	expectedCount int,
	timeout time.Duration,
) {
	t.Logf("Waiting for %d CrdbNode(s) for cluster %s", expectedCount, clusterName)
	ctx := context.Background()
	err := wait.PollUntilContextTimeout(ctx, 10*time.Second, timeout, true,
		func(ctx context.Context) (bool, error) {
			if expectedCount > 0 {
				phase, err := k8s.RunKubectlAndGetOutputE(t, kubectlOptions,
					"get", v1beta1CrdbClusterResource, clusterName, "--ignore-not-found",
					"-o", "jsonpath={.status.migration.phase}")
				if err != nil {
					return false, err
				}
				// Helm migration creates the CrdbCluster asynchronously after the
				// StatefulSet is labeled. An absent cluster or phase is not a failure.
				if phase == "" {
					return false, nil
				}
				if phase == phaseStopped || phase == "Failed" {
					logMigrationResources(t, kubectlOptions, clusterName)
					logMigrationEvents(t, kubectlOptions, clusterName)
					return false, fmt.Errorf("migration entered %s while waiting for Ready replacements", phase)
				}
			}
			out, err := k8s.RunKubectlAndGetOutputE(t, kubectlOptions,
				"get", "crdbnode",
				"-l", fmt.Sprintf("crdb.cockroachlabs.com/cluster=%s", clusterName),
				"-o", "name")
			if err != nil {
				return false, nil
			}
			count := 0
			if strings.TrimSpace(out) != "" {
				count = len(strings.Split(strings.TrimSpace(out), "\n"))
			}
			t.Logf("CrdbNode count: %d (expected %d)", count, expectedCount)
			if count != expectedCount || expectedCount == 0 {
				return count == expectedCount, nil
			}
			for _, name := range strings.Fields(out) {
				_, podName, _ := strings.Cut(name, "/")
				pod, err := k8s.GetPodE(t, kubectlOptions, podName)
				if err != nil || !migrationReplacementReady(pod) {
					return false, nil
				}
			}
			return true, nil
		},
	)
	require.NoError(t, err, "timed out waiting for %d CrdbNode(s)", expectedCount)
}

// waitForSTSReplicas polls until the StatefulSet spec.replicas equals expectedReplicas.
func waitForSTSReplicas(
	t *testing.T,
	kubectlOptions *k8s.KubectlOptions,
	stsName string,
	expectedReplicas int,
	timeout time.Duration,
) {
	ctx := context.Background()
	err := wait.PollUntilContextTimeout(ctx, 10*time.Second, timeout, true,
		func(ctx context.Context) (bool, error) {
			out, err := k8s.RunKubectlAndGetOutputE(t, kubectlOptions,
				"get", "sts", stsName, "-o", "jsonpath={.spec.replicas}")
			if err != nil {
				return false, nil
			}
			var replicas int
			_, _ = fmt.Sscan(out, &replicas)
			return replicas == expectedReplicas, nil
		},
	)
	require.NoError(t, err, "timed out waiting for STS replicas to equal %d", expectedReplicas)
}

// requireMigrationComplete asserts that the migrated v1beta1 CrdbCluster is in
// the terminal migration state: spec.mode=MutableOnly,
// status.migration.phase=Complete, and crdb.io/migrate=complete.
func requireMigrationComplete(
	t *testing.T, kubectlOptions *k8s.KubectlOptions, clusterName string,
) {
	mode, err := k8s.RunKubectlAndGetOutputE(t, kubectlOptions,
		"get", v1beta1CrdbClusterResource, clusterName, "-o", "jsonpath={.spec.mode}")
	require.NoError(t, err)
	require.Equal(t, "MutableOnly", mode, "CrdbCluster spec.mode should be MutableOnly after migration")

	phase, err := k8s.RunKubectlAndGetOutputE(t, kubectlOptions,
		"get", v1beta1CrdbClusterResource, clusterName, "-o", "jsonpath={.status.migration.phase}")
	require.NoError(t, err)
	require.Equal(t, phaseComplete, phase, "migration phase should be Complete")
	requireJSONPathEquals(t, kubectlOptions, v1beta1CrdbClusterResource, clusterName,
		"{.spec.regions[0].code}", cloudRegion)
	requireJSONPathEqualsEventually(t, kubectlOptions, v1beta1CrdbClusterResource, clusterName,
		"{.status.region}", cloudRegion, 2*time.Minute)

	requireJSONPathEqualsEventually(t, kubectlOptions, v1beta1CrdbClusterResource, clusterName,
		"{.metadata.labels.crdb\\.io/migrate}", migrateLabelComplete, 2*time.Minute)
	requireMigrationPDBHandoff(t, kubectlOptions, clusterName)
	requireMigrationMarkersRemoved(t, kubectlOptions, clusterName)
}

// annotateResourcesForHelmAdoption applies Helm ownership annotations and labels to
// the resources created by the migration controller.
func annotateResourcesForHelmAdoption(
	t *testing.T, kubectlOptions *k8s.KubectlOptions, clusterName, helmReleaseName, namespace string,
) {
	serviceAccountName := clusterName
	if saName, err := k8s.RunKubectlAndGetOutputE(
		t,
		kubectlOptions,
		"get", v1beta1CrdbClusterResource, clusterName,
		"-o", "jsonpath={.spec.template.spec.podTemplate.spec.serviceAccountName}",
	); err == nil && saName != "" {
		serviceAccountName = saName
	}

	resources := []string{
		fmt.Sprintf("crdbcluster/%s", clusterName),
		fmt.Sprintf("serviceaccount/%s", serviceAccountName),
		fmt.Sprintf("service/%s", clusterName),
		fmt.Sprintf("service/%s-public", clusterName),
		fmt.Sprintf("role/%s", clusterName),
		fmt.Sprintf("rolebinding/%s", clusterName),
	}
	for _, resource := range resources {
		if _, err := k8s.RunKubectlAndGetOutputE(t, kubectlOptions, "get", resource); err != nil {
			continue
		}
		k8s.RunKubectl(t, kubectlOptions, "annotate", resource,
			fmt.Sprintf("meta.helm.sh/release-name=%s", helmReleaseName),
			fmt.Sprintf("meta.helm.sh/release-namespace=%s", namespace),
			"--overwrite")
		k8s.RunKubectl(t, kubectlOptions, "label", resource,
			"app.kubernetes.io/managed-by=Helm",
			"--overwrite")
	}
}

func exportValuesForHelmAdoption(t *testing.T, clusterName, namespace string) string {
	require.NoError(t, os.MkdirAll(manifestsDirPath, 0700))

	retry.DoWithRetry(t, "export helm values from migrated v1beta1 cluster", 5, 2*time.Second, func() (string, error) {
		exportValuesCmd := shell.Command{
			Command: migrationHelperPath,
			Args: []string{
				"export-values",
				"--kubeconfig", migrationKubeConfig,
				fmt.Sprintf("--crdb-cluster=%s", clusterName),
				fmt.Sprintf("--namespace=%s", namespace),
				fmt.Sprintf("--output-dir=%s", manifestsDirPath),
			},
		}
		return shell.RunCommandAndGetOutputE(t, exportValuesCmd)
	})

	return filepath.Join(manifestsDirPath, "values.yaml")
}

func adoptIntoCockroachDBHelmChart(
	t *testing.T,
	kubectlOptions *k8s.KubectlOptions,
	clusterName, helmReleaseName, namespace string,
	valuesFile string,
) {
	requireNoMigrationAnnotations(t, kubectlOptions, clusterName)
	annotateResourcesForHelmAdoption(t, kubectlOptions, clusterName, helmReleaseName, namespace)
	annotateIngressResourcesForHelmAdoption(t, kubectlOptions, helmReleaseName, namespace)
	deleteStaleClusterScopedRBAC(t, kubectlOptions, clusterName, namespace)

	helmPath, _ := operator.HelmChartPaths()
	verifyMigrationChartApply(t, kubectlOptions, helmPath, helmReleaseName, clusterName, valuesFile)
	helm.Upgrade(t, &helm.Options{
		KubectlOptions: kubectlOptions,
		ValuesFiles:    []string{valuesFile},
		SetValues: migrationChartValues(map[string]string{
			"cockroachdb.crdbCluster.rollingRestartDelay": "10s",
		}),
		ExtraArgs: map[string][]string{
			"upgrade": {"--force-conflicts"},
		},
	}, helmPath, helmReleaseName)
	requireHelmSSAOwnership(t, kubectlOptions, clusterName)
	requireNoOperatorSpecOwnership(t, kubectlOptions, clusterName)

	managedBy, err := k8s.RunKubectlAndGetOutputE(t, kubectlOptions, "get", v1beta1CrdbClusterResource, clusterName,
		"-o", "jsonpath={.metadata.labels.app\\.kubernetes\\.io/managed-by}")
	require.NoError(t, err)
	require.Equal(t, "Helm", managedBy)

	verifyHelmAdoptionSupportsUpgrade(t, kubectlOptions, helmPath, clusterName, helmReleaseName, valuesFile)
}

// requireNoLegacyV1Alpha1ManagedFields verifies migration completion removed
// v1alpha1 field ownership before an SSA manager adopts the v1beta1 object.
func requireNoLegacyV1Alpha1ManagedFields(
	t *testing.T, kubectlOptions *k8s.KubectlOptions, clusterName string,
) {
	t.Helper()

	raw, err := k8s.RunKubectlAndGetOutputE(t, kubectlOptions,
		"get", v1beta1CrdbClusterResource, clusterName,
		"--show-managed-fields=true", "-o", "json")
	require.NoError(t, err)

	var object struct {
		Metadata struct {
			ManagedFields []json.RawMessage `json:"managedFields"`
		} `json:"metadata"`
	}
	require.NoError(t, json.Unmarshal([]byte(raw), &object))

	for _, rawEntry := range object.Metadata.ManagedFields {
		var entry struct {
			Manager    string `json:"manager"`
			APIVersion string `json:"apiVersion"`
		}
		require.NoError(t, json.Unmarshal(rawEntry, &entry))
		require.NotEqual(t, "crdb.cockroachlabs.com/v1alpha1", entry.APIVersion,
			"migration completion retained legacy v1alpha1 ownership for manager %q", entry.Manager)
	}
}

// annotateIngressResourcesForHelmAdoption transfers Helm ownership for ingress
// resources when the matching values.yaml enables them.
func annotateIngressResourcesForHelmAdoption(
	t *testing.T, kubectlOptions *k8s.KubectlOptions, helmReleaseName, namespace string,
) {
	for _, resource := range []string{
		fmt.Sprintf("ingress/ui-%s", helmReleaseName),
		fmt.Sprintf("ingress/sql-%s", helmReleaseName),
	} {
		if _, err := k8s.RunKubectlAndGetOutputE(t, kubectlOptions, "get", resource); err != nil {
			continue
		}
		k8s.RunKubectl(t, kubectlOptions, "annotate", resource,
			fmt.Sprintf("meta.helm.sh/release-name=%s", helmReleaseName),
			fmt.Sprintf("meta.helm.sh/release-namespace=%s", namespace),
			"--overwrite")
		k8s.RunKubectl(t, kubectlOptions, "label", resource,
			"app.kubernetes.io/managed-by=Helm",
			"--overwrite")
	}
}

func verifyHelmAdoptionSupportsUpgrade(
	t *testing.T,
	kubectlOptions *k8s.KubectlOptions,
	helmPath, clusterName, helmReleaseName, valuesFile string,
) {
	initialPods := k8s.ListPods(t, kubectlOptions, metav1.ListOptions{
		LabelSelector: operator.LabelSelector,
	})
	require.NotEmpty(t, initialPods, "no cockroachdb pods found before helm upgrade")

	initialPodUIDs := make(map[string]string, len(initialPods))
	for _, pod := range initialPods {
		initialPodUIDs[pod.Name] = string(pod.UID)
	}

	upgradeTime := time.Now().UTC()

	helm.Upgrade(t, &helm.Options{
		KubectlOptions: kubectlOptions,
		ValuesFiles:    []string{valuesFile},
		SetValues: migrationChartValues(map[string]string{
			"cockroachdb.crdbCluster.rollingRestartDelay": "12s",
			"cockroachdb.crdbCluster.timestamp":           upgradeTime.Format(time.RFC3339),
		}),
	}, helmPath, helmReleaseName)
	requireHelmSSAOwnership(t, kubectlOptions, clusterName)

	_, err := retry.DoWithRetryE(t, "wait for helm adoption upgrade rollout", 60, 10*time.Second, func() (string, error) {
		restartAnnotation, err := k8s.RunKubectlAndGetOutputE(
			t,
			kubectlOptions,
			"get", v1beta1CrdbClusterResource, clusterName,
			"-o", "jsonpath={.spec.template.spec.podTemplate.metadata.annotations.helm\\.sh/restartedAt}",
		)
		if err != nil {
			return "", err
		}
		if restartAnnotation != upgradeTime.Format(time.RFC3339) {
			return "", fmt.Errorf("expected restartedAt annotation %q but found %q", upgradeTime.Format(time.RFC3339), restartAnnotation)
		}

		pods := k8s.ListPods(t, kubectlOptions, metav1.ListOptions{
			LabelSelector: operator.LabelSelector,
		})
		if len(pods) != len(initialPods) {
			return "", fmt.Errorf("expected %d cockroachdb pods after helm upgrade, found %d", len(initialPods), len(pods))
		}

		for _, pod := range pods {
			if initialPodUIDs[pod.Name] == string(pod.UID) {
				return "", fmt.Errorf("pod %s has not been recreated after helm upgrade", pod.Name)
			}
			if pod.Status.Phase != corev1.PodRunning {
				return "", fmt.Errorf("pod %s is not running after helm upgrade", pod.Name)
			}
			if !k8s.IsPodAvailable(&pod) {
				return "", fmt.Errorf("pod %s is not ready after helm upgrade", pod.Name)
			}
		}

		return "helm adoption upgrade completed", nil
	})
	require.NoError(t, err)
	requireJSONPathEquals(t, kubectlOptions, v1beta1CrdbClusterResource, clusterName,
		"{.spec.rollingRestartDelay}", "12s")
	requireNoOperatorSpecOwnership(t, kubectlOptions, clusterName)
}

// verifyFreshV1Beta1HelmClusterDuringCoexistence verifies that a native
// v1beta1 cluster can be installed and upgraded with Helm 4 SSA while an active,
// non-Helm v1alpha1 cluster remains available in the same watched namespace.
func verifyFreshV1Beta1HelmClusterDuringCoexistence(
	t *testing.T,
	kubectlOptions *k8s.KubectlOptions,
	namespace, sourceClusterName string,
	sourceCluster testutil.CockroachCluster,
) {
	t.Helper()

	requireJSONPathEquals(t, kubectlOptions, v1alpha1CrdbClusterResource, sourceClusterName,
		"{.metadata.labels.crdb\\.io/skip-reconcile}", "")

	const (
		freshReleaseName = "ssa-coexistence"
		freshClusterName = "ssa-coexistence"
	)

	helmPath, _ := operator.HelmChartPaths()
	installOptions := &helm.Options{
		KubectlOptions: kubectlOptions,
		SetValues: operator.PatchHelmValues(map[string]string{
			"k8s.fullnameOverride":               freshClusterName,
			"cockroachdb.tls.enabled":            "false",
			"cockroachdb.tls.selfSigner.enabled": "false",
			"cockroachdb.crdbCluster.image.name": migration.CockroachVersion,
		}),
		SetJsonValues: map[string]string{
			"cockroachdb.crdbCluster.regions": operator.MustMarshalJSON([]map[string]interface{}{
				{
					"code":          cloudRegion,
					"nodes":         1,
					"cloudProvider": cloudProvider,
					"namespace":     namespace,
				},
			}),
		},
	}
	installOptions.SetValues["cockroachdb.crdbCluster.rollingRestartDelay"] = "10s"

	t.Log("Installing a native v1beta1 cluster with Helm 4 SSA during API coexistence")
	helm.Install(t, installOptions, helmPath, freshReleaseName)
	defer func() {
		t.Log("Removing the native v1beta1 coexistence cluster")
		helm.Delete(t, &helm.Options{KubectlOptions: kubectlOptions}, freshReleaseName, true)
		waitForClusterPodCount(t, kubectlOptions, freshClusterName, 0, 5*time.Minute)
	}()

	requireHelmSSAOwnership(t, kubectlOptions, freshClusterName)
	requireNoMigrationAnnotations(t, kubectlOptions, freshClusterName)
	waitForClusterPodCount(t, kubectlOptions, freshClusterName, 1, 10*time.Minute)

	freshCluster := testutil.CockroachCluster{
		Cfg:             cfg,
		K8sClient:       k8sClient,
		StatefulSetName: freshClusterName,
		Namespace:       namespace,
		DesiredNodes:    1,
		Context:         k3dClusterName,
	}
	freshPodName := requireSingleClusterPodName(t, kubectlOptions, freshClusterName)
	testutil.RequireCRDBClusterToFunction(t, freshCluster, false, freshPodName)

	upgradeOptions := &helm.Options{
		KubectlOptions: kubectlOptions,
		SetValues: map[string]string{
			"k8s.labels.helm-ssa-upgrade": "verified",
		},
		ExtraArgs: map[string][]string{
			"upgrade": {"--reuse-values"},
		},
	}
	t.Log("Upgrading the native v1beta1 cluster with Helm 4 SSA")
	helm.Upgrade(t, upgradeOptions, helmPath, freshReleaseName)
	requireHelmSSAOwnership(t, kubectlOptions, freshClusterName)
	requireNoMigrationAnnotations(t, kubectlOptions, freshClusterName)
	requireJSONPathEquals(t, kubectlOptions, v1beta1CrdbClusterResource, freshClusterName,
		"{.metadata.labels.helm-ssa-upgrade}", "verified")
	waitForClusterPodCount(t, kubectlOptions, freshClusterName, 1, 5*time.Minute)
	freshPodName = requireSingleClusterPodName(t, kubectlOptions, freshClusterName)
	testutil.RequireCRDBClusterToFunction(t, freshCluster, true, freshPodName)

	// The source remains active, functional, and outside Helm ownership while
	// the native v1beta1 release is installed and upgraded.
	requireJSONPathEquals(t, kubectlOptions, v1alpha1CrdbClusterResource, sourceClusterName,
		"{.metadata.labels.crdb\\.io/skip-reconcile}", "")
	helmManagedBy, err := k8s.RunKubectlAndGetOutputE(t, kubectlOptions,
		"get", v1alpha1CrdbClusterResource, sourceClusterName,
		"-o", "jsonpath={.metadata.labels.app\\.kubernetes\\.io/managed-by}")
	require.NoError(t, err)
	require.Empty(t, helmManagedBy, "v1alpha1 source cluster must not be managed by Helm")
	testutil.RequireClusterToBeReadyEventuallyTimeout(t, sourceCluster, 5*time.Minute)
	testutil.RequireCRDBToFunction(t, sourceCluster, true)
}

func requireSingleClusterPodName(
	t *testing.T, kubectlOptions *k8s.KubectlOptions, clusterName string,
) string {
	t.Helper()
	pods, err := k8s.ListPodsE(t, kubectlOptions, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("crdb.cockroachlabs.com/cluster=%s", clusterName),
	})
	require.NoError(t, err)

	readyPodNames := make([]string, 0, len(pods))
	for i := range pods {
		if pods[i].DeletionTimestamp == nil && k8s.IsPodAvailable(&pods[i]) {
			readyPodNames = append(readyPodNames, pods[i].Name)
		}
	}
	require.Len(t, readyPodNames, 1,
		"expected one Ready pod for cluster %s, found %v", clusterName, readyPodNames)
	return readyPodNames[0]
}

func waitForClusterPodCount(
	t *testing.T,
	kubectlOptions *k8s.KubectlOptions,
	clusterName string,
	expected int,
	timeout time.Duration,
) {
	t.Helper()
	const requiredStableReadyObservations = 3

	ctx := context.Background()
	stablePodSet := ""
	stableReadyObservations := 0
	err := wait.PollUntilContextTimeout(ctx, 10*time.Second, timeout, true,
		func(ctx context.Context) (bool, error) {
			pods, err := k8s.ListPodsE(t, kubectlOptions, metav1.ListOptions{
				LabelSelector: fmt.Sprintf("crdb.cockroachlabs.com/cluster=%s", clusterName),
			})
			if err != nil {
				return false, err
			}
			if len(pods) != expected {
				stablePodSet = ""
				stableReadyObservations = 0
				t.Logf("expected %d pod(s) for cluster %s; found %d", expected, clusterName, len(pods))
				return false, nil
			}
			if expected == 0 {
				return true, nil
			}

			podIDs := make([]string, 0, len(pods))
			for i := range pods {
				if pods[i].DeletionTimestamp != nil || !k8s.IsPodAvailable(&pods[i]) {
					stablePodSet = ""
					stableReadyObservations = 0
					return false, nil
				}
				podIDs = append(podIDs, fmt.Sprintf("%s:%s", pods[i].Name, pods[i].UID))
			}

			sort.Strings(podIDs)
			currentPodSet := strings.Join(podIDs, ",")
			if currentPodSet != stablePodSet {
				stablePodSet = currentPodSet
				stableReadyObservations = 1
				return false, nil
			}

			stableReadyObservations++
			if stableReadyObservations < requiredStableReadyObservations {
				t.Logf("waiting for cluster %s pods to remain stable (%d/%d)",
					clusterName, stableReadyObservations, requiredStableReadyObservations)
				return false, nil
			}
			return true, nil
		},
	)
	require.NoError(t, err)
}

// deleteStaleClusterScopedRBAC removes the migration-created ClusterRole and
// ClusterRoleBinding that are not adopted by the Helm chart.
func deleteStaleClusterScopedRBAC(
	t *testing.T, kubectlOptions *k8s.KubectlOptions, clusterName, namespace string,
) {
	k8s.RunKubectl(t, kubectlOptions, "delete", "clusterrole",
		fmt.Sprintf("%s-%s", namespace, clusterName), "--ignore-not-found")
	k8s.RunKubectl(t, kubectlOptions, "delete", "clusterrolebinding",
		fmt.Sprintf("%s-%s", namespace, clusterName), "--ignore-not-found")
}

// Verify the controller completed the PDB handoff before chart adoption. Deleting
// source budgets in the test would hide a missing controller cleanup step.
func requireMigrationPDBHandoff(
	t *testing.T, kubectlOptions *k8s.KubectlOptions, clusterName string,
) {
	t.Helper()
	for _, name := range []string{clusterName, clusterName + "-budget"} {
		retry.DoWithRetry(t, "wait for source PDB removal: "+name, 12, 5*time.Second, func() (string, error) {
			out, err := k8s.RunKubectlAndGetOutputE(t, kubectlOptions, "get", "pdb", name, "--ignore-not-found", "-o", "name")
			if err != nil {
				return "", err
			}
			if strings.TrimSpace(out) != "" {
				return "", fmt.Errorf("source PDB %s remains after handoff", name)
			}
			return "source PDB removed", nil
		})
	}
	raw, err := k8s.RunKubectlAndGetOutputE(t, kubectlOptions, "get", "pdb", clusterName+"-pdb", "-o", "json")
	require.NoError(t, err)
	var pdb policyv1.PodDisruptionBudget
	require.NoError(t, json.Unmarshal([]byte(raw), &pdb))
	require.NotNil(t, pdb.Spec.MinAvailable)
	require.Equal(t, map[string]string{
		"app": "cockroachdb", "crdb.cockroachlabs.com/cluster": clusterName,
	}, pdb.Spec.Selector.MatchLabels)
	raw, err = k8s.RunKubectlAndGetOutputE(t, kubectlOptions, "get", v1beta1CrdbClusterResource, clusterName, "-o", "json")
	require.NoError(t, err)
	var cluster v1beta1.CrdbCluster
	require.NoError(t, json.Unmarshal([]byte(raw), &cluster))
	expected := 0
	for _, region := range cluster.Spec.Regions {
		expected += int(region.Nodes)
	}
	require.Positive(t, expected)
	waitForCrdbNodeCount(t, kubectlOptions, clusterName, expected, 3*time.Minute)
	require.Equal(t, expected-1, pdb.Spec.MinAvailable.IntValue(), "handoff must allow one unavailable node")
	names, err := k8s.RunKubectlAndGetOutputE(t, kubectlOptions, "get", "pdb", "-o", "jsonpath={.items[*].metadata.name}")
	require.NoError(t, err)
	require.Equal(t, []string{clusterName + "-pdb"}, strings.Fields(names), "handoff left extra disruption budgets")
}

func requireHelmSourceFieldsPreserved(
	t *testing.T, kubectlOptions *k8s.KubectlOptions, clusterName string,
) {
	requireJSONPathEquals(t, kubectlOptions, v1beta1CrdbClusterResource, clusterName,
		"{.spec.template.spec.walFailoverSpec.path}", "/cockroach/wal-failover")
	requireJSONPathEquals(t, kubectlOptions, v1beta1CrdbClusterResource, clusterName,
		"{.spec.template.spec.walFailoverSpec.size}", "1Gi")
	requireJSONPathEquals(t, kubectlOptions, v1beta1CrdbClusterResource, clusterName,
		"{.spec.template.spec.logsStore.mountPath}", "/cockroach/test-logs/")
	requireJSONPathEquals(t, kubectlOptions, v1beta1CrdbClusterResource, clusterName,
		"{.spec.template.spec.logsStore.volumeClaimTemplate.metadata.name}", "logsdir")
	requireJSONPathEquals(t, kubectlOptions, v1beta1CrdbClusterResource, clusterName,
		"{.spec.template.spec.logsStore.volumeClaimTemplate.spec.resources.requests.storage}", "1Gi")
	requireJSONPathEquals(t, kubectlOptions, v1beta1CrdbClusterResource, clusterName,
		"{.spec.template.spec.loggingConfigMapName}", fmt.Sprintf("%s-log-config", clusterName))
	requireJSONPathContains(t, kubectlOptions, v1beta1CrdbClusterResource, clusterName,
		"{.spec.template.spec.startFlags}", fmt.Sprintf("region=%s,zone=$(printf zone-a)", cloudRegion))
	requirePodJoin(t, kubectlOptions, clusterName, fmt.Sprintf("%s-join.%s.svc.cluster.local:26257", clusterName, kubectlOptions.Namespace))
	requireNodeCertificateNames(t, kubectlOptions, clusterName+"-node-secret", "migration.example.test", "192.0.2.10", clusterName+"-join."+kubectlOptions.Namespace+".svc.cluster.local")
	requireJSONPathEquals(t, kubectlOptions, "crdbnode", fmt.Sprintf("%s-0", clusterName),
		"{.spec.logsStore.mountPath}", "/cockroach/test-logs/")
	requireJSONPathEquals(t, kubectlOptions, "crdbnode", fmt.Sprintf("%s-0", clusterName),
		"{.spec.logsStore.volumeClaimTemplate.metadata.name}", "logsdir")
}

func requireOperatorSourceFieldsPreserved(
	t *testing.T, kubectlOptions *k8s.KubectlOptions, clusterName string,
) {
	t.Helper()
	raw, err := k8s.RunKubectlAndGetOutputE(t, kubectlOptions,
		"get", v1beta1CrdbClusterResource, clusterName, "-o", "json")
	require.NoError(t, err)
	var cluster v1beta1.CrdbCluster
	require.NoError(t, json.Unmarshal([]byte(raw), &cluster))
	require.Len(t, cluster.Spec.Regions, 1)
	region := cluster.Spec.Regions[0]
	require.Equal(t, cloudRegion, region.Code)
	require.Equal(t, cloudProvider, region.CloudProvider,
		"the region must retain the source cloudProvider through migration and chart adoption")
	require.Positive(t, region.Nodes)

	requireJSONPathEquals(t, kubectlOptions, v1beta1CrdbClusterResource, clusterName,
		"{.spec.template.spec.loggingConfigMapName}", "logging-config")
	requireJSONPathEquals(t, kubectlOptions, v1beta1CrdbClusterResource, clusterName,
		"{.spec.template.spec.podTemplate.spec.priorityClassName}", "operator-critical")
	requireJSONPathEquals(t, kubectlOptions, v1beta1CrdbClusterResource, clusterName,
		"{.spec.template.spec.podTemplate.spec.terminationGracePeriodSeconds}", "60")
	requireJSONPathEquals(t, kubectlOptions, v1beta1CrdbClusterResource, clusterName,
		"{.spec.template.spec.podTemplate.metadata.annotations.crdb}", "isCool")
	requireJSONPathEquals(t, kubectlOptions, v1beta1CrdbClusterResource, clusterName,
		"{.spec.template.spec.podTemplate.spec.containers[0].resources.requests.cpu}", "200m")
	requireJSONPathEquals(t, kubectlOptions, "crdbnode", fmt.Sprintf("%s-0", clusterName),
		"{.spec.podTemplate.metadata.annotations.crdb}", "isCool")
	requireJSONPathEquals(t, kubectlOptions, "crdbnode", fmt.Sprintf("%s-0", clusterName),
		"{.spec.podTemplate.spec.containers[0].resources.requests.cpu}", "200m")

	logConfigData, err := k8s.RunKubectlAndGetOutputE(t, kubectlOptions,
		"get", "configmap", "logging-config", "-o", "jsonpath={.data}")
	require.NoError(t, err)
	require.Contains(t, logConfigData, "logs.yaml",
		"logging ConfigMap should use logs.yaml key after migration")
	require.NotContains(t, logConfigData, "logging.yaml",
		"logging ConfigMap should no longer have logging.yaml key after migration")
}

func requireJSONPathEquals(
	t *testing.T, kubectlOptions *k8s.KubectlOptions, resource, name, jsonPath, expected string,
) {
	t.Helper()
	out, err := k8s.RunKubectlAndGetOutputE(t, kubectlOptions, "get", resource, name, "-o", "jsonpath="+jsonPath)
	require.NoError(t, err)
	require.Equal(t, expected, out)
}

func requireJSONPathEqualsEventually(
	t *testing.T,
	kubectlOptions *k8s.KubectlOptions,
	resource, name, jsonPath, expected string,
	timeout time.Duration,
) {
	t.Helper()
	_, err := retry.DoWithRetryE(t, "wait for "+jsonPath+" on "+resource+"/"+name,
		int(timeout/(2*time.Second)), 2*time.Second, func() (string, error) {
			out, err := k8s.RunKubectlAndGetOutputE(t, kubectlOptions,
				"get", resource, name, "-o", "jsonpath="+jsonPath)
			if err != nil {
				return "", err
			}
			if out != expected {
				return "", fmt.Errorf("expected %s to equal %q, got %q", jsonPath, expected, out)
			}
			return out, nil
		})
	require.NoError(t, err)
}

func requireJSONPathContains(
	t *testing.T, kubectlOptions *k8s.KubectlOptions, resource, name, jsonPath, expected string,
) {
	t.Helper()
	out, err := k8s.RunKubectlAndGetOutputE(t, kubectlOptions, "get", resource, name, "-o", "jsonpath="+jsonPath)
	require.NoError(t, err)
	require.Contains(t, out, expected)
}
