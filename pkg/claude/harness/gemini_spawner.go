package harness

import (
	"strings"

	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
)

// geminiSpawner builds the `gemini` invocation that runs inside the tmux pane.
// Like the other spawners it is pure (spec in → shell string out) so the
// "unset omits the flag" guarantee is unit-testable without tmux, and it
// shell-quotes everything handed to `sh -c`.
//
// Every option below was read from the CLI's own yargs definition
// (packages/cli/src/config/config.ts at GeminiPinnedVersion), not inferred
// from another harness:
//
//   - `--resume <id>` (`-r`) resumes by full session UUID, by 1-based index,
//     or by the literal `latest`. tclaude always passes the full id, so the
//     index and `latest` affordances — which could open a DIFFERENT session —
//     are never reached. Resolution is scoped to the project of the process
//     cwd, which is why the pane's cwd (tmux new-session -c) must be the
//     conversation's own project directory; tclaude already launches it there.
//   - `--session-id <id>` starts a NEW session under the given id and exits
//     with an error if that id already exists. Ids are restricted to
//     [A-Za-z0-9_-]; tclaude only ever passes a UUID.
//   - `--resume`, `--session-id` and `--session-file` are mutually exclusive
//     (a yargs .check), hence the either/or below.
//   - `--model=<m>` (`-m`).
//   - `--prompt-interactive=<prompt>` (`-i`) submits the prompt and stays
//     interactive. `-p` is the headless form that exits after the turn, so it
//     must never appear in a pane.
//
// Working directory is NOT passed: the pane is started with
// `tmux new-session -c <cwd>` and Gemini uses the process cwd, the same way
// every other spawner relies on tmux for it.
type geminiSpawner struct{}

func (geminiSpawner) Binary() string { return "gemini" }

// BuildCommand assembles the Gemini invocation: env exports + the binary, then
// either an exact `--resume <id>` or a fresh launch's `--session-id`, an
// optional `--model`, any pass-through args, and finally the optional
// `--prompt-interactive=<prompt>` first turn.
//
// Fields with no Gemini flag are IGNORED rather than approximated: there is no
// effort flag (the catalog rejects a non-empty effort first), and the
// auto-review, permission-profile and remote-control fields belong
// to contracts this descriptor leaves nil, so the resolvers refuse an explicit
// value before a spec reaches this function. HarnessBuiltinMode is honored as
// environment rather than a flag (see gemini_sandbox.go), and ApprovalPolicy as
// `--approval-mode=<mode>` (see gemini_approval.go).
//
// spec.Name is also ignored, and that one is worth stating: Gemini CLI has no
// launch-time name flag and no title store of its own. `session new` records a
// fresh launch's name in tclaude's conversation index instead, keyed by the
// session id it pins.
func (geminiSpawner) BuildCommand(spec SpawnSpec) string {
	binary := "gemini"
	if spec.ExecutablePath != "" {
		binary = clcommon.ShellQuoteArg(spec.ExecutablePath)
	}
	// The sandbox mode's environment goes LAST before the binary: GEMINI_SANDBOX
	// outranks Gemini's flag and settings.json, and placing it after the
	// forwarded exports and the profile's pre-launch script means neither can
	// override the posture that was recorded. See gemini_sandbox.go.
	cmd := spec.EnvExports + spec.PreLaunchScript + geminiSandboxEnvPrefix(spec.HarnessBuiltinMode) + binary
	if spec.ResumeID != "" {
		cmd += " --resume " + clcommon.ShellQuoteArg(spec.ResumeID)
	} else if spec.SessionID != "" {
		// Quoted defensively even though the spawn boundary validates a UUID.
		cmd += " --session-id " + clcommon.ShellQuoteArg(spec.SessionID)
	}
	if spec.Model != "" {
		// `--model=<m>`, attached for the same yargs reason as the first turn
		// below, and quoted because this string is handed to `sh -c`.
		cmd += " " + clcommon.ShellQuoteArg("--model="+spec.Model)
	}
	if arg := geminiApprovalArg(spec.ApprovalPolicy); arg != "" {
		cmd += " " + arg
	}
	if len(spec.ExtraArgs) > 0 {
		quoted := make([]string, len(spec.ExtraArgs))
		for i, a := range spec.ExtraArgs {
			quoted[i] = clcommon.ShellQuoteArg(a)
		}
		cmd += " " + strings.Join(quoted, " ")
	}
	// The first turn is emitted on a resume too. The TUI submits the initial
	// prompt only once the Gemini client has initialized (AppContainer's
	// initial-prompt effect waits on geminiClient.isInitialized), and a resumed
	// launch hands the loaded conversation to that same client, so the prompt
	// lands in the resumed conversation rather than a fresh one.
	//
	// Spelled `--prompt-interactive=<prompt>` rather than `-i <prompt>`: the
	// option is `nargs: 1`, and yargs will not take a following token that
	// starts with `-` as its value — `gemini -i "- fix the tests"` exits with
	// "Not enough arguments" before the TUI starts. The attached form binds
	// everything after the first `=`. Emitted last, as one quoted argument.
	if spec.InitialPrompt != "" {
		cmd += " " + clcommon.ShellQuoteArg("--prompt-interactive="+spec.InitialPrompt)
	}
	return cmd
}
