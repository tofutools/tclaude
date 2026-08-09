package agentd

import (
	"net/http"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// dashboardTerminalStatusState is the exact subset of agentState used by the
// terminal-tab classifier. Keeping a separate wire type prevents standalone
// terminal polls from growing when the full dashboard roster gains fields.
type dashboardTerminalStatusState struct {
	Status         string `json:"status,omitempty"`
	StatusDetail   string `json:"status_detail,omitempty"`
	SubagentCount  int    `json:"subagent_count,omitempty"`
	BgShellCount   int    `json:"bg_shell_count,omitempty"`
	MonitorCount   int    `json:"monitor_count,omitempty"`
	ExitReason     string `json:"exit_reason,omitempty"`
	RecoveryStatus string `json:"recovery_status,omitempty"`
	RecoveryDetail string `json:"recovery_detail,omitempty"`
}

type dashboardTerminalAgentStatus struct {
	AgentID string                       `json:"agent_id,omitempty"`
	ConvID  string                       `json:"conv_id"`
	Online  bool                         `json:"online"`
	Waking  bool                         `json:"waking,omitempty"`
	State   dashboardTerminalStatusState `json:"state"`
}

// writeDashboardTerminalAgentStatus returns only the one-agent state needed by a
// detached terminal's title. The dashboard tab already receives this state
// from its shared /api/snapshot poll; the standalone page has no shared store.
func writeDashboardTerminalAgentStatus(w http.ResponseWriter, selector string) {
	resolved, _, err := agent.ResolveSelectorCached(selector)
	if err != nil {
		http.Error(w, "resolve agent: "+err.Error(), http.StatusNotFound)
		return
	}

	alive, _ := cachedLiveTmuxSessions()
	rows, err := db.FindSessionsByConvID(resolved.ConvID)
	if err != nil {
		http.Error(w, "read agent status: "+err.Error(), http.StatusInternalServerError)
		return
	}
	state := stateForConvInSessions(rows, alive)
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, dashboardTerminalAgentStatus{
		AgentID: resolved.AgentID,
		ConvID:  resolved.ConvID,
		Online:  isConvOnlineInSessions(rows, alive),
		Waking:  isConvWaking(resolved.ConvID),
		State: dashboardTerminalStatusState{
			Status:         state.Status,
			StatusDetail:   state.StatusDetail,
			SubagentCount:  state.SubagentCount,
			BgShellCount:   state.BgShellCount,
			MonitorCount:   state.MonitorCount,
			ExitReason:     state.ExitReason,
			RecoveryStatus: state.RecoveryStatus,
			RecoveryDetail: state.RecoveryDetail,
		},
	})
}
