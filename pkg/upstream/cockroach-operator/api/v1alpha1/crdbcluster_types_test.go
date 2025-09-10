package v1alpha1

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/yaml"
)

func TestCrdbWalFailoverStatus(t *testing.T) {
	testCases := []struct {
		name     string
		status   CrdbWalFailoverStatus
		expected string
	}{
		{
			name:     "WAL enable status",
			status:   WalEnable,
			expected: "enable",
		},
		{
			name:     "WAL disable status",
			status:   WalDisable,
			expected: "disable",
		},
		{
			name:     "WAL not set status",
			status:   WalNotSet,
			expected: "",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.expected, string(tc.status))
		})
	}
}

func TestCrdbWalFailoverSpec(t *testing.T) {
	testCases := []struct {
		name string
		spec CrdbWalFailoverSpec
	}{
		{
			name: "WAL failover enabled with full configuration",
			spec: CrdbWalFailoverSpec{
				Name:             "failoverdir",
				Size:             "50Gi",
				StorageClassName: "fast-ssd",
				Status:           WalEnable,
				Path:             "/custom/wal",
			},
		},
		{
			name: "WAL failover disabled",
			spec: CrdbWalFailoverSpec{
				Status: WalDisable,
			},
		},
		{
			name: "WAL failover with minimal configuration",
			spec: CrdbWalFailoverSpec{
				Size:   "10Gi",
				Status: WalEnable,
				Path:   "/wal",
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// Test that the spec can be marshaled and unmarshaled
			yamlData, err := yaml.Marshal(tc.spec)
			require.NoError(t, err)

			var unmarshaledSpec CrdbWalFailoverSpec
			err = yaml.Unmarshal(yamlData, &unmarshaledSpec)
			require.NoError(t, err)

			// Verify all fields are preserved
			assert.Equal(t, tc.spec.Name, unmarshaledSpec.Name)
			assert.Equal(t, tc.spec.Size, unmarshaledSpec.Size)
			assert.Equal(t, tc.spec.StorageClassName, unmarshaledSpec.StorageClassName)
			assert.Equal(t, tc.spec.Status, unmarshaledSpec.Status)
			assert.Equal(t, tc.spec.Path, unmarshaledSpec.Path)
		})
	}
}

func TestCrdbClusterSpec_WithWalFailoverSpec(t *testing.T) {
	testCases := []struct {
		name        string
		clusterSpec CrdbClusterSpec
		hasWalSpec  bool
	}{
		{
			name: "Cluster spec with WAL failover enabled",
			clusterSpec: CrdbClusterSpec{
				TLSEnabled: true,
				Regions: []CrdbClusterRegion{
					{
						Code:  "us-central1",
						Nodes: 3,
					},
				},
				WalFailoverSpec: &CrdbWalFailoverSpec{
					Name:             "failoverdir",
					Size:             "50Gi",
					StorageClassName: "fast-ssd",
					Status:           WalEnable,
					Path:             "/custom/wal",
				},
			},
			hasWalSpec: true,
		},
		{
			name: "Cluster spec with WAL failover disabled",
			clusterSpec: CrdbClusterSpec{
				TLSEnabled: true,
				Regions: []CrdbClusterRegion{
					{
						Code:  "us-central1",
						Nodes: 3,
					},
				},
				WalFailoverSpec: &CrdbWalFailoverSpec{
					Status: WalDisable,
				},
			},
			hasWalSpec: true,
		},
		{
			name: "Cluster spec without WAL failover",
			clusterSpec: CrdbClusterSpec{
				TLSEnabled: true,
				Regions: []CrdbClusterRegion{
					{
						Code:  "us-central1",
						Nodes: 3,
					},
				},
				WalFailoverSpec: nil,
			},
			hasWalSpec: false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// Test marshaling and unmarshaling
			yamlData, err := yaml.Marshal(tc.clusterSpec)
			require.NoError(t, err)

			var unmarshaledSpec CrdbClusterSpec
			err = yaml.Unmarshal(yamlData, &unmarshaledSpec)
			require.NoError(t, err)

			// Verify WAL failover spec presence
			if tc.hasWalSpec {
				require.NotNil(t, unmarshaledSpec.WalFailoverSpec)
				assert.Equal(t, tc.clusterSpec.WalFailoverSpec.Status, unmarshaledSpec.WalFailoverSpec.Status)
				assert.Equal(t, tc.clusterSpec.WalFailoverSpec.Name, unmarshaledSpec.WalFailoverSpec.Name)
				assert.Equal(t, tc.clusterSpec.WalFailoverSpec.Size, unmarshaledSpec.WalFailoverSpec.Size)
				assert.Equal(t, tc.clusterSpec.WalFailoverSpec.StorageClassName, unmarshaledSpec.WalFailoverSpec.StorageClassName)
				assert.Equal(t, tc.clusterSpec.WalFailoverSpec.Path, unmarshaledSpec.WalFailoverSpec.Path)
			} else {
				assert.Nil(t, unmarshaledSpec.WalFailoverSpec)
			}

			// Verify other fields are preserved
			assert.Equal(t, tc.clusterSpec.TLSEnabled, unmarshaledSpec.TLSEnabled)
			assert.Equal(t, tc.clusterSpec.Regions, unmarshaledSpec.Regions)
		})
	}
}

func TestCrdbWalFailoverSpec_JSONTags(t *testing.T) {
	// Test that JSON tags are working properly
	spec := CrdbWalFailoverSpec{
		Name:             "failoverdir",
		Size:             "50Gi",
		StorageClassName: "fast-ssd",
		Status:           WalEnable,
		Path:             "/custom/wal",
	}

	// Marshal to YAML (which uses JSON tags under the hood)
	yamlData, err := yaml.Marshal(spec)
	require.NoError(t, err)

	// Verify the YAML contains the expected fields
	yamlString := string(yamlData)
	assert.Contains(t, yamlString, "name: failoverdir")
	assert.Contains(t, yamlString, "size: 50Gi")
	assert.Contains(t, yamlString, "storageClassName: fast-ssd")
	assert.Contains(t, yamlString, "status: enable")
	assert.Contains(t, yamlString, "path: /custom/wal")
}

func TestCrdbWalFailoverSpec_Validation(t *testing.T) {
	testCases := []struct {
		name  string
		spec  CrdbWalFailoverSpec
		valid bool
	}{
		{
			name: "Valid enabled spec",
			spec: CrdbWalFailoverSpec{
				Name:             "failoverdir",
				Size:             "50Gi",
				StorageClassName: "fast-ssd",
				Status:           WalEnable,
				Path:             "/custom/wal",
			},
			valid: true,
		},
		{
			name: "Valid disabled spec",
			spec: CrdbWalFailoverSpec{
				Status: WalDisable,
			},
			valid: true,
		},
		{
			name: "Enabled spec with empty path",
			spec: CrdbWalFailoverSpec{
				Name:             "failoverdir",
				Size:             "50Gi",
				StorageClassName: "fast-ssd",
				Status:           WalEnable,
				Path:             "",
			},
			valid: false, // Should be invalid as enabled spec needs a path
		},
		{
			name: "Empty status",
			spec: CrdbWalFailoverSpec{
				Name:             "failoverdir",
				Size:             "50Gi",
				StorageClassName: "fast-ssd",
				Status:           WalNotSet,
				Path:             "/custom/wal",
			},
			valid: false, // Should be invalid as status must be set
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// Basic validation logic (this would typically be done by admission controllers)
			isValid := tc.spec.Status != WalNotSet
			if tc.spec.Status == WalEnable {
				isValid = isValid && tc.spec.Path != "" && tc.spec.Size != ""
			}

			assert.Equal(t, tc.valid, isValid, "Validation result should match expected")
		})
	}
}