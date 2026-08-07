package harness

import "fmt"

// SupportsCopilotAPI reports whether the harness can be driven over a
// tclaude-owned API channel instead of tmux `send-keys`.
//
// Copilot CLI only, for now, and deliberately not generalised into an
// "API-backed if the harness supports it" capability: Copilot's embedded
// JSON-RPC server (`copilot --ui-server`) runs INSIDE the same process that
// owns the pane's TUI, while OpenCode's managed server is a separate process
// with its own supervision and handshake. Those two shapes have not yet shown
// us a common abstraction worth guessing at, so each harness names its own
// capability until one appears. See TCL-1051.
//
// Gated on the harness NAME rather than a capability func for the same reason
// as SupportsAutoMemory: there is no per-harness lifecycle implementation to
// probe — the mode is a launch posture tclaude records and later acts on.
func (h *Harness) SupportsCopilotAPI() bool {
	return h != nil && h.Name == CopilotName
}

// CanCopilotAPI is the UI-side predicate a spawn/profile control gates on
// (mirrors CanAutoMemory). Identical to SupportsCopilotAPI today; kept separate
// so a future "supported but not steerable here" case has a seam.
func (h *Harness) CanCopilotAPI() bool {
	return h.SupportsCopilotAPI()
}

// ResolveCopilotAPI gates the API-backed-mode opt-in and returns the posture to
// thread into the launch.
//
// nil (nothing pinned it, anywhere in the profile tier stack) resolves to
// FALSE, which is the send-keys path every Copilot agent has always taken. That
// default is the whole point of the flag: the API path is built alongside the
// send-keys one and agents move over per spawn, not all at once.
//
// Asking for it on a harness that has no such mode is an error rather than a
// silent drop, so a mistake surfaces at the spawn boundary instead of leaving
// an operator wondering why their agent is still on send-keys.
//
// One function serves both the daemon spawn path and the direct `session new`
// path.
func ResolveCopilotAPI(h *Harness, requested *bool) (bool, error) {
	if requested == nil {
		return false, nil
	}
	if *requested && !h.CanCopilotAPI() {
		return false, fmt.Errorf("harness %q has no API-backed mode "+
			"(API-backed mode is a %s feature; not available for this harness)",
			harnessName(h), CopilotName)
	}
	return *requested, nil
}
