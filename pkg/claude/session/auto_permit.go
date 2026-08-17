package session

// auto_permit.go names the permission prompts a harness reserves for a human
// keystroke, and which permission slug consents to each.
//
// The prompt this exists for is Claude Code's EnterWorktree safety check. When
// the target worktree lives outside the directory Claude Code manages itself,
// the confirmation is a hardcoded gate that ignores allow-rules, the auto-mode
// classifier and PreToolUse hook approvals alike — only a keystroke clears it,
// so an agent stalls on an operation its operator is perfectly happy to have
// run unattended, with no configuration anywhere that says so. The permission
// grant is that configuration.
//
// Nothing here decides or presses anything. The PermissionRequest hook names
// the tool being gated; agentd decides whether the operator consented and sends
// the keystroke (see the daemon's auto_permit.go). That split is not stylistic:
// an agent launched into a sandbox is deliberately cut off from both tmux and
// the database, so a decision or a keystroke from inside the pane would fail
// for exactly the agents most likely to want this. The daemon runs on the host,
// owns the pane injection lock, and resolves the caller from its own recorded
// pids.

// PermAutoPermitEnterWorktree lets tclaude answer Claude Code's EnterWorktree
// safety check for an agent. Declared here, next to the tool it answers, rather
// than with agentd's other slugs — agentd's permission registry references this
// constant, and the hook path cannot import agentd.
const PermAutoPermitEnterWorktree = "auto-permit.enter-worktree"

// AutoPermitAccept describes how one gated tool is answered: the slug that
// consents to it, and the tmux keys that accept its dialog.
type AutoPermitAccept struct {
	Slug string
	// Keys are compile-time constants; nothing a hook reports ever reaches
	// send-keys.
	//
	// Enter alone: the dialog opens with "Yes" highlighted, so it is the key
	// the human would press. A digit is deliberately not used — were the
	// dialog somehow not up, a stray Enter submits an empty prompt (harmless)
	// while a stray digit types a character into the composer.
	Keys []string
}

// autoPermitAccepts is the whole vocabulary of auto-permit: one entry per named
// prompt, no wildcards. Widening it is a code change, reviewed as such.
var autoPermitAccepts = map[string]AutoPermitAccept{
	"EnterWorktree": {Slug: PermAutoPermitEnterWorktree, Keys: []string{"Enter"}},
}

// AutoPermitAcceptForTool returns how a gated tool is answered, if it is one
// auto-permit knows. agentd uses it to map a hook event to the slug it must
// check and the keys it may press.
func AutoPermitAcceptForTool(toolName string) (AutoPermitAccept, bool) {
	accept, ok := autoPermitAccepts[toolName]
	return accept, ok
}

// autoPermitNeedsDaemon reports whether this event is one only agentd can act
// on: a permission prompt auto-permit knows how to answer.
//
// A brokered launch already sends every event to the daemon. This is what makes
// an ORDINARY launch — whose hooks apply in-process — hand this one event over
// the same way, since the decision (a permission lookup) and the action (a
// keystroke) both belong on the host. It selects the existing delivery path; it
// does not add one.
func autoPermitNeedsDaemon(input HookCallbackInput) bool {
	if input.HookEventName != "PermissionRequest" {
		return false
	}
	_, known := autoPermitAccepts[input.ToolName]
	return known
}
