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

// `tclaude agent delete <selector>` — permanently remove an agent.
//
// Wipes every row that references the conv-id across the agent /
// conv / cron / succession / sessions tables, plus the .jsonl file
// and the ~/.claude/session-env/<conv-id> token. Useful for
// orphaned aliases left over from spawn-without-startup-write
// (pre-bc7ec81 behaviour) or any agent the human just doesn't want
// around any more.
//
// Refuses when the target's tmux session is alive — the human must
// stop it first via `tclaude agent stop <selector>`. `--force`
// kills the tmux session inline before deleting (mirrors the stop
// endpoint's --force).
//
// Auth: requires the agent.delete permission (NOT default-granted —
// destructive) OR being an owner of a group containing the target.
// Self-delete via this command is refused; humans should use
// `tclaude conv rm` instead.

type deleteParams struct {
	Selector string `pos:"true" help:"Target conv: alias, full conv-id, or 8+-char prefix"`
	Force    bool   `long:"force" short:"f" help:"Kill the tmux session before deleting (otherwise refuses when target is alive)"`
	Yes      bool   `long:"yes" short:"y" help:"Skip the confirmation prompt"`
}

func deleteCmd() *cobra.Command {
	return boa.CmdT[deleteParams]{
		Use:   "delete",
		Short: "Permanently delete an agent (cleanup orphans / unwanted aliases)",
		Long: "Wipes every row that references the conv-id across the agent / " +
			"conv / cron / succession / sessions tables, plus the .jsonl file " +
			"and the ~/.claude/session-env/<conv-id> token. " +
			"\n\n" +
			"Refuses when the target's tmux session is alive — pass --force " +
			"to kill the session inline. " +
			"\n\n" +
			"Auth: requires the agent.delete permission (not default-granted) " +
			"OR being an owner of a group containing the target. Self-delete " +
			"is refused via this command; use `tclaude conv rm` instead.",
		ParamEnrich: common.DefaultParamEnricher(),
		InitFuncCtx: func(ctx *boa.HookContext, p *deleteParams, _ *cobra.Command) error {
			boa.GetParamT(ctx, &p.Selector).SetAlternativesFunc(completeConvSelectors)
			return nil
		},
		RunFunc: func(p *deleteParams, _ *cobra.Command, _ []string) {
			os.Exit(runDelete(p, os.Stdout, os.Stderr, os.Stdin))
		},
	}.ToCobra()
}

func runDelete(p *deleteParams, stdout, stderr io.Writer, stdin io.Reader) int {
	selector := strings.TrimSpace(p.Selector)
	if selector == "" {
		fmt.Fprintln(stderr, "Error: a target selector is required")
		return rcInvalidArg
	}
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	if !p.Yes {
		// Read once on the daemon to surface what we're about to
		// destroy. Cheap; kept here so the prompt is informative even
		// when the daemon is the only place that knows the conv-id.
		fmt.Fprintf(stdout, "About to permanently delete agent %q (every related row + the .jsonl).\n",
			selector)
		if p.Force {
			fmt.Fprintln(stdout, "--force is set: any live tmux session will be killed.")
		}
		fmt.Fprint(stdout, "Continue? [y/N]: ")
		buf := make([]byte, 8)
		n, _ := stdin.Read(buf)
		ans := strings.ToLower(strings.TrimSpace(string(buf[:n])))
		if ans != "y" && ans != "yes" {
			fmt.Fprintln(stdout, "Aborted.")
			return rcOK
		}
	}
	path := "/v1/agent/" + url.PathEscape(selector) + "/delete"
	if p.Force {
		path += "?force=1"
	}
	var resp struct {
		ConvID            string         `json:"conv_id"`
		CallerConv        string         `json:"caller_conv,omitempty"`
		Action            string         `json:"action"`
		PreStop           string         `json:"pre_stop,omitempty"`
		JsonlRemoved      bool           `json:"jsonl_removed"`
		SessionEnvRemoved bool           `json:"session_env_removed"`
		DBCounts          map[string]int `json:"db_counts"`
	}
	if err := DaemonRequest(http.MethodDelete, path, nil, &resp, DaemonOpts{}); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
	}
	fmt.Fprintf(stdout, "%s: %s\n", short(resp.ConvID), resp.Action)
	if resp.PreStop != "" {
		fmt.Fprintf(stdout, "  pre-stop: %s\n", resp.PreStop)
	}
	fmt.Fprintf(stdout, "  jsonl removed: %v\n", resp.JsonlRemoved)
	fmt.Fprintf(stdout, "  session-env removed: %v\n", resp.SessionEnvRemoved)
	if len(resp.DBCounts) > 0 {
		var nonZero []string
		for k, v := range resp.DBCounts {
			if v > 0 {
				nonZero = append(nonZero, fmt.Sprintf("%s=%d", k, v))
			}
		}
		if len(nonZero) > 0 {
			fmt.Fprintf(stdout, "  db rows removed: %s\n", strings.Join(nonZero, ", "))
		} else {
			fmt.Fprintln(stdout, "  db rows removed: (none — already gone)")
		}
	}
	if resp.CallerConv != "" {
		fmt.Fprintf(stdout, "  called by: %s\n", short(resp.CallerConv))
	}
	return rcOK
}
