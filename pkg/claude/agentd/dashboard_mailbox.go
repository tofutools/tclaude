package agentd

import (
	"net/http"
	"sort"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/session"
)

// The dashboard mail client — a read-only introspection view over every
// mailbox the daemon stores, so the operator can see what agents
// actually said to each other (and to the human) when something goes
// wrong between them.
//
// Two cookie-authed GET endpoints, both dashboard-only twins of data
// the `tclaude agent inbox` CLI reads:
//
//   GET /api/mailboxes        -> handleDashboardMailboxes
//       Enumerates one mailbox per conv that has any agent-to-agent
//       mail (plus every active agent, even with an empty mailbox) and
//       the special "human" mailbox (the human.notify channel). Each
//       carries in/out/unread counts and a recency timestamp for the
//       sidebar.
//
//   GET /api/mailbox?id=<conv|human>  -> handleDashboardMailbox
//       Returns the selected mailbox's messages — for an agent mailbox,
//       its received + sent rows merged newest-first; for "human", the
//       human_messages rows. Titles are resolved so the reading pane
//       can render friendly sender/recipient names.
//
// These are *introspection* surfaces: viewing an agent mailbox never
// mutates that agent's read-state (that would corrupt the agent's own
// inbox view). Read/clear/delete actions remain only on the human
// mailbox, reusing the existing /api/human-messages/* endpoints.
//
// Wired into the dashboard mux from registerDashboardEditRoutes.

// humanMailboxID is the sentinel mailbox id for the human.notify
// channel. Agent mailboxes are keyed by conv-id (a UUID), so this
// reserved word never collides with a real conv.
const humanMailboxID = "human"

func registerDashboardMailboxRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/api/mailboxes", handleDashboardMailboxes)
	mux.HandleFunc("/api/mailbox", handleDashboardMailbox)
}

// dashboardMailbox is one sidebar entry. Kind is "human" or "agent".
type dashboardMailbox struct {
	ID     string   `json:"id"`
	Kind   string   `json:"kind"`
	Title  string   `json:"title"`
	Short  string   `json:"short,omitempty"`
	Online bool     `json:"online"`
	Groups []string `json:"groups,omitempty"`
	In     int      `json:"in"`
	Out    int      `json:"out"`
	Total  int      `json:"total"`
	Unread int      `json:"unread"`
	LastAt string   `json:"last_at,omitempty"`
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

	mailboxes := make([]dashboardMailbox, 0, len(convSet)+1)

	// The human.notify channel always leads the list — it is the
	// operator's own folder, distinct from the agent-to-agent traffic.
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
		c := counts[conv]
		title := agent.CachedTitle(conv)
		if title == agent.UnknownTitle {
			title = ""
		}
		mb := dashboardMailbox{
			ID:     conv,
			Kind:   "agent",
			Title:  title,
			Short:  agent.ShortID(conv),
			Online: isConvOnlineIn(conv, aliveSessions),
			Groups: groupsByConv[conv],
			In:     c.In,
			Out:    c.Out,
			Total:  c.In + c.Out,
			Unread: c.Unread,
		}
		if !c.Last.IsZero() {
			mb.LastAt = c.Last.Format(time.RFC3339)
		}
		mailboxes = append(mailboxes, mb)
	}

	// Sort agent mailboxes by recency — newest mail on top, the way a
	// mail client lists folders by last activity — then by title, then
	// conv-id for a stable tiebreak. The human folder stays pinned first
	// (mailboxes[0]); only the agent tail (mailboxes[1:]) is reordered.
	agents := mailboxes[1:]
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

// mailboxMessagesLimit caps how many messages either direction returns
// — generous (this is operator introspection, not an agent's working
// inbox) but bounded so a runaway mailbox can't blow up the response.
const mailboxMessagesLimit = 1000

// handleDashboardMailbox serves GET /api/mailbox?id=<conv|human> — the
// selected mailbox's messages. Cookie-authed (dashboard-only).
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

	if id == humanMailboxID {
		writeJSON(w, http.StatusOK, map[string]any{
			"id":       humanMailboxID,
			"kind":     "human",
			"title":    "Human notifications",
			"messages": humanMailboxMessages(),
		})
		return
	}

	msgs, err := agentMailboxMessages(id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	title := agent.CachedTitle(id)
	if title == agent.UnknownTitle {
		title = ""
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id":       id,
		"kind":     "agent",
		"title":    title,
		"short":    agent.ShortID(id),
		"messages": msgs,
	})
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

// agentMailboxMessages merges a conv's received (inbox) and sent
// (outbox) agent_messages into one newest-first list, deduplicating the
// rare self-addressed row that appears in both. Titles for the
// counterpart conv(s) are resolved best-effort for the reading pane.
func agentMailboxMessages(conv string) ([]mailboxMessage, error) {
	groupNames := map[int64]string{}
	if gs, err := db.ListAgentGroups(); err == nil {
		for _, g := range gs {
			groupNames[g.ID] = g.Name
		}
	}

	received, err := db.ListAgentMessagesForConv(conv, mailboxMessagesLimit)
	if err != nil {
		return nil, err
	}
	sent, err := db.ListAgentMessagesFromConv(conv, mailboxMessagesLimit)
	if err != nil {
		return nil, err
	}

	// Memoize title lookups: a two-agent thread repeats the same one or
	// two conv-ids across hundreds of rows, and this runs on every 2s
	// refresh while the folder is open, so caching collapses thousands
	// of GetConvIndex reads into a handful.
	titleCache := map[string]string{}
	titleOf := func(c string) string {
		if c == "" {
			return ""
		}
		if t, ok := titleCache[c]; ok {
			return t
		}
		t := agent.TitleFor(c)
		titleCache[c] = t
		return t
	}
	decorate := func(ids []string) []recipientLine {
		if len(ids) == 0 {
			return nil
		}
		ls := make([]recipientLine, 0, len(ids))
		for _, id := range ids {
			ls = append(ls, recipientLine{ConvID: id, Title: titleOf(id)})
		}
		return ls
	}

	out := make([]mailboxMessage, 0, len(received)+len(sent))
	seen := map[int64]struct{}{}
	add := func(m *db.AgentMessage, dir string) {
		if _, dup := seen[m.ID]; dup {
			return
		}
		seen[m.ID] = struct{}{}
		mm := mailboxMessage{
			ID:           m.ID,
			Direction:    dir,
			FromConv:     m.FromConv,
			FromTitle:    titleOf(m.FromConv),
			ToConv:       m.ToConv,
			ToTitle:      titleOf(m.ToConv),
			ToRecipients: decorate(m.ToRecipients),
			CcRecipients: decorate(m.CcRecipients),
			Group:        groupNames[m.GroupID],
			Subject:      m.Subject,
			Body:         m.Body,
			CreatedAt:    m.CreatedAt.Format(time.RFC3339),
			Read:         !m.ReadAt.IsZero(),
			ParentID:     m.ParentID,
		}
		if !m.DeliveredAt.IsZero() {
			mm.DeliveredAt = m.DeliveredAt.Format(time.RFC3339)
		}
		out = append(out, mm)
	}
	for _, m := range received {
		add(m, "in")
	}
	for _, m := range sent {
		add(m, "out")
	}

	// Newest first across the merged set (each side arrives sorted, but
	// the union is not).
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt != out[j].CreatedAt {
			return out[i].CreatedAt > out[j].CreatedAt
		}
		return out[i].ID > out[j].ID
	})
	return out, nil
}
