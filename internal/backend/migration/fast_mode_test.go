package migration

import (
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/model"
	"testing"
)

func TestImportedFastModePreservesNullableIntent(t *testing.T) {
	for _, tc := range []struct {
		raw  any
		want model.FastMode
	}{{nil, ""}, {int64(0), model.FastModeOff}, {int64(1), model.FastModeOn}, {false, model.FastModeOff}, {true, model.FastModeOn}} {
		d := desiredFromRow(map[string]any{"harness": "codex", "fast_mode": tc.raw})
		require.Equal(t, tc.want, d.FastMode)
	}
	d := desiredFromRow(map[string]any{"harness": "codex", "initial_spawn_config": `{"harness":"codex","fast_mode":true}`})
	require.Equal(t, model.FastModeOn, d.FastMode)
	d, err := applyAgentRelaunchPolicy(map[string]any{"relaunch_profile": `{"version":1,"fast_mode":false}`}, d)
	require.NoError(t, err)
	require.Equal(t, model.FastModeOff, d.FastMode)
}
