package agentd

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"time"

	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/claude/session"
)

// Remote-control toggle (JOH-257). Drives Claude Code's built-in Remote
// Access (the `/remote-control` slash) from the daemon, the dashboard and
// the `tclaude agent remote-control` CLI, so an operator can expose any
// agent in the fleet to claude.ai/code + the Claude mobile app without
// touching its pane by hand.
//
// Two facts shape this path:
//
//   - `/remote-control` is a TOGGLE with no programmatic readback. tclaude
//     tracks its own best-known state (sessions.remote_control, JOH-256);
//     the recorded flag decides the injection DIRECTION. Drift is possible
//     if a human toggles in-pane — the state is "best-known", not authoritative.
//   - DISABLING prompts for confirmation, so the disable path submits the
//     toggle and then an extra Enter to answer the prompt.
//
// This is a send-keys injection sink. The toggle token is a compile-time
// constant sourced from the harness Lifecycle (never user input), so nothing
// injectable rides the wire — but any change here still warrants a cold review.

// aliveSessionForConv returns the most-recently-updated session row for
// convID whose tmux session is currently alive, or nil when none is live.
// Both the lifecycle-slash injection sink (injectSlashCommand) and the
// remote-control toggle resolve their target pane through this, so a conv
// with several stale rows still picks the one actually running.
func aliveSessionForConv(convID string) *db.SessionRow {
	candidates, err := db.FindSessionsByConvID(convID)
	if err != nil {
		return nil
	}
	for _, c := range candidates {
		if c.TmuxSession != "" && session.IsTmuxSessionAlive(c.TmuxSession) {
			return c
		}
	}
	return nil
}

// remoteControlConfirmDelay is the pause between submitting the
// /remote-control toggle and sending the confirmation Enter on a DISABLE.
// Claude Code prompts before turning Remote Access OFF; the extra Enter
// answers that prompt. Deliberately a touch longer than injectSettleDelay so
// the confirm dialog has rendered before we answer it. A package var so flow
// tests can shrink it (SetRemoteControlConfirmDelayForTest). If a future CC
// build drops the confirm prompt, the extra Enter lands on an empty prompt —
// a harmless no-op.
var remoteControlConfirmDelay = 700 * time.Millisecond

// remoteControlResp is the JSON wire shape for the toggle endpoints. Action
// is one of "enabled" | "disabled" | "noop" | "status"; RemoteControl is the
// resulting best-known state. CallerConv is set only on a cross-agent call.
type remoteControlResp struct {
	ConvID        string `json:"conv_id"`
	CallerConv    string `json:"caller_conv,omitempty"`
	RemoteControl bool   `json:"remote_control"`
	Action        string `json:"action"`
	Note          string `json:"note,omitempty"`
}

// deliverRemoteControl injects the harness's remote-control toggle into the
// conv's live pane and, on a DISABLE, follows it with a confirmation Enter.
// Returns the session row it injected into (so the caller records the new
// state on that exact row) and whether delivery succeeded. The caller MUST
// have gated on Harness.CanRemoteControl() first.
func deliverRemoteControl(convID string, h *harness.Harness, disable bool, reason string) (*db.SessionRow, bool) {
	sess := aliveSessionForConv(convID)
	if sess == nil {
		return nil, false
	}
	target := sess.TmuxSession + ":0.0"
	// The toggle token is a compile-time constant from the harness Lifecycle
	// (CC's "/remote-control") — never user input — so the send-keys sink
	// carries nothing injectable.
	if err := injectTextAndSubmit(target, h.Life.RemoteControlCommand()); err != nil {
		slog.Warn("remote-control inject failed", "error", err, "tmux", sess.TmuxSession, "reason", reason)
		return nil, false
	}
	if disable {
		// Answer CC's "disable remote control?" confirmation.
		time.Sleep(remoteControlConfirmDelay)
		if err := clcommon.TmuxCommand("send-keys", "-t", target, "Enter").Run(); err != nil {
			// The toggle itself was submitted; the state may already have
			// flipped. Log and treat as delivered — a stuck confirm dialog
			// is a best-effort residual, not a failure of the toggle.
			slog.Warn("remote-control confirm-enter failed", "error", err, "tmux", sess.TmuxSession, "reason", reason)
		}
	}
	slog.Info("remote-control injected via send-keys",
		"conv_id", convID,
		"line", h.Life.RemoteControlCommand(),
		"disable", disable,
		"reason", reason,
		"tmux_session", sess.TmuxSession,
	)
	return sess, true
}

// handleWhoamiRemoteControl toggles the caller harness's built-in remote
// access on the caller's OWN pane. Permission-gated on self.remote-control.
func handleWhoamiRemoteControl(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method", "POST only")
		return
	}
	convID, ok := requirePermission(w, r, PermSelfRemoteControl)
	if !ok {
		return
	}
	if convID == "" {
		writeError(w, http.StatusBadRequest, "invalid_arg",
			"this endpoint toggles the calling agent's own remote control; humans should use the harness's /remote-control directly, or use POST /v1/agent/{conv}/remote-control to act on another agent")
		return
	}
	runRemoteControlOrchestration(w, r, convID, convID)
}

// handleAgentRemoteControl toggles ANOTHER agent's remote access. Routed via
// handleAgentByConv. Auth: agent.remote-control slug OR caller owns a group
// containing target.
func handleAgentRemoteControl(w http.ResponseWriter, r *http.Request, targetConv string) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method", "POST only")
		return
	}
	caller, ok := requireCrossAgentPermission(w, r, PermAgentRemoteControl, targetConv)
	if !ok {
		return
	}
	runRemoteControlOrchestration(w, r, targetConv, caller)
}

// runRemoteControlOrchestration is the shared body for the self + cross-agent
// toggle endpoints. It reads the intent (on|off|toggle|status), gates on the
// harness capability, and — for a mutating intent that actually changes the
// best-known state — injects the toggle (with the disable confirm) and records
// the new state on the live session row. caller is recorded in the response
// for cross-agent calls; for self it equals target.
func runRemoteControlOrchestration(w http.ResponseWriter, r *http.Request, target, caller string) {
	var body struct {
		Intent string `json:"intent"`
	}
	if r.ContentLength != 0 {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_arg", err.Error())
			return
		}
	}
	intent := strings.TrimSpace(strings.ToLower(body.Intent))
	if intent == "" {
		intent = "toggle"
	}
	switch intent {
	case "on", "off", "toggle", "status":
	default:
		writeError(w, http.StatusBadRequest, "invalid_arg",
			`intent must be one of "on", "off", "toggle", "status"`)
		return
	}

	h := harnessForConv(target)
	if !h.CanRemoteControl() {
		writeError(w, http.StatusConflict, "unsupported_harness",
			"harness "+h.Name+" has no built-in remote access; the remote-control toggle is unavailable for this agent")
		return
	}

	sess := aliveSessionForConv(target)

	// status: report best-known state without touching the pane.
	if intent == "status" {
		current := false
		if sess != nil {
			current = sess.RemoteControl
		} else if rc, err := db.RemoteControlForConv(target); err == nil {
			current = rc
		}
		resp := remoteControlResp{ConvID: target, RemoteControl: current, Action: "status"}
		if sess == nil {
			resp.Note = "no live session; reporting last-known state"
		}
		if caller != "" && caller != target {
			resp.CallerConv = caller
		}
		writeJSON(w, http.StatusOK, resp)
		return
	}

	// Mutating intents need a live pane to inject into.
	if sess == nil {
		writeError(w, http.StatusServiceUnavailable, "no_tmux",
			"target conv "+short8(target)+" has no live tmux session to toggle remote control on")
		return
	}
	current := sess.RemoteControl

	var desired bool
	switch intent {
	case "on":
		desired = true
	case "off":
		desired = false
	case "toggle":
		desired = !current
	}

	if desired == current {
		resp := remoteControlResp{
			ConvID:        target,
			RemoteControl: current,
			Action:        "noop",
			Note:          "remote control already " + onOff(current),
		}
		if caller != "" && caller != target {
			resp.CallerConv = caller
		}
		writeJSON(w, http.StatusOK, resp)
		return
	}

	disable := !desired
	reason := slashReason("remote-control "+onOff(desired), caller, target)
	injected, ok := deliverRemoteControl(target, h, disable, reason)
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "no_tmux",
			"could not deliver remote-control toggle to conv "+short8(target)+" (no live tmux session to inject into)")
		return
	}
	if err := db.SetSessionRemoteControl(injected.ID, desired); err != nil {
		// Injection landed but the state write failed: log and still report
		// the intended state — the pane has been toggled, so the recorded
		// flag is the residual that's out of step, not the pane.
		slog.Warn("remote-control state write failed after injection",
			"conv_id", target, "session_id", injected.ID, "desired", desired, "error", err)
	}
	action := "enabled"
	if disable {
		action = "disabled"
	}
	resp := remoteControlResp{ConvID: target, RemoteControl: desired, Action: action}
	if caller != "" && caller != target {
		resp.CallerConv = caller
	}
	writeJSON(w, http.StatusOK, resp)
}

// onOff renders a remote-control state as the words used in responses + audit.
func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}
