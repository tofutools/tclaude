package agent

import (
	"encoding/json"
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
	Selector string `pos:"true" help:"Target conv: title, full conv-id, or 8+-char prefix"`
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
	Selector string `pos:"true" help:"Target conv: title, full conv-id, or 8+-char prefix"`
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
// orphaned agents left over from spawn-without-startup-write
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
	Selector string `pos:"true" help:"Target conv: title, full conv-id, or 8+-char prefix"`
	Force    bool   `long:"force" short:"f" help:"Kill the tmux session before deleting (otherwise refuses when target is alive)"`
	Yes      bool   `long:"yes" short:"y" help:"Skip the confirmation prompt"`
}

func deleteCmd() *cobra.Command {
	return boa.CmdT[deleteParams]{
		Use:   "delete",
		Short: "Permanently delete an agent (cleanup orphans / unwanted agents)",
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

	// 1. Resolve the selector up front so the prompt shows the actual
	//    conv(s) about to be deleted. Ambiguous selectors (e.g. a title
	//    shared by an orphan and a fresh clone) become a batch delete —
	//    we list every match before asking for confirmation.
	targets, err := resolveDeleteTargets(selector)
	if err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
	}
	if len(targets) == 0 {
		fmt.Fprintf(stderr, "Error: no conversation matches %q\n", selector)
		return rcNotFound
	}

	if !p.Yes {
		if len(targets) == 1 {
			t := targets[0]
			fmt.Fprintf(stdout, "About to permanently delete agent %s (%s).\n",
				short(t.ConvID), describeTarget(t))
		} else {
			fmt.Fprintf(stdout, "Selector %q matches %d conversations — ALL will be deleted:\n",
				selector, len(targets))
			for _, t := range targets {
				fmt.Fprintf(stdout, "  • %s — %s\n", short(t.ConvID), describeTarget(t))
			}
		}
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

	// 2. Delete each target by its full conv-id so the daemon's resolver
	//    can't re-ambiguate.
	failed := 0
	for _, t := range targets {
		if rc := deleteOne(t.ConvID, p.Force, stdout, stderr); rc != rcOK {
			failed++
		}
	}
	if failed > 0 {
		return rcIOFailure
	}
	return rcOK
}

// resolveDeleteTargets resolves a selector to one or more conv-ids via
// GET /v1/lookup. On a single match the daemon returns 200 + {conv_id};
// on ambiguity it returns 409 + {candidates: [...]}.
func resolveDeleteTargets(selector string) ([]*peerEntry, error) {
	var single struct {
		ConvID string `json:"conv_id"`
	}
	err := DaemonGet("/v1/lookup?selector="+url.QueryEscape(selector), &single)
	if err == nil {
		return []*peerEntry{{ConvID: single.ConvID}}, nil
	}
	if de, ok := err.(*DaemonError); ok && de.Code == "ambiguous" {
		var body struct {
			Candidates []*peerEntry `json:"candidates"`
		}
		if jerr := json.Unmarshal(de.Raw, &body); jerr == nil && len(body.Candidates) > 0 {
			return body.Candidates, nil
		}
	}
	return nil, err
}

// describeTarget returns a short human-readable summary used in the
// confirmation prompt: "<title> in groups [a, b]" / "(unknown)".
func describeTarget(t *peerEntry) string {
	parts := []string{}
	if t.Title != "" {
		parts = append(parts, t.Title)
	} else {
		parts = append(parts, "(unknown)")
	}
	if len(t.Groups) > 0 {
		parts = append(parts, "in "+strings.Join(t.Groups, ","))
	}
	return strings.Join(parts, " ")
}

// deleteOne fires DELETE /v1/agent/{conv}/delete for a single conv-id
// and prints the per-conv outcome. Returns the CLI rc for that delete.
func deleteOne(convID string, force bool, stdout, stderr io.Writer) int {
	path := "/v1/agent/" + url.PathEscape(convID) + "/delete"
	if force {
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
		fmt.Fprintf(stderr, "Error deleting %s: %v\n", short(convID), err)
		return MapDaemonErrorToRC(err)
	}
	fmt.Fprintf(stdout, "%s: %s\n", short(resp.ConvID), resp.Action)
	if resp.PreStop != "" {
		fmt.Fprintf(stdout, "  pre-stop: %s\n", resp.PreStop)
	}
	if resp.JsonlRemoved {
		fmt.Fprintln(stdout, "  jsonl removed: true")
	}
	if resp.SessionEnvRemoved {
		fmt.Fprintln(stdout, "  session-env removed: true")
	}
	if len(resp.DBCounts) > 0 {
		var nonZero []string
		for k, v := range resp.DBCounts {
			if v > 0 {
				nonZero = append(nonZero, fmt.Sprintf("%s=%d", k, v))
			}
		}
		if len(nonZero) > 0 {
			fmt.Fprintf(stdout, "  db rows removed: %s\n", strings.Join(nonZero, ", "))
		}
	}
	if resp.CallerConv != "" {
		fmt.Fprintf(stdout, "  called by: %s\n", short(resp.CallerConv))
	}
	return rcOK
}
