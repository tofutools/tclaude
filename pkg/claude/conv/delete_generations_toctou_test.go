package conv

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// TestDeleteAgentAllGenerations_HeadRotatesDuringSweep is the regression guard
// for the JOH-290 generation-sweep TOCTOU (fixed in PR #590).
//
// DeleteAgentAllGenerations resolves the actor's head, then captures the whole
// generation set, then sweeps every non-head generation. If the actor's head
// rotates (a `tclaude agent reincarnate` / Claude Code `/clear`) in the window
// between the head check and the generation capture, the newly-current
// conversation now appears in the captured set — and without the JOH-290
// live-head recheck it would be swept as a "predecessor", destroying the live
// conversation.
//
// We drive that race deterministically via the afterHeadCheckForTest seam: the
// hook rotates the head C→D inside the window, then we assert D (the new live
// head) survives the sweep while A, B (genuine predecessors) and C (the original
// head, torn down last) are reaped. Revert the JOH-290 recheck in delete.go and
// this test fails: D is deleted as a predecessor (and, because D is the live
// head by then, its delete tears the whole actor down).
func TestDeleteAgentAllGenerations_HeadRotatesDuringSweep(t *testing.T) {
	setupTestDB(t)
	dir := t.TempDir()

	// Four conversation generations of one actor, each with a real .jsonl on
	// disk and a conv_index row pointing at it (so DeleteConvByID resolves and
	// removes the file). A → B → C is the live chain (C the head); D is staged
	// on disk for the in-window rotation.
	files := map[string]string{}
	for _, id := range []string{"A", "B", "C", "D"} {
		files[id] = createConvFile(t, dir, id, true)
		indexInDB(t, dir, id, files[id])
	}

	// Build the live chain A → B → C; C is the head generation.
	_, _, err := db.EnsureAgentForConv("A", "spawn")
	require.NoError(t, err, "EnsureAgentForConv A")
	_, err = db.RotateAgentConv("A", "B", "clear")
	require.NoError(t, err, "rotate A→B")
	_, err = db.RotateAgentConv("B", "C", "reincarnate")
	require.NoError(t, err, "rotate B→C")

	actor, err := db.AgentIDForConv("C")
	require.NoError(t, err)
	require.NotEmpty(t, actor)

	// The race: rotate the head C→D in the TOCTOU window. After this fires the
	// captured generation set is {A, B, C, D} and the live head is D.
	var fired int
	afterHeadCheckForTest = func() {
		fired++
		_, rerr := db.RotateAgentConv("C", "D", "reincarnate")
		require.NoError(t, rerr, "in-window rotate C→D")
	}
	t.Cleanup(func() { afterHeadCheckForTest = nil })

	// Delete from C's handle — the head at the initial check, but a predecessor
	// by the time the sweep runs.
	_, swept, err := DeleteAgentAllGenerations("C")
	require.NoError(t, err, "DeleteAgentAllGenerations")
	require.Equal(t, 1, fired, "the TOCTOU hook fired exactly once")

	// The headline regression assertion: the rotated-in live head D must survive.
	assert.NotContains(t, swept, "D", "the new live head must not be swept as a predecessor")
	assert.FileExists(t, files["D"], "D's .jsonl must survive the sweep")
	dAgent, err := db.AgentIDForConv("D")
	require.NoError(t, err)
	assert.Equal(t, actor, dAgent, "D still resolves to the live actor")
	live, err := db.GetAgent(actor)
	require.NoError(t, err)
	require.NotNil(t, live, "the actor survives the sweep")
	assert.Equal(t, "D", live.CurrentConvID, "D is the actor's live head")

	// A, B (genuine predecessors) and C (original head, torn down last) are reaped.
	assert.ElementsMatch(t, []string{"A", "B", "C"}, swept, "A, B and C are swept")
	for _, id := range []string{"A", "B", "C"} {
		assert.NoFileExists(t, files[id], "%s's .jsonl must be deleted", id)
	}
}
