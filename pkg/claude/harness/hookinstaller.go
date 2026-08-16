package harness

import (
	"path/filepath"
	"strings"
)

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

// isTclaudeHookCommand reports whether a hook command belongs to tclaude —
// any command whose first shell word has the basename "tclaude". The basename
// match is deliberate: it lets a stale absolute-path tclaude hook from an
// earlier install be recognised and repaired. The trade-off is that ANY binary
// named "tclaude" is treated as ours; a user hook pointing at an unrelated
// tool that happens to share the name would be replaced on install
// (vanishingly unlikely, and the assumption every tclaude installer makes).
func isTclaudeHookCommand(command string) bool {
	first := firstShellCommandWord(command)
	if first == "" {
		return false
	}
	return filepath.Base(first) == "tclaude"
}

// firstShellCommandWord decodes the quoting forms emitted by ShellQuoteArg so
// an absolute tclaude path containing spaces/apostrophes is still recognized
// and repaired on upgrade. It intentionally parses only the first shell word.
func firstShellCommandWord(command string) string {
	command = strings.TrimSpace(command)
	var out strings.Builder
	var quote byte
	for i := 0; i < len(command); i++ {
		c := command[i]
		if quote == 0 {
			switch c {
			case ' ', '\t', '\r', '\n':
				return out.String()
			case '\'', '"':
				quote = c
			case '\\':
				if i+1 < len(command) {
					i++
					out.WriteByte(command[i])
				}
			default:
				out.WriteByte(c)
			}
			continue
		}
		if c == quote {
			quote = 0
			continue
		}
		if quote == '"' && c == '\\' && i+1 < len(command) {
			i++
			out.WriteByte(command[i])
			continue
		}
		out.WriteByte(c)
	}
	return out.String()
}

// TrustedHookInstaller is the optional extension for harnesses whose command
// hooks have a separate executable-trust store. Setup invokes it only when the
// operator explicitly selects that harness; merely finding another harness on
// PATH is enough to install its declarations, but not to grant execution trust.
type TrustedHookInstaller interface {
	HookInstaller

	// AutoTrustSupported reports whether automatic trust can be attempted in
	// this environment. The operation itself must obtain harness-authoritative
	// identity and fail closed when that capability is unavailable.
	AutoTrustSupported() (bool, string)
	// InstallTrusted preflights both files, installs the declarations, then asks
	// the harness for their authoritative identity and persists only that trust.
	InstallTrusted() error
	// TrustInstalled trusts the already-installed current declarations.
	TrustInstalled() error
	// Trusted reports whether the installed declarations match current trust.
	Trusted() bool
}
