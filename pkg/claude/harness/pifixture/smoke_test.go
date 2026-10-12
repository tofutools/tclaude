package pifixture

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/claude/harness/transcript"
	"github.com/tofutools/tclaude/pkg/testharness"
	"github.com/tofutools/tclaude/pkg/testutil"
)

func materializedExtension(t *testing.T) string {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	path, err := harness.MaterializePiExtension()
	require.NoError(t, err)
	return path
}

// normalize round-trips through JSON so Go-built and recorded payloads
// compare as the same types.
func normalize(t *testing.T, v any) map[string]any {
	t.Helper()
	raw, err := json.Marshal(v)
	require.NoError(t, err)
	var out map[string]any
	require.NoError(t, json.Unmarshal(raw, &out))
	return out
}

// The extension's payloads must be exactly what PiSim hands the hook callback
// in flow tests, or those tests prove nothing about a real pane. One turn with
// a tool call covers start, prompt, both tool events, the settled turn and the
// end of the session.
func TestPiExtensionPayloadFidelity(t *testing.T) {
	w := NewWorld(t)
	ext := materializedExtension(t)
	id := uuid.NewString()
	prompt := "please run TOOL now"
	out, err := w.Run(w.Project, "-p", prompt, "--session-id", id, "--extension", ext, "--model", MockModel)
	require.NoError(t, err, "pi failed:\n%s", out)

	got := w.RecordedPayloads()
	require.Len(t, got, 6, "payloads: %v", got)
	transcript := got[0]["transcript_path"].(string)
	assert.Equal(t, filepath.Join(w.AgentDir, "sessions", testharness.PiSessionDirName(w.Project)),
		filepath.Dir(transcript), "PiSim writes sessions where pi does")
	assert.True(t, strings.HasSuffix(transcript, "_"+id+".jsonl"))

	want := []struct {
		event  string
		fields map[string]any
	}{
		{"SessionStart", map[string]any{"source": "startup"}},
		{"UserPromptSubmit", map[string]any{"prompt": prompt}},
		{"PreToolUse", map[string]any{"tool_name": "bash", "tool_input": map[string]any{"command": ToolCommand}}},
		{"PostToolUse", map[string]any{"tool_name": "bash"}},
		{"Stop", nil},
		{"SessionEnd", map[string]any{"reason": "prompt_input_exit"}},
	}
	for i, step := range want {
		recorded := got[i]
		// The context meter reading is the extension's own measurement; P4
		// consumes it. Check its shape here and compare the rest exactly.
		ctxUsage, ok := recorded["tclaude_context"].(map[string]any)
		require.Truef(t, ok, "%s carries no tclaude_context: %v", step.event, recorded)
		assert.EqualValues(t, 64000, ctxUsage["context_window"], step.event)
		delete(recorded, "tclaude_context")
		// Spend is reported once the session has some: from the first
		// response, the tool call, on. Its figures are checked in
		// TestPiExtensionReportsSpendAndCompactsAtPinnedWindow.
		_, spent := recorded["tclaude_usage"]
		assert.Equal(t, i >= 2, spent, "%s: tclaude_usage", step.event)
		delete(recorded, "tclaude_usage")
		expected := normalize(t, testharness.PiHookPayload(id, transcript, w.Project, MockModel, step.event, step.fields))
		assert.Equalf(t, expected, recorded, "payload %d (%s)", i, step.event)
	}
}

// A session id tclaude presets is created when absent, and a session file
// path resumes from any directory without pi's cross-project fork prompt.
func TestPiSessionIdentityFlags(t *testing.T) {
	w := NewWorld(t)
	id := uuid.NewString()
	out, err := w.Run(w.Project, "-p", "first", "--session-id", id, "--model", MockModel)
	require.NoError(t, err, "pi failed:\n%s", out)
	matches, _ := filepath.Glob(filepath.Join(w.AgentDir, "sessions", "*", "*_"+id+".jsonl"))
	require.Len(t, matches, 1, "--session-id must create exactly one session file")

	elsewhere := filepath.Join(filepath.Dir(w.Project), "elsewhere")
	require.NoError(t, os.MkdirAll(elsewhere, 0o700))
	out, err = w.Run(elsewhere, "-p", "second", "--session", matches[0], "--model", MockModel)
	require.NoError(t, err, "resume by path from another cwd failed:\n%s", out)
	raw, err := os.ReadFile(matches[0])
	require.NoError(t, err)
	assert.Contains(t, string(raw), `"second"`, "the resumed turn lands in the original session")
	assert.Equal(t, 1, strings.Count(string(raw), `"type":"session"`), "no fork: one session header")
}

// The version gate must accept the pinned binary's own --version output.
func TestPiVersionGateAcceptsPinnedBinary(t *testing.T) {
	w := NewWorld(t)
	require.NoError(t, harness.CheckPiVersion(w.Binary))
}

// Everything else PiSim feeds the flow tests, driven headlessly through pi's
// RPC mode: a turn, a manual compaction, /clone and /new rotating the session,
// an aborted turn, slash text that is a model turn, and the end of the session.
func TestPiExtensionLifecycleFidelity(t *testing.T) {
	w := NewWorld(t)
	ext := materializedExtension(t)
	promptsDir := filepath.Join(w.Project, ".pi", "prompts")
	require.NoError(t, os.MkdirAll(promptsDir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(promptsDir, "greet.md"), []byte("Say hello\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(w.AgentDir, "trust.json"),
		[]byte("{\n  "+strconvQuote(w.Project)+": true\n}\n"), 0o600))
	id := uuid.NewString()
	rpc := w.StartRPC(w.Project, "--session-id", id, "--extension", ext, "--model", MockModel)

	turn := func(message string) {
		t.Helper()
		require.Equal(t, true, rpc.Send(map[string]any{"type": "prompt", "message": message})["success"])
		rpc.AwaitEvent("agent_settled")
	}
	// A turn with a tool call leaves history older than what compaction
	// keeps verbatim.
	first := "first turn: run TOOL"
	turn(first)
	compacted := rpc.Send(map[string]any{"type": "compact"})
	require.Equalf(t, true, compacted["success"], "compact: %v", compacted)
	require.Equal(t, true, rpc.Send(map[string]any{"type": "clone"})["success"])
	require.Equal(t, true, rpc.Send(map[string]any{"type": "new_session"})["success"])
	require.Equal(t, true, rpc.Send(map[string]any{"type": "prompt", "message": "SLOW turn"})["success"])
	rpc.AwaitEvent("agent_start")
	require.Equal(t, true, rpc.Send(map[string]any{"type": "abort"})["success"])
	rpc.AwaitEvent("agent_settled")
	turn("/greet")
	rpc.Close()

	got := w.RecordedPayloads()
	var events []string
	for _, p := range got {
		events = append(events, p["hook_event_name"].(string))
	}
	want := []struct {
		event  string
		fields map[string]any
	}{
		{"SessionStart", map[string]any{"source": "startup"}},
		{"UserPromptSubmit", map[string]any{"prompt": first}},
		{"PreToolUse", map[string]any{"tool_name": "bash", "tool_input": map[string]any{"command": ToolCommand}}},
		{"PostToolUse", map[string]any{"tool_name": "bash"}},
		{"Stop", nil},
		{"PreCompact", map[string]any{"trigger": "manual"}},
		{"PostCompact", map[string]any{"trigger": "manual"}},
		{"SessionEnd", map[string]any{"reason": "clear"}},
		{"SessionStart", map[string]any{"source": "clear"}},
		{"SessionEnd", map[string]any{"reason": "clear"}},
		{"SessionStart", map[string]any{"source": "clear"}},
		{"UserPromptSubmit", map[string]any{"prompt": "SLOW turn"}},
		{"Stop", nil},
		{"UserPromptSubmit", map[string]any{"prompt": "/greet"}},
		{"Stop", nil},
		{"SessionEnd", map[string]any{"reason": "prompt_input_exit"}},
	}
	require.Lenf(t, got, len(want), "events: %v", events)
	ids := map[string]bool{}
	for i, step := range want {
		recorded := got[i]
		delete(recorded, "tclaude_context")
		delete(recorded, "tclaude_usage")
		convID, _ := recorded["session_id"].(string)
		transcript, _ := recorded["transcript_path"].(string)
		ids[convID] = true
		assert.Truef(t, strings.HasSuffix(transcript, "_"+convID+".jsonl"), "%s: transcript %s for %s", step.event, transcript, convID)
		expected := normalize(t, testharness.PiHookPayload(convID, transcript, w.Project, MockModel, step.event, step.fields))
		assert.Equalf(t, expected, recorded, "payload %d (%s)", i, step.event)
	}
	assert.Equal(t, id, got[0]["session_id"], "the preset id is the first session")
	assert.Len(t, ids, 3, "/clone and /new each move to a new session id")
	assert.Equal(t, got[7]["session_id"], got[0]["session_id"], "SessionEnd names the session being left")
	assert.NotEqual(t, got[8]["session_id"], got[7]["session_id"], "SessionStart names the session entered")
}

func strconvQuote(s string) string {
	raw, _ := json.Marshal(s)
	return string(raw)
}

// A history translated from another harness lands where pi looks for the
// project's sessions, and resuming it by id sends the carried turns to the
// model.
func TestPiLandedTranscriptResumes(t *testing.T) {
	w := NewWorld(t)
	t.Setenv("HOME", testutil.CanonicalTempDir(t))
	db.ResetForTest()
	t.Cleanup(db.ResetForTest)
	t.Setenv(harness.PiAgentDirEnvVar, w.AgentDir)
	t.Setenv(harness.PiSessionDirEnvVar, "")

	src := transcript.New(transcript.Source{Harness: harness.DefaultName, ConvID: "source-conv"})
	src.Append(transcript.Entry{Role: transcript.RoleUser, Origin: transcript.OriginHuman, Blocks: []transcript.Block{transcript.Text("Remember the secret word: PELICAN.")}})
	src.Append(transcript.Entry{Role: transcript.RoleAssistant, Origin: transcript.OriginAgent, Blocks: []transcript.Block{transcript.Text("Noted, I will remember it.")}})
	pi := harness.MustGet(harness.PiName)
	plan := transcript.NewPlan(src, transcript.Options{Target: harness.PiName, TargetModel: MockModel, InlineMedia: true})
	var encoded bytes.Buffer
	sourceID, err := pi.Transcript.Encode(plan, w.Project, &encoded)
	require.NoError(t, err)
	id, cleanup, err := pi.History.Import(encoded.Bytes(), sourceID, w.Project)
	require.NoError(t, err)
	t.Cleanup(cleanup)

	out, err := w.Run(w.Project, "-p", "What was the secret word?", "--session-id", id, "--model", MockModel)
	require.NoError(t, err, "pi failed:\n%s", out)
	sent := w.Mock.LastMessages()
	assert.Contains(t, sent, "PELICAN", "the carried turn reaches the model")
	assert.Contains(t, sent, "Noted, I will remember it.")
	assert.Contains(t, sent, "What was the secret word?")
	matches, _ := filepath.Glob(filepath.Join(w.AgentDir, "sessions", "*", "*_"+id+".jsonl"))
	require.Len(t, matches, 1, "pi resumed the landed session rather than starting another")
}

// The extension reports what the session itself spent, priced from pi's
// catalog, and compacts between runs once the context reaches a pinned
// auto-compaction window, which tclaude hears as an automatic compaction.
func TestPiExtensionReportsSpendAndCompactsAtPinnedWindow(t *testing.T) {
	w := NewWorld(t)
	ext := materializedExtension(t)
	// One response puts ~1230 tokens in context (the mock's usage).
	w.ExtraEnv = []string{harness.AutoCompactWindowEnvVar + "=1000"}
	id := uuid.NewString()
	rpc := w.StartRPC(w.Project, "--session-id", id, "--extension", ext, "--model", MockModel)
	require.Equal(t, true, rpc.Send(map[string]any{"type": "prompt", "message": "please run TOOL now"})["success"])
	rpc.AwaitEvent("agent_settled")
	rpc.AwaitEvent("compaction_end")
	rpc.Close()

	got := w.RecordedPayloads()
	byEvent := map[string]map[string]any{}
	var events []string
	for _, p := range got {
		event := p["hook_event_name"].(string)
		events = append(events, event)
		if _, seen := byEvent[event]; !seen {
			byEvent[event] = p
		}
	}
	require.Contains(t, byEvent, "PreCompact", "events: %v", events)
	assert.Equal(t, "auto", byEvent["PreCompact"]["trigger"])
	require.Contains(t, byEvent, "PostCompact", "events: %v", events)
	assert.Equal(t, "auto", byEvent["PostCompact"]["trigger"])

	// Two responses (the tool call, then the answer) at $1/$2 per million.
	spend, ok := byEvent["Stop"]["tclaude_usage"].(map[string]any)
	require.True(t, ok, "Stop carries no tclaude_usage: %v", byEvent["Stop"])
	assert.EqualValues(t, 2, spend["calls"])
	assert.EqualValues(t, 2400, spend["input_tokens"])
	assert.EqualValues(t, 60, spend["output_tokens"])
	assert.InDelta(t, 0.00252, spend["cost_usd"], 1e-9)
	assert.Equal(t, false, spend["subscription"], "an API key is billed per token")
}
