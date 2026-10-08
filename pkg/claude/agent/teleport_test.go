package agent

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestTeleportBackupCLIFlagsAndControls(t *testing.T) {
	cmd := teleportCmd()
	require.NotNil(t, cmd.Flags().Lookup("keep-paused-backup"))
	for _, name := range []string{"report", "recover", "status"} {
		found, _, err := cmd.Find([]string{name})
		require.NoError(t, err)
		require.Equal(t, name, found.Name())
	}
	var stdout, stderr bytes.Buffer
	require.Equal(t, rcInvalidArg, runTeleport(&teleportParams{Target: "peer", Clone: true, KeepPausedBackup: true}, &stdout, &stderr))
	require.Contains(t, stderr.String(), "mutually exclusive")
	require.Equal(t, "paused (teleported to node/agt_target)", peerStatus(&peerEntry{TeleportPaused: "paused (teleported to node/agt_target)"}))
}
