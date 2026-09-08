package nativeactivity

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/ports"
)

func TestCheckpointRetainsCorrelationAndRefusesMalformedActivity(t *testing.T) {
	directory := t.TempDir()
	require.NoError(t, os.Chmod(directory, 0o700))
	var original State
	at := time.Now().UTC()
	original.Record(ports.AgentActivityIdle, at)
	require.NoError(t, original.Save(directory, "primary"))
	var foreign State
	require.NoError(t, foreign.Restore(directory, "other"))
	value, _ := foreign.Observation()
	require.Equal(t, ports.AgentActivityUnknown, value)
	var restored State
	require.NoError(t, restored.Restore(directory, "primary"))
	value, observedAt := restored.Observation()
	require.Equal(t, ports.AgentActivityIdle, value)
	require.Equal(t, at, observedAt)
	require.NoError(t, os.WriteFile(filepath.Join(directory, "activity.json"), []byte(`{"SessionID":"primary","State":"idle"}`), 0o600))
	var invalid State
	require.Error(t, invalid.Restore(directory, "primary"))
	value, _ = invalid.Observation()
	require.Equal(t, ports.AgentActivityUnknown, value)
}
