package agentd_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// These scenarios pin model/effort inheritance across the lifecycle
// verbs that mint a successor CC instance: reincarnate, clone (both
// the fresh and the jsonl-copy path) and resume. The predecessor's
// LIVE model is what the statusline hook recorded on its session row
// (sessions.model_id + effort_level) — so a mid-life /model switch is
// inherited, not the launch-time flag. Assertions sit at the Spawner
// boundary (World.SpawnModel / SpawnEffort), the production seam where
// liveSpawnNew / liveSpawnResume later build the --model flag.

// reportModel simulates what the statusline callback does for a live
// session: persist the full model ID + reasoning effort onto the
// session row keyed by the tclaude session id (the label).
func reportModel(t *testing.T, label, modelID, effort string) {
	t.Helper()
	require.NoError(t, db.UpdateSessionModelID(label, modelID), "record model id")
	if effort != "" {
		require.NoError(t, db.UpdateSessionEffort(label, effort), "record effort")
	}
}

// Scenario: a worker that last reported running claude-opus-4-8 at
// effort high is reincarnated. The successor must be spawned with that
// same model + effort — NOT claude's default.
func TestReincarnate_InheritsLiveModelAndEffort(t *testing.T) {
	f := newFlow(t)

	const oldConv = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaa1"
	const oldLabel = "spwn-old-001"
	f.HaveAliveSession(oldConv, oldLabel, "tclaude-"+oldLabel, "/tmp/work")
	reportModel(t, oldLabel, "claude-opus-4-8", "high")

	r := f.AsHuman().Reincarnate(oldConv, "fresh start")

	model, ok := f.World.SpawnModel(r.NewConv)
	require.True(t, ok, "no spawn recorded for successor conv %s", r.NewConv)
	assert.Equal(t, "claude-opus-4-8", model, "successor must run the predecessor's model")
	effort, ok := f.World.SpawnEffort(r.NewConv)
	require.True(t, ok)
	assert.Equal(t, "high", effort, "successor must run the predecessor's effort")
}

// Scenario: a Codex worker records an OpenAI model id and Codex reasoning
// effort. Inheritance must validate those through the Codex harness catalog,
// not Claude Code's model aliases, or every Codex successor would silently
// fall back to the default model.
func TestReincarnate_CodexInheritsLiveModelAndEffort(t *testing.T) {
	f := newFlow(t)

	const oldConv = "019ec004-4250-79b1-9ade-ebaea41591aa"
	const oldLabel = "spwn-codex-old"
	f.HaveAliveCodexSession(oldConv, oldLabel, "tclaude-"+oldLabel, "/tmp/work")
	now := time.Now().Format(time.RFC3339)
	require.NoError(t, db.UpsertConvIndex(&db.ConvIndexRow{
		ConvID:      oldConv,
		ProjectDir:  "/tmp/work",
		ProjectPath: "/tmp/work",
		FirstPrompt: "codex work",
		Created:     now,
		Modified:    now,
		Harness:     "codex",
	}))
	reportModel(t, oldLabel, "gpt-5-codex", "high")

	r := f.AsHuman().Reincarnate(oldConv, "fresh start")

	model, ok := f.World.SpawnModel(r.NewConv)
	require.True(t, ok, "no spawn recorded for Codex successor conv %s", r.NewConv)
	assert.Equal(t, "gpt-5-codex", model, "Codex successor must run the predecessor's model")
	effort, ok := f.World.SpawnEffort(r.NewConv)
	require.True(t, ok)
	assert.Equal(t, "high", effort, "Codex successor must run the predecessor's effort")
}

// Scenario: the predecessor ran a 1M-context variant on an OLDER Claude
// Code build whose model.id carried NO window suffix — the
// context_window_size snapshot (1000000) is what distinguishes it — so
// the successor's --model must come out as `<id>[1m]`. (The current-build
// counterpart, where model.id already carries the suffix, is covered by
// TestAgentResume_1MModelIDWithSuffix_NoDoubleSuffix.)
func TestReincarnate_1MContextWindow_AppendsSuffix(t *testing.T) {
	f := newFlow(t)

	const oldConv = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaa2"
	const oldLabel = "spwn-old-1m"
	f.HaveAliveSession(oldConv, oldLabel, "tclaude-"+oldLabel, "/tmp/work")
	reportModel(t, oldLabel, "claude-fable-5", "")
	require.NoError(t, db.UpdateContextSnapshot(oldLabel, 42, 400_000, 8_000, 1_000_000))

	r := f.AsHuman().Reincarnate(oldConv, "fresh start")

	model, ok := f.World.SpawnModel(r.NewConv)
	require.True(t, ok, "no spawn recorded for successor conv %s", r.NewConv)
	assert.Equal(t, "claude-fable-5[1m]", model, "1M window must select the [1m] variant")
}

// Scenario: the predecessor never reported a model (statusbar never
// ticked, or an older Claude Code without model.id). Inheritance must
// fail open: "" threads through so the spawn omits --model and claude
// resolves its own default — the pre-inheritance behaviour, never an
// error.
func TestReincarnate_NoModelReported_FallsBackToDefault(t *testing.T) {
	f := newFlow(t)

	const oldConv = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaa3"
	const oldLabel = "spwn-old-noop"
	f.HaveAliveSession(oldConv, oldLabel, "tclaude-"+oldLabel, "/tmp/work")

	r := f.AsHuman().Reincarnate(oldConv, "fresh start")

	model, ok := f.World.SpawnModel(r.NewConv)
	require.True(t, ok, "no spawn recorded for successor conv %s", r.NewConv)
	assert.Equal(t, "", model, `no recorded model must thread "" (spawn omits --model)`)
	effort, ok := f.World.SpawnEffort(r.NewConv)
	require.True(t, ok)
	assert.Equal(t, "", effort, `no recorded effort must thread ""`)
}

// Scenario: a garbage model_id on the row (hand-edited DB, future
// statusline format drift) must not poison the spawn — validation
// collapses it to "" rather than forwarding it to `claude --model`.
func TestReincarnate_InvalidRecordedModel_FallsBackToDefault(t *testing.T) {
	f := newFlow(t)

	const oldConv = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaa4"
	const oldLabel = "spwn-old-junk"
	f.HaveAliveSession(oldConv, oldLabel, "tclaude-"+oldLabel, "/tmp/work")
	reportModel(t, oldLabel, "gpt-6-please; rm -rf /", "")

	r := f.AsHuman().Reincarnate(oldConv, "fresh start")

	model, ok := f.World.SpawnModel(r.NewConv)
	require.True(t, ok, "no spawn recorded for successor conv %s", r.NewConv)
	assert.Equal(t, "", model, "an unvalidatable model id must collapse to the default")
}

// Scenario: a fresh clone (no_copy_conv) of an agent running on
// claude-sonnet-4-6. The sibling is a fork of the original and must
// come up on the same model.
func TestCloneFresh_InheritsLiveModel(t *testing.T) {
	f := newFlow(t)

	const oldConv = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaa5"
	const oldLabel = "spwn-old-clne"
	f.HaveAliveSession(oldConv, oldLabel, "tclaude-"+oldLabel, "/tmp/work")
	reportModel(t, oldLabel, "claude-sonnet-4-6", "max")

	c := f.AsHuman().CloneFresh(oldConv)

	model, ok := f.World.SpawnModel(c.NewConv)
	require.True(t, ok, "no spawn recorded for clone conv %s", c.NewConv)
	assert.Equal(t, "claude-sonnet-4-6", model, "clone must run the original's model")
	effort, ok := f.World.SpawnEffort(c.NewConv)
	require.True(t, ok)
	assert.Equal(t, "max", effort, "clone must run the original's effort")
}

// Scenario: a copy-conv clone — the path that forks the jsonl and
// resumes into it via `tclaude session new -r`. `claude --resume` does
// NOT restore the conversation's model by itself, so the resume spawn
// must carry the inherited --model too.
func TestCloneCopy_InheritsLiveModel(t *testing.T) {
	f := newFlow(t)

	const oldConv = "11111111-2222-3333-4444-555555555555"
	const oldLabel = "spwn-old-copy"
	f.HaveAliveSession(oldConv, oldLabel, "tclaude-"+oldLabel, "/tmp/work")
	reportModel(t, oldLabel, "claude-opus-4-8", "")

	c := f.AsHuman().CloneWith(oldConv, map[string]any{})
	require.Equal(t, 200, c.Code, "clone (copy path): %s", c.Raw)
	require.NotEmpty(t, c.NewConv, "copy-path clone minted a conv")

	model, ok := f.World.SpawnModel(c.NewConv)
	require.True(t, ok, "no resume-spawn recorded for clone conv %s", c.NewConv)
	assert.Equal(t, "claude-opus-4-8", model, "copy-path clone must resume on the original's model")
}

// Scenario: an offline agent is resumed. The fresh pane re-opens the
// SAME conversation, so it must come back on the model that conv last
// reported — its freshest session row carries it.
func TestAgentResume_InheritsLiveModel(t *testing.T) {
	f := newFlow(t)

	const conv = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaa6"
	const label = "spwn-old-rsme"
	const tmux = "tclaude-" + label
	f.HaveAliveSession(conv, label, tmux, f.World.HomeDir)
	reportModel(t, label, "claude-fable-5", "high")
	f.MarkOffline(tmux)

	r := f.AsHuman().Resume(conv)
	require.Equal(t, "resumed", r.Action, "resume action: %s", r.Raw)

	model, ok := f.World.SpawnModel(conv)
	require.True(t, ok, "no resume-spawn recorded for conv %s", conv)
	assert.Equal(t, "claude-fable-5", model, "resumed agent must come back on its own model")
	effort, ok := f.World.SpawnEffort(conv)
	require.True(t, ok)
	assert.Equal(t, "high", effort, "resumed agent must come back on its own effort")
}

// Scenario (regression): current Claude Code builds report model.id WITH
// the [1m] window suffix already attached (e.g. "claude-opus-4-8[1m]"),
// not the bare id the inheritance code originally assumed. A resume of a
// 1M-context agent must still come back on the 1M variant exactly once.
//
// The pre-fix code blind-appended "[1m]" whenever the window snapshot was
// 1M, producing "claude-opus-4-8[1m][1m]" — which fails ValidateModel and
// collapses --model to "", so `claude --resume` fell back to the family
// Claude Code restores from the conversation (right) but lost the 1M
// window (wrong); effort, validated independently, survived. That exact
// "right family + right effort, no [1m]" symptom is what this pins.
func TestAgentResume_1MModelIDWithSuffix_NoDoubleSuffix(t *testing.T) {
	f := newFlow(t)

	const conv = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaa7"
	const label = "spwn-old-1msfx"
	const tmux = "tclaude-" + label
	f.HaveAliveSession(conv, label, tmux, f.World.HomeDir)
	// model.id already carries [1m], window snapshot also says 1M.
	reportModel(t, label, "claude-opus-4-8[1m]", "high")
	require.NoError(t, db.UpdateContextSnapshot(label, 12, 120_000, 800, 1_000_000))
	f.MarkOffline(tmux)

	r := f.AsHuman().Resume(conv)
	require.Equal(t, "resumed", r.Action, "resume action: %s", r.Raw)

	model, ok := f.World.SpawnModel(conv)
	require.True(t, ok, "no resume-spawn recorded for conv %s", conv)
	assert.Equal(t, "claude-opus-4-8[1m]", model, "1M model.id with suffix must resume on [1m] exactly once")
	effort, ok := f.World.SpawnEffort(conv)
	require.True(t, ok)
	assert.Equal(t, "high", effort, "effort must survive alongside the 1M model")
}
