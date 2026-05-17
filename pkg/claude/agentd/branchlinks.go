package agentd

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// branchlinks.go enriches the dashboard's Branch column with clickable
// web links: a GitHub URL for the branch itself (a compare view for a
// feature branch, a tree view for the default branch) and, when one
// exists, the branch's open pull request.
//
// The data behind a link — the repo's GitHub URL, its default branch,
// and the branch's PR — comes from `git` and `gh`. Those are
// subprocess + network calls, and ResolveLocation deliberately stays a
// pure DB read (the v28 "no-git-per-refresh" goal). So the snapshot
// path NEVER shells out: lookupBranchLink reads a DB-backed cache
// (the shared git_cache table) and, on a stale/missing entry, kicks an
// async background refresh. The snapshot serves whatever the cache
// holds — empty on the first miss, populated a refresh later.
//
// `gitInfoResolver` is the subprocess seam, mirroring the
// clcommon.Default / agentd.Spawn / openTerminal boundary handles:
// production shells out to git + gh, flow tests swap in a fake.

// branchLinkTTL bounds how stale a cached branch-link entry may get
// before lookupBranchLink schedules a background refresh. The
// dashboard polls /api/snapshot every 5s; refreshing PR state on every
// poll would hammer `gh`, so the cache absorbs the gap. PR state
// changes rarely — 90s is fresh enough that a newly-opened PR appears
// within a poll or two, infrequent enough to stay cheap.
const branchLinkTTL = 90 * time.Second

// branchLinkCmdTimeout caps each git/gh subprocess. `gh pr view` hits
// the network, so a hung call would otherwise leak the refresh
// goroutine and pin its single-flight key forever — the cap guarantees
// the goroutine always returns and the key is released.
const branchLinkCmdTimeout = 12 * time.Second

// repoLinksView is the per-row link block embedded in the dashboard's
// dashboardAgent / dashboardMember wire shapes alongside
// agentLocationView. Dashboard-only — it never rides the agent-facing
// /v1/peers surface, which must not pay a git/gh cost. All fields are
// omitempty: an agent outside a GitHub repo, or one whose links
// haven't resolved yet, simply renders the branch as plain text.
type repoLinksView struct {
	BranchURL        string `json:"branch_url,omitempty"`         // web link for the current branch
	BranchPRNumber   int    `json:"branch_pr_number,omitempty"`   // open PR # for the current branch; 0 = none
	BranchPRURL      string `json:"branch_pr_url,omitempty"`      // web link to that PR
	StartupBranchURL string `json:"startup_branch_url,omitempty"` // web link for the startup branch
	StartupPRNumber  int    `json:"startup_pr_number,omitempty"`  // open PR # for the startup branch
	StartupPRURL     string `json:"startup_pr_url,omitempty"`     // web link to that PR
}

// repoBranchInfo is the cached git/gh resolution for one
// (repoDir, branch) pair, stored as a JSON blob in the git_cache
// table. An entry with an empty RepoURL is a *negative* cache result —
// "resolved, no GitHub links" — which stops a non-GitHub repo from
// re-triggering a refresh on every 5s snapshot.
type repoBranchInfo struct {
	RepoURL       string    `json:"repo_url"`       // https://github.com/owner/repo, or "" for non-GitHub
	DefaultBranch string    `json:"default_branch"` // the repo's default branch (main/master/...)
	Branch        string    `json:"branch"`         // the branch this entry resolved
	PRNumber      int       `json:"pr_number"`      // PR number for Branch; 0 = none
	PRURL         string    `json:"pr_url"`         // web link to that PR
	PRState       string    `json:"pr_state"`       // open|merged|closed; "" = no PR
	FetchedAt     time.Time `json:"fetched_at"`     // resolution time — drives the TTL check
}

// gitInfoResolver is the subprocess boundary for branch-link
// resolution. Production shells out to git + gh (liveGitInfoResolver);
// flow tests swap in a deterministic fake via SetGitInfoResolverForTest.
// It returns ok=false when the dir isn't a GitHub repo (or git failed)
// — the caller then writes a negative cache entry.
var gitInfoResolver = liveGitInfoResolver

// branchHistoryPREnrichment gates whether refreshBranchLink stamps the
// resolved PR onto the conv_branch_history table. Off by default: v1
// of the branch-history feature records the branches an agent worked
// on and leaves the PR columns empty until a branch→PR caching
// strategy is designed. The daemon flips it on at startup from
// config.Agent.BranchHistoryPREnrichment (see serve.go); flow tests
// flip it via SetBranchHistoryPREnrichmentForTest.
//
// Note this gates only the *stamp*: the branch re-scan and the
// PostToolUse hook append never resolve PRs or shell out to gh, so
// they run identically whether this is on or off. When on, the stamp
// adds zero gh calls — it reuses the resolution refreshBranchLink
// already performed for the dashboard's own Branch column.
var branchHistoryPREnrichment bool

// branchLinkInflight single-flights background refreshes: a key is
// present while its refresh goroutine runs, so the 5s snapshot poll
// can't stack a fresh refresh on top of an in-progress one.
var branchLinkInflight sync.Map

// branchLinksFor resolves the link block for one agent. The current
// branch always gets a lookup; the startup branch reuses that result
// when it's the same branch in the same dir (the common case — the
// agent never moved), and only gets its own lookup when it diverges.
func branchLinksFor(loc agentLocationView) repoLinksView {
	var v repoLinksView
	v.BranchURL, v.BranchPRNumber, v.BranchPRURL = lookupBranchLink(loc.CurrentDir, loc.Branch)
	if loc.StartupBranch == loc.Branch && loc.StartupDir == loc.CurrentDir {
		v.StartupBranchURL, v.StartupPRNumber, v.StartupPRURL = v.BranchURL, v.BranchPRNumber, v.BranchPRURL
	} else {
		v.StartupBranchURL, v.StartupPRNumber, v.StartupPRURL = lookupBranchLink(loc.StartupDir, loc.StartupBranch)
	}
	return v
}

// lookupBranchLink returns the web link + PR info for a (repoDir,
// branch) pair from the DB-backed cache. It NEVER shells out: on a
// missing or stale entry it schedules an async refresh and returns
// whatever it has (empty on a cold miss, stale otherwise). A blank
// repoDir/branch — a detached HEAD, or an agent outside a git repo —
// resolves to no link.
func lookupBranchLink(repoDir, branch string) (url string, prNumber int, prURL string) {
	if repoDir == "" || branch == "" {
		return "", 0, ""
	}
	key := branchLinkCacheKey(repoDir, branch)
	var info repoBranchInfo
	fresh := false
	if row, err := db.LoadGitCache(key); err == nil && row != nil {
		if json.Unmarshal(row.Data, &info) == nil {
			fresh = time.Since(info.FetchedAt) < branchLinkTTL
		}
	}
	if !fresh {
		scheduleBranchLinkRefresh(repoDir, branch, key)
	}
	return branchWebURL(info.RepoURL, info.DefaultBranch, branch), info.PRNumber, info.PRURL
}

// scheduleBranchLinkRefresh kicks a single background git/gh
// resolution for a (repoDir, branch) pair, deduplicated by cache key —
// a second caller while one is already running is a no-op. Runs via
// goBackground so flow tests can drain it with WaitForBackgroundForTest.
func scheduleBranchLinkRefresh(repoDir, branch, key string) {
	if _, busy := branchLinkInflight.LoadOrStore(key, struct{}{}); busy {
		return
	}
	goBackground(func() {
		defer branchLinkInflight.Delete(key)
		refreshBranchLink(repoDir, branch, key)
	})
}

// refreshBranchLink resolves a (repoDir, branch) pair through
// gitInfoResolver and writes the result — positive or negative — into
// the git_cache table. A non-GitHub repo (or a git failure) still
// writes an entry with an empty RepoURL so the dir isn't re-resolved
// on every snapshot; the TTL still lets it retry later.
func refreshBranchLink(repoDir, branch, key string) {
	info, ok := gitInfoResolver(repoDir, branch)
	if !ok {
		info = repoBranchInfo{Branch: branch}
	}
	info.FetchedAt = time.Now()
	data, err := json.Marshal(info)
	if err != nil {
		return
	}
	if err := db.SaveGitCache(key, data, info.FetchedAt); err != nil {
		slog.Warn("branchlinks: failed to cache resolution",
			"error", err, "repo", repoDir, "branch", branch, "module", "agentd")
	}
	// Mirror the PR snapshot onto the conv_branch_history rows for this
	// (repoDir, branch). The history table rides the resolution the
	// dashboard already pays for here — for an agent's active and
	// startup branches — rather than shelling out to `gh` itself.
	//
	// Gated off by default (branchHistoryPREnrichment) — v1 ships
	// branch history with empty PR columns. Only stamp when a PR was
	// actually found: `gh` is best-effort and regularly rate-limited,
	// and a failed `gh pr view` is indistinguishable from "no PR" —
	// both yield PRNumber 0. Stamping that zero would wipe a good
	// snapshot off a branch the agent has since moved away from (it
	// gets no further refresh), so a zero is treated as "no new info".
	// A merged or closed PR still reports a non-zero number, so genuine
	// state changes land.
	//
	// KNOWN MUST-FIX BEFORE ENABLING:
	//   M1 — SetConvBranchHistoryPR matches rows on (repo_dir, branch),
	//   but a scan row stores repo_dir = the launch cwd while this
	//   resolver and the hook use the resolved git worktree root. For an
	//   agent working in a worktree (the case the feature targets) those
	//   differ, so the stamp can miss the scan row. Resolve repo_dir
	//   consistently — to the git worktree root — on both the write and
	//   match sides before this flag is turned on.
	//   m4 — the PRNumber>0 guard means a genuinely *deleted* PR is
	//   never cleared from a stale snapshot. Fixing this needs the
	//   resolver to distinguish "gh ran, found no PR" from "gh failed"
	//   (e.g. via `gh pr list` exit codes) so only the former clears.
	// Both are harmless while the flag is off.
	if branchHistoryPREnrichment && info.PRNumber > 0 {
		if err := db.SetConvBranchHistoryPR(repoDir, branch, info.PRNumber, info.PRURL, info.PRState); err != nil {
			slog.Warn("branchlinks: failed to stamp branch-history PR",
				"error", err, "repo", repoDir, "branch", branch, "module", "agentd")
		}
	}
}

// branchLinkCacheKey derives the git_cache primary key for a
// (repoDir, branch) pair. The `bl_` prefix namespaces these entries
// away from the statusbar's bare repo-hash keys so the two never
// collide in the shared table.
func branchLinkCacheKey(repoDir, branch string) string {
	h := sha256.Sum256([]byte("branchlink\x00" + repoDir + "\x00" + branch))
	return "bl_" + hex.EncodeToString(h[:8])
}

// branchWebURL builds the GitHub web link for a branch: a compare view
// (default...branch — the branch's diff) for a feature branch, or a
// tree view for the default branch, where a compare against itself
// would be empty. Returns "" when the repo isn't on GitHub or the
// branch is unknown.
func branchWebURL(repoURL, defaultBranch, branch string) string {
	if repoURL == "" || branch == "" {
		return ""
	}
	if defaultBranch == "" || branch == defaultBranch {
		return repoURL + "/tree/" + branch
	}
	return repoURL + "/compare/" + defaultBranch + "..." + branch
}

// liveGitInfoResolver is the production gitInfoResolver: it shells out
// to `git` (origin remote, default branch) and `gh` (the branch's PR).
// Every call is best-effort — a missing `gh`, an unauthenticated `gh`,
// or a non-GitHub remote just yields fewer links, never an error.
func liveGitInfoResolver(repoDir, branch string) (repoBranchInfo, bool) {
	if repoDir == "" || branch == "" {
		return repoBranchInfo{}, false
	}
	repoURL := repoHTTPSFromRemote(gitInDir(repoDir, "remote", "get-url", "origin"))
	if repoURL == "" {
		// Not a GitHub repo (or no remote): nothing to link to.
		return repoBranchInfo{}, false
	}
	info := repoBranchInfo{
		RepoURL:       repoURL,
		Branch:        branch,
		DefaultBranch: gitDefaultBranch(repoDir),
	}
	// A PR lookup only makes sense for a non-default branch — the
	// default branch is the PR *target*, never its head. This also
	// skips the slowest call (`gh` hits the network) for the common
	// case of an agent sitting on main.
	if info.DefaultBranch == "" || branch != info.DefaultBranch {
		info.PRNumber, info.PRURL, info.PRState = ghPRForBranch(repoDir, branch)
	}
	return info, true
}

// runInDir runs name+args anchored at dir under a timeout and returns
// trimmed stdout, or "" on any failure (non-zero exit, timeout, binary
// missing). Anchored (cmd.Dir) rather than relying on the daemon's own
// working directory — it inspects arbitrary agent repos.
func runInDir(dir, name string, args ...string) string {
	ctx, cancel := context.WithTimeout(context.Background(), branchLinkCmdTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// gitInDir runs a git command anchored at dir, returning trimmed
// stdout or "" on failure.
func gitInDir(dir string, args ...string) string {
	return runInDir(dir, "git", args...)
}

// gitDefaultBranch returns the repo's default branch — origin/HEAD's
// target when known, else whichever of main/master exists. "" when
// neither resolves.
func gitDefaultBranch(dir string) string {
	if ref := gitInDir(dir, "symbolic-ref", "refs/remotes/origin/HEAD", "--short"); ref != "" {
		// ref looks like "origin/main" — take the segment after the last /.
		if i := strings.LastIndexByte(ref, '/'); i >= 0 && i+1 < len(ref) {
			return ref[i+1:]
		}
		return ref
	}
	for _, b := range []string{"main", "master"} {
		if gitInDir(dir, "rev-parse", "--verify", "--quiet", "refs/heads/"+b) != "" {
			return b
		}
	}
	return ""
}

// ghPRForBranch returns the number, URL and state of the pull request
// whose head is branch, via `gh pr view`. The state is lower-cased to
// open|merged|closed. Returns (0, "", "") when there's no PR, gh isn't
// installed, or gh isn't authenticated — all best-effort.
func ghPRForBranch(dir, branch string) (number int, url, state string) {
	out := runInDir(dir, "gh", "pr", "view", branch, "--json", "number,url,state")
	if out == "" {
		return 0, "", ""
	}
	var pr struct {
		Number int    `json:"number"`
		URL    string `json:"url"`
		State  string `json:"state"`
	}
	if json.Unmarshal([]byte(out), &pr) != nil {
		return 0, "", ""
	}
	return pr.Number, pr.URL, strings.ToLower(pr.State)
}

// repoHTTPSFromRemote normalises a git remote URL to its GitHub web
// base (https://github.com/owner/repo). Returns "" for a non-GitHub
// host — the dashboard then renders the branch as plain text rather
// than guessing a host-specific URL scheme.
func repoHTTPSFromRemote(remote string) string {
	remote = strings.TrimSpace(remote)
	var path string
	switch {
	case strings.HasPrefix(remote, "git@github.com:"):
		path = strings.TrimPrefix(remote, "git@github.com:")
	case strings.HasPrefix(remote, "ssh://git@github.com/"):
		path = strings.TrimPrefix(remote, "ssh://git@github.com/")
	case strings.HasPrefix(remote, "https://github.com/"):
		path = strings.TrimPrefix(remote, "https://github.com/")
	case strings.HasPrefix(remote, "http://github.com/"):
		path = strings.TrimPrefix(remote, "http://github.com/")
	default:
		return ""
	}
	path = strings.Trim(strings.TrimSuffix(strings.TrimSpace(path), ".git"), "/")
	if path == "" {
		return ""
	}
	return "https://github.com/" + path
}
