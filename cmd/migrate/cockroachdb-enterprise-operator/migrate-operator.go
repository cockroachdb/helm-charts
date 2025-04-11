package cockroachdb_enterprise_operator

import (
	"github.com/cockroachdb/helm-charts/pkg/migrate"
	"github.com/spf13/cobra"
)

var (
	statefulset string
	namespace   string
	stsManifest string
)

var buildManifestFromOperator = &cobra.Command{
	Use:   "operator",
	Short: "Generate migration manifests from cockroachdb-enterprise-operator from operator",
	Long:  "Generate manifests for migrating from the public cockroachdb operator to the cloud operator.",
	RunE:  buildManifestFromCockroachDBOperator,
}

func init() {
	buildManifestFromOperator.PersistentFlags().StringVar(&statefulset, "crdb-cluster", "", "name of crdbcluster resource")
	buildManifestFromOperator.PersistentFlags().StringVar(&namespace, "namespace", "default", "namespace of crdbcluster resource")
	buildManifestFromOperator.PersistentFlags().StringVar(&stsManifest, "cluster-manifest", "", "path to public manifest backup")
	_ = buildManifestFromOperator.MarkPersistentFlagRequired("crdb-cluster")
	buildManifestCmd.AddCommand(buildManifestFromOperator)
}

func buildManifestFromCockroachDBOperator(cmd *cobra.Command, args []string) error {
	var options []migrate.Option

	if stsManifest != "" {
		options = append(options, migrate.WithObjectManifest(stsManifest))
	}
	if statefulset != "" {
		options = append(options, migrate.WithObject(statefulset))
	}

	migration, err := migrate.NewMigration(cloudProvider, cloudRegion, kubeconfig, namespace, outputDir, options...)
	if err != nil {
		return err
	}

	if err := migration.FromPublicOperator(); err != nil {
		return err
	}

	return nil
}
