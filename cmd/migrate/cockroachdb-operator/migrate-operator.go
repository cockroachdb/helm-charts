package cockroachdb_operator

import (
	"fmt"
	"github.com/cockroachdb/helm-charts/pkg/migrate"
	"github.com/spf13/cobra"
)

var (
	crdbClusterName string
	namespace       string
)

var buildManifestFromOperator = &cobra.Command{
	Use:   "operator",
	Short: "Generate migration manifests for the CockroachDB Operator from the Public Operator (https://github.com/cockroachdb/cockroach-operator)",
	Long: `Generate the required Kubernetes manifests to assist in migrating from the Public Operator 
to the CockroachDB Operator.

This command is designed to simplify the migration process by generating manifests that are compatible 
with the CockroachDB Operator, including resources such as CRDBNode and values.yaml

It is intended for users who initially deployed CockroachDB via the Public Operator (https://github.com/cockroachdb/cockroach-operator) 
and now wish to take advantage of the additional capabilities and lifecycle management features offered 
by the CockroachDB Operator.

Always review the generated manifests thoroughly and test in a staging environment before applying changes 
to a production cluster.
`,
	RunE: buildManifestFromCockroachDBOperator,
}

func init() {
	buildManifestFromOperator.PersistentFlags().StringVar(&crdbClusterName, "crdb-cluster", "", "name of crdbcluster resource")
	buildManifestFromOperator.PersistentFlags().StringVar(&namespace, "namespace", "default", "namespace of crdbcluster resource")
	_ = buildManifestFromOperator.MarkPersistentFlagRequired("crdb-cluster")
	buildManifestCmd.AddCommand(buildManifestFromOperator)
}

func buildManifestFromCockroachDBOperator(cmd *cobra.Command, args []string) error {
	migration, err := migrate.NewManifest(cloudProvider, cloudRegion, kubeconfig, crdbClusterName, namespace, outputDir)
	if err != nil {
		return err
	}

	if err := migration.FromPublicOperator(); err != nil {
		return err
	}

	fmt.Println("✅ Migration manifests successfully generated.")
	fmt.Printf("📁 Output directory: %s\n", outputDir)
	fmt.Println("📌 Next steps:")
	fmt.Printf("   1. Review the generated YAML files under the '%s' directory.\n", outputDir)
	fmt.Println("   2. Follow the README.md under scripts/migration/operator directory")
	fmt.Println("   3. Monitor the cluster to ensure a smooth transition.")
	fmt.Printf("\n⚠️ WARNING:\n")
	fmt.Println("   Always review the generated manifests thoroughly and test in staging environment")
	fmt.Println("   before applying it to the production cluster.")
	fmt.Println("   Do not generate the manifests once you scaled down statefulset.")

	return nil
}
