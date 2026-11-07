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

// taskboardCmd is `tclaude agent taskboard …`: a group's short-term task
// board (docs/taskboard.md). It is the view of right now and the next few
// tasks; the durable backlog stays in the external tracker a task links to.
func taskboardCmd() *cobra.Command {
	return boa.CmdT[struct{}]{
		Use:   "taskboard",
		Short: "Read and change a group's short-term task board",
		Long: "A group's task board is the view of right now and the next few tasks — rarely more than about ten. " +
			"It is not the backlog: the durable backlog lives in an external tracker (awb, Linear, GitHub), and a task " +
			"may link to its item there with --ref. Work paused for longer goes back to that tracker: drop the task here " +
			"(its ref stays in the history).\n\n" +
			"Tasks are in one of two stages: ideas (captured, not yet queued; where `add` lands) and next (the queue, top " +
			"first). Members of the group read and change its board; anyone else needs taskboard.read / taskboard.write, " +
			"scoped with --scope group=<name> or unscoped. A task in a group you cannot read is reported as not found. " +
			"Needs features.taskboard.",
		ParamEnrich: common.DefaultParamEnricher(),
		SubCmds: []*cobra.Command{
			taskboardLsCmd(),
			taskboardShowCmd(),
			taskboardHistoryCmd(),
			taskboardAddCmd(),
			taskboardEditCmd(),
			taskboardMoveCmd(),
			taskboardStageCmd("promote", "next", "Promote an idea to the end of the queue"),
			taskboardStageCmd("demote", "idea", "Demote a queued task to the top of the ideas"),
			taskboardBlockCmd(),
			taskboardStatusCmd("unblock", "open", "Unblock a blocked task", false),
			taskboardStatusCmd("drop", "dropped", "Drop a task off the board (park it in the external tracker)", true),
			taskboardStatusCmd("done", "done", "Close a task as done", true),
			taskboardAssignCmd(),
			taskboardUnassignCmd("unassign", "Take a task back from its assignee (needs taskboard.assign)"),
			taskboardTakeCmd(),
			taskboardUnassignCmd("release", "Give back a task assigned to you"),
			taskboardReportCmd(),
		},
	}.ToCobra()
}

// taskboardTask is the daemon's wire shape for one task.
type taskboardTask struct {
	ID          string   `json:"id"`
	Group       string   `json:"group"`
	Title       string   `json:"title"`
	Body        string   `json:"body,omitempty"`
	Stage       string   `json:"stage"`
	Status      string   `json:"status"`
	BlockReason string   `json:"block_reason,omitempty"`
	DependsOn   []string `json:"depends_on"`
	Unmet       []string `json:"unmet,omitempty"`
	Queued      bool     `json:"queued,omitempty"`
	CreatedBy   string   `json:"created_by"`
	CreatedAt   string   `json:"created_at"`
	UpdatedAt   string   `json:"updated_at"`
	ClosedAt    string   `json:"closed_at,omitempty"`
	Assignee    *struct {
		AgentID string `json:"agent_id"`
		Name    string `json:"name,omitempty"`
	} `json:"assignee,omitempty"`
	AssignedBy  string `json:"assigned_by,omitempty"`
	Outcome     string `json:"outcome,omitempty"`
	OutcomeNote string `json:"outcome_note,omitempty"`
	Failures    int    `json:"failures,omitempty"`
	Ref         *struct {
		Kind  string `json:"kind"`
		URL   string `json:"url,omitempty"`
		Label string `json:"label,omitempty"`
	} `json:"ref,omitempty"`
}

func (t taskboardTask) refText() string {
	if t.Ref == nil {
		return ""
	}
	if t.Ref.Label != "" {
		return t.Ref.Label
	}
	return t.Ref.URL
}

func taskboardPath(id string, suffix string) string {
	return "/v1/taskboard/tasks/" + url.PathEscape(strings.TrimSpace(id)) + suffix
}

func writeJSONTo(stdout io.Writer, v any) int {
	enc := json.NewEncoder(stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return rcIOFailure
	}
	return rcOK
}

// taskboardWrite sends a mutation and prints the task it returns.
func taskboardWrite(method, path string, body any, askHuman string, asJSON bool, verb string, stdout, stderr io.Writer) int {
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	ask, rc := parseAskHumanFlag(askHuman, stdout, stderr)
	if rc != rcOK {
		return rc
	}
	var t taskboardTask
	if err := DaemonRequest(method, path, body, &t, DaemonOpts{AskHuman: ask}); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
	}
	if asJSON {
		return writeJSONTo(stdout, t)
	}
	fmt.Fprintf(stdout, "%s %s: %s  [%s, %s]\n", verb, t.ID, t.Title, t.Stage, t.Status)
	return rcOK
}

// ---- ls ----

type taskboardLsParams struct {
	Group string `pos:"true" optional:"true" help:"Group whose board to list (default: your group, when you are in exactly one)"`
	Stage string `long:"stage" optional:"true" help:"Only this stage: idea or next"`
	All   bool   `long:"all" help:"Also list done and dropped tasks not yet archived"`
	Mine  bool   `long:"mine" help:"Only the live tasks assigned to you, in every group"`
	JSON  bool   `long:"json" help:"Output JSON"`
}

func (t taskboardTask) assigneeText() string {
	if t.Assignee == nil {
		return ""
	}
	if t.Assignee.Name != "" {
		return t.Assignee.Name
	}
	return t.Assignee.AgentID
}

func taskboardLsCmd() *cobra.Command {
	return boa.CmdT[taskboardLsParams]{
		Use:         "ls",
		Short:       "List a group's task board: the queue (next), then the ideas",
		ParamEnrich: common.DefaultParamEnricher(),
		InitFuncCtx: func(ctx *boa.HookContext, p *taskboardLsParams, _ *cobra.Command) error {
			boa.GetParamT(ctx, &p.Group).SetAlternativesFunc(completeGroupNames)
			return nil
		},
		RunFunc: func(p *taskboardLsParams, _ *cobra.Command, _ []string) {
			os.Exit(runTaskboardLs(p, cliStdout, cliStderr))
		},
	}.ToCobra()
}

func runTaskboardLs(p *taskboardLsParams, stdout, stderr io.Writer) int {
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	q := url.Values{}
	if p.Stage != "" {
		q.Set("stage", p.Stage)
	}
	if p.All {
		q.Set("all", "1")
	}
	path := "/v1/taskboard"
	if g := strings.TrimSpace(p.Group); g != "" {
		path = "/v1/groups/" + url.PathEscape(g) + "/taskboard"
	}
	if p.Mine {
		if p.Group != "" || p.Stage != "" || p.All {
			fmt.Fprintln(stderr, "Error: --mine takes no group, --stage or --all")
			return rcInvalidArg
		}
		return runTaskboardMine(p.JSON, stdout, stderr)
	}
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	var resp struct {
		Group   string          `json:"group"`
		Tasks   []taskboardTask `json:"tasks"`
		Locked  bool            `json:"locked"`
		Writers []string        `json:"writers,omitempty"`
	}
	if err := DaemonGet(path, &resp); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
	}
	if p.JSON {
		return writeJSONTo(stdout, resp)
	}
	if len(resp.Tasks) == 0 {
		fmt.Fprintf(stdout, "%s: the task board is empty\n", resp.Group)
		return rcOK
	}
	fmt.Fprintf(stdout, "%s task board\n", resp.Group)
	if resp.Locked {
		fmt.Fprintf(stdout, "(locked to writers: %s)\n", strings.Join(append([]string{"owners"}, resp.Writers...), ", "))
	}
	section := ""
	n := 0
	for _, t := range resp.Tasks {
		s := t.Stage
		if t.Status == "done" || t.Status == "dropped" {
			s = "closed"
		}
		if s != section {
			section = s
			n = 0
			switch s {
			case "next":
				fmt.Fprintln(stdout, "── next ──")
			case "idea":
				fmt.Fprintln(stdout, "── ideas ──")
			default:
				fmt.Fprintln(stdout, "── done / dropped ──")
			}
		}
		n++
		state := t.Status
		if t.Queued {
			state = "queued"
		}
		line := fmt.Sprintf("%3d. %s  %-7s  %s", n, t.ID, state, t.Title)
		if ref := t.refText(); ref != "" {
			line += "  [" + ref + "]"
		}
		if len(t.DependsOn) > 0 {
			line += "  after " + strings.Join(t.DependsOn, ",")
		}
		if a := t.assigneeText(); a != "" {
			line += "  @" + a
		}
		if t.BlockReason != "" {
			line += "  — " + t.BlockReason
		}
		fmt.Fprintln(stdout, line)
	}
	return rcOK
}

// ---- show / history ----

type taskboardShowParams struct {
	Task string `pos:"true" help:"Task id (tk_…)"`
	JSON bool   `long:"json" help:"Output JSON"`
}

func taskboardShowCmd() *cobra.Command {
	return boa.CmdT[taskboardShowParams]{
		Use:         "show",
		Short:       "Show one task",
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(p *taskboardShowParams, _ *cobra.Command, _ []string) {
			os.Exit(runTaskboardShow(p, cliStdout, cliStderr))
		},
	}.ToCobra()
}

func runTaskboardShow(p *taskboardShowParams, stdout, stderr io.Writer) int {
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	var t taskboardTask
	if err := DaemonGet(taskboardPath(p.Task, ""), &t); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
	}
	if p.JSON {
		return writeJSONTo(stdout, t)
	}
	fmt.Fprintf(stdout, "%s  %s\n", t.ID, t.Title)
	fmt.Fprintf(stdout, "  group:   %s\n  stage:   %s\n  status:  %s\n", t.Group, t.Stage, t.Status)
	if t.BlockReason != "" {
		fmt.Fprintf(stdout, "  blocked: %s\n", t.BlockReason)
	}
	if t.Ref != nil {
		fmt.Fprintf(stdout, "  ref:     %s %s %s\n", t.Ref.Kind, t.Ref.Label, t.Ref.URL)
	}
	if a := t.assigneeText(); a != "" {
		fmt.Fprintf(stdout, "  assigned: %s (by %s)\n", a, t.AssignedBy)
	}
	if t.Outcome != "" {
		fmt.Fprintf(stdout, "  last report: %s %s\n", t.Outcome, t.OutcomeNote)
	}
	if t.Failures > 0 {
		fmt.Fprintf(stdout, "  failures: %d\n", t.Failures)
	}
	if len(t.DependsOn) > 0 {
		fmt.Fprintf(stdout, "  after:   %s", strings.Join(t.DependsOn, ", "))
		if len(t.Unmet) > 0 {
			fmt.Fprintf(stdout, "  (unmet: %s)", strings.Join(t.Unmet, ", "))
		}
		fmt.Fprintln(stdout)
	}
	fmt.Fprintf(stdout, "  created: %s by %s\n", t.CreatedAt, t.CreatedBy)
	if t.ClosedAt != "" {
		fmt.Fprintf(stdout, "  closed:  %s\n", t.ClosedAt)
	}
	if t.Body != "" {
		fmt.Fprintf(stdout, "\n%s\n", t.Body)
	}
	return rcOK
}

func taskboardHistoryCmd() *cobra.Command {
	return boa.CmdT[taskboardShowParams]{
		Use:         "history",
		Short:       "Show a task's history",
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(p *taskboardShowParams, _ *cobra.Command, _ []string) {
			os.Exit(runTaskboardHistory(p, cliStdout, cliStderr))
		},
	}.ToCobra()
}

func runTaskboardHistory(p *taskboardShowParams, stdout, stderr io.Writer) int {
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	var resp struct {
		ID     string `json:"id"`
		Title  string `json:"title"`
		Events []struct {
			At     string `json:"at"`
			Actor  string `json:"actor"`
			Kind   string `json:"kind"`
			Detail string `json:"detail,omitempty"`
		} `json:"events"`
	}
	if err := DaemonGet(taskboardPath(p.Task, "/events"), &resp); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
	}
	if p.JSON {
		return writeJSONTo(stdout, resp)
	}
	fmt.Fprintf(stdout, "%s  %s\n", resp.ID, resp.Title)
	for _, e := range resp.Events {
		line := fmt.Sprintf("  %s  %-9s  %s", e.At, e.Kind, e.Actor)
		if e.Detail != "" {
			line += "  " + e.Detail
		}
		fmt.Fprintln(stdout, line)
	}
	return rcOK
}

// ---- add / edit ----

type taskboardAddParams struct {
	Group    string `pos:"true" help:"Group whose board to add to"`
	Title    string `pos:"true" help:"Task title (one line, up to 200 bytes)"`
	Body     string `long:"body" optional:"true" help:"Task brief (up to 8 KiB)"`
	BodyFile string `long:"body-file" optional:"true" help:"Read the brief from this file ('-' for stdin)"`
	Ref      string `long:"ref" optional:"true" help:"External ref: an http(s) URL, owner/repo#n, or a tracker id (tcl-a1b2c3 for awb, LIN-123 for Linear; prefix awb: or linear: to say which)"`
	RefLabel string `long:"ref-label" optional:"true" help:"Display label for the ref (default: derived)"`
	Next     bool   `long:"next" help:"Add to the queue (next) instead of the ideas"`
	After    string `long:"after" optional:"true" help:"Comma-separated task ids this task depends on"`
	Top      bool   `long:"top" help:"Place at the top of its stage instead of the end"`
	Before   string `long:"before" optional:"true" help:"Place just before this task (and in its stage)"`
	AskHuman string `long:"ask-human" optional:"true" help:"On permission denial, ask the human via popup with this timeout (e.g. '30s'). Capped at 300s. Timeout = deny."`
	JSON     bool   `long:"json" help:"Output JSON"`
}

func taskboardAddCmd() *cobra.Command {
	return boa.CmdT[taskboardAddParams]{
		Use:         "add",
		Short:       "Add a task (lands in the ideas unless --next)",
		ParamEnrich: common.DefaultParamEnricher(),
		InitFuncCtx: func(ctx *boa.HookContext, p *taskboardAddParams, _ *cobra.Command) error {
			boa.GetParamT(ctx, &p.Group).SetAlternativesFunc(completeGroupNames)
			boa.GetParamT(ctx, &p.AskHuman).SetAlternativesFunc(completeAskHumanDurations)
			return nil
		},
		RunFunc: func(p *taskboardAddParams, _ *cobra.Command, _ []string) {
			os.Exit(runTaskboardAdd(p, os.Stdin, cliStdout, cliStderr))
		},
	}.ToCobra()
}

// readTaskBody resolves --body / --body-file; set reports whether either was given.
func readTaskBody(body, file string, stdin io.Reader) (text string, set bool, err error) {
	if body != "" && file != "" {
		return "", false, fmt.Errorf("give --body or --body-file, not both")
	}
	if file == "" {
		return body, body != "", nil
	}
	var b []byte
	if file == "-" {
		b, err = io.ReadAll(io.LimitReader(stdin, 64<<10))
	} else {
		b, err = os.ReadFile(file)
	}
	if err != nil {
		return "", false, err
	}
	return strings.TrimRight(string(b), "\n"), true, nil
}

func splitTaskIDs(s string) []string {
	var out []string
	for part := range strings.SplitSeq(s, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

func runTaskboardAdd(p *taskboardAddParams, stdin io.Reader, stdout, stderr io.Writer) int {
	body, _, err := readTaskBody(p.Body, p.BodyFile, stdin)
	if err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return rcInvalidArg
	}
	if p.Top && p.Before != "" {
		fmt.Fprintln(stderr, "Error: give --top or --before, not both")
		return rcInvalidArg
	}
	req := map[string]any{"title": p.Title, "body": body, "ref": p.Ref, "ref_label": p.RefLabel,
		"after": splitTaskIDs(p.After), "position": map[string]any{"top": p.Top, "before": p.Before}}
	if p.Next {
		req["stage"] = "next"
	}
	path := "/v1/groups/" + url.PathEscape(strings.TrimSpace(p.Group)) + "/taskboard/tasks"
	return taskboardWrite(http.MethodPost, path, req, p.AskHuman, p.JSON, "added", stdout, stderr)
}

type taskboardEditParams struct {
	Task      string `pos:"true" help:"Task id (tk_…)"`
	Title     string `long:"title" optional:"true" help:"New title"`
	Body      string `long:"body" optional:"true" help:"New brief"`
	ClearBody bool   `long:"clear-body" help:"Remove the brief"`
	BodyFile  string `long:"body-file" optional:"true" help:"Read the new brief from this file ('-' for stdin)"`
	Ref       string `long:"ref" optional:"true" help:"New external ref (URL, owner/repo#n, or tracker id)"`
	RefLabel  string `long:"ref-label" optional:"true" help:"New display label for the ref"`
	ClearRef  bool   `long:"clear-ref" help:"Remove the ref"`
	AddDep    string `long:"add-dep" optional:"true" help:"Comma-separated task ids to depend on"`
	RmDep     string `long:"rm-dep" optional:"true" help:"Comma-separated task ids to stop depending on"`
	AskHuman  string `long:"ask-human" optional:"true" help:"On permission denial, ask the human via popup with this timeout (e.g. '30s'). Capped at 300s. Timeout = deny."`
	JSON      bool   `long:"json" help:"Output JSON"`
}

func taskboardEditCmd() *cobra.Command {
	return boa.CmdT[taskboardEditParams]{
		Use:         "edit",
		Short:       "Edit a task's title, brief, ref or dependencies",
		ParamEnrich: common.DefaultParamEnricher(),
		InitFuncCtx: func(ctx *boa.HookContext, p *taskboardEditParams, _ *cobra.Command) error {
			boa.GetParamT(ctx, &p.AskHuman).SetAlternativesFunc(completeAskHumanDurations)
			return nil
		},
		RunFunc: func(p *taskboardEditParams, _ *cobra.Command, _ []string) {
			os.Exit(runTaskboardEdit(p, os.Stdin, cliStdout, cliStderr))
		},
	}.ToCobra()
}

func runTaskboardEdit(p *taskboardEditParams, stdin io.Reader, stdout, stderr io.Writer) int {
	body, bodySet, err := readTaskBody(p.Body, p.BodyFile, stdin)
	if err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return rcInvalidArg
	}
	if p.ClearRef && (p.Ref != "" || p.RefLabel != "") {
		fmt.Fprintln(stderr, "Error: give --clear-ref or --ref/--ref-label, not both")
		return rcInvalidArg
	}
	req := map[string]any{}
	if p.Title != "" {
		req["title"] = p.Title
	}
	if p.ClearBody && bodySet {
		fmt.Fprintln(stderr, "Error: give --clear-body or --body/--body-file, not both")
		return rcInvalidArg
	}
	if bodySet || p.ClearBody {
		req["body"] = body
	}
	if p.Ref != "" {
		req["ref"] = p.Ref
	}
	if p.RefLabel != "" {
		req["ref_label"] = p.RefLabel
	}
	if p.ClearRef {
		req["clear_ref"] = true
	}
	if deps := splitTaskIDs(p.AddDep); len(deps) > 0 {
		req["add_deps"] = deps
	}
	if deps := splitTaskIDs(p.RmDep); len(deps) > 0 {
		req["rm_deps"] = deps
	}
	if len(req) == 0 {
		fmt.Fprintln(stderr, "Error: nothing to change; see --help")
		return rcInvalidArg
	}
	return taskboardWrite(http.MethodPatch, taskboardPath(p.Task, ""), req, p.AskHuman, p.JSON, "edited", stdout, stderr)
}

// ---- move / stage / status ----

type taskboardMoveParams struct {
	Task     string `pos:"true" help:"Task id (tk_…)"`
	Top      bool   `long:"top" help:"To the top of its stage"`
	Bottom   bool   `long:"bottom" help:"To the end of its stage"`
	Before   string `long:"before" optional:"true" help:"Just before this task (crossing into the other stage promotes or demotes)"`
	After    string `long:"after" optional:"true" help:"Just after this task (crossing into the other stage promotes or demotes)"`
	AskHuman string `long:"ask-human" optional:"true" help:"On permission denial, ask the human via popup with this timeout (e.g. '30s'). Capped at 300s. Timeout = deny."`
	JSON     bool   `long:"json" help:"Output JSON"`
}

func taskboardMoveCmd() *cobra.Command {
	return boa.CmdT[taskboardMoveParams]{
		Use:         "move",
		Short:       "Reorder a task",
		ParamEnrich: common.DefaultParamEnricher(),
		InitFuncCtx: func(ctx *boa.HookContext, p *taskboardMoveParams, _ *cobra.Command) error {
			boa.GetParamT(ctx, &p.AskHuman).SetAlternativesFunc(completeAskHumanDurations)
			return nil
		},
		RunFunc: func(p *taskboardMoveParams, _ *cobra.Command, _ []string) {
			os.Exit(runTaskboardMove(p, cliStdout, cliStderr))
		},
	}.ToCobra()
}

func runTaskboardMove(p *taskboardMoveParams, stdout, stderr io.Writer) int {
	n := 0
	for _, set := range []bool{p.Top, p.Bottom, p.Before != "", p.After != ""} {
		if set {
			n++
		}
	}
	if n != 1 {
		fmt.Fprintln(stderr, "Error: give exactly one of --top, --bottom, --before, --after")
		return rcInvalidArg
	}
	req := map[string]any{"top": p.Top, "bottom": p.Bottom, "before": p.Before, "after": p.After}
	return taskboardWrite(http.MethodPost, taskboardPath(p.Task, "/move"), req, p.AskHuman, p.JSON, "moved", stdout, stderr)
}

type taskboardTaskParams struct {
	Task     string `pos:"true" help:"Task id (tk_…)"`
	AskHuman string `long:"ask-human" optional:"true" help:"On permission denial, ask the human via popup with this timeout (e.g. '30s'). Capped at 300s. Timeout = deny."`
	JSON     bool   `long:"json" help:"Output JSON"`
}

func taskboardStageCmd(use, stage, short string) *cobra.Command {
	return boa.CmdT[taskboardTaskParams]{
		Use:         use,
		Short:       short,
		ParamEnrich: common.DefaultParamEnricher(),
		InitFuncCtx: func(ctx *boa.HookContext, p *taskboardTaskParams, _ *cobra.Command) error {
			boa.GetParamT(ctx, &p.AskHuman).SetAlternativesFunc(completeAskHumanDurations)
			return nil
		},
		RunFunc: func(p *taskboardTaskParams, _ *cobra.Command, _ []string) {
			os.Exit(taskboardWrite(http.MethodPost, taskboardPath(p.Task, "/stage"), map[string]any{"stage": stage},
				p.AskHuman, p.JSON, use+"d", cliStdout, cliStderr))
		},
	}.ToCobra()
}

type taskboardStatusParams struct {
	Task     string `pos:"true" help:"Task id (tk_…)"`
	Reason   string `long:"reason" optional:"true" help:"Why (kept in the task's history)"`
	AskHuman string `long:"ask-human" optional:"true" help:"On permission denial, ask the human via popup with this timeout (e.g. '30s'). Capped at 300s. Timeout = deny."`
	JSON     bool   `long:"json" help:"Output JSON"`
}

func taskboardStatusCmd(use, status, short string, withReason bool) *cobra.Command {
	verbs := map[string]string{"unblock": "unblocked", "drop": "dropped", "done": "closed"}
	if !withReason {
		return boa.CmdT[taskboardTaskParams]{
			Use:         use,
			Short:       short,
			ParamEnrich: common.DefaultParamEnricher(),
			InitFuncCtx: func(ctx *boa.HookContext, p *taskboardTaskParams, _ *cobra.Command) error {
				boa.GetParamT(ctx, &p.AskHuman).SetAlternativesFunc(completeAskHumanDurations)
				return nil
			},
			RunFunc: func(p *taskboardTaskParams, _ *cobra.Command, _ []string) {
				os.Exit(taskboardWrite(http.MethodPost, taskboardPath(p.Task, "/status"), map[string]any{"status": status},
					p.AskHuman, p.JSON, verbs[use], cliStdout, cliStderr))
			},
		}.ToCobra()
	}
	return boa.CmdT[taskboardStatusParams]{
		Use:         use,
		Short:       short,
		ParamEnrich: common.DefaultParamEnricher(),
		InitFuncCtx: func(ctx *boa.HookContext, p *taskboardStatusParams, _ *cobra.Command) error {
			boa.GetParamT(ctx, &p.AskHuman).SetAlternativesFunc(completeAskHumanDurations)
			return nil
		},
		RunFunc: func(p *taskboardStatusParams, _ *cobra.Command, _ []string) {
			os.Exit(taskboardWrite(http.MethodPost, taskboardPath(p.Task, "/status"),
				map[string]any{"status": status, "reason": p.Reason}, p.AskHuman, p.JSON, verbs[use], cliStdout, cliStderr))
		},
	}.ToCobra()
}

type taskboardBlockParams struct {
	Task     string `pos:"true" help:"Task id (tk_…)"`
	Reason   string `long:"reason" help:"Why the task is blocked"`
	AskHuman string `long:"ask-human" optional:"true" help:"On permission denial, ask the human via popup with this timeout (e.g. '30s'). Capped at 300s. Timeout = deny."`
	JSON     bool   `long:"json" help:"Output JSON"`
}

func taskboardBlockCmd() *cobra.Command {
	return boa.CmdT[taskboardBlockParams]{
		Use:         "block",
		Short:       "Mark a task blocked, with a reason",
		ParamEnrich: common.DefaultParamEnricher(),
		InitFuncCtx: func(ctx *boa.HookContext, p *taskboardBlockParams, _ *cobra.Command) error {
			boa.GetParamT(ctx, &p.AskHuman).SetAlternativesFunc(completeAskHumanDurations)
			return nil
		},
		RunFunc: func(p *taskboardBlockParams, _ *cobra.Command, _ []string) {
			os.Exit(taskboardWrite(http.MethodPost, taskboardPath(p.Task, "/status"),
				map[string]any{"status": "blocked", "reason": p.Reason}, p.AskHuman, p.JSON, "blocked", cliStdout, cliStderr))
		},
	}.ToCobra()
}

// ---- assign / take / release / report ----

func runTaskboardMine(asJSON bool, stdout, stderr io.Writer) int {
	tasks, rc := fetchTaskboardMine(stderr)
	if rc != rcOK {
		return rc
	}
	if asJSON {
		return writeJSONTo(stdout, map[string]any{"tasks": tasks})
	}
	if len(tasks) == 0 {
		fmt.Fprintln(stdout, "no tasks are assigned to you")
		return rcOK
	}
	for _, t := range tasks {
		line := fmt.Sprintf("%s  %-8s %-7s  %s", t.ID, t.Group, t.Status, t.Title)
		if ref := t.refText(); ref != "" {
			line += "  [" + ref + "]"
		}
		fmt.Fprintln(stdout, line)
	}
	return rcOK
}

func fetchTaskboardMine(stderr io.Writer) ([]taskboardTask, int) {
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return nil, rc
	}
	var resp struct {
		Tasks []taskboardTask `json:"tasks"`
	}
	if err := DaemonGet("/v1/taskboard/mine", &resp); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return nil, MapDaemonErrorToRC(err)
	}
	return resp.Tasks, rcOK
}

type taskboardAssignParams struct {
	Task     string `pos:"true" help:"Task id (tk_…)"`
	To       string `long:"to" help:"Agent to assign it to (name or id); an active member of the task's group"`
	AskHuman string `long:"ask-human" optional:"true" help:"On permission denial, ask the human via popup with this timeout (e.g. '30s'). Capped at 300s. Timeout = deny."`
	JSON     bool   `long:"json" help:"Output JSON"`
}

func taskboardAssignCmd() *cobra.Command {
	return boa.CmdT[taskboardAssignParams]{
		Use:   "assign",
		Short: "Assign a task to a member of its group (they get an inbox brief)",
		Long: "Assign a task to an active member of the task's group. An idea is promoted to the queue on assign. " +
			"The assignee gets an inbox message from you with the brief and the report commands. Assigning another " +
			"agent needs taskboard.assign (group owners have it for their groups). The assignment never retires or " +
			"stops the agent.",
		ParamEnrich: common.DefaultParamEnricher(),
		InitFuncCtx: func(ctx *boa.HookContext, p *taskboardAssignParams, _ *cobra.Command) error {
			boa.GetParamT(ctx, &p.AskHuman).SetAlternativesFunc(completeAskHumanDurations)
			return nil
		},
		RunFunc: func(p *taskboardAssignParams, _ *cobra.Command, _ []string) {
			if strings.TrimSpace(p.To) == "" {
				fmt.Fprintln(cliStderr, "Error: --to is required (use `taskboard take` to take a task yourself)")
				os.Exit(rcInvalidArg)
			}
			os.Exit(taskboardWrite(http.MethodPost, taskboardPath(p.Task, "/assign"), map[string]any{"to": p.To},
				p.AskHuman, p.JSON, "assigned", cliStdout, cliStderr))
		},
	}.ToCobra()
}

func taskboardTakeCmd() *cobra.Command {
	return boa.CmdT[taskboardTaskParams]{
		Use:         "take",
		Short:       "Take a queued, unassigned task of your group for yourself",
		ParamEnrich: common.DefaultParamEnricher(),
		InitFuncCtx: func(ctx *boa.HookContext, p *taskboardTaskParams, _ *cobra.Command) error {
			boa.GetParamT(ctx, &p.AskHuman).SetAlternativesFunc(completeAskHumanDurations)
			return nil
		},
		RunFunc: func(p *taskboardTaskParams, _ *cobra.Command, _ []string) {
			os.Exit(taskboardWrite(http.MethodPost, taskboardPath(p.Task, "/assign"), map[string]any{"to": ""},
				p.AskHuman, p.JSON, "took", cliStdout, cliStderr))
		},
	}.ToCobra()
}

func taskboardUnassignCmd(use, short string) *cobra.Command {
	verb := map[string]string{"unassign": "unassigned", "release": "released"}[use]
	return boa.CmdT[taskboardTaskParams]{
		Use:         use,
		Short:       short,
		ParamEnrich: common.DefaultParamEnricher(),
		InitFuncCtx: func(ctx *boa.HookContext, p *taskboardTaskParams, _ *cobra.Command) error {
			boa.GetParamT(ctx, &p.AskHuman).SetAlternativesFunc(completeAskHumanDurations)
			return nil
		},
		RunFunc: func(p *taskboardTaskParams, _ *cobra.Command, _ []string) {
			os.Exit(taskboardWrite(http.MethodPost, taskboardPath(p.Task, "/unassign"), map[string]any{},
				p.AskHuman, p.JSON, verb, cliStdout, cliStderr))
		},
	}.ToCobra()
}

type taskboardReportParams struct {
	Outcome string `pos:"true" help:"done or failed"`
	Task    string `long:"task" optional:"true" help:"Task id (default: the one live task assigned to you)"`
	Note    string `long:"note" optional:"true" help:"What you did, or why it failed (up to 4 KiB)"`
	JSON    bool   `long:"json" help:"Output JSON"`
}

func taskboardReportCmd() *cobra.Command {
	return boa.CmdT[taskboardReportParams]{
		Use:   "report",
		Short: "Report done or failed on a task assigned to you",
		Long: "Report done or failed on a task assigned to you. done closes the task; failed gives it back " +
			"(after three failed reports it is blocked). Reporting does not end your session, and the board's writer " +
			"lock never stops it. --task may be left out when exactly one live task is assigned to you.",
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(p *taskboardReportParams, _ *cobra.Command, _ []string) {
			os.Exit(runTaskboardReport(p, cliStdout, cliStderr))
		},
	}.ToCobra()
}

func runTaskboardReport(p *taskboardReportParams, stdout, stderr io.Writer) int {
	outcome := strings.TrimSpace(p.Outcome)
	if outcome != "done" && outcome != "failed" {
		fmt.Fprintln(stderr, "Error: report done or report failed")
		return rcInvalidArg
	}
	task := strings.TrimSpace(p.Task)
	if task == "" {
		tasks, rc := fetchTaskboardMine(stderr)
		if rc != rcOK {
			return rc
		}
		switch len(tasks) {
		case 0:
			fmt.Fprintln(stderr, "Error: no live task is assigned to you")
			return rcNotFound
		case 1:
			task = tasks[0].ID
		default:
			ids := make([]string, 0, len(tasks))
			for _, t := range tasks {
				ids = append(ids, t.ID)
			}
			fmt.Fprintf(stderr, "Error: %d tasks are assigned to you (%s); name one with --task\n", len(tasks), strings.Join(ids, ", "))
			return rcAmbiguous
		}
	}
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	var resp struct {
		Task     taskboardTask `json:"task"`
		Repeated bool          `json:"repeated"`
		Blocked  bool          `json:"blocked"`
	}
	if err := DaemonRequest(http.MethodPost, taskboardPath(task, "/report"),
		map[string]any{"outcome": outcome, "note": p.Note}, &resp, DaemonOpts{}); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
	}
	if p.JSON {
		return writeJSONTo(stdout, resp)
	}
	switch {
	case resp.Repeated:
		fmt.Fprintf(stdout, "already reported %s on %s\n", outcome, resp.Task.ID)
	case resp.Blocked:
		fmt.Fprintf(stdout, "reported failed on %s; it is now blocked (%s)\n", resp.Task.ID, resp.Task.BlockReason)
	default:
		fmt.Fprintf(stdout, "reported %s on %s: %s  [%s]\n", outcome, resp.Task.ID, resp.Task.Title, resp.Task.Status)
	}
	return rcOK
}

// ---- groups set-taskboard-writers ----

type groupsTaskboardWritersParams struct {
	Group    string   `pos:"true" help:"Group whose task board to lock"`
	Agents   []string `pos:"true" optional:"true" help:"Members who may still change the board (owners, explicit grant holders and the operator always may)"`
	Members  bool     `long:"members" help:"Unlock: every member may change the board again"`
	Show     bool     `long:"show" help:"Only show the current lock"`
	AskHuman string   `long:"ask-human" optional:"true" help:"On permission denial, ask the human via popup with this timeout (e.g. '30s'). Capped at 300s. Timeout = deny."`
}

func groupsSetTaskboardWritersCmd() *cobra.Command {
	return boa.CmdT[groupsTaskboardWritersParams]{
		Use:   "set-taskboard-writers",
		Short: "Lock a group's task board to listed writers, or open it to every member (--members)",
		Long: "By default every member of a group may change its task board and take its tasks. Listing writers " +
			"narrows that to them; owners, agents with an explicit taskboard.write or taskboard.assign grant, and " +
			"the operator are not affected, and reporting on your own task is never locked. With no agents and no " +
			"--members the board is locked to owners only. Needs taskboard.manage (group owners have it).",
		ParamEnrich: common.DefaultParamEnricher(),
		InitFuncCtx: func(ctx *boa.HookContext, p *groupsTaskboardWritersParams, _ *cobra.Command) error {
			boa.GetParamT(ctx, &p.Group).SetAlternativesFunc(completeGroupNames)
			boa.GetParamT(ctx, &p.AskHuman).SetAlternativesFunc(completeAskHumanDurations)
			return nil
		},
		RunFunc: func(p *groupsTaskboardWritersParams, _ *cobra.Command, _ []string) {
			os.Exit(runGroupsSetTaskboardWriters(p, cliStdout, cliStderr))
		},
	}.ToCobra()
}

func runGroupsSetTaskboardWriters(p *groupsTaskboardWritersParams, stdout, stderr io.Writer) int {
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	path := "/v1/groups/" + url.PathEscape(strings.TrimSpace(p.Group)) + "/taskboard/writers"
	var resp struct {
		Group   string `json:"group"`
		Locked  bool   `json:"locked"`
		Writers []struct {
			AgentID string `json:"agent_id"`
			Name    string `json:"name,omitempty"`
		} `json:"writers"`
	}
	if p.Show {
		if err := DaemonGet(path, &resp); err != nil {
			fmt.Fprintf(stderr, "Error: %v\n", err)
			return MapDaemonErrorToRC(err)
		}
	} else {
		if p.Members && len(p.Agents) > 0 {
			fmt.Fprintln(stderr, "Error: give agents or --members, not both")
			return rcInvalidArg
		}
		ask, rc := parseAskHumanFlag(p.AskHuman, stdout, stderr)
		if rc != rcOK {
			return rc
		}
		body := map[string]any{"members": p.Members, "agents": p.Agents}
		if err := DaemonRequest(http.MethodPost, path, body, &resp, DaemonOpts{AskHuman: ask}); err != nil {
			fmt.Fprintf(stderr, "Error: %v\n", err)
			return MapDaemonErrorToRC(err)
		}
	}
	if !resp.Locked {
		fmt.Fprintf(stdout, "%s: task board open to every member\n", resp.Group)
		return rcOK
	}
	names := []string{}
	for _, w := range resp.Writers {
		names = append(names, firstNonEmptyStr(w.Name, w.AgentID))
	}
	if len(names) == 0 {
		fmt.Fprintf(stdout, "%s: task board locked to owners only\n", resp.Group)
	} else {
		fmt.Fprintf(stdout, "%s: task board locked to owners and %s\n", resp.Group, strings.Join(names, ", "))
	}
	return rcOK
}

func firstNonEmptyStr(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
