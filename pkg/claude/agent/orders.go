package agent

import (
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/standingorders"
	"github.com/tofutools/tclaude/pkg/common"
)

// `tclaude agent orders` — trigger-driven standing orders (TCL-841).
//
// READ-ONLY, deliberately. There are no add/rm/enable/disable verbs here, and
// their absence is the design rather than an unfinished edge.
//
// An order's text becomes high-authority, model-visible context, so ACTIVATING
// one is an authority question and not a convenience question. A CLI verb
// writing the database directly would answer it with ambient process state:
// these commands run inside the agent's own pane, so any agent could invoke
// them, and the only identity available to attribute the write is
// CurrentConvID — which resolves through TCLAUDE_SESSION_ID, documented at the
// launch seam as caller-controlled compatibility state. An agent that simply
// unset it would have its own order recorded as operator-authored. Attribution
// is not authorization in any case: knowing who wrote a row says nothing about
// whether they may target that group.
//
// Mutation therefore belongs behind agentd, reusing cron's owner /
// group-owner / permission checks, where the caller is resolved from recorded
// host pids rather than from anything the caller can set. That route, the
// propose-vs-activate split, and the agent-facing skill are TCL-844's subject.
// Until it exists, orders are seeded by tests and direct database writes, and
// this command inspects them.
//
// `explain` is the verb that matters most here. Whether standing orders get
// trusted comes down to whether the operator can ask "why did this fire, or
// why didn't it" and get a straight answer, so it is a first-class dry-run
// over the real evaluator rather than a description of it.

func ordersCmd() *cobra.Command {
	return boa.CmdT[struct{}]{
		Use:   "orders",
		Short: "Inspect trigger-driven standing orders",
		Long: "List, show, and dry-run standing orders: durable guidance delivered when a trigger matches " +
			"rather than on a wall clock. Supported triggers are session.start, user.prompt, tool.before, " +
			"and tool.after, with optional validated RE2 matching over normalized event fields. " +
			"An order declares the delivery timing it REQUIRES; a harness that cannot meet " +
			"it reports unsupported rather than downgrading silently. An optional per-agent cooldown limits " +
			"successful deliveries without depending on conversation generation; optional trailing-edge " +
			"debounce coalesces matching bursts into one queued next-turn reminder.",
		ParamEnrich: common.DefaultParamEnricher(),
		SubCmds: []*cobra.Command{
			ordersLsCmd(),
			ordersShowCmd(),
			ordersExplainCmd(),
		},
	}.ToCobra()
}

// ---- ls ----

func ordersLsCmd() *cobra.Command {
	return boa.CmdT[struct{}]{
		Use:         "ls",
		Short:       "List standing orders",
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(_ *struct{}, _ *cobra.Command, _ []string) {
			os.Exit(runOrdersLs(os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

func runOrdersLs(stdout, stderr io.Writer) int {
	orders, err := db.ListStandingOrders()
	if err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return rcIOFailure
	}
	if len(orders) == 0 {
		fmt.Fprintln(stdout, "(no standing orders)")
		return rcOK
	}
	fmt.Fprintf(stdout, "%-4s  %-16s  %-8s  %-30s  %-18s  %-20s  %s\n",
		"ID", "NAME", "ENABLED", "TRIGGER", "TIMING", "TARGET", "LAST")
	fmt.Fprintln(stdout, strings.Repeat("─", 122))
	for _, o := range orders {
		enabled := "yes"
		if !o.Enabled {
			enabled = "no"
			if o.DisabledReason != "" {
				enabled = "no*"
			}
		}
		last := "(never)"
		if rec, err := db.LatestStandingDelivery(o.ID); err == nil && rec != nil {
			last = rec.Outcome
		}
		fmt.Fprintf(stdout, "%-4d  %-16s  %-8s  %-30s  %-18s  %-20s  %s\n",
			o.ID, truncate(o.Name, 16), enabled, truncate(o.TriggerLabel(), 30),
			o.Timing, truncate(ordersTargetLabel(o), 20), last)
	}
	fmt.Fprintln(stdout, "\n* auto-paused by tclaude (see the reason in `orders show`), not disabled by hand.")
	return rcOK
}

func ordersTargetLabel(o *db.StandingOrder) string {
	if o.IsGlobalTarget() {
		return "global"
	}
	var labels []string
	if o.IsGroupTarget() {
		label := "group:#" + strconv.FormatInt(o.GroupID, 10)
		if g, err := db.GetAgentGroupByID(o.GroupID); err == nil && g != nil {
			label = "group:" + g.Name
		}
		if o.TargetRole != "" {
			label += "/" + o.TargetRole
		}
		labels = append(labels, label)
	} else if o.TargetAgent != "" {
		labels = append(labels, shortID(o.TargetAgent))
	} else {
		labels = append(labels, "(unresolved stable agent)")
	}
	for _, groupID := range o.AdditionalGroupIDs {
		label := "group:#" + strconv.FormatInt(groupID, 10)
		if g, err := db.GetAgentGroupByID(groupID); err == nil && g != nil {
			label = "group:" + g.Name
		}
		labels = append(labels, label)
	}
	return strings.Join(labels, " + ")
}

func shortID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

// ---- show ----

type ordersShowParams struct {
	Name string `pos:"true" help:"Order name or numeric id (from 'tclaude agent orders ls')."`
}

func ordersShowCmd() *cobra.Command {
	return boa.CmdT[ordersShowParams]{
		Use:         "show",
		Short:       "Show one standing order in full, with its capability matrix and recent deliveries",
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(p *ordersShowParams, _ *cobra.Command, _ []string) {
			os.Exit(runOrdersShow(os.Stdout, os.Stderr, p.Name))
		},
	}.ToCobra()
}

func runOrdersShow(stdout, stderr io.Writer, selector string) int {
	o, rc := resolveOrder(stderr, selector)
	if rc != rcOK {
		return rc
	}

	author := standingorders.AuthorLabel(o)
	fmt.Fprintf(stdout, "Order:     %s (id %d, revision %d)\n", o.Name, o.ID, o.Revision)
	fmt.Fprintf(stdout, "Author:    %s\n", author)
	fmt.Fprintf(stdout, "Enabled:   %v", o.Enabled)
	if o.DisabledReason != "" {
		fmt.Fprintf(stdout, " (auto-paused: %s)", o.DisabledReason)
	}
	fmt.Fprintf(stdout, "\nTarget:    %s\n", ordersTargetLabel(o))
	fmt.Fprintf(stdout, "Trigger:   %s\n", o.TriggerLabel())
	fmt.Fprintf(stdout, "Timing:    %s (required — no silent downgrade)\n", o.Timing)
	fmt.Fprintf(stdout, "Cadence:   %s\n", o.Cadence)
	fmt.Fprintf(stdout, "Cooldown:  %s\n", ordersCooldownLabel(o.CooldownSeconds))
	fmt.Fprintf(stdout, "Debounce:  %s\n", ordersDebounceLabel(o.DebounceSeconds))
	fmt.Fprintf(stdout, "\nText delivered to the agent:\n  %s\n", o.Summary)

	fmt.Fprintln(stdout, "\nPer-harness capability:")
	byH := standingorders.CapabilityByHarnessForOrder(o)
	for _, h := range standingorders.KnownHarnesses {
		c := byH[h]
		line := fmt.Sprintf("  %-10s %-12s via %s", h, c.Status, c.Transport)
		if c.Detail != "" {
			line += "\n             " + c.Detail
		}
		fmt.Fprintln(stdout, line)
	}

	recs, err := db.ListStandingDeliveries(o.ID, 10)
	if err != nil {
		fmt.Fprintf(stderr, "Error reading deliveries: %v\n", err)
		return rcIOFailure
	}
	fmt.Fprintln(stdout, "\nRecent evaluations:")
	if len(recs) == 0 {
		fmt.Fprintln(stdout, "  (none recorded)")
		return rcOK
	}
	for _, r := range recs {
		fmt.Fprintf(stdout, "  %s  %-22s  %-13s  %s\n",
			r.CreatedAt.Format("2006-01-02 15:04:05"), r.Outcome, r.Harness, r.Detail)
	}
	return rcOK
}

func ordersCooldownLabel(seconds int64) string {
	if seconds <= 0 {
		return "off"
	}
	return (time.Duration(seconds) * time.Second).String() + " per stable recipient agent"
}

func ordersDebounceLabel(seconds int64) string {
	if seconds <= 0 {
		return "off"
	}
	return (time.Duration(seconds) * time.Second).String() +
		" trailing edge (queued next-turn message)"
}

// ---- explain ----

type ordersExplainParams struct {
	Event   string `long:"event" optional:"true" default:"session.start" help:"Trigger event to simulate: session.start | user.prompt | tool.before | tool.after."`
	Source  string `long:"source" optional:"true" default:"startup" help:"Event source: startup | resume | clear | compact."`
	Conv    string `long:"conv" optional:"true" help:"Conversation to evaluate as. Defaults to the current one."`
	Harness string `long:"harness" optional:"true" default:"claude" help:"Harness to evaluate as: claude | codex | opencode. Decides which timing guarantees are available."`
	Trimmed bool   `long:"trimmed" optional:"true" help:"Simulate a hook payload whose tool fields were dropped for size, to see which orders could not be evaluated at all."`
	Cwd     string `long:"cwd" optional:"true" help:"Working directory matcher input."`
	Prompt  string `long:"prompt" optional:"true" help:"Prompt matcher input for user.prompt."`
	Tool    string `long:"tool" optional:"true" help:"Tool-name matcher input for tool.before/tool.after."`
	Input   string `long:"input" optional:"true" help:"Compact JSON tool-input matcher value for tool.before/tool.after."`
}

func ordersExplainCmd() *cobra.Command {
	return boa.CmdT[ordersExplainParams]{
		Use:   "explain",
		Short: "Dry-run every standing order against a hypothetical event and say why each would or would not fire",
		Long: "Runs the real evaluator — the same code the hook path uses — against a simulated event, without " +
			"delivering anything or writing to the ledger. Distinguishes a clean non-match from an order that " +
			"could not be evaluated (trimmed payload), one whose timing this harness cannot honour, and one " +
			"suppressed by its cadence.",
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(p *ordersExplainParams, _ *cobra.Command, _ []string) {
			os.Exit(runOrdersExplain(os.Stdout, os.Stderr, p))
		},
	}.ToCobra()
}

func runOrdersExplain(stdout, stderr io.Writer, p *ordersExplainParams) int {
	convID := strings.TrimSpace(p.Conv)
	if convID == "" {
		got, err := CurrentConvID()
		if err != nil {
			fmt.Fprintf(stderr, "Error: no conversation given and the current one could not be resolved: %v\n", err)
			return rcInvalidArg
		}
		convID = got
	}

	ev := standingorders.Event{
		Event:          p.Event,
		Source:         standingorders.NormalizeSource(p.Event, p.Source),
		ConvID:         convID,
		Harness:        p.Harness,
		PayloadTrimmed: p.Trimmed,
		OccurredAt:     time.Now(),
		Cwd:            strings.TrimSpace(p.Cwd),
		Prompt:         p.Prompt,
		ToolName:       standingorders.NormalizeToolName(p.Tool),
		ToolInput:      standingorders.NormalizeToolInput([]byte(p.Input)),
	}

	agentID, err := db.AgentIDForConv(convID)
	if err != nil {
		fmt.Fprintf(stderr, "Error resolving agent for %s: %v\n", convID, err)
		return rcIOFailure
	}
	ev.AgentID = agentID
	if agentID != "" {
		groups, err := db.ListGroupsForAgent(agentID)
		if err != nil {
			fmt.Fprintf(stderr, "Error reading groups: %v\n", err)
			return rcIOFailure
		}
		for _, g := range groups {
			ev.Memberships = append(ev.Memberships, standingorders.Membership{
				GroupID: g.ID,
				Role:    ordersRoleInGroup(g.ID, convID),
			})
		}
	}

	// Same filter the hot path applies, so a dry run cannot promise a delivery
	// the real path would never attempt.
	orders, err := db.ListStandingOrdersForExplain()
	if err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return rcIOFailure
	}
	if len(orders) == 0 {
		fmt.Fprintln(stdout, "(no standing orders)")
		return rcOK
	}

	fmt.Fprintf(stdout, "Simulating %s(source=%s) for conv %s as harness %q",
		ev.Event, ev.Source, shortID(convID), ev.Harness)
	if ev.PayloadTrimmed {
		fmt.Fprint(stdout, ", payload trimmed")
	}
	fmt.Fprintln(stdout, "\n(dry run: nothing is delivered and nothing is recorded)")
	fmt.Fprintln(stdout)

	// The real cadence state is consulted so a suppressed order is reported as
	// suppressed — an explain that ignored the ledger would confidently
	// predict a delivery that would not happen.
	decisions := standingorders.EvaluateAll(
		orders, ev, db.StandingOrderDeliveredInEpoch, db.LatestSuccessfulStandingDeliveryAt)
	for _, d := range decisions {
		mark := "—"
		if d.Deliver {
			mark = "→"
		}
		fmt.Fprintf(stdout, "%s %-16s %-22s %s\n", mark, d.Order.Name, d.Outcome, d.Detail)
	}

	text := standingorders.RenderContext(decisions)
	if text == "" {
		fmt.Fprintln(stdout, "\nNothing would be delivered.")
		return rcOK
	}
	fmt.Fprintf(stdout, "\nText that would be delivered:\n%s\n", text)
	return rcOK
}

func ordersRoleInGroup(groupID int64, convID string) string {
	members, err := db.ListAgentGroupMembers(groupID)
	if err != nil {
		return ""
	}
	for _, m := range members {
		if m.ConvID == convID {
			return m.Role
		}
	}
	return ""
}

// resolveOrder accepts a name or a numeric id. A numeric selector is tried as
// a name FIRST, so an order someone named "12" stays addressable.
func resolveOrder(stderr io.Writer, selector string) (*db.StandingOrder, int) {
	selector = strings.TrimSpace(selector)
	if selector == "" {
		fmt.Fprintln(stderr, "Error: an order name or id is required")
		return nil, rcInvalidArg
	}
	o, err := db.GetStandingOrderByName(selector)
	if err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return nil, rcIOFailure
	}
	if o != nil {
		return o, rcOK
	}
	if id, convErr := strconv.ParseInt(selector, 10, 64); convErr == nil {
		o, err = db.GetStandingOrder(id)
		if err != nil {
			fmt.Fprintf(stderr, "Error: %v\n", err)
			return nil, rcIOFailure
		}
		if o != nil {
			return o, rcOK
		}
	}
	fmt.Fprintf(stderr, "Error: no standing order %q\n", selector)
	return nil, rcNotFound
}
