// Package github reads one composition-selected pull request and its head's
// check runs and commit statuses. It performs no GitHub mutations.
package github

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/ports"
)

const maxPages = 5
const maxResponseBytes = 4 << 20

// Config belongs to server composition. Neither the token nor API address is
// accepted from an authenticated workload's rule request.
type Config struct {
	Name        string
	Repository  string
	PullRequest uint64
	APIBaseURL  string
	Token       string
	Client      *http.Client
}

type Source struct {
	mu                               sync.Mutex
	lastPoll                         time.Time
	cached                           ports.AutomationFactBatch
	lastError                        error
	name, repository, baseURL, token string
	number                           uint64
	client                           *http.Client
}

var repositoryPart = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,99}$`)
var commitID = regexp.MustCompile(`^[a-fA-F0-9]{40}([a-fA-F0-9]{24})?$`)

func New(c Config) (*Source, error) {
	parts := strings.Split(c.Repository, "/")
	if !repositoryPart.MatchString(c.Name) || len(parts) != 2 || !repositoryPart.MatchString(parts[0]) || !repositoryPart.MatchString(parts[1]) || c.PullRequest == 0 {
		return nil, errors.New("named GitHub source requires an exact owner/repository and pull request number")
	}
	if strings.ContainsAny(c.Token, "\r\n") {
		return nil, errors.New("invalid GitHub credential")
	}
	base := c.APIBaseURL
	if base == "" {
		base = "https://api.github.com"
	}
	u, err := url.Parse(base)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Scheme != "https" && (u.Scheme != "http" || (u.Hostname() != "127.0.0.1" && u.Hostname() != "localhost" && u.Hostname() != "::1"))) {
		return nil, errors.New("GitHub API address must be HTTPS or a loopback fixture")
	}
	client := http.Client{Timeout: 20 * time.Second}
	if c.Client != nil {
		client = *c.Client
	}
	// A redirect must not move the configured source or its credential elsewhere.
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &Source{name: c.Name, repository: strings.ToLower(c.Repository), number: c.PullRequest, baseURL: strings.TrimRight(base, "/"), token: c.Token, client: &client}, nil
}

func (s *Source) SourceID() string { return s.name }

func (s *Source) CollectAutomationFacts(ctx context.Context, req ports.AutomationFactCollectRequest) (ports.AutomationFactBatch, error) {
	if req.Resource.Kind != model.FactResourceRepositoryPullReq || req.Resource.ID != "" || req.Resource.Repository != s.repository || req.Resource.PullRequest != s.number {
		return ports.AutomationFactBatch{}, errors.New("GitHub fact target does not match the configured source")
	}
	if req.Now.IsZero() || req.Limit < 2 {
		return ports.AutomationFactBatch{}, errors.New("GitHub collection requires observation time and capacity for two facts")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.lastPoll.IsZero() && req.Now.Sub(s.lastPoll) < time.Minute {
		return copyBatch(s.cached), s.lastError
	}
	s.lastPoll = req.Now
	batch, err := s.collect(ctx, req)
	s.cached, s.lastError = batch, err
	return copyBatch(batch), err
}

func copyBatch(batch ports.AutomationFactBatch) ports.AutomationFactBatch {
	batch.Facts = append([]model.NormalizedFact(nil), batch.Facts...)
	return batch
}

func (s *Source) collect(ctx context.Context, req ports.AutomationFactCollectRequest) (ports.AutomationFactBatch, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	pr, err := s.pull(ctx)
	if err != nil {
		return ports.AutomationFactBatch{}, err
	}
	checks, err := s.checks(ctx, pr.Head.SHA)
	if err != nil {
		return ports.AutomationFactBatch{}, err
	}
	statuses, err := s.statuses(ctx, pr.Head.SHA)
	if err != nil {
		return ports.AutomationFactBatch{}, err
	}
	// Check the head again after collecting CI. An interleaved force-push cannot
	// attach old-commit success to the new pull request snapshot.
	confirmed, err := s.pull(ctx)
	if err != nil {
		return ports.AutomationFactBatch{}, err
	}
	if pr != confirmed {
		return ports.AutomationFactBatch{}, errors.New("pull request changed while collecting CI")
	}
	sort.Slice(checks, func(i, j int) bool { return checks[i].ID < checks[j].ID })
	sort.Slice(statuses, func(i, j int) bool { return statuses[i].ID < statuses[j].ID })
	prValue := pr.State
	if pr.Merged {
		prValue = "merged"
	} else if pr.Draft && pr.State == "open" {
		prValue = "draft"
	}
	ciValue, changed := ciState(checks, statuses)
	resource := req.Resource
	facts := []model.NormalizedFact{
		{Source: s.name, EventID: digest(pr), Kind: model.FactPullRequestChanged, Value: prValue, Resource: resource, OccurredAt: pr.UpdatedAt, ObservedAt: req.Now.UTC()},
		{Source: s.name, EventID: digest(struct {
			Head     string
			Checks   []check
			Statuses []status
		}{pr.Head.SHA, checks, statuses}), Kind: model.FactCICompleted, Value: ciValue, Resource: resource, OccurredAt: changed, ObservedAt: req.Now.UTC()},
	}
	if facts[1].OccurredAt.IsZero() {
		facts[1].OccurredAt = pr.UpdatedAt
	}
	// The source returns one complete snapshot; pagination is consumed privately.
	return ports.AutomationFactBatch{Facts: facts}, nil
}

func digest(v any) string {
	b, _ := json.Marshal(v)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

type pull struct {
	Number    uint64    `json:"number"`
	State     string    `json:"state"`
	Draft     bool      `json:"draft"`
	Merged    bool      `json:"merged"`
	UpdatedAt time.Time `json:"updated_at"`
	Head      struct {
		SHA string `json:"sha"`
	} `json:"head"`
	Base struct {
		Repo struct {
			FullName string `json:"full_name"`
		} `json:"repo"`
	} `json:"base"`
}

func (s *Source) pull(ctx context.Context) (pull, error) {
	var p pull
	err := s.get(ctx, fmt.Sprintf("/repos/%s/pulls/%d", s.repository, s.number), &p)
	if err != nil {
		return p, err
	}
	if p.Number != s.number || strings.ToLower(p.Base.Repo.FullName) != s.repository || !commitID.MatchString(p.Head.SHA) || p.UpdatedAt.IsZero() || (p.State != "open" && p.State != "closed") {
		return pull{}, errors.New("GitHub returned an invalid pull request identity or state")
	}
	return p, nil
}

type check struct {
	ID          int64     `json:"id"`
	Status      string    `json:"status"`
	Conclusion  string    `json:"conclusion"`
	StartedAt   time.Time `json:"started_at"`
	CompletedAt time.Time `json:"completed_at"`
}
type status struct {
	ID        int64     `json:"id"`
	Context   string    `json:"context"`
	State     string    `json:"state"`
	UpdatedAt time.Time `json:"updated_at"`
}

func (s *Source) checks(ctx context.Context, sha string) ([]check, error) {
	var all []check
	var total int
	seen := map[int64]bool{}
	for page := 1; page <= maxPages; page++ {
		var out struct {
			Total *int    `json:"total_count"`
			Runs  []check `json:"check_runs"`
		}
		if err := s.get(ctx, fmt.Sprintf("/repos/%s/commits/%s/check-runs?filter=latest&per_page=100&page=%d", s.repository, sha, page), &out); err != nil {
			return nil, err
		}
		if out.Total == nil {
			return nil, errors.New("GitHub response omits coverage")
		}
		if page == 1 {
			total = *out.Total
		}
		if *out.Total != total || total < 0 || total > maxPages*100 || out.Runs == nil || len(out.Runs) > 100 {
			return nil, errors.New("GitHub check-run coverage is incomplete or changed")
		}
		for _, run := range out.Runs {
			if run.ID <= 0 || seen[run.ID] || run.Status == "" {
				return nil, errors.New("GitHub check-run identity is invalid or duplicated")
			}
			seen[run.ID] = true
			all = append(all, run)
		}
		if len(all) == total {
			return all, nil
		}
		if len(out.Runs) < 100 {
			return nil, errors.New("GitHub check-run page is incomplete")
		}
	}
	return nil, errors.New("GitHub check-run coverage exceeds the bounded collector")
}
func (s *Source) statuses(ctx context.Context, sha string) ([]status, error) {
	var all []status
	var total int
	seen := map[int64]bool{}
	for page := 1; page <= maxPages; page++ {
		var out struct {
			SHA      string   `json:"sha"`
			Total    *int     `json:"total_count"`
			Statuses []status `json:"statuses"`
		}
		if err := s.get(ctx, fmt.Sprintf("/repos/%s/commits/%s/status?per_page=100&page=%d", s.repository, sha, page), &out); err != nil {
			return nil, err
		}
		if out.Total == nil {
			return nil, errors.New("GitHub response omits coverage")
		}
		if page == 1 {
			total = *out.Total
		}
		if !strings.EqualFold(out.SHA, sha) || *out.Total != total || total < 0 || total > maxPages*100 || out.Statuses == nil || len(out.Statuses) > 100 {
			return nil, errors.New("GitHub commit-status coverage is incomplete or changed")
		}
		for _, value := range out.Statuses {
			if value.ID <= 0 || seen[value.ID] || value.Context == "" || value.UpdatedAt.IsZero() {
				return nil, errors.New("GitHub commit-status identity is invalid or duplicated")
			}
			seen[value.ID] = true
			all = append(all, value)
		}
		if len(all) == total {
			return all, nil
		}
		if len(out.Statuses) < 100 {
			return nil, errors.New("GitHub commit-status page is incomplete")
		}
	}
	return nil, errors.New("GitHub commit-status coverage exceeds the bounded collector")
}

func ciState(checks []check, statuses []status) (string, time.Time) {
	if len(checks)+len(statuses) == 0 {
		return "unknown", time.Time{}
	}
	value := "succeeded"
	var latest time.Time
	rank := map[string]int{"succeeded": 0, "failed": 1, "pending": 2, "unknown": 3}
	add := func(state string, at time.Time) {
		if rank[state] > rank[value] {
			value = state
		}
		if at.After(latest) {
			latest = at
		}
	}
	for _, c := range checks {
		state := "unknown"
		switch c.Status {
		case "queued", "in_progress", "waiting", "pending", "requested":
			state = "pending"
		case "completed":
			switch c.Conclusion {
			case "success", "neutral", "skipped":
				state = "succeeded"
			case "failure", "cancelled", "timed_out", "action_required", "stale", "startup_failure":
				state = "failed"
			}
			if c.CompletedAt.IsZero() {
				state = "unknown"
			}
		}
		at := c.CompletedAt
		if at.IsZero() {
			at = c.StartedAt
		}
		add(state, at)
	}
	for _, s := range statuses {
		state := "unknown"
		switch s.State {
		case "success":
			state = "succeeded"
		case "pending":
			state = "pending"
		case "failure", "error":
			state = "failed"
		}
		add(state, s.UpdatedAt)
	}
	return value, latest
}

func (s *Source) get(ctx context.Context, path string, out any) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, s.baseURL+path, nil)
	if err != nil {
		return errors.New("invalid GitHub request")
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if s.token != "" {
		request.Header.Set("Authorization", "Bearer "+s.token)
	}
	response, err := s.client.Do(request)
	if err != nil {
		return fmt.Errorf("GitHub source read failed: %w", ctxOrReadError(ctx))
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("GitHub source returned HTTP %d", response.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil {
		return errors.New("GitHub response read failed")
	}
	if len(data) > maxResponseBytes {
		return errors.New("GitHub response exceeds byte bound")
	}
	if err = json.Unmarshal(data, out); err != nil {
		return errors.New("GitHub response is not valid expected JSON")
	}
	return nil
}
func ctxOrReadError(ctx context.Context) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return errors.New("transport error")
}
