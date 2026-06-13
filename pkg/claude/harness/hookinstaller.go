package harness

// HookInstaller installs, checks, and repairs the tclaude callback hooks
// in a harness's config target, and surfaces any manual trust step the
// harness requires afterward.
//
// The config target differs per harness — Claude Code writes a `hooks`
// section in ~/.claude/settings.json (JSON); Codex writes its own hooks
// config under ~/.codex and additionally requires the hooks to be
// *trusted* before they run. This contract hides both differences so
// `tclaude setup` can install the callback for whichever harness the user
// is enabling without knowing the storage details.
//
// The hook callback itself is already harness-agnostic: every harness
// invokes the same `tclaude session hook-callback` command, which reads a
// snake_case JSON payload from stdin (Codex's payload matches Claude
// Code's field-for-field). Only the install LOCATION and the trust step
// vary, which is exactly what this contract abstracts.
type HookInstaller interface {
	// Install installs or repairs the tclaude callback hooks in the
	// harness's config target. Idempotent: a second call with the hooks
	// already present is a no-op (or a clean repair of a stale/duplicate
	// entry), never a duplicate.
	Install() error

	// Check reports whether the tclaude hooks are installed and current.
	// missing lists the events still needing the callback; needsRepair is
	// true when a stale (wrong-binary) or duplicate tclaude hook is found.
	Check() (installed bool, missing []string, needsRepair bool)

	// ConfigTarget is the human-readable path of the config file the hooks
	// live in, for setup/diagnostic messages.
	ConfigTarget() string

	// TrustNote returns any manual trust/enable step the user must perform
	// after install for the hooks to actually run (Codex requires
	// non-managed command hooks to be trusted via `/hooks`), or "" when
	// the harness needs none (Claude Code).
	TrustNote() string
}
