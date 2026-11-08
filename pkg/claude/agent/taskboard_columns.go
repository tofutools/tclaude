package agent

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/common"
)

// Custom stage columns (tcl-plby5h, docs/taskboard.md): `taskboard columns`
// lists, adds, renames, reorders and removes a group's own columns, such as
// "In review" or "In CI". A task goes in one with `taskboard move <task>
// --stage <column>`.

type taskboardColumn struct {
	Slug  string `json:"slug"`
	Label string `json:"label"`
}

type taskboardColumnsResp struct {
	Group   string            `json:"group"`
	Columns []taskboardColumn `json:"columns"`
	Cap     int               `json:"cap,omitempty"`
	Moved   []string          `json:"moved,omitempty"`
}

// taskboardColumnsText reads a board's custom columns: "In review
// [in-review], In CI [in-ci]".
func taskboardColumnsText(cols []taskboardColumn) string {
	parts := make([]string, 0, len(cols))
	for _, c := range cols {
		parts = append(parts, c.Label+" ["+c.Slug+"]")
	}
	return strings.Join(parts, ", ")
}

func taskboardColumnsPath(group, slug string) string {
	p := "/v1/groups/" + url.PathEscape(group) + "/taskboard/columns"
	if slug != "" {
		p += "/" + url.PathEscape(slug)
	}
	return p
}

func taskboardColumnsCmd() *cobra.Command {
	return boa.CmdT[struct{}]{
		Use:   "columns",
		Short: "List and manage a group's custom stage columns (In review, In CI, …)",
		Long: "A group's board may have up to 4 custom columns of its own, such as \"In review\" or \"In CI\". " +
			"They sit between In progress and Blocked, in the group's order; the built-in columns (Ideas, Next, " +
			"In progress, Blocked, Done) are fixed. A task stands in a custom column with " +
			"`taskboard move <task> --stage <column>` and keeps its assignee; a task in a custom column never stalls. " +
			"`--stage next` takes it out again. Adding, renaming, reordering and removing columns needs " +
			"taskboard.manage on the group (owners hold it); listing them needs taskboard.read.",
		ParamEnrich: common.DefaultParamEnricher(),
		SubCmds: []*cobra.Command{
			taskboardColumnsLsCmd(),
			taskboardColumnsAddCmd(),
			taskboardColumnsRenameCmd(),
			taskboardColumnsMoveCmd(),
			taskboardColumnsRmCmd(),
		},
	}.ToCobra()
}

// runTaskboardColumns sends one columns request and prints the columns.
func runTaskboardColumns(peer, method, path string, body any, askHuman string, asJSON bool, verb string, stdout, stderr io.Writer) int {
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	ask, rc := parseAskHumanFlag(askHuman, stdout, stderr)
	if rc != rcOK {
		return rc
	}
	var resp taskboardColumnsResp
	if err := remoteRequestOpts(peer, method, path, body, &resp, DaemonOpts{AskHuman: ask}); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
	}
	if resp.Columns == nil {
		resp.Columns = []taskboardColumn{}
	}
	if asJSON {
		return writeJSONTo(stdout, resp)
	}
	if verb != "" {
		fmt.Fprintln(stdout, verb)
	}
	if len(resp.Moved) > 0 {
		fmt.Fprintf(stdout, "back to next: %s\n", strings.Join(resp.Moved, ", "))
	}
	if len(resp.Columns) == 0 {
		fmt.Fprintf(stdout, "%s: no custom columns (Ideas, Next, In progress, Blocked, Done)\n", resp.Group)
		return rcOK
	}
	fmt.Fprintf(stdout, "%s columns: Ideas, Next, In progress, ", resp.Group)
	for _, c := range resp.Columns {
		fmt.Fprintf(stdout, "%s [%s], ", c.Label, c.Slug)
	}
	fmt.Fprintln(stdout, "Blocked, Done")
	return rcOK
}

type taskboardColumnsLsParams struct {
	Group string `pos:"true" help:"Group whose board's columns to list"`
	Peer  string `long:"peer" optional:"true" help:"A linked node's board instead, under the grants it gave this node (taskboard.read)"`
	JSON  bool   `long:"json" help:"Output JSON"`
}

func taskboardColumnsLsCmd() *cobra.Command {
	return boa.CmdT[taskboardColumnsLsParams]{
		Use:         "ls",
		Short:       "List a board's columns, the custom ones with their slugs",
		ParamEnrich: common.DefaultParamEnricher(),
		InitFuncCtx: func(ctx *boa.HookContext, p *taskboardColumnsLsParams, _ *cobra.Command) error {
			boa.GetParamT(ctx, &p.Group).SetAlternativesFunc(completeGroupNames)
			return nil
		},
		RunFunc: func(p *taskboardColumnsLsParams, _ *cobra.Command, _ []string) {
			taskboardExit(runTaskboardColumns(p.Peer, http.MethodGet, taskboardColumnsPath(p.Group, ""), nil, "", p.JSON, "",
				cliStdout, cliStderr))
		},
	}.ToCobra()
}

type taskboardColumnsAddParams struct {
	Group    string `pos:"true" help:"Group whose board gets the column"`
	Label    string `pos:"true" help:"The column's name, e.g. \"In review\" (at most 40 bytes)"`
	Slug     string `long:"slug" optional:"true" help:"Its slug for --stage (default: made from the label, e.g. in-review)"`
	Before   string `long:"before" optional:"true" help:"Place it before this custom column (slug); default: the last custom column, just before Blocked"`
	Peer     string `long:"peer" optional:"true" help:"A linked node's board instead, under the grants it gave this node (taskboard.manage)"`
	AskHuman string `long:"ask-human" optional:"true" help:"On permission denial, ask the human via popup with this timeout (e.g. '30s'). Capped at 300s. Timeout = deny."`
	JSON     bool   `long:"json" help:"Output JSON"`
}

func taskboardColumnsAddCmd() *cobra.Command {
	return boa.CmdT[taskboardColumnsAddParams]{
		Use:         "add",
		Short:       "Add a custom column (needs taskboard.manage)",
		ParamEnrich: common.DefaultParamEnricher(),
		InitFuncCtx: func(ctx *boa.HookContext, p *taskboardColumnsAddParams, _ *cobra.Command) error {
			boa.GetParamT(ctx, &p.Group).SetAlternativesFunc(completeGroupNames)
			boa.GetParamT(ctx, &p.AskHuman).SetAlternativesFunc(completeAskHumanDurations)
			return nil
		},
		RunFunc: func(p *taskboardColumnsAddParams, _ *cobra.Command, _ []string) {
			body := map[string]any{"label": p.Label, "slug": p.Slug, "before": p.Before}
			taskboardExit(runTaskboardColumns(p.Peer, http.MethodPost, taskboardColumnsPath(p.Group, ""), body, p.AskHuman, p.JSON,
				"added column "+p.Label, cliStdout, cliStderr))
		},
	}.ToCobra()
}

type taskboardColumnsRenameParams struct {
	Group    string `pos:"true" help:"Group whose board has the column"`
	Column   string `pos:"true" help:"The column's slug"`
	Label    string `pos:"true" help:"Its new name (the slug stays)"`
	Peer     string `long:"peer" optional:"true" help:"A linked node's board instead, under the grants it gave this node (taskboard.manage)"`
	AskHuman string `long:"ask-human" optional:"true" help:"On permission denial, ask the human via popup with this timeout (e.g. '30s'). Capped at 300s. Timeout = deny."`
	JSON     bool   `long:"json" help:"Output JSON"`
}

func taskboardColumnsRenameCmd() *cobra.Command {
	return boa.CmdT[taskboardColumnsRenameParams]{
		Use:         "rename",
		Short:       "Rename a custom column; its slug stays (needs taskboard.manage)",
		ParamEnrich: common.DefaultParamEnricher(),
		InitFuncCtx: func(ctx *boa.HookContext, p *taskboardColumnsRenameParams, _ *cobra.Command) error {
			boa.GetParamT(ctx, &p.Group).SetAlternativesFunc(completeGroupNames)
			boa.GetParamT(ctx, &p.AskHuman).SetAlternativesFunc(completeAskHumanDurations)
			return nil
		},
		RunFunc: func(p *taskboardColumnsRenameParams, _ *cobra.Command, _ []string) {
			taskboardExit(runTaskboardColumns(p.Peer, http.MethodPatch, taskboardColumnsPath(p.Group, p.Column),
				map[string]any{"label": p.Label}, p.AskHuman, p.JSON, "renamed "+p.Column, cliStdout, cliStderr))
		},
	}.ToCobra()
}

type taskboardColumnsMoveParams struct {
	Group    string `pos:"true" help:"Group whose board has the column"`
	Column   string `pos:"true" help:"The column's slug"`
	Before   string `long:"before" optional:"true" help:"Just before this custom column (slug)"`
	Last     bool   `long:"last" help:"To the end of the custom columns, just before Blocked"`
	Peer     string `long:"peer" optional:"true" help:"A linked node's board instead, under the grants it gave this node (taskboard.manage)"`
	AskHuman string `long:"ask-human" optional:"true" help:"On permission denial, ask the human via popup with this timeout (e.g. '30s'). Capped at 300s. Timeout = deny."`
	JSON     bool   `long:"json" help:"Output JSON"`
}

func taskboardColumnsMoveCmd() *cobra.Command {
	return boa.CmdT[taskboardColumnsMoveParams]{
		Use:         "move",
		Short:       "Reorder a custom column among the custom ones (needs taskboard.manage)",
		ParamEnrich: common.DefaultParamEnricher(),
		InitFuncCtx: func(ctx *boa.HookContext, p *taskboardColumnsMoveParams, _ *cobra.Command) error {
			boa.GetParamT(ctx, &p.Group).SetAlternativesFunc(completeGroupNames)
			boa.GetParamT(ctx, &p.AskHuman).SetAlternativesFunc(completeAskHumanDurations)
			return nil
		},
		RunFunc: func(p *taskboardColumnsMoveParams, _ *cobra.Command, _ []string) {
			if (p.Before == "") == !p.Last {
				fmt.Fprintln(cliStderr, "Error: give exactly one of --before, --last")
				taskboardExit(rcInvalidArg)
				return
			}
			taskboardExit(runTaskboardColumns(p.Peer, http.MethodPatch, taskboardColumnsPath(p.Group, p.Column),
				map[string]any{"before": p.Before, "to_end": p.Last}, p.AskHuman, p.JSON, "moved "+p.Column, cliStdout, cliStderr))
		},
	}.ToCobra()
}

type taskboardColumnsRmParams struct {
	Group    string `pos:"true" help:"Group whose board has the column"`
	Column   string `pos:"true" help:"The column's slug"`
	Peer     string `long:"peer" optional:"true" help:"A linked node's board instead, under the grants it gave this node (taskboard.manage)"`
	AskHuman string `long:"ask-human" optional:"true" help:"On permission denial, ask the human via popup with this timeout (e.g. '30s'). Capped at 300s. Timeout = deny."`
	JSON     bool   `long:"json" help:"Output JSON"`
}

func taskboardColumnsRmCmd() *cobra.Command {
	return boa.CmdT[taskboardColumnsRmParams]{
		Use:         "rm",
		Short:       "Remove a custom column; its tasks go back to next (needs taskboard.manage)",
		ParamEnrich: common.DefaultParamEnricher(),
		InitFuncCtx: func(ctx *boa.HookContext, p *taskboardColumnsRmParams, _ *cobra.Command) error {
			boa.GetParamT(ctx, &p.Group).SetAlternativesFunc(completeGroupNames)
			boa.GetParamT(ctx, &p.AskHuman).SetAlternativesFunc(completeAskHumanDurations)
			return nil
		},
		RunFunc: func(p *taskboardColumnsRmParams, _ *cobra.Command, _ []string) {
			taskboardExit(runTaskboardColumns(p.Peer, http.MethodDelete, taskboardColumnsPath(p.Group, p.Column), nil,
				p.AskHuman, p.JSON, "removed "+p.Column, cliStdout, cliStderr))
		},
	}.ToCobra()
}
