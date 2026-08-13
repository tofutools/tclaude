package agentd

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

const (
	authoredOpenPRCacheKey     = "authored_open_prs_v1"
	authoredOpenPRPollInterval = 30 * time.Second
	authoredOpenPRMaxBackoff   = 5 * time.Minute
	authoredOpenPRLimit        = 100
	authoredOpenPRTitleMax     = 240
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
        commits(last:1){nodes{commit{statusCheckRollup{contexts(first:100){nodes{
          __typename
          ... on CheckRun{name status conclusion detailsUrl startedAt completedAt}
          ... on StatusContext{context state targetUrl description createdAt}
        }}}}}}
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
}

type dashboardAuthoredOpenPRs struct {
	Available bool                      `json:"available"`
	Login     string                    `json:"login,omitempty"`
	SearchURL string                    `json:"search_url,omitempty"`
	UpdatedAt string                    `json:"updated_at,omitempty"`
	Total     int                       `json:"total"`
	Truncated bool                      `json:"truncated,omitempty"`
	Items     []dashboardAuthoredOpenPR `json:"items"`
}

var authoredOpenPRResolver = liveAuthoredOpenPRResolver

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
	data, err := json.Marshal(view)
	if err != nil {
		return fmt.Errorf("encode authored open PR cache: %w", err)
	}
	if err := db.SaveGitCache(authoredOpenPRCacheKey, data, now); err != nil {
		return fmt.Errorf("save authored open PR cache: %w", err)
	}
	return nil
}

func liveAuthoredOpenPRResolver() (dashboardAuthoredOpenPRs, error) {
	login, ok := cachedGitHubLogin()
	if !ok {
		return dashboardAuthoredOpenPRs{}, fmt.Errorf("resolve active GitHub login")
	}
	args := []string{
		"api", "--hostname", "github.com", "graphql",
		"-f", "query=" + authoredOpenPRGraphQLQuery,
		"-f", "q=author:" + login + " is:open type:pr sort:updated-desc",
		"-F", "first=" + strconv.Itoa(authoredOpenPRLimit),
	}
	out, err := runInDirWithError("", "gh", args...)
	if err != nil {
		return dashboardAuthoredOpenPRs{}, err
	}
	view, checksByURL, err := decodeAuthoredOpenPRGraphQL([]byte(out), login)
	if err != nil {
		return dashboardAuthoredOpenPRs{}, err
	}
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
									Contexts struct {
										Nodes []json.RawMessage `json:"nodes"`
									} `json:"contexts"`
								} `json:"statusCheckRollup"`
							} `json:"commit"`
						} `json:"nodes"`
					} `json:"commits"`
				} `json:"nodes"`
			} `json:"search"`
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
	}
	checksByURL := make(map[string]prChecksInfo)
	for _, node := range payload.Data.Search.Nodes {
		ref, ok := githubPRRefFromURL(node.URL)
		if !ok || node.Number <= 0 || ref.number != node.Number {
			continue
		}
		title := strings.TrimSpace(node.Title)
		if len(title) > authoredOpenPRTitleMax {
			title = title[:authoredOpenPRTitleMax] + "…"
		}
		item := dashboardAuthoredOpenPR{
			Number: node.Number, URL: strings.TrimSpace(node.URL), Title: title,
			Repository: ref.repo, Draft: node.IsDraft, UpdatedAt: node.UpdatedAt,
		}
		if len(node.Commits.Nodes) > 0 && node.Commits.Nodes[0].Commit.StatusCheckRollup != nil {
			raw, _ := json.Marshal(node.Commits.Nodes[0].Commit.StatusCheckRollup.Contexts.Nodes)
			info := parseStatusCheckRollup(raw, now)
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
	return view, checksByURL, nil
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

func loadAuthoredOpenPRsSnapshot() dashboardAuthoredOpenPRs {
	empty := dashboardAuthoredOpenPRs{Items: []dashboardAuthoredOpenPR{}}
	row, err := db.LoadGitCache(authoredOpenPRCacheKey)
	if err != nil || row == nil || json.Unmarshal(row.Data, &empty) != nil {
		return dashboardAuthoredOpenPRs{Items: []dashboardAuthoredOpenPR{}}
	}
	if empty.Items == nil {
		empty.Items = []dashboardAuthoredOpenPR{}
	}
	return empty
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
	for i := range view.Items {
		if ref, ok := byPR[prStateKey(view.Items[i].URL)]; ok {
			view.Items[i].AgentID = ref.id
			view.Items[i].AgentTitle = ref.title
		}
	}
	return view
}
