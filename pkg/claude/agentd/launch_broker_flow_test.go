package agentd_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/session"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// Flow tests for brokered launch operations: the `tclaude session new` that
// agentd forks for a managed launch sends its database steps to
// /v1/launch/op instead of writing the database. These drive that endpoint
// through the production mux with the wire shapes the launcher sends, and
// read the result back the way every other surface does.

const launchBrokerCallerPID = 8100

// launchOp posts one operation as the launcher process would.
func launchOp(t *testing.T, f *testharness.Flow, ticket, op string, args any) (int, map[string]any) {
	t.Helper()
	raw, err := json.Marshal(args)
	require.NoError(t, err)
	req := testharness.JSONRequest(t, http.MethodPost, session.LaunchOpPath,
		session.BrokeredLaunchRequest{Ticket: ticket, Op: op, Args: raw})
	req = agentd.AsAgentPeerWithPID(req, "", launchBrokerCallerPID)
	rec := testharness.Serve(f.Mux, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

func beginLaunch(t *testing.T, f *testharness.Flow, ticket, sessionID, generation string) {
	t.Helper()
	code, body := launchOp(t, f, ticket, session.LaunchBeginOp,
		session.LaunchBinding{SessionID: sessionID, Generation: generation})
	require.Equal(t, http.StatusOK, code, "begin: %v", body)
	// The daemon tells the launcher what it can run, so a launcher newer
	// than the daemon can say so instead of failing on a refusal.
	result, _ := body["result"].(map[string]any)
	assert.Contains(t, result["ops"], "session.save_launch_row")
}

func launchRowArgs(sessionID, generation string, pid int) map[string]any {
	args := map[string]any{"state": &session.SessionState{
		ID: sessionID, TmuxSession: sessionID, Cwd: "/work", Status: session.StatusIdle,
		Harness: "copilot", PID: pid, ConvID: "c0000000-" + sessionID,
	}}
	if generation != "" {
		args["generation"] = generation
		args["gate_state"] = db.SessionExitGateUngated
	}
	return args
}

// A managed launch's whole sessions-row lifecycle, written by the daemon on
// the launcher's behalf.
func TestLaunchBroker_WritesTheLaunchRowForItsLauncher(t *testing.T) {
	f := newFlow(t)
	const id, gen = "spwn-launch-broker", "0123456789abcdef0123456789abcdef"
	ticket, _ := agentd.MintLaunchTicketForTest(t, id)
	ref := map[string]any{"session_id": id, "generation": gen}

	code, _ := launchOp(t, f, ticket, "session.row_exists", map[string]any{"session_id": id})
	assert.Equal(t, http.StatusForbidden, code, "a ticket serves nothing before it is bound to a launch")

	beginLaunch(t, f, ticket, id, gen)
	beginLaunch(t, f, ticket, id, gen) // a retried begin is harmless

	code, body := launchOp(t, f, ticket, "session.row_exists", map[string]any{"session_id": id})
	require.Equal(t, http.StatusOK, code, "%v", body)
	assert.Equal(t, map[string]any{"exists": false}, body["result"])

	copilotAPI := true
	for _, step := range []struct {
		op   string
		args any
	}{
		{"session.save_launch_row", launchRowArgs(id, gen, 0)},
		{"exit.mark_pending", ref},
		{"exit.set_binding", map[string]any{
			"session_id": id, "generation": gen,
			"token_hash": strings.Repeat("ab", 32), "pane_id": "%7",
		}},
		{"session.save_launched_state", launchRowArgs(id, "", 4242)},
		{"session.set_resume_provenance", map[string]any{"session_id": id, "value": "provenance-v1"}},
		{"session.seed_model", map[string]any{"session_id": id, "value": "launch-model"}},
		{"session.record_launch_posture", map[string]any{
			"session_id": id, "harness": "copilot",
			"posture": session.LaunchPosture{CopilotAPI: &copilotAPI, FastMode: "on"},
		}},
		{"exit.mark_releasing", ref},
		{"exit.mark_released", ref},
	} {
		code, body := launchOp(t, f, ticket, step.op, step.args)
		require.Equal(t, http.StatusOK, code, "%s: %v", step.op, body)
	}

	row, err := db.LoadSession(id)
	require.NoError(t, err)
	assert.Equal(t, 4242, row.PID)
	assert.Equal(t, "provenance-v1", row.ResumeProvenance)
	identity, err := db.GetSessionExitLaunchIdentity(id)
	require.NoError(t, err)
	assert.Equal(t, gen, identity.Generation)
	assert.Equal(t, db.SessionExitGateReleased, identity.GateState)

	code, body = launchOp(t, f, ticket, "session.row_exists", map[string]any{"session_id": id})
	require.Equal(t, http.StatusOK, code)
	assert.Equal(t, map[string]any{"exists": true}, body["result"])

	// The launch posture is what a later relaunch reads. A brokered launch
	// that lost it would relaunch a Copilot API agent as a send-keys one.
	posture, err := db.RecordedLaunchPostureForConv("c0000000-" + id)
	require.NoError(t, err)
	require.NotNil(t, posture)
	require.NotNil(t, posture.CopilotAPI, "the brokered launch recorded its Copilot API posture")
	assert.True(t, *posture.CopilotAPI)
}

// A ticket speaks for one launch only: not for another session, not under a
// different generation, and not once it is re-bound, unknown or revoked.
func TestLaunchBroker_ATicketOnlySpeaksForItsOwnLaunch(t *testing.T) {
	f := newFlow(t)
	f.HaveAliveSession("5ac10000-aaaa-bbbb-cccc-444444444444", "spwn-launch-victim", "tmux-launch-victim", f.World.HomeDir)
	victimBefore, err := db.LoadSession("spwn-launch-victim")
	require.NoError(t, err)

	const id, gen = "spwn-launch-own", "fedcba9876543210fedcba9876543210"
	// Minted for no particular session: the launcher derives the id itself
	// when it is given neither a label nor a conversation.
	ticket, revoke := agentd.MintLaunchTicketForTest(t, "")
	beginLaunch(t, f, ticket, id, gen)

	// Minted for a session: it cannot be bound to any other, so a ticket
	// that escaped its launcher could not be pointed at a third session.
	pinned, _ := agentd.MintLaunchTicketForTest(t, "spwn-launch-pinned")
	code, _ := launchOp(t, f, pinned, session.LaunchBeginOp,
		session.LaunchBinding{SessionID: "spwn-launch-victim", Generation: gen})
	assert.Equal(t, http.StatusForbidden, code, "a ticket binds only to the session it was minted for")

	code, _ = launchOp(t, f, ticket, "session.save_launch_row", launchRowArgs("spwn-launch-victim", gen, 666))
	assert.Equal(t, http.StatusForbidden, code, "another session's row")
	code, _ = launchOp(t, f, ticket, "session.delete_launch_row",
		map[string]any{"session_id": "spwn-launch-victim", "generation": gen})
	assert.Equal(t, http.StatusForbidden, code, "another session's rollback")
	code, _ = launchOp(t, f, ticket, "session.save_launch_row", launchRowArgs(id, "another-generation", 1))
	assert.Equal(t, http.StatusForbidden, code, "a generation the ticket is not bound to")
	code, _ = launchOp(t, f, ticket, session.LaunchBeginOp,
		session.LaunchBinding{SessionID: "spwn-launch-victim", Generation: gen})
	assert.Equal(t, http.StatusForbidden, code, "a ticket cannot be re-bound")
	code, _ = launchOp(t, f, ticket, "session.drop_everything", map[string]any{"session_id": id})
	assert.Equal(t, http.StatusNotImplemented, code, "an operation the registry does not hold")
	code, _ = launchOp(t, f, "not-a-ticket", "session.row_exists", map[string]any{"session_id": id})
	assert.Equal(t, http.StatusForbidden, code, "no ticket, no authority")

	victimAfter, err := db.LoadSession("spwn-launch-victim")
	require.NoError(t, err)
	assert.Equal(t, victimBefore.PID, victimAfter.PID, "the other session's row is untouched")
	assert.Equal(t, victimBefore.ConvID, victimAfter.ConvID)

	code, _ = launchOp(t, f, ticket, "session.save_launch_row", launchRowArgs(id, gen, 0))
	require.Equal(t, http.StatusOK, code)
	revoke()
	code, _ = launchOp(t, f, ticket, "session.row_exists", map[string]any{"session_id": id})
	assert.Equal(t, http.StatusForbidden, code, "the ticket dies with its launcher")
}

// An old launcher must not complete a newer launch of the same session: once
// the row carries another generation its operations are dropped with 409, and
// its rollback leaves the newer row alone.
func TestLaunchBroker_DropsOperationsFromASupersededLaunch(t *testing.T) {
	f := newFlow(t)
	const id, oldGen, newGen = "spwn-launch-raced", "00000000000000000000000000000001", "00000000000000000000000000000002"
	oldTicket, _ := agentd.MintLaunchTicketForTest(t, id)
	newTicket, _ := agentd.MintLaunchTicketForTest(t, id)

	beginLaunch(t, f, oldTicket, id, oldGen)
	code, _ := launchOp(t, f, oldTicket, "session.save_launch_row", launchRowArgs(id, oldGen, 0))
	require.Equal(t, http.StatusOK, code)

	beginLaunch(t, f, newTicket, id, newGen)
	code, _ = launchOp(t, f, newTicket, "session.save_launch_row", launchRowArgs(id, newGen, 0))
	require.Equal(t, http.StatusOK, code)
	code, _ = launchOp(t, f, newTicket, "session.save_launched_state", launchRowArgs(id, "", 9001))
	require.Equal(t, http.StatusOK, code)

	code, body := launchOp(t, f, oldTicket, "session.save_launched_state", launchRowArgs(id, "", 4242))
	assert.Equal(t, http.StatusConflict, code, "%v", body)
	assert.Equal(t, "stale_launch", body["code"])
	code, _ = launchOp(t, f, oldTicket, "session.seed_model", map[string]any{"session_id": id, "value": "old-model"})
	assert.Equal(t, http.StatusConflict, code)
	// The gate operations are fenced too, so the old launcher's are dropped
	// the same way rather than failing on their generation-conditional SQL.
	code, _ = launchOp(t, f, oldTicket, "exit.mark_pending", map[string]any{"session_id": id, "generation": oldGen})
	assert.Equal(t, http.StatusConflict, code)
	code, _ = launchOp(t, f, oldTicket, "session.delete_launch_row", map[string]any{"session_id": id, "generation": oldGen})
	assert.Equal(t, http.StatusOK, code, "a rollback that matches nothing is not an error")

	row, err := db.LoadSession(id)
	require.NoError(t, err, "the newer launch's row survives the older launch's rollback")
	assert.Equal(t, 9001, row.PID)
	identity, err := db.GetSessionExitLaunchIdentity(id)
	require.NoError(t, err)
	assert.Equal(t, newGen, identity.Generation)
	assert.Equal(t, db.SessionExitGateUngated, identity.GateState)
}

// The endpoint is the daemon socket's: a request with no peer process (the
// dashboard's network listener) is refused before its ticket is looked at.
func TestLaunchBroker_RefusesACallerWithNoPeerProcess(t *testing.T) {
	f := newFlow(t)
	ticket, _ := agentd.MintLaunchTicketForTest(t, "spwn-launch-net")
	raw, err := json.Marshal(session.LaunchBinding{SessionID: "spwn-launch-net", Generation: "g"})
	require.NoError(t, err)
	req := testharness.JSONRequest(t, http.MethodPost, session.LaunchOpPath,
		session.BrokeredLaunchRequest{Ticket: ticket, Op: session.LaunchBeginOp, Args: raw})
	rec := testharness.Serve(f.Mux, agentd.AsAgentPeerWithPID(req, "", 0))
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

// Before a launch has a generation the launcher has to settle its identity,
// which takes two reads. A ticket serves those, and only those, before it is
// bound, and only for the session and conversation the daemon minted it for.
func TestLaunchBroker_UnboundTicketServesOnlyItsOwnIdentityReads(t *testing.T) {
	f := newFlow(t)
	const victimConv = "5ac10000-dddd-eeee-ffff-444444444444"
	f.HaveAliveSession(victimConv, "spwn-launch-prebind-victim", "tmux-launch-prebind-victim", f.World.HomeDir)
	const id, conv = "spwn-launch-prebind", "c0000000-dddd-eeee-ffff-444444444444"
	f.HaveAliveSession(conv, id, "tmux-launch-prebind", f.World.HomeDir)
	ticket, _ := agentd.MintLaunchTicketForLaunchTest(t, id, conv, "")

	code, body := launchOp(t, f, ticket, "session.load_state", map[string]any{"session_id": id})
	require.Equal(t, http.StatusOK, code, "%v", body)
	state, _ := body["result"].(map[string]any)["state"].(map[string]any)
	assert.Equal(t, "tmux-launch-prebind", state["tmuxSession"], "the launcher learns who owns its label")
	code, body = launchOp(t, f, ticket, "session.for_resumed_conv", map[string]any{"conv_id": conv})
	require.Equal(t, http.StatusOK, code, "%v", body)
	states, _ := body["result"].(map[string]any)["states"].([]any)
	assert.Len(t, states, 1, "and whether the conversation it resumes already has a pane")

	code, _ = launchOp(t, f, ticket, "session.load_state", map[string]any{"session_id": "spwn-launch-prebind-victim"})
	assert.Equal(t, http.StatusForbidden, code, "not another session's row")
	code, _ = launchOp(t, f, ticket, "session.for_resumed_conv", map[string]any{"conv_id": victimConv})
	assert.Equal(t, http.StatusForbidden, code, "not another conversation's sessions")
	code, _ = launchOp(t, f, ticket, "session.save_launch_row", launchRowArgs(id, "0123456789abcdef0123456789abcdef", 0))
	assert.Equal(t, http.StatusForbidden, code, "and no write before the bind")

	fresh, _ := agentd.MintLaunchTicketForTest(t, "spwn-launch-prebind-fresh")
	code, _ = launchOp(t, f, fresh, "session.for_resumed_conv", map[string]any{"conv_id": conv})
	assert.Equal(t, http.StatusForbidden, code, "a launch that resumes nothing reads no conversation's sessions")
}

// The Codex app-server runtime a launch may report on is the one the daemon
// started for it; a launcher cannot mark another agent's runtime.
func TestLaunchBroker_CodexRuntimeOperationsAreBoundToTheLaunchRuntime(t *testing.T) {
	f := newFlow(t)
	const id, gen = "spwn-launch-codex", "abcdefabcdefabcdefabcdefabcdefab"
	ticket, _ := agentd.MintLaunchTicketForLaunchTest(t, id, "", "runtime-of-this-launch")
	beginLaunch(t, f, ticket, id, gen)
	code, _ := launchOp(t, f, ticket, "session.save_launch_row", launchRowArgs(id, gen, 0))
	require.Equal(t, http.StatusOK, code)

	code, _ = launchOp(t, f, ticket, "codex.mark_app_server_unavailable", map[string]any{
		"session_id": id, "runtime_generation": "runtime-of-another-agent", "value": "forged",
	})
	assert.Equal(t, http.StatusForbidden, code)
	code, _ = launchOp(t, f, ticket, "codex.set_app_server_version", map[string]any{
		"session_id": id, "runtime_generation": "runtime-of-another-agent", "value": "0.0.0",
	})
	assert.Equal(t, http.StatusForbidden, code)

	noRuntime, _ := agentd.MintLaunchTicketForTest(t, "spwn-launch-codex-none")
	beginLaunch(t, f, noRuntime, "spwn-launch-codex-none", gen)
	code, _ = launchOp(t, f, noRuntime, "codex.mark_app_server_unavailable", map[string]any{
		"session_id": "spwn-launch-codex-none", "runtime_generation": "runtime-of-this-launch", "value": "forged",
	})
	assert.Equal(t, http.StatusForbidden, code, "a launch given no runtime may name none")
}
