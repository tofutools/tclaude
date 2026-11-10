package agentd

import (
	"net/http"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/tofutools/tclaude/pkg/claude/agent"
)

// GET /api/companion/{agent}/peek — the dashboard companion's hover preview
// (tcl-6i0283). A minimized companion chip shows the last few lines of its
// agent's pane, read-only, so the operator can see what the agent is doing
// (or which prompt it is blocked on) without opening the terminal and taking
// the keyboard away from the page.
//
// It reads nothing a dashboard session could not already see: the same
// operator can attach the full terminal over /api/open-window-ws. The read is
// plain text (no escape sequences), bounded in lines and line length.

var companionAgentRE = regexp.MustCompile(`^agt_[0-9a-f]{32}$`)

const (
	companionPeekLines    = 14
	companionPeekLineMax  = 200
	companionPeekScanLast = 400
)

// capturePaneText is swapped by tests; production reads the pane through
// tmux under the shared subprocess deadline.
var capturePaneText = func(target string) (string, error) {
	out, err := tmuxOutputWithTimeout("capture-pane", "-p", "-J", "-t", target)
	return string(out), err
}

func handleDashboardCompanionPeek(w http.ResponseWriter, r *http.Request) {
	if !checkDashboardAuth(w, r) {
		return
	}
	agentID := r.PathValue("agent")
	if !companionAgentRE.MatchString(agentID) {
		writeError(w, http.StatusBadRequest, "invalid_arg", "agent must be an agent id")
		return
	}
	res, _, err := agent.ResolveSelector(agentID)
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "no such agent")
		return
	}
	sess := pickAliveSession(res.ConvID)
	if sess == nil {
		writeJSON(w, http.StatusOK, map[string]any{"agent_id": agentID, "online": false, "lines": []string{}})
		return
	}
	screen, err := capturePaneText(sess.TmuxSession + ":0.0")
	if err != nil {
		writeError(w, http.StatusBadGateway, "capture_failed", "could not read the agent's pane")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"agent_id": agentID, "online": true, "lines": companionPeekLinesOf(screen)})
}

// companionPeekLinesOf keeps the last non-blank stretch of a captured screen:
// trailing blank rows (an idle TUI's empty area) are dropped, then at most
// companionPeekLines rows remain, each clipped to companionPeekLineMax bytes on
// a rune boundary.
func companionPeekLinesOf(screen string) []string {
	rows := strings.Split(strings.ReplaceAll(screen, "\r", ""), "\n")
	if len(rows) > companionPeekScanLast {
		rows = rows[len(rows)-companionPeekScanLast:]
	}
	for len(rows) > 0 && strings.TrimSpace(rows[len(rows)-1]) == "" {
		rows = rows[:len(rows)-1]
	}
	if len(rows) > companionPeekLines {
		rows = rows[len(rows)-companionPeekLines:]
	}
	out := make([]string, 0, len(rows))
	for _, row := range rows {
		row = strings.TrimRight(row, " \t")
		if len(row) > companionPeekLineMax {
			cut := companionPeekLineMax
			for cut > 0 && !utf8.RuneStart(row[cut]) {
				cut--
			}
			row = row[:cut] + "…"
		}
		out = append(out, row)
	}
	return out
}
