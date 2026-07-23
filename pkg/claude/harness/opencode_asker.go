package harness

// openCodeAsker builds the `opencode` argv for a one-shot `tclaude ask`
// turn. OpenCode 1.18.4 has the same command fork as Codex:
//
//   - CAPTURE (spec.Print) uses non-interactive `opencode run`. The built-in
//     Plan agent is the safest available CLI posture: it denies edit tools
//     while retaining enough read access to answer questions. `run`
//     auto-rejects permission requests unless `--auto` is present, so capture
//     deliberately omits that dangerous flag. This is best effort, not a hard
//     read-only sandbox: OpenCode exposes no such run flag, Plan leaves bash
//     allowed by default, and user config can override agent permissions.
//   - INTERACTIVE runs the full `opencode` TUI with `--prompt`. A human is
//     present, so it inherits the user's agent and permission defaults instead
//     of forcing Plan. The top-level TUI has no `--variant`, so a validated
//     effort is emitted only by the capture path.
//
// OpenCode session ids are server-minted `ses_…` values. A fresh ask therefore
// ignores AskSpec.SessionID and lets the ask flow discover the new conversation
// through ConvStore after the turn; only a known ResumeID becomes `--session`.
type openCodeAsker struct{}

var _ Asker = openCodeAsker{}

func (openCodeAsker) BuildAskArgv(spec AskSpec) []string {
	if spec.Print {
		argv := []string{"opencode", "run", "--agent", "plan"}
		if spec.ResumeID != "" {
			argv = append(argv, "--session", spec.ResumeID)
		}
		if spec.Model != "" {
			argv = append(argv, "--model", spec.Model)
		}
		if spec.Effort != "" {
			argv = append(argv, "--variant", spec.Effort)
		}
		// yargs is configured with populate--, and run appends args["--"] to
		// its message array. The guard keeps an untrusted leading-dash prompt
		// out of flag parsing while preserving it as one argv element.
		if spec.Prompt != "" {
			argv = append(argv, "--", spec.Prompt)
		}
		return argv
	}

	argv := []string{"opencode"}
	if spec.ResumeID != "" {
		argv = append(argv, "--session", spec.ResumeID)
	}
	if spec.Model != "" {
		argv = append(argv, "--model", spec.Model)
	}
	// TuiThreadCommand passes --prompt straight into the initial message; it
	// is an option value, never reparsed as flags, so a leading dash is safe.
	if spec.Prompt != "" {
		argv = append(argv, "--prompt", spec.Prompt)
	}
	return argv
}

// PreMintsConvID is false because OpenCode creates `ses_…` ids server-side.
// AskSpec.SessionID is UUID-shaped and intentionally ignored; the generic ask
// flow discovers a fresh OpenCode conversation through ConvStore afterwards.
func (openCodeAsker) PreMintsConvID() bool { return false }

// NoisyCaptureStderr is false because OpenCode's default formatted run output
// is stream-dependent. With redirected stdout, the clean completed answer goes
// to stdout while UI headers/tool chatter go to stderr, so command substitution
// stays clean. With TTY stdout, however, OpenCode renders the answer itself
// through the same stderr UI channel; asking the generic runner to hide
// "noisy" stderr would therefore hide the answer in normal terminal use too.
func (openCodeAsker) NoisyCaptureStderr() bool { return false }
