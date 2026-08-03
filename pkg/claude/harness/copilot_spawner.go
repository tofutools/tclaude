package harness

import (
	"strings"

	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
)

// copilotSpawner builds the `copilot` invocation that runs inside the tmux
// pane. Like the other spawners it stays pure (string in → string out) so the
// "unset omits the flag" guarantee is unit-testable without tmux, and it
// shell-quotes everything handed to `sh -c`.
//
// Every flag below is taken verbatim from Copilot CLI's documented
// command-line options table, INCLUDING its exact `=`-vs-space spelling. That
// precision is load-bearing rather than cosmetic:
//
//   - `-r`, `--resume[=VALUE]` takes an OPTIONAL value, so only the `=` form
//     binds the id. A space-separated `--resume <id>` would leave the option
//     bare — which opens the interactive session picker, or, when the picker
//     cannot be shown, exits with an error — and would then try to parse the
//     id as a positional.
//   - `--session-id ID` is documented with a space.
//   - `--name=NAME`, `--model=MODEL` and `--effort=LEVEL` are documented with
//     `=`.
//
// Working directory is NOT passed via `-C`: tclaude launches the pane with
// `tmux new-session -c <cwd>`, and Copilot uses the pane's cwd — the same way
// the Claude Code and Codex spawners rely on tmux for cwd.
type copilotSpawner struct{}

func (copilotSpawner) Binary() string { return "copilot" }

// BuildCommand assembles the Copilot invocation: env exports + the binary,
// then either an exact `--resume=<id>` or a fresh launch's `--session-id` /
// `--name`, an optional `--model` / `--effort`, any pass-through args, and
// finally the optional `-i <prompt>` first turn.
//
// Fields with no documented Copilot flag (sandbox mode, approval policy,
// auto-review, permission profile, remote control, hook-trust bypass, the
// per-session settings payload) are IGNORED here rather than approximated.
// The descriptor leaves those contracts nil, so the resolvers reject an
// explicit value long before a spec reaches this function.
func (copilotSpawner) BuildCommand(spec SpawnSpec) string {
	binary := "copilot"
	if spec.ExecutablePath != "" {
		binary = clcommon.ShellQuoteArg(spec.ExecutablePath)
	}
	cmd := spec.EnvExports + binary
	if spec.ResumeID != "" {
		// `--resume=<id>` resumes EXACTLY this conversation. tclaude always
		// knows the full id it wants, so it never uses the option's fuzzier
		// affordances (bare `--resume`'s picker, an id prefix, a session name)
		// — those could silently open a different conversation. The docs also
		// forbid combining resume with `--session-id`, `--continue`,
		// `--connect` and the worktree flags, hence the else-branch below.
		cmd += " --resume=" + clcommon.ShellQuoteArg(spec.ResumeID)
	} else {
		// `--session-id <uuid>` pins the conversation id for a FRESH launch, so
		// the daemon can enroll the agent before the pane starts. Copilot
		// creates a new session for an unmatched value only when it is a valid
		// UUID (a name or an id prefix never creates one), which is why the
		// spawn boundary validates the shape. Quoted defensively even though it
		// is a validated UUID.
		if spec.SessionID != "" {
			cmd += " --session-id " + clcommon.ShellQuoteArg(spec.SessionID)
		}
		// `--name=<name>` sets the session's display name at launch — the same
		// name `/rename` writes and `--resume` matches on. Fresh launches only:
		// the documented purpose is "set a name for the NEW session", and its
		// behavior on a resume is unverified, so tclaude renames a resumed
		// conversation through the ordinary in-pane `/rename` path instead.
		// Quoted because the name is free-ish text handed to `sh -c`.
		if spec.Name != "" {
			cmd += " --name=" + clcommon.ShellQuoteArg(spec.Name)
		}
	}
	if spec.Model != "" {
		// `--model=<model>` (`auto` lets Copilot pick). Quoted defensively:
		// the catalog gates the charset, but this string is handed to `sh -c`.
		cmd += " --model=" + clcommon.ShellQuoteArg(spec.Model)
	}
	if spec.Effort != "" {
		// `--effort=<level>`. Copilot's documented levels are exactly tclaude's
		// (low/medium/high/xhigh/max), so the validated token passes straight
		// through with no per-model remapping of the kind Codex needs.
		cmd += " --effort=" + clcommon.ShellQuoteArg(spec.Effort)
	}
	if len(spec.ExtraArgs) > 0 {
		// Appended as plain args, each quoted individually. Copilot documents
		// no `--` pass-through separator, so tclaude does not invent one.
		quoted := make([]string, len(spec.ExtraArgs))
		for i, a := range spec.ExtraArgs {
			quoted[i] = clcommon.ShellQuoteArg(a)
		}
		cmd += " " + strings.Join(quoted, " ")
	}
	// `-i PROMPT` starts an INTERACTIVE session and automatically executes the
	// prompt — the flag a TUI pane wants. `-p/--prompt` is the headless form
	// that exits after completion, so it must never appear here. Emitted last
	// so no other option can swallow the value, and quoted as a single arg so
	// the whole prompt stays one PROMPT rather than splitting into stray
	// flags/words. Allowed on a resume too: the documented `--resume` behavior
	// discusses `-i` as a resume-time mode, and dropping a caller's prompt
	// silently would be worse than not offering it at all.
	if spec.InitialPrompt != "" {
		cmd += " -i " + clcommon.ShellQuoteArg(spec.InitialPrompt)
	}
	return cmd
}
