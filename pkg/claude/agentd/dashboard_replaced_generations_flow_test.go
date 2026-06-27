package agentd_test

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
		cwd   = "/tmp/rgwork"
	)

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
