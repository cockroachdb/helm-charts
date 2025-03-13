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
	"errors"
	"fmt"
	"log"
	"os"

	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/cockroachdb/helm-charts/pkg/generator"
)

var (
	cl  client.Client
	ctx context.Context
)

// rootCmd represents the base command when called without any subcommands
var rootCmd = &cobra.Command{
	Use:   "self-signer",
	Short: "self-signer generates/rotates certs for secure CockroachDB mode",
	Long:  `self-signer is a tool used to generate or rotate CA cert, Node cert and Client cert`,
}

// Execute adds all child commands to the root command and sets flags appropriately.
// This is called by main.main(). It only needs to happen once to the rootCmd.
func Execute() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
}

func init() {
	// all the common flags are attached to root command
	rootCmd.PersistentFlags().StringVar(&caSecret, "ca-secret", "", "name of user provided CA secret")

	rootCmd.PersistentFlags().StringVar(&caDuration, "ca-duration", "43800h", "duration of CA cert. Defaults to 43800h (5 years)")
	rootCmd.PersistentFlags().StringVar(&caExpiry, "ca-expiry", "648h", "expiry window for CA cert. Defaults to 27 days")

	rootCmd.PersistentFlags().StringVar(&nodeDuration, "node-duration", "8760h", "duration of Node cert. Defaults to 365h (1 year)")
	rootCmd.PersistentFlags().StringVar(&nodeExpiry, "node-expiry", "168h", "expiry window for Node cert. Defaults to 7 days")

	rootCmd.PersistentFlags().StringVar(&clientDuration, "client-duration", "672h", "duration of Client cert. Defaults to 28 days")
	rootCmd.PersistentFlags().StringVar(&clientExpiry, "client-expiry", "48h", "expiry window for Client(root) cert. Defaults to 2 days")

	var err error
	ctx = context.Background()
	runtimeScheme := runtime.NewScheme()

	_ = clientgoscheme.AddToScheme(runtimeScheme)
	config := controllerruntime.GetConfigOrDie()

	cl, err = client.New(config, client.Options{
		Scheme: runtimeScheme,
		Mapper: nil,
	})
	if err != nil {
		log.Panic("Failed to create client for certificate generation", err)
	}
}

func getInitialConfig(caDuration, caExpiry, nodeDuration, nodeExpiry, clientDuration,
	clientExpiry string) (generator.GenerateCert, error) {

	genCert := generator.NewGenerateCert(cl)

	if err := genCert.CaCertConfig.SetConfig(caDuration, caExpiry); err != nil {
		return genCert, err
	}

	if err := genCert.NodeCertConfig.SetConfig(nodeDuration, nodeExpiry); err != nil {
		return genCert, err
	}

	if err := genCert.ClientCertConfig.SetConfig(clientDuration, clientExpiry); err != nil {
		return genCert, err
	}

	if !clientOnly {
		// STATEFULSET_NAME is derived from {{ template "cockroachdb.fullname" . }} in helm chart.
		stsName, exists := os.LookupEnv("STATEFULSET_NAME")
		if !exists {
			return genCert, errors.New("Required STATEFULSET_NAME env not found")
		}
		genCert.PublicServiceName = stsName + "-public"
		genCert.DiscoveryServiceName = stsName

		domain, exists := os.LookupEnv("CLUSTER_DOMAIN")
		if !exists {
			return genCert, errors.New("Required CLUSTER_DOMAIN env not found")
		}
		genCert.ClusterDomain = domain
	}

	return genCert, nil
}
