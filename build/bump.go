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

package main

import (
	"bytes"
	"fmt"
	"io/ioutil"
	"os"
	"strings"
	"text/template"

	"github.com/Masterminds/semver/v3"
	"gopkg.in/yaml.v3"
)

const (
	chartsFile         = "cockroachdb/Chart.yaml"
	valuesFile         = "cockroachdb/values.yaml"
	readmeFile         = "cockroachdb/README.md"
	chartsFileTemplate = "build/templates/Chart.yaml"
	valuesFileTemplate = "build/templates/values.yaml"
	readmeFileTemplate = "build/templates/README.md"
)

type templateArgs struct {
	Version    string
	AppVersion string
}

func main() {
	if len(os.Args) != 2 {
		fmt.Println("Usage: go run build/bump.go crdbversion")
		os.Exit(1)
	}
	if err := run(os.Args[1]); err != nil {
		fmt.Fprintf(os.Stderr, "cannot run: %s", err)
		os.Exit(1)
	}

}

func run(crdbVersion string) error {
	// Trim the "v" prefix if exists. It will be added explicitly in the templates when needed.
	crdbVersion = strings.TrimPrefix(crdbVersion, "v")
	chartVersion, err := getChartVersion(chartsFile)
	if err != nil {
		return fmt.Errorf("cannot get chart version: %w", err)
	}
	// Bump the charts version to be nice to helm.
	newChartVersion, err := bumpVersion(chartVersion)
	if err != nil {
		return fmt.Errorf("cannot bump chart version: %w", err)
	}
	args := templateArgs{
		Version:    newChartVersion,
		AppVersion: crdbVersion,
	}
	if err := processTemplate(
		chartsFileTemplate,
		chartsFile,
		args,
		fmt.Sprintf("# Generated file, DO NOT EDIT. Source: %s\n", chartsFileTemplate),
	); err != nil {
		return fmt.Errorf("cannot process %s -> %s: %w", chartsFileTemplate, chartsFile, err)
	}
	if err := processTemplate(
		valuesFileTemplate,
		valuesFile,
		args,
		fmt.Sprintf("# Generated file, DO NOT EDIT. Source: %s\n", valuesFileTemplate),
	); err != nil {
		return fmt.Errorf("cannot process %s -> %s: %w", valuesFileTemplate, valuesFile, err)
	}
	if err := processTemplate(
		readmeFileTemplate,
		readmeFile,
		args,
		fmt.Sprintf("<!--- Generated file, DO NOT EDIT. Source: %s --->\n", readmeFileTemplate),
	); err != nil {
		return fmt.Errorf("cannot process %s -> %s: %w", readmeFileTemplate, readmeFile, err)
	}
	return nil
}

// processTemplate reads a template file, applies the template arguments and writes it to the specified location
func processTemplate(
	templateFile string, outputFile string, args templateArgs, header string,
) error {
	t, err := template.ParseFiles(templateFile)
	if err != nil {
		return fmt.Errorf("failed to parse template %s: %w", templateFile, err)
	}
	var buf bytes.Buffer
	err = t.Execute(&buf, args)
	if err != nil {
		return fmt.Errorf("cannot execute template: %w", err)
	}
	fileInfo, err := os.Stat(templateFile)
	if err != nil {
		return fmt.Errorf("cannot stat %s: %w", templateFile, err)
	}
	if err := os.WriteFile(outputFile, []byte(header+buf.String()), fileInfo.Mode()); err != nil {
		return fmt.Errorf("cannot write file: %w", err)
	}
	return nil
}

// bumpVersion increases the patch release version (the last digit) of a given version
func bumpVersion(version string) (string, error) {
	semanticVersion, err := semver.NewVersion(version)
	if err != nil {
		return "", fmt.Errorf("cannot parse version: %w", err)
	}
	nextVersion := semanticVersion.IncPatch()
	return nextVersion.Original(), nil
}

// getChartVersion reads chart version from Chart.yaml file
func getChartVersion(chartPath string) (string, error) {
	chartContents, err := ioutil.ReadFile(chartPath)
	if err != nil {
		return "", fmt.Errorf("cannot open chart file %s: %w", chartPath, err)
	}
	// Minimal definition, only to extract the version
	chart := struct {
		Version string
	}{}
	if err := yaml.Unmarshal(chartContents, &chart); err != nil {
		return "", fmt.Errorf("cannot unmarshal %s: %w", chartPath, err)
	}
	return chart.Version, nil
}
