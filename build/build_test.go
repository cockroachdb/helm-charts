package main

import (
	"os"
	"strings"
	"testing"

	"github.com/Masterminds/semver/v3"
)

func mustVer(s string) parsedVersion {
	v, err := semver.NewVersion(s)
	if err != nil {
		panic(err)
	}
	return parsedVersion{v}
}

func TestBumpCockroachDBChart(t *testing.T) {
	testCases := []struct {
		name         string
		chartVersion string
		appVersion   string
		newCRDB      string
		wantVersion  string
	}{
		{
			name:         "CRDB patch bump increments chart patch",
			chartVersion: "26.1.0",
			appVersion:   "26.1.3",
			newCRDB:      "26.1.4",
			wantVersion:  "26.1.1",
		},
		{
			name:         "second CRDB patch bump",
			chartVersion: "26.1.1",
			appVersion:   "26.1.4",
			newCRDB:      "26.1.5",
			wantVersion:  "26.1.2",
		},
		{
			name:         "chart-only fix (same CRDB version)",
			chartVersion: "26.1.0",
			appVersion:   "26.1.3",
			newCRDB:      "26.1.3",
			wantVersion:  "26.1.1",
		},
		{
			name:         "CRDB minor bump starts new chart line",
			chartVersion: "26.1.2",
			appVersion:   "26.1.5",
			newCRDB:      "26.2.0",
			wantVersion:  "26.2.0",
		},
		{
			name:         "CRDB major bump starts new chart line",
			chartVersion: "26.1.2",
			appVersion:   "26.1.5",
			newCRDB:      "27.1.0",
			wantVersion:  "27.1.0",
		},
		{
			name:         "first release of new series",
			chartVersion: "25.2.3",
			appVersion:   "25.2.7",
			newCRDB:      "26.1.0",
			wantVersion:  "26.1.0",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			chart := versions{
				Version:    mustVer(tc.chartVersion),
				AppVersion: mustVer(tc.appVersion),
			}
			newCRDB := semver.MustParse(tc.newCRDB)
			got, err := bumpCockroachDBChart(chart, newCRDB)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.wantVersion {
				t.Errorf("got %s, want %s", got, tc.wantVersion)
			}
		})
	}
}

func TestBumpLegacyChart(t *testing.T) {
	testCases := []struct {
		name         string
		chartVersion string
		appVersion   string
		newCRDB      string
		wantVersion  string
	}{
		{
			name:         "CRDB patch bump increments chart patch",
			chartVersion: "20.0.4",
			appVersion:   "26.1.3",
			newCRDB:      "26.1.4",
			wantVersion:  "20.0.5",
		},
		{
			name:         "CRDB minor bump increments chart major",
			chartVersion: "20.0.4",
			appVersion:   "26.1.3",
			newCRDB:      "26.2.0",
			wantVersion:  "21.0.0",
		},
		{
			name:         "CRDB major bump increments chart major",
			chartVersion: "20.0.4",
			appVersion:   "26.1.3",
			newCRDB:      "27.1.0",
			wantVersion:  "21.0.0",
		},
		{
			name:         "chart-only fix (same CRDB version)",
			chartVersion: "20.0.4",
			appVersion:   "26.1.3",
			newCRDB:      "26.1.3",
			wantVersion:  "20.0.5",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			chart := versions{
				Version:    mustVer(tc.chartVersion),
				AppVersion: mustVer(tc.appVersion),
			}
			newCRDB := semver.MustParse(tc.newCRDB)
			got, err := bumpLegacyChart(chart, newCRDB)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.wantVersion {
				t.Errorf("got %s, want %s", got, tc.wantVersion)
			}
		})
	}
}

func TestValidateChartTarget(t *testing.T) {
	testCases := []struct {
		target  string
		wantErr bool
	}{
		{"", false},
		{"cockroachdb", false},
		{"operator", false},
		{"foobar", true},
		{"legacy", true},
		{"parent", true},
	}

	for _, tc := range testCases {
		t.Run(tc.target, func(t *testing.T) {
			err := validateChartTarget(tc.target)
			if (err != nil) != tc.wantErr {
				t.Errorf("validateChartTarget(%q) error = %v, wantErr %v", tc.target, err, tc.wantErr)
			}
		})
	}
}

func TestValidateNoDowngrade(t *testing.T) {
	testCases := []struct {
		name     string
		current  string
		proposed string
		wantErr  bool
	}{
		{"upgrade allowed", "1.0.0", "1.1.0", false},
		{"same version allowed", "1.0.0", "1.0.0", false},
		{"patch upgrade allowed", "26.1.3", "26.1.4", false},
		{"major upgrade allowed", "1.0.0", "2.0.0", false},
		{"downgrade rejected", "1.1.0", "1.0.0", true},
		{"patch downgrade rejected", "26.1.3", "26.1.2", true},
		{"major downgrade rejected", "2.0.0", "1.9.9", true},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			current := semver.MustParse(tc.current)
			proposed := semver.MustParse(tc.proposed)
			err := validateNoDowngrade(current, proposed, "test")
			if (err != nil) != tc.wantErr {
				t.Errorf("validateNoDowngrade(%s, %s) error = %v, wantErr %v", tc.current, tc.proposed, err, tc.wantErr)
			}
		})
	}
}

func TestComputeAllArgs(t *testing.T) {
	if err := os.Chdir(".."); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir("build") })

	legacyChart, err := getVersions(chartPaths[chartKindLegacy])
	if err != nil {
		t.Fatal(err)
	}
	cockroachdbChart, err := getVersions(chartPaths[chartKindCockroachDB])
	if err != nil {
		t.Fatal(err)
	}
	nextCRDBPatch := cockroachdbChart.AppVersion.IncPatch()
	nextCRDBPatchVersion := &nextCRDBPatch
	nextCRDBVersion := nextCRDBPatch.Original()

	testCases := []struct {
		name        string
		chartTarget string
		version     string
		wantErr     string
		check       func(t *testing.T, result map[chartKind]templateArgs)
	}{
		{
			name:        "operator helm shortcut rejected",
			chartTarget: "operator",
			version:     "helm",
			wantErr:     "cannot parse operator version",
		},
		{
			name:        "cockroachdb bump does not change legacy",
			chartTarget: "cockroachdb",
			version:     nextCRDBVersion,
			check: func(t *testing.T, result map[chartKind]templateArgs) {
				legacy := result[chartKindLegacy]
				if legacy.Version != legacyChart.Version.String() {
					t.Errorf("legacy version changed to %s, want %s", legacy.Version, legacyChart.Version)
				}
				if legacy.AppVersion != legacyChart.AppVersion.String() {
					t.Errorf("legacy appVersion changed to %s, want %s", legacy.AppVersion, legacyChart.AppVersion)
				}
				crdb := result[chartKindCockroachDB]
				if crdb.AppVersion != nextCRDBVersion {
					t.Errorf("cockroachdb appVersion = %s, want %s", crdb.AppVersion, nextCRDBVersion)
				}
			},
		},
		{
			name:        "unscoped bump changes both cockroachdb and legacy",
			chartTarget: "",
			version:     nextCRDBVersion,
			check: func(t *testing.T, result map[chartKind]templateArgs) {
				wantLegacyVersion, err := bumpLegacyChart(legacyChart, nextCRDBPatchVersion)
				if err != nil {
					t.Fatal(err)
				}
				legacy := result[chartKindLegacy]
				if legacy.Version != wantLegacyVersion {
					t.Errorf("legacy version = %s, want %s", legacy.Version, wantLegacyVersion)
				}
				if legacy.AppVersion != nextCRDBVersion {
					t.Errorf("legacy appVersion = %s, want %s", legacy.AppVersion, nextCRDBVersion)
				}
				crdb := result[chartKindCockroachDB]
				if crdb.AppVersion != nextCRDBVersion {
					t.Errorf("cockroachdb appVersion = %s, want %s", crdb.AppVersion, nextCRDBVersion)
				}
			},
		},
		{
			name:        "operator explicit version sets both version and appVersion",
			chartTarget: "operator",
			version:     "1.0.0-rc.2",
			check: func(t *testing.T, result map[chartKind]templateArgs) {
				op := result[chartKindOperator]
				if op.Version != "1.0.0-rc.2" {
					t.Errorf("operator version = %s, want 1.0.0-rc.2", op.Version)
				}
				if op.AppVersion != "1.0.0-rc.2" {
					t.Errorf("operator appVersion = %s, want 1.0.0-rc.2", op.AppVersion)
				}
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			result, err := computeAllArgs(tc.chartTarget, tc.version)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("expected error containing %q, got nil", tc.wantErr)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error %q does not contain %q", err.Error(), tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			tc.check(t, result)
		})
	}
}

func TestChartKindFromPath(t *testing.T) {
	testCases := []struct {
		path string
		want chartKind
	}{
		{"cockroachdb", chartKindLegacy},
		{"cockroachdb/Chart.yaml", chartKindLegacy},
		{"cockroachdb-parent", chartKindParent},
		{"cockroachdb-parent/Chart.yaml", chartKindParent},
		{"cockroachdb-parent/charts/cockroachdb", chartKindCockroachDB},
		{"cockroachdb-parent/charts/cockroachdb/Chart.yaml", chartKindCockroachDB},
		{"cockroachdb-parent/charts/operator", chartKindOperator},
		{"cockroachdb-parent/charts/operator/Chart.yaml", chartKindOperator},
	}

	for _, tc := range testCases {
		t.Run(tc.path, func(t *testing.T) {
			got := chartKindFromPath(tc.path)
			if got != tc.want {
				t.Errorf("chartKindFromPath(%q) = %s, want %s", tc.path, got, tc.want)
			}
		})
	}
}
