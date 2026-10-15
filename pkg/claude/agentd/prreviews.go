package agentd

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// prreviews.go adds the review/approval status behind every PR badge: the
// pill after the CI indicator in the Groups tab's Branch column and in the
// footer's Open PRs popover, plus the reviewer list in its hover panel.
//
// The constraint is GitHub's rate limit, so nothing here adds a request:
//
//   - reviewDecision is a scalar on the PullRequest node. It rides every tick
//     of the authored-PR search and every per-PR `gh pr list/view` the
//     branch-link and presented-PR refreshes already make, at zero extra
//     GraphQL cost. It is null unless branch protection REQUIRES reviews,
//     though, so on many repositories it alone says nothing.
//
//   - The reviewer list (latestOpinionatedReviews) is a connection and costs
//     one extra GraphQL point per search. The authored-PR search therefore
//     asks for it at most once per authoredOpenPRReviewInterval rather than on
//     every 10s tick, and the rest of the time this cache keeps the last list
//     while still taking each fresh decision.
//
// Reviews are cached per PR identity (prStateKey), exactly like CI checks, so
// the branch, startup, presented and footer badges for one PR all show one
// answer. The snapshot path only reads the cache.

const (
	// authoredOpenPRReviewInterval is how often the authored-PR search widens
	// to include each PR's reviewer list. Six 10s ticks: an approval shows up
	// within a minute for about one extra GraphQL point per minute.
	authoredOpenPRReviewInterval = 60 * time.Second
	// prReviewsMaxReviewers bounds one PR's reviewer list in cache and snapshot.
	prReviewsMaxReviewers = 20
	// prReviewsSearchListAuthority is how long a reviewer list from the
	// authored-PR search outranks one from a per-PR `gh` call. gh only exposes
	// latestReviews, whose per-reviewer entry can be a later COMMENT that
	// hides an approval; the search reads latestOpinionatedReviews and knows
	// bots. While the search keeps refreshing its list, gh only updates the
	// decision; once the search stops covering the PR, gh's list is used.
	prReviewsSearchListAuthority = 3 * authoredOpenPRReviewInterval

	prReviewSourceSearch = "search"
	prReviewSourceGH     = "gh"
)

// authoredOpenPRReviewsFetchedAt is the unix-nano time of the last successful
// authored-PR search that included reviewer lists.
var authoredOpenPRReviewsFetchedAt atomic.Int64

func authoredOpenPRReviewsDue(now time.Time) bool {
	last := authoredOpenPRReviewsFetchedAt.Load()
	return last == 0 || now.Sub(time.Unix(0, last)) >= authoredOpenPRReviewInterval
}

// prReviewer is one reviewer's latest opinionated review.
type prReviewer struct {
	Login       string `json:"login"`
	State       string `json:"state"` // approved|changes_requested|dismissed
	Bot         bool   `json:"bot,omitempty"`
	URL         string `json:"url,omitempty"`
	SubmittedAt string `json:"submitted_at,omitempty"`
}

// prReviewSummary is both the cached blob and the snapshot shape. The
// reviewer list is small and bounded, so unlike CI checks it rides the
// snapshot directly and the hover panel needs no endpoint or GitHub call.
type prReviewSummary struct {
	// Decision is GitHub's reviewDecision: approved|changes_requested|
	// review_required, or "" when the repository requires no reviews.
	Decision string `json:"decision,omitempty"`
	// Approved and ChangesRequested count human reviewers only. Bots are
	// listed in Reviewers but never make a PR look approved.
	Approved         int `json:"approved"`
	ChangesRequested int `json:"changes_requested"`
	// ReviewersKnown distinguishes "no reviews yet" from "reviewer list not
	// fetched yet", which render differently.
	ReviewersKnown     bool         `json:"reviewers_known,omitempty"`
	Reviewers          []prReviewer `json:"reviewers,omitempty"`
	ReviewersSource    string       `json:"reviewers_source,omitempty"`
	ReviewersFetchedAt string       `json:"reviewers_fetched_at,omitempty"`
	FetchedAt          string       `json:"fetched_at,omitempty"`
}

// prReviewObservation is what one GitHub read learned about a PR's reviews.
// Reviewers is meaningful only when ReviewersFetched is set; a decision-only
// observation keeps the previously cached list.
type prReviewObservation struct {
	Decision         string
	Reviewers        []prReviewer
	ReviewersFetched bool
	Source           string
}

func prReviewsCacheKey(rawURL string) string {
	h := sha256.Sum256([]byte("pr-reviews\x00" + prStateKey(rawURL)))
	return "prr_" + hex.EncodeToString(h[:8])
}

func normalizePRReviewDecision(raw string) string {
	switch strings.ToUpper(strings.TrimSpace(raw)) {
	case "APPROVED":
		return "approved"
	case "CHANGES_REQUESTED":
		return "changes_requested"
	case "REVIEW_REQUIRED":
		return "review_required"
	default:
		return ""
	}
}

// normalizePRReviewer turns one GitHub review into a reviewer row, or reports
// false for a review that carries no opinion (COMMENTED, PENDING) or no
// usable author. Login and URL end up as text and an href in the dashboard,
// so both are constrained here rather than trusted.
func normalizePRReviewer(login, state, url, submittedAt string, bot bool) (prReviewer, bool) {
	var normalized string
	switch strings.ToUpper(strings.TrimSpace(state)) {
	case "APPROVED":
		normalized = "approved"
	case "CHANGES_REQUESTED":
		normalized = "changes_requested"
	case "DISMISSED":
		normalized = "dismissed"
	default:
		return prReviewer{}, false
	}
	login = strings.TrimSpace(login)
	if strings.HasSuffix(login, "[bot]") {
		bot = true
	}
	if !validPRReviewerLogin(login) {
		return prReviewer{}, false
	}
	url = strings.TrimSpace(url)
	if !strings.HasPrefix(url, "https://github.com/") || strings.ContainsAny(url, " \t\r\n\"'<>") {
		url = ""
	}
	if _, err := time.Parse(time.RFC3339, submittedAt); err != nil {
		submittedAt = ""
	}
	return prReviewer{Login: login, State: normalized, Bot: bot, URL: url, SubmittedAt: submittedAt}, true
}

func validPRReviewerLogin(login string) bool {
	name := strings.TrimSuffix(login, "[bot]")
	if name == "" || len(name) > 64 {
		return false
	}
	for _, r := range name {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_') {
			return false
		}
	}
	return true
}

// mergePRReviewObservation folds a fresh observation into the cached summary.
func mergePRReviewObservation(prev *prReviewSummary, obs prReviewObservation, now time.Time) prReviewSummary {
	out := prReviewSummary{Decision: obs.Decision, FetchedAt: now.Format(time.RFC3339)}
	if prev != nil {
		out.ReviewersKnown = prev.ReviewersKnown
		out.Reviewers = prev.Reviewers
		out.ReviewersSource = prev.ReviewersSource
		out.ReviewersFetchedAt = prev.ReviewersFetchedAt
	}
	if obs.ReviewersFetched && !searchListStillAuthoritative(prev, obs.Source, now) {
		reviewers := append([]prReviewer(nil), obs.Reviewers...)
		sort.SliceStable(reviewers, func(i, j int) bool {
			return reviewerRank(reviewers[i]) < reviewerRank(reviewers[j])
		})
		if len(reviewers) > prReviewsMaxReviewers {
			reviewers = reviewers[:prReviewsMaxReviewers]
		}
		out.ReviewersKnown = true
		out.Reviewers = reviewers
		out.ReviewersSource = obs.Source
		out.ReviewersFetchedAt = now.Format(time.RFC3339)
	}
	for _, r := range out.Reviewers {
		if r.Bot {
			continue
		}
		switch r.State {
		case "approved":
			out.Approved++
		case "changes_requested":
			out.ChangesRequested++
		}
	}
	return out
}

func searchListStillAuthoritative(prev *prReviewSummary, source string, now time.Time) bool {
	if prev == nil || source == prReviewSourceSearch || prev.ReviewersSource != prReviewSourceSearch {
		return false
	}
	at, err := time.Parse(time.RFC3339, prev.ReviewersFetchedAt)
	return err == nil && now.Sub(at) < prReviewsSearchListAuthority
}

// reviewerRank keeps the rows that block or unblock a merge on top: changes
// requested, approvals, then dismissed reviews; bots after humans.
func reviewerRank(r prReviewer) int {
	rank := 2
	switch r.State {
	case "changes_requested":
		rank = 0
	case "approved":
		rank = 1
	}
	if r.Bot {
		rank += 3
	}
	return rank
}

func loadPRReviews(rawURL string) (prReviewSummary, bool) {
	row, err := db.LoadGitCache(prReviewsCacheKey(rawURL))
	if err != nil || row == nil {
		return prReviewSummary{}, false
	}
	var s prReviewSummary
	if json.Unmarshal(row.Data, &s) != nil {
		return prReviewSummary{}, false
	}
	return s, true
}

// savePRReviewObservation merges and stores one observation. Best-effort, as
// savePRChecks: a failed write only means the badge waits for the next one.
func savePRReviewObservation(rawURL string, obs prReviewObservation, now time.Time) {
	if prStateKey(rawURL) == "" {
		return
	}
	var prev *prReviewSummary
	if cached, ok := loadPRReviews(rawURL); ok {
		prev = &cached
	}
	merged := mergePRReviewObservation(prev, obs, now)
	data, err := json.Marshal(merged)
	if err != nil {
		return
	}
	if err := db.SaveGitCache(prReviewsCacheKey(rawURL), data, now); err != nil {
		slog.Warn("pr-reviews: failed to cache review status",
			"error", err, "url", rawURL, "module", "agentd")
	}
}

// ghLatestReview is one element of `gh pr list/view --json latestReviews`.
type ghLatestReview struct {
	Author struct {
		Login string `json:"login"`
	} `json:"author"`
	State       string `json:"state"`
	SubmittedAt string `json:"submittedAt"`
}

// prReviewObservationFromGH builds an observation from the review fields a
// per-PR gh call returned alongside its existing ones.
func prReviewObservationFromGH(decision string, reviews []ghLatestReview) *prReviewObservation {
	obs := &prReviewObservation{
		Decision: normalizePRReviewDecision(decision), ReviewersFetched: true, Source: prReviewSourceGH,
	}
	for _, r := range reviews {
		if reviewer, ok := normalizePRReviewer(r.Author.Login, r.State, "", r.SubmittedAt, false); ok {
			obs.Reviewers = append(obs.Reviewers, reviewer)
		}
	}
	return obs
}

// prReviewsIndexFor batch-loads the review summaries for every PR URL the
// snapshot renders, keyed by prStateKey. One LoadGitCacheBatch per snapshot,
// mirroring prChecksIndexFor.
func prReviewsIndexFor(rawURLs []string) map[string]*prReviewSummary {
	stateKeyByCacheKey := make(map[string]string, len(rawURLs))
	keys := make([]string, 0, len(rawURLs))
	for _, rawURL := range rawURLs {
		stateKey := prStateKey(rawURL)
		if stateKey == "" {
			continue
		}
		cacheKey := prReviewsCacheKey(rawURL)
		if _, seen := stateKeyByCacheKey[cacheKey]; seen {
			continue
		}
		stateKeyByCacheKey[cacheKey] = stateKey
		keys = append(keys, cacheKey)
	}
	out := map[string]*prReviewSummary{}
	if len(keys) == 0 {
		return out
	}
	rows, err := db.LoadGitCacheBatch(keys)
	if err != nil {
		return out
	}
	for cacheKey, row := range rows {
		if row == nil {
			continue
		}
		var s prReviewSummary
		if json.Unmarshal(row.Data, &s) != nil {
			continue
		}
		out[stateKeyByCacheKey[cacheKey]] = &s
	}
	return out
}

// withPRReviews stamps the cached review summaries onto every badge in a row.
func (v repoLinksView) withPRReviews(idx map[string]*prReviewSummary) repoLinksView {
	if len(idx) == 0 {
		return v
	}
	v.BranchReviews = idx[prStateKey(v.BranchPRURL)]
	v.StartupReviews = idx[prStateKey(v.StartupPRURL)]
	for i := range v.PresentedPRs {
		v.PresentedPRs[i].Reviews = idx[prStateKey(v.PresentedPRs[i].URL)]
	}
	return v
}

// withAuthoredPRReviews stamps review summaries onto the footer's open list.
// Closed and merged PRs carry none: whether something already landed was
// approved is not a question the footer answers.
func withAuthoredPRReviews(view dashboardAuthoredOpenPRs, idx map[string]*prReviewSummary) dashboardAuthoredOpenPRs {
	if len(idx) == 0 {
		return view
	}
	for i := range view.Items {
		view.Items[i].Reviews = idx[prStateKey(view.Items[i].URL)]
	}
	return view
}
