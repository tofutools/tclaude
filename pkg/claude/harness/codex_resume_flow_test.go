package harness_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/claude/common/convops"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// JOH-156 Phase-2b — the Codex spawn/resume FLOW, asserted at the
// production conv read path. The sibling codex_convstore_test.go pins the
// static read surface (a freshly-written rollout lists/resolves/titles
// correctly). These tests add the lifecycle dimension CC's SpawnResume
// flow has but Codex didn't: a session is created, the TUI exits, then
// `codex resume <id>` re-opens the SAME rollout and appends more turns —
// and the production ConvStore must still see ONE coherent conversation
// with a stable id / cwd / harness tag, its content grown, not a
// duplicate.
//
// This is the uncorrelated parity the operator asked for, on the resume
// axis: the sim author (this) drives CodexSim; the parser author owns the
// ConvStore under test. Resume here is HydrateCodexSim — the faithful
// model of `codex resume <id>` locating the rollout by session id and
// re-arming it to append (Codex resume is a subcommand, not a flag, and
// reopens the existing rollout file).

// TestCodexResumeFlow_OneConvAcrossResume is the core flow: spawn → exit →
// resume → append, with the production ConvStore re-scanned at each step.
func TestCodexResumeFlow_OneConvAcrossResume(t *testing.T) {
	home := codexTestHome(t)
	cwd := "/home/gigur/git/proj"
	const id = "019ec004-4250-79b1-9ade-ebaea4135453"
	cs := codexConvs(t)

	// Spawn: a Codex session with one exchange on disk.
	cx := startCodexSim(t, home, id, cwd, "first prompt", "first reply")

	entries, err := cs.ListConvs("")
	require.NoError(t, err)
	e0, ok := findEntry(entries, id)
	require.True(t, ok, "spawned Codex conv should list")
	assert.Equal(t, codexName, e0.Harness, "Codex conv tagged codex")
	assert.Equal(t, cwd, e0.ProjectPath, "real cwd is the resume target")
	assert.Equal(t, "first prompt", e0.FirstPrompt)
	require.NotZero(t, e0.FileSize)

	// Exit: the TUI closes (tmux kill-session / user quit). The rollout
	// stays on disk.
	cx.Shutdown()

	// Resume: `codex resume <id>` locates the rollout BY ID (date tree
	// scan), not by the path we happen to remember.
	resumed := testharness.HydrateCodexSim(t, home, id, cwd)
	assert.Equal(t, cx.RolloutPath, resumed.RolloutPath, "resume reopens the same rollout file")
	require.NoError(t, resumed.Start(), "resume re-arms the session")
	require.NoError(t, resumed.WriteUserInput("follow-up after resume"))

	// Re-scan: STILL one conversation for this id — resume must not fork a
	// second conv — with identity intact and content grown.
	entries2, err := cs.ListConvs("")
	require.NoError(t, err)
	assert.Equal(t, 1, countEntries(entries2, id), "resume keeps exactly one conv for the id")
	e1, ok := findEntry(entries2, id)
	require.True(t, ok)
	assert.Equal(t, codexName, e1.Harness, "harness tag survives the resume + re-scan")
	assert.Equal(t, cwd, e1.ProjectPath, "cwd stable across resume")
	assert.Equal(t, "first prompt", e1.FirstPrompt, "resume appends; it does not rewrite the first prompt")
	assert.Greater(t, e1.FileSize, e0.FileSize, "the appended turn grew the rollout")
	assert.Equal(t, e0.FullPath, e1.FullPath, "same rollout file backs the conv")

	// Resolve by a short id prefix still maps to the real cwd + harness —
	// the handle `codex resume` would use.
	ref, err := cs.Resolve(id[:8], cwd, false)
	require.NoError(t, err)
	require.NotNil(t, ref, "short-prefix resolve should find the resumed conv")
	assert.Equal(t, id, ref.ConvID)
	assert.Equal(t, cwd, ref.ProjectPath)
	assert.Equal(t, codexName, ref.Harness)
}

// TestCodexResumeFlow_RenamedTitleSurvivesResume pins that a native Codex
// rename — which lives in the threads state DB, NOT the rollout — is not
// disturbed by a resume that appends rollout turns. The title store and
// the turn log are independent; appending to one must not perturb the
// other.
func TestCodexResumeFlow_RenamedTitleSurvivesResume(t *testing.T) {
	home := codexTestHome(t)
	cwd := "/home/gigur/git/proj"
	const id = "019ec004-4250-79b1-9ade-ebaea4135454"
	cs := codexConvs(t)

	cx := startCodexSim(t, home, id, cwd, "hello", "hi")
	// A user rename persisted in ~/.codex/state_5.sqlite threads.title
	// (title != first user message ⇒ a real rename, not the derived title).
	writeCodexThread(t, home, codexThreadSeed{
		ID:               id,
		Cwd:              cwd,
		Title:            "Renamed By User",
		FirstUserMessage: "hello",
		CreatedAt:        1781337965,
		UpdatedAt:        1781337973,
	})

	title0, err := cs.Title(id)
	require.NoError(t, err)
	assert.Equal(t, "Renamed By User", title0, "rename visible before resume")

	// Resume + append a turn.
	cx.Shutdown()
	resumed := testharness.HydrateCodexSim(t, home, id, cwd)
	require.NoError(t, resumed.Start())
	require.NoError(t, resumed.WriteUserInput("more work"))

	// The rename still stands at both surfaces after the append.
	title1, err := cs.Title(id)
	require.NoError(t, err)
	assert.Equal(t, "Renamed By User", title1, "rename survives resume (lives in threads DB, not rollout)")

	entries, err := cs.ListConvs("")
	require.NoError(t, err)
	e, ok := findEntry(entries, id)
	require.True(t, ok)
	assert.Equal(t, "Renamed By User", e.CustomTitle, "native rename surfaces as CustomTitle post-resume")
}

// countEntries counts how many listed entries carry the given session id —
// the duplicate guard a resume flow needs (resume must not fork a second
// conv for the same rollout).
func countEntries(entries []convops.SessionEntry, id string) int {
	n := 0
	for _, e := range entries {
		if e.SessionID == id {
			n++
		}
	}
	return n
}
