package agentd

import (
	"net/http"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/convindex"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/session"
)

// Three paginated, server-filtered GET endpoints that carry the dashboard
// lists that used to ride on the 2s /api/snapshot poll in full —
// retired agents, non-agent (promotion-candidate) conversations, and
// replaced (superseded) generations. The user can accumulate hundreds of
// retired agents, so shipping each list every poll was pure waste; these
// page + filter in SQL instead. All three are cookie-authed (dashboard-only),
// GET, and share one envelope + one offset/limit/q contract:
//
//	GET /api/retired       ?offset=&limit=&q=
//	GET /api/conversations ?offset=&limit=&q=
//	GET /api/replaced      ?offset=&limit=&q=
//
//	{ "rows": [...], "offset": <served>, "limit": <served; 0 == unbounded>,
//	  "total": <count matching q>, "total_unfiltered": <count ignoring q> }
//
// offset >= 0 (default 0). limit >= 0 (default 0 == UNBOUNDED / full list, the
// modal "show all" path; when > 0 clamped to <= maxListPageLimit). q is an
// optional case-insensitive title / conv-id / agent-id contains filter applied
// in SQL. The served offset is resolved against the live total (clampListOffset)
// so a stale offset past the end never strands the client on an empty page.
//
// Wired into the dashboard mux from registerDashboardEditRoutes.

// maxListPageLimit caps a bounded page request so a hand-crafted query can't
// ask the daemon to materialise an unbounded page (mirrors maxMailboxPageSize).
// limit == 0 is the deliberate unbounded "show all" case and is NOT capped.
const maxListPageLimit = 500

func registerDashboardLists(mux *http.ServeMux) {
	mux.HandleFunc("/api/retired", handleDashboardRetired)
	mux.HandleFunc("/api/conversations", handleDashboardConversations)
	mux.HandleFunc("/api/replaced", handleDashboardReplaced)
}

// listPageParams parses the shared offset / limit / q query params. offset is
// clamped to >= 0; limit to [0, maxListPageLimit] where 0 means UNBOUNDED; q is
// trimmed. The caller resolves the served offset against the live total via
// clampListOffset.
func listPageParams(r *http.Request) (offset, limit int, q string) {
	offset = atoiOr(r.URL.Query().Get("offset"), 0)
	if offset < 0 {
		offset = 0
	}
	limit = atoiOr(r.URL.Query().Get("limit"), 0)
	if limit < 0 {
		limit = 0
	}
	if limit > maxListPageLimit {
		limit = maxListPageLimit
	}
	q = strings.TrimSpace(r.URL.Query().Get("q"))
	return offset, limit, q
}

// clampListOffset resolves a requested OFFSET against a known total into the
// offset actually served — the offset-based twin of clampOffset
// (dashboard_mailbox.go). limit <= 0 (unbounded) always serves offset 0. An
// offset at/past the end is pulled back to the start of the last full page so a
// stale offset never strands on an empty page; an in-range offset is honoured
// as-is (offset pagination permits arbitrary windows, not just page multiples).
func clampListOffset(offset, limit, total int) int {
	if limit <= 0 {
		return 0
	}
	if offset < 0 {
		offset = 0
	}
	if total == 0 {
		return 0
	}
	if offset >= total {
		return ((total - 1) / limit) * limit
	}
	return offset
}

// writeListPage emits the shared paginated envelope. rows is always a
// (possibly empty) typed slice — never nil — so the dashboard JS can .map() it
// directly.
func writeListPage(w http.ResponseWriter, rows any, offset, limit, total, totalUnfiltered int) {
	writeJSON(w, http.StatusOK, map[string]any{
		"rows":             rows,
		"offset":           offset,
		"limit":            limit,
		"total":            total,
		"total_unfiltered": totalUnfiltered,
	})
}

// handleDashboardRetired serves GET /api/retired — one newest-retirement-first
// page of retired actors, each reinstatable. Replaces the snapshot's former
// retired[] list (collectRetiredSnapshot). Cookie-authed (dashboard-only).
func handleDashboardRetired(w http.ResponseWriter, r *http.Request) {
	if !checkDashboardAuth(w, r) {
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	offset, limit, q := listPageParams(r)
	total, totalUnfiltered, err := db.CountRetiredAgents(q)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	served := clampListOffset(offset, limit, total)
	agents, err := db.ListRetiredAgentsPage(q, served, limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	alive, _ := session.LiveTmuxSessions()
	rows := make([]dashboardRetiredAgent, 0, len(agents))
	for _, e := range agents {
		retiredAt := ""
		if !e.RetiredAt.IsZero() {
			retiredAt = e.RetiredAt.Format(time.RFC3339)
		}
		rows = append(rows, dashboardRetiredAgent{
			AgentID:          e.AgentID,
			ConvID:           e.CurrentConvID,
			Title:            agent.CachedTitle(e.CurrentConvID),
			Online:           isConvOnlineIn(e.CurrentConvID, alive),
			RetiredAt:        retiredAt,
			RetiredBy:        e.RetiredBy,
			RetiredByDisplay: resolveRetiredByDisplay(e.RetiredBy, e.RetiredByAgent),
			RetireReason:     e.RetireReason,
		})
	}
	writeListPage(w, rows, served, limit, total, totalUnfiltered)
}

// handleDashboardConversations serves GET /api/conversations — one
// newest-modified-first page of promotion-candidate conversations (non-agent
// conv_index rows). Replaces the snapshot's former conversations[] list
// (collectConversationsSnapshot). Cookie-authed (dashboard-only).
func handleDashboardConversations(w http.ResponseWriter, r *http.Request) {
	if !checkDashboardAuth(w, r) {
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	offset, limit, q := listPageParams(r)
	total, totalUnfiltered, err := db.CountNonAgentConvIndex(q)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	served := clampListOffset(offset, limit, total)
	convs, err := db.ListNonAgentConvIndexPage(q, served, limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	alive, _ := session.LiveTmuxSessions()
	rows := make([]dashboardConversation, 0, len(convs))
	for _, row := range convs {
		if row.ConvID == "" {
			continue
		}
		// Plain conversations are non-agents — never /rename'd — so the title is
		// a summary or a raw first prompt. Render it through convindex.FormatConvTitle
		// (the same formatter `conv ls` uses) so the dashboard stops leaking
		// uncleaned first-prompt text (system tags, newlines). The fsnotify monitor
		// keeps these cached rows fresh, so no per-row .jsonl rescan is needed.
		rows = append(rows, dashboardConversation{
			ConvID:   row.ConvID,
			Title:    convindex.FormatConvTitle(row.CustomTitle, row.Summary, row.FirstPrompt),
			Online:   isConvOnlineIn(row.ConvID, alive),
			State:    stateForConvIn(row.ConvID, alive),
			Modified: row.Modified,
		})
	}
	writeListPage(w, rows, served, limit, total, totalUnfiltered)
}

// handleDashboardReplaced serves GET /api/replaced — one newest-replacement-first
// page of superseded predecessor generations, each annotated with its owning
// actor + how/when it was replaced. Replaces the snapshot's former replaced[]
// list (collectReplacedGenerationsSnapshot); the DB query swaps the old
// O(actors) GenerationsForAgent walk for one paged JOIN over
// agent_conv_succession. Cookie-authed (dashboard-only).
func handleDashboardReplaced(w http.ResponseWriter, r *http.Request) {
	if !checkDashboardAuth(w, r) {
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	offset, limit, q := listPageParams(r)
	total, totalUnfiltered, err := db.CountReplacedGenerations(q)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	served := clampListOffset(offset, limit, total)
	gens, err := db.ListReplacedGenerationsPage(q, served, limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	// No tmux probe here: a replaced row is a SUPERSEDED predecessor generation
	// — the actor already advanced its live pointer off it, so the old conv-id
	// never has a live pane. Online is always false; skipping the probe drops a
	// per-request `tmux list-sessions` fork (this handler runs every 2s while
	// the Replaced group is shown, plus the mail tab's prev-gen pull).
	rows := make([]dashboardReplacedGen, 0, len(gens))
	for _, g := range gens {
		actorTitle := agent.CachedTitle(g.CurrentConvID)
		// Predecessor's own title, falling back to the live actor title when the
		// predecessor's index row is gone (CachedTitle returns the "(unknown)"
		// sentinel, which we treat as missing — mirrors the old snapshot logic).
		title := agent.CachedTitle(g.OldConvID)
		if title == "" || title == agent.UnknownTitle {
			title = actorTitle
		}
		replacedAt := ""
		if !g.SucceededAt.IsZero() {
			replacedAt = g.SucceededAt.Format(time.RFC3339)
		}
		rows = append(rows, dashboardReplacedGen{
			ConvID:       g.OldConvID,
			Title:        title,
			Reason:       g.Reason,
			ReplacedAt:   replacedAt,
			Online:       false, // predecessor generation — never has a live pane
			ActorAgentID: g.AgentID,
			ActorConvID:  g.CurrentConvID,
			ActorTitle:   actorTitle,
			ActorRetired: g.RetiredAt != "",
		})
	}
	writeListPage(w, rows, served, limit, total, totalUnfiltered)
}
