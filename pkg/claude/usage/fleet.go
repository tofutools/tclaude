package usage

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"

	"github.com/tofutools/tclaude/pkg/claude/common/usagerules"
	"github.com/tofutools/tclaude/pkg/common"
)

// fleet.go — `tclaude usage fleet`: fleet usage limits (spend summed across
// trusted peers that share usage.read) and what each peer shares.

type fleetReadout struct {
	Limits []struct {
		ID          int64           `json:"id"`
		Target      string          `json:"target"`
		TargetLabel string          `json:"target_label"`
		IncludeSelf bool            `json:"include_self"`
		Enabled     bool            `json:"enabled"`
		Rule        usagerules.Rule `json:"rule"`
	} `json:"limits"`
	Statuses []struct {
		status
		FleetID int64 `json:"fleet_id"`
		Counted int   `json:"counted"`
		Members []struct {
			Label   string  `json:"label"`
			Value   float64 `json:"value"`
			State   string  `json:"state"`
			Enforce string  `json:"enforce"`
			SendErr string  `json:"send_error"`
		} `json:"members"`
	} `json:"statuses"`
	Nodes []struct {
		Label   string   `json:"label"`
		Pools   []string `json:"pools"`
		Error   string   `json:"error"`
		Summary *struct {
			Grants []string                      `json:"grants"`
			Totals map[string]map[string]float64 `json:"totals"`
			Limits []status                      `json:"limits"`
		} `json:"summary"`
	} `json:"nodes"`
	Controls []struct {
		PeerLabel string  `json:"peer_label"`
		RuleName  string  `json:"rule_name"`
		Fleet     string  `json:"fleet"`
		Pct       float64 `json:"pct"`
		Block     bool    `json:"block"`
		Stop      bool    `json:"stop"`
		Until     string  `json:"until"`
	} `json:"controls"`
}

func renderFleet(raw json.RawMessage, out io.Writer) error {
	var d fleetReadout
	if err := json.Unmarshal(raw, &d); err != nil {
		return err
	}
	fmt.Fprintln(out, "Fleet limits:")
	if len(d.Limits) == 0 {
		fmt.Fprintln(out, "  (none) — add one with: tclaude usage fleet limit add --target all ...")
	}
	for _, l := range d.Limits {
		self := ""
		if l.IncludeSelf {
			self = " + this node"
		}
		state := ""
		if !l.Enabled {
			state = " [disabled]"
		}
		fmt.Fprintf(out, "  #%d %s%s: %s%s\n", l.ID, l.TargetLabel, self, describeRule(l.Rule), state)
		for _, s := range d.Statuses {
			if s.FleetID != l.ID {
				continue
			}
			fmt.Fprintf(out, "      now %s, %d node(s) counted\n", s.reading(), s.Counted)
			for _, m := range s.Members {
				line := fmt.Sprintf("        %-16s %-10s %s", m.Label, m.State, amount(s.Metric, m.Value))
				if m.Enforce != "" && m.Enforce != "self" {
					line += "  enforce: " + strings.ReplaceAll(m.Enforce, "_", " ")
				}
				if m.SendErr != "" {
					line += "  (send: " + m.SendErr + ")"
				}
				fmt.Fprintln(out, line)
			}
		}
	}
	fmt.Fprintln(out, "\nNodes:")
	if len(d.Nodes) == 0 {
		fmt.Fprintln(out, "  (no trusted peers)")
	}
	for _, n := range d.Nodes {
		pools := ""
		if len(n.Pools) > 0 {
			pools = " [" + strings.Join(n.Pools, ", ") + "]"
		}
		switch {
		case n.Summary == nil:
			reason := n.Error
			if reason == "" {
				reason = "not read yet"
			}
			fmt.Fprintf(out, "  %s%s: %s\n", n.Label, pools, reason)
		case n.Summary.Totals == nil:
			fmt.Fprintf(out, "  %s%s: shares no usage with this node (grants it holds there: %s)\n", n.Label, pools, grantList(n.Summary.Grants))
		default:
			t := n.Summary.Totals
			fmt.Fprintf(out, "  %s%s: today %s, this week %s, this month %s — grants: %s\n", n.Label, pools,
				amount("", t["day"]["total_usd"]), amount("", t["week"]["total_usd"]), amount("", t["month"]["total_usd"]), grantList(n.Summary.Grants))
			for _, l := range n.Summary.Limits {
				fmt.Fprintf(out, "      limit %q: %s\n", l.Name, l.reading())
			}
		}
	}
	if len(d.Controls) > 0 {
		fmt.Fprintln(out, "\nPeers' fleet limits in force here:")
		for _, c := range d.Controls {
			var what []string
			if c.Block {
				what = append(what, "blocks spawns")
			}
			if c.Stop {
				what = append(what, "stops agents")
			}
			fmt.Fprintf(out, "  %s %q (%s) at %.0f%%: %s, until %s unless renewed\n", c.PeerLabel, c.RuleName, c.Fleet, c.Pct, strings.Join(what, ", "), c.Until)
		}
	}
	return nil
}

func grantList(grants []string) string {
	if len(grants) == 0 {
		return "none"
	}
	return strings.Join(grants, ", ")
}

type fleetParams struct {
	JSON  bool `long:"json" help:"Output the daemon's JSON"`
	Fresh bool `long:"fresh" help:"Read every peer's summary now instead of reusing one under a minute old"`
}

type fleetLimitAddParams struct {
	Target   string   `long:"target" optional:"true" help:"all (default: every trusted peer sharing usage.read) | pool:<node pool> (a sub-fleet) | peer:<peer label or instance id>"`
	NoSelf   bool     `long:"no-self" help:"Leave this node's own spend out of the sum (and its agents out of the actions)"`
	File     string   `long:"file" optional:"true" help:"Read the rule as JSON from this file ('-' for stdin) instead of the rule flags"`
	Name     string   `long:"name" optional:"true" help:"Rule name, shown in alerts (e.g. 'fleet monthly')"`
	Metric   string   `long:"metric" optional:"true" help:"total_usd (default) | real_usd | whatif_usd"`
	Period   string   `long:"period" optional:"true" help:"day | week | month (default)"`
	Budget   float64  `long:"budget" optional:"true" help:"Budget in USD over the whole fleet"`
	Step     []string `long:"step" optional:"true" help:"Ladder step AT:ACTION[:TARGETS][:every=N%|Nm], repeatable: notify (agent, operator), wrap-up, hard-stop, block-spawn. Agent-directed steps reach this node's agents and those of every member that granted usage-rules.enforce"`
	DryRun   bool     `long:"dry-run" help:"Only tell the operator what each step would have done"`
	Disabled bool     `long:"disabled" help:"Create the limit disabled"`
}

type fleetLimitSetParams struct {
	ID      int64    `pos:"true" help:"Fleet limit id (see 'tclaude usage fleet')"`
	Target  string   `long:"target" optional:"true" help:"Change the target: all | pool:<node pool> | peer:<peer>"`
	Self    bool     `long:"self" help:"Count this node"`
	NoSelf  bool     `long:"no-self" help:"Stop counting this node"`
	File    string   `long:"file" optional:"true" help:"Replace the rule with JSON from this file ('-' for stdin)"`
	Name    string   `long:"name" optional:"true" help:"Replace the rule: its name (with --step and the other rule flags)"`
	Metric  string   `long:"metric" optional:"true" help:"total_usd | real_usd | whatif_usd"`
	Period  string   `long:"period" optional:"true" help:"day | week | month"`
	Budget  float64  `long:"budget" optional:"true" help:"Budget in USD over the whole fleet"`
	Step    []string `long:"step" optional:"true" help:"Ladder step AT:ACTION[:TARGETS][:every=N%|Nm], repeatable"`
	DryRun  bool     `long:"dry-run" help:"Only tell the operator what each step would have done"`
	Enable  bool     `long:"enable" help:"Enable the limit"`
	Disable bool     `long:"disable" help:"Disable the limit"`
}

type fleetIDParams struct {
	ID int64 `pos:"true" help:"Fleet limit id (see 'tclaude usage fleet')"`
}

func fleetCmd() *cobra.Command {
	cmd := boa.CmdT[fleetParams]{Use: "fleet", Short: "Show fleet usage limits (spend summed across trusted peers) and what each peer shares", ParamEnrich: common.DefaultParamEnricher(), RunFunc: func(p *fleetParams, cmd *cobra.Command, _ []string) {
		path := "/v1/usage/fleet"
		if p.Fresh {
			path += "?fresh=1"
		}
		run(cmd, query(path, p.JSON, cmd.OutOrStdout(), renderFleet))
	}}.ToCobra()
	cmd.AddCommand(boa.CmdT[struct{}]{Use: "limit", Short: "Add, change or remove a fleet or sub-fleet (node pool) usage limit", ParamEnrich: common.DefaultParamEnricher(), SubCmds: []*cobra.Command{
		boa.CmdT[fleetLimitAddParams]{Use: "add", Short: "Add a fleet limit, e.g. --target pool:gpu-rigs --name rigs --period month --budget 2000 --step 80:notify:operator --step 100:block-spawn", ParamEnrich: common.DefaultParamEnricher(), RunFunc: func(p *fleetLimitAddParams, cmd *cobra.Command, _ []string) {
			r, err := ruleFlags{File: p.File, Name: p.Name, Metric: p.Metric, Period: p.Period, Budget: p.Budget, Step: p.Step, DryRun: p.DryRun}.rule()
			if err == nil {
				if r.Period == "" {
					r.Period = usagerules.PeriodMonth
				}
				target := p.Target
				if target == "" {
					target = "all"
				}
				err = daemonDo(http.MethodPost, "/v1/usage/fleet-limits", map[string]any{"target": target, "include_self": !p.NoSelf, "rule": r, "enabled": !p.Disabled}, cmd.OutOrStdout())
			}
			run(cmd, err)
		}}.ToCobra(),
		boa.CmdT[fleetLimitSetParams]{Use: "set", Short: "Change a fleet limit: its target, whether it counts this node, its rule (same flags as add), or --enable / --disable it", ParamEnrich: common.DefaultParamEnricher(), RunFunc: func(p *fleetLimitSetParams, cmd *cobra.Command, _ []string) {
			run(cmd, setFleetLimit(p, cmd.OutOrStdout()))
		}}.ToCobra(),
		boa.CmdT[fleetIDParams]{Use: "rm", Short: "Remove a fleet limit (releases any block or stop it holds on peers)", ParamEnrich: common.DefaultParamEnricher(), RunFunc: func(p *fleetIDParams, cmd *cobra.Command, _ []string) {
			run(cmd, daemonDo(http.MethodDelete, fmt.Sprintf("/v1/usage/fleet-limits/%d", p.ID), nil, cmd.OutOrStdout()))
		}}.ToCobra(),
	}}.ToCobra())
	return cmd
}

func setFleetLimit(p *fleetLimitSetParams, out io.Writer) error {
	if p.Enable && p.Disable {
		return errors.New("--enable and --disable are mutually exclusive")
	}
	if p.Self && p.NoSelf {
		return errors.New("--self and --no-self are mutually exclusive")
	}
	body := map[string]any{}
	if p.Enable || p.Disable {
		body["enabled"] = p.Enable
	}
	if p.Self || p.NoSelf {
		body["include_self"] = p.Self
	}
	if p.Target != "" {
		body["target"] = p.Target
	}
	if p.File != "" || p.Name != "" || len(p.Step) > 0 {
		r, err := ruleFlags{File: p.File, Name: p.Name, Metric: p.Metric, Period: p.Period, Budget: p.Budget, Step: p.Step, DryRun: p.DryRun}.rule()
		if err != nil {
			return err
		}
		body["rule"] = r
	} else if p.Budget != 0 || p.Metric != "" || p.Period != "" || p.DryRun {
		return errors.New("set replaces the whole rule: give --name and --step along with the other rule flags, or --file")
	}
	if len(body) == 0 {
		return errors.New("nothing to change: give --target, --self/--no-self, rule flags, --file, --enable or --disable")
	}
	return daemonDo(http.MethodPut, fmt.Sprintf("/v1/usage/fleet-limits/%d", p.ID), body, out)
}
