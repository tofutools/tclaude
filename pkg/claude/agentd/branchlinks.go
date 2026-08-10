package agentd

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
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
// dashboard polls /api/snapshot every 2s; refreshing PR state on every
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
	BranchURL        string            `json:"branch_url,omitempty"`         // web link for the current branch
	BranchPRNumber   int               `json:"branch_pr_number,omitempty"`   // PR # for the current branch; 0 = none
	BranchPRURL      string            `json:"branch_pr_url,omitempty"`      // web link to that PR
	BranchPRState    string            `json:"branch_pr_state,omitempty"`    // open|merged|closed for the current branch's PR
	StartupBranchURL string            `json:"startup_branch_url,omitempty"` // web link for the startup branch
	StartupPRNumber  int               `json:"startup_pr_number,omitempty"`  // PR # for the startup branch; 0 = none
	StartupPRURL     string            `json:"startup_pr_url,omitempty"`     // web link to that PR
	StartupPRState   string            `json:"startup_pr_state,omitempty"`   // open|merged|closed for the startup branch's PR
	PresentedPRs     []presentedPRView `json:"presented_prs,omitempty"`      // agent-authored PRs shown alongside branch PRs
	// CI check summaries for the branch/startup PR badges. Counts only —
	// the per-check list is served on demand by /api/pr-checks so the 2s
	// snapshot never carries it. nil when the PR's checks are unresolved
	// (or the PR has none), which renders as no indicator at all.
	BranchChecks     *prChecksSummary `json:"branch_checks,omitempty"`
	StartupChecks    *prChecksSummary `json:"startup_checks,omitempty"`
	branchPRUpdated  time.Time
	startupPRUpdated time.Time
}

// repoBranchInfo is the cached git/gh resolution for one
// (repoDir, branch) pair, stored as a JSON blob in the git_cache
// table. An entry with an empty RepoURL is a *negative* cache result —
// "resolved, no GitHub links" — which stops a non-GitHub repo from
// re-triggering a refresh on every 2s snapshot.
type repoBranchInfo struct {
	RepoURL       string    `json:"repo_url"`       // https://github.com/owner/repo, or "" for non-GitHub
	DefaultBranch string    `json:"default_branch"` // the repo's default branch (main/master/...)
	Branch        string    `json:"branch"`         // the branch this entry resolved
	PRNumber      int       `json:"pr_number"`      // PR number for Branch; 0 = none
	PRURL         string    `json:"pr_url"`         // web link to that PR
	PRState       string    `json:"pr_state"`       // open|merged|closed; "" = no PR
	FetchedAt     time.Time `json:"fetched_at"`     // resolution time — drives the TTL check
	// Checks rides the same `gh pr view` call the PR fields come from, but
	// is deliberately NOT persisted here: check state is cached per PR
	// identity (prc_ keys, see prchecks.go) so branch, startup and presented
	// badges for one PR share a single answer. This field is only the
	// resolver's out-channel to refreshBranchLink.
	Checks *prChecksInfo `json:"-"`
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
// present while its refresh goroutine runs, so the 2s snapshot poll
// can't stack a fresh refresh on top of an in-progress one.
var branchLinkInflight sync.Map

// branchLinksForRow resolves the link block for one agent from preloaded parts
// — the dashboard snapshot's hot path (TCL-368). The caller supplies the
// agent_workspace row it already batch-loaded plus a git_cache map keyed by
// branch-link cache key, so every slot resolves from memory instead of a
// per-slot db.LoadGitCache.
//
// The current branch always gets a lookup; the startup branch reuses that
// result when it's the same branch in the same dir (the common case — the agent
// never moved), and only gets its own lookup when it diverges. When the
// statusbar has published a live agent_workspace snapshot for convID whose
// branch matches a slot, that snapshot's repo/PR wins over the bl_ cache: the
// statusbar already paid for `git` + `gh` and stamped its result on CC's render
// cadence, bridging the gap between a branch flip and the next async bl_ refresh
// (5–90s otherwise). The override only applies to the launch dir —
// agent_workspace never sees a worktree the agent has Bash'ed into, so a moved
// agent keeps the bl_ lookup for its worktree slot.
//
// The stale/miss background refresh still fires — lookupBranchLinkRow forwards
// to scheduleBranchLinkRefresh — so a cold or aged entry keeps driving the async
// git/gh resolution across the 2s poll.
func branchLinksForRow(convID string, loc agentLocationView, ws db.AgentWorkspace, gitCache map[string]*db.GitCacheRow) repoLinksView {
	return branchLinksForParts(convID, loc, ws, func(repoDir, branch string) (string, int, string, string, time.Time) {
		return lookupBranchLinkRow(gitCache, repoDir, branch)
	})
}

// branchLinksForParts is the shared core of the branch-link resolution: it
// resolves the current + startup link slots through the supplied lookup
// function, then applies the live agent_workspace override. The per-slot link
// source (a preloaded git_cache map, via lookupBranchLinkRow) and the workspace
// row are threaded in as arguments so the resolution + override logic stays a
// single implementation.
func branchLinksForParts(convID string, loc agentLocationView, ws db.AgentWorkspace, lookup func(repoDir, branch string) (string, int, string, string, time.Time)) repoLinksView {
	var v repoLinksView
	v.BranchURL, v.BranchPRNumber, v.BranchPRURL, v.BranchPRState, v.branchPRUpdated = lookup(loc.CurrentDir, loc.Branch)
	if loc.StartupBranch == loc.Branch && loc.StartupDir == loc.CurrentDir {
		v.StartupBranchURL, v.StartupPRNumber, v.StartupPRURL, v.StartupPRState = v.BranchURL, v.BranchPRNumber, v.BranchPRURL, v.BranchPRState
		v.startupPRUpdated = v.branchPRUpdated
	} else {
		v.StartupBranchURL, v.StartupPRNumber, v.StartupPRURL, v.StartupPRState, v.startupPRUpdated = lookup(loc.StartupDir, loc.StartupBranch)
	}

	if convID == "" {
		return v
	}
	if ws.ConvID == "" || ws.RepoURL == "" || ws.Branch == "" {
		return v
	}
	webURL := branchWebURL(ws.RepoURL, ws.DefaultBranch, ws.Branch)
	// Branch slot: only override when the agent is on the launch dir
	// (the dir agent_workspace describes) AND the branch matches.
	if ws.Branch == loc.Branch && ws.Cwd != "" && loc.CurrentDir == ws.Cwd {
		v.BranchURL = webURL
		v.BranchPRNumber, v.BranchPRURL, v.BranchPRState, v.branchPRUpdated = reconcilePRSlot(
			v.BranchPRNumber, v.BranchPRURL, v.BranchPRState, v.branchPRUpdated, ws)
	}
	// Startup slot: workspace's Cwd is by definition the launch dir, so
	// matching ws.Branch to StartupBranch is enough.
	if ws.Branch == loc.StartupBranch && ws.Cwd != "" && loc.StartupDir == ws.Cwd {
		v.StartupBranchURL = webURL
		v.StartupPRNumber, v.StartupPRURL, v.StartupPRState, v.startupPRUpdated = reconcilePRSlot(
			v.StartupPRNumber, v.StartupPRURL, v.StartupPRState, v.startupPRUpdated, ws)
	}
	return v
}

// reconcilePRSlot merges the live agent_workspace snapshot into a resolved link
// slot. It decides WHICH pull request the slot names, not only what state to
// show for it.
//
// Same pull request in both sources: newestPRState reconciles the states, the
// long-standing rule — whichever source looked most recently owns the answer.
//
// Different pull requests is the case the identity has to be decided for, and
// "none at all" is one of the two. It is not hypothetical: the statusbar's
// snapshot and this daemon's own resolution run on independent clocks, so
// between an agent opening a PR and the statusbar's next lookup there is a
// window where one source knows about it and the other does not. Taking the
// workspace row unconditionally — which is what this did — let the source that
// had not looked yet erase the PR the other had already found, and the badge
// vanished from the dashboard for the length of that window.
//
// A PR beats no PR, in either direction, rather than the newer observation
// winning. That is the deliberate asymmetry: neither side can tell "there is
// no pull request" apart from "the lookup failed" — a `gh pr view` that exits
// non-zero because the caller is unauthenticated is indistinguishable from one
// that found nothing, on BOTH paths — so an absence is never evidence strong
// enough to retract a PR somebody actually saw. A pull request that genuinely
// disappears is not a thing GitHub does.
func reconcilePRSlot(number int, url, state string, updated time.Time, ws db.AgentWorkspace) (int, string, string, time.Time) {
	switch {
	case samePRURL(ws.PRURL, url):
		s, at := newestPRState(state, updated, ws.PRState, ws.UpdatedAt)
		return ws.PRNumber, ws.PRURL, s, at
	case ws.PRURL == "" && url != "":
		return number, url, state, updated
	case url == "" && ws.PRURL != "":
		return ws.PRNumber, ws.PRURL, ws.PRState, ws.UpdatedAt
	}
	// Two different pull requests, both real: the newer sighting wins.
	if !updated.IsZero() && updated.After(ws.UpdatedAt) {
		return number, url, state, updated
	}
	return ws.PRNumber, ws.PRURL, ws.PRState, ws.UpdatedAt
}

// withPresentedPRs attaches explicitly presented PRs and reconciles duplicate
// URLs against the branch/startup slots. Link placement and state freshness are
// deliberately independent: whichever source most recently checked a PR owns
// its displayed open/merged/closed state, even when the frontend later hides a
// duplicate presented badge.
func (v repoLinksView) withPresentedPRs(prs []presentedPRView) repoLinksView {
	v.PresentedPRs = prs
	for _, pr := range prs {
		if samePRURL(pr.URL, v.BranchPRURL) {
			v.BranchPRState, v.branchPRUpdated =
				newestPRState(v.BranchPRState, v.branchPRUpdated, pr.State, pr.updatedAt)
		}
		if samePRURL(pr.URL, v.StartupPRURL) {
			v.StartupPRState, v.startupPRUpdated =
				newestPRState(v.StartupPRState, v.startupPRUpdated, pr.State, pr.updatedAt)
		}
	}
	return v
}

type prStateObservation struct {
	state     string
	updatedAt time.Time
}

type prStateIndex map[string]prStateObservation

func (idx prStateIndex) add(rawURL, state string, updatedAt time.Time) {
	key := prStateKey(rawURL)
	state = strings.ToLower(strings.TrimSpace(state))
	if key == "" || state == "" {
		return
	}
	current, ok := idx[key]
	if !ok {
		idx[key] = prStateObservation{state: state, updatedAt: updatedAt}
		return
	}
	selected, selectedAt := newestPRState(current.state, current.updatedAt, state, updatedAt)
	idx[key] = prStateObservation{state: selected, updatedAt: selectedAt}
}

func (idx prStateIndex) addRepoLinks(v repoLinksView) {
	idx.add(v.BranchPRURL, v.BranchPRState, v.branchPRUpdated)
	idx.add(v.StartupPRURL, v.StartupPRState, v.startupPRUpdated)
}

// withFreshestPRStates gives every badge for the same PR the same state,
// regardless of whether that badge originated from automatic branch discovery
// or explicit presentation, and regardless of which agent row observed it.
func (v repoLinksView) withFreshestPRStates(idx prStateIndex) repoLinksView {
	if state, ok := idx[prStateKey(v.BranchPRURL)]; ok {
		v.BranchPRState = state.state
		v.branchPRUpdated = state.updatedAt
	}
	if state, ok := idx[prStateKey(v.StartupPRURL)]; ok {
		v.StartupPRState = state.state
		v.startupPRUpdated = state.updatedAt
	}
	for i := range v.PresentedPRs {
		state, ok := idx[prStateKey(v.PresentedPRs[i].URL)]
		if !ok {
			continue
		}
		v.PresentedPRs[i].State = state.state
		v.PresentedPRs[i].updatedAt = state.updatedAt
		if !state.updatedAt.IsZero() {
			v.PresentedPRs[i].UpdatedAt = state.updatedAt.Format("2006-01-02T15:04:05Z07:00")
		}
	}
	return v
}

func newestPRState(current string, currentAt time.Time, candidate string, candidateAt time.Time) (string, time.Time) {
	currentState := strings.TrimSpace(current)
	candidateState := strings.TrimSpace(candidate)
	if candidateState == "" {
		return current, currentAt
	}
	if currentState == "" {
		return candidate, candidateAt
	}
	// A GitHub PR cannot be reopened after it is merged. In particular, a
	// slower gh-pr-view request can return its older "open" observation after
	// the quick merged-PR search has completed. Wall-clock freshness cannot
	// make that lifecycle transition valid, so merged wins in either source
	// position before timestamps are considered.
	currentMerged := strings.EqualFold(currentState, "merged")
	candidateMerged := strings.EqualFold(candidateState, "merged")
	if currentMerged != candidateMerged {
		if currentMerged {
			return current, currentAt
		}
		return candidate, candidateAt
	}
	if candidateAt.IsZero() && !currentAt.IsZero() {
		return current, currentAt
	}
	if currentAt.IsZero() || !candidateAt.Before(currentAt) {
		return candidate, candidateAt
	}
	return current, currentAt
}

func prStateKey(rawURL string) string {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return ""
	}
	if ref, ok := githubPRRefFromURL(rawURL); ok {
		return "github:" + ref.key()
	}
	return "url:" + rawURL
}

func samePRURL(a, b string) bool {
	aKey := prStateKey(a)
	return aKey != "" && aKey == prStateKey(b)
}

// lookupBranchLinkRow returns the web link + PR info for a (repoDir, branch)
// pair from a preloaded git_cache map (the snapshot hot path): it derives the
// cache key, reads the row from the map (nil == miss), and resolves through the
// shared core — which STILL schedules a background refresh on a stale/missing
// entry. A blank repoDir/branch — a detached HEAD, or an agent outside a git
// repo — resolves to no link.
func lookupBranchLinkRow(gitCache map[string]*db.GitCacheRow, repoDir, branch string) (url string, prNumber int, prURL, prState string, fetchedAt time.Time) {
	if repoDir == "" || branch == "" {
		return "", 0, "", "", time.Time{}
	}
	key := branchLinkCacheKey(repoDir, branch)
	return lookupBranchLinkFromCache(repoDir, branch, key, gitCache[key])
}

// lookupBranchLinkOne resolves a single (repoDir, branch) pair, loading its
// git_cache row itself rather than taking a preloaded map.
//
// The dashboard batch-loads every key it needs per snapshot, which is why the
// hot path takes a map; a caller asking about ONE pair — the status bar, via
// /v1/statusline/branch-pr — has nothing to batch and would otherwise have to
// pass a map containing the very row it came to fetch. It routes through the
// same core, so the async refresh a stale or cold entry schedules still fires:
// that side effect is what makes the FIRST ask populate the cache the second
// one answers from.
func lookupBranchLinkOne(repoDir, branch string) (url string, prNumber int, prURL, prState string, fetchedAt time.Time) {
	if repoDir == "" || branch == "" {
		return "", 0, "", "", time.Time{}
	}
	key := branchLinkCacheKey(repoDir, branch)
	// A read failure is a cold miss, not an error: the core's answer for a
	// nil row is "nothing yet, and a refresh is now scheduled", which is
	// exactly what an unreadable row should produce.
	row, _ := db.LoadGitCache(key)
	return lookupBranchLinkFromCache(repoDir, branch, key, row)
}

// lookupBranchLinkFromCache is the shared resolution core for a (repoDir,
// branch) pair given its already-loaded git_cache row (nil == cold miss). It
// NEVER shells out: on a missing or stale entry it schedules the async refresh
// and returns whatever the row held (empty on a cold miss, stale otherwise).
// The refresh side effect is load-bearing — it is what drives PR/branch state
// forward across the 2s poll — so both the singular and the batch caller route
// through here to keep it firing.
func lookupBranchLinkFromCache(repoDir, branch, key string, row *db.GitCacheRow) (url string, prNumber int, prURL, prState string, fetchedAt time.Time) {
	var info repoBranchInfo
	fresh := false
	if row != nil {
		if json.Unmarshal(row.Data, &info) == nil {
			fresh = time.Since(info.FetchedAt) < branchLinkTTL
		}
	}
	if !fresh {
		scheduleBranchLinkRefresh(repoDir, branch, key)
	}
	return branchWebURL(info.RepoURL, info.DefaultBranch, branch),
		info.PRNumber, info.PRURL, info.PRState, info.FetchedAt
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
	// The check rollup travels with the PR fields but is cached per PR
	// identity, so every badge for this PR (branch, startup, presented,
	// on any agent row) reads one answer.
	if info.PRURL != "" && info.Checks != nil {
		checks := *info.Checks
		checks.PRState = info.PRState
		checks.FetchedAt = info.FetchedAt
		checks.Summary = summarizePRChecks(checks.Checks, checks.FetchedAt)
		savePRChecks(info.PRURL, checks)
	}
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
	// KNOWN LIMITATIONS (harmless while the flag is off; worth a look
	// before enabling it):
	//   - repo_dir provenance: a scan row stores the launch cwd while
	//     this resolver and the hook use the git worktree root. They
	//     agree for an agent launched at a repo/worktree root (the
	//     normal case) and CanonicalizeRepoDir collapses symlink/
	//     trailing-slash spellings, but a subdir launch still records
	//     two spellings — see CanonicalizeRepoDir's doc. The stamp then
	//     reaches whichever row's repo_dir matches the resolved dir;
	//     the other (cosmetic-dup) row keeps empty PR columns.
	//   - m4: the PRNumber>0 guard means a genuinely *deleted* PR is
	//     never cleared from a stale snapshot. Distinguishing "gh ran,
	//     found no PR" from "gh failed" (e.g. via `gh pr list` exit
	//     codes) would let only the former clear.
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
		info.PRNumber, info.PRURL, info.PRState, info.Checks = ghPRForBranch(repoDir, branch)
	}
	return info, true
}

// runInDir runs name+args anchored at dir under a timeout and returns
// trimmed stdout, or "" on any failure (non-zero exit, timeout, binary
// missing). Anchored (cmd.Dir) rather than relying on the daemon's own
// working directory — it inspects arbitrary agent repos.
func runInDir(dir, name string, args ...string) string {
	out, err := runInDirWithError(dir, name, args...)
	if err != nil {
		return ""
	}
	return out
}

// runInDirWithError is runInDir's diagnostic form. It retains a short,
// single-line stderr detail so long-lived background pollers can distinguish
// missing binaries, authentication failures, and API/rate-limit errors without
// dumping an unbounded subprocess response into output.log.
func runInDirWithError(dir, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), branchLinkCmdTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		detail := strings.Join(strings.Fields(stderr.String()), " ")
		if len(detail) > 500 {
			detail = detail[:500] + "..."
		}
		if detail != "" {
			return "", fmt.Errorf("%s: %w", detail, err)
		}
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
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

// ghPRForBranch returns the number, URL, state and CI check rollup of the
// pull request whose head is branch, via `gh pr view`. The state is
// lower-cased to open|merged|closed. Returns zero values when there's no
// PR, gh isn't installed, or gh isn't authenticated — all best-effort.
//
// statusCheckRollup rides this existing call rather than getting one of
// its own: the dashboard already pays for a `gh pr view` per branch PR per
// branchLinkTTL, and asking for one more JSON field is free next to a
// second network round-trip per PR.
func ghPRForBranch(dir, branch string) (number int, url, state string, checks *prChecksInfo) {
	out := runInDir(dir, "gh", "pr", "view", branch, "--json", "number,url,state,statusCheckRollup")
	if out == "" {
		// The CI rollup is an enhancement; the PR link is not. A `gh` that
		// rejects the field (old version, or a host that doesn't serve it)
		// must not take the branch's PR number/URL/state down with it, so
		// retry once for the fields that predate this feature.
		return ghPRForBranchWithoutChecks(dir, branch)
	}
	var pr struct {
		Number            int             `json:"number"`
		URL               string          `json:"url"`
		State             string          `json:"state"`
		StatusCheckRollup json.RawMessage `json:"statusCheckRollup"`
	}
	if json.Unmarshal([]byte(out), &pr) != nil {
		return 0, "", "", nil
	}
	resolved := parseStatusCheckRollup(pr.StatusCheckRollup, time.Now())
	return pr.Number, pr.URL, strings.ToLower(pr.State), &resolved
}

// ghPRForBranchWithoutChecks is ghPRForBranch's fallback: the pre-CI-badge
// query. Reached only when the richer one failed, which is also the case
// where a real "no PR here" answer is indistinguishable from a broken `gh`
// — so a failure here stays silent exactly as it did before.
func ghPRForBranchWithoutChecks(dir, branch string) (int, string, string, *prChecksInfo) {
	out := runInDir(dir, "gh", "pr", "view", branch, "--json", "number,url,state")
	if out == "" {
		return 0, "", "", nil
	}
	var pr struct {
		Number int    `json:"number"`
		URL    string `json:"url"`
		State  string `json:"state"`
	}
	if json.Unmarshal([]byte(out), &pr) != nil {
		return 0, "", "", nil
	}
	slog.Debug("branchlinks: resolved PR without the CI rollup",
		"repo", dir, "branch", branch, "pr", pr.Number, "module", "agentd")
	return pr.Number, pr.URL, strings.ToLower(pr.State), nil
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
