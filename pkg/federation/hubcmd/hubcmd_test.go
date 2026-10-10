package hubcmd

import (
	"io"
	"runtime"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/federation/hubupdate"
)

func TestServeParamsPlainServeWithoutSupervisorLabel(t *testing.T) {
	// Use the production command definition and its Boa validation, replacing
	// only execution so this test neither listens nor creates a hub database.
	definition := serveDefinition()
	var received *serveParams
	definition.RunFunc = func(p *serveParams, _ *cobra.Command, _ []string) { received = p }
	cmd, err := definition.ToCobraE()
	require.NoError(t, err)
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"--listen", "127.0.0.1:18470"})
	require.NoError(t, cmd.Execute())
	require.NotNil(t, received)
	require.Equal(t, "127.0.0.1:18470", received.Listen)
	require.Empty(t, received.SupervisorLabel)
	require.False(t, received.Supervised)
}

func TestSupervisedMacOSRequiresSupervisorLabel(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("launchd validation")
	}
	_, err := hubupdate.DetectSupervisor("")
	require.ErrorContains(t, err, "--supervisor-label")
}
