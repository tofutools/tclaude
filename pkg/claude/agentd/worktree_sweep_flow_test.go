package agentd_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/worktree"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// worktree_sweep_flow_test.go exercises the repo-wide worktree janitor
// (worktree_sweep.go) end to end through the dashboard mux, with the git
// seam faked so no real repos are needed. Two surfaces:
//
//	GET  /api/groups/{name}/worktrees   — discovery + classification
//	POST /api/worktrees/cleanup         — explicit-path removal + prune
//
// The fake models one repo "/repo" with eight worktrees: the main repo, a
// live-agent worktree, an offline (still-enrolled) agent worktree, a clean
// orphan, a dirty orphan, a clean retired-agent worktree, a dirty
// retired-agent worktree — one of each classification the modal renders —
// plus a mixed worktree bound to BOTH a retired and a still-active agent,
// which must stay protected "agent" (the "all retired" gate, not "any").

// --- wire shapes (mirror the unexported handler responses) -----------

type sweepAgentWire struct {
	ConvID  string `json:"conv_id"`
	Title   string `json:"title"`
	Online  bool   `json:"online"`
	Retired bool   `json:"retired"`
}

type sweepWorktreeWire struct {
	Path     string           `json:"path"`
	Name     string           `json:"name"`
	Branch   string           `json:"branch"`
	RepoRoot string           `json:"repo_root"`
	IsMain   bool             `json:"is_main"`
	Dirty    bool             `json:"dirty"`
	Agents   []sweepAgentWire `json:"agents"`
	Category string           `json:"category"`
	Checked  bool             `json:"checked"`
	Reason   string           `json:"reason"`
}

type sweepDiscoverWire struct {
	Group     string              `json:"group"`
	RepoRoots []string            `json:"repo_roots"`
	Worktrees []sweepWorktreeWire `json:"worktrees"`
}

type sweepCleanupWire struct {
	Outcomes []struct {
		Path   string `json:"path"`
		Branch string `json:"branch"`
		Result string `json:"result"`
		Detail string `json:"detail"`
	} `json:"outcomes"`
	Removed  int `json:"removed"`
	Branches int `json:"branches"`
	Skipped  int `json:"skipped"`
	Failed   int `json:"failed"`
}

// --- the git seam fake ----------------------------------------------

type fakeSweep struct {
	worktrees []worktree.WorktreeInfo            // the repo's full worktree list
	roots     map[string]string                  // candidate dir -> repo root
	statuses  map[string]worktree.WorktreeStatus // dir -> inspect result
	dirty     map[string]bool                    // worktree path -> dirty
	mainRepo  string                             // resolved main repo for any worktree

	mu      sync.Mutex
	removed []string
	pruned  []string
}

func (f *fakeSweep) list(string) ([]worktree.WorktreeInfo, error) {
	// git worktree list is repo-global — the full list regardless of which
	// worktree root we anchor at.
	return f.worktrees, nil
}

func (f *fakeSweep) repoRoot(path string) (string, error) {
	if r, ok := f.roots[path]; ok {
		return r, nil
	}
	return "", assertNotRepo(path)
}

func (f *fakeSweep) inspect(dir string) worktree.WorktreeStatus {
	if st, ok := f.statuses[dir]; ok {
		return st
	}
	return worktree.WorktreeStatus{Kind: "none"}
}

func (f *fakeSweep) dirtyFn(dir string) bool { return f.dirty[dir] }

func (f *fakeSweep) main(string) string { return f.mainRepo }

func (f *fakeSweep) prune(dir string) error {
	f.mu.Lock()
	f.pruned = append(f.pruned, dir)
	f.mu.Unlock()
	return nil
}

func (f *fakeSweep) remove(root string, _ bool) (bool, error) {
	f.mu.Lock()
	f.removed = append(f.removed, root)
	f.mu.Unlock()
	return true, nil
}

func (f *fakeSweep) removeBranch(root, branch string, _ bool) (bool, bool, error) {
	f.mu.Lock()
	f.removed = append(f.removed, root)
	f.mu.Unlock()
	deleted := branch != "" && strings.ToLower(branch) != "main" && strings.ToLower(branch) != "master"
	return true, deleted, nil
}

func (f *fakeSweep) wasRemoved(root string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range f.removed {
		if r == root {
			return true
		}
	}
	return false
}

func (f *fakeSweep) wasPruned(dir string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, p := range f.pruned {
		if p == dir {
			return true
		}
	}
	return false
}

func assertNotRepo(path string) error { return &notRepoErr{path} }

type notRepoErr struct{ path string }

func (e *notRepoErr) Error() string { return e.path + " is not inside a git repository" }

// installFakeSweep wires every worktree-sweep seam at the fake. The two
// inspect/remove seams are shared with the per-agent cleanup path, so
// they go through SetWorktreeFnsForTest + SetRetireWorktreeFnForTest; the
// repo-wide ones go through SetSweepWorktreeFnsForTest.
func installFakeSweep(t *testing.T, f *fakeSweep) {
	t.Helper()
	t.Cleanup(agentd.SetWorktreeFnsForTest(f.inspect, f.remove))
	t.Cleanup(agentd.SetRetireWorktreeFnForTest(f.removeBranch))
	t.Cleanup(agentd.SetSweepWorktreeFnsForTest(f.list, f.repoRoot, f.dirtyFn, f.main, f.prune))
}

// repoFixture builds the canonical one-repo / five-worktree fixture and
// the group + members that pin the live/offline agents into it.
func repoFixture(t *testing.T, f *testharness.Flow) *fakeSweep {
	t.Helper()
	const (
		repo    = "/repo"
		wtLive  = "/repo-wt-live"
		wtAgt   = "/repo-wt-agent"
		wtOrph  = "/repo-wt-orphan"
		wtDrt   = "/repo-wt-dirty"
		wtRetC  = "/repo-wt-retired"
		wtRetD  = "/repo-wt-retired-dirty"
		wtMix   = "/repo-wt-mixed"
		live    = "wliv-1111-2222-3333-4444"
		agt     = "wagt-1111-2222-3333-4444"
		retCln  = "wrtc-1111-2222-3333-4444"
		retDrty = "wrtd-1111-2222-3333-4444"
		mixRet  = "wmxr-1111-2222-3333-4444"
		mixAct  = "wmxa-1111-2222-3333-4444"
	)
	f.HaveConvWithTitle(live, "live-worker")
	f.HaveConvWithTitle(agt, "offline-worker")
	f.HaveConvWithTitle(retCln, "retired-clean")
	f.HaveConvWithTitle(retDrty, "retired-dirty")
	f.HaveConvWithTitle(mixRet, "mixed-retired")
	f.HaveConvWithTitle(mixAct, "mixed-active")
	f.HaveAliveSession(live, "spwn-live", "tmux-live", wtLive)
	f.HaveAliveSession(agt, "spwn-agt", "tmux-agt", wtAgt)
	f.HaveAliveSession(retCln, "spwn-retc", "tmux-retc", wtRetC)
	f.HaveAliveSession(retDrty, "spwn-retd", "tmux-retd", wtRetD)
	// Two agents share the mixed worktree (same cwd → same root).
	f.HaveAliveSession(mixRet, "spwn-mxr", "tmux-mxr", wtMix)
	f.HaveAliveSession(mixAct, "spwn-mxa", "tmux-mxa", wtMix)
	f.MarkOffline("tmux-agt")
	f.MarkOffline("tmux-retc")
	f.MarkOffline("tmux-retd")
	f.MarkOffline("tmux-mxr")
	f.MarkOffline("tmux-mxa")
	f.HaveGroup("squad")
	f.HaveMember("squad", live)
	f.HaveMember("squad", agt)
	// The retired agents are NOT group members (retire unjoins every group)
	// — their worktrees still surface because `git worktree list` is
	// repo-global and their session rows still pin a cwd. HaveRetiredAgent
	// flips the enrollment to retired, the signal the classifier keys on.
	f.HaveRetiredAgent(retCln)
	f.HaveRetiredAgent(retDrty)
	// The mixed worktree binds one retired + one still-active agent. The
	// "all retired" gate must keep it in the protected "agent" bucket, so an
	// "any retired" regression would be caught here.
	f.HaveRetiredAgent(mixRet)
	f.HaveEnrolledAgent(mixAct)
	_, err := db.SetAgentGroupDefaultCwd("squad", repo)
	require.NoError(t, err, "set group default cwd")

	fs := &fakeSweep{
		worktrees: []worktree.WorktreeInfo{
			{Path: repo, Branch: "main", IsMain: true},
			{Path: wtLive, Branch: "live"},
			{Path: wtAgt, Branch: "agent"},
			{Path: wtOrph, Branch: "orphan"},
			{Path: wtDrt, Branch: "dirty"},
			{Path: wtRetC, Branch: "retired"},
			{Path: wtRetD, Branch: "retired-dirty"},
			{Path: wtMix, Branch: "mixed"},
		},
		roots: map[string]string{
			repo:   repo, // default_cwd resolves to the main repo root
			wtLive: wtLive,
			wtAgt:  wtAgt,
		},
		statuses: map[string]worktree.WorktreeStatus{
			repo:   {Root: repo, Branch: "main", Kind: "main"},
			wtLive: {Root: wtLive, Branch: "live", Kind: "linked"},
			wtAgt:  {Root: wtAgt, Branch: "agent", Kind: "linked"},
			wtOrph: {Root: wtOrph, Branch: "orphan", Kind: "linked"},
			wtDrt:  {Root: wtDrt, Branch: "dirty", Kind: "linked"},
			wtRetC: {Root: wtRetC, Branch: "retired", Kind: "linked"},
			wtRetD: {Root: wtRetD, Branch: "retired-dirty", Kind: "linked"},
			wtMix:  {Root: wtMix, Branch: "mixed", Kind: "linked"},
		},
		dirty:    map[string]bool{wtDrt: true, wtRetD: true},
		mainRepo: repo,
	}
	installFakeSweep(t, fs)
	return fs
}

func discoverWorktrees(t *testing.T, mux http.Handler, group string) sweepDiscoverWire {
	t.Helper()
	r, err := http.NewRequest(http.MethodGet, "/api/groups/"+group+"/worktrees", nil)
	require.NoError(t, err)
	rec := testharness.Serve(mux, r)
	require.Equal(t, http.StatusOK, rec.Code, "GET worktrees body=%s", rec.Body.String())
	var out sweepDiscoverWire
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	return out
}

func byPath(wts []sweepWorktreeWire) map[string]sweepWorktreeWire {
	m := map[string]sweepWorktreeWire{}
	for _, wt := range wts {
		m[wt.Path] = wt
	}
	return m
}

// Scenario: discovery classifies every worktree of the group's repo and
// pre-ticks only the clean orphan.
func TestWorktreeSweep_DiscoverClassifies(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)
	repoFixture(t, f)

	mux := agentd.BuildDashboardHandlerForTest()
	out := discoverWorktrees(t, mux, "squad")

	require.Len(t, out.Worktrees, 8, "all eight worktrees discovered")
	m := byPath(out.Worktrees)

	assert.Equal(t, "main", m["/repo"].Category)
	assert.True(t, m["/repo"].IsMain, "main worktree flagged is_main")
	assert.False(t, m["/repo"].Checked, "main never pre-ticked")

	assert.Equal(t, "live", m["/repo-wt-live"].Category, "online agent's worktree is live")
	assert.False(t, m["/repo-wt-live"].Checked, "live worktree not pre-ticked")
	require.Len(t, m["/repo-wt-live"].Agents, 1)
	assert.True(t, m["/repo-wt-live"].Agents[0].Online)

	assert.Equal(t, "agent", m["/repo-wt-agent"].Category, "offline enrolled agent's worktree is agent-bound")
	assert.False(t, m["/repo-wt-agent"].Checked, "resume-bound worktree not pre-ticked")
	require.Len(t, m["/repo-wt-agent"].Agents, 1)
	assert.False(t, m["/repo-wt-agent"].Agents[0].Online)
	assert.False(t, m["/repo-wt-agent"].Agents[0].Retired, "a still-enrolled agent is not retired")

	assert.Equal(t, "orphan", m["/repo-wt-orphan"].Category)
	assert.True(t, m["/repo-wt-orphan"].Checked, "clean orphan IS pre-ticked")
	assert.False(t, m["/repo-wt-orphan"].Dirty)

	assert.Equal(t, "orphan", m["/repo-wt-dirty"].Category)
	assert.True(t, m["/repo-wt-dirty"].Dirty)
	assert.False(t, m["/repo-wt-dirty"].Checked, "dirty orphan left for review, not pre-ticked")

	// A clean worktree whose only bound agent is retired is its own
	// category and IS pre-ticked — the janitor's prime cleanup target.
	assert.Equal(t, "retired", m["/repo-wt-retired"].Category, "retired agent's worktree is its own category")
	assert.True(t, m["/repo-wt-retired"].Checked, "clean retired-agent worktree IS pre-ticked")
	assert.False(t, m["/repo-wt-retired"].Dirty)
	require.Len(t, m["/repo-wt-retired"].Agents, 1)
	assert.False(t, m["/repo-wt-retired"].Agents[0].Online)
	assert.True(t, m["/repo-wt-retired"].Agents[0].Retired, "the bound agent is flagged retired")

	// ...but a dirty one is held back for review, exactly like a dirty orphan.
	assert.Equal(t, "retired", m["/repo-wt-retired-dirty"].Category)
	assert.True(t, m["/repo-wt-retired-dirty"].Dirty)
	assert.False(t, m["/repo-wt-retired-dirty"].Checked, "dirty retired worktree left for review, not pre-ticked")

	// The mixed worktree binds a retired AND a still-active agent. The "all
	// retired" gate must keep it in the protected "agent" bucket — an "any
	// retired" regression would mis-tick it. (Proves the gate's defensive core.)
	mix := m["/repo-wt-mixed"]
	assert.Equal(t, "agent", mix.Category, "a still-active bound agent keeps the worktree protected")
	assert.False(t, mix.Checked, "mixed worktree must not be pre-ticked")
	require.Len(t, mix.Agents, 2, "both bound agents are reported")
	var retired, active int
	for _, a := range mix.Agents {
		if a.Retired {
			retired++
		} else {
			active++
		}
	}
	assert.Equal(t, 1, retired, "exactly one bound agent is retired")
	assert.Equal(t, 1, active, "exactly one bound agent is still active")
}

// Scenario: the explicit-path cleanup removes the picked orphans (with
// their branches), skips the main repo and the live-agent worktree, and
// prunes the repo afterwards.
func TestWorktreeSweep_CleanupRemovesAndProtects(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)
	fs := repoFixture(t, f)

	mux := agentd.BuildDashboardHandlerForTest()
	body := `{"paths":["/repo-wt-orphan","/repo-wt-dirty","/repo-wt-live","/repo"],"delete_branches":true}`
	r, err := http.NewRequest(http.MethodPost, "/api/worktrees/cleanup", strings.NewReader(body))
	require.NoError(t, err)
	r.Header.Set("Content-Type", "application/json")
	rec := testharness.Serve(mux, r)
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	var out sweepCleanupWire
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))

	assert.Equal(t, 2, out.Removed, "two orphans removed")
	assert.Equal(t, 2, out.Branches, "both orphan branches deleted")
	assert.Equal(t, 2, out.Skipped, "main + live worktree skipped")
	assert.Equal(t, 0, out.Failed)

	assert.True(t, fs.wasRemoved("/repo-wt-orphan"))
	assert.True(t, fs.wasRemoved("/repo-wt-dirty"))
	assert.False(t, fs.wasRemoved("/repo-wt-live"), "live-agent worktree must be kept")
	assert.False(t, fs.wasRemoved("/repo"), "main repo must be kept")
	assert.True(t, fs.wasPruned("/repo"), "the repo is pruned after the sweep")

	// Per-path reasons surfaced to the modal.
	reasons := map[string]string{}
	for _, o := range out.Outcomes {
		reasons[o.Path] = o.Result
	}
	assert.Equal(t, "removed_with_branch", reasons["/repo-wt-orphan"])
	assert.Equal(t, "skipped", reasons["/repo-wt-live"])
	assert.Equal(t, "skipped", reasons["/repo"])
}

// Scenario: with delete_branches off, only the worktree directory goes —
// the branch is kept.
func TestWorktreeSweep_CleanupKeepsBranchWhenOff(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)
	fs := repoFixture(t, f)

	mux := agentd.BuildDashboardHandlerForTest()
	body := `{"paths":["/repo-wt-orphan"],"delete_branches":false}`
	r, err := http.NewRequest(http.MethodPost, "/api/worktrees/cleanup", strings.NewReader(body))
	require.NoError(t, err)
	r.Header.Set("Content-Type", "application/json")
	rec := testharness.Serve(mux, r)
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	var out sweepCleanupWire
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	assert.Equal(t, 1, out.Removed)
	assert.Equal(t, 0, out.Branches, "branch kept when delete_branches is off")
	assert.True(t, fs.wasRemoved("/repo-wt-orphan"))
}
