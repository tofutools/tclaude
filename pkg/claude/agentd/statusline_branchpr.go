package agentd

import (
	"net/http"
	"strings"
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
// This costs none of that, because THE ANSWER ALREADY EXISTS. branchlinks.go
// resolves (repoDir, branch) → PR on a 90-second cache for every agent the
// dashboard lists, whether or not anyone is looking at it. Handing that value
// back is a database read: no new GitHub traffic, no credential spent, no grant,
// no audit row. The proxy keeps its own `pr ls --head` verb for callers that
// genuinely want a fresh, attributable read — that one is still gated and still
// audited.
//
// Two properties are load-bearing:
//
//  1. THE DIRECTORY IS NOT A PARAMETER. It is resolved from the caller's own
//     identity through the same locationView every other agent surface uses. A
//     caller-supplied path would let any confirmed agent ask about any
//     repository on the host — a filesystem reach this deliberately does not
//     lend, and the one thing that would make an ungated endpoint a real
//     widening rather than a re-read of what the pane already displays.
//  2. NEITHER IS THE BRANCH. The caller sends one, but it is compared and then
//     discarded — the lookup runs on the branch the DAEMON resolved. That is
//     not fastidiousness: the branch reaches `gh pr view`'s argv (see
//     ghPRForBranch), and `gh pr view` accepts `<number> | <url> | <branch>`,
//     so a URL argument re-aims it at ANOTHER REPOSITORY and a bare number
//     selects a pull request by id. On an ungated, unaudited route a
//     caller-supplied value in that position would let any confirmed agent
//     read any pull request the operator's token can reach, private ones
//     included. Sanitising it would mean out-guessing another tool's argument
//     parser; not passing it at all needs no such argument.
//
//     What the caller's branch is FOR is agreement. The status bar has just
//     run `git` and knows its branch exactly, while the stored location can
//     lag a flip by a refresh — so a mismatch means the two are talking about
//     different branches, and the honest answer is nothing at all. The pane
//     then falls back to `gh`, exactly as it did before this route existed.
//
// No permission slug and no audit row, by operator ruling: with those
// properties holding, this returns the agent's own pull-request link — which
// its own status bar has always displayed, and which the operator can see in
// the dashboard.

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
	loc := locationView(p.ConvID)
	dir := loc.CurrentDir
	if dir == "" {
		dir = loc.StartupDir
	}
	// The caller's branch is only ever COMPARED. Everything below runs on the
	// daemon's own resolved values, so nothing the caller sends reaches `gh` —
	// see the header for why that is the whole security argument for an
	// ungated route. A disagreement, or a location the daemon cannot resolve,
	// answers with nothing and the pane falls back to `gh`.
	branch := strings.TrimSpace(r.URL.Query().Get("branch"))
	if dir == "" || loc.Branch == "" || branch != loc.Branch {
		writeJSON(w, http.StatusOK, statuslineBranchPRResponse{})
		return
	}
	_, prNumber, prURL, prState, _ := lookupBranchLinkOne(dir, loc.Branch)
	writeJSON(w, http.StatusOK, statuslineBranchPRResponse{
		PRNumber: prNumber,
		PRURL:    prURL,
		PRState:  prState,
	})
}
