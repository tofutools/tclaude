package usage

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/usagerules"
	"github.com/tofutools/tclaude/pkg/common"
)

// rules.go — `tclaude usage rules|self|limit|profile|assign|adjust`: the CLI
// over agentd's usage limits and usage profiles. Reads need usage.read;
// changes need usage-rules.manage (effectively human-only).

func run(cmd *cobra.Command, err error) {
	if err != nil {
		fmt.Fprintln(cmd.ErrOrStderr(), "Error:", err)
		os.Exit(1)
	}
}

// RuleCmds are the usage-rule subcommands of `tclaude usage`.
func RuleCmds() []*cobra.Command {
	return []*cobra.Command{rulesCmd(), selfCmd(), limitCmd(), profileCmd(), assignCmd(), adjustCmd()}
}

type jsonParams struct {
	JSON bool `long:"json" help:"Output the daemon's JSON"`
}

func rulesCmd() *cobra.Command {
	return boa.CmdT[jsonParams]{Use: "rules", Short: "Show usage limits, usage profiles, assignments and current readings", ParamEnrich: common.DefaultParamEnricher(), RunFunc: func(p *jsonParams, cmd *cobra.Command, _ []string) {
		run(cmd, query("/v1/usage/rules", p.JSON, cmd.OutOrStdout(), renderRules))
	}}.ToCobra()
}

func selfCmd() *cobra.Command {
	return boa.CmdT[jsonParams]{Use: "self", Short: "Show the calling agent's usage budgets and the limits covering it", ParamEnrich: common.DefaultParamEnricher(), RunFunc: func(p *jsonParams, cmd *cobra.Command, _ []string) {
		run(cmd, query("/v1/usage/self", p.JSON, cmd.OutOrStdout(), renderSelf))
	}}.ToCobra()
}

type status struct {
	Scope      string  `json:"scope"`
	Group      string  `json:"group"`
	AgentID    string  `json:"agent_id"`
	AgentTitle string  `json:"agent_title"`
	Profile    string  `json:"profile"`
	Layer      string  `json:"layer"`
	Paused     string  `json:"paused"`
	Name       string  `json:"name"`
	Metric     string  `json:"metric"`
	Period     string  `json:"period"`
	Value      float64 `json:"value"`
	Budget     float64 `json:"budget"`
	Pct        float64 `json:"pct"`
	DryRun     bool    `json:"dry_run"`
	Reached    string  `json:"reached"`
	RuleKey    string  `json:"rule_key"`
	Next       *struct {
		At     float64 `json:"at"`
		Action string  `json:"action"`
	} `json:"next"`
}

func amount(metric string, v float64) string {
	if metric == string(usagerules.MetricWindowPct) {
		return fmt.Sprintf("%.0f%%", v)
	}
	return fmt.Sprintf("$%.2f", v)
}

func (s status) reading() string {
	out := fmt.Sprintf("%s / %s (%.0f%%)", amount(s.Metric, s.Value), amount(s.Metric, s.Budget), s.Pct)
	if s.Next != nil {
		out += fmt.Sprintf(", next %s at %.0f%%", strings.ReplaceAll(s.Next.Action, "_", "-"), s.Next.At)
	}
	if s.Paused != "" {
		out += " [" + s.Paused + "]"
	}
	if s.DryRun {
		out += " [dry run]"
	}
	return out
}

type rulesReadout struct {
	Profiles []struct {
		ID    int64             `json:"id"`
		Name  string            `json:"name"`
		Descr string            `json:"descr"`
		Rules []usagerules.Rule `json:"rules"`
	} `json:"profiles"`
	Limits []struct {
		ID      int64           `json:"id"`
		Scope   string          `json:"scope"`
		Group   string          `json:"group"`
		Enabled bool            `json:"enabled"`
		Rule    usagerules.Rule `json:"rule"`
	} `json:"limits"`
	Assignments []struct {
		Scope       string `json:"scope"`
		ScopeName   string `json:"scope_name"`
		ScopeID     string `json:"scope_id"`
		ProfileName string `json:"profile_name"`
		Source      string `json:"source"`
	} `json:"assignments"`
	Statuses    []status `json:"statuses"`
	EvaluatedAt string   `json:"evaluated_at"`
}

func describeRule(r usagerules.Rule) string {
	steps := make([]string, len(r.Steps))
	for i, s := range r.Steps {
		steps[i] = usagerules.FormatStep(s)
	}
	what := fmt.Sprintf("%s %s %s", r.Metric, r.Period, amount(string(r.Metric), r.Budget))
	if r.Window != "" {
		what = "window_pct " + r.Window
	}
	if r.DryRun {
		what += " (dry run)"
	}
	return fmt.Sprintf("%q %s: %s", r.Name, what, strings.Join(steps, " "))
}

func renderRules(raw json.RawMessage, out io.Writer) error {
	var data rulesReadout
	if err := json.Unmarshal(raw, &data); err != nil {
		return err
	}
	readings := map[string]status{}
	for _, s := range data.Statuses {
		readings[s.RuleKey+"|"+s.AgentID] = s
	}
	fmt.Fprintln(out, "Limits (one shared counter each)")
	if len(data.Limits) == 0 {
		fmt.Fprintln(out, "  none")
	}
	for _, l := range data.Limits {
		scope := "global"
		if l.Scope == "group" {
			scope = "group " + l.Group
		}
		state := ""
		if !l.Enabled {
			state = " [disabled]"
		}
		fmt.Fprintf(out, "  #%d %s %s%s\n", l.ID, scope, describeRule(l.Rule), state)
		if s, ok := readings[fmt.Sprintf("limit:%d|", l.ID)]; ok {
			fmt.Fprintf(out, "      now %s\n", s.reading())
		}
	}
	fmt.Fprintln(out, "\nUsage profiles (applied to each agent separately)")
	if len(data.Profiles) == 0 {
		fmt.Fprintln(out, "  none")
	}
	for _, p := range data.Profiles {
		fmt.Fprintf(out, "  %s", p.Name)
		if p.Descr != "" {
			fmt.Fprintf(out, " — %s", p.Descr)
		}
		fmt.Fprintln(out)
		for _, r := range p.Rules {
			fmt.Fprintf(out, "      %s\n", describeRule(r))
		}
	}
	fmt.Fprintln(out, "\nAssignments (nearest layer wins: agent, else its groups, else global)")
	if len(data.Assignments) == 0 {
		fmt.Fprintln(out, "  none")
	}
	for _, a := range data.Assignments {
		where := a.Scope
		if a.ScopeName != "" {
			where += " " + a.ScopeName
		} else if a.ScopeID != "" {
			where += " " + a.ScopeID
		}
		if a.Source != "" {
			where += " (from " + a.Source + ")"
		}
		fmt.Fprintf(out, "  %s → %s\n", where, a.ProfileName)
	}
	var agents []status
	for _, s := range data.Statuses {
		if s.Scope == "agent" {
			agents = append(agents, s)
		}
	}
	if len(agents) > 0 {
		fmt.Fprintln(out, "\nAgent budgets")
		tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "AGENT\tPROFILE\tRULE\tREADING")
		for _, s := range agents {
			name := s.AgentTitle
			if name == "" {
				name = s.AgentID
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", name, s.Profile, s.Name, s.reading())
		}
		if err := tw.Flush(); err != nil {
			return err
		}
	}
	if data.EvaluatedAt != "" {
		fmt.Fprintf(out, "\nReadings as of %s (evaluated every few minutes).\n", data.EvaluatedAt)
	}
	return nil
}

func renderSelf(raw json.RawMessage, out io.Writer) error {
	var data struct {
		Layer       string   `json:"layer"`
		Statuses    []status `json:"statuses"`
		Limits      []status `json:"limits"`
		Exempt      bool     `json:"exempt"`
		SnoozeUntil string   `json:"snooze_until"`
		Profiles    []struct {
			Name string `json:"name"`
		} `json:"profiles"`
	}
	if err := json.Unmarshal(raw, &data); err != nil {
		return err
	}
	switch {
	case data.Exempt:
		fmt.Fprintln(out, "You are exempt from usage-profile budgets.")
	case len(data.Profiles) == 0:
		fmt.Fprintln(out, "No usage profile applies to you.")
	default:
		names := make([]string, len(data.Profiles))
		for i, p := range data.Profiles {
			names[i] = p.Name
		}
		fmt.Fprintf(out, "Usage profiles: %s (from the %s layer)\n", strings.Join(names, ", "), data.Layer)
	}
	if data.SnoozeUntil != "" {
		fmt.Fprintf(out, "Budget steps are snoozed until %s.\n", data.SnoozeUntil)
	}
	for _, s := range data.Statuses {
		fmt.Fprintf(out, "  %s (%s %s): %s\n", s.Name, s.Metric, s.Period, s.reading())
	}
	if len(data.Limits) > 0 {
		fmt.Fprintln(out, "Shared limits covering you:")
		for _, s := range data.Limits {
			scope := "global"
			if s.Scope == "group" {
				scope = "group " + s.Group
			}
			fmt.Fprintf(out, "  %s %s: %s\n", scope, s.Name, s.reading())
		}
	}
	return nil
}

// ruleFlags builds one rule from flags or a JSON file. Each command
// declares the flags itself (see ruleFields) and converts with ruleFlags().
type ruleFlags struct {
	File, Name, Metric, Period, Window string
	Budget                             float64
	Step                               []string
	DryRun                             bool
}

func readJSONFile(path string, v any) error {
	var data []byte
	var err error
	if path == "-" {
		data, err = io.ReadAll(os.Stdin)
	} else {
		data, err = os.ReadFile(path)
	}
	if err != nil {
		return err
	}
	return json.Unmarshal(data, v)
}

func (f ruleFlags) rule() (usagerules.Rule, error) {
	var r usagerules.Rule
	if f.File != "" {
		return r, readJSONFile(f.File, &r)
	}
	if f.Name == "" || len(f.Step) == 0 {
		return r, errors.New("give --name and at least one --step, or --file")
	}
	r = usagerules.Rule{Name: f.Name, Metric: usagerules.Metric(f.Metric), Period: usagerules.Period(f.Period), Window: f.Window, Budget: f.Budget, DryRun: f.DryRun}
	for _, spec := range f.Step {
		s, err := usagerules.ParseStep(spec)
		if err != nil {
			return r, err
		}
		r.Steps = append(r.Steps, s)
	}
	return r, nil
}

func daemonDo(method, path string, in any, out io.Writer) error {
	var raw json.RawMessage
	if err := agent.DaemonRequest(method, path, in, &raw, agent.DaemonOpts{}); err != nil {
		return err
	}
	_, err := fmt.Fprintln(out, strings.TrimSpace(string(raw)))
	return err
}

type limitAddParams struct {
	File     string   `long:"file" optional:"true" help:"Read the rule as JSON from this file ('-' for stdin) instead of the rule flags"`
	Name     string   `long:"name" optional:"true" help:"Rule name, shown in alerts (e.g. 'monthly')"`
	Metric   string   `long:"metric" optional:"true" help:"total_usd (default: API spend, else subscription-equivalent) | real_usd | whatif_usd | window_pct (global limits only)"`
	Period   string   `long:"period" optional:"true" help:"day | week | month | lifetime (usage profiles only); window_pct rules use the provider window"`
	Window   string   `long:"window" optional:"true" help:"Provider window for window_pct, e.g. anthropic:five_hour or openai:seven_day"`
	Budget   float64  `long:"budget" optional:"true" help:"Budget in USD (ignored for window_pct)"`
	Step     []string `long:"step" optional:"true" help:"Ladder step AT:ACTION[:TARGETS][:every=N%|Nm], repeatable. Actions: notify (targets agent, operator, agent+operator), wrap-up, hard-stop, block-spawn (limits only). E.g. --step 50:notify:operator --step 100:wrap-up --step 110:hard-stop"`
	DryRun   bool     `long:"dry-run" help:"Only tell the operator what each step would have done"`
	Scope    string   `long:"scope" optional:"true" help:"global (default) | group"`
	Group    string   `long:"group" optional:"true" help:"Group name for a group limit"`
	Disabled bool     `long:"disabled" help:"Create the limit disabled"`
}

type limitSetParams struct {
	ID      int64    `pos:"true" help:"Limit id (see 'tclaude usage rules')"`
	File    string   `long:"file" optional:"true" help:"Read the rule as JSON from this file ('-' for stdin) instead of the rule flags"`
	Name    string   `long:"name" optional:"true" help:"Rule name, shown in alerts (e.g. 'monthly')"`
	Metric  string   `long:"metric" optional:"true" help:"total_usd (default: API spend, else subscription-equivalent) | real_usd | whatif_usd | window_pct (global limits only)"`
	Period  string   `long:"period" optional:"true" help:"day | week | month | lifetime (usage profiles only); window_pct rules use the provider window"`
	Window  string   `long:"window" optional:"true" help:"Provider window for window_pct, e.g. anthropic:five_hour or openai:seven_day"`
	Budget  float64  `long:"budget" optional:"true" help:"Budget in USD (ignored for window_pct)"`
	Step    []string `long:"step" optional:"true" help:"Ladder step AT:ACTION[:TARGETS][:every=N%|Nm], repeatable. Actions: notify (targets agent, operator, agent+operator), wrap-up, hard-stop, block-spawn (limits only). E.g. --step 50:notify:operator --step 100:wrap-up --step 110:hard-stop"`
	DryRun  bool     `long:"dry-run" help:"Only tell the operator what each step would have done"`
	Enable  bool     `long:"enable" help:"Enable the limit"`
	Disable bool     `long:"disable" help:"Disable the limit"`
}

func (p *limitAddParams) flags() ruleFlags {
	return ruleFlags{p.File, p.Name, p.Metric, p.Period, p.Window, p.Budget, p.Step, p.DryRun}
}

func (p *limitSetParams) flags() ruleFlags {
	return ruleFlags{p.File, p.Name, p.Metric, p.Period, p.Window, p.Budget, p.Step, p.DryRun}
}

func (p *profileSetParams) flags() ruleFlags {
	return ruleFlags{p.File, p.Name, p.Metric, p.Period, p.Window, p.Budget, p.Step, p.DryRun}
}

type idParams struct {
	ID int64 `pos:"true" help:"Limit id (see 'tclaude usage rules')"`
}

func limitCmd() *cobra.Command {
	return boa.CmdT[struct{}]{Use: "limit", Short: "Add, change or remove a global or group usage limit (one shared counter)", ParamEnrich: common.DefaultParamEnricher(), SubCmds: []*cobra.Command{
		boa.CmdT[limitAddParams]{Use: "add", Short: "Add a limit, e.g. --name monthly --period month --budget 500 --step 50:notify:operator --step 100:wrap-up", ParamEnrich: common.DefaultParamEnricher(), RunFunc: func(p *limitAddParams, cmd *cobra.Command, _ []string) {
			r, err := p.flags().rule()
			if err == nil {
				scope := p.Scope
				if scope == "" {
					scope = "global"
				}
				enabled := !p.Disabled
				err = daemonDo(http.MethodPost, "/v1/usage/limits", map[string]any{"scope": scope, "group": p.Group, "rule": r, "enabled": enabled}, cmd.OutOrStdout())
			}
			run(cmd, err)
		}}.ToCobra(),
		boa.CmdT[limitSetParams]{Use: "set", Short: "Replace a limit's rule (same flags as add), or only --enable / --disable it", ParamEnrich: common.DefaultParamEnricher(), RunFunc: func(p *limitSetParams, cmd *cobra.Command, _ []string) {
			run(cmd, setLimit(p, cmd.OutOrStdout()))
		}}.ToCobra(),
		boa.CmdT[idParams]{Use: "rm", Short: "Remove a limit", ParamEnrich: common.DefaultParamEnricher(), RunFunc: func(p *idParams, cmd *cobra.Command, _ []string) {
			run(cmd, daemonDo(http.MethodDelete, fmt.Sprintf("/v1/usage/limits/%d", p.ID), nil, cmd.OutOrStdout()))
		}}.ToCobra(),
	}}.ToCobra()
}

func setLimit(p *limitSetParams, out io.Writer) error {
	if p.Enable && p.Disable {
		return errors.New("--enable and --disable are mutually exclusive")
	}
	body := map[string]any{}
	if p.Enable || p.Disable {
		body["enabled"] = p.Enable
	}
	if p.File == "" && p.Name == "" && len(p.Step) == 0 {
		if len(body) == 0 {
			return errors.New("nothing to change: give rule flags, --file, --enable or --disable")
		}
		// Toggle only: resend the current rule unchanged.
		var data rulesReadout
		if err := agent.DaemonGet("/v1/usage/rules", &data); err != nil {
			return err
		}
		found := false
		for _, l := range data.Limits {
			if l.ID == p.ID {
				body["rule"], found = l.Rule, true
			}
		}
		if !found {
			return fmt.Errorf("no usage limit #%d", p.ID)
		}
	} else {
		r, err := p.flags().rule()
		if err != nil {
			return err
		}
		body["rule"] = r
	}
	return daemonDo(http.MethodPut, fmt.Sprintf("/v1/usage/limits/%d", p.ID), body, out)
}

type profileSetParams struct {
	Profile string   `pos:"true" help:"Usage profile name"`
	Descr   string   `long:"descr" optional:"true" help:"Description"`
	File    string   `long:"file" optional:"true" help:"Read the rule as JSON from this file ('-' for stdin) instead of the rule flags"`
	Name    string   `long:"name" optional:"true" help:"Rule name, shown in alerts (e.g. 'monthly')"`
	Metric  string   `long:"metric" optional:"true" help:"total_usd (default: API spend, else subscription-equivalent) | real_usd | whatif_usd | window_pct (global limits only)"`
	Period  string   `long:"period" optional:"true" help:"day | week | month | lifetime (usage profiles only); window_pct rules use the provider window"`
	Window  string   `long:"window" optional:"true" help:"Provider window for window_pct, e.g. anthropic:five_hour or openai:seven_day"`
	Budget  float64  `long:"budget" optional:"true" help:"Budget in USD (ignored for window_pct)"`
	Step    []string `long:"step" optional:"true" help:"Ladder step AT:ACTION[:TARGETS][:every=N%|Nm], repeatable. Actions: notify (targets agent, operator, agent+operator), wrap-up, hard-stop, block-spawn (limits only). E.g. --step 50:notify:operator --step 100:wrap-up --step 110:hard-stop"`
	DryRun  bool     `long:"dry-run" help:"Only tell the operator what each step would have done"`
}

type nameParams struct {
	Name string `pos:"true" help:"Usage profile name"`
}

func profileCmd() *cobra.Command {
	return boa.CmdT[struct{}]{Use: "profile", Short: "Create, replace or remove a usage profile (per-agent budgets)", ParamEnrich: common.DefaultParamEnricher(), SubCmds: []*cobra.Command{
		boa.CmdT[profileSetParams]{Use: "set", Short: "Create or replace a usage profile: rules from --file (a JSON array of rules), or one rule from the flags, e.g. --name budget --period lifetime --budget 20 --step 80:notify:agent+operator --step 100:wrap-up --step 120:hard-stop", ParamEnrich: common.DefaultParamEnricher(), RunFunc: func(p *profileSetParams, cmd *cobra.Command, _ []string) {
			run(cmd, setProfile(p, cmd.OutOrStdout()))
		}}.ToCobra(),
		boa.CmdT[nameParams]{Use: "rm", Short: "Remove a usage profile and its assignments", ParamEnrich: common.DefaultParamEnricher(), RunFunc: func(p *nameParams, cmd *cobra.Command, _ []string) {
			run(cmd, daemonDo(http.MethodDelete, "/v1/usage/profiles/"+url.PathEscape(p.Name), nil, cmd.OutOrStdout()))
		}}.ToCobra(),
	}}.ToCobra()
}

func setProfile(p *profileSetParams, out io.Writer) error {
	var rules []usagerules.Rule
	if p.File != "" { // a profile file holds a JSON array of rules
		if err := readJSONFile(p.File, &rules); err != nil {
			return err
		}
	} else {
		r, err := p.flags().rule()
		if err != nil {
			return err
		}
		rules = []usagerules.Rule{r}
	}
	body := map[string]any{"name": p.Profile, "descr": p.Descr, "rules": rules}
	var data rulesReadout
	if err := agent.DaemonGet("/v1/usage/rules", &data); err != nil {
		return err
	}
	for _, existing := range data.Profiles {
		if strings.EqualFold(existing.Name, p.Profile) {
			return daemonDo(http.MethodPut, "/v1/usage/profiles/"+url.PathEscape(existing.Name), body, out)
		}
	}
	return daemonDo(http.MethodPost, "/v1/usage/profiles", body, out)
}

type assignParams struct {
	Scope   string   `long:"scope" help:"global | group | agent | spawn_profile"`
	Target  string   `long:"target" optional:"true" help:"Group name, agent selector, or spawn profile name (not for global)"`
	Profile []string `long:"profile" optional:"true" help:"Usage profile to apply at this layer; repeatable. None clears the layer, so the next layer out applies"`
}

func assignCmd() *cobra.Command {
	return boa.CmdT[assignParams]{Use: "assign", Short: "Set which usage profiles apply at a layer (global default, a group, an agent, or a spawn profile)", ParamEnrich: common.DefaultParamEnricher(), RunFunc: func(p *assignParams, cmd *cobra.Command, _ []string) {
		profiles := p.Profile
		if profiles == nil {
			profiles = []string{}
		}
		run(cmd, daemonDo(http.MethodPut, "/v1/usage/assignments", map[string]any{"scope": p.Scope, "target": p.Target, "profiles": profiles}, cmd.OutOrStdout()))
	}}.ToCobra()
}

type adjustParams struct {
	Agent    string  `pos:"true" help:"Agent selector"`
	Exempt   bool    `long:"exempt" help:"Exempt the agent from its usage profiles"`
	NoExempt bool    `long:"no-exempt" help:"End the exemption"`
	Snooze   int     `long:"snooze" optional:"true" help:"Hold the agent's budget steps for this many minutes; 0 ends a snooze" default:"-1"`
	Rule     string  `long:"rule" optional:"true" help:"Rule key for --extra (e.g. profile:3:budget, see 'tclaude usage rules --json')"`
	Extra    float64 `long:"extra" optional:"true" help:"Total extra budget (USD) on --rule for this agent; 0 removes it" default:"-1"`
}

func adjustCmd() *cobra.Command {
	return boa.CmdT[adjustParams]{Use: "adjust", Short: "Adjust one agent's budget: --extra on a rule, --snooze, or --exempt", ParamEnrich: common.DefaultParamEnricher(), RunFunc: func(p *adjustParams, cmd *cobra.Command, _ []string) {
		body := map[string]any{}
		if p.Exempt && p.NoExempt {
			run(cmd, errors.New("--exempt and --no-exempt are mutually exclusive"))
		}
		if p.Exempt || p.NoExempt {
			body["exempt"] = p.Exempt
		}
		if p.Snooze >= 0 {
			body["snooze_minutes"] = p.Snooze
		}
		if p.Extra >= 0 {
			body["rule_key"], body["extra_budget"] = p.Rule, p.Extra
		}
		if len(body) == 0 {
			run(cmd, errors.New("nothing to change: give --extra with --rule, --snooze, --exempt or --no-exempt"))
		}
		run(cmd, daemonDo(http.MethodPost, "/v1/usage/agents/"+url.PathEscape(p.Agent)+"/adjust", body, cmd.OutOrStdout()))
	}}.ToCobra()
}
