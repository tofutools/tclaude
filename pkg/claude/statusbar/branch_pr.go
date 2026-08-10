package statusbar

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// branch_pr.go asks agentd for the pull request on this pane's branch, instead
// of shelling out to `gh` for it.
//
// The status bar is the only part of tclaude outside agentd that ever needed a
// GitHub credential — everything else it asks git for is local, and every other
// `gh` call in the tree already lives in the daemon. That left one asymmetry: a
// pane whose sandbox denies ~/.config/gh rendered its PR link from a `gh` that
// could not authenticate, so the link silently vanished for exactly the agents
// tclaude sandboxes hardest.
//
// The daemon is asked because IT ALREADY KNOWS. branchlinks.go resolves
// (repoDir, branch) → PR on a 90-second cache for every agent the dashboard
// lists, so this is a database read on the far side of a unix socket: no
// GitHub traffic, no credential spent, no permission grant, and no audit row on
// a surface that re-renders several times a second. Routing it through the
// GitHub proxy instead would have cost all four — see
// agentd/statusline_branchpr.go, which is where that reasoning lives, and
// docs/git-proxy.md for the proxy's own `pr ls --head`, which remains the
// gated and audited way to ask GitHub itself.
//
// The cadence is therefore unchanged: this rides the same 15-second git
// snapshot the bar has always used, because asking costs the daemon a cache
// read and nothing more.

// branchPRTimeout bounds the ask. The daemon answers from its own cache and
// never blocks on the network for this route, so a slow answer means a wedged
// daemon — and a status line that waits on one is a visibly frozen pane, which
// is a worse defect than a missing link.
const branchPRTimeout = time.Second

// ghPRTimeout bounds the `gh pr view` fallback, which DOES reach the network.
// The call used to be unbounded, so a `gh` waiting on a network that never
// answers froze the pane for as long as it liked.
const ghPRTimeout = 3 * time.Second

// branchPRResponse mirrors the daemon's wire shape.
type branchPRResponse struct {
	// Resolved reports that the daemon has actually looked. It is what
	// separates "there is no pull request on this branch" — a real answer,
	// and the usual one on a freshly pushed branch — from "not resolved
	// yet", which is what a cold cache says on the first ask and must not be
	// rendered as "no PR".
	Resolved bool   `json:"resolved"`
	PRNumber int    `json:"pr_number"`
	PRURL    string `json:"pr_url"`
	PRState  string `json:"pr_state"`
}

// daemonBranchPR asks agentd for branch's pull request, reporting ok=false when
// the daemon could not answer — not running, could not place this pane, or has
// not resolved the branch yet. The caller falls back to `gh` on false, which is
// what covers the cold-cache window: the same ask that misses also schedules
// the daemon's refresh, so the second one lands.
//
// The branch is sent; the DIRECTORY is not, and deliberately cannot be. The
// daemon resolves that from this pane's own identity, so no caller can point it
// at a repository that is not its own.
func daemonBranchPR(ctx context.Context, branch string) (number int, prURL, state string, ok bool) {
	client, err := daemonSocketClient(branchPRTimeout)
	if err != nil {
		return 0, "", "", false
	}
	reqCtx, cancel := context.WithTimeout(ctx, branchPRTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet,
		"http://tclaude/v1/statusline/branch-pr?branch="+url.QueryEscape(branch), nil)
	if err != nil {
		return 0, "", "", false
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, "", "", false
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return 0, "", "", false
	}
	var out branchPRResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&out); err != nil {
		return 0, "", "", false
	}
	if !out.Resolved {
		return 0, "", "", false
	}
	return out.PRNumber, out.PRURL, strings.ToLower(out.PRState), true
}

// dropForeignRepoPR discards a PR that does not belong to the repository this
// snapshot describes.
//
// The two halves of a snapshot come from different places and can disagree
// about which repository they mean. RepoURL and Branch come from bare `git` in
// the statusline process's own working directory, while the daemon answers for
// the directory IT has recorded as this agent's. When an agent's harness cwd is
// a different repository from the one the daemon tracks, the answer describes a
// repository the bar is not rendering, and without this the link would show
// repository B's pull request under repository A's branch.
//
// Belt and braces on the `gh` path too, which resolves against whatever
// repository its own cwd is in.
func dropForeignRepoPR(data *GitSnapshot) {
	if data.PRURL == "" || data.RepoURL == "" {
		return
	}
	// Case-insensitively, because the two strings come from different places
	// and GitHub does not force them to agree: RepoURL is whatever the local
	// remote is spelled as, while the PR URL carries the owner and repository
	// casing GitHub has on record. A remote written `github.com/ToFuTools/…`
	// against a PR URL GitHub renders as `tofutools` is the same repository,
	// and dropping its PR would blank a link that is perfectly correct.
	prefix := strings.ToLower(strings.TrimSuffix(data.RepoURL, "/")) + "/"
	if strings.HasPrefix(strings.ToLower(data.PRURL), prefix) {
		return
	}
	data.PRNumber, data.PRURL, data.PRState = 0, "", ""
}
