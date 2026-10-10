package hubcmd

import (
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/federation/hub"
	"github.com/tofutools/tclaude/pkg/federation/proto"
	"github.com/tofutools/tclaude/pkg/testutil"
)

func TestHubCommandsExposeDatabaseFlag(t *testing.T) {
	var check func(*cobra.Command)
	check = func(cmd *cobra.Command) {
		if cmd.HasSubCommands() {
			for _, child := range cmd.Commands() {
				check(child)
			}
			return
		}
		require.NotNil(t, cmd.Flags().Lookup("db"), "%s must expose --db", cmd.CommandPath())
	}
	check(RootCmd())
}

func TestHubAdmitUsesExplicitDatabase(t *testing.T) {
	dir := testutil.CanonicalTempDir(t)
	t.Setenv("TCLAUDE_HUB_DIR", filepath.Join(dir, "default"))
	path := filepath.Join(dir, "selected.sqlite")
	id, err := proto.NewIdentity()
	require.NoError(t, err)
	cmd := admitCmd()
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{id.ID(), "--db", path})
	require.NoError(t, cmd.Execute())
	store, err := hub.OpenStore(path)
	require.NoError(t, err)
	defer func() { _ = store.Close() }()
	admitted, err := store.Get(id.ID())
	require.NoError(t, err)
	require.NotNil(t, admitted, "admission must be written to the selected DB")
	_, err = os.Stat(DefaultDBPath())
	require.True(t, os.IsNotExist(err), "explicit --db must not touch the default DB")
}
