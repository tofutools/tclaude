package agentd

import (
	"net/http"
	"path/filepath"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// statusline_branchpr.go serves GET /v1/statusline/branch-pr — the pull request
// for the caller's own current branch, read from the resolution this daemon has
// ALREADY performed for the dashboard's Branch column.
//
// It exists because the status bar is the only part of tclaude outside agentd
// that ever needed a GitHub credential, and a pane whose sandbox denies
// ~/.config/gh could not spend one: its `gh pr view` failed to authenticate and
// the PR link silently vanished for exactly the agents tclaude sandboxes
// hardest.
//
// The obvious fix — have the status bar call the GitHub proxy — was measured
// and rejected. A proxied read spends the OPERATOR's credential, needs the
// `proxy.github.read` grant, and writes an audit row, and a status line
// re-renders several times a second: it would have put a credentialed,
// audit-logged GitHub call on a display refresh, and buried the trail of what
// agents actually did with that credential under render traffic.
//
// This costs none of that, because it reuses a resolution the daemon already
// performs: branchlinks.go maps (repoDir, branch) → PR behind a 90-second
// cache, and handing that value back is a database read — no new GitHub
// traffic, no credential spent, no grant, no audit row. The proxy keeps its own
// `pr ls --head` verb for callers that genuinely want a fresh, attributable
// read; that one is still gated and still audited.
//
// "Already performs" is not the same as "performs on a timer", and the
// difference decides whether this route works at all. NOTHING in agentd
// refreshes branch links on a schedule. There are exactly two drivers:
// /api/snapshot, which runs only while a browser is polling the dashboard, and
// this route. An implementation that merely READ the cache would therefore
// answer nothing forever for an operator who never opens the dashboard.
//
// So the ask drives the work. lookupBranchLinkOne routes through the same core
// the dashboard uses, whose cold-or-stale path schedules the async refresh —
// which means the FIRST ask returns nothing and arranges for the second to
// land, on the status bar's own 15-second cadence and with `gh` covering the
// gap. TestStatuslineBranchPRResolvesWithoutADashboard pins exactly that
// sequence with no dashboard code path involved.
//
// Two properties are load-bearing:
//
//  1. THE DIRECTORY IS NOT A PARAMETER, AND NOT agent_workdir EITHER. It comes
//     from the session's recorded launch directory — the same root
//     resolveProxyRepo uses, and for the identical reason, quoted there: that
//     value is written by the daemon at spawn time and the caller never
//     authors it.
//
//     The obvious-looking source is agent.ResolveLocation's CurrentDir, and it
//     is a trap. That resolves through `agent_workdir`, which the PostToolUse
//     hook writes from `filepath.Dir(tool_input.file_path)` — a raw payload
//     field, on the failure arm too, and the brokered hook route clamps
//     nothing and carries no permission slug. An agent can therefore nominate
//     ANY path by attempting an edit, and the daemon would then run `git` and
//     `gh` with cmd.Dir set there, where repo-local config picks the
//     repository. `--head` being selector-free would not matter: cwd chooses
//     the repo. spawn_dir_trust.go says the same thing in one line —
//     agent_workdir is "display state, not an authorization root".
//
//  2. The branch is the caller's, validated. Once (1) holds it is no longer a
//     repository selector: `gh pr list --head <branch>` (see ghPRListArgs)
//     filters by branch name inside the directory (1) chose, and gh documents
//     that it does not even accept the `owner:branch` cross-repo form. What
//     the gate below still buys is refusing a value that would read as a FLAG
//     in that position, plus the ordinary ref-shape rules.
//
// No permission slug and no audit row, by operator ruling: with those
// properties holding, this returns the agent's own pull-request link — which
// its own status bar has always displayed, and which the operator can see in
// the dashboard.
//
// TCL-1161 tracks the remaining, deliberately deferred cost: the route is an
// unmetered trigger for `gh`, bounded to the agent's own repository.

// statuslineBranchPRResponse is the wire shape.
//
// There is deliberately no "resolved" flag. An earlier shape had one, meaning
// "this daemon has looked and there is no pull request" as against "it has not
// looked yet", so the caller could skip its `gh` fallback on the first. The
// daemon cannot honestly answer that: refreshBranchLink stamps FetchedAt on
// EVERY outcome, including a `gh` that failed and a directory that resolved to
// the wrong repository — so "looked, found nothing" and "looked in the wrong
// place" were indistinguishable, and the caller suppressed a `gh` that would
// have found the PR. An empty answer here therefore means only "nothing to
// offer", and the caller falls back exactly as it did before this route
// existed.
type statuslineBranchPRResponse struct {
	PRNumber int    `json:"pr_number,omitempty"`
	PRURL    string `json:"pr_url,omitempty"`
	PRState  string `json:"pr_state,omitempty"`
}

func handleStatuslineBranchPR(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method", "GET only")
		return
	}
	p := peerFromContext(r.Context())
	// Fail closed on anyone the daemon cannot place. An unconfirmed caller has
	// no location to resolve, so there is nothing to answer anyway — but this
	// is the check that keeps that a refusal rather than an accident.
	if classify(p) != classAgent || p.ConvID == "" {
		writeJSON(w, http.StatusOK, statuslineBranchPRResponse{})
		return
	}
	dir, ok := recordedLaunchDirForConv(p.ConvID)
	if !ok {
		writeJSON(w, http.StatusOK, statuslineBranchPRResponse{})
		return
	}
	// Validated, then used as a branch FILTER inside the directory resolved
	// above — never as anything that could select a repository. See the header.
	branch := strings.TrimSpace(r.URL.Query().Get("branch"))
	if fault := validateBranchName(branch); fault != nil {
		writeJSON(w, http.StatusOK, statuslineBranchPRResponse{})
		return
	}
	_, prNumber, prURL, prState, _ := lookupBranchLinkOne(dir, branch)
	writeJSON(w, http.StatusOK, statuslineBranchPRResponse{
		PRNumber: prNumber,
		PRURL:    prURL,
		PRState:  prState,
	})
}

// recordedLaunchDirForConv resolves the one directory this route will act in:
// the conversation's recorded launch dir, resume provenance first.
//
// Deliberately the same resolution resolveProxyRepo performs, minus the git
// gates it needs because it is about to spend a credential on a REMOTE. The
// shared part is the part that matters: the path comes from daemon-authored
// session state, so no request and no agent-written table can steer it.
func recordedLaunchDirForConv(convID string) (string, bool) {
	sess, err := db.FindSessionByConvID(convID)
	if err != nil || sess == nil {
		return "", false
	}
	dir, err := recordedLaunchDir(sess)
	if err != nil {
		return "", false
	}
	// Absolute only, for the same reason the proxy insists on it: a relative
	// path would be resolved against whatever working directory the daemon
	// happens to have.
	if dir == "" || !filepath.IsAbs(dir) {
		return "", false
	}
	return dir, true
}
