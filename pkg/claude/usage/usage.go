// Package usage exposes daemon-backed quota and cost queries for operators and agents.
package usage

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"text/tabwriter"
	"time"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/common"
)

type Params struct {
	JSON bool `long:"json" help:"Output structured quota observations and forecasts"`
}

func Cmd() *cobra.Command {
	return boa.CmdT[Params]{Use: "usage", Short: "Show cached account quotas and usage forecasts", Long: "Read subscription usage for all observed providers through agentd.\nIncludes sample age, resets, and forecasts; no provider API requests are made.\nAgents require usage.read. --json replaces the old raw Anthropic API format.", ParamEnrich: common.DefaultParamEnricher(), RunFunc: func(p *Params, cmd *cobra.Command, _ []string) {
		if err := runUsage(p, cmd.OutOrStdout()); err != nil {
			fmt.Fprintln(cmd.ErrOrStderr(), "Error:", err)
			os.Exit(1)
		}
	}}.ToCobra()
}

type CostsParams struct {
	JSON      bool   `long:"json" help:"Output compact cost totals and provider summaries as JSON"`
	From      string `long:"from" optional:"true" help:"First local calendar date (YYYY-MM-DD); defaults to month start"`
	To        string `long:"to" optional:"true" help:"Last local calendar date (YYYY-MM-DD); defaults to today"`
	Self      bool   `long:"self" help:"Only the calling agent's spend across linked conversations"`
	Days      bool   `long:"days" help:"Include daily cost totals"`
	Agents    bool   `long:"agents" help:"Include detailed per-day conversation costs and agent attribution"`
	Models    bool   `long:"models" help:"Include cost totals grouped by model"`
	Harnesses bool   `long:"harnesses" help:"Include cost totals grouped by coding harness"`
}

func CostsCmd() *cobra.Command {
	return boa.CmdT[CostsParams]{Use: "costs", Short: "Show recorded API costs and subscription WHAT-IF estimates", Long: "Read raw cost history through agentd (agents require costs.read).\nDefaults to month-to-date, with today's totals and per-provider totals.\nAdd --days, --agents, --models, or --harnesses for optional breakdowns in text or JSON.\nWHAT-IF estimates follow the operator's subscription-cost setting and are shown separately.\nDashboard display multipliers are not applied. --self requires an identified agent.", ParamEnrich: common.DefaultParamEnricher(), RunFunc: func(p *CostsParams, cmd *cobra.Command, _ []string) {
		if err := runCosts(p, cmd.OutOrStdout()); err != nil {
			fmt.Fprintln(cmd.ErrOrStderr(), "Error:", err)
			os.Exit(1)
		}
	}}.ToCobra()
}

// Preserve the complete daemon payload for --json; the text renderer decodes
// only the fields it needs, so additions are immediately available to agents.
func query(path string, asJSON bool, out io.Writer, render func(json.RawMessage, io.Writer) error) error {
	var raw json.RawMessage
	if err := agent.DaemonGet(path, &raw); err != nil {
		return err
	}
	if asJSON {
		var formatted bytes.Buffer
		if err := json.Indent(&formatted, raw, "", "  "); err != nil {
			return err
		}
		formatted.WriteByte('\n')
		_, err := formatted.WriteTo(out)
		return err
	}
	return render(raw, out)
}

func runUsage(p *Params, out io.Writer) error {
	return query("/v1/usage/summary", p.JSON, out, renderUsage)
}

type forecast struct {
	Status string  `json:"status"`
	Rate   float64 `json:"rate_pct_per_hour"`
	HitsAt string  `json:"hits_limit_at"`
}
type usageReadout struct {
	Windows []struct {
		Provider  string              `json:"provider"`
		Name      string              `json:"window_name"`
		Status    string              `json:"status"`
		Pct       float64             `json:"pct"`
		Used      float64             `json:"used_units"`
		Limit     float64             `json:"limit_units"`
		Age       *int64              `json:"age_seconds"`
		ResetsAt  string              `json:"resets_at"`
		Forecasts map[string]forecast `json:"forecasts"`
	} `json:"windows"`
	CoverageWarnings []json.RawMessage `json:"coverage_warnings"`
}

func renderUsage(raw json.RawMessage, out io.Writer) error {
	var data usageReadout
	if err := json.Unmarshal(raw, &data); err != nil {
		return err
	}
	fmt.Fprintln(out, "Account quotas (cached observations, shared by all agents)")
	if len(data.Windows) == 0 {
		fmt.Fprintln(out, "Usage unavailable: no recorded quota observations.")
		return nil
	}
	tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "PROVIDER\tWINDOW\tUSED\tSTATUS\tSAMPLE AGE\tRESETS\tFORECAST (7d span)")
	for _, w := range data.Windows {
		used := fmt.Sprintf("%.1f%%", w.Pct)
		if w.Limit > 0 {
			used += fmt.Sprintf(" (%.2f/%.2f units)", w.Used, w.Limit)
		}
		f := w.Forecasts["span"]
		prediction := f.Status
		if prediction == "" {
			prediction = "unavailable"
		}
		if f.HitsAt != "" {
			prediction += "; limit " + f.HitsAt
		}
		if f.Rate > 0 {
			prediction += fmt.Sprintf("; %.2f pp/h", f.Rate)
		}
		if w.Status != "current" {
			prediction = "unavailable (last observation " + w.Status + ")"
		}
		reset := w.ResetsAt
		if reset == "" {
			reset = "unknown"
		}
		age := "unknown"
		if w.Age != nil {
			age = (time.Duration(*w.Age) * time.Second).String()
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", w.Provider, w.Name, used, w.Status, age, reset, prediction)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	if len(data.CoverageWarnings) > 0 {
		fmt.Fprintln(out, "Coverage warning: some OpenCode activity lacks recent native quota observations; see --json.")
	}
	return nil
}

func runCosts(p *CostsParams, out io.Writer) error {
	q := url.Values{}
	for name, value := range map[string]string{"from": p.From, "to": p.To} {
		if value != "" {
			if _, err := time.Parse("2006-01-02", value); err != nil {
				return fmt.Errorf("bad %s date, want YYYY-MM-DD", name)
			}
			q.Set(name, value)
		}
	}
	if p.From != "" && p.To != "" && p.From > p.To {
		return fmt.Errorf("from must be on or before to")
	}
	if p.Self {
		q.Set("self", "true")
	}
	for name, include := range map[string]bool{"days": p.Days, "agents": p.Agents, "models": p.Models, "harnesses": p.Harnesses} {
		if include {
			q.Set(name, "true")
		}
	}
	path := "/v1/costs"
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	return query(path, p.JSON, out, renderCosts)
}

type costSummary struct {
	Name   string  `json:"name"`
	Real   float64 `json:"real_total_usd"`
	WhatIf float64 `json:"what_if_total_usd"`
}

type costReadout struct {
	From          string        `json:"from"`
	To            string        `json:"to"`
	Timezone      string        `json:"timezone"`
	Scope         string        `json:"scope"`
	Real          float64       `json:"real_total_usd"`
	WhatIf        float64       `json:"what_if_total_usd"`
	TodayReal     float64       `json:"today_real_usd"`
	TodayWhatIf   float64       `json:"today_what_if_usd"`
	WhatIfEnabled bool          `json:"what_if_enabled"`
	Providers     []costSummary `json:"providers"`
	Models        []costSummary `json:"models"`
	Harnesses     []costSummary `json:"harnesses"`
	Days          []struct {
		Day    string  `json:"day"`
		Real   float64 `json:"real_cost_usd"`
		WhatIf float64 `json:"what_if_cost_usd"`
	} `json:"days"`
	Agents []struct {
		AgentID  string  `json:"agent_id"`
		ConvID   string  `json:"conv_id"`
		Title    string  `json:"title"`
		Day      string  `json:"day"`
		Provider string  `json:"provider"`
		Model    string  `json:"model"`
		Real     float64 `json:"real_cost_usd"`
		WhatIf   float64 `json:"what_if_cost_usd"`
	} `json:"agents"`
}

func renderCosts(raw json.RawMessage, out io.Writer) error {
	var data costReadout
	if err := json.Unmarshal(raw, &data); err != nil {
		return err
	}
	fmt.Fprintf(out, "%s costs: %s through %s (%s; raw USD)\n", data.Scope, data.From, data.To, data.Timezone)
	fmt.Fprintf(out, "Recorded API: $%.4f total; $%.4f today within selected range\n", data.Real, data.TodayReal)
	if data.WhatIfEnabled {
		fmt.Fprintf(out, "WHAT-IF subscription estimate: $%.4f total; $%.4f today within selected range\n", data.WhatIf, data.TodayWhatIf)
	} else {
		fmt.Fprintln(out, "WHAT-IF subscription estimates: disabled")
	}
	tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	summary := func(label string, rows []costSummary) {
		fmt.Fprintf(tw, "%s\tRECORDED USD\tWHAT-IF USD\n", label)
		for _, row := range rows {
			fmt.Fprintf(tw, "%s\t%.4f\t%.4f\n", row.Name, row.Real, row.WhatIf)
		}
	}
	summary("PROVIDER", data.Providers)
	if data.Models != nil {
		fmt.Fprintln(tw)
		summary("MODEL", data.Models)
	}
	if data.Harnesses != nil {
		fmt.Fprintln(tw)
		summary("HARNESS", data.Harnesses)
	}
	if data.Days != nil {
		fmt.Fprintln(tw, "\nDAY\tRECORDED USD\tWHAT-IF USD")
		for _, row := range data.Days {
			fmt.Fprintf(tw, "%s\t%.4f\t%.4f\n", row.Day, row.Real, row.WhatIf)
		}
	}
	if data.Agents != nil {
		if err := tw.Flush(); err != nil {
			return err
		}
		tw = tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "\nAGENT / CONVERSATION\tTITLE\tDAY\tPROVIDER\tMODEL\tRECORDED USD\tWHAT-IF USD")
		for _, row := range data.Agents {
			id := row.AgentID
			if id == "" {
				id = row.ConvID
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%.4f\t%.4f\n", id, row.Title, row.Day, row.Provider, row.Model, row.Real, row.WhatIf)
		}
	}
	return tw.Flush()
}
