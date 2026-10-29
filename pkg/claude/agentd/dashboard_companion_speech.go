package agentd

import (
	"net/http"
	"strconv"
	"time"
)

// GET /api/companion/{agent}/speech?since=<unix-nanos> — what an agent has
// said since `since`, for the companion's voice output (tcl-q3qcfw). When a
// pinned agent with voice on ends its turn, the companion reads its last text
// here and speaks it.
//
// It reuses the scribe chat's transcript live tail (scribeLiveTail), keeping
// only the agent's text blocks, never tool calls. It reads nothing a dashboard
// session could not already see: the same operator can attach the agent's
// full terminal over /api/open-window-ws. Like the peek, it is an own-node
// route, never served to a peer view.

type companionSpeechItem struct {
	Text string    `json:"text"`
	At   time.Time `json:"at"`
}

func handleDashboardCompanionSpeech(w http.ResponseWriter, r *http.Request) {
	if !checkDashboardAuth(w, r) {
		return
	}
	agentID := r.PathValue("agent")
	if !companionAgentRE.MatchString(agentID) {
		writeError(w, http.StatusBadRequest, "invalid_arg", "agent must be an agent id")
		return
	}
	var since time.Time
	if raw := r.URL.Query().Get("since"); raw != "" {
		ns, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || ns < 0 {
			writeError(w, http.StatusBadRequest, "invalid_arg", "since must be unix nanoseconds")
			return
		}
		since = time.Unix(0, ns)
	}
	now := time.Now()
	live := scribeLiveTail(agentID, since)
	items := []companionSpeechItem{}
	for _, it := range live.Items {
		// Only timestamped text can be ordered against the cursor.
		if it.Kind != "text" || it.At.IsZero() {
			continue
		}
		items = append(items, companionSpeechItem{Text: it.Text, At: it.At})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"items": items, "now": now.UnixNano(), "unavailable": live.Unavailable,
	})
}
