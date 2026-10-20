package migrate

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cockroachdb/helm-charts/pkg/generator"
	"github.com/cockroachdb/helm-charts/pkg/resource"
	"github.com/cockroachdb/helm-charts/pkg/security"
	"github.com/cockroachdb/helm-charts/pkg/testutils"
	"github.com/mitchellh/hashstructure/v2"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	testStsName      = "test-crdb"
	testNamespace    = "default"
	testKeySize      = 2048
	legacyClientLife = 672 * time.Hour
	nodeLife         = 8760 * time.Hour
)

// legacyExtraSANs are the names the legacy chart's additionalSANs setting put
// on the node certificate. Nothing in the migration's inputs records them
// except the certificate itself.
var legacyExtraSANs = []string{"sql.example.com", "10.0.0.10"}

// seedLegacySelfSignerCluster builds the Kubernetes objects migrate-certs
// finds on a cluster the legacy chart's self-signer managed: the
// StatefulSet with its Helm release annotation, the CA secret with its key,
// a node secret whose certificate carries the standard hosts plus
// legacyExtraSANs (and no join names), and a ready 672h root client secret.
// It returns the fake client and the seeded client certificate bytes.
func seedLegacySelfSignerCluster(t *testing.T) (client.Client, []byte) {
	t.Helper()
	dir := t.TempDir()
	caKey := filepath.Join(dir, "ca.key")
	require.NoError(t, security.CreateCAPair(dir, caKey, testKeySize, 5*365*24*time.Hour, true, true))

	// The legacy chart was not operator-managed, so no join names.
	legacy := generator.GenerateCert{
		PublicServiceName:    testStsName + "-public",
		DiscoveryServiceName: testStsName,
		ClusterDomain:        "cluster.local",
	}
	hosts := append(legacy.NodeCertHosts(testNamespace), legacyExtraSANs...)
	require.NoError(t, security.CreateNodePair(dir, caKey, testKeySize, nodeLife, true, hosts))
	require.NoError(t, security.CreateClientPair(dir, caKey, testKeySize, legacyClientLife, true,
		security.SQLUsername{U: security.RootUser}, false))

	read := func(name string) []byte {
		b, err := os.ReadFile(filepath.Join(dir, name))
		require.NoError(t, err)
		return b
	}
	caCert, caKeyPEM := read("ca.crt"), read("ca.key")
	nodeCert, nodeKey := read("node.crt"), read("node.key")
	clientCert, clientKey := read("client.root.crt"), read("client.root.key")

	// Check the seed is what the tests believe it is: the legacy extras are
	// on the certificate and the join names are not.
	seed, err := security.GetCertObj(nodeCert)
	require.NoError(t, err)
	require.Contains(t, seed.DNSNames, "sql.example.com")
	require.NotContains(t, seed.DNSNames, testStsName+"-join")
	seedIPs := make([]string, 0, len(seed.IPAddresses))
	for _, ip := range seed.IPAddresses {
		seedIPs = append(seedIPs, ip.String())
	}
	require.Contains(t, seedIPs, "10.0.0.10")

	// tlsSecret builds a secret the way the legacy self-signer saved it,
	// including the data-hash annotation that marks it ready.
	now := time.Now()
	tlsSecret := func(name string, d time.Duration, cert, key []byte) *corev1.Secret {
		data := map[string][]byte{corev1.TLSCertKey: cert, corev1.TLSPrivateKeyKey: key, resource.CaCert: caCert}
		hash, err := hashstructure.Hash(data, hashstructure.FormatV2, nil)
		require.NoError(t, err)
		annotations := resource.GetSecretAnnotations(now.Format(time.RFC3339), now.Add(d).Format(time.RFC3339), d.String())
		annotations[resource.SecretDataHash] = fmt.Sprintf("%d", hash)
		return &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace, Annotations: annotations},
			Type:       corev1.SecretTypeTLS,
			Data:       data,
		}
	}
	objs := []client.Object{
		&appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{
			Name: testStsName, Namespace: testNamespace,
			Annotations: map[string]string{"meta.helm.sh/release-name": testStsName},
		}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: testStsName + "-ca-secret", Namespace: testNamespace},
			Data: map[string][]byte{resource.CaCert: caCert, resource.CaKey: caKeyPEM}},
		tlsSecret(testStsName+"-node-secret", nodeLife, nodeCert, nodeKey),
		tlsSecret(testStsName+"-client-secret", legacyClientLife, clientCert, clientKey),
	}
	return testutils.NewFakeClient(testutils.InitScheme(t), objs...), clientCert
}

func readSecret(t *testing.T, cl client.Client, name string) *corev1.Secret {
	t.Helper()
	s := &corev1.Secret{}
	require.NoError(t, cl.Get(context.Background(), types.NamespacedName{Namespace: testNamespace, Name: name}, s))
	return s
}

// TestGenerateCertsForOperatorSelfSigner drives the self-signer branch of the
// migration against a cluster the legacy chart managed, and records what
// comes out the other side.
//
// It documents the current behavior: the regenerated node certificate gains
// the join names but loses the SANs the legacy chart added, and the legacy
// 672h client certificate is kept as is even though nothing will rotate it.
// The next commit flips both assertions.
func TestGenerateCertsForOperatorSelfSigner(t *testing.T) {
	cl, seededClientCert := seedLegacySelfSignerCluster(t)

	rc := generator.NewGenerateCert(cl)
	rc.OperatorManaged = true
	rc.DiscoveryServiceName = testStsName
	rc.PublicServiceName = testStsName + "-public"
	rc.ClusterDomain = "cluster.local"
	require.NoError(t, rc.NodeCertConfig.SetConfig(nodeLife.String(), "168h"))
	require.NoError(t, rc.ClientCertConfig.SetConfig(nodeLife.String(), "48h"))

	require.NoError(t, GenerateCertsForOperator(cl, testNamespace, &rc))

	node, err := security.GetCertObj(readSecret(t, cl, testStsName+"-node-secret").Data[corev1.TLSCertKey])
	require.NoError(t, err)
	for _, h := range []string{testStsName + "-join", testStsName + "-join." + testNamespace} {
		require.Contains(t, node.DNSNames, h, "regenerated node cert must carry the join names")
	}
	ips := make([]string, 0, len(node.IPAddresses))
	for _, ip := range node.IPAddresses {
		ips = append(ips, ip.String())
	}
	// Current behavior: the legacy chart's extra SANs are dropped.
	require.NotContains(t, node.DNSNames, "sql.example.com")
	require.NotContains(t, ips, "10.0.0.10")

	// Current behavior: the ready legacy client certificate is kept.
	require.Equal(t, seededClientCert, readSecret(t, cl, testStsName+"-client-secret").Data[corev1.TLSCertKey])
}
