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
	chartsFileTemplate = "build/templates/cockroachdb/Chart.yaml"
	valuesFileTemplate = "build/templates/cockroachdb/values.yaml"
	readmeFileTemplate = "build/templates/cockroachdb/README.md"

	cockroachDBChartFile          = "cockroachdb-parent/charts/cockroachdb/Chart.yaml"
	cockroachDBValuesFile         = "cockroachdb-parent/charts/cockroachdb/values.yaml"
	cockroachDBReadmeFile         = "cockroachdb-parent/charts/cockroachdb/README.md"
	cockroachDBChartsFileTemplate = "build/templates/cockroachdb-parent/charts/cockroachdb/Chart.yaml"
	cockroachDBValuesFileTemplate = "build/templates/cockroachdb-parent/charts/cockroachdb/values.yaml"
	cockroachDBReadmeFileTemplate = "build/templates/cockroachdb-parent/charts/cockroachdb/README.md"

	operatorChartFile          = "cockroachdb-parent/charts/operator/Chart.yaml"
	operatorValuesFile         = "cockroachdb-parent/charts/operator/values.yaml"
	operatorReadmeFile         = "cockroachdb-parent/charts/operator/README.md"
	operatorChartsFileTemplate = "build/templates/cockroachdb-parent/charts/operator/Chart.yaml"
	operatorValuesFileTemplate = "build/templates/cockroachdb-parent/charts/operator/values.yaml"
	operatorReadmeFileTemplate = "build/templates/cockroachdb-parent/charts/operator/README.md"

	parentChartFile          = "cockroachdb-parent/Chart.yaml"
	parentValuesFile         = "cockroachdb-parent/values.yaml"
	parentReadmeFile         = "cockroachdb-parent/README.md"
	parentChartFileTemplate  = "build/templates/cockroachdb-parent/Chart.yaml"
	parentValuesFileTemplate = "build/templates/cockroachdb-parent/values.yaml"
	parentReadmeFileTemplate = "build/templates/cockroachdb-parent/README.md"
)

const usage = `Usage:
- go run build/build.go bump <crdbversion>
- go run build/build.go generate
`

type HelmTemplate struct {
	chartsFile, valuesFile, readmeFile                         string
	chartsFileTemplate, valuesFileTemplate, readmeFileTemplate string
}

type parsedVersion struct {
	*semver.Version
}
type versions struct {
	Version    parsedVersion `yaml:"version"`
	AppVersion parsedVersion `yaml:"appVersion"`
}

type templateArgs struct {
	Version    string
	AppVersion string
}

// UnmarshalYAML implements the Unmarshaller interface for the version fields
func (v *parsedVersion) UnmarshalYAML(value *yaml.Node) error {
	version, err := semver.NewVersion(value.Value)
	if err != nil {
		return fmt.Errorf("cannot parse version %s: %w", value.Value, err)
	}
	v.Version = version
	return err
}

func main() {
	if len(os.Args) < 2 {
		fmt.Print(usage)
		os.Exit(1)
	}

	helmTemplates := make([]HelmTemplate, 0)
	helmTemplates = append(helmTemplates, HelmTemplate{
		chartsFile:         chartsFile,
		valuesFile:         valuesFile,
		readmeFile:         readmeFile,
		chartsFileTemplate: chartsFileTemplate,
		valuesFileTemplate: valuesFileTemplate,
		readmeFileTemplate: readmeFileTemplate,
	}, HelmTemplate{
		chartsFile:         operatorChartFile,
		valuesFile:         operatorValuesFile,
		readmeFile:         operatorReadmeFile,
		chartsFileTemplate: operatorChartsFileTemplate,
		valuesFileTemplate: operatorValuesFileTemplate,
		readmeFileTemplate: operatorReadmeFileTemplate,
	}, HelmTemplate{
		chartsFile:         cockroachDBChartFile,
		valuesFile:         cockroachDBValuesFile,
		readmeFile:         cockroachDBReadmeFile,
		chartsFileTemplate: cockroachDBChartsFileTemplate,
		valuesFileTemplate: cockroachDBValuesFileTemplate,
		readmeFileTemplate: cockroachDBReadmeFileTemplate,
	}, HelmTemplate{
		chartsFile:         parentChartFile,
		valuesFile:         parentValuesFile,
		readmeFile:         parentReadmeFile,
		chartsFileTemplate: parentChartFileTemplate,
		valuesFileTemplate: parentValuesFileTemplate,
		readmeFileTemplate: parentReadmeFileTemplate,
	})

	switch os.Args[1] {
	case "bump":
		if len(os.Args) < 3 {
			fmt.Print(usage)
			os.Exit(1)
		}

		for i := range helmTemplates {
			h := helmTemplates[i]
			if err := h.bump(os.Args[2]); err != nil {
				fmt.Fprintf(os.Stderr, "cannot run: %s", err)
				os.Exit(1)
			}
		}
		return
	case "generate":
		if len(os.Args) < 2 {
			fmt.Print(usage)
			os.Exit(1)
		}
		for i := range helmTemplates {
			h := helmTemplates[i]
			if err := h.generate(); err != nil {
				fmt.Fprintf(os.Stderr, "cannot run: %s", err)
				os.Exit(1)
			}
		}
		return
	}

	fmt.Print(usage)
	os.Exit(1)
}

// regenerate destination files based on templates, which should
// result in a zero diff, if template is up-to-date with destination files.
func (h *HelmTemplate) generate() error {
	chart, err := getVersions(h.chartsFile)
	if err != nil {
		return fmt.Errorf("cannot get chart versions: %w", err)
	}
	return h.processTemplates(chart.Version.String(), chart.AppVersion.String())
}

func (h *HelmTemplate) bump(version string) error {
	// Trim the "v" prefix if exists. It will be added explicitly in the templates when needed.
	crdbVersion, err := semver.NewVersion(strings.TrimPrefix(version, "v"))
	if err != nil {
		return fmt.Errorf("cannot parse version %s: %w", version, err)
	}
	chart, err := getVersions(h.chartsFile)
	if err != nil {
		return fmt.Errorf("cannot get chart versions: %w", err)
	}
	// Bump the chart version to be nice to helm.
	newChartVersion, err := bumpVersion(chart, crdbVersion)
	if err != nil {
		return fmt.Errorf("cannot bump chart version: %w", err)
	}
	return h.processTemplates(newChartVersion, crdbVersion.Original())
}

func (h *HelmTemplate) processTemplates(version string, appVersion string) error {
	args := templateArgs{
		Version:    version,
		AppVersion: appVersion,
	}
	if err := processTemplate(
		h.chartsFileTemplate,
		h.chartsFile,
		args,
		fmt.Sprintf("# Generated file, DO NOT EDIT. Source: %s\n", h.chartsFileTemplate),
	); err != nil {
		return fmt.Errorf("cannot process %s -> %s: %w", h.chartsFileTemplate, h.chartsFile, err)
	}
	if err := processTemplate(
		h.valuesFileTemplate,
		h.valuesFile,
		args,
		fmt.Sprintf("# Generated file, DO NOT EDIT. Source: %s\n", h.valuesFileTemplate),
	); err != nil {
		return fmt.Errorf("cannot process %s -> %s: %w", h.valuesFileTemplate, h.valuesFile, err)
	}
	if err := processTemplate(
		h.readmeFileTemplate,
		h.readmeFile,
		args,
		fmt.Sprintf("<!--- Generated file, DO NOT EDIT. Source: %s --->\n", h.readmeFileTemplate),
	); err != nil {
		return fmt.Errorf("cannot process %s -> %s: %w", h.readmeFileTemplate, h.readmeFile, err)
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
func bumpVersion(chart versions, newCRDBVersion *semver.Version) (string, error) {
	// Bump chart major version in case appVersion changes its major or minor version
	// For example, 22.1.0 or 22.2.0 should trigger this behaviour.
	if chart.AppVersion.Major() != newCRDBVersion.Major() ||
		chart.AppVersion.Minor() != newCRDBVersion.Minor() {
		nextMajor := chart.Version.IncMajor()
		nextVersion, err := semver.NewVersion(fmt.Sprintf("%d.0.0", nextMajor.Major()))
		if err != nil {
			return "", fmt.Errorf("cannot parse next version: %w", err)
		}
		return nextVersion.Original(), nil
	}
	// If the version includes a prerelease label like `-preview`, the IncrPatch function automatically removes it.
	// For example, v25.0.0-preview becomes v25.0.0 after incrementing the patch.
	// To retain the prerelease label, we now increment the patch twice and re-append the prerelease.
	// This ensures that versions with a prerelease set maintain the label even after a patch increment.
	if chart.Version.Prerelease() != "" {
		nextVersion, err := chart.Version.IncPatch().IncPatch().SetPrerelease(chart.Version.Prerelease())
		return nextVersion.Original(), err
	}
	nextVersion := chart.Version.IncPatch()
	return nextVersion.Original(), nil
}

// getVersions reads chart and app versions from Chart.yaml file
func getVersions(chartPath string) (versions, error) {
	chartContents, err := os.ReadFile(chartPath)
	if err != nil {
		return versions{}, fmt.Errorf("cannot open chart file %s: %w", chartPath, err)
	}
	var chart versions
	if err := yaml.Unmarshal(chartContents, &chart); err != nil {
		return versions{}, fmt.Errorf("cannot unmarshal %s: %w", chartPath, err)
	}
	return chart, nil
}
