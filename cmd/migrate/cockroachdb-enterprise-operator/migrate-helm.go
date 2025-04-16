package cockroachdb_enterprise_operator

import (
	"fmt"
	"github.com/cockroachdb/helm-charts/pkg/migrate"
	"github.com/spf13/cobra"
)

var (
	statefulSetName string
)

// buildManifestFromHelm only supports migrating the CockroachDB StatefulSet deployed via the official Helm chart.
var buildManifestFromHelm = &cobra.Command{
	Use:   "helm",
	Short: "Generate migration manifests for Cockroach Enterprise Operator from Official CockroachDB Helm chart(https://artifacthub.io/packages/helm/cockroachdb/cockroachdb)",
	Long: `Generate the necessary Kubernetes manifests to migrate from a CockroachDB deployment created using 
the official CockroachDB Helm chart to one managed by the Cockroach Enterprise Operator.

This command is designed to simplify the migration process by generating manifests that are compatible 
with the Cockroach Enterprise Operator, including resources such as CRDBNode and values.yaml

It is intended for users who initially deployed CockroachDB via the Helm chart provided by Cockroach Labs 
and now wish to take advantage of the additional capabilities and lifecycle management features offered 
by the Cockroach Enterprise Operator.

Before running this command, ensure that your Helm-based deployment closely follows the official Helm chart 
structure.

Always review the generated manifests thoroughly and test in a staging environment before applying changes 
to a production cluster.
`,
	RunE: buildManifestFromCockroachDBHelmChart,
}

func init() {
	buildManifestFromHelm.PersistentFlags().StringVar(&statefulSetName, "statefulset-name", "", "name of cockroachdb statefulset resource")
	buildManifestFromHelm.PersistentFlags().StringVar(&namespace, "namespace", "default", "namespace of cockroachdb statefulset resource")
	_ = buildManifestFromHelm.MarkPersistentFlagRequired("statefulset-name")
	buildManifestCmd.AddCommand(buildManifestFromHelm)
}

func buildManifestFromCockroachDBHelmChart(cmd *cobra.Command, args []string) error {
	migration, err := migrate.NewManifest(cloudProvider, cloudRegion, kubeconfig, statefulSetName, namespace, outputDir)
	if err != nil {
		return err
	}

	if err := migration.FromHelmChart(); err != nil {
		return err
	}

	fmt.Println("✅ Migration manifests successfully generated.")
	fmt.Printf("📁 Output directory: %s\n", outputDir)
	fmt.Println("📌 Next steps:")
	fmt.Printf("   1. Review the generated YAML files under the '%s' directory.\n", outputDir)
	fmt.Println("   2. Follow the README.md under scripts/migration/helm directory")
	fmt.Println("   3. Monitor the cluster to ensure a smooth transition.")
	fmt.Printf("\n⚠️ WARNING:\n")
	fmt.Println("   Always review the generated manifests thoroughly and test in staging environment")
	fmt.Println("   before applying it to the production cluster.")
	fmt.Println("   Do not generate the manifests, once you scaled down statefulset.")
	return nil
}
