package agentd_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/session"
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
	cwd := f.TestCwd("rw-linked")
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
	require.Contains(t, fw.branchesRemoved(), "feat", "the branch must be passed to the removal seam")
	assert.False(t, f.World.Tmux.IsAlive("tmux-rwwt"), "shutdown must stop the session")

	// Retire semantics still hold — the agent leaves the active roster.
	snap := fetchDashSnapshot(t, mux)
	assert.False(t, agentInSnap(snap.Agents, conv), "a retired agent leaves the active roster")
	require.NotNil(t, retiredRow(snap, conv), "the retired agent must appear in retired[]")
}

// A detached worktree whose directory vanished still has a Git registration.
// Retire exposes and removes that registration through the surviving main
// checkout, without inventing a branch deletion for detached HEAD.
func TestRetire_RemovesMissingDetachedRegisteredWorktree(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)

	const conv = "rwmd-1111-2222-3333-4444"
	repo := f.TestCwd("rw-missing-detached-main")
	missing := filepath.Join(filepath.Dir(repo), "rw-missing-detached")
	f.HaveConvWithTitle(conv, "missing-detached-worker")
	f.HaveAliveSession(conv, "spwn-rwmd", "tmux-rwmd", missing)
	f.MarkOffline("tmux-rwmd")
	f.HaveEnrolledAgent(conv)
	f.HaveGroup("squad")
	_, err := db.SetAgentGroupDefaultCwd("squad", repo)
	require.NoError(t, err)

	fw := installFakeWorktrees(t, map[string]worktree.WorktreeStatus{
		repo: {Root: repo, Branch: "main", Kind: "main"},
	})
	fw.setRegisteredBranch(missing, "")
	t.Cleanup(agentd.SetSweepWorktreeFnsForTest(
		func(string) ([]worktree.WorktreeInfo, error) {
			return []worktree.WorktreeInfo{
				{Path: repo, Branch: "main", IsMain: true},
				{Path: missing},
			}, nil
		},
		func(path string) (string, error) {
			if path == repo {
				return repo, nil
			}
			return "", assertNotRepo(path)
		},
		func(string) bool { return false },
	))

	mux := agentd.BuildDashboardHandlerForTest()
	probe := testharness.Serve(mux,
		testharness.JSONRequest(t, http.MethodGet, "/api/agents/"+conv+"/worktree", nil))
	require.Equal(t, http.StatusOK, probe.Code, "body=%s", probe.Body.String())
	assert.Contains(t, probe.Body.String(), `"kind":"linked"`)
	assert.Contains(t, probe.Body.String(), `"removable":true`)

	code, resp := postRetireWt(t, mux, conv, "shutdown=1&delete_worktree=1")
	require.Equal(t, http.StatusOK, code)
	require.NotNil(t, resp.Worktree)
	assert.Equal(t, "removed", resp.Worktree.Action)
	assert.Equal(t, "worktree removed", resp.Worktree.Detail)
	assert.True(t, fw.wasRemoved(missing))
	assert.NotContains(t, fw.branchesRemoved(), "main")
}

// Scenario: delete_worktree WITHOUT shutdown keeps the worktree — we
// never yank a worktree out from under a still-running agent. The
// response says the worktree was kept and the seam is never hit.
func TestRetire_DeleteWorktreeWithoutShutdownKeepsWorktree(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)

	const conv = "rwns-1111-2222-3333-4444"
	cwd := f.TestCwd("rw-keep")
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
	shared := f.TestCwd("rw-shared")
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

// Regression for TCL-581: the surviving agent's PostToolUse tracker can point
// somewhere other than the directory it was launched in. Its startup root is
// still its process cwd and must remain claimed independently; retiring the
// last disposable sibling must not delete that live agent's root and branch.
func TestRetire_DeleteWorktreeKeepsLiveSiblingStartupRoot(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)

	const leaving = "rwsl-1111-2222-3333-4444"
	const original = "rwso-1111-2222-3333-4444"
	repo, _ := initRepoOnMain(t)
	root, err := worktree.AddWorktreeIn(repo, "original-agent", "main", "")
	require.NoError(t, err)
	trackedElsewhere := t.TempDir()
	f.HaveConvWithTitle(leaving, "disposable-sibling")
	f.HaveConvWithTitle(original, "original-agent")
	f.HaveAliveSession(leaving, "spwn-rwsl", "tmux-rwsl", root)
	f.HaveAliveSession(original, "spwn-rwso", "tmux-rwso", root)
	f.HaveEnrolledAgent(leaving)
	f.HaveEnrolledAgent(original)
	require.NoError(t, db.UpsertAgentWorkdir(original, trackedElsewhere, trackedElsewhere, "other"))
	mux := agentd.BuildDashboardHandlerForTest()
	code, resp := postRetireWt(t, mux, leaving, "shutdown=1&delete_worktree=1")
	require.Equal(t, http.StatusOK, code)
	require.NotNil(t, resp.Worktree)
	assert.Equal(t, "kept", resp.Worktree.Action)
	assert.Contains(t, resp.Worktree.Detail, "shared")
	assert.DirExists(t, root, "another live agent's startup root must survive")
	assert.NoError(t, exec.Command("git", "-C", repo, "show-ref", "--verify",
		"refs/heads/original-agent").Run(),
		"the live agent's startup branch must survive with its directory")
}

// A launch-time symlink may disappear while the pane still has the resolved
// physical directory as its cwd. The immutable provenance captured at launch
// must keep that physical root claimed; the now-dangling lexical startup path
// alone cannot do so.
func TestRetire_DeleteWorktreeKeepsPhysicalStartupAfterAliasRemoved(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)

	const leaving = "rwal-1111-2222-3333-4444"
	const original = "rwap-1111-2222-3333-4444"
	repo, base := initRepoOnMain(t)
	root, err := worktree.AddWorktreeIn(repo, "physical-owner", "main", "")
	require.NoError(t, err)
	alias := filepath.Join(base, "launch-alias")
	trackedElsewhere := t.TempDir()
	require.NoError(t, os.Symlink(root, alias))

	f.HaveConvWithTitle(leaving, "disposable-alias-sibling")
	f.HaveConvWithTitle(original, "physical-root-owner")
	f.HaveAliveSession(leaving, "spwn-rwal", "tmux-rwal", root)
	f.HaveAliveSession(original, "spwn-rwap", "tmux-rwap", alias)
	f.HaveEnrolledAgent(leaving)
	f.HaveEnrolledAgent(original)
	require.NoError(t, db.UpsertAgentWorkdir(original, trackedElsewhere, trackedElsewhere, "other"))
	require.NoError(t, os.Remove(alias), "simulate launch alias disappearing")
	mux := agentd.BuildDashboardHandlerForTest()
	code, resp := postRetireWt(t, mux, leaving, "shutdown=1&delete_worktree=1")
	require.Equal(t, http.StatusOK, code)
	require.NotNil(t, resp.Worktree)
	assert.Equal(t, "kept", resp.Worktree.Action)
	assert.Contains(t, resp.Worktree.Detail, "shared")
	assert.DirExists(t, root,
		"the live pane's physical startup root must remain claimed after its alias disappears")
}

// Scenario: an agent is retired, then a fresh agent reuses the same name and
// worktree path. The old conversation's session/location row remains in the
// database by design, but its pane is offline and its actor is retired. The
// fresh agent's retire preview must not mistake that historical row for a
// surviving worktree claimant (the dashboard would otherwise disable its
// delete-worktree checkbox as "shared with another agent").
func TestRetireWorktreePreview_IgnoresOfflineRetiredPriorAgent(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)

	const prior = "rwpr-1111-2222-3333-4444"
	const current = "rwcu-1111-2222-3333-4444"
	reused := f.TestCwd("rw-reused-name")
	const reusedTmux = "rw-reused-name"
	f.HaveConvWithTitle(prior, "banana")
	f.HaveAliveSession(prior, "spwn-rwpr", reusedTmux, reused)
	f.HaveEnrolledAgent(prior)
	f.MarkOffline(reusedTmux)
	f.HaveRetiredAgent(prior)

	f.HaveConvWithTitle(current, "banana")
	f.HaveAliveSession(current, "spwn-rwcu", reusedTmux, reused)
	f.HaveEnrolledAgent(current)
	// SessionEnd can persist exited just before tmux actually disappears. The
	// newest launch still owns the reused live name; an older non-exited row
	// must not steal it merely because of status.
	currentSession, err := db.LoadSession("spwn-rwcu")
	require.NoError(t, err)
	currentSession.Status = session.StatusExited
	require.NoError(t, db.SaveSession(currentSession))
	installFakeWorktrees(t, map[string]worktree.WorktreeStatus{
		reused: {Root: reused, Branch: "banana", Kind: "linked"},
	})

	mux := agentd.BuildDashboardHandlerForTest()
	rec := testharness.Serve(mux,
		testharness.JSONRequest(t, http.MethodGet, "/api/agents/"+current+"/worktree", nil))
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	var got struct {
		Shared    bool `json:"shared"`
		Removable bool `json:"removable"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.False(t, got.Shared, "an offline retired predecessor is historical, not a surviving claimant")
	assert.True(t, got.Removable, "the fresh agent must be allowed to remove its reused worktree")
}

// A retired actor whose pane was deliberately left running is different from
// the stale-row case above: its process still has this worktree as its cwd, so
// the preview must continue protecting the directory.
func TestRetireWorktreePreview_KeepsWorktreeClaimedByLiveRetiredPane(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)

	const prior = "rwlr-1111-2222-3333-4444"
	const current = "rwlc-1111-2222-3333-4444"
	reused := f.TestCwd("rw-live-retired")
	f.HaveConvWithTitle(prior, "banana")
	f.HaveAliveSession(prior, "spwn-rwlr", "tmux-rwlr", reused)
	f.HaveEnrolledAgent(prior)
	f.HaveRetiredAgent(prior) // pane intentionally remains alive

	f.HaveConvWithTitle(current, "banana")
	f.HaveAliveSession(current, "spwn-rwlc", "tmux-rwlc", reused)
	f.HaveEnrolledAgent(current)
	installFakeWorktrees(t, map[string]worktree.WorktreeStatus{
		reused: {Root: reused, Branch: "banana", Kind: "linked"},
	})

	mux := agentd.BuildDashboardHandlerForTest()
	rec := testharness.Serve(mux,
		testharness.JSONRequest(t, http.MethodGet, "/api/agents/"+current+"/worktree", nil))
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	var got struct {
		Shared    bool `json:"shared"`
		Removable bool `json:"removable"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.True(t, got.Shared, "a live retired pane still owns its cwd")
	assert.False(t, got.Removable, "cleanup must not remove a running pane's worktree")
}

// Scenario: the repo's main worktree is never removed by retire.
func TestRetire_DeleteWorktreeKeepsMainWorktree(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)

	const conv = "rwmn-1111-2222-3333-4444"
	cwd := f.TestCwd("rw-main")
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
	cwd := f.TestCwd("rw-untouched")
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
// runs (the fixture holds its pane open), so the response reports "scheduled"
// and the worktree is removed by a background waiter once the pane
// exits. The deferred outcome is also surfaced in the dashboard
// Messages tab, since the optimistic toast already fired.
func TestRetire_DeleteWorktreeDeferredUntilAgentExits(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)

	const conv = "rwdf-1111-2222-3333-4444"
	cwd := f.TestCwd("rw-deferred")
	f.HaveConvWithTitle(conv, "slow-exit-worker")
	f.HaveAliveSession(conv, "spwn-rwdf", "tmux-rwdf", cwd)
	f.HaveEnrolledAgent(conv)
	fw := installFakeWorktrees(t, map[string]worktree.WorktreeStatus{
		cwd: {Root: cwd, Branch: "feat", Kind: "linked"},
	})

	// Hold the pane open so the agent is still alive when the retire handler
	// decides what to do — forcing the deferred path rather than the inline
	// (already-offline) one. The hold, not a wall-clock delay, is what makes
	// the handler's liveness check see a live pane every time.
	cc := f.World.CCs.GetByConvID(conv)
	require.NotNil(t, cc, "no CCSim registered for %s", conv)
	exitPane := holdRetiringPane(t, f, cc, "tmux-rwdf")

	mux := agentd.BuildDashboardHandlerForTest()
	code, resp := postRetireWt(t, mux, conv, "shutdown=1&delete_worktree=1")
	require.Equal(t, http.StatusOK, code)
	require.NotNil(t, resp.Worktree)
	assert.Equal(t, "scheduled", resp.Worktree.Action,
		"a still-alive agent defers the removal; detail=%s", resp.Worktree.Detail)
	// At response time the worktree must NOT yet be removed — the agent
	// is still exiting.
	assert.False(t, fw.wasRemoved(cwd), "removal must wait until the agent exits")

	// Now let the pane go, then drain the background waiter; it polls until
	// the pane goes offline, then removes the worktree.
	exitPane()
	agentd.WaitForBackgroundForTest()

	assert.True(t, fw.wasRemoved(cwd), "the worktree must be removed after the agent exits")
	require.Contains(t, fw.branchesRemoved(), "feat")

	// A SUCCESSFUL deferred delete is silent — it matches the optimistic
	// toast, so it must NOT post a Messages-tab notice (no success noise).
	msgs, err := db.ListHumanMessages()
	require.NoError(t, err)
	assert.Empty(t, msgs, "a successful deferred delete must not post a human notice; got %+v", msgs)
}

// Scenario: a DEFERRED delete that FAILS (git removal errors) DOES post
// a human notice — the optimistic toast already fired, so the human must
// learn the promise wasn't kept.
func TestRetire_DeleteWorktreeDeferredFailurePostsNotice(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)

	const conv = "rwfa-1111-2222-3333-4444"
	cwd := f.TestCwd("rw-fail")
	f.HaveConvWithTitle(conv, "fail-worker")
	f.HaveAliveSession(conv, "spwn-rwfa", "tmux-rwfa", cwd)
	f.HaveEnrolledAgent(conv)
	fw := installFakeWorktrees(t, map[string]worktree.WorktreeStatus{
		cwd: {Root: cwd, Branch: "feat", Kind: "linked"},
	})
	fw.removeErr = errors.New("git worktree remove: permission denied")

	cc := f.World.CCs.GetByConvID(conv)
	require.NotNil(t, cc, "no CCSim registered for %s", conv)
	exitPane := holdRetiringPane(t, f, cc, "tmux-rwfa")

	mux := agentd.BuildDashboardHandlerForTest()
	code, resp := postRetireWt(t, mux, conv, "shutdown=1&delete_worktree=1")
	require.Equal(t, http.StatusOK, code)
	require.NotNil(t, resp.Worktree)
	assert.Equal(t, "scheduled", resp.Worktree.Action)

	exitPane()
	agentd.WaitForBackgroundForTest()

	msgs, err := db.ListHumanMessages()
	require.NoError(t, err)
	require.NotEmpty(t, msgs, "a FAILED deferred delete must post a human notice")
	assert.Contains(t, msgs[0].Subject, "failed")
	assert.Contains(t, msgs[0].Body, "permission denied",
		"the notice should carry the failure reason; body=%q", msgs[0].Body)
}

// Scenario: a delete whose agent ignores /exit. Before TCL-1001 this was the
// leak the operator reported: nothing stronger followed the soft exit, so the
// grace expired with the pane still running and the delete was skipped. The
// escalation ladder finishes the pane, so the human's delete actually happens
// and no "kept" notice is needed.
//
// The retire now WAITS for that ladder, so convergence happens inside the
// request: the pane is already gone when the worktree step runs, and the
// removal is INLINE ("removed") rather than a promise ("scheduled"). The
// operator gets the real outcome in the response instead of a maybe.
func TestRetire_DeleteWorktreeEscalatesPastAgentThatIgnoresExit(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	// Shrink the grace so the deferred waiter resolves fast instead of after
	// the production 60s. It still comfortably outlasts the (also shrunken)
	// escalation deadline, which is the production relationship being pinned.
	t.Cleanup(agentd.SetRetireWorktreeGraceForTest(2 * time.Second))
	f := newFlow(t)

	const conv = "rwhe-1111-2222-3333-4444"
	cwd := f.TestCwd("rw-hung")
	f.HaveConvWithTitle(conv, "hung-worker")
	f.HaveAliveSession(conv, "spwn-rwhe", "tmux-rwhe", cwd)
	f.HaveEnrolledAgent(conv)
	fw := installFakeWorktrees(t, map[string]worktree.WorktreeStatus{
		cwd: {Root: cwd, Branch: "feat", Kind: "linked"},
	})

	// Hung agent: it consumes /exit and never goes offline on its own.
	cc := f.World.CCs.GetByConvID(conv)
	require.NotNil(t, cc, "no CCSim registered for %s", conv)
	cc.OnInput("/exit", func(c *testharness.CCSim, line string) bool {
		_ = c.WriteUserTurn("[hung agent: /exit ignored]")
		return true // consume — never MarkDead
	})

	mux := agentd.BuildDashboardHandlerForTest()
	code, resp := postRetireWt(t, mux, conv, "shutdown=1&delete_worktree=1")
	require.Equal(t, http.StatusOK, code)
	require.NotNil(t, resp.Worktree)
	assert.Equal(t, "removed", resp.Worktree.Action,
		"the wait converges the ladder inside the request, so the delete is done, not promised; detail=%s",
		resp.Worktree.Detail)

	agentd.WaitForBackgroundForTest()

	assert.False(t, f.World.Tmux.IsAlive("tmux-rwhe"),
		"retire must converge: an agent that ignores /exit gets escalated, not waited out")
	assert.True(t, fw.wasRemoved(cwd),
		"the promised worktree delete must happen once escalation ends the pane")
	msgs, err := db.ListHumanMessages()
	require.NoError(t, err)
	assert.Empty(t, msgs,
		"a delete that was kept as promised stays silent; got %+v", msgs)
}

// Scenario: the residual failure the "kept" notice still exists for — a pane
// that survives EVERY rung of the ladder (tmux kill reports success and
// changes nothing; the signals reach nothing). The human must still be told
// the promised delete did not happen.
func TestRetire_DeleteWorktreeUnkillableAgentPostsKeptNotice(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	t.Cleanup(agentd.SetRetireWorktreeGraceForTest(300 * time.Millisecond))
	f := newFlow(t)
	// Nothing the daemon does can end this process: the signals are delivered
	// to a stub that keeps reporting the pid alive.
	//
	// Installed AFTER newFlow and TestMain's binary-wide neutral pair
	// (TCL-1035) — installing before them would be silently overwritten and
	// this scenario would stop reaching the signal rungs it exists for.
	// cleanupAfterBackgroundDrain is what keeps the restore from racing the
	// ladder goroutine that reads these hooks.
	//
	// The recorded signals are ASSERTED at the end, and that is what makes the
	// ordering above enforce itself rather than be a comment. Without the
	// assertion this scenario passes either way: the kill-resistant pane keeps
	// the session listed, the grace expires and the kept notice is posted
	// whether the ladder reached the signal rungs or stood down at kill-pane.
	// Someone reinstating the old ordering would get a green test that had
	// quietly stopped covering the path it is named for.
	var (
		signalMu  sync.Mutex
		signalled []syscall.Signal
	)
	cleanupAfterBackgroundDrain(t, agentd.SetSoftExitEscalationProcessForTest(
		func(int) bool { return true },
		func(_ int, sig syscall.Signal) error {
			signalMu.Lock()
			defer signalMu.Unlock()
			signalled = append(signalled, sig)
			return nil
		},
	))

	const conv = "rwhu-1111-2222-3333-4444"
	cwd := f.TestCwd("rw-unkillable")
	f.HaveConvWithTitle(conv, "unkillable-worker")
	f.HaveAliveSession(conv, "spwn-rwhu", "tmux-rwhu", cwd)
	f.HaveEnrolledAgent(conv)
	fw := installFakeWorktrees(t, map[string]worktree.WorktreeStatus{
		cwd: {Root: cwd, Branch: "feat", Kind: "linked"},
	})
	f.World.Tmux.SetKillResistantForTest("tmux-rwhu", true)

	cc := f.World.CCs.GetByConvID(conv)
	require.NotNil(t, cc, "no CCSim registered for %s", conv)
	cc.OnInput("/exit", func(c *testharness.CCSim, line string) bool {
		_ = c.WriteUserTurn("[hung agent: /exit ignored]")
		return true
	})

	mux := agentd.BuildDashboardHandlerForTest()
	code, resp := postRetireWt(t, mux, conv, "shutdown=1&delete_worktree=1")
	require.Equal(t, http.StatusOK, code)
	require.NotNil(t, resp.Worktree)
	assert.Equal(t, "scheduled", resp.Worktree.Action)

	agentd.WaitForBackgroundForTest()

	assert.False(t, fw.wasRemoved(cwd),
		"a worktree must never be removed under a still-running agent")
	msgs, err := db.ListHumanMessages()
	require.NoError(t, err)
	require.NotEmpty(t, msgs, "a kept (agent-never-exited) outcome must post a human notice")
	assert.Contains(t, msgs[0].Subject, "kept")
	assert.Contains(t, msgs[0].Body, "did not exit")

	signalMu.Lock()
	defer signalMu.Unlock()
	assert.Equal(t, []syscall.Signal{syscall.SIGTERM, syscall.SIGKILL}, signalled,
		"this scenario exists to cover a process that survives the WHOLE ladder; "+
			"an empty list means it stood down at kill-pane and stopped covering it")
}
