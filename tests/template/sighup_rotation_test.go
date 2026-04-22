package template

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/gruntwork-io/terratest/modules/helm"
	"github.com/gruntwork-io/terratest/modules/k8s"
	"github.com/gruntwork-io/terratest/modules/random"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
)

var (
	sighupHelmChartPath = func() string {
		path, err := filepath.Abs("../../cockroachdb")
		if err != nil {
			panic(err)
		}
		return path
	}()
	sighupReleaseName   = "sighup-test"
	sighupNamespaceName = "sighup-" + strings.ToLower(random.UniqueId())
)

// TestSighupRotationValidation tests that enableSighupRotation cannot be enabled with self-signed certificates
func TestSighupRotationValidation(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name       string
		values     map[string]string
		expectErr  bool
		errMessage string
	}{
		{
			name: "SIGHUP rotation with self-signed certs should fail",
			values: map[string]string{
				"tls.enabled":                  "true",
				"tls.enableSighupRotation":     "true",
				"tls.certs.selfSigner.enabled": "true",
				"operator.enabled":             "false",
			},
			expectErr:  true,
			errMessage: "Can not enable SIGHUP rotation with self-signed certificates",
		},
		{
			name: "SIGHUP rotation with cert-manager should succeed",
			values: map[string]string{
				"tls.enabled":                  "true",
				"tls.enableSighupRotation":     "true",
				"tls.certs.selfSigner.enabled": "false",
				"tls.certs.certManager":        "true",
				"operator.enabled":             "false",
			},
			expectErr: false,
		},
		{
			name: "SIGHUP rotation with provided certs should succeed",
			values: map[string]string{
				"tls.enabled":                  "true",
				"tls.enableSighupRotation":     "true",
				"tls.certs.selfSigner.enabled": "false",
				"tls.certs.certManager":        "false",
				"tls.certs.provided":           "true",
				"operator.enabled":             "false",
			},
			expectErr: false,
		},
	}

	for _, tc := range testCases {
		tc := tc
		t.Run(tc.name, func(subT *testing.T) {
			subT.Parallel()

			options := &helm.Options{
				KubectlOptions: k8s.NewKubectlOptions("", "", sighupNamespaceName),
				SetValues:      tc.values,
			}

			_, err := helm.RenderTemplateE(subT, options, sighupHelmChartPath, sighupReleaseName, []string{"templates/statefulset.yaml"})

			if tc.expectErr {
				require.Error(subT, err)
				require.Contains(subT, err.Error(), tc.errMessage)
			} else {
				require.NoError(subT, err)
			}
		})
	}
}

// TestSighupRotationStatefulSetInitContainers verifies that copy-certs initContainer is skipped when SIGHUP rotation is enabled
func TestSighupRotationStatefulSetInitContainers(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name                         string
		values                       map[string]string
		expectCopyCertsInitContainer bool
	}{
		{
			name: "SIGHUP rotation enabled - no copy-certs initContainer",
			values: map[string]string{
				"tls.enabled":                  "true",
				"tls.enableSighupRotation":     "true",
				"tls.certs.selfSigner.enabled": "false",
				"tls.certs.certManager":        "true",
				"operator.enabled":             "false",
			},
			expectCopyCertsInitContainer: false,
		},
		{
			name: "SIGHUP rotation disabled - copy-certs initContainer present",
			values: map[string]string{
				"tls.enabled":                  "true",
				"tls.enableSighupRotation":     "false",
				"tls.certs.selfSigner.enabled": "false",
				"tls.certs.certManager":        "true",
				"operator.enabled":             "false",
			},
			expectCopyCertsInitContainer: true,
		},
	}

	for _, tc := range testCases {
		tc := tc
		t.Run(tc.name, func(subT *testing.T) {
			subT.Parallel()

			options := &helm.Options{
				KubectlOptions: k8s.NewKubectlOptions("", "", sighupNamespaceName),
				SetValues:      tc.values,
			}

			output, err := helm.RenderTemplateE(subT, options, sighupHelmChartPath, sighupReleaseName, []string{"templates/statefulset.yaml"})
			require.NoError(subT, err)

			var sts appsv1.StatefulSet
			helm.UnmarshalK8SYaml(subT, output, &sts)

			hasCopyCerts := false
			for _, ic := range sts.Spec.Template.Spec.InitContainers {
				if ic.Name == "copy-certs" {
					hasCopyCerts = true
					break
				}
			}

			require.Equal(subT, tc.expectCopyCertsInitContainer, hasCopyCerts,
				"Expected copy-certs initContainer presence: %v, got: %v", tc.expectCopyCertsInitContainer, hasCopyCerts)
		})
	}
}

// TestSighupRotationStatefulSetVolumes verifies volume configuration when SIGHUP rotation is enabled
func TestSighupRotationStatefulSetVolumes(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name                string
		values              map[string]string
		expectCertsCombined bool
		expectCertsVolume   bool
	}{
		{
			name: "SIGHUP rotation enabled - certs-combined volume present",
			values: map[string]string{
				"tls.enabled":                  "true",
				"tls.enableSighupRotation":     "true",
				"tls.certs.selfSigner.enabled": "false",
				"tls.certs.certManager":        "true",
				"operator.enabled":             "false",
			},
			expectCertsCombined: true,
			expectCertsVolume:   false,
		},
		{
			name: "SIGHUP rotation disabled - certs volume present",
			values: map[string]string{
				"tls.enabled":                  "true",
				"tls.enableSighupRotation":     "false",
				"tls.certs.selfSigner.enabled": "false",
				"tls.certs.certManager":        "true",
				"operator.enabled":             "false",
			},
			expectCertsCombined: false,
			expectCertsVolume:   true,
		},
	}

	for _, tc := range testCases {
		tc := tc
		t.Run(tc.name, func(subT *testing.T) {
			subT.Parallel()

			options := &helm.Options{
				KubectlOptions: k8s.NewKubectlOptions("", "", sighupNamespaceName),
				SetValues:      tc.values,
			}

			output, err := helm.RenderTemplateE(subT, options, sighupHelmChartPath, sighupReleaseName, []string{"templates/statefulset.yaml"})
			require.NoError(subT, err)

			var sts appsv1.StatefulSet
			helm.UnmarshalK8SYaml(subT, output, &sts)

			hasCertsCombined := false
			hasCertsVolume := false

			for _, vol := range sts.Spec.Template.Spec.Volumes {
				if vol.Name == "certs-combined" {
					hasCertsCombined = true
					// Verify it's a projected volume
					require.NotNil(subT, vol.Projected, "certs-combined should be a projected volume")
				}
				if vol.Name == "certs" {
					hasCertsVolume = true
				}
			}

			require.Equal(subT, tc.expectCertsCombined, hasCertsCombined,
				"Expected certs-combined volume: %v, got: %v", tc.expectCertsCombined, hasCertsCombined)
			require.Equal(subT, tc.expectCertsVolume, hasCertsVolume,
				"Expected certs volume: %v, got: %v", tc.expectCertsVolume, hasCertsVolume)
		})
	}
}

// TestSighupRotationStatefulSetVolumeMounts verifies volume mount configuration when SIGHUP rotation is enabled
func TestSighupRotationStatefulSetVolumeMounts(t *testing.T) {
	t.Parallel()

	options := &helm.Options{
		KubectlOptions: k8s.NewKubectlOptions("", "", sighupNamespaceName),
		SetValues: map[string]string{
			"tls.enabled":                  "true",
			"tls.enableSighupRotation":     "true",
			"tls.certs.selfSigner.enabled": "false",
			"tls.certs.certManager":        "true",
			"operator.enabled":             "false",
		},
	}

	output, err := helm.RenderTemplateE(t, options, sighupHelmChartPath, sighupReleaseName, []string{"templates/statefulset.yaml"})
	require.NoError(t, err)

	var sts appsv1.StatefulSet
	helm.UnmarshalK8SYaml(t, output, &sts)

	// Find the db container
	var dbContainer *corev1.Container
	for i := range sts.Spec.Template.Spec.Containers {
		if sts.Spec.Template.Spec.Containers[i].Name == "db" {
			dbContainer = &sts.Spec.Template.Spec.Containers[i]
			break
		}
	}
	require.NotNil(t, dbContainer, "db container not found")

	// Verify certs-combined is mounted to /cockroach/cockroach-certs/
	hasCertsCombinedMount := false
	for _, vm := range dbContainer.VolumeMounts {
		if vm.Name == "certs-combined" && vm.MountPath == "/cockroach/cockroach-certs/" {
			hasCertsCombinedMount = true
			break
		}
	}
	require.True(t, hasCertsCombinedMount, "certs-combined should be mounted to /cockroach/cockroach-certs/")

	// Verify client-secret is mounted to /cockroach/certs/
	hasClientSecretMount := false
	for _, vm := range dbContainer.VolumeMounts {
		if vm.Name == "client-secret" && vm.MountPath == "/cockroach/certs/" {
			hasClientSecretMount = true
			break
		}
	}
	require.True(t, hasClientSecretMount, "client-secret should be mounted to /cockroach/certs/")
}

// TestSighupRotationProjectedVolumePermissions verifies that projected volumes have correct permissions (0440)
func TestSighupRotationProjectedVolumePermissions(t *testing.T) {
	t.Parallel()

	options := &helm.Options{
		KubectlOptions: k8s.NewKubectlOptions("", "", sighupNamespaceName),
		SetValues: map[string]string{
			"tls.enabled":                  "true",
			"tls.enableSighupRotation":     "true",
			"tls.certs.selfSigner.enabled": "false",
			"tls.certs.certManager":        "true",
			"operator.enabled":             "false",
		},
	}

	output, err := helm.RenderTemplateE(t, options, sighupHelmChartPath, sighupReleaseName, []string{"templates/statefulset.yaml"})
	require.NoError(t, err)

	var sts appsv1.StatefulSet
	helm.UnmarshalK8SYaml(t, output, &sts)

	// Find the certs-combined projected volume
	var certsCombinedVolume *corev1.Volume
	for i := range sts.Spec.Template.Spec.Volumes {
		if sts.Spec.Template.Spec.Volumes[i].Name == "certs-combined" {
			certsCombinedVolume = &sts.Spec.Template.Spec.Volumes[i]
			break
		}
	}
	require.NotNil(t, certsCombinedVolume, "certs-combined volume not found")
	require.NotNil(t, certsCombinedVolume.Projected, "certs-combined should be a projected volume")

	// Verify permissions are 0440 (octal 440 = decimal 288)
	expectedMode := int32(288) // 0440 in octal = 288 in decimal
	for _, source := range certsCombinedVolume.Projected.Sources {
		if source.Secret != nil {
			for _, item := range source.Secret.Items {
				require.NotNil(t, item.Mode, "Mode should be set for %s", item.Path)
				require.Equal(t, expectedMode, *item.Mode,
					"Expected mode 288 (0440) for %s, got %d", item.Path, *item.Mode)
			}
		}
	}
}

// TestSighupRotationJobInitVolumes verifies job.init.yaml volume configuration when SIGHUP rotation is enabled
func TestSighupRotationJobInitVolumes(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name                         string
		values                       map[string]string
		expectCopyCertsInitContainer bool
		expectClientSecretVolume     bool
		expectClientCertsVolume      bool
	}{
		{
			name: "SIGHUP rotation enabled - no copy-certs, client-secret volume present",
			values: map[string]string{
				"tls.enabled":                  "true",
				"tls.enableSighupRotation":     "true",
				"tls.certs.selfSigner.enabled": "false",
				"tls.certs.certManager":        "true",
				"operator.enabled":             "false",
			},
			expectCopyCertsInitContainer: false,
			expectClientSecretVolume:     true,
			expectClientCertsVolume:      false,
		},
		{
			name: "SIGHUP rotation disabled - copy-certs present, client-certs volume present",
			values: map[string]string{
				"tls.enabled":                  "true",
				"tls.enableSighupRotation":     "false",
				"tls.certs.selfSigner.enabled": "false",
				"tls.certs.certManager":        "true",
				"operator.enabled":             "false",
			},
			expectCopyCertsInitContainer: true,
			expectClientSecretVolume:     false,
			expectClientCertsVolume:      true,
		},
	}

	for _, tc := range testCases {
		tc := tc
		t.Run(tc.name, func(subT *testing.T) {
			subT.Parallel()

			options := &helm.Options{
				KubectlOptions: k8s.NewKubectlOptions("", "", sighupNamespaceName),
				SetValues:      tc.values,
			}

			output, err := helm.RenderTemplateE(subT, options, sighupHelmChartPath, sighupReleaseName, []string{"templates/job.init.yaml"})
			require.NoError(subT, err)

			var job batchv1.Job
			helm.UnmarshalK8SYaml(subT, output, &job)

			// Check initContainers
			hasCopyCerts := false
			for _, ic := range job.Spec.Template.Spec.InitContainers {
				if ic.Name == "copy-certs" {
					hasCopyCerts = true
					break
				}
			}
			require.Equal(subT, tc.expectCopyCertsInitContainer, hasCopyCerts,
				"Expected copy-certs initContainer in job.init: %v, got: %v", tc.expectCopyCertsInitContainer, hasCopyCerts)

			// Check volumes
			hasClientSecret := false
			hasClientCerts := false
			for _, vol := range job.Spec.Template.Spec.Volumes {
				if vol.Name == "client-secret" {
					hasClientSecret = true
				}
				if vol.Name == "client-certs" {
					hasClientCerts = true
				}
			}
			require.Equal(subT, tc.expectClientSecretVolume, hasClientSecret,
				"Expected client-secret volume in job.init: %v, got: %v", tc.expectClientSecretVolume, hasClientSecret)
			require.Equal(subT, tc.expectClientCertsVolume, hasClientCerts,
				"Expected client-certs volume in job.init: %v, got: %v", tc.expectClientCertsVolume, hasClientCerts)
		})
	}
}
