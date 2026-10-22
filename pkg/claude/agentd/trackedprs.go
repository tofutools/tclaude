package agentd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// trackedprs.go backs the colleague tabs of the footer's PR popover: the
// operator names other GitHub users and the popover lists their open (and
// recently closed) pull requests next to the operator's own.
//
// Cost is the design constraint. Each tracked author gets one GraphQL request
// per tick — an open search, plus an aliased recently closed search when that
// window is on — on a slower cadence than the operator's own list, which also
// drives Groups badges. The requests are deliberately not aliased into one:
// GitHub cuts a GraphQL request off at 10s of server time, one author's search
// with check rollups already costs ~5–6s, so two or more authors in one
// request failed every tick with a 502. Per-author requests also let one slow
// or refused author keep its last good list while the others refresh. They
// run at most trackedPRSearchParallelism at a time and are staggered across
// the interval, since GitHub's secondary rate limits count server CPU time and
// discourage bursts of concurrent expensive searches. GitHub search only
// returns pull requests the operator's gh credential can see, so "PRs we can
// reach" needs no extra filtering.
//
// The list is daemon state, kept in the dashboard_prefs table under a daemon-
// owned key and edited only through POST /api/open-prs/tracked-authors. It is
// deliberately not a config.json knob: the Config tab saves the whole config
// object it loaded, which would silently drop an author added in the
// meantime.

const (
	trackedPRAuthorsPrefKey = "tclaude.daemon.open-prs.tracked-authors"
	trackedPRAuthorsMax     = 8
	trackedPRPollInterval   = 60 * time.Second
	trackedPRMaxBackoff     = 10 * time.Minute
	trackedPROpenLimit      = 50
	// trackedPRDegradedOpenLimit is the smaller open page an author whose
	// search timed out on GitHub gets for trackedPRDegradedFor: the check
	// rollups of every PR on the page are what make a prolific author's
	// search run past GitHub's 10s limit.
	trackedPRDegradedOpenLimit = 20
	trackedPRDegradedFor       = time.Hour
	trackedPRRecentLimit       = 30
	trackedPRCachePrefix       = "tracked_author_prs_v1_"
	// trackedPRSearchTimeout bounds one author's request. GitHub gives up at
	// ~10s server-side; the rest covers network latency.
	trackedPRSearchTimeout = 20 * time.Second
	// trackedPRSearchParallelism caps the per-author requests in flight.
	trackedPRSearchParallelism = 2
	// trackedPRSearchSpread is the window a regular tick spreads its request
	// starts over; a kicked tick (an author was just added) does not wait, and
	// a kick during a regular tick drops the rest of its spacing.
	trackedPRSearchSpread = trackedPRPollInterval / 2
)

var (
	trackedPRResolver = liveTrackedPRResolver
	// trackedPRUserLookup reports whether a GitHub user exists; known is
	// false when GitHub could not be asked (offline, logged out).
	trackedPRUserLookup = liveTrackedPRUserLookup
	// trackedPRGraphQL runs one author's search and returns its stdout even
	// when gh exits non-zero; tests swap in a canned GitHub response.
	trackedPRGraphQL = liveTrackedPRGraphQL
	// trackedPRKick wakes the poller when an author is added, so the new tab
	// fills within a second instead of up to a minute later.
	trackedPRKick             = make(chan struct{}, 1)
	trackedPRReviewsFetchedAt atomic.Int64
	// trackedPRAuthorsEditMu serializes the endpoint's load-modify-save, so
	// two quick adds cannot drop one another.
	trackedPRAuthorsEditMu       sync.Mutex
	errTrackedPRNoActiveIdentity = fmt.Errorf("resolve active GitHub login")
)

// loadTrackedPRAuthors returns the tracked logins in the order they were
// added. The stored value is re-validated on every read.
func loadTrackedPRAuthors() []string {
	raw, ok, err := db.GetDashboardPref(trackedPRAuthorsPrefKey)
	if err != nil || !ok {
		return nil
	}
	var stored []string
	if json.Unmarshal([]byte(raw), &stored) != nil {
		return nil
	}
	return normalizeTrackedPRAuthors(stored)
}

func normalizeTrackedPRAuthors(logins []string) []string {
	out := make([]string, 0, min(len(logins), trackedPRAuthorsMax))
	seen := map[string]bool{}
	for _, login := range logins {
		login = strings.TrimSpace(login)
		if !isGitHubOwnerSlug(login) || seen[strings.ToLower(login)] {
			continue
		}
		seen[strings.ToLower(login)] = true
		out = append(out, login)
		if len(out) == trackedPRAuthorsMax {
			break
		}
	}
	return out
}

func saveTrackedPRAuthors(logins []string) error {
	if len(logins) == 0 {
		return db.DeleteDashboardPref(trackedPRAuthorsPrefKey)
	}
	data, err := json.Marshal(logins)
	if err != nil {
		return err
	}
	return db.SetDashboardPref(trackedPRAuthorsPrefKey, string(data))
}

func trackedPRCacheKey(self, author string) string {
	return trackedPRCachePrefix + strings.ToLower(self) + "_" + strings.ToLower(author)
}

func startTrackedPRAuthorsPoller(stop <-chan struct{}) {
	go func() {
		timer := time.NewTimer(0)
		defer timer.Stop()
		failures := 0
		authorFailures := map[string]int{}
		for {
			spread := trackedPRSearchSpread
			select {
			case <-stop:
				return
			case <-trackedPRKick:
				spread = 0
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
			case <-timer.C:
			}
			started := time.Now()
			authors, failed, err := pollTrackedPRAuthors(spread, stop)
			logTrackedPRAuthorFailures(authorFailures, authors, failed, err)
			// The interval runs from the tick's start, so the spacing does not
			// stretch the cadence.
			delay := max(trackedPRPollInterval-time.Since(started), trackedPRPollInterval/4)
			if errors.Is(err, errGitHubBackoff) {
				// Paused by the shared rate-limit backoff, which logged it:
				// wait it out rather than counting a failure.
				remaining, _ := gitHubBackoffRemaining(time.Now())
				delay = max(delay, remaining)
			} else if err != nil {
				failures++
				delay = trackedPRRetryDelay(failures)
				if failures == 1 || delay > trackedPRRetryDelay(failures-1) {
					slog.Warn("tracked-prs: GitHub search failed; keeping last good cache",
						"error", err, "kind", classifyGHError(err), "consecutive_failures", failures,
						"authors", authors, "duration", time.Since(started).Round(time.Millisecond),
						"retry_in", delay, "module", "agentd")
				}
			} else if failures > 0 {
				slog.Info("tracked-prs: GitHub search recovered",
					"previous_failures", failures, "authors", authors, "module", "agentd")
				failures = 0
			}
			timer.Reset(delay)
		}
	}()
}

// logTrackedPRAuthorFailures logs one author's failure streak when it starts
// and when it ends, while other authors still refresh. A tick where every
// author failed (err != nil) is the poller's own failure line instead.
func logTrackedPRAuthorFailures(streaks map[string]int, authors []string, failed map[string]error, err error) {
	if err != nil {
		return
	}
	tracked := make(map[string]bool, len(authors))
	for _, author := range authors {
		key := strings.ToLower(author)
		tracked[key] = true
		ferr, bad := failed[author]
		if bad && !trackedPRFailureCounts(ferr) {
			continue
		}
		if bad {
			streaks[key]++
			if streaks[key] == 1 {
				slog.Warn("tracked-prs: GitHub search failed for one author; keeping its last good list",
					"author", author, "error", ferr, "kind", classifyGHError(ferr), "module", "agentd")
			}
		} else if streaks[key] > 0 {
			slog.Info("tracked-prs: GitHub search recovered for one author",
				"author", author, "previous_failures", streaks[key], "module", "agentd")
			delete(streaks, key)
		}
	}
	for key := range streaks {
		if !tracked[key] {
			delete(streaks, key)
		}
	}
}

// trackedPRFailureCounts reports whether an author's error is a failed
// search, as opposed to a search not sent this tick: deferred after repeated
// timeouts, paused by the shared rate-limit backoff, or cut by shutdown.
func trackedPRFailureCounts(err error) bool {
	return !errors.Is(err, errTrackedPRAuthorDeferred) && !errors.Is(err, errGitHubBackoff) &&
		!errors.Is(err, errTrackedPRTickStopped)
}

func trackedPRRetryDelay(failures int) time.Duration {
	if failures <= 0 {
		return trackedPRPollInterval
	}
	return min(trackedPRPollInterval<<min(failures, 4), trackedPRMaxBackoff)
}

func kickTrackedPRAuthorsPoller() {
	select {
	case trackedPRKick <- struct{}{}:
	default:
	}
}

// trackedPRAuthorResult is one author's search outcome. err is a failed
// request (a timeout, a 5xx, the network): that author keeps its cached list
// untouched. A refusal GitHub answered is view.Error instead.
type trackedPRAuthorResult struct {
	view dashboardAuthoredOpenPRs
	err  error
}

// pollTrackedPRAuthors searches every tracked author that is due (see
// trackedPRSlowAuthors), starting the requests across spread (see
// trackedPRStartPacer), and caches each author's list under the active
// identity, the same namespacing as the operator's own list so a gh account
// switch never shows the old account's view. It returns the tracked authors
// and the error of each one not refreshed; err is set only when the whole
// tick failed — no identity, or every request sent failed.
func pollTrackedPRAuthors(spread time.Duration, stop <-chan struct{}) (authors []string, failed map[string]error, err error) {
	authors = loadTrackedPRAuthors()
	if len(authors) == 0 {
		return nil, nil, nil
	}
	self, ok := cachedGitHubLogin()
	if !ok {
		return authors, nil, errTrackedPRNoActiveIdentity
	}
	cfg, _ := config.Load()
	now := time.Now()
	reviewsMark := trackedPRReviewsFetchedAt.Load()
	withReviews := trackedPRReviewsDue(now)
	due, openLimits := planTrackedPRSearches(authors, now)
	failed = map[string]error{}
	for _, author := range authors {
		if _, ok := openLimits[author]; !ok {
			failed[author] = errTrackedPRAuthorDeferred
		}
	}
	if len(due) == 0 {
		return authors, failed, nil
	}
	results := trackedPRResolver(due, openLimits, cfg.RecentPRWindowDays(), now, withReviews,
		trackedPRStartPacer(spread/time.Duration(len(due)), stop))
	sentFailed := 0
	for _, result := range results {
		recordTrackedPRSearch(result.view.Login, result.err, now)
		if result.err != nil {
			failed[result.view.Login] = result.err
			sentFailed++
		}
	}
	if sentFailed == len(results) {
		return authors, failed, results[0].err
	}
	if withReviews {
		// An add during this search reset the mark to 0; keep that reset so
		// the kicked poll fetches the new author's reviewers.
		trackedPRReviewsFetchedAt.CompareAndSwap(reviewsMark, now.UnixNano())
	}
	for _, result := range results {
		if result.err != nil {
			continue
		}
		view := result.view
		if view.Error != "" {
			// Keep an earlier good list, flagged with the refusal, rather
			// than blanking the tab.
			if row, err := db.LoadGitCache(trackedPRCacheKey(self, view.Login)); err == nil && row != nil {
				var cached dashboardAuthoredOpenPRs
				if json.Unmarshal(row.Data, &cached) == nil && cached.Available {
					cached.Error = view.Error
					view = cached
				}
			}
		} else {
			view.Available = true
			shareAuthoredPRView(view, now)
		}
		if view.Error == "" || !view.Available {
			view.UpdatedAt = now.Format(time.RFC3339)
		}
		data, err := json.Marshal(view)
		if err != nil {
			continue
		}
		if err := db.SaveGitCache(trackedPRCacheKey(self, view.Login), data, now); err != nil {
			slog.Warn("tracked-prs: failed to cache author list",
				"error", err, "author", view.Login, "module", "agentd")
		}
	}
	return authors, failed, nil
}

func trackedPRReviewsDue(now time.Time) bool {
	last := trackedPRReviewsFetchedAt.Load()
	return last == 0 || now.Sub(time.Unix(0, last)) >= authoredOpenPRReviewInterval
}

// trackedPRSearchArgs builds the `gh api graphql` argv for the given authors:
// the shared open and recently closed fragments, aliased u<i>/r<i> per author.
// The poller passes one author per request (see the file comment).
func trackedPRSearchArgs(authors []string, openLimit, windowDays int, now time.Time, withReviews bool) []string {
	vars := []string{"$first:Int!"}
	if windowDays > 0 {
		vars = append(vars, "$rfirst:Int!")
	}
	var body strings.Builder
	args := []string{"api", "--hostname", "github.com", "graphql"}
	var values []string
	since := now.UTC().AddDate(0, 0, -windowDays).Format("2006-01-02")
	for i, author := range authors {
		n := strconv.Itoa(i)
		vars = append(vars, "$q"+n+":String!")
		body.WriteString(strings.Replace(authoredOpenPRSearchFragment,
			"search(query:$q,", "u"+n+": search(query:$q"+n+",", 1))
		body.WriteString("\n")
		values = append(values, "-f", "q"+n+"=author:"+author+" is:open type:pr sort:updated-desc")
		if windowDays > 0 {
			vars = append(vars, "$qr"+n+":String!")
			body.WriteString(strings.Replace(authoredRecentPRSearchFragment,
				"recent: search(query:$qr,", "r"+n+": search(query:$qr"+n+",", 1))
			body.WriteString("\n")
			values = append(values, "-f", "qr"+n+"=author:"+author+" is:closed type:pr closed:>="+since+" sort:updated-desc")
		}
	}
	query := "query(" + strings.Join(vars, ",") + "){\n" + body.String() + "}"
	if withReviews {
		query = strings.ReplaceAll(query, "updatedAt reviewDecision", "updatedAt reviewDecision"+authoredOpenPRReviewsSelection)
	}
	args = append(args, "-f", "query="+query, "-F", "first="+strconv.Itoa(openLimit))
	if windowDays > 0 {
		args = append(args, "-F", "rfirst="+strconv.Itoa(trackedPRRecentLimit))
	}
	return append(args, values...)
}

// errTrackedPRTickStopped marks the authors a tick skipped at shutdown.
var errTrackedPRTickStopped = fmt.Errorf("tracked PR poll stopped")

// errTrackedPRAuthorDeferred marks an author whose search is backed off after
// repeated timeouts.
var errTrackedPRAuthorDeferred = fmt.Errorf("tracked PR search deferred after repeated timeouts")

// trackedPRSlowAuthors remembers authors whose search times out on GitHub.
// Retrying such a request as-is cannot succeed, and every attempt still
// counts toward GitHub's secondary rate limits, which charge server time. So
// a timeout drops the author's open page to trackedPRDegradedOpenLimit for
// trackedPRDegradedFor, and a second timeout in a row defers their next
// search with a backoff that doubles up to trackedPRMaxBackoff. The other
// authors keep their normal cadence.
var trackedPRSlowAuthors = struct {
	sync.Mutex
	byLogin map[string]*trackedPRSlowAuthor
}{byLogin: map[string]*trackedPRSlowAuthor{}}

type trackedPRSlowAuthor struct {
	timeouts      int
	deferUntil    time.Time
	degradedUntil time.Time
}

// planTrackedPRSearches returns the authors due a search this tick and the
// open page size for each of them.
func planTrackedPRSearches(authors []string, now time.Time) ([]string, map[string]int) {
	trackedPRSlowAuthors.Lock()
	defer trackedPRSlowAuthors.Unlock()
	tracked := make(map[string]bool, len(authors))
	due := make([]string, 0, len(authors))
	limits := make(map[string]int, len(authors))
	for _, author := range authors {
		key := strings.ToLower(author)
		tracked[key] = true
		slow := trackedPRSlowAuthors.byLogin[key]
		if slow != nil && now.Before(slow.deferUntil) {
			continue
		}
		due = append(due, author)
		limits[author] = trackedPROpenLimit
		if slow != nil && now.Before(slow.degradedUntil) {
			limits[author] = trackedPRDegradedOpenLimit
		}
	}
	for key := range trackedPRSlowAuthors.byLogin {
		if !tracked[key] {
			delete(trackedPRSlowAuthors.byLogin, key)
		}
	}
	return due, limits
}

// recordTrackedPRSearch updates an author's timeout state from one search.
func recordTrackedPRSearch(author string, err error, now time.Time) {
	key := strings.ToLower(author)
	trackedPRSlowAuthors.Lock()
	defer trackedPRSlowAuthors.Unlock()
	slow := trackedPRSlowAuthors.byLogin[key]
	switch kind := classifyGHError(err); {
	case err == nil:
		if slow != nil {
			slow.timeouts = 0
			if !now.Before(slow.degradedUntil) {
				delete(trackedPRSlowAuthors.byLogin, key)
			}
		}
	case kind == ghErrGitHubTimeout || kind == ghErrGitHub5xx || kind == ghErrTimeoutLocal:
		if slow == nil {
			slow = &trackedPRSlowAuthor{}
			trackedPRSlowAuthors.byLogin[key] = slow
		}
		slow.timeouts++
		slow.degradedUntil = now.Add(trackedPRDegradedFor)
		if slow.timeouts >= 2 {
			slow.deferUntil = now.Add(min(trackedPRPollInterval<<min(slow.timeouts-1, 4), trackedPRMaxBackoff))
		}
	}
}

// trackedPRStartPacer waits step between a tick's request starts. A kick (an
// author was just added) ends the waiting for the rest of the tick and is
// re-queued, so the tick that searches the new author follows right after
// this one. It returns false once stop closes: skip the remaining requests.
func trackedPRStartPacer(step time.Duration, stop <-chan struct{}) func() bool {
	return func() bool {
		if step <= 0 {
			return true
		}
		timer := time.NewTimer(step)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-trackedPRKick:
			step = 0
			kickTrackedPRAuthorsPoller()
		case <-stop:
			return false
		}
		return true
	}
}

// liveTrackedPRResolver sends one request per author, asking for
// openLimits[author] open PRs, at most trackedPRSearchParallelism in flight,
// calling beforeNext between starts. Results come back in authors' order.
func liveTrackedPRResolver(authors []string, openLimits map[string]int, windowDays int, now time.Time, withReviews bool, beforeNext func() bool) []trackedPRAuthorResult {
	results := make([]trackedPRAuthorResult, len(authors))
	slots := make(chan struct{}, trackedPRSearchParallelism)
	var wg sync.WaitGroup
	for i, author := range authors {
		if i > 0 && !beforeNext() {
			for j := i; j < len(authors); j++ {
				results[j] = trackedPRAuthorResult{view: dashboardAuthoredOpenPRs{Login: authors[j]}, err: errTrackedPRTickStopped}
			}
			break
		}
		slots <- struct{}{}
		wg.Go(func() {
			defer func() { <-slots }()
			results[i] = searchTrackedPRAuthor(author, openLimits[author], windowDays, now, withReviews)
		})
	}
	wg.Wait()
	return results
}

func searchTrackedPRAuthor(author string, openLimit, windowDays int, now time.Time, withReviews bool) trackedPRAuthorResult {
	// gh exits non-zero when GraphQL reports any error, even with partial
	// data — a tracked login that no longer exists is exactly that — so the
	// output is decoded whether or not the call failed.
	authors := []string{author}
	out, runErr := trackedPRGraphQL(trackedPRSearchArgs(authors, openLimit, windowDays, now, withReviews))
	views, err := decodeTrackedPRGraphQL(out, authors, openLimit, windowDays)
	if err != nil {
		if runErr != nil {
			err = runErr
		}
		return trackedPRAuthorResult{view: dashboardAuthoredOpenPRs{Login: author}, err: err}
	}
	return trackedPRAuthorResult{view: views[0]}
}

// decodeTrackedPRGraphQL splits the aliased response into one list per author
// by re-shaping each alias pair into the operator search's response and
// running the shared decoder. An error whose path names one alias marks only
// that author; any other error fails the tick.
func decodeTrackedPRGraphQL(data []byte, authors []string, openLimit, windowDays int) ([]dashboardAuthoredOpenPRs, error) {
	var payload struct {
		Data   map[string]json.RawMessage `json:"data"`
		Errors []struct {
			Message string `json:"message"`
			Path    []any  `json:"path"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(data), &payload); err != nil {
		return nil, fmt.Errorf("decode tracked PR search: %w", err)
	}
	refused := map[string]string{}
	for _, e := range payload.Errors {
		alias, _ := firstPathElement(e.Path)
		switch {
		case alias == "":
			return nil, fmt.Errorf("tracked PR search: %s", e.Message)
		case len(e.Path) == 1:
			// The whole search was refused (e.g. a login that no longer
			// exists). A deeper path is one PR's field — a repository behind
			// SSO, say — and the rest of that author's list still decodes.
			refused[strings.TrimLeft(alias, "ur")] = e.Message
		}
	}
	// No data at all is an HTTP-level failure (bad credentials, a 5xx body):
	// fail the tick so every tab keeps its last good list.
	if payload.Data == nil {
		return nil, fmt.Errorf("tracked PR search returned no data")
	}
	present := func(key string) bool {
		raw := bytes.TrimSpace(payload.Data[key])
		return len(raw) > 0 && !bytes.Equal(raw, []byte("null"))
	}
	views := make([]dashboardAuthoredOpenPRs, 0, len(authors))
	for i, author := range authors {
		n := strconv.Itoa(i)
		if msg, bad := refused[n]; bad {
			views = append(views, dashboardAuthoredOpenPRs{
				Login: author, Items: []dashboardAuthoredOpenPR{}, Recent: []dashboardAuthoredOpenPR{},
				Error: truncateAuthoredPRTitle(msg),
			})
			continue
		}
		if !present("u"+n) || (windowDays > 0 && !present("r"+n)) {
			return nil, fmt.Errorf("tracked PR search returned no result for %s", author)
		}
		recent := payload.Data["r"+n]
		if len(recent) == 0 {
			recent = json.RawMessage("null")
		}
		shaped, err := json.Marshal(map[string]any{"data": map[string]json.RawMessage{
			"search": payload.Data["u"+n], "recent": recent,
		}})
		if err != nil {
			return nil, err
		}
		view, checksByURL, err := decodeAuthoredOpenPRGraphQL(shaped, author)
		if err != nil {
			return nil, err
		}
		view.Truncated = view.Total > openLimit
		view.RecentWindowDays = windowDays
		if windowDays > 0 {
			view.RecentTruncated = len(view.Recent) >= trackedPRRecentLimit
		} else {
			view.RecentTruncated = false
		}
		for rawURL, info := range checksByURL {
			savePRChecks(rawURL, info)
		}
		views = append(views, view)
	}
	return views, nil
}

func liveTrackedPRGraphQL(args []string) (out []byte, err error) {
	if err := gitHubBackoffGate(args); err != nil {
		return nil, err
	}
	defer func() { noteGitHubResult(args, err) }()
	ctx, cancel := context.WithTimeout(context.Background(), trackedPRSearchTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "gh", args...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err = cmd.Output()
	if err != nil {
		// Keep gh's stderr ("gh: HTTP 502") and the error page's title: when
		// stdout does not decode, this error is all the poller can log.
		return out, subprocessError(ctx, trackedPRSearchTimeout, err, stderr.String(), out)
	}
	return out, nil
}

func liveTrackedPRUserLookup(login string) (exists, known bool) {
	_, err := runInDirWithError("", "gh", "api", "--hostname", "github.com", "users/"+login, "--jq", ".login")
	if err == nil {
		return true, true
	}
	if strings.Contains(err.Error(), "Not Found") || strings.Contains(err.Error(), "HTTP 404") {
		return false, true
	}
	return false, false
}

func firstPathElement(path []any) (string, bool) {
	if len(path) == 0 {
		return "", false
	}
	s, ok := path[0].(string)
	return s, ok
}

// loadTrackedPRAuthorsSnapshot returns one list per tracked author, in order,
// for the active identity. An author not searched yet is listed unavailable,
// so the popover can show its tab while the first search runs.
func loadTrackedPRAuthorsSnapshot(cfg *config.Config) []dashboardAuthoredOpenPRs {
	authors := loadTrackedPRAuthors()
	if len(authors) == 0 {
		return nil
	}
	authoredOpenPRActiveLogin.RLock()
	self := authoredOpenPRActiveLogin.login
	authoredOpenPRActiveLogin.RUnlock()
	keys := make([]string, 0, len(authors))
	for _, author := range authors {
		keys = append(keys, trackedPRCacheKey(self, author))
	}
	rows := map[string]*db.GitCacheRow{}
	if isGitHubOwnerSlug(self) {
		rows, _ = db.LoadGitCacheBatch(keys)
	}
	out := make([]dashboardAuthoredOpenPRs, 0, len(authors))
	for i, author := range authors {
		view := dashboardAuthoredOpenPRs{Login: author}
		if row := rows[keys[i]]; row == nil || json.Unmarshal(row.Data, &view) != nil ||
			!strings.EqualFold(view.Login, author) {
			view = dashboardAuthoredOpenPRs{Login: author}
		}
		if view.Items == nil {
			view.Items = []dashboardAuthoredOpenPR{}
		}
		if view.Recent == nil {
			view.Recent = []dashboardAuthoredOpenPR{}
		}
		out = append(out, applyRecentPRWindow(view, cfg, author))
	}
	return out
}

// trackedPRAuthorsBody is POST /api/open-prs/tracked-authors.
type trackedPRAuthorsBody struct {
	Login  string `json:"login"`
	Action string `json:"action"` // add|remove
}

// handleDashboardTrackedPRAuthors adds or removes one tracked author and
// returns the resulting list. Dashboard-only: the list is the operator's own,
// and every author added costs this node's GitHub quota.
func handleDashboardTrackedPRAuthors(w http.ResponseWriter, r *http.Request) {
	if !checkDashboardAuth(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	var body trackedPRAuthorsBody
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "bad_json", "request body is not valid JSON")
		return
	}
	login := strings.TrimPrefix(strings.TrimSpace(body.Login), "@")
	if !isGitHubOwnerSlug(login) {
		writeError(w, http.StatusBadRequest, "bad_login", "not a valid GitHub username")
		return
	}
	trackedPRAuthorsEditMu.Lock()
	defer trackedPRAuthorsEditMu.Unlock()
	authors := loadTrackedPRAuthors()
	switch body.Action {
	case "add":
		for _, existing := range authors {
			if strings.EqualFold(existing, login) {
				writeJSON(w, http.StatusOK, map[string]any{"authors": authors})
				return
			}
		}
		if len(authors) >= trackedPRAuthorsMax {
			writeError(w, http.StatusBadRequest, "too_many",
				fmt.Sprintf("at most %d people can be tracked", trackedPRAuthorsMax))
			return
		}
		// A typo is refused here rather than failing every later search. If
		// GitHub cannot be asked right now the author is kept; the poller
		// then reports a refusal on that author's tab alone.
		if exists, known := trackedPRUserLookup(login); known && !exists {
			writeError(w, http.StatusBadRequest, "no_such_user", "no GitHub user named "+login)
			return
		}
		authors = append(authors, login)
	case "remove":
		kept := authors[:0:0]
		for _, existing := range authors {
			if !strings.EqualFold(existing, login) {
				kept = append(kept, existing)
			}
		}
		authors = kept
	default:
		writeError(w, http.StatusBadRequest, "bad_action", `action must be "add" or "remove"`)
		return
	}
	if err := saveTrackedPRAuthors(authors); err != nil {
		writeError(w, http.StatusInternalServerError, "db", err.Error())
		return
	}
	if body.Action == "add" {
		// The new author's first search brings its reviewer list too.
		trackedPRReviewsFetchedAt.Store(0)
		kickTrackedPRAuthorsPoller()
	}
	writeJSON(w, http.StatusOK, map[string]any{"authors": authors})
}
