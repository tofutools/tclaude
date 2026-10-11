package agent

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/harness/transcript"
	"github.com/tofutools/tclaude/pkg/common"
)

type morphParams struct {
	ProceedWithoutSummary bool   `long:"proceed-without-summary" short:"-" help:"Explicitly continue without a source-written summary (provider outage): carry readable history and the full-transcript pointer, accepting history loss"`
	ConfirmHistoryLoss    bool   `long:"confirm-history-loss" help:"Confirm a large history cut and wait for the source agent to write a summary before morphing"`
	Agent                 string `pos:"true" optional:"true" help:"Agent to morph: title, agent_id, conv-id or prefix, or group:<name> for every other member of a group. Default: yourself"`
	Profile               string `long:"profile" short:"p" optional:"true" help:"Take the new launch form from this spawn profile; a profile for another harness makes it a cross-harness morph. Explicit flags override it"`
	Harness               string `long:"harness" optional:"true" help:"Morph into another harness: the history is translated into a new conversation of that harness. Needs --ask-for-approval (or a profile that sets one); an agent caller must use --profile"`
	History               string `long:"history" optional:"true" help:"Cross-harness only: how the conversation is carried. auto (native history, else saved to files the agent reads) | context-file | handoff (the agent first writes a handoff for its successor; needs a live agent)"`
	Model                 string `long:"model" optional:"true" help:"New model (validated for the agent's harness)"`
	Effort                string `long:"effort" optional:"true" help:"New reasoning effort (per-harness; Gemini has none)"`
	Approval              string `long:"ask-for-approval" optional:"true" help:"New launch approval posture (per-harness values, as for spawn). An agent caller may not grant a posture broader than its own"`
	Tools                 string `long:"tools" optional:"true" help:"New OpenCode tool governance: allow | ask | deny"`
	AskTimeout            string `long:"ask-user-question-timeout" optional:"true" help:"New Claude Code AskUserQuestion idle-timeout: inherit | never | 60s | 5m | 10m"`
	AutoCompactWindow     string `long:"auto-compact-window" optional:"true" help:"New Claude Code auto-compact window in tokens"`
	TrustDir              bool   `long:"trust-dir" help:"Cross-harness only: pre-trust the working directory in the new harness so its pane does not stop on the folder-trust dialog (edits that harness's trust store, as spawn --trust-dir does)"`
	Now                   bool   `long:"now" help:"Stop and relaunch immediately even if the agent is busy or stuck, resuming from its last durable history. Not available for a self-morph"`
	Back                  bool   `long:"back" help:"Morph back to the form the last morph replaced"`
	Cancel                bool   `long:"cancel" help:"Cancel a pending morph"`
	DryRun                bool   `long:"dry-run" help:"Show the before -> after form and whether you may apply it, without changing anything"`
	JSON                  bool   `long:"json" help:"Print the per-agent results as JSON"`
	HistoryWindowTokens   int64  `long:"history-window-tokens" optional:"true" help:"Cross-harness history budget window in tokens (1024–10000000), overriding the unknown-model 32k assumption; verified smaller model limits still win"`
}

func morphCmd() *cobra.Command {
	return boa.CmdT[morphParams]{
		Use:   "morph",
		Short: "Relaunch an agent in place with a different model, effort, approval posture or harness",
		Long: "Morph changes an agent's launch form within its harness and relaunches it by " +
			"resuming the same conversation. The agent keeps its agent_id, history, inbox, " +
			"groups and permissions, and gets a short inbox note saying what it was and what " +
			"it is now. Sandbox settings, working directory and drive cannot be morphed.\n\n" +
			"With --harness (or a profile for another harness) the agent moves to another " +
			"harness: its conversation is translated into a new native conversation of that " +
			"harness, which it resumes. Reasoning is not carried and tool calls become text " +
			"records; --dry-run shows what would be carried and lost. The full transcript is " +
			"kept under the tclaude data directory. Approval values do not map across " +
			"harnesses, so the new posture must be given; tclaude's own sandbox layer carries " +
			"over, and an agent whose sandbox is its harness's own can only be moved by the " +
			"operator.\n\n" +
			"By default the morph applies at once when the agent is fully idle (or offline), " +
			"and otherwise waits until it next goes idle (at most 30 minutes). --now stops a " +
			"busy or stuck agent immediately. A self-morph always waits for the current turn " +
			"to end.\n\n" +
			"Permissions: self.morph to morph yourself; agent.morph, or groups.members.morph " +
			"(conferred by group ownership) covering every group of the target, to morph " +
			"another agent. A grant scoped to spawn_profile allows only --profile morphs into " +
			"those profiles. The operator needs no grant.",
		ParamEnrich: common.DefaultParamEnricher(),
		InitFuncCtx: func(ctx *boa.HookContext, p *morphParams, _ *cobra.Command) error {
			boa.GetParamT(ctx, &p.Agent).SetAlternativesFunc(completeConvSelectors)
			return nil
		},
		RunFunc: func(p *morphParams, _ *cobra.Command, _ []string) {
			os.Exit(runMorph(p, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

type morphFormView struct {
	Harness           string `json:"harness"`
	Model             string `json:"model,omitempty"`
	Effort            string `json:"effort,omitempty"`
	Approval          string `json:"approval,omitempty"`
	Tools             string `json:"tools,omitempty"`
	AskTimeout        string `json:"ask_timeout,omitempty"`
	AutoCompactWindow string `json:"auto_compact_window,omitempty"`
	Profile           string `json:"profile,omitempty"`
	Sandbox           string `json:"sandbox,omitempty"`
}

func (v morphFormView) String() string {
	parts := []string{v.Harness}
	model := v.Model
	if model == "" {
		model = "default model"
	}
	parts = append(parts, model)
	for _, kv := range [][2]string{
		{"effort", v.Effort}, {"approval", v.Approval}, {"tools", v.Tools},
		{"ask timeout", v.AskTimeout}, {"auto-compact", v.AutoCompactWindow}, {"profile", v.Profile},
		{"harness sandbox", v.Sandbox},
	} {
		if kv[1] != "" {
			parts = append(parts, kv[0]+" "+kv[1])
		}
	}
	return strings.Join(parts, " · ")
}

type morphResult struct {
	Agent        string                 `json:"agent"`
	AgentID      string                 `json:"agent_id,omitempty"`
	ConvID       string                 `json:"conv_id,omitempty"`
	Result       string                 `json:"result"`
	Before       *morphFormView         `json:"before,omitempty"`
	After        *morphFormView         `json:"after,omitempty"`
	Detail       string                 `json:"detail,omitempty"`
	Error        string                 `json:"error,omitempty"`
	Authority    string                 `json:"authority,omitempty"`
	PendingMorph *db.AgentPendingMorph  `json:"pending_morph,omitempty"`
	History      *transcript.LossReport `json:"history,omitempty"`
	Sandbox      *morphSandbox          `json:"sandbox,omitempty"`
}

// morphSandbox is how the agent is confined after a cross-harness morph:
// tclaude's own sandbox and the harness's, named separately.
type morphSandbox struct {
	Implementation string `json:"implementation"`
	Tclaude        string `json:"tclaude"`
	Harness        string `json:"harness"`
	Unsandboxed    bool   `json:"unsandboxed,omitempty"`
	Warning        string `json:"warning,omitempty"`
}

func (p *morphParams) body() map[string]any {
	body := map[string]any{}
	set := func(key, v string) {
		if v = strings.TrimSpace(v); v != "" {
			body[key] = v
		}
	}
	set("profile", p.Profile)
	set("harness", p.Harness)
	set("history", p.History)
	if p.ProceedWithoutSummary {
		body["proceed_without_summary"] = true
	}
	if p.ConfirmHistoryLoss {
		body["confirm_history_loss"] = true
	}
	if p.HistoryWindowTokens != 0 {
		body["history_window_tokens"] = p.HistoryWindowTokens
	}
	set("model", p.Model)
	set("effort", p.Effort)
	set("approval", p.Approval)
	set("tools", p.Tools)
	set("ask_timeout", p.AskTimeout)
	set("auto_compact_window", p.AutoCompactWindow)
	for key, on := range map[string]bool{"trust_dir": p.TrustDir, "now": p.Now, "back": p.Back, "cancel": p.Cancel, "dry_run": p.DryRun} {
		if on {
			body[key] = true
		}
	}
	return body
}

func runMorph(p *morphParams, stdout, stderr io.Writer) int {
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	targets := []string{strings.TrimSpace(p.Agent)}
	if group, ok := strings.CutPrefix(targets[0], "group:"); ok {
		group = strings.TrimSpace(group)
		if group == "" {
			fmt.Fprintln(stderr, "Error: group:<name> needs a group name")
			return rcInvalidArg
		}
		var peers []*peerEntry
		if err := DaemonGet("/v1/peers?group="+url.QueryEscape(group), &peers); err != nil {
			fmt.Fprintf(stderr, "Error: %v\n", err)
			return MapDaemonErrorToRC(err)
		}
		targets = targets[:0]
		for _, pe := range peers {
			if pe.AgentID != "" {
				targets = append(targets, pe.AgentID)
			}
		}
		if len(targets) == 0 {
			fmt.Fprintf(stderr, "Error: group %s has no other agents to morph\n", group)
			return rcNotFound
		}
	}
	body := p.body()
	results := make([]morphResult, 0, len(targets))
	rc := rcOK
	for _, target := range targets {
		path := "/v1/whoami/morph"
		label := "self"
		if target != "" {
			path = "/v1/agent/" + url.PathEscape(target) + "/morph"
			label = target
		}
		res := morphResult{Agent: label}
		if err := DaemonRequest(http.MethodPost, path, body, &res, DaemonOpts{Timeout: 3 * time.Minute}); err != nil {
			res.Result, res.Error = "refused", err.Error()
			rc = MapDaemonErrorToRC(err)
		}
		res.Agent = label
		results = append(results, res)
	}
	if p.JSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if len(targets) == 1 && !strings.HasPrefix(strings.TrimSpace(p.Agent), "group:") {
			_ = enc.Encode(results[0])
		} else {
			_ = enc.Encode(results)
		}
		return rc
	}
	for _, res := range results {
		name := res.Agent
		if res.AgentID != "" {
			name = shortAgentID(res.AgentID, res.ConvID)
		}
		if res.Error != "" {
			fmt.Fprintf(stderr, "%s: refused: %s\n", name, res.Error)
			continue
		}
		fmt.Fprintf(stdout, "%s: %s\n", name, res.Result)
		if res.Before != nil {
			fmt.Fprintf(stdout, "  was: %s\n", res.Before)
		}
		if res.After != nil && res.Result != "cancelled" {
			fmt.Fprintf(stdout, "  now: %s\n", res.After)
		}
		if res.Detail != "" {
			fmt.Fprintf(stdout, "  %s\n", res.Detail)
		}
		if res.History != nil {
			printMorphHistory(stdout, res.History)
		}
		if sb := res.Sandbox; sb != nil && res.Result != "cancelled" {
			fmt.Fprintf(stdout, "  tclaude sandbox: %s\n", sb.Tclaude)
			fmt.Fprintf(stdout, "  harness sandbox: %s\n", sb.Harness)
			if sb.Warning != "" {
				fmt.Fprintf(stdout, "  warning: %s\n", sb.Warning)
			}
		}
		if res.Result == "dry_run" && res.Authority != "" {
			fmt.Fprintf(stdout, "  allowed by: %s\n", res.Authority)
		}
	}
	return rc
}

func printMorphHistory(w io.Writer, r *transcript.LossReport) {
	rung := map[string]string{
		"native":       "installed as a native conversation",
		"context_file": "saved to files the agent is told to read",
		"handoff":      "the agent writes a handoff for its successor first (tclaude agent morph-handoff)",
	}[r.Fallback]
	if rung == "" {
		rung = r.Fallback
	}
	fmt.Fprintf(w, "  history: %d of %d entries carried, %s\n", r.Carried, r.SourceEntries, rung)
	if r.ContextWindowTokens > 0 {
		fmt.Fprintf(w, "  history window: %d tokens (%s), budget %d\n", r.ContextWindowTokens, r.ContextWindowSource, r.BudgetTokens)
	}
	if r.CarriedEstimatedTokens > 0 {
		fmt.Fprintf(w, "  estimated history tokens: %d source → %d carried\n", r.SourceEstimatedTokens, r.CarriedEstimatedTokens)
	}
	if r.BudgetWarning {
		fmt.Fprintf(w, "  history cut: %d%% kept, %d%% omitted from inline history; %s\n", r.KeptPercent, r.DroppedPercent, r.SelectionReason)
	}
	if r.NeedsSummary {
		fmt.Fprintln(w, "  confirm with --confirm-history-loss; the source writes a summary before departure")
	}
	for _, line := range r.Lines() {
		fmt.Fprintf(w, "    %s\n", line)
	}
	if r.TranscriptPath != "" {
		fmt.Fprintf(w, "  full transcript: %s\n", r.TranscriptPath)
	}
}

// pendingMorphLine renders a pending morph for agent ls / whoami.
func pendingMorphLine(pm *db.AgentPendingMorph) string {
	if pm == nil {
		return ""
	}
	var parts []string
	if pm.Target.Harness != "" {
		parts = append(parts, "harness "+pm.Target.Harness)
	}
	add := func(k string, v *string) {
		if v != nil && *v != "" {
			parts = append(parts, k+" "+*v)
		}
	}
	model := pm.Target.Model
	if model != nil && *model != "" && pm.Target.ContextWindowSize != nil && *pm.Target.ContextWindowSize == 1_000_000 {
		m := *model + "[1m]"
		model = &m
	}
	add("model", model)
	add("effort", pm.Target.Effort)
	add("approval", pm.Target.Approval)
	add("tools", pm.Target.Tools)
	add("ask timeout", pm.Target.AskTimeout)
	add("auto-compact", pm.Target.AutoCompactWindow)
	if pm.Target.Profile != "" {
		parts = append(parts, "profile "+pm.Target.Profile)
	}
	what := strings.Join(parts, " · ")
	if pm.Back {
		what = "back to " + what
	}
	if pm.History == "handoff" {
		if pm.Handoff == nil || strings.TrimSpace(pm.Handoff.Body) == "" {
			what += ", awaiting the agent's handoff"
		} else {
			what += ", handoff submitted"
		}
	}
	return fmt.Sprintf("pending morph -> %s (by %s, expires %s)", what, pm.Actor, pm.ExpiresAt.Local().Format("15:04"))
}

type morphHandoffParams struct {
	Handoff string `pos:"true" optional:"true" help:"The handoff text. Omit it and use --file for anything long"`
	File    string `long:"file" short:"f" optional:"true" help:"Read the handoff from this file ('-' reads stdin). Mutually exclusive with the positional argument"`
}

func morphHandoffCmd() *cobra.Command {
	return boa.CmdT[morphHandoffParams]{
		Use:   "morph-handoff",
		Short: "Submit the handoff a pending cross-harness morph of you is waiting for",
		Long: "When a cross-harness morph of you is requested with --history handoff (or your " +
			"history cannot be translated), tclaude asks you to write a handoff for the agent " +
			"that continues your work in the new harness. Submit it with this command, then end " +
			"your turn; the morph applies once you are idle. Submitting again replaces the " +
			"earlier handoff. Keep it under 64 KiB and point at files for detail.",
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(p *morphHandoffParams, _ *cobra.Command, _ []string) {
			os.Exit(runMorphHandoff(p, os.Stdin, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

func runMorphHandoff(p *morphHandoffParams, stdin io.Reader, stdout, stderr io.Writer) int {
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	body, rc := resolveBodyInput(p.Handoff, p.File, "the handoff argument", stdin, stderr)
	if rc != rcOK {
		return rc
	}
	if strings.TrimSpace(body) == "" {
		fmt.Fprintln(stderr, "Error: the handoff is empty")
		return rcInvalidArg
	}
	var resp struct {
		Detail string `json:"detail"`
	}
	if err := DaemonRequest(http.MethodPost, "/v1/whoami/morph/handoff", map[string]any{"body": body}, &resp,
		DaemonOpts{Timeout: 30 * time.Second}); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
	}
	fmt.Fprintln(stdout, resp.Detail)
	return rcOK
}
