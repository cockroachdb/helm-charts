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

package security

import (
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"os/exec"
	"time"
)

// Instead of using custom code to generate the certificates this code executes the crdb binary which then generates the certificates

// SQLUsername is used to define the username created in the client certificate
type SQLUsername struct {
	U string
}

const (
	KeyFileMode  = 0600
	CertFileMode = 0644
)

// PemUsage indicates the purpose of a given certificate.
type PemUsage uint32

const (
	_ PemUsage = iota
	// CAPem describes the main CA certificate.
	CAPem
	// TenantClientCAPem describes the CA certificate used to broker authN/Z for SQL
	// tenants wishing to access the KV layer.
	TenantClientCAPem
	// ClientCAPem describes the CA certificate used to verify client certificates.
	ClientCAPem
	// UICAPem describes the CA certificate used to verify the Admin UI server certificate.
	UICAPem
	// NodePem describes the server certificate for the node, possibly a combined server/client
	// certificate for user Node if a separate 'client.node.crt' is not present.
	NodePem
	// UIPem describes the server certificate for the admin UI.
	UIPem
	// ClientPem describes a client certificate.
	ClientPem
	// TenantClientPem describes a SQL tenant client certificate.
	TenantClientPem
)

// The following constants are used to run the crdb binary
const (
	CR            string = "cockroach"
	CERT          string = "cert"
	CREATE_CA     string = "create-ca"
	CREATE_NODE   string = "create-node"
	CREATE_CLIENT string = "create-client"

	CERTS_DIR string = "--certs-dir=%s"
	CA_KEY    string = "--ca-key=%s"
	Life_Time string = "--lifetime=%s"
)

// CreateCAPair creates a general CA certificate and associated key.
func CreateCAPair(
	certsDir, caKeyPath string,
	keySize int,
	lifetime time.Duration,
	allowKeyReuse bool,
	overwrite bool,
) error {
	return createCACertAndKey(certsDir, caKeyPath, CAPem, keySize, lifetime, allowKeyReuse, overwrite)
}

// createCACertAndKey creates a CA key and a CA certificate.
// If the certs directory does not exist, it is created.
// If the key does not exist, it is created.
// The certificate is written to the certs directory. If the file already exists,
// we append the original certificates to the new certificate.
//
// The filename of the certificate file must be specified.
// It should be one of:
// - ca.crt: the general CA certificate
// - ca-client.crt: the CA certificate to verify client certificates
func createCACertAndKey(certsDir, caKeyPath string, caType PemUsage, keySize int, lifetime time.Duration, allowKeyReuse bool, overwrite bool) error {
	if len(caKeyPath) == 0 {
		return errors.New("the path to the CA key is required")
	}
	if len(certsDir) == 0 {
		return errors.New("the path to the certs directory is required")
	}
	if caType != CAPem {
		return fmt.Errorf("caType argument to createCACertAndKey must be CAPem (%d), got: %d", CAPem, caType)
	}

	certsDirParam := fmt.Sprintf(CERTS_DIR, certsDir)
	caKeyParam := fmt.Sprintf(CA_KEY, caKeyPath)
	lifetimeParam := fmt.Sprintf(Life_Time, lifetime.String())

	// run the crdb binary to generate the CA
	execCmd(CREATE_CA, certsDirParam, caKeyParam, lifetimeParam)

	return nil
}

// CreateNodePair creates a node key and certificate.
// The CA cert and key must load properly. If multiple certificates
// exist in the CA cert, the first one is used.
func CreateNodePair(certsDir, caKeyPath string, keySize int, lifetime time.Duration, overwrite bool, hosts []string) error {
	if len(caKeyPath) == 0 {
		return errors.New("the path to the CA key is required")
	}
	if len(certsDir) == 0 {
		return errors.New("the path to the certs directory is required")
	}

	certsDirParam := fmt.Sprintf(CERTS_DIR, certsDir)
	caKeyParam := fmt.Sprintf(CA_KEY, caKeyPath)
	lifetimeParam := fmt.Sprintf(Life_Time, lifetime.String())
	args := append(hosts, certsDirParam, caKeyParam, lifetimeParam)
	args = append([]string{CREATE_NODE}, args...)

	// run the crdb binary to generate the node certificates
	execCmd(args...)

	return nil
}

// CreateClientPair creates a node key and certificate.
// The CA cert and key must load properly. If multiple certificates
// exist in the CA cert, the first one is used.
// If a client CA exists, this is used instead.
// If wantPKCS8Key is true, the private key in PKCS#8 encoding is written as well.
func CreateClientPair(certsDir, caKeyPath string, keySize int, lifetime time.Duration, overwrite bool,
	user SQLUsername, wantPKCS8Key bool) error {

	if len(caKeyPath) == 0 {
		return errors.New("the path to the CA key is required")
	}

	if len(certsDir) == 0 {
		return errors.New("the path to the certs directory is required")
	}

	certsDirParam := fmt.Sprintf(CERTS_DIR, certsDir)
	caKeyParam := fmt.Sprintf(CA_KEY, caKeyPath)
	lifetimeParam := fmt.Sprintf(Life_Time, lifetime.String())

	// TODO pks options do we need them?
	// run the crdb binary to generate the node certificates
	execCmd(CREATE_CLIENT, user.U, certsDirParam, caKeyParam, lifetimeParam)

	return nil
}

// execCmd is a simple wrapper our exec that allows us to run a command
func execCmd(args ...string) {
	args = append([]string{CERT}, args...)
	cmd := exec.Command(CR, args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		// TODO should we panic here or throw an error?
		// a panic will restart the pod
		panic(fmt.Sprintf("error: %s: %s\nout: %s\n", args, err, out))
	}
}

func GetCertObj(pemCert []byte) (*x509.Certificate, error) {
	block, _ := pem.Decode(pemCert)
	if block == nil {
		return nil, errors.New("failed to decode certificate")
	}

	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, err
	}

	return cert, nil
}
