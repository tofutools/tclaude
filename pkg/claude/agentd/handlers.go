package agentd

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/session"
)

// --- /v1/whoami ---

type whoamiResp struct {
	IsHuman bool     `json:"is_human"`
	ConvID  string   `json:"conv_id,omitempty"`
	Title   string   `json:"title,omitempty"`
	Groups  []string `json:"groups,omitempty"`
}

func handleWhoami(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method", "GET only")
		return
	}
	p := peerFromContext(r.Context())
	if p.ConvID == "" {
		writeJSON(w, http.StatusOK, whoamiResp{IsHuman: true})
		return
	}
	row := agent.FreshConvRow(p.ConvID)
	title := "(unnamed)"
	if row != nil {
		if t := agent.DisplayTitle(row); t != "" {
			title = t
		}
	}
	groups, _ := db.ListGroupsForConv(p.ConvID)
	gs := make([]string, 0, len(groups))
	for _, g := range groups {
		gs = append(gs, g.Name)
	}
	writeJSON(w, http.StatusOK, whoamiResp{ConvID: p.ConvID, Title: title, Groups: gs})
}

// --- /v1/lookup ---

func handleLookup(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method", "GET only")
		return
	}
	if _, ok := requireAgent(w, r); !ok {
		return
	}
	selector := r.URL.Query().Get("selector")
	if selector == "" {
		writeError(w, http.StatusBadRequest, "invalid_arg", "missing selector")
		return
	}
	res, matches, err := agent.ResolveSelector(selector)
	if errors.Is(err, agent.ErrAmbiguous) {
		writeJSON(w, http.StatusConflict, map[string]any{
			"error":      "selector matches multiple conversations",
			"code":       "ambiguous",
			"candidates": peerEntriesFromResolved(matches),
		})
		return
	}
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"conv_id": res.ConvID})
}

// --- /v1/peers ---

type peerEntry struct {
	ConvID string   `json:"conv_id"`
	Title  string   `json:"title"`
	Alias  string   `json:"alias,omitempty"`
	Role   string   `json:"role,omitempty"`
	Descr  string   `json:"descr,omitempty"`
	Groups []string `json:"groups"`
}

func handlePeers(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method", "GET only")
		return
	}
	myID, ok := requireAgent(w, r)
	if !ok {
		return
	}
	groups, err := db.ListGroupsForConv(myID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	byConv := map[string]*peerEntry{}
	for _, g := range groups {
		members, _ := db.ListAgentGroupMembers(g.ID)
		for _, m := range members {
			if m.ConvID == myID {
				continue
			}
			pe, exists := byConv[m.ConvID]
			if !exists {
				row, _ := db.GetConvIndex(m.ConvID)
				title := "(unknown)"
				if row != nil {
					if t := agent.DisplayTitle(row); t != "" {
						title = t
					}
				}
				pe = &peerEntry{
					ConvID: m.ConvID,
					Title:  title,
					Alias:  m.Alias,
					Role:   m.Role,
					Descr:  m.Descr,
				}
				byConv[m.ConvID] = pe
			}
			pe.Groups = append(pe.Groups, g.Name)
		}
	}
	out := make([]*peerEntry, 0, len(byConv))
	for _, pe := range byConv {
		out = append(out, pe)
	}
	writeJSON(w, http.StatusOK, out)
}

func peerEntriesFromResolved(rs []*agent.Resolved) []*peerEntry {
	out := make([]*peerEntry, 0, len(rs))
	for _, r := range rs {
		title := ""
		if r.Row != nil {
			title = agent.DisplayTitle(r.Row)
		}
		out = append(out, &peerEntry{ConvID: r.ConvID, Title: title})
	}
	return out
}

// --- /v1/messages (POST), /v1/messages/{id} (GET) ---

type sendReq struct {
	To      string `json:"to"`
	Subject string `json:"subject,omitempty"`
	Body    string `json:"body"`
}

type sendResp struct {
	ID        int64  `json:"id"`
	Delivered bool   `json:"delivered"`
	ViaGroup  string `json:"via_group"`
}

func handleMessages(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method", "POST only")
		return
	}
	fromID, ok := requireAgent(w, r)
	if !ok {
		return
	}
	var req sendReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", err.Error())
		return
	}
	if strings.TrimSpace(req.Body) == "" {
		writeError(w, http.StatusBadRequest, "invalid_arg", "body is empty")
		return
	}
	target, matches, err := agent.ResolveSelector(req.To)
	if errors.Is(err, agent.ErrAmbiguous) {
		writeJSON(w, http.StatusConflict, map[string]any{
			"error":      "target matches multiple conversations",
			"code":       "ambiguous",
			"candidates": peerEntriesFromResolved(matches),
		})
		return
	}
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", err.Error())
		return
	}
	if target.ConvID == fromID {
		writeError(w, http.StatusBadRequest, "invalid_arg", "cannot message self")
		return
	}
	shared, err := db.SharedGroupsForConvs(fromID, target.ConvID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	if len(shared) == 0 {
		writeError(w, http.StatusForbidden, "auth", "not in a shared group with target")
		return
	}
	via := shared[0]
	id, err := db.InsertAgentMessage(&db.AgentMessage{
		GroupID:  via.ID,
		FromConv: fromID,
		ToConv:   target.ConvID,
		Subject:  req.Subject,
		Body:     req.Body,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	delivered := nudgeIfAlive(id, fromID, target.ConvID, via.ID, via.Name, req.Subject)
	writeJSON(w, http.StatusOK, sendResp{ID: id, Delivered: delivered, ViaGroup: via.Name})
}

// nudgeIfAlive looks up the target's tmux session and, if alive, sends
// the bracketed system-style nudge. Returns true on successful delivery.
//
// This is the half that broke for sandboxed senders in v1: the daemon
// owns the tmux side here, so the sender's sandbox is irrelevant.
//
// The DB can hold multiple session rows for the same conv_id (auto-register
// creates new rows alongside stale ones from previous launches). We pick
// the first one whose tmux session is actually alive, most-recent first.
func nudgeIfAlive(msgID int64, fromID, toID string, groupID int64, groupName, subject string) bool {
	candidates, err := db.FindSessionsByConvID(toID)
	if err != nil {
		return false
	}
	var sess *db.SessionRow
	for _, c := range candidates {
		if c.TmuxSession == "" {
			continue
		}
		if session.IsTmuxSessionAlive(c.TmuxSession) {
			sess = c
			break
		}
	}
	if sess == nil {
		return false
	}
	fromAlias := agent.AliasFor(groupID, fromID)
	if fromAlias == "" {
		fromAlias = "(unnamed)"
	}
	subjectClause := ""
	if subject != "" {
		subjectClause = fmt.Sprintf(" subject=%q", subject)
	}
	nudge := fmt.Sprintf(
		"[system: new agent message #%d from %s (%s) in group %q.%s read with: tclaude agent inbox read %d. reply with: tclaude agent message %s \"...\".]",
		msgID, fromAlias, agent.ShortID(fromID), groupName, subjectClause, msgID, agent.ShortID(fromID),
	)
	target := sess.TmuxSession + ":0.0"
	// Two-step send: the Enter in the first call lands as a newline inside
	// CC's input textarea, so a second Enter is needed to actually submit.
	// Same pattern as pkg/claude/task/run.go's sendTmuxMessage / sendTmuxEnter.
	if err := clcommon.TmuxCommand("send-keys", "-t", target, nudge, "Enter").Run(); err != nil {
		slog.Warn("nudge failed (text)", "error", err, "tmux", sess.TmuxSession)
		return false
	}
	if err := clcommon.TmuxCommand("send-keys", "-t", target, "Enter").Run(); err != nil {
		slog.Warn("nudge failed (submit)", "error", err, "tmux", sess.TmuxSession)
		return false
	}
	_ = db.MarkAgentMessageDelivered(msgID)
	return true
}

// --- /v1/messages/{id} (GET) ---

func handleMessageByID(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method", "GET only")
		return
	}
	myID, ok := requireAgent(w, r)
	if !ok {
		return
	}
	idStr := strings.TrimPrefix(r.URL.Path, "/v1/messages/")
	if idStr == "" || strings.Contains(idStr, "/") {
		writeError(w, http.StatusBadRequest, "invalid_arg", "missing id")
		return
	}
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", "invalid id")
		return
	}
	m, err := db.GetAgentMessage(id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	if m == nil {
		writeError(w, http.StatusNotFound, "not_found", fmt.Sprintf("no message #%d", id))
		return
	}
	if m.ToConv != myID {
		writeError(w, http.StatusForbidden, "auth", "message is not addressed to you")
		return
	}
	if r.URL.Query().Get("keep-unread") != "1" && m.ReadAt.IsZero() {
		_ = db.MarkAgentMessageRead(id)
	}
	groupName := ""
	if g, _ := groupByID(m.GroupID); g != nil {
		groupName = g.Name
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id":         m.ID,
		"from":       m.FromConv,
		"from_alias": agent.AliasFor(m.GroupID, m.FromConv),
		"to":         m.ToConv,
		"group":      groupName,
		"subject":    m.Subject,
		"body":       m.Body,
		"created_at": m.CreatedAt.Format(time.RFC3339),
	})
}

// --- /v1/inbox ---

type inboxItem struct {
	ID        int64  `json:"id"`
	From      string `json:"from"`
	FromShort string `json:"from_short"`
	Group     string `json:"group"`
	Subject   string `json:"subject,omitempty"`
	Preview   string `json:"preview,omitempty"`
	CreatedAt string `json:"created_at"`
	Read      bool   `json:"read"`
}

func handleInbox(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method", "GET only")
		return
	}
	myID, ok := requireAgent(w, r)
	if !ok {
		return
	}
	limit := 20
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	unreadOnly := r.URL.Query().Get("unread") == "1" || r.URL.Query().Get("unread") == "true"
	msgs, err := db.ListAgentMessagesForConv(myID, limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	groupNames := map[int64]string{}
	if gs, err := db.ListAgentGroups(); err == nil {
		for _, g := range gs {
			groupNames[g.ID] = g.Name
		}
	}
	out := make([]inboxItem, 0, len(msgs))
	for _, m := range msgs {
		if unreadOnly && !m.ReadAt.IsZero() {
			continue
		}
		out = append(out, inboxItem{
			ID:        m.ID,
			From:      m.FromConv,
			FromShort: agent.ShortID(m.FromConv),
			Group:     groupNames[m.GroupID],
			Subject:   m.Subject,
			Preview:   bodyPreview(m.Body),
			CreatedAt: m.CreatedAt.Format(time.RFC3339),
			Read:      !m.ReadAt.IsZero(),
		})
	}
	writeJSON(w, http.StatusOK, out)
}

func bodyPreview(s string) string {
	const max = 80
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
}

func groupByID(id int64) (*db.AgentGroup, error) {
	groups, err := db.ListAgentGroups()
	if err != nil {
		return nil, err
	}
	for _, g := range groups {
		if g.ID == id {
			return g, nil
		}
	}
	return nil, nil
}

// --- /v1/groups (GET = anyone, POST = human only) ---

type groupSummary struct {
	Name    string `json:"name"`
	Descr   string `json:"descr,omitempty"`
	Members int    `json:"members"`
}

func handleGroups(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		// Anyone (token or not) can list groups.
		groups, err := db.ListAgentGroups()
		if err != nil {
			writeError(w, http.StatusInternalServerError, "io", err.Error())
			return
		}
		out := make([]groupSummary, 0, len(groups))
		for _, g := range groups {
			members, _ := db.ListAgentGroupMembers(g.ID)
			out = append(out, groupSummary{Name: g.Name, Descr: g.Descr, Members: len(members)})
		}
		writeJSON(w, http.StatusOK, out)
	case http.MethodPost:
		if !requireHuman(w, r) {
			return
		}
		var body struct {
			Name  string `json:"name"`
			Descr string `json:"descr,omitempty"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_arg", err.Error())
			return
		}
		if body.Name == "" {
			writeError(w, http.StatusBadRequest, "invalid_arg", "name is required")
			return
		}
		if existing, _ := db.GetAgentGroupByName(body.Name); existing != nil {
			writeError(w, http.StatusConflict, "exists", "group already exists")
			return
		}
		id, err := db.CreateAgentGroup(body.Name, body.Descr)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "io", err.Error())
			return
		}
		writeJSON(w, http.StatusCreated, map[string]any{"id": id, "name": body.Name})
	default:
		writeError(w, http.StatusMethodNotAllowed, "method", "GET or POST")
	}
}

// --- /v1/groups/{name}* dispatcher ---

func handleGroupByName(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/v1/groups/")
	parts := strings.SplitN(rest, "/", 3)
	name := parts[0]
	if name == "" {
		writeError(w, http.StatusBadRequest, "invalid_arg", "missing group name")
		return
	}
	g, err := db.GetAgentGroupByName(name)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	if g == nil {
		writeError(w, http.StatusNotFound, "not_found", "no such group")
		return
	}

	// /v1/groups/{name}/members[*]
	if len(parts) >= 2 && parts[1] == "members" {
		switch r.Method {
		case http.MethodGet:
			handleGroupMembersList(w, r, g)
		case http.MethodPost:
			handleGroupMembersAdd(w, r, g)
		case http.MethodDelete:
			if len(parts) < 3 || parts[2] == "" {
				writeError(w, http.StatusBadRequest, "invalid_arg", "missing member id")
				return
			}
			handleGroupMembersRemove(w, r, g, parts[2])
		default:
			writeError(w, http.StatusMethodNotAllowed, "method", "GET, POST, or DELETE")
		}
		return
	}

	// /v1/groups/{name}
	switch r.Method {
	case http.MethodDelete:
		if !requireHuman(w, r) {
			return
		}
		if err := db.DeleteAgentGroup(name); err != nil {
			writeError(w, http.StatusConflict, "constraint", err.Error())
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		writeError(w, http.StatusMethodNotAllowed, "method", "DELETE")
	}
}

type memberJSON struct {
	ConvID string `json:"conv_id"`
	Title  string `json:"title"`
	Alias  string `json:"alias,omitempty"`
	Role   string `json:"role,omitempty"`
	Descr  string `json:"descr,omitempty"`
}

func handleGroupMembersList(w http.ResponseWriter, _ *http.Request, g *db.AgentGroup) {
	members, err := db.ListAgentGroupMembers(g.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	out := make([]memberJSON, 0, len(members))
	for _, m := range members {
		row, _ := db.GetConvIndex(m.ConvID)
		title := "(unknown)"
		if row != nil {
			if t := agent.DisplayTitle(row); t != "" {
				title = t
			}
		}
		out = append(out, memberJSON{
			ConvID: m.ConvID,
			Title:  title,
			Alias:  m.Alias,
			Role:   m.Role,
			Descr:  m.Descr,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

func handleGroupMembersAdd(w http.ResponseWriter, r *http.Request, g *db.AgentGroup) {
	if !requireHuman(w, r) {
		return
	}
	var body struct {
		Conv  string `json:"conv"`
		Alias string `json:"alias,omitempty"`
		Role  string `json:"role,omitempty"`
		Descr string `json:"descr,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", err.Error())
		return
	}
	if body.Conv == "" {
		writeError(w, http.StatusBadRequest, "invalid_arg", "conv is required")
		return
	}
	res, _, err := agent.ResolveSelector(body.Conv)
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", err.Error())
		return
	}
	if err := db.AddAgentGroupMember(&db.AgentGroupMember{
		GroupID: g.ID,
		ConvID:  res.ConvID,
		Alias:   body.Alias,
		Role:    body.Role,
		Descr:   body.Descr,
	}); err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"conv_id": res.ConvID})
}

func handleGroupMembersRemove(w http.ResponseWriter, r *http.Request, g *db.AgentGroup, convSelector string) {
	if !requireHuman(w, r) {
		return
	}
	res, _, err := agent.ResolveSelector(convSelector)
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", err.Error())
		return
	}
	if err := db.RemoveAgentGroupMember(g.ID, res.ConvID); err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

