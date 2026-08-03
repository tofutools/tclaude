package session_test

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/claude/harness/copilotfixture"
	"github.com/tofutools/tclaude/pkg/claude/session"
)

// tclaude decodes GitHub Copilot's hook payloads with the SAME struct it uses
// for Claude Code and Codex — no translator, no second set of json tags. That
// only works because Copilot answers Claude Code's event names with Claude
// Code's payload, which is an undocumented runtime behavior rather than a
// published contract.
//
// This file is what keeps that claim honest. Every case below decodes bytes
// RECORDED FROM THE REAL PINNED CLI (pkg/claude/harness/copilotfixture/
// testdata/1.0.77/hooks), not a hand-written literal. If a future Copilot
// renames a field or changes a type, these fail rather than tclaude silently
// enrolling empty conv-ids.

func decodeCopilotHook(t *testing.T, payload json.RawMessage) session.HookCallbackInput {
	t.Helper()
	var in session.HookCallbackInput
	require.NoError(t, json.Unmarshal(payload, &in), "payload: %s", payload)
	return in
}

// TestCopilotHookPayloads_DecodeCanonically walks the whole recorded scenario
// and asserts each installed event lands in the canonical fields tclaude's
// status machine reads.
func TestCopilotHookPayloads_DecodeCanonically(t *testing.T) {
	capture := copilotfixture.LoadHookCapture(t, copilotfixture.HookScenarioClaudeDialect)

	// The recorded firing order, which is NOT the order any other harness
	// uses: Copilot announces the prompt before it announces the session.
	assert.Equal(t, []string{
		"UserPromptSubmit", "SessionStart", "PreToolUse", "PermissionRequest",
		"PostToolUse", "Stop", "SessionEnd",
		"UserPromptSubmit", "SessionStart", "Stop", "SessionEnd",
	}, capture.EventNames(),
		"recorded order is evidence: UserPromptSubmit fires BEFORE SessionStart")

	for _, event := range harness.CopilotHookEvents {
		payloads := capture.FindAll(event)
		require.NotEmpty(t, payloads, "no recorded payload for installed event %s", event)
		for _, payload := range payloads {
			in := decodeCopilotHook(t, payload)
			assert.Equal(t, event, in.HookEventName,
				"hook_event_name must identify the event tclaude registered")
			assert.Equal(t, copilotfixture.HookSessionIDPlaceholder, in.ConvID,
				"session_id must decode into the conv-id every event is keyed on")
			assert.Equal(t, copilotfixture.HookCwdPlaceholder, in.Cwd,
				"cwd must decode on every event, not just SessionStart")
		}
	}
}

// TestCopilotHookPayloads_SessionStartSource pins the fresh/resume
// discriminator. tclaude reads `source` to tell a NEW process booting
// ("startup"-like) from an in-process conversation switch, and Copilot fires
// SessionStart on a resumed launch too — with the same session id and the NEW
// prompt in initial_prompt.
func TestCopilotHookPayloads_SessionStartSource(t *testing.T) {
	capture := copilotfixture.LoadHookCapture(t, copilotfixture.HookScenarioClaudeDialect)
	starts := capture.FindAll("SessionStart")
	require.Len(t, starts, 2, "the scenario records a fresh launch and a resume")

	fresh := decodeCopilotHook(t, starts[0])
	assert.Equal(t, "new", fresh.Source,
		"Copilot spells a fresh launch \"new\", not Claude Code's \"startup\"")

	resumed := decodeCopilotHook(t, starts[1])
	assert.Equal(t, "resume", resumed.Source)
	assert.Equal(t, fresh.ConvID, resumed.ConvID,
		"a resumed session keeps its id, which is what makes --resume=<id> enrollable")
}

// TestCopilotHookPayloads_ToolFieldsAreCanonical covers the fields the workdir
// and branch tracking in applyHook actually reads. The dialect matters here
// more than anywhere else: Copilot's own camelCase payload delivers tool
// arguments as a JSON-encoded STRING and its own lowercase tool names, while
// the dialect tclaude registers delivers a real object and Claude's tool names
// — which is why no translation is needed.
func TestCopilotHookPayloads_ToolFieldsAreCanonical(t *testing.T) {
	capture := copilotfixture.LoadHookCapture(t, copilotfixture.HookScenarioClaudeDialect)
	payload, ok := capture.Find("PostToolUse")
	require.True(t, ok)

	in := decodeCopilotHook(t, payload)
	assert.Equal(t, "Bash", in.ToolName,
		"the dialect translates tool names to Claude's, so tool handling needs no mapping")

	var toolInput map[string]any
	require.NoError(t, json.Unmarshal(in.ToolInput, &toolInput),
		"tool_input must be an OBJECT; a JSON-encoded string would silently mis-parse")
	assert.Equal(t, "echo hookprobe", toolInput["command"])

	// The one field where the dialect is NOT field-for-field: Copilot spells
	// the tool result `tool_result`, while Claude Code's canonical field is
	// `tool_response`, so it decodes as empty.
	//
	// That costs nothing today and is recorded rather than papered over.
	// tclaude reads ToolResponse for exactly one purpose — pulling
	// backgroundTaskId out of a Claude Code `Bash` call launched with
	// run_in_background (see bgshell.go) — and Copilot has no background-shell
	// mechanism to track. Adding a `tool_result` alias to the canonical struct
	// would put a second spelling on the broker wire to gain a field nothing
	// reads. If Copilot ever grows background shells, this is the line to
	// revisit; until then the gap belongs on TCL-976's list, not in the wire.
	assert.Empty(t, in.ToolResponse,
		"tool_result is Copilot's spelling; the canonical field is tool_response")
	var fields map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(payload, &fields))
	require.Contains(t, fields, "tool_result", "the result is present, just under Copilot's name")
	var toolResult map[string]any
	require.NoError(t, json.Unmarshal(fields["tool_result"], &toolResult))
	assert.Equal(t, "success", toolResult["result_type"])
}

// TestCopilotHookPayloads_StopEndsTheTurn pins the event the whole integration
// hangs on. Without a turn-end event tclaude would have to choose between an
// agent stuck at "working" forever and no status at all.
func TestCopilotHookPayloads_StopEndsTheTurn(t *testing.T) {
	capture := copilotfixture.LoadHookCapture(t, copilotfixture.HookScenarioClaudeDialect)
	stops := capture.FindAll("Stop")
	require.Len(t, stops, 2, "one Stop per turn — the tool round trip did not add a second")

	in := decodeCopilotHook(t, stops[0])
	assert.Equal(t, "Stop", in.HookEventName)
	assert.False(t, in.StopHookActive,
		"stop_hook_active decodes as the bool the re-entrancy guard expects")

	var fields map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(stops[0], &fields))
	assert.Contains(t, fields, "transcript_path",
		"Stop carries the session's event log path — recorded for TCL-976, unused here")
}

// TestCopilotHookPayloads_SessionEndIsAnExit checks the reason field against
// sessionEndIsExit's semantics: Copilot's "complete" is not one of Claude
// Code's two non-exit reasons ("clear", "resume"), so it falls to the exit
// side, which is the safe direction. Exit AUTHORITY still stays with the
// reaper — SessionEnd has only ever been observed on a clean run.
func TestCopilotHookPayloads_SessionEndIsAnExit(t *testing.T) {
	capture := copilotfixture.LoadHookCapture(t, copilotfixture.HookScenarioClaudeDialect)
	payload, ok := capture.Find("SessionEnd")
	require.True(t, ok)

	in := decodeCopilotHook(t, payload)
	assert.Equal(t, "complete", in.Reason)
	assert.NotContains(t, []string{"clear", "resume"}, in.Reason,
		"a reason tclaude treats as an in-process transition would leave a dead session idle")
}

// TestCopilotHookPayloads_NegativeEvidence documents the two events tclaude
// deliberately does not install, using their own recorded payloads.
//
// PermissionRequest is the sharper of the two: registered under its PascalCase
// Claude name it STILL answers in Copilot's camelCase dialect, so the canonical
// struct decodes almost nothing from it — no event name, no conv-id. Enrolling
// it would not merely be wrong about permissions, it would be wrong about which
// session it belonged to.
func TestCopilotHookPayloads_NegativeEvidence(t *testing.T) {
	capture := copilotfixture.LoadHookCapture(t, copilotfixture.HookScenarioClaudeDialect)

	for _, event := range []string{"PreToolUse", "PermissionRequest"} {
		assert.NotContains(t, harness.CopilotHookEvents, event,
			"%s is recorded as evidence for NOT installing it", event)
		_, ok := capture.Find(event)
		assert.True(t, ok, "the %s capture must stay committed as that evidence", event)
	}

	payload, _ := capture.Find("PermissionRequest")
	in := decodeCopilotHook(t, payload)
	assert.Empty(t, in.HookEventName,
		"PermissionRequest ignores the dialect: no hook_event_name to route on")
	assert.Empty(t, in.ConvID,
		"...and no session_id either, so tclaude could not even attribute it")

	var fields map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(payload, &fields))
	assert.Contains(t, fields, "sessionId", "it spells the id camelCase instead")
	assert.Contains(t, fields, "toolName")
}
