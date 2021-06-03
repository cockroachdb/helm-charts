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

package cert_generator

import (
	"fmt"

	"github.com/spf13/cobra"
)

// rotateCmd represents the rotate command
var rotateCmd = &cobra.Command{
	Use:   "rotate",
	Short: "rotates a CA, Node or Client certificate",
	Long: `rotate sub-command rotates the CA cert, Node cert and Client certs`,
	Run: rotate,
}

func init() {
	rootCmd.AddCommand(rotateCmd)
}

func rotate(cmd *cobra.Command, args []string) {
	fmt.Println("rotate called")
}