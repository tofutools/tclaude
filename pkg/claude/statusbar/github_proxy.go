package statusbar

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// github_proxy.go routes the status bar's ONE credentialed read — "is there a
// pull request for the branch I am on?" — through agentd's GitHub proxy.
//
// It exists because the status bar is the only part of tclaude outside agentd
// that ever needed a GitHub credential. Everything else it asks git for is
// local (the remote URL, the branch, the default branch), and every other `gh`
// call in the tree already lives in the daemon, which runs unsandboxed and
// holds the token. That left one asymmetry: a pane whose sandbox denies
// ~/.config/gh — the posture the proxy exists to make workable — rendered its
// PR link from a `gh` that could not authenticate, so the link silently
// vanished for exactly the agents the proxy was built for.
//
// The gate is the operator's configuration, projected through /v1/info. With
// no proxy configured the status bar shells out to `gh` exactly as it always
// has; the proxy is an opt-in surface and this must not be the thing that
// turns it on. See docs/git-proxy.md.
//
// Two costs are worth stating plainly, because they are why the lookup is
// throttled harder on this path than on the `gh` one (see prLookupTTL):
//
//   - The call spends the OPERATOR'S GitHub credential and is audited, like
//     every other proxy verb. A status line refreshing several times a second
//     must not turn the audit log into a render log.
//   - It needs the `proxy.github.read` slug, which is not granted by default.
//     An agent without it simply keeps the bar it would have had.

const (
	// ghProxyProbeTimeout bounds the /v1/info capability probe. It is a
	// constant-returning read on a local socket, so a slow one means the
	// daemon is wedged — and a status bar must never wait on that.
	ghProxyProbeTimeout = time.Second

	// ghProxyCallTimeout bounds the pull-request read itself, which does reach
	// GitHub. Same three seconds the render broker allows: a status line that
	// blocks is a visibly frozen pane, and a missing PR link is not.
	//
	// It is far under the daemon's own 60s bound, so a genuinely slow GitHub
	// leaves the daemon still working on a request nobody is reading. That is
	// the right trade here — the answer is cosmetic and the next lookup will
	// ask again.
	ghProxyCallTimeout = 3 * time.Second

	// ghProxyPRListLimit is how many pull requests to ask for on the branch.
	// More than one because a branch can carry several over its life — a
	// closed attempt, then the real one — and the newest is not always the
	// open one. Ten is far more than any branch legitimately accumulates and
	// still a single cheap page.
	ghProxyPRListLimit = 10
)

// ghProxyPRListResponse is the part of the proxy's outcome envelope this
// caller needs. An HTTP 200 means the daemon REACHED GitHub; ExitCode is
// GitHub's own verdict, so both have to be checked.
type ghProxyPRListResponse struct {
	ExitCode int             `json:"exit_code"`
	JSON     json.RawMessage `json:"json"`
	Stderr   string          `json:"stderr"`
}

// ghProxyPREntry is the subset of a `pr ls` row the bar renders from.
type ghProxyPREntry struct {
	Number      int    `json:"number"`
	State       string `json:"state"`
	URL         string `json:"url"`
	HeadRefName string `json:"headRefName"`
}

// githubProxyEnabled reports whether this pane's PR lookup should go through
// agentd rather than `gh`.
//
// The answer comes from the DAEMON, never from local config, and that is the
// whole point: a sandboxed pane cannot read ~/.tclaude/data/config.json, so
// asking it would report "no proxy" precisely where the proxy matters most.
// /v1/info's `proxy` bit is the capability projection of the operator's policy
// — one boolean, with none of the allow-list behind it crossing the sandbox
// boundary — and it already accounts for a per-agent remote-scoped grant.
//
// Everything else is false: no daemon, an older daemon that does not project
// the bit, a malformed answer. False means "use `gh`, as before", which is the
// behaviour that cannot regress anyone.
func githubProxyEnabled() bool {
	client, err := daemonSocketClient(ghProxyProbeTimeout)
	if err != nil {
		return false
	}
	resp, err := client.Get("http://tclaude/v1/info")
	if err != nil {
		return false
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return false
	}
	var info struct {
		Proxy *bool `json:"proxy"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&info); err != nil {
		return false
	}
	return info.Proxy != nil && *info.Proxy
}

// proxyPRInfo asks agentd for the pull request opened from branch, returning
// ok=false when the proxy could not answer at all — the daemon went away, the
// agent holds no `proxy.github.read` grant, the remote is not allow-listed,
// GitHub refused. The caller falls back to `gh` on false, so a refusal costs
// the bar nothing it would otherwise have had.
//
// "No pull request for this branch" is an ANSWER, not a failure: it returns
// ok=true with a zero number, so the caller caches the negative result instead
// of asking GitHub again on the next render.
func proxyPRInfo(branch string) (number int, url, state string, ok bool) {
	body, err := json.Marshal(map[string]any{
		"head": branch,
		// Every state, because a merged or closed PR is still the branch's
		// pull request and the bar renders it with that state. Filtering to
		// open would make a just-merged PR's link disappear.
		"state": "all",
		"limit": ghProxyPRListLimit,
	})
	if err != nil {
		return 0, "", "", false
	}
	client, err := daemonSocketClient(ghProxyCallTimeout)
	if err != nil {
		return 0, "", "", false
	}
	req, err := http.NewRequest(http.MethodPost, "http://tclaude/v1/github/pr/list", bytes.NewReader(body))
	if err != nil {
		return 0, "", "", false
	}
	req.Header.Set("Content-Type", "application/json")
	// No Idempotency-Key: this is a read, and a key would have the daemon
	// persist its response for the idempotency TTL for no benefit at all.
	resp, err := client.Do(req)
	if err != nil {
		return 0, "", "", false
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		logProxyPRFailure(branch, resp.StatusCode, "")
		return 0, "", "", false
	}
	var out ghProxyPRListResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil {
		return 0, "", "", false
	}
	if out.ExitCode != 0 {
		logProxyPRFailure(branch, resp.StatusCode, out.Stderr)
		return 0, "", "", false
	}
	var prs []ghProxyPREntry
	if err := json.Unmarshal(out.JSON, &prs); err != nil {
		return 0, "", "", false
	}
	pr := pickBranchPR(prs, branch)
	if pr == nil {
		return 0, "", "", true
	}
	return pr.Number, pr.URL, strings.ToLower(pr.State), true
}

// pickBranchPR chooses which of a branch's pull requests the bar shows, in the
// order `gh pr view <branch>` does: an open one if there is one, otherwise the
// most recently created.
//
// The rows arrive newest-created first, so "first open, else first" is exactly
// that rule. The head-name re-check is belt and braces — the daemon filtered
// on it — but it is one comparison, and it keeps a widened filter from
// putting another branch's PR in this branch's status bar.
func pickBranchPR(prs []ghProxyPREntry, branch string) *ghProxyPREntry {
	var newest *ghProxyPREntry
	for i := range prs {
		if prs[i].HeadRefName != "" && prs[i].HeadRefName != branch {
			continue
		}
		if strings.EqualFold(prs[i].State, "open") {
			return &prs[i]
		}
		if newest == nil {
			newest = &prs[i]
		}
	}
	return newest
}

// logProxyPRFailure records a refusal at DEBUG and nothing louder.
//
// The common cause is an operator who configured the proxy without granting
// this agent `proxy.github.read`, which is a deliberate posture rather than a
// fault — and the bar degrades to `gh` and then to no PR link, which is the
// same thing an unauthenticated `gh` has always produced. A WARN here would
// fire on a timer, forever, in every such pane.
func logProxyPRFailure(branch string, status int, stderr string) {
	slog.Debug("status-bar: agentd's github proxy did not answer the branch's PR",
		"branch", branch, "status", status, "stderr", strings.TrimSpace(stderr), "module", "hooks")
}
