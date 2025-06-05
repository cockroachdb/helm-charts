package cockroachdb_enterprise_operator

import (
	"fmt"
	"log"

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
	Short: "Migrate certs for the CockroachDB Enterprise Operator",
	Long: `Migrate and manage certificates for the CockroachDB Enterprise Operator.

This command performs the following operations:
1. Moves the existing CA certificate from a Kubernetes Secret to a ConfigMap because the operator 
   expects the CA certificate to be in a ConfigMap.
2. Regenerates node certificates to include Subject Alternative Names (SAN) for the CockroachDB join service
3. Generates client certificates if they are not already present in the cluster

The command supports customization of certificate durations and expiry windows for:
- Node certificates (default: 1 year duration, 7 days expiry window)
- Client certificates (default: 28 days duration, 2 days expiry window)

The migration process ensures all certificates are properly configured for use with the CockroachDB Enterprise Operator.`,
	RunE: migrateCertsForCockroachEnterpriseOperator,
}

func init() {
	migrateCertsCmd.PersistentFlags().StringVar(&statefulSetName, "statefulset-name", "", "name of the cockroachdb statefulset resource")
	migrateCertsCmd.PersistentFlags().StringVar(&namespace, "namespace", "default", "name of the cockroachdb statefulset namespace")

	migrateCertsCmd.PersistentFlags().StringVar(&caSecret, "ca-secret", "", "name of user provided CA secret")

	migrateCertsCmd.PersistentFlags().StringVar(&nodeDuration, "node-duration", "8760h", "duration of Node cert. Defaults to 365h (1 year)")
	migrateCertsCmd.PersistentFlags().StringVar(&nodeExpiry, "node-expiry", "168h", "expiry window for Node cert. Defaults to 7 days")

	migrateCertsCmd.PersistentFlags().StringVar(&clientDuration, "client-duration", "672h", "duration of Client cert. Defaults to 28 days")
	migrateCertsCmd.PersistentFlags().StringVar(&clientExpiry, "client-expiry", "48h", "expiry window for Client(root) cert. Defaults to 2 days")

	migrateCertsCmd.PersistentFlags().StringVar(&clusterDomain, "cluster-domain", "cluster.local", "cluster domain")
	migrateCertsCmd.MarkFlagRequired("statefulset-name")

	var err error
	runtimeScheme := runtime.NewScheme()
	_ = certv1.AddToScheme(runtimeScheme)

	_ = clientgoscheme.AddToScheme(runtimeScheme)
	config := controllerruntime.GetConfigOrDie()

	cl, err = client.New(config, client.Options{
		Scheme: runtimeScheme,
		Mapper: nil,
	})
	if err != nil {
		log.Panic("Failed to create client for certificate migration", err)
	}
}

func migrateCertsForCockroachEnterpriseOperator(cmd *cobra.Command, args []string) error {
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
