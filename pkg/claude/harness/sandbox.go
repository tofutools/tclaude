package harness

import (
	"fmt"
	"strings"
)

// SandboxCatalog is the optional capability for a harness that takes a
// launch-time sandbox-mode flag (Codex's `--sandbox`). A harness whose
// sandbox is configured out of band — Claude Code, whose sandbox lives in
// settings.json, not a launch flag — leaves Harness.Sandbox nil, so the
// spawn path performs no sandbox handling for it (SupportsSandbox() is
// false; passing a mode is an error the caller surfaces).
//
// The contract is deliberately small: name the secure default mode, and
// validate/normalize a requested one. The cwd-safety check (a sandboxed
// agent's cwd must not expose $HOME) is a separate, boundary-level concern
// because it needs the resolved cwd — see CodexSandboxCwdConflict.
type SandboxCatalog interface {
	// DefaultMode is the mode a tclaude-spawned agent runs under when the
	// caller didn't choose one. It must be a *sandboxed* mode (never a
	// full-access one): unspecified means "secure by default".
	DefaultMode() string
	// ValidateMode normalizes and validates a requested mode. The empty
	// string is returned unchanged (callers substitute DefaultMode where a
	// default is wanted, via ResolveSandboxMode); any other value is either
	// a recognized mode (returned trimmed) or an error naming the valid set.
	ValidateMode(mode string) (string, error)
}

// ResolveSandboxMode is the single entry point the spawn boundaries (CLI
// `session new`, agentd spawn/resume) use to turn a requested sandbox mode
// into the value to thread into SpawnSpec.SandboxMode:
//
//   - Harness has no launch sandbox flag (Claude Code): an explicit mode is
//     an error (its sandbox is settings.json-driven, not a launch flag); an
//     empty request resolves to "" (omit).
//   - Harness takes one (Codex): an empty request resolves to the secure
//     DefaultMode (workspace-write); any explicit mode is validated.
//
// requested is trimmed first, so surrounding whitespace never leaks into
// the flag.
func ResolveSandboxMode(h *Harness, requested string) (string, error) {
	requested = strings.TrimSpace(requested)
	if !h.SupportsSandbox() {
		if requested != "" {
			return "", fmt.Errorf("harness %q has no launch-time sandbox mode "+
				"(its sandbox is configured out of band, not via --sandbox)", h.Name)
		}
		return "", nil
	}
	if requested == "" {
		requested = h.Sandbox.DefaultMode()
	}
	return h.Sandbox.ValidateMode(requested)
}
