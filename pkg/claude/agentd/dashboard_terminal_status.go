package agentd

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/session"
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

// terminalStatusForSessions is the status-only counterpart of
// stateForConvInSessions. It deliberately skips context/model/cost telemetry,
// harness catalogs, broker accounting, and Codex/Copilot read-through work.
// The pieces retained below are exactly those consumed by terminalTabStatus:
// live hook status, reconciled background counts, exit reason, and recovery.
func terminalStatusForSessions(
	rows []*db.SessionRow,
	aliveSet map[string]struct{},
) (dashboardTerminalStatusState, bool) {
	if len(rows) == 0 {
		return dashboardTerminalStatusState{}, false
	}
	pick := rows[0]
	online := false
	for _, row := range rows {
		if row.TmuxSession == "" {
			continue
		}
		if _, ok := aliveSet[row.TmuxSession]; ok {
			pick = row
			online = true
			break
		}
	}

	out := dashboardTerminalStatusState{
		Status:       pick.Status,
		StatusDetail: pick.StatusDetail,
	}
	if online {
		if set := db.ParseSubagentSet(pick.SubagentsJSON); set != nil {
			out.SubagentCount = set.LiveCount(time.Now())
		} else {
			out.SubagentCount = pick.SubagentCount
		}
		background := backgroundCountsOnRead(pick, true)
		out.BgShellCount = background.Shells
		out.MonitorCount = background.Monitors
		switch {
		case out.SubagentCount == 0 && out.BgShellCount == 0 && out.MonitorCount == 0:
			if out.Status == session.StatusMainAgentIdle {
				out.Status = session.StatusIdle
				out.StatusDetail = ""
			}
		case out.Status == session.StatusIdle:
			out.Status = session.StatusMainAgentIdle
			out.StatusDetail = session.BackgroundActivityDetail(
				out.SubagentCount, out.BgShellCount, out.MonitorCount)
		case out.Status == session.StatusMainAgentIdle:
			out.StatusDetail = session.BackgroundActivityDetail(
				out.SubagentCount, out.BgShellCount, out.MonitorCount)
		}
	} else {
		out.Status = session.StatusExited
		out.StatusDetail = ""
		if reason, err := db.GetSessionExitReason(pick.ID); err == nil {
			out.ExitReason = reason
		} else {
			slog.Warn("dashboard terminal status: read exit_reason failed",
				"session", pick.ID, "error", err)
		}
	}

	if recovery, err := db.AgentRecoveryForConv(pick.ConvID); err == nil && recovery != nil {
		now := time.Now()
		if recoveryStatusVisible(*recovery, pick.LastHook, online, now) {
			out.RecoveryStatus = recovery.Status
			out.RecoveryDetail = recoveryStateDetail(*recovery, now)
		}
	} else if err != nil {
		slog.Warn("dashboard terminal status: read agent recovery failed",
			"conv", pick.ConvID, "error", err)
	}
	return out, online
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
	state, online := terminalStatusForSessions(rows, alive)
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, dashboardTerminalAgentStatus{
		AgentID: resolved.AgentID,
		ConvID:  resolved.ConvID,
		Online:  online,
		Waking:  isConvWaking(resolved.ConvID),
		State:   state,
	})
}
