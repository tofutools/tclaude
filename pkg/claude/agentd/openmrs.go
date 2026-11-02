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
	"crypto/sha256"
	"encoding/hex"
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
	// EveryoneError says why the last project-wide read failed. The
	// instance itself is fine, and its last good project tabs stay.
	EveryoneError string `json:"everyone_error,omitempty"`
}

// dashboardAuthoredOpenMRs is the snapshot's authored_open_mrs: the Open PRs
// shape plus the per-instance links.
type dashboardAuthoredOpenMRs struct {
	dashboardAuthoredOpenPRs
	Instances []dashboardAuthoredOpenMRInstance `json:"instances"`
	// Projects are the popover's project tabs: everyone's open merge
	// requests on the configured instances, one entry per project.
	Projects []dashboardOpenMRProject `json:"projects"`
	// EveryoneTotal is the footer's second number: every open merge request
	// across Projects. EveryoneTruncated says it is a floor.
	EveryoneTotal     int  `json:"everyone_total"`
	EveryoneTruncated bool `json:"everyone_truncated,omitempty"`
}

// authoredOpenMRState is the poller's last result. key names the configuration
// it was read under (the instances and the token file); a snapshot built under
// any other configuration shows nothing rather than a list the current
// credential never produced.
var authoredOpenMRState struct {
	sync.RWMutex
	key string
	// tokenDigest is a SHA-256 digest of the token the lists were read with
	// (never the token). The token FILE can be rewritten to another account
	// under the same path, and lists read for the old one must not survive.
	tokenDigest string
	updatedAt   time.Time
	instances   []dashboardAuthoredOpenMRInstance
	// good is each instance's last successful answer, kept through a failed
	// poll so one bad response does not empty the popover.
	good map[string]authoredOpenMRAnswer
	// everyone is each instance's last successful project-wide answer, kept
	// the same way.
	everyone map[string]authoredOpenMREveryone
}

type authoredOpenMRAnswer struct {
	items     []dashboardAuthoredOpenPR
	total     int
	truncated bool
}

// authoredOpenMREveryone is one instance's project-wide answer: every open
// merge request the token can see, grouped by project.
type authoredOpenMREveryone struct {
	projects []dashboardOpenMRProject
	// truncated is set when discovery stopped at its page cap, so projects
	// beyond it are missing.
	truncated bool
}

// dashboardOpenMRProject is one project tab in the Open MRs popover: every
// open merge request in the project, whoever wrote it.
type dashboardOpenMRProject struct {
	Base    string `json:"base"`
	Host    string `json:"host"`
	Project string `json:"project"`
	// URL is the project's own merge request list.
	URL       string `json:"url"`
	Total     int    `json:"total"`
	Truncated bool   `json:"truncated,omitempty"`
	// Agents marks a project an agent in this fleet is working in; those
	// tabs come first.
	Agents bool                      `json:"agents,omitempty"`
	Items  []dashboardAuthoredOpenPR `json:"items"`
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
				var delay time.Duration
				previous := failures
				delay, failures = authoredOpenMRNextDelay(retry, err, failures)
				switch {
				case retry > 0:
					slog.Info("open-mrs: GitLab rate limit; waiting", "retry_in", delay, "module", "agentd")
				case err != nil:
					if failures == 1 || delay > authoredOpenMRRetryDelay(failures-1) {
						slog.Warn("open-mrs: GitLab merge request poll failed; keeping last good list",
							"error", err, "consecutive_failures", failures, "retry_in", delay, "module", "agentd")
					}
				case previous > 0:
					slog.Info("open-mrs: GitLab merge request poll recovered",
						"previous_failures", previous, "module", "agentd")
				}
				timer.Reset(delay)
			}
		}
	}()
}

// authoredOpenMRNextDelay is the wait before the next poll, and the new
// failure count: a rate limit waits out what GitLab asked for (at least the
// poll interval, at most the backoff cap) without counting as a failure, an
// error backs off exponentially, and success resets.
func authoredOpenMRNextDelay(retry time.Duration, err error, failures int) (time.Duration, int) {
	switch {
	case retry > 0:
		return min(max(authoredOpenMRPollInterval, retry), authoredOpenMRMaxBackoff), failures
	case err != nil:
		failures++
		return authoredOpenMRRetryDelay(failures), failures
	}
	return authoredOpenMRPollInterval, 0
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

	sum := sha256.Sum256([]byte(token))
	digest := hex.EncodeToString(sum[:])
	authoredOpenMRState.RLock()
	good := map[string]authoredOpenMRAnswer{}
	everyone := map[string]authoredOpenMREveryone{}
	if authoredOpenMRState.key == key && authoredOpenMRState.tokenDigest == digest {
		for base, answer := range authoredOpenMRState.good {
			good[base] = answer
		}
		for base, answer := range authoredOpenMRState.everyone {
			everyone[base] = answer
		}
	}
	authoredOpenMRState.RUnlock()

	authoredOpenMRAgentProjects.Lock()
	agentProjects := authoredOpenMRAgentProjects.set
	authoredOpenMRAgentProjects.Unlock()

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
			// Everyone's merge requests only once the token has resolved a
			// user here; a failure keeps the last good project lists.
			all, retry, err := fetchEveryoneOpenMRs(ctx, base, token, agentProjects)
			if err != nil {
				inst.EveryoneError = err.Error()
				wait = max(wait, retry)
			} else {
				everyone[base] = all
			}
		}
		instances = append(instances, inst)
	}

	authoredOpenMRState.Lock()
	authoredOpenMRState.key = key
	authoredOpenMRState.tokenDigest = digest
	authoredOpenMRState.good = good
	authoredOpenMRState.everyone = everyone
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
	authoredOpenMRState.tokenDigest = ""
	authoredOpenMRState.good = nil
	authoredOpenMRState.everyone = nil
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
	// Author is asked for by the project-wide query only.
	Author struct {
		Username string `json:"username"`
	} `json:"author"`
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
	switch strings.ToUpper(node.DetailedMergeStatus) {
	case "REQUESTED_CHANGES":
		item.Reviews.ChangesRequested = 1
	case "NOT_APPROVED":
		// The project's own approval rules still want approvals, however
		// many people have approved: never ready to merge.
		item.Reviews.Decision = "review_required"
	}
	if t, err := time.Parse(time.RFC3339, node.UpdatedAt); err == nil {
		item.UpdatedAt = t.UTC().Format(time.RFC3339)
	}
	return item, true
}

const (
	// everyoneOpenMRPages caps discovery: at 100 merge requests a page, the
	// project-wide view covers the 500 most recently updated open ones.
	everyoneOpenMRPages = 5
	// everyoneOpenMRPerProject is the page each project tab asks for.
	everyoneOpenMRPerProject = 100
	// everyoneOpenMRChunk is how many projects one GraphQL query reads.
	everyoneOpenMRChunk = 5
	// everyoneOpenMRMaxProjects caps the projects read per instance and
	// poll: the ones agents work in first, then the busiest. With discovery,
	// that bounds a poll at 9 requests per instance whatever the instance
	// holds, each under its own authoredOpenMRTimeout.
	everyoneOpenMRMaxProjects = 20
)

// authoredOpenMRAgentProjects is the set of "<base>/<project>" roots agents
// work in, as the last snapshot saw them. The poller reads it to put those
// projects first when it has to cap.
var authoredOpenMRAgentProjects struct {
	sync.Mutex
	set map[string]bool
}

// fetchEveryoneOpenMRs reads every open merge request the token can see on
// one instance, grouped by project. GitLab's GraphQL has no instance-wide
// merge request list, so discovery is the REST list (scope=all), which names
// the projects. Each project is then read through GraphQL with the same fields
// as the Mine list, so pipelines and approvals mean the same thing in both.
//
// The work is bounded whatever the instance holds: discovery stops at
// everyoneOpenMRPages, and at most everyoneOpenMRMaxProjects projects are read
// (agents' projects first, then the ones with the most open merge requests).
// Every request runs under its own timeout, so a slow instance fails one step
// rather than starving the rest. Anything cut is reported as truncated.
func fetchEveryoneOpenMRs(ctx context.Context, base, token string, agentProjects map[string]bool) (authoredOpenMREveryone, time.Duration, error) {
	var order []string
	counts := map[string]int{}
	truncated := false
	for page := 1; page <= everyoneOpenMRPages; page++ {
		pageCtx, cancel := context.WithTimeout(ctx, authoredOpenMRTimeout)
		res, err := glDo(pageCtx, base, token, glAPIRequest{
			Method: http.MethodGet,
			Path:   "merge_requests",
			Query: url.Values{
				"scope": {"all"}, "state": {"opened"}, "order_by": {"updated_at"},
				"per_page": {"100"}, "page": {strconv.Itoa(page)},
			},
		})
		cancel()
		if err != nil {
			return authoredOpenMREveryone{}, 0, err
		}
		if res.Status == http.StatusTooManyRequests {
			return authoredOpenMREveryone{}, glRetryAfter(res, time.Now()), errors.New(glRateLimitText(res))
		}
		if res.Status != http.StatusOK {
			return authoredOpenMREveryone{}, 0, errors.New(glErrorText(res))
		}
		var list []struct {
			WebURL string `json:"web_url"`
		}
		if err := json.Unmarshal(res.Body, &list); err != nil {
			return authoredOpenMREveryone{}, 0, fmt.Errorf("decode GitLab response: %w", err)
		}
		for _, mr := range list {
			ref, ok := gitlabMRRefFromURL(mr.WebURL)
			if !ok || !strings.EqualFold(ref.base, base) {
				continue
			}
			if counts[ref.project] == 0 {
				order = append(order, ref.project)
			}
			counts[ref.project]++
		}
		if len(list) < 100 {
			break
		}
		truncated = page == everyoneOpenMRPages
	}

	// Agents' projects first, then the busiest; discovery order (most
	// recently updated first) breaks ties.
	rank := map[string]int{}
	for i, project := range order {
		rank[project] = i
	}
	sort.SliceStable(order, func(i, j int) bool {
		a, b := order[i], order[j]
		agentA, agentB := agentProjects[strings.ToLower(base+"/"+a)], agentProjects[strings.ToLower(base+"/"+b)]
		if agentA != agentB {
			return agentA
		}
		if counts[a] != counts[b] {
			return counts[a] > counts[b]
		}
		return rank[a] < rank[b]
	})
	if len(order) > everyoneOpenMRMaxProjects {
		order = order[:everyoneOpenMRMaxProjects]
		truncated = true
	}

	out := authoredOpenMREveryone{truncated: truncated}
	fetchedAt := time.Now().UTC().Format(time.RFC3339)
	for start := 0; start < len(order); start += everyoneOpenMRChunk {
		chunk := order[start:min(start+everyoneOpenMRChunk, len(order))]
		chunkCtx, cancel := context.WithTimeout(ctx, authoredOpenMRTimeout)
		got, retry, err := fetchOpenMRProjects(chunkCtx, base, token, chunk, fetchedAt)
		cancel()
		if err != nil {
			return authoredOpenMREveryone{}, retry, err
		}
		out.projects = append(out.projects, got...)
	}
	return out, 0, nil
}

// fetchOpenMRProjects reads up to everyoneOpenMRChunk projects' open merge
// requests in one GraphQL query, one alias per project. The query text is
// built here from a count; every project path travels as a variable.
func fetchOpenMRProjects(ctx context.Context, base, token string, projects []string, fetchedAt string) ([]dashboardOpenMRProject, time.Duration, error) {
	var params, fields strings.Builder
	variables := map[string]any{}
	for i, project := range projects {
		if i > 0 {
			params.WriteString(", ")
		}
		fmt.Fprintf(&params, "$p%d: ID!", i)
		fmt.Fprintf(&fields, "  p%d: project(fullPath: $p%d) { mergeRequests(state: opened, first: %d) { count nodes { ...mr } } }\n",
			i, i, everyoneOpenMRPerProject)
		variables["p"+strconv.Itoa(i)] = project
	}
	query := "query(" + params.String() + ") {\n" + fields.String() + "}\n" +
		"fragment mr on MergeRequest { iid title webUrl draft updatedAt conflicts detailedMergeStatus " +
		"author { username } headPipeline { status } approvedBy { nodes { username } } }"
	res, err := glDo(ctx, base, token, glAPIRequest{
		Method:  http.MethodPost,
		GraphQL: true,
		Body:    map[string]any{"query": query, "variables": variables},
	})
	if err != nil {
		return nil, 0, err
	}
	if res.Status == http.StatusTooManyRequests {
		return nil, glRetryAfter(res, time.Now()), errors.New(glRateLimitText(res))
	}
	if res.Status != http.StatusOK {
		return nil, 0, errors.New(glErrorText(res))
	}
	var doc struct {
		Data map[string]*struct {
			MergeRequests struct {
				Count int            `json:"count"`
				Nodes []glOpenMRNode `json:"nodes"`
			} `json:"mergeRequests"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(res.Body, &doc); err != nil {
		return nil, 0, fmt.Errorf("decode GitLab response: %w", err)
	}
	if len(doc.Errors) > 0 {
		return nil, 0, fmt.Errorf("GitLab GraphQL: %s", truncateAuthoredPRTitle(doc.Errors[0].Message))
	}
	var out []dashboardOpenMRProject
	for i, project := range projects {
		got := doc.Data["p"+strconv.Itoa(i)]
		if got == nil {
			continue // not visible to the token any more
		}
		entry := dashboardOpenMRProject{
			Base: base, Host: gitlabBaseHost(base), Project: project,
			URL:   base + "/" + project + "/-/merge_requests",
			Items: []dashboardAuthoredOpenPR{},
		}
		for _, node := range got.MergeRequests.Nodes {
			item, ok := authoredOpenMRItem(base, node, fetchedAt)
			if !ok || !strings.EqualFold(item.Repository, project) {
				continue
			}
			if glUsernameRE.MatchString(node.Author.Username) {
				item.Author = node.Author.Username
			}
			entry.Items = append(entry.Items, item)
		}
		entry.Total = max(got.MergeRequests.Count, len(entry.Items))
		entry.Truncated = got.MergeRequests.Count > len(got.MergeRequests.Nodes)
		if entry.Total > 0 {
			out = append(out, entry)
		}
	}
	return out, 0, nil
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
	view.Projects, view.EveryoneTotal, view.EveryoneTruncated = openMRProjectsSnapshot(authoredOpenMRState.everyone, agents)
	return &view
}

// openMRProjectsSnapshot is the project tabs: a copy of every instance's
// project lists, matched to agents, with the projects agents work in first,
// then the busiest. Each tab carries its own rows; the shared state is never
// written.
func openMRProjectsSnapshot(everyone map[string]authoredOpenMREveryone, agents []dashboardAgent) ([]dashboardOpenMRProject, int, bool) {
	// An agent's project is read off its branch links: a GitLab web link is
	// <base>/<project>/-/tree/<branch> (or /-/compare/…, or a merge
	// request), so the project root is everything before "/-/".
	agentRepos := map[string]bool{}
	for _, agent := range agents {
		links := []string{agent.BranchURL, agent.StartupBranchURL, agent.BranchPRURL, agent.StartupPRURL}
		for _, presented := range agent.PresentedPRs {
			links = append(links, presented.URL)
		}
		for _, link := range links {
			if root, _, ok := strings.Cut(link, "/-/"); ok && strings.HasPrefix(root, "https://") {
				agentRepos[strings.ToLower(root)] = true
			}
		}
	}
	authoredOpenMRAgentProjects.Lock()
	authoredOpenMRAgentProjects.set = agentRepos
	authoredOpenMRAgentProjects.Unlock()
	projects := []dashboardOpenMRProject{}
	total := 0
	truncated := false
	for _, answer := range everyone {
		truncated = truncated || answer.truncated
		for _, project := range answer.projects {
			copied := project
			view := associateAuthoredOpenPRs(dashboardAuthoredOpenPRs{
				Items: append([]dashboardAuthoredOpenPR(nil), project.Items...),
			}, agents)
			copied.Items = view.Items
			sortAuthoredOpenPRs(copied.Items)
			copied.Agents = agentRepos[strings.ToLower(copied.Base+"/"+copied.Project)]
			total += copied.Total
			truncated = truncated || copied.Truncated
			projects = append(projects, copied)
		}
	}
	sort.SliceStable(projects, func(i, j int) bool {
		a, b := projects[i], projects[j]
		if a.Agents != b.Agents {
			return a.Agents
		}
		if a.Total != b.Total {
			return a.Total > b.Total
		}
		return strings.ToLower(a.Base+"/"+a.Project) < strings.ToLower(b.Base+"/"+b.Project)
	})
	return projects, total, truncated
}
