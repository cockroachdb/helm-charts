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
)

// generateCmd represents the generate command
var generateCmd = &cobra.Command{
	Use:   "generate",
	Short: "generates a CA, Node or Client certificate",
	Long:  `generate sub-command generates CA cert if not not given, Node certs and root client cert`,
	Run:   generate,
}

var (
	caDuration, nodeDuration, clientDuration string
	caExpiry, nodeExpiry, clientExpiry       string
	caSecret                                 string
	clientOnly                               bool
)

func init() {
	generateCmd.Flags().BoolVar(&clientOnly, "client-only", false, "generate certificates for custom user")
	rootCmd.AddCommand(generateCmd)
}

func generate(cmd *cobra.Command, args []string) {

	genCert, err := getInitialConfig(caDuration, caExpiry, nodeDuration, nodeExpiry, clientDuration, clientExpiry)
	if err != nil {
		panic(err)
	}

	genCert.CaSecret = caSecret

	namespace, exists := os.LookupEnv("NAMESPACE")
	if !exists {
		log.Panic("Required NAMESPACE env not found")
	}

	if clientOnly {
		if err := genCert.ClientCertGenerate(ctx, namespace); err != nil {
			log.Panic(err)
		}
	} else {
		if err := genCert.Do(ctx, namespace); err != nil {
			log.Panic(err)
		}
	}
}
