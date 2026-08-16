package agentd

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

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
// proof — and then naming the repository EXPLICITLY (`gh --repo owner/repo`,
// outside the agent's work tree) instead of letting the working directory
// select it. Finally it bounds the answer by the operator's own
// `agent.git_proxy.allowed_remotes` list — their daemon-wide statement about
// which repositories the daemon's credentials may reach. A remote-scoped
// permission grant states the same thing per agent and authorizes the proxy
// verbs on its own, but there is no calling agent here to read one from, so it
// cannot bound this surface.
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

// branchLinkRepoResolveTimeout bounds the WHOLE repository resolution, not each
// probe within it. Five or so local git calls share it, which is deliberate:
// the alternative is a per-probe deadline whose worst case is the sum, and this
// runs on a background refresh goroutine whose single-flight key is held until
// it returns. A budget the caller can reason about matters more here than
// letting a stalled filesystem have another 12 seconds per probe.
const branchLinkRepoResolveTimeout = 30 * time.Second

// resolveBranchLinkRepo validates dir and resolves the repository it belongs
// to, under the operator policy the caller already loaded. It returns ok=false
// for every refusal — an unreachable path, a non-repository, a redirected git
// dir, a non-GitHub or non-allow-listed remote — because the caller's contract
// is "no links", not "an error". The caller writes a negative cache entry
// either way, so a refused directory is not re-probed on every 2s snapshot.
func resolveBranchLinkRepo(ctx context.Context, policy config.GitProxyConfig, dir string) (branchLinkRepo, bool) {
	if len(policy.AllowedRemotes) == 0 {
		// No allow-list is no authorization. The caller only reaches here with
		// a non-empty one, but the gate is cheap and this function must not
		// depend on that for its safety.
		return branchLinkRepo{}, false
	}
	ctx, cancel := context.WithTimeout(ctx, branchLinkRepoResolveTimeout)
	defer cancel()
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
	//
	// The credential-helper list is deliberately empty. gitProxyConfigPins
	// clears the helper list and re-adds the operator's own global ones so a
	// PROXIED FETCH can still authenticate; nothing here authenticates, so
	// re-adding them buys nothing and costs two `git config --get-all`
	// subprocesses on every refresh — subprocesses that read the operator's
	// HOME and would spend this function's whole budget first if that home ever
	// stalled. Clearing without re-adding is the stricter half of the pin.
	pins := gitProxyConfigPins(hooksDir, gitProxySSHCommand(policy), nil)
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
	// EXACTLY two path segments, then a slug check on each — the same refusal
	// githubproxy.go makes, for the same reason. matchRemotePattern admits a
	// pattern shorter than the target as a PREFIX, while OwnerRepo() is first
	// segment + last segment, so the two rules disagree the moment a remote has
	// more than two segments:
	//
	//   allow-list  github.com/acme/widgets        (the "one repo" form)
	//   remote      github.com/acme/widgets/secret
	//
	// The allow-list admits that remote and the derived slug becomes
	// acme/secret — a repository the operator never listed, handed straight to
	// `gh --repo`. Re-deriving a repository from a path matched under a
	// different rule is an allow-list escape, not a nicety. A GitHub repository
	// is always owner/repo, and branch links are GitHub-only anyway, so
	// refusing anything else costs nothing here.
	if len(ref.Path) != 2 {
		slog.Debug("branchlinks: refusing to derive a repository from a nested remote path",
			"repo", ref.Key(), "module", "agentd")
		return branchLinkRepo{}, false
	}
	ownerRepo := ref.OwnerRepo()
	owner, repoName, _ := strings.Cut(ownerRepo, "/")
	if !isGitHubOwnerSlug(owner) || !isGitHubRepoSlug(repoName) {
		slog.Debug("branchlinks: refusing a remote that is not a valid github owner/repo pair",
			"repo", ref.Key(), "module", "agentd")
		return branchLinkRepo{}, false
	}
	// parseRemoteURL has already applied git's url.<base>.insteadOf rewrites,
	// so the allow-list above saw the destination git would actually use. What
	// is left for repoHTTPSFromRemote is the web-link form, which exists only
	// for github.com.
	repoURL := repoHTTPSFromRemote(remote)
	if repoURL == "" {
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
		//
		// This is the one probe on this path that runs UNPINNED: the shared
		// helper builds its own argv, as it does for resolveProxyRepoAt, which
		// has no pins at the point it calls this either. Its single verb is
		// `rev-parse --git-common-dir`, a path lookup that consults none of the
		// program-selecting keys the pins exist for.
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
