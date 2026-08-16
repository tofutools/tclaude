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
	recentlyMergedPRDashboardTargetWindow    = 10 * time.Minute
	defaultRecentlyMergedPRSearchResultLimit = 20
	maxRecentlyMergedPRSearchResultLimit     = 100
	maxRecentlyMergedPRSearchQueryChars      = 200
	maxRecentlyMergedPRBooleanOperators      = 5
)

var (
	presentedPRInflight           sync.Map
	presentedPRCacheMu            sync.Mutex
	presentedPRInfoResolver       = livePresentedPRInfoResolver
	presentedPRAccessValidator    = livePresentedPRAccessValidator
	presentedPRRemotePolicyCheck  = validatePresentedPRRemotePolicy
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
	// Checks is the CI summary for this PR — counts only; see prchecks.go.
	Checks    *prChecksSummary `json:"checks,omitempty"`
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
	// Checks rides this resolver's `gh pr view` call but is cached under
	// the shared per-PR check key instead of here — same arrangement as
	// repoBranchInfo.Checks. Resolver out-channel only, never persisted.
	Checks *prChecksInfo `json:"-"`
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
	if err := presentedPRRemotePolicyCheck(rawURL); err != nil {
		slog.Warn("presented-pr: refusing refresh outside git proxy policy",
			"error", err, "agent_id", agentID, "url", rawURL, "module", "agentd")
		return
	}
	info, ok := presentedPRInfoResolver(rawURL)
	now := time.Now()
	if !ok {
		info = presentedPRInfo{URL: strings.TrimSpace(rawURL)}
	}
	info.State = strings.ToLower(strings.TrimSpace(info.State))
	info.FetchedAt = now
	if info.Checks != nil {
		checks := *info.Checks
		checks.PRState = info.State
		checks.FetchedAt = now
		checks.Summary = summarizePRChecks(checks.Checks, now)
		savePRChecks(rawURL, checks)
	}
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
	args, ok := presentedPRViewArgs(rawURL, "number,url,state,isDraft,statusCheckRollup")
	if !ok {
		return presentedPRInfo{}, false
	}
	out := runInDir("", "gh", args...)
	if out == "" {
		// Same reasoning as ghPRForBranch: the rollup is an enhancement, the
		// PR's own state is what the badge colour depends on. Retry without
		// the newer field rather than losing both.
		return livePresentedPRInfoWithoutChecks(rawURL)
	}
	var pr struct {
		Number            int             `json:"number"`
		URL               string          `json:"url"`
		State             string          `json:"state"`
		IsDraft           bool            `json:"isDraft"`
		StatusCheckRollup json.RawMessage `json:"statusCheckRollup"`
	}
	if json.Unmarshal([]byte(out), &pr) != nil {
		return presentedPRInfo{}, false
	}
	if pr.URL == "" {
		pr.URL = strings.TrimSpace(rawURL)
	}
	checks := parseStatusCheckRollup(pr.StatusCheckRollup, time.Now())
	return presentedPRInfo{
		Number: pr.Number, URL: pr.URL, State: prStateFromGH(pr.State, pr.IsDraft), Checks: &checks,
	}, true
}

// livePresentedPRInfoWithoutChecks is the presented-PR twin of
// ghPRForBranchWithoutChecks, and asks for the same long-guaranteed field
// set only — no isDraft, for the reason documented there. A draft that has
// to come through this retry renders as a plain open badge.
func livePresentedPRInfoWithoutChecks(rawURL string) (presentedPRInfo, bool) {
	args, ok := presentedPRViewArgs(rawURL, "number,url,state")
	if !ok {
		return presentedPRInfo{}, false
	}
	out := runInDir("", "gh", args...)
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
	return presentedPRInfo{Number: pr.Number, URL: pr.URL, State: prStateFromGH(pr.State, false)}, true
}

func savePresentedPRCache(key, rawURL string, info presentedPRInfo, now time.Time) {
	// The per-PR resolver and daemon-wide merged search write this cache
	// concurrently. Serialize their read/choose/write sequence so a slower
	// open result cannot overwrite the durable merged tombstone.
	presentedPRCacheMu.Lock()
	defer presentedPRCacheMu.Unlock()

	if row, err := db.LoadGitCache(key); err == nil && row != nil {
		var current presentedPRInfo
		if json.Unmarshal(row.Data, &current) == nil {
			currentAt := current.FetchedAt
			if currentAt.IsZero() {
				currentAt = row.FetchedAt
			}
			state, fetchedAt := newestPRState(current.State, currentAt, info.State, info.FetchedAt)
			info.State = state
			info.FetchedAt = fetchedAt
			now = fetchedAt
		}
	}
	data, err := json.Marshal(info)
	if err != nil {
		return
	}
	if err := db.SaveGitCache(key, data, now); err != nil {
		slog.Warn("presented-pr: failed to cache PR refresh",
			"error", err, "url", rawURL, "module", "agentd")
	}
}

// cachedPresentedPRStates loads the durable per-PR observations for the URLs
// already present in dashboard branch/startup slots. The cache survives the
// presented badge's 90-second grace period, so a merged observation remains a
// reconciliation tombstone after the agent_prs row is marked handled.
func cachedPresentedPRStates(rawURLs []string) prStateIndex {
	urlByKey := make(map[string]string, len(rawURLs))
	keys := make([]string, 0, len(rawURLs))
	for _, rawURL := range rawURLs {
		rawURL = strings.TrimSpace(rawURL)
		if rawURL == "" {
			continue
		}
		key := presentedPRCacheKey(rawURL)
		if _, exists := urlByKey[key]; exists {
			continue
		}
		urlByKey[key] = rawURL
		keys = append(keys, key)
	}
	rows, err := db.LoadGitCacheBatch(keys)
	if err != nil {
		return make(prStateIndex)
	}
	idx := make(prStateIndex, len(rows))
	for key, row := range rows {
		var info presentedPRInfo
		if row == nil || json.Unmarshal(row.Data, &info) != nil {
			continue
		}
		fetchedAt := info.FetchedAt
		if fetchedAt.IsZero() {
			fetchedAt = row.FetchedAt
		}
		idx.add(urlByKey[key], info.State, fetchedAt)
	}
	return idx
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

// dashboardPRSearchTargets enumerates the PR URLs backing dashboard badges
// that have no agent_prs row: branch/startup links from recently refreshed
// bl_ git_cache entries, and statusbar-published agent_workspace snapshots.
// Only non-terminal GitHub PRs qualify, and a URL whose durable ppr_
// observation is already terminal is dropped — once a merge is detected, the
// PR stops widening the search query even while its stale-open bl_ entry
// waits out the branch-link TTL. Keyed by githubPRRef.key().
func dashboardPRSearchTargets(now time.Time) map[string]dashboardPRTarget {
	type candidate struct {
		url   string
		state string
	}
	var candidates []candidate
	since := now.Add(-recentlyMergedPRDashboardTargetWindow)
	blRows, err := db.ListGitCacheByPrefixSince("bl_", since)
	if err != nil {
		slog.Warn("presented-pr: failed to list branch-link cache for merged search",
			"error", err, "module", "agentd")
	}
	for _, row := range blRows {
		var info repoBranchInfo
		if row == nil || json.Unmarshal(row.Data, &info) != nil {
			continue
		}
		candidates = append(candidates, candidate{url: info.PRURL, state: info.PRState})
	}
	wsRows, err := db.ListAgentWorkspacePRsSince(since)
	if err != nil {
		slog.Warn("presented-pr: failed to list workspace PRs for merged search",
			"error", err, "module", "agentd")
	}
	for _, w := range wsRows {
		candidates = append(candidates, candidate{url: w.PRURL, state: w.PRState})
	}

	urls := make([]string, 0, len(candidates))
	seen := make(map[string]bool)
	for _, c := range candidates {
		u := strings.TrimSpace(c.url)
		if u == "" || seen[u] || isTerminalPresentedPRState(c.state) || !isGitHubPresentedPRURL(u) {
			continue
		}
		seen[u] = true
		urls = append(urls, u)
	}
	if len(urls) == 0 {
		return nil
	}
	known := cachedPresentedPRStates(urls)
	out := make(map[string]dashboardPRTarget)
	for _, u := range urls {
		if obs, ok := known[prStateKey(u)]; ok && isTerminalPresentedPRState(obs.state) {
			continue
		}
		ref, ok := githubPRRefFromURL(u)
		if !ok {
			continue
		}
		t := out[ref.key()]
		t.repo = ref.repo
		t.urls = append(t.urls, u)
		out[ref.key()] = t
	}
	return out
}

// dashboardPRTarget is one branch-detected PR the merged search should cover:
// its repo qualifier plus every URL spelling observed for it, each of which
// gets a ppr_ tombstone when the search reports the PR merged.
type dashboardPRTarget struct {
	repo string
	urls []string
}

// pollRecentlyMergedPRs complements the individual gh-pr-view refresh path.
// One bulk search catches the common case (a recently merged PR authored by
// the authenticated gh user) within about ten seconds. It covers every PR the
// dashboard is showing as not-yet-terminal — explicitly presented agent_prs
// rows and branch/startup badges alike. Anything it cannot see — another
// author's PR, an unmerged close, a result outside the bounded 20–100 result
// page, or a failed search — remains covered by the existing per-PR resolver
// and the branch-link TTL refresh.
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
			if !ok || presentedPRRemotePolicyCheck(row.PRURL) != nil {
				continue
			}
			targets[ref.key()] = append(targets[ref.key()], row)
			reposByKey[strings.ToLower(ref.repo)] = ref.repo
		}
	}
	urlTargets := dashboardPRSearchTargets(time.Now())
	targetCount := len(targets)
	for key, t := range urlTargets {
		if presentedPRRemotePolicyCheck("https://github.com/"+t.repo+"/pull/1") != nil {
			continue
		}
		reposByKey[strings.ToLower(t.repo)] = t.repo
		if _, dup := targets[key]; !dup {
			targetCount++
		}
	}
	if targetCount == 0 {
		return false, nil
	}

	repos := make([]string, 0, len(reposByKey))
	for _, repo := range reposByKey {
		repos = append(repos, repo)
	}
	sort.Strings(repos)

	resultLimit := recentlyMergedPRResultLimit(targetCount)
	merged, err := recentlyMergedPRsResolver(repos, resultLimit)
	if err != nil {
		return true, fmt.Errorf("gh recently merged PR search: %w", err)
	}
	now := time.Now()
	cached := make(map[string]bool)
	cachePRState := func(rawURL string, info presentedPRInfo) {
		key := presentedPRCacheKey(rawURL)
		if cached[key] {
			return
		}
		cached[key] = true
		savePresentedPRCache(key, rawURL, info, now)
	}
	for _, info := range merged {
		ref, valid := githubPRRefFromURL(info.URL)
		if !valid {
			continue
		}
		rows := targets[ref.key()]
		urls := urlTargets[ref.key()].urls
		if len(rows) == 0 && len(urls) == 0 {
			continue
		}
		info.State = "merged"
		info.FetchedAt = now
		for _, row := range rows {
			if _, err := db.UpdateAgentPRState(row.AgentID, row.PRURL, info.State); err != nil {
				slog.Warn("presented-pr: failed to apply merged search result",
					"error", err, "agent_id", row.AgentID, "url", row.PRURL, "module", "agentd")
			}
			cachePRState(row.PRURL, info)
		}
		for _, u := range urls {
			cachePRState(u, info)
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
	// Canonical GitHub PR identity makes /pull/42 and /pull/42/files share the
	// same terminal-state tombstone. Non-GitHub URLs retain exact URL identity.
	h := sha256.Sum256([]byte("presented-pr\x00" + prStateKey(rawURL)))
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
	ref, ok := githubPRRefFromURL(rawURL)
	if !ok || rawURL != "https://github.com/"+ref.repo+"/pull/"+strconv.Itoa(ref.number) {
		return fmt.Errorf("PR URL must have the form https://github.com/<owner>/<repo>/pull/<number>")
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
	case "", "open", "draft", "merged", "closed", "handled":
		return state, nil
	default:
		return "", fmt.Errorf("PR state must be one of: open, draft, merged, closed, handled")
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
