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

// `tclaude agent stop <selector>` and `tclaude agent resume <selector>`
// — single-conv variants of the bulk `groups stop` / `groups resume`.
// Useful when a manager agent (or human) wants to act on one
// subordinate without affecting the rest of a group.
//
// No self path: an agent can already stop itself by typing `/exit`
// directly in its own CC pane; the daemon-routed verb is only
// interesting when the target is a DIFFERENT conv. So both verbs
// require a positional selector.
//
// Auth: agent.stop / agent.resume slugs (default human-only) OR the
// caller is an owner of a group containing the target. Same shape as
// agent.compact / agent.rename / agent.reincarnate.

type stopParams struct {
	Selector string `pos:"true" help:"Target conv: alias, full conv-id, or 8+-char prefix"`
	Force    bool   `long:"force" short:"f" help:"Use tmux kill-session instead of soft-stop /exit (drops unsubmitted input)"`
}

func stopCmd() *cobra.Command {
	return boa.CmdT[stopParams]{
		Use:   "stop",
		Short: "Stop another agent's tmux session",
		Long: "Soft-stops the target agent's tmux pane by injecting `/exit`. " +
			"With --force, falls back to tmux kill-session — last resort, " +
			"drops any unsubmitted input the agent hadn't sent yet. " +
			"\n\n" +
			"Idempotent: agents already offline come back as " +
			"`skipped:already_offline`. " +
			"\n\n" +
			"Auth: requires the agent.stop permission OR being an owner of a " +
			"group containing the target. The single-conv variant of " +
			"`tclaude agent groups stop`.",
		ParamEnrich: common.DefaultParamEnricher(),
		InitFuncCtx: func(ctx *boa.HookContext, p *stopParams, _ *cobra.Command) error {
			boa.GetParamT(ctx, &p.Selector).SetAlternativesFunc(completeConvSelectors)
			return nil
		},
		RunFunc: func(p *stopParams, _ *cobra.Command, _ []string) {
			os.Exit(runStop(p, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

func runStop(p *stopParams, stdout, stderr io.Writer) int {
	selector := strings.TrimSpace(p.Selector)
	if selector == "" {
		fmt.Fprintln(stderr, "Error: a target selector is required")
		return rcInvalidArg
	}
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	path := "/v1/agent/" + url.PathEscape(selector) + "/stop"
	if p.Force {
		path += "?force=1"
	}
	var resp struct {
		ConvID      string `json:"conv_id"`
		CallerConv  string `json:"caller_conv,omitempty"`
		Action      string `json:"action"`
		Detail      string `json:"detail,omitempty"`
		TmuxSession string `json:"tmux_session,omitempty"`
	}
	if err := DaemonRequest(http.MethodPost, path, nil, &resp, DaemonOpts{}); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
	}
	fmt.Fprintf(stdout, "%s: %s\n", short(resp.ConvID), resp.Action)
	if resp.TmuxSession != "" {
		fmt.Fprintf(stdout, "  tmux: %s\n", resp.TmuxSession)
	}
	if resp.Detail != "" {
		fmt.Fprintf(stdout, "  detail: %s\n", resp.Detail)
	}
	if resp.CallerConv != "" {
		fmt.Fprintf(stdout, "  called by: %s\n", short(resp.CallerConv))
	}
	return rcOK
}

type resumeParams struct {
	Selector string `pos:"true" help:"Target conv: alias, full conv-id, or 8+-char prefix"`
}

func resumeCmd() *cobra.Command {
	return boa.CmdT[resumeParams]{
		Use:   "resume",
		Short: "Resume another agent into a fresh tmux session",
		Long: "Spawns `tclaude session new -r <conv> -d --global` for the target " +
			"conv if it isn't already online, attaching it to a fresh tmux " +
			"pane. Lands the agent in the cwd recorded on its last session " +
			"row, falling back to the daemon's cwd if none is known. " +
			"\n\n" +
			"Idempotent: agents already online come back as " +
			"`skipped:already_online`. " +
			"\n\n" +
			"Auth: requires the agent.resume permission OR being an owner of a " +
			"group containing the target. The single-conv variant of " +
			"`tclaude agent groups resume`.",
		ParamEnrich: common.DefaultParamEnricher(),
		InitFuncCtx: func(ctx *boa.HookContext, p *resumeParams, _ *cobra.Command) error {
			boa.GetParamT(ctx, &p.Selector).SetAlternativesFunc(completeConvSelectors)
			return nil
		},
		RunFunc: func(p *resumeParams, _ *cobra.Command, _ []string) {
			os.Exit(runResume(p, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

func runResume(p *resumeParams, stdout, stderr io.Writer) int {
	selector := strings.TrimSpace(p.Selector)
	if selector == "" {
		fmt.Fprintln(stderr, "Error: a target selector is required")
		return rcInvalidArg
	}
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	path := "/v1/agent/" + url.PathEscape(selector) + "/resume"
	var resp struct {
		ConvID     string `json:"conv_id"`
		CallerConv string `json:"caller_conv,omitempty"`
		Action     string `json:"action"`
		Detail     string `json:"detail,omitempty"`
	}
	if err := DaemonRequest(http.MethodPost, path, nil, &resp, DaemonOpts{}); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
	}
	fmt.Fprintf(stdout, "%s: %s\n", short(resp.ConvID), resp.Action)
	if resp.Detail != "" {
		fmt.Fprintf(stdout, "  detail: %s\n", resp.Detail)
	}
	if resp.CallerConv != "" {
		fmt.Fprintf(stdout, "  called by: %s\n", short(resp.CallerConv))
	}
	return rcOK
}
