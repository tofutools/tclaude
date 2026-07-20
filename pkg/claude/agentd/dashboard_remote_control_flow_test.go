package agentd_test

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// Scenario: the fleet remote-control switchboard (JOH-259) needs the
// dashboard snapshot to surface each agent's best-known Remote Access flag
// (sessions.remote_control, JOH-256) so the per-row toggle + the at-a-glance
// "remote on" badge can render. An armed agent shows remote_control=true on
// BOTH the Agents[] roster and its group Members[] row (the two places the
// row controls draw); an unarmed agent shows false; and the flag survives the
// agent going offline (it is best-known intent, not a live readback).
func TestDashboardSnapshot_RemoteControlSurfaces(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))

	f := newFlow(t)
	f.HaveGroup("crew")

	const armedConv = "0190ar01-1111-2222-3333-444444444444"
	const armedLabel = "spwn-arm1"
	const offConv = "0190of01-1111-2222-3333-444444444444"
	const offLabel = "spwn-off1"
	const deadConv = "0190dd01-1111-2222-3333-444444444444"
	const deadLabel = "spwn-dead1"

	f.HaveAliveSession(armedConv, armedLabel, "tmux-arm1", f.TestCwd("arm"))
	f.HaveAliveSession(offConv, offLabel, "tmux-off1", f.TestCwd("off"))
	f.HaveAliveSession(deadConv, deadLabel, "tmux-dead1", f.TestCwd("dead"))
	f.HaveMember("crew", armedConv)
	f.HaveMember("crew", offConv)
	f.HaveMember("crew", deadConv)

	// Arm remote control on the armed + (soon-to-be) dead agents; leave the
	// off agent at its default (off). This is the same persisted state the
	// toggle endpoint records (JOH-256/257).
	require.NoError(t, db.SetSessionRemoteControl(armedLabel, true))
	require.NoError(t, db.SetSessionRemoteControl(deadLabel, true))

	// The dead agent's pane goes away — its best-known flag must still ride
	// the snapshot (surfaced regardless of liveness, like harness/sandbox).
	f.MarkOffline("tmux-dead1")

	snap := fetchDashSnapshot(t, agentd.BuildDashboardHandlerForTest())

	// Armed agent: remote_control=true on both surfaces.
	armedAgent := findDashAgent(snap, armedConv)
	require.NotNil(t, armedAgent, "armed agent missing from Agents[]")
	assert.True(t, armedAgent.State.RemoteControl, "Agents[] armed remote_control")
	armedMember := findDashMember(snap, "crew", armedConv)
	require.NotNil(t, armedMember, "armed agent missing from group members")
	assert.True(t, armedMember.State.RemoteControl, "Members[] armed remote_control")

	// Unarmed agent: remote_control=false (omitempty → decodes to false).
	offMember := findDashMember(snap, "crew", offConv)
	require.NotNil(t, offMember, "off agent missing from group members")
	assert.False(t, offMember.State.RemoteControl, "an unarmed agent reports remote_control off")

	// Offline armed agent: the best-known flag survives the pane dying.
	deadMember := findDashMember(snap, "crew", deadConv)
	require.NotNil(t, deadMember, "dead agent missing from group members")
	assert.False(t, deadMember.Online, "the dead agent is offline")
	assert.True(t, deadMember.State.RemoteControl,
		"an offline agent's best-known remote_control flag must still surface")
}

// Scenario: the dashboard's per-row remote-control toggle POSTs to
// /api/agents/{conv}/remote-control — the cookie-authenticated twin of
// /v1/agent/{conv}/remote-control. The dashboard cookie is the human-consent
// layer (asDashboardHumanPeer), so the call clears the agent.remote-control
// gate without an operator token, exactly the way the rename/clone dashboard
// verbs do. This pins the NEW route end-to-end: enable injects the harness's
// `/remote-control` toggle into the pane and records the best-known state;
// disable exercises the same path the other direction.
func TestDashboardRemoteControl_ToggleViaDashboardRoute(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))

	f := newFlow(t)
	f.HaveGroup("crew")
	const conv = "0190tg01-1111-2222-3333-444444444444"
	const tmux = "tmux-tg1"
	f.HaveAliveSession(conv, "spwn-tg1", tmux, f.TestCwd("work"))
	f.HaveMember("crew", conv)

	mux := agentd.BuildDashboardHandlerForTest()
	post := func(intent string) *httptest.ResponseRecorder {
		t.Helper()
		return testharness.Serve(mux, testharness.JSONRequest(t, http.MethodPost,
			"/api/agents/"+conv+"/remote-control", map[string]any{"intent": intent}))
	}

	// Enable through the dashboard route.
	on := post("on")
	require.Equalf(t, http.StatusOK, on.Code, "dashboard enable body=%s", on.Body.String())
	f.AssertSentContains(tmux+":0.0", "/remote-control", 10*time.Second)
	got, err := db.RemoteControlForConv(conv)
	require.NoError(t, err)
	assert.True(t, got, "dashboard enable persists best-known state on")

	// And the snapshot the dashboard re-reads on refresh reflects it.
	snap := fetchDashSnapshot(t, mux)
	m := findDashMember(snap, "crew", conv)
	require.NotNil(t, m, "agent missing from group members after enable")
	assert.True(t, m.State.RemoteControl, "snapshot reflects the armed state after the dashboard toggle")

	// Disable through the dashboard route (exercises the confirm-Enter path).
	off := post("off")
	require.Equalf(t, http.StatusOK, off.Code, "dashboard disable body=%s", off.Body.String())
	got, err = db.RemoteControlForConv(conv)
	require.NoError(t, err)
	assert.False(t, got, "dashboard disable persists best-known state off")
}

// Scenario: the dashboard remote-control route refuses a harness with no
// built-in Remote Access (Codex → RemoteControlCommand "") with a 409, the
// same gate the /v1 handler enforces. The UI also hides the control for such
// a harness, but the route must fail closed for defence in depth.
func TestDashboardRemoteControl_CodexRefused(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))

	f := newFlow(t)
	f.HaveGroup("crew")
	const conv = "0190cx01-1111-2222-3333-444444444444"
	f.HaveAliveCodexSession(conv, "spwn-cx1", "tmux-cx1", f.TestCwd("work"))
	f.HaveMember("crew", conv)

	mux := agentd.BuildDashboardHandlerForTest()
	rec := testharness.Serve(mux, testharness.JSONRequest(t, http.MethodPost,
		"/api/agents/"+conv+"/remote-control", map[string]any{"intent": "on"}))
	assert.Equalf(t, http.StatusConflict, rec.Code,
		"Codex has no Remote Access; the dashboard toggle must refuse with 409; body=%s", rec.Body.String())
}
