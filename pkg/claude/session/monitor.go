package session

import (
	"encoding/json"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// This file decodes the Claude Code hook payload that moves the monitor
// ledger (db.MonitorSet): the PostToolUse of a `Monitor` call. The
// retirement side is shared with background shells — a `TaskStop` names a
// task id from the one namespace both live in, and is decoded once in
// bgshell.go.
//
// As with the background-shell decoders, this shape is UNDOCUMENTED
// upstream and was established empirically: a Monitor call's tool_response
// is `{"taskId": "...", "timeoutMs": N, "persistent": bool}`. Every decoder
// here therefore fails closed and silently — a payload that does not match
// is simply not evidence, never an error that could fail a hook. The
// ledger's deadline, its TTL, and the daemon's liveness reconcile are what
// keep the badge honest if a harness version changes this shape.

// monitorToolName is the tool name the ledger reacts to.
const monitorToolName = "Monitor"

// monitorToolInput is the subset of a Monitor `tool_input` the ledger
// needs. Exactly one of Command and WS is present: a watch either runs a
// script or opens a socket, and the harness rejects a call carrying both.
type monitorToolInput struct {
	Command     string          `json:"command"`
	Description string          `json:"description"`
	WS          *monitorWSInput `json:"ws"`
	// TimeoutMs and Persistent are the CALLER's requested bounds. The
	// harness echoes both back in tool_response, which is the authority —
	// but the same hook carries them here too, so an unreadable response
	// need not cost the deadline. See monitorLaunch.
	TimeoutMs  int64 `json:"timeout_ms"`
	Persistent bool  `json:"persistent"`
}

// monitorWSInput is the websocket source. Only the URL is kept, as the
// entry's human-readable label when no description was given.
type monitorWSInput struct {
	URL string `json:"url"`
}

// monitorToolResponse is the subset of a Monitor `tool_response` the
// ledger needs: the harness's handle for the watch — the same id a later
// TaskStop names, and the ledger key — plus the deadline the harness will
// enforce on its own.
//
// TimeoutMs is only meaningful when Persistent is false; a persistent
// watch runs until the session ends or a TaskStop kills it.
type monitorToolResponse struct {
	TaskID     string `json:"taskId"`
	TimeoutMs  int64  `json:"timeoutMs"`
	Persistent bool   `json:"persistent"`
}

// harnessTracksMonitors reports whether this session's harness has
// monitors tclaude can track. An unknown or unresolvable harness folds to
// FALSE, for the same reason harnessTracksBackgroundShells does: adding
// ledger entries for a harness with no monitor concept would grow a count
// nothing ever retires except the TTL.
func harnessTracksMonitors(name string) bool {
	h, err := harness.Resolve(name)
	if err != nil {
		return false
	}
	return h.SupportsMonitors()
}

// monitorLaunch decodes a PostToolUse payload into a monitor launch. ok is
// false for anything that is not a `Monitor` call — the overwhelmingly
// common case, since this runs on every tool hook.
//
// deadline is the zero time for a persistent watch, and for one whose
// payload carried no usable timeout: an absent bound must degrade to "no
// deadline" and leave retirement to the process reconcile and the TTL,
// never to a deadline in the past that would retire a live watch instantly.
//
// Both bounds are read from tool_response FIRST (the harness's own echo of
// what it will enforce) and from tool_input as a fallback, because the two
// failure directions are not symmetric and this file must fail closed in
// each:
//
//   - A LOST timeout leaves a short watch bounded only by the TTL. For a
//     websocket watch — which the process reconcile can never retire —
//     that is up to MonitorTTL of badge for work that ended in a minute,
//     so the input's timeout_ms is used when the response yields none.
//   - A LOST `persistent` would invent an absolute deadline for a watch
//     that has none, retiring a live monitor early. So persistent is the
//     OR of both sources: either one claiming it suppresses the deadline.
//
// A missing taskId yields an empty id rather than a rejection, matching
// bgShellLaunch: the launch DID happen, and db.MonitorSet.Add keys such an
// entry anonymously. A command entry still self-heals through the
// reconcile, which matches on the command; it only loses the ability to
// honour a TaskStop naming that id.
func monitorLaunch(input HookCallbackInput, now time.Time) (launch monitorLaunchInfo, ok bool) {
	if input.HookEventName != "PostToolUse" || input.ToolName != monitorToolName {
		return monitorLaunchInfo{}, false
	}
	var in monitorToolInput
	if err := json.Unmarshal(input.ToolInput, &in); err != nil {
		return monitorLaunchInfo{}, false
	}
	// A call carrying neither a command nor a socket is not a watch this
	// ledger can describe or ever retire on evidence, so it is not counted.
	wsURL := ""
	if in.WS != nil {
		wsURL = in.WS.URL
	}
	if in.Command == "" && wsURL == "" {
		return monitorLaunchInfo{}, false
	}

	var resp monitorToolResponse
	// A tool_response that is absent, or not an object at all, is not a
	// reason to drop the launch — only to lose the id and the deadline.
	_ = json.Unmarshal(input.ToolResponse, &resp)

	launch = monitorLaunchInfo{
		ID:      resp.TaskID,
		Command: in.Command,
		Label:   in.Description,
		WS:      in.Command == "",
	}
	if launch.Label == "" {
		launch.Label = wsURL
	}
	timeoutMs := resp.TimeoutMs
	if timeoutMs <= 0 {
		timeoutMs = in.TimeoutMs
	}
	persistent := resp.Persistent || in.Persistent
	if !persistent && timeoutMs > 0 {
		launch.Deadline = now.Add(time.Duration(timeoutMs) * time.Millisecond)
	}
	return launch, true
}

// monitorLaunchInfo is one decoded `Monitor` launch, in the terms
// db.MonitorSet.Add takes.
type monitorLaunchInfo struct {
	ID      string
	Command string
	Label   string
	WS      bool
	// Deadline is when the harness will end this watch on its own. Zero
	// means unbounded — a persistent watch, or a payload that carried no
	// usable timeout.
	Deadline time.Time
}
