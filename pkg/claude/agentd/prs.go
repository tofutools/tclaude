package agentd

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

const (
	maxAgentPRURLLen                         = 2048
	maxAgentPRSummaryLen                     = 200
	recentlyMergedPRPollInterval             = 10 * time.Second
	recentlyMergedPRMaxBackoff               = 5 * time.Minute
	defaultRecentlyMergedPRSearchResultLimit = 20
	maxRecentlyMergedPRSearchResultLimit     = 100
	maxRecentlyMergedPRSearchQueryChars      = 200
	maxRecentlyMergedPRBooleanOperators      = 5
)

var (
	presentedPRInflight           sync.Map
	presentedPRInfoResolver       = livePresentedPRInfoResolver
	recentlyMergedPRsResolver     = liveRecentlyMergedPRsResolver
	recentlyMergedPRSearch        = liveRecentlyMergedPRSearch
	githubLoginResolver           = liveGitHubLoginResolver
	githubConfiguredLoginResolver = liveConfiguredGitHubLoginResolver
	githubEnvironmentTokenPresent = liveGitHubEnvironmentTokenPresent
	githubLoginCache              struct {
		sync.Mutex
		login            string
		environmentToken bool
	}
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
	updatedAt time.Time
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
			URL:       u,
			Number:    deriveGitHubPRNumber(u),
			Summary:   row.Summary,
			State:     row.State,
			updatedAt: row.UpdatedAt,
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
// then once per interval. Consecutive failures exponentially back off to five
// minutes; warnings are emitted only while that delay grows, then one recovery
// message closes the incident. The search input comes from all unhandled
// presented PR rows in one DB read, so its cadence and subprocess count never
// scale with the number of agents or groups.
func startRecentlyMergedPRPoller(stop <-chan struct{}) {
	go func() {
		timer := time.NewTimer(0)
		defer timer.Stop()
		var backoff recentlyMergedPRPollBackoff
		for {
			select {
			case <-stop:
				return
			case <-timer.C:
				attempted, err := pollRecentlyMergedPRs()
				delay, warn, recovered, previousFailures := backoff.next(attempted, err)
				if warn {
					slog.Warn("presented-pr: recently merged search failed; backing off",
						"error", err, "consecutive_failures", backoff.failures,
						"retry_in", delay, "module", "agentd")
				}
				if recovered {
					slog.Info("presented-pr: recently merged search recovered",
						"previous_failures", previousFailures, "module", "agentd")
				}
				timer.Reset(delay)
			}
		}
	}()
}

type recentlyMergedPRPollBackoff struct {
	failures int
}

func (b *recentlyMergedPRPollBackoff) next(attempted bool, err error) (delay time.Duration, warn, recovered bool, previousFailures int) {
	previousFailures = b.failures
	if err != nil {
		b.failures++
		delay = recentlyMergedPRRetryDelay(b.failures)
		previousDelay := recentlyMergedPRRetryDelay(b.failures - 1)
		warn = b.failures == 1 || delay > previousDelay
		return delay, warn, false, previousFailures
	}
	b.failures = 0
	// A no-target interval clears an old streak but must not claim that GitHub
	// recovered: no request ran. A successful attempted poll closes the
	// incident with one recovery message.
	recovered = attempted && previousFailures > 0
	return recentlyMergedPRPollInterval, false, recovered, previousFailures
}

func recentlyMergedPRRetryDelay(failures int) time.Duration {
	if failures <= 0 {
		return recentlyMergedPRPollInterval
	}
	delay := recentlyMergedPRPollInterval
	for range min(failures, 5) {
		delay *= 2
	}
	return min(delay, recentlyMergedPRMaxBackoff)
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
// — another author's PR, an unmerged close, a result outside the bounded
// 20–100 result page, or a failed search — remains covered by the existing
// per-PR resolver.
func pollRecentlyMergedPRs() (bool, error) {
	all, err := db.ListUnhandledAgentPRs()
	if err != nil {
		return false, fmt.Errorf("list presented PRs: %w", err)
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
		return false, nil
	}

	repos := make([]string, 0, len(reposByKey))
	for _, repo := range reposByKey {
		repos = append(repos, repo)
	}
	sort.Strings(repos)

	resultLimit := recentlyMergedPRResultLimit(len(targets))
	merged, err := recentlyMergedPRsResolver(repos, resultLimit)
	if err != nil {
		return true, fmt.Errorf("gh recently merged PR search: %w", err)
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
	return true, nil
}

func recentlyMergedPRResultLimit(targetCount int) int {
	return min(max(defaultRecentlyMergedPRSearchResultLimit, targetCount), maxRecentlyMergedPRSearchResultLimit)
}

func liveRecentlyMergedPRsResolver(repos []string, resultLimit int) ([]presentedPRInfo, error) {
	login, ok := cachedGitHubLogin()
	if !ok {
		return nil, fmt.Errorf("resolve active GitHub login")
	}
	searchRepos := boundedRecentlyMergedPRSearchRepos(login, repos)
	if len(repos) > 0 && len(searchRepos) == 0 {
		// The server-side repo narrowing had to fall away to keep the query
		// valid. Use the whole one-request page to reduce the chance that an
		// unrelated recent merge crowds a tracked PR out of the global result.
		resultLimit = maxRecentlyMergedPRSearchResultLimit
	}
	args := []string{
		"api", "--hostname", "github.com", "--method", "GET", "search/issues",
		"-F", "advanced_search=true",
		"-f", "q=" + recentlyMergedPRSearchQueryForRepos(login, searchRepos),
		"-f", "sort=updated",
		"-f", "order=desc",
		"-F", "per_page=" + strconv.Itoa(resultLimit),
	}
	out, err := recentlyMergedPRSearch(args...)
	if err != nil {
		return nil, err
	}
	var search struct {
		Items []struct {
			Number  int    `json:"number"`
			HTMLURL string `json:"html_url"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(out), &search); err != nil {
		return nil, fmt.Errorf("decode search response: %w", err)
	}
	result := make([]presentedPRInfo, 0, len(search.Items))
	for _, pr := range search.Items {
		result = append(result, presentedPRInfo{
			Number: pr.Number,
			URL:    strings.TrimSpace(pr.HTMLURL),
			State:  "merged",
		})
	}
	return result, nil
}

func recentlyMergedPRSearchQuery(login string, repos []string) string {
	return recentlyMergedPRSearchQueryForRepos(login, boundedRecentlyMergedPRSearchRepos(login, repos))
}

func recentlyMergedPRSearchQueryForRepos(login string, repos []string) string {
	query := "author:" + login + " is:merged type:pr"
	if len(repos) == 0 {
		return query
	}
	repoQueries := make([]string, 0, len(repos))
	for _, repo := range repos {
		repoQueries = append(repoQueries, "repo:"+repo)
	}
	if len(repoQueries) == 1 {
		return query + " " + repoQueries[0]
	}
	return query + " (" + strings.Join(repoQueries, " OR ") + ")"
}

func liveRecentlyMergedPRSearch(args ...string) (string, error) {
	return runInDirWithError("", "gh", args...)
}

// boundedRecentlyMergedPRSearchRepos keeps the generated GitHub search query
// below its documented query-length ceiling. If the deduped repo qualifiers
// would make the query too long, dropping all repo filters is safer than
// polling only a subset: the global authored-by-login result can still match
// every referenced PR, and remains one Search request.
func boundedRecentlyMergedPRSearchRepos(login string, repos []string) []string {
	if len(repos)-1 > maxRecentlyMergedPRBooleanOperators {
		return nil
	}
	query := recentlyMergedPRSearchQueryForRepos(login, repos)
	if len(query) > maxRecentlyMergedPRSearchQueryChars {
		return nil
	}
	return repos
}

func cachedGitHubLogin() (string, bool) {
	environmentToken := githubEnvironmentTokenPresent()
	githubLoginCache.Lock()
	defer githubLoginCache.Unlock()
	if environmentToken {
		// GH_TOKEN/GITHUB_TOKEN take precedence over stored gh credentials.
		// Resolve that token's identity once instead of trusting a potentially
		// different configured user whose author query would silently miss.
		if githubLoginCache.environmentToken && githubLoginCache.login != "" {
			return githubLoginCache.login, true
		}
		return resolveAndCacheGitHubLogin(true)
	}

	configuredLogin, configuredOK := githubConfiguredLoginResolver()
	configuredLogin = strings.TrimSpace(configuredLogin)
	if configuredOK && isGitHubOwnerSlug(configuredLogin) {
		// `gh auth switch` updates the host's configured active user. Checking
		// this local value on each poll follows switches immediately without an
		// API request; the network search remains one request per attempt.
		githubLoginCache.login = configuredLogin
		githubLoginCache.environmentToken = false
		return configuredLogin, true
	}
	if !githubLoginCache.environmentToken && githubLoginCache.login != "" {
		return githubLoginCache.login, true
	}
	return resolveAndCacheGitHubLogin(false)
}

// resolveAndCacheGitHubLogin is called with githubLoginCache locked.
func resolveAndCacheGitHubLogin(environmentToken bool) (string, bool) {
	login, ok := githubLoginResolver()
	login = strings.TrimSpace(login)
	if !ok || !isGitHubOwnerSlug(login) {
		return "", false
	}
	githubLoginCache.login = login
	githubLoginCache.environmentToken = environmentToken
	return login, true
}

func invalidateCachedGitHubLogin() {
	githubLoginCache.Lock()
	githubLoginCache.login = ""
	githubLoginCache.environmentToken = false
	githubLoginCache.Unlock()
}

func liveGitHubLoginResolver() (string, bool) {
	login := runInDir("", "gh", "api", "--hostname", "github.com", "user", "--jq", ".login")
	return login, login != ""
}

func liveConfiguredGitHubLoginResolver() (string, bool) {
	login := runInDir("", "gh", "config", "get", "user", "--host", "github.com")
	return login, login != ""
}

func liveGitHubEnvironmentTokenPresent() bool {
	return strings.TrimSpace(os.Getenv("GH_TOKEN")) != "" ||
		strings.TrimSpace(os.Getenv("GITHUB_TOKEN")) != ""
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
		segs[2] != "pull" || !isAllDigits(segs[3]) ||
		!isGitHubOwnerSlug(segs[0]) || !isGitHubRepoSlug(segs[1]) {
		return githubPRRef{}, false
	}
	n, _ := strconv.Atoi(segs[3])
	if n <= 0 {
		return githubPRRef{}, false
	}
	return githubPRRef{repo: segs[0] + "/" + segs[1], number: n}, true
}

func isGitHubOwnerSlug(s string) bool {
	if len(s) == 0 || len(s) > 39 || !isASCIIAlphaNumeric(s[0]) ||
		!isASCIIAlphaNumeric(s[len(s)-1]) {
		return false
	}
	previousHyphen := false
	for i := range len(s) {
		c := s[i]
		if isASCIIAlphaNumeric(c) {
			previousHyphen = false
			continue
		}
		if c != '-' || previousHyphen {
			return false
		}
		previousHyphen = true
	}
	return true
}

func isGitHubRepoSlug(s string) bool {
	if len(s) == 0 || len(s) > 100 {
		return false
	}
	for i := range len(s) {
		c := s[i]
		if !isASCIIAlphaNumeric(c) && c != '-' && c != '_' && c != '.' {
			return false
		}
	}
	return true
}

func isASCIIAlphaNumeric(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
}
