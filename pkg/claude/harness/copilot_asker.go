package harness

// copilotAsker builds the `copilot` argv for a one-shot `tclaude ask` turn
// (TCL-994). Like the other askers it returns an ARGV rather than a shell
// string: `tclaude ask` execs it directly, so the untrusted question — which
// carries any piped stdin payload — stays ONE element of the slice and is never
// shell-quoted or split into stray flags.
//
// Copilot forks the same two ways the other harnesses do, and both halves use
// flags the pinned-1.0.77 fixture lab measured rather than flags the docs
// merely list:
//
//   - CAPTURE (spec.Print) is the headless `-p PROMPT` form, which runs the
//     turn and exits. Its stdout is the ANSWER ALONE; the run summary
//     (changes/duration/tokens and a `Resume  copilot --resume=<id>` line) goes
//     to stderr, which is what NoisyCaptureStderr below reports.
//   - INTERACTIVE is the `-i PROMPT` form the spawner already drives in a tmux
//     pane: the full TUI, attached to the caller's terminal, with the question
//     submitted at launch.
//
// The `=`-vs-space spellings are copied from copilotSpawner and are load-bearing
// for the same reason there: `--resume[=VALUE]` takes an OPTIONAL value, so only
// the `=` form binds the id — a space-separated `--resume <id>` leaves the
// option bare, which opens the session picker (or fails when it cannot be
// drawn) and then treats the id as a positional.
//
// NO PERMISSION FLAGS ARE EMITTED, and for capture that is a measured posture
// rather than an omission. Headless, with a terminal absent, Copilot cannot draw
// a permission prompt — and it does not silently proceed either: a tool call
// that would need one comes back "Permission denied and could not request
// permission from user", the model receives that as the tool result and the turn
// finishes normally. Measured on the pinned binary for both an unsafe shell
// command and the built-in `create` file tool (TestCopilotAskCaptureCannotWrite):
// the target file survived, while the same call under `--allow-all-tools`
// executed. So a capture is read-only-ish BY CONSTRUCTION, and emitting the
// promoter — which is the one flag that would change this — is exactly what an
// unattended one-shot must not do. `--no-ask-user` is likewise unnecessary:
// `ask_user` is only advertised when there IS a terminal to ask through.
//
// Nothing here is a claim about the INTERACTIVE arm's containment. A human is at
// the terminal there, drives Copilot's own dialogs, and gets the posture their
// own configuration persists — the same deal the other harnesses' interactive
// ask gives.
//
// AskSpec fields Copilot has no measured flag for are IGNORED rather than
// approximated, exactly as copilotSpawner ignores the spawn fields with no
// documented flag. That covers LaunchPosture (the descriptor leaves
// OneShotReplay nil, so no brokered resume reaches this asker) and Ephemeral
// (Copilot has no measured equivalent of `codex exec --ephemeral`; a turn it
// runs is appended to the conversation). Stream is ignored because this asker
// deliberately does not implement StreamAsker — see NoisyCaptureStderr's
// neighbours below.
type copilotAsker struct{}

var _ Asker = copilotAsker{}

func (copilotAsker) BuildAskArgv(spec AskSpec) []string {
	argv := []string{"copilot"}

	// Exactly one of the two conversation-identity flags, never both: the CLI
	// documents `--resume` as incompatible with `--session-id`, and AskSpec
	// contracts them as mutually exclusive. The id is always a full UUID the ask
	// flow holds — never a prefix or a session name, both of which `--resume`
	// also accepts and either of which could attach the turn to a DIFFERENT
	// conversation.
	switch {
	case spec.ResumeID != "":
		argv = append(argv, "--resume="+spec.ResumeID)
	case spec.SessionID != "":
		argv = append(argv, "--session-id", spec.SessionID)
	}
	if spec.Model != "" {
		argv = append(argv, "--model="+spec.Model)
	}
	if spec.Effort != "" {
		// Copilot's documented effort levels are exactly tclaude's, so the
		// validated token passes through with no per-model remapping.
		argv = append(argv, "--effort="+spec.Effort)
	}

	if spec.Print {
		// Capture hygiene, and both halves are about what lands in `x=$(tclaude
		// ask …)`: `--no-color` keeps escape sequences out of a captured answer
		// even when Copilot believes it is writing to a terminal, and
		// `--log-level none` keeps the CLI's own diagnostics out of the streams.
		// Neither touches the answer text itself.
		argv = append(argv, "--no-color", "--log-level", "none")
		// `-p PROMPT` last so no earlier option can swallow the value. A
		// leading-dash prompt is safe here without an end-of-options guard, and
		// that is measured rather than assumed: a payload beginning with `--`
		// reached the provider verbatim as the user message
		// (TestCopilotAskCapturePassesLeadingDashPrompt). Copilot documents no
		// `--` separator, so tclaude does not invent one.
		if spec.Prompt != "" {
			argv = append(argv, "-p", spec.Prompt)
		}
		return argv
	}

	// Interactive: the TUI, with the question submitted at launch. `-i` is the
	// interactive twin of `-p` and must never be confused with it — `-p` exits
	// after the turn, which would close the pane a human is sitting in. Emitted
	// last for the same reason `-p` is.
	//
	// No `--no-color` / `--log-level` here: a human wants Copilot's ordinary
	// rendering, and the capture-hygiene flags exist only to protect a captured
	// answer.
	if spec.Prompt != "" {
		argv = append(argv, "-i", spec.Prompt)
	}
	return argv
}

// PreMintsConvID is true: Copilot is Claude-shaped here rather than Codex-shaped.
// `--session-id <uuid>` creates the conversation under a caller-chosen id, so
// `tclaude ask` records the (terminal,cwd)→conv mapping before the turn runs and
// every later ask resumes that exact id. Measured on the pinned binary: a
// headless `-p` run under a pinned id creates `session-state/<uuid>`, the
// production ConvStore lists it for the launch cwd, and a following
// `--resume=<uuid> -p` sends the earlier exchange back with the new question
// (roles system, user, assistant, user) while the session keeps its id.
func (copilotAsker) PreMintsConvID() bool { return true }

// NoisyCaptureStderr is true. A headless `-p` run splits its output cleanly:
// stdout carries the answer and nothing else, while stderr carries a human run
// summary — a changed-files count, a duration, a token tally, and a
// `Resume  copilot --resume=<id>` line. That footer is noise in a captured
// answer (and the resume line would put a conversation id in front of anyone
// who redirected stderr into a log), so `tclaude ask` buffers it by default and
// surfaces it on `--verbose` or when the run fails.
func (copilotAsker) NoisyCaptureStderr() bool { return true }
