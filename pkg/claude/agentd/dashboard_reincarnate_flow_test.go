package agentd_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// These tests exercise the dashboard reincarnate button's two modes —
// the cookie-authenticated POST /api/agents/{conv}/reincarnate endpoint.
//
//   - "self" (the DEFAULT): the daemon does NOT reincarnate the agent.
//     It delivers an inbox message asking the agent to reincarnate
//     itself, so the agent collects its own context and writes its own
//     handoff. The target's tmux session is left running.
//   - "force": the unchanged direct path — the daemon spawns the
//     successor and soft-exits the original immediately.
//
// The split matters because a daemon-forced reincarnation cannot write
// a context-aware handoff; only the agent knows its own working state.

// reincDashMux sets a popup base URL — so the dashboard auth's Origin
// pin is satisfiable — and returns the dashboard mux. Self-contained so
// these tests don't couple to another flow file's helper.
func reincDashMux(t *testing.T) http.Handler {
	t.Helper()
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	return agentd.BuildDashboardHandlerForTest()
}

// postReincarnate POSTs /api/agents/{conv}/reincarnate through the
// dashboard mux.
func postReincarnate(t *testing.T, mux http.Handler, conv string, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	return testharness.Serve(mux, testharness.JSONRequest(t, http.MethodPost,
		"/api/agents/"+conv+"/reincarnate", body))
}

// sentExitTo reports whether any recorded send-keys delivered `/exit`
// to the given tmux pane — the signal that the daemon soft-killed the
// session. Self-mode must never trip this.
func sentExitTo(sent []testharness.SentKey, tmuxPane string) bool {
	for _, sk := range sent {
		if sk.Target == tmuxPane && strings.Contains(sk.Text, "/exit") {
			return true
		}
	}
	return false
}

// Scenario: the default mode (self) — POST with focus_hint. The agent
// gets exactly one inbox message instructing it to reincarnate itself,
// the focus hint is folded in as guidance, and the session is left
// running. Nothing is force-killed; no successor is spawned.
func TestDashboardReincarnate_SelfMode_DeliversInstructionWithFocusHint(t *testing.T) {
	f := newFlow(t)

	const conv = "reia-aaaa-bbbb-cccc-000000000001"
	const tmux = "tclaude-spwn-reia"
	f.HaveConvWithTitle(conv, "worker-self")
	f.HaveAliveSession(conv, "spwn-reia", tmux, "/tmp/work")
	f.HaveGroup("team")
	f.HaveMember("team", conv)

	mux := reincDashMux(t)
	rec := postReincarnate(t, mux, conv, map[string]any{
		"mode":       "self",
		"focus_hint": "capture the open questions about the cron scheduler",
	})
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	var resp struct {
		Mode      string `json:"mode"`
		ConvID    string `json:"conv_id"`
		MessageID int64  `json:"message_id"`
		Delivered bool   `json:"delivered"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp), "body=%s", rec.Body.String())
	assert.Equal(t, "self", resp.Mode)
	assert.Equal(t, conv, resp.ConvID)
	assert.Greater(t, resp.MessageID, int64(0))
	assert.True(t, resp.Delivered, "an alive target is nudged immediately")

	// Real surface: the agent's inbox has exactly one message — the
	// reincarnate-yourself instruction, with the focus hint folded in.
	rows, err := db.ListAgentMessagesForConv(conv, 100)
	require.NoError(t, err)
	require.Len(t, rows, 1, "exactly one instruction message lands in the inbox")
	body := rows[0].Body
	assert.Contains(t, body, "reincarnate yourself")
	assert.Contains(t, body, "tclaude agent reincarnate",
		"the instruction names the self-reincarnate command")
	assert.Contains(t, body, "Write a handoff for your successor",
		"the instruction asks the agent to write its own handoff")
	assert.Contains(t, body, "capture the open questions about the cron scheduler",
		"the human's focus hint is folded into the message")
	assert.Contains(t, body, "Focus hint from the human",
		"the hint is framed as guidance, not the whole task")

	// The session is left running — self-mode never force-kills.
	assert.True(t, f.World.Tmux.IsAlive(tmux), "target session must stay alive in self-mode")
	assert.False(t, sentExitTo(f.World.Tmux.Sent(), tmux+":0.0"),
		"self-mode must not inject /exit; sent=%+v", f.World.Tmux.Sent())

	// The agent was nudged over tmux — the normal new-message path.
	f.AssertSentContains(tmux+":0.0", "new agent message", 2*time.Second)

	// No succession edge recorded — the conv was not superseded.
	assert.Equal(t, conv, db.ResolveLatestConv(conv),
		"self-mode records no succession; the conv is still its own head")
}

// Scenario: an omitted `mode` field defaults to self — the new
// default. The daemon delivers the self-reincarnate instruction and
// leaves the session running.
func TestDashboardReincarnate_DefaultModeIsSelf(t *testing.T) {
	f := newFlow(t)

	const conv = "reib-aaaa-bbbb-cccc-000000000001"
	const tmux = "tclaude-spwn-reib"
	f.HaveConvWithTitle(conv, "worker-default")
	f.HaveAliveSession(conv, "spwn-reib", tmux, "/tmp/work")
	f.HaveGroup("team")
	f.HaveMember("team", conv)

	mux := reincDashMux(t)
	// No `mode` key at all — the endpoint must default to self.
	rec := postReincarnate(t, mux, conv, map[string]any{})
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	var resp struct {
		Mode string `json:"mode"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp), "body=%s", rec.Body.String())
	assert.Equal(t, "self", resp.Mode, "omitted mode defaults to self")

	rows, err := db.ListAgentMessagesForConv(conv, 100)
	require.NoError(t, err)
	require.Len(t, rows, 1, "the default path still delivers the self-reincarnate instruction")
	assert.Contains(t, rows[0].Body, "reincarnate yourself")

	assert.True(t, f.World.Tmux.IsAlive(tmux), "default mode must not kill the session")
	assert.Equal(t, conv, db.ResolveLatestConv(conv), "default mode records no succession")
}

// Scenario: self mode with no focus hint — the agent is asked for a
// general handoff and the message carries no focus-hint section.
func TestDashboardReincarnate_SelfMode_NoFocusHintGeneralHandoff(t *testing.T) {
	f := newFlow(t)

	const conv = "reic-aaaa-bbbb-cccc-000000000001"
	const tmux = "tclaude-spwn-reic"
	f.HaveConvWithTitle(conv, "worker-nohint")
	f.HaveAliveSession(conv, "spwn-reic", tmux, "/tmp/work")
	f.HaveGroup("team")
	f.HaveMember("team", conv)

	mux := reincDashMux(t)
	rec := postReincarnate(t, mux, conv, map[string]any{"mode": "self"})
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	rows, err := db.ListAgentMessagesForConv(conv, 100)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Contains(t, rows[0].Body, "reincarnate yourself")
	assert.NotContains(t, rows[0].Body, "Focus hint from the human",
		"a blank focus hint adds no focus-hint section")
}

// Scenario: self mode against an OFFLINE target. The force path needs
// a live tmux session to spawn into and would 503 here — but self mode
// just queues the instruction in the inbox, to be picked up when the
// agent next comes online. This is the property that lets self mode
// work where force cannot.
func TestDashboardReincarnate_SelfMode_OfflineTargetQueuesInInbox(t *testing.T) {
	f := newFlow(t)

	const conv = "reid-aaaa-bbbb-cccc-000000000001"
	f.HaveConvWithTitle(conv, "worker-offline")
	f.HaveEnrolledAgent(conv)
	// Deliberately no HaveAliveSession — the target has no live pane.

	mux := reincDashMux(t)
	rec := postReincarnate(t, mux, conv, map[string]any{"mode": "self"})
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	var resp struct {
		Delivered bool  `json:"delivered"`
		MessageID int64 `json:"message_id"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp), "body=%s", rec.Body.String())
	assert.False(t, resp.Delivered, "an offline target cannot be nudged immediately")
	assert.Greater(t, resp.MessageID, int64(0), "the instruction is still queued")

	rows, err := db.ListAgentMessagesForConv(conv, 100)
	require.NoError(t, err)
	require.Len(t, rows, 1, "the instruction waits in the inbox for the agent to come online")
	assert.Contains(t, rows[0].Body, "reincarnate yourself")
}

// Scenario: a focus hint carrying a control character is rejected with
// 400 before any row is written — the composed instruction rides the
// inbox and must clear the same charset rule as any agent message.
func TestDashboardReincarnate_SelfMode_RejectsControlCharFocusHint(t *testing.T) {
	f := newFlow(t)

	const conv = "reie-aaaa-bbbb-cccc-000000000001"
	const tmux = "tclaude-spwn-reie"
	f.HaveConvWithTitle(conv, "worker-badhint")
	f.HaveAliveSession(conv, "spwn-reie", tmux, "/tmp/work")
	f.HaveGroup("team")
	f.HaveMember("team", conv)

	mux := reincDashMux(t)
	rec := postReincarnate(t, mux, conv, map[string]any{
		"mode":       "self",
		"focus_hint": "bad\x01hint",
	})
	require.Equal(t, http.StatusBadRequest, rec.Code, "body=%s", rec.Body.String())
	// Pin that the focus-hint validation specifically fired — not some
	// other 400 — so the test can't pass for the wrong reason.
	assert.Contains(t, rec.Body.String(), "invalid_focus_hint")

	rows, err := db.ListAgentMessagesForConv(conv, 100)
	require.NoError(t, err)
	assert.Empty(t, rows, "a rejected request writes no inbox row")
	assert.True(t, f.World.Tmux.IsAlive(tmux), "a rejected request never touches the session")
}

// Scenario: an unknown `mode` value is rejected with 400 — the
// endpoint's default branch — and writes no inbox row.
func TestDashboardReincarnate_UnknownMode_BadRequest(t *testing.T) {
	f := newFlow(t)

	const conv = "reig-aaaa-bbbb-cccc-000000000001"
	const tmux = "tclaude-spwn-reig"
	f.HaveConvWithTitle(conv, "worker-badmode")
	f.HaveAliveSession(conv, "spwn-reig", tmux, "/tmp/work")
	f.HaveGroup("team")
	f.HaveMember("team", conv)

	mux := reincDashMux(t)
	rec := postReincarnate(t, mux, conv, map[string]any{"mode": "bogus"})
	require.Equal(t, http.StatusBadRequest, rec.Code, "body=%s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "unknown reincarnate mode")

	rows, err := db.ListAgentMessagesForConv(conv, 100)
	require.NoError(t, err)
	assert.Empty(t, rows, "a rejected request writes no inbox row")
	assert.True(t, f.World.Tmux.IsAlive(tmux), "a rejected request never touches the session")
}

// Scenario: force mode is the unchanged direct reincarnation. The
// daemon spawns a fresh successor, bumps the title, soft-exits the old
// pane, and migrates group membership — exactly as the /v1 reincarnate
// endpoint does. This is the regression guard that the mode switch did
// not disturb the force path.
func TestDashboardReincarnate_ForceMode_StillDirectReincarnation(t *testing.T) {
	f := newFlow(t)

	const oldConv = "reif-aaaa-bbbb-cccc-000000000001"
	const oldTmux = "tclaude-spwn-reif"
	f.HaveConvWithTitle(oldConv, "worker-r-3")
	f.HaveAliveSession(oldConv, "spwn-reif", oldTmux, "/tmp/work")
	g := f.HaveGroup("alpha")
	f.HaveMember("alpha", oldConv)

	mux := reincDashMux(t)
	// focus_hint is a self-mode field; force-mode's decoder must simply
	// ignore it (extra JSON fields are dropped) and proceed normally.
	rec := postReincarnate(t, mux, oldConv, map[string]any{
		"mode":       "force",
		"follow_up":  "fresh start",
		"focus_hint": "ignored in force mode",
	})
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	var resp struct {
		OldConv     string `json:"old_conv"`
		NewConv     string `json:"new_conv"`
		NewTitle    string `json:"new_title"`
		TmuxSession string `json:"tmux_session"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp), "body=%s", rec.Body.String())
	assert.Equal(t, oldConv, resp.OldConv)
	require.NotEmpty(t, resp.NewConv, "force mode spawns a successor conv")
	assert.NotEqual(t, oldConv, resp.NewConv)
	assert.Equal(t, "worker-r-4", resp.NewTitle, "the -r-<N> suffix increments from the parent")

	// The successor pane is renamed; the old pane is soft-exited.
	f.AssertSentContains(resp.TmuxSession+":0.0", "/rename worker-r-4", 5*time.Second)
	assert.True(t, f.World.Tmux.WaitForSendKeys(oldTmux+":0.0", "/exit", 2*time.Second),
		"old pane should receive /exit in force mode; sent=%+v", f.World.Tmux.Sent())

	// Group membership migrated old → new — the same surface invariant
	// the /v1 reincarnate flow test pins.
	f.AssertGroupMember(g.Name, resp.NewConv, "worker-r-4", 5*time.Second)
	f.AssertNotGroupMember(g.Name, oldConv)
}
