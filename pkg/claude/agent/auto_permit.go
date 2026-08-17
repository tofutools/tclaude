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

// auto_permit.go is `tclaude agent auto-permit {ls,on,off,log}` — the operator's
// consent switch for the handful of permission prompts a harness reserves for a
// human keystroke.
//
// The motivating case is Claude Code's EnterWorktree safety check: when the
// target worktree lives outside the directory Claude Code manages itself, the
// confirmation is a hardcoded gate that ignores allow-rules, the auto-mode
// classifier and PreToolUse hook approvals alike. An operator who is happy for
// that to happen unattended has no configuration anywhere to say so — and the
// agent simply stalls. Opting in tells tclaude agentd it may press the accept
// key for that ONE named prompt, and every press it makes is recorded (`log`,
// and the dashboard's Audit tab).
//
// This is deliberately not a blanket accept-everything mode; the condition list
// is compile-time and narrow. `--dangerously-skip-permissions` already covers
// the blanket case and is the honest way to ask for it.
//
// Gated on `self.auto-permit` for one's own opt-ins — NOT default-granted and
// NOT implied by group ownership, so it takes an explicit grant or a per-call
// --ask-human approval. `--target <selector>` acts on ANOTHER agent via the
// usual manager pattern (`agent.auto-permit`, or owning a group containing the
// target).
func autoPermitCmd() *cobra.Command {
	return boa.CmdT[struct{}]{
		Use:   "auto-permit",
		Short: "Manage which named permission prompts are auto-answered for an agent",
		Long: "Opt an agent in or out of having tclaude agentd answer a NAMED permission prompt " +
			"on the operator's behalf — for prompts a harness reserves for a human keystroke and " +
			"no allow-rule, auto-mode setting or PreToolUse hook can pre-approve (Claude Code's " +
			"EnterWorktree safety check is the motivating case).\n\n" +
			"Off by default for every agent, one narrow named condition at a time — this is not a " +
			"blanket accept-everything mode; use `--dangerously-skip-permissions` if that is what " +
			"you want. Every auto-answer is recorded: see `auto-permit log` or the dashboard's " +
			"Audit tab.\n\n" +
			"Gated on `self.auto-permit`, which is NOT granted by default and NOT implied by group " +
			"ownership; pass `--ask-human <timeout>` for a one-off approval popup. Use " +
			"--target <selector> to act on ANOTHER agent — requires `agent.auto-permit`, or being " +
			"an owner of a group containing the target.",
		ParamEnrich: common.DefaultParamEnricher(),
		SubCmds: []*cobra.Command{
			autoPermitLsCmd(),
			autoPermitOnCmd(),
			autoPermitOffCmd(),
			autoPermitLogCmd(),
		},
	}.ToCobra()
}

// autoPermitCondition is one entry of the daemon's condition registry as it
// appears on the wire, with this agent's opt-in state folded in.
type autoPermitCondition struct {
	Name      string `json:"name"`
	Summary   string `json:"summary"`
	Harness   string `json:"harness"`
	Enabled   bool   `json:"enabled"`
	GrantedBy string `json:"granted_by,omitempty"`
	GrantedAt string `json:"granted_at,omitempty"`
}

// autoPermitResp is the shared wire shape of the opt-in endpoints.
type autoPermitResp struct {
	ConvID            string                `json:"conv_id"`
	Conditions        []autoPermitCondition `json:"conditions"`
	UnknownConditions []string              `json:"unknown_conditions,omitempty"`
	CallerConv        string                `json:"caller_conv,omitempty"`
	CallerAgentID     string                `json:"caller_agent_id,omitempty"`
}

// --- auto-permit ls ---

type autoPermitLsParams struct {
	Target string `long:"target" optional:"true" help:"Show ANOTHER agent's opt-ins instead of self. Selector: title, full conv-id, or 8+-char prefix. Requires agent.auto-permit, or owning a group containing the target."`
}

func autoPermitLsCmd() *cobra.Command {
	return boa.CmdT[autoPermitLsParams]{
		Use:         "ls",
		Short:       "List the auto-permit conditions and which are enabled",
		ParamEnrich: common.DefaultParamEnricher(),
		InitFuncCtx: func(ctx *boa.HookContext, p *autoPermitLsParams, _ *cobra.Command) error {
			boa.GetParamT(ctx, &p.Target).SetAlternativesFunc(completeConvSelectors)
			return nil
		},
		RunFunc: func(p *autoPermitLsParams, _ *cobra.Command, _ []string) {
			os.Exit(runAutoPermitLs(p, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

func runAutoPermitLs(p *autoPermitLsParams, stdout, stderr io.Writer) int {
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	var resp autoPermitResp
	if err := DaemonGet(autoPermitPath(p.Target), &resp); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
	}
	printAutoPermitConditions(stdout, &resp)
	return rcOK
}

func printAutoPermitConditions(stdout io.Writer, resp *autoPermitResp) {
	fmt.Fprintf(stdout, "Auto-permit conditions for %s:\n", short(resp.ConvID))
	for _, c := range resp.Conditions {
		state := "off"
		if c.Enabled {
			state = "ON"
			if c.GrantedBy != "" {
				state += " (by " + c.GrantedBy + ")"
			}
		}
		fmt.Fprintf(stdout, "  %-18s %-24s %s [%s]\n", c.Name, state, c.Summary, c.Harness)
	}
	if len(resp.Conditions) == 0 {
		fmt.Fprintln(stdout, "  (this build registers no auto-permit conditions)")
	}
	for _, name := range resp.UnknownConditions {
		fmt.Fprintf(stdout, "  %-18s ON but UNKNOWN to this build — inert; turn it off with `auto-permit off %s`\n",
			name, name)
	}
}

// --- auto-permit on / off ---

type autoPermitSetParams struct {
	Condition string `pos:"true" help:"Condition name to enable/disable, e.g. enter-worktree. Run 'auto-permit ls' for the registry."`
	Target    string `long:"target" optional:"true" help:"Act on ANOTHER agent instead of self. Requires agent.auto-permit, or owning a group containing the target."`
	AskHuman  string `long:"ask-human" optional:"true" help:"On permission denial, ask the human via popup with this timeout (e.g. '30s'). Capped at 300s. Timeout = deny. Self-target only."`
}

func autoPermitOnCmd() *cobra.Command {
	return boa.CmdT[autoPermitSetParams]{
		Use:   "on",
		Short: "Consent to a named permission prompt being answered automatically",
		Long: "Records the operator's standing consent for ONE named prompt condition. " +
			"From then on tclaude agentd presses the accept key when it sees that exact dialog " +
			"on the agent's pane — and only that dialog: the pane is read immediately before the " +
			"keystroke, so a different prompt is never answered. Every press is recorded.",
		ParamEnrich: common.DefaultParamEnricher(),
		InitFuncCtx: func(ctx *boa.HookContext, p *autoPermitSetParams, _ *cobra.Command) error {
			boa.GetParamT(ctx, &p.Target).SetAlternativesFunc(completeConvSelectors)
			boa.GetParamT(ctx, &p.AskHuman).SetAlternativesFunc(completeAskHumanDurations)
			return nil
		},
		RunFunc: func(p *autoPermitSetParams, _ *cobra.Command, _ []string) {
			os.Exit(runAutoPermitSet(p, true, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

func autoPermitOffCmd() *cobra.Command {
	return boa.CmdT[autoPermitSetParams]{
		Use:         "off",
		Short:       "Withdraw consent for a named permission prompt",
		ParamEnrich: common.DefaultParamEnricher(),
		InitFuncCtx: func(ctx *boa.HookContext, p *autoPermitSetParams, _ *cobra.Command) error {
			boa.GetParamT(ctx, &p.Target).SetAlternativesFunc(completeConvSelectors)
			boa.GetParamT(ctx, &p.AskHuman).SetAlternativesFunc(completeAskHumanDurations)
			return nil
		},
		RunFunc: func(p *autoPermitSetParams, _ *cobra.Command, _ []string) {
			os.Exit(runAutoPermitSet(p, false, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

func runAutoPermitSet(p *autoPermitSetParams, enabled bool, stdout, stderr io.Writer) int {
	condition := strings.TrimSpace(p.Condition)
	if condition == "" {
		fmt.Fprintln(stderr, "Error: a condition name is required (see `tclaude agent auto-permit ls`).")
		return rcInvalidArg
	}
	target := strings.TrimSpace(p.Target)
	if target != "" && p.AskHuman != "" {
		fmt.Fprintln(stderr, "Error: --ask-human is only supported when targeting self; cross-agent calls require an explicit slug grant or group ownership.")
		return rcInvalidArg
	}
	ask, err := ParseAskHuman(p.AskHuman)
	if err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return rcInvalidArg
	}
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	if ask > 0 {
		fmt.Fprintf(stdout, "Waiting up to %s for human approval...\n", ask)
	}
	var resp autoPermitResp
	body := map[string]any{"condition": condition, "enabled": enabled}
	if err := DaemonRequest(http.MethodPost, autoPermitPath(target), body, &resp,
		DaemonOpts{AskHuman: ask}); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
	}
	verb := "Enabled"
	if !enabled {
		verb = "Disabled"
	}
	by := ""
	if resp.CallerConv != "" {
		by = " (by " + shortAgentID(resp.CallerAgentID, resp.CallerConv) + ")"
	}
	fmt.Fprintf(stdout, "%s auto-permit condition %q for %s%s\n", verb, condition, short(resp.ConvID), by)
	return rcOK
}

// --- auto-permit log ---

type autoPermitLogParams struct {
	Target string `long:"target" optional:"true" help:"Only show answers for this agent (title, full conv-id, or 8+-char prefix). Default: every agent."`
	Limit  int    `long:"limit" optional:"true" default:"20" help:"How many recent answers to show (max 200)."`
}

func autoPermitLogCmd() *cobra.Command {
	return boa.CmdT[autoPermitLogParams]{
		Use:   "log",
		Short: "Show the permission prompts that were auto-answered",
		Long: "Lists what tclaude agentd approved on the operator's behalf, newest first. " +
			"The same records appear in the dashboard's Audit tab (source: auto-permit).",
		ParamEnrich: common.DefaultParamEnricher(),
		InitFuncCtx: func(ctx *boa.HookContext, p *autoPermitLogParams, _ *cobra.Command) error {
			boa.GetParamT(ctx, &p.Target).SetAlternativesFunc(completeConvSelectors)
			return nil
		},
		RunFunc: func(p *autoPermitLogParams, _ *cobra.Command, _ []string) {
			os.Exit(runAutoPermitLog(p, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

type autoPermitAnswer struct {
	At         string `json:"at"`
	ConvID     string `json:"conv_id"`
	AgentLabel string `json:"agent_label"`
	Detail     string `json:"detail"`
}

func runAutoPermitLog(p *autoPermitLogParams, stdout, stderr io.Writer) int {
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	path := "/v1/auto-permit/log?limit=" + url.QueryEscape(fmt.Sprint(p.Limit))
	if t := strings.TrimSpace(p.Target); t != "" {
		// The target is resolved daemon-side against the audit rows, so a
		// title or conv-id prefix both work here.
		path += "&conv=" + url.QueryEscape(t)
	}
	var resp struct {
		Answers []autoPermitAnswer `json:"answers"`
	}
	if err := DaemonGet(path, &resp); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
	}
	if len(resp.Answers) == 0 {
		fmt.Fprintln(stdout, "No prompts have been auto-answered.")
		return rcOK
	}
	for _, a := range resp.Answers {
		label := a.AgentLabel
		if label == "" {
			label = short(a.ConvID)
		}
		fmt.Fprintf(stdout, "%s  %s  %s\n", a.At, label, a.Detail)
	}
	return rcOK
}

// --- shared helpers ---

// autoPermitPath returns the read/write endpoint for the given target (self when
// empty).
func autoPermitPath(target string) string {
	if t := strings.TrimSpace(target); t != "" {
		return "/v1/agent/" + url.PathEscape(t) + "/auto-permit"
	}
	return "/v1/whoami/auto-permit"
}
