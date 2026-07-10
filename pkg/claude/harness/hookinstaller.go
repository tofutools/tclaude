package harness

// HookInstaller installs, checks, and repairs the tclaude callback hooks
// in a harness's config target, and surfaces any remaining manual enable step.
//
// The config target differs per harness — Claude Code writes a `hooks`
// section in ~/.claude/settings.json (JSON); Codex writes its own hooks
// config under ~/.codex and can seed narrowly-scoped trust records when Codex
// is explicitly selected. This contract hides both differences so
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
	// true when stale, duplicate, or unusable harness-specific state is found.
	Check() (installed bool, missing []string, needsRepair bool)

	// ConfigTarget is the human-readable path of the config file the hooks
	// live in, for setup/diagnostic messages.
	ConfigTarget() string

	// TrustNote returns any manual enable step the user must perform after
	// install for the hooks to run, or "" when setup completed everything.
	TrustNote() string
}

// TrustedHookInstaller is the optional extension for harnesses whose command
// hooks have a separate executable-trust store. Setup invokes it only when the
// operator explicitly selects that harness; merely finding another harness on
// PATH is enough to install its declarations, but not to grant execution trust.
type TrustedHookInstaller interface {
	HookInstaller

	// AutoTrustSupported verifies that this tclaude build knows the selected
	// harness version's private trust-key/hash contract.
	AutoTrustSupported() (bool, string)
	// InstallTrusted preflights both files, persists trust first, then installs
	// the matching hook declarations. A stale trust record without a matching
	// declaration is inert; the reverse order can create a startup review gate.
	InstallTrusted() error
	// TrustInstalled trusts the already-installed current declarations.
	TrustInstalled() error
	// Trusted reports whether the installed declarations match current trust.
	Trusted() bool
}
