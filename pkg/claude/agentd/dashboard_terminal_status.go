package agentd

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/session"
)

// dashboardTerminalStatusState is the exact subset of agentState used by the
// terminal-tab classifier. Keeping a separate wire type prevents standalone
// terminal polls from growing when the full dashboard roster gains fields.
type dashboardTerminalStatusState struct {
	Harness        string `json:"harness,omitempty"`
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
	AgentID string `json:"agent_id,omitempty"`
	ConvID  string `json:"conv_id"`
	Online  bool   `json:"online"`
	Waking  bool   `json:"waking,omitempty"`
	// Steward marks a live Steward, whose detached window offers shutting it down.
	Steward bool                         `json:"steward,omitempty"`
	State   dashboardTerminalStatusState `json:"state"`
	// Voice carries what a detached window has no snapshot for: how its mic
	// talks to the agent.
	Voice config.VoiceConfig `json:"voice"`
	// Keybindings carries dashboard.keybindings, so a detached window's
	// shortcuts follow the operator's rebindings too.
	Keybindings map[string]string `json:"keybindings,omitempty"`
}

// terminalStatusForSessions is the status-only counterpart of
// stateForConvInSessions. It deliberately skips context/model/cost telemetry,
// harness catalogs, broker accounting, and full Codex telemetry. Copilot log following supplies background counts.
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
		Harness:      pick.Harness,
		Status:       pick.Status,
		StatusDetail: session.StatusDetailForDisplay(pick.StatusDetail),
	}
	if online {
		refreshCopilotContextSnapshotOnRead(pick, true)
		refreshGeminiContextSnapshotOnRead(pick, true)
		codexInterruptedSubagents := refreshCodexInterruptedSubagentsForStatus(pick, true)
		live := reconcileLiveRowStatus(pick, codexInterruptedSubagents, func() backgroundCounts {
			return backgroundCountsOnRead(pick, true)
		})
		out.Status, out.StatusDetail = live.Status, live.StatusDetail
		out.SubagentCount = live.SubagentCount
		out.BgShellCount, out.MonitorCount = live.BgShellCount, live.MonitorCount
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
	cfg, _ := config.LoadShared()
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, dashboardTerminalAgentStatus{
		AgentID: resolved.AgentID,
		ConvID:  resolved.ConvID,
		Online:  online,
		Waking:  isConvWaking(resolved.ConvID),
		Steward: isLiveSteward(resolved.ConvID),
		State:   state,

		Voice:       cfg.Voice(),
		Keybindings: cfg.DashboardKeybindings(),
	})
}

// liveRowStatus is the reconciled status of a live session row: what the
// dashboard row and the detached terminal's title both show for it.
type liveRowStatus struct {
	Status        string
	StatusDetail  string
	SubagentCount int
	BgShellCount  int
	MonitorCount  int
}

// reconcileLiveRowStatus derives a live row's background counts and the status
// that has to agree with them. It is the one copy used by both the dashboard
// row (stateForConvInSessionsBatched) and the detached terminal
// (terminalStatusForSessions); the two used to carry their own copies, and the
// terminal's drifted away from the display filter on the detail.
//
// codexInterrupted holds sub-agent IDs a Codex rollout shows were interrupted
// without a SubagentStop hook; they are dropped from the hook ledger. background
// does the process-table reconcile, passed in so the dashboard can time it.
func reconcileLiveRowStatus(
	pick *db.SessionRow,
	codexInterrupted map[string]struct{},
	background func() backgroundCounts,
) liveRowStatus {
	out := liveRowStatus{
		Status:       pick.Status,
		StatusDetail: session.StatusDetailForDisplay(pick.StatusDetail),
	}
	// Prefer the TTL-filtered ledger over the raw cached count, so a phantom
	// entry (a sub-agent whose SubagentStop was lost) stops being displayed as
	// soon as it expires, even if no hook has fired since to sweep it from
	// storage. An empty ledger with a non-zero count is a row last written by a
	// pre-ledger hook binary: surface its raw count rather than hiding real work.
	if set := db.ParseSubagentSet(pick.SubagentsJSON); set != nil {
		for id := range codexInterrupted {
			delete(set, id)
		}
		out.SubagentCount = set.LiveCount(time.Now())
	} else {
		out.SubagentCount = pick.SubagentCount
	}
	out.SubagentCount = copilotAPISubagentCount(pick, out.SubagentCount)
	// Background shells are children of the harness process and monitors
	// belong to it. For a live row this re-derives both ledgers from the
	// agent's actual descendant processes in one pass. The badges are only
	// trustworthy because of this step, since the hook stream announces a
	// launch of either kind but never its end.
	counts := background()
	out.BgShellCount = counts.Shells
	out.MonitorCount = counts.Monitors
	// Keep the status consistent with the reconciled counts. The stored row's
	// status_detail was written by whichever hook last fired, off the counts AS
	// THEY WERE THEN, so any of the three cases below can leave it disagreeing
	// with the badges rendered beside it.
	//
	// Settling DOWN: a row can be stuck at main_agent_idle / "N subagents
	// running" when a ledger emptied without a stop event (TTL expiry, or a
	// background shell whose process is simply gone) and no later hook
	// re-settled it. The badges already read 0 then, so a busy detail next to
	// them would be self-contradicting.
	//
	// Holding UP: background activity can outlive not just the turn but the
	// hook that would have recorded it. Claude Code's idle_prompt Notification,
	// for one, sets a bare idle without consulting either ledger. Rendering that
	// agent as plain idle is self-contradicting when the adjacent badge says a
	// sub-agent, shell, or monitor is still working.
	//
	// PARTIAL change: two shells reconciled down to one moves neither boundary,
	// so without the last case the pill would keep reading "2 background shells
	// running" next to a ⚙+1 badge. Re-rendering the detail from the reconciled
	// counts costs nothing and cannot drift: it is the same formatter the hook
	// uses, so a row whose counts did NOT change is rewritten to the identical
	// string.
	//
	// Only the DISPLAY is adjusted in every case; the stored row converges on
	// the session's next hook.
	switch {
	case out.SubagentCount == 0 && out.BgShellCount == 0 && out.MonitorCount == 0:
		if out.Status == session.StatusMainAgentIdle {
			out.Status = session.StatusIdle
			out.StatusDetail = ""
		}
	case out.Status == session.StatusIdle:
		out.Status = session.StatusMainAgentIdle
		out.StatusDetail = session.BackgroundActivityDetail(out.SubagentCount, out.BgShellCount, out.MonitorCount)
	case out.Status == session.StatusMainAgentIdle:
		out.StatusDetail = session.BackgroundActivityDetail(out.SubagentCount, out.BgShellCount, out.MonitorCount)
	}
	return out
}
