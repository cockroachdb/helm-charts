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
	chartsFileTemplate = "build/templates/Chart.yaml"
	valuesFileTemplate = "build/templates/values.yaml"
	readmeFileTemplate = "build/templates/README.md"
)

const usage = `Usage:
- go run build/build.go bump <crdbversion>
- go run build/build.go generate
`

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

	switch os.Args[1] {
	case "bump":
		if len(os.Args) < 3 {
			fmt.Print(usage)
			os.Exit(1)
		}
		if err := bump(os.Args[2]); err != nil {
			fmt.Fprintf(os.Stderr, "cannot run: %s", err)
			os.Exit(1)
		}
		return
	case "generate":
		if len(os.Args) < 2 {
			fmt.Print(usage)
			os.Exit(1)
		}
		if err := generate(); err != nil {
			fmt.Fprintf(os.Stderr, "cannot run: %s", err)
			os.Exit(1)
		}
		return
	}

	fmt.Print(usage)
	os.Exit(1)
}

// regenerate destination files based on templates, which should
// result in a zero diff, if template is up-to-date with destination files.
func generate() error {
	chart, err := getVersions(chartsFile)
	if err != nil {
		return fmt.Errorf("cannot get chart versions: %w", err)
	}
	return processTemplates(chart.Version.String(), chart.AppVersion.String())
}

func bump(version string) error {
	// Trim the "v" prefix if exists. It will be added explicitly in the templates when needed.
	crdbVersion, err := semver.NewVersion(strings.TrimPrefix(version, "v"))
	if err != nil {
		return fmt.Errorf("cannot parse version %s: %w", version, err)
	}
	chart, err := getVersions(chartsFile)
	if err != nil {
		return fmt.Errorf("cannot get chart versions: %w", err)
	}
	// Bump the chart version to be nice to helm.
	newChartVersion, err := bumpVersion(chart, crdbVersion)
	if err != nil {
		return fmt.Errorf("cannot bump chart version: %w", err)
	}
	return processTemplates(newChartVersion, crdbVersion.Original())
}

func processTemplates(version string, appVersion string) error {
	args := templateArgs{
		Version:    version,
		AppVersion: appVersion,
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
