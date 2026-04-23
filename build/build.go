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
- go run build/build.go bump <crdbversion>                      Bump CRDB version (cockroachdb + legacy charts)
- go run build/build.go bump --chart cockroachdb <crdbversion>  Bump cockroachdb chart for new CRDB version
- go run build/build.go bump --chart operator <version>         Bump operator chart version
- go run build/build.go bump helm                               Chart-only fix (patch bump, no appVersion change)
- go run build/build.go generate                                Regenerate files from templates
`

type chartKind int

const (
	chartKindLegacy      chartKind = iota // cockroachdb/
	chartKindCockroachDB                  // cockroachdb-parent/charts/cockroachdb/
	chartKindOperator                     // cockroachdb-parent/charts/operator/
	chartKindParent                       // cockroachdb-parent/
)

type parsedVersion struct {
	*semver.Version
}
type versions struct {
	Version    parsedVersion `yaml:"version"`
	AppVersion parsedVersion `yaml:"appVersion"`
}

type templateArgs struct {
	Version            string
	AppVersion         string
	OperatorVersion    string
	CockroachDBVersion string
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

		chartTarget := ""
		version := ""

		if os.Args[2] == "--chart" {
			if len(os.Args) < 5 {
				fmt.Print(usage)
				os.Exit(1)
			}
			chartTarget = os.Args[3]
			version = os.Args[4]
		} else {
			version = os.Args[2]
		}

		if err := validateChartTarget(chartTarget); err != nil {
			fmt.Fprintf(os.Stderr, "%s\n", err)
			os.Exit(1)
		}

		if err := generate(chartTarget, version); err != nil {
			fmt.Fprintf(os.Stderr, "cannot run: %s", err)
			os.Exit(1)
		}
	case "generate":
		if err := generate("", ""); err != nil {
			fmt.Fprintf(os.Stderr, "cannot run: %s", err)
			os.Exit(1)
		}
	default:
		fmt.Print(usage)
		os.Exit(1)
	}
}

var chartPaths = map[chartKind]string{
	chartKindLegacy:      "cockroachdb/Chart.yaml",
	chartKindCockroachDB: "cockroachdb-parent/charts/cockroachdb/Chart.yaml",
	chartKindOperator:    "cockroachdb-parent/charts/operator/Chart.yaml",
	chartKindParent:      "cockroachdb-parent/Chart.yaml",
}

func validateChartTarget(target string) error {
	switch target {
	case "", "cockroachdb", "operator":
		return nil
	default:
		return fmt.Errorf("unknown chart target %q, must be 'cockroachdb' or 'operator'", target)
	}
}

func validateNoDowngrade(current, proposed *semver.Version, component string) error {
	if proposed.LessThan(current) {
		return fmt.Errorf("cannot downgrade %s from %s to %s", component, current, proposed)
	}
	return nil
}

func chartKindFromPath(relPath string) chartKind {
	switch {
	case strings.HasPrefix(relPath, "cockroachdb-parent/charts/operator"):
		return chartKindOperator
	case strings.HasPrefix(relPath, "cockroachdb-parent/charts/cockroachdb"):
		return chartKindCockroachDB
	case strings.HasPrefix(relPath, "cockroachdb-parent"):
		return chartKindParent
	default:
		return chartKindLegacy
	}
}

func generate(chartTarget, version string) error {
	const templatesDir = "build/templates"
	const outputDir = "."

	dirInfo, err := os.Stat(templatesDir)
	if err != nil {
		return fmt.Errorf("cannot stat templates directory: %w", err)
	}
	if !dirInfo.IsDir() {
		return fmt.Errorf("%s is not a directory", templatesDir)
	}

	allArgs, err := computeAllArgs(chartTarget, version)
	if err != nil {
		return fmt.Errorf("cannot compute chart versions: %w", err)
	}

	return filepath.Walk(templatesDir, func(filePath string, fileInfo os.FileInfo, e error) error {
		if e != nil {
			return e
		}
		if !fileInfo.Mode().IsRegular() {
			return nil
		}

		dir := filepath.Dir(filePath)
		fileName := filepath.Base(filePath)
		relDir, err := filepath.Rel(templatesDir, dir)
		if err != nil {
			return err
		}
		destDir := filepath.Join(outputDir, relDir)
		destFile := filepath.Join(destDir, fileInfo.Name())

		kind := chartKindFromPath(relDir)
		args := allArgs[kind]

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

func computeAllArgs(chartTarget, version string) (map[chartKind]templateArgs, error) {
	result := make(map[chartKind]templateArgs)

	legacyChart, err := getVersions(chartPaths[chartKindLegacy])
	if err != nil {
		return nil, fmt.Errorf("cannot read legacy chart: %w", err)
	}
	cockroachdbChart, err := getVersions(chartPaths[chartKindCockroachDB])
	if err != nil {
		return nil, fmt.Errorf("cannot read cockroachdb chart: %w", err)
	}
	operatorChart, err := getVersions(chartPaths[chartKindOperator])
	if err != nil {
		return nil, fmt.Errorf("cannot read operator chart: %w", err)
	}

	if version == "" {
		result[chartKindLegacy] = templateArgs{
			Version:    legacyChart.Version.String(),
			AppVersion: legacyChart.AppVersion.String(),
		}
		result[chartKindCockroachDB] = templateArgs{
			Version:    cockroachdbChart.Version.String(),
			AppVersion: cockroachdbChart.AppVersion.String(),
		}
		result[chartKindOperator] = templateArgs{
			Version:    operatorChart.Version.String(),
			AppVersion: operatorChart.AppVersion.String(),
		}
	} else if chartTarget == "operator" {
		newVersion := strings.TrimPrefix(version, "v")
		newVer, err := semver.NewVersion(newVersion)
		if err != nil {
			return nil, fmt.Errorf("cannot parse operator version %s: %w", version, err)
		}
		if err := validateNoDowngrade(operatorChart.Version.Version, newVer, "operator chart"); err != nil {
			return nil, err
		}
		result[chartKindOperator] = templateArgs{
			Version:    newVersion,
			AppVersion: newVersion,
		}
		result[chartKindCockroachDB] = templateArgs{
			Version:    cockroachdbChart.Version.String(),
			AppVersion: cockroachdbChart.AppVersion.String(),
		}
		result[chartKindLegacy] = templateArgs{
			Version:    legacyChart.Version.String(),
			AppVersion: legacyChart.AppVersion.String(),
		}
	} else {
		crdbVersion := cockroachdbChart.AppVersion.Version
		if version != "helm" {
			crdbVersion, err = semver.NewVersion(strings.TrimPrefix(version, "v"))
			if err != nil {
				return nil, fmt.Errorf("cannot parse CRDB version %s: %w", version, err)
			}
			if err := validateNoDowngrade(cockroachdbChart.AppVersion.Version, crdbVersion, "CRDB version"); err != nil {
				return nil, err
			}
		}

		newCockroachDBVersion, err := bumpCockroachDBChart(cockroachdbChart, crdbVersion)
		if err != nil {
			return nil, fmt.Errorf("cannot bump cockroachdb chart: %w", err)
		}
		result[chartKindCockroachDB] = templateArgs{
			Version:    newCockroachDBVersion,
			AppVersion: crdbVersion.Original(),
		}

		newLegacyVersion, err := bumpLegacyChart(legacyChart, crdbVersion)
		if err != nil {
			return nil, fmt.Errorf("cannot bump legacy chart: %w", err)
		}
		result[chartKindLegacy] = templateArgs{
			Version:    newLegacyVersion,
			AppVersion: crdbVersion.Original(),
		}

		result[chartKindOperator] = templateArgs{
			Version:    operatorChart.Version.String(),
			AppVersion: operatorChart.AppVersion.String(),
		}
	}

	result[chartKindParent] = templateArgs{
		Version:            result[chartKindCockroachDB].AppVersion,
		AppVersion:         result[chartKindCockroachDB].AppVersion,
		OperatorVersion:    result[chartKindOperator].Version,
		CockroachDBVersion: result[chartKindCockroachDB].Version,
	}

	return result, nil
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

// bumpCockroachDBChart implements locked major.minor versioning:
// chart major.minor follows CRDB major.minor, chart patch increments independently.
func bumpCockroachDBChart(chart versions, newCRDBVersion *semver.Version) (string, error) {
	if chart.AppVersion.Major() != newCRDBVersion.Major() || chart.AppVersion.Minor() != newCRDBVersion.Minor() {
		return fmt.Sprintf("%d.%d.0", newCRDBVersion.Major(), newCRDBVersion.Minor()), nil
	}
	nextPatch := chart.Version.IncPatch()
	return nextPatch.Original(), nil
}

// bumpLegacyChart preserves the old statefulset chart's versioning behavior:
// major bump on CRDB major.minor change, patch bump otherwise.
func bumpLegacyChart(chart versions, newCRDBVersion *semver.Version) (string, error) {
	if chart.AppVersion.Major() != newCRDBVersion.Major() || chart.AppVersion.Minor() != newCRDBVersion.Minor() {
		nextMajor := chart.Version.IncMajor()
		return fmt.Sprintf("%d.0.0", nextMajor.Major()), nil
	}
	nextPatch := chart.Version.IncPatch()
	return nextPatch.Original(), nil
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
