package cockroachdb_operator

import (
	"fmt"
	"path/filepath"

	"github.com/cockroachdb/helm-charts/pkg/migrate"
	"github.com/spf13/cobra"
	"k8s.io/client-go/util/homedir"
)

var (
	exportKubeconfig string
	exportOutputDir  string
)

var exportValuesCmd = &cobra.Command{
	Use:   "export-values",
	Short: "Generate Helm values from an existing migrated v1beta1 CrdbCluster",
	Long: `Generate a values.yaml for the CockroachDB Helm chart from an existing
migrated v1beta1 CrdbCluster.

This is meant for the post-migration adoption flow where the source Helm
StatefulSet or public operator v1alpha1 object is no longer the source of truth
and users want Helm values from the live migrated cluster.`,
	RunE: exportValuesFromV1beta1,
}

func init() {
	exportValuesCmd.PersistentFlags().StringVar(&crdbClusterName, "crdb-cluster", "", "name of migrated v1beta1 crdbcluster resource")
	exportValuesCmd.PersistentFlags().StringVar(&namespace, "namespace", "default", "namespace of migrated v1beta1 crdbcluster resource")
	exportValuesCmd.PersistentFlags().StringVar(&exportKubeconfig, "kubeconfig", filepath.Join(homedir.HomeDir(), ".kube", "config"), "path to kubeconfig file")
	exportValuesCmd.PersistentFlags().StringVar(&exportOutputDir, "output-dir", "./manifests", "output directory for generated values.yaml")
	_ = exportValuesCmd.MarkPersistentFlagRequired("crdb-cluster")
	rootCmd.AddCommand(exportValuesCmd)
}

func exportValuesFromV1beta1(cmd *cobra.Command, args []string) error {
	exporter, err := migrate.NewValuesExporter(exportKubeconfig, crdbClusterName, namespace, exportOutputDir)
	if err != nil {
		return err
	}

	if err := exporter.ExportValuesFromV1beta1(); err != nil {
		return err
	}

	fmt.Println("✅ values.yaml successfully generated from migrated v1beta1 resources.")
	fmt.Printf("📁 Output directory: %s\n", exportOutputDir)
	fmt.Println("📌 Next steps:")
	fmt.Printf("   1. Review '%s/values.yaml'.\n", exportOutputDir)
	fmt.Println("   2. Annotate existing resources for Helm ownership.")
	fmt.Println("   3. Run helm upgrade with this values.yaml.")
	return nil
}
