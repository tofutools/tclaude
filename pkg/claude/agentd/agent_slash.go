package agentd

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/paneinput"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// handleWhoamiRename injects `/rename <title>` into the caller's own CC
// pane. Permission-gated on `self.rename`.
//
// Title is restricted to [A-Za-z0-9_-]+ (min 1, max 64 chars) to prevent
// keystroke-injection. Since the title becomes literal send-keys input,
// anything in it (newlines, slashes, control chars) lands in the input
// box; a permissive title would let a permitted agent execute arbitrary
// slash commands by sneaking a newline + another `/<cmd>` in.
func handleWhoamiRename(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method", "POST only")
		return
	}
	convID, ok := requirePermission(w, r, PermSelfRename)
	if !ok {
		return
	}
	if convID == "" {
		writeError(w, http.StatusBadRequest, "invalid_arg",
			"this endpoint renames the calling agent's own conversation; humans should use Claude Code's /rename directly, or use POST /v1/agent/{conv}/rename to rename another agent")
		return
	}
	runRenameOrchestration(w, r, convID, convID)
}

// handleAgentRename injects `/rename <title>` into ANOTHER agent's CC
// pane. Routed via handleAgentByConv. Auth: requireCrossAgentPermission
// (agent.rename, or groups.members.rename covering every active group
// containing the target).
func handleAgentRename(w http.ResponseWriter, r *http.Request, targetConv string) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method", "POST only")
		return
	}
	caller, ok := requireCrossAgentPermission(w, r, PermAgentRename, targetConv)
	if !ok {
		return
	}
	runRenameOrchestration(w, r, targetConv, caller)
}

// runRenameOrchestration validates the title charset, injects
// `/rename <title>` into the target's pane, and writes the JSON
// response. caller is recorded in the response when distinct from
// target so the audit trail has both sides.
//
// When body.Auto is true, the title is ignored and a senderless inbox message
// asks the agent to pick a title for itself via the agent-rename skill / CLI.
// It uses the same async delivery, hold, retry, and pane-contention path as
// every other post-startup programmatic message.
func runRenameOrchestration(w http.ResponseWriter, r *http.Request, target, caller string) {
	var body struct {
		Title string `json:"title"`
		Auto  bool   `json:"auto"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", err.Error())
		return
	}

	if body.Auto {
		// Auto-rename: defer the title choice to the agent itself through the
		// universal inbox. Spelling out the allowed
		// charset up front saves a back-and-forth when the agent
		// picks something the title validator would reject.
		instruction := "Please rename yourself to give this conversation a clearer title. " +
			"Run: tclaude agent rename \"<your-chosen-title>\". " +
			"Pick a 3-4-word kebab-case slug that captures what you've been working on or what " +
			"your role is — e.g. \"fix-bug-abc-123\", \"working-on-new-ui\", or \"worker-agent-a\". " +
			"Allowed: 1-64 characters from [A-Za-z0-9_-[]{}() ] only; single spaces ok, " +
			"no slashes / quotes / newlines / unicode."
		id, err := queueAgentMessage(&db.AgentMessage{
			FromConv:     "",
			ToConv:       target,
			Subject:      "Choose a conversation title",
			Body:         instruction,
			ToRecipients: []string{target},
		})
		if err != nil {
			writeError(w, http.StatusInternalServerError, "io", "queue auto-rename instruction: "+err.Error())
			return
		}
		resp := map[string]any{
			"conv_id":    target,
			"auto":       true,
			"message_id": id,
			"queued":     true,
			"pending":    queueDepthFor(target, false),
			"note":       "auto-rename instruction queued through the target's inbox; it will pick its own title on its next turn",
		}
		if caller != "" && caller != target {
			resp["caller_conv"] = caller
			stampCallerAgentID(resp, caller)
		}
		writeJSON(w, http.StatusOK, resp)
		return
	}

	body.Title = strings.TrimSpace(body.Title)
	if !isValidRenameTitle(body.Title) {
		writeError(w, http.StatusBadRequest, "invalid_title",
			"REJECTED. Title must be 1-64 characters from [A-Za-z0-9_-[]{}() ]. "+
				"Single ASCII spaces are allowed; consecutive spaces, tabs, newlines, "+
				"slashes, quotes, and unicode are NOT allowed and will not be allowed. "+
				"This is a hard security gate against keystroke injection (the title becomes "+
				"literal tmux send-keys input) — it is not a style preference, not configurable, "+
				"and not bypassable. Do not retry with a similar title; pick one that uses only "+
				"the allowed characters.")
		return
	}
	codexSelected := false
	if harnessForConv(target).Name == harness.CodexName {
		var codexSelectionErr error
		codexSelected, codexSelectionErr = codexAppServerSelected(target)
		if codexSelectionErr != nil {
			writeError(w, http.StatusServiceUnavailable, "control_unavailable",
				"could not determine the target Codex control drive")
			return
		}
	}
	if codexSelected {
		if err := renameCodexAppServerThread(target, body.Title); err != nil {
			writeCodexControlError(w, err, "rename")
			return
		}
		cacheDeliveredTitle(target, body.Title, harnessForConv(target).Name)
	} else if !deliverRename(target, body.Title) {
		if renameInputBlocked(target) {
			writeError(w, http.StatusConflict, "busy", paneinput.ErrAwaitingHumanInput.Error())
			return
		}
		writeError(w, http.StatusServiceUnavailable, "no_tmux",
			"could not deliver rename to conv "+short8(target)+" (no live tmux session to inject into, or the harness's title store rejected it)")
		return
	}
	resp := map[string]any{
		"conv_id": target,
		"title":   body.Title,
		"note":    "rename delivered; the harness will surface the new title on its next turn",
	}
	if caller != "" && caller != target {
		resp["caller_conv"] = caller
		stampCallerAgentID(resp, caller)
	}
	writeJSON(w, http.StatusOK, resp)
}

// compactToken is the lifecycle-slash selector for compaction: the
// target harness's compact command (CC's `/compact`), or "" when the
// harness has no scriptable compaction. nil-safe so a bare descriptor
// can't panic the handler.
func compactToken(h *harness.Harness) string {
	if h == nil || h.Life == nil {
		return ""
	}
	return h.Life.CompactCommand()
}

// handleWhoamiCompact injects the caller harness's compact command into
// the caller's own pane. Optional follow-up text is queued as a fresh
// prompt right after. Permission-gated on `self.compact`.
func handleWhoamiCompact(w http.ResponseWriter, r *http.Request) {
	handleSelfSlash(w, r, PermSelfCompact, compactToken, "compact")
}

// handleAgentCompact injects the target harness's compact command into
// ANOTHER agent's pane. Routed via handleAgentByConv (the dispatcher
// resolves targetConv from the URL). Auth: agent.compact, or
// groups.members.compact covering every active group containing the
// target. Same body shape as the self variant.
func handleAgentCompact(w http.ResponseWriter, r *http.Request, targetConv string) {
	handleAgentSlash(w, r, PermAgentCompact, targetConv, compactToken, "compact")
}

func handleWhoamiInterrupt(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method", "POST only")
		return
	}
	convID, ok := requirePermission(w, r, PermSelfInterrupt)
	if !ok {
		return
	}
	if convID == "" {
		writeError(w, http.StatusBadRequest, "invalid_arg",
			"this endpoint operates on the calling agent; humans should target POST /v1/agent/{conv}/interrupt")
		return
	}
	runCodexInterrupt(w, convID, convID)
}

func handleAgentInterrupt(w http.ResponseWriter, r *http.Request, targetConv string) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method", "POST only")
		return
	}
	caller, ok := requireCrossAgentPermission(w, r, PermAgentInterrupt, targetConv)
	if !ok {
		return
	}
	runCodexInterrupt(w, targetConv, caller)
}

func runCodexInterrupt(w http.ResponseWriter, target, caller string) {
	if harnessForConv(target).Name != harness.CodexName {
		writeError(w, http.StatusBadRequest, "unsupported",
			"turn interruption is available only for Codex conversations on the app-server drive")
		return
	}
	codexSelected, codexSelectionErr := codexAppServerSelected(target)
	if codexSelectionErr != nil {
		writeError(w, http.StatusServiceUnavailable, "control_unavailable",
			"could not determine the target Codex control drive")
		return
	}
	if !codexSelected {
		writeError(w, http.StatusBadRequest, "unsupported",
			"turn interruption is available only for Codex conversations on the app-server drive")
		return
	}
	if err := interruptCodexAppServerThread(target); err != nil {
		writeCodexControlError(w, err, "interrupt")
		return
	}
	resp := map[string]any{
		"conv_id": target, "action": "interrupt",
		"note": "the verified active Codex turn was interrupted through app-server",
	}
	if caller != "" && caller != target {
		resp["caller_conv"] = caller
		stampCallerAgentID(resp, caller)
	}
	writeJSON(w, http.StatusOK, resp)
}

func writeCodexControlError(w http.ResponseWriter, err error, action string) {
	failure := codexControlFailure(err)
	status := http.StatusServiceUnavailable
	code := string(failure)
	switch failure {
	case slashFailureBusy:
		status = http.StatusConflict
	case slashFailureUnsupported:
		status = http.StatusBadRequest
	case slashFailureStarting, slashFailureDisconnected:
		status = http.StatusServiceUnavailable
	case slashFailureControl:
		code = "failed"
	}
	writeError(w, status, code, "Codex app-server "+action+" failed: "+err.Error())
}

// slashToken selects a harness's lifecycle slash command (e.g. its
// compact command) for a given target harness, returning "" when the
// harness does not support that action.
type slashToken func(*harness.Harness) string

// handleSelfSlash factors out self-targeted lifecycle-slash handlers like
// /compact. The command itself is sourced from the target harness via
// token (resolved once the target conv — and thus its harness — is known).
func handleSelfSlash(w http.ResponseWriter, r *http.Request, perm string, token slashToken, label string) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method", "POST only")
		return
	}
	convID, ok := requirePermission(w, r, perm)
	if !ok {
		return
	}
	if convID == "" {
		// No calling conv to resolve a harness from; name the default
		// harness's command in the guidance message.
		slash := token(harness.Default())
		writeError(w, http.StatusBadRequest, "invalid_arg",
			"this endpoint operates on the calling agent's own conversation; humans should use the harness's "+slash+" directly, or use POST /v1/agent/{conv}/"+label+" to act on another agent")
		return
	}
	runSlashOrchestration(w, r, convID, convID, token, label)
}

// handleAgentSlash is the cross-agent counterpart to handleSelfSlash.
// Auth via requireCrossAgentPermission (slug OR owner-of-group).
func handleAgentSlash(w http.ResponseWriter, r *http.Request, perm, targetConv string, token slashToken, label string) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method", "POST only")
		return
	}
	caller, ok := requireCrossAgentPermission(w, r, perm, targetConv)
	if !ok {
		return
	}
	runSlashOrchestration(w, r, targetConv, caller, token, label)
}

// slashReason renders the audit "reason" recorded when a lifecycle slash
// (e.g. /compact) is injected into a pane, capturing WHO drove it so the
// output.log line answers "where did that /compact come from?":
//   - caller == ""      → a human via the dashboard / an owner over the
//     socket (cross-agent handlers return "" for the human peer class).
//   - caller == target  → an agent acting on its own conversation.
//   - otherwise         → a cross-agent call; the caller conv is named.
func slashReason(label, caller, target string) string {
	switch caller {
	case "":
		return label + " (human/dashboard)"
	case target:
		return "self-" + label
	default:
		return label + " (caller=" + short8(caller) + ")"
	}
}

// runSlashOrchestration validates the optional follow_up body, injects
// the slash command into the target's pane, and writes the JSON
// response. caller is recorded in the response for cross-agent calls
// so the audit trail has both sides; for self the value is the same as
// target.
func runSlashOrchestration(w http.ResponseWriter, r *http.Request, target, caller string, token slashToken, label string) {
	var body struct {
		FollowUp string `json:"follow_up"`
	}
	if r.ContentLength != 0 {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_arg", err.Error())
			return
		}
	}
	body.FollowUp = strings.TrimSpace(body.FollowUp)
	if body.FollowUp != "" && !isValidFollowUp(body.FollowUp) {
		writeError(w, http.StatusBadRequest, "invalid_follow_up",
			"REJECTED. Follow-up must be 1-4096 printable characters; tabs, newlines, "+
				"and other control characters are not allowed (each newline would be "+
				"treated as a submit by tmux send-keys, splitting the prompt). Strip "+
				"control chars and resubmit.")
		return
	}
	// Source the slash command from the target's harness so a pane is
	// never typed a command it can't parse. "" = the harness has no such
	// command (e.g. a harness without scriptable compaction).
	h := harnessForConv(target)
	slash := token(h)
	if slash == "" {
		writeError(w, http.StatusBadRequest, "unsupported",
			"harness "+h.Name+" does not support "+label)
		return
	}
	if h.Name == harness.OpenCodeName && body.FollowUp != "" {
		// tui.command.execute only acknowledges event publication. OpenCode
		// 1.18.4 starts session.compact asynchronously in the attach client,
		// so prompt_async here could win the race and start a new turn before
		// compaction. Reject the pair before dispatch; callers can compact
		// without a follow-up, wait for idle, then send the next turn.
		writeError(w, http.StatusConflict, "follow_up_unordered",
			"OpenCode cannot safely order a compact follow-up; compact without a follow-up, wait for idle, then send the next turn")
		return
	}
	// BOTH halves come back FROM the dispatch — the transport that carried it
	// and, on failure, the step that said no. Neither is re-derived afterwards
	// from a condition that could have changed, or that the dispatch never
	// reached. TCL-1053 shipped the other version.
	reason := slashReason(label, caller, target)
	if a := peerActionFromRequest(r); a != nil {
		// A peer's action names the target as its caller: say who drove it.
		reason = label + " (" + peerRequesterLabel(a.peer) + ")"
	}
	transport, failure := dispatchSlashCommand(
		target, slash, body.FollowUp, reason)
	if transport == slashTransportNone {
		writeError(w, http.StatusServiceUnavailable, string(failure),
			slashFailureDetail(failure, target, slash))
		return
	}
	resp := map[string]any{
		"conv_id": target,
		"action":  label,
		"note":    slashNote(transport, slash, body.FollowUp != ""),
	}
	if caller != "" && caller != target {
		resp["caller_conv"] = caller
		stampCallerAgentID(resp, caller)
	}
	if body.FollowUp != "" {
		resp["follow_up"] = body.FollowUp
	}
	writeJSON(w, http.StatusOK, resp)
}

// slashFailureDetail renders the operator-facing sentence for a dispatch that
// delivered nothing.
//
// Phrased from the step that failed, not from the harness: "no pane was found"
// and "a managed channel refused" are different remedies, and the second is
// deliberately worded so an operator does not go looking for the command in the
// pane — for an API-driven agent it was never typed there, by design.
func slashFailureDetail(failure slashFailure, target, slash string) string {
	switch failure {
	case slashFailureStarting:
		return "target conv " + short8(target) + " selected the Codex app-server drive, but its control channel is still starting; retry shortly"
	case slashFailureBusy:
		return "target conv " + short8(target) + " is busy or awaiting human interaction; retry once the target is idle"
	case slashFailureUnsupported:
		return "target conv " + short8(target) + " cannot map " + slash + " to a supported typed control operation"
	case slashFailureDisconnected:
		return "target conv " + short8(target) + " selected the Codex app-server drive, but its verified control channel is disconnected; relaunch or resume it"
	case slashFailureSettling:
		return "target conv " + short8(target) + " is still running " + slash +
			"; it finishes on its own, and retrying only checks on it"
	case slashFailureControl:
		return "target conv " + short8(target) + " could not dispatch " + slash +
			" through its managed API channel; it is deliberately not typed into the " +
			"pane instead. Retry when the session is idle and its channel is up"
	}
	return "target conv " + short8(target) + " has no live tmux session to inject " +
		slash + " into"
}

// slashNote describes what actually happened, per transport. Each arm is
// written from that channel's real ordering guarantee, because the three
// differ and the difference is the whole reason a caller reads this field:
// send-keys cannot order a follow-up behind the command, OpenCode refuses the
// pair outright, and Copilot's compaction RPC resolves only when compaction is
// done, so its follow-up is ordered by construction.
func slashNote(transport slashTransport, slash string, hasFollowUp bool) string {
	switch transport {
	case slashTransportOpenCodeAPI:
		return slash + " dispatched via the managed OpenCode TUI API"
	case slashTransportCopilotAPI:
		if hasFollowUp {
			return slash + " requested over the Copilot API; the follow-up is submitted once " +
				slash + " resolves, so the two are ordered"
		}
		return slash + " requested over the Copilot API; the outcome lands in the daemon log"
	case slashTransportCodexAPI:
		if hasFollowUp {
			return slash + " requested through Codex app-server; the follow-up is submitted as a typed turn after compaction settles"
		}
		return slash + " requested through Codex app-server and observed until compaction settled"
	case slashTransportNone:
		// Unreachable from the handler, which returns on a none transport before
		// asking for a note. Spelled out anyway because a function whose ONLY
		// job is to stop transport strings from being wrong must not have an arm
		// that renders "submitted via tmux send-keys" for a delivery that did
		// not happen.
		return "nothing was delivered"
	default:
		if hasFollowUp {
			return slash + " + follow-up submitted via tmux send-keys; the follow-up bytes queue in the pty until CC resumes reading after " + slash + " settles"
		}
		return slash + " submitted via tmux send-keys; CC will process it on its next turn"
	}
}
