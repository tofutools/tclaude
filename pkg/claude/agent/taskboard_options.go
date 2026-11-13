package agent

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"text/tabwriter"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/claude/common/boardopts"
	"github.com/tofutools/tclaude/pkg/common"
)

// Board options (docs/taskboard.md "Board options"): `taskboard options`
// shows and changes how one group's board behaves. A board stores only what
// it sets; the rest is inherited from the global taskboard.* config or the
// built-in default, and `show` says which for every value.

type taskboardOptionsResp struct {
	Group    string             `json:"group"`
	Options  boardopts.Options  `json:"options"`
	Resolved boardopts.Resolved `json:"resolved"`
	Globals  struct {
		IdleCheck *bool  `json:"idle_check,omitempty"`
		IdleGrace string `json:"idle_grace,omitempty"`
	} `json:"globals"`
	Version   int64    `json:"version"`
	UpdatedBy string   `json:"updated_by,omitempty"`
	UpdatedAt string   `json:"updated_at,omitempty"`
	Keys      []string `json:"keys"`
}

func taskboardOptionsPath(group string) string {
	return "/v1/groups/" + url.PathEscape(group) + "/taskboard/options"
}

const taskboardOptionsLong = "A board's options set how it behaves. They are stored with the board, so they work the same " +
	"on a linked node's board (--peer). A board stores only what it sets; every other value is inherited from the " +
	"global config (taskboard.idle_check, taskboard.idle_grace in ~/.tclaude/data/config.json) or the built-in " +
	"default, and `show` says where each value comes from.\n\n" +
	"The idle check (on by default): with it on, an assigned, started task whose agent sits idle without reporting " +
	"for the grace period is marked stalled, the assignee is nudged and the board tells who it is set to tell. A queued " +
	"task (waiting on a dependency that is not done, or in line for a full lane), a blocked task and a task in a custom column never stall, unless the column is " +
	"listed in idle.check_columns. taskboard.idle_grace \"0\" turns the check off on every board.\n\n" +
	"  idle.enabled        on | off                       (default on, or taskboard.idle_check)\n" +
	"  idle.grace          a duration, 1m to 168h         (default taskboard.idle_grace, else 5m)\n" +
	"  idle.nudge          on | off: nudge the assignee   (default on)\n" +
	"  idle.tell           assigner, owners, operator     (default assigner; none for no one)\n" +
	"  idle.check_columns  custom column slugs            (default none)\n\n" +
	"The automatic messages: every event's message is on by default and goes where it always has. Each has\n" +
	"  events.<event>.enabled  on | off\n" +
	"  events.<event>.to       any of assignee, assigner, creator, owners, operator; none for no one\n" +
	"The events: assigned, started, unassigned, reopened, moved_away, comment, report_done, report_failed,\n" +
	"unblocked, epic_close, limit_reached. Whoever caused an event never hears of it, and nobody hears twice.\n\n" +
	"Work-in-progress limits: none by default. A lane is In progress or a custom column:\n" +
	"  limits.in_progress.max          how many tasks In progress holds  (none or 0 for no limit)\n" +
	"  limits.columns.<column>.max     how many tasks the column holds\n" +
	"  limits.at_limit                 queue | refuse | warn            (default queue)\n" +
	"  limits.in_progress.at_limit     the same, for In progress only\n" +
	"  limits.columns.<column>.at_limit\n" +
	"  limits.per                      board | assignee: count the whole lane, or each assignee's tasks in it\n" +
	"At a full lane, queue keeps the task out of it, in line (one for In progress is held in Next) until a slot\n" +
	"frees; refuse refuses the change; warn lets it in. The operator goes over a limit with --over-limit.\n\n" +
	"Reading them needs taskboard.options.read, or taskboard.options.own.read, which every member holds on its own group's " +
	"board; changing them needs taskboard.options.write (owners hold it, and taskboard.manage implies it), or\n" +
	"taskboard.options.own.write on your own group's board. taskboard.read / taskboard.write cover the tasks, not the options."

func taskboardOptionsCmd() *cobra.Command {
	return boa.CmdT[struct{}]{
		Use:         "options",
		Short:       "Show and change a board's options (idle check, messages, limits)",
		Long:        taskboardOptionsLong,
		ParamEnrich: common.DefaultParamEnricher(),
		SubCmds: []*cobra.Command{
			taskboardOptionsShowCmd(),
			taskboardOptionsSetCmd(),
			taskboardOptionsResetCmd(),
		},
	}.ToCobra()
}

// runTaskboardOptions sends one options request and prints the options.
func runTaskboardOptions(peer, method, path string, body any, askHuman string, asJSON bool, verb string, stdout, stderr io.Writer) int {
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	ask, rc := parseAskHumanFlag(askHuman, stdout, stderr)
	if rc != rcOK {
		return rc
	}
	var resp taskboardOptionsResp
	if err := remoteRequestOpts(peer, method, path, body, &resp, DaemonOpts{AskHuman: ask}); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
	}
	if asJSON {
		return writeJSONTo(stdout, resp)
	}
	if verb != "" {
		fmt.Fprintln(stdout, verb)
	}
	printTaskboardOptions(stdout, resp)
	return rcOK
}

func printTaskboardOptions(w io.Writer, resp taskboardOptionsResp) {
	idle := resp.Resolved.Idle
	state := "off"
	switch {
	case idle.GloballyOff:
		state = "off on every board (taskboard.idle_grace is 0)"
	case idle.Active:
		state = "on"
	}
	header := resp.Group + " board options"
	if resp.Version > 0 {
		header += fmt.Sprintf(" (version %d", resp.Version)
		if resp.UpdatedBy != "" {
			header += ", changed by " + resp.UpdatedBy
		}
		if resp.UpdatedAt != "" {
			header += " " + resp.UpdatedAt
		}
		header += ")"
	} else {
		header += " (all inherited)"
	}
	fmt.Fprintln(w, header)
	fmt.Fprintf(w, "idle check: %s\n", state)
	tw := tabwriter.NewWriter(w, 2, 4, 2, ' ', 0)
	onOff := map[bool]string{true: "on", false: "off"}
	list := func(l []string) string {
		if len(l) == 0 {
			return "none"
		}
		return strings.Join(l, ",")
	}
	from := func(source, global string) string {
		switch {
		case source == boardopts.SourceBoard && global != "":
			return "set on this board (global: " + global + ")"
		case source == boardopts.SourceBoard:
			return "set on this board"
		case source == boardopts.SourceGlobal:
			return "global"
		}
		return "default"
	}
	globalCheck := ""
	if resp.Globals.IdleCheck != nil {
		globalCheck = onOff[*resp.Globals.IdleCheck]
	}
	fmt.Fprintf(tw, "  idle.enabled\t%s\t%s\n", onOff[idle.Enabled.Value], from(idle.Enabled.Source, globalCheck))
	fmt.Fprintf(tw, "  idle.grace\t%s\t%s\n", idle.Grace.Value, from(idle.Grace.Source, resp.Globals.IdleGrace))
	fmt.Fprintf(tw, "  idle.nudge\t%s\t%s\n", onOff[idle.Nudge.Value], from(idle.Nudge.Source, ""))
	fmt.Fprintf(tw, "  idle.tell\t%s\t%s\n", list(idle.Tell.Value), from(idle.Tell.Source, ""))
	fmt.Fprintf(tw, "  idle.check_columns\t%s\t%s\n", list(idle.CheckColumns.Value), from(idle.CheckColumns.Source, ""))
	_ = tw.Flush()
	if len(resp.Resolved.Events) == 0 {
		return
	}
	fmt.Fprintln(w, "messages:")
	tw = tabwriter.NewWriter(w, 2, 4, 2, ' ', 0)
	for _, e := range resp.Resolved.Events {
		source := e.Enabled.Source
		if e.To.Source == boardopts.SourceBoard {
			source = boardopts.SourceBoard
		}
		fmt.Fprintf(tw, "  events.%s\t%s\t%s\t%s\n", e.Name, onOff[e.Enabled.Value], list(e.To.Value), from(source, ""))
	}
	_ = tw.Flush()
	limits := resp.Resolved.Limits
	if len(limits.Lanes) == 0 {
		return
	}
	fmt.Fprintln(w, "limits:")
	tw = tabwriter.NewWriter(w, 2, 4, 2, ' ', 0)
	fmt.Fprintf(tw, "  limits.at_limit\t%s\t%s\n", limits.AtLimit.Value, from(limits.AtLimit.Source, ""))
	fmt.Fprintf(tw, "  limits.per\t%s\t%s\n", limits.Per.Value, from(limits.Per.Source, ""))
	for _, l := range limits.Lanes {
		key := "limits.columns." + l.Lane
		if l.Lane == boardopts.LaneInProgress {
			key = "limits." + l.Lane
		}
		most := "none"
		if l.Max.Value > 0 {
			most = fmt.Sprint(l.Max.Value)
		}
		source := l.Max.Source
		if l.AtLimit.Source == boardopts.SourceBoard && limits.AtLimit.Source != boardopts.SourceBoard {
			source = boardopts.SourceBoard
		}
		fmt.Fprintf(tw, "  %s\tmax %s\t%s\t%s\n", key, most, l.AtLimit.Value, from(source, ""))
	}
	_ = tw.Flush()
}

type taskboardOptionsShowParams struct {
	Group string `pos:"true" help:"Group whose board's options to show"`
	Peer  string `long:"peer" optional:"true" help:"A linked node's board instead, under the grants it gave this node (taskboard.options.read)"`
	JSON  bool   `long:"json" help:"Output JSON: what the board sets (options), what is in force (resolved) and the version"`
}

func taskboardOptionsShowCmd() *cobra.Command {
	return boa.CmdT[taskboardOptionsShowParams]{
		Use:         "show",
		Short:       "Show a board's options, each with where it comes from",
		ParamEnrich: common.DefaultParamEnricher(),
		InitFuncCtx: func(ctx *boa.HookContext, p *taskboardOptionsShowParams, _ *cobra.Command) error {
			boa.GetParamT(ctx, &p.Group).SetAlternativesFunc(completeGroupNames)
			return nil
		},
		RunFunc: func(p *taskboardOptionsShowParams, _ *cobra.Command, _ []string) {
			taskboardExit(runTaskboardOptions(p.Peer, http.MethodGet, taskboardOptionsPath(p.Group), nil, "", p.JSON, "",
				cliStdout, cliStderr))
		},
	}.ToCobra()
}

type taskboardOptionsSetParams struct {
	Group    string   `pos:"true" help:"Group whose board to change"`
	Settings []string `pos:"true" help:"key=value pairs, e.g. idle.enabled=on idle.grace=10m idle.tell=assigner,owners (value default: inherit again)"`
	Peer     string   `long:"peer" optional:"true" help:"A linked node's board instead, under the grants it gave this node (taskboard.options.write)"`
	AskHuman string   `long:"ask-human" optional:"true" help:"On permission denial, ask the human via popup with this timeout (e.g. '30s'). Capped at 300s. Timeout = deny."`
	JSON     bool     `long:"json" help:"Output JSON"`
}

func taskboardOptionsSetCmd() *cobra.Command {
	return boa.CmdT[taskboardOptionsSetParams]{
		Use:         "set",
		Short:       "Set board options: key=value … (needs taskboard.options.write)",
		Long:        "Set one or more options in one change, e.g. idle.enabled=on events.comment.enabled=off events.report_failed.to=assigner,owners. The keys:\n\n" + strings.Join(boardopts.Keys, "\n"),
		ParamEnrich: common.DefaultParamEnricher(),
		InitFuncCtx: func(ctx *boa.HookContext, p *taskboardOptionsSetParams, _ *cobra.Command) error {
			boa.GetParamT(ctx, &p.Group).SetAlternativesFunc(completeGroupNames)
			boa.GetParamT(ctx, &p.AskHuman).SetAlternativesFunc(completeAskHumanDurations)
			return nil
		},
		RunFunc: func(p *taskboardOptionsSetParams, _ *cobra.Command, args []string) {
			// The raw arguments, not p.Settings: a list value such as
			// idle.tell=assigner,owners keeps its commas.
			pairs := p.Settings
			if len(args) > 1 {
				pairs = args[1:]
			}
			set := map[string]string{}
			for _, kv := range pairs {
				k, v, ok := strings.Cut(kv, "=")
				if !ok || strings.TrimSpace(k) == "" {
					fmt.Fprintf(cliStderr, "Error: %q is not key=value, e.g. idle.enabled=on (the keys: tclaude agent taskboard options set --help)\n", kv)
					taskboardExit(rcInvalidArg)
					return
				}
				set[strings.TrimSpace(k)] = v
			}
			taskboardExit(runTaskboardOptions(p.Peer, http.MethodPatch, taskboardOptionsPath(p.Group), map[string]any{"set": set},
				p.AskHuman, p.JSON, "", cliStdout, cliStderr))
		},
	}.ToCobra()
}

type taskboardOptionsResetParams struct {
	Group    string   `pos:"true" help:"Group whose board to change"`
	Keys     []string `pos:"true" help:"Options to inherit again: keys, an event (events.comment), a section (idle, events) or all"`
	Peer     string   `long:"peer" optional:"true" help:"A linked node's board instead, under the grants it gave this node (taskboard.options.write)"`
	AskHuman string   `long:"ask-human" optional:"true" help:"On permission denial, ask the human via popup with this timeout (e.g. '30s'). Capped at 300s. Timeout = deny."`
	JSON     bool     `long:"json" help:"Output JSON"`
}

func taskboardOptionsResetCmd() *cobra.Command {
	return boa.CmdT[taskboardOptionsResetParams]{
		Use:         "reset",
		Short:       "Drop board options so they are inherited again (needs taskboard.options.write)",
		ParamEnrich: common.DefaultParamEnricher(),
		InitFuncCtx: func(ctx *boa.HookContext, p *taskboardOptionsResetParams, _ *cobra.Command) error {
			boa.GetParamT(ctx, &p.Group).SetAlternativesFunc(completeGroupNames)
			boa.GetParamT(ctx, &p.AskHuman).SetAlternativesFunc(completeAskHumanDurations)
			return nil
		},
		RunFunc: func(p *taskboardOptionsResetParams, _ *cobra.Command, _ []string) {
			taskboardExit(runTaskboardOptions(p.Peer, http.MethodPatch, taskboardOptionsPath(p.Group), map[string]any{"reset": p.Keys},
				p.AskHuman, p.JSON, "", cliStdout, cliStderr))
		},
	}.ToCobra()
}
