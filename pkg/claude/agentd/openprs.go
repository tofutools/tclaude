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
	authoredOpenPRCachePrefix = "authored_open_prs_v1_"
	// This is the dashboard's one daemon-wide PR poll. It replaces the former
	// separate 10-second merged-PR search, so keep that faster cadence while
	// sharing its result across the footer and Groups surfaces.
	authoredOpenPRPollInterval = 10 * time.Second
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
//
// The open search is a shared fragment because BOTH query shapes below embed
// it, and the two-search shape is the one a default configuration actually
// runs — a second copy would silently drift the moment someone edited the
// rollup selection here.
const authoredOpenPRSearchFragment = `  search(query:$q,type:ISSUE,first:$first){
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
  }`

const authoredOpenPRGraphQLQuery = "query($q:String!,$first:Int!){\n" +
	authoredOpenPRSearchFragment + "\n}"

// authoredRecentPRGraphQLQuery adds a second aliased search when the "recently
// closed" window is enabled, so the poller still makes ONE GitHub request per
// tick. Closed pull requests deliberately carry no check rollup: CI state on
// something already merged or abandoned is noise, and the rollup is the
// expensive half of the response.
const authoredRecentPRGraphQLQuery = "query($q:String!,$first:Int!,$qr:String!,$rfirst:Int!){\n" +
	authoredOpenPRSearchFragment + `
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
	Local      bool             `json:"local,omitempty"`
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

// locallyKnownPR is a PR candidate learned from an agent's branch/startup
// links or an explicit presentation. Title is optional because branch-link
// caches currently carry state and identity but not the PR title.
type locallyKnownPR struct {
	URL   string
	Title string
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
	// RecentTruncated reports that GitHub matched more closed pull requests in
	// the window than authoredRecentPRLimit returned. The popover says so
	// rather than presenting the cap as a complete count — the search is
	// ordered by last update, so a truncated page is not even "the N most
	// recently closed".
	RecentTruncated bool `json:"recent_truncated,omitempty"`
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
	for range min(failures, 5) {
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
		state := "open"
		if view.Items[i].Draft {
			state = "draft"
		}
		savePresentedPRCache(presentedPRCacheKey(view.Items[i].URL), view.Items[i].URL, presentedPRInfo{
			Number: view.Items[i].Number, URL: view.Items[i].URL, State: state, FetchedAt: now,
		}, now)
		if view.Items[i].Checks == nil {
			continue
		}
		info := prChecksInfo{
			Summary:   *view.Items[i].Checks,
			PRState:   state,
			FetchedAt: now,
		}
		// The resolver saved the full check list already. Do not overwrite it
		// with this summary-only projection if that save was successful.
		if cached, ok := loadPRChecks(view.Items[i].URL); !ok || len(cached.Checks) == 0 {
			savePRChecks(view.Items[i].URL, info)
		}
	}
	for i := range view.Recent {
		savePresentedPRCache(presentedPRCacheKey(view.Recent[i].URL), view.Recent[i].URL, presentedPRInfo{
			Number: view.Recent[i].Number, URL: view.Recent[i].URL,
			State: view.Recent[i].State, FetchedAt: now,
		}, now)
	}
	applyAuthoredStatesToPresentedPRs(view)
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

// applyAuthoredStatesToPresentedPRs keeps the durable presented-PR lifecycle
// aligned with the shared observation cache. Terminal observations start the
// badge's expiry grace period; open/draft observations also matter because a
// closed (but not merged) GitHub PR can be reopened. UpdateAgentPRState owns
// the race guards, including the rule that merged can never regress.
func applyAuthoredStatesToPresentedPRs(view dashboardAuthoredOpenPRs) {
	observed := make(map[string]string, len(view.Items)+len(view.Recent))
	for _, pr := range view.Items {
		state := "open"
		if pr.Draft {
			state = "draft"
		}
		observed[prStateKey(pr.URL)] = state
	}
	for _, pr := range view.Recent {
		if isTerminalPresentedPRState(pr.State) {
			observed[prStateKey(pr.URL)] = strings.ToLower(strings.TrimSpace(pr.State))
		}
	}
	if len(observed) == 0 {
		return
	}
	all, err := listVisiblePresentedPRs()
	if err != nil {
		slog.Warn("open-prs: failed to reconcile presented PR rows", "error", err, "module", "agentd")
		return
	}
	for _, rows := range all {
		for _, row := range rows {
			state, ok := observed[prStateKey(row.PRURL)]
			if !ok || strings.EqualFold(row.State, state) {
				continue
			}
			if _, err := db.UpdateAgentPRState(row.AgentID, row.PRURL, state); err != nil {
				slog.Warn("open-prs: failed to apply state to presented PR",
					"error", err, "agent_id", row.AgentID, "url", row.PRURL,
					"state", state, "module", "agentd")
			}
		}
	}
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
	// Even when the footer's recent filter is disabled, fetch one day of
	// terminal authored PRs. Groups uses the same response to retire merged
	// badges, replacing its former independent GitHub Search poll.
	fetchWindowDays := max(windowDays, 1)
	out, err := runInDirWithError("", "gh", authoredPRSearchArgs(login, fetchWindowDays, time.Now())...)
	if err != nil {
		return dashboardAuthoredOpenPRs{}, err
	}
	view, checksByURL, err := decodeAuthoredOpenPRGraphQL([]byte(out), login)
	if err != nil {
		return dashboardAuthoredOpenPRs{}, err
	}
	view.RecentWindowDays = fetchWindowDays
	for rawURL, info := range checksByURL {
		savePRChecks(rawURL, info)
	}
	return view, nil
}

// authoredPRSearchArgs builds the `gh api graphql` argv for one poll. It is
// separated from the subprocess call so the query selection, the search
// qualifiers and the date arithmetic are unit-testable — a typo in `qr=`
// would otherwise only show up as a silently empty recent list in production.
// Every value lands in argv, never in a shell string.
func authoredPRSearchArgs(login string, windowDays int, now time.Time) []string {
	query := authoredOpenPRGraphQLQuery
	extra := []string(nil)
	if windowDays > 0 {
		// GitHub's `closed:` qualifier is whole-UTC-day granularity, so the
		// bound is a date. The snapshot re-filters to the exact instant window
		// (filterRecentAuthoredPRs), which keeps a same-day close visible
		// without this query having to reason about hours.
		since := now.UTC().AddDate(0, 0, -windowDays).Format("2006-01-02")
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
	return append(args, extra...)
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
	view.RecentTruncated = payload.Data.Recent.IssueCount > authoredRecentPRLimit
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
		case "passing":
			return 2
		case "pending":
			// A clean run in flight is the one state that does not need
			// operator attention yet, so keep it behind actionable PRs.
			return 3
		}
	}
	return 1
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

// addAuthoredOpenPRStates folds the daemon-wide GitHub search into the same
// per-PR observation index used by branch, startup and presented badges. The
// search fetch time is the observation time; a PR's updatedAt is content
// activity and cannot be used to decide which poll saw its state last.
func addAuthoredOpenPRStates(idx prStateIndex, view dashboardAuthoredOpenPRs) {
	fetchedAt, _ := time.Parse(time.RFC3339, strings.TrimSpace(view.UpdatedAt))
	for _, pr := range view.Items {
		state := "open"
		if pr.Draft {
			state = "draft"
		}
		idx.add(pr.URL, state, fetchedAt)
	}
	for _, pr := range view.Recent {
		idx.add(pr.URL, pr.State, fetchedAt)
	}
}

// reconcileAuthoredOpenPRs projects the shared per-PR state/check caches back
// onto the footer list. Groups and the footer therefore render one observation
// for an identity on every 2-second snapshot, even between GitHub poll ticks.
// If another shared writer observes a merge first, move that cached open item
// to Recent immediately instead of leaving it in two contradictory surfaces.
func reconcileAuthoredOpenPRs(
	view dashboardAuthoredOpenPRs,
	states prStateIndex,
	checks map[string]*prChecksSummary,
	now time.Time,
) dashboardAuthoredOpenPRs {
	recentByKey := make(map[string]int, len(view.Recent))
	for i := range view.Recent {
		recentByKey[prStateKey(view.Recent[i].URL)] = i
	}
	open := make([]dashboardAuthoredOpenPR, 0, len(view.Items))
	for _, pr := range view.Items {
		key := prStateKey(pr.URL)
		if summary := checks[key]; summary != nil {
			copy := *summary
			pr.Checks = &copy
		}
		observation, known := states[key]
		if !known || !isTerminalPresentedPRState(observation.state) {
			if known {
				pr.Draft = strings.EqualFold(observation.state, "draft")
			}
			open = append(open, pr)
			continue
		}

		view.Total = max(0, view.Total-1)
		if view.RecentWindowDays <= 0 || observation.updatedAt.IsZero() ||
			observation.updatedAt.Before(now.Add(-time.Duration(view.RecentWindowDays)*24*time.Hour)) {
			continue
		}
		pr.State = strings.ToLower(observation.state)
		pr.ClosedAt = observation.updatedAt.Format(time.RFC3339)
		if i, exists := recentByKey[key]; exists {
			view.Recent[i] = pr
			continue
		}
		recentByKey[key] = len(view.Recent)
		view.Recent = append(view.Recent, pr)
	}
	view.Items = open

	for i := range view.Recent {
		key := prStateKey(view.Recent[i].URL)
		if observation, ok := states[key]; ok && isTerminalPresentedPRState(observation.state) {
			view.Recent[i].State = strings.ToLower(observation.state)
		}
		if summary := checks[key]; summary != nil {
			copy := *summary
			view.Recent[i].Checks = &copy
		}
	}
	view.Recent = filterRecentAuthoredPRs(view.Recent, view.RecentWindowDays, now)
	sort.SliceStable(view.Recent, func(i, j int) bool {
		return view.Recent[i].ClosedAt > view.Recent[j].ClosedAt
	})
	return view
}

// unionLocallyKnownOpenPRs adds open PRs learned from agent activity that the
// authored-PR search has not indexed yet. It only projects already-loaded
// snapshot caches; callers must not perform I/O to assemble its inputs.
func unionLocallyKnownOpenPRs(
	view dashboardAuthoredOpenPRs,
	localPRs []locallyKnownPR,
	states prStateIndex,
	checks map[string]*prChecksSummary,
) dashboardAuthoredOpenPRs {
	if !view.Available {
		return view
	}

	known := make(map[string]struct{}, len(view.Items)+len(view.Recent))
	for _, list := range [][]dashboardAuthoredOpenPR{view.Items, view.Recent} {
		for _, pr := range list {
			if key := prStateKey(pr.URL); key != "" {
				known[key] = struct{}{}
			}
		}
	}

	// Retain the first locally observed URL for deterministic output, but let a
	// later source supply the title when the branch cache had none.
	candidates := make(map[string]locallyKnownPR, len(localPRs))
	order := make([]string, 0, len(localPRs))
	for _, candidate := range localPRs {
		ref, ok := githubPRRefFromURL(candidate.URL)
		if !ok {
			continue
		}
		key := "github:" + ref.key()
		if _, exists := candidates[key]; !exists {
			candidates[key] = candidate
			order = append(order, key)
			continue
		}
		if current := candidates[key]; truncateAuthoredPRTitle(current.Title) == "" &&
			truncateAuthoredPRTitle(candidate.Title) != "" {
			current.Title = candidate.Title
			candidates[key] = current
		}
	}

	for _, key := range order {
		if _, exists := known[key]; exists {
			continue
		}
		candidate := candidates[key]
		ref, ok := githubPRRefFromURL(candidate.URL)
		if !ok {
			continue
		}
		observation, ok := states[key]
		if !ok || (!strings.EqualFold(observation.state, "open") &&
			!strings.EqualFold(observation.state, "draft")) {
			continue
		}

		title := truncateAuthoredPRTitle(candidate.Title)
		if title == "" {
			title = ref.repo + "#" + strconv.Itoa(ref.number)
		}
		pr := dashboardAuthoredOpenPR{
			Number: ref.number, Repository: ref.repo,
			URL:   "https://github.com/" + ref.repo + "/pull/" + strconv.Itoa(ref.number),
			Title: title, Local: true,
			Draft: strings.EqualFold(observation.state, "draft"),
		}
		if !observation.updatedAt.IsZero() {
			pr.UpdatedAt = observation.updatedAt.Format(time.RFC3339)
		}
		if summary := checks[key]; summary != nil {
			copy := *summary
			pr.Checks = &copy
		}
		view.Items = append(view.Items, pr)
		view.Total++
		known[key] = struct{}{}
	}

	sort.SliceStable(view.Items, func(i, j int) bool {
		ri, rj := authoredOpenPRAttentionRank(view.Items[i]), authoredOpenPRAttentionRank(view.Items[j])
		if ri != rj {
			return ri < rj
		}
		return view.Items[i].UpdatedAt > view.Items[j].UpdatedAt
	})
	return view
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
