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
	// groupMailboxPrefix namespaces a group folder's id ("group:<name>")
	// so it can never collide with an agent folder (a conv-id UUID) or the
	// "all"/"human" sentinels. The remainder after the prefix is the group
	// name verbatim — a name may contain ":", so callers strip only this
	// leading token. Keyed by name (not the stable group-id) because the
	// Groups-tab deep links already carry the name; a rename strands the
	// selection, which the frontend snaps back to "all" (pruneSelections).
	groupMailboxPrefix = "group:"
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

// mailboxIncludeEmpty reports whether the request opted into agents with
// an empty mailbox (the Messages-tab "show agents without messages"
// toggle). Default off: the roster hides agent folders that have neither
// sent nor received any mail — freshly-spawned / never-messaged agents
// that the active-agent set adds purely so they could be inspected —
// until the operator ticks the box. Unlike include_retired this is a
// roster-only filter: an empty mailbox owns no agent_messages rows, so it
// touches neither the "all" firehose nor its badge.
func mailboxIncludeEmpty(r *http.Request) bool {
	switch r.URL.Query().Get("include_empty") {
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
		if e.CurrentConvID == "" {
			continue
		}
		set[e.CurrentConvID] = struct{}{}
		ids = append(ids, e.CurrentConvID)
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
	// Members is the current member count of a group folder (kind="group")
	// — drives the sidebar tooltip. Omitted (0) for agent/human/all.
	Members int `json:"members,omitempty"`
	// MemberConvs lists the conv-ids a group folder (kind="group" only)
	// nests beneath itself when expanded: its current members, plus — when
	// the operator opted into retired agents (include_retired) — its retired
	// ex-members recovered from message history (retire unjoined them from
	// the live membership). The nested rows reuse the flat agent entries the
	// roster already carries — a member kept off that flat list by the empty
	// / text filter simply doesn't nest. Members (above) stays the current
	// member count. Omitted for agent/human/all.
	MemberConvs []string `json:"member_convs,omitempty"`
	In          int      `json:"in"`
	Out         int      `json:"out"`
	Total       int      `json:"total"`
	Unread      int      `json:"unread"`
	LastAt      string   `json:"last_at,omitempty"`
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
	includeEmpty := mailboxIncludeEmpty(r)
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
			convSet[e.CurrentConvID] = struct{}{}
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

	agentBoxes := make([]dashboardMailbox, 0, len(convSet))
	for conv := range convSet {
		_, retired := retiredSet[conv]
		// Retired agents are hidden unless the operator opted in; when
		// shown they carry the Retired flag so the frontend can mark them.
		if retired && !includeRetired {
			continue
		}
		c := counts[conv]
		// Agents with an empty mailbox (neither sent nor received any mail)
		// are hidden unless the operator opted in. A conv with any mail is in
		// counts, so an empty mailbox here only ever comes from the
		// active-agent set — a freshly-spawned / never-messaged agent — never
		// a folder that actually holds messages. No firehose/badge
		// reconciliation is needed (unlike retired): an empty mailbox owns no
		// rows, so excluding it changes nothing the "all" view counts.
		if c.In == 0 && c.Out == 0 && !includeEmpty {
			continue
		}
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
		agentBoxes = append(agentBoxes, mb)
	}

	// Group folders sit between the pinned virtual folders and the
	// per-agent folders — one per group, an aggregate "all member traffic"
	// view. Total is the distinct row count in the group scope (members'
	// traffic + the group's multicasts); In/Out/Unread are left 0 the way
	// the "all" aggregate leaves them — those are per-recipient notions
	// with no meaning for a group view. ListAgentGroups is name-ordered, so
	// the group section reads alphabetically. Like the "all" badge, a group
	// folder hides retired members' traffic by default — its badge counts
	// the same exclude-scoped total its folder serves — so the operator
	// must opt in (include_retired) to count retired traffic in either place.
	// When opted in, buildGroupMailboxes also nests retired ex-members back
	// under their former group (retire unjoined them, so they're gone from the
	// live membership) using each group's message history.
	groupBoxes := buildGroupMailboxes(retiredSet, retiredIDs, includeRetired)

	// Sort agent mailboxes by recency — newest mail on top, the way a mail
	// client lists folders by last activity — then by title, then conv-id
	// for a stable tiebreak.
	sort.Slice(agentBoxes, func(i, j int) bool {
		if agentBoxes[i].LastAt != agentBoxes[j].LastAt {
			return agentBoxes[i].LastAt > agentBoxes[j].LastAt
		}
		if agentBoxes[i].Title != agentBoxes[j].Title {
			return agentBoxes[i].Title < agentBoxes[j].Title
		}
		return agentBoxes[i].ID < agentBoxes[j].ID
	})

	// Final order: [all, human] (pinned), then groups, then agents.
	mailboxes = append(mailboxes, groupBoxes...)
	mailboxes = append(mailboxes, agentBoxes...)

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
	ID        int64  `json:"id"`
	Direction string `json:"direction"`
	FromConv  string `json:"from_conv,omitempty"`
	// FromAgent / ToAgent are the sender's / recipient's stable agent_id
	// (JOH-27), denormalised from the stored agent_messages snapshot so the
	// reading pane can lead with `name (agt_xxxxxxxx)` and stay attributable
	// after the conv is pruned. Empty when the conv was never an actor (a
	// plain conv, or a since-deleted agent); the frontend then falls back to
	// the short conv-id prefix via shortAgentId.
	FromAgent    string          `json:"from_agent,omitempty"`
	FromTitle    string          `json:"from_title,omitempty"`
	ToConv       string          `json:"to_conv,omitempty"`
	ToAgent      string          `json:"to_agent,omitempty"`
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
		// operator opened it on purpose, so they see all of its mail. A
		// lookup error fails the request (matches the roster handler)
		// rather than silently falling open to the unfiltered firehose.
		var exclude []string
		if !mailboxIncludeRetired(r) {
			_, ids, err := retiredAgentConvs()
			if err != nil {
				writeError(w, http.StatusInternalServerError, "io", err.Error())
				return
			}
			exclude = ids
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

	if name, ok := strings.CutPrefix(id, groupMailboxPrefix); ok {
		// A group folder: all member traffic + the group's own multicasts.
		g, err := db.GetAgentGroupByName(name)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "io", err.Error())
			return
		}
		if g == nil {
			writeError(w, http.StatusNotFound, "not_found", "no such group: "+name)
			return
		}
		members, err := groupMemberConvs(g.ID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "io", err.Error())
			return
		}
		// Hide retired members' traffic unless the operator opted in — the
		// same exclude the "all" firehose applies, so a member's DM to a
		// retired agent (and a retired sender's channel post) drops while a
		// current member's traffic and channel posts survive. A lookup error
		// fails the request (matches the all branch) rather than silently
		// falling open to the unfiltered group scope.
		var exclude []string
		if !mailboxIncludeRetired(r) {
			_, ids, err := retiredAgentConvs()
			if err != nil {
				writeError(w, http.StatusInternalServerError, "io", err.Error())
				return
			}
			exclude = ids
		}
		p, err := groupMailboxPage(members, g.ID, q, page, pageSize, exclude)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "io", err.Error())
			return
		}
		writeMailboxPage(w, map[string]any{
			"id":    id,
			"kind":  "group",
			"title": g.Name,
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
		ls = append(ls, recipientLine{ConvID: id, AgentID: peerAgentID(id), Title: d.titleOf(id)})
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
		FromAgent:    m.FromAgent,
		FromTitle:    d.titleOf(m.FromConv),
		ToConv:       m.ToConv,
		ToAgent:      m.ToAgent,
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
	return mailboxPageForScope(scope, forConv, q, page, pageSize)
}

// groupMailboxPage builds one newest-first page of a GROUP folder — all
// traffic touching any current member (sender or recipient) plus the
// group's own multicasts (see MailboxFilter.ScopeConvs / ScopeGroupID).
// Like the "all" firehose it has no single self to be relative to, so
// every row renders from→to (forConv == ""). excludeConvs drops retired
// members' traffic exactly as the "all" firehose does (the caller passes
// the retired conv-ids unless the operator opted into "show retired"): a
// member's DM to a retired agent disappears, and a channel multicast
// survives unless its own sender is retired. Passing nil shows the whole
// scope (the opted-in case).
func groupMailboxPage(members []string, groupID int64, q string, page, pageSize int, excludeConvs []string) (mailboxPage, error) {
	scope := db.MailboxFilter{ScopeConvs: members, ScopeGroupID: groupID, ExcludeConvs: excludeConvs}
	return mailboxPageForScope(scope, "", q, page, pageSize)
}

// mailboxPageForScope is the shared count → search → clamp → list →
// decorate pipeline behind agentMailboxPage / groupMailboxPage. scope is
// the folder predicate (a specific agent, a group, or the unscoped "all"
// firehose); forConv labels each row's direction relative to the folder
// ("in"/"out" for an agent folder, "" for an aggregate folder that
// renders from→to).
func mailboxPageForScope(scope db.MailboxFilter, forConv, q string, page, pageSize int) (mailboxPage, error) {
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

// groupMemberConvs returns the conv-ids of a group's current members,
// dropping any blank id. The set that scopes a group folder.
func groupMemberConvs(groupID int64) ([]string, error) {
	members, err := db.ListAgentGroupMembers(groupID)
	if err != nil {
		return nil, err
	}
	convs := make([]string, 0, len(members))
	for _, m := range members {
		if m.ConvID != "" {
			convs = append(convs, m.ConvID)
		}
	}
	return convs, nil
}

// buildGroupMailboxes builds one sidebar roster entry per group — the
// "view this group's messages" folders. Each carries the distinct row
// count in the group scope as its Total; a per-group CountMailbox is cheap
// (groups are few and human-curated). A lookup failure for one group skips
// just that group rather than failing the whole roster.
//
// retiredIDs (the retired-agent conv-ids) and includeRetired together
// govern two retired-aware adjustments:
//   - The badge total: when the operator hasn't opted in, retiredIDs scopes
//     out retired members' traffic — the roster twin of the group folder's
//     own exclude (groupMailboxPage), so the badge matches the folder it
//     labels. When opted in nothing is excluded and the whole scope counts.
//   - The nested member list: when the operator HAS opted in, MemberConvs is
//     augmented with the group's retired ex-members (recovered from message
//     history via retiredGroupParticipants) so the sidebar can nest them
//     under the group they used to belong to. Retire unjoins a member from
//     every group, so a retired ex-member is gone from the live membership
//     (groupMemberConvs) and would otherwise never nest even though its flat
//     agent folder is shown. Members stays the *current* member count.
func buildGroupMailboxes(retiredSet map[string]struct{}, retiredIDs []string, includeRetired bool) []dashboardMailbox {
	groups, err := db.ListAgentGroups()
	if err != nil {
		return nil
	}
	var excludeConvs []string
	if !includeRetired {
		excludeConvs = retiredIDs
	}
	out := make([]dashboardMailbox, 0, len(groups))
	for _, g := range groups {
		// Skip archived (soft-deleted) groups — the Groups tab hides them
		// from its default listing, so they carry no "view messages" deep
		// link; listing a folder for one here would be an unreachable,
		// inconsistent stray. (An archived group can still be opened
		// directly by id if something already holds the selection; the
		// handler resolves it — this only governs the roster.)
		if g.IsArchived() {
			continue
		}
		members, err := groupMemberConvs(g.ID)
		if err != nil {
			continue
		}
		total, err := db.CountMailbox(db.MailboxFilter{ScopeConvs: members, ScopeGroupID: g.ID, ExcludeConvs: excludeConvs})
		if err != nil {
			continue
		}
		memberConvs := members
		if includeRetired && len(retiredSet) > 0 {
			memberConvs = retiredGroupParticipants(members, g.ID, retiredSet)
		}
		out = append(out, dashboardMailbox{
			ID:          groupMailboxPrefix + g.Name,
			Kind:        "group",
			Title:       g.Name,
			Total:       total,
			Members:     len(members),
			MemberConvs: memberConvs,
		})
	}
	return out
}

// retiredGroupParticipants returns members with the group's retired
// ex-members appended — the conv-ids that have group-routed traffic for
// groupID (GroupMessageParticipants) and are in retiredSet, minus any
// already present. It is how a retired ex-member nests back under its former
// group on the Messages tab once the operator opts into retired agents:
// retire deletes the membership row, but the agent's group_id-stamped
// messages persist, so the association is recoverable. The order is current
// members first (their joined_at order), retired ex-members after.
func retiredGroupParticipants(members []string, groupID int64, retiredSet map[string]struct{}) []string {
	parts, err := db.GroupMessageParticipants(groupID)
	if err != nil {
		return members
	}
	have := make(map[string]struct{}, len(members))
	for _, m := range members {
		have[m] = struct{}{}
	}
	for _, p := range parts {
		if _, retired := retiredSet[p]; !retired {
			continue
		}
		if _, dup := have[p]; dup {
			continue
		}
		have[p] = struct{}{}
		members = append(members, p)
	}
	return members
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
