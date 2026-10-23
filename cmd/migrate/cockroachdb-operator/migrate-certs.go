package cockroachdb_operator

import (
	"fmt"

	"github.com/cockroachdb/helm-charts/pkg/generator"
	"github.com/cockroachdb/helm-charts/pkg/migrate"
	certv1 "github.com/jetstack/cert-manager/pkg/apis/certmanager/v1"
	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var (
	cl             client.Client
	caSecret       string
	nodeDuration   string
	nodeExpiry     string
	clientDuration string
	clientExpiry   string
	clusterDomain  string
)

var migrateCertsCmd = &cobra.Command{
	Use:   "migrate-certs",
	Short: "Migrate certs for the CockroachDB Operator",
	Long: `Migrate and manage certificates for the CockroachDB Operator.

This command performs the following operations:
1. Moves the existing CA certificate from a Kubernetes Secret to a ConfigMap because the operator 
   expects the CA certificate to be in a ConfigMap.
2. Regenerates node certificates to include Subject Alternative Names (SAN) for the CockroachDB join service,
   keeping any additional SANs the existing node certificate carries
3. Reissues the client certificate

The command supports customization of certificate durations and expiry windows for:
- Node certificates (default: 1 year duration, 7 days expiry window)
- Client certificates (default: same duration as node certificates, 2 days expiry window)

The migration process ensures all certificates are properly configured for use with the CockroachDB Operator.`,
	RunE: migrateCertsForCockroachDbOperator,
}

func init() {
	migrateCertsCmd.PersistentFlags().StringVar(&statefulSetName, "statefulset-name", "", "name of the cockroachdb statefulset resource")
	migrateCertsCmd.PersistentFlags().StringVar(&namespace, "namespace", "default", "name of the cockroachdb statefulset namespace")

	migrateCertsCmd.PersistentFlags().StringVar(&caSecret, "ca-secret", "", "name of user provided CA secret")

	migrateCertsCmd.PersistentFlags().StringVar(&nodeDuration, "node-duration", "8760h", "duration of Node cert. Defaults to 8760h (1 year)")
	migrateCertsCmd.PersistentFlags().StringVar(&nodeExpiry, "node-expiry", "168h", "expiry window for Node cert. Defaults to 7 days")

	migrateCertsCmd.PersistentFlags().StringVar(&clientDuration, "client-duration", "", "duration of Client cert. Defaults to --node-duration, because nothing rotates the client cert after migration")
	migrateCertsCmd.PersistentFlags().StringVar(&clientExpiry, "client-expiry", "48h", "expiry window for Client(root) cert. Defaults to 2 days")

	migrateCertsCmd.PersistentFlags().StringVar(&clusterDomain, "cluster-domain", "cluster.local", "cluster domain")
	_ = migrateCertsCmd.MarkFlagRequired("statefulset-name")
}

func migrateCertsForCockroachDbOperator(cmd *cobra.Command, args []string) error {
	// Without the self-signer nothing rotates the client certificate, so give
	// it the node certificate's lifetime unless the user chose otherwise.
	if clientDuration == "" {
		clientDuration = nodeDuration
	}

	runtimeScheme := runtime.NewScheme()
	if err := certv1.AddToScheme(runtimeScheme); err != nil {
		return err
	}
	if err := clientgoscheme.AddToScheme(runtimeScheme); err != nil {
		return err
	}
	config, err := controllerruntime.GetConfig()
	if err != nil {
		return err
	}
	cl, err = client.New(config, client.Options{
		Scheme: runtimeScheme,
		Mapper: nil,
	})
	if err != nil {
		return err
	}

	genCerts := generator.NewGenerateCert(cl)
	genCerts.CaSecret = caSecret
	if err := genCerts.NodeCertConfig.SetConfig(nodeDuration, nodeExpiry); err != nil {
		return err
	}
	if err := genCerts.ClientCertConfig.SetConfig(clientDuration, clientExpiry); err != nil {
		return err
	}
	genCerts.OperatorManaged = true
	genCerts.DiscoveryServiceName = statefulSetName
	genCerts.PublicServiceName = fmt.Sprintf("%s-public", statefulSetName)
	genCerts.ClusterDomain = clusterDomain

	if err := migrate.GenerateCertsForOperator(cl, namespace, &genCerts); err != nil {
		return err
	}

	return nil
}
