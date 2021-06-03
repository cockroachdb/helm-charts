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
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"io/ioutil"
	"path/filepath"
	"time"

	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/cockroachdb/helm-charts/pkg/kube"
	"github.com/cockroachdb/helm-charts/pkg/resource"
	"github.com/cockroachdb/helm-charts/pkg/security"
	"github.com/cockroachdb/helm-charts/pkg/utils"
)

const defaultKeySize = 2048

// Options settable via command-line flags. See below for defaults.
var keySize int
var allowCAKeyReuse bool
var overwriteFiles bool
var generatePKCS8Key bool

func initPreFlagsCertDefaults() {
	keySize = defaultKeySize
	allowCAKeyReuse = false
	overwriteFiles = false
	generatePKCS8Key = false
}

// generateCert is the structure containing all the certificate relate info
type generateCert struct {
	client               client.Client
	CertsDir             string
	CaSecret             string
	CAKey                string
	PublicServiceName    string
	DiscoveryServiceName string
	CaCertConfig         *certConfig
	NodeCertConfig       *certConfig
	ClientCertConfig     *certConfig
}

type certConfig struct {
	Duration     time.Duration
	ExpiryWindow time.Duration
}

func (c *certConfig) SetConfig(duration, expiryW string) error {

	dur, err := time.ParseDuration(duration)
	if err != nil {
		return fmt.Errorf("failed to parse duration %s", err.Error())
	}
	c.Duration = dur

	expW, err := time.ParseDuration(expiryW)
	if err != nil {
		return fmt.Errorf("failed to expiryWindow %s", err.Error())
	}
	c.ExpiryWindow = expW

	return nil
}

func NewGenerateCert(cl client.Client) generateCert {
	return generateCert{
		client:           cl,
		CaCertConfig:     &certConfig{},
		NodeCertConfig:   &certConfig{},
		ClientCertConfig: &certConfig{},
	}
}

const (
	caCrtKey = "ca.crt"
	caKey    = "ca.key"
)

// TODO: Handle situation when job is retried and when some secretes are already created in the first run

// Do func generates the various certificates required and then stores the certificates in secrets.
func (rc *generateCert) Do(ctx context.Context, namespace string) error {

	// create the various temporary directories to store the certificates in
	// the directors will delete when the code is completed.
	logrus.SetLevel(logrus.InfoLevel)

	certsDir, cleanup := util.CreateTempDir("certsDir")
	defer cleanup()
	rc.CertsDir = certsDir

	caDir, cleanupCADir := util.CreateTempDir("caDir")
	defer cleanupCADir()
	rc.CAKey = filepath.Join(caDir, "ca.key")
	// generate the base CA cert and key
	if err := rc.generateCA(ctx, rc.GetCASecretName(), namespace); err != nil {
		msg := "error generating CA"
		logrus.Error(err, msg)
		return errors.Wrap(err, msg)
	}

	// generate the node certificate for the database to use
	if _, err := rc.generateNodeCert(ctx, rc.GetNodeSecretName(), namespace); err != nil {
		msg := "error generating Node Certificate"
		logrus.Error(err, msg)
		return errors.Wrap(err, msg)
	}

	// generate the client certificates for the database to use
	if err := rc.generateClientCert(ctx, rc.GetClientSecretName(), namespace); err != nil {
		msg := "error generating Client Certificate"
		logrus.Error(err, msg)
		return errors.Wrap(err, msg)
	}

	return nil
}

func (rc *generateCert) generateCA(ctx context.Context, CASecretName string, namespace string) error {

	// if CA secret is given by user then validate it and use that
	if rc.CaSecret != "" {
		logrus.Infof("skipping CA cert generation, using user provided CA secret [%s]", rc.CaSecret)

		secret, err := resource.LoadTLSSecret(CASecretName,
			resource.NewKubeResource(ctx, rc.client, namespace, kube.DefaultPersister))
		if err != nil {
			return errors.Wrap(err, "failed to get CA key secret")
		}

		// check if the secret contains required info
		if !secret.ReadyCA() {
			return errors.Wrap(err, "CA secret doesn't contain the required CA cert/key")
		}

		if err := ioutil.WriteFile(filepath.Join(rc.CertsDir, caCrtKey), secret.CA(), 0600); err != nil {
			return errors.Wrap(err, "failed to write CA cert")
		}

		if err := ioutil.WriteFile(rc.CAKey, secret.CAKey(), 0644); err != nil {
			return errors.Wrap(err, "failed to write CA key")
		}

		return nil
	}

	logrus.Info("generating CA")

	err := errors.Wrap(
		security.CreateCAPair(
			rc.CertsDir,
			rc.CAKey,
			keySize,
			rc.CaCertConfig.Duration,
			allowCAKeyReuse,
			overwriteFiles),
		"failed to generate CA cert and key")
	if err != nil {
		return err
	}
	// Read the ca key into memory
	cakey, err := ioutil.ReadFile(rc.CAKey)
	if err != nil {
		return errors.Wrap(err, "unable to read ca.key")
	}

	// create and save the TLS certificates into a secret
	secret := resource.CreateTLSSecret(CASecretName, corev1.SecretTypeOpaque,
		resource.NewKubeResource(ctx, rc.client, namespace, kube.DefaultPersister))

	if err = secret.UpdateCAKey(cakey); err != nil {
		return errors.Wrap(err, "failed to update ca key secret ")
	}

	logrus.Infof("generated and saved ca key in secret [%s]", CASecretName)
	return nil
}

func (rc *generateCert) generateNodeCert(ctx context.Context, nodeSecretName string, namespace string) (string, error) {
	logrus.Info("generating node certificate")

	// load the secret.  If it exists don't update the cert
	secret, err := resource.LoadTLSSecret(nodeSecretName,
		resource.NewKubeResource(ctx, rc.client, namespace, kube.DefaultPersister))
	if client.IgnoreNotFound(err) != nil {
		return "", errors.Wrap(err, "failed to get node TLS secret")
	}

	// if the secret is ready then don't update the secret
	if secret.Ready() {
		logrus.Info("not updating node certificate as it exists")
		return rc.getCertificateExpirationDate(ctx, secret.Key())
	}

	//  TODO: take domain as env
	domain := "svc.cluster.local"
	// hosts are the various DNS names and IP address that have to exist in the Node certificates
	// for the database to function
	hosts := []string{
		"localhost",
		"127.0.0.1",
		rc.PublicServiceName,
		fmt.Sprintf("%s.%s", rc.PublicServiceName, namespace),
		fmt.Sprintf("%s.%s.%s", rc.PublicServiceName, namespace, domain),
		fmt.Sprintf("*.%s", rc.DiscoveryServiceName),
		fmt.Sprintf("*.%s.%s", rc.DiscoveryServiceName, namespace),
		fmt.Sprintf("*.%s.%s.%s", rc.DiscoveryServiceName, namespace, domain),
	}

	// create the Node Pair certificates
	err = errors.Wrap(
		security.CreateNodePair(
			rc.CertsDir,
			rc.CAKey,
			keySize,
			rc.NodeCertConfig.Duration,
			overwriteFiles,
			hosts),
		"failed to generate node certificate and key")

	if err != nil {
		return "", err
	}

	// Read the node certificates into memory
	ca, err := ioutil.ReadFile(filepath.Join(rc.CertsDir, "ca.crt"))
	if err != nil {
		return "", errors.Wrap(err, "unable to read ca.crt")
	}

	pemCert, err := ioutil.ReadFile(filepath.Join(rc.CertsDir, "node.crt"))
	if err != nil {
		return "", errors.Wrap(err, "unable to read node.crt")
	}

	pemKey, err := ioutil.ReadFile(filepath.Join(rc.CertsDir, "node.key"))
	if err != nil {
		return "", errors.Wrap(err, "unable to ready node.key")
	}

	// create and save the TLS certificates into a secret
	secret = resource.CreateTLSSecret(nodeSecretName, corev1.SecretTypeTLS,
		resource.NewKubeResource(ctx, rc.client, namespace, kube.DefaultPersister))

	if err = secret.UpdateCertAndKeyAndCA(pemCert, pemKey, ca); err != nil {
		return "", errors.Wrap(err, "failed to update node TLS secret certs")
	}

	logrus.Infof("generated and saved node certificate and key in secret [%s]", nodeSecretName)

	// TODO: ADD getCertificateExpirationDate here when required
	return "", nil
}

func (rc *generateCert) GetCASecretName() string {
	return rc.PublicServiceName + "-ca-secret"
}

func (rc *generateCert) GetNodeSecretName() string {
	return rc.PublicServiceName + "-node-secret"
}

func (rc *generateCert) GetClientSecretName() string {
	return rc.PublicServiceName + "-client-secret"
}

func (rc *generateCert) generateClientCert(ctx context.Context, clientSecretName string, namespace string) error {
	logrus.Info("generating client certificate")

	// load the secret.  If it exists don't update the cert
	secret, err := resource.LoadTLSSecret(clientSecretName,
		resource.NewKubeResource(ctx, rc.client, namespace, kube.DefaultPersister))
	if client.IgnoreNotFound(err) != nil {
		return errors.Wrap(err, "failed to get client TLS secret")
	}

	// if the secret is ready then don't update the secret
	if secret.Ready() {
		logrus.Info("not updating client certificate")
		return nil
	}

	// Create the user for the certificate
	u := &security.SQLUsername{
		U: "root",
	}

	// Create the client certificates
	err = errors.Wrap(
		security.CreateClientPair(
			rc.CertsDir,
			rc.CAKey,
			keySize,
			rc.ClientCertConfig.Duration,
			overwriteFiles,
			*u,
			generatePKCS8Key),
		"failed to generate client certificate and key")
	if err != nil {
		return err
	}

	// Load the certificates into memory
	ca, err := ioutil.ReadFile(filepath.Join(rc.CertsDir, "ca.crt"))
	if err != nil {
		return errors.Wrap(err, "unable to read ca.crt")
	}

	pemCert, err := ioutil.ReadFile(filepath.Join(rc.CertsDir, "client.root.crt"))
	if err != nil {
		return errors.Wrap(err, "unable to read client.root.crt")
	}

	pemKey, err := ioutil.ReadFile(filepath.Join(rc.CertsDir, "client.root.key"))
	if err != nil {
		return errors.Wrap(err, "unable to read client.root.key")
	}

	// create and save the TLS certificates into a secret
	secret = resource.CreateTLSSecret(clientSecretName, corev1.SecretTypeTLS, resource.NewKubeResource(ctx, rc.client, namespace, kube.DefaultPersister))

	if err = secret.UpdateCertAndKeyAndCA(pemCert, pemKey, ca); err != nil {
		return errors.Wrap(err, "failed to update client TLS secret certs")
	}

	logrus.Infof("generated and saved client certificate and key in secret [%s]", clientSecretName)
	return nil
}

func (rc *generateCert) getCertificateExpirationDate(ctx context.Context, pemCert []byte) (string, error) {
	logrus.Info("getExpirationDate from cert")
	block, _ := pem.Decode(pemCert)
	if block == nil {
		return "", errors.New("failed to decode certificate")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return "", errors.Wrap(err, "failed to parse certificate")
	}

	logrus.Info("getExpirationDate from cert", "Not before:", cert.NotBefore.Format(time.RFC3339), "Not after:", cert.NotAfter.Format(time.RFC3339))
	return cert.NotAfter.Format(time.RFC3339), nil
}
