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
	"github.com/tofutools/tclaude/pkg/common"
)

type morphParams struct {
	Agent             string `pos:"true" optional:"true" help:"Agent to morph: title, agent_id, conv-id or prefix, or group:<name> for every other member of a group. Default: yourself"`
	Profile           string `long:"profile" short:"p" optional:"true" help:"Take the new launch form from this spawn profile (same harness only; its sandbox fields are not applied). Explicit flags override it"`
	Model             string `long:"model" optional:"true" help:"New model (validated for the agent's harness)"`
	Effort            string `long:"effort" optional:"true" help:"New reasoning effort (per-harness; Gemini has none)"`
	Approval          string `long:"ask-for-approval" optional:"true" help:"New launch approval posture (per-harness values, as for spawn). An agent caller may not grant a posture broader than its own"`
	Tools             string `long:"tools" optional:"true" help:"New OpenCode tool governance: allow | ask | deny"`
	AskTimeout        string `long:"ask-user-question-timeout" optional:"true" help:"New Claude Code AskUserQuestion idle-timeout: inherit | never | 60s | 5m | 10m"`
	AutoCompactWindow string `long:"auto-compact-window" optional:"true" help:"New Claude Code auto-compact window in tokens"`
	Now               bool   `long:"now" help:"Stop and relaunch immediately even if the agent is busy or stuck, resuming from its last durable history. Not available for a self-morph"`
	Back              bool   `long:"back" help:"Morph back to the form the last morph replaced"`
	Cancel            bool   `long:"cancel" help:"Cancel a pending morph"`
	DryRun            bool   `long:"dry-run" help:"Show the before -> after form and whether you may apply it, without changing anything"`
	JSON              bool   `long:"json" help:"Print the per-agent results as JSON"`
}

func morphCmd() *cobra.Command {
	return boa.CmdT[morphParams]{
		Use:   "morph",
		Short: "Relaunch an agent in place with a different model, effort or approval posture",
		Long: "Morph changes an agent's launch form within its harness and relaunches it by " +
			"resuming the same conversation. The agent keeps its agent_id, history, inbox, " +
			"groups and permissions, and gets a short inbox note saying what it was and what " +
			"it is now. Sandbox settings, working directory, harness and drive cannot be " +
			"morphed.\n\n" +
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
	} {
		if kv[1] != "" {
			parts = append(parts, kv[0]+" "+kv[1])
		}
	}
	return strings.Join(parts, " · ")
}

type morphResult struct {
	Agent        string                `json:"agent"`
	AgentID      string                `json:"agent_id,omitempty"`
	ConvID       string                `json:"conv_id,omitempty"`
	Result       string                `json:"result"`
	Before       *morphFormView        `json:"before,omitempty"`
	After        *morphFormView        `json:"after,omitempty"`
	Detail       string                `json:"detail,omitempty"`
	Error        string                `json:"error,omitempty"`
	Authority    string                `json:"authority,omitempty"`
	PendingMorph *db.AgentPendingMorph `json:"pending_morph,omitempty"`
}

func (p *morphParams) body() map[string]any {
	body := map[string]any{}
	set := func(key, v string) {
		if v = strings.TrimSpace(v); v != "" {
			body[key] = v
		}
	}
	set("profile", p.Profile)
	set("model", p.Model)
	set("effort", p.Effort)
	set("approval", p.Approval)
	set("tools", p.Tools)
	set("ask_timeout", p.AskTimeout)
	set("auto_compact_window", p.AutoCompactWindow)
	for key, on := range map[string]bool{"now": p.Now, "back": p.Back, "cancel": p.Cancel, "dry_run": p.DryRun} {
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
		if res.Result == "dry_run" && res.Authority != "" {
			fmt.Fprintf(stdout, "  allowed by: %s\n", res.Authority)
		}
	}
	return rc
}

// pendingMorphLine renders a pending morph for agent ls / whoami.
func pendingMorphLine(pm *db.AgentPendingMorph) string {
	if pm == nil {
		return ""
	}
	var parts []string
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
	return fmt.Sprintf("pending morph -> %s (by %s, expires %s)", what, pm.Actor, pm.ExpiresAt.Local().Format("15:04"))
}
