package agentd

import (
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// GET /api/scribe/{agent}/chat — the process editor's docked scribe chat.
//
// A scribe is talked to through operator mail (POST /api/operator-message);
// what it says back is its final message of each turn, which the Stop hook
// records for scribes only (db.RecordScribeTurnReply). This read merges both
// halves into one bounded, time-ordered transcript, plus — while a turn is
// open — a live tail of what the scribe has done so far in it. It serves
// scribes only, so it never becomes a general "read any agent's replies"
// surface.

var scribeChatAgentRE = regexp.MustCompile(`^agt_[0-9a-f]{32}$`)

const scribeChatPromptLimit = db.ScribeTurnReplyKeep

type scribeChatEntry struct {
	ID      string    `json:"id"`
	Role    string    `json:"role"` // "human" | "scribe"
	Subject string    `json:"subject,omitempty"`
	Body    string    `json:"body"`
	At      time.Time `json:"at"`
}

func handleDashboardScribeChat(w http.ResponseWriter, r *http.Request) {
	if !checkDashboardAuth(w, r) {
		return
	}
	agentID := r.PathValue("agent")
	if !scribeChatAgentRE.MatchString(agentID) {
		writeError(w, http.StatusBadRequest, "invalid_arg", "agent must be an agent id")
		return
	}
	ok, err := db.IsScribeAgent(agentID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "db", err.Error())
		return
	}
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "no such scribe")
		return
	}
	replies, err := db.ListScribeTurnReplies(agentID, 0)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "db", err.Error())
		return
	}
	prompts, err := db.ListOperatorMessagesToAgent(agentID, scribeChatPromptLimit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "db", err.Error())
		return
	}
	entries := make([]scribeChatEntry, 0, len(replies)+len(prompts))
	for _, p := range prompts {
		entries = append(entries, scribeChatEntry{ID: "h" + strconv.FormatInt(p.ID, 10), Role: "human", Subject: p.Subject, Body: p.Body, At: p.CreatedAt})
	}
	for _, reply := range replies {
		entries = append(entries, scribeChatEntry{ID: "s" + strconv.FormatInt(reply.ID, 10), Role: "scribe", Body: reply.Body, At: reply.CreatedAt})
	}
	sort.SliceStable(entries, func(i, j int) bool { return entries[i].At.Before(entries[j].At) })
	resp := map[string]any{"agent_id": agentID, "messages": entries}
	// A turn is open while the human spoke last; only then is the live tail
	// of the scribe's transcript worth reading (dashboard_scribe_chat_live.go).
	if n := len(entries); n > 0 && entries[n-1].Role == "human" {
		var since time.Time
		for _, reply := range replies {
			if reply.CreatedAt.After(since) {
				since = reply.CreatedAt
			}
		}
		resp["live"] = scribeLiveTail(agentID, since)
	}
	writeJSON(w, http.StatusOK, resp)
}
