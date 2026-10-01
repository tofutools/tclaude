package harness

// geminiAsker builds the `gemini` argv for a one-shot `tclaude ask` turn. Like
// every asker it returns an ARGV, exec'd without a shell, so the question
// (which carries any piped stdin payload) stays one element and is never
// shell-quoted or split.
//
// Both arms use options read from the CLI's yargs definition at
// GeminiPinnedVersion:
//
//   - CAPTURE (spec.Print) is headless `--prompt=<q>`: the turn runs and the
//     CLI exits, writing the answer to stdout in the default `text` output
//     format. Headless mode resolves every `ask_user` policy decision to DENY
//     (docs/reference/policy-engine.md), so under Gemini's default approval
//     mode a capture cannot edit files or run shell commands it would have to
//     ask about — read-only-ish by construction, with no flag needed. tclaude
//     emits no approval option here; an operator whose own settings.json
//     already defaults to a broader mode keeps that posture.
//   - INTERACTIVE is `--prompt-interactive=<q>`: the full TUI on the caller's
//     terminal with the question submitted at launch.
//
// The `=` spelling is load-bearing for both: yargs does not bind a following
// argument that starts with `-` as an option's value, so `-p "--why?"` would
// parse the question as an (unknown) option. `--prompt=<q>` binds whatever
// follows the first `=`, leading dash included.
//
// Headless mode REFUSES to run in an untrusted folder (FatalUntrustedWorkspace
// in userStartupWarnings.ts) and names `--skip-trust` as the way around it.
// tclaude does not pass that option: trusting a folder enables its project
// settings, hooks and broader approval modes, which is the operator's call,
// not a side effect of asking a question. The CLI's own error explains the
// remedy.
//
// Effort is ignored (the catalog refuses a non-empty value before a spec gets
// here), as are LaunchPosture and Ephemeral: Gemini has no ephemeral headless
// turn, and OneShotReplay stays unsupported so a brokered séance cannot run.
type geminiAsker struct{}

var _ Asker = geminiAsker{}

func (geminiAsker) BuildAskArgv(spec AskSpec) []string {
	argv := []string{"gemini"}
	// Exactly one identity option: the CLI rejects --resume with --session-id.
	switch {
	case spec.ResumeID != "":
		argv = append(argv, "--resume", spec.ResumeID)
	case spec.SessionID != "":
		argv = append(argv, "--session-id", spec.SessionID)
	}
	if spec.Model != "" {
		argv = append(argv, "--model", spec.Model)
	}
	if spec.Print {
		if spec.Prompt != "" {
			argv = append(argv, "--prompt="+spec.Prompt)
		}
		return argv
	}
	if spec.Prompt != "" {
		argv = append(argv, "--prompt-interactive="+spec.Prompt)
	}
	return argv
}

// PreMintsConvID is true: `--session-id <uuid>` creates the conversation under
// the caller's id in headless mode too (resolveSessionId runs before the
// interactive/headless split), so the (terminal,cwd)→conv mapping is recorded
// before the turn and later asks `--resume` that exact id.
func (geminiAsker) PreMintsConvID() bool { return true }

// NoisyCaptureStderr is true: in headless mode the CLI's diagnostics (credential
// loading, startup warnings, tool errors) go to stderr while the answer goes to
// stdout, so `tclaude ask` buffers stderr and surfaces it on --verbose or when
// the run fails.
func (geminiAsker) NoisyCaptureStderr() bool { return true }
