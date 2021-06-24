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
	"github.com/spf13/cobra"
	"log"
	"os"
	"time"
)

// rotateCmd represents the rotate command
var rotateCmd = &cobra.Command{
	Use:   "rotate",
	Short: "rotates a CA, Node or Client certificate",
	Long:  `rotate sub-command rotates the CA cert, Node cert and Client certs`,
	Run:   rotate,
}

var (
	clientFlag, caFlag, nodeFlag bool
	caCron, nodeAndClientCron    string
	readinessWait                string
)

func init() {
	rootCmd.AddCommand(rotateCmd)

	rotateCmd.Flags().BoolVar(&clientFlag, "client", false, "if set rotates client certificate")
	rotateCmd.Flags().BoolVar(&nodeFlag, "node", false, "if set rotates node certificate")
	rotateCmd.Flags().BoolVar(&caFlag, "ca", false, "if set rotates ca certificate")

	rotateCmd.Flags().StringVar(&caCron, "ca-cron", "", "cron of the CA certificate rotation cron")
	rotateCmd.Flags().StringVar(&nodeAndClientCron, "node-client-cron", "", "cron of the node and client certificate rotation cron")

	rotateCmd.Flags().StringVar(&readinessWait, "readiness-wait", "30s", "readiness wait for each replica of crdb cluster")

}

func rotate(cmd *cobra.Command, args []string) {
	if clientFlag && nodeFlag && caFlag {
		log.Panic("CA, Node and client can't be rotated at the same time. Only CA or (Node and Client) can be " +
			"rotated at a time")
	}

	if !clientFlag && !nodeFlag && !caFlag {
		log.Panic("None of the CA, Node and client is provided for cert rotation")
	}

	genCert, err := getInitialConfig(caDuration, caExpiry, nodeDuration, nodeExpiry, clientDuration, clientExpiry)
	if err != nil {
		panic(err)
	}

	namespace, exists := os.LookupEnv("NAMESPACE")
	if !exists {
		log.Panic("Required NAMESPACE env not found")
	}

	timeout, err := time.ParseDuration(readinessWait)
	if err != nil {
		log.Panicf("failed to parse readiness-wait duration %s", err.Error())
	}
	genCert.ReadinessWait = timeout

	genCert.CaSecret = caSecret
	genCert.RotateCACert = caFlag
	genCert.CACronSchedule = caCron

	genCert.RotateClientCert = clientFlag
	genCert.RotateNodeCert = nodeFlag
	genCert.NodeAndClientCronSchedule = nodeAndClientCron

	if err := genCert.Do(ctx, namespace); err != nil {
		log.Panic(err)
	}

}
