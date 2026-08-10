package statusbar

import (
	"bytes"
	"context"
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
// The gate is the operator's policy, projected through /v1/info as a single
// boolean this pane could not otherwise read. Without it the status bar shells
// out to `gh` exactly as it always has; the proxy is an opt-in surface and
// this must not be the thing that turns it on. See docs/git-proxy.md.
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
	// prLookupBudget is the TOTAL wall clock one PR lookup may spend, across
	// every step it takes: the capability probe, the proxied read, and the
	// `gh` fallback behind it.
	//
	// One budget rather than a timeout per step, because the steps compose.
	// Per-step bounds add up, and a wedged daemon socket that accepts but
	// never answers would spend the probe's bound, then the read's, and only
	// then start an unbounded subprocess — inside the statusline command the
	// harness is waiting on. What the pane can afford is a property of the
	// pane, not of how many places the answer might come from.
	prLookupBudget = 4 * time.Second

	// ghProxyProbeTimeout bounds the /v1/info capability probe WITHIN that
	// budget. It is a constant-returning read on a local socket, so a slow one
	// means the daemon is wedged, and leaving the rest of the budget for the
	// call that actually reaches GitHub is the better use of it.
	ghProxyProbeTimeout = time.Second

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
	// IsCrossRepository marks a pull request whose head branch lives in a
	// FORK. The daemon's --head filter matches the bare ref NAME across every
	// head repository, so on a public repo a local branch called "fix-typo"
	// lists an outside contributor's fork branch of the same name alongside
	// its own. This is the only field that tells them apart.
	IsCrossRepository bool `json:"isCrossRepository"`
}

// githubProxyEnabled reports whether this pane's PR lookup should go through
// agentd rather than `gh`.
//
// The answer comes from the DAEMON, never from local config, and that is the
// whole point: a sandboxed pane cannot read ~/.tclaude/data/config.json, so
// asking it would report "no proxy" precisely where the proxy matters most.
// One boolean crosses the boundary; none of the allow-list behind it does.
//
// It reads `github_read`, NOT the broader `proxy` bit the CLI uses to decide
// whether to show its command tree. `proxy` is true whenever the operator
// configured the proxy at all, or the caller holds a scoped grant on any of
// the four proxy slugs — so an agent granted only `proxy.git.push` would be
// told yes, then be refused by every read it made, on a timer, with an audit
// row per refusal. `github_read` asks for the slug this call actually needs
// plus the remote policy that slug depends on.
//
// Everything else is false: no daemon, an older daemon that does not project
// the bit, a malformed answer. False means "use `gh`, as before", which is the
// behaviour that cannot regress anyone.
func githubProxyEnabled(ctx context.Context) bool {
	client, err := daemonSocketClient(ghProxyProbeTimeout)
	if err != nil {
		return false
	}
	probeCtx, cancel := context.WithTimeout(ctx, ghProxyProbeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(probeCtx, http.MethodGet, "http://tclaude/v1/info", nil)
	if err != nil {
		return false
	}
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return false
	}
	var info struct {
		GitHubRead *bool `json:"github_read"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&info); err != nil {
		return false
	}
	return info.GitHubRead != nil && *info.GitHubRead
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
func proxyPRInfo(ctx context.Context, branch string) (number int, url, state string, ok bool) {
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
	// Whatever is left of the shared budget. Far under the daemon's own 60s
	// bound, so a genuinely slow GitHub leaves the daemon still working on a
	// request nobody is reading — the right trade for a cosmetic answer the
	// next lookup will ask for again.
	client, err := daemonSocketClient(prLookupBudget)
	if err != nil {
		return 0, "", "", false
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"http://tclaude/v1/github/pr/list", bytes.NewReader(body))
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
// that rule.
//
// Fork pull requests are dropped, and that is not belt and braces — it is the
// difference between this and `gh`, which matches a branch on its head LABEL
// (`owner:branch` when the head repository differs) while GraphQL's filter
// matches the bare ref NAME across every head repository. Without the check, a
// local branch named `fix-typo` on a public repo renders an outside
// contributor's fork PR of the same name as though it were the agent's own
// work. The head-name re-check beside it is the cheap guard against the
// daemon's filter ever widening.
func pickBranchPR(prs []ghProxyPREntry, branch string) *ghProxyPREntry {
	var newest *ghProxyPREntry
	for i := range prs {
		if prs[i].IsCrossRepository {
			continue
		}
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
