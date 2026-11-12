package testutil

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gruntwork-io/terratest/modules/k8s"
	corev1 "k8s.io/api/core/v1"
)

// CaptureFailureArtifacts saves diagnostics when ARTIFACT_DIR is set and the
// test failed. E2E tests call this before deleting releases or namespaces or
// tearing down infrastructure, so the snapshot shows the failed state.
// Collection is bounded and best effort so it cannot hide the original failure.
//
// Local files have no automatic expiry; the CI workflow handles upload retention
// and removes its copy after upload.
func CaptureFailureArtifacts(t *testing.T, options *k8s.KubectlOptions) {
	t.Helper()
	root := os.Getenv("ARTIFACT_DIR")
	if !t.Failed() || root == "" {
		return
	}
	cluster := artifactName(options.ContextName)
	if cluster == "" {
		cluster = "current-context"
	}
	dir := filepath.Join(root, t.Name(), cluster)
	if err := os.MkdirAll(filepath.Dir(dir), 0700); err != nil {
		t.Logf("Cannot create artifact directory: %v", err)
		return
	}
	// Preserve the first snapshot, before any cleanup changed the cluster.
	if err := os.Mkdir(dir, 0700); err != nil {
		if !os.IsExist(err) {
			t.Logf("Cannot create artifact directory: %v", err)
		}
		return
	}
	collectFailureArtifacts(t, options, dir)
}

func collectFailureArtifacts(t *testing.T, options *k8s.KubectlOptions, dir string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	run := func(file, command string, args ...string) []byte {
		if ctx.Err() != nil {
			return nil
		}
		commandCtx, commandCancel := context.WithTimeout(ctx, 15*time.Second)
		defer commandCancel()
		out, err := exec.CommandContext(commandCtx, command, args...).CombinedOutput()
		if err != nil {
			out = append(out, []byte(fmt.Sprintf("\nCollection error: %v\n", err))...)
		}
		if err := os.WriteFile(filepath.Join(dir, file), out, 0600); err != nil {
			t.Logf("Cannot write artifact %s: %v", file, err)
		}
		return out
	}

	// Agent logs remain available when the API is unreachable. Exclude server
	// logs, which can contain control-plane credentials in public CI artifacts.
	if strings.HasPrefix(options.ContextName, "k3d-") {
		cluster := strings.TrimPrefix(options.ContextName, "k3d-")
		containers := run("docker-containers.txt", "docker", "ps", "-a", "--filter",
			"label=k3d.cluster="+cluster, "--format", "{{.Names}}")
		for _, container := range strings.Fields(string(containers)) {
			if strings.HasPrefix(container, "k3d-"+cluster+"-agent-") {
				run("docker-"+artifactName(container)+".log", "docker", "logs", "--since=30m", "--tail=2000", container)
			}
		}
	}

	baseArgs := []string{"--request-timeout=10s"}
	if options.ContextName != "" {
		baseArgs = append(baseArgs, "--context", options.ContextName)
	}
	if options.ConfigPath != "" {
		baseArgs = append(baseArgs, "--kubeconfig", options.ConfigPath)
	}
	kubectl := func(file string, args ...string) []byte {
		return run(file, "kubectl", append(append([]string{}, baseArgs...), args...)...)
	}

	for _, snapshot := range []struct {
		file string
		args []string
	}{
		{"pods.txt", []string{"get", "pods", "-A", "-o", "wide", "--show-labels"}},
		{"events.txt", []string{"get", "events", "-A", "--sort-by=.lastTimestamp"}},
		{"nodes.txt", []string{"get", "nodes", "-o", "wide", "--show-labels"}},
		{"describe-nodes.txt", []string{"describe", "nodes"}},
		{"crdbclusters.yaml", []string{"get", "crdbclusters.v1beta1.crdb.cockroachlabs.com", "-A", "-o", "yaml", "--show-managed-fields=true"}},
		{"legacy-crdbclusters.yaml", []string{"get", "crdbclusters.v1alpha1.crdb.cockroachlabs.com", "-A", "-o", "yaml", "--show-managed-fields=true"}},
		{"crdbnodes.yaml", []string{"get", "crdbnodes", "-A", "-o", "yaml"}},
		{"workloads.yaml", []string{"get", "deployments,statefulsets,jobs,cronjobs", "-A", "-o", "yaml"}},
		{"services.yaml", []string{"get", "services,endpointslices", "-A", "-o", "yaml"}},
		{"pdb.yaml", []string{"get", "pdb", "-A", "-o", "yaml"}},
		{"pvc.yaml", []string{"get", "pvc", "-A", "-o", "yaml"}},
		{"pv.yaml", []string{"get", "pv", "-o", "yaml"}},
	} {
		kubectl(snapshot.file, snapshot.args...)
	}

	// Use the pod inventory to capture every container, including init containers
	// and previous instances. Do not collect Secrets or kubeconfig contents.
	var pods corev1.PodList
	if err := json.Unmarshal(kubectl("pod-inventory.json", "get", "pods", "-A", "-o", "json"), &pods); err != nil {
		t.Logf("Cannot read pod inventory for artifact collection: %v", err)
		return
	}
	for _, pod := range pods.Items {
		name := artifactName(pod.Namespace + "-" + pod.Name)
		kubectl(name+".txt", "describe", "pod", pod.Name, "-n", pod.Namespace)
		containers := append(append([]corev1.Container{}, pod.Spec.InitContainers...), pod.Spec.Containers...)
		for _, container := range containers {
			file := name + "-" + artifactName(container.Name)
			args := []string{"logs", pod.Name, "-n", pod.Namespace, "-c", container.Name,
				"--since=30m", "--tail=2000", "--timestamps=true"}
			kubectl(file+".log", args...)
			kubectl(file+"-previous.log", append(args, "--previous")...)
		}
	}
}

func artifactName(name string) string {
	return strings.NewReplacer("/", "_", "\\", "_", ":", "_").Replace(name)
}
