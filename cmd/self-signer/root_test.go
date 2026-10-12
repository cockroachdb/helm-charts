package self_signer

import (
	"strconv"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
)

func TestOperatorManagedConfig(t *testing.T) {
	t.Setenv("STATEFULSET_NAME", "test-crdb")
	t.Setenv("CLUSTER_DOMAIN", "cluster.local")
	t.Cleanup(func() { operatorManaged = false })
	for _, cmd := range []*cobra.Command{generateCmd, rotateCmd} {
		for _, managed := range []bool{false, true} {
			args := []string{}
			if managed {
				args = append(args, "--operator-managed=true")
			}
			t.Run(cmd.Name()+"/operator="+strconv.FormatBool(managed), func(t *testing.T) {
				operatorManaged = false
				require.NoError(t, cmd.ParseFlags(args))
				gen, err := getInitialConfig("43800h", "648h", "8760h", "168h", "672h", "48h")
				require.NoError(t, err)
				require.Equal(t, managed, gen.OperatorManaged)
			})
		}
	}
}
