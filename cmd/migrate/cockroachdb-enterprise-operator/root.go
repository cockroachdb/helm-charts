package cockroachdb_enterprise_operator

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
	"k8s.io/client-go/util/homedir"
)

// rootCmd represents the base command when called without any subcommands
var rootCmd = &cobra.Command{
	Use:   "migration-helper",
	Short: "CLI to help user migrate to CockroachDb Enterprise Operator",
	Long: `migration-helper is used to help users on cockroachdb-operator or public helm chart to migrate their
existing cockroachdb clusters to CockroachDB Enterprise Operator.`,
}

var buildManifestCmd = &cobra.Command{
	Use:   "build-manifest",
	Short: "Generate migration manifests for the Cockroachdb Enterprise Operator",
	Long:  "It generates the required manifest to migrate to the CockroachDB Enterprise Operator.",
}

var (
	cloudProvider string
	cloudRegion   string
	kubeconfig    string
	outputDir     string
)

// Execute adds all child commands to the root command and sets flags appropriately.
// This is called by main.main(). It only needs to happen once to the rootCmd.
func Execute() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
}

func init() {
	buildManifestCmd.PersistentFlags().StringVar(&cloudProvider, "cloud-provider", "", "name of cloud provider")
	buildManifestCmd.PersistentFlags().StringVar(&cloudRegion, "cloud-region", "", "name of cloud provider region")
	buildManifestCmd.PersistentFlags().StringVar(&kubeconfig, "kubeconfig", filepath.Join(homedir.HomeDir(), ".kube", "config"), "path to kubeconfig file")
	buildManifestCmd.PersistentFlags().StringVar(&outputDir, "output-dir", "./manifests", "manifest output directory")
	_ = buildManifestCmd.MarkPersistentFlagRequired("cloud-provider")
	_ = buildManifestCmd.MarkPersistentFlagRequired("cloud-region")
	rootCmd.AddCommand(buildManifestCmd)
}
