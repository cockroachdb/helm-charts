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

package resource

import (
	"fmt"
	"time"

	"github.com/mitchellh/hashstructure/v2"
	"github.com/robfig/cron"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	CaCert         = "ca.crt"
	CaKey          = "ca.key"
	CertValidFrom  = "certificate-valid-from"
	CertValidUpto  = "certificate-valid-upto"
	CertDuration   = "certificate-duration"
	SecretDataHash = "secret-data-hash"
)

// CreateTLSSecret returns a TLSSecret struct that is used to store the certs via secrets.
func CreateTLSSecret(name string, secretType corev1.SecretType, r Resource) *TLSSecret {

	s := &TLSSecret{
		Resource: r,
		secret: &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name: name,
			},
			Type: secretType,
		},
	}

	return s
}

// LoadTLSSecret fetches secret from the API server
func LoadTLSSecret(name string, r Resource) (*TLSSecret, error) {
	s := &TLSSecret{
		Resource: r,
		secret: &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name: name,
			},
		},
	}

	err := s.Fetch(s.secret)

	s.secret = s.secret.DeepCopy()

	if s.secret.Data == nil {
		s.secret.Data = map[string][]byte{}
	}

	return s, err
}

type TLSSecret struct {
	Resource

	secret *corev1.Secret
}

// ReadyCA checks if the CA secret contains required data
func (s *TLSSecret) ReadyCA() bool {
	data := s.secret.Data

	if _, ok := data[CaKey]; !ok {
		return false
	}

	if _, ok := data[CaCert]; !ok {
		return false
	}

	return true
}

// ValidateAnnotations validates if all the required annotations are present
func (s *TLSSecret) ValidateAnnotations() bool {
	annotations := s.secret.Annotations

	if annotations == nil {
		return false
	}

	if _, ok := annotations[CertValidFrom]; !ok {
		return false
	}

	if _, ok := annotations[CertValidUpto]; !ok {
		return false
	}

	if _, ok := annotations[CertDuration]; !ok {
		return false
	}

	if _, ok := annotations[SecretDataHash]; !ok {
		return false
	}

	return true
}

// IsRotationRequired validates if all the required annotations are present
func (s *TLSSecret) IsRotationRequired(duration time.Duration, cronStr string) (bool, string) {
	annotations := s.secret.Annotations

	// validate secret data hash
	hash, err := hashstructure.Hash(s.secret.Data, hashstructure.FormatV2, nil)
	if err != nil {
		return true, "Failed to verify secret data hash, creating new certificate"
	}

	currentHash := fmt.Sprintf("%d", hash)
	existingHash := annotations[SecretDataHash]

	if currentHash != existingHash {
		return true, "Secret data altered, rotating certificate"
	}

	// validate duration
	existingDuration := annotations[CertDuration]
	if duration.String() != existingDuration {
		return true, "Certificate duration mismatch, rotating certificate"
	}

	// validate expiry. If expiry is before the next cron, then rotate the certificate
	validUpto := annotations[CertValidUpto]

	return s.CertExpired(time.Now(), cronStr, validUpto)
}

func (s *TLSSecret) CertExpired(now time.Time, cronStr string, validUpto string) (bool, string) {
	expiryTime, err := time.Parse(time.RFC3339, validUpto)
	if err != nil {
		return true, "Failed to verify expiry date, rotating certificate"
	}

	cronSchedule, err := cron.ParseStandard(cronStr)
	if err != nil {
		return true, "Failed to verify expiry date due to invalid cron, rotating certificate"
	}

	nextRun := cronSchedule.Next(now)

	// if cert is expiring before next run or within (next run + 1 hour)
	if expiryTime.Sub(nextRun) < 1*time.Hour {
		return true, "Certificate about to expire, rotating certificate"
	}

	return false, ""
}

// Ready checks if secret contains required data
func (s *TLSSecret) Ready() bool {
	data := s.secret.Data
	if _, ok := data[CaCert]; !ok {
		return false
	}

	if _, ok := data[corev1.TLSCertKey]; !ok {
		return false
	}

	if _, ok := data[corev1.TLSPrivateKeyKey]; !ok {
		return false
	}

	return true
}

// UpdateTLSSecret updates three different certificates at the same time.
// It save the TLSCert, the CA, and the TLSPrivateKey in a secret.
func (s *TLSSecret) UpdateTLSSecret(cert, key, ca []byte, annotations map[string]string) error {
	newCert, newCA := append([]byte{}, cert...), append([]byte{}, ca...)
	newKey := append([]byte{}, key...)
	data := map[string][]byte{corev1.TLSCertKey: newCert, CaCert: newCA, corev1.TLSPrivateKeyKey: newKey}

	// create hash of the new data
	hash, err := hashstructure.Hash(data, hashstructure.FormatV2, nil)
	if err != nil {
		return err
	}

	annotations[SecretDataHash] = fmt.Sprintf("%d", hash)

	_, err = s.Persist(s.secret, func() error {
		s.secret.Data = data
		s.secret.Annotations = annotations

		return nil
	})

	return err
}

// UpdateCASecret updates CA key and CA Cert
func (s *TLSSecret) UpdateCASecret(cakey []byte, caCert []byte, annotations map[string]string) error {
	newCAKey := append([]byte{}, cakey...)
	newCACert := append([]byte{}, caCert...)
	data := map[string][]byte{CaKey: newCAKey, CaCert: newCACert}

	// create hash of the new data
	hash, err := hashstructure.Hash(data, hashstructure.FormatV2, nil)
	if err != nil {
		return err
	}

	annotations[SecretDataHash] = fmt.Sprintf("%d", hash)

	_, err = s.Persist(s.secret, func() error {
		s.secret.Data = data
		s.secret.Annotations = annotations

		return nil
	})

	return err
}

// Secret returns the Secret object
func (s *TLSSecret) Secret() *corev1.Secret {
	return s.secret
}

func (s *TLSSecret) CA() []byte {
	return s.secret.Data[CaCert]
}
func (s *TLSSecret) CAKey() []byte {
	return s.secret.Data[CaKey]
}

func (s *TLSSecret) TLSCert() []byte {
	return s.secret.Data[corev1.TLSCertKey]
}

func (s *TLSSecret) TLSPrivateKey() []byte {
	return s.secret.Data[corev1.TLSPrivateKeyKey]
}

func GetSecretAnnotations(validFrom, validUpto, duration string) map[string]string {
	return map[string]string{
		CertValidUpto: validUpto,
		CertValidFrom: validFrom,
		CertDuration:  duration,
	}
}
