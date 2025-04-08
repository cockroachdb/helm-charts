package cockroachdb_enterprise_operator

import (
	"github.com/cockroachdb/helm-charts/pkg/migrate"
	"github.com/spf13/cobra"
)

var (
	crdbCluster     string
	clusterManifest string
)

var buildManifestFromHelm = &cobra.Command{
	Use:   "helm",
	Short: "Generate migration manifests for CockroachDB Enterprise Operator from Helm chart",
	Long:  "Generate manifests for migrating from the public CockroachDB Helm chart to the CockroachDB Enterprise Operator.",
	RunE:  buildManifestFromCockroachDBHelmChart,
}

func init() {
	buildManifestFromHelm.PersistentFlags().StringVar(&crdbCluster, "statefulset", "", "name of statefulset resource")
	buildManifestFromHelm.PersistentFlags().StringVar(&namespace, "namespace", "default", "namespace of crdbcluster resource")
	buildManifestFromHelm.PersistentFlags().StringVar(&clusterManifest, "statefulset-manifest", "", "path to statefulset manifest backup")
	buildManifestCmd.AddCommand(buildManifestFromHelm)
}

func buildManifestFromCockroachDBHelmChart(cmd *cobra.Command, args []string) error {
	var options []migrate.Option

	if clusterManifest != "" {
		options = append(options, migrate.WithObjectManifest(clusterManifest))
	}
	if crdbCluster != "" {
		options = append(options, migrate.WithObject(crdbCluster))
	}

	migration, err := migrate.NewMigration(cloudProvider, cloudRegion, kubeconfig, namespace, outputDir, options...)
	if err != nil {
		return err
	}

	if err := migration.FromHelmChart(); err != nil {
		return err
	}

	return nil
}
