package infra

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/api/compute/v1"
	"gopkg.in/yaml.v3"
)

func TestWaitForComputeOperationErrors(t *testing.T) {
	errAPI := errors.New("permission denied")
	for _, tc := range []struct {
		name      string
		op        *compute.Operation
		err       error
		wantError bool
	}{
		{"success", &compute.Operation{Status: "DONE"}, nil, false},
		{"API failure", nil, errAPI, true},
		{"operation failure", &compute.Operation{Status: "DONE", Error: &compute.OperationError{Errors: []*compute.OperationErrorErrors{{Code: "RESOURCE_IN_USE"}}}}, nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := waitForComputeOperation(context.Background(), func(context.Context) (*compute.Operation, error) { return tc.op, tc.err })
			if tc.wantError {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	require.ErrorIs(t, waitForComputeOperation(ctx, func(context.Context) (*compute.Operation, error) {
		return &compute.Operation{Status: "RUNNING"}, nil
	}), context.Canceled)
}

func TestOpenShiftPreservationOnlyOutsideCI(t *testing.T) {
	t.Setenv(envOpenShiftSkipTeardown, "true")
	t.Setenv(githubActions, "true")
	require.False(t, shouldPreserveOpenShift())
	t.Setenv(githubActions, "")
	require.True(t, shouldPreserveOpenShift())
	t.Setenv(envOpenShiftSkipTeardown, "false")
	require.False(t, shouldPreserveOpenShift())
}

func TestCIOwnershipAndOpenShiftMetadataPublication(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(githubActions, "true")
	t.Setenv("GITHUB_RUN_ID", "12345")
	t.Setenv("GITHUB_REPOSITORY", "cockroachdb/helm-charts")
	t.Setenv("GCP_PROJECT_ID", "test-project")
	t.Setenv("OPENSHIFT_CLEANUP_GCS_URI", "gs://test-project-openshift-ci-debug/cleanup")
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	log := filepath.Join(dir, "commands")
	t.Setenv("CLEANUP_TEST_COMMANDS", log)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "gcloud"), []byte(`#!/bin/sh
echo "$*" >> "$CLEANUP_TEST_COMMANDS"
if [ "$FAIL_METADATA_UPLOAD" = true ]; then
  exit 1
fi
`), 0755))
	metadata, err := gcpCleanupMetadata(ProviderOpenShift)
	require.NoError(t, err)
	require.Equal(t, "12345", metadata["run_id"])
	var config strings.Builder
	require.NoError(t, openShiftInstallConfigTemplate.Execute(&config, openShiftInstallConfigData{CIMetadata: metadata}))
	var parsed struct {
		Platform struct {
			GCP struct {
				UserLabels []struct{ Key, Value string } `yaml:"userLabels"`
			}
		}
	}
	require.NoError(t, yaml.Unmarshal([]byte(config.String()), &parsed))
	require.Len(t, parsed.Platform.GCP.UserLabels, 3)
	require.Equal(t, "helm-charts-e2e", parsed.Platform.GCP.UserLabels[0].Value)
	installDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(installDir, "metadata.json"), []byte(`{"infraID":"ocp-test-abc"}`), 0600))
	t.Setenv("FAIL_METADATA_UPLOAD", "true")
	require.Error(t, publishOpenShiftCleanupState(installDir))
	commands, err := os.ReadFile(log)
	require.NoError(t, err)
	require.Contains(t, string(commands), "/ocp-test-abc/metadata.json")
	require.NotContains(t, string(commands), "/ocp-test-abc/ci-cleanup.json")
	t.Setenv("FAIL_METADATA_UPLOAD", "false")
	require.NoError(t, os.WriteFile(log, nil, 0600))
	require.NoError(t, publishOpenShiftCleanupState(installDir))
	commands, err = os.ReadFile(log)
	require.NoError(t, err)
	lines := strings.Split(strings.TrimSpace(string(commands)), "\n")
	require.Len(t, lines, 2)
	require.Contains(t, lines[0], "/ocp-test-abc/metadata.json")
	require.Contains(t, lines[1], "/ocp-test-abc/ci-cleanup.json")
	t.Setenv("GITHUB_RUN_ID", "")
	_, err = gcpCleanupMetadata(ProviderGCP)
	require.Error(t, err)
}
