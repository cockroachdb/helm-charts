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
	"path/filepath"
	"strings"
	"text/template"

	"github.com/Masterminds/semver/v3"
	"gopkg.in/yaml.v3"
)

const usage = `Usage:
// Bump cockroachdb version and chart version in Chart.yaml file.
- go run build/build.go bump <crdbversion>
// Bump cockroachdb chart version in Chart.yaml file.
- go run build/build.go bump helm
// Generate files based on templates in build/templates directory.
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

		if err := generate(os.Args[2]); err != nil {
			fmt.Fprintf(os.Stderr, "cannot run: %s", err)
			os.Exit(1)
		}
		return
	case "generate":
		if len(os.Args) < 2 {
			fmt.Print(usage)
			os.Exit(1)
		}

		if err := generate(""); err != nil {
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
// If version is specified, it will be used to bump the version in Chart.yaml file.
func generate(version string) error {
	const templatesDir = "build/templates"
	const outputDir = "."
	var args templateArgs

	dirInfo, err := os.Stat(templatesDir)
	if err != nil {
		return fmt.Errorf("cannot stat templates directory: %w", err)
	}
	if !dirInfo.IsDir() {
		return fmt.Errorf("%s is not a directory", templatesDir)
	}

	return filepath.Walk(templatesDir, func(filePath string, fileInfo os.FileInfo, e error) error {
		if e != nil {
			return e
		}
		// Skip directories
		if !fileInfo.Mode().IsRegular() {
			return nil
		}
		// calculate file directory relative to the given root directory.
		dir := filepath.Dir(filePath)
		fileName := filepath.Base(filePath)
		relDir, err := filepath.Rel(templatesDir, dir)
		if err != nil {
			return err
		}
		destDir := filepath.Join(outputDir, relDir)
		destFile := filepath.Join(destDir, fileInfo.Name())
		if fileName == "Chart.yaml" {
			args, err = buildTemplateArgs(destFile, version)
			if err != nil {
				return err
			}
		}
		doNotEditStatement := fmt.Sprintf("# Generated file, DO NOT EDIT. Source: %s\n", filePath)
		if fileName == "README.md" {
			doNotEditStatement = fmt.Sprintf("<!--- Generated file, DO NOT EDIT. Source: %s --->\n", filePath)
		}
		if err := processTemplate(
			filePath,
			destFile,
			args,
			doNotEditStatement,
		); err != nil {
			return fmt.Errorf("cannot process %s -> %s: %w", filePath, destFile, err)
		}

		return nil
	})
}

// buildTemplateArgs reads the Chart.yaml file and returns the template arguments.
func buildTemplateArgs(destFile, version string) (templateArgs, error) {
	chart, err := getVersions(destFile)
	if err != nil {
		return templateArgs{}, fmt.Errorf("cannot get chart versions: %w", err)
	}
	if version == "" {
		return templateArgs{
			Version:    chart.Version.String(),
			AppVersion: chart.AppVersion.String(),
		}, nil
	}

	crdbVersion := semver.MustParse(chart.AppVersion.String())
	if version != "helm" {
		crdbVersion, err = semver.NewVersion(strings.TrimPrefix(version, "v"))
		if err != nil {
			return templateArgs{}, fmt.Errorf("cannot parse version %s: %w", version, err)
		}
	}

	newChartVersion, err := bumpVersion(chart, crdbVersion)
	if err != nil {
		return templateArgs{}, fmt.Errorf("cannot bump chart version: %w", err)
	}
	return templateArgs{
		Version:    newChartVersion,
		AppVersion: crdbVersion.Original(),
	}, nil
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

	// If the chart version is the same as the new CRDB version, we increment the BUILD number.
	// Increment BUILD number for the new chart version where AppVersion and Version are the same.
	if chart.AppVersion.String() == newCRDBVersion.String() && chart.Version.Major() == chart.AppVersion.Major() {
		build := chart.Version.Metadata()
		newBuild := "1"
		if build != "" {
			parts := strings.Split(build, ".")
			if len(parts) == 1 {
				if n, err := fmt.Sscanf(parts[0], "%s", &newBuild); n == 1 && err == nil {
					var val int
					fmt.Sscanf(parts[0], "%d", &val)
					newBuild = fmt.Sprintf("%d", val+1)
				}
			}
		}

		baseVersionStr := fmt.Sprintf("%d.%d.%d", chart.Version.Major(), chart.Version.Minor(), chart.Version.Patch())
		if chart.Version.Prerelease() != "" {
			baseVersionStr = fmt.Sprintf("%s-%s", baseVersionStr, chart.Version.Prerelease())
		}

		// Construct the new version string
		return fmt.Sprintf("%s+%s", baseVersionStr, newBuild), nil
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
