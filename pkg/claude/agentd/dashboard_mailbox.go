package agentd

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
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
// has been retired, for the roster's skip / mark decision. Message queries
// never take this set — they exclude retired traffic in SQL
// (MailboxFilter.ExcludeRetired), so it never becomes a bound id list.
func retiredAgentConvs() (map[string]struct{}, error) {
	ids, err := db.ListRetiredConvIDs()
	if err != nil {
		return nil, err
	}
	set := make(map[string]struct{}, len(ids))
	for _, c := range ids {
		set[c] = struct{}{}
	}
	return set, nil
}

func registerDashboardMailboxRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/api/mailboxes", handleDashboardMailboxes)
	mux.HandleFunc("/api/mailbox", handleDashboardMailbox)
	mux.HandleFunc("POST /api/mailbox/delete", handleDashboardMailboxDelete)
	mux.HandleFunc("POST /api/mailbox/wipe", handleDashboardMailboxWipe)
	mux.HandleFunc("POST /api/mailbox/mark-read", handleDashboardMailboxMarkRead)
	registerDashboardAgentMessageAttachmentRoutes(mux)
}

// dashboardMailbox is one sidebar entry. Kind is "human" or "agent".
// Retired marks an agent folder whose enrollment has been retired — the
// roster only carries these when the operator opted in (include_retired),
// so the frontend can flag them visually.
type dashboardMailbox struct {
	ID    string `json:"id"`
	Kind  string `json:"kind"`
	Title string `json:"title"`
	Short string `json:"short,omitempty"`
	// AgentID is the agent folder's stable agent_id, resolved live from the
	// folder's conv. It lets the sidebar lead a nameless folder with its
	// agt_ handle (instead of a short conv-id) and hover the full
	// "agent_id / conv-id" pair. Empty for pinned / group folders and any
	// conv with no agent enrollment.
	AgentID string `json:"agent_id,omitempty"`
	Online  bool   `json:"online"`
	Retired bool   `json:"retired,omitempty"`
	// PrevGen marks an agent folder whose conv is a superseded predecessor
	// generation of a still-existing actor (left behind by a reincarnate /
	// Claude Code /clear). The roster drops these when hide_prev_gens is set.
	PrevGen bool     `json:"prev_gen,omitempty"`
	Groups  []string `json:"groups,omitempty"`
	// Members is the current member count of a group folder (kind="group")
	// — drives the sidebar tooltip. Omitted (0) for agent/human/all.
	Members int `json:"members,omitempty"`
	// memberConvs lists the conv-ids a group folder nests beneath itself
	// when expanded: its current members, plus — when the operator opted
	// into retired agents (include_retired) — its retired ex-members
	// recovered from message history (retire unjoined them from the live
	// membership). Members (above) stays the current member count. Only
	// built for the groups the request names in expand; never sent (with
	// retired history it can run to thousands of ids per group).
	memberConvs []string
	// MemberBoxes carries one page of the nested agent folders of a group
	// the request named in expand: the memberConvs that pass the same
	// retired / empty / previous-generation / q filters as the flat agent
	// list, newest activity first, members_limit of them (default
	// defaultRosterAgents). MembersTotal counts every such folder, so the
	// sidebar can offer the rest. Nested rows come with their group rather
	// than from the paged flat list, so a member nests whether or not it
	// falls on the current agent page.
	MemberBoxes  []dashboardMailbox `json:"member_boxes,omitempty"`
	MembersTotal int                `json:"members_total,omitempty"`
	In           int                `json:"in"`
	Out          int                `json:"out"`
	Total        int                `json:"total"`
	Unread       int                `json:"unread"`
	LastAt       string             `json:"last_at,omitempty"`
}

// Roster agent paging. The sidebar's flat agent list is one newest-activity
// -first page of agents_limit folders; the client asks for more as the
// operator scrolls ("load more"). An expanded group's nested member list
// pages the same way (members_limit). maxRosterAgents caps a request; the
// dashboard mirrors it as ROSTER_MAX (fused-mail-model.js) to stop offering
// "Show more" at the cap.
const (
	defaultRosterAgents = 100
	maxRosterAgents     = 2000
)

// rosterLimit parses a roster page size: absent or malformed means
// defaultRosterAgents, clamped to [0, maxRosterAgents].
func rosterLimit(v string) int {
	return min(max(atoiOr(v, defaultRosterAgents), 0), maxRosterAgents)
}

// handleDashboardMailboxes serves GET /api/mailboxes — the sidebar
// roster. Cookie-authed (dashboard-only).
//
// Query params (all optional):
//
//	include_retired / include_empty  show retired / never-messaged agents
//	hide_prev_gens                   drop predecessor-generation folders
//	q                                the sidebar filter box: agent folders whose
//	                                 title, agent_id, conv-id or group name
//	                                 contains it (pinned and group rows are
//	                                 filtered client-side; they are few)
//	agents_limit                     page size of the flat agent list (0 = totals only)
//	expand                           group name whose member folders to embed
//	                                 (MemberBoxes); repeatable
//	members_limit                    "<n>:<group name>": page size of that
//	                                 expanded group's member folders; repeatable
//	selected                         the open folder: its agent entry is
//	                                 returned as selected_mailbox even when it
//	                                 is off the page
//
// Besides mailboxes (pinned, groups, then the agent page) the response
// carries agents_total / agents_unread over every agent folder matching the
// filters (not just the page), and selected_valid — whether the selected
// agent folder is still visible under the retired / empty / previous-
// generation filters (ignoring q and paging), echoed with the selected id it
// describes, so the client can snap a stranded selection back to the
// firehose.
//
// Per-folder decoration (title, agent_id, liveness) is batch-loaded only for
// the folders the response carries, so the cost tracks the page, not the
// number of agents.
func handleDashboardMailboxes(w http.ResponseWriter, r *http.Request) {
	if !checkDashboardAuth(w, r) {
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	qv := r.URL.Query()
	includeRetired := mailboxIncludeRetired(r)
	includeEmpty := mailboxIncludeEmpty(r)
	hidePrevGens := isTruthy(qv.Get("hide_prev_gens"))
	filter := strings.ToLower(strings.TrimSpace(qv.Get("q")))
	agentsLimit := rosterLimit(qv.Get("agents_limit"))
	selected := qv.Get("selected")
	expanded := map[string]bool{}
	for _, name := range qv["expand"] {
		expanded[name] = true
	}
	membersLimit := map[string]int{}
	for _, v := range qv["members_limit"] {
		if n, name, ok := strings.Cut(v, ":"); ok {
			membersLimit[name] = rosterLimit(n)
		}
	}

	retiredSet, err := retiredAgentConvs()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	counts, err := db.MailboxCounts()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	prevGens := map[string]struct{}{}
	if ids, err := db.ListPrevGenConvIDs(); err == nil {
		for _, c := range ids {
			prevGens[c] = struct{}{}
		}
	}
	groupsByConv, _ := db.GroupNamesByConv()
	aliveSessions, _ := cachedLiveTmuxSessions()

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

	// visible applies the roster's scope toggles. Retired agents are hidden
	// unless the operator opted in; when shown they carry the Retired flag.
	// Agents with an empty mailbox (neither sent nor received any mail) are
	// hidden unless opted in — a conv with any mail is in counts, so an empty
	// mailbox only ever comes from the active-agent set, never a folder that
	// holds messages, and hiding it changes nothing the "all" view counts.
	// Predecessor generations are hidden on request (hide_prev_gens) — only
	// their folder, never their messages.
	visible := func(conv string) bool {
		if _, ok := convSet[conv]; !ok {
			return false
		}
		if _, retired := retiredSet[conv]; retired && !includeRetired {
			return false
		}
		if c := counts[conv]; c.In == 0 && c.Out == 0 && !includeEmpty {
			return false
		}
		if _, prev := prevGens[conv]; prev && hidePrevGens {
			return false
		}
		return true
	}
	var titleMatches map[string]struct{}
	if filter != "" {
		titleMatches = map[string]struct{}{}
		if ids, err := db.MailboxRosterMatches(filter); err == nil {
			for _, c := range ids {
				titleMatches[c] = struct{}{}
			}
		}
	}
	matchesFilter := func(conv string) bool {
		if filter == "" {
			return true
		}
		if _, ok := titleMatches[conv]; ok {
			return true
		}
		if strings.Contains(strings.ToLower(conv), filter) {
			return true
		}
		for _, g := range groupsByConv[conv] {
			if strings.Contains(strings.ToLower(g), filter) {
				return true
			}
		}
		return false
	}

	// The flat agent list: every visible, matching folder, newest mail on
	// top the way a mail client lists folders by last activity, conv-id as
	// the stable tiebreak; then one page of it.
	candidates := make([]string, 0, len(convSet))
	agentsUnread := 0
	for conv := range convSet {
		if visible(conv) && matchesFilter(conv) {
			candidates = append(candidates, conv)
			agentsUnread += counts[conv].Unread
		}
	}
	byActivity := func(convs []string) {
		sort.Slice(convs, func(i, j int) bool {
			li, lj := counts[convs[i]].Last, counts[convs[j]].Last
			if !li.Equal(lj) {
				return li.After(lj)
			}
			return convs[i] < convs[j]
		})
	}
	byActivity(candidates)
	page := candidates[:min(agentsLimit, len(candidates))]

	allTotal, groupTotals, err := db.MailboxRosterTotals(!includeRetired)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	groupBoxes := buildGroupMailboxes(groupTotals, retiredSet, includeRetired, expanded)

	// Each expanded group's nested list: its visible, matching members,
	// newest activity first, one page of them.
	memberPages := make([][]string, len(groupBoxes))
	need := append([]string(nil), page...)
	for i := range groupBoxes {
		g := &groupBoxes[i]
		if !expanded[g.Title] {
			continue
		}
		var shown []string
		for _, conv := range g.memberConvs {
			if visible(conv) && matchesFilter(conv) {
				shown = append(shown, conv)
			}
		}
		byActivity(shown)
		limit, ok := membersLimit[g.Title]
		if !ok {
			limit = defaultRosterAgents
		}
		g.MembersTotal = len(shown)
		memberPages[i] = shown[:min(limit, len(shown))]
		need = append(need, memberPages[i]...)
	}
	// Decorate exactly the agent folders this response carries: the page,
	// the expanded groups' member pages, and the selected folder.
	if _, ok := convSet[selected]; ok {
		need = append(need, selected)
	}
	boxes := agentMailboxEntries(need, counts, retiredSet, prevGens, groupsByConv, aliveSessions)
	for i, convs := range memberPages {
		for _, conv := range convs {
			groupBoxes[i].MemberBoxes = append(groupBoxes[i].MemberBoxes, boxes[conv])
		}
	}

	mailboxes := make([]dashboardMailbox, 0, len(page)+len(groupBoxes)+2)
	// The virtual "all messages" folder leads the list — the
	// chronological firehose of every agent-to-agent message, so the
	// operator can read traffic across convs in one place. Its total is
	// the distinct row count (not the In+Out sum the per-conv tallies
	// produce). Unread is left 0: "unread" is a per-recipient notion that
	// has no meaning for an aggregate view. When retired agents are hidden
	// the count matches the filtered firehose.
	mailboxes = append(mailboxes, dashboardMailbox{
		ID:    allMailboxID,
		Kind:  "all",
		Title: "All agent messages",
		Total: allTotal,
	})
	// The human.notify channel comes next — it is the operator's own
	// folder, distinct from the agent-to-agent traffic.
	humanTotal, humanUnread, humanLastAt, err := db.HumanMessageStats()
	if err != nil {
		slog.Warn("mailbox: human message stats failed", "error", err)
	}
	humanBox := dashboardMailbox{
		ID:     humanMailboxID,
		Kind:   "human",
		Title:  "Human notifications",
		In:     humanTotal,
		Total:  humanTotal,
		Unread: humanUnread,
	}
	if !humanLastAt.IsZero() {
		humanBox.LastAt = humanLastAt.Format(time.RFC3339)
	}
	mailboxes = append(mailboxes, humanBox)
	// Final order: [all, human] (pinned), then groups, then the agent page.
	mailboxes = append(mailboxes, groupBoxes...)
	for _, conv := range page {
		mailboxes = append(mailboxes, boxes[conv])
	}

	resp := map[string]any{
		"mailboxes":      mailboxes,
		"agents_total":   len(candidates),
		"agents_unread":  agentsUnread,
		"selected":       selected,
		"selected_valid": visible(selected),
	}
	if mb, ok := boxes[selected]; ok {
		resp["selected_mailbox"] = mb
	}
	writeJSON(w, http.StatusOK, resp)
}

// agentMailboxEntries builds the roster entries for convs, batch-loading
// their titles, agent ids and session liveness in one read per table.
func agentMailboxEntries(convs []string, counts map[string]db.MailboxCount,
	retiredSet, prevGens map[string]struct{}, groupsByConv map[string][]string,
	aliveSessions map[string]struct{}) map[string]dashboardMailbox {
	convIndex, _ := db.GetConvIndexBatch(convs)
	convAgents, _ := db.AgentsByConv(convs)
	sessions, _ := db.FindSessionsByConvIDs(convs)
	out := make(map[string]dashboardMailbox, len(convs))
	for _, conv := range convs {
		if _, done := out[conv]; done {
			continue
		}
		_, retired := retiredSet[conv]
		_, prev := prevGens[conv]
		c := counts[conv]
		title := agent.CachedTitleFromParts(convIndex[conv], convAgents[conv].PendingName)
		if title == agent.UnknownTitle {
			title = ""
		}
		mb := dashboardMailbox{
			ID:      conv,
			Kind:    "agent",
			Title:   title,
			Short:   agent.ShortID(conv),
			AgentID: convAgents[conv].AgentID,
			Online:  isConvOnlineInSessions(sessions[conv], aliveSessions),
			Retired: retired,
			PrevGen: prev,
			Groups:  groupsByConv[conv],
			In:      c.In,
			Out:     c.Out,
			Total:   c.In + c.Out,
			Unread:  c.Unread,
		}
		if !c.Last.IsZero() {
			mb.LastAt = c.Last.Format(time.RFC3339)
		}
		out[conv] = mb
	}
	return out
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
	FromAgent        string          `json:"from_agent,omitempty"`
	FromTitle        string          `json:"from_title,omitempty"`
	ToConv           string          `json:"to_conv,omitempty"`
	ToAgent          string          `json:"to_agent,omitempty"`
	ToTitle          string          `json:"to_title,omitempty"`
	ToRecipients     []recipientLine `json:"to_recipients,omitempty"`
	CcRecipients     []recipientLine `json:"cc_recipients,omitempty"`
	Group            string          `json:"group,omitempty"`
	Subject          string          `json:"subject,omitempty"`
	Body             string          `json:"body"`
	CreatedAt        string          `json:"created_at"`
	ForwardedTo      string          `json:"forwarded_to,omitempty"`
	ForwardState     string          `json:"forward_state,omitempty"`
	DeliveredAt      string          `json:"delivered_at,omitempty"`
	NudgeDiscardedAt string          `json:"nudge_discarded_at,omitempty"`
	Read             bool            `json:"read"`
	ParentID         int64           `json:"parent_id,omitempty"`
	// Attachment is the first file (single-card surfaces); Attachments
	// carries every file: those a notification published, or for agent mail
	// its stored attachments and the briefing files its body lists
	// (dashboard_agent_message_attachments.go).
	Attachment  *dashboardHumanMessageAttachment   `json:"attachment,omitempty"`
	Attachments []*dashboardHumanMessageAttachment `json:"attachments,omitempty"`
	// OperatorAuthored marks mail the human/operator sent to an agent. Those
	// rows carry an empty FromConv, which is indistinguishable on the wire
	// from an internal system handoff, so without this flag the frontend has
	// no sender to name and renders them party-less — invisible as human mail
	// in the aggregate "all" folder. Sourced from operator_agent_messages
	// (see db.OperatorAuthoredMessages), not from the sender columns.
	OperatorAuthored bool `json:"operator_authored,omitempty"`
	// Action is a notification's action link. A peer reading this mailbox never
	// gets it: see peerMailboxReadKey.
	Action *dashboardHumanMessageAction `json:"action,omitempty"`
	// A notification's decision, as dashboardHumanMessage carries it, for the
	// reader's buttons. A peer's reader shows them and answers on that node.
	Options     []string `json:"options,omitempty"`
	Recommended int      `json:"recommended,omitempty"`
	Decision    string   `json:"decision,omitempty"`
	DecidedAt   string   `json:"decided_at,omitempty"`
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
	// HasMore reports rows older than this page. With a before cursor it is
	// the only way to know whether the folder continues.
	HasMore bool
}

// mailboxWindow is which slice of a folder to serve: a 1-based offset page,
// or, when Before is set, the PageSize rows older than message id Before (a
// keyset cursor). Fleet views page with the cursor: it stays stable while
// new mail arrives and lets a client merge several nodes' folders without
// re-reading every node down to the page it shows (see fleet-pager.js).
type mailboxWindow struct {
	Page, PageSize int
	Before         int64
}

// handleDashboardMailbox serves
// GET /api/mailbox?id=<conv|human|all>&q=&sender=&page=&page_size=&before=&focus=&unread= — one
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
	sender := strings.TrimSpace(r.URL.Query().Get("sender"))
	page, pageSize := mailboxPageParams(r)
	before, _ := strconv.ParseInt(r.URL.Query().Get("before"), 10, 64)
	win := mailboxWindow{Page: page, PageSize: pageSize, Before: max(before, 0)}

	if id == humanMailboxID {
		focus, _ := strconv.ParseInt(r.URL.Query().Get("focus"), 10, 64)
		unread := r.URL.Query().Get("unread") == "1"
		p := humanMailboxPage(db.HumanMessageFilter{Sender: sender, Query: q, Unread: unread}, win, focus)
		if r.Context().Value(peerMailboxReadKey{}) != nil {
			for i := range p.Messages {
				p.Messages[i].Action = nil
			}
		}
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
		p, err := agentMailboxPage("", q, win, !mailboxIncludeRetired(r))
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
		// Hide retired members' traffic unless the operator opted in — the
		// same exclude the "all" firehose applies, so a member's DM to a
		// retired agent (and a retired sender's channel post) drops while a
		// current member's traffic and channel posts survive. A lookup error
		// fails the request (matches the all branch) rather than silently
		// falling open to the unfiltered group scope.
		p, err := groupMailboxPage(g.ID, q, win, !mailboxIncludeRetired(r))
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

	p, err := agentMailboxPage(id, q, win, false)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	title := agent.CachedTitle(id)
	if title == agent.UnknownTitle {
		title = ""
	}
	writeMailboxPage(w, map[string]any{
		"id":       id,
		"kind":     "agent",
		"title":    title,
		"short":    agent.ShortID(id),
		"agent_id": peerAgentID(id),
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
	head["has_more"] = p.HasMore
	// Tells a fleet client this node honours before=<id>; an older node
	// ignores it and serves the newest page instead.
	head["cursor_paging"] = true
	writeJSON(w, http.StatusOK, head)
}

// humanMailboxMessages maps the human_messages snapshot into the shared
// mailboxMessage shape so the reading pane renders uniformly. Every row
// is direction "in" — agents notifying the human.
func humanMailboxMessages(page []*db.HumanMessage) []mailboxMessage {
	rows, _ := humanMessageViews(page)
	out := make([]mailboxMessage, 0, len(rows))
	for _, m := range rows {
		out = append(out, mailboxMessage{
			ID:          m.ID,
			Direction:   "in",
			FromConv:    m.FromConv,
			FromAgent:   m.FromAgent,
			FromTitle:   m.FromTitle,
			Group:       m.Group,
			Subject:     m.Subject,
			Body:        m.Body,
			CreatedAt:   m.CreatedAt,
			Read:        m.Read,
			Attachment:  m.Attachment,
			Attachments: m.Attachments,
			Action:      m.Action,
			Options:     m.Options,
			Recommended: m.Recommended,
			Decision:    m.Decision,
			DecidedAt:   m.DecidedAt,
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
	// operatorMsgs is the operator-authored subset of the page being
	// decorated, prefilled in one query by withOperatorMessages. Nil when
	// the caller didn't prefill, which reads as "none" — a missing flag
	// only costs the human-operator label, never a dropped row.
	operatorMsgs map[int64]bool
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

// withOperatorMessages prefills the operator-authored set for one page of
// rows, so toMessage can flag each without a per-row query. Every id on the
// page is looked up, not just the sender-less ones: "operator mail always
// has an empty from_conv" holds for today's four writers but nothing in db
// enforces it, and the query is batched either way, so filtering would only
// buy a silent mis-attribution the day that changes. A lookup failure leaves
// the set empty rather than failing the page — the folder still lists
// correctly, only the human-operator attribution is lost — but it is logged,
// since a persistently failing lookup is otherwise invisible.
func (d *mailboxDecorator) withOperatorMessages(rows []*db.AgentMessage) *mailboxDecorator {
	ids := make([]int64, 0, len(rows))
	for _, m := range rows {
		ids = append(ids, m.ID)
	}
	set, err := db.OperatorAuthoredMessagesBatch(ids)
	if err != nil {
		slog.Warn("mailbox: operator-authored lookup failed; rows lose human-operator attribution",
			"rows", len(ids), "error", err)
		return d
	}
	d.operatorMsgs = set
	return d
}

func (d *mailboxDecorator) titleOf(c string) string {
	if c == "" {
		return ""
	}
	if t, ok := d.titleCache[c]; ok {
		return t
	}
	t := mailboxDisplayTitle(c)
	d.titleCache[c] = t
	return t
}

func mailboxDisplayTitle(id string) string {
	if id == "" {
		return ""
	}
	conv := id
	if strings.HasPrefix(id, db.AgentIDPrefix) {
		if cur, err := db.CurrentConvForAgent(id); err == nil && cur != "" {
			conv = cur
		}
	}
	t := agent.CachedTitle(conv)
	if t == agent.UnknownTitle {
		return ""
	}
	return t
}

// recipients renders the stored audience arrays for the reading pane. convs is
// the conv-id array; agents is the 1:1 stable-agent_id companion (JOH-284). Each
// line prefers the persisted agent_id (pruning-immune) and falls back to a live
// conv→agent lookup only on the empty slots (legacy pre-v79 rows / non-actor
// recipients) — mirroring decorateRecipients on the CLI side.
func (d *mailboxDecorator) recipients(convs, agents []string) []recipientLine {
	if len(convs) == 0 {
		return nil
	}
	ls := make([]recipientLine, 0, len(convs))
	for i, id := range convs {
		ls = append(ls, recipientLine{ConvID: id, AgentID: storedAgentOrResolve(agents, i, id), Title: d.titleOf(id)})
	}
	return ls
}

// toMessage maps one stored row into the reading-pane shape. dir is the
// direction relative to the folder being viewed ("in"/"out" for an agent
// folder, "" for the aggregate "all" folder, which renders from→to).
func (d *mailboxDecorator) toMessage(m *db.AgentMessage, dir string) mailboxMessage {
	mm := mailboxMessage{
		ID:               m.ID,
		Direction:        dir,
		FromConv:         m.FromConv,
		FromAgent:        m.FromAgent,
		FromTitle:        d.titleOf(m.FromConv),
		ToConv:           m.ToConv,
		ToAgent:          m.ToAgent,
		ToTitle:          d.titleOf(m.ToConv),
		ToRecipients:     d.recipients(m.ToRecipients, m.ToRecipientAgents),
		CcRecipients:     d.recipients(m.CcRecipients, m.CcRecipientAgents),
		Group:            d.groupNames[m.GroupID],
		Subject:          m.Subject,
		Body:             m.Body,
		CreatedAt:        m.CreatedAt.Format(time.RFC3339),
		Read:             !m.ReadAt.IsZero(),
		ParentID:         m.ParentID,
		OperatorAuthored: d.operatorMsgs[m.ID],
	}
	mm.ForwardedTo, mm.ForwardState = localHomeMailStatus(m.ID)
	if !m.DeliveredAt.IsZero() {
		mm.DeliveredAt = m.DeliveredAt.Format(time.RFC3339)
	}
	if !m.NudgeDiscardedAt.IsZero() {
		mm.NudgeDiscardedAt = m.NudgeDiscardedAt.Format(time.RFC3339)
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
// for free. excludeRetired drops rows touching retired agents (the "all"
// firehose sets it when the operator hasn't opted in; a specific folder
// never does). Counts: total = rows matching q within
// that scope, totalUnfiltered = rows in the (still exclude-scoped) folder
// regardless of q — so the pager and the search count both reflect the
// same hidden-retired view.
func agentMailboxPage(forConv, q string, win mailboxWindow, excludeRetired bool) (mailboxPage, error) {
	scope := db.MailboxFilter{ForConv: forConv, ExcludeRetired: excludeRetired}
	return mailboxPageForScope(scope, forConv, q, win)
}

// groupMailboxPage builds one newest-first page of a GROUP folder — all
// traffic touching any current member (sender or recipient) plus the
// group's own multicasts (see MailboxFilter.ScopeGroupID).
// Like the "all" firehose it has no single self to be relative to, so
// every row renders from→to (forConv == ""). excludeRetired drops retired
// members' traffic exactly as the "all" firehose does (the caller sets it
// unless the operator opted into "show retired"): a member's DM to a
// retired agent disappears, and a channel multicast survives unless its own
// sender is retired. false shows the whole scope (the opted-in case).
func groupMailboxPage(groupID int64, q string, win mailboxWindow, excludeRetired bool) (mailboxPage, error) {
	scope := db.MailboxFilter{ScopeGroupID: groupID, ExcludeRetired: excludeRetired}
	return mailboxPageForScope(scope, "", q, win)
}

// mailboxPageForScope is the shared count → search → clamp → list →
// decorate pipeline behind agentMailboxPage / groupMailboxPage. scope is
// the folder predicate (a specific agent, a group, or the unscoped "all"
// firehose); forConv labels each row's direction relative to the folder
// ("in"/"out" for an agent folder, "" for an aggregate folder that
// renders from→to).
func mailboxPageForScope(scope db.MailboxFilter, forConv, q string, win mailboxWindow) (mailboxPage, error) {
	pageSize := win.PageSize
	totalUnfiltered, err := db.CountMailbox(scope)
	if err != nil {
		return mailboxPage{}, err
	}

	filter := scope
	total := totalUnfiltered
	if q != "" {
		filter.Text = q
		if total, err = db.CountMailbox(filter); err != nil {
			return mailboxPage{}, err
		}
	}

	servedPage, offset := clampOffset(win.Page, pageSize, total)
	if win.Before > 0 {
		servedPage, offset = 1, 0
		filter.BeforeID = win.Before
	}
	rows, err := db.ListMailboxPage(filter, pageSize, offset)
	if err != nil {
		return mailboxPage{}, err
	}
	hasMore := offset+len(rows) < total
	if win.Before > 0 {
		hasMore = false
		if len(rows) == pageSize {
			older := filter
			older.BeforeID = rows[len(rows)-1].ID
			next, err := db.ListMailboxPage(older, 1, 0)
			if err != nil {
				return mailboxPage{}, err
			}
			hasMore = len(next) > 0
		}
	}
	dec := newMailboxDecorator().withOperatorMessages(rows)
	attachments := agentMessageAttachmentViews(rows)
	msgs := make([]mailboxMessage, 0, len(rows))
	for _, m := range rows {
		mm := dec.toMessage(m, directionFor(forConv, m))
		if list := attachments[m.ID]; len(list) > 0 {
			mm.Attachments, mm.Attachment = list, list[0]
		}
		msgs = append(msgs, mm)
	}
	return mailboxPage{
		Messages:        msgs,
		Page:            servedPage,
		PageSize:        pageSize,
		Total:           total,
		TotalUnfiltered: totalUnfiltered,
		HasMore:         hasMore,
	}, nil
}

// buildGroupMailboxes builds one sidebar roster entry per group — the
// "view this group's messages" folders. Each carries the distinct row
// count in the group scope as its Total (totals, from db.MailboxRosterTotals)
// and its current member count; memberships load in one batch query. Groups
// named in expanded also carry their member conv-ids (memberConvs).
//
// retiredSet (the retired-agent conv-ids) and includeRetired together
// govern two retired-aware adjustments:
//   - The badge total: when the operator hasn't opted in, retired members'
//     traffic is excluded — the roster twin of the group folder's
//     own exclude (groupMailboxPage), so the badge matches the folder it
//     labels. When opted in nothing is excluded and the whole scope counts.
//   - The nested member list: when the operator HAS opted in, memberConvs is
//     augmented with the group's retired ex-members (recovered from message
//     history, db.GroupMessageParticipantsAll) so the sidebar can nest them
//     under the group they used to belong to. Retire unjoins a member from
//     every group, so a retired ex-member is gone from the live membership
//     and would otherwise never nest even though its flat agent folder is
//     shown. Members stays the *current* member count.
func buildGroupMailboxes(totals map[int64]int, retiredSet map[string]struct{}, includeRetired bool, expanded map[string]bool) []dashboardMailbox {
	groups, err := db.ListAgentGroups()
	if err != nil {
		return nil
	}
	// Skip archived (soft-deleted) groups — the Groups tab hides them from
	// its default listing, so they carry no "view messages" deep link;
	// listing a folder for one here would be an unreachable, inconsistent
	// stray. (An archived group can still be opened directly by id if
	// something already holds the selection; the handler resolves it — this
	// only governs the roster.)
	live := make([]*db.AgentGroup, 0, len(groups))
	ids := make([]int64, 0, len(groups))
	for _, g := range groups {
		if !g.IsArchived() {
			live = append(live, g)
			ids = append(ids, g.ID)
		}
	}
	membersByGroup, err := db.ListAgentGroupMembersBatch(ids)
	if err != nil {
		return nil
	}
	var participants map[int64][]string
	if includeRetired && len(retiredSet) > 0 && len(expanded) > 0 {
		participants, _ = db.GroupMessageParticipantsAll()
	}
	out := make([]dashboardMailbox, 0, len(live))
	for _, g := range live {
		members := make([]string, 0, len(membersByGroup[g.ID]))
		for _, m := range membersByGroup[g.ID] {
			if m.ConvID != "" {
				members = append(members, m.ConvID)
			}
		}
		var memberConvs []string
		if expanded[g.Name] {
			memberConvs = members
			if participants != nil {
				memberConvs = withRetiredParticipants(members, participants[g.ID], retiredSet)
			}
		}
		out = append(out, dashboardMailbox{
			ID:          groupMailboxPrefix + g.Name,
			Kind:        "group",
			Title:       g.Name,
			Total:       totals[g.ID],
			Members:     len(members),
			memberConvs: memberConvs,
		})
	}
	return out
}

// withRetiredParticipants returns members with the group's retired
// ex-members appended — the participants (convs with group-routed traffic
// for the group) that are in retiredSet, minus any already present. It is
// how a retired ex-member nests back under its former group on the Messages
// tab once the operator opts into retired agents: retire deletes the
// membership row, but the agent's group_id-stamped messages persist, so the
// association is recoverable. The order is current members first (their
// joined_at order), retired ex-members after.
func withRetiredParticipants(members, participants []string, retiredSet map[string]struct{}) []string {
	out := append([]string(nil), members...)
	have := make(map[string]struct{}, len(members))
	for _, m := range members {
		have[m] = struct{}{}
	}
	for _, p := range participants {
		if _, retired := retiredSet[p]; !retired {
			continue
		}
		if _, dup := have[p]; dup {
			continue
		}
		have[p] = struct{}{}
		out = append(out, p)
	}
	return out
}

// humanMailboxPage paginates the human_messages folder in SQL: the sender
// filter, search, count and page window all run in the query, so only the
// served page is read.
//
// focus, when it names a row in the filtered list, overrides page with the
// page holding that row: the snapshot only carries recent and unread
// notifications, so the browser cannot compute an older row's page itself.
func humanMailboxPage(filter db.HumanMessageFilter, win mailboxWindow, focus int64) mailboxPage {
	page, pageSize := win.Page, win.PageSize
	empty := mailboxPage{Messages: []mailboxMessage{}, Page: 1, PageSize: pageSize}
	total, err := db.CountHumanMessages(filter)
	if err != nil {
		slog.Warn("mailbox: count human messages failed", "error", err)
		return empty
	}
	totalUnfiltered := total
	if filter != (db.HumanMessageFilter{}) {
		if totalUnfiltered, err = db.CountHumanMessages(db.HumanMessageFilter{}); err != nil {
			slog.Warn("mailbox: count human messages failed", "error", err)
			return empty
		}
	}
	if focus > 0 {
		if i, err := db.HumanMessagePosition(filter, focus); err == nil && i >= 0 {
			page = i/pageSize + 1
		}
	}
	servedPage, offset := clampOffset(page, pageSize, total)
	window := filter
	if win.Before > 0 {
		servedPage, offset = 1, 0
		window.BeforeID = win.Before
	}
	rows, err := db.ListHumanMessagesPage(window, offset, pageSize)
	if err != nil {
		slog.Warn("mailbox: list human messages failed", "error", err)
		return empty
	}
	hasMore := offset+len(rows) < total
	if win.Before > 0 {
		hasMore = false
		if len(rows) == pageSize {
			older := filter
			older.BeforeID = rows[len(rows)-1].ID
			next, err := db.ListHumanMessagesPage(older, 0, 1)
			hasMore = err == nil && len(next) > 0
		}
	}
	return mailboxPage{
		Messages:        humanMailboxMessages(rows),
		Page:            servedPage,
		PageSize:        pageSize,
		Total:           total,
		TotalUnfiltered: totalUnfiltered,
		HasMore:         hasMore,
	}
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
