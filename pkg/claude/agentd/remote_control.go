package agentd

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"time"

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
//   - DISABLING opens a confirm MENU (whose default highlight is NOT
//     "disconnect"), so the disable path submits the toggle and then drives
//     that menu — Up, Up, Enter — to select "disconnect".
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
// /remote-control toggle and driving its confirm menu on a DISABLE. Claude
// Code opens a confirm menu before turning Remote Access OFF; this delay lets
// it render before we send the menu keystrokes. Deliberately a touch longer
// than injectSettleDelay. A package var so flow tests can shrink it
// (SetRemoteControlConfirmDelayForTest).
var remoteControlConfirmDelay = 700 * time.Millisecond

// remoteControlMenuStepDelay is the gap between the individual keystrokes that
// drive CC's disable-confirm menu (remoteControlDisableMenuKeys). The menu has
// already rendered (remoteControlConfirmDelay); this is the settle each
// highlight move needs before the next key registers. Operator-tuned to a
// comfortable ~quarter-second after a too-short 150ms left moves dropping on a
// busy pane. A package var so flow tests can shrink it
// (SetRemoteControlConfirmDelayForTest shrinks both).
var remoteControlMenuStepDelay = 350 * time.Millisecond

// remoteControlDisableMenuKeys selects CC's "disconnect" entry in the confirm
// menu that opens when /remote-control is toggled while ON: two Ups move the
// highlight from the default entry up to "disconnect", then Enter chooses it
// (operator-verified). A single Enter would accept the default and leave
// Remote Access ON. These are compile-time constant keys — nothing
// user-derived rides the send-keys sink here. If a future CC build changes the
// menu layout, this slice is the one place to adjust.
var remoteControlDisableMenuKeys = []string{"Up", "Up", "Enter"}

// remoteControlResp is the JSON wire shape for the toggle endpoints. Action
// is one of "enabled" | "disabled" | "noop" | "status"; RemoteControl is the
// resulting effective state. CallerConv is set only on a cross-agent call.
type remoteControlResp struct {
	ConvID     string `json:"conv_id"`
	CallerConv string `json:"caller_conv,omitempty"`
	// CallerAgentID is the asking agent's stable actor key — companion to
	// CallerConv, set when a different agent asked. The CLI leads with this
	// (it survives the caller reincarnating) and falls back to CallerConv.
	CallerAgentID string `json:"caller_agent_id,omitempty"`
	RemoteControl bool   `json:"remote_control"`
	Action        string `json:"action"`
	Note          string `json:"note,omitempty"`
	// Observed is the state read DIRECTLY from the live pane footer on a
	// status call: "on" | "failed" | "off" | "unknown". Empty when the pane
	// wasn't read (no live session). It is distinct from RemoteControl, which
	// is the effective answer — the observed state when read confidently, else
	// the tracked best-known flag.
	Observed string `json:"observed,omitempty"`
	// Source records where RemoteControl came from: "pane" (observed from the
	// live footer) or "tracked" (tclaude's best-known flag).
	Source string `json:"source,omitempty"`
	// SessionURL is the claude.ai/code link parsed from the footer pill's
	// hyperlink when armed — i.e. where you actually connect. Best-effort.
	SessionURL string `json:"session_url,omitempty"`
}

// deliverRemoteControl injects the harness's remote-control toggle into the
// conv's live pane and, on a DISABLE, drives the confirm menu it opens with
// Up, Up, Enter to select "disconnect". Returns the session row it injected
// into (so the caller records the new
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
	toggle := h.Life.RemoteControlCommand()
	if disable {
		// DISABLE opens a confirm menu whose default highlight is NOT
		// "disconnect", so the menu must be driven by hand: submit the toggle
		// ONCE, let the menu render, then Up, Up, Enter to land on
		// "disconnect". This must NOT go through injectTextAndSubmit — that
		// helper's belt-and-suspenders second Enter would land on the menu and
		// accept its default ("keep connected"), dismissing it before our
		// Up,Up,Enter could select disconnect, which is exactly what left
		// Remote Access stuck ON. See injectMenuToggle for the full rationale.
		if err := injectMenuToggle(target, toggle, remoteControlDisableMenuKeys, remoteControlConfirmDelay, remoteControlMenuStepDelay); err != nil {
			slog.Warn("remote-control disable inject failed", "error", err, "tmux", sess.TmuxSession, "reason", reason)
			return nil, false
		}
	} else {
		// ENABLE does not open a confirm menu, so the plain type-and-submit
		// (with its paste-safe double Enter) is correct here.
		if err := injectTextAndSubmit(target, toggle); err != nil {
			slog.Warn("remote-control enable inject failed", "error", err, "tmux", sess.TmuxSession, "reason", reason)
			return nil, false
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

	// status: report the state. With a live pane we OBSERVE it directly (read
	// the /rc footer pill) and self-heal the tracked flag; without one we fall
	// back to the last-known tracked value. This is the only readback path and
	// it is on-demand only — never polled.
	if intent == "status" {
		tracked := false
		if sess != nil {
			tracked = sess.RemoteControl
		} else if rc, err := db.RemoteControlForConv(target); err == nil {
			tracked = rc
		}
		resp := remoteControlResp{ConvID: target, RemoteControl: tracked, Action: "status", Source: "tracked"}
		if sess == nil {
			resp.Note = "no live session; reporting last-known state"
		} else {
			obs := observeRemoteControl(sess.TmuxSession + ":0.0")
			resp.Observed = obs.state.String()
			switch obs.state {
			case rcSeenOn, rcSeenFailed:
				resp.RemoteControl = true
				resp.Source = "pane"
				resp.SessionURL = obs.sessionURL
				resp.Note = obs.note
				selfHealRemoteControl(sess, true)
			case rcSeenOff:
				resp.RemoteControl = false
				resp.Source = "pane"
				selfHealRemoteControl(sess, false)
			case rcUnknown:
				// Couldn't confirm from the pane — keep the tracked flag and
				// explain why (e.g. pane too narrow to draw the pill).
				resp.Note = obs.note
			}
		}
		if caller != "" && caller != target {
			resp.CallerConv = caller
			resp.CallerAgentID = peerAgentID(caller)
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
	// Pick the toggle DIRECTION from the live pane when we can read it
	// confidently, so a drifted tracked flag (someone toggled in-pane) can't
	// send the toggle the wrong way. Fall back to the tracked flag when the
	// pane is unreadable. Self-heal as a side effect.
	current := sess.RemoteControl
	if obs := observeRemoteControl(sess.TmuxSession + ":0.0"); obs.state != rcUnknown {
		current = obs.state.armed()
		selfHealRemoteControl(sess, current)
	}

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
			resp.CallerAgentID = peerAgentID(caller)
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
		resp.CallerAgentID = peerAgentID(caller)
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
