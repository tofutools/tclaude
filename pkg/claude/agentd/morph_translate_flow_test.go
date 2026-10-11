package agentd_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/convops"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/claude/harness/transcript"
	"github.com/tofutools/tclaude/pkg/testharness"
	"github.com/tofutools/tclaude/pkg/testutil"
)

// Cross-harness morph, history leg: an agent's native conversation in one
// harness is translated and installed as a native conversation of another,
// which then resumes it by id and keeps appending to the same file. The
// morph endpoint wiring lands separately; these flows pin the translation
// against the real on-disk formats, conv index and read paths.

const morphSecret = "Remember the secret word: PELICAN."

func historyText(t *testing.T, h *harness.Harness, convID, cwd string) string {
	t.Helper()
	tr, err := harness.DecodeHistory(h, convID, cwd)
	require.NoError(t, err)
	var b bytes.Buffer
	require.NoError(t, transcript.WriteMarkdown(tr, &b))
	return b.String()
}

func TestMorphTranslateClaudeToCodexResumesNatively(t *testing.T) {
	f := newFlow(t)
	t.Setenv("CODEX_HOME", "")
	const source = "0f0e0d0c-0b0a-4908-8706-050403020101"
	cwd := testutil.CanonicalTempDir(t)
	f.HaveAliveSession(source, "morph-src", "morph-pane", cwd)
	cc := f.World.CCs.GetByLabel("morph-src")
	require.NotNil(t, cc)
	turn := func(typ, id, parent string, msg map[string]any) {
		require.NoError(t, cc.AppendTurn(map[string]any{"type": typ, "uuid": id, "parentUuid": parent, "cwd": cwd, "timestamp": "2026-10-01T10:00:00.000Z", "message": msg}))
	}
	turn("user", "u1", "", map[string]any{"role": "user", "content": morphSecret})
	turn("assistant", "a1", "u1", map[string]any{"id": "m1", "role": "assistant", "model": "claude-opus-5-5", "content": []any{
		map[string]any{"type": "thinking", "thinking": "private reasoning", "signature": "sig"},
		map[string]any{"type": "tool_use", "id": "toolu_1", "name": "Bash", "input": map[string]any{"command": "ls"}}}})
	turn("user", "u2", "a1", map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": "toolu_1", "content": "README.md\nmain.go"}}})
	turn("assistant", "a2", "u2", map[string]any{"id": "m2", "role": "assistant", "model": "claude-opus-5-5", "content": []any{map[string]any{"type": "text", "text": "There are two files."}}})

	// The conv watcher re-indexes a grown transcript; do what it does.
	convops.ScanAndUpsertFile(cc.JsonlPath)

	claude, codex := mustGetHarness(t, harness.DefaultName), mustGetHarness(t, harness.CodexName)
	before, err := claude.History.Export(source, cwd)
	require.NoError(t, err)

	preview, err := harness.PreviewTranslation(claude, codex, source, cwd, harness.TranslateOptions{})
	require.NoError(t, err)
	assert.Equal(t, "native", preview.Fallback)
	assert.Equal(t, 1, preview.Count(transcript.LossReasoningDropped))

	artifacts := filepath.Join(testutil.CanonicalTempDir(t), "morph")
	res, err := harness.TranslateHistory(claude, codex, source, cwd, harness.TranslateOptions{ArtifactDir: artifacts})
	require.NoError(t, err)
	assert.NotEqual(t, source, res.ConvID)
	assert.Equal(t, preview.Items, res.Report.Items, "the preview reports exactly what the apply does")

	// Codex resumes by id: the rollout is found in its date tree and the
	// next turn appends to the same file.
	cx := testharness.HydrateCodexSim(t, f.World.HomeDir, res.ConvID, cwd)
	_, statErr := os.Stat(cx.RolloutPath)
	require.NoError(t, statErr, "codex resume locates the translated rollout by id")
	require.NoError(t, cx.Start())
	require.NoError(t, cx.WriteExchange("What was the secret word?", "PELICAN"))

	entries, err := codex.Convs.ListConvs("")
	require.NoError(t, err)
	var listed bool
	for _, e := range entries {
		if e.SessionID == res.ConvID {
			listed = true
			assert.Equal(t, harness.CodexName, e.Harness)
			assert.Equal(t, cwd, e.ProjectPath)
			assert.Equal(t, morphSecret, e.FirstPrompt)
		}
	}
	require.True(t, listed, "the translated conversation lists as one Codex conversation")
	ref, err := codex.Convs.Resolve(res.ConvID, cwd, false)
	require.NoError(t, err)
	require.NotNil(t, ref)

	text := historyText(t, codex, res.ConvID, cwd)
	for _, want := range []string{morphSecret, "⟦tool call · Bash⟧", "README.md\nmain.go", "There are two files.", "carried over from claude", "What was the secret word?"} {
		assert.Contains(t, text, want)
	}
	assert.NotContains(t, text, "private reasoning")

	after, err := claude.History.Export(source, cwd)
	require.NoError(t, err)
	assert.Equal(t, before, after, "the source conversation is only read")

	md, err := os.ReadFile(filepath.Join(artifacts, "transcript.md"))
	require.NoError(t, err)
	assert.Contains(t, string(md), "private reasoning", "the kept transcript file is complete")
	var report transcript.LossReport
	raw, err := os.ReadFile(filepath.Join(artifacts, "loss-report.json"))
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(raw, &report))
	assert.Equal(t, "codex", report.Target)
	assert.Equal(t, "native", report.Fallback)

	res.Cleanup()
	_, err = os.Stat(cx.RolloutPath)
	assert.True(t, os.IsNotExist(err), "cleanup removes only the installed translation")
}

func TestMorphTranslateCodexToClaudeResumesNatively(t *testing.T) {
	f := newFlow(t)
	t.Setenv("CODEX_HOME", "")
	const source = "019fe740-43a4-7023-b8ae-1ee64459f2b7"
	cwd := testutil.CanonicalTempDir(t)
	cx := f.HaveAliveCodexSession(source, "morph-codex", "morph-codex-pane", cwd)
	require.NoError(t, cx.WriteExchange(morphSecret, "Noted."))
	require.NoError(t, cx.WriteUserInput("List the files."))
	require.NoError(t, cx.AppendResponseItem(map[string]any{"type": "reasoning", "summary": []any{}, "encrypted_content": "gAAAA"}))
	require.NoError(t, cx.AppendResponseItem(map[string]any{"type": "function_call", "name": "exec_command", "arguments": `{"cmd":"ls"}`, "call_id": "call_1"}))
	require.NoError(t, cx.AppendResponseItem(map[string]any{"type": "function_call_output", "call_id": "call_1", "output": "README.md\nmain.go"}))
	require.NoError(t, cx.WriteAgentMessage("There are two files."))

	codex, claude := mustGetHarness(t, harness.CodexName), mustGetHarness(t, harness.DefaultName)
	res, err := harness.TranslateHistory(codex, claude, source, cwd, harness.TranslateOptions{})
	require.NoError(t, err)

	// Claude Code resumes by id from the project directory; the next turn
	// appends to the same JSONL.
	cc := testharness.HydrateCCSim(t, f.World.HomeDir, res.ConvID, cwd)
	_, statErr := os.Stat(cc.JsonlPath)
	require.NoError(t, statErr, "claude resume locates the translated transcript by id")
	require.NoError(t, cc.Start())
	require.NoError(t, cc.WriteUserTurn("What was the secret word?"))

	ref, err := claude.Convs.Resolve(res.ConvID, cwd, false)
	require.NoError(t, err)
	require.NotNil(t, ref)
	assert.Equal(t, cwd, ref.ProjectPath)

	text := historyText(t, claude, res.ConvID, cwd)
	for _, want := range []string{morphSecret, "Noted.", "⟦tool call · exec_command⟧", "README.md\nmain.go", "There are two files.", "carried over from codex", "What was the secret word?"} {
		assert.Contains(t, text, want)
	}
	assert.Equal(t, 1, res.Report.Count(transcript.LossReasoningDropped))
	assert.Zero(t, res.Report.Count(transcript.LossUnknownRecord), res.Report.Lines())
}

func mustGetHarness(t *testing.T, name string) *harness.Harness {
	t.Helper()
	h, ok := harness.Get(name)
	require.True(t, ok)
	return h
}

// The context-file rung: when a translated history cannot be installed
// natively, the destination still gets a native conversation, whose one turn
// points at the history saved as files.
func TestMorphTranslateContextFileFallbackResumes(t *testing.T) {
	f := newFlow(t)
	t.Setenv("CODEX_HOME", "")
	const source = "0f0e0d0c-0b0a-4908-8706-050403020102"
	cwd := testutil.CanonicalTempDir(t)
	f.HaveAliveSession(source, "ctx-src", "ctx-pane", cwd)
	cc := f.World.CCs.GetByLabel("ctx-src")
	require.NotNil(t, cc)
	require.NoError(t, cc.AppendTurn(map[string]any{"type": "user", "uuid": "u1", "parentUuid": "", "cwd": cwd,
		"timestamp": "2026-10-01T10:00:00.000Z", "message": map[string]any{"role": "user", "content": morphSecret}}))
	convops.ScanAndUpsertFile(cc.JsonlPath)

	claude, codex := mustGetHarness(t, harness.DefaultName), mustGetHarness(t, harness.CodexName)
	artifacts := filepath.Join(testutil.CanonicalTempDir(t), "morph")
	opts := harness.TranslateOptions{ArtifactDir: artifacts}
	_, plan, err := harness.PlanTranslation(claude, codex, source, cwd, opts)
	require.NoError(t, err)
	res, err := harness.InstallContextFile(codex, plan, cwd, artifacts)
	require.NoError(t, err)
	assert.Equal(t, "context_file", res.Report.Fallback)

	briefing, err := os.ReadFile(filepath.Join(artifacts, "briefing.md"))
	require.NoError(t, err)
	assert.Contains(t, string(briefing), morphSecret, "the briefing carries the budgeted history")
	cx := testharness.HydrateCodexSim(t, f.World.HomeDir, res.ConvID, cwd)
	require.NoError(t, cx.Start())
	text := historyText(t, codex, res.ConvID, cwd)
	assert.Contains(t, text, filepath.Join(artifacts, "briefing.md"), "the one native turn points at the briefing")
	assert.NotContains(t, text, morphSecret, "the history itself stays in the files")
	var report transcript.LossReport
	raw, err := os.ReadFile(filepath.Join(artifacts, "loss-report.json"))
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(raw, &report))
	assert.Equal(t, "context_file", report.Fallback)
}
