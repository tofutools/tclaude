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
// The daemon is asked because it resolves this anyway: branchlinks.go maps
// (repoDir, branch) → PR behind a 90-second cache, so this is a database read
// on the far side of a unix socket — no GitHub traffic, no credential spent, no
// permission grant, and no audit row on a surface that re-renders several times
// a second. Routing it through the GitHub proxy instead would have cost all
// four; see agentd/statusline_branchpr.go, where that reasoning lives, and
// docs/git-proxy.md for the proxy's own `pr ls --head`, which remains the gated
// and audited way to ask GitHub itself.
//
// It resolves on DEMAND, though, not on a timer — the daemon has no scheduled
// branch-link refresh, so the first ask on a cold cache returns nothing and
// merely schedules the work. That is the main reason the `gh` fallback below
// still earns its place: it covers the gap until the next render's ask lands.
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
	PRNumber int    `json:"pr_number"`
	PRURL    string `json:"pr_url"`
	PRState  string `json:"pr_state"`
}

// daemonBranchPR asks agentd for branch's pull request, reporting ok=false
// whenever it does not come back with one — no daemon, a pane it cannot place,
// a cache it has not warmed yet, or a branch with no pull request at all.
//
// A PR IS THE ONLY SUCCESS. The daemon is not asked to distinguish "I looked
// and there is none" from "I have not looked", because it cannot do so
// honestly: refreshBranchLink stamps its cache on every outcome, including a
// `gh` that failed and a directory that resolved to the wrong repository. A
// caller that trusted such a flag would suppress the `gh` fallback that would
// have found the PR. So an empty answer costs exactly what it cost before this
// route existed — one `gh pr view` — and the saving is on the branches that
// actually have a pull request, which is where the daemon's answer is worth
// having.
//
// The cold-cache miss is not wasted either: the same ask that returns nothing
// schedules the daemon's resolution, so the next render's ask lands.
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
	if out.PRURL == "" {
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
	repo := repoSlug(data.RepoURL)
	// An unrecognisable repo URL is not evidence of anything, and blanking a
	// PR on it would be the same silent-drop bug in the other direction.
	if repo == "" || repo == repoSlug(data.PRURL) {
		return
	}
	data.PRNumber, data.PRURL, data.PRState = 0, "", ""
}

// repoSlug reduces a repository or pull-request URL to `host/owner/repo`,
// lower-cased, or "" when it does not look like one.
//
// A comparison rather than a string prefix, because the two URLs it has to
// reconcile are spelled by different authorities and agree on nothing but
// those three segments:
//
//	https://github.com/o/r          the remote, as getRepoHTTPS rewrites it
//	ssh://git@github.com/o/r        the remote, cloned over ssh — getRepoHTTPS
//	                                rewrites only the `git@host:` form
//	https://github.com/o/r/pull/7   the pull request, as GitHub renders it
//
// A prefix test calls the second of those foreign to the third and silently
// blanks a perfectly correct link. Case folding matters for the same reason:
// GitHub keeps the owner and repository casing it has on record, while the
// remote is spelled however the operator cloned it.
func repoSlug(rawURL string) string {
	s := strings.ToLower(strings.TrimSpace(rawURL))
	if _, after, ok := strings.Cut(s, "://"); ok {
		s = after
	}
	// Strip any userinfo — `git@`, and the `user:token@` a credential helper
	// may have written into the remote.
	if _, after, ok := strings.Cut(s, "@"); ok {
		s = after
	}
	// The scp-like form `host:owner/repo` has no scheme and separates the host
	// with a colon. Rewriting it to a slash here is what lets a repository
	// cloned that way — every non-github.com host, since getRepoHTTPS rewrites
	// only `git@github.com:` — be recognised as its own rather than falling
	// through as unparseable, which would switch the guard off entirely.
	if host, rest, ok := strings.Cut(s, ":"); ok {
		// `host:22/owner/repo` is a PORT, not a path; drop it. Distinguished
		// from the scp form by what follows: digits then a slash.
		if port, tail, isPort := strings.Cut(rest, "/"); isPort && isAllDigits(port) {
			s = host + "/" + tail
		} else {
			s = host + "/" + rest
		}
	}
	segs := strings.Split(strings.Trim(s, "/"), "/")
	if len(segs) < 3 || segs[0] == "" || segs[1] == "" || segs[2] == "" {
		return ""
	}
	return segs[0] + "/" + segs[1] + "/" + strings.TrimSuffix(segs[2], ".git")
}

func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	return strings.IndexFunc(s, func(r rune) bool { return r < '0' || r > '9' }) < 0
}
