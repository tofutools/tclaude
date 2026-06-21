// Package ask implements `tclaude ask` — put an ad-hoc question to a coding
// harness from anywhere, without taking over your terminal with a tmux
// session (project tclaude-ask, JOH-250).
//
// It runs the harness as a FOREGROUND child of the caller's shell, holding
// focus until the turn is done:
//
//   - interactive by default (stdout is a tty): the agent streams its answer
//     and can ask you back / do work, then exits and your shell is yours again;
//   - captured automatically when piped (`git diff | ai "safe?"`, `x=$(ai …)`):
//     adds `-p`, folds the piped payload into the question, prints clean text.
//
// The conversation is persisted and resumed per (terminal, cwd), so repeated
// questions from the same terminal+directory continue one thread. `--new`
// starts a fresh one.
//
// This is the MVP "model A→client-foreground" cut from the project's decision
// spike (JOH-249): the client runs the harness directly; agentd is not in the
// path (the thread map is a plain DB table). A warm standby pool and non-
// terminal clients are later phases.
package ask

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/google/uuid"
	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/convops"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/common"
)

// Params are the `tclaude ask` flags. The question itself is positional args.
type Params struct {
	Print       bool   `long:"print" short:"p" help:"Print the answer and exit. This is the default; pass it explicitly in scripts, or as the opposite of -i."`
	Interactive bool   `long:"interactive" short:"i" help:"Open the full interactive session (TUI) instead of printing — when you want a back-and-forth or to let the agent ask you questions. Needs a real terminal."`
	New         bool   `long:"new" help:"Start a fresh conversation for this terminal+directory before asking (forgets prior context)."`
	Model       string `long:"model" short:"m" optional:"true" help:"Model for this question (e.g. haiku for snappy answers). Overrides the configured ask default; unset uses it (sonnet by default)."`
	Effort      string `long:"effort" short:"e" optional:"true" help:"Reasoning effort for this question (low, medium, high, xhigh, max). Overrides the configured ask default; unset uses it (medium by default)."`
	Where       bool   `long:"where" help:"Print the resolved ask bucket — terminal key + detection source, cwd, and current conversation id — then exit without asking. Handy for checking terminal detection across emulators."`
	Verbose     bool   `long:"verbose" short:"v" help:"Show the harness's full capture-mode transcript on stderr (the session banner, hook lifecycle, token counts). Off by default so a printed answer is just the answer; a failed run shows it regardless. No effect on -i."`
	NoSmoothing bool   `long:"no-smoothing" help:"Print the streamed answer chunk-by-chunk as it arrives, instead of pacing it into a smooth character-by-character typewriter. Only affects a live answer on a terminal (piped/captured output is never smoothed). Also settable via TCLAUDE_ASK_SMOOTH=0."`
}

func Cmd() *cobra.Command {
	c := boa.CmdT[Params]{
		Use:   "ask [question]",
		Short: "Ask a harness an ad-hoc question without taking over your terminal",
		Long: "Ask a coding harness a question from your shell.\n\n" +
			"By default it prints the answer and returns — you keep your shell, no tmux\n" +
			"session to attach to or babysit. Use -i when you want the full interactive\n" +
			"session instead (a back-and-forth, or to let the agent ask you questions).\n\n" +
			"Questions from the same terminal+directory continue one conversation\n" +
			"(use --new to start fresh). Pipe input to fold it into the question:\n\n" +
			"  tclaude ask \"what is the largest file here and why?\"\n" +
			"  git diff | tclaude ask \"is this change safe to push?\"\n" +
			"  big=$(tclaude ask \"one-word: is main.go too big?\")\n" +
			"  tclaude ask -i \"refactor utils.go — ask me if anything's unclear\"\n",
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(params *Params, cmd *cobra.Command, args []string) {
			if err := runFromCLI(params, args); err != nil {
				var ee *exec.ExitError
				if errors.As(err, &ee) {
					// Propagate the harness's own exit code so `$(ask …)` and
					// `ask … && …` behave like running the harness directly.
					os.Exit(ee.ExitCode())
				}
				fmt.Fprintf(os.Stderr, "Error: %v\n", err)
				os.Exit(1)
			}
		},
	}.ToCobra()
	c.Args = cobra.ArbitraryArgs
	return c
}

// askInput is the fully-resolved, side-effect-free description of one ask
// invocation. The CLI layer (runFromCLI) fills it from the environment, os
// args and stdin; tests construct it directly. Keeping runAsk a pure function
// of this struct (plus the swappable runner) is what makes the flow testable
// without a real terminal or a real harness.
type askInput struct {
	TermKey  string
	Cwd      string
	Question string
	// Harness is the resolved harness for a FRESH thread (from the ask config
	// profile, else the default). It is ignored for an existing thread, which
	// keeps its own recorded harness — you can't switch a conversation's
	// harness mid-thread. Empty falls back to harness.DefaultName.
	Harness          string
	StdinPayload     string // piped stdin, "" when stdin is a terminal
	Model            string
	Effort           string
	ForceInteractive bool
	New              bool
	// Verbose keeps the harness's capture-mode stderr transcript visible
	// (otherwise hidden for harnesses that write one — see
	// Asker.NoisyCaptureStderr). No effect in interactive mode.
	Verbose bool
	// NoSmoothing forwards the streamed answer chunk-by-chunk instead of pacing
	// it into a typewriter. Only consulted when streaming to a TTY; see
	// resolveSmooth for how it composes with TCLAUDE_ASK_SMOOTH.
	NoSmoothing      bool
	StdinIsTerminal  bool
	StdoutIsTerminal bool
	// StderrIsTerminal gates the streaming "working…" indicator: it is drawn on
	// stderr, so we only show it when stderr is a real terminal (never into a
	// redirected stderr). Defaults false, so tests get no spinner.
	StderrIsTerminal bool
}

// askIO carries the streams runAsk wires into the harness process. In an
// interactive turn Stdin is the caller's real terminal (so the agent can
// prompt back); in capture mode it is left nil (the question is already in the
// prompt). Stdout/Stderr are always the caller's, so the answer streams live.
type askIO struct {
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
}

func runFromCLI(p *Params, args []string) error {
	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("resolve working directory: %w", err)
	}

	// --where is a read-only debug probe: report the resolved bucket and exit
	// without reading stdin, loading config, or running a harness.
	if p.Where {
		return printWhere(cwd, os.Stdout)
	}

	if p.Print && p.Interactive {
		return errors.New("--print and --interactive are mutually exclusive")
	}

	stdinIsTTY := term.IsTerminal(int(os.Stdin.Fd()))
	stdoutIsTTY := term.IsTerminal(int(os.Stdout.Fd()))
	stderrIsTTY := term.IsTerminal(int(os.Stderr.Fd()))

	var payload string
	if !stdinIsTTY {
		// stdin is redirected/piped — read it as the context payload.
		b, err := io.ReadAll(os.Stdin)
		if err != nil {
			return fmt.Errorf("read piped stdin: %w", err)
		}
		payload = string(b)
	}

	// Resolve the harness + model/effort a FRESH ask runs at, applying the
	// precedence flag > selected ask profile > config.ask > built-in default
	// (JOH-253 / JOH-252). The config file is the same ~/.tclaude/config.json
	// the dashboard edits; a load failure degrades to the built-in default
	// rather than failing the ask. runAsk stays a pure argv builder (empty =
	// omit the flag); the defaulting lives here, in the env/config layer. The
	// harness only applies to a fresh thread — an existing one keeps its
	// recorded harness inside runAsk.
	cfg, err := config.Load()
	if err != nil {
		cfg = config.DefaultConfig()
	}
	harnessName, model, effort := resolveAskTarget(p.Model, p.Effort, cfg)

	in := askInput{
		TermKey:          TerminalKey(),
		Cwd:              cwd,
		Question:         strings.TrimSpace(strings.Join(args, " ")),
		Harness:          harnessName,
		StdinPayload:     payload,
		Model:            model,
		Effort:           effort,
		ForceInteractive: p.Interactive,
		New:              p.New,
		Verbose:          p.Verbose,
		NoSmoothing:      p.NoSmoothing,
		StdinIsTerminal:  stdinIsTTY,
		StdoutIsTerminal: stdoutIsTTY,
		StderrIsTerminal: stderrIsTTY,
	}
	return runAsk(in, askIO{Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr})
}

// resolveSmooth decides whether `tclaude ask` paces ("smooths") the streamed
// answer into a character-by-character typewriter. Precedence: the
// --no-smoothing flag forces it OFF; otherwise TCLAUDE_ASK_SMOOTH set to a
// falsey value (0/false/off/no) turns it off; otherwise it is on (the default).
// Only consulted when streaming to a real terminal — a piped/captured stdout is
// never smoothed regardless.
func resolveSmooth(noSmoothing bool) bool {
	if noSmoothing {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(os.Getenv("TCLAUDE_ASK_SMOOTH"))) {
	case "0", "false", "off", "no":
		return false
	default:
		return true
	}
}

// printWhere implements `tclaude ask --where`: it prints the bucket an ask from
// here would land in — the resolved terminal key and which source identified
// the terminal, the cwd, and the conversation currently mapped to that
// (terminal, cwd) pair (or a note that the next ask starts fresh). It's purely
// observational, so a DB lookup failure is reported inline rather than failing.
func printWhere(cwd string, w io.Writer) error {
	id, source := resolveTerminalID()
	boot := bootID()
	termKey := id + "." + boot

	fmt.Fprintf(w, "term-key:  %s\n", termKey)
	fmt.Fprintf(w, "term-id:   %s  (source: %s)\n", id, source)
	fmt.Fprintf(w, "boot-id:   %s\n", boot)
	fmt.Fprintf(w, "cwd:       %s\n", cwd)

	thread, err := db.GetAskThread(termKey, cwd)
	switch {
	case err != nil:
		fmt.Fprintf(w, "conv-id:   (lookup failed: %v)\n", err)
	case thread == nil:
		fmt.Fprintln(w, "conv-id:   (none yet — the next ask here starts a fresh conversation)")
	default:
		fmt.Fprintf(w, "conv-id:   %s  (harness: %s)\n", thread.ConvID, thread.Harness)
	}
	return nil
}

// resolveAskTarget resolves the harness + model + effort a FRESH `tclaude ask`
// runs at, applying precedence per field: a per-call flag wins, else the
// selected ask profile, else the dedicated config.ask block, else the
// built-in default constants. The harness comes from the selected ask profile
// (JOH-252 fold-in, Option A) — a spawn profile from the groups tab, reused
// here for its harness/model/effort subset — else the default (Claude Code).
//
// A named-but-missing profile self-heals to the no-profile path (a deleted or
// renamed profile must not hard-error every ask). Spawn-profile fields
// irrelevant to a one-shot ask (agent name, role, sandbox, …) are ignored;
// only harness/model/effort are read. The built-in default constants
// (DefaultAskModel/DefaultAskEffort) are Claude-catalog values, so they are
// applied only when the resolved harness is Claude — a non-Claude harness with
// no pinned model/effort leaves them blank, and the asker omits the flags so
// Codex uses its own configured defaults.
//
// The returned values are raw strings still validated against the resolved
// harness's catalog by runAsk. db.GetSpawnProfile is the only I/O; a load
// error degrades to the no-profile path rather than failing the ask.
func resolveAskTarget(flagModel, flagEffort string, cfg *config.Config) (harnessName, model, effort string) {
	harnessName = harness.DefaultName

	if name := cfg.AskProfileName(); name != "" {
		if prof, err := db.GetSpawnProfile(name); err == nil && prof != nil {
			if prof.Harness != "" {
				harnessName = prof.Harness
			}
			model, effort = prof.Model, prof.Effort
		} else {
			// Missing / unreadable profile → fall back to config.ask defaults.
			model, effort = cfg.ResolvedAskProfile()
		}
	} else {
		model, effort = cfg.ResolvedAskProfile()
	}

	// The built-in defaults only make sense for the Claude catalog.
	// ResolvedAskProfile already applied them on the no-profile path; for a
	// Claude profile with blank fields, apply them here too. A non-Claude
	// harness keeps blanks (→ the asker omits the flags).
	if harnessName == harness.DefaultName {
		if model == "" {
			model = config.DefaultAskModel
		}
		if effort == "" {
			effort = config.DefaultAskEffort
		}
	}

	if flagModel != "" {
		model = flagModel
	}
	if flagEffort != "" {
		effort = flagEffort
	}
	return harnessName, model, effort
}

func runAsk(in askInput, aio askIO) error {
	prompt := assemblePrompt(in.Question, in.StdinPayload)

	if in.New {
		if err := db.DeleteAskThread(in.TermKey, in.Cwd); err != nil {
			return fmt.Errorf("reset ask thread: %w", err)
		}
		if prompt == "" {
			fmt.Fprintln(aio.Stderr, "ask: started a fresh conversation for this terminal+directory")
			return nil
		}
	}
	if prompt == "" {
		return errors.New("no question given — pass a question or pipe input (e.g. `git diff | tclaude ask \"safe?\"`)")
	}

	// Print is the default: the common case is "ask a question, get the answer
	// back, keep your shell" (and it sidesteps the interactive workspace-trust
	// dialog in a fresh dir). Interactive — the full TUI, where the agent can
	// ask you back and you drive a session — is opt-in via -i, and only when a
	// real terminal is on both ends (a piped stdin has no keyboard for the TUI;
	// a redirected stdout has nowhere to render it).
	printMode := true
	if in.ForceInteractive {
		if !in.StdinIsTerminal || !in.StdoutIsTerminal {
			return errors.New("--interactive needs a real terminal (stdin/stdout is piped or redirected)")
		}
		printMode = false
	}

	thread, err := db.GetAskThread(in.TermKey, in.Cwd)
	if err != nil {
		return fmt.Errorf("look up ask thread: %w", err)
	}

	// Pick the harness: a FRESH thread uses the config-resolved harness
	// (in.Harness); an EXISTING thread keeps its own recorded harness — you
	// can't switch a conversation's harness mid-thread. Both fall back to the
	// default.
	fresh := thread == nil
	harnessName := harness.DefaultName
	switch {
	case fresh && in.Harness != "":
		harnessName = in.Harness
	case !fresh && thread.Harness != "":
		harnessName = thread.Harness
	}
	h, err := harness.Resolve(harnessName)
	if err != nil {
		return fmt.Errorf("resolve harness %q: %w", harnessName, err)
	}
	if !h.SupportsAsk() {
		return fmt.Errorf("harness %q does not support `tclaude ask` yet", h.Name)
	}

	// Self-heal a stale mapping: if the recorded conversation no longer exists
	// (a fresh turn that died before the harness wrote it, or one the user
	// deleted via `tclaude conv`), start fresh instead of trying to --resume a
	// ghost — otherwise every question would error until the user found --new.
	if !fresh && !convExists(h, thread.ConvID, in.Cwd) {
		fresh = true
	}

	// Stream the answer live only when a human is actually watching it arrive:
	// print mode, stdout is a real terminal, and the harness can emit an
	// incremental event stream (StreamAsker). A piped/redirected stdout reads
	// the answer whole regardless of whether we streamed, so it keeps the
	// simpler, exact buffered path — which also leaves `x=$(tclaude ask …)`
	// capture semantics untouched. Interactive mode already streams natively.
	streamRender := printMode && in.StdoutIsTerminal && h.SupportsAskStream()

	spec := harness.AskSpec{Print: printMode, Stream: streamRender, Prompt: prompt}
	if in.Model != "" {
		m, err := h.Models.ValidateModel(in.Model)
		if err != nil {
			return fmt.Errorf("invalid --model: %w", err)
		}
		spec.Model = m
	}
	if in.Effort != "" {
		e, err := h.Models.ValidateEffort(in.Effort)
		if err != nil {
			return fmt.Errorf("invalid --effort: %w", err)
		}
		spec.Effort = e
	}

	// Decide the conv-id. Three cases:
	//   - resume: the id is already known (thread.ConvID).
	//   - fresh + pre-minting harness (Claude): mint an id and pin it with
	//     --session-id, so the mapping is recorded up front.
	//   - fresh + non-pre-minting harness (Codex): no preset id — Codex makes
	//     its id at the first turn (JOH-205). Snapshot the harness's convs now;
	//     after the run, resolveFresh() returns the id that newly appeared.
	var convID string
	var resolveFresh func() string
	switch {
	case !fresh:
		convID = thread.ConvID
		spec.ResumeID = convID
	case h.PreMintsAskConvID():
		// (Two concurrent asks from the same terminal+cwd would each mint their
		// own and the last write wins, orphaning one empty conv — an accepted
		// MVP corner, not corruption.)
		convID = uuid.NewString()
		spec.SessionID = convID
	default:
		resolveFresh = newFreshConvResolver(h, in.Cwd)
	}

	plan := runPlan{
		Argv:   h.Ask.BuildAskArgv(spec),
		Cwd:    in.Cwd,
		Stdout: aio.Stdout,
		Stderr: aio.Stderr,
	}
	if !printMode {
		plan.Stdin = aio.Stdin
	}

	// In streaming mode the harness's stdout is its raw event stream, not the
	// answer — route it through the harness's StreamFilter, which writes the
	// clean incremental text to the real stdout. The flusher (if any) is called
	// once after the process exits to surface a buffered final answer/error and
	// end the line cleanly. spec.Stream is only ever set for a SupportsAskStream
	// harness, so the type-assert always holds here.
	var streamFlush harness.AskStreamFlusher
	var spinner *streamSpinner
	if spec.Stream {
		if sa, ok := h.Ask.(harness.StreamAsker); ok {
			// A "working…" indicator on stderr while the answer is still on its way
			// — shown only when stderr is itself a terminal (so a redirected stderr
			// never collects escape codes). The filter erases it the instant the
			// first character prints; see streamSpinner.
			var status harness.StreamStatus
			if in.StderrIsTerminal {
				spinner = newStreamSpinner(aio.Stderr)
				status = spinner
			}
			filtered := sa.StreamFilter(aio.Stdout, resolveSmooth(in.NoSmoothing), status)
			plan.Stdout = filtered
			if fl, ok := filtered.(harness.AskStreamFlusher); ok {
				streamFlush = fl
			}
		}
	}
	if spinner != nil {
		spinner.start()
	}

	// In capture mode some harnesses (Codex) write a verbose human transcript to
	// stderr — banner, `hook: …` lines, token counts — separate from the clean
	// answer on stdout. Buffer that so a printed answer is just the answer;
	// --verbose keeps it live, and a failed run flushes the buffer so a real
	// error is never swallowed.
	hideStderr := printMode && !in.Verbose && h.Ask.NoisyCaptureStderr()
	var stderrBuf bytes.Buffer
	if hideStderr {
		plan.Stderr = &stderrBuf
	}

	started, runErr := runner(plan)
	if streamFlush != nil {
		// Surface any trailing answer/error the stream implied and end the line,
		// whether or not the run succeeded. A flush write error is non-fatal —
		// the answer already streamed; don't mask the run's own outcome with it.
		if flushErr := streamFlush.Flush(); flushErr != nil && runErr == nil {
			fmt.Fprintf(aio.Stderr, "ask: warning: could not finish streaming output: %v\n", flushErr)
		}
	}
	if spinner != nil {
		// Tear the indicator down and join its goroutine. The filter already
		// hid it before the last character; this also covers a turn that printed
		// nothing at all.
		spinner.Done()
		spinner.wait()
	}
	if hideStderr && runErr != nil {
		// The run failed — surface the suppressed transcript so the failure
		// isn't silent (an auth error, a bad model, a sandbox denial, …).
		_, _ = io.Copy(aio.Stderr, &stderrBuf)
	}
	if started {
		// For a non-pre-minting fresh ask, discover the id Codex just created.
		// An empty result (no new conv — e.g. the run errored before writing
		// one) simply skips the mapping, so the next ask starts fresh.
		if resolveFresh != nil {
			convID = resolveFresh()
		}
		// Record / refresh the (terminal,cwd)→conv mapping. We persist whenever
		// the process started (incl. a Ctrl-C'd interactive turn, whose conv was
		// already created), and rely on the self-heal check above to fall back to
		// fresh next time if this conv turns out never to have been written. A
		// persist failure is non-fatal — the answer already happened; we only
		// lose continuity for the next question.
		if convID != "" {
			if err := db.SetAskThread(in.TermKey, in.Cwd, convID, h.Name); err != nil {
				fmt.Fprintf(aio.Stderr, "ask: warning: could not persist conversation mapping: %v\n", err)
			}
		}
	}
	return runErr
}

// newFreshConvResolver snapshots a non-pre-minting harness's conversations in
// cwd and returns a closure that, called AFTER a fresh ask runs, returns the
// conv-id that newly appeared (Codex mints its id at the first turn — JOH-205,
// then exposes it via the rollout/threads store). Swapped in tests. The
// before/after diff (rather than "newest conv in cwd") is what distinguishes
// the conv this ask created from one an unrelated turn merely touched;
// concurrent asks in the same cwd are an accepted corner — the newest new id
// wins, mirroring the pre-minting last-write-wins note above.
var newFreshConvResolver = liveFreshConvResolver

func liveFreshConvResolver(h *harness.Harness, cwd string) func() string {
	before, beforeErr := listAskConvs(h, cwd)
	beforeIDs := convIDSet(before)
	return func() string {
		// If the "before" snapshot itself failed, we can't tell which conv is
		// new — every pre-existing one would look new and we'd risk mapping the
		// wrong thread. Skip the mapping instead; the next ask starts fresh,
		// which beats resuming someone else's conversation.
		if beforeErr != nil {
			return ""
		}
		after, err := listAskConvs(h, cwd)
		if err != nil {
			return ""
		}
		var newestID string
		var newestMtime int64
		for _, e := range after {
			if e.SessionID == "" || beforeIDs[e.SessionID] {
				continue
			}
			if newestID == "" || e.FileMtime >= newestMtime {
				newestID, newestMtime = e.SessionID, e.FileMtime
			}
		}
		return newestID
	}
}

// convIDSet is the set of non-empty conv-ids in entries — the "before"
// snapshot the fresh-conv resolver diffs the post-run listing against.
func convIDSet(entries []convops.SessionEntry) map[string]bool {
	set := make(map[string]bool, len(entries))
	for _, e := range entries {
		if e.SessionID != "" {
			set[e.SessionID] = true
		}
	}
	return set
}

func listAskConvs(h *harness.Harness, cwd string) ([]convops.SessionEntry, error) {
	if h.Convs == nil {
		return nil, nil
	}
	return h.Convs.ListConvs(cwd)
}

// convExists reports whether a recorded ask conversation is still on disk, so a
// stale mapping self-heals to a fresh thread instead of resuming a ghost.
// Swapped in tests (the flow tests use a fake harness runner that writes no
// real conversation files). See liveConvExists.
var convExists = liveConvExists

// liveConvExists checks the harness's conversation store via ConvStore.Exists
// (Claude's per-cwd `.jsonl`, Codex's ~/.codex rollouts). A confirmed-absent
// conv self-heals to fresh; a harness with no ConvStore, or a store that
// can't be read (a transient error), is treated as present so a flaky read
// never silently drops a valid thread's continuity.
func liveConvExists(h *harness.Harness, convID, cwd string) bool {
	if h.Convs == nil {
		return true
	}
	ok, err := h.Convs.Exists(convID, cwd)
	if err != nil {
		return true
	}
	return ok
}

// assemblePrompt builds the single prompt string from the typed question and
// any piped stdin. When both are present the payload is appended under a
// labelled fence so the agent can tell question from data; piped-only input is
// itself the question.
func assemblePrompt(question, payload string) string {
	payload = strings.TrimRight(payload, "\n")
	switch {
	case question != "" && payload != "":
		return question + "\n\n--- piped input (stdin) ---\n" + payload
	case question == "" && payload != "":
		return payload
	default:
		return question
	}
}

// runPlan is the concrete process to run for one ask turn. It is the single
// swappable subprocess boundary in this package (the testharness convention:
// production runs the real harness; tests assign a fake `runner`).
type runPlan struct {
	Argv   []string
	Cwd    string
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
}

// runner executes a runPlan. Swapped in tests; see liveRunner for the
// production path. It reports whether the process actually STARTED (so the
// caller knows whether the conversation was created and the mapping should be
// recorded) separately from the run error.
var runner = liveRunner

func liveRunner(p runPlan) (started bool, err error) {
	if len(p.Argv) == 0 {
		return false, errors.New("empty command")
	}
	bin, lookErr := exec.LookPath(p.Argv[0])
	if lookErr != nil {
		return false, fmt.Errorf("%s not found on PATH: %w", p.Argv[0], lookErr)
	}
	cmd := exec.Command(bin, p.Argv[1:]...)
	cmd.Dir = p.Cwd
	cmd.Stdin = p.Stdin
	cmd.Stdout = p.Stdout
	cmd.Stderr = p.Stderr

	runErr := cmd.Run()
	if runErr == nil {
		return true, nil
	}
	var ee *exec.ExitError
	if errors.As(runErr, &ee) {
		// The process ran and exited non-zero (incl. a signal/Ctrl-C). It
		// started, so the conversation exists; surface the exit error for code
		// propagation.
		return true, ee
	}
	// Failed to even start (binary vanished between LookPath and exec, etc.).
	return false, runErr
}
