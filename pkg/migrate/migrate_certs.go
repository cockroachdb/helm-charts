package migrate

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/cockroachdb/helm-charts/pkg/generator"
	"github.com/cockroachdb/helm-charts/pkg/kube"
	"github.com/cockroachdb/helm-charts/pkg/resource"
	"github.com/cockroachdb/helm-charts/pkg/security"
	util "github.com/cockroachdb/helm-charts/pkg/utils"
	certv1 "github.com/jetstack/cert-manager/pkg/apis/certmanager/v1"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var ctx = context.Background()

func GenerateCertsForOperator(cl client.Client, namespace string, rc *generator.GenerateCert) error {
	certsDir, cleanup := util.CreateTempDir("certsDir")
	defer cleanup()
	rc.CertsDir = certsDir

	rc.CAKey = filepath.Join(certsDir, "ca.key")

	operatorType, err := identifyHelmOrPublicOperator(cl, rc.DiscoveryServiceName, namespace)
	if err != nil {
		return err
	}

	if operatorType == "helm" {
		isCertManager, err := updateCertsForCertManager(cl, rc, namespace)
		if err != nil {
			return err
		}
		if isCertManager {
			var caSecret = "cockroach-ca"
			if rc.CaSecret != "" {
				caSecret = rc.CaSecret
			}
			// If the cert-manager is used, we don't need to generate the certs.
			fmt.Println("✅ Successfully updated the cockroachdb-node certificate to include the new DNS names.")

			fmt.Println()
			fmt.Println("ℹ️  Note: By default, cert-manager stores the CA certificate in a Secret, which is used by the Issuer.")
			fmt.Println()
			fmt.Println("🔁 To provide the CA certificate in a ConfigMap (required by some applications like CockroachDB), you can use the trust-manager project:")
			fmt.Println("   [trust-manager] https://cert-manager.io/docs/trust/trust-manager/")
			fmt.Println()
			fmt.Println("⚙️  The trust-manager can be configured to automatically copy the CA certificate from a Secret to a ConfigMap.")
			fmt.Println()
			fmt.Println("📦 If your CA Secret is in the 'cockroachdb' namespace, make sure your trust-manager is configured to reference it.")
			fmt.Println("   You can do this by setting the trust namespace via Helm:")
			fmt.Printf("helm upgrade trust-manager jetstack/trust-manager --install --namespace cert-manager --set app.trust.namespace=%s\n", namespace)
			fmt.Println()
			fmt.Println("ℹ️  You can use the following command to generate the required ConfigMap:")
			fmt.Printf(`cat <<EOF | kubectl apply -f -
apiVersion: trust.cert-manager.io/v1alpha1
kind: Bundle
metadata:
  name: %s-ca-crt
spec:
  sources:
    - secret:
        name: %s
        key: ca.crt
  target:
    configMap:
      key: ca.crt
    namespaceSelector:
      matchLabels:
       kubernetes.io/metadata.name: %s
EOF`, rc.DiscoveryServiceName, caSecret, namespace)
			fmt.Println()
			fmt.Printf("Configmap name will be %s-ca-crt in %s namespace \n", rc.DiscoveryServiceName, namespace)

			return nil
		}
	}

	if rc.CaSecret == "" {
		if operatorType == "operator" {
			rc.CaSecret = fmt.Sprintf("%s-ca", rc.DiscoveryServiceName)
		} else {
			rc.CaSecret = fmt.Sprintf("%s-ca-secret", rc.DiscoveryServiceName)
		}
	}

	nodeSecretName := fmt.Sprintf("%s-node-secret", rc.DiscoveryServiceName)
	clientSecretName := fmt.Sprintf("%s-client-secret", rc.DiscoveryServiceName)

	// Load the CA secret from the CA secret or the node secret
	// CA secret will have ca.crt in the official helm chart and not in the public operator managed secret
	// Node secret will have ca.crt in the public operator managed secret.
	if err := loadCASecret(cl, rc, fmt.Sprintf("%s-node", rc.DiscoveryServiceName), namespace); err != nil {
		return err
	}

	if err := rc.GenerateNodeCert(ctx, nodeSecretName, namespace); err != nil {
		return err
	}

	if err := rc.GenerateClientCert(ctx, clientSecretName, namespace); err != nil {
		return err
	}

	return nil
}

func identifyHelmOrPublicOperator(cl client.Client, name, namespace string) (string, error) {
	var sts appsv1.StatefulSet
	if err := cl.Get(context.Background(), types.NamespacedName{Name: name, Namespace: namespace}, &sts); err != nil {
		return "", err
	}

	if len(sts.OwnerReferences) > 0 && sts.OwnerReferences[0].Kind == "CrdbCluster" {
		return "operator", nil
	}

	if strings.Contains(sts.Name, sts.Annotations["meta.helm.sh/release-name"]) {
		return "helm", nil
	}

	return "", fmt.Errorf("unknown statefulset deployment")
}

// updateCertsForCertManager updates the node certificate for cert-manager
func updateCertsForCertManager(cl client.Client, rc *generator.GenerateCert, namespace string) (bool, error) {
	// Get the node certificate CR
	nodeCertificate := certv1.Certificate{}
	nodeCertificateName := fmt.Sprintf("%s-node", rc.DiscoveryServiceName)
	if err := cl.Get(context.Background(), types.NamespacedName{Name: nodeCertificateName, Namespace: namespace}, &nodeCertificate); err != nil {
		return false, nil
	}

	// Update the node certificate with some DNSNames
	dnsNames := []string{
		fmt.Sprintf("%s-join", rc.DiscoveryServiceName),
		fmt.Sprintf("%s-join.%s", rc.DiscoveryServiceName, namespace),
		fmt.Sprintf("%s-join.%s.svc.%s", rc.DiscoveryServiceName, namespace, rc.ClusterDomain),
	}
	nodeCertificate.Spec.DNSNames = append(nodeCertificate.Spec.DNSNames, dnsNames...)

	// update the node certificate with the new DNSNames
	if err := cl.Update(context.Background(), &nodeCertificate); err != nil {
		return false, errors.Wrap(err, "failed to update node certificate with new DNSNames")
	}

	return true, nil
}

// loadCASecret loads the CA secret from the CA secret or the node secret
// If ca.crt is present in the CA secret, then we use the CA secret
// If ca.crt is not present in the CA secret, then we use the node secret
// If ca.crt is not present in the node secret, then we return an error
func loadCASecret(cl client.Client, rc *generator.GenerateCert, existingNodeSecretName, namespace string) error {
	var caCert, caKey []byte
	secret, err := resource.LoadTLSSecret(rc.CaSecret, resource.NewKubeResource(ctx, cl, namespace, kube.DefaultPersister))
	if err != nil {
		return errors.Wrap(err, "failed to get CA key secret")
	}

	if !secret.ReadyCA() {
		if !secret.IsCAKeyPresent() {
			return errors.Wrap(err, "CA secret doesn't contain the required CA key")
		}

		nodeSecret, err := resource.LoadTLSSecret(existingNodeSecretName, resource.NewKubeResource(ctx, cl, namespace, kube.DefaultPersister))
		if err != nil {
			return errors.Wrap(err, "failed to get node secret")
		}

		if !nodeSecret.IsCACertPresent() {
			return errors.Wrap(err, "node secret doesn't contain the required CA cert")
		}

		caCert = nodeSecret.CA()
	} else {
		caCert = secret.CA()
	}

	caKey = secret.CAKey()
	cm := resource.CreateConfigMap(namespace, rc.CaSecret, caCert,
		resource.NewKubeResource(ctx, cl, namespace, kube.DefaultPersister))
	if err = cm.Update(); err != nil {
		return errors.Wrap(err, "failed to update CA cert in ConfigMap")
	}
	logrus.Infof("Generated and saved CA certificate in ConfigMap [%s]", rc.CaSecret)

	if err := os.WriteFile(filepath.Join(rc.CertsDir, resource.CaCert), caCert, security.CertFileMode); err != nil {
		return errors.Wrap(err, "failed to write CA cert")
	}

	if err := os.WriteFile(rc.CAKey, caKey, security.KeyFileMode); err != nil {
		return errors.Wrap(err, "failed to write CA key")
	}

	return nil
}
