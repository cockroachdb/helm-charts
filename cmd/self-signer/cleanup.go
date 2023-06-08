/*
Copyright 2021 The Cockroach Authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package self_signer

import (
	"log"
	"os"

	"github.com/spf13/cobra"
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	"github.com/cockroachdb/helm-charts/pkg/resource"
)

// cleanupCmd represents the cleanup command
var cleanupCmd = &cobra.Command{
	Use:   "cleanup",
	Short: "cleanup cleans up the secrets generated using self-signer utility",
	Long:  `cleanup sub-command cleans up the secrets i.e. node, client and CA secrets generated using self-signer utility`,
	Run:   cleanup,
}

var namespace string

func init() {
	cleanupCmd.Flags().StringVar(&namespace, "namespace", "", "namespace of the resources to be cleaned up")
	if err := cleanupCmd.MarkFlagRequired("namespace"); err != nil {
		log.Fatal(err)
	}
	rootCmd.AddCommand(cleanupCmd)
}

func cleanup(cmd *cobra.Command, args []string) {

	stsName, exists := os.LookupEnv("STATEFULSET_NAME")
	if !exists {
		log.Fatal("Required STATEFULSET_NAME env not found")
	}

	resource.Clean(ctx, cl, namespace, stsName)
}
