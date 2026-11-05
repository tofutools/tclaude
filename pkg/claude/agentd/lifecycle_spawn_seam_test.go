package agentd

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/launchundo"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

func noHistory(*harness.Harness) *spawnFailure { return nil }

// A harness that cannot preset its conversation enrolls after it connects,
// unless the spawn imports history, which always enrolls before launch.
func TestResolveSpawnLaunchSeamEnrollsImportsBeforeLaunch(t *testing.T) {
	resetTestDB(t)
	codex := harness.MustGet(harness.CodexName)
	imported := ""
	enroll, _, fail := resolveSpawnLaunchSeam(nil, &spawnParams{}, codex, false, "", noHistory, &imported)
	require.Nil(t, fail)
	assert.False(t, enroll)

	imported = "imported-conv"
	enroll, _, fail = resolveSpawnLaunchSeam(nil, &spawnParams{}, codex, false, "", noHistory, &imported)
	require.Nil(t, fail)
	assert.True(t, enroll, "an import enrolls before launch")
}

// A route-capable launch that cannot use the preset-conversation seam is refused.
func TestResolveSpawnLaunchSeamRefusesARouteLaunchWithoutEnrollment(t *testing.T) {
	resetTestDB(t)
	imported := ""
	_, _, fail := resolveSpawnLaunchSeam(nil, &spawnParams{DarwinRouteCapable: true},
		harness.MustGet(harness.CodexName), false, "", noHistory, &imported)
	require.NotNil(t, fail)
	assert.Equal(t, http.StatusUnprocessableEntity, fail.Status)
	assert.Equal(t, "darwin_route_launch", fail.Kind)
}

// A history install failure stops the spawn.
func TestResolveSpawnLaunchSeamStopsOnAHistoryFailure(t *testing.T) {
	resetTestDB(t)
	imported := ""
	want := &spawnFailure{http.StatusBadRequest, "history", "boom"}
	_, _, fail := resolveSpawnLaunchSeam(nil, &spawnParams{}, harness.MustGet(harness.DefaultName), false, "",
		func(*harness.Harness) *spawnFailure { return want }, &imported)
	assert.Same(t, want, fail)
}

// A seed-needing harness that is not enrolled before launch gets its welcome
// as the first-turn prompt, and no preset conversation.
func TestEnrollOrSeedSpawnSeedsACodexLaunch(t *testing.T) {
	resetTestDB(t)
	var args clcommon.SpawnArgs
	conv, msg, inlined, fail := enrollOrSeedSpawn(nil, &spawnParams{Name: "seeded"}, &args,
		harness.MustGet(harness.CodexName), "", "", nil, &launchundo.Undo{}, false)
	require.Nil(t, fail)
	assert.Empty(t, conv)
	assert.Zero(t, msg)
	assert.False(t, inlined)
	assert.NotEmpty(t, args.InitialPrompt)
}

// An offline landing commits what the launch enrolled, so the deferred
// unwind leaves it in place.
func TestLandSpawnWithoutLaunchKeepsTheEnrollment(t *testing.T) {
	resetTestDB(t)
	undo := &launchundo.Undo{}
	unwound := false
	undo.Add(func() { unwound = true })
	outcome, fail := landSpawnWithoutLaunch(&spawnParams{NoLaunch: true, AgentID: "agt_landed"},
		harness.MustGet(harness.DefaultName), "conv-landed", undo)
	require.Nil(t, fail)
	assert.Equal(t, "conv-landed", outcome.ConvID)
	assert.Equal(t, "agt_landed", outcome.AgentID)
	undo.UnwindUnlessCommitted()
	assert.False(t, unwound, "the landed conversation keeps its enrollment")
}
