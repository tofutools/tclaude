package pifixture

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
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
	// One response puts ~12k tokens in context (the mock's usage).
	w.ExtraEnv = []string{harness.AutoCompactWindowEnvVar + "=10000"}
	id := uuid.NewString()
	rpc := w.StartRPC(w.Project, "--session-id", id, "--extension", ext, "--model", MockModel)
	require.Equal(t, true, rpc.Send(map[string]any{"type": "prompt", "message": "please run TOOL now"})["success"])
	rpc.AwaitEvent("agent_settled")
	rpc.AwaitEvent("compaction_end")
	// tclaude's footer segment measures the context against the pin: 0 at
	// startup, ~12k of 10k when the run settles.
	segment := func(pattern string) {
		t.Helper()
		re := regexp.MustCompile(pattern)
		rpc.Await(func(line map[string]any) bool {
			return line["type"] == "extension_ui_request" && line["method"] == "setStatus" &&
				line["statusKey"] == "tclaude" && re.MatchString(fmt.Sprint(line["statusText"]))
		})
	}
	segment(`^tclaude · ctx 0% of 10k$`)
	segment(`^tclaude · ctx 1[0-9]{2}% of 10k$`)
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
	assert.EqualValues(t, 24000, spend["input_tokens"])
	assert.EqualValues(t, 60, spend["output_tokens"])
	assert.InDelta(t, 0.02412, spend["cost_usd"], 1e-9)
	assert.Equal(t, false, spend["subscription"], "an API key is billed per token")
}

// A session tclaude copies (clone, agent bundle, federation) starts at the
// import: its copied history's spend stays with the source, and the copy
// reports only its own.
func TestPiImportedSessionReportsOnlyItsOwnSpend(t *testing.T) {
	w := NewWorld(t)
	ext := materializedExtension(t)
	db.ResetForTest()
	t.Cleanup(db.ResetForTest)
	t.Setenv(harness.PiAgentDirEnvVar, w.AgentDir)
	t.Setenv(harness.PiSessionDirEnvVar, "")

	source := uuid.NewString()
	out, err := w.Run(w.Project, "-p", "first", "--session-id", source, "--model", MockModel)
	require.NoError(t, err, "pi failed:\n%s", out)
	pi := harness.MustGet(harness.PiName)
	raw, err := pi.History.Export(source, w.Project)
	require.NoError(t, err)
	id, cleanup, err := pi.History.Import(raw, source, w.Project)
	require.NoError(t, err)
	t.Cleanup(cleanup)

	out, err = w.Run(w.Project, "-p", "second", "--session-id", id, "--extension", ext, "--model", MockModel)
	require.NoError(t, err, "pi failed:\n%s", out)
	var stop map[string]any
	for _, p := range w.RecordedPayloads() {
		if p["hook_event_name"] == "Stop" {
			stop = p
		}
	}
	require.NotNil(t, stop)
	spend, ok := stop["tclaude_usage"].(map[string]any)
	require.True(t, ok, "Stop carries no tclaude_usage: %v", stop)
	assert.EqualValues(t, 1, spend["calls"], "only the copy's own response")
}

// Under the ask posture, tclaude's permission extension asks before pi runs a
// tool that is not read-only: an approved call runs, a declined one reaches
// the model as blocked, the open dialog reports a permission prompt to
// tclaude, and with no human to ask the call is blocked unasked.
func TestPiPermissionExtensionAsksBeforeActing(t *testing.T) {
	w := NewWorld(t)
	ext := materializedExtension(t)
	permissions := harness.PiPermissionExtensionPath(ext)
	id := uuid.NewString()
	rpc := w.StartRPC(w.Project, "--session-id", id, "--extension", ext, "--extension", permissions, "--model", MockModel)

	confirm := func() map[string]any {
		t.Helper()
		return rpc.Await(func(line map[string]any) bool {
			return line["type"] == "extension_ui_request" && line["method"] == "confirm"
		})
	}
	// Each marker is computed by the shell, so the command text the mock
	// echoes back can never satisfy an assertion on its own.
	w.Mock.Command = "echo approved_$((6*7))"
	require.Equal(t, true, rpc.Send(map[string]any{"type": "prompt", "message": "please run TOOL now"})["success"])
	request := confirm()
	assert.Contains(t, request["title"], "bash")
	assert.Contains(t, request["message"], "echo approved_", "the human sees the command")
	rpc.Reply(map[string]any{"type": "extension_ui_response", "id": request["id"], "confirmed": true})
	rpc.AwaitEvent("agent_settled")
	assert.Contains(t, w.Mock.LastMessages(), "approved_42", "the approved command ran")

	w.Mock.Command = "echo declined_$((6*7))"
	require.Equal(t, true, rpc.Send(map[string]any{"type": "prompt", "message": "TOOL again"})["success"])
	request = confirm()
	rpc.Reply(map[string]any{"type": "extension_ui_response", "id": request["id"], "confirmed": false})
	rpc.AwaitEvent("agent_settled")
	result := w.Mock.LastMessages()
	assert.NotContains(t, result, "declined_42", "the declined command did not run")
	assert.Contains(t, result, "the human did not approve bash", "the model is told why")

	// Terminal escapes in the command are shown, never obeyed, so the
	// dialog cannot be made to read as a different command.
	w.Mock.Command = "echo \x1b[8mhidden \u202eevil"
	require.Equal(t, true, rpc.Send(map[string]any{"type": "prompt", "message": "TOOL once more"})["success"])
	request = confirm()
	message, _ := request["message"].(string)
	assert.NotContains(t, message, "\x1b")
	assert.NotContains(t, message, "\u202e")
	assert.Contains(t, message, `\u001b[8mhidden`)
	assert.Contains(t, message, `\u202eevil`)
	rpc.Reply(map[string]any{"type": "extension_ui_response", "id": request["id"], "confirmed": false})
	rpc.AwaitEvent("agent_settled")
	rpc.Close()

	var prompts int
	for _, payload := range w.RecordedPayloads() {
		if payload["hook_event_name"] == "Notification" && payload["notification_type"] == "permission_prompt" {
			prompts++
		}
	}
	assert.Equal(t, 3, prompts, "each open dialog reports a permission prompt")

	w.Mock.Command = "echo unasked_$((6*7))"
	_, err := w.Run(w.Project, "-p", "TOOL in print mode", "--extension", permissions, "--model", MockModel)
	require.NoError(t, err)
	result = w.Mock.LastMessages()
	assert.NotContains(t, result, "unasked_42")
	assert.Contains(t, result, "no human can answer")
}

// `tclaude ask --harness pi`: the asker's argv runs against the real pi, a
// buffered capture prints the answer, a streamed one renders the JSONL event
// stream through the asker's filter, and the second ask continues the thread
// the first one minted.
func TestPiAskBufferedStreamedAndResumed(t *testing.T) {
	w := NewWorld(t)
	t.Setenv(harness.PiAgentDirEnvVar, w.AgentDir)
	h := harness.MustGet(harness.PiName)
	require.True(t, h.PreMintsAskConvID())
	stream, ok := h.Ask.(harness.StreamAsker)
	require.True(t, ok, "pi streams its answers")
	id := uuid.NewString()

	run := func(spec harness.AskSpec) (stdout string) {
		t.Helper()
		argv := h.Ask.BuildAskArgv(spec)
		require.Equal(t, "pi", argv[0])
		cmd := exec.Command(w.Binary, argv[1:]...)
		cmd.Dir = w.Project
		cmd.Env = w.Env()
		var out, errOut strings.Builder
		var sink io.Writer = &out
		if spec.Stream {
			sink = stream.StreamFilter(&out, false, nil)
		}
		cmd.Stdout = sink
		cmd.Stderr = &errOut
		require.NoError(t, cmd.Run(), "pi ask failed: %s", errOut.String())
		if flusher, ok := sink.(harness.AskStreamFlusher); ok {
			require.NoError(t, flusher.Flush())
		}
		return out.String()
	}

	answer := run(harness.AskSpec{Print: true, SessionID: id, Model: MockModel, Prompt: "first question: what is the secret?"})
	assert.Contains(t, answer, "This is the mock reply.")

	streamed := run(harness.AskSpec{Print: true, Stream: true, ResumeID: id, Model: MockModel, Prompt: "second question"})
	assert.Contains(t, streamed, "This is the mock reply.")
	assert.NotContains(t, streamed, `"type"`, "only the answer's text, never the event stream")
	assert.Contains(t, w.Mock.LastMessages(), "first question: what is the secret?", "the second ask continues the first one's thread")

	matches, _ := filepath.Glob(filepath.Join(w.AgentDir, "sessions", "*", "*_"+id+".jsonl"))
	assert.Len(t, matches, 1, "both asks wrote one conversation")
}

// A managed launch's skills and rules, registered as the production spawner
// renders them: the managed skill shadows a same-named skill in the shared
// ~/.agents/skills root, another ambient skill still loads, and the rules are
// in the system prompt before and after a compaction, beside the operator's
// own APPEND_SYSTEM.md.
func TestPiManagedSkillsShadowAmbientAndRulesSurviveCompaction(t *testing.T) {
	w := NewWorld(t)
	home := filepath.Dir(w.AgentDir)
	skill := func(root, name, description string) {
		dir := filepath.Join(root, name)
		require.NoError(t, os.MkdirAll(dir, 0o700))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "SKILL.md"),
			[]byte("---\nname: "+name+"\ndescription: "+description+"\n---\nFollow the steps.\n"), 0o600))
	}
	ambient := filepath.Join(home, ".agents", "skills")
	skill(ambient, "review", "AMBIENT-REVIEW-SKILL")
	skill(ambient, "deploy", "AMBIENT-DEPLOY-SKILL")
	managed := filepath.Join(home, "tclaude-managed-fixture")
	skill(filepath.Join(managed, "skills"), "review", "MANAGED-REVIEW-SKILL")
	require.NoError(t, os.WriteFile(filepath.Join(managed, "RULES.md"), []byte("MANAGED-RULE-PINEAPPLE"), 0o400))
	require.NoError(t, os.WriteFile(filepath.Join(w.AgentDir, "APPEND_SYSTEM.md"), []byte("OPERATOR-APPEND-MANGO"), 0o600))

	id := uuid.NewString()
	launch, err := testharness.ParsePiLaunch(harness.MustGet(harness.PiName).Spawn.BuildCommand(harness.SpawnSpec{
		SessionID:               id,
		ApprovalPolicy:          harness.PiApprovalAuto,
		StatusExtensionPath:     materializedExtension(t),
		TclaudeExecutable:       w.Recorder,
		ManagedInstructionsRoot: managed,
	}))
	require.NoError(t, err)
	require.Len(t, launch.Extensions, 2)
	require.Equal(t, managed, launch.Extensions[1])
	w.ExtraEnv = append(w.ExtraEnv, harness.PiManagedRulesEnvVar+"="+launch.Env[harness.PiManagedRulesEnvVar])
	args := []string{"--session-id", launch.SessionID, "--model", MockModel}
	for _, extension := range launch.Extensions {
		args = append(args, "--extension", extension)
	}
	for _, prompt := range launch.AppendSystemPrompt {
		args = append(args, "--append-system-prompt", prompt)
	}
	rpc := w.StartRPC(w.Project, args...)
	turn := func(message string) string {
		t.Helper()
		require.Equal(t, true, rpc.Send(map[string]any{"type": "prompt", "message": message})["success"])
		rpc.AwaitEvent("agent_settled")
		return w.Mock.LastMessages()
	}
	check := func(sent, when string) {
		t.Helper()
		assert.Contains(t, sent, "MANAGED-REVIEW-SKILL", "%s: the managed skill is offered", when)
		assert.NotContains(t, sent, "AMBIENT-REVIEW-SKILL", "%s: it shadows the ambient one", when)
		assert.Contains(t, sent, "AMBIENT-DEPLOY-SKILL", "%s: other ambient skills still load", when)
		assert.Contains(t, sent, "MANAGED-RULE-PINEAPPLE", "%s: the rules are in the system prompt", when)
		assert.Contains(t, sent, "OPERATOR-APPEND-MANGO", "%s: the operator's APPEND_SYSTEM.md still loads", when)
	}
	check(turn("first turn: run TOOL"), "before compaction")
	compacted := rpc.Send(map[string]any{"type": "compact"})
	require.Equalf(t, true, compacted["success"], "compact: %v", compacted)
	check(turn("after the compaction"), "after compaction")
	rpc.Close()
}

// A refused pinned-window compaction must retain the history and emit only
// PreCompact(auto), never claim a completed compaction to the daemon.
func TestPiThresholdCompactionRefusal(t *testing.T) {
	w := NewWorld(t)
	ext := materializedExtension(t)
	w.ExtraEnv = []string{harness.AutoCompactWindowEnvVar + "=10000"}
	recorder := "#!/bin/sh\n[ \"$1 $2\" = \"session hook-callback\" ] || exit 64\ncat >> " + shellQuote(w.Payloads) + "\nprintf '%s\\n' '{\"decision\":\"block\"}'\n"
	require.NoError(t, os.WriteFile(w.Recorder, []byte(recorder), 0o700))
	id := uuid.NewString()
	rpc := w.StartRPC(w.Project, "--session-id", id, "--extension", ext, "--model", MockModel)
	require.Equal(t, true, rpc.Send(map[string]any{"type": "prompt", "message": "please run TOOL now"})["success"])
	rpc.AwaitEvent("agent_settled")
	rpc.AwaitEvent("compaction_end")
	rpc.Close()
	pre := 0
	for _, payload := range w.RecordedPayloads() {
		switch payload["hook_event_name"] {
		case "PreCompact":
			pre++
			require.Equal(t, "auto", payload["trigger"])
		case "PostCompact":
			t.Fatal("refused compaction emitted PostCompact")
		}
	}
	require.Equal(t, 1, pre, "the hook must actually receive the threshold decision")
	files, err := filepath.Glob(filepath.Join(w.AgentDir, "sessions", "*", "*_"+id+".jsonl"))
	require.NoError(t, err)
	require.Len(t, files, 1)
	raw, err := os.ReadFile(files[0])
	require.NoError(t, err)
	require.NotContains(t, string(raw), `"type":"compaction"`)
}

func TestPiSelectDialogReportsElicitation(t *testing.T) {
	w := NewWorld(t)
	ext := materializedExtension(t)
	probe := filepath.Join(w.Project, "select-probe.ts")
	require.NoError(t, os.WriteFile(probe, []byte(`export default function(pi: any) {
 pi.registerCommand("test-select", {handler: async (_args: string, ctx: any) => {
  await ctx.ui.select("Pick a color", ["red", "blue"]);
 }});
}
`), 0o600))
	rpc := w.StartRPC(w.Project, "--session-id", uuid.NewString(), "--extension", ext, "--extension", probe, "--model", MockModel)
	// Extension commands acknowledge after their handler completes; send
	// without waiting so the test can answer the dialog first.
	rpc.Reply(map[string]any{"type": "prompt", "message": "/test-select"})
	dialog := rpc.Await(func(line map[string]any) bool {
		return line["type"] == "extension_ui_request" && line["method"] == "select"
	})
	rpc.Reply(map[string]any{"type": "extension_ui_response", "id": dialog["id"], "value": "red"})
	ack := rpc.Await(func(line map[string]any) bool { return line["type"] == "response" && line["command"] == "prompt" })
	require.Equal(t, true, ack["success"])
	rpc.Close()
	var notification map[string]any
	for _, payload := range w.RecordedPayloads() {
		if payload["hook_event_name"] == "Notification" {
			notification = payload
		}
	}
	require.NotNil(t, notification, "the real select must report a UI prompt")
	require.Equal(t, "elicitation_dialog", notification["notification_type"])
}
