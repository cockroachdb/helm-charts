/*
Copyright 2021 The Cockroach Authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    https://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package generator

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/cockroachdb/helm-charts/pkg/kube"
	"github.com/cockroachdb/helm-charts/pkg/resource"
	"github.com/cockroachdb/helm-charts/pkg/security"
	util "github.com/cockroachdb/helm-charts/pkg/utils"
)

const defaultKeySize = 2048

// Options settable via command-line flags. See below for defaults.
var keySize int
var allowCAKeyReuse bool
var overwriteFiles bool
var generatePKCS8Key bool

func init() {
	keySize = defaultKeySize
	allowCAKeyReuse = false
	overwriteFiles = true
	generatePKCS8Key = false
}

// GenerateCert is the structure containing all the certificate related info
type GenerateCert struct {
	client                    client.Client
	CertsDir                  string
	CaSecret                  string
	CAKey                     string
	CaCertConfig              *certConfig
	RotateCACert              bool
	CACronSchedule            string
	NodeCertConfig            *certConfig
	RotateNodeCert            bool
	ClientCertConfig          *certConfig
	RotateClientCert          bool
	NodeAndClientCronSchedule string
	PublicServiceName         string
	DiscoveryServiceName      string
	ClusterDomain             string
	ReadinessWait             time.Duration
	PodUpdateTimeout          time.Duration
}

type certConfig struct {
	Duration     time.Duration
	ExpiryWindow time.Duration
}

// SetConfig sets the certificate duration and expiryWindow
func (c *certConfig) SetConfig(duration, expiryWindow string) error {

	dur, err := time.ParseDuration(duration)
	if err != nil {
		return fmt.Errorf("failed to parse duration %s", err.Error())
	}
	c.Duration = dur

	expW, err := time.ParseDuration(expiryWindow)
	if err != nil {
		return fmt.Errorf("failed to expiryWindow %s", err.Error())
	}
	c.ExpiryWindow = expW

	return nil
}

func NewGenerateCert(cl client.Client) GenerateCert {
	return GenerateCert{
		client:           cl,
		CaCertConfig:     &certConfig{},
		NodeCertConfig:   &certConfig{},
		ClientCertConfig: &certConfig{},
	}
}

// Do func generates the various certificates required and then stores them in respective secrets.
func (rc *GenerateCert) Do(ctx context.Context, namespace string) error {

	// create the various temporary directories to store the certificates in.
	// These directories will be deleted when the code flow is completed.
	logrus.SetLevel(logrus.InfoLevel)

	certsDir, cleanup := util.CreateTempDir("certsDir")
	defer cleanup()
	rc.CertsDir = certsDir

	caDir, cleanupCADir := util.CreateTempDir("caDir")
	defer cleanupCADir()
	rc.CAKey = filepath.Join(caDir, "ca.key")

	// generate the base CA cert and key
	if err := rc.generateCA(ctx, rc.getCASecretName(), namespace); err != nil {
		msg := " error Generating CA"
		logrus.Error(err, msg)
		return errors.Wrap(err, msg)
	}

	// In the case of rotate CA, skip node and client certificate rotation
	if rc.RotateCACert {
		return nil
	}

	// generate the client certificates for the database to use
	if err := rc.generateClientCert(ctx, rc.getClientSecretName(), namespace); err != nil {
		msg := " error Generating Client Certificate"
		logrus.Error(err, msg)
		return errors.Wrap(err, msg)
	}

	// generate the node certificate for the database to use
	if err := rc.generateNodeCert(ctx, rc.getNodeSecretName(), namespace); err != nil {
		msg := " error Generating Node Certificate"
		logrus.Error(err, msg)
		return errors.Wrap(err, msg)
	}

	return nil
}

// ClientCertGenerate generates the custom user client only certificates and creates the secret.
func (rc *GenerateCert) ClientCertGenerate(ctx context.Context, namespace string) error {
	logrus.SetLevel(logrus.InfoLevel)

	certsDir, cleanup := util.CreateTempDir("certsDir")
	defer cleanup()
	rc.CertsDir = certsDir

	caDir, cleanupCADir := util.CreateTempDir("caDir")
	defer cleanupCADir()
	rc.CAKey = filepath.Join(caDir, "ca.key")

	caSecret, caSecretExist := os.LookupEnv("CA_SECRET")
	if rc.CaSecret == "" && caSecret == "" {
		return errors.New("provide CA secret name to generate custom user client certificates")
	} else if caSecretExist {
		rc.CaSecret = caSecret
	}

	// Load the CA secrets into certificate files in caDir and certDir
	if err := rc.LoadCASecret(ctx, namespace); err != nil {
		return err
	}

	// generate the client certificates for the database to use
	if err := rc.generateClientCert(ctx, rc.getClientSecretName(), namespace); err != nil {
		msg := " error Generating Client Certificate"
		logrus.Error(err, msg)
		return errors.Wrap(err, msg)
	}

	return nil
}

// generateCA generates the CA key and certificate if not given by the user and stores them in a secret.
func (rc *GenerateCert) generateCA(ctx context.Context, CASecretName string, namespace string) error {

	// if CA secret is given by user then validate it and use that
	if rc.CaSecret != "" {
		logrus.Infof("skipping CA cert generation, using user provided CA secret [%s]", rc.CaSecret)

		return rc.LoadCASecret(ctx, namespace)
	}

	secret, err := resource.LoadTLSSecret(CASecretName, resource.NewKubeResource(ctx, rc.client, namespace, kube.DefaultPersister))
	if client.IgnoreNotFound(err) != nil {
		return errors.Wrap(err, "failed to get CA secret")
	}

	// inline func used to generate CA cert and key
	generate := func(rc *GenerateCert, CASecretName, namespace string) error {
		logrus.Info("Generating CA")

		// create the CA Pair certificates
		if err = errors.Wrap(
			security.CreateCAPair(
				rc.CertsDir,
				rc.CAKey,
				keySize,
				rc.CaCertConfig.Duration,
				allowCAKeyReuse,
				overwriteFiles),
			"failed to generate CA cert and key"); err != nil {
			return err
		}

		// Read the ca key into memory
		cakey, err := os.ReadFile(rc.CAKey)
		if err != nil {
			return errors.Wrap(err, "unable to read ca.key")
		}

		// Read the ca cert into memory
		caCert, err := os.ReadFile(filepath.Join(rc.CertsDir, resource.CaCert))
		if err != nil {
			return errors.Wrap(err, "unable to read ca.crt")
		}

		validFrom, validUpto, err := rc.getCertLife(caCert)
		if err != nil {
			return err
		}

		// create and save the TLS certificates into a secret
		secret = resource.CreateTLSSecret(CASecretName, corev1.SecretTypeOpaque,
			resource.NewKubeResource(ctx, rc.client, namespace, kube.DefaultPersister))

		// add certificate info in the secret annotations
		annotations := resource.GetSecretAnnotations(validFrom, validUpto, rc.CaCertConfig.Duration.String())

		if err = secret.UpdateCASecret(cakey, caCert, annotations); err != nil {
			return errors.Wrap(err, "failed to update ca key secret ")
		}

		logrus.Infof("Generated and saved CA key and certificate in secret [%s]", CASecretName)
		return nil
	}

	// check if the existing secret is ready to be consumed. If found ready, skip cert generation
	if secret.ReadyCA() && secret.ValidateAnnotations() {

		if rc.RotateCACert {
			isRequired, reason := secret.IsRotationRequired(rc.CaCertConfig.Duration, rc.CACronSchedule)
			if isRequired {
				logrus.Infof("CA Certificate: %s", reason)

				// writing old cert file so that the new CA is a bundle of both old and new CA cert
				if err := os.WriteFile(filepath.Join(rc.CertsDir, resource.CaCert), secret.CA(), security.CertFileMode); err != nil {
					return errors.Wrap(err, "failed to write CA cert")
				}

				if err := generate(rc, CASecretName, namespace); err != nil {
					return err
				}

				return rc.UpdateNewCA(ctx, namespace)

			}
		}

		logrus.Infof("CA secret [%s] is found in ready state, skipping CA generation", CASecretName)

		if err := os.WriteFile(filepath.Join(rc.CertsDir, resource.CaCert), secret.CA(), security.CertFileMode); err != nil {
			return errors.Wrap(err, "failed to write CA cert")
		}

		if err := os.WriteFile(rc.CAKey, secret.CAKey(), security.KeyFileMode); err != nil {
			return errors.Wrap(err, "failed to write CA key")
		}
		return nil
	}

	// generate new certificate
	return generate(rc, CASecretName, namespace)
}

// generateNodeCert generates the Node key and certificate and stores them in a secret.
func (rc *GenerateCert) generateNodeCert(ctx context.Context, nodeSecretName string, namespace string) (err error) {

	secret, err := resource.LoadTLSSecret(nodeSecretName, resource.NewKubeResource(ctx, rc.client, namespace, kube.DefaultPersister))
	if client.IgnoreNotFound(err) != nil {
		return errors.Wrap(err, "failed to get node TLS secret")
	}

	// inline func used to generate node cert and key
	generate := func(rc *GenerateCert, nodeSecretName, namespace string) error {
		logrus.Info("Generating node certificate")

		// hosts are the various DNS names and IP address that have to exist in the Node certificates
		// for the database to function
		hosts := []string{
			"localhost",
			"127.0.0.1",
			rc.PublicServiceName,
			fmt.Sprintf("%s.%s", rc.PublicServiceName, namespace),
			fmt.Sprintf("%s.%s.svc.%s", rc.PublicServiceName, namespace, rc.ClusterDomain),
			fmt.Sprintf("*.%s", rc.DiscoveryServiceName),
			fmt.Sprintf("*.%s.%s", rc.DiscoveryServiceName, namespace),
			fmt.Sprintf("*.%s.%s.svc.%s", rc.DiscoveryServiceName, namespace, rc.ClusterDomain),
		}

		// create the Node Pair certificates
		if err = errors.Wrap(
			security.CreateNodePair(
				rc.CertsDir,
				rc.CAKey,
				keySize,
				rc.NodeCertConfig.Duration,
				overwriteFiles,
				hosts),
			"failed to generate node certificate and key"); err != nil {
			return err
		}

		// Read the CA certificate into memory
		ca, err := os.ReadFile(filepath.Join(rc.CertsDir, resource.CaCert))
		if err != nil {
			return errors.Wrap(err, "unable to read ca.crt")
		}

		// Read the node certificate into memory
		pemCert, err := os.ReadFile(filepath.Join(rc.CertsDir, "node.crt"))
		if err != nil {
			return errors.Wrap(err, "unable to read node.crt")
		}

		validFrom, validUpto, err := rc.getCertLife(pemCert)
		if err != nil {
			return err
		}

		// Read the node key into memory
		pemKey, err := os.ReadFile(filepath.Join(rc.CertsDir, "node.key"))
		if err != nil {
			return errors.Wrap(err, "unable to ready node.key")
		}

		// add certificate info in the secret annotations
		annotations := resource.GetSecretAnnotations(validFrom, validUpto, rc.NodeCertConfig.Duration.String())

		// create and save the TLS certificates into a secret
		secret = resource.CreateTLSSecret(nodeSecretName, corev1.SecretTypeTLS,
			resource.NewKubeResource(ctx, rc.client, namespace, kube.DefaultPersister))

		if err = secret.UpdateTLSSecret(pemCert, pemKey, ca, annotations); err != nil {
			return errors.Wrap(err, "failed to update node TLS secret certs")
		}

		logrus.Infof("Generated and saved node key and certificate in secret [%s]", nodeSecretName)

		return nil
	}
	// check if the existing secret is ready to be consumed. If found ready, skip cert generation
	if secret.Ready() && secret.ValidateAnnotations() {

		if rc.RotateNodeCert {
			isRequired, reason := secret.IsRotationRequired(rc.NodeCertConfig.Duration, rc.NodeAndClientCronSchedule)
			if isRequired {
				logrus.Infof("Node Certificate: %s", reason)

				if err = generate(rc, nodeSecretName, namespace); err != nil {
					return err
				}

				if err = kube.RollingUpdate(ctx, rc.client, rc.DiscoveryServiceName, namespace, rc.ReadinessWait, rc.PodUpdateTimeout); err != nil {
					return
				}
				return nil
			}
		}

		logrus.Infof("Node secret [%s] is found in ready state, skipping Node cert generation", nodeSecretName)
		return nil
	}

	return generate(rc, nodeSecretName, namespace)

}

// generateClientCert generates the Client key and certificate and stores them in a secret.
func (rc *GenerateCert) generateClientCert(ctx context.Context, clientSecretName string, namespace string) error {

	user, userExist := os.LookupEnv("USER_NAME")
	if !userExist {
		user = security.RootUser
	} else {
		clientSecretName = fmt.Sprintf("%s-client-secret", user)
	}

	secret, err := resource.LoadTLSSecret(clientSecretName, resource.NewKubeResource(ctx, rc.client, namespace, kube.DefaultPersister))
	if client.IgnoreNotFound(err) != nil {
		return errors.Wrap(err, "failed to get client secret")
	}

	// inline func used to generate client cert and key
	generate := func(rc *GenerateCert, clientSecretName, namespace string) error {
		logrus.Info("Generating client certificate")

		// Create the user for the certificate
		u := &security.SQLUsername{
			U: user,
		}

		// Create the client certificates
		if err = errors.Wrap(
			security.CreateClientPair(
				rc.CertsDir,
				rc.CAKey,
				keySize,
				rc.ClientCertConfig.Duration,
				overwriteFiles,
				*u,
				generatePKCS8Key),
			"failed to generate client certificate and key"); err != nil {
			return err
		}

		// Load the CA certificate into memory
		ca, err := os.ReadFile(filepath.Join(rc.CertsDir, resource.CaCert))
		if err != nil {
			return errors.Wrap(err, "unable to read ca.crt")
		}

		// Load the client user certificate into memory
		userCertFile := fmt.Sprintf("client.%s.crt", user)
		pemCert, err := os.ReadFile(filepath.Join(rc.CertsDir, userCertFile))
		if err != nil {
			return errors.Wrap(err, fmt.Sprintf("unable to read %s", userCertFile))
		}

		validFrom, validUpto, err := rc.getCertLife(pemCert)
		if err != nil {
			return err

		}

		// Load the client root key into memory
		userKeyFile := fmt.Sprintf("client.%s.key", user)
		pemKey, err := os.ReadFile(filepath.Join(rc.CertsDir, userKeyFile))
		if err != nil {
			return errors.Wrap(err, fmt.Sprintf("unable to read %s", userKeyFile))
		}

		// add certificate info in the secret annotations
		annotations := resource.GetSecretAnnotations(validFrom, validUpto, rc.ClientCertConfig.Duration.String())

		// create and save the TLS certificates into a secret
		secret = resource.CreateTLSSecret(clientSecretName, corev1.SecretTypeTLS,
			resource.NewKubeResource(ctx, rc.client, namespace, kube.DefaultPersister))

		if err = secret.UpdateTLSSecret(pemCert, pemKey, ca, annotations); err != nil {
			return errors.Wrap(err, "failed to update client TLS secret certs")
		}

		logrus.Infof("Generated and saved client key and certificate in secret [%s]", clientSecretName)
		return nil
	}

	// check if the existing is ready to be consumed. If found ready, skip cert generation
	if secret.Ready() && secret.ValidateAnnotations() {

		if rc.RotateClientCert {
			isRequired, reason := secret.IsRotationRequired(rc.ClientCertConfig.Duration, rc.NodeAndClientCronSchedule)
			if isRequired {
				logrus.Infof("Client Certificate: %s", reason)
				return generate(rc, clientSecretName, namespace)
			}
		}

		logrus.Infof("Client secret [%s] is found in ready state, skipping Client cert generation", clientSecretName)
		return nil
	}

	return generate(rc, clientSecretName, namespace)
}

func (rc *GenerateCert) getCASecretName() string {
	return rc.DiscoveryServiceName + "-ca-secret"
}

func (rc *GenerateCert) getNodeSecretName() string {
	return rc.DiscoveryServiceName + "-node-secret"
}

func (rc *GenerateCert) getClientSecretName() string {
	return rc.DiscoveryServiceName + "-client-secret"
}

// getCertLife return the certificate starting and expiration date
func (rc *GenerateCert) getCertLife(pemCert []byte) (validFrom string, validUpto string, err error) {
	cert, err := security.GetCertObj(pemCert)
	if err != nil {
		return validFrom, validUpto, err
	}

	logrus.Debug("getExpirationDate from cert", "Not before:", cert.NotBefore.Format(time.RFC3339), "Not after:", cert.NotAfter.Format(time.RFC3339))
	return cert.NotBefore.Format(time.RFC3339), cert.NotAfter.Format(time.RFC3339), nil
}

func (rc *GenerateCert) UpdateNewCA(ctx context.Context, namespace string) error {
	ca, err := os.ReadFile(filepath.Join(rc.CertsDir, resource.CaCert))
	if err != nil {
		return errors.Wrap(err, "unable to read ca.crt")
	}

	logrus.Info("Updating new CA in node secret")
	nodeSecret, err := resource.LoadTLSSecret(rc.getNodeSecretName(), resource.NewKubeResource(ctx, rc.client, namespace, kube.DefaultPersister))
	if err != nil {
		return errors.Wrap(err, "failed to get node TLS secret")
	}

	if err = nodeSecret.UpdateTLSSecret(nodeSecret.TLSCert(), nodeSecret.TLSPrivateKey(), ca,
		nodeSecret.Secret().Annotations); err != nil {
		return errors.Wrap(err, "failed to update node TLS secret certs")
	}

	logrus.Info("Updated new CA in node secret")

	logrus.Info("Updating new CA in client secret")

	clientSecret, err := resource.LoadTLSSecret(rc.getClientSecretName(), resource.NewKubeResource(ctx, rc.client, namespace, kube.DefaultPersister))
	if err != nil {
		return errors.Wrap(err, "failed to get client secret")
	}

	if err = clientSecret.UpdateTLSSecret(clientSecret.TLSCert(), clientSecret.TLSPrivateKey(), ca,
		clientSecret.Secret().Annotations); err != nil {
		return errors.Wrap(err, "failed to update client TLS secret certs")
	}

	logrus.Info("Updating new CA in client secret")

	if err := kube.RollingUpdate(ctx, rc.client, rc.DiscoveryServiceName, namespace, rc.ReadinessWait, rc.PodUpdateTimeout); err != nil {
		return err
	}
	return nil
}

// LoadCASecret loads the CA secret and write the CA certificate and key to the CA cert directory.
func (rc *GenerateCert) LoadCASecret(ctx context.Context, namespace string) error {
	secret, err := resource.LoadTLSSecret(rc.CaSecret, resource.NewKubeResource(ctx, rc.client, namespace, kube.DefaultPersister))
	if err != nil {
		return errors.Wrap(err, "failed to get CA key secret")
	}

	// check if the secret contains required info
	if !secret.ReadyCA() {
		return errors.Wrap(err, "CA secret doesn't contain the required CA cert/key")
	}

	if err := os.WriteFile(filepath.Join(rc.CertsDir, resource.CaCert), secret.CA(), security.CertFileMode); err != nil {
		return errors.Wrap(err, "failed to write CA cert")
	}

	if err := os.WriteFile(rc.CAKey, secret.CAKey(), security.KeyFileMode); err != nil {
		return errors.Wrap(err, "failed to write CA key")
	}

	return nil
}
