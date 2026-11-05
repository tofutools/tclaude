package session

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// A launch that asks for no resource cgroup keeps its command, and the
// cleanup runNew holds is a callable no-op.
func TestWrapLaunchInResourceCgroupLeavesAnUnrequestedLaunchAlone(t *testing.T) {
	cmd, cleanup, err := wrapLaunchInResourceCgroup(&NewParams{}, "spwn-nocg",
		sandboxpolicy.ImplementationHarnessBuiltin, nil, nil, "exec claude")
	require.NoError(t, err)
	assert.Equal(t, "exec claude", cmd)
	require.NotNil(t, cleanup)
	cleanup()
}

// When the launched pane's directory cannot be read, a managed launch is
// refused (its unattended resume would have nothing to check) while a direct
// session launches without the metadata.
func TestCaptureLaunchProvenanceRefusesOnlyAManagedLaunch(t *testing.T) {
	prev := clcommon.Default
	clcommon.Default = fakeTmux{} // every tmux query fails
	t.Cleanup(func() { clcommon.Default = prev })

	_, err := captureLaunchProvenance("spwn-prov", "spwn-prov", true)
	require.ErrorContains(t, err, "capture managed launch provenance metadata")

	provenance, err := captureLaunchProvenance("spwn-prov", "spwn-prov", false)
	require.NoError(t, err)
	assert.Empty(t, provenance)
}

// Codex has no startup callback that reports its model and effort, so the
// launch seeds the validated choices onto the row; Claude reports its own.
func TestRecordLaunchPostureAndSeedsSeedsHarnessesWithoutACallback(t *testing.T) {
	isolateCacheAndDB(t)
	for _, id := range []string{"spwn-seed-codex", "spwn-seed-claude"} {
		require.NoError(t, SaveSessionState(&SessionState{ID: id, Status: StatusIdle}))
	}

	recordLaunchPostureAndSeeds("spwn-seed-codex", harness.MustGet(harness.CodexName), &NewParams{},
		"gpt-test", "high", false, false, nil, "")
	recordLaunchPostureAndSeeds("spwn-seed-claude", harness.MustGet(harness.DefaultName), &NewParams{},
		"claude-test", "high", false, false, nil, "")

	seeded := func(id string) (model, modelID, effort string) {
		d, err := db.Open()
		require.NoError(t, err)
		require.NoError(t, d.QueryRow(
			`SELECT COALESCE(model,''), COALESCE(model_id,''), COALESCE(effort_level,'') FROM sessions WHERE id = ?`, id,
		).Scan(&model, &modelID, &effort))
		return model, modelID, effort
	}
	model, modelID, effort := seeded("spwn-seed-codex")
	assert.Equal(t, []string{"gpt-test", "gpt-test", "high"}, []string{model, modelID, effort})
	model, _, effort = seeded("spwn-seed-claude")
	assert.Empty(t, model, "Claude reports its own model through the statusline")
	assert.Empty(t, effort)
}
