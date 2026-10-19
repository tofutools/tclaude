package agentd

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// prdetails.go serves the hover card behind a PR link in the Groups tab:
// title, state, author, branches, diffstat and a short excerpt of the
// description.
//
// It follows the /api/pr-checks shape — nothing rides the 2s snapshot, the
// panel's own endpoint (GET /api/pr-details) answers from a per-PR cache and
// schedules one `gh pr view` behind itself when that cache is stale — but on
// a much longer TTL: a title or diffstat moves on a push, not every few
// seconds, and the card is read rather than watched.
//
// A successful read also shares its state and title with the Groups badges
// (see sharePRDetailsObservation): the card is often the freshest GitHub read
// the daemon has for that PR.
//
// The description is reduced to a plain-text excerpt here rather than in the
// browser, so the cache and the wire carry a few hundred characters instead
// of a PR body that can run to tens of kilobytes.

const (
	// prDetailsTTL bounds how stale a cached card may be before a hover
	// re-fetches it.
	prDetailsTTL = 60 * time.Second
	// prDetailsMaxBody caps how much of a PR body the excerpt pass reads.
	prDetailsMaxBody = 64 << 10
	// prDetailsExcerptRunes is the excerpt's length budget. The panel clamps
	// to three lines anyway; this keeps the payload bounded.
	prDetailsExcerptRunes = 420
	// prDetailsMaxTitleRunes truncates a pathological title.
	prDetailsMaxTitleRunes = 300
	// prDetailsMaxRefRunes truncates a pathological branch name.
	prDetailsMaxRefRunes = 200
	// prDetailsFailureCooldown holds off another `gh` call after a failed
	// read (gh missing or logged out, PR gone, rate limit), so a hover — or a
	// pointer sweeping across a column of PR links — does not spawn one
	// subprocess per poll against something that just failed.
	prDetailsFailureCooldown = 30 * time.Second
)

var (
	prDetailsInflight sync.Map
	// prDetailsFailedAt records, per cache key, when the last read failed.
	prDetailsFailedAt sync.Map
)

// prDetailsFields is the `gh pr view --json` field list the card needs.
const prDetailsFields = "title,body,state,isDraft,author,createdAt,mergedAt,closedAt," +
	"additions,deletions,changedFiles,commits,headRefName,baseRefName"

// prDetailsFetcher is the subprocess seam: production shells out to
// `gh pr view --json <prDetailsFields>` and returns its raw JSON; flow tests
// swap in a fake so the real parse and excerpt pass still run.
var prDetailsFetcher = livePRDetailsFetcher

// prDetails is the cached card.
type prDetails struct {
	Title        string    `json:"title"`
	State        string    `json:"state"` // open|draft|merged|closed
	Author       string    `json:"author,omitempty"`
	HeadRef      string    `json:"head_ref,omitempty"`
	BaseRef      string    `json:"base_ref,omitempty"`
	Excerpt      string    `json:"excerpt,omitempty"`
	Additions    int       `json:"additions"`
	Deletions    int       `json:"deletions"`
	ChangedFiles int       `json:"changed_files"`
	Commits      int       `json:"commits"`
	CreatedAt    string    `json:"created_at,omitempty"` // RFC3339
	MergedAt     string    `json:"merged_at,omitempty"`
	ClosedAt     string    `json:"closed_at,omitempty"`
	FetchedAt    time.Time `json:"fetched_at"`
}

func prDetailsCacheKey(rawURL string) string {
	h := sha256.Sum256([]byte("pr-details\x00" + prStateKey(rawURL)))
	return "prd_" + hex.EncodeToString(h[:8])
}

func livePRDetailsFetcher(rawURL string) (string, bool) {
	args, ok := presentedPRViewArgs(rawURL, prDetailsFields, gitProxyHardeningActive())
	if !ok {
		return "", false
	}
	out := runInDir("", "gh", args...)
	return out, out != ""
}

// parsePRDetails turns `gh pr view --json <prDetailsFields>` output into a
// card. Unknown or missing fields degrade to zero values.
func parsePRDetails(raw string) (prDetails, bool) {
	var payload struct {
		Title   string `json:"title"`
		Body    string `json:"body"`
		State   string `json:"state"`
		IsDraft bool   `json:"isDraft"`
		Author  struct {
			Login string `json:"login"`
		} `json:"author"`
		CreatedAt    string            `json:"createdAt"`
		MergedAt     string            `json:"mergedAt"`
		ClosedAt     string            `json:"closedAt"`
		Additions    int               `json:"additions"`
		Deletions    int               `json:"deletions"`
		ChangedFiles int               `json:"changedFiles"`
		Commits      []json.RawMessage `json:"commits"`
		HeadRefName  string            `json:"headRefName"`
		BaseRefName  string            `json:"baseRefName"`
	}
	if json.Unmarshal([]byte(raw), &payload) != nil {
		return prDetails{}, false
	}
	state := strings.ToLower(strings.TrimSpace(payload.State))
	if state == "open" && payload.IsDraft {
		state = "draft"
	}
	return prDetails{
		Title:        truncateRunes(strings.TrimSpace(payload.Title), prDetailsMaxTitleRunes),
		State:        state,
		Author:       truncateRunes(strings.TrimSpace(payload.Author.Login), 100),
		HeadRef:      truncateRunes(strings.TrimSpace(payload.HeadRefName), prDetailsMaxRefRunes),
		BaseRef:      truncateRunes(strings.TrimSpace(payload.BaseRefName), prDetailsMaxRefRunes),
		Excerpt:      prBodyExcerpt(payload.Body, prDetailsExcerptRunes),
		Additions:    max(0, payload.Additions),
		Deletions:    max(0, payload.Deletions),
		ChangedFiles: max(0, payload.ChangedFiles),
		Commits:      len(payload.Commits),
		CreatedAt:    rfc3339OrEmpty(payload.CreatedAt),
		MergedAt:     rfc3339OrEmpty(payload.MergedAt),
		ClosedAt:     rfc3339OrEmpty(payload.ClosedAt),
	}, true
}

// rfc3339OrEmpty passes a GitHub timestamp through only when it parses, and
// drops Go's zero-time sentinel that gh emits for an unset mergedAt/closedAt.
func rfc3339OrEmpty(raw string) string {
	t, err := time.Parse(time.RFC3339, strings.TrimSpace(raw))
	if err != nil || t.Year() <= 1 {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

func truncateRunes(s string, limit int) string {
	if utf8.RuneCountInString(s) <= limit {
		return s
	}
	r := []rune(s)
	return strings.TrimSpace(string(r[:limit])) + "…"
}

var (
	prBodyHTMLComment = regexp.MustCompile(`(?s)<!--.*?-->`)
	prBodyImage       = regexp.MustCompile(`!\[[^\]]*\]\([^)]*\)`)
	prBodyLink        = regexp.MustCompile(`\[([^\]]*)\]\([^)]*\)`)
	prBodyTag         = regexp.MustCompile(`</?[A-Za-z][^>]*>`)
	prBodyListMarker  = regexp.MustCompile(`^(?:[-*+]|\d+[.)])\s+(?:\[[ xX]\]\s+)?`)
	prBodyRule        = regexp.MustCompile(`^(?:[-*_]\s*){3,}$`)
	prBodySpaces      = regexp.MustCompile(`\s+`)
)

// prBodyExcerpt reduces a markdown PR body to its opening prose. Headings are
// dropped entirely, which is what skips a leading "## Background / Purpose"
// (every PR in this repo opens with one) so the excerpt starts at the actual
// why. Code blocks, tables, rules, images and HTML comments are dropped too;
// links keep their text. The result is cut at a word boundary within limit
// runes.
func prBodyExcerpt(body string, limit int) string {
	if len(body) > prDetailsMaxBody {
		body = body[:prDetailsMaxBody]
	}
	body = strings.ReplaceAll(body, "\r\n", "\n")
	body = prBodyHTMLComment.ReplaceAllString(body, "")
	var parts []string
	inFence := ""
	for _, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(line)
		if fence := fenceMarker(trimmed); fence != "" {
			switch {
			case inFence == "":
				inFence = fence
			case strings.HasPrefix(fence, inFence):
				inFence = ""
			}
			continue
		}
		if inFence != "" {
			continue
		}
		for strings.HasPrefix(trimmed, ">") {
			trimmed = strings.TrimSpace(strings.TrimPrefix(trimmed, ">"))
		}
		if trimmed == "" || strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, "|") ||
			prBodyRule.MatchString(trimmed) {
			continue
		}
		trimmed = prBodyListMarker.ReplaceAllString(trimmed, "")
		trimmed = prBodyImage.ReplaceAllString(trimmed, "")
		trimmed = prBodyLink.ReplaceAllString(trimmed, "$1")
		trimmed = prBodyTag.ReplaceAllString(trimmed, "")
		trimmed = strings.NewReplacer("**", "", "__", "", "~~", "", "`", "").Replace(trimmed)
		if trimmed = strings.TrimSpace(trimmed); trimmed != "" {
			parts = append(parts, trimmed)
		}
		if utf8.RuneCountInString(strings.Join(parts, " ")) > limit {
			break
		}
	}
	text := prBodySpaces.ReplaceAllString(strings.Join(parts, " "), " ")
	if utf8.RuneCountInString(text) <= limit {
		return text
	}
	r := []rune(text)[:limit]
	if cut := strings.LastIndexByte(string(r), ' '); cut > limit/2 {
		return strings.TrimRight(string(r)[:cut], " ,;:-") + "…"
	}
	return string(r) + "…"
}

func fenceMarker(line string) string {
	for _, marker := range []string{"```", "~~~"} {
		if strings.HasPrefix(line, marker) {
			n := len(line) - len(strings.TrimLeft(line, marker[:1]))
			return strings.Repeat(marker[:1], n)
		}
	}
	return ""
}

func savePRDetails(rawURL string, details prDetails) {
	data, err := json.Marshal(details)
	if err != nil {
		return
	}
	if err := db.SaveGitCache(prDetailsCacheKey(rawURL), data, details.FetchedAt); err != nil {
		slog.Warn("pr-details: failed to cache card",
			"error", err, "url", rawURL, "module", "agentd")
	}
}

func loadPRDetails(rawURL string) (prDetails, bool) {
	row, err := db.LoadGitCache(prDetailsCacheKey(rawURL))
	if err != nil || row == nil {
		return prDetails{}, false
	}
	var details prDetails
	if json.Unmarshal(row.Data, &details) != nil {
		return prDetails{}, false
	}
	if details.FetchedAt.IsZero() {
		details.FetchedAt = row.FetchedAt
	}
	return details, true
}

// schedulePRDetailsRefresh kicks one background `gh` resolution for a PR,
// deduplicated per PR identity, unless a read failed within the cooldown.
// Reports whether a read is now in flight — one it started or one already
// running — so the panel knows whether asking again will bring a newer card.
func schedulePRDetailsRefresh(rawURL string) bool {
	key := prDetailsCacheKey(rawURL)
	if _, busy := prDetailsInflight.Load(key); busy {
		return true
	}
	if at, failed := prDetailsFailedAt.Load(key); failed && time.Since(at.(time.Time)) < prDetailsFailureCooldown {
		return false
	}
	if err := presentedPRRemotePolicyCheck(rawURL); err != nil {
		slog.Warn("pr-details: refusing refresh outside git proxy policy",
			"error", err, "url", rawURL, "module", "agentd")
		prDetailsFailedAt.Store(key, time.Now())
		return false
	}
	if _, busy := prDetailsInflight.LoadOrStore(key, struct{}{}); busy {
		return true
	}
	goBackground(func() {
		defer prDetailsInflight.Delete(key)
		raw, ok := prDetailsFetcher(rawURL)
		var details prDetails
		if ok {
			details, ok = parsePRDetails(raw)
		}
		if !ok {
			prDetailsFailedAt.Store(key, time.Now())
			return
		}
		prDetailsFailedAt.Delete(key)
		details.FetchedAt = time.Now()
		savePRDetails(rawURL, details)
		sharePRDetailsObservation(rawURL, details)
	})
	return true
}

// sharePRDetailsObservation feeds a hover read's state and title into the
// same per-PR observation cache and agent_prs rows the background pollers
// write, so the Groups badges catch up with what the card just showed instead
// of waiting for the next poll. savePresentedPRCache and UpdateAgentPRState
// keep their usual race guards (merged never regresses).
func sharePRDetailsObservation(rawURL string, details prDetails) {
	if details.State == "" {
		return
	}
	savePresentedPRCache(presentedPRCacheKey(rawURL), rawURL, presentedPRInfo{
		Title:     truncateAuthoredPRTitle(details.Title),
		Number:    deriveGitHubPRNumber(rawURL),
		URL:       strings.TrimSpace(rawURL),
		State:     details.State,
		FetchedAt: details.FetchedAt,
	}, details.FetchedAt)
	applyObservedStatesToPresentedPRs(map[string]string{prStateKey(rawURL): details.State}, "pr-details")
}

// prDetailsResponse is the hover card's wire shape. Refreshing marks a `gh`
// read in flight behind this response, so the panel knows a newer answer is
// coming and polls again; once it is false, asking again changes nothing.
type prDetailsResponse struct {
	URL        string     `json:"url"`
	Details    *prDetails `json:"details,omitempty"`
	Resolved   bool       `json:"resolved"`
	Refreshing bool       `json:"refreshing"`
}

// handleDashboardPRDetails serves GET /api/pr-details?url=<github pr url>.
// Cookie-authed (dashboard-only) and read-only. A merged PR's card is frozen
// once cached: nothing about it changes any more. A closed one is not — it
// can be reopened.
func handleDashboardPRDetails(w http.ResponseWriter, r *http.Request) {
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
	details, ok := loadPRDetails(rawURL)
	stale := !ok || (time.Since(details.FetchedAt) >= prDetailsTTL && details.State != "merged")
	resp := prDetailsResponse{URL: rawURL, Resolved: ok}
	if stale {
		resp.Refreshing = schedulePRDetailsRefresh(rawURL)
	}
	if ok {
		resp.Details = &details
	}
	writeJSON(w, http.StatusOK, resp)
}
