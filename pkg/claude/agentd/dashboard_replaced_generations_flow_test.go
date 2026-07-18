package agentd_test

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/convops"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

func findDashReplaced(snap dashSnapshot, convID string) *dashReplaced {
	for i := range snap.Replaced {
		if snap.Replaced[i].ConvID == convID {
			return &snap.Replaced[i]
		}
	}
	return nil
}

// Scenario: surfacing + pruning REPLACED (predecessor) conversation generations.
//
// Since PR3b the Retired tray is actor-level, so a reincarnate / Claude Code
// /clear predecessor no longer appears anywhere in the dashboard. This feature
// adds them back as a default-hidden "Replaced generations" virtual group, fed
// by the snapshot's replaced[] list, with a per-row exact-delete via the
// dedicated DELETE /api/agent-generations/{conv} endpoint.
//
// The endpoint's critical invariant (cold-review hazard): a per-generation
// delete must NEVER resolve a predecessor forward to the actor's live head — so
// it refuses the live head with 409 and only deletes the exact predecessor,
// leaving the live actor and its identity intact.
//
// Setup: an agent (group member + permission grant) at genesis conv A, /clear'd
// once → A (predecessor) and B (live head).
func TestReplacedGenerations_SnapshotAndExactDelete(t *testing.T) {
	restoreURL := agentd.SetPopupBaseURLForTest("http://127.0.0.1:0")
	t.Cleanup(restoreURL)

	f := newFlow(t)

	const (
		group = "alpha"
		convA = "aaaa1111-2222-3333-4444-555555555555"
		label = "spwn-rg-001"
		tmux  = "tclaude-spwn-rg-001"
	)
	cwd := f.TestCwd("rgwork")

	g := f.HaveGroup(group)
	f.HaveAliveSession(convA, label, tmux, cwd)
	f.HaveMember(group, convA)
	require.NoError(t, db.GrantAgentPermission(convA, "self.compact", "test"), "grant")

	// Rotate A → B. The actor stays active; A becomes a replaced generation.
	c := f.Clear(label)
	require.Equal(t, convA, c.OldConv)
	convB := c.NewConv
	actor, err := db.AgentIDForConv(convB)
	require.NoError(t, err)
	require.NotEmpty(t, actor)

	mux := agentd.BuildDashboardHandlerForTest()

	// --- Snapshot: A surfaces under replaced[], annotated with its actor; B
	//     (the live head) does NOT. ---
	snap := fetchDashSnapshot(t, mux)
	repA := findDashReplaced(snap, convA)
	require.NotNil(t, repA, "predecessor %s must appear in snapshot replaced[]; got %+v", convA, snap.Replaced)
	assert.Equal(t, convB, repA.ActorConvID, "replaced row points at the live head as its actor")
	assert.Equal(t, "clear", repA.Reason, "replaced-via reason is the rotation that superseded it")
	assert.False(t, repA.ActorRetired, "the owning actor is active, not retired")
	assert.Nil(t, findDashReplaced(snap, convB), "the live head must NOT appear under replaced[]")

	jsonl := func(conv string) string {
		return filepath.Join(convops.GetClaudeProjectPath(cwd), conv+".jsonl")
	}
	_, statErr := os.Stat(jsonl(convA))
	require.NoError(t, statErr, "precondition: predecessor .jsonl exists before delete")

	// --- Hazard guard: generation-delete of the LIVE head is refused (409),
	//     and nothing is deleted. ---
	headReq, _ := http.NewRequest(http.MethodDelete, "/api/agent-generations/"+convB, nil)
	headRec := testharness.Serve(mux, headReq)
	assert.Equal(t, http.StatusConflict, headRec.Code,
		"deleting the live head via the generation endpoint must 409; body=%s", headRec.Body.String())
	stillHead, err := db.GetAgent(actor)
	require.NoError(t, err)
	require.NotNil(t, stillHead, "the 409 must not have deleted the actor")
	assert.Equal(t, convB, stillHead.CurrentConvID, "live head unchanged after the refused delete")

	// --- Happy path: generation-delete A removes ONLY A. ---
	delReq, _ := http.NewRequest(http.MethodDelete, "/api/agent-generations/"+convA, nil)
	delRec := testharness.Serve(mux, delReq)
	require.Equal(t, http.StatusOK, delRec.Code, "generation delete body=%s", delRec.Body.String())

	// A is gone: file + DB rows + actor linkage.
	_, statErr = os.Stat(jsonl(convA))
	assert.True(t, os.IsNotExist(statErr), "predecessor .jsonl removed; stat err=%v", statErr)
	aAgent, err := db.AgentIDForConv(convA)
	require.NoError(t, err)
	assert.Empty(t, aAgent, "predecessor unlinked from its actor")

	// The live actor survives intact: head unchanged, membership + permission
	// kept, B still resolves.
	survivor, err := db.GetAgent(actor)
	require.NoError(t, err)
	require.NotNil(t, survivor, "the live actor must survive a predecessor delete")
	assert.Equal(t, convB, survivor.CurrentConvID, "live head unchanged")
	bState, err := db.AgentState(convB)
	require.NoError(t, err)
	assert.Equal(t, db.AgentStateActive, bState, "the live generation still resolves as an active agent")
	members, err := db.ListAgentGroupMembers(g.ID)
	require.NoError(t, err)
	assert.Len(t, members, 1, "the actor keeps its group membership after a predecessor delete")
	perms, err := db.ListAgentPermissionsForConv(convB)
	require.NoError(t, err)
	assert.NotEmpty(t, perms, "the actor keeps its permission grant after a predecessor delete")

	// Snapshot no longer lists A under replaced[]; B is still the live head.
	snap2 := fetchDashSnapshot(t, mux)
	assert.Nil(t, findDashReplaced(snap2, convA), "deleted predecessor must drop off replaced[]")

	// --- Guard rails: bad shape → 400, unknown/non-generation conv → 404. ---
	badReq, _ := http.NewRequest(http.MethodDelete, "/api/agent-generations/not-a-uuid", nil)
	assert.Equal(t, http.StatusBadRequest, testharness.Serve(mux, badReq).Code,
		"a non-UUID selector must 400")
	ghostReq, _ := http.NewRequest(http.MethodDelete,
		"/api/agent-generations/dddddddd-2222-3333-4444-555555555555", nil)
	assert.Equal(t, http.StatusNotFound, testharness.Serve(mux, ghostReq).Code,
		"a conv-id that is not a linked generation must 404")
}

// Scenario: the "Replaced generations" list defaults to newest-replacement-first
// ACROSS actors — the most recently superseded generation sits at the top,
// whichever actor it belongs to (the order the dashboard falls back to with no
// clickable-header sort active). Two actors each rotate once; we pin the
// rotation timestamps to distinct seconds and assert the snapshot orders the two
// predecessors by their replacement time, newest first — then flip the
// timestamps and assert the order flips with them (proving the sort keys on
// replacement time, not actor identity / insertion order).
func TestReplacedGenerations_DefaultNewestReplacementFirst(t *testing.T) {
	restoreURL := agentd.SetPopupBaseURLForTest("http://127.0.0.1:0")
	t.Cleanup(restoreURL)

	f := newFlow(t)

	const (
		group  = "alpha"
		convX0 = "aaaa3333-4444-5555-6666-777777777777"
		labelX = "spwn-rg3-x01"
		tmuxX  = "tclaude-spwn-rg3-x01"
		convY0 = "bbbb3333-4444-5555-6666-777777777777"
		labelY = "spwn-rg3-y01"
		tmuxY  = "tclaude-spwn-rg3-y01"
	)
	cwdX := f.TestCwd("rg3workx")
	cwdY := f.TestCwd("rg3worky")

	f.HaveGroup(group)

	// Actor X: genesis convX0 → /clear (convX0 becomes a replaced generation).
	f.HaveAliveSession(convX0, labelX, tmuxX, cwdX)
	f.HaveMember(group, convX0)
	f.Clear(labelX)

	// Actor Y: genesis convY0 → /clear (convY0 becomes a replaced generation).
	f.HaveAliveSession(convY0, labelY, tmuxY, cwdY)
	f.HaveMember(group, convY0)
	f.Clear(labelY)

	// A predecessor's "replaced at" is the succeeded_at of its succession edge
	// (agent_conv_succession, keyed by the predecessor's old_conv_id) — the
	// timestamp /api/replaced orders by. Pin each predecessor's edge to a
	// distinct second so the newest-replacement-first order is deterministic.
	setSucceededAt := func(oldConv, ts string) {
		t.Helper()
		d, err := db.Open()
		require.NoError(t, err)
		_, err = d.Exec(`UPDATE agent_conv_succession SET succeeded_at = ? WHERE old_conv_id = ?`, ts, oldConv)
		require.NoError(t, err)
	}
	// X replaced at 00:00:01, Y replaced at 00:00:03 → Y is the newer
	// replacement.
	setSucceededAt(convX0, "2020-01-01T00:00:01Z")
	setSucceededAt(convY0, "2020-01-01T00:00:03Z")

	mux := agentd.BuildDashboardHandlerForTest()

	// orderedReplaced returns the snapshot's replaced[] conv-ids restricted to
	// the ones we care about, in snapshot order.
	orderedReplaced := func(snap dashSnapshot, want ...string) []string {
		in := map[string]bool{}
		for _, c := range want {
			in[c] = true
		}
		var got []string
		for _, r := range snap.Replaced {
			if in[r.ConvID] {
				got = append(got, r.ConvID)
			}
		}
		return got
	}

	snap := fetchDashSnapshot(t, mux)
	assert.Equal(t, []string{convY0, convX0}, orderedReplaced(snap, convX0, convY0),
		"newest replacement (Y, 00:00:03) sorts above the older one (X, 00:00:01)")

	// Flip: make X the newer replacement (00:00:09 > Y's 00:00:03). The order
	// must follow the replacement time, not the actor — so X now leads.
	setSucceededAt(convX0, "2020-01-01T00:00:09Z")
	snap2 := fetchDashSnapshot(t, mux)
	assert.Equal(t, []string{convX0, convY0}, orderedReplaced(snap2, convX0, convY0),
		"after backdating Y below X, the newer X replacement leads")
}

// Scenario (cold-review hazard): deleting a MIDDLE generation must not strand an
// OLDER generation's stale-id forwarding. Stale references forward through the
// succession chain (agent_conv_succession), so removing generation B from a
// chain A → B → C has to re-link A → C — otherwise a reference to the genesis A
// would stop resolving to the live head C.
func TestReplacedGenerations_DeleteMiddleBridgesSuccession(t *testing.T) {
	restoreURL := agentd.SetPopupBaseURLForTest("http://127.0.0.1:0")
	t.Cleanup(restoreURL)

	f := newFlow(t)

	const (
		group = "alpha"
		convA = "aaaa2222-3333-4444-5555-666666666666"
		label = "spwn-rg2-001"
		tmux  = "tclaude-spwn-rg2-001"
	)
	cwd := f.TestCwd("rg2work")

	f.HaveGroup(group)
	f.HaveAliveSession(convA, label, tmux, cwd)
	f.HaveMember(group, convA)

	// Two rotations → A (genesis) → B → C (live head).
	c1 := f.Clear(label)
	convB := c1.NewConv
	c2 := f.Clear(label)
	require.Equal(t, convB, c2.OldConv)
	convC := c2.NewConv

	// Precondition: the genesis forwards to the live head.
	require.Equal(t, convC, db.ResolveLatestConv(convA), "A forwards to head before delete")

	mux := agentd.BuildDashboardHandlerForTest()

	// Delete the MIDDLE generation B via the dedicated endpoint.
	req, _ := http.NewRequest(http.MethodDelete, "/api/agent-generations/"+convB, nil)
	rec := testharness.Serve(mux, req)
	require.Equal(t, http.StatusOK, rec.Code, "delete middle generation body=%s", rec.Body.String())

	// B is gone; the chain is healed so A still forwards to the live head C.
	bAgent, err := db.AgentIDForConv(convB)
	require.NoError(t, err)
	assert.Empty(t, bAgent, "the middle generation is unlinked")
	assert.Equal(t, convC, db.ResolveLatestConv(convA),
		"the genesis still forwards to the live head after the middle delete")
	res, _, err := agent.ResolveSelector(convA)
	require.NoError(t, err)
	assert.Equal(t, convC, res.ConvID, "ResolveSelector(genesis) resolves to the live head")

	// A is still a replaced generation of the live actor; B has dropped off.
	snap := fetchDashSnapshot(t, mux)
	repA := findDashReplaced(snap, convA)
	require.NotNil(t, repA, "the genesis generation still appears under replaced[]")
	assert.Equal(t, convC, repA.ActorConvID, "and still points at the live head")
	assert.Nil(t, findDashReplaced(snap, convB), "the deleted middle generation drops off replaced[]")
}
