package agentd_test

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/worktree"
)

// initRepoOnMain creates a real git repo with one empty commit on a
// branch named `main`, inside a fresh parent temp dir, and returns
// both the (symlink-resolved) repo path and that parent. Worktrees
// `git worktree add` cuts default to `../<repo>-<branch>`, so anchoring
// the repo one level under a temp dir keeps every worktree sibling
// inside the t.TempDir() tree and thus auto-cleaned.
func initRepoOnMain(t *testing.T) (repo, parent string) {
	t.Helper()
	parent, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err, "EvalSymlinks tempdir")
	repo = filepath.Join(parent, "repo")
	require.NoError(t, os.MkdirAll(repo, 0o755), "mkdir repo")
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		if out, gerr := cmd.CombinedOutput(); gerr != nil {
			t.Fatalf("git %v: %v\n%s", args, gerr, out)
		}
	}
	run("init", "-q")
	run("config", "user.email", "test@example.com")
	run("config", "user.name", "tclaude tests")
	run("config", "commit.gpgsign", "false")
	run("commit", "-q", "--allow-empty", "-m", "init")
	// -M renames whatever the init default branch was (master / main)
	// to main, so worktree.DefaultBranchIn resolves deterministically.
	run("branch", "-M", "main")
	return repo, parent
}

// initEmptyRepoOnMain creates a real git repo with NO commits — an
// unborn HEAD on `main` — inside a fresh parent temp dir. This is the
// brand-new-repo case where `git worktree add … <base>` can't work
// (there's no commit to base on), so a worktree must be cut as an
// orphan branch. Same parent-anchoring as initRepoOnMain.
func initEmptyRepoOnMain(t *testing.T) (repo, parent string) {
	t.Helper()
	parent, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err, "EvalSymlinks tempdir")
	repo = filepath.Join(parent, "repo")
	require.NoError(t, os.MkdirAll(repo, 0o755), "mkdir repo")
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		if out, gerr := cmd.CombinedOutput(); gerr != nil {
			t.Fatalf("git %v: %v\n%s", args, gerr, out)
		}
	}
	run("init", "-q", "-b", "main")
	run("config", "user.email", "test@example.com")
	run("config", "user.name", "tclaude tests")
	run("config", "commit.gpgsign", "false")
	return repo, parent
}

// Scenario: a human spawns `--worktree feat-x` into a BRAND-NEW repo —
// `git init` done, but no commits yet (unborn HEAD). `git worktree add
// … <base>` can't work (nothing to base on); the spawn must still land
// the agent in its own worktree, cut as an orphan branch. This is the
// regression for the dashboard's empty-base-branch / "could not
// determine base branch" failure on a fresh repo.
func TestSpawnCLI_WorktreeInEmptyRepoCutsOrphan(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")
	bridgeAgentClientToMux(t, f.Mux)

	repo, parent := initEmptyRepoOnMain(t)

	stderr := new(bytes.Buffer)
	resp, rc := agent.RunSpawn(
		&agent.SpawnParams{Group: "alpha", Name: "worker", Cwd: repo, Worktree: "feat-x"},
		new(bytes.Buffer), stderr, new(bytes.Buffer),
	)
	require.Equalf(t, 0, rc, "RunSpawn rc, stderr=%s", stderr.String())
	require.NotNil(t, resp, "RunSpawn resp")

	// The orphan worktree git would have created: ../<repo-base>-<branch>.
	wantWorktree := filepath.Join(parent, "repo-feat-x")
	info, statErr := os.Stat(wantWorktree)
	require.NoErrorf(t, statErr, "orphan worktree dir should exist at %s", wantWorktree)
	assert.True(t, info.IsDir(), "worktree path should be a directory")

	// It's a real linked worktree of the repo, on branch feat-x.
	wts, err := worktree.ListWorktreesIn(repo)
	require.NoError(t, err, "ListWorktreesIn")
	var found *worktree.WorktreeInfo
	for i := range wts {
		if wts[i].Branch == "feat-x" {
			found = &wts[i]
			break
		}
	}
	require.NotNil(t, found, "repo should have a worktree on branch feat-x; got %+v", wts)

	// The agent launched IN the orphan worktree — the SessionRow records
	// it as the new agent's cwd.
	rows, err := db.FindSessionsByConvID(resp.ConvID)
	require.NoError(t, err, "FindSessionsByConvID")
	require.NotEmpty(t, rows, "no session row for conv %s", resp.ConvID)
	assert.Equal(t, resolveSym(t, wantWorktree), resolveSym(t, rows[0].Cwd),
		"spawned agent's cwd should be the orphan worktree, not the repo")
}

// Scenario: a human runs `tclaude agent spawn alpha worker --worktree
// feat-x` from a git repo. The CLI must create a git worktree on
// branch feat-x and spawn the new agent INTO it — the CLI equivalent
// of the dashboard spawn modal's worktree picker.
//
// Real surfaces: the worktree exists on disk on the right branch, and
// the SessionRow the spawn produced records the worktree as the new
// agent's cwd (so the agent genuinely launched there).
func TestSpawnCLI_WorktreeCreatesAndLaunchesInIt(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")
	bridgeAgentClientToMux(t, f.Mux)

	repo, parent := initRepoOnMain(t)

	stderr := new(bytes.Buffer)
	resp, rc := agent.RunSpawn(
		&agent.SpawnParams{Group: "alpha", Name: "worker", Cwd: repo, Worktree: "feat-x"},
		new(bytes.Buffer), stderr, new(bytes.Buffer),
	)
	require.Equalf(t, 0, rc, "RunSpawn rc, stderr=%s", stderr.String())
	require.NotNil(t, resp, "RunSpawn resp")

	// The worktree git would have created: ../<repo-base>-<branch>.
	wantWorktree := filepath.Join(parent, "repo-feat-x")
	info, statErr := os.Stat(wantWorktree)
	require.NoErrorf(t, statErr, "worktree dir should exist at %s", wantWorktree)
	assert.True(t, info.IsDir(), "worktree path should be a directory")

	// It must be a real linked worktree of the repo, on branch feat-x.
	wts, err := worktree.ListWorktreesIn(repo)
	require.NoError(t, err, "ListWorktreesIn")
	var found *worktree.WorktreeInfo
	for i := range wts {
		if wts[i].Branch == "feat-x" {
			found = &wts[i]
			break
		}
	}
	require.NotNil(t, found, "repo should have a worktree on branch feat-x; got %+v", wts)
	assert.Equal(t, resolveSym(t, wantWorktree), resolveSym(t, found.Path),
		"worktree on feat-x should sit at the default sibling path")

	// The agent launched IN the worktree — the SessionRow the spawn
	// produced records the worktree as its cwd.
	rows, err := db.FindSessionsByConvID(resp.ConvID)
	require.NoError(t, err, "FindSessionsByConvID")
	require.NotEmpty(t, rows, "no session row for conv %s", resp.ConvID)
	assert.Equal(t, resolveSym(t, wantWorktree), resolveSym(t, rows[0].Cwd),
		"spawned agent's cwd should be the worktree, not the repo")
}

// Scenario: a human spawns an agent whose launch dir is a "virtual
// monorepo" (a plain folder, not a git repo) while the code work
// belongs in a git worktree of a nested sub-repo. `--worktree-repo`
// points the worktree at the sub-repo; `--cwd` stays the monorepo.
//
// The CLI must create the worktree in the sub-repo, keep the agent's
// cwd at the monorepo, and ride the worktree path/branch along so the
// daemon's welcome message tells the agent where to edit code. This is
// CLI parity with the dashboard's separate "CWD" vs "Worktree repo"
// fields.
func TestSpawnCLI_WorktreeRepoMonorepoRidesAlong(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")
	bridgeAgentClientToMux(t, f.Mux)

	// monorepo: a plain dir holding a nested git sub-repo.
	monorepo, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err, "EvalSymlinks monorepo")
	subrepo := filepath.Join(monorepo, "svc")
	require.NoError(t, os.MkdirAll(subrepo, 0o755), "mkdir subrepo")
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = subrepo
		if out, gerr := cmd.CombinedOutput(); gerr != nil {
			t.Fatalf("git %v: %v\n%s", args, gerr, out)
		}
	}
	run("init", "-q")
	run("config", "user.email", "test@example.com")
	run("config", "user.name", "tclaude tests")
	run("config", "commit.gpgsign", "false")
	run("commit", "-q", "--allow-empty", "-m", "init")
	run("branch", "-M", "main")

	stderr := new(bytes.Buffer)
	resp, rc := agent.RunSpawn(
		&agent.SpawnParams{
			Group: "alpha", Name: "worker",
			Cwd:          monorepo,
			Worktree:     "feat-y",
			WorktreeRepo: subrepo,
		},
		new(bytes.Buffer), stderr, new(bytes.Buffer),
	)
	require.Equalf(t, 0, rc, "RunSpawn rc, stderr=%s", stderr.String())
	require.NotNil(t, resp, "RunSpawn resp")

	// The worktree was created in the sub-repo (sibling of svc).
	wantWorktree := filepath.Join(monorepo, "svc-feat-y")
	info, statErr := os.Stat(wantWorktree)
	require.NoErrorf(t, statErr, "worktree dir should exist at %s", wantWorktree)
	assert.True(t, info.IsDir(), "worktree path should be a directory")

	// The agent launched in the MONOREPO, not the worktree — that's the
	// whole point of the sub-repo-worktree flow.
	rows, err := db.FindSessionsByConvID(resp.ConvID)
	require.NoError(t, err, "FindSessionsByConvID")
	require.NotEmpty(t, rows, "no session row for conv %s", resp.ConvID)
	assert.Equal(t, resolveSym(t, monorepo), resolveSym(t, rows[0].Cwd),
		"agent should launch in the monorepo (cwd), not the worktree")

	// The welcome injected into the new pane names the worktree path +
	// branch so the agent edits code in the right place.
	target := resp.TmuxSession + ":0.0"
	f.AssertSentContains(target, wantWorktree, 5*time.Second)
	f.AssertSentContains(target, "feat-y", 5*time.Second)
}

// Scenario: a human runs `tclaude agent spawn alpha worker
// --no-group-context -m "<brief>"` for a group that carries a shared
// startup context. The flag must opt the new agent OUT of the group
// context while still delivering the per-spawn task brief. A second
// spawn without the flag proves the context IS delivered by default —
// so the test pins the flag's effect, not just an absence.
func TestSpawnCLI_NoGroupContextOptsOut(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")
	bridgeAgentClientToMux(t, f.Mux)
	chdirTo(t, resolveSym(t, t.TempDir()))

	const groupContext = "SECRET-PHOENIX-CONTEXT-MARKER"
	const brief = "audit the auth comparisons"
	if _, err := db.SetAgentGroupDefaultContext("alpha", groupContext); err != nil {
		t.Fatalf("SetAgentGroupDefaultContext: %v", err)
	}

	// With --no-group-context: the brief is delivered, the group
	// context is not.
	stderr := new(bytes.Buffer)
	optedOut, rc := agent.RunSpawn(
		&agent.SpawnParams{Group: "alpha", Name: "worker", InitialMessage: brief, NoGroupContext: true},
		new(bytes.Buffer), stderr, new(bytes.Buffer),
	)
	require.Equalf(t, 0, rc, "RunSpawn (opted out) rc, stderr=%s", stderr.String())
	require.NotNil(t, optedOut, "RunSpawn (opted out) resp")

	rows, err := db.ListAgentMessagesForConv(optedOut.ConvID, 100)
	require.NoError(t, err, "ListAgentMessagesForConv (opted out)")
	require.Len(t, rows, 1, "opted-out spawn with a brief still gets one inbox message")
	assert.Contains(t, rows[0].Body, brief, "the per-spawn brief must still be delivered")
	assert.NotContains(t, rows[0].Body, groupContext,
		"--no-group-context must keep the group context out of the briefing")

	// Without the flag: the same group context IS folded in — proof the
	// flag is what made the difference above.
	included, rc := agent.RunSpawn(
		&agent.SpawnParams{Group: "alpha", Name: "worker2", InitialMessage: brief},
		new(bytes.Buffer), new(bytes.Buffer), new(bytes.Buffer),
	)
	require.Equal(t, 0, rc, "RunSpawn (default) rc")
	require.NotNil(t, included, "RunSpawn (default) resp")

	rows, err = db.ListAgentMessagesForConv(included.ConvID, 100)
	require.NoError(t, err, "ListAgentMessagesForConv (default)")
	require.Len(t, rows, 1, "default spawn gets one inbox message")
	assert.Contains(t, rows[0].Body, groupContext,
		"by default the group context is delivered (parity with every other spawn path)")
}

// Scenario: --worktree-base and --worktree-repo are modifiers of
// --worktree; passing either without --worktree is a usage error
// caught before any daemon call, not a silently-ignored flag.
func TestSpawnCLI_WorktreeModifiersRequireWorktree(t *testing.T) {
	for _, tc := range []struct {
		name   string
		params agent.SpawnParams
		want   string
	}{
		{"base-without-worktree",
			agent.SpawnParams{Group: "alpha", WorktreeBase: "main"},
			"--worktree-base requires --worktree"},
		{"repo-without-worktree",
			agent.SpawnParams{Group: "alpha", WorktreeRepo: "/some/repo"},
			"--worktree-repo requires --worktree"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stderr := new(bytes.Buffer)
			p := tc.params
			resp, rc := agent.RunSpawn(&p, new(bytes.Buffer), stderr, new(bytes.Buffer))
			assert.Nil(t, resp, "no spawn response on a usage error")
			assert.Equal(t, 3, rc, "rcInvalidArg expected")
			assert.Contains(t, stderr.String(), tc.want, "error should name the missing flag")
		})
	}
}

// Scenario: a `tclaude agent spawn --worktree` whose spawn request is
// then rejected by the daemon (here: the target group is already at
// its member cap) must NOT leak the git worktree the CLI created up
// front. RunSpawn tears the freshly-created worktree back down — but
// keeps the branch, so a retry reuses it rather than tripping over a
// half-cleaned state. This pins the failure-path cleanup in RunSpawn.
func TestSpawnCLI_WorktreeTornDownWhenSpawnRejected(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")
	// Fill alpha to its cap: a spawn into it is now refused 409
	// group_full — a guardrail that binds the human caller too, so it
	// fires on the bridged (human-peer) CLI path.
	const incumbent = "exis-aaaa-bbbb-cccc-111111111111"
	f.HaveMember("alpha", incumbent)
	_, err := db.SetAgentGroupMaxMembers("alpha", 1)
	require.NoError(t, err, "SetAgentGroupMaxMembers")
	bridgeAgentClientToMux(t, f.Mux)

	repo, parent := initRepoOnMain(t)

	stderr := new(bytes.Buffer)
	resp, rc := agent.RunSpawn(
		&agent.SpawnParams{Group: "alpha", Name: "worker", Cwd: repo, Worktree: "feat-x"},
		new(bytes.Buffer), stderr, new(bytes.Buffer),
	)
	// The spawn was rejected — no response, a non-zero rc. The daemon
	// returned 409 group_full (the bridge surfaces the status; the
	// human-readable body isn't decoded in the test transport).
	require.Nilf(t, resp, "a rejected spawn returns no response; stderr=%s", stderr.String())
	require.NotEqual(t, 0, rc, "a rejected spawn returns a non-zero rc")
	assert.Contains(t, stderr.String(), "409", "the rejection status is surfaced")
	assert.Contains(t, stderr.String(), "removed the worktree",
		"RunSpawn should report it cleaned up the orphaned worktree")

	// The worktree directory is gone — RunSpawn tore it back down — and
	// the repo no longer registers a worktree on feat-x.
	orphan := filepath.Join(parent, "repo-feat-x")
	_, statErr := os.Stat(orphan)
	assert.Truef(t, os.IsNotExist(statErr),
		"orphaned worktree dir should be removed; stat err=%v", statErr)
	wts, err := worktree.ListWorktreesIn(repo)
	require.NoError(t, err, "ListWorktreesIn")
	for _, wt := range wts {
		assert.NotEqualf(t, "feat-x", wt.Branch,
			"repo should no longer have a worktree on feat-x; got %+v", wts)
	}

	// ...but the branch survives, so a retry reuses it rather than
	// erroring on a half-cleaned state.
	assert.Containsf(t, worktree.BranchesIn(repo), "feat-x",
		"the feat-x branch must be kept; branches=%v", worktree.BranchesIn(repo))
}
