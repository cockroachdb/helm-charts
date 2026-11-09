package testutil

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/gruntwork-io/terratest/modules/k8s"
	"github.com/stretchr/testify/require"
)

func TestCollectFailureArtifacts(t *testing.T) {
	dir := t.TempDir()
	commands := filepath.Join(dir, "commands.txt")
	t.Setenv("ARTIFACT_TEST_COMMANDS", commands)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "kubectl"), []byte(`#!/bin/sh
printf '%s\n' "$*" >> "$ARTIFACT_TEST_COMMANDS"
case "$*" in
  *'get pods -A -o json')
    printf '%s\n' '{"items":[{"metadata":{"name":"crdb-0","namespace":"test"},"spec":{"initContainers":[{"name":"init"}],"containers":[{"name":"db"},{"name":"cert-reloader"}]}}]}'
    ;;
  *'get events '*) echo 'events unavailable'; exit 1 ;;
  *) printf '%s\n' "$*" ;;
esac
`), 0700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "docker"), []byte(`#!/bin/sh
printf '%s\n' "$*" >> "$ARTIFACT_TEST_COMMANDS"
case "$1" in
  ps) printf '%s\n' k3d-snapshot-server-0 k3d-snapshot-agent-0 k3d-other-agent-0 ;;
  logs) echo 'k3s node logs' ;;
esac
`), 0700))

	collectFailureArtifacts(t, k8s.NewKubectlOptions("k3d-snapshot", "/test/kubeconfig", "test"), dir)
	read := func(file string) string {
		data, err := os.ReadFile(filepath.Join(dir, file))
		require.NoError(t, err)
		return string(data)
	}
	// A failed collection command must not prevent later snapshots or logs.
	require.Contains(t, read("events.txt"), "Collection error:")
	require.Contains(t, read("crdbclusters.yaml"), "crdbclusters.v1beta1.crdb.cockroachlabs.com")
	require.Contains(t, read("legacy-crdbclusters.yaml"), "crdbclusters.v1alpha1.crdb.cockroachlabs.com")
	require.Contains(t, read("crdbclusters.yaml"), "--show-managed-fields=true")
	require.Contains(t, read("legacy-crdbclusters.yaml"), "--show-managed-fields=true")
	for _, container := range []string{"init", "db", "cert-reloader"} {
		require.Contains(t, read("test-crdb-0-"+container+".log"), "--since=30m --tail=2000")
		require.Contains(t, read("test-crdb-0-"+container+"-previous.log"), "--previous")
	}
	require.Contains(t, read("docker-k3d-snapshot-agent-0.log"), "k3s node logs")
	require.NoFileExists(t, filepath.Join(dir, "docker-k3d-snapshot-server-0.log"))
	require.NoFileExists(t, filepath.Join(dir, "docker-k3d-other-agent-0.log"))
	invocations := read("commands.txt")
	require.Contains(t, invocations, "--context k3d-snapshot --kubeconfig /test/kubeconfig")
	require.Contains(t, invocations, "label=k3d.cluster=snapshot")
	require.NotContains(t, invocations, "get secrets")
	require.NotContains(t, invocations, "config view")
}

func TestCaptureFailureArtifactsSkipsSuccessfulTests(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "artifacts")
	t.Setenv("ARTIFACT_DIR", dir)
	CaptureFailureArtifacts(t, k8s.NewKubectlOptions("k3d-snapshot", "", "test"))
	require.NoDirExists(t, dir)
}
