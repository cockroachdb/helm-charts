package main

import (
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

func TestBumpVersion_MajorOrMinorChange(t *testing.T) {
	testcases := []struct {
		name         string
		chartVersion string
		appVersion   string
		newCRDB      string
		gotVersion   string
	}{
		{
			name:         "bump major version",
			chartVersion: "25.1.6",
			appVersion:   "25.1.6",
			newCRDB:      "26.0.0",
			gotVersion:   "26.0.0",
		},
		{
			name:         "bump minor version",
			chartVersion: "25.1.6",
			appVersion:   "25.1.6",
			newCRDB:      "25.2.0",
			gotVersion:   "25.2.0",
		},
		{
			name:         "bump major version with preview",
			chartVersion: "25.1.6-preview",
			appVersion:   "25.1.6",
			newCRDB:      "26.0.0",
			gotVersion:   "26.0.0-preview",
		},
		{
			name:         "bump minor version with preview",
			chartVersion: "25.1.6-preview",
			appVersion:   "25.1.6",
			newCRDB:      "25.2.0",
			gotVersion:   "25.2.0-preview",
		},
		{
			name:         "bump major version for old chart version",
			chartVersion: "16.1.0",
			appVersion:   "25.1.6",
			newCRDB:      "26.0.0",
			gotVersion:   "17.0.0",
		},
		{
			name:         "bump minor version for old chart version",
			chartVersion: "16.1.0",
			appVersion:   "25.1.6",
			newCRDB:      "25.2.0",
			gotVersion:   "17.0.0",
		},
	}

	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			chart := versions{
				Version:    mustVer(tc.chartVersion),
				AppVersion: mustVer(tc.appVersion),
			}
			newCRDB := semver.MustParse(tc.newCRDB)
			got, err := bumpVersion(chart, newCRDB)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.gotVersion {
				t.Errorf("expected %s, got %s", tc.gotVersion, got)
			}
		})
	}
}

func TestBumpVersion_BuildMetadata(t *testing.T) {
	testCases := []struct {
		name         string
		chartVersion string
		appVersion   string
		gotVersion   string
	}{
		{
			name:         "bump build metadata first time",
			chartVersion: "25.1.6",
			appVersion:   "25.1.6",
			gotVersion:   "25.1.6+1",
		},
		{
			name:         "bump build metadata first time with preview",
			chartVersion: "25.1.6-preview",
			appVersion:   "25.1.6",
			gotVersion:   "25.1.6-preview+1",
		},
		{
			name:         "bump build metadata with existing metadata",
			chartVersion: "25.1.6+1",
			appVersion:   "25.1.6",
			gotVersion:   "25.1.6+2",
		},
		{
			name:         "bump build metadata with existing metadata and preview",
			chartVersion: "25.1.6-preview+1",
			appVersion:   "25.1.6",
			gotVersion:   "25.1.6-preview+2",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			chart := versions{
				Version:    mustVer(tc.chartVersion),
				AppVersion: mustVer(tc.appVersion),
			}
			newCRDB := semver.MustParse(tc.appVersion)
			got, err := bumpVersion(chart, newCRDB)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.gotVersion {
				t.Errorf("expected %s, got %s", tc.gotVersion, got)
			}
		})
	}
}

func TestBumpVersion_DefaultPatch(t *testing.T) {
	testCases := []struct {
		name         string
		chartVersion string
		appVersion   string
		newCRDB      string
		gotVersion   string
	}{
		{
			name:         "bump patch version for operator based chart",
			chartVersion: "25.1.6",
			appVersion:   "25.1.6",
			newCRDB:      "25.1.7",
			gotVersion:   "25.1.7",
		},
		{
			name:         "bump patch version for old chart version",
			chartVersion: "16.1.0",
			appVersion:   "25.1.6",
			newCRDB:      "25.1.7",
			gotVersion:   "16.1.1",
		},
		{
			name:         "bump patch version with preview",
			chartVersion: "25.1.6-preview",
			appVersion:   "25.1.6",
			newCRDB:      "25.1.7",
			gotVersion:   "25.1.7-preview",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			chart := versions{
				Version:    mustVer(tc.chartVersion),
				AppVersion: mustVer(tc.appVersion),
			}
			newCRDB := semver.MustParse(tc.newCRDB)
			got, err := bumpVersion(chart, newCRDB)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.gotVersion {
				t.Errorf("expected %s, got %s", tc.gotVersion, got)
			}
		})
	}
}
