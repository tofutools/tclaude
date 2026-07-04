package agent

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/common"
)

// task.go is `tclaude agent task {set,clear,show}` — the per-agent
// task-reference link (the dashboard's Task column). The link is an
// http(s) URL pointing at the work item the agent is on: a Linear issue,
// a GitHub issue/PR, a ticket, …
//
// By default the target is the calling agent itself (requires
// `self.task`). `--target <selector>` acts on ANOTHER agent — the
// manager pattern (requires `agent.task`, or being an owner of a group
// containing the target). This mirrors `tclaude agent rename`.
func taskCmd() *cobra.Command {
	return boa.CmdT[struct{}]{
		Use:   "task",
		Short: "Manage an agent's task-reference link (the dashboard Task column)",
		Long: "Set, clear, or show an agent's task-reference link — an http(s) URL " +
			"(a Linear issue, GitHub issue/PR, ticket, …) rendered as a clickable label in the " +
			"dashboard's Task column. By default operates on the calling agent (requires `self.task`); " +
			"use --target <selector> to act on ANOTHER agent — requires `agent.task`, or being an owner " +
			"of a group containing the target.",
		ParamEnrich: common.DefaultParamEnricher(),
		SubCmds: []*cobra.Command{
			taskSetCmd(),
			taskClearCmd(),
			taskShowCmd(),
		},
	}.ToCobra()
}

// --- task set ---

type taskSetParams struct {
	URL      string `pos:"true" help:"Task URL (http(s) only) — e.g. a Linear issue or GitHub issue/PR link"`
	Label    string `long:"label" short:"l" optional:"true" help:"Optional display label overriding the auto-derived one (Linear->JOH-xxx, GitHub->#nnn, else host)"`
	Target   string `long:"target" optional:"true" help:"Act on ANOTHER agent instead of self. Selector: title, full conv-id, or 8+-char prefix. Requires agent.task, or owning a group containing the target."`
	AskHuman string `long:"ask-human" optional:"true" help:"On permission denial, ask the human via popup with this timeout (e.g. '30s' or '60'). Capped at 300s. Timeout = deny. Self-target only."`
}

func taskSetCmd() *cobra.Command {
	return boa.CmdT[taskSetParams]{
		Use:         "set",
		Short:       "Set an agent's task-reference link",
		ParamEnrich: common.DefaultParamEnricher(),
		InitFuncCtx: func(ctx *boa.HookContext, p *taskSetParams, _ *cobra.Command) error {
			boa.GetParamT(ctx, &p.Target).SetAlternativesFunc(completeConvSelectors)
			boa.GetParamT(ctx, &p.AskHuman).SetAlternativesFunc(completeAskHumanDurations)
			return nil
		},
		RunFunc: func(p *taskSetParams, _ *cobra.Command, _ []string) {
			os.Exit(runTaskSet(p, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

func runTaskSet(p *taskSetParams, stdout, stderr io.Writer) int {
	taskURL := strings.TrimSpace(p.URL)
	if taskURL == "" {
		fmt.Fprintln(stderr, "Error: a task URL is required (use `task clear` to remove one).")
		return rcInvalidArg
	}
	body := map[string]any{"url": taskURL, "label": strings.TrimSpace(p.Label)}
	return taskWrite(p.Target, p.AskHuman, body, stdout, stderr)
}

// --- task clear ---

type taskClearParams struct {
	Target   string `long:"target" optional:"true" help:"Act on ANOTHER agent instead of self. Selector: title, full conv-id, or 8+-char prefix. Requires agent.task, or owning a group containing the target."`
	AskHuman string `long:"ask-human" optional:"true" help:"On permission denial, ask the human via popup with this timeout (e.g. '30s' or '60'). Capped at 300s. Timeout = deny. Self-target only."`
}

func taskClearCmd() *cobra.Command {
	return boa.CmdT[taskClearParams]{
		Use:         "clear",
		Short:       "Clear an agent's task-reference link",
		ParamEnrich: common.DefaultParamEnricher(),
		InitFuncCtx: func(ctx *boa.HookContext, p *taskClearParams, _ *cobra.Command) error {
			boa.GetParamT(ctx, &p.Target).SetAlternativesFunc(completeConvSelectors)
			boa.GetParamT(ctx, &p.AskHuman).SetAlternativesFunc(completeAskHumanDurations)
			return nil
		},
		RunFunc: func(p *taskClearParams, _ *cobra.Command, _ []string) {
			os.Exit(runTaskClear(p, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

func runTaskClear(p *taskClearParams, stdout, stderr io.Writer) int {
	return taskWrite(p.Target, p.AskHuman, map[string]any{"clear": true}, stdout, stderr)
}

// taskWrite POSTs a set/clear to the whoami or cross-agent task endpoint
// and renders the result. Shared by set and clear.
func taskWrite(target, askHuman string, body map[string]any, stdout, stderr io.Writer) int {
	target = strings.TrimSpace(target)
	if target != "" && askHuman != "" {
		fmt.Fprintln(stderr, "Error: --ask-human is only supported when targeting self; cross-agent calls require an explicit slug grant or group ownership.")
		return rcInvalidArg
	}
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	ask, err := ParseAskHuman(askHuman)
	if err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return rcInvalidArg
	}
	if ask > 0 {
		fmt.Fprintf(stdout, "Waiting up to %s for human approval...\n", ask)
	}
	path := "/v1/whoami/task"
	if target != "" {
		path = "/v1/agent/" + url.PathEscape(target) + "/task"
	}
	var resp taskRefResp
	if err := DaemonRequest(http.MethodPost, path, body, &resp, DaemonOpts{AskHuman: ask}); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
	}
	printTaskResult(stdout, &resp)
	return rcOK
}

// --- task show ---

type taskShowParams struct {
	Target string `long:"target" optional:"true" help:"Show ANOTHER agent's link instead of self. Selector: title, full conv-id, or 8+-char prefix. Requires agent.task, or owning a group containing the target."`
}

func taskShowCmd() *cobra.Command {
	return boa.CmdT[taskShowParams]{
		Use:         "show",
		Short:       "Show an agent's task-reference link",
		ParamEnrich: common.DefaultParamEnricher(),
		InitFuncCtx: func(ctx *boa.HookContext, p *taskShowParams, _ *cobra.Command) error {
			boa.GetParamT(ctx, &p.Target).SetAlternativesFunc(completeConvSelectors)
			return nil
		},
		RunFunc: func(p *taskShowParams, _ *cobra.Command, _ []string) {
			os.Exit(runTaskShow(p, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

func runTaskShow(p *taskShowParams, stdout, stderr io.Writer) int {
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	target := strings.TrimSpace(p.Target)
	path := "/v1/whoami/task"
	if target != "" {
		path = "/v1/agent/" + url.PathEscape(target) + "/task"
	}
	var resp taskRefResp
	if err := DaemonGet(path, &resp); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
	}
	if resp.TaskURL == "" {
		fmt.Fprintf(stdout, "%s: no task link set\n", short(resp.ConvID))
		return rcOK
	}
	fmt.Fprintf(stdout, "%s: %s  (%s)\n", short(resp.ConvID), resp.TaskURL, resp.TaskLabel)
	return rcOK
}

// taskRefResp is the shared wire shape of the task endpoints.
type taskRefResp struct {
	ConvID        string `json:"conv_id"`
	TaskURL       string `json:"task_ref_url,omitempty"`
	TaskLabel     string `json:"task_ref_label,omitempty"`
	Cleared       bool   `json:"cleared,omitempty"`
	CallerConv    string `json:"caller_conv,omitempty"`
	CallerAgentID string `json:"caller_agent_id,omitempty"`
}

func printTaskResult(stdout io.Writer, resp *taskRefResp) {
	by := ""
	if resp.CallerConv != "" {
		by = " (by " + shortAgentID(resp.CallerAgentID, resp.CallerConv) + ")"
	}
	if resp.Cleared {
		fmt.Fprintf(stdout, "Cleared task link for %s%s\n", short(resp.ConvID), by)
		return
	}
	fmt.Fprintf(stdout, "Set task link for %s to %s (%s)%s\n", short(resp.ConvID), resp.TaskURL, resp.TaskLabel, by)
}
