package agent

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/common"
)

// `tclaude agent seance [question]` — summon the spirit of a dead
// predecessor and ask it what it knew (JOH-25, inspired by `gt seance`
// from Steve Yegge's Gas Town).
//
// When an agent reincarnates, the old conversation is retired but its
// full `.jsonl` transcript survives on disk. A séance resumes that dead
// conversation HEADLESSLY for a single turn — `claude -p --resume
// <predecessor>` run from the predecessor's own launch dir — puts one
// question to it, and hands back JUST the answer. The successor pays for
// the answer (a few hundred tokens) instead of dragging the whole
// transcript back into its live context window, which is the very thing
// reincarnate was trying to shed.
//
// The mechanics are the `tclaude ask` headless-Q&A primitive (JOH-250/
// 252) aimed at a retired conv-id: the same harness-agnostic Asker argv
// builder, the same one-shot capture, just resolved against a dead
// ancestor and run in that ancestor's cwd (resume is cwd-scoped — a
// conv is only resumable from where it was created). Hooks are
// suppressed (TCLAUDE_IGNORE_HOOKS=true) so the ephemeral resume never
// flips the dead agent back to "alive", re-enrolls it, or fires
// notifications: the predecessor stays in its grave; we only consult it.

type seanceParams struct {
	Question string `pos:"true" optional:"true" help:"What to ask the predecessor. Quote multi-word strings. Read from --file instead for long/multi-line questions. Omit only with --print-cmd."`
	File     string `long:"file" short:"f" optional:"true" help:"Read the question from this file ('-' reads stdin) instead of the positional argument. Sidesteps shell quoting. Mutually exclusive with the positional argument."`
	Target   string `long:"target" optional:"true" help:"Consult a SPECIFIC dead conversation (conv-id or 8+-char prefix) instead of your immediate predecessor. Unlike most selectors this does NOT redirect forward to a live successor — a séance addresses a specific generation."`
	Back     int    `long:"back" optional:"true" default:"1" help:"Walk back this many generations of reincarnation before asking (1 = immediate predecessor). Ignored when --target is given."`
	Model    string `long:"model" optional:"true" help:"Model for the séance turn (default: the harness's configured default). The predecessor's own model is not recorded; you pick the medium for the summoning."`
	Effort   string `long:"effort" optional:"true" help:"Reasoning effort for the séance turn (harness-specific; default: harness default)."`
	Timeout  string `long:"timeout" optional:"true" help:"Cap the séance call, e.g. '90s' or '3m'. Default: no timeout."`
	PrintCmd bool   `long:"print-cmd" help:"Resolve the predecessor + command and print them WITHOUT running anything. No LLM call, no cost — use it to verify targeting (and the resume cwd) for free before spending tokens."`
}

func seanceCmd() *cobra.Command {
	return boa.CmdT[seanceParams]{
		Use:   "seance",
		Short: "Consult a dead predecessor: ask your previous incarnation a question and get back just its answer",
		Long: "Summons the spirit of a retired predecessor conversation — the agent you " +
			"reincarnated from — by resuming its session headlessly for ONE turn, putting " +
			"your question to it, and returning only the answer. The predecessor's full " +
			"transcript answers from its own memory; you pay for the answer, not for " +
			"re-loading its whole history into your live context. " +
			"\n\n" +
			"By default the target is your immediate predecessor (the agent whose identity " +
			"you inherited at reincarnate). Use --back N to reach further up the chain, or " +
			"--target <conv-id> to consult a specific dead conversation. " +
			"\n\n" +
			"A séance is a deliberate, billable act: it replays the predecessor's full " +
			"context to answer. Use --print-cmd to see exactly what would run (and from " +
			"which directory) without spending anything. Resume is cwd-scoped, so the " +
			"predecessor's launch directory must still exist; a removed worktree means a " +
			"grave that can no longer be reached.",
		ParamEnrich: common.DefaultParamEnricher(),
		InitFuncCtx: func(ctx *boa.HookContext, p *seanceParams, _ *cobra.Command) error {
			boa.GetParamT(ctx, &p.Target).SetAlternativesFunc(completeConvSelectors)
			return nil
		},
		RunFunc: func(p *seanceParams, _ *cobra.Command, _ []string) {
			os.Exit(runSeance(p, os.Stdin, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

// seanceRun is the single swappable subprocess boundary in this command
// (the testharness convention — production resumes the real harness;
// tests assign a fake that records the plan and returns canned text).
type seancePlan struct {
	Argv    []string
	Cwd     string
	Timeout time.Duration
	Stdin   io.Reader
	Stdout  io.Writer
	Stderr  io.Writer
}

var seanceRun = liveSeanceRun

func liveSeanceRun(p seancePlan) error {
	if len(p.Argv) == 0 {
		return fmt.Errorf("empty command")
	}
	ctx := context.Background()
	if p.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, p.Timeout)
		defer cancel()
	}
	cmd := exec.CommandContext(ctx, p.Argv[0], p.Argv[1:]...)
	cmd.Dir = p.Cwd
	// Suppress tclaude's hooks for the ephemeral resume: the séance must
	// not flip the dead conv back to "alive", re-enroll it, or fire
	// notifications. Mirrors the headless `claude -p` call in task/add.go.
	cmd.Env = append(os.Environ(), "TCLAUDE_IGNORE_HOOKS=true")
	cmd.Stdin = p.Stdin
	cmd.Stdout = p.Stdout
	cmd.Stderr = p.Stderr
	return cmd.Run()
}

func runSeance(p *seanceParams, stdin io.Reader, stdout, stderr io.Writer) int {
	// 1) Resolve the question (positional or --file/stdin). Skippable
	//    only in --print-cmd mode, where we're just inspecting targeting.
	rawQ, rc := resolveBodyInput(p.Question, p.File, "the question argument", stdin, stderr)
	if rc != rcOK {
		return rc
	}
	question := strings.TrimSpace(rawQ)
	if question == "" && !p.PrintCmd {
		fmt.Fprintln(stderr, "Error: a question is required (give it inline or via --file). Use --print-cmd to inspect targeting without asking.")
		return rcInvalidArg
	}

	var timeout time.Duration
	if p.Timeout != "" {
		d, err := time.ParseDuration(p.Timeout)
		if err != nil || d < 0 {
			fmt.Fprintf(stderr, "Error: invalid --timeout %q (use e.g. 90s, 3m)\n", p.Timeout)
			return rcInvalidArg
		}
		timeout = d
	}

	// 2) Resolve which dead conversation to consult.
	target, rc := resolveSeanceTarget(p, stderr)
	if rc != rcOK {
		return rc
	}

	// 3) Recover its launch dir (resume is cwd-scoped) and its harness.
	loc := ResolveLocation(target)
	cwd := loc.StartupDir
	if cwd == "" {
		fmt.Fprintf(stderr, "Error: cannot locate predecessor %s's working directory; its grave is unreachable.\n", short(target))
		return rcNotFound
	}
	row := FreshConvRowResolved(target)
	harnessName := ""
	if row != nil {
		harnessName = row.Harness
	}
	h, err := harness.Resolve(harnessName)
	if err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return rcInvalidArg
	}
	if !h.SupportsAsk() {
		fmt.Fprintf(stderr, "Error: harness %q cannot hold a séance (no headless resume/ask support).\n", h.Name)
		return rcInvalidArg
	}

	// 4) Build the headless one-shot resume argv via the shared Asker.
	spec := harness.AskSpec{
		ResumeID: target,
		Prompt:   question,
		Print:    true, // capture mode: print the answer and exit
		Model:    p.Model,
		Effort:   p.Effort,
	}
	argv := h.Ask.BuildAskArgv(spec)

	if p.PrintCmd {
		fmt.Fprintf(stdout, "predecessor: %s\n", short(target))
		fmt.Fprintf(stdout, "harness:     %s\n", h.Name)
		fmt.Fprintf(stdout, "cwd:         %s\n", cwd)
		fmt.Fprintf(stdout, "command:     %s\n", strings.Join(argv, " "))
		return rcOK
	}

	// 5) Hold the séance. The answer streams straight to our stdout.
	fmt.Fprintf(stderr, "Summoning %s (resuming in %s)...\n", short(target), cwd)
	plan := seancePlan{
		Argv:    argv,
		Cwd:     cwd,
		Timeout: timeout,
		Stdin:   stdin,
		Stdout:  stdout,
		Stderr:  stderr,
	}
	if err := seanceRun(plan); err != nil {
		fmt.Fprintf(stderr, "Error: the séance failed: %v\n", err)
		return rcIOFailure
	}
	return rcOK
}

// resolveSeanceTarget picks the dead conversation to consult: an explicit
// --target (resolved to a specific generation, NOT redirected forward to
// a live successor), else the caller's own predecessor walked back
// --back generations.
func resolveSeanceTarget(p *seanceParams, stderr io.Writer) (string, int) {
	if t := strings.TrimSpace(p.Target); t != "" {
		// A séance addresses a specific generation, so resolve WITHOUT the
		// usual forward redirect (which would walk us to the live head and
		// have the agent consult itself). A raw conv-id or prefix is the
		// supported form.
		if row, err := db.GetConvIndex(t); err == nil && row != nil {
			return row.ConvID, rcOK
		}
		if row, err := db.FindConvIndexByPrefix(t); err == nil && row != nil {
			return row.ConvID, rcOK
		}
		fmt.Fprintf(stderr, "Error: no conversation matches --target %q (pass a conv-id or 8+-char prefix).\n", t)
		return "", rcNotFound
	}

	back := max(p.Back, 1)
	me, err := currentConvID()
	if err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return "", rcNotFound
	}
	ancestor, hops, err := db.ResolvePredecessorN(me, back)
	if err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return "", rcIOFailure
	}
	if ancestor == "" {
		fmt.Fprintln(stderr, "Error: you have no predecessor to consult — this conversation was not reincarnated from another agent.")
		fmt.Fprintln(stderr, "Use --target <conv-id> to hold a séance with a specific dead conversation.")
		return "", rcNotFound
	}
	if hops < back {
		fmt.Fprintf(stderr, "Note: chain is only %d generation(s) deep; consulting the oldest ancestor.\n", hops)
	}
	return ancestor, rcOK
}
