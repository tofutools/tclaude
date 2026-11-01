package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/common"
)

// sandboxImplCmd is `tclaude agent sandbox-impl` — read or reassign the sandbox
// IMPLEMENTATION an existing agent will relaunch under.
//
// There is no self-targeted form. The implementation is applied by a launch, so
// the target is another agent named positionally — stopped, or idle with
// `set --restart` — rather than the `--target` flag the self-defaulting
// lifecycle verbs use.
func sandboxImplCmd() *cobra.Command {
	return boa.CmdT[struct{}]{
		Use:     "sandbox-impl",
		Aliases: []string{"sandbox-implementation"},
		Short:   "Show or assign the sandbox implementation an agent relaunches under",
		Long: "Reads and rewrites the durable sandbox implementation recorded for an existing agent — " +
			"the layer that owns OS-level confinement for its launches.\n\n" +
			"The implementation is normally frozen when an agent is spawned. `set` is how an agent " +
			"created before an implementation existed is moved onto it: the classic case is giving an " +
			"older agent `resource-only`, so its next launch runs in a per-agent cgroup with the " +
			"accounting, OOM attribution and kill handle that brings.\n\n" +
			"The assignment takes effect on the next launch. A stopped agent is assigned and left " +
			"stopped; a running agent needs `set --restart`, which stops the idle agent, assigns and " +
			"resumes it. Both subcommands require the `agent.sandbox-impl` permission, " +
			"which group ownership does NOT confer.",
		ParamEnrich: common.DefaultParamEnricher(),
		SubCmds:     []*cobra.Command{sandboxImplShowCmd(), sandboxImplSetCmd(), sandboxImplDefaultCmd()},
	}.ToCobra()
}

type sandboxImplShowParams struct {
	Agent string `pos:"true" help:"Agent selector: title, full conv-id, or 8+-char prefix"`
	JSON  bool   `long:"json" help:"Emit the stable JSON object instead of the human view"`
}

func sandboxImplShowCmd() *cobra.Command {
	return boa.CmdT[sandboxImplShowParams]{
		Use:         "show",
		Short:       "Show the sandbox implementation an agent will relaunch under",
		ParamEnrich: common.DefaultParamEnricher(),
		InitFuncCtx: func(ctx *boa.HookContext, p *sandboxImplShowParams, _ *cobra.Command) error {
			boa.GetParamT(ctx, &p.Agent).SetAlternativesFunc(completeConvSelectors)
			return nil
		},
		RunFunc: func(p *sandboxImplShowParams, _ *cobra.Command, _ []string) {
			os.Exit(runSandboxImplShow(p, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

type sandboxImplSetParams struct {
	Agent          string `pos:"true" help:"Agent selector: title, full conv-id, or 8+-char prefix"`
	Implementation string `pos:"true" help:"harness-builtin | tclaude-layer | resource-only | off"`
	Sandbox        string `long:"sandbox" optional:"true" help:"Pin the harness-builtin sandbox mode recorded alongside the implementation. Omitted, the agent's recorded mode is carried through the same resolution a launch applies."`
	Restart        bool   `long:"restart" optional:"true" help:"If the agent is running, stop it (it must be idle), assign, and resume the same conversation under the new implementation. Without it, a running agent is refused."`
	Yes            bool   `long:"yes" short:"y" optional:"true" help:"Accept moving an OpenCode agent out of private state, which is otherwise refused with what the move does."`
	JSON           bool   `long:"json" help:"Emit the stable JSON object instead of the human view"`
	AskHuman       string `long:"ask-human" optional:"true" help:"On permission denial, ask the human via popup with this timeout (e.g. '30s'). Capped at 300s. Timeout = deny."`
}

func sandboxImplSetCmd() *cobra.Command {
	return boa.CmdT[sandboxImplSetParams]{
		Use:   "set",
		Short: "Assign the sandbox implementation an agent will relaunch under",
		Long: "Records a new sandbox implementation for an existing agent. The posture is applied by " +
			"the launch that follows, so a running agent is refused unless --restart is given: then, " +
			"once every gate below has passed, the idle agent is stopped, the posture recorded, and the " +
			"same conversation resumed under it. A stopped agent is assigned and left stopped.\n\n" +
			"The recorded harness sandbox mode moves with the implementation, because the " +
			"implementation decides what the harness's own sandbox may be — `resource-only`, `off` " +
			"and `tclaude-layer` all derive the mode their launch runs under. A mode derived that way " +
			"is not carried forward onto the next implementation (the harness default is used " +
			"instead), so restoring harness-builtin does not silently keep the off mode its " +
			"predecessor forced. Use --sandbox to pin a mode explicitly.\n\n" +
			"The gates a launch runs are run here too, against the chain the relaunch will resolve: " +
			"an implementation this harness or host cannot run, rules it cannot represent, or a " +
			"ceiling it cannot carry are all refused now rather than at wake time — where a relaunch " +
			"has no allow-unenforced override to rescue it. `resource-only` additionally probes that " +
			"this host can actually create the per-agent cgroup, since a relaunch would only degrade " +
			"to a notice.\n\n" +
			"Moving an OpenCode agent out of private state is refused without --yes: the refusal " +
			"says how many conversations move, who can read them afterwards, and what is deleted.",
		ParamEnrich: common.DefaultParamEnricher(),
		InitFuncCtx: func(ctx *boa.HookContext, p *sandboxImplSetParams, _ *cobra.Command) error {
			boa.GetParamT(ctx, &p.Agent).SetAlternativesFunc(completeConvSelectors)
			boa.GetParamT(ctx, &p.Implementation).SetAlternatives(sandboxImplementationChoices())
			boa.GetParamT(ctx, &p.AskHuman).SetAlternativesFunc(completeAskHumanDurations)
			return nil
		},
		RunFunc: func(p *sandboxImplSetParams, _ *cobra.Command, _ []string) {
			os.Exit(runSandboxImplSet(p, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

// sandboxImplementationChoices is the closed set, for shell completion and the
// local pre-check.
func sandboxImplementationChoices() []string {
	return []string{
		string(sandboxpolicy.ImplementationHarnessBuiltin),
		string(sandboxpolicy.ImplementationTclaudeLayer),
		string(sandboxpolicy.ImplementationResourceOnly),
		string(sandboxpolicy.ImplementationOff),
	}
}

// sandboxImplSetTimeout covers stopping the agent, moving its OpenCode
// conversations, each a few OpenCode runs, and resuming it. The default budget
// gave up while the daemon carried on.
const sandboxImplSetTimeout = 3 * time.Minute

// sandboxMoveOutConfirmCode is the daemon's refusal of an assignment that would
// move an OpenCode agent out of private state unconfirmed.
const sandboxMoveOutConfirmCode = "opencode_move_out_confirm"

// sandboxImplResp mirrors the daemon's sandbox-impl payload.
type sandboxImplResp struct {
	ConvID           string `json:"conv_id"`
	AgentID          string `json:"agent_id,omitempty"`
	Harness          string `json:"harness,omitempty"`
	Implementation   string `json:"sandbox_implementation"`
	Previous         string `json:"previous_sandbox_implementation,omitempty"`
	Sandbox          string `json:"sandbox,omitempty"`
	Source           string `json:"sandbox_source,omitempty"`
	TemporarySandbox bool   `json:"temporary_sandbox_active,omitempty"`
	Online           bool   `json:"online"`
	ResourceCgroup   bool   `json:"resource_cgroup"`
	// ResourceCgroupRequired distinguishes a boundary the relaunch cannot
	// proceed without — which the assignment probed — from one it merely
	// attempts, which it did not.
	ResourceCgroupRequired bool `json:"resource_cgroup_required,omitempty"`
	// Restart is set when a --restart assignment stopped and resumed a live
	// agent to apply it.
	Restart         string `json:"restart,omitempty"`
	SwitchedClients int    `json:"switched_clients,omitempty"`
	// Note is what an operator should know about what the assignment did.
	Note string `json:"note,omitempty"`
}

// runSandboxImplShow reads one agent's durable posture and renders it.
func runSandboxImplShow(p *sandboxImplShowParams, stdout, stderr io.Writer) int {
	target := strings.TrimSpace(p.Agent)
	if target == "" {
		fmt.Fprintln(stderr, "Error: an agent selector is required.")
		return rcInvalidArg
	}
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	var resp sandboxImplResp
	if err := DaemonRequest(http.MethodGet,
		sandboxImplPath(target), nil, &resp, DaemonOpts{}); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
	}
	return printSandboxImpl(&resp, p.JSON, stdout, stderr)
}

// runSandboxImplSet assigns a new implementation. The value is checked against
// the closed set locally so a typo does not cost a round-trip; the daemon
// re-validates against the resolved harness and host, which is the authority.
func runSandboxImplSet(p *sandboxImplSetParams, stdout, stderr io.Writer) int {
	target := strings.TrimSpace(p.Agent)
	if target == "" {
		fmt.Fprintln(stderr, "Error: an agent selector is required.")
		return rcInvalidArg
	}
	implementation := strings.TrimSpace(p.Implementation)
	if implementation == "" {
		fmt.Fprintf(stderr, "Error: an implementation is required (%s).\n",
			strings.Join(sandboxImplementationChoices(), " | "))
		return rcInvalidArg
	}
	// Reject a misspelling locally so the operator sees the closed set rather
	// than a round-trip. The daemon validates again against the resolved harness
	// and host, which is where the authoritative refusal lives.
	if _, err := sandboxpolicy.NormalizeImplementation(implementation); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return rcInvalidArg
	}
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	ask, err := ParseAskHuman(p.AskHuman)
	if err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return rcInvalidArg
	}
	if ask > 0 {
		fmt.Fprintf(stdout, "Waiting up to %s for human approval...\n", ask)
	}
	body := map[string]any{
		"implementation": implementation,
		"sandbox":        strings.TrimSpace(p.Sandbox),
		"restart":        p.Restart,
	}
	if p.Yes {
		body["confirm_move_out"] = true
	}
	var resp sandboxImplResp
	if err := DaemonRequest(http.MethodPost,
		sandboxImplPath(target), body, &resp, DaemonOpts{AskHuman: ask, Timeout: sandboxImplSetTimeout}); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		var daemonErr *DaemonError
		if errors.As(err, &daemonErr) && daemonErr.Code == sandboxMoveOutConfirmCode {
			fmt.Fprintln(stderr, "Run again with --yes to move them.")
		}
		return MapDaemonErrorToRC(err)
	}
	return printSandboxImpl(&resp, p.JSON, stdout, stderr)
}

func sandboxImplPath(target string) string {
	return "/v1/agent/" + url.PathEscape(target) + "/sandbox-impl"
}

// printSandboxImpl renders one posture, naming what an assignment replaced and
// what the operator still has to do for it to take effect.
func printSandboxImpl(resp *sandboxImplResp, asJSON bool, stdout, stderr io.Writer) int {
	if asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(resp); err != nil {
			fmt.Fprintf(stderr, "Error: %v\n", err)
			return rcIOFailure
		}
		return rcOK
	}
	label := short(resp.ConvID)
	if resp.Previous != "" && resp.Previous != resp.Implementation {
		fmt.Fprintf(stdout, "%s: sandbox implementation %s → %s\n",
			label, sandboxImplementationDisplayName(resp.Previous), sandboxImplementationDisplayName(resp.Implementation))
	} else {
		fmt.Fprintf(stdout, "%s: sandbox implementation %s\n", label, sandboxImplementationDisplayName(resp.Implementation))
	}
	if resp.Sandbox != "" {
		line := "  harness sandbox mode: " + sandboxImplementationDisplayName(resp.Sandbox)
		if resp.Source != "" {
			line += " (chosen by " + resp.Source + ")"
		}
		fmt.Fprintln(stdout, line)
	}
	if resp.ResourceCgroup {
		// The assignment probes only a boundary the relaunch would fail without,
		// so only there can this promise that the next launch gets one. What a
		// launch merely attempts was not probed, and an undelegated host runs it
		// anyway with a disclosure — so do not claim a boundary nothing proved.
		line := "  per-agent cgroup: yes (attempted by the next launch; without a delegated " +
			"cgroup v2 subtree it launches anyway and discloses the missing accounting)"
		if resp.ResourceCgroupRequired {
			line = "  per-agent cgroup: yes (created by the next launch)"
		}
		fmt.Fprintln(stdout, line)
	}
	if resp.TemporarySandbox {
		fmt.Fprintln(stdout, "  note: the temporary dashboard sandbox unlock is active; it suspends this posture until restored")
	}
	if resp.Note != "" {
		fmt.Fprintln(stdout, "  note: "+resp.Note)
	}
	if resp.Restart != "" {
		fmt.Fprintln(stdout, "  restarted: the agent is running under the new implementation")
	} else if resp.Online {
		fmt.Fprintln(stdout, "  note: the agent is running; the recorded posture applies to its next launch")
	} else if resp.Previous != "" {
		fmt.Fprintln(stdout, "  wake the agent to launch it under the new implementation")
	}
	return rcOK
}

// sandboxImplementationDisplayName is human-facing only; JSON and command
// arguments retain the stable implementation values.
func sandboxImplementationDisplayName(value string) string {
	if sandboxpolicy.Implementation(value) == sandboxpolicy.ImplementationTclaudeLayer {
		return "tclaude’s built-in sandbox"
	}
	return value
}
