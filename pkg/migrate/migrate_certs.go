package migrate

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/cockroachdb/helm-charts/pkg/generator"
	"github.com/cockroachdb/helm-charts/pkg/kube"
	"github.com/cockroachdb/helm-charts/pkg/resource"
	"github.com/cockroachdb/helm-charts/pkg/security"
	util "github.com/cockroachdb/helm-charts/pkg/utils"
	appsv1 "k8s.io/api/apps/v1"
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

	// If we are using the operator to manage secrets then we need to store the CA cert in a
	// ConfigMap.
	if rc.CaSecret != "" && rc.OperatorManaged {
		cm := resource.CreateConfigMap(namespace, rc.CaSecret, caCert,
			resource.NewKubeResource(ctx, cl, namespace, kube.DefaultPersister))
		if err = cm.Update(); err != nil {
			return errors.Wrap(err, "failed to update CA cert in ConfigMap")
		}
		logrus.Infof("Generated and saved CA certificate in ConfigMap [%s]", rc.CaSecret)
	}

	if err := os.WriteFile(filepath.Join(rc.CertsDir, resource.CaCert), caCert, security.CertFileMode); err != nil {
		return errors.Wrap(err, "failed to write CA cert")
	}

	if err := os.WriteFile(rc.CAKey, caKey, security.KeyFileMode); err != nil {
		return errors.Wrap(err, "failed to write CA key")
	}

	return nil
}
