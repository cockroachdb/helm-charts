package migrate

import (
	"context"
	"fmt"
	"net"
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

// clusterShape selects which pre-migration deployment
// seedLegacySelfSignerCluster reproduces.
type clusterShape int

const (
	// helmChart is a cluster the legacy chart's self-signer managed.
	helmChart clusterShape = iota
	// helmChartNoNodeSecret is helmChart with the node secret missing.
	helmChartNoNodeSecret
	// publicOperator is a cluster the public operator managed.
	publicOperator
)

// seedLegacySelfSignerCluster builds the Kubernetes objects migrate-certs
// finds on a cluster the legacy chart's self-signer managed: the
// StatefulSet with its Helm release annotation, the CA secret with its key,
// a node secret whose certificate carries the standard hosts plus
// legacyExtraSANs (and no join names), and a ready 672h root client secret.
// The publicOperator shape instead owns the StatefulSet by a CrdbCluster,
// keeps only ca.key in <name>-ca and the node certificate, with ca.crt, in
// <name>-node, and has no client secret. It returns the fake client and the
// seeded client certificate bytes.
func seedLegacySelfSignerCluster(t *testing.T, shape clusterShape) (client.Client, []byte) {
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
	var objs []client.Object
	switch shape {
	case helmChart, helmChartNoNodeSecret:
		objs = []client.Object{
			&appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{
				Name: testStsName, Namespace: testNamespace,
				Annotations: map[string]string{"meta.helm.sh/release-name": testStsName},
			}},
			&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: testStsName + "-ca-secret", Namespace: testNamespace},
				Data: map[string][]byte{resource.CaCert: caCert, resource.CaKey: caKeyPEM}},
			tlsSecret(testStsName+"-client-secret", legacyClientLife, clientCert, clientKey),
		}
		if shape == helmChart {
			objs = append(objs, tlsSecret(testStsName+"-node-secret", nodeLife, nodeCert, nodeKey))
		}
	case publicOperator:
		objs = []client.Object{
			&appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{
				Name: testStsName, Namespace: testNamespace,
				OwnerReferences: []metav1.OwnerReference{{
					APIVersion: "crdb.cockroachlabs.com/v1alpha1", Kind: "CrdbCluster", Name: testStsName, UID: "crdbcluster-uid",
				}},
			}},
			// The public operator keeps ca.crt out of the CA secret; the
			// migration reads it from the node secret instead.
			&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: testStsName + "-ca", Namespace: testNamespace},
				Data: map[string][]byte{resource.CaKey: caKeyPEM}},
			tlsSecret(testStsName+"-node", nodeLife, nodeCert, nodeKey),
		}
	default:
		t.Fatalf("unknown cluster shape %d", shape)
	}
	return testutils.NewFakeClient(testutils.InitScheme(t), objs...), clientCert
}

func readSecret(t *testing.T, cl client.Client, name string) *corev1.Secret {
	t.Helper()
	s := &corev1.Secret{}
	require.NoError(t, cl.Get(context.Background(), types.NamespacedName{Namespace: testNamespace, Name: name}, s))
	return s
}

// newMigrationGenerateCert configures a GenerateCert the way migrate-certs
// does for testStsName.
func newMigrationGenerateCert(t *testing.T, cl client.Client) generator.GenerateCert {
	t.Helper()
	rc := generator.NewGenerateCert(cl)
	rc.OperatorManaged = true
	rc.DiscoveryServiceName = testStsName
	rc.PublicServiceName = testStsName + "-public"
	rc.ClusterDomain = "cluster.local"
	require.NoError(t, rc.NodeCertConfig.SetConfig(nodeLife.String(), "168h"))
	require.NoError(t, rc.ClientCertConfig.SetConfig(nodeLife.String(), "48h"))
	return rc
}

// requireNodeCert parses the regenerated node certificate and checks it
// carries every standard DNS name, join names included.
func requireNodeCert(t *testing.T, cl client.Client, rc *generator.GenerateCert) (dnsNames, ips []string) {
	t.Helper()
	node, err := security.GetCertObj(readSecret(t, cl, testStsName+"-node-secret").Data[corev1.TLSCertKey])
	require.NoError(t, err)
	var dnsOnly []string
	for _, h := range rc.NodeCertHosts(testNamespace) {
		if net.ParseIP(h) == nil {
			dnsOnly = append(dnsOnly, h)
		}
	}
	require.Subset(t, node.DNSNames, dnsOnly, "regenerated node cert must carry the standard hosts and join names")
	for _, ip := range node.IPAddresses {
		ips = append(ips, ip.String())
	}
	return node.DNSNames, ips
}

// TestGenerateCertsForOperatorSelfSigner drives the self-signer branch of the
// migration against a cluster the legacy chart managed, and checks that the
// identity the old certificates carried survives: the regenerated node
// certificate gains the join names and keeps the SANs the legacy chart added,
// and the legacy 672h client certificate is reissued with the node
// certificate's lifetime, because nothing rotates it after migration.
func TestGenerateCertsForOperatorSelfSigner(t *testing.T) {
	cl, seededClientCert := seedLegacySelfSignerCluster(t, helmChart)
	rc := newMigrationGenerateCert(t, cl)

	require.NoError(t, GenerateCertsForOperator(cl, testNamespace, &rc))

	dnsNames, ips := requireNodeCert(t, cl, &rc)
	// The legacy chart's extra SANs are carried over from the old certificate.
	require.Contains(t, dnsNames, "sql.example.com")
	require.Contains(t, ips, "10.0.0.10")

	// The client certificate is reissued with the node certificate's lifetime.
	newClient := readSecret(t, cl, testStsName+"-client-secret").Data[corev1.TLSCertKey]
	require.NotEqual(t, seededClientCert, newClient)
	parsed, err := security.GetCertObj(newClient)
	require.NoError(t, err)
	require.WithinDuration(t, time.Now().Add(nodeLife), parsed.NotAfter, 24*time.Hour)
}

// TestGenerateCertsForOperatorWithoutExistingNodeSecret checks that a missing
// node secret is not an error: there is nothing to carry over, and the node
// certificate is issued with the standard hosts and join names only.
func TestGenerateCertsForOperatorWithoutExistingNodeSecret(t *testing.T) {
	cl, _ := seedLegacySelfSignerCluster(t, helmChartNoNodeSecret)
	rc := newMigrationGenerateCert(t, cl)

	require.NoError(t, GenerateCertsForOperator(cl, testNamespace, &rc))

	requireNodeCert(t, cl, &rc)
	require.Empty(t, rc.AdditionalSANs)
}

// TestGenerateCertsForOperatorPublicOperatorShape drives the migration
// against the objects the public operator leaves behind, where the node
// certificate lives in <name>-node rather than <name>-node-secret, and checks
// that its extra SANs are carried over all the same.
func TestGenerateCertsForOperatorPublicOperatorShape(t *testing.T) {
	cl, _ := seedLegacySelfSignerCluster(t, publicOperator)
	rc := newMigrationGenerateCert(t, cl)

	require.NoError(t, GenerateCertsForOperator(cl, testNamespace, &rc))

	dnsNames, ips := requireNodeCert(t, cl, &rc)
	require.Contains(t, dnsNames, "sql.example.com")
	require.Contains(t, ips, "10.0.0.10")
}
