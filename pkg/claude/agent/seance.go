package agent

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/common"
)

// `tclaude agent seance [question]` — summon the spirit of a dead
// predecessor and ask it what it knew (JOH-25, inspired by `gt seance`
// from Steve Yegge's Gas Town).
//
// When an agent reincarnates, the old conversation is retired but its
// full harness transcript survives on disk. A séance resumes that dead
// conversation HEADLESSLY for a single ephemeral turn from the predecessor's
// own launch dir, puts one question to it, and hands back JUST the answer. The
// successor pays for the answer (a few hundred tokens) instead of dragging the
// whole transcript back into its live context window, which is the very thing
// reincarnate was trying to shed.
//
// The mechanics are the `tclaude ask` headless-Q&A primitive (JOH-250/
// 252) aimed at a retired conv-id: the same harness-agnostic Asker argv
// builder and one-shot capture, just resolved against a dead ancestor and run
// in that ancestor's cwd (resume is cwd-scoped — a conv is only resumable from
// where it was created). The authenticated daemon runs this one subprocess:
// managed callers cannot safely write their harness home, while agentd can
// initialize the harness without exposing that home to the caller. The daemon
// replays the predecessor's recorded sandbox and approval posture, suppresses
// tclaude lifecycle hooks, bounds output/time, and does not persist the
// consultation back into the dead session.

type seanceParams struct {
	Question string `pos:"true" optional:"true" help:"What to ask the predecessor. Quote multi-word strings. Read from --file instead for long/multi-line questions. Omit only with --print-cmd."`
	File     string `long:"file" short:"f" optional:"true" help:"Read the question from this file ('-' reads stdin) instead of the positional argument. Sidesteps shell quoting. Mutually exclusive with the positional argument."`
	Target   string `long:"target" optional:"true" help:"Agent or exact dead generation to consult. Agent selectors (stable agent-id or name) apply --back from that actor's live head; a conv-id or 8+-char prefix addresses that exact generation without redirecting forward."`
	Back     int    `long:"back" optional:"true" default:"1" help:"Walk back this many generations (1 = immediate predecessor). Applies to self and agent selectors; an exact conv-id already names the generation."`
	Model    string `long:"model" optional:"true" help:"Model for the séance turn (default: the harness's configured default). The predecessor's own model is not recorded; you pick the medium for the summoning."`
	Effort   string `long:"effort" optional:"true" help:"Reasoning effort for the séance turn (harness-specific; default: harness default)."`
	Timeout  string `long:"timeout" optional:"true" help:"Cap the séance call, e.g. '90s' or '3m'. Default: 5m; maximum: 10m."`
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
			"you inherited at reincarnate). Use --back N to reach further up the chain. " +
			"With --target, a stable agent-id or name selects that actor and applies --back " +
			"from its live head; a conv-id or 8+-char prefix selects that exact dead generation. " +
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

// seanceResolveResp is the daemon-owned half of a séance plan. Resolution
// belongs in agentd because succession, session cwd and harness metadata live
// in private ~/.tclaude/data.
type seanceResolveResp struct {
	Predecessor     string   `json:"predecessor"`
	Harness         string   `json:"harness"`
	Cwd             string   `json:"cwd"`
	Hops            int      `json:"hops"`
	Requested       int      `json:"requested_back"`
	Exact           bool     `json:"exact"`
	Sandbox         string   `json:"sandbox"`
	Approval        string   `json:"approval"`
	AutoReview      bool     `json:"auto_review"`
	SandboxDenyDirs []string `json:"sandbox_deny_dirs,omitempty"`
}

type seanceRunResp struct {
	Answer      string `json:"answer"`
	Predecessor string `json:"predecessor"`
	Harness     string `json:"harness"`
	Truncated   bool   `json:"truncated,omitempty"`
}

const (
	defaultSeanceTimeout = 5 * time.Minute
	maxSeanceTimeout     = 10 * time.Minute
	maxSeanceQuestion    = 32 << 10
)

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

	timeout := defaultSeanceTimeout
	if p.Timeout != "" {
		d, err := time.ParseDuration(p.Timeout)
		if err != nil || d <= 0 {
			fmt.Fprintf(stderr, "Error: invalid --timeout %q (use e.g. 90s, 3m)\n", p.Timeout)
			return rcInvalidArg
		}
		timeout = d
	}
	if timeout > maxSeanceTimeout {
		fmt.Fprintf(stderr, "Error: --timeout must not exceed %s\n", maxSeanceTimeout)
		return rcInvalidArg
	}
	if len(question) > maxSeanceQuestion {
		fmt.Fprintf(stderr, "Error: question is too long (%d bytes; maximum %d)\n", len(question), maxSeanceQuestion)
		return rcInvalidArg
	}

	// 2) Ask agentd to resolve the dead generation and its launch metadata.
	//    The daemon owns the private DB; this CLI must never reach into
	//    ~/.tclaude/data directly from an agent sandbox.
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	var resolved seanceResolveResp
	if err := DaemonRequest(http.MethodPost, "/v1/whoami/seance", map[string]any{
		"target": strings.TrimSpace(p.Target),
		"back":   max(p.Back, 1),
	}, &resolved, DaemonOpts{}); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return mapSeanceDaemonErrorToRC(err)
	}
	target := resolved.Predecessor
	if target == "" || resolved.Cwd == "" {
		fmt.Fprintln(stderr, "Error: agentd returned an incomplete séance plan")
		return rcIOFailure
	}

	// 3) Build the headless one-shot resume argv via the shared Asker.
	h, err := harness.Resolve(resolved.Harness)
	if err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return rcInvalidArg
	}
	if !h.SupportsAsk() {
		fmt.Fprintf(stderr, "Error: harness %q cannot hold a séance (no headless resume/ask support).\n", h.Name)
		return rcInvalidArg
	}

	model, err := h.Models.ValidateModel(p.Model)
	if err != nil {
		fmt.Fprintf(stderr, "Error: invalid --model: %v\n", err)
		return rcInvalidArg
	}
	effort, err := h.Models.ValidateEffort(p.Effort)
	if err != nil {
		fmt.Fprintf(stderr, "Error: invalid --effort: %v\n", err)
		return rcInvalidArg
	}
	posture := harness.SpawnSpec{
		SandboxMode:     resolved.Sandbox,
		SandboxDenyDirs: append([]string(nil), resolved.SandboxDenyDirs...),
		ApprovalPolicy:  resolved.Approval,
		AutoReview:      resolved.AutoReview,
	}
	if h.Name == harness.CodexName && posture.SandboxMode == harness.SandboxManagedProfile {
		// The daemon creates a launch-unique profile at execution time. Keep a
		// visibly symbolic name in --print-cmd rather than pretending the
		// generic read-only ask posture is what will run.
		posture.SandboxMode = ""
		posture.PermissionProfile = harness.CodexAgentProfile + "-<launch-id>"
	}
	spec := harness.AskSpec{
		ResumeID:      target,
		Prompt:        question,
		Print:         true, // capture mode: print the answer and exit
		Ephemeral:     true,
		LaunchPosture: &posture,
		Model:         model,
		Effort:        effort,
	}
	argv := h.Ask.BuildAskArgv(spec)

	if !resolved.Exact && resolved.Hops > 0 && resolved.Hops < resolved.Requested {
		fmt.Fprintf(stderr, "Note: chain is only %d generation(s) deep; consulting the oldest ancestor.\n", resolved.Hops)
	}
	if p.PrintCmd {
		fmt.Fprintf(stdout, "predecessor: %s\n", short(target))
		fmt.Fprintf(stdout, "harness:     %s\n", h.Name)
		fmt.Fprintf(stdout, "cwd:         %s\n", resolved.Cwd)
		fmt.Fprintf(stdout, "command:     %s\n", strings.Join(argv, " "))
		return rcOK
	}

	// 4) Hold the séance through agentd. A managed agent intentionally cannot
	//    write its harness home, so a nested Codex/Claude process cannot even
	//    initialize in this CLI's sandbox. agentd owns the narrow, audited,
	//    bounded one-shot execution boundary and replays the predecessor's
	//    recorded sandbox/approval posture from the private DB.
	fmt.Fprintf(stderr, "Summoning %s (resuming in %s)...\n", short(target), resolved.Cwd)
	var result seanceRunResp
	if err := DaemonRequest(http.MethodPost, "/v1/whoami/seance/run", map[string]any{
		// Pin execution to the exact generation that the free planning call
		// resolved. A concurrent reincarnation cannot silently retarget a
		// billable request to a different ancestor.
		"target":     target,
		"back":       1,
		"question":   question,
		"model":      model,
		"effort":     effort,
		"timeout_ms": timeout.Milliseconds(),
	}, &result, DaemonOpts{Timeout: timeout + 30*time.Second}); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return mapSeanceDaemonErrorToRC(err)
	}
	if result.Truncated {
		fmt.Fprintln(stderr, "Error: agentd truncated the séance answer")
		return rcIOFailure
	}
	_, _ = io.WriteString(stdout, result.Answer)
	return rcOK
}

func mapSeanceDaemonErrorToRC(err error) int {
	if de, ok := err.(*DaemonError); ok {
		switch de.Code {
		case "permission":
			return rcAuth
		case "unsupported_harness":
			return rcInvalidArg
		case "invalid_arg":
			return rcInvalidArg
		}
	}
	return MapDaemonErrorToRC(err)
}
