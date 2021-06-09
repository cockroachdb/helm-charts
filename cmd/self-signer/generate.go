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
	"context"
	"log"
	"os"

	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	_ "k8s.io/client-go/plugin/pkg/client/auth"
	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/cockroachdb/helm-charts/pkg/generator"
)

// generateCmd represents the generate command
var generateCmd = &cobra.Command{
	Use:   "generate",
	Short: "generates a CA, Node or Client certificate",
	Long:  `generate sub-command generates CA cert if not not given, Node certs and root client cert`,
	Run:   generate,
}

var (
	caDurationFlag, nodeDurationFlag, clientDurationFlag string
	caExpiryFlag, nodeExpiryFlag, clientExpiryFlag       string
	caSecretFlag                                         string
)

func init() {

	rootCmd.AddCommand(generateCmd)

	generateCmd.Flags().StringVar(&caDurationFlag, "ca-duration", "43800h", "duration of CA cert. Defaults to 43800h (5 years)")
	generateCmd.Flags().StringVar(&caExpiryFlag, "ca-expiry", "648h", "expiry window for CA cert. Defaults to 27 days")

	generateCmd.Flags().StringVar(&nodeDurationFlag, "node-duration", "8760h", "duration of Node cert. Defaults to 365h (1 year)")
	generateCmd.Flags().StringVar(&nodeExpiryFlag, "node-expiry", "168h", "expiry window for Node cert. Defaults to 7 days")

	generateCmd.Flags().StringVar(&clientDurationFlag, "client-duration", "672h", "duration of Client cert. Defaults to 28 days")
	generateCmd.Flags().StringVar(&clientExpiryFlag, "client-expiry", "48h", "expiry window for Client(root) cert. Defaults to 2 days")

	generateCmd.Flags().StringVar(&caSecretFlag, "ca-secret", "", "name of user provided CA secret")
}

func generate(cmd *cobra.Command, args []string) {
	runtimeScheme := runtime.NewScheme()
	ctx := context.Background()
	_ = clientgoscheme.AddToScheme(runtimeScheme)
	config := controllerruntime.GetConfigOrDie()
	cl, err := client.New(config, client.Options{
		Scheme: runtimeScheme,
		Mapper: nil,
	})
	if err != nil {
		log.Panic("Failed to create client for certificate generation", err)
	}

	genCert := generator.NewGenerateCert(cl)

	if err := genCert.CaCertConfig.SetConfig(caDurationFlag, caExpiryFlag); err != nil {
		log.Panic(err)
	}

	if err := genCert.NodeCertConfig.SetConfig(nodeDurationFlag, nodeExpiryFlag); err != nil {
		log.Panic(err)
	}

	if err := genCert.ClientCertConfig.SetConfig(clientDurationFlag, clientExpiryFlag); err != nil {
		log.Panic(err)
	}

	genCert.CaSecret = caSecretFlag

	stsName, exists := os.LookupEnv("STATEFULSET_NAME")
	if !exists {
		log.Panic("Required STATEFULSET_NAME env not found")
	}

	namespace, exists := os.LookupEnv("NAMESPACE")
	if !exists {
		log.Panic("Required NAMESPACE env not found")
	}

	domain, exists := os.LookupEnv("CLUSTER_DOMAIN")
	if !exists {
		log.Panic("Required CLUSTER_DOMAIN env not found")
	}

	genCert.PublicServiceName = stsName
	genCert.DiscoveryServiceName = stsName + "-public"
	genCert.ClusterDomain = domain

	if err := genCert.Do(ctx, namespace); err != nil {
		log.Panic(err)
	}
}
