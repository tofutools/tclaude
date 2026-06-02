package agentd_test

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/worktree"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// Flow coverage for retire + optional worktree/branch cleanup: a retire
// can also remove the git worktree the agent was working in AND delete
// its local branch. The removal must run only AFTER the agent's process
// exits (its cwd IS the worktree), so the handler removes inline when
// the agent is already offline and defers otherwise. These scenarios
// drive the per-row retire button (POST /api/agents/{conv}/retire) with
// ?delete_worktree=1 and assert the worktree seam was (or wasn't) hit.

// retireWtResp decodes the retire response's worktree sub-object, which
// the ?delete_worktree path adds.
type retireWtResp struct {
	ConvID   string `json:"conv_id"`
	Shutdown *struct {
		Action string `json:"action"`
	} `json:"shutdown"`
	Worktree *struct {
		Action string `json:"action"`
		Detail string `json:"detail"`
	} `json:"worktree"`
}

// postRetireWt fires the retire request with a raw query string and
// decodes the worktree-aware response.
func postRetireWt(t *testing.T, mux http.Handler, conv, query string) (int, retireWtResp) {
	t.Helper()
	path := "/api/agents/" + conv + "/retire"
	if query != "" {
		path += "?" + query
	}
	rec := testharness.Serve(mux, testharness.JSONRequest(t, http.MethodPost, path, nil))
	var resp retireWtResp
	if rec.Code == http.StatusOK {
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp),
			"decode retire response: %s", rec.Body.String())
	}
	return rec.Code, resp
}

// Scenario: retire with shutdown + delete_worktree removes the linked
// worktree AND deletes its branch. The sim's /exit is synchronous, so
// the agent is already offline by the time cleanup runs — the removal
// happens inline and the response reports it.
func TestRetire_DeleteWorktreeRemovesWorktreeAndBranch(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)

	const conv = "rwwt-1111-2222-3333-4444"
	const cwd = "/tmp/rw-linked"
	f.HaveConvWithTitle(conv, "wt-worker")
	f.HaveAliveSession(conv, "spwn-rwwt", "tmux-rwwt", cwd)
	f.HaveEnrolledAgent(conv)
	fw := installFakeWorktrees(t, map[string]worktree.WorktreeStatus{
		cwd: {Root: cwd, Branch: "feat", Kind: "linked"},
	})

	mux := agentd.BuildDashboardHandlerForTest()
	code, resp := postRetireWt(t, mux, conv, "shutdown=1&delete_worktree=1")
	require.Equal(t, http.StatusOK, code)
	require.NotNil(t, resp.Worktree, "delete_worktree must report a worktree outcome")
	assert.Equal(t, "removed", resp.Worktree.Action,
		"an already-offline agent's worktree is removed inline; detail=%s", resp.Worktree.Detail)
	assert.Contains(t, resp.Worktree.Detail, "branch feat")

	assert.True(t, fw.wasRemoved(cwd), "the linked worktree must be removed")
	require.Contains(t, fw.branchRemoved, "feat", "the branch must be passed to the removal seam")
	assert.False(t, f.World.Tmux.IsAlive("tmux-rwwt"), "shutdown must stop the session")

	// Retire semantics still hold — the agent leaves the active roster.
	snap := fetchDashSnapshot(t, mux)
	assert.False(t, agentInSnap(snap.Agents, conv), "a retired agent leaves the active roster")
	require.NotNil(t, retiredRow(snap, conv), "the retired agent must appear in retired[]")
}

// Scenario: delete_worktree WITHOUT shutdown keeps the worktree — we
// never yank a worktree out from under a still-running agent. The
// response says the worktree was kept and the seam is never hit.
func TestRetire_DeleteWorktreeWithoutShutdownKeepsWorktree(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)

	const conv = "rwns-1111-2222-3333-4444"
	const cwd = "/tmp/rw-keep"
	f.HaveConvWithTitle(conv, "kept-wt-worker")
	f.HaveAliveSession(conv, "spwn-rwns", "tmux-rwns", cwd)
	f.HaveEnrolledAgent(conv)
	fw := installFakeWorktrees(t, map[string]worktree.WorktreeStatus{
		cwd: {Root: cwd, Branch: "feat", Kind: "linked"},
	})

	mux := agentd.BuildDashboardHandlerForTest()
	code, resp := postRetireWt(t, mux, conv, "shutdown=0&delete_worktree=1")
	require.Equal(t, http.StatusOK, code)
	require.NotNil(t, resp.Worktree)
	assert.Equal(t, "kept", resp.Worktree.Action)
	assert.Contains(t, resp.Worktree.Detail, "still running")

	assert.False(t, fw.wasRemoved(cwd), "a live agent's worktree must not be removed")
	assert.True(t, f.World.Tmux.IsAlive("tmux-rwns"), "shutdown=0 keeps the session alive")
}

// Scenario: a worktree another surviving agent still works in is kept,
// even when one of its sharers is retired with delete_worktree.
func TestRetire_DeleteWorktreeKeepsSharedWorktree(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)

	const leaving = "rwsh-1111-2222-3333-4444"
	const staying = "rwst-1111-2222-3333-4444"
	const shared = "/tmp/rw-shared"
	f.HaveConvWithTitle(leaving, "leaving")
	f.HaveConvWithTitle(staying, "staying")
	f.HaveAliveSession(leaving, "spwn-rwsh", "tmux-rwsh", shared)
	f.HaveAliveSession(staying, "spwn-rwst", "tmux-rwst", shared)
	f.HaveEnrolledAgent(leaving)
	f.HaveEnrolledAgent(staying)
	fw := installFakeWorktrees(t, map[string]worktree.WorktreeStatus{
		shared: {Root: shared, Branch: "feat", Kind: "linked"},
	})

	mux := agentd.BuildDashboardHandlerForTest()
	code, resp := postRetireWt(t, mux, leaving, "shutdown=1&delete_worktree=1")
	require.Equal(t, http.StatusOK, code)
	require.NotNil(t, resp.Worktree)
	assert.Equal(t, "kept", resp.Worktree.Action)
	assert.Contains(t, resp.Worktree.Detail, "shared")
	assert.False(t, fw.wasRemoved(shared),
		"a worktree another agent still works in must be kept")
}

// Scenario: the repo's main worktree is never removed by retire.
func TestRetire_DeleteWorktreeKeepsMainWorktree(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)

	const conv = "rwmn-1111-2222-3333-4444"
	const cwd = "/tmp/rw-main"
	f.HaveConvWithTitle(conv, "main-repo-worker")
	f.HaveAliveSession(conv, "spwn-rwmn", "tmux-rwmn", cwd)
	f.HaveEnrolledAgent(conv)
	fw := installFakeWorktrees(t, map[string]worktree.WorktreeStatus{
		cwd: {Root: cwd, Branch: "main", Kind: "main"},
	})

	mux := agentd.BuildDashboardHandlerForTest()
	code, resp := postRetireWt(t, mux, conv, "shutdown=1&delete_worktree=1")
	require.Equal(t, http.StatusOK, code)
	require.NotNil(t, resp.Worktree)
	assert.Equal(t, "kept", resp.Worktree.Action)
	assert.Contains(t, resp.Worktree.Detail, "main repo")
	assert.False(t, fw.wasRemoved(cwd), "the main worktree must never be removed")
}

// Scenario: retire WITHOUT delete_worktree leaves the worktree entirely
// untouched — no worktree outcome at all, the pre-feature behaviour.
func TestRetire_NoDeleteWorktreeLeavesWorktreeUntouched(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)

	const conv = "rwno-1111-2222-3333-4444"
	const cwd = "/tmp/rw-untouched"
	f.HaveConvWithTitle(conv, "untouched-worker")
	f.HaveAliveSession(conv, "spwn-rwno", "tmux-rwno", cwd)
	f.HaveEnrolledAgent(conv)
	fw := installFakeWorktrees(t, map[string]worktree.WorktreeStatus{
		cwd: {Root: cwd, Branch: "feat", Kind: "linked"},
	})

	mux := agentd.BuildDashboardHandlerForTest()
	code, resp := postRetireWt(t, mux, conv, "shutdown=1")
	require.Equal(t, http.StatusOK, code)
	assert.Nil(t, resp.Worktree, "no delete_worktree → no worktree outcome reported")
	assert.False(t, fw.wasRemoved(cwd), "the worktree must be untouched without delete_worktree")
}

// Scenario: the DEFERRED path — the agent is still alive when retire
// runs (its /exit takes a moment), so the response reports "scheduled"
// and the worktree is removed by a background waiter once the pane
// exits. The deferred outcome is also surfaced in the dashboard
// Messages tab, since the optimistic toast already fired.
func TestRetire_DeleteWorktreeDeferredUntilAgentExits(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)

	const conv = "rwdf-1111-2222-3333-4444"
	const cwd = "/tmp/rw-deferred"
	f.HaveConvWithTitle(conv, "slow-exit-worker")
	f.HaveAliveSession(conv, "spwn-rwdf", "tmux-rwdf", cwd)
	f.HaveEnrolledAgent(conv)
	fw := installFakeWorktrees(t, map[string]worktree.WorktreeStatus{
		cwd: {Root: cwd, Branch: "feat", Kind: "linked"},
	})

	// Make /exit take a moment so the agent is still alive when the
	// retire handler decides what to do — forcing the deferred path
	// rather than the inline (already-offline) one. With the flow
	// harness shrinking injectTextAndSubmit's settle gap to ~nothing,
	// stopOneConv returns in milliseconds, so a short delay is plenty of
	// margin for the handler's liveness check.
	cc := f.World.CCs.GetByConvID(conv)
	require.NotNil(t, cc, "no CCSim registered for %s", conv)
	cc.SetCommandDelay("/exit", 200*time.Millisecond)

	mux := agentd.BuildDashboardHandlerForTest()
	code, resp := postRetireWt(t, mux, conv, "shutdown=1&delete_worktree=1")
	require.Equal(t, http.StatusOK, code)
	require.NotNil(t, resp.Worktree)
	assert.Equal(t, "scheduled", resp.Worktree.Action,
		"a still-alive agent defers the removal; detail=%s", resp.Worktree.Detail)
	// At response time the worktree must NOT yet be removed — the agent
	// is still exiting.
	assert.False(t, fw.wasRemoved(cwd), "removal must wait until the agent exits")

	// Drain the background waiter; it polls until the pane goes offline,
	// then removes the worktree and posts the outcome.
	agentd.WaitForBackgroundForTest()

	assert.True(t, fw.wasRemoved(cwd), "the worktree must be removed after the agent exits")
	require.Contains(t, fw.branchRemoved, "feat")

	// The deferred outcome is surfaced to the human (Messages tab).
	msgs, err := db.ListHumanMessages()
	require.NoError(t, err)
	require.NotEmpty(t, msgs, "the deferred cleanup must post a human-facing notice")
	assert.Contains(t, msgs[0].Body, "feat",
		"the notice should name the removed worktree/branch; body=%q", msgs[0].Body)
}
