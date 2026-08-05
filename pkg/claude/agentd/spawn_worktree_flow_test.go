package agentd_test

import (
	"bytes"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/claude/worktree"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// Scenario: a SANDBOXED agent asks the daemon to prepare the worktree
// behind `--worktree feat-x`. The git commands run in the daemon, not in
// the caller — which is the whole point: the caller's own filesystem
// restrictions have nothing to do with a checkout it never touches. What
// stands in their place is the dir write-proof: before creating
// anything, the daemon challenges the caller over the repo and the
// directory the worktree will land in, so no agent gets a checkout
// somewhere it could not have put one itself.
func TestSpawnWorktreePrepare_AgentProvesRepoAndParentThenDaemonCreates(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")
	const parentConv = "parent-wtpr-aaaa-bbbb-cccc-111111111111"
	haveSpawnCapableSandboxParent(t, f, "alpha", parentConv, harness.DefaultName, harness.ClaudeSandboxInherit)

	repo, repoParent := initRepoOnMain(t)
	body := map[string]any{"repo": repo, "branch": "feat-x", "base": "main"}

	ch := decodeWriteProofChallenge(t,
		agentReq(t, f, parentConv, http.MethodPost, "/v1/worktrees/prepare", body))
	assert.ElementsMatch(t, []string{repo, repoParent}, ch.WriteProof.Dirs,
		"creating a worktree must be proved against the repo and the dir it lands in")

	answerChallenge(t, ch)
	body["write_proof_token"] = ch.WriteProof.Token
	rec := agentReq(t, f, parentConv, http.MethodPost, "/v1/worktrees/prepare", body)
	require.Equalf(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	var resp agent.WorktreePrepareResponse
	testharness.DecodeJSON(t, rec, &resp)
	assert.True(t, resp.Created, "a fresh worktree was cut")
	assert.NotEmpty(t, resp.DiscardToken, "a created worktree is addressable for teardown")

	wantPath := filepath.Join(repoParent, "repo-feat-x")
	assert.Equal(t, wantPath, resp.Path, "default sibling worktree path")
	info, statErr := os.Stat(wantPath)
	require.NoErrorf(t, statErr, "the daemon should have created %s", wantPath)
	assert.True(t, info.IsDir(), "worktree path should be a directory")

	// A second prepare for the same branch reuses it — no proof needed,
	// nothing created.
	rec = agentReq(t, f, parentConv, http.MethodPost, "/v1/worktrees/prepare",
		map[string]any{"repo": repo, "branch": "feat-x"})
	require.Equalf(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	var reuse agent.WorktreePrepareResponse
	testharness.DecodeJSON(t, rec, &reuse)
	assert.False(t, reuse.Created, "an existing worktree on the branch is reused")
	assert.Equal(t, resp.Path, reuse.Path, "reuse returns the same path")
	assert.Empty(t, reuse.DiscardToken,
		"a reused worktree is not this caller's to discard — no token")

	// The discard token from the create call takes it back down, branch
	// and all — the daemon cut that branch for this worktree.
	rec = agentReq(t, f, parentConv, http.MethodPost, "/v1/worktrees/discard",
		map[string]any{"token": resp.DiscardToken})
	require.Equalf(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	var discarded agent.WorktreeDiscardResponse
	testharness.DecodeJSON(t, rec, &discarded)
	assert.True(t, discarded.Removed, "the worktree directory is removed")
	assert.True(t, discarded.BranchDeleted, "the branch cut for it goes with it")
	_, statErr = os.Stat(wantPath)
	assert.Truef(t, os.IsNotExist(statErr), "worktree dir should be gone; stat err=%v", statErr)
	assert.NotContains(t, worktree.BranchesIn(repo), "feat-x", "branch should be gone")

	// The token is single-use: a second discard finds nothing.
	rec = agentReq(t, f, parentConv, http.MethodPost, "/v1/worktrees/discard",
		map[string]any{"token": resp.DiscardToken})
	assert.Equal(t, http.StatusNotFound, rec.Code, "a spent discard token is not re-usable")
}

// Scenario: the negative half of the same guarantee — an agent that
// does NOT answer the challenge gets nothing. This is the assertion that
// catches a future refactor dropping the proof gate: the caller retries
// with a token but without creating the proof files, and the daemon must
// refuse and leave the repo untouched (no worktree, no branch).
func TestSpawnWorktreePrepare_UnprovedAgentCreatesNothing(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")
	const parentConv = "parent-wtnp-aaaa-bbbb-cccc-111111111111"
	haveSpawnCapableSandboxParent(t, f, "alpha", parentConv, harness.DefaultName, harness.ClaudeSandboxInherit)

	repo, repoParent := initRepoOnMain(t)
	body := map[string]any{"repo": repo, "branch": "feat-x", "base": "main"}
	ch := decodeWriteProofChallenge(t,
		agentReq(t, f, parentConv, http.MethodPost, "/v1/worktrees/prepare", body))

	// Retry with the token but WITHOUT writing the proof files — the
	// shape of an agent whose sandbox cannot write those directories.
	body["write_proof_token"] = ch.WriteProof.Token
	rec := agentReq(t, f, parentConv, http.MethodPost, "/v1/worktrees/prepare", body)
	require.Equalf(t, http.StatusForbidden, rec.Code, "body=%s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "write-permission proof file",
		"the refusal should name what is missing")

	_, statErr := os.Stat(filepath.Join(repoParent, "repo-feat-x"))
	assert.Truef(t, os.IsNotExist(statErr), "nothing should have been created; stat err=%v", statErr)
	assert.NotContains(t, worktree.BranchesIn(repo), "feat-x", "no branch should have been cut")
}

// Scenario: the worktree was cut on a branch that already existed. The
// discard removes the directory it created and leaves the branch alone —
// deleting it would destroy work the caller never asked this endpoint to
// create.
func TestSpawnWorktreeDiscard_KeepsAPreExistingBranch(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")
	const caller = "owner-wtkb-aaaa-bbbb-cccc-111111111111"
	haveSpawnCapableSandboxParent(t, f, "alpha", caller, harness.DefaultName, harness.ClaudeSandboxOff)

	repo, repoParent := initRepoOnMain(t)
	gitInRepo(t, repo, "branch", "feat-x")

	rec := agentReq(t, f, caller, http.MethodPost, "/v1/worktrees/prepare",
		map[string]any{"repo": repo, "branch": "feat-x"})
	require.Equalf(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	var prepared agent.WorktreePrepareResponse
	testharness.DecodeJSON(t, rec, &prepared)
	require.True(t, prepared.Created, "an existing branch with no worktree still needs one cut")

	rec = agentReq(t, f, caller, http.MethodPost, "/v1/worktrees/discard",
		map[string]any{"token": prepared.DiscardToken})
	require.Equalf(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	var discarded agent.WorktreeDiscardResponse
	testharness.DecodeJSON(t, rec, &discarded)
	assert.True(t, discarded.Removed, "the worktree directory is removed")
	assert.False(t, discarded.BranchDeleted, "a branch we did not cut is not ours to delete")
	assert.Contains(t, worktree.BranchesIn(repo), "feat-x", "the pre-existing branch survives")
	_, statErr := os.Stat(filepath.Join(repoParent, "repo-feat-x"))
	assert.Truef(t, os.IsNotExist(statErr), "worktree dir should be gone; stat err=%v", statErr)
}

// gitInRepo runs a git command in repo and fails the test on error.
func gitInRepo(t *testing.T, repo string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = repo
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// Scenario: one agent may not discard a worktree another agent prepared.
// The token is the only address the endpoint accepts, and it is bound to
// the caller that minted it — a removal surface must not be steerable by
// a bystander who guesses (or is handed) someone else's token.
func TestSpawnWorktreeDiscard_RefusesAnotherAgentsToken(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")
	const owner = "owner-wtdc-aaaa-bbbb-cccc-111111111111"
	const bystander = "other-wtdc-aaaa-bbbb-cccc-222222222222"
	haveSpawnCapableSandboxParent(t, f, "alpha", owner, harness.DefaultName, harness.ClaudeSandboxOff)
	haveSpawnCapableSandboxParent(t, f, "alpha", bystander, harness.DefaultName, harness.ClaudeSandboxOff)

	repo, repoParent := initRepoOnMain(t)
	rec := agentReq(t, f, owner, http.MethodPost, "/v1/worktrees/prepare",
		map[string]any{"repo": repo, "branch": "feat-x"})
	require.Equalf(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	var prepared agent.WorktreePrepareResponse
	testharness.DecodeJSON(t, rec, &prepared)
	require.True(t, prepared.Created)

	rec = agentReq(t, f, bystander, http.MethodPost, "/v1/worktrees/discard",
		map[string]any{"token": prepared.DiscardToken})
	assert.Equal(t, http.StatusNotFound, rec.Code, "a foreign token addresses nothing")
	_, statErr := os.Stat(filepath.Join(repoParent, "repo-feat-x"))
	assert.NoError(t, statErr, "the worktree must survive the refused discard")
}

// Scenario: the checkout fails half-way — `git worktree add -b` creates
// the branch ref BEFORE it populates the directory, so the branch
// survives a failure the directory does not. Left behind, it silently
// turns the next attempt into a reuse-this-branch spawn that ignores
// --worktree-base, which is how a worker ends up on a stale tree. The
// daemon rolls the branch back with the checkout.
//
// The failure is induced the way it happens for real: the directory the
// worktree would land in cannot be written (here the repo's parent is
// read-only; in the field it was a sandbox denying one tracked path).
func TestSpawnCLI_WorktreeBranchRolledBackWhenCheckoutFails(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")
	bridgeAgentClientToMux(t, f.Mux)

	if os.Geteuid() == 0 {
		// Mode bits do not bind root, so the checkout would succeed and
		// this test would assert the opposite of what happens.
		t.Skip("read-only directory does not stop root")
	}

	repo, parent := initRepoOnMain(t)
	require.NoError(t, os.Chmod(parent, 0o555), "make the worktree's parent read-only")
	// Restore write access so t.TempDir()'s own cleanup can run.
	t.Cleanup(func() { _ = os.Chmod(parent, 0o755) })

	stderr := new(bytes.Buffer)
	resp, rc := agent.RunSpawn(
		&agent.SpawnParams{Group: "alpha", Name: "worker", Cwd: repo, Worktree: "feat-x", WorktreeBase: "main"},
		new(bytes.Buffer), stderr, new(bytes.Buffer),
	)
	require.Nilf(t, resp, "a failed worktree checkout spawns nothing; stderr=%s", stderr.String())
	require.NotEqual(t, 0, rc, "a failed worktree checkout is a non-zero rc")

	assert.NotContainsf(t, worktree.BranchesIn(repo), "feat-x",
		"the branch the failed checkout cut must be rolled back; branches=%v",
		worktree.BranchesIn(repo))
	_, statErr := os.Stat(filepath.Join(parent, "repo-feat-x"))
	assert.Truef(t, os.IsNotExist(statErr), "no worktree dir should survive; stat err=%v", statErr)
}
