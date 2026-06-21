package agentd

import (
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/session"
)

// The dashboard mail client — an introspection + cleanup view over
// every mailbox the daemon stores, so the operator can see what agents
// actually said to each other (and to the human) when something goes
// wrong between them, and prune that history.
//
// Read surfaces — cookie-authed GETs, dashboard-only twins of data the
// `tclaude agent inbox` CLI reads:
//
//   GET /api/mailboxes        -> handleDashboardMailboxes
//       Enumerates one mailbox per conv that has any agent-to-agent
//       mail (plus every active agent, even with an empty mailbox), the
//       special "human" mailbox (the human.notify channel), and the
//       virtual "all" folder (every agent_messages row). Each carries
//       in/out/unread counts and a recency timestamp for the sidebar.
//
//   GET /api/mailbox?id=<all|human|conv>&q=&page=&page_size=
//       -> handleDashboardMailbox
//       Returns ONE newest-first page of the selected mailbox's messages
//       — "all" is every row; an agent mailbox is its received + sent
//       rows (a single OR predicate dedups the self-addressed row);
//       "human" is the human_messages rows. Titles are resolved so the
//       reading pane can render friendly names.
//
//       Pagination + search are server-side so the Messages tab pages
//       through the whole folder (not a client-loaded prefix) and the
//       search box filters the entire folder before paging. q matches
//       subject / body / conv-id and — resolved against the conv index +
//       group roster — counterpart titles and group names, mirroring the
//       old client-side filter. The response carries page, page_size,
//       total (rows matching q) and total_unfiltered (rows in the folder)
//       so the pager and count can render without a second request. page
//       is clamped to the last page, so a stale page after a delete still
//       lands on real rows (the served page comes back in the response).
//
// Mutation surfaces — cookie-authed POSTs. Merely VIEWING an agent
// mailbox still never mutates that agent's read-state (that would corrupt
// the agent's own inbox view); the read-state stays the agent's. But the
// operator (whose cookie + Origin is the human-consent layer) may delete
// the shared rows, and may EXPLICITLY set their read-state — repairing a
// stuck agent's inbox is the whole point of the mark-read action, distinct
// from a drive-by view:
//
//   POST /api/mailbox/delete    {ids:[...]}              -> handleDashboardMailboxDelete
//   POST /api/mailbox/wipe      {convs:[...]}            -> handleDashboardMailboxWipe
//   POST /api/mailbox/mark-read {ids:[...],read:bool}    -> handleDashboardMailboxMarkRead
//                               {conv:"<id>",read:true}  (folder-level "mark all read")
//
// The human folder keeps its own /api/human-messages/* mutation path
// (its delete accepts an ids array for multi-select).
//
// Wired into the dashboard mux from registerDashboardEditRoutes.

// humanMailboxID is the sentinel mailbox id for the human.notify
// channel. allMailboxID is the sentinel for the virtual "all messages"
// folder — every agent_messages row, newest-first, across every conv.
// Agent mailboxes are keyed by conv-id (a UUID), so these reserved words
// never collide with a real conv.
const (
	humanMailboxID = "human"
	allMailboxID   = "all"
)

// mailboxIncludeRetired reports whether the request opted into retired
// agents (the Messages-tab "show retired agents" toggle). Default off:
// the roster hides retired-agent folders and the "all" firehose omits
// their traffic until the operator ticks the box.
func mailboxIncludeRetired(r *http.Request) bool {
	switch r.URL.Query().Get("include_retired") {
	case "1", "true", "yes":
		return true
	default:
		return false
	}
}

// retiredAgentConvs returns the set of conv-ids whose agent enrollment
// has been retired, both as a membership set (for the roster's skip /
// mark decision) and as a slice (for MailboxFilter.ExcludeConvs). A nil
// error with empty results is the common case (no retired agents).
func retiredAgentConvs() (map[string]struct{}, []string, error) {
	rows, err := db.ListRetiredAgents()
	if err != nil {
		return nil, nil, err
	}
	set := make(map[string]struct{}, len(rows))
	ids := make([]string, 0, len(rows))
	for _, e := range rows {
		if e.ConvID == "" {
			continue
		}
		set[e.ConvID] = struct{}{}
		ids = append(ids, e.ConvID)
	}
	return set, ids, nil
}

func registerDashboardMailboxRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/api/mailboxes", handleDashboardMailboxes)
	mux.HandleFunc("/api/mailbox", handleDashboardMailbox)
	mux.HandleFunc("POST /api/mailbox/delete", handleDashboardMailboxDelete)
	mux.HandleFunc("POST /api/mailbox/wipe", handleDashboardMailboxWipe)
	mux.HandleFunc("POST /api/mailbox/mark-read", handleDashboardMailboxMarkRead)
}

// dashboardMailbox is one sidebar entry. Kind is "human" or "agent".
// Retired marks an agent folder whose enrollment has been retired — the
// roster only carries these when the operator opted in (include_retired),
// so the frontend can flag them visually.
type dashboardMailbox struct {
	ID      string   `json:"id"`
	Kind    string   `json:"kind"`
	Title   string   `json:"title"`
	Short   string   `json:"short,omitempty"`
	Online  bool     `json:"online"`
	Retired bool     `json:"retired,omitempty"`
	Groups  []string `json:"groups,omitempty"`
	In      int      `json:"in"`
	Out     int      `json:"out"`
	Total   int      `json:"total"`
	Unread  int      `json:"unread"`
	LastAt  string   `json:"last_at,omitempty"`
}

// handleDashboardMailboxes serves GET /api/mailboxes — the sidebar
// roster. Cookie-authed (dashboard-only).
func handleDashboardMailboxes(w http.ResponseWriter, r *http.Request) {
	if !checkDashboardAuth(w, r) {
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}

	includeRetired := mailboxIncludeRetired(r)
	retiredSet, retiredIDs, err := retiredAgentConvs()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}

	counts, err := db.MailboxCounts()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	groupsByConv, _ := db.GroupNamesByConv()
	aliveSessions, _ := session.LiveTmuxSessions()

	// The conv set = every conv that has mail ∪ every active agent (so a
	// freshly-spawned agent with an empty mailbox still gets a folder).
	convSet := map[string]struct{}{}
	for conv := range counts {
		if conv != "" {
			convSet[conv] = struct{}{}
		}
	}
	if active, err := db.ListActiveAgents(); err == nil {
		for _, e := range active {
			convSet[e.ConvID] = struct{}{}
		}
	}

	mailboxes := make([]dashboardMailbox, 0, len(convSet)+2)

	// The virtual "all messages" folder leads the list — the
	// chronological firehose of every agent-to-agent message, so the
	// operator can read traffic across convs in one place. Its total is
	// the distinct row count (not the In+Out sum the per-conv tallies
	// produce). Unread is left 0: "unread" is a per-recipient notion that
	// has no meaning for an aggregate view. When retired agents are hidden
	// the count must match the filtered firehose, so it counts the same
	// excluded scope rather than the raw row total.
	allTotal := 0
	if includeRetired || len(retiredIDs) == 0 {
		allTotal, _ = db.CountAgentMessages()
	} else {
		allTotal, _ = db.CountMailbox(db.MailboxFilter{ExcludeConvs: retiredIDs})
	}
	mailboxes = append(mailboxes, dashboardMailbox{
		ID:    allMailboxID,
		Kind:  "all",
		Title: "All agent messages",
		Total: allTotal,
	})

	// The human.notify channel comes next — it is the operator's own
	// folder, distinct from the agent-to-agent traffic.
	humanMsgs, humanUnread := buildHumanMessagesSnapshot()
	mailboxes = append(mailboxes, dashboardMailbox{
		ID:     humanMailboxID,
		Kind:   "human",
		Title:  "Human notifications",
		In:     len(humanMsgs),
		Total:  len(humanMsgs),
		Unread: humanUnread,
		LastAt: latestHumanMessageAt(humanMsgs),
	})

	for conv := range convSet {
		_, retired := retiredSet[conv]
		// Retired agents are hidden unless the operator opted in; when
		// shown they carry the Retired flag so the frontend can mark them.
		if retired && !includeRetired {
			continue
		}
		c := counts[conv]
		title := agent.CachedTitle(conv)
		if title == agent.UnknownTitle {
			title = ""
		}
		mb := dashboardMailbox{
			ID:      conv,
			Kind:    "agent",
			Title:   title,
			Short:   agent.ShortID(conv),
			Online:  isConvOnlineIn(conv, aliveSessions),
			Retired: retired,
			Groups:  groupsByConv[conv],
			In:      c.In,
			Out:     c.Out,
			Total:   c.In + c.Out,
			Unread:  c.Unread,
		}
		if !c.Last.IsZero() {
			mb.LastAt = c.Last.Format(time.RFC3339)
		}
		mailboxes = append(mailboxes, mb)
	}

	// Sort agent mailboxes by recency — newest mail on top, the way a
	// mail client lists folders by last activity — then by title, then
	// conv-id for a stable tiebreak. The two virtual folders stay pinned
	// at the head (mailboxes[0]=all, mailboxes[1]=human); only the agent
	// tail (mailboxes[2:]) is reordered.
	agents := mailboxes[2:]
	sort.Slice(agents, func(i, j int) bool {
		if agents[i].LastAt != agents[j].LastAt {
			return agents[i].LastAt > agents[j].LastAt
		}
		if agents[i].Title != agents[j].Title {
			return agents[i].Title < agents[j].Title
		}
		return agents[i].ID < agents[j].ID
	})

	writeJSON(w, http.StatusOK, map[string]any{"mailboxes": mailboxes})
}

// latestHumanMessageAt returns the newest human message's timestamp (the
// snapshot is already newest-first), or "" when empty.
func latestHumanMessageAt(msgs []dashboardHumanMessage) string {
	if len(msgs) == 0 {
		return ""
	}
	return msgs[0].CreatedAt
}

// mailboxMessage is one message in a mailbox's reading list. Direction
// is relative to the selected mailbox: "in" = received, "out" = sent.
// For the human mailbox every row is "in" (agents → human).
type mailboxMessage struct {
	ID           int64           `json:"id"`
	Direction    string          `json:"direction"`
	FromConv     string          `json:"from_conv,omitempty"`
	FromTitle    string          `json:"from_title,omitempty"`
	ToConv       string          `json:"to_conv,omitempty"`
	ToTitle      string          `json:"to_title,omitempty"`
	ToRecipients []recipientLine `json:"to_recipients,omitempty"`
	CcRecipients []recipientLine `json:"cc_recipients,omitempty"`
	Group        string          `json:"group,omitempty"`
	Subject      string          `json:"subject,omitempty"`
	Body         string          `json:"body"`
	CreatedAt    string          `json:"created_at"`
	DeliveredAt  string          `json:"delivered_at,omitempty"`
	Read         bool            `json:"read"`
	ParentID     int64           `json:"parent_id,omitempty"`
}

// Mailbox pagination bounds. defaultMailboxPageSize is what the
// dashboard requests when the operator hasn't picked a page size;
// maxMailboxPageSize caps the request so a hand-crafted query can't ask
// the daemon to materialise an unbounded page.
const (
	defaultMailboxPageSize = 50
	maxMailboxPageSize     = 500
)

// mailboxPageParams parses the page (1-based) and page_size query params,
// clamping page_size to [1, maxMailboxPageSize] and page to >= 1. The
// requested page may still exceed the last page; clampOffset handles that
// against the live total so deletions can't strand the operator on an
// empty page.
func mailboxPageParams(r *http.Request) (page, pageSize int) {
	page = max(atoiOr(r.URL.Query().Get("page"), 1), 1)
	pageSize = atoiOr(r.URL.Query().Get("page_size"), defaultMailboxPageSize)
	if pageSize < 1 {
		pageSize = defaultMailboxPageSize
	}
	pageSize = min(pageSize, maxMailboxPageSize)
	return page, pageSize
}

func atoiOr(s string, def int) int {
	if n, err := strconv.Atoi(strings.TrimSpace(s)); err == nil {
		return n
	}
	return def
}

// clampOffset resolves a requested 1-based page against a known total
// into the page actually served and its row offset. A page past the last
// is pulled back to the last page (so a stale "page 7" after a delete
// still shows real rows); an empty folder serves page 1, offset 0.
func clampOffset(page, pageSize, total int) (servedPage, offset int) {
	maxPage := 1
	if total > 0 {
		maxPage = (total + pageSize - 1) / pageSize
	}
	page = max(min(page, maxPage), 1)
	return page, (page - 1) * pageSize
}

// mailboxPage is the paginated read response shared by all three folder
// kinds. Total counts rows matching the search; TotalUnfiltered counts
// the whole folder — the pager divides Total into pages, the count chip
// shows "Total / TotalUnfiltered" while searching.
type mailboxPage struct {
	Messages        []mailboxMessage
	Page            int
	PageSize        int
	Total           int
	TotalUnfiltered int
}

// handleDashboardMailbox serves
// GET /api/mailbox?id=<conv|human|all>&q=&page=&page_size= — one
// newest-first page of the selected mailbox's messages, server-filtered
// by q. Cookie-authed (dashboard-only).
func handleDashboardMailbox(w http.ResponseWriter, r *http.Request) {
	if !checkDashboardAuth(w, r) {
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	id := r.URL.Query().Get("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "invalid_arg", "id is required (a conv-id or \"human\")")
		return
	}
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	page, pageSize := mailboxPageParams(r)

	if id == humanMailboxID {
		p := humanMailboxPage(q, page, pageSize)
		writeMailboxPage(w, map[string]any{
			"id":    humanMailboxID,
			"kind":  "human",
			"title": "Human notifications",
		}, p)
		return
	}

	if id == allMailboxID {
		// The firehose drops retired agents' traffic unless the operator
		// opted in. A specific agent folder (below) never excludes — the
		// operator opened it on purpose, so they see all of its mail.
		var exclude []string
		if !mailboxIncludeRetired(r) {
			if _, ids, err := retiredAgentConvs(); err == nil {
				exclude = ids
			}
		}
		p, err := agentMailboxPage("", q, page, pageSize, exclude)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "io", err.Error())
			return
		}
		writeMailboxPage(w, map[string]any{
			"id":    allMailboxID,
			"kind":  "all",
			"title": "All agent messages",
		}, p)
		return
	}

	p, err := agentMailboxPage(id, q, page, pageSize, nil)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	title := agent.CachedTitle(id)
	if title == agent.UnknownTitle {
		title = ""
	}
	writeMailboxPage(w, map[string]any{
		"id":    id,
		"kind":  "agent",
		"title": title,
		"short": agent.ShortID(id),
	}, p)
}

// writeMailboxPage merges the folder-identity fields with the paginated
// body and emits the JSON. messages is always a (possibly empty) array,
// never null, so the JS can .map() it directly.
func writeMailboxPage(w http.ResponseWriter, head map[string]any, p mailboxPage) {
	if p.Messages == nil {
		p.Messages = []mailboxMessage{}
	}
	head["messages"] = p.Messages
	head["page"] = p.Page
	head["page_size"] = p.PageSize
	head["total"] = p.Total
	head["total_unfiltered"] = p.TotalUnfiltered
	writeJSON(w, http.StatusOK, head)
}

// resolveMailboxSearch turns the search box text into the two id-sets
// MailboxFilter can't derive from agent_messages alone: convs whose
// resolved display title contains q (matched the same way the reading
// pane renders from_title/to_title — via agent.TitleFor over the small
// set of convs that appear in any message), and groups whose name
// contains q. Returns (nil, nil) for an empty query.
func resolveMailboxSearch(q string) (titleConvs []string, groupIDs []int64) {
	if q == "" {
		return nil, nil
	}
	lq := strings.ToLower(q)
	if convs, err := db.DistinctAgentMessageConvs(); err == nil {
		for _, c := range convs {
			t := agent.TitleFor(c)
			if t != "" && strings.Contains(strings.ToLower(t), lq) {
				titleConvs = append(titleConvs, c)
			}
		}
	}
	if gs, err := db.ListAgentGroups(); err == nil {
		for _, g := range gs {
			if strings.Contains(strings.ToLower(g.Name), lq) {
				groupIDs = append(groupIDs, g.ID)
			}
		}
	}
	return titleConvs, groupIDs
}

// humanMailboxMessages maps the human_messages snapshot into the shared
// mailboxMessage shape so the reading pane renders uniformly. Every row
// is direction "in" — agents notifying the human.
func humanMailboxMessages() []mailboxMessage {
	rows, _ := buildHumanMessagesSnapshot()
	out := make([]mailboxMessage, 0, len(rows))
	for _, m := range rows {
		out = append(out, mailboxMessage{
			ID:        m.ID,
			Direction: "in",
			FromConv:  m.FromConv,
			FromTitle: m.FromTitle,
			Group:     m.Group,
			Subject:   m.Subject,
			Body:      m.Body,
			CreatedAt: m.CreatedAt,
			Read:      m.Read,
		})
	}
	return out
}

// mailboxDecorator memoizes the per-conv title lookups and group-name
// resolution that turning a db.AgentMessage into a wire-shape
// mailboxMessage needs. A two-agent thread repeats the same one or two
// conv-ids across hundreds of rows, and the folder builders run on every
// 2s refresh while a folder is open, so caching collapses thousands of
// GetConvIndex reads into a handful. Shared by the agent-folder and
// all-folder builders.
type mailboxDecorator struct {
	groupNames map[int64]string
	titleCache map[string]string
}

func newMailboxDecorator() *mailboxDecorator {
	groupNames := map[int64]string{}
	if gs, err := db.ListAgentGroups(); err == nil {
		for _, g := range gs {
			groupNames[g.ID] = g.Name
		}
	}
	return &mailboxDecorator{groupNames: groupNames, titleCache: map[string]string{}}
}

func (d *mailboxDecorator) titleOf(c string) string {
	if c == "" {
		return ""
	}
	if t, ok := d.titleCache[c]; ok {
		return t
	}
	t := agent.TitleFor(c)
	d.titleCache[c] = t
	return t
}

func (d *mailboxDecorator) recipients(ids []string) []recipientLine {
	if len(ids) == 0 {
		return nil
	}
	ls := make([]recipientLine, 0, len(ids))
	for _, id := range ids {
		ls = append(ls, recipientLine{ConvID: id, Title: d.titleOf(id)})
	}
	return ls
}

// toMessage maps one stored row into the reading-pane shape. dir is the
// direction relative to the folder being viewed ("in"/"out" for an agent
// folder, "" for the aggregate "all" folder, which renders from→to).
func (d *mailboxDecorator) toMessage(m *db.AgentMessage, dir string) mailboxMessage {
	mm := mailboxMessage{
		ID:           m.ID,
		Direction:    dir,
		FromConv:     m.FromConv,
		FromTitle:    d.titleOf(m.FromConv),
		ToConv:       m.ToConv,
		ToTitle:      d.titleOf(m.ToConv),
		ToRecipients: d.recipients(m.ToRecipients),
		CcRecipients: d.recipients(m.CcRecipients),
		Group:        d.groupNames[m.GroupID],
		Subject:      m.Subject,
		Body:         m.Body,
		CreatedAt:    m.CreatedAt.Format(time.RFC3339),
		Read:         !m.ReadAt.IsZero(),
		ParentID:     m.ParentID,
	}
	if !m.DeliveredAt.IsZero() {
		mm.DeliveredAt = m.DeliveredAt.Format(time.RFC3339)
	}
	return mm
}

// directionFor labels a row relative to the folder being viewed: "in"
// (received) when the folder conv is the recipient, "out" (sent)
// otherwise. The "all" firehose (forConv == "") has no self to be
// relative to, so every row is "" and renders from→to. A self-addressed
// row (to == from == folder) reads as "in" — matching the old merge that
// listed the inbox copy first when deduplicating.
func directionFor(forConv string, m *db.AgentMessage) string {
	if forConv == "" {
		return ""
	}
	if m.ToConv == forConv {
		return "in"
	}
	return "out"
}

// agentMailboxPage builds one newest-first page of an agent folder (or
// the "all" firehose when forConv == ""), server-filtered by q. The scope
// predicate `(to_conv = ? OR from_conv = ?)` returns each row once, so the
// self-addressed row that the old two-query merge had to dedup is handled
// for free. excludeConvs drops rows touching those convs (the "all"
// firehose passes retired agents here when the operator hasn't opted in;
// a specific folder passes nil). Counts: total = rows matching q within
// that scope, totalUnfiltered = rows in the (still exclude-scoped) folder
// regardless of q — so the pager and the search count both reflect the
// same hidden-retired view.
func agentMailboxPage(forConv, q string, page, pageSize int, excludeConvs []string) (mailboxPage, error) {
	scope := db.MailboxFilter{ForConv: forConv, ExcludeConvs: excludeConvs}
	totalUnfiltered, err := db.CountMailbox(scope)
	if err != nil {
		return mailboxPage{}, err
	}

	filter := scope
	total := totalUnfiltered
	if q != "" {
		filter.Text = q
		filter.TitleConvs, filter.GroupIDs = resolveMailboxSearch(q)
		if total, err = db.CountMailbox(filter); err != nil {
			return mailboxPage{}, err
		}
	}

	servedPage, offset := clampOffset(page, pageSize, total)
	rows, err := db.ListMailboxPage(filter, pageSize, offset)
	if err != nil {
		return mailboxPage{}, err
	}
	dec := newMailboxDecorator()
	msgs := make([]mailboxMessage, 0, len(rows))
	for _, m := range rows {
		msgs = append(msgs, dec.toMessage(m, directionFor(forConv, m)))
	}
	return mailboxPage{
		Messages:        msgs,
		Page:            servedPage,
		PageSize:        pageSize,
		Total:           total,
		TotalUnfiltered: totalUnfiltered,
	}, nil
}

// humanMailboxPage paginates the human_messages folder. The snapshot is
// small (the operator's own notifications) and lives in a different
// table with its own builder, so search + paging happen in Go over the
// already-loaded, newest-first snapshot rather than in SQL.
func humanMailboxPage(q string, page, pageSize int) mailboxPage {
	all := humanMailboxMessages()
	filtered := all
	if q != "" {
		lq := strings.ToLower(q)
		filtered = make([]mailboxMessage, 0, len(all))
		for _, m := range all {
			if humanMsgMatchesSearch(m, lq) {
				filtered = append(filtered, m)
			}
		}
	}
	total := len(filtered)
	servedPage, offset := clampOffset(page, pageSize, total)
	end := offset + pageSize
	if offset > total {
		offset = total
	}
	if end > total {
		end = total
	}
	return mailboxPage{
		Messages:        filtered[offset:end],
		Page:            servedPage,
		PageSize:        pageSize,
		Total:           total,
		TotalUnfiltered: len(all),
	}
}

// humanMsgMatchesSearch reports whether a human notification matches the
// (already-lowercased) query, over the same fields the agent-folder
// search covers that a human message has: sender title / conv-id, group,
// subject, body.
func humanMsgMatchesSearch(m mailboxMessage, lq string) bool {
	for _, s := range []string{m.FromTitle, m.FromConv, m.Group, m.Subject, m.Body} {
		if s != "" && strings.Contains(strings.ToLower(s), lq) {
			return true
		}
	}
	return false
}

// handleDashboardMailboxDelete serves POST /api/mailbox/delete — hard-
// deletes the listed agent_messages rows by id ({"ids":[...]}). The
// operator's per-message / multi-select delete on the Messages tab.
// Unconditional (the cookie + Origin gate is the human-consent layer,
// same as the other dashboard mutations). Cookie-authed.
func handleDashboardMailboxDelete(w http.ResponseWriter, r *http.Request) {
	if !checkDashboardAuth(w, r) {
		return
	}
	// A list of ids — cap the body generously above a full-page
	// "select all then delete" (maxMailboxPageSize ids) but well below
	// anything that could blow up memory.
	r.Body = http.MaxBytesReader(w, r.Body, 256*1024)
	var body struct {
		IDs []int64 `json:"ids"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", "invalid JSON body")
		return
	}
	if len(body.IDs) == 0 {
		writeError(w, http.StatusBadRequest, "invalid_arg", "ids is required (one or more message ids)")
		return
	}
	n, err := db.DeleteAgentMessagesByIDs(body.IDs)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": n})
}

// handleDashboardMailboxWipe serves POST /api/mailbox/wipe — hard-
// deletes every agent_messages row touching any of the listed convs
// ({"convs":[...]}), sender or recipient. The operator's "wipe selected
// mailboxes" bulk action. Cookie-authed.
func handleDashboardMailboxWipe(w http.ResponseWriter, r *http.Request) {
	if !checkDashboardAuth(w, r) {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 256*1024)
	var body struct {
		Convs []string `json:"convs"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", "invalid JSON body")
		return
	}
	if len(body.Convs) == 0 {
		writeError(w, http.StatusBadRequest, "invalid_arg", "convs is required (one or more mailbox conv-ids)")
		return
	}
	n, err := db.WipeAgentMessagesForConvs(body.Convs)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": n})
}

// handleDashboardMailboxMarkRead serves POST /api/mailbox/mark-read — sets
// the read-state of agent_messages rows on the recipient's behalf. Two modes:
//
//	{ids:[...], read:bool}   — mark the listed rows read (read=true) / unread
//	                           (read=false)
//	{conv:"<id>", read:true} — mark every message that conv has RECEIVED read
//	                           (the per-folder "mark all read")
//
// This is the operator's authority to repair a stuck agent's inbox read-state:
// unlike merely viewing an agent mailbox (which never touches the agent's
// read-state), it is an explicit operator action, gated the same way as the
// delete/wipe mutations (cookie + Origin = human consent). conv mode supports
// read=true only — marking a whole inbox unread has no use and would be a
// footgun. Returns {"marked": n}, the count of rows whose state changed.
// Cookie-authed.
//
// The two modes differ on purpose (see SetAgentMessagesRead vs
// MarkAgentMailboxRead): ids mode is direction-agnostic — it flips whatever
// rows are named, including ones the viewed agent SENT (whose read_at is the
// recipient's), which is the operator's explicit per-row / bulk-selection
// choice — whereas conv mode is received-side-only so a one-click whole-folder
// "mark all read" can't silently flip other agents' read-state. When both ids
// and conv are present conv wins (the frontend never sends both).
func handleDashboardMailboxMarkRead(w http.ResponseWriter, r *http.Request) {
	if !checkDashboardAuth(w, r) {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 256*1024)
	var body struct {
		IDs  []int64 `json:"ids"`
		Conv string  `json:"conv"`
		Read bool    `json:"read"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", "invalid JSON body")
		return
	}
	var (
		n   int64
		err error
	)
	switch {
	case body.Conv != "":
		if !body.Read {
			writeError(w, http.StatusBadRequest, "invalid_arg", "conv mode supports read=true only")
			return
		}
		n, err = db.MarkAgentMailboxRead(body.Conv)
	case len(body.IDs) > 0:
		n, err = db.SetAgentMessagesRead(body.IDs, body.Read)
	default:
		writeError(w, http.StatusBadRequest, "invalid_arg", "ids or conv is required")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"marked": n})
}
