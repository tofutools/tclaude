package harness

// codexAsker builds the `codex` argv for a one-shot `tclaude ask` turn
// (JOH-252). Like claudeAsker it returns an argv slice (exec'd directly, no
// shell) so the untrusted question is one element of the slice — never shell-
// quoted and never split into stray flags/words.
//
// Codex's command model forks the shape more than Claude Code's:
//
//   - CAPTURE (spec.Print) uses Codex's non-interactive `codex exec` — the
//     `claude -p` analogue: fresh `codex exec …`, resume `codex exec resume
//     <id> …` (verified against codex-cli 0.141.0; resume is a sub-subcommand,
//     the id its first positional). `codex exec` never escalates to a human
//     (a blocked action just returns to the model), so there is no approval
//     prompt that could deadlock an unattended capture — Codex has no
//     `--ask-for-approval` on exec to set. We additionally pin a READ-ONLY
//     sandbox so a captured answer can read enough to answer ("is this diff
//     safe?", "largest file here?") but can't write/act on the workspace, and
//     pass `--skip-git-repo-check` so ask works from any directory (exec
//     otherwise refuses a non-git cwd).
//   - INTERACTIVE drives the `codex` TUI attached to the caller's terminal —
//     the interactive `claude` analogue: fresh `codex …`, resume `codex resume
//     <id> …`. A human is present and can approve, so it inherits the user's
//     own config defaults (sandbox/approval) rather than the locked-down
//     capture posture.
//
// conv-id: Codex does NOT pre-mint (PreMintsConvID == false). A fresh ask runs
// with no preset id; the ask flow discovers the id Codex created from the
// rollout store afterwards (JOH-205). AskSpec.SessionID is therefore ignored
// here — only ResumeID (an id the flow already knows) shapes the argv.
type codexAsker struct{}

var _ Asker = codexAsker{}

func (codexAsker) BuildAskArgv(spec AskSpec) []string {
	argv := []string{"codex"}

	if spec.Print {
		// Non-interactive capture: `codex exec` (+ `resume <id>` to continue).
		argv = append(argv, "exec")
		if spec.ResumeID != "" {
			// `codex exec resume <SESSION_ID> …` — the id is the first
			// positional; it's a UUID the flow already holds, so it's safe
			// before the `--` guard below.
			argv = append(argv, "resume", spec.ResumeID)
		}
		// Run from any directory, not just a git repo (ask is invoked from
		// anywhere). exec/exec-resume both accept this; the TUI handles a
		// non-git cwd interactively instead.
		argv = append(argv, "--skip-git-repo-check")
		// Read-only sandbox: a captured answer can read to answer but never
		// write/act. `codex exec` takes the `--sandbox` flag; `codex exec
		// resume` does NOT (verified against codex-cli 0.141.0), so on a resume
		// we assert the same policy via the `-c sandbox_mode=…` config override
		// (accepted by both subcommands) instead.
		if spec.ResumeID != "" {
			argv = append(argv, "-c", `sandbox_mode="`+SandboxReadOnly+`"`)
		} else {
			argv = append(argv, "--sandbox", SandboxReadOnly)
		}
		argv = appendCodexAskModelEffort(argv, spec)
		// Untrusted prompt as the trailing positional, behind `--`: a captured
		// payload (e.g. a piped `git diff`) can start with `-`, and exec also
		// reads stdin when the prompt is a bare `-`, so `--` stops Codex
		// parsing the prompt as a flag or subcommand.
		if spec.Prompt != "" {
			argv = append(argv, "--", spec.Prompt)
		}
		return argv
	}

	// Interactive: the `codex` TUI (+ `resume <id>` to continue).
	if spec.ResumeID != "" {
		argv = append(argv, "resume", spec.ResumeID)
	}
	argv = appendCodexAskModelEffort(argv, spec)
	// The question is the trailing positional the TUI submits itself at launch.
	// No `--`: like claudeAsker's interactive path, an end-of-options marker
	// can suppress submit-at-launch, and an interactive prompt is a typed
	// question (rarely leading-dash) that stays a single argv element anyway.
	if spec.Prompt != "" {
		argv = append(argv, spec.Prompt)
	}
	return argv
}

// PreMintsConvID is false: Codex generates its conv-id at the first turn and
// only exposes it afterwards (rollout file / threads store — JOH-205), so a
// fresh ask can't pin it up front. The ask flow discovers the id from the
// ConvStore after the turn and records the (terminal,cwd)→conv mapping then.
func (codexAsker) PreMintsConvID() bool { return false }

// NoisyCaptureStderr is true: `codex exec` prints the clean final message to
// stdout but a verbose human transcript to stderr — the session banner, `hook:
// …` lifecycle lines, and a token-count footer. `tclaude ask` hides that by
// default so a captured answer is just the answer (`--verbose` keeps it; a
// failed run still flushes it so errors aren't swallowed).
func (codexAsker) NoisyCaptureStderr() bool { return true }

// appendCodexAskModelEffort appends `--model` and the reasoning-effort `-c`
// override when set, mirroring codexSpawner: the model passes through; the
// effort maps onto Codex's reasoning-effort scale (codexReasoningEffort).
// Both are validated tokens (the ask flow runs them through ModelCatalog),
// emitted only when non-empty so an unset field leaves Codex on its own
// default. No shell-quoting — argv is exec'd directly, so each is one element;
// the `key="value"` TOML form matches Codex's own `-c model="o3"` convention.
func appendCodexAskModelEffort(argv []string, spec AskSpec) []string {
	if spec.Model != "" {
		argv = append(argv, "--model", spec.Model)
	}
	if spec.Effort != "" {
		argv = append(argv, "-c", `model_reasoning_effort="`+codexReasoningEffort(spec.Effort)+`"`)
	}
	return argv
}
