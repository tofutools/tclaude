package agentd

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// prchecks.go adds the CI status behind every PR badge the dashboard
// renders: a compact n/m indicator in the Groups tab's Branch column and
// the hover panel listing each individual check.
//
// Two properties shape the design:
//
//   - The snapshot path (polled every 2s, per agent row) must not grow a
//     subprocess. So the *background* check data is never fetched on its
//     own: the check rollup rides along on the `gh pr view` calls the
//     branch-link and presented-PR refreshes already make, and the
//     snapshot only reads the resulting cache. Adding statusCheckRollup
//     to those calls costs one more JSON field, not one more request.
//
//   - A human staring at a hover panel wants the checks to move. So the
//     panel's own endpoint (GET /api/pr-checks) schedules a dedicated
//     refresh on a much shorter TTL, and the frontend re-polls it while
//     the pointer is over the badge or panel. That cost is bounded by
//     "a human is looking at exactly one PR right now".
//
// Checks are cached per *PR identity* (prStateKey), not per (repoDir,
// branch): the same PR reaches the dashboard as a branch link on one row,
// a startup link on another, and an explicitly presented PR on a third,
// and all three should show one answer.
//
// A merged or closed PR's checks are frozen — no poll, background or
// hover, ever refreshes one again (the single exception is a terminal PR
// we hold no checks for at all, which gets one fetch so the panel isn't
// permanently empty).

const (
	// prChecksTTL bounds how stale cached checks may be before the hover
	// endpoint treats them as worth re-fetching. Much shorter than
	// branchLinkTTL: a hovering human is watching a run progress, and the
	// dashboard polls this endpoint only while that hover lasts.
	prChecksHotTTL = 6 * time.Second
	// prChecksMaxRuns caps how many individual checks are cached and served
	// for one PR. Large monorepo PRs can carry hundreds; the panel scrolls,
	// but neither the cache blob nor the response should be unbounded.
	prChecksMaxRuns = 200
	// prChecksMaxNameLen truncates a single check's display strings. GitHub
	// allows long job names (matrix legs especially); the panel wraps, but a
	// pathological name should not dominate the payload.
	prChecksMaxNameLen = 160
)

// prChecksInflight single-flights hover-driven refreshes per PR, mirroring
// branchLinkInflight: a second hover poll landing while one `gh` call is
// still running is a no-op.
var prChecksInflight sync.Map

// prChecksResolver is the subprocess seam for the hover-driven refresh,
// mirroring gitInfoResolver / presentedPRInfoResolver. Production shells
// out to `gh pr view --json statusCheckRollup`; flow tests swap in a fake.
var prChecksResolver = livePRChecksResolver

// prCheckRun is one row in the hover panel: a single check run or commit
// status context. Timestamps ride as RFC3339 strings rather than a
// pre-rendered duration so the frontend can tick a still-running check's
// elapsed time between polls instead of showing the age of the cache.
type prCheckRun struct {
	Name        string `json:"name"`
	Bucket      string `json:"bucket"`                 // pass|fail|pending|skipped
	Conclusion  string `json:"conclusion,omitempty"`   // human-facing detail: success, failure, in progress, ...
	Source      string `json:"source,omitempty"`       // workflow name, or the app/context that reported it
	URL         string `json:"url,omitempty"`          // details link for this check
	StartedAt   string `json:"started_at,omitempty"`   // RFC3339
	CompletedAt string `json:"completed_at,omitempty"` // RFC3339
}

// prChecksSummary is the compact shape that rides the dashboard snapshot
// next to each PR badge — counts only, never the check list. The list is
// served on demand by /api/pr-checks so the 2s snapshot stays small even
// when a hundred agents each show a PR with fifty checks.
type prChecksSummary struct {
	Total   int `json:"total"`
	Passed  int `json:"passed"`
	Failed  int `json:"failed"`
	Pending int `json:"pending"`
	Skipped int `json:"skipped"`
	// State is the aggregate: passing|failing|pending|none. The badge
	// colours off this rather than re-deriving it from the counts.
	State     string `json:"state"`
	FetchedAt string `json:"fetched_at,omitempty"` // RFC3339
}

// prChecksInfo is the cached blob: the summary the snapshot serves plus
// the full list the hover panel serves.
type prChecksInfo struct {
	Summary prChecksSummary `json:"summary"`
	Checks  []prCheckRun    `json:"checks,omitempty"`
	// PRState is the PR's own open|merged|closed as observed by whichever
	// refresh wrote this blob. It rides here so the hover endpoint can stop
	// polling a merged PR reached through a *branch* link, which has no
	// presented-PR row to read the state from.
	PRState   string    `json:"pr_state,omitempty"`
	FetchedAt time.Time `json:"fetched_at"`
}

// prChecksCacheKey namespaces check blobs in the shared git_cache table,
// keyed by canonical PR identity so /pull/42 and /pull/42/files collapse
// (the same normalisation presentedPRCacheKey applies).
func prChecksCacheKey(rawURL string) string {
	h := sha256.Sum256([]byte("pr-checks\x00" + prStateKey(rawURL)))
	return "prc_" + hex.EncodeToString(h[:8])
}

// summarize recomputes the aggregate counts from a check list.
func summarizePRChecks(checks []prCheckRun, fetchedAt time.Time) prChecksSummary {
	s := prChecksSummary{Total: len(checks)}
	for _, c := range checks {
		switch c.Bucket {
		case "pass":
			s.Passed++
		case "fail":
			s.Failed++
		case "skipped":
			s.Skipped++
		default:
			s.Pending++
		}
	}
	switch {
	case s.Total == 0:
		s.State = "none"
	case s.Failed > 0:
		s.State = "failing"
	case s.Pending > 0:
		s.State = "pending"
	default:
		s.State = "passing"
	}
	if !fetchedAt.IsZero() {
		s.FetchedAt = fetchedAt.Format(time.RFC3339)
	}
	return s
}

// statusCheckRollupNode is the union `gh pr view --json statusCheckRollup`
// returns: GitHub Actions (and other check-suite apps) report CheckRun
// nodes, while legacy commit statuses report StatusContext nodes. The two
// carry different field names for the same three ideas — name, state,
// where to look — so both are decoded into one struct and normalised by
// normalizeRollupNode.
type statusCheckRollupNode struct {
	TypeName string `json:"__typename"`
	// CheckRun
	Name         string `json:"name"`
	Status       string `json:"status"`     // QUEUED|IN_PROGRESS|COMPLETED|WAITING|PENDING|REQUESTED
	Conclusion   string `json:"conclusion"` // SUCCESS|FAILURE|SKIPPED|NEUTRAL|CANCELLED|TIMED_OUT|ACTION_REQUIRED|STARTUP_FAILURE
	DetailsURL   string `json:"detailsUrl"`
	WorkflowName string `json:"workflowName"`
	StartedAt    string `json:"startedAt"`
	CompletedAt  string `json:"completedAt"`
	// StatusContext
	Context     string `json:"context"`
	State       string `json:"state"` // SUCCESS|PENDING|EXPECTED|FAILURE|ERROR
	TargetURL   string `json:"targetUrl"`
	Description string `json:"description"`
	CreatedAt   string `json:"createdAt"`
}

// normalizeRollupNode maps one rollup node onto a prCheckRun. ok=false for
// a node carrying no usable name — nothing to show in the panel.
//
// Bucketing follows what a human reading the badge expects, which is not
// quite GitHub's raw vocabulary: NEUTRAL counts as passing (it is
// explicitly "not a failure"), CANCELLED and TIMED_OUT count as failing (a
// run that did not finish is not a green light), and SKIPPED is its own
// bucket so it can be excluded from the badge's denominator — 12/14 should
// not read as "2 outstanding" when both were skipped by a path filter.
func normalizeRollupNode(n statusCheckRollupNode) (prCheckRun, bool) {
	run := prCheckRun{
		StartedAt:   strings.TrimSpace(n.StartedAt),
		CompletedAt: strings.TrimSpace(n.CompletedAt),
	}
	if n.TypeName == "StatusContext" || (n.Name == "" && n.Context != "") {
		run.Name = clipPRCheckText(n.Context)
		run.URL = strings.TrimSpace(n.TargetURL)
		run.Source = clipPRCheckText(n.Description)
		switch strings.ToUpper(strings.TrimSpace(n.State)) {
		case "SUCCESS":
			run.Bucket, run.Conclusion = "pass", "success"
		case "FAILURE", "ERROR":
			run.Bucket, run.Conclusion = "fail", strings.ToLower(strings.TrimSpace(n.State))
		case "":
			run.Bucket, run.Conclusion = "pending", "pending"
		default:
			run.Bucket, run.Conclusion = "pending", strings.ToLower(strings.TrimSpace(n.State))
		}
		if run.StartedAt == "" {
			run.StartedAt = strings.TrimSpace(n.CreatedAt)
		}
		return run, run.Name != ""
	}

	run.Name = clipPRCheckText(n.Name)
	run.URL = strings.TrimSpace(n.DetailsURL)
	run.Source = clipPRCheckText(n.WorkflowName)
	status := strings.ToUpper(strings.TrimSpace(n.Status))
	conclusion := strings.ToUpper(strings.TrimSpace(n.Conclusion))
	if status != "COMPLETED" && conclusion == "" {
		run.Bucket = "pending"
		run.Conclusion = prCheckPendingLabel(status)
		return run, run.Name != ""
	}
	switch conclusion {
	case "SUCCESS", "NEUTRAL":
		run.Bucket = "pass"
	case "SKIPPED":
		run.Bucket = "skipped"
	case "":
		run.Bucket = "pending"
	default:
		// FAILURE, CANCELLED, TIMED_OUT, ACTION_REQUIRED, STARTUP_FAILURE.
		run.Bucket = "fail"
	}
	run.Conclusion = strings.ToLower(strings.ReplaceAll(conclusion, "_", " "))
	if run.Conclusion == "" {
		run.Conclusion = prCheckPendingLabel(status)
	}
	return run, run.Name != ""
}

func prCheckPendingLabel(status string) string {
	switch status {
	case "IN_PROGRESS":
		return "in progress"
	case "QUEUED":
		return "queued"
	case "WAITING":
		return "waiting"
	case "REQUESTED":
		return "requested"
	case "":
		return "pending"
	default:
		return strings.ToLower(strings.ReplaceAll(status, "_", " "))
	}
}

func clipPRCheckText(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > prChecksMaxNameLen {
		return s[:prChecksMaxNameLen] + "…"
	}
	return s
}

// parseStatusCheckRollup turns the raw statusCheckRollup array into a
// cacheable blob. A PR with no checks parses to an empty, non-nil result:
// "resolved, zero checks" is a real answer the badge renders as a dash,
// distinct from "never resolved".
func parseStatusCheckRollup(raw json.RawMessage, fetchedAt time.Time) prChecksInfo {
	var nodes []statusCheckRollupNode
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &nodes)
	}
	checks := make([]prCheckRun, 0, len(nodes))
	for _, n := range nodes {
		run, ok := normalizeRollupNode(n)
		if !ok {
			continue
		}
		checks = append(checks, run)
		if len(checks) >= prChecksMaxRuns {
			break
		}
	}
	return prChecksInfo{
		Summary:   summarizePRChecks(checks, fetchedAt),
		Checks:    checks,
		FetchedAt: fetchedAt,
	}
}

// savePRChecks writes a resolved blob to the shared cache. Best-effort: a
// failed write just means the next refresh re-resolves.
func savePRChecks(rawURL string, info prChecksInfo) {
	if prStateKey(rawURL) == "" {
		return
	}
	data, err := json.Marshal(info)
	if err != nil {
		return
	}
	if err := db.SaveGitCache(prChecksCacheKey(rawURL), data, info.FetchedAt); err != nil {
		slog.Warn("pr-checks: failed to cache rollup",
			"error", err, "url", rawURL, "module", "agentd")
	}
}

func loadPRChecks(rawURL string) (prChecksInfo, bool) {
	if prStateKey(rawURL) == "" {
		return prChecksInfo{}, false
	}
	row, err := db.LoadGitCache(prChecksCacheKey(rawURL))
	if err != nil || row == nil {
		return prChecksInfo{}, false
	}
	var info prChecksInfo
	if json.Unmarshal(row.Data, &info) != nil {
		return prChecksInfo{}, false
	}
	if info.FetchedAt.IsZero() {
		info.FetchedAt = row.FetchedAt
	}
	return info, true
}

// prChecksIndexFor batch-loads the summaries for every PR URL the snapshot
// is about to render, keyed by prStateKey so the branch, startup and
// presented badges for one PR all resolve to the same entry. One
// LoadGitCacheBatch per snapshot, mirroring cachedPresentedPRStates.
func prChecksIndexFor(rawURLs []string) map[string]*prChecksSummary {
	urlByKey := make(map[string]string, len(rawURLs))
	keys := make([]string, 0, len(rawURLs))
	for _, rawURL := range rawURLs {
		stateKey := prStateKey(rawURL)
		if stateKey == "" {
			continue
		}
		cacheKey := prChecksCacheKey(rawURL)
		if _, seen := urlByKey[cacheKey]; seen {
			continue
		}
		urlByKey[cacheKey] = stateKey
		keys = append(keys, cacheKey)
	}
	if len(keys) == 0 {
		return map[string]*prChecksSummary{}
	}
	rows, err := db.LoadGitCacheBatch(keys)
	if err != nil {
		return map[string]*prChecksSummary{}
	}
	out := make(map[string]*prChecksSummary, len(rows))
	for cacheKey, row := range rows {
		if row == nil {
			continue
		}
		var info prChecksInfo
		if json.Unmarshal(row.Data, &info) != nil {
			continue
		}
		if info.Summary.Total == 0 && len(info.Checks) == 0 {
			// Resolved-but-empty: no badge to draw.
			continue
		}
		summary := info.Summary
		out[urlByKey[cacheKey]] = &summary
	}
	return out
}

// withPRChecks stamps the cached summaries onto every badge in one row.
func (v repoLinksView) withPRChecks(idx map[string]*prChecksSummary) repoLinksView {
	if len(idx) == 0 {
		return v
	}
	v.BranchChecks = idx[prStateKey(v.BranchPRURL)]
	v.StartupChecks = idx[prStateKey(v.StartupPRURL)]
	for i := range v.PresentedPRs {
		v.PresentedPRs[i].Checks = idx[prStateKey(v.PresentedPRs[i].URL)]
	}
	return v
}

// schedulePRChecksRefresh kicks one background `gh` resolution for a PR,
// deduplicated per PR identity.
func schedulePRChecksRefresh(rawURL string) {
	key := prChecksCacheKey(rawURL)
	if _, busy := prChecksInflight.LoadOrStore(key, struct{}{}); busy {
		return
	}
	goBackground(func() {
		defer prChecksInflight.Delete(key)
		info, ok := prChecksResolver(rawURL)
		if !ok {
			return
		}
		// The dedicated resolver reads the rollup, not the PR's own state;
		// keep whatever state a previous (piggybacked) write recorded rather
		// than clearing the terminal marker that stops future polls.
		if info.PRState == "" {
			if previous, had := loadPRChecks(rawURL); had {
				info.PRState = previous.PRState
			}
		}
		info.FetchedAt = time.Now()
		info.Summary = summarizePRChecks(info.Checks, info.FetchedAt)
		savePRChecks(rawURL, info)
	})
}

// livePRChecksResolver is the production resolver for a hover-driven
// refresh. Separate from the piggybacked background path because that one
// rides an existing `gh pr view`; this one is the extra call a hovering
// human explicitly asked for.
func livePRChecksResolver(rawURL string) (prChecksInfo, bool) {
	out := runInDir("", "gh", "pr", "view", strings.TrimSpace(rawURL), "--json", "state,statusCheckRollup")
	if out == "" {
		return prChecksInfo{}, false
	}
	var payload struct {
		State             string          `json:"state"`
		StatusCheckRollup json.RawMessage `json:"statusCheckRollup"`
	}
	if json.Unmarshal([]byte(out), &payload) != nil {
		return prChecksInfo{}, false
	}
	info := parseStatusCheckRollup(payload.StatusCheckRollup, time.Now())
	info.PRState = strings.ToLower(strings.TrimSpace(payload.State))
	return info, true
}

// prChecksRefreshAllowed decides whether a hover poll may spend a `gh`
// call. A merged or closed PR's checks never change again, so the badge
// keeps serving the frozen cache instead of re-polling forever. The one
// exception is a terminal PR we hold nothing for: a single fetch there is
// what stops the panel from being permanently empty for a PR that merged
// before the dashboard ever saw it.
func prChecksRefreshAllowed(rawURL string, cached prChecksInfo, haveChecks bool) bool {
	if !haveChecks {
		return true
	}
	if isTerminalPresentedPRState(cached.PRState) {
		return false
	}
	info, err := db.LoadGitCache(presentedPRCacheKey(rawURL))
	if err != nil || info == nil {
		return true
	}
	var state presentedPRInfo
	if json.Unmarshal(info.Data, &state) != nil {
		return true
	}
	return !isTerminalPresentedPRState(state.State)
}

// prChecksResponse is the hover panel's wire shape. Stale marks a served
// blob that is older than the hot TTL and has a refresh in flight — the
// frontend keeps showing it (with the age) rather than blanking, and its
// next poll picks up the newer answer.
type prChecksResponse struct {
	URL       string          `json:"url"`
	Summary   prChecksSummary `json:"summary"`
	Checks    []prCheckRun    `json:"checks"`
	FetchedAt string          `json:"fetched_at,omitempty"`
	Resolved  bool            `json:"resolved"`
	Stale     bool            `json:"stale"`
	Refreshed bool            `json:"refreshing"`
}

// handleDashboardPRChecks serves GET /api/pr-checks?url=<github pr url>.
// Cookie-authed (dashboard-only), read-only, and the only path that spends
// a subprocess on checks outside the piggybacked background refresh.
func handleDashboardPRChecks(w http.ResponseWriter, r *http.Request) {
	if !checkDashboardAuth(w, r) {
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	rawURL := strings.TrimSpace(r.URL.Query().Get("url"))
	if rawURL == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "url is required")
		return
	}
	if len(rawURL) > maxAgentPRURLLen || !isGitHubPresentedPRURL(rawURL) {
		writeError(w, http.StatusBadRequest, "bad_request", "url must be a GitHub pull request URL")
		return
	}
	info, ok := loadPRChecks(rawURL)
	stale := !ok || time.Since(info.FetchedAt) >= prChecksHotTTL
	refreshing := false
	if stale && prChecksRefreshAllowed(rawURL, info, ok) {
		schedulePRChecksRefresh(rawURL)
		refreshing = true
	}
	resp := prChecksResponse{
		URL:       rawURL,
		Summary:   info.Summary,
		Checks:    info.Checks,
		Resolved:  ok,
		Stale:     stale,
		Refreshed: refreshing,
	}
	if resp.Checks == nil {
		resp.Checks = []prCheckRun{}
	}
	if !info.FetchedAt.IsZero() {
		resp.FetchedAt = info.FetchedAt.Format(time.RFC3339)
	}
	writeJSON(w, http.StatusOK, resp)
}
