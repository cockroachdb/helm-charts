package testutil

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/gruntwork-io/terratest/modules/helm"
	"github.com/gruntwork-io/terratest/modules/k8s"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func InstallIngressAndMetalLB(t *testing.T) {
	t.Log("Installing NGINX Ingress Controller")

	// 1. Add repo and install chart
	options := &helm.Options{}

	_, _ = helm.RunHelmCommandAndGetOutputE(t, options, "repo", "add", "ingress-nginx", "https://kubernetes.github.io/ingress-nginx")
	_, _ = helm.RunHelmCommandAndGetOutputE(t, options, "repo", "update")

	helm.Install(t, options, "ingress-nginx/ingress-nginx", "ingress-nginx")

	// 2. Install MetalLB
	t.Log("Installing MetalLB")

	kubectlOptionsMetallb := k8s.NewKubectlOptions("", "", "metallb-system")

	k8s.KubectlApply(t, kubectlOptionsMetallb, "https://raw.githubusercontent.com/metallb/metallb/v0.15.2/config/manifests/metallb-native.yaml")

	// Wait for MetalLB controller and speaker pods to be ready
	t.Log("Waiting for MetalLB pods to be ready")
	pods := k8s.ListPods(t, kubectlOptionsMetallb, metav1.ListOptions{})

	for _, pod := range pods {
		t.Logf("Waiting for pod %s to be available...", pod.Name)
		k8s.WaitUntilPodAvailable(t, kubectlOptionsMetallb, pod.Name, 60, 5*time.Second)
	}

	// 3. Fetch the Docker network IP range used by k3d
	t.Log("Getting IP range from k3d Docker network")
	cmd := exec.Command("docker", "network", "inspect", "k3d-chart-testing", "--format", "{{(index .IPAM.Config 0).Subnet}}")
	output, err := cmd.Output()
	require.NoError(t, err)

	subnet := strings.TrimSpace(string(output))
	t.Logf("Docker network subnet: %s", subnet)

	// Example: 172.20.0.0/16 -> use 172.20.255.1-172.20.255.25
	parts := strings.Split(subnet, ".")
	require.Len(t, parts, 4)
	ipRange := fmt.Sprintf("%s.%s.%s.1-%s.%s.%s.25", parts[0], parts[1], "255", parts[0], parts[1], "255")

	t.Logf("MetalLB IP address pool: %s", ipRange)

	// 4. Apply MetalLB IPAddressPool and L2Advertisement
	ipPoolYAML := fmt.Sprintf(`
apiVersion: metallb.io/v1beta1
kind: IPAddressPool
metadata:
  name: demo-pool
  namespace: metallb-system
spec:
  addresses:
  - %s
---
apiVersion: metallb.io/v1beta1
kind: L2Advertisement
metadata:
  name: demo-advertisement
  namespace: metallb-system
`, ipRange)

	t.Log("Applying MetalLB IPAddressPool and L2Advertisement config")
	k8s.KubectlApplyFromString(t, kubectlOptionsMetallb, ipPoolYAML)
}

func UninstallIngressAndMetalLB(t *testing.T) {
	t.Log("Uninstalling NGINX Ingress Controller")

	// 1. Uninstall ingress-nginx Helm release
	options := &helm.Options{}
	err := helm.DeleteE(t, options, "ingress-nginx", true)
	if err != nil {
		t.Logf("Warning: Failed to uninstall ingress-nginx: %v", err)
	} else {
		t.Log("Successfully uninstalled ingress-nginx")
	}

	// 2. Delete MetalLB resources
	t.Log("Uninstalling MetalLB")

	kubectlOptionsMetallb := k8s.NewKubectlOptions("", "", "metallb-system")

	// Delete the IPAddressPool and L2Advertisement
	t.Log("Deleting MetalLB IPAddressPool and L2Advertisement config")
	_ = k8s.KubectlDeleteFromStringE(t, kubectlOptionsMetallb, `
apiVersion: metallb.io/v1beta1
kind: IPAddressPool
metadata:
  name: demo-pool
  namespace: metallb-system
---
apiVersion: metallb.io/v1beta1
kind: L2Advertisement
metadata:
  name: demo-advertisement
  namespace: metallb-system
`)

	// Delete the MetalLB manifests (controller and speaker)
	t.Log("Deleting MetalLB core manifests")
	err = k8s.KubectlDeleteE(t, kubectlOptionsMetallb, "https://raw.githubusercontent.com/metallb/metallb/v0.15.2/config/manifests/metallb-native.yaml")
	if err != nil {
		t.Logf("Warning: Failed to delete MetalLB core manifests: %v", err)
	} else {
		t.Log("Successfully deleted MetalLB core manifests")
	}
}

func TestIngressRoutingDirect(t *testing.T, hostName string) {
	kubectlOptions := k8s.NewKubectlOptions("", "", "default")

	// Get the Service object
	svc := k8s.GetService(t, kubectlOptions, "ingress-nginx-controller")

	ingressIP := k8s.GetServiceEndpoint(t, kubectlOptions, svc, 80)
	t.Logf("Ingress IP: %s", ingressIP)

	url := fmt.Sprintf("http://%s/", ingressIP)

	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		t.Fatalf("Failed to create request: %v", err)
	}
	req.Host = hostName // This mimics Ingress routing based on host
	dialer := &net.Dialer{}
	transport := &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			// Always connect to the ingress IP, but pretend the host is ui.local.com
			if strings.Contains(addr, hostName) {
				addr = ingressIP
			}
			return dialer.DialContext(ctx, network, addr)
		},
	}

	client := &http.Client{
		Timeout:   5 * time.Second,
		Transport: transport,
	}

	// Retry logic
	retries := 10
	for i := 0; i < retries; i++ {
		resp, err := client.Do(req)
		if err != nil {
			t.Logf("Attempt %d: Request failed: %v", i+1, err)
		} else {
			defer resp.Body.Close()
			t.Logf("Attempt %d: Status Code: %d", i+1, resp.StatusCode)
			if resp.StatusCode == 200 {
				t.Log("Ingress returned 200 OK — success")
				return
			}
		}
		time.Sleep(5 * time.Second)
	}

	t.Fatalf("Failed to get 200 OK from ingress after %d attempts", retries)
}
