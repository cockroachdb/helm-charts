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
	Use:   "build-manifest",
	Short: "Generate migration manifests",
	Long:  `Generate manifests for migrating to the cloud operator.`,
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
	rootCmd.PersistentFlags().StringVar(&cloudProvider, "cloud-provider", "", "name of cloud provider")
	rootCmd.PersistentFlags().StringVar(&cloudRegion, "cloud-region", "", "name of cloud provider region")
	rootCmd.PersistentFlags().StringVar(&kubeconfig, "kubeconfig", filepath.Join(homedir.HomeDir(), ".kube", "config"), "path to kubeconfig file")
	rootCmd.PersistentFlags().StringVar(&outputDir, "output-dir", "./manifests", "manifest output directory")
	_ = rootCmd.MarkPersistentFlagRequired("cloud-provider")
	_ = rootCmd.MarkPersistentFlagRequired("cloud-region")
}
