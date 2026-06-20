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

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/common"
)

// Params are the `tclaude ask` flags. The question itself is positional args.
type Params struct {
	Print       bool   `long:"print" short:"p" help:"Print the answer and exit (non-interactive). Implied when stdin/stdout is piped."`
	Interactive bool   `long:"interactive" short:"i" help:"Force an interactive session even when output is redirected (needs a terminal on stdin)."`
	New         bool   `long:"new" help:"Start a fresh conversation for this terminal+directory before asking (forgets prior context)."`
	Model       string `long:"model" short:"m" optional:"true" help:"Model for this question (e.g. haiku for snappy answers). Defaults to your configured model."`
}

func Cmd() *cobra.Command {
	c := boa.CmdT[Params]{
		Use:   "ask [question]",
		Short: "Ask a harness an ad-hoc question without taking over your terminal",
		Long: "Ask a coding harness a question from your shell.\n\n" +
			"Runs the harness in the foreground attached to your terminal, holding\n" +
			"focus until the answer is done — then your shell is yours again. No tmux\n" +
			"session to attach to or babysit.\n\n" +
			"Questions from the same terminal+directory continue one conversation\n" +
			"(use --new to start fresh). Pipe input to fold it into the question:\n\n" +
			"  tclaude ask \"what is the largest file here and why?\"\n" +
			"  git diff | tclaude ask \"is this change safe to push?\"\n" +
			"  big=$(tclaude ask -p \"one-word: is main.go too big?\")\n",
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
	TermKey          string
	Cwd              string
	Question         string
	StdinPayload     string // piped stdin, "" when stdin is a terminal
	Model            string
	ForcePrint       bool
	ForceInteractive bool
	New              bool
	StdinIsTerminal  bool
	StdoutIsTerminal bool
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

	stdinIsTTY := term.IsTerminal(int(os.Stdin.Fd()))
	stdoutIsTTY := term.IsTerminal(int(os.Stdout.Fd()))

	var payload string
	if !stdinIsTTY {
		// stdin is redirected/piped — read it as the context payload.
		b, err := io.ReadAll(os.Stdin)
		if err != nil {
			return fmt.Errorf("read piped stdin: %w", err)
		}
		payload = string(b)
	}

	in := askInput{
		TermKey:          TerminalKey(),
		Cwd:              cwd,
		Question:         strings.TrimSpace(strings.Join(args, " ")),
		StdinPayload:     payload,
		Model:            p.Model,
		ForcePrint:       p.Print,
		ForceInteractive: p.Interactive,
		New:              p.New,
		StdinIsTerminal:  stdinIsTTY,
		StdoutIsTerminal: stdoutIsTTY,
	}
	return runAsk(in, askIO{Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr})
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

	// Capture mode is forced by --print, or whenever a stream is redirected:
	// piped stdin has no terminal to read interactive replies from, and a
	// redirected stdout means the caller wants the answer as data. --interactive
	// overrides, but only when stdin is still a real terminal.
	printMode := in.ForcePrint || !in.StdinIsTerminal || !in.StdoutIsTerminal
	if in.ForceInteractive {
		if !in.StdinIsTerminal {
			return errors.New("--interactive needs a terminal on stdin, but stdin is piped")
		}
		printMode = false
	}

	thread, err := db.GetAskThread(in.TermKey, in.Cwd)
	if err != nil {
		return fmt.Errorf("look up ask thread: %w", err)
	}

	// Fresh thread → mint a conv-id and pin it with --session-id so we can
	// record the mapping; existing thread → resume its conv-id.
	fresh := thread == nil
	harnessName := harness.DefaultName
	if !fresh && thread.Harness != "" {
		harnessName = thread.Harness
	}
	h, err := harness.Resolve(harnessName)
	if err != nil {
		return fmt.Errorf("resolve harness %q: %w", harnessName, err)
	}
	if !h.SupportsAsk() {
		return fmt.Errorf("harness %q does not support `tclaude ask` yet", h.Name)
	}

	spec := harness.AskSpec{Print: printMode, Prompt: prompt}
	if in.Model != "" {
		m, err := h.Models.ValidateModel(in.Model)
		if err != nil {
			return fmt.Errorf("invalid --model: %w", err)
		}
		spec.Model = m
	}

	var convID string
	if fresh {
		convID = uuid.NewString()
		spec.SessionID = convID
	} else {
		convID = thread.ConvID
		spec.ResumeID = convID
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

	started, runErr := runner(plan)
	if started {
		// The conversation now exists on disk (claude created it under cwd as
		// soon as it launched, even if the turn was interrupted), so record /
		// refresh the mapping. A persist failure is non-fatal — the answer
		// already happened; we only lose continuity for the next question.
		if err := db.SetAskThread(in.TermKey, in.Cwd, convID, h.Name); err != nil {
			fmt.Fprintf(aio.Stderr, "ask: warning: could not persist conversation mapping: %v\n", err)
		}
	}
	return runErr
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
