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
//   - reviews list real approvals (approvedBy), and the merge decision comes
//     from detailedMergeStatus (prreviews_gitlab.go). GitLab's own "approved"
//     flag is deliberately not used: it is true with zero approvals on a
//     project that requires none. A second query reads the reviewers' states
//     for the user's own merge requests, where the instance has them, and
//     both go into the review cache every other PR surface reads;
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
	// authoredRecentMREvery is how many polls apart the recently closed list
	// is read: every 150s at the 30s poll. A merged merge request is
	// history, so it has no need of the open list's pace.
	authoredRecentMREvery = 5
	// authoredRecentMRLimit is the page asked for per state and instance.
	authoredRecentMRLimit = 50
)

// recentMRWindowDays is the recently closed list's lookback. Merge requests
// share the pull requests' dashboard.recent_pr_window_days; this one line
// is where they would get their own.
func recentMRWindowDays(cfg *config.Config) int { return cfg.RecentPRWindowDays() }

// authoredRecentMRQuery reads the user's merge requests merged or closed
// since $after, one alias per state, because authoredMergeRequests takes a
// single state. Only the first page of each, most recently updated first; a
// merge request's last update is its merge or close, or later.
const authoredRecentMRQuery = `query($first: Int!, $after: Time!) {
  currentUser {
    merged: authoredMergeRequests(state: merged, updatedAfter: $after, sort: UPDATED_DESC, first: $first) {
      count
      nodes { iid title webUrl updatedAt mergedAt headPipeline { status } }
    }
    closed: authoredMergeRequests(state: closed, updatedAfter: $after, sort: UPDATED_DESC, first: $first) {
      count
      nodes { iid title webUrl updatedAt closedAt headPipeline { status } }
    }
  }
}`

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
	// EveryoneByProject is set when the instance refused the instance-wide
	// list (a project-limited token) and everyone's merge requests were read
	// project by project instead: the number of projects read.
	EveryoneByProject int `json:"everyone_by_project,omitempty"`
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
	// recent is each instance's last recently closed list, kept the same
	// way. recentTried is each instance's last attempt at it, failed or not,
	// which paces the next one; polls counts the polls.
	recent      map[string]authoredRecentMRs
	recentTried map[string]recentMRAttempt
	polls       int
	// refused is each instance whose instance-wide merge request list the
	// token may not call, and when it last said so. Such an instance is read
	// project by project, and asked again every everyoneOpenMRReprobe.
	refused map[string]everyoneRefusal
}

// everyoneRefusal is an instance's last refusal of the instance-wide list.
type everyoneRefusal struct {
	at     time.Time
	reason string
}

// recentMRAttempt is when (by poll number) and under which window an
// instance's recently closed list was last asked for.
type recentMRAttempt struct {
	poll       int
	windowDays int
}

// authoredRecentMRs is one instance's recently closed list and the window it
// was read with.
type authoredRecentMRs struct {
	items      []dashboardAuthoredOpenPR
	windowDays int
	truncated  bool
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
	// byProject is how many projects were read individually because the
	// instance-wide list was refused, counting those GitLab answered for (a
	// path that is a group, or gone, does not count); zero when discovery
	// named them.
	byProject int
	// found counts the projects a read answered for, with open merge
	// requests or without.
	found int
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

	digest := authoredOpenMRTokenDigest(token)
	authoredOpenMRState.RLock()
	good := map[string]authoredOpenMRAnswer{}
	everyone := map[string]authoredOpenMREveryone{}
	recent := map[string]authoredRecentMRs{}
	recentTried := map[string]recentMRAttempt{}
	refused := map[string]everyoneRefusal{}
	polls := 0
	if authoredOpenMRState.key == key && authoredOpenMRState.tokenDigest == digest {
		for base, answer := range authoredOpenMRState.good {
			good[base] = answer
		}
		for base, answer := range authoredOpenMRState.everyone {
			everyone[base] = answer
		}
		for base, answer := range authoredOpenMRState.recent {
			recent[base] = answer
		}
		for base, attempt := range authoredOpenMRState.recentTried {
			recentTried[base] = attempt
		}
		for base, refusal := range authoredOpenMRState.refused {
			refused[base] = refusal
		}
		polls = authoredOpenMRState.polls
	}
	authoredOpenMRState.RUnlock()
	windowDays := recentMRWindowDays(cfg)

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
			wait = max(wait, shareAuthoredOpenMRReviews(ctx, base, token, answer.items, time.Now()))
			good[base] = answer
			inst.Username = username
			inst.URL = base + "/dashboard/merge_requests?state=opened&author_username=" + url.QueryEscape(username)
			// Everyone's merge requests only once the token has resolved a
			// user here; a failure keeps the last good project lists.
			all, retry, err := readEveryoneOpenMRs(ctx, base, token, agentProjects,
				everyoneFallbackKnown(policy, base, answer.items), refused, time.Now())
			if err != nil {
				inst.EveryoneError = err.Error()
				wait = max(wait, retry)
			} else {
				everyone[base] = all
				inst.EveryoneByProject = all.byProject
			}
			// The recently closed list, authoredRecentMREvery polls after the
			// last attempt, or at once when the window changed under it. A
			// failed attempt counts, so a failing read waits as long as a
			// good one; the last good list stays meanwhile.
			tried, have := recentTried[base]
			switch {
			case windowDays <= 0:
				delete(recent, base)
				delete(recentTried, base)
			case !have || tried.windowDays != windowDays || polls-tried.poll >= authoredRecentMREvery:
				recentTried[base] = recentMRAttempt{poll: polls, windowDays: windowDays}
				got, retry, err := fetchRecentMRs(ctx, base, token, windowDays, time.Now())
				switch {
				case errors.Is(err, errGLUnknownField):
					// An instance too old for the query has no list, so no
					// Closed filter, rather than an empty one.
					delete(recent, base)
				case err != nil:
					wait = max(wait, retry)
					slog.Debug("open-mrs: GitLab recently closed read failed; keeping the last list",
						"host", inst.Host, "error", err, "module", "agentd")
				default:
					recent[base] = got
				}
			}
		}
		instances = append(instances, inst)
	}

	authoredOpenMRState.Lock()
	authoredOpenMRState.key = key
	authoredOpenMRState.tokenDigest = digest
	authoredOpenMRState.good = good
	authoredOpenMRState.everyone = everyone
	authoredOpenMRState.recent = recent
	authoredOpenMRState.recentTried = recentTried
	authoredOpenMRState.refused = refused
	authoredOpenMRState.polls = polls + 1
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

// shareAuthoredOpenMRReviews writes the user's merge requests' reviews into the
// review cache, so the Branch column and presented badges for the same merge
// request read the same answer as the footer. The list query gave the decision
// and the approvals; the reviewer read adds change requests where the
// instance has reviewer states. When that read fails for any other reason,
// only the decision is written and the cached reviewer list stands. It
// returns the wait a 429 on the reviewer read asked for.
func shareAuthoredOpenMRReviews(ctx context.Context, base, token string, items []dashboardAuthoredOpenPR, now time.Time) time.Duration {
	readCtx, cancel := context.WithTimeout(ctx, authoredOpenMRTimeout)
	reviewers, err := fetchAuthoredMRReviewers(readCtx, base, token)
	cancel()
	if err != nil && !errors.Is(err, errGLUnknownField) {
		slog.Debug("open-mrs: GitLab reviewer read failed; keeping cached reviewers",
			"host", gitlabBaseHost(base), "error", err, "module", "agentd")
	}
	for i := range items {
		if items[i].reviewObs == nil {
			continue
		}
		obs := *items[i].reviewObs
		switch node, ok := reviewers[prStateKey(items[i].URL)]; {
		case ok:
			obs.Reviewers = glReviewers(base, node.ApprovedBy.Nodes, node.Reviewers.Nodes)
		case !errors.Is(err, errGLUnknownField):
			// Read and missing (opened between the two reads), or not read.
			// Over a cached reviewer list, the decision alone, so a list with
			// change requests is not replaced by one without. With none known
			// yet (a fresh daemon, a new merge request), the list query's
			// approvals, rather than a row that has lost them.
			if cached, known := loadPRReviews(items[i].URL); known && cached.ReviewersKnown {
				obs.Reviewers, obs.ReviewersFetched = nil, false
			}
		}
		savePRReviewObservation(items[i].URL, obs, now)
	}
	return glRetryFrom(err)
}

// authoredOpenMRTokenDigest identifies a token without keeping it: lists are
// only ever shown for the token they were read with.
func authoredOpenMRTokenDigest(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func resetAuthoredOpenMRs(key string) {
	authoredOpenMRState.Lock()
	authoredOpenMRState.key = key
	authoredOpenMRState.tokenDigest = ""
	authoredOpenMRState.good = nil
	authoredOpenMRState.everyone = nil
	authoredOpenMRState.recent = nil
	authoredOpenMRState.recentTried = nil
	authoredOpenMRState.refused = nil
	authoredOpenMRState.polls = 0
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
	ApprovedBy glReviewUsers `json:"approvedBy"`
	// Author is asked for by the project-wide query only.
	Author struct {
		Username string `json:"username"`
	} `json:"author"`
	// MergedAt and ClosedAt are asked for by the recently closed query only.
	MergedAt string `json:"mergedAt"`
	ClosedAt string `json:"closedAt"`
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
	// Approvals only: this query asks for no reviewer states, and a
	// NOT_APPROVED status (the project's approval rules still want more,
	// however many people approved) reads as review required.
	obs := glReviewObservation(base, glMRReviewNode{
		DetailedMergeStatus: node.DetailedMergeStatus,
		ApprovedBy:          node.ApprovedBy,
	})
	at, err := time.Parse(time.RFC3339, fetchedAt)
	if err != nil {
		at = time.Now()
	}
	reviews := mergePRReviewObservation(nil, obs, at)
	item := dashboardAuthoredOpenPR{
		Number:     ref.number,
		URL:        ref.base + "/" + ref.project + "/-/merge_requests/" + strconv.Itoa(ref.number),
		Title:      truncateAuthoredPRTitle(node.Title),
		Repository: ref.project,
		Draft:      node.Draft,
		Conflicts:  node.Conflicts,
		Checks:     glPipelineChecks(node.HeadPipeline, fetchedAt),
		Reviews:    &reviews,
		reviewObs:  &obs,
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
	// everyoneOpenMRReprobe is how long an instance that refused the
	// instance-wide list is read project by project before it is asked again.
	everyoneOpenMRReprobe = 10 * time.Minute
)

// errEveryoneRefused is the instance-wide list's 403: the token may read
// projects, but not list the whole instance (a project-limited fine-grained
// token answers insufficient_granular_scope).
type errEveryoneRefused struct{ reason string }

func (e *errEveryoneRefused) Error() string { return e.reason }

// readEveryoneOpenMRs is one instance's project-wide read. It asks for the
// instance-wide list unless that was refused within everyoneOpenMRReprobe;
// a refused instance is read project by project instead, from the projects
// already known to matter: the agents', then those of the user's own open
// merge requests and the exact projects allowed_remotes names (known). Only
// when that fails too, or has no project to read, is the read an error, and
// the error carries both reasons. refused is updated in place.
func readEveryoneOpenMRs(ctx context.Context, base, token string, agentProjects map[string]bool, known []string,
	refused map[string]everyoneRefusal, now time.Time) (authoredOpenMREveryone, time.Duration, error) {
	refusal, wasRefused := refused[base]
	var probeRetry time.Duration
	if !wasRefused || now.Sub(refusal.at) >= everyoneOpenMRReprobe {
		all, retry, err := fetchEveryoneOpenMRs(ctx, base, token, agentProjects)
		var refusedErr *errEveryoneRefused
		switch {
		case errors.As(err, &refusedErr):
			refusal = everyoneRefusal{at: now, reason: refusedErr.reason}
		case err == nil:
			delete(refused, base)
			return all, retry, nil
		case !wasRefused:
			return all, retry, err
		default:
			// A re-probe that failed for another reason (a timeout, a 5xx,
			// a 429) says nothing new about the token: keep reading project
			// by project, and ask again after the next interval.
			refusal.at = now
			probeRetry = retry
		}
		refused[base] = refusal
	}
	projects := everyoneFallbackProjects(base, agentProjects, known)
	if len(projects) == 0 {
		return authoredOpenMREveryone{}, 0, fmt.Errorf("%s: this token can't list the whole instance, and no project "+
			"is known to read one by one (none an agent works in, none with your open merge requests, "+
			"no exact project in allowed_remotes)", refusal.reason)
	}
	truncated := false
	if len(projects) > everyoneOpenMRMaxProjects {
		projects, truncated = projects[:everyoneOpenMRMaxProjects], true
	}
	all, retry, err := fetchOpenMRProjectChunks(ctx, base, token, projects)
	if err != nil {
		return authoredOpenMREveryone{}, max(retry, probeRetry), fmt.Errorf("%s (listing the instance); reading its projects one by one: %w",
			refusal.reason, err)
	}
	all.truncated, all.byProject = truncated, all.found
	return all, probeRetry, nil
}

// everyoneFallbackKnown is the part of a refused instance's project set the
// poller knows besides the agents': the projects of the user's own open merge
// requests on it, then every allowed_remotes entry that names one exact
// project there (host/group/.../project, no wildcard). An entry naming a group
// with subgroups cannot be told from a project by its shape; GitLab answers
// such a path with no project, and it is skipped. allowed_remotes is
// lower-cased by ResolvedGitProxy, so these paths take GitLab's own spelling
// only from the fullPath fetchOpenMRProjects asks for.
func everyoneFallbackKnown(policy config.GitProxyConfig, base string, mine []dashboardAuthoredOpenPR) []string {
	var out []string
	for _, item := range mine {
		out = append(out, item.Repository)
	}
	for _, pattern := range policy.AllowedRemotes {
		segs := strings.Split(strings.Trim(pattern, "/"), "/")
		if len(segs) < 3 || strings.Contains(pattern, "*") {
			continue
		}
		if !strings.EqualFold(strings.TrimRight(policy.GitLabBase(segs[0]), "/"), base) {
			continue
		}
		out = append(out, strings.TrimSuffix(strings.Join(segs[1:], "/"), ".git"))
	}
	return out
}

// everyoneFallbackProjects orders a refused instance's projects: the ones
// agents work in first (sorted, as the set has no order), then known, each
// once whatever its case.
func everyoneFallbackProjects(base string, agentProjects map[string]bool, known []string) []string {
	prefix := strings.ToLower(strings.TrimRight(base, "/")) + "/"
	var agents []string
	for root := range agentProjects {
		if project, ok := strings.CutPrefix(strings.ToLower(root), prefix); ok {
			agents = append(agents, project)
		}
	}
	sort.Strings(agents)
	seen := map[string]bool{}
	var out []string
	for _, project := range append(agents, known...) {
		project = strings.Trim(project, "/")
		key := strings.ToLower(project)
		if project == "" || strings.Contains(project, "/-/") || seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, project)
	}
	return out
}

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
		if res.Status == http.StatusForbidden {
			// Not a 401: a bad token fails the user's own list too.
			return authoredOpenMREveryone{}, 0, &errEveryoneRefused{reason: glErrorText(res)}
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
	out, retry, err := fetchOpenMRProjectChunks(ctx, base, token, order)
	if err != nil {
		return authoredOpenMREveryone{}, retry, err
	}
	out.truncated = truncated
	return out, 0, nil
}

// fetchOpenMRProjectChunks reads the given projects everyoneOpenMRChunk to a
// GraphQL query, each query under its own timeout.
func fetchOpenMRProjectChunks(ctx context.Context, base, token string, order []string) (authoredOpenMREveryone, time.Duration, error) {
	var out authoredOpenMREveryone
	fetchedAt := time.Now().UTC().Format(time.RFC3339)
	for start := 0; start < len(order); start += everyoneOpenMRChunk {
		chunk := order[start:min(start+everyoneOpenMRChunk, len(order))]
		chunkCtx, cancel := context.WithTimeout(ctx, authoredOpenMRTimeout)
		got, found, retry, err := fetchOpenMRProjects(chunkCtx, base, token, chunk, fetchedAt)
		cancel()
		if err != nil {
			return authoredOpenMREveryone{}, retry, err
		}
		out.projects = append(out.projects, got...)
		out.found += found
	}
	return out, 0, nil
}

// fetchOpenMRProjects reads up to everyoneOpenMRChunk projects' open merge
// requests in one GraphQL query, one alias per project. The query text is
// built here from a count; every project path travels as a variable.
// It also returns how many of the projects GitLab answered for.
func fetchOpenMRProjects(ctx context.Context, base, token string, projects []string, fetchedAt string) ([]dashboardOpenMRProject, int, time.Duration, error) {
	var params, fields strings.Builder
	variables := map[string]any{}
	for i, project := range projects {
		if i > 0 {
			params.WriteString(", ")
		}
		fmt.Fprintf(&params, "$p%d: ID!", i)
		fmt.Fprintf(&fields, "  p%d: project(fullPath: $p%d) { fullPath mergeRequests(state: opened, first: %d) { count nodes { ...mr } } }\n",
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
		return nil, 0, 0, err
	}
	if res.Status == http.StatusTooManyRequests {
		return nil, 0, glRetryAfter(res, time.Now()), errors.New(glRateLimitText(res))
	}
	if res.Status != http.StatusOK {
		return nil, 0, 0, errors.New(glErrorText(res))
	}
	var doc struct {
		Data map[string]*struct {
			FullPath      string `json:"fullPath"`
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
		return nil, 0, 0, fmt.Errorf("decode GitLab response: %w", err)
	}
	if len(doc.Errors) > 0 {
		return nil, 0, 0, fmt.Errorf("GitLab GraphQL: %s", truncateAuthoredPRTitle(doc.Errors[0].Message))
	}
	var out []dashboardOpenMRProject
	found := 0
	for i, project := range projects {
		got := doc.Data["p"+strconv.Itoa(i)]
		if got == nil {
			continue // not visible to the token any more, or not a project
		}
		found++
		// A path known only lower-cased (an agent's link, allowed_remotes)
		// takes the project's own spelling.
		if strings.EqualFold(got.FullPath, project) {
			project = got.FullPath
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
	return out, found, 0, nil
}

// fetchRecentMRs reads one instance's recently closed list: the user's merge
// requests merged or closed within windowDays, newest first. Each row keeps
// its head pipeline's last status, read here and never polled again. An
// instance whose GraphQL lacks a field the query names is not asked again.
func fetchRecentMRs(ctx context.Context, base, token string, windowDays int, now time.Time) (authoredRecentMRs, time.Duration, error) {
	if glRecentMRsKnownMissing(base) {
		return authoredRecentMRs{}, 0, errGLUnknownField
	}
	ctx, cancel := context.WithTimeout(ctx, authoredOpenMRTimeout)
	defer cancel()
	type page struct {
		Count int            `json:"count"`
		Nodes []glOpenMRNode `json:"nodes"`
	}
	var data struct {
		CurrentUser *struct {
			Merged page `json:"merged"`
			Closed page `json:"closed"`
		} `json:"currentUser"`
	}
	after := now.Add(-time.Duration(windowDays) * 24 * time.Hour).UTC().Format(time.RFC3339)
	err := glGraphQL(ctx, base, token, authoredRecentMRQuery,
		map[string]any{"first": authoredRecentMRLimit, "after": after}, &data)
	if errors.Is(err, errGLUnknownField) {
		glRecentMRsMissing.Store(strings.ToLower(base), true)
	}
	if err != nil {
		return authoredRecentMRs{}, glRetryFrom(err), err
	}
	if data.CurrentUser == nil {
		return authoredRecentMRs{}, 0, errors.New("GitLab did not resolve the token to a user")
	}
	fetchedAt := now.UTC().Format(time.RFC3339)
	out := authoredRecentMRs{windowDays: windowDays, items: []dashboardAuthoredOpenPR{}}
	for _, list := range []struct {
		state string
		page  page
	}{{"merged", data.CurrentUser.Merged}, {"closed", data.CurrentUser.Closed}} {
		for _, node := range list.page.Nodes {
			item, ok := authoredOpenMRItem(base, node, fetchedAt)
			if !ok {
				continue
			}
			at := node.MergedAt
			if list.state == "closed" {
				at = node.ClosedAt
			}
			when, err := time.Parse(time.RFC3339, strings.TrimSpace(at))
			if err != nil {
				continue // no instant to place it in the window by
			}
			// History: no review pill, and nothing for the review cache.
			item.Reviews, item.reviewObs, item.Draft, item.Conflicts = nil, nil, false, false
			item.State, item.ClosedAt = list.state, when.UTC().Format(time.RFC3339)
			out.items = append(out.items, item)
		}
		out.truncated = out.truncated || list.page.Count > len(list.page.Nodes)
	}
	sort.SliceStable(out.items, func(i, j int) bool { return out.items[i].ClosedAt > out.items[j].ClosedAt })
	return out, 0, nil
}

// glRecentMRsMissing holds the instance bases (lower-cased) whose GraphQL
// refused the recently closed query over a field or argument it lacks.
var glRecentMRsMissing sync.Map

func glRecentMRsKnownMissing(base string) bool {
	_, missing := glRecentMRsMissing.Load(strings.ToLower(base))
	return missing
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
// and at least one instance has answered. The cached reviews are stamped on
// after the poller's state is released, so its cache read never holds up a
// poll.
func authoredOpenMRsSnapshot(cfg *config.Config, agents []dashboardAgent) *dashboardAuthoredOpenMRs {
	view := authoredOpenMRsFromState(cfg, agents)
	if view != nil {
		stampOpenMRReviews(view, agents)
	}
	return view
}

// authoredOpenMRsFromState copies the poller's last result into a view.
func authoredOpenMRsFromState(cfg *config.Config, agents []dashboardAgent) *dashboardAuthoredOpenMRs {
	policy := cfg.ResolvedGitProxy()
	key := authoredOpenMRConfigKey(authoredOpenMRBases(policy), policy.GitLabTokenFile)
	if key == "" {
		return nil
	}
	// The token FILE may have been rewritten to another account since the
	// last poll: lists read with another token, or with none now readable,
	// are not shown. One small file read per snapshot.
	token, fault := gitlabToken(policy)
	if fault != nil {
		return nil
	}
	digest := authoredOpenMRTokenDigest(token)
	authoredOpenMRState.RLock()
	defer authoredOpenMRState.RUnlock()
	if authoredOpenMRState.key != key || authoredOpenMRState.tokenDigest != digest || len(authoredOpenMRState.good) == 0 {
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
	// The recently closed list, in the narrower of the configured window and
	// the one it was read with, as for pull requests (applyRecentPRWindow):
	// a list read under a 3-day window cannot answer for 14.
	if windowDays := recentMRWindowDays(cfg); windowDays > 0 && len(authoredOpenMRState.recent) > 0 {
		for _, answer := range authoredOpenMRState.recent {
			windowDays = min(windowDays, answer.windowDays)
			view.Recent = append(view.Recent, answer.items...)
			view.RecentTruncated = view.RecentTruncated || answer.truncated
		}
		sort.SliceStable(view.Recent, func(i, j int) bool { return view.Recent[i].ClosedAt > view.Recent[j].ClosedAt })
		view.RecentWindowDays = windowDays
		view.Recent = filterRecentAuthoredPRs(view.Recent, windowDays, time.Now())
	}
	view.dashboardAuthoredOpenPRs = associateAuthoredOpenPRs(view.dashboardAuthoredOpenPRs, agents)
	view.Projects, view.EveryoneTotal, view.EveryoneTruncated = openMRProjectsSnapshot(authoredOpenMRState.everyone, agents)
	return &view
}

// stampOpenMRReviews puts the cached reviews (prreviews.go) on the footer's
// rows, so a row reads what the Branch column reads for the same merge
// request. The user's own merge requests cost one batched cache read; a
// project tab's row takes reviews only where they are already in hand, from
// that read or from an agent's badges, so everyone's merge requests add no
// read. A row with nothing cached keeps the approvals its list query gave.
func stampOpenMRReviews(view *dashboardAuthoredOpenMRs, agents []dashboardAgent) {
	urls := make([]string, 0, len(view.Items))
	for _, item := range view.Items {
		urls = append(urls, item.URL)
	}
	idx := prReviewsIndexFor(urls)
	for _, agent := range agents {
		add := func(url string, reviews *prReviewSummary) {
			if key := prStateKey(url); reviews != nil && key != "" && idx[key] == nil {
				idx[key] = reviews
			}
		}
		add(agent.BranchPRURL, agent.BranchReviews)
		add(agent.StartupPRURL, agent.StartupReviews)
		for _, presented := range agent.PresentedPRs {
			add(presented.URL, presented.Reviews)
		}
	}
	stamp := func(items []dashboardAuthoredOpenPR) {
		for i := range items {
			if cached := idx[prStateKey(items[i].URL)]; cached != nil {
				items[i].Reviews = cached
			}
		}
	}
	stamp(view.Items)
	for i := range view.Projects {
		stamp(view.Projects[i].Items)
	}
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
