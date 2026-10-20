package agentd

import (
	"bytes"
	"context"
	"encoding/json"
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
// Cost is the design constraint. Every tracked author rides ONE GraphQL
// request per tick — an aliased open search, plus a recently closed search
// when that window is on, per author — on a slower cadence than the
// operator's own list, which also drives Groups badges. GitHub search only
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
	trackedPRRecentLimit    = 30
	trackedPRCachePrefix    = "tracked_author_prs_v1_"
	// trackedPRSearchTimeout bounds the one aliased request; eight authors
	// with check rollups is a much larger query than a branch lookup.
	trackedPRSearchTimeout = 30 * time.Second
)

var (
	trackedPRResolver = liveTrackedPRResolver
	// trackedPRUserLookup reports whether a GitHub user exists; known is
	// false when GitHub could not be asked (offline, logged out).
	trackedPRUserLookup = liveTrackedPRUserLookup
	// trackedPRGraphQL runs the aliased search and returns its stdout even
	// when gh exits non-zero; tests swap in a canned GitHub response.
	trackedPRGraphQL = liveTrackedPRGraphQL
	// trackedPRKick wakes the poller when an author is added, so the new tab
	// fills within a second instead of up to a minute later.
	trackedPRKick                = make(chan struct{}, 1)
	trackedPRReviewsFetchedAt    atomic.Int64
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
		for {
			select {
			case <-stop:
				return
			case <-trackedPRKick:
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
			case <-timer.C:
			}
			err := pollTrackedPRAuthors()
			delay := trackedPRPollInterval
			if err != nil {
				failures++
				delay = min(trackedPRPollInterval<<min(failures, 4), trackedPRMaxBackoff)
				if failures == 1 {
					slog.Warn("tracked-prs: GitHub search failed; keeping last good cache",
						"error", err, "retry_in", delay, "module", "agentd")
				}
			} else {
				failures = 0
			}
			timer.Reset(delay)
		}
	}()
}

func kickTrackedPRAuthorsPoller() {
	select {
	case trackedPRKick <- struct{}{}:
	default:
	}
}

// pollTrackedPRAuthors runs one search for every tracked author and caches
// each author's list under the active identity, the same namespacing as the
// operator's own list so a gh account switch never shows the old account's
// view.
func pollTrackedPRAuthors() error {
	authors := loadTrackedPRAuthors()
	if len(authors) == 0 {
		return nil
	}
	self, ok := cachedGitHubLogin()
	if !ok {
		return errTrackedPRNoActiveIdentity
	}
	cfg, _ := config.Load()
	now := time.Now()
	reviewsMark := trackedPRReviewsFetchedAt.Load()
	withReviews := trackedPRReviewsDue(now)
	views, err := trackedPRResolver(authors, cfg.RecentPRWindowDays(), now, withReviews)
	if err != nil {
		return err
	}
	if withReviews {
		// An add during this search reset the mark to 0; keep that reset so
		// the kicked poll fetches the new author's reviewers.
		trackedPRReviewsFetchedAt.CompareAndSwap(reviewsMark, now.UnixNano())
	}
	for _, view := range views {
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
	return nil
}

func trackedPRReviewsDue(now time.Time) bool {
	last := trackedPRReviewsFetchedAt.Load()
	return last == 0 || now.Sub(time.Unix(0, last)) >= authoredOpenPRReviewInterval
}

// trackedPRSearchArgs builds the `gh api graphql` argv for one tick: the
// shared open and recently closed fragments, aliased u<i>/r<i> per author.
func trackedPRSearchArgs(authors []string, windowDays int, now time.Time, withReviews bool) []string {
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
	args = append(args, "-f", "query="+query, "-F", "first="+strconv.Itoa(trackedPROpenLimit))
	if windowDays > 0 {
		args = append(args, "-F", "rfirst="+strconv.Itoa(trackedPRRecentLimit))
	}
	return append(args, values...)
}

func liveTrackedPRResolver(authors []string, windowDays int, now time.Time, withReviews bool) ([]dashboardAuthoredOpenPRs, error) {
	// gh exits non-zero when GraphQL reports any error, even with partial
	// data — a tracked login that no longer exists is exactly that — so the
	// output is decoded whether or not the call failed.
	out, runErr := trackedPRGraphQL(trackedPRSearchArgs(authors, windowDays, now, withReviews))
	views, err := decodeTrackedPRGraphQL(out, authors, windowDays)
	if err != nil {
		if runErr != nil {
			return nil, runErr
		}
		return nil, err
	}
	return views, nil
}

// decodeTrackedPRGraphQL splits the aliased response into one list per author
// by re-shaping each alias pair into the operator search's response and
// running the shared decoder. An error whose path names one alias marks only
// that author; any other error fails the tick.
func decodeTrackedPRGraphQL(data []byte, authors []string, windowDays int) ([]dashboardAuthoredOpenPRs, error) {
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
		view.Truncated = view.Total > trackedPROpenLimit
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

func liveTrackedPRGraphQL(args []string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), trackedPRSearchTimeout)
	defer cancel()
	return exec.CommandContext(ctx, "gh", args...).Output()
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
