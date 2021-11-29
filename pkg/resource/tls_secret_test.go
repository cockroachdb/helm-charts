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

package resource_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/cockroachdb/helm-charts/pkg/kube"
	"github.com/cockroachdb/helm-charts/pkg/resource"
	"github.com/cockroachdb/helm-charts/pkg/testutils"
)

func TestLoadTLSSecret(t *testing.T) {
	ctx := context.TODO()
	scheme := testutils.InitScheme(t)
	fakeClient := testutils.NewFakeClient(scheme)
	r := resource.NewKubeResource(ctx, fakeClient, "test-namespace", kube.DefaultPersister)

	_, err := resource.LoadTLSSecret("non-existing", r)
	assert.True(t, apierrors.IsNotFound(err))
}

func TestTLSSecretReady(t *testing.T) {
	ctx := context.TODO()
	scheme := testutils.InitScheme(t)
	name := "test-secret"
	namespace := "test-namespace"

	tests := []struct {
		name     string
		secret   client.Object
		expected bool
	}{
		{
			name: "secret missing required fields",
			secret: secretObj(
				name,
				namespace,
				map[string][]byte{"someKey": {}},
				nil),
			expected: false,
		},
		{
			name: "secret has all required fields",
			secret: secretObj(
				name,
				namespace,
				map[string][]byte{"ca.crt": {}, "tls.crt": {}, "tls.key": {}},
				nil),
			expected: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fakeClient := testutils.NewFakeClient(scheme, tt.secret)
			r := resource.NewKubeResource(ctx, fakeClient, namespace, kube.DefaultPersister)

			actual, err := resource.LoadTLSSecret(name, r)
			require.NoError(t, err)
			assert.Equal(t, tt.expected, actual.Ready())

		})
	}
}

func TestCASecretReady(t *testing.T) {
	ctx := context.TODO()
	scheme := testutils.InitScheme(t)
	name := "test-secret"
	namespace := "test-namespace"

	tests := []struct {
		name     string
		secret   client.Object
		expected bool
	}{
		{
			name: "secret missing required fields",
			secret: secretObj(
				name,
				namespace,
				map[string][]byte{"someKey": {}},
				nil),
			expected: false,
		},
		{
			name: "secret has all required fields",
			secret: secretObj(
				name,
				namespace,
				map[string][]byte{"ca.crt": {}, "ca.key": {}},
				nil),
			expected: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fakeClient := testutils.NewFakeClient(scheme, tt.secret)
			r := resource.NewKubeResource(ctx, fakeClient, namespace, kube.DefaultPersister)

			actual, err := resource.LoadTLSSecret(name, r)
			require.NoError(t, err)
			assert.Equal(t, tt.expected, actual.ReadyCA())

		})
	}
}

func TestValidateAnnotations(t *testing.T) {
	ctx := context.TODO()
	scheme := testutils.InitScheme(t)
	name := "test-secret"
	namespace := "test-namespace"

	tests := []struct {
		name     string
		secret   client.Object
		expected bool
	}{
		{
			name: "secret missing required annotations",
			secret: secretObj(
				name,
				namespace,
				nil,
				map[string]string{"someKey": "somevalue"}),
			expected: false,
		},
		{
			name: "secret missing one of the required annotations",
			secret: secretObj(
				name,
				namespace,
				nil,
				map[string]string{
					resource.CertValidUpto: "validUpto",
					resource.CertValidFrom: "validFrom",
					resource.CertDuration:  "duration",
				}),
			expected: false,
		},
		{
			name: "secret having all the required annotations",
			secret: secretObj(
				name,
				namespace,
				nil,
				map[string]string{
					resource.CertValidUpto:  "validUpto",
					resource.CertValidFrom:  "validFrom",
					resource.CertDuration:   "duration",
					resource.SecretDataHash: "123",
				}),
			expected: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fakeClient := testutils.NewFakeClient(scheme, tt.secret)
			r := resource.NewKubeResource(ctx, fakeClient, namespace, kube.DefaultPersister)

			actual, err := resource.LoadTLSSecret(name, r)
			require.NoError(t, err)
			assert.Equal(t, tt.expected, actual.ValidateAnnotations())

		})
	}
}

func TestUpdateCASecret(t *testing.T) {
	ctx := context.TODO()
	scheme := testutils.InitScheme(t)
	name := "test-secret"
	namespace := "test-namespace"

	fakeClient := testutils.NewFakeClient(scheme)
	r := resource.NewKubeResource(ctx, fakeClient, namespace, kube.DefaultPersister)
	secret := resource.CreateTLSSecret(name, corev1.SecretTypeOpaque, r)

	annotations := resource.GetSecretAnnotations("validFrom", "validUpto", "duration")
	data := map[string][]byte{
		"ca.crt": []byte("c2FtcGxlIGNlcnQ="), // sample cert
		"ca.key": []byte("c2FtcGxlIGtleQ=="), // sample key
	}

	err := secret.UpdateCASecret(data["ca.key"], data["ca.crt"], annotations)
	require.NoError(t, err)

	secret, err = resource.LoadTLSSecret(name, r)
	require.NoError(t, err)

	assert.Equal(t, data, secret.Secret().Data)
	assert.Equal(t, annotations, secret.Secret().GetAnnotations())
}

func TestUpdateTLSSecret(t *testing.T) {
	ctx := context.TODO()
	scheme := testutils.InitScheme(t)
	name := "test-secret"
	namespace := "test-namespace"

	fakeClient := testutils.NewFakeClient(scheme)
	r := resource.NewKubeResource(ctx, fakeClient, namespace, kube.DefaultPersister)
	secret := resource.CreateTLSSecret(name, corev1.SecretTypeOpaque, r)

	annotations := resource.GetSecretAnnotations("validFrom", "validUpto", "duration")
	data := map[string][]byte{
		"ca.crt":  []byte("c2FtcGxlIGNlcnQ="), // sample cert
		"tls.key": []byte("c2FtcGxlIGtleQ=="), // sample key
		"tls.crt": []byte("c2FtcGxlIGNlcnQ="), // sample key
	}

	err := secret.UpdateTLSSecret(data["tls.crt"], data["tls.key"], data["ca.crt"], annotations)
	require.NoError(t, err)

	secret, err = resource.LoadTLSSecret(name, r)
	require.NoError(t, err)

	assert.Equal(t, data, secret.Secret().Data)
	assert.Equal(t, annotations, secret.Secret().GetAnnotations())
}

func TestIsRotationRequired(t *testing.T) {
	ctx := context.TODO()
	scheme := testutils.InitScheme(t)
	name := "test-secret"
	namespace := "test-namespace"

	tests := []struct {
		name     string
		secret   client.Object
		duration time.Duration
		cronStr  string
		rotate   bool
		Reason   string
	}{
		{
			name: "secret having some modified fields (data-hash is different)",
			secret: secretObj(
				name,
				namespace,
				map[string][]byte{"ca.crt": {}, "tls.crt": {}, "tls.key": {}},
				map[string]string{
					resource.CertValidUpto:  "2021-08-06T04:15:35Z",
					resource.CertValidFrom:  "2021-07-06T04:15:35Z",
					resource.CertDuration:   "720h0m0s",
					resource.SecretDataHash: "123",
				}),
			rotate: true,
			Reason: "Secret data altered, rotating certificate",
		},
		{
			name: "secret having different certificate duration then current duration (duration mismatch)",
			secret: secretObj(
				name,
				namespace,
				map[string][]byte{"ca.crt": {}, "tls.crt": {}, "tls.key": {}},
				map[string]string{
					resource.CertValidUpto:  "2021-08-06T04:15:35Z",
					resource.CertValidFrom:  "2021-07-06T04:15:35Z",
					resource.CertDuration:   "720h0m0s",
					resource.SecretDataHash: "6889078329698146222",
				}),
			duration: 750 * time.Hour,
			rotate:   true,
			Reason:   "Certificate duration mismatch, rotating certificate",
		},

		{
			name: "secret having invalid expiry date in annotations",
			secret: secretObj(
				name,
				namespace,
				map[string][]byte{"ca.crt": {}, "tls.crt": {}, "tls.key": {}},
				map[string]string{
					resource.CertValidUpto:  "invalid-date",
					resource.CertValidFrom:  "2021-07-06T04:15:35Z",
					resource.CertDuration:   "720h0m0s",
					resource.SecretDataHash: "6889078329698146222",
				}),
			duration: 720 * time.Hour,
			rotate:   true,
			Reason:   "Failed to verify expiry date, rotating certificate",
		},

		{
			name: "secret having expiry not before the next cron",
			secret: secretObj(
				name,
				namespace,
				map[string][]byte{"ca.crt": {}, "tls.crt": {}, "tls.key": {}},
				map[string]string{
					resource.CertValidUpto:  time.Now().Add(time.Hour * 720).Format(time.RFC3339),
					resource.CertValidFrom:  time.Now().Format(time.RFC3339),
					resource.CertDuration:   "720h0m0s",
					resource.SecretDataHash: "6889078329698146222",
				}),
			duration: 720 * time.Hour,
			cronStr:  "@weekly",
			rotate:   false,
		},

		{
			name: "When invalid cron string is passed",
			secret: secretObj(
				name,
				namespace,
				map[string][]byte{"ca.crt": {}, "tls.crt": {}, "tls.key": {}},
				map[string]string{
					resource.CertValidUpto:  "2021-08-06T04:15:35Z",
					resource.CertValidFrom:  "2021-07-06T04:15:35Z",
					resource.CertDuration:   "720h0m0s",
					resource.SecretDataHash: "6889078329698146222",
				}),
			duration: 720 * time.Hour,
			cronStr:  "@invalid",
			rotate:   true,
			Reason:   "Failed to verify expiry date due to invalid cron, rotating certificate",
		},

		{
			name: "secret having expiry before the next cron",
			secret: secretObj(
				name,
				namespace,
				map[string][]byte{"ca.crt": {}, "tls.crt": {}, "tls.key": {}},
				map[string]string{
					resource.CertValidUpto:  "2021-08-06T04:15:35Z",
					resource.CertValidFrom:  "2021-07-06T04:15:35Z",
					resource.CertDuration:   "720h0m0s",
					resource.SecretDataHash: "6889078329698146222",
				}),
			duration: 720 * time.Hour,
			cronStr:  "@yearly",
			rotate:   true,
			Reason:   "Certificate about to expire, rotating certificate",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fakeClient := testutils.NewFakeClient(scheme, tt.secret)
			r := resource.NewKubeResource(ctx, fakeClient, namespace, kube.DefaultPersister)

			actual, err := resource.LoadTLSSecret(name, r)
			require.NoError(t, err)
			isRequired, reason := actual.IsRotationRequired(tt.duration, tt.cronStr)

			assert.Equal(t, tt.rotate, isRequired)
			assert.Equal(t, tt.Reason, reason)

		})
	}
}

func secretObj(name, namespace string, data map[string][]byte, annotations map[string]string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   namespace,
			Annotations: annotations,
		},
		Data: data,
	}
}

func TestCertExpired(t *testing.T) {
	type args struct {
		now       time.Time
		cronStr   string
		validUpto string
	}
	tests := []struct {
		name   string
		secret *resource.TLSSecret
		args   args
		rotate bool
		reason string
	}{
		{
			name:   "not-expiring-soon",
			secret: &resource.TLSSecret{},
			args: args{
				now:       time.Date(2021, time.November, 10, 0, 0, 0, 0, time.UTC),
				cronStr:   "0 0 */23 * *",
				validUpto: time.Date(2021, time.December, 02, 0, 0, 0, 0, time.UTC).Format(time.RFC3339),
			},
			rotate: false,
			reason: "",
		},
		{
			name:   "invalid-expiration",
			secret: &resource.TLSSecret{},
			args: args{
				now:       time.Now(),
				cronStr:   "0 0 */23 * *",
				validUpto: "invalid-expiration-date",
			},
			rotate: true,
			reason: "Failed to verify expiry date, rotating certificate",
		},
		{
			name:   "expire-before-next-run",
			secret: &resource.TLSSecret{},
			args: args{
				now:       time.Date(2021, time.November, 10, 0, 0, 0, 0, time.UTC),
				cronStr:   "0 0 */23 * *",
				validUpto: time.Date(2021, time.November, 22, 0, 0, 0, 0, time.UTC).Format(time.RFC3339),
			},
			rotate: true,
			reason: "Certificate about to expire, rotating certificate",
		},
		{
			name:   "expire-before-next-to-next-run",
			secret: &resource.TLSSecret{},
			args: args{
				now:       time.Date(2021, time.November, 10, 0, 0, 0, 0, time.UTC),
				cronStr:   "0 0 */23 * *",
				validUpto: time.Date(2021, time.November, 27, 0, 0, 0, 0, time.UTC).Format(time.RFC3339),
			},
			rotate: true,
			reason: "Certificate about to expire, rotating certificate",
		},
		{
			name:   "expire-equals-next-to-next-run",
			secret: &resource.TLSSecret{},
			args: args{
				now:       time.Date(2021, time.November, 10, 0, 0, 0, 0, time.UTC),
				cronStr:   "0 0 */23 * *",
				validUpto: time.Date(2021, time.December, 1, 0, 0, 0, 0, time.UTC).Format(time.RFC3339),
			},
			rotate: true,
			reason: "Certificate about to expire, rotating certificate",
		},
		{
			name:   "not-expiring-before-next-to-next-run",
			secret: &resource.TLSSecret{},
			args: args{
				now:       time.Date(2021, time.November, 10, 0, 0, 0, 0, time.UTC),
				cronStr:   "0 0 */23 * *",
				validUpto: time.Date(2021, time.December, 2, 0, 0, 0, 0, time.UTC).Format(time.RFC3339),
			},
			rotate: false,
			reason: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			needsRotation, msg := tt.secret.CertExpired(tt.args.now, tt.args.cronStr, tt.args.validUpto)
			assert.Equal(t, tt.rotate, needsRotation)
			assert.Equal(t, tt.reason, msg)
		})
	}
}
