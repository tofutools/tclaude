package agentd

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

const (
	maxAgentPRURLLen                  = 2048
	maxAgentPRSummaryLen              = 200
	recentlyMergedPRPollInterval      = 10 * time.Second
	recentlyMergedPRSearchResultLimit = 20
)

var (
	presentedPRInflight       sync.Map
	presentedPRInfoResolver   = livePresentedPRInfoResolver
	recentlyMergedPRsResolver = liveRecentlyMergedPRsResolver
)

// presentedPRView is the dashboard wire shape for explicitly presented PRs.
// It is separate from repoLinksView's branch/startup PR fields because an
// agent may want to present a PR after leaving the branch, or present multiple
// related PRs. The frontend dedupes by URL against branch/startup PR links.
type presentedPRView struct {
	URL       string `json:"url"`
	Number    int    `json:"number,omitempty"`
	Summary   string `json:"summary,omitempty"`
	State     string `json:"state,omitempty"`
	UpdatedAt string `json:"updated_at,omitempty"`
}

func presentedPRViews(rows []db.AgentPR) []presentedPRView {
	if len(rows) == 0 {
		return nil
	}
	out := make([]presentedPRView, 0, len(rows))
	seen := map[string]bool{}
	for _, row := range rows {
		u := strings.TrimSpace(row.PRURL)
		if u == "" || seen[u] {
			continue
		}
		seen[u] = true
		v := presentedPRView{
			URL:     u,
			Number:  deriveGitHubPRNumber(u),
			Summary: row.Summary,
			State:   row.State,
		}
		if !row.UpdatedAt.IsZero() {
			v.UpdatedAt = row.UpdatedAt.Format("2006-01-02T15:04:05Z07:00")
		}
		out = append(out, v)
	}
	return out
}

// preloadPresentedPRsForDashboard is the dashboard snapshot's single
// agent_prs read. It also piggybacks the existing branch-link freshness budget:
// stale GitHub PRs schedule an async `gh` refresh, and terminal PRs stay
// visible for one TTL before being marked handled and omitted.
func preloadPresentedPRsForDashboard(now time.Time) map[string][]db.AgentPR {
	all, err := db.ListUnhandledAgentPRs()
	if err != nil {
		return map[string][]db.AgentPR{}
	}
	out := map[string][]db.AgentPR{}
	for agentID, rows := range all {
		for _, row := range rows {
			if terminalPresentedPRExpired(row, now) {
				if _, err := db.MarkAgentPRHandled(row.AgentID, row.PRURL); err != nil {
					slog.Warn("presented-pr: failed to mark terminal PR handled",
						"error", err, "agent_id", row.AgentID, "url", row.PRURL, "module", "agentd")
				}
				continue
			}
			if presentedPRNeedsRefresh(row, now) {
				schedulePresentedPRRefresh(row.AgentID, row.PRURL)
			}
			out[agentID] = append(out[agentID], row)
		}
	}
	return out
}

func terminalPresentedPRExpired(row db.AgentPR, now time.Time) bool {
	if !isTerminalPresentedPRState(row.State) || row.UpdatedAt.IsZero() {
		return false
	}
	return now.Sub(row.UpdatedAt) >= branchLinkTTL
}

func presentedPRNeedsRefresh(row db.AgentPR, now time.Time) bool {
	if !isGitHubPresentedPRURL(row.PRURL) || isTerminalPresentedPRState(row.State) {
		return false
	}
	if row.State != "" && !row.UpdatedAt.IsZero() && now.Sub(row.UpdatedAt) < branchLinkTTL {
		return false
	}
	return !presentedPRCacheFresh(row.PRURL, now)
}

func isTerminalPresentedPRState(state string) bool {
	switch strings.ToLower(strings.TrimSpace(state)) {
	case "merged", "closed":
		return true
	default:
		return false
	}
}

type presentedPRInfo struct {
	Number    int       `json:"number"`
	URL       string    `json:"url"`
	State     string    `json:"state"`
	FetchedAt time.Time `json:"fetched_at"`
}

func presentedPRCacheFresh(rawURL string, now time.Time) bool {
	row, err := db.LoadGitCache(presentedPRCacheKey(rawURL))
	if err != nil || row == nil {
		return false
	}
	var info presentedPRInfo
	if json.Unmarshal(row.Data, &info) != nil || info.FetchedAt.IsZero() {
		return false
	}
	return now.Sub(info.FetchedAt) < branchLinkTTL
}

func schedulePresentedPRRefresh(agentID, rawURL string) {
	key := presentedPRCacheKey(rawURL)
	if _, busy := presentedPRInflight.LoadOrStore(key, struct{}{}); busy {
		return
	}
	goBackground(func() {
		defer presentedPRInflight.Delete(key)
		refreshPresentedPR(agentID, rawURL, key)
	})
}

func refreshPresentedPR(agentID, rawURL, key string) {
	info, ok := presentedPRInfoResolver(rawURL)
	now := time.Now()
	if !ok {
		info = presentedPRInfo{URL: strings.TrimSpace(rawURL)}
	}
	info.State = strings.ToLower(strings.TrimSpace(info.State))
	info.FetchedAt = now
	savePresentedPRCache(key, rawURL, info, now)
	if !ok || info.State == "" {
		return
	}
	if _, err := db.UpdateAgentPRState(agentID, rawURL, info.State); err != nil {
		slog.Warn("presented-pr: failed to refresh PR state",
			"error", err, "agent_id", agentID, "url", rawURL, "state", info.State, "module", "agentd")
	}
}

func livePresentedPRInfoResolver(rawURL string) (presentedPRInfo, bool) {
	out := runInDir("", "gh", "pr", "view", strings.TrimSpace(rawURL), "--json", "number,url,state")
	if out == "" {
		return presentedPRInfo{}, false
	}
	var pr struct {
		Number int    `json:"number"`
		URL    string `json:"url"`
		State  string `json:"state"`
	}
	if json.Unmarshal([]byte(out), &pr) != nil {
		return presentedPRInfo{}, false
	}
	if pr.URL == "" {
		pr.URL = strings.TrimSpace(rawURL)
	}
	return presentedPRInfo{Number: pr.Number, URL: pr.URL, State: strings.ToLower(pr.State)}, true
}

func savePresentedPRCache(key, rawURL string, info presentedPRInfo, now time.Time) {
	data, err := json.Marshal(info)
	if err != nil {
		return
	}
	if err := db.SaveGitCache(key, data, now); err != nil {
		slog.Warn("presented-pr: failed to cache PR refresh",
			"error", err, "url", rawURL, "module", "agentd")
	}
}

// startRecentlyMergedPRPoller runs one daemon-wide GitHub search immediately,
// then once per interval. The search input comes from all unhandled presented
// PR rows in one DB read, so its cadence and subprocess count never scale with
// the number of agents or groups.
func startRecentlyMergedPRPoller(stop <-chan struct{}) {
	go func() {
		pollRecentlyMergedPRs()
		ticker := time.NewTicker(recentlyMergedPRPollInterval)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				pollRecentlyMergedPRs()
			}
		}
	}()
}

type githubPRRef struct {
	repo   string
	number int
}

func (r githubPRRef) key() string {
	return strings.ToLower(r.repo) + "#" + strconv.Itoa(r.number)
}

// pollRecentlyMergedPRs complements the individual gh-pr-view refresh path.
// One bulk search catches the common case (a recently merged PR authored by
// the authenticated gh user) within about ten seconds. Anything it cannot see
// — another author's PR, an unmerged close, a result outside the newest 20, or
// a failed search — remains covered by the existing per-PR resolver.
func pollRecentlyMergedPRs() {
	all, err := db.ListUnhandledAgentPRs()
	if err != nil {
		slog.Warn("presented-pr: failed to list PRs for merged search",
			"error", err, "module", "agentd")
		return
	}

	targets := make(map[string][]db.AgentPR)
	reposByKey := make(map[string]string)
	for _, rows := range all {
		for _, row := range rows {
			if isTerminalPresentedPRState(row.State) {
				continue
			}
			ref, ok := githubPRRefFromURL(row.PRURL)
			if !ok {
				continue
			}
			targets[ref.key()] = append(targets[ref.key()], row)
			reposByKey[strings.ToLower(ref.repo)] = ref.repo
		}
	}
	if len(targets) == 0 {
		return
	}

	repos := make([]string, 0, len(reposByKey))
	for _, repo := range reposByKey {
		repos = append(repos, repo)
	}
	sort.Strings(repos)

	merged, ok := recentlyMergedPRsResolver(repos)
	if !ok {
		return
	}
	now := time.Now()
	cached := make(map[string]bool)
	for _, info := range merged {
		ref, valid := githubPRRefFromURL(info.URL)
		if !valid {
			continue
		}
		rows := targets[ref.key()]
		if len(rows) == 0 {
			continue
		}
		info.State = "merged"
		info.FetchedAt = now
		for _, row := range rows {
			if _, err := db.UpdateAgentPRState(row.AgentID, row.PRURL, info.State); err != nil {
				slog.Warn("presented-pr: failed to apply merged search result",
					"error", err, "agent_id", row.AgentID, "url", row.PRURL, "module", "agentd")
			}
			if !cached[row.PRURL] {
				savePresentedPRCache(presentedPRCacheKey(row.PRURL), row.PRURL, info, now)
				cached[row.PRURL] = true
			}
		}
	}
}

func liveRecentlyMergedPRsResolver(repos []string) ([]presentedPRInfo, bool) {
	args := []string{
		"search", "prs",
		"--author=@me",
		"--merged",
		"--sort", "updated",
		"--limit", strconv.Itoa(recentlyMergedPRSearchResultLimit),
		"--json", "repository,number,title,closedAt,url",
	}
	for _, repo := range repos {
		args = append(args, "--repo", repo)
	}
	out := runInDir("", "gh", args...)
	if out == "" {
		return nil, false
	}
	var prs []struct {
		Number int    `json:"number"`
		URL    string `json:"url"`
	}
	if json.Unmarshal([]byte(out), &prs) != nil {
		return nil, false
	}
	result := make([]presentedPRInfo, 0, len(prs))
	for _, pr := range prs {
		result = append(result, presentedPRInfo{
			Number: pr.Number,
			URL:    strings.TrimSpace(pr.URL),
			State:  "merged",
		})
	}
	return result, true
}

func presentedPRCacheKey(rawURL string) string {
	h := sha256.Sum256([]byte("presented-pr\x00" + strings.TrimSpace(rawURL)))
	return "ppr_" + hex.EncodeToString(h[:8])
}

func validateAgentPRURL(rawURL string) error {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return fmt.Errorf("PR URL is empty")
	}
	if len(rawURL) > maxAgentPRURLLen {
		return fmt.Errorf("PR URL is too long (%d > %d chars)", len(rawURL), maxAgentPRURLLen)
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("PR URL is not a valid URL: %w", err)
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return fmt.Errorf("PR URL must be http(s), got %q", u.Scheme)
	}
	if u.Host == "" {
		return fmt.Errorf("PR URL must include a host")
	}
	return nil
}

func validateAgentPRSummary(summary string) error {
	if len(summary) > maxAgentPRSummaryLen {
		return fmt.Errorf("PR summary is too long (%d > %d chars)", len(summary), maxAgentPRSummaryLen)
	}
	return nil
}

func normalizeAgentPRState(state string) (string, error) {
	state = strings.ToLower(strings.TrimSpace(state))
	switch state {
	case "", "open", "merged", "closed", "handled":
		return state, nil
	default:
		return "", fmt.Errorf("PR state must be one of: open, merged, closed, handled")
	}
}

func isGitHubPresentedPRURL(rawURL string) bool {
	return deriveGitHubPRNumber(rawURL) > 0
}

func deriveGitHubPRNumber(rawURL string) int {
	ref, ok := githubPRRefFromURL(rawURL)
	if !ok {
		return 0
	}
	return ref.number
}

func githubPRRefFromURL(rawURL string) (githubPRRef, bool) {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || !strings.EqualFold(u.Host, "github.com") {
		return githubPRRef{}, false
	}
	segs := pathSegments(u.Path)
	if len(segs) < 4 || segs[0] == "" || segs[1] == "" ||
		segs[2] != "pull" || !isAllDigits(segs[3]) {
		return githubPRRef{}, false
	}
	n, _ := strconv.Atoi(segs[3])
	if n <= 0 {
		return githubPRRef{}, false
	}
	return githubPRRef{repo: segs[0] + "/" + segs[1], number: n}, true
}
