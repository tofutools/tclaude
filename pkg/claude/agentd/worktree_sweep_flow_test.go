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
// The fake models one repo "/repo" with five worktrees: the main repo, a
// live-agent worktree, an offline-agent worktree, a clean orphan and a
// dirty orphan — one of each classification the modal renders.

// --- wire shapes (mirror the unexported handler responses) -----------

type sweepAgentWire struct {
	ConvID string `json:"conv_id"`
	Title  string `json:"title"`
	Online bool   `json:"online"`
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
		repo   = "/repo"
		wtLive = "/repo-wt-live"
		wtAgt  = "/repo-wt-agent"
		wtOrph = "/repo-wt-orphan"
		wtDrt  = "/repo-wt-dirty"
		live   = "wliv-1111-2222-3333-4444"
		agt    = "wagt-1111-2222-3333-4444"
	)
	f.HaveConvWithTitle(live, "live-worker")
	f.HaveConvWithTitle(agt, "offline-worker")
	f.HaveAliveSession(live, "spwn-live", "tmux-live", wtLive)
	f.HaveAliveSession(agt, "spwn-agt", "tmux-agt", wtAgt)
	f.MarkOffline("tmux-agt")
	f.HaveGroup("squad")
	f.HaveMember("squad", live)
	f.HaveMember("squad", agt)
	_, err := db.SetAgentGroupDefaultCwd("squad", repo)
	require.NoError(t, err, "set group default cwd")

	fs := &fakeSweep{
		worktrees: []worktree.WorktreeInfo{
			{Path: repo, Branch: "main", IsMain: true},
			{Path: wtLive, Branch: "live"},
			{Path: wtAgt, Branch: "agent"},
			{Path: wtOrph, Branch: "orphan"},
			{Path: wtDrt, Branch: "dirty"},
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
		},
		dirty:    map[string]bool{wtDrt: true},
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

	require.Len(t, out.Worktrees, 5, "all five worktrees discovered")
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

	assert.Equal(t, "orphan", m["/repo-wt-orphan"].Category)
	assert.True(t, m["/repo-wt-orphan"].Checked, "clean orphan IS pre-ticked")
	assert.False(t, m["/repo-wt-orphan"].Dirty)

	assert.Equal(t, "orphan", m["/repo-wt-dirty"].Category)
	assert.True(t, m["/repo-wt-dirty"].Dirty)
	assert.False(t, m["/repo-wt-dirty"].Checked, "dirty orphan left for review, not pre-ticked")
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
