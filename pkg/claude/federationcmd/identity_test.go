package federationcmd

import (
	"github.com/stretchr/testify/require"
	"testing"
)

func TestIdentityOperatorCommandFlags(t *testing.T) {
	root := identityCmd()
	for _, name := range []string{"rotate", "recover-local", "recover-peer", "revoke-old", "rotations"} {
		command, _, err := root.Find([]string{name})
		require.NoError(t, err)
		require.Equal(t, name, command.Name())
		if name != "rotations" {
			require.NotNil(t, command.Flags().Lookup("apply"))
		}
		if name == "recover-peer" {
			require.NotNil(t, command.Flags().Lookup("fingerprint"))
		}
	}
}
