package agent

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGuideFill_ACommaStaysInsideItsValue(t *testing.T) {
	// Parsing only: running the command would reach a daemon.
	cmd := guideFillCmd()
	require.NoError(t, cmd.ParseFlags([]string{"--form", "spawn", "--set", "agent-spawn-init-msg=first, then second", "--set", "agent-spawn-name=a"}))
	got, err := cmd.Flags().GetStringArray("set")
	require.NoError(t, err)
	assert.Equal(t, []string{"agent-spawn-init-msg=first, then second", "agent-spawn-name=a"}, got)
}
