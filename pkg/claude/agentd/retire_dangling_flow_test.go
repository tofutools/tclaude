package agentd_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// A "dangling" agent entry is an enrollment whose conversation data is
// gone: no conv_index row, no group membership, no succession chain. The
// dashboard still surfaces it (db.ListActiveAgents walks the enrollment
// table), but agent.ResolveSelector — which retire runs first — dead-ends
// on "no conversation matches", so retire used to 404 and the entry got
// stuck on the roster with no way to remove it.
//
// The fix: retire detects the dangling case and returns a 409 +
// {dangling:true} signal instead of the dead 404, so the dashboard can
// pop a "remove the dangling entry?" confirm whose OK fires the DELETE
// orphan-purge (the only meaningful cleanup — there's no conversation to
// demote). These flow tests pin that behaviour at both retire surfaces.

// danglingConvID is a canonical-shape (8-4-4-4-12 hex) conv-id with no
// conversation data behind it — looksLikeConvID accepts it, but nothing
// resolves it.
const danglingConvID = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"

// TestRetire_DanglingEntry_DashboardSignalsThenDeletes walks the full
// operator flow: the dashboard retire returns the dangling signal (not a
// dead 404), the enrollment is left untouched (retire must not silently
// demote a dangling entry), and the follow-up DELETE the confirm modal
// fires purges the orphan completely.
func TestRetire_DanglingEntry_DashboardSignalsThenDeletes(t *testing.T) {
	restoreURL := agentd.SetPopupBaseURLForTest("http://127.0.0.1:0")
	t.Cleanup(restoreURL)

	f := newFlow(t)

	// Enroll WITHOUT any conversation data — the dangling state.
	f.HaveEnrolledAgent(danglingConvID)
	st, err := db.AgentState(danglingConvID)
	require.NoError(t, err)
	require.Equal(t, db.AgentStateActive, st, "precondition: an active enrollment with no conv data")

	dash := agentd.BuildDashboardHandlerForTest()

	// 1) Dashboard retire → 409 + {dangling:true}, NOT a dead 404.
	rec := testharness.Serve(dash, testharness.JSONRequest(t, http.MethodPost,
		"/api/agents/"+danglingConvID+"/retire?shutdown=0&delete_worktree=0", nil))
	require.Equal(t, http.StatusConflict, rec.Code, "retire body=%s", rec.Body.String())
	var sig struct {
		Dangling bool   `json:"dangling"`
		ConvID   string `json:"conv_id"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &sig), "decode retire signal")
	assert.True(t, sig.Dangling, "dashboard retire must flag dangling; body=%s", rec.Body.String())
	assert.Equal(t, danglingConvID, sig.ConvID)

	// Retire must not have mutated the enrollment — the dangling signal is
	// read-only; only the explicit DELETE removes anything.
	st, _ = db.AgentState(danglingConvID)
	assert.Equal(t, db.AgentStateActive, st, "dangling retire must not demote the enrollment")

	// 2) The follow-up DELETE the confirm modal fires purges the orphan.
	drec := testharness.Serve(dash, testharness.JSONRequest(t, http.MethodDelete,
		"/api/agents/"+danglingConvID, nil))
	require.Truef(t, drec.Code == http.StatusNoContent || drec.Code == http.StatusOK,
		"delete dangling: code=%d body=%s", drec.Code, drec.Body.String())

	// The entry is fully gone — no longer an agent at all.
	st, _ = db.AgentState(danglingConvID)
	assert.Equal(t, db.AgentStateNone, st, "dangling entry must be fully removed after delete")
}

// TestRetire_DanglingEntry_V1DispatcherSignals proves the /v1 dispatcher
// (the path the `tclaude agent retire` CLI takes) emits the same dangling
// signal — so the CLI can print actionable guidance instead of the raw
// resolver error.
func TestRetire_DanglingEntry_V1DispatcherSignals(t *testing.T) {
	f := newFlow(t)

	const conv = "aaaaaaaa-1111-2222-3333-444444444444"
	f.HaveEnrolledAgent(conv)

	r := agentd.AsHumanPeer(testharness.JSONRequest(t, http.MethodPost,
		"/v1/agent/"+conv+"/retire?shutdown=0&delete_worktree=0", nil))
	rec := testharness.Serve(f.Mux, r)
	require.Equal(t, http.StatusConflict, rec.Code, "v1 retire body=%s", rec.Body.String())
	var sig struct {
		Dangling bool `json:"dangling"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &sig), "decode v1 retire signal")
	assert.True(t, sig.Dangling, "v1 retire must flag dangling; body=%s", rec.Body.String())
}

// TestRetire_UnknownConvID_StaysNotFound guards the gate: a UUID-shaped
// selector that is NOT a known agent entry (no enrollment) must still
// 404, never the dangling signal — we only offer to "remove a dangling
// entry" for something that actually IS one.
func TestRetire_UnknownConvID_StaysNotFound(t *testing.T) {
	restoreURL := agentd.SetPopupBaseURLForTest("http://127.0.0.1:0")
	t.Cleanup(restoreURL)

	_ = newFlow(t)

	const unknown = "ffffffff-0000-1111-2222-333333333333"
	st, _ := db.AgentState(unknown)
	require.Equal(t, db.AgentStateNone, st, "precondition: not an agent")

	dash := agentd.BuildDashboardHandlerForTest()
	rec := testharness.Serve(dash, testharness.JSONRequest(t, http.MethodPost,
		"/api/agents/"+unknown+"/retire?shutdown=0&delete_worktree=0", nil))
	assert.Equal(t, http.StatusNotFound, rec.Code,
		"a non-agent UUID must 404, not be offered for dangling removal; body=%s", rec.Body.String())
}

// TestRetire_DanglingEntry_V1GatedForUnauthorizedAgent guards the
// disclosure fix: on the /v1 path the dangling 409 must be gated behind
// the same permission a normal retire requires, so an unauthorized agent
// caller can't use the signal to learn "this UUID is a dangling agent
// enrollment" (vs an unknown conv). An agent with no agent.retire grant,
// and not owning a group containing the (group-less) dangling conv, must
// get 403 — never the 409 dangling body.
func TestRetire_DanglingEntry_V1GatedForUnauthorizedAgent(t *testing.T) {
	f := newFlow(t)

	const dangling = "aaaaaaaa-2222-3333-4444-555555555555"
	f.HaveEnrolledAgent(dangling)

	// An unrelated agent with no permissions and no group ownership.
	const intruder = "bbbbbbbb-2222-3333-4444-555555555555"
	r := agentd.AsAgentPeer(testharness.JSONRequest(t, http.MethodPost,
		"/v1/agent/"+dangling+"/retire?shutdown=0&delete_worktree=0", nil), intruder)
	rec := testharness.Serve(f.Mux, r)
	require.Equal(t, http.StatusForbidden, rec.Code,
		"unauthorized agent must be refused, not handed the dangling signal; body=%s", rec.Body.String())
	assert.NotContains(t, rec.Body.String(), "dangling",
		"the 403 must not leak the dangling flag")
}

// TestRetire_DanglingEntry_RetiredAlsoOffered covers the second dangling
// state: isDanglingAgentEntry accepts a RETIRED enrollment too, so a
// retired-and-gone entry hits the dangling branch (rather than the
// "not an active agent" 409) and is offered for removal. The follow-up
// DELETE purges it.
func TestRetire_DanglingEntry_RetiredAlsoOffered(t *testing.T) {
	restoreURL := agentd.SetPopupBaseURLForTest("http://127.0.0.1:0")
	t.Cleanup(restoreURL)

	f := newFlow(t)

	const conv = "cccccccc-2222-3333-4444-555555555555"
	f.HaveRetiredAgent(conv) // enrolled then retired, no conversation data
	st, _ := db.AgentState(conv)
	require.Equal(t, db.AgentStateRetired, st, "precondition: a retired enrollment with no conv data")

	dash := agentd.BuildDashboardHandlerForTest()
	rec := testharness.Serve(dash, testharness.JSONRequest(t, http.MethodPost,
		"/api/agents/"+conv+"/retire?shutdown=0&delete_worktree=0", nil))
	require.Equal(t, http.StatusConflict, rec.Code, "retire body=%s", rec.Body.String())
	var sig struct {
		Dangling bool `json:"dangling"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &sig))
	assert.True(t, sig.Dangling, "a retired dangling entry must be offered for removal; body=%s", rec.Body.String())

	drec := testharness.Serve(dash, testharness.JSONRequest(t, http.MethodDelete,
		"/api/agents/"+conv, nil))
	require.Truef(t, drec.Code == http.StatusNoContent || drec.Code == http.StatusOK,
		"delete retired-dangling: code=%d body=%s", drec.Code, drec.Body.String())
	st, _ = db.AgentState(conv)
	assert.Equal(t, db.AgentStateNone, st, "retired dangling entry must be fully removed after delete")
}
