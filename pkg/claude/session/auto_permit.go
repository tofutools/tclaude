package session

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/paneinput"
)

// auto_permit.go answers the permission prompts that no allow-rule can reach.
//
// Claude Code's EnterWorktree safety check is the case: when the target
// worktree lives outside the directory Claude Code manages itself, the
// confirmation is a hardcoded gate that ignores allow-rules, the auto-mode
// classifier and PreToolUse hook approvals alike. Only a keystroke clears it,
// so an agent stalls on an operation its operator is perfectly happy to have
// run unattended, with no configuration anywhere that says so.
//
// The permission grant IS that configuration. An operator who grants an agent
// `auto-permit.enter-worktree` has said "answer that one prompt for me"; the
// PermissionRequest hook — which already tells us exactly which tool is being
// gated — then presses the accept key in the pane it is running in, and records
// what it answered.
//
// Deliberately narrow: one slug per named prompt, nothing wildcarded, and no
// slug is default-granted. Blanket acceptance is what
// `--dangerously-skip-permissions` is for.

// PermAutoPermitEnterWorktree lets tclaude answer Claude Code's EnterWorktree
// safety check for this agent. Declared here rather than with the other slugs
// in agentd because the hook (which cannot import agentd) is what consumes it;
// agentd's permission registry references this constant.
const PermAutoPermitEnterWorktree = "auto-permit.enter-worktree"

// autoPermitAccepts maps a gated tool name to the slug that consents to it and
// the keys that accept its dialog. The keys are compile-time constants — no
// hook-supplied text ever reaches send-keys from here.
//
// Enter alone: the dialog opens with "Yes" highlighted, so it is the keystroke
// the human would press. A digit is deliberately not used — if the dialog were
// somehow not up, a stray Enter submits an empty prompt (harmless) while a
// stray digit would type a character into the composer.
var autoPermitAccepts = map[string]struct {
	slug string
	keys []string
}{
	"EnterWorktree": {slug: PermAutoPermitEnterWorktree, keys: []string{"Enter"}},
}

// autoPermitSettleDelay is how long we let the TUI paint the dialog before
// pressing. The hook fires as the prompt is raised, so the keystroke must not
// beat it onto the screen. A var so tests need not sleep.
var autoPermitSettleDelay = 400 * time.Millisecond

// maybeAutoPermit answers the just-raised permission prompt when the operator
// has granted this agent the matching slug. Called from the PermissionRequest
// hook, which names the tool being gated — that name is the whole condition;
// nothing here inspects the pane.
//
// Best-effort in every failure direction: no grant, an unknown tool, no pane,
// or a failed send-keys all leave the prompt exactly as it is, waiting for the
// human.
func maybeAutoPermit(state *SessionState, toolName string) {
	keys, ok := autoPermitAcceptFor(state, toolName)
	if !ok {
		return
	}
	go injectAutoPermitAccept(state.ID, state.ConvID, state.TmuxSession, toolName, keys)
}

// autoPermitAcceptFor decides whether this prompt is answerable, and with which
// keys. Split out from the injection so the decision — which is the whole
// security-relevant part — is testable without a pane.
func autoPermitAcceptFor(state *SessionState, toolName string) ([]string, bool) {
	accept, known := autoPermitAccepts[toolName]
	if !known || state == nil || state.TmuxSession == "" {
		return nil, false
	}
	granted, err := db.HasAgentPermissionRow(state.ConvID, accept.slug)
	if err != nil {
		slog.Warn("auto-permit: grant lookup failed", "error", err,
			"slug", accept.slug, "conv", state.ConvID, "module", "hooks")
		return nil, false
	}
	if !granted {
		return nil, false
	}
	return accept.keys, true
}

// injectAutoPermitAccept presses the accept keys after the settle delay, then
// records the answer. It runs off the hook's critical path: Claude Code waits
// on the hook to return, and the dialog it is about to draw cannot appear until
// it does.
func injectAutoPermitAccept(sessionID, convID, tmuxSession, toolName string, keys []string) {
	time.Sleep(autoPermitSettleDelay)
	if err := paneinput.SendKeys(tmuxSession+":0.0", paneinput.Options{}, keys...); err != nil {
		slog.Warn("auto-permit: accept keystroke failed", "error", err,
			"tool", toolName, "tmux", tmuxSession, "module", "hooks")
		return
	}
	slog.Info("auto-permit: answered a permission prompt on the operator's behalf",
		"tool", toolName, "conv", convID, "module", "hooks")
	recordAutoPermitAnswer(sessionID, convID, tmuxSession, toolName)
}

// recordAutoPermitAnswer writes the answer to the audit trail, so the operator
// can see after the fact what was approved for them — in the dashboard's Audit
// tab, alongside every other action taken against this agent.
func recordAutoPermitAnswer(sessionID, convID, tmuxSession, toolName string) {
	if _, err := db.InsertAuditLog(db.AuditLogEntry{
		At:          time.Now(),
		ActorKind:   db.AuditActorSystem,
		ActorLabel:  "tclaude",
		Verb:        "auto-permit.answer",
		TargetConv:  convID,
		TargetLabel: convID,
		Detail:      "auto-answered " + toolName + " (granted via " + autoPermitAccepts[toolName].slug + ")",
		Status:      http.StatusOK,
		Source:      db.AuditSourceHook,
		SessionID:   sessionID,
		TmuxSession: tmuxSession,
	}); err != nil {
		slog.Warn("auto-permit: failed to record the answered prompt",
			"error", err, "tool", toolName, "conv", convID, "module", "hooks")
	}
}
