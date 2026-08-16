package agentd

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
)

// branchlinks_repo.go is the repository gate in front of the dashboard's
// branch-link resolution (TCL-1169).
//
// branchlinks.go resolves an agent's Branch column by running `git` and `gh`
// INSIDE a directory the agent owns. Two things follow from that, and neither
// is fixed by pinning the directory:
//
//   - the repository is not an input the daemon passes, it is state in the
//     directory. A one-line `.git` gitfile ("gitdir: /elsewhere/.git") makes
//     `git remote get-url origin` report another repository's remote, and
//     `remote.origin.gh-resolved` — what `gh repo set-default` writes — re-aims
//     `gh` while leaving the remote URL, and therefore the dashboard, looking
//     entirely normal;
//   - the directory itself is only as trustworthy as its source. The current
//     dir arrives from agent_workdir, written verbatim from a PostToolUse hook
//     payload, so it names a path of the agent's choosing.
//
// The consequence was that the operator's `gh` credential could be spent on a
// repository they never allow-listed, its check rollup cached, and a foreign
// pull request rendered as that agent's own.
//
// This file closes that by resolving the repository the way resolveProxyRepo
// does — physical path, work-tree root, git-dir containment, linked-worktree
// proof — and then naming the repository EXPLICITLY (`gh --repo owner/repo`
// from a neutral directory) instead of letting the working directory select it.
// Finally it bounds the answer by the operator's own
// `agent.git_proxy.allowed_remotes` list, which is the only statement in the
// system about which repositories the daemon's credentials may reach.
//
// That last gate is why the whole path is conditional. An operator who has not
// configured the Git proxy has stated nothing, so applying the allow-list to
// them would empty the dashboard's Branch column rather than protect it. So
// gitProxyHardeningActive() selects between the two: proxy configured means
// this gate, proxy absent means the pre-existing unbounded resolution. See
// liveGitInfoResolver.

// branchLinkRepo is a directory that survived the gate: a validated work-tree
// root plus the GitHub identity resolved from its own origin remote. Nothing
// downstream re-reads the directory to find out which repository it is — that
// is the point.
type branchLinkRepo struct {
	Root          string // physical git work-tree root
	RepoURL       string // https://github.com/owner/repo — the dashboard's web link base
	OwnerRepo     string // owner/repo, passed to `gh --repo`
	DefaultBranch string // the repo's default branch, resolved under the same pins
}

// resolveBranchLinkRepo validates dir and resolves the repository it belongs
// to. It returns ok=false for every refusal — an unreachable path, a
// non-repository, a redirected git dir, a non-GitHub or non-allow-listed
// remote — because the caller's contract is "no links", not "an error". The
// caller writes a negative cache entry either way, so a refused directory is
// not re-probed on every 2s snapshot.
func resolveBranchLinkRepo(ctx context.Context, dir string) (branchLinkRepo, bool) {
	cfg, err := config.Load()
	if err != nil {
		slog.Debug("branchlinks: refusing resolution, configuration unreadable",
			"error", err, "module", "agentd")
		return branchLinkRepo{}, false
	}
	policy := cfg.ResolvedGitProxy()
	if len(policy.AllowedRemotes) == 0 {
		// Reached only when the config became readable-but-empty between the
		// activation check and here. Nothing is allow-listed, so nothing may
		// be resolved.
		return branchLinkRepo{}, false
	}
	gitPath, err := proxyBinary("git")
	if err != nil {
		return branchLinkRepo{}, false
	}
	hooksDir, err := gitProxyHooksDir()
	if err != nil {
		slog.Debug("branchlinks: refusing resolution, no private hooks directory",
			"error", err, "module", "agentd")
		return branchLinkRepo{}, false
	}
	// The same `-c` prefix every proxied git invocation carries. These probes
	// are local and read-only, but they still run against configuration the
	// agent wrote, and core.fsmonitor / core.alternateRefsCommand / core.pager
	// select a PROGRAM rather than describe a value. The pins are what stop a
	// repository from executing something on the daemon host merely by being
	// looked at.
	pins := gitProxyConfigPins(hooksDir, gitProxySSHCommand(policy),
		globalCredentialHelpers(ctx, gitPath))
	// Every probe names its directory twice — as `-C <dir>` and as the child's
	// working directory — so neither the daemon's own cwd nor a later argv
	// change can move where git looks.
	probe := func(workDir string, args ...string) string {
		argv := append(append([]string(nil), pins...), "-C", workDir)
		res, err := proxyExec(ctx, ProxyCommand{
			Tool: "git",
			Path: gitPath,
			Args: append(argv, args...),
			Dir:  workDir,
			Env:  gitProxyEnv(),
		})
		if err != nil || res.ExitCode != 0 {
			return ""
		}
		return strings.TrimSpace(res.Stdout)
	}

	root, ok := branchLinkRepoRoot(ctx, gitPath, dir, probe)
	if !ok {
		return branchLinkRepo{}, false
	}

	// The remote is read from the VALIDATED root, not from the directory the
	// caller named — a subdirectory carries no config of its own, but a
	// redirected one would have.
	remote := probe(root, "remote", "get-url", "origin")
	ref, err := parseRemoteURL(remote)
	if err != nil {
		return branchLinkRepo{}, false
	}
	if !remoteAllowed(ref, policy.AllowedRemotes) {
		slog.Debug("branchlinks: repository is not on agent.git_proxy.allowed_remotes",
			"repo", ref.Key(), "module", "agentd")
		return branchLinkRepo{}, false
	}
	// parseRemoteURL has already applied git's url.<base>.insteadOf rewrites,
	// so the allow-list above saw the destination git would actually use. What
	// is left for repoHTTPSFromRemote is the web-link form, which exists only
	// for github.com.
	repoURL := repoHTTPSFromRemote(remote)
	ownerRepo := ref.OwnerRepo()
	if repoURL == "" || ownerRepo == "" {
		return branchLinkRepo{}, false
	}
	return branchLinkRepo{
		Root:      root,
		RepoURL:   repoURL,
		OwnerRepo: ownerRepo,
		DefaultBranch: gitDefaultBranchWith(func(args ...string) string {
			return probe(root, args...)
		}),
	}, true
}

// branchLinkRepoRoot is resolveProxyRepo's directory gate, applied to a
// directory the dashboard was asked about rather than to a session's recorded
// launch dir. The checks and their order are deliberately the same ones, for
// the reasons documented at resolveProxyRepo and acceptLinkedWorktree; keeping
// them identical is what stops a rule added there from being silently missing
// here.
func branchLinkRepoRoot(ctx context.Context, gitPath, dir string, probe func(string, ...string) string) (string, bool) {
	dir = strings.TrimSpace(dir)
	if dir == "" || !filepath.IsAbs(dir) {
		return "", false
	}
	// Resolve symlinks before asking git anything, so the path handed to the
	// subprocess is the physical one and cannot be re-aimed by swapping a link
	// between the check and the use.
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", false
	}
	root := probe(resolved, "rev-parse", "--path-format=absolute", "--show-toplevel")
	if root == "" || !filepath.IsAbs(root) {
		return "", false
	}
	root = filepath.Clean(root)

	// A `.git` GITFILE leaves the toplevel pointing at the agent's own
	// directory while the real GIT_DIR — config, refs, and therefore remotes —
	// lives somewhere else entirely. Without this an agent drops a one-line
	// .git file naming another repository's admin dir and the dashboard
	// resolves, links and credentials that repository instead.
	gitDir := probe(resolved, "rev-parse", "--absolute-git-dir")
	if gitDir == "" {
		return "", false
	}
	gitDir = canonicalProxyPath(gitDir)
	if !sandboxpolicy.PathContainsOrEqual(root, gitDir) {
		// Linked worktrees legitimately point outside the work tree, and this
		// project's own worktree helpers create them, so they are admitted —
		// but only on the back-pointer that PROVES the link, never on the
		// shape alone.
		if fault := acceptLinkedWorktree(ctx, gitPath, resolved, root, gitDir); fault != nil {
			slog.Debug("branchlinks: refusing a work tree whose git dir is redirected",
				"root", root, "git_dir", gitDir, "module", "agentd")
			return "", false
		}
	}

	// A directory that is not itself inside a repository makes git walk
	// upward, and if the operator's home is a dotfiles repo the daemon would
	// resolve THAT. A work tree at or above home is never an agent's project.
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		if resolvedHome, err := filepath.EvalSymlinks(home); err == nil {
			home = resolvedHome
		}
		if sandboxpolicy.PathContainsOrEqual(root, filepath.Clean(home)) {
			slog.Debug("branchlinks: refusing a work tree that contains the operator's home",
				"root", root, "module", "agentd")
			return "", false
		}
	}
	return root, true
}
