package agentd

// openmrs.go — the footer's Open MRs indicator, the GitLab counterpart of
// Open PRs (openprs.go).
//
// One background poller asks every GitLab instance the operator configured in
// agent.git_proxy.gitlab_hosts for the token user's open merge requests, in a
// single GraphQL query per instance that also carries each request's head
// pipeline, approvals and merge status. The merged list lives in memory only:
// the 2-second snapshot copies it and matches it to agents, and a restarted
// daemon simply asks again.
//
// The list is shaped like the Open PRs list (dashboardAuthoredOpenPRs), so the
// footer's filter model reads it unchanged:
//
//   - checks carry the head pipeline's status as ONE pipeline-level entry
//     (passing, failing or pending), not per-job counts;
//   - reviews count real approvals (approvedBy), and one change request when
//     GitLab reports the merge request blocked on requested changes. GitLab's
//     own "approved" flag is deliberately not used: it is true with zero
//     approvals on a project that requires none;
//   - conflicts flags a merge conflict, which always needs attention.
//
// It shows only when gitlab_hosts names an instance and the token resolves a
// user on at least one of them; a gitlab.com-only setup with no gitlab_hosts
// entry does not show it.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/config"
)

const (
	authoredOpenMRPollInterval = 30 * time.Second
	authoredOpenMRMaxBackoff   = 5 * time.Minute
	// authoredOpenMRLimit is the page the query asks for per instance. The
	// popover says "Showing the first N" when an instance has more.
	authoredOpenMRLimit = 100
	// authoredOpenMRTimeout bounds one instance's query.
	authoredOpenMRTimeout = 20 * time.Second
	// authoredOpenMRDefaultRetry is the wait after a 429 that named none.
	authoredOpenMRDefaultRetry = time.Minute
)

// authoredOpenMRQuery is the one query the poller sends. It is a constant;
// the only value it takes travels as a variable.
const authoredOpenMRQuery = `query($first: Int!) {
  currentUser {
    username
    authoredMergeRequests(state: opened, first: $first) {
      count
      nodes {
        iid
        title
        webUrl
        draft
        updatedAt
        conflicts
        detailedMergeStatus
        headPipeline { status }
        approvedBy { nodes { username } }
      }
    }
  }
}`

// dashboardAuthoredOpenMRInstance is one configured instance as the popover
// footer shows it: the "Open all on <host>" link, or why the instance could
// not be read.
type dashboardAuthoredOpenMRInstance struct {
	Base     string `json:"base"`
	Host     string `json:"host"`
	Username string `json:"username,omitempty"`
	// URL is the instance's own list of the user's open merge requests.
	URL   string `json:"url,omitempty"`
	Error string `json:"error,omitempty"`
}

// dashboardAuthoredOpenMRs is the snapshot's authored_open_mrs: the Open PRs
// shape plus the per-instance links.
type dashboardAuthoredOpenMRs struct {
	dashboardAuthoredOpenPRs
	Instances []dashboardAuthoredOpenMRInstance `json:"instances"`
}

// authoredOpenMRState is the poller's last result. key names the configuration
// it was read under (the instances and the token file); a snapshot built under
// any other configuration shows nothing rather than a list the current
// credential never produced.
var authoredOpenMRState struct {
	sync.RWMutex
	key       string
	updatedAt time.Time
	instances []dashboardAuthoredOpenMRInstance
	// good is each instance's last successful answer, kept through a failed
	// poll so one bad response does not empty the popover.
	good map[string]authoredOpenMRAnswer
}

type authoredOpenMRAnswer struct {
	items     []dashboardAuthoredOpenPR
	total     int
	truncated bool
}

// authoredOpenMRBases is the instance set the indicator covers: the explicit
// gitlab_hosts entries only, sorted and de-duplicated.
func authoredOpenMRBases(policy config.GitProxyConfig) []string {
	seen := map[string]bool{}
	var out []string
	for host := range policy.GitLabHosts {
		base := strings.TrimRight(policy.GitLabBase(host), "/")
		if base != "" && !seen[base] {
			seen[base] = true
			out = append(out, base)
		}
	}
	sort.Strings(out)
	return out
}

func authoredOpenMRConfigKey(bases []string, tokenFile string) string {
	if len(bases) == 0 {
		return ""
	}
	return strings.Join(bases, ",") + "|" + strings.TrimSpace(tokenFile)
}

func startAuthoredOpenMRPoller(stop <-chan struct{}) {
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-stop
		cancel()
	}()
	go func() {
		timer := time.NewTimer(0)
		defer timer.Stop()
		failures := 0
		for {
			select {
			case <-stop:
				return
			case <-timer.C:
				retry, err := pollAuthoredOpenMRs(ctx)
				delay := authoredOpenMRPollInterval
				switch {
				case retry > 0:
					delay = min(max(delay, retry), authoredOpenMRMaxBackoff)
					slog.Info("open-mrs: GitLab rate limit; waiting", "retry_in", delay, "module", "agentd")
				case err != nil:
					failures++
					delay = authoredOpenMRRetryDelay(failures)
					if failures == 1 || delay > authoredOpenMRRetryDelay(failures-1) {
						slog.Warn("open-mrs: GitLab merge request poll failed; keeping last good list",
							"error", err, "consecutive_failures", failures, "retry_in", delay, "module", "agentd")
					}
				case failures > 0:
					slog.Info("open-mrs: GitLab merge request poll recovered",
						"previous_failures", failures, "module", "agentd")
					failures = 0
				}
				timer.Reset(delay)
			}
		}
	}()
}

func authoredOpenMRRetryDelay(failures int) time.Duration {
	delay := authoredOpenMRPollInterval
	for range min(max(failures, 0), 4) {
		delay *= 2
	}
	return min(delay, authoredOpenMRMaxBackoff)
}

// pollAuthoredOpenMRs asks every configured instance once. It returns the
// longest wait a rate-limited instance asked for, and an error when no
// instance answered.
func pollAuthoredOpenMRs(ctx context.Context) (time.Duration, error) {
	cfg, err := config.Load()
	if err != nil {
		return 0, fmt.Errorf("load config: %w", err)
	}
	policy := cfg.ResolvedGitProxy()
	bases := authoredOpenMRBases(policy)
	key := authoredOpenMRConfigKey(bases, policy.GitLabTokenFile)
	if key == "" || strings.TrimSpace(policy.GitLabTokenFile) == "" {
		// Nothing configured is not a failure; there is just nothing to show.
		resetAuthoredOpenMRs(key)
		return 0, nil
	}
	token, fault := gitlabToken(policy)
	if fault != nil {
		resetAuthoredOpenMRs(key)
		return 0, errors.New(fault.Msg)
	}

	authoredOpenMRState.RLock()
	good := map[string]authoredOpenMRAnswer{}
	if authoredOpenMRState.key == key {
		for base, answer := range authoredOpenMRState.good {
			good[base] = answer
		}
	}
	authoredOpenMRState.RUnlock()

	var wait time.Duration
	var lastErr error
	answered := 0
	instances := make([]dashboardAuthoredOpenMRInstance, 0, len(bases))
	for _, base := range bases {
		inst := dashboardAuthoredOpenMRInstance{Base: base, Host: gitlabBaseHost(base)}
		answer, username, retry, err := fetchAuthoredOpenMRs(ctx, base, token)
		if err != nil {
			inst.Error = err.Error()
			wait = max(wait, retry)
			lastErr = fmt.Errorf("%s: %w", inst.Host, err)
		} else {
			answered++
			good[base] = answer
			inst.Username = username
			inst.URL = base + "/dashboard/merge_requests?state=opened&author_username=" + url.QueryEscape(username)
		}
		instances = append(instances, inst)
	}

	authoredOpenMRState.Lock()
	authoredOpenMRState.key = key
	authoredOpenMRState.good = good
	authoredOpenMRState.instances = instances
	if answered > 0 {
		authoredOpenMRState.updatedAt = time.Now()
	}
	authoredOpenMRState.Unlock()

	if answered == 0 && lastErr != nil {
		return wait, lastErr
	}
	return wait, nil
}

func resetAuthoredOpenMRs(key string) {
	authoredOpenMRState.Lock()
	authoredOpenMRState.key = key
	authoredOpenMRState.good = nil
	authoredOpenMRState.instances = nil
	authoredOpenMRState.updatedAt = time.Time{}
	authoredOpenMRState.Unlock()
}

// gitlabBaseHost is the host[:port] a link label names.
func gitlabBaseHost(base string) string {
	if u, err := url.Parse(base); err == nil && u.Host != "" {
		return strings.ToLower(u.Host)
	}
	return base
}

// glUsernameRE bounds a GitLab username before it reaches a link: GitLab's own
// rule is letters, digits, underscores, dots and hyphens.
var glUsernameRE = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,254}$`)

type glOpenMRNode struct {
	IID                 string `json:"iid"`
	Title               string `json:"title"`
	WebURL              string `json:"webUrl"`
	Draft               bool   `json:"draft"`
	UpdatedAt           string `json:"updatedAt"`
	Conflicts           bool   `json:"conflicts"`
	DetailedMergeStatus string `json:"detailedMergeStatus"`
	HeadPipeline        *struct {
		Status string `json:"status"`
	} `json:"headPipeline"`
	ApprovedBy struct {
		Nodes []struct {
			Username string `json:"username"`
		} `json:"nodes"`
	} `json:"approvedBy"`
}

// fetchAuthoredOpenMRs runs the query against one instance.
func fetchAuthoredOpenMRs(ctx context.Context, base, token string) (authoredOpenMRAnswer, string, time.Duration, error) {
	ctx, cancel := context.WithTimeout(ctx, authoredOpenMRTimeout)
	defer cancel()
	res, err := glDo(ctx, base, token, glAPIRequest{
		Method:  http.MethodPost,
		GraphQL: true,
		Body: map[string]any{
			"query":     authoredOpenMRQuery,
			"variables": map[string]any{"first": authoredOpenMRLimit},
		},
	})
	if err != nil {
		return authoredOpenMRAnswer{}, "", 0, err
	}
	if res.Status == http.StatusTooManyRequests {
		return authoredOpenMRAnswer{}, "", glRetryAfter(res, time.Now()), errors.New(glRateLimitText(res))
	}
	if res.Status != http.StatusOK {
		return authoredOpenMRAnswer{}, "", 0, errors.New(glErrorText(res))
	}
	var doc struct {
		Data struct {
			CurrentUser *struct {
				Username              string `json:"username"`
				AuthoredMergeRequests struct {
					Count int            `json:"count"`
					Nodes []glOpenMRNode `json:"nodes"`
				} `json:"authoredMergeRequests"`
			} `json:"currentUser"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(res.Body, &doc); err != nil {
		return authoredOpenMRAnswer{}, "", 0, fmt.Errorf("decode GitLab response: %w", err)
	}
	if len(doc.Errors) > 0 {
		return authoredOpenMRAnswer{}, "", 0, fmt.Errorf("GitLab GraphQL: %s", truncateAuthoredPRTitle(doc.Errors[0].Message))
	}
	user := doc.Data.CurrentUser
	if user == nil {
		return authoredOpenMRAnswer{}, "", 0, errors.New("GitLab did not resolve the token to a user")
	}
	if !glUsernameRE.MatchString(user.Username) {
		return authoredOpenMRAnswer{}, "", 0, errors.New("GitLab returned an invalid username")
	}
	fetchedAt := time.Now().UTC().Format(time.RFC3339)
	answer := authoredOpenMRAnswer{items: []dashboardAuthoredOpenPR{}}
	for _, node := range user.AuthoredMergeRequests.Nodes {
		if item, ok := authoredOpenMRItem(base, node, fetchedAt); ok {
			answer.items = append(answer.items, item)
		}
	}
	answer.total = max(user.AuthoredMergeRequests.Count, len(answer.items))
	answer.truncated = user.AuthoredMergeRequests.Count > len(user.AuthoredMergeRequests.Nodes)
	return answer, user.Username, 0, nil
}

// authoredOpenMRItem maps one merge request to a footer row. Its URL must
// parse as a merge request on THIS instance, and the row is built from that
// parse rather than from the node's own fields, so the link, number and
// project the dashboard shows are all ones the instance check vouched for.
func authoredOpenMRItem(base string, node glOpenMRNode, fetchedAt string) (dashboardAuthoredOpenPR, bool) {
	ref, ok := gitlabMRRefFromURL(node.WebURL)
	if !ok || !strings.EqualFold(ref.base, strings.TrimRight(base, "/")) {
		return dashboardAuthoredOpenPR{}, false
	}
	if iid, err := strconv.Atoi(node.IID); err != nil || iid != ref.number {
		return dashboardAuthoredOpenPR{}, false
	}
	item := dashboardAuthoredOpenPR{
		Number:     ref.number,
		URL:        ref.base + "/" + ref.project + "/-/merge_requests/" + strconv.Itoa(ref.number),
		Title:      truncateAuthoredPRTitle(node.Title),
		Repository: ref.project,
		Draft:      node.Draft,
		Conflicts:  node.Conflicts,
		Checks:     glPipelineChecks(node.HeadPipeline, fetchedAt),
		Reviews: &prReviewSummary{
			Approved:       len(node.ApprovedBy.Nodes),
			ReviewersKnown: true,
			FetchedAt:      fetchedAt,
		},
	}
	if strings.EqualFold(node.DetailedMergeStatus, "REQUESTED_CHANGES") {
		item.Reviews.ChangesRequested = 1
	}
	if t, err := time.Parse(time.RFC3339, node.UpdatedAt); err == nil {
		item.UpdatedAt = t.UTC().Format(time.RFC3339)
	}
	return item, true
}

// glPipelineChecks renders a head pipeline as one pipeline-level check. No
// pipeline is no checks (nil): the footer treats that as needing attention,
// as it does a PR without CI. A skipped pipeline ran nothing, so it reads the
// same way.
func glPipelineChecks(pipeline *struct {
	Status string `json:"status"`
}, fetchedAt string) *prChecksSummary {
	if pipeline == nil {
		return nil
	}
	summary := &prChecksSummary{Total: 1, FetchedAt: fetchedAt}
	switch strings.ToUpper(pipeline.Status) {
	case "SUCCESS":
		summary.Passed, summary.State = 1, "passing"
	case "FAILED", "CANCELED", "CANCELING":
		summary.Failed, summary.State = 1, "failing"
	case "SKIPPED":
		return nil
	default:
		// CREATED, WAITING_FOR_RESOURCE, PREPARING, PENDING, RUNNING,
		// SCHEDULED, MANUAL, WAITING_FOR_CALLBACK and anything newer: not
		// finished, nothing failed.
		summary.Pending, summary.State = 1, "pending"
	}
	return summary
}

// glRetryAfter is how long a 429 asked to wait: Retry-After, else the
// RateLimit-Reset instant, else a minute.
func glRetryAfter(res glAPIResult, now time.Time) time.Duration {
	if s := strings.TrimSpace(res.Header.Get("Retry-After")); s != "" {
		if secs, err := strconv.Atoi(s); err == nil && secs > 0 {
			return time.Duration(secs) * time.Second
		}
	}
	if s := strings.TrimSpace(res.Header.Get("RateLimit-Reset")); s != "" {
		if epoch, err := strconv.ParseInt(s, 10, 64); err == nil {
			if d := time.Unix(epoch, 0).Sub(now); d > 0 {
				return d
			}
		}
	}
	return authoredOpenMRDefaultRetry
}

// authoredOpenMRsSnapshot is the snapshot's view: nil (the indicator hidden)
// unless the poller's last result was read under the current configuration
// and at least one instance has answered.
func authoredOpenMRsSnapshot(cfg *config.Config, agents []dashboardAgent) *dashboardAuthoredOpenMRs {
	policy := cfg.ResolvedGitProxy()
	key := authoredOpenMRConfigKey(authoredOpenMRBases(policy), policy.GitLabTokenFile)
	authoredOpenMRState.RLock()
	defer authoredOpenMRState.RUnlock()
	if key == "" || authoredOpenMRState.key != key || len(authoredOpenMRState.good) == 0 {
		return nil
	}
	view := dashboardAuthoredOpenMRs{
		dashboardAuthoredOpenPRs: dashboardAuthoredOpenPRs{
			Available:  true,
			AlwaysShow: cfg.AlwaysShowOpenPRs(),
			Items:      []dashboardAuthoredOpenPR{},
			Recent:     []dashboardAuthoredOpenPR{},
		},
		Instances: append([]dashboardAuthoredOpenMRInstance(nil), authoredOpenMRState.instances...),
	}
	if !authoredOpenMRState.updatedAt.IsZero() {
		view.UpdatedAt = authoredOpenMRState.updatedAt.Format(time.RFC3339)
	}
	for _, answer := range authoredOpenMRState.good {
		// A copy, so matching agents below never writes into the shared state.
		view.Items = append(view.Items, answer.items...)
		view.Total += answer.total
		view.Truncated = view.Truncated || answer.truncated
	}
	sortAuthoredOpenPRs(view.Items)
	view.dashboardAuthoredOpenPRs = associateAuthoredOpenPRs(view.dashboardAuthoredOpenPRs, agents)
	return &view
}
