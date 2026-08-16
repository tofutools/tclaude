package agentd

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
)

// branchlinks_repo_test.go runs REAL git against throwaway repositories, for
// the reason gitproxy_realgit_test.go gives: a test that swaps the subprocess
// boundary can only assert what argv the daemon built, never what git does with
// it — and every vector in TCL-1169 is a thing git or gh does with a directory.
//
// Each hardening test is paired with a CONTROL showing the attack was armed:
// the legacy resolution, still reachable for an operator who has not configured
// the Git proxy, reports the attacker's chosen repository. Without that pairing
// a passing test could mean the fixture never worked.

// branchLinkGitRepo builds a repository with one commit on `main`, a `feat`
// branch, and origin pointing at originURL. It returns the work tree.
func branchLinkGitRepo(t *testing.T, gitPath, dir, originURL string) string {
	t.Helper()
	require.NoError(t, os.MkdirAll(dir, 0o755))
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command(gitPath, args...)
		cmd.Dir = dir
		cmd.Env = realGitEnv(dir)
		out, err := cmd.CombinedOutput()
		require.NoErrorf(t, err, "git %s: %s", strings.Join(args, " "), out)
	}
	run("init", "-q", ".")
	run("config", "user.email", "t@example.invalid")
	run("config", "user.name", "t")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hi\n"), 0o644))
	run("add", "a.txt")
	run("commit", "-qm", "init")
	run("branch", "-M", "main")
	run("branch", "feat")
	run("remote", "add", "origin", originURL)
	return dir
}

// branchLinkFixture prepares the daemon-side state every test here needs: a
// temp DB and HOME (so the private data dir and the home-containment check both
// live in the fixture), the operator's allow-list, and a pinned git binary. It
// returns the git path and a temp root to build repositories under.
func branchLinkFixture(t *testing.T, allowedRemotes ...string) (string, string) {
	t.Helper()
	gitPath := gitAvailable(t)
	setupTestDB(t)
	require.NoError(t, config.Save(&config.Config{Agent: &config.AgentConfig{
		GitProxy: &config.GitProxyConfig{AllowedRemotes: allowedRemotes},
	}}))
	// EvalSymlinks for the same reason realGitRepo does: git canonicalises the
	// paths it reports, and on macOS the raw t.TempDir() spelling differs.
	root, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	return gitPath, root
}

// TestBranchLinkRepoRefusesAGitfileRedirect covers vector 2 of TCL-1169.
//
// A one-line `.git` file — "gitdir: /elsewhere/.git" — is enough to make every
// question the resolver asks about a directory be answered by another
// repository's metadata, including `remote get-url origin`. The agent owns the
// directory the dashboard resolves, so writing that file is within reach.
func TestBranchLinkRepoRefusesAGitfileRedirect(t *testing.T) {
	gitPath, root := branchLinkFixture(t, "github.com")

	victim := branchLinkGitRepo(t, gitPath, filepath.Join(root, "victim"),
		"https://github.com/victim-org/private-repo.git")
	attacker := filepath.Join(root, "attacker")
	require.NoError(t, os.MkdirAll(attacker, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(attacker, ".git"),
		[]byte("gitdir: "+filepath.Join(victim, ".git")+"\n"), 0o644))

	// Control: the attack is armed. The unhardened read — which is exactly what
	// legacyGitInfo still performs — reports the victim's repository.
	assert.Equal(t, "https://github.com/victim-org/private-repo",
		repoHTTPSFromRemote(gitInDir(attacker, "remote", "get-url", "origin")),
		"fixture check: without the gate the redirect resolves the victim's repo")

	_, ok := resolveBranchLinkRepo(context.Background(), attacker)
	assert.False(t, ok, "a work tree whose git dir points elsewhere must not resolve")
}

// TestBranchLinkRepoAcceptsALinkedWorktree is the other side of that gate. This
// project's own worktree helpers create linked worktrees, whose git dir
// legitimately lives outside the work tree, so refusing the shape wholesale
// would take branch links away from the agents most likely to have them.
func TestBranchLinkRepoAcceptsALinkedWorktree(t *testing.T) {
	gitPath, root := branchLinkFixture(t, "github.com/tofutools/*")

	main := branchLinkGitRepo(t, gitPath, filepath.Join(root, "main"),
		"https://github.com/tofutools/tclaude.git")
	linked := filepath.Join(root, "linked")
	cmd := exec.Command(gitPath, "worktree", "add", "-q", linked, "feat")
	cmd.Dir = main
	cmd.Env = realGitEnv(main)
	out, err := cmd.CombinedOutput()
	require.NoErrorf(t, err, "git worktree add: %s", out)

	repo, ok := resolveBranchLinkRepo(context.Background(), linked)
	require.True(t, ok, "a registered linked worktree is the agent's own repository")
	assert.Equal(t, linked, repo.Root)
	assert.Equal(t, "https://github.com/tofutools/tclaude", repo.RepoURL)
	assert.Equal(t, "tofutools/tclaude", repo.OwnerRepo)
	assert.Equal(t, "main", repo.DefaultBranch)
}

// TestBranchLinkRepoIgnoresGhResolved covers vector 1 of TCL-1169.
//
// `remote.origin.gh-resolved` is gh's own default-repo mechanism, what
// `gh repo set-default` writes. It re-aims gh while leaving
// `git remote get-url origin` — and therefore the dashboard's link — untouched,
// so nothing looks wrong while the operator's token is spent elsewhere.
//
// The gate's answer is not to detect the key but to stop consulting the
// directory: the repository is resolved from the remote and handed to gh as
// --repo (see TestGHPRListArgsNamesTheRepositoryExplicitly), so a config key gh
// would have read has nothing left to influence.
func TestBranchLinkRepoIgnoresGhResolved(t *testing.T) {
	gitPath, root := branchLinkFixture(t, "github.com/tofutools/*")

	repoDir := branchLinkGitRepo(t, gitPath, filepath.Join(root, "work"),
		"https://github.com/tofutools/tclaude.git")
	cmd := exec.Command(gitPath, "config", "remote.origin.gh-resolved", "victim-org/private-repo")
	cmd.Dir = repoDir
	cmd.Env = realGitEnv(repoDir)
	out, err := cmd.CombinedOutput()
	require.NoErrorf(t, err, "git config: %s", out)

	repo, ok := resolveBranchLinkRepo(context.Background(), repoDir)
	require.True(t, ok)
	assert.Equal(t, "tofutools/tclaude", repo.OwnerRepo,
		"the repository gh is told about comes from the remote, not from gh-resolved")
}

// TestBranchLinkRepoBoundsResolutionByTheOperatorsAllowList is the deliberate
// decision TCL-1169 asked for: the resolution is bounded by
// agent.git_proxy.allowed_remotes. That list is the only statement an operator
// makes about which repositories the daemon's credentials may reach, and this
// path spends the same `gh` credential the proxy does.
func TestBranchLinkRepoBoundsResolutionByTheOperatorsAllowList(t *testing.T) {
	gitPath, root := branchLinkFixture(t, "github.com/tofutools/*")

	outside := branchLinkGitRepo(t, gitPath, filepath.Join(root, "outside"),
		"https://github.com/other-org/other-repo.git")
	_, ok := resolveBranchLinkRepo(context.Background(), outside)
	assert.False(t, ok, "a repository the operator never allow-listed must not be resolved")

	inside := branchLinkGitRepo(t, gitPath, filepath.Join(root, "inside"),
		"git@github.com:tofutools/tclaude.git")
	repo, ok := resolveBranchLinkRepo(context.Background(), inside)
	require.True(t, ok, "an allow-listed repository still resolves, ssh remote included")
	assert.Equal(t, "https://github.com/tofutools/tclaude", repo.RepoURL)
}

// TestBranchLinkRepoAppliesInsteadOfBeforeTheAllowList pins the order that
// makes the allow-list mean anything. `url.<base>.insteadOf` is repo-local
// config, so an agent can write it, and `remote get-url` reports the REWRITTEN
// destination — which is the string the allow-list must see.
func TestBranchLinkRepoAppliesInsteadOfBeforeTheAllowList(t *testing.T) {
	gitPath, root := branchLinkFixture(t, "github.com/tofutools/*")

	repoDir := branchLinkGitRepo(t, gitPath, filepath.Join(root, "work"),
		"https://github.com/tofutools/tclaude.git")
	cmd := exec.Command(gitPath, "config",
		"url.https://github.com/victim-org/.insteadOf", "https://github.com/tofutools/")
	cmd.Dir = repoDir
	cmd.Env = realGitEnv(repoDir)
	out, err := cmd.CombinedOutput()
	require.NoErrorf(t, err, "git config: %s", out)

	_, ok := resolveBranchLinkRepo(context.Background(), repoDir)
	assert.False(t, ok, "the rewritten destination is what the allow-list is applied to")
}

// TestBranchLinkRepoResolvesASymlinkedPath pins that the path handed to git is
// the physical one. The dashboard's directory arrives from agent_workdir, so a
// symlink swapped between the check and the use would otherwise re-aim
// everything downstream of it.
func TestBranchLinkRepoResolvesASymlinkedPath(t *testing.T) {
	gitPath, root := branchLinkFixture(t, "github.com/tofutools/*")

	repoDir := branchLinkGitRepo(t, gitPath, filepath.Join(root, "work"),
		"https://github.com/tofutools/tclaude.git")
	alias := filepath.Join(root, "alias")
	require.NoError(t, os.Symlink(repoDir, alias))

	repo, ok := resolveBranchLinkRepo(context.Background(), alias)
	require.True(t, ok)
	assert.Equal(t, repoDir, repo.Root, "the resolved root is the physical path")
}

// TestBranchLinkRepoRefusesANonRepository keeps the negative-cache contract
// honest: a directory outside any git repo is a refusal, not an error, so the
// caller writes "resolved, no links" and stops re-probing it every snapshot.
func TestBranchLinkRepoRefusesANonRepository(t *testing.T) {
	_, root := branchLinkFixture(t, "github.com")

	plain := filepath.Join(root, "plain")
	require.NoError(t, os.MkdirAll(plain, 0o755))
	_, ok := resolveBranchLinkRepo(context.Background(), plain)
	assert.False(t, ok)

	_, ok = resolveBranchLinkRepo(context.Background(), "relative/path")
	assert.False(t, ok, "a non-absolute directory is never resolved")
}

// TestLiveGitInfoResolverGatesHardeningOnTheGitProxy pins the operator-facing
// contract of TCL-1169: the new behaviour arrives with the Git proxy, and an
// operator who has not configured one keeps exactly what they had.
//
// Both halves matter. The hardened half must refuse the redirect; the legacy
// half must still follow it — not because that is desirable, but because the
// allow-list this hardening is bounded by does not exist for that operator, and
// blanking their Branch column is not a security improvement they asked for.
func TestLiveGitInfoResolverGatesHardeningOnTheGitProxy(t *testing.T) {
	gitPath, root := branchLinkFixture(t, "github.com/tofutools/*")

	victim := branchLinkGitRepo(t, gitPath, filepath.Join(root, "victim"),
		"https://github.com/victim-org/private-repo.git")
	attacker := filepath.Join(root, "attacker")
	require.NoError(t, os.MkdirAll(attacker, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(attacker, ".git"),
		[]byte("gitdir: "+filepath.Join(victim, ".git")+"\n"), 0o644))

	_, ok := liveGitInfoResolver(attacker, "feat")
	assert.False(t, ok, "with the Git proxy configured, the redirect is refused")

	// The same daemon, the same directory, an operator who configured no proxy.
	require.NoError(t, config.Save(&config.Config{}))
	require.False(t, gitProxyHardeningActive())
	info, ok := liveGitInfoResolver(attacker, "feat")
	require.True(t, ok, "the pre-existing behaviour is unchanged when the proxy is absent")
	assert.Equal(t, "https://github.com/victim-org/private-repo", info.RepoURL)
}
