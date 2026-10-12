package claude

import (
	"bytes"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/agentipc/agentipctest"
	"github.com/tofutools/tclaude/pkg/federation/hubcmd"
	"github.com/tofutools/tclaude/pkg/testutil"
)

func TestHubCommandMirrorsStandaloneTree(t *testing.T) {
	agentipctest.IsolateManagedAgentEnv(t)
	t.Setenv("HOME", testutil.CanonicalTempDir(t))
	main := Cmd()
	main.Use = "tclaude"
	var walk func(*cobra.Command, []string)
	walk = func(cmd *cobra.Command, path []string) {
		for _, child := range cmd.Commands() {
			args := append(append([]string(nil), path...), child.Name())
			mirrored, rest, err := main.Find(append([]string{"hub"}, args...))
			require.NoError(t, err)
			require.Empty(t, rest)
			require.Equal(t, child.Name(), mirrored.Name())
			require.Equal(t, child.Short, mirrored.Short)
			child.Flags().VisitAll(func(flag *pflag.Flag) {
				got := mirrored.Flags().Lookup(flag.Name)
				require.NotNil(t, got)
				require.Equal(t, flag.DefValue, got.DefValue)
				require.Equal(t, flag.Shorthand, got.Shorthand)
			})
			walk(child, args)
		}
	}
	walk(hubcmd.RootCmd(), nil)
	main.SetArgs([]string{"hub", "admin", "rotate-claim", "--help"})
	var output bytes.Buffer
	main.SetOut(&output)
	main.SetErr(&output)
	require.NoError(t, main.Execute())
	require.Contains(t, output.String(), "tclaude hub admin rotate-claim")
	require.Contains(t, output.String(), "--print-claim-token")
	require.Contains(t, output.String(), "--db")
}
