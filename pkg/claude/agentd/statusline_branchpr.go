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
//  2. The branch IS the caller's, because the status bar has just run `git` and
//     knows it exactly, while the stored location can lag a branch flip by a
//     refresh. It selects a cache key WITHIN the resolved directory and can
//     therefore not reach outside it.
//
// No permission slug and no audit row, by operator ruling: this returns the
// agent's own pull-request link, which its own status bar has always displayed,
// and which the operator can see in the dashboard.

// statuslineBranchPRResponse is the wire shape. Resolved distinguishes "this
// daemon has looked and there is no pull request" from "it has not looked yet",
// which the caller cannot infer from an empty PR: the first answer is final
// until the branch moves, the second means try `gh` and ask again shortly.
type statuslineBranchPRResponse struct {
	Resolved bool   `json:"resolved"`
	PRNumber int    `json:"pr_number,omitempty"`
	PRURL    string `json:"pr_url,omitempty"`
	PRState  string `json:"pr_state,omitempty"`
	RepoURL  string `json:"repo_url,omitempty"`
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
	branch := strings.TrimSpace(r.URL.Query().Get("branch"))
	if branch == "" || len(branch) > maxGitProxyRefLen {
		writeJSON(w, http.StatusOK, statuslineBranchPRResponse{})
		return
	}
	loc := locationView(p.ConvID)
	dir := loc.CurrentDir
	if dir == "" {
		dir = loc.StartupDir
	}
	if dir == "" {
		writeJSON(w, http.StatusOK, statuslineBranchPRResponse{})
		return
	}
	_, prNumber, prURL, prState, fetchedAt := lookupBranchLinkOne(dir, branch)
	writeJSON(w, http.StatusOK, statuslineBranchPRResponse{
		Resolved: !fetchedAt.IsZero(),
		PRNumber: prNumber,
		PRURL:    prURL,
		PRState:  prState,
	})
}
