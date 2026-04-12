package migrate

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
)

const (
	autoMigrationOperatorRelease = "crdb-migration-operator"
	v1alpha1CrdbClusterResource  = "crdbclusters.v1alpha1.crdb.cockroachlabs.com"
	v1beta1CrdbClusterResource   = "crdbclusters.v1beta1.crdb.cockroachlabs.com"

	// Migration phases as reported in status.migration.phase.
	phaseCertMigration = "CertMigration"
	phasePodMigration  = "PodMigration"
	phaseFinalization  = "Finalization"
	phaseComplete      = "Complete"
	phaseStopped       = "Stopped"

	// Migration label values set by the controller on the source resource.
	migrateLabelStopped = "stopped"

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
	t.Run("helm stop and resume", testHelmAutoStopResume)
	t.Run("helm rollback", testHelmAutoRollback)
	t.Run("operator forward migration", testOperatorAutoForwardMigration)
	t.Run("operator stop and resume", testOperatorAutoStopResume)
	t.Run("operator rollback", testOperatorAutoRollback)
	t.Run("helm cert-manager migration", testHelmAutoCertManagerMigration)
}

// testHelmAutoForwardMigration installs a Helm StatefulSet cluster, runs the full
// automatic migration to completion, validates data integrity, then adopts it via
// the Helm chart.
func testHelmAutoForwardMigration(t *testing.T) {
	ns := "cockroach" + strings.ToLower(random.UniqueId())
	kubectlOptions := k8s.NewKubectlOptions("", "", ns)

	cleanupPublicOperatorWebhookArtifacts(t)
	stsName, crdbCluster := installHelmSourceCluster(t, kubectlOptions, ns)
	defer func() {
		_ = os.RemoveAll(manifestsDirPath)
	}()
	defer func() {
		_ = helm.DeleteE(t, &helm.Options{KubectlOptions: kubectlOptions}, releaseName, true)
		waitForClusterResourcesGone(t, kubectlOptions)
		uninstallMigrationOperator(t, kubectlOptions)
		k8s.DeleteNamespace(t, kubectlOptions, ns)
	}()

	testutil.RequireCRDBClusterToBeReadyEventuallyTimeout(t, kubectlOptions, crdbCluster, 600*time.Second)
	testutil.RequireCRDBToFunction(t, crdbCluster, false)

	installMigrationOperator(t, kubectlOptions, ns)

	// Start migration.
	k8s.RunKubectl(t, kubectlOptions, "label", "sts", stsName, "crdb.io/migrate=start")

	// Wait for migration controller to create CrdbCluster and advance through phases.
	waitForMigrationPhase(t, kubectlOptions, stsName, phaseCertMigration, 5*time.Minute)
	waitForMigrationPhase(t, kubectlOptions, stsName, phasePodMigration, 10*time.Minute)

	// Verify CrdbNodes are being created one by one and cluster stays healthy.
	waitForCrdbNodeCount(t, kubectlOptions, stsName, crdbCluster.DesiredNodes, 20*time.Minute)

	waitForMigrationPhase(t, kubectlOptions, stsName, phaseFinalization, 5*time.Minute)

	// Finalization complete: delete the StatefulSet to trigger Phase=Complete.
	t.Log("Deleting StatefulSet to trigger migration completion")
	deleteStatefulSetAndWaitGone(t, kubectlOptions, stsName, 3*time.Minute)

	waitForMigrationPhase(t, kubectlOptions, stsName, phaseComplete, 3*time.Minute)

	// Assert final state.
	requireMigrationComplete(t, kubectlOptions, stsName)
	requireHelmSourceFieldsPreserved(t, kubectlOptions, stsName)

	// Validate data survived migration.
	crdbCluster.ClientSecret = fmt.Sprintf("%s-client-secret", stsName)
	crdbCluster.NodeSecret = fmt.Sprintf("%s-node-secret", stsName)
	testutil.RequireCRDBToFunction(t, crdbCluster, true)

	valuesFile := exportValuesForHelmAdoption(t, stsName, ns)

	// Adopt into Helm chart with values generated from the live migrated v1beta1
	// cluster so takeover reflects the actual controller output.
	adoptIntoCockroachDBHelmChart(t, kubectlOptions, stsName, releaseName, ns, valuesFile)
	requireHelmSourceFieldsPreserved(t, kubectlOptions, stsName)
	testutil.RequireCRDBToFunction(t, crdbCluster, true)
}

// testHelmAutoStopResume migrates a Helm cluster, stops mid-way, verifies the cluster
// stays functional during the pause, then resumes to completion.
func testHelmAutoStopResume(t *testing.T) {
	ns := "cockroach" + strings.ToLower(random.UniqueId())
	kubectlOptions := k8s.NewKubectlOptions("", "", ns)

	cleanupPublicOperatorWebhookArtifacts(t)
	stsName, crdbCluster := installHelmSourceCluster(t, kubectlOptions, ns)
	defer func() {
		_ = os.RemoveAll(manifestsDirPath)
	}()
	defer func() {
		_ = helm.DeleteE(t, &helm.Options{KubectlOptions: kubectlOptions}, releaseName, true)
		waitForClusterResourcesGone(t, kubectlOptions)
		uninstallMigrationOperator(t, kubectlOptions)
		k8s.DeleteNamespace(t, kubectlOptions, ns)
	}()

	testutil.RequireCRDBClusterToBeReadyEventuallyTimeout(t, kubectlOptions, crdbCluster, 600*time.Second)
	testutil.RequireCRDBToFunction(t, crdbCluster, false)

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

	// Resume migration.
	k8s.RunKubectl(t, kubectlOptions, "label", "sts", stsName, "crdb.io/migrate=start", "--overwrite")
	waitForCrdbNodeCount(t, kubectlOptions, stsName, crdbCluster.DesiredNodes, 20*time.Minute)
	waitForMigrationPhase(t, kubectlOptions, stsName, phaseFinalization, 5*time.Minute)

	// Per the migration docs, the source StatefulSet must be deleted to move
	// from Finalization to Complete.
	deleteStatefulSetAndWaitGone(t, kubectlOptions, stsName, 3*time.Minute)
	waitForMigrationPhase(t, kubectlOptions, stsName, phaseComplete, 3*time.Minute)
	requireMigrationComplete(t, kubectlOptions, stsName)
	requireHelmSourceFieldsPreserved(t, kubectlOptions, stsName)
	testutil.RequireCRDBToFunction(t, crdbCluster, true)

	valuesFile := exportValuesForHelmAdoption(t, stsName, ns)
	adoptIntoCockroachDBHelmChart(t, kubectlOptions, stsName, releaseName, ns, valuesFile)
	requireHelmSourceFieldsPreserved(t, kubectlOptions, stsName)
	testutil.RequireCRDBToFunction(t, crdbCluster, true)
}

// testHelmAutoRollback starts migration, waits until at least one node is replaced,
// then triggers rollback and verifies the StatefulSet is fully restored.
func testHelmAutoRollback(t *testing.T) {
	ns := "cockroach" + strings.ToLower(random.UniqueId())
	kubectlOptions := k8s.NewKubectlOptions("", "", ns)

	cleanupPublicOperatorWebhookArtifacts(t)
	stsName, crdbCluster := installHelmSourceCluster(t, kubectlOptions, ns)
	defer func() {
		_ = os.RemoveAll(manifestsDirPath)
	}()
	defer func() {
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

	installMigrationOperator(t, kubectlOptions, ns)

	k8s.RunKubectl(t, kubectlOptions, "label", "sts", stsName, "crdb.io/migrate=start")
	waitForMigrationPhase(t, kubectlOptions, stsName, phasePodMigration, 10*time.Minute)
	// At least 1 CrdbNode must exist before rolling back.
	waitForCrdbNodeCount(t, kubectlOptions, stsName, 1, 5*time.Minute)

	// Remove the label to trigger rollback.
	k8s.RunKubectl(t, kubectlOptions, "label", "sts", stsName, "crdb.io/migrate-")

	// Verify rollback: STS restored to original replicas, all CrdbNodes deleted.
	waitForSTSReplicas(t, kubectlOptions, stsName, crdbCluster.DesiredNodes, 10*time.Minute)
	waitForCrdbNodeCount(t, kubectlOptions, stsName, 0, 5*time.Minute)

	// Cluster must be healthy after rollback.
	testutil.RequireCRDBClusterToBeReadyEventuallyTimeout(t, kubectlOptions, crdbCluster, 300*time.Second)
	testutil.RequireCRDBToFunction(t, crdbCluster, true)
}

// testOperatorAutoForwardMigration migrates a public-operator cluster to the Cloud
// operator, validates data integrity, then cleans up public operator resources.
func testOperatorAutoForwardMigration(t *testing.T) {
	ns := "cockroach" + strings.ToLower(random.UniqueId())
	kubectlOptions := k8s.NewKubectlOptions("", "", ns)

	clusterName, crdbCluster := installPublicOperatorSourceCluster(t, kubectlOptions, ns)
	defer func() {
		_ = os.RemoveAll(manifestsDirPath)
	}()
	defer func() {
		_ = helm.DeleteE(t, &helm.Options{KubectlOptions: kubectlOptions}, releaseName, true)
		waitForClusterResourcesGone(t, kubectlOptions)
		uninstallMigrationOperator(t, kubectlOptions)
		k8s.RunKubectl(t, kubectlOptions, "delete", "priorityclass", "operator-critical", "--ignore-not-found")
		publicOperatorKubectlOptions := k8s.NewKubectlOptions("", "", migration.OperatorNamespace)
		k8s.RunKubectl(t, publicOperatorKubectlOptions, "delete", "deployment", migration.OperatorDeploymentName, "--ignore-not-found=true")
		k8s.RunKubectl(t, kubectlOptions, "delete", "clusterrole", "cockroachdb-operator", "--ignore-not-found")
		k8s.RunKubectl(t, kubectlOptions, "delete", "clusterrolebinding", "cockroachdb-operator", "--ignore-not-found")
		k8s.DeleteNamespace(t, kubectlOptions, ns)
	}()

	testutil.RequireClusterToBeReadyEventuallyTimeout(t, crdbCluster, 600*time.Second)
	testutil.RequireCRDBToFunction(t, crdbCluster, false)

	// Step 1: Pause public operator before installing Cloud operator.
	k8s.RunKubectl(t, kubectlOptions, "label", v1alpha1CrdbClusterResource, clusterName, "crdb.io/skip-reconcile=true", "--overwrite")

	// Step 2: Annotate CrdbCluster with cloud region and provider for conversion.
	retry.DoWithRetry(t, "annotate crdbcluster", 5, 2*time.Second, func() (string, error) {
		if err := k8s.RunKubectlE(t, kubectlOptions, "annotate", v1alpha1CrdbClusterResource, clusterName,
			fmt.Sprintf("crdb.cockroachlabs.com/cloudProvider=%s", cloudProvider), "--overwrite"); err != nil {
			return "", err
		}
		return k8s.RunKubectlAndGetOutputE(t, kubectlOptions, "annotate", v1alpha1CrdbClusterResource, clusterName,
			fmt.Sprintf("crdb.cockroachlabs.com/regionCode=%s", cloudRegion), "--overwrite")
	})

	// Step 3: Install Cloud operator scoped to this namespace.
	installMigrationOperator(t, kubectlOptions, ns)

	// Step 4: Start migration.
	k8s.RunKubectl(t, kubectlOptions, "label", v1alpha1CrdbClusterResource, clusterName, "crdb.io/migrate=start")

	waitForMigrationPhase(t, kubectlOptions, clusterName, phaseCertMigration, 5*time.Minute)
	waitForMigrationPhase(t, kubectlOptions, clusterName, phasePodMigration, 10*time.Minute)
	waitForCrdbNodeCount(t, kubectlOptions, clusterName, crdbCluster.DesiredNodes, 20*time.Minute)
	waitForMigrationPhase(t, kubectlOptions, clusterName, phaseFinalization, 5*time.Minute)

	// Per the migration docs, the source StatefulSet must be deleted to move
	// from Finalization to Complete.
	deleteStatefulSetAndWaitGone(t, kubectlOptions, clusterName, 3*time.Minute)
	waitForMigrationPhase(t, kubectlOptions, clusterName, phaseComplete, 3*time.Minute)
	requireMigrationComplete(t, kubectlOptions, clusterName)
	requireOperatorSourceFieldsPreserved(t, kubectlOptions, clusterName)

	// Validate data survived migration.
	crdbCluster.ClientSecret = fmt.Sprintf("%s-client-secret", clusterName)
	crdbCluster.NodeSecret = fmt.Sprintf("%s-node-secret", clusterName)
	testutil.RequireCRDBToFunction(t, crdbCluster, true)

	// Keep skip-reconcile on the source v1alpha1 resource after successful
	// migration so the public operator cannot resume reconciling it.
	skipReconcile, _ := k8s.RunKubectlAndGetOutputE(t, kubectlOptions, "get", v1alpha1CrdbClusterResource, clusterName,
		"-o", "jsonpath={.metadata.labels.crdb\\.io/skip-reconcile}")
	require.Equal(t, "true", skipReconcile, "skip-reconcile label should remain after migration complete")

	valuesFile := exportValuesForHelmAdoption(t, clusterName, ns)

	// Adopt the migrated cluster into the CockroachDB Helm chart with values
	// generated from the live migrated v1beta1 resources.
	adoptIntoCockroachDBHelmChart(t, kubectlOptions, clusterName, clusterName, ns, valuesFile)
	requireOperatorSourceFieldsPreserved(t, kubectlOptions, clusterName)
	testutil.RequireCRDBToFunction(t, crdbCluster, true)

	// Patch storedVersions to remove v1alpha1 now that migration is complete.
	patchStoredVersions(t)

	// Disable migration mode on the operator now that storedVersions is clean.
	disableMigrationMode(t, kubectlOptions, ns)

	testutil.RequireCRDBToFunction(t, crdbCluster, true)
}

// testOperatorAutoStopResume pauses a public-operator migration and then resumes it.
func testOperatorAutoStopResume(t *testing.T) {
	ns := "cockroach" + strings.ToLower(random.UniqueId())
	kubectlOptions := k8s.NewKubectlOptions("", "", ns)

	clusterName, crdbCluster := installPublicOperatorSourceCluster(t, kubectlOptions, ns)
	defer func() {
		_ = os.RemoveAll(manifestsDirPath)
	}()
	defer func() {
		_ = helm.DeleteE(t, &helm.Options{KubectlOptions: kubectlOptions}, releaseName, true)
		waitForClusterResourcesGone(t, kubectlOptions)
		uninstallMigrationOperator(t, kubectlOptions)
		k8s.RunKubectl(t, kubectlOptions, "delete", "priorityclass", "operator-critical", "--ignore-not-found")
		publicOperatorKubectlOptions := k8s.NewKubectlOptions("", "", migration.OperatorNamespace)
		k8s.RunKubectl(t, publicOperatorKubectlOptions, "delete", "deployment", migration.OperatorDeploymentName, "--ignore-not-found=true")
		k8s.DeleteNamespace(t, kubectlOptions, ns)
	}()

	testutil.RequireClusterToBeReadyEventuallyTimeout(t, crdbCluster, 600*time.Second)
	testutil.RequireCRDBToFunction(t, crdbCluster, false)

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

	// Resume.
	k8s.RunKubectl(t, kubectlOptions, "label", v1alpha1CrdbClusterResource, clusterName, "crdb.io/migrate=start", "--overwrite")
	waitForCrdbNodeCount(t, kubectlOptions, clusterName, crdbCluster.DesiredNodes, 20*time.Minute)
	waitForMigrationPhase(t, kubectlOptions, clusterName, phaseFinalization, 5*time.Minute)

	// Per the migration docs, the source StatefulSet must be deleted to move
	// from Finalization to Complete.
	deleteStatefulSetAndWaitGone(t, kubectlOptions, clusterName, 3*time.Minute)
	waitForMigrationPhase(t, kubectlOptions, clusterName, phaseComplete, 3*time.Minute)
	requireMigrationComplete(t, kubectlOptions, clusterName)
	requireOperatorSourceFieldsPreserved(t, kubectlOptions, clusterName)

	skipReconcile, _ := k8s.RunKubectlAndGetOutputE(t, kubectlOptions, "get", v1alpha1CrdbClusterResource, clusterName,
		"-o", "jsonpath={.metadata.labels.crdb\\.io/skip-reconcile}")
	require.Equal(t, "true", skipReconcile, "skip-reconcile label should remain after migration complete")

	valuesFile := exportValuesForHelmAdoption(t, clusterName, ns)
	adoptIntoCockroachDBHelmChart(t, kubectlOptions, clusterName, clusterName, ns, valuesFile)
	requireOperatorSourceFieldsPreserved(t, kubectlOptions, clusterName)
	testutil.RequireCRDBToFunction(t, crdbCluster, true)

	// Patch storedVersions to remove v1alpha1 now that migration is complete.
	patchStoredVersions(t)

	// Disable migration mode on the operator now that storedVersions is clean.
	disableMigrationMode(t, kubectlOptions, ns)

	testutil.RequireCRDBToFunction(t, crdbCluster, true)
}

// testOperatorAutoRollback triggers rollback mid-migration and verifies the public
// operator can resume control cleanly.
func testOperatorAutoRollback(t *testing.T) {
	ns := "cockroach" + strings.ToLower(random.UniqueId())
	kubectlOptions := k8s.NewKubectlOptions("", "", ns)

	clusterName, crdbCluster := installPublicOperatorSourceCluster(t, kubectlOptions, ns)
	defer func() {
		// Rollback does not adopt into Helm, so explicitly delete CRs before
		// waiting. The operator is still running and will process finalizers.
		_ = k8s.RunKubectlE(t, kubectlOptions, "delete", "crdbclusters.crdb.cockroachlabs.com", "--all", "--ignore-not-found=true")
		waitForClusterResourcesGone(t, kubectlOptions)
		uninstallMigrationOperator(t, kubectlOptions)
		k8s.RunKubectl(t, kubectlOptions, "delete", "priorityclass", "operator-critical", "--ignore-not-found")
		publicOperatorKubectlOptions := k8s.NewKubectlOptions("", "", migration.OperatorNamespace)
		k8s.RunKubectl(t, publicOperatorKubectlOptions, "delete", "deployment", migration.OperatorDeploymentName, "--ignore-not-found=true")
		k8s.DeleteNamespace(t, kubectlOptions, ns)
	}()

	testutil.RequireClusterToBeReadyEventuallyTimeout(t, crdbCluster, 600*time.Second)
	testutil.RequireCRDBToFunction(t, crdbCluster, false)

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

	// Assert logging ConfigMap key was reverted to logging.yaml on rollback.
	logConfigData, err := k8s.RunKubectlAndGetOutputE(t, kubectlOptions,
		"get", "configmap", "logging-config", "-o", "jsonpath={.data}")
	require.NoError(t, err)
	require.Contains(t, logConfigData, "logging.yaml",
		"logging ConfigMap key should revert to logging.yaml after rollback")

	// Cluster must be healthy after rollback with public operator in control.
	testutil.RequireClusterToBeReadyEventuallyTimeout(t, crdbCluster, 300*time.Second)
	testutil.RequireCRDBToFunction(t, crdbCluster, true)
}

// testHelmAutoCertManagerMigration installs a Helm cluster with cert-manager TLS,
// runs automatic migration, and verifies cert-manager resources are preserved.
func testHelmAutoCertManagerMigration(t *testing.T) {
	const caSecretName = "cockroach-ca"
	ns := "cockroach" + strings.ToLower(random.UniqueId())
	kubectlOptions := k8s.NewKubectlOptions("", "", ns)
	certManagerK8sOptions := k8s.NewKubectlOptions("", "", testutil.CertManagerNamespace)

	cleanupPublicOperatorWebhookArtifacts(t)
	testutil.InstallCertManager(t, certManagerK8sOptions)
	defer func() {
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
			"operator.enabled":                         "false",
			"conf.cluster-name":                        "test",
			"init.provisioning.enabled":                "true",
			"init.provisioning.databases[0].name":      migration.TestDBName,
			"init.provisioning.databases[0].owners[0]": "root",
			"conf.locality":                            fmt.Sprintf("topology.kubernetes.io/region=%s", cloudRegion),
			"tls.certs.selfSigner.enabled":             "false",
			"tls.certs.certManager":                    "true",
			"tls.certs.certManagerIssuer.name":         testutil.SelfSignedIssuerName,
		}),
	}
	helmChartPath := fmt.Sprintf("%s/cockroachdb", testutil.GetGitRoot())
	helm.Install(t, helmOptions, helmChartPath, releaseName)
	defer func() {
		_ = helm.DeleteE(t, &helm.Options{KubectlOptions: kubectlOptions}, releaseName, true)
		// Cert-manager test does not adopt into Helm, so explicitly delete CRs.
		_ = k8s.RunKubectlE(t, kubectlOptions, "delete", "crdbclusters.crdb.cockroachlabs.com", "--all", "--ignore-not-found=true")
		waitForClusterResourcesGone(t, kubectlOptions)
		uninstallMigrationOperator(t, kubectlOptions)
		k8s.DeleteNamespace(t, kubectlOptions, ns)
	}()

	k8s.WaitUntilServiceAvailable(t, kubectlOptions, fmt.Sprintf("%s-public", stsName), 30, 2*time.Second)

	caConfigMapName := fmt.Sprintf("%s-ca-crt", stsName)
	testutil.InstallTrustManager(t, certManagerK8sOptions, ns)
	testutil.CreateBundle(t, kubectlOptions, caSecretName, caConfigMapName)
	defer testutil.DeleteBundle(t, kubectlOptions)

	testutil.RequireCRDBClusterToBeReadyEventuallyTimeout(t, kubectlOptions, crdbCluster, 600*time.Second)
	testutil.RequireCRDBToFunction(t, crdbCluster, false)

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
	waitForMigrationPhase(t, kubectlOptions, stsName, phaseFinalization, 5*time.Minute)

	// Per the migration docs, the source StatefulSet must be deleted to move
	// from Finalization to Complete.
	deleteStatefulSetAndWaitGone(t, kubectlOptions, stsName, 3*time.Minute)
	waitForMigrationPhase(t, kubectlOptions, stsName, phaseComplete, 3*time.Minute)
	requireMigrationComplete(t, kubectlOptions, stsName)

	// Cert-manager Certificate resources must still exist after migration.
	k8s.RunKubectl(t, kubectlOptions, "get", "certificates.cert-manager.io", fmt.Sprintf("%s-node", stsName))
	k8s.RunKubectl(t, kubectlOptions, "get", "certificates.cert-manager.io", fmt.Sprintf("%s-root-client", stsName))
}

// --- Helpers ---

// installMigrationOperator installs the Cloud Operator with migration enabled and
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
	clusterKubectl := k8s.NewKubectlOptions("", "", "")
	for _, crd := range []string{
		"crdbclusters.crdb.cockroachlabs.com",
		"crdbnodes.crdb.cockroachlabs.com",
		"crdbtenants.crdb.cockroachlabs.com",
	} {
		_ = k8s.RunKubectlE(t, clusterKubectl, "delete", "crd", crd, "--ignore-not-found=true")
	}
}

func cleanupPublicOperatorWebhookArtifacts(t *testing.T) {
	kubectlOptions := k8s.NewKubectlOptions("", "", migration.OperatorNamespace)
	k8s.RunKubectl(t, kubectlOptions, "delete", "deployment", migration.OperatorDeploymentName, "--ignore-not-found=true")
	k8s.RunKubectl(t, kubectlOptions, "delete", "service", "cockroach-operator-webhook-service", "--ignore-not-found=true")
	k8s.RunKubectl(t, kubectlOptions, "delete", "validatingwebhookconfiguration", "cockroach-operator-validating-webhook-configuration", "--ignore-not-found=true")
	k8s.RunKubectl(t, kubectlOptions, "delete", "mutatingwebhookconfiguration", "cockroach-operator-mutating-webhook-configuration", "--ignore-not-found=true")
}

// patchStoredVersions removes v1alpha1 from the CrdbCluster CRD storedVersions,
// leaving only v1beta1. This should be called after all v1alpha1 clusters have
// been migrated and the public operator has been removed.
func patchStoredVersions(t *testing.T) {
	kubectlOptions := k8s.NewKubectlOptions("", "", "")
	k8s.RunKubectl(t, kubectlOptions, "patch", "crd", "crdbclusters.crdb.cockroachlabs.com",
		"--type=merge", "--subresource=status", "-p", `{"status":{"storedVersions":["v1beta1"]}}`)

	// Verify the patch took effect.
	out, err := k8s.RunKubectlAndGetOutputE(t, kubectlOptions,
		"get", "crd", "crdbclusters.crdb.cockroachlabs.com",
		"-o", "jsonpath={.status.storedVersions}")
	require.NoError(t, err)
	require.Equal(t, `["v1beta1"]`, out, "storedVersions should only contain v1beta1")
}

// disableMigrationMode upgrades the operator chart with migration.enabled=false,
// then verifies the operator pods are healthy, v1alpha1 is no longer served, and
// the conversion webhook is removed from the CRD.
func disableMigrationMode(t *testing.T, kubectlOptions *k8s.KubectlOptions, namespace string) {
	_, operatorChartPath := operator.HelmChartPaths()
	opts := &helm.Options{
		KubectlOptions: kubectlOptions,
		SetValues: map[string]string{
			"numReplicas":       "1",
			"migration.enabled": "false",
			"watchNamespaces":   namespace,
		},
		ExtraArgs: map[string][]string{"upgrade": {"--wait"}},
	}
	helm.Upgrade(t, opts, operatorChartPath, autoMigrationOperatorRelease)

	// Verify operator pods are running after the upgrade.
	pods := k8s.ListPods(t, kubectlOptions, metav1.ListOptions{LabelSelector: operator.OperatorLabelSelector})
	require.NotEmpty(t, pods, "operator pods should exist after disabling migration")
	for i := range pods {
		testutil.RequirePodToBeCreatedAndReady(t, kubectlOptions, pods[i].Name, 120*time.Second)
	}

	// Verify v1alpha1 is no longer served.
	clusterKubectl := k8s.NewKubectlOptions("", "", "")
	v1alpha1Served, err := k8s.RunKubectlAndGetOutputE(t, clusterKubectl,
		"get", "crd", "crdbclusters.crdb.cockroachlabs.com",
		"-o", "jsonpath={.spec.versions[?(@.name=='v1alpha1')].served}")
	require.NoError(t, err)
	require.True(t, v1alpha1Served == "" || v1alpha1Served == "false",
		"v1alpha1 should not be served after disabling migration, got: %s", v1alpha1Served)

	// Verify conversion webhook is removed from CRD.
	conversionStrategy, err := k8s.RunKubectlAndGetOutputE(t, clusterKubectl,
		"get", "crd", "crdbclusters.crdb.cockroachlabs.com",
		"-o", "jsonpath={.spec.conversion.strategy}")
	require.NoError(t, err)
	require.True(t, conversionStrategy == "" || conversionStrategy == "None",
		"conversion strategy should be None after disabling migration, got: %s", conversionStrategy)
}

// waitForClusterResourcesGone waits until CrdbClusters and CrdbNodes are fully
// removed from the namespace. After helm adoption, helm delete handles
// CrdbCluster/CrdbNode deletion. For rollback tests where no helm adoption
// occurs, the CrdbCluster must be explicitly deleted before calling this.
func waitForClusterResourcesGone(t *testing.T, kubectlOptions *k8s.KubectlOptions) {
	resources := []string{
		"crdbclusters.crdb.cockroachlabs.com",
		"crdbnodes.crdb.cockroachlabs.com",
	}
	for _, res := range resources {
		retry.DoWithRetry(t, fmt.Sprintf("wait for %s to be gone", res), 60, 5*time.Second, func() (string, error) {
			out, err := k8s.RunKubectlAndGetOutputE(t, kubectlOptions, "get", res, "-o", "name")
			if err != nil {
				// CRD might not exist, that's fine.
				return "gone", nil
			}
			if strings.TrimSpace(out) != "" {
				return "", fmt.Errorf("%s still present: %s", res, out)
			}
			return "gone", nil
		})
	}
}

// installHelmSourceCluster installs a Helm StatefulSet-based CockroachDB cluster with
// WAL failover and a logging config to maximize migration coverage.
// Returns the StatefulSet name and a CockroachCluster descriptor.
func installHelmSourceCluster(
	t *testing.T, kubectlOptions *k8s.KubectlOptions, namespace string,
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
			"conf.locality":                            fmt.Sprintf("topology.kubernetes.io/region=%s", cloudRegion),
			// This source cluster uses dedicated volumes for WAL failover and logs.
			// Migration should preserve both on the v1beta1 target.
			"storage.PersistentVolume.enabled":           "true",
			"conf.wal-failover.value":                    "path=/cockroach/wal-failover",
			"conf.wal-failover.persistentVolume.enabled": "true",
			"conf.wal-failover.persistentVolume.path":    "wal-failover",
			"conf.wal-failover.persistentVolume.size":    "1Gi",
			"conf.log.enabled":                           "true",
			"conf.log.config.file-defaults.dir":          "/cockroach/test-logs",
			"conf.log.persistentVolume.enabled":          "true",
			"conf.log.persistentVolume.path":             "test-logs",
			"conf.log.persistentVolume.size":             "1Gi",
		}),
	}

	h := &migration.HelmInstall{
		Namespace:   namespace,
		HelmOptions: helmOptions,
		CrdbCluster: crdbCluster,
	}
	h.InstallHelm(t)
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
			return migrationPhaseReached(phase, expectedPhase), nil
		},
	)
	if err != nil {
		// Log last error for debugging.
		lastErr, _ := k8s.RunKubectlAndGetOutputE(t, kubectlOptions,
			"get", v1beta1CrdbClusterResource, clusterName,
			"-o", "jsonpath={.status.migration.lastError}")
		t.Logf("Migration lastError: %s", lastErr)
		logMigrationEvents(t, kubectlOptions, clusterName)
	}
	require.NoError(t, err, "timed out waiting for migration phase %q", expectedPhase)
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
// equals expectedCount. Pass 0 to wait for all CrdbNodes to be deleted.
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
			return count == expectedCount, nil
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
// the terminal migration state: spec.mode=MutableOnly and
// status.migration.phase=Complete.
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
	annotateResourcesForHelmAdoption(t, kubectlOptions, clusterName, helmReleaseName, namespace)
	annotateIngressResourcesForHelmAdoption(t, kubectlOptions, helmReleaseName, namespace)
	deleteStaleClusterScopedRBAC(t, kubectlOptions, clusterName, namespace)
	deleteStalePodDisruptionBudgets(t, kubectlOptions, clusterName)

	helmPath, _ := operator.HelmChartPaths()
	helm.Upgrade(t, &helm.Options{
		KubectlOptions: kubectlOptions,
		ValuesFiles:    []string{valuesFile},
		SetValues: map[string]string{
			"migration.enabled": "true",
		},
	}, helmPath, helmReleaseName)

	managedBy, err := k8s.RunKubectlAndGetOutputE(t, kubectlOptions, "get", v1beta1CrdbClusterResource, clusterName,
		"-o", "jsonpath={.metadata.labels.app\\.kubernetes\\.io/managed-by}")
	require.NoError(t, err)
	require.Equal(t, "Helm", managedBy)

	verifyHelmAdoptionSupportsUpgrade(t, kubectlOptions, helmPath, clusterName, helmReleaseName, valuesFile)
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

	initialTimestamp := initialPods[0].CreationTimestamp.Time
	for _, pod := range initialPods[1:] {
		if pod.CreationTimestamp.Time.Before(initialTimestamp) {
			initialTimestamp = pod.CreationTimestamp.Time
		}
	}

	upgradeTime := time.Now().UTC()

	helm.Upgrade(t, &helm.Options{
		KubectlOptions: kubectlOptions,
		ValuesFiles:    []string{valuesFile},
		SetValues: map[string]string{
			"migration.enabled":                 "true",
			"cockroachdb.crdbCluster.timestamp": upgradeTime.Format(time.RFC3339),
		},
	}, helmPath, helmReleaseName)

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
		if len(pods) == 0 {
			return "", fmt.Errorf("no cockroachdb pods found after helm upgrade")
		}

		for _, pod := range pods {
			if !pod.CreationTimestamp.Time.After(initialTimestamp) {
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
	k8s.RunKubectl(t, kubectlOptions, "delete", "clusterrole", clusterName, "--ignore-not-found")
	k8s.RunKubectl(t, kubectlOptions, "delete", "clusterrolebinding", clusterName, "--ignore-not-found")
}

// deleteStalePodDisruptionBudgets removes the old source PDB before Helm adoption.
// Helm StatefulSet sources use the -budget suffix while public operator sources use
// the cluster name directly.
func deleteStalePodDisruptionBudgets(
	t *testing.T, kubectlOptions *k8s.KubectlOptions, clusterName string,
) {
	k8s.RunKubectl(t, kubectlOptions, "delete", "poddisruptionbudget", clusterName, "--ignore-not-found")
	k8s.RunKubectl(t, kubectlOptions, "delete", "poddisruptionbudget", fmt.Sprintf("%s-budget", clusterName), "--ignore-not-found")
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
		"{.spec.template.spec.localityLabels}", "topology.kubernetes.io/region")
	requireJSONPathEquals(t, kubectlOptions, "crdbnode", fmt.Sprintf("%s-0", clusterName),
		"{.spec.logsStore.mountPath}", "/cockroach/test-logs/")
	requireJSONPathEquals(t, kubectlOptions, "crdbnode", fmt.Sprintf("%s-0", clusterName),
		"{.spec.logsStore.volumeClaimTemplate.metadata.name}", "logsdir")
}

func requireOperatorSourceFieldsPreserved(
	t *testing.T, kubectlOptions *k8s.KubectlOptions, clusterName string,
) {
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

func requireJSONPathContains(
	t *testing.T, kubectlOptions *k8s.KubectlOptions, resource, name, jsonPath, expected string,
) {
	t.Helper()
	out, err := k8s.RunKubectlAndGetOutputE(t, kubectlOptions, "get", resource, name, "-o", "jsonpath="+jsonPath)
	require.NoError(t, err)
	require.Contains(t, out, expected)
}
