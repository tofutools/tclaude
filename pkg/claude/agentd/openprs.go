package agentd

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

const (
	authoredOpenPRCachePrefix  = "authored_open_prs_v1_"
	authoredOpenPRPollInterval = 30 * time.Second
	authoredOpenPRMaxBackoff   = 5 * time.Minute
	authoredOpenPRLimit        = 100
	authoredOpenPRTitleMax     = 240
	// authoredRecentPRLimit bounds the second (recently closed/merged) search.
	// The recent list is a "what did I just land" glance, not an archive, so it
	// stays well below the open-PR page size.
	authoredRecentPRLimit = 50
)

// One GraphQL search returns the operator's cross-repository PR list and the
// head commit's check rollup. That keeps this daemon-wide feature at one
// GitHub request per poll instead of one gh subprocess per open PR.
const authoredOpenPRGraphQLQuery = `query($q:String!,$first:Int!){
  search(query:$q,type:ISSUE,first:$first){
    issueCount
    nodes{
      ... on PullRequest{
        number title url isDraft updatedAt
        repository{nameWithOwner}
        commits(last:1){nodes{commit{statusCheckRollup{state contexts(first:100){totalCount nodes{
          __typename
          ... on CheckRun{name status conclusion detailsUrl startedAt completedAt}
          ... on StatusContext{context state targetUrl description createdAt}
        }}}}}}
      }
    }
  }
}`

// authoredRecentPRGraphQLQuery is appended as a second aliased search when the
// "recently closed" window is enabled, so the poller still makes ONE GitHub
// request per tick. Closed pull requests deliberately carry no check rollup:
// CI state on something already merged or abandoned is noise, and the rollup
// is the expensive half of the response.
const authoredRecentPRGraphQLQuery = `query($q:String!,$first:Int!,$qr:String!,$rfirst:Int!){
  search(query:$q,type:ISSUE,first:$first){
    issueCount
    nodes{
      ... on PullRequest{
        number title url isDraft updatedAt
        repository{nameWithOwner}
        commits(last:1){nodes{commit{statusCheckRollup{state contexts(first:100){totalCount nodes{
          __typename
          ... on CheckRun{name status conclusion detailsUrl startedAt completedAt}
          ... on StatusContext{context state targetUrl description createdAt}
        }}}}}}
      }
    }
  }
  recent: search(query:$qr,type:ISSUE,first:$rfirst){
    issueCount
    nodes{
      ... on PullRequest{
        number title url isDraft updatedAt state mergedAt closedAt
        repository{nameWithOwner}
      }
    }
  }
}`

type dashboardAuthoredOpenPR struct {
	Number     int              `json:"number"`
	URL        string           `json:"url"`
	Title      string           `json:"title"`
	Repository string           `json:"repository"`
	Draft      bool             `json:"draft,omitempty"`
	UpdatedAt  string           `json:"updated_at,omitempty"`
	Checks     *prChecksSummary `json:"checks,omitempty"`
	AgentID    string           `json:"agent_id,omitempty"`
	AgentTitle string           `json:"agent_title,omitempty"`
	// State and ClosedAt are populated only for the recently closed list:
	// "merged" or "closed", and the RFC3339 instant it reached that state.
	// Items in the open list leave both empty.
	State    string `json:"state,omitempty"`
	ClosedAt string `json:"closed_at,omitempty"`
}

type dashboardAuthoredOpenPRs struct {
	Available bool                      `json:"available"`
	Login     string                    `json:"login,omitempty"`
	SearchURL string                    `json:"search_url,omitempty"`
	UpdatedAt string                    `json:"updated_at,omitempty"`
	Total     int                       `json:"total"`
	Truncated bool                      `json:"truncated,omitempty"`
	Items     []dashboardAuthoredOpenPR `json:"items"`
	// AlwaysShow mirrors config dashboard.always_show_open_prs. It is
	// resolved when the SNAPSHOT is built, not when the cache is written, so
	// toggling the knob takes effect on the next dashboard poll instead of
	// waiting for the next GitHub search.
	AlwaysShow bool `json:"always_show"`
	// Recent carries pull requests merged or closed inside the configured
	// window (dashboard.recent_pr_window_days), newest first. Empty when the
	// window is 0 (filter disabled). It is a separate list, never merged into
	// Items, so the open-PR count and filters stay about open work.
	Recent []dashboardAuthoredOpenPR `json:"recent"`
	// RecentWindowDays is the resolved lookback the Recent list was built
	// with. 0 means the "recently closed" filter is off.
	RecentWindowDays int `json:"recent_window_days"`
	// RecentSearchURL is the GitHub search escape hatch for the recent list,
	// mirroring SearchURL for the open one.
	RecentSearchURL string `json:"recent_search_url,omitempty"`
}

var authoredOpenPRResolver = liveAuthoredOpenPRResolver

// authoredOpenPRActiveLogin is deliberately process-local. A daemon restart
// publishes no durable PR cache until this process has successfully resolved
// the active credential and refreshed it; that prevents a previous gh user's
// private titles/repositories leaking after an auth switch or token change.
var authoredOpenPRActiveLogin struct {
	sync.RWMutex
	login string
}

func startAuthoredOpenPRPoller(stop <-chan struct{}) {
	go func() {
		timer := time.NewTimer(0)
		defer timer.Stop()
		failures := 0
		for {
			select {
			case <-stop:
				return
			case <-timer.C:
				err := pollAuthoredOpenPRs()
				delay := authoredOpenPRPollInterval
				if err != nil {
					failures++
					delay = authoredOpenPRRetryDelay(failures)
					if failures == 1 || delay > authoredOpenPRRetryDelay(failures-1) {
						slog.Warn("open-prs: GitHub search failed; keeping last good cache",
							"error", err, "consecutive_failures", failures,
							"retry_in", delay, "module", "agentd")
					}
				} else if failures > 0 {
					slog.Info("open-prs: GitHub search recovered",
						"previous_failures", failures, "module", "agentd")
					failures = 0
				}
				timer.Reset(delay)
			}
		}
	}()
}

func authoredOpenPRRetryDelay(failures int) time.Duration {
	if failures <= 0 {
		return authoredOpenPRPollInterval
	}
	delay := authoredOpenPRPollInterval
	for range min(failures, 4) {
		delay *= 2
	}
	return min(delay, authoredOpenPRMaxBackoff)
}

func pollAuthoredOpenPRs() error {
	view, err := authoredOpenPRResolver()
	if err != nil {
		return err
	}
	now := time.Now()
	view.Available = true
	view.UpdatedAt = now.Format(time.RFC3339)
	if view.Items == nil {
		view.Items = []dashboardAuthoredOpenPR{}
	}
	for i := range view.Items {
		if view.Items[i].Checks == nil {
			continue
		}
		info := prChecksInfo{
			Summary:   *view.Items[i].Checks,
			PRState:   "open",
			FetchedAt: now,
		}
		// The resolver saved the full check list already. Do not overwrite it
		// with this summary-only projection if that save was successful.
		if cached, ok := loadPRChecks(view.Items[i].URL); !ok || len(cached.Checks) == 0 {
			savePRChecks(view.Items[i].URL, info)
		}
	}
	if !isGitHubOwnerSlug(view.Login) {
		return fmt.Errorf("resolver returned invalid GitHub login")
	}
	data, err := json.Marshal(view)
	if err != nil {
		return fmt.Errorf("encode authored open PR cache: %w", err)
	}
	if err := db.SaveGitCache(authoredOpenPRCacheKey(view.Login), data, now); err != nil {
		return fmt.Errorf("save authored open PR cache: %w", err)
	}
	setAuthoredOpenPRActiveLogin(view.Login)
	return nil
}

func liveAuthoredOpenPRResolver() (dashboardAuthoredOpenPRs, error) {
	login, ok := cachedGitHubLogin()
	if !ok {
		setAuthoredOpenPRActiveLogin("")
		return dashboardAuthoredOpenPRs{}, fmt.Errorf("resolve active GitHub login")
	}
	// Switch the snapshot's cache namespace as soon as the local credential
	// identity is known, before the network search. If the request then fails,
	// the old identity's private metadata is already hidden; a prior cache for
	// this same identity may still be served while the poller retries.
	setAuthoredOpenPRActiveLogin(login)
	cfg, _ := config.Load()
	windowDays := cfg.RecentPRWindowDays()
	query := authoredOpenPRGraphQLQuery
	extra := []string(nil)
	if windowDays > 0 {
		// GitHub's `closed:` qualifier is whole-UTC-day granularity, so the
		// bound is a date. The snapshot re-filters to the exact instant window
		// (recentPRCutoff), which keeps a same-day close visible without this
		// query having to reason about hours.
		since := time.Now().UTC().AddDate(0, 0, -windowDays).Format("2006-01-02")
		query = authoredRecentPRGraphQLQuery
		extra = []string{
			"-f", "qr=author:" + login + " is:closed type:pr closed:>=" + since + " sort:updated-desc",
			"-F", "rfirst=" + strconv.Itoa(authoredRecentPRLimit),
		}
	}
	args := []string{
		"api", "--hostname", "github.com", "graphql",
		"-f", "query=" + query,
		"-f", "q=author:" + login + " is:open type:pr sort:updated-desc",
		"-F", "first=" + strconv.Itoa(authoredOpenPRLimit),
	}
	args = append(args, extra...)
	out, err := runInDirWithError("", "gh", args...)
	if err != nil {
		return dashboardAuthoredOpenPRs{}, err
	}
	view, checksByURL, err := decodeAuthoredOpenPRGraphQL([]byte(out), login)
	if err != nil {
		return dashboardAuthoredOpenPRs{}, err
	}
	view.RecentWindowDays = windowDays
	for rawURL, info := range checksByURL {
		savePRChecks(rawURL, info)
	}
	return view, nil
}

func decodeAuthoredOpenPRGraphQL(data []byte, login string) (dashboardAuthoredOpenPRs, map[string]prChecksInfo, error) {
	var payload struct {
		Data struct {
			Search struct {
				IssueCount int `json:"issueCount"`
				Nodes      []struct {
					Number     int    `json:"number"`
					Title      string `json:"title"`
					URL        string `json:"url"`
					IsDraft    bool   `json:"isDraft"`
					UpdatedAt  string `json:"updatedAt"`
					Repository struct {
						NameWithOwner string `json:"nameWithOwner"`
					} `json:"repository"`
					Commits struct {
						Nodes []struct {
							Commit struct {
								StatusCheckRollup *struct {
									State    string `json:"state"`
									Contexts struct {
										TotalCount int               `json:"totalCount"`
										Nodes      []json.RawMessage `json:"nodes"`
									} `json:"contexts"`
								} `json:"statusCheckRollup"`
							} `json:"commit"`
						} `json:"nodes"`
					} `json:"commits"`
				} `json:"nodes"`
			} `json:"search"`
			Recent struct {
				IssueCount int `json:"issueCount"`
				Nodes      []struct {
					Number     int    `json:"number"`
					Title      string `json:"title"`
					URL        string `json:"url"`
					IsDraft    bool   `json:"isDraft"`
					UpdatedAt  string `json:"updatedAt"`
					State      string `json:"state"`
					MergedAt   string `json:"mergedAt"`
					ClosedAt   string `json:"closedAt"`
					Repository struct {
						NameWithOwner string `json:"nameWithOwner"`
					} `json:"repository"`
				} `json:"nodes"`
			} `json:"recent"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return dashboardAuthoredOpenPRs{}, nil, fmt.Errorf("decode authored open PR search: %w", err)
	}
	if len(payload.Errors) > 0 {
		return dashboardAuthoredOpenPRs{}, nil, fmt.Errorf("authored open PR search: %s", payload.Errors[0].Message)
	}
	now := time.Now()
	view := dashboardAuthoredOpenPRs{
		Available: true,
		Login:     login,
		SearchURL: "https://github.com/pulls?q=" + url.QueryEscape("is:open is:pr author:"+login),
		Total:     payload.Data.Search.IssueCount,
		Truncated: payload.Data.Search.IssueCount > authoredOpenPRLimit,
		Items:     []dashboardAuthoredOpenPR{},
		Recent:    []dashboardAuthoredOpenPR{},
	}
	checksByURL := make(map[string]prChecksInfo)
	for _, node := range payload.Data.Search.Nodes {
		ref, ok := githubPRRefFromURL(node.URL)
		if !ok || node.Number <= 0 || ref.number != node.Number {
			continue
		}
		item := dashboardAuthoredOpenPR{
			Number: node.Number, URL: strings.TrimSpace(node.URL), Title: truncateAuthoredPRTitle(node.Title),
			Repository: ref.repo, Draft: node.IsDraft, UpdatedAt: node.UpdatedAt,
		}
		if len(node.Commits.Nodes) > 0 && node.Commits.Nodes[0].Commit.StatusCheckRollup != nil {
			raw, _ := json.Marshal(node.Commits.Nodes[0].Commit.StatusCheckRollup.Contexts.Nodes)
			info := parseStatusCheckRollup(raw, now)
			reconcileTruncatedPRChecks(&info.Summary,
				node.Commits.Nodes[0].Commit.StatusCheckRollup.State,
				node.Commits.Nodes[0].Commit.StatusCheckRollup.Contexts.TotalCount)
			info.PRState = "open"
			item.Checks = &info.Summary
			checksByURL[item.URL] = info
		}
		view.Items = append(view.Items, item)
	}
	sort.SliceStable(view.Items, func(i, j int) bool {
		ri, rj := authoredOpenPRAttentionRank(view.Items[i]), authoredOpenPRAttentionRank(view.Items[j])
		if ri != rj {
			return ri < rj
		}
		return view.Items[i].UpdatedAt > view.Items[j].UpdatedAt
	})
	for _, node := range payload.Data.Recent.Nodes {
		ref, ok := githubPRRefFromURL(node.URL)
		if !ok || node.Number <= 0 || ref.number != node.Number {
			continue
		}
		state := "closed"
		closedAt := strings.TrimSpace(node.ClosedAt)
		if strings.EqualFold(strings.TrimSpace(node.State), "MERGED") {
			state = "merged"
			if merged := strings.TrimSpace(node.MergedAt); merged != "" {
				closedAt = merged
			}
		}
		view.Recent = append(view.Recent, dashboardAuthoredOpenPR{
			Number: node.Number, URL: strings.TrimSpace(node.URL),
			Title: truncateAuthoredPRTitle(node.Title), Repository: ref.repo,
			Draft: node.IsDraft, UpdatedAt: node.UpdatedAt,
			State: state, ClosedAt: closedAt,
		})
	}
	// Newest first: the recent list answers "what did I just land", so recency
	// is the only useful order — unlike the open list, nothing here needs
	// attention ranking.
	sort.SliceStable(view.Recent, func(i, j int) bool {
		return view.Recent[i].ClosedAt > view.Recent[j].ClosedAt
	})
	return view, checksByURL, nil
}

// truncateAuthoredPRTitle bounds a PR title before it reaches the snapshot,
// so one pathological title cannot bloat every dashboard poll.
func truncateAuthoredPRTitle(raw string) string {
	title := strings.TrimSpace(raw)
	if len(title) > authoredOpenPRTitleMax {
		return title[:authoredOpenPRTitleMax] + "…"
	}
	return title
}

func authoredOpenPRAttentionRank(pr dashboardAuthoredOpenPR) int {
	if pr.Checks != nil {
		switch pr.Checks.State {
		case "failing":
			return 0
		case "pending":
			return 1
		case "passing":
			return 3
		}
	}
	return 2
}

func reconcileTruncatedPRChecks(summary *prChecksSummary, rollupState string, total int) {
	if summary == nil || total <= summary.Total {
		return
	}
	summary.Total = total
	known := summary.Passed + summary.Failed + summary.Pending + summary.Skipped
	missing := max(0, total-known)
	switch strings.ToUpper(strings.TrimSpace(rollupState)) {
	case "FAILURE", "ERROR":
		summary.State = "failing"
		if summary.Failed == 0 {
			summary.Failed = 1
			missing--
		}
	case "PENDING", "EXPECTED":
		summary.State = "pending"
		summary.Pending += missing
		missing = 0
	case "SUCCESS":
		summary.State = "passing"
		summary.Passed += missing
		missing = 0
	default:
		// An unknown aggregate cannot honestly turn a partial page green.
		summary.State = "pending"
		summary.Pending += missing
		missing = 0
	}
	if missing > 0 {
		summary.Pending += missing
	}
}

func loadAuthoredOpenPRsSnapshot(cfg *config.Config) dashboardAuthoredOpenPRs {
	blank := func() dashboardAuthoredOpenPRs {
		return dashboardAuthoredOpenPRs{
			Items:      []dashboardAuthoredOpenPR{},
			Recent:     []dashboardAuthoredOpenPR{},
			AlwaysShow: cfg.AlwaysShowOpenPRs(),
		}
	}
	view := blank()
	authoredOpenPRActiveLogin.RLock()
	login := authoredOpenPRActiveLogin.login
	authoredOpenPRActiveLogin.RUnlock()
	if !isGitHubOwnerSlug(login) {
		return view
	}
	row, err := db.LoadGitCache(authoredOpenPRCacheKey(login))
	if err != nil || row == nil || json.Unmarshal(row.Data, &view) != nil {
		return blank()
	}
	if !strings.EqualFold(view.Login, login) {
		return blank()
	}
	if view.Items == nil {
		view.Items = []dashboardAuthoredOpenPR{}
	}
	// The two knobs are resolved here rather than at poll time so a config
	// change lands on the next 2-second dashboard poll instead of waiting for
	// the next GitHub search. Narrowing the window therefore takes effect
	// immediately; widening it still needs a poll to fetch the older PRs.
	view.AlwaysShow = cfg.AlwaysShowOpenPRs()
	// A widened window cannot be honoured from a cache fetched under the old
	// one, so the snapshot advertises the narrower of the two until the poller
	// catches up — better than promising 14 days and listing 3.
	windowDays := min(cfg.RecentPRWindowDays(), view.RecentWindowDays)
	view.RecentWindowDays = windowDays
	view.Recent = filterRecentAuthoredPRs(view.Recent, windowDays, time.Now())
	view.RecentSearchURL = ""
	if windowDays > 0 {
		view.RecentSearchURL = "https://github.com/pulls?q=" + url.QueryEscape(
			"is:closed is:pr author:"+login+" closed:>="+
				time.Now().UTC().AddDate(0, 0, -windowDays).Format("2006-01-02"))
	}
	return view
}

// filterRecentAuthoredPRs drops cached entries that have aged out of (or were
// never inside) the configured window. A cached list can outlive a narrowed
// window by up to one poll interval, and GitHub's date-granular `closed:`
// bound is deliberately generous, so the snapshot enforces the exact instant.
func filterRecentAuthoredPRs(items []dashboardAuthoredOpenPR, windowDays int, now time.Time) []dashboardAuthoredOpenPR {
	out := []dashboardAuthoredOpenPR{}
	if windowDays <= 0 {
		return out
	}
	cutoff := now.Add(-time.Duration(windowDays) * 24 * time.Hour)
	for _, item := range items {
		closed, err := time.Parse(time.RFC3339, strings.TrimSpace(item.ClosedAt))
		if err != nil || closed.Before(cutoff) {
			continue
		}
		out = append(out, item)
	}
	return out
}

func authoredOpenPRCacheKey(login string) string {
	return authoredOpenPRCachePrefix + strings.ToLower(strings.TrimSpace(login))
}

func setAuthoredOpenPRActiveLogin(login string) {
	authoredOpenPRActiveLogin.Lock()
	authoredOpenPRActiveLogin.login = strings.TrimSpace(login)
	authoredOpenPRActiveLogin.Unlock()
}

func associateAuthoredOpenPRs(view dashboardAuthoredOpenPRs, agents []dashboardAgent) dashboardAuthoredOpenPRs {
	type agentRef struct{ id, title string }
	byPR := make(map[string]agentRef)
	for _, agent := range agents {
		ref := agentRef{id: agent.AgentID, title: agent.Title}
		for _, rawURL := range []string{agent.BranchPRURL, agent.StartupPRURL} {
			if key := prStateKey(rawURL); key != "" {
				byPR[key] = ref
			}
		}
		for _, pr := range agent.PresentedPRs {
			if key := prStateKey(pr.URL); key != "" {
				byPR[key] = ref
			}
		}
	}
	for _, list := range [][]dashboardAuthoredOpenPR{view.Items, view.Recent} {
		for i := range list {
			if ref, ok := byPR[prStateKey(list[i].URL)]; ok {
				list[i].AgentID = ref.id
				list[i].AgentTitle = ref.title
			}
		}
	}
	return view
}
