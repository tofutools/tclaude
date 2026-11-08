package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

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
			"first). Members of the group read its board and track their own work on it (taskboard.own): a task a member adds " +
			"is assigned to it, and it edits, blocks, closes and reports on its own tasks, and takes any unassigned one. " +
			"Owners change everything (taskboard.write); anyone else needs taskboard.read / taskboard.write, " +
			"scoped with --scope group=<name> or unscoped, and a group grant of taskboard.write gives every member of a group the broad rights. A task in a group you cannot read is reported as not found. " +
			"On by default; features.taskboard false turns it off.",
		ParamEnrich: common.DefaultParamEnricher(),
		SubCmds: []*cobra.Command{
			taskboardLsCmd(),
			taskboardShowCmd(),
			taskboardActivityCmd(),
			taskboardCommentCmd(),
			taskboardAddCmd(),
			taskboardAskCmd(),
			taskboardEditCmd(),
			taskboardAttachCmd(),
			taskboardDetachCmd(),
			taskboardAttachmentsCmd(),
			taskboardMoveCmd(),
			taskboardMoveToGroupCmd(),
			taskboardStageCmd("promote", "next", "Promote an idea to the end of the queue"),
			taskboardStageCmd("demote", "idea", "Demote a queued task to the top of the ideas"),
			taskboardBlockCmd(),
			taskboardStatusCmd("unblock", "open", "Unblock a blocked task", false),
			taskboardStatusCmd("drop", "dropped", "Drop a task off the board (park it in the external tracker)", true),
			taskboardStatusCmd("done", "done", "Close a task as done", true),
			taskboardReopenCmd(),
			taskboardAssignCmd(),
			taskboardUnassignCmd("unassign", "Take a task back from its assignee (needs taskboard.assign)"),
			taskboardTakeCmd(),
			taskboardStartCmd(),
			taskboardHoldCmd(),
			taskboardUnassignCmd("release", "Give back a task assigned to you"),
			taskboardReportCmd(),
		},
	}.ToCobra()
}

// taskboardExit ends a taskboard verb; RunTaskboardCLI swaps it so a verb can
// run in-process.
var taskboardExit = os.Exit

// taskboardExitCode is the panic RunTaskboardCLI's taskboardExit raises.
type taskboardExitCode int

// RunTaskboardCLI runs one `tclaude agent taskboard …` invocation (or, with
// args[0] "set-taskboard-writers", `tclaude agent groups set-taskboard-writers`)
// in-process and returns its exit code, its output going to stdout and
// stderr. It is the flow tests' entry into the CLI; it is not safe to run
// concurrently.
func RunTaskboardCLI(args []string, stdout, stderr io.Writer) (rc int) {
	prevExit, prevOut, prevErr := taskboardExit, cliStdout, cliStderr
	taskboardExit = func(code int) { panic(taskboardExitCode(code)) }
	cliStdout, cliStderr = stdout, stderr
	defer func() {
		taskboardExit, cliStdout, cliStderr = prevExit, prevOut, prevErr
		if r := recover(); r != nil {
			code, ok := r.(taskboardExitCode)
			if !ok {
				panic(r)
			}
			rc = int(code)
		}
	}()
	cmd := taskboardCmd()
	if len(args) > 0 && args[0] == "set-taskboard-writers" {
		cmd = groupsSetTaskboardWritersCmd()
		args = args[1:]
	}
	cmd.SetArgs(args)
	cmd.SetOut(stdout)
	cmd.SetErr(stderr)
	if err := cmd.Execute(); err != nil {
		return rcInvalidArg
	}
	return rcOK
}

// taskboardTask is the daemon's wire shape for one task.
type taskboardTask struct {
	ID               string   `json:"id"`
	Group            string   `json:"group"`
	Title            string   `json:"title"`
	Body             string   `json:"body,omitempty"`
	Stage            string   `json:"stage"`
	Status           string   `json:"status"`
	BlockReason      string   `json:"block_reason,omitempty"`
	BlockedOn        string   `json:"blocked_on,omitempty"`
	DependsOn        []string `json:"depends_on"`
	Unmet            []string `json:"unmet,omitempty"`
	Queued           bool     `json:"queued,omitempty"`
	CreatedBy        string   `json:"created_by"`
	CreatedByName    string   `json:"created_by_name,omitempty"`
	LastEditedBy     string   `json:"last_edited_by,omitempty"`
	LastEditedByName string   `json:"last_edited_by_name,omitempty"`
	LastEditedAt     string   `json:"last_edited_at,omitempty"`
	CommentCount     int      `json:"comment_count"`
	CreatedAt        string   `json:"created_at"`
	UpdatedAt        string   `json:"updated_at"`
	ClosedAt         string   `json:"closed_at,omitempty"`
	Assignee         *struct {
		AgentID string `json:"agent_id"`
		Name    string `json:"name,omitempty"`
	} `json:"assignee,omitempty"`
	AssignedBy  string `json:"assigned_by,omitempty"`
	Held        bool   `json:"held,omitempty"`
	Outcome     string `json:"outcome,omitempty"`
	OutcomeNote string `json:"outcome_note,omitempty"`
	Failures    int    `json:"failures,omitempty"`
	Stalled     bool   `json:"stalled,omitempty"`
	IdleSince   string `json:"idle_since,omitempty"`
	Ref         *struct {
		Kind  string `json:"kind"`
		URL   string `json:"url,omitempty"`
		Label string `json:"label,omitempty"`
		Local bool   `json:"local,omitempty"`
		ID    string `json:"id,omitempty"`
	} `json:"ref,omitempty"`
	Attachments []taskboardAttachment `json:"attachments,omitempty"`
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
func taskboardWrite(peer, method, path string, body any, askHuman string, asJSON bool, verb string, stdout, stderr io.Writer) int {
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	ask, rc := parseAskHumanFlag(askHuman, stdout, stderr)
	if rc != rcOK {
		return rc
	}
	var t taskboardTask
	if err := remoteRequestOpts(peer, method, path, body, &t, DaemonOpts{AskHuman: ask}); err != nil {
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
	Group string `pos:"true" optional:"true" help:"Group whose board to list (default: your group when you are in exactly one; otherwise every board you can read, briefly)"`
	Stage string `long:"stage" optional:"true" help:"Only this stage: idea or next"`
	All   bool   `long:"all" help:"Also list done and dropped tasks not yet archived"`
	Mine  bool   `long:"mine" help:"Only the live tasks assigned to you, in every group"`
	// Summary is the dashboard's counts for every board you can read, one
	// line per group (GET /v1/taskboard/context).
	Summary bool   `long:"summary" help:"Counts for every group board you can read, one line per group"`
	Peer    string `long:"peer" optional:"true" help:"List a linked node's board instead (name the group), under the grants it gave this node (taskboard.read)"`
	JSON    bool   `long:"json" help:"Output JSON"`
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
			taskboardExit(runTaskboardLs(p, cliStdout, cliStderr))
		},
	}.ToCobra()
}

func runTaskboardLs(p *taskboardLsParams, stdout, stderr io.Writer) int {
	if p.Summary {
		if strings.TrimSpace(p.Group) != "" || p.Stage != "" || p.All || p.Mine {
			fmt.Fprintln(stderr, "Error: --summary covers every board you can read; it takes no group, --stage, --all or --mine")
			return rcInvalidArg
		}
		return runTaskboardSummary(p.Peer, p.JSON, stdout, stderr)
	}
	if strings.TrimSpace(p.Peer) != "" && p.Mine {
		fmt.Fprintln(stderr, "Error: --mine lists your own tasks on this node; it takes no --peer")
		return rcInvalidArg
	}
	if strings.TrimSpace(p.Peer) != "" && strings.TrimSpace(p.Group) == "" {
		fmt.Fprintln(stderr, "Error: --peer needs the group whose board to list")
		return rcInvalidArg
	}
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
		Group       string          `json:"group"`
		Tasks       []taskboardTask `json:"tasks"`
		Locked      bool            `json:"locked"`
		Writers     []string        `json:"writers,omitempty"`
		WriterNames []string        `json:"writer_names,omitempty"`
	}
	if err := remoteRequest(p.Peer, http.MethodGet, path, nil, &resp); err != nil {
		// No group: an agent in one group lists that board; anyone else (the
		// operator, or an agent in several groups) gets every board it can
		// read, briefly. Only a plain `ls` falls back: with --stage, --all or
		// --json the caller asked for one board's rows, and with no filter the
		// own-group route's only invalid_arg is "no single group".
		var de *DaemonError
		if strings.TrimSpace(p.Group) == "" && strings.TrimSpace(p.Peer) == "" &&
			p.Stage == "" && !p.All && !p.JSON &&
			errors.As(err, &de) && de.Code == "invalid_arg" {
			return runTaskboardOverview(stdout, stderr)
		}
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
	}
	if p.JSON {
		return writeJSONTo(stdout, resp)
	}
	printRemoteScheduleBanner(stdout, p.Peer, "task board")
	if len(resp.Tasks) == 0 {
		fmt.Fprintf(stdout, "%s: the task board is empty\n", resp.Group)
		return rcOK
	}
	fmt.Fprintf(stdout, "%s task board\n", resp.Group)
	if resp.Locked {
		writers := resp.WriterNames
		if len(writers) == 0 {
			writers = resp.Writers
		}
		fmt.Fprintf(stdout, "(locked to writers: %s)\n", strings.Join(append([]string{"owners"}, writers...), ", "))
	}
	printTaskboardRows(stdout, resp.Tasks)
	return rcOK
}

// printTaskboardRows prints tasks under their section headings, numbered as
// the dashboard's list numbers them: the live tasks one sequence, the queue
// then the ideas; closed tasks unnumbered.
func printTaskboardRows(stdout io.Writer, tasks []taskboardTask) {
	section := ""
	n := 0
	for _, t := range tasks {
		closed := t.Status == "done" || t.Status == "dropped"
		s := t.Stage
		if closed {
			s = "closed"
		}
		if s != section {
			section = s
			switch s {
			case "next":
				fmt.Fprintln(stdout, "── next ──")
			case "idea":
				fmt.Fprintln(stdout, "── ideas ──")
			default:
				fmt.Fprintln(stdout, "── done / dropped ──")
			}
		}
		num := "    "
		if !closed {
			n++
			num = fmt.Sprintf("%3d.", n)
		}
		fmt.Fprintln(stdout, num+" "+taskboardRowText(t))
	}
}

// taskboardRowText is one task's line after its number.
func taskboardRowText(t taskboardTask) string {
	state := t.Status
	if t.Queued {
		state = "queued"
	}
	if t.Stalled {
		state = "stalled"
	}
	line := fmt.Sprintf("%s  %-7s  %s", t.ID, state, t.Title)
	if ref := t.refText(); ref != "" {
		line += "  [" + ref + "]"
	}
	if len(t.DependsOn) > 0 {
		line += "  after " + strings.Join(t.DependsOn, ",")
	}
	if a := t.assigneeText(); a != "" {
		line += "  @" + a
		if t.Held {
			line += " (not started)"
		}
	}
	if t.BlockReason != "" {
		line += "  — " + t.BlockReason
	}
	return line
}

// taskboardOverviewRows is how many live rows the overview shows per board.
const taskboardOverviewRows = 5

// runTaskboardOverview is `ls` with no group for a caller with no single
// group: every board it can read, its counts and its first live rows.
func runTaskboardOverview(stdout, stderr io.Writer) int {
	var resp struct {
		Groups []struct {
			Group  string          `json:"group"`
			Counts TaskboardCounts `json:"counts"`
			Tasks  []taskboardTask `json:"tasks"`
		} `json:"groups"`
	}
	if err := DaemonGet("/v1/taskboard/context", &resp); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
	}
	if len(resp.Groups) == 0 {
		fmt.Fprintln(stdout, "no task boards you can read")
		return rcOK
	}
	for i, g := range resp.Groups {
		if i > 0 {
			fmt.Fprintln(stdout)
		}
		fmt.Fprintf(stdout, "%s: %s\n", g.Group, taskboardCountsLine(g.Counts))
		rows := g.Tasks[:min(len(g.Tasks), taskboardOverviewRows)]
		printTaskboardRows(stdout, rows)
		if more := g.Counts.Live - len(rows); more > 0 {
			fmt.Fprintf(stdout, "  … %d more: tclaude agent taskboard ls %s\n", more, g.Group)
		}
	}
	fmt.Fprintln(stdout, "\nOne board in full: tclaude agent taskboard ls <group>")
	return rcOK
}

// TaskboardCounts are one board's counts as GET /v1/taskboard/context
// reports them.
type TaskboardCounts struct {
	Live         int `json:"live"`
	NeedsYou     int `json:"needs_you"`
	InProgress   int `json:"in_progress"`
	Next         int `json:"next"`
	Ideas        int `json:"ideas"`
	Blocked      int `json:"blocked"`
	Stalled      int `json:"stalled"`
	WaitingOnYou int `json:"waiting_on_you"`
	IdeasForYou  int `json:"ideas_for_you,omitempty"`
	Queued       int `json:"queued"`
	DoneThisWeek int `json:"done_this_week"`
	Dropped      int `json:"dropped"`
}

// TaskboardSummaryGroup is one readable group in `ls --summary`.
type TaskboardSummaryGroup struct {
	Group  string          `json:"group"`
	Counts TaskboardCounts `json:"counts"`
}

// RunTaskboardSummary prints the counts of every board the caller can read,
// the same counts as the dashboard's Task board sections (`ls --summary`).
// Exported so flow tests can drive it through the agent client bridge.
func RunTaskboardSummary(asJSON bool, stdout, stderr io.Writer) int {
	return runTaskboardSummary("", asJSON, stdout, stderr)
}

func runTaskboardSummary(peer string, asJSON bool, stdout, stderr io.Writer) int {
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	var resp struct {
		Groups []TaskboardSummaryGroup `json:"groups"`
	}
	if err := remoteRequest(peer, http.MethodGet, "/v1/taskboard/context", nil, &resp); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
	}
	if resp.Groups == nil {
		resp.Groups = []TaskboardSummaryGroup{}
	}
	if asJSON {
		return writeJSONTo(stdout, resp)
	}
	printRemoteScheduleBanner(stdout, peer, "task boards")
	if len(resp.Groups) == 0 {
		fmt.Fprintln(stdout, "no task boards you can read")
		return rcOK
	}
	for _, g := range resp.Groups {
		fmt.Fprintf(stdout, "%s: %s\n", g.Group, taskboardCountsLine(g.Counts))
	}
	return rcOK
}

// taskboardCountsLine reads as the dashboard's folded section, with what
// needs you broken down: "9 · 2 needs you (1 stalled, 1 blocked) · 2 in
// progress · 6 next · 3 ideas · 12 done this week".
func taskboardCountsLine(c TaskboardCounts) string {
	parts := []string{fmt.Sprintf("%d", c.Live)}
	add := func(n int, label string) {
		if n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", n, label))
		}
	}
	if c.NeedsYou > 0 {
		var why []string
		if c.Stalled > 0 {
			why = append(why, fmt.Sprintf("%d stalled", c.Stalled))
		}
		if c.Blocked > 0 {
			why = append(why, fmt.Sprintf("%d blocked", c.Blocked))
		}
		if c.WaitingOnYou > 0 {
			why = append(why, fmt.Sprintf("%d waiting on you", c.WaitingOnYou))
		}
		label := "needs you"
		if len(why) > 0 {
			label += " (" + strings.Join(why, ", ") + ")"
		}
		add(c.NeedsYou, label)
	}
	add(c.InProgress, "in progress")
	add(c.Next, "next")
	if c.Ideas == 1 {
		add(c.Ideas, "idea")
	} else {
		add(c.Ideas, "ideas")
	}
	if c.IdeasForYou == 1 {
		add(c.IdeasForYou, "idea for you")
	} else {
		add(c.IdeasForYou, "ideas for you")
	}
	add(c.DoneThisWeek, "done this week")
	add(c.Dropped, "dropped")
	return strings.Join(parts, " · ")
}

// ---- show / history ----

type taskboardShowParams struct {
	Task string `pos:"true" help:"Task id (tk_…)"`
	Peer string `long:"peer" optional:"true" help:"A task on a linked node's board, under the grants it gave this node (taskboard.read)"`
	JSON bool   `long:"json" help:"Output JSON"`
}

func taskboardShowCmd() *cobra.Command {
	return boa.CmdT[taskboardShowParams]{
		Use:         "show",
		Short:       "Show one task",
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(p *taskboardShowParams, _ *cobra.Command, _ []string) {
			taskboardExit(runTaskboardShow(p, cliStdout, cliStderr))
		},
	}.ToCobra()
}

func runTaskboardShow(p *taskboardShowParams, stdout, stderr io.Writer) int {
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	var t taskboardTask
	if err := remoteRequest(p.Peer, http.MethodGet, taskboardPath(p.Task, ""), nil, &t); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
	}
	if p.JSON {
		return writeJSONTo(stdout, t)
	}
	printRemoteScheduleBanner(stdout, p.Peer, "task")
	fmt.Fprintf(stdout, "%s  %s\n", t.ID, t.Title)
	fmt.Fprintf(stdout, "  group:   %s\n  stage:   %s\n  status:  %s\n", t.Group, t.Stage, t.Status)
	if t.BlockReason != "" {
		fmt.Fprintf(stdout, "  blocked: %s\n", t.BlockReason)
	}
	if t.BlockedOn != "" {
		fmt.Fprintf(stdout, "  waits on: %s (closing it as done reopens this task)\n", t.BlockedOn)
	}
	if t.Stalled {
		fmt.Fprintf(stdout, "  stalled: its assignee has been idle since %s without reporting\n", t.IdleSince)
	}
	if t.Ref != nil {
		fmt.Fprintf(stdout, "  ref:     %s\n", strings.Join(nonEmpty(t.Ref.Kind, t.Ref.Label, t.Ref.URL), " "))
	}
	if a := t.assigneeText(); a != "" {
		fmt.Fprintf(stdout, "  assigned: %s (by %s)\n", a, t.AssignedBy)
	}
	if t.Outcome != "" {
		report := t.Outcome
		if t.OutcomeNote != "" {
			report += " — " + t.OutcomeNote
		}
		fmt.Fprintf(stdout, "  last report: %s\n", report)
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
	if len(t.Attachments) > 0 {
		fmt.Fprintf(stdout, "  files:   %d (save them: tclaude agent taskboard attachments %s --save <dir>)\n", len(t.Attachments), t.ID)
		printTaskboardAttachments(stdout, "    ", t.Attachments)
	}
	fmt.Fprintf(stdout, "  created: %s by %s\n", t.CreatedAt, actorText(t.CreatedBy, t.CreatedByName))
	if t.LastEditedBy != "" {
		fmt.Fprintf(stdout, "  edited:  %s by %s\n", t.LastEditedAt, actorText(t.LastEditedBy, t.LastEditedByName))
	}
	if t.CommentCount > 0 {
		fmt.Fprintf(stdout, "  comments: %d (tclaude agent taskboard activity %s)\n", t.CommentCount, t.ID)
	}
	if t.ClosedAt != "" {
		fmt.Fprintf(stdout, "  closed:  %s\n", t.ClosedAt)
	}
	if t.Body != "" {
		fmt.Fprintf(stdout, "\n%s\n", t.Body)
	}
	return rcOK
}

// ---- add / edit ----

type taskboardAddParams struct {
	Group    string `pos:"true" help:"Group whose board to add to"`
	Title    string `pos:"true" optional:"true" help:"Task title (one line, up to 200 bytes); not with an import flag"`
	Body     string `long:"body" optional:"true" help:"Task brief (up to 8 KiB)"`
	BodyFile string `long:"body-file" optional:"true" help:"Read the brief from this file ('-' for stdin)"`
	Ref      string `long:"ref" optional:"true" help:"External ref: an http(s) URL, owner/repo#n, or a tracker id (tcl-a1b2c3 for awb, LIN-123 for Linear; prefix awb: or linear: to say which)"`
	RefLabel string `long:"ref-label" optional:"true" help:"Display label for the ref (default: derived)"`
	// ResolveRef is the dashboard's New task form's ref lookup.
	ResolveRef bool   `long:"resolve-ref" help:"Look a bare awb or Linear --ref up through its tracker proxy to fill in its link, as an import does (the title stays yours)"`
	Next       bool   `long:"next" help:"Add to the queue (next) instead of the ideas (unassigned: needs taskboard.write); with --assignee or --mine, queued for them, not started"`
	Idea       bool   `long:"idea" help:"With --assignee or --mine: an idea in their lane for them to triage, not started"`
	Mine       bool   `long:"mine" help:"Assign it to yourself as it is created (in progress); what an add under taskboard.own alone always does"`
	After      string `long:"after" optional:"true" help:"Comma-separated task ids this task depends on"`
	Top        bool   `long:"top" help:"Place at the top of its stage instead of the end"`
	Before     string `long:"before" optional:"true" help:"Place just before this task (and in its stage)"`
	// Start state: the task is created in it, in one call.
	Assignee string `long:"assignee" optional:"true" help:"Assign it as it is created (name or id of an active member, or operator for the operator of the board's node): it starts in progress, in next, unless --idea or --next holds it there, not started; with --done, who did it. Someone else needs taskboard.assign; asking the operator about your own work needs only taskboard.own"`
	Blocks   string `long:"blocks" optional:"true" help:"With --assignee operator: this task of the board (tk_…, typically yours) waits on the new one. It gains it as a dependency and is blocked on it, in the same write; closing the operator task as done reopens it"`
	Blocked  string `long:"blocked" optional:"true" help:"Start it blocked, with this reason"`
	Done     bool   `long:"done" help:"Log work already done: it is created done, in done this week (with --assignee, who did it; nobody is mailed)"`
	// Import flags: one task per ref or line, titles looked up through the
	// tracker proxies.
	FromAWB    string   `long:"from-awb" optional:"true" help:"Import awb issues: comma-separated ids (tcl-a1b2c3,…), one task each"`
	FromLinear string   `long:"from-linear" optional:"true" help:"Import Linear issues: comma-separated ids (LIN-1,…), one task each"`
	FromGitHub string   `long:"from-github" optional:"true" help:"Import GitHub issues or pull requests: comma-separated owner/repo#n or URLs, one task each"`
	LinesFile  string   `long:"lines-file" optional:"true" help:"Add one task per line of this file ('-' for stdin), skipping blank lines and '#' comments; a line that is only an awb, Linear or GitHub id becomes a task with that ref"`
	Attach     []string `long:"attach" optional:"true" help:"Attach a file you can read (repeatable; at most 10, 4 MiB each; images, text, data and PDF). The task and its files land together"`
	Peer       string   `long:"peer" optional:"true" help:"Act on a linked node's board instead, under the grants it gave this node (taskboard.read / taskboard.write)"`
	AskHuman   string   `long:"ask-human" optional:"true" help:"On permission denial, ask the human via popup with this timeout (e.g. '30s'). Capped at 300s. Timeout = deny."`
	JSON       bool     `long:"json" help:"Output JSON"`
}

func taskboardAddCmd() *cobra.Command {
	return boa.CmdT[taskboardAddParams]{
		Use:   "add",
		Short: "Add a task, or import refs or lines (lands in the ideas unless --next)",
		Long: "Add one task with a title, or import several. With taskboard.write a task lands unassigned in the ideas " +
			"(or the queue with --next), unless --mine or --assignee assigns it. A member without taskboard.write adds under " +
			"taskboard.own: the task is assigned to it as it is created and starts in progress (or blocked with --blocked, " +
			"done with --done); an unassigned task (--next, or an idea) and an import need taskboard.write. Imports: --from-awb, --from-linear and --from-github add one task per " +
			"ref, titled from the tracker through its tclaude proxy (you need the matching proxy.*.read; without it, or when " +
			"no proxy is configured or the lookup fails, the id becomes the title and the output says why). --lines-file adds " +
			"one task per line. A ref already on the board is refused on its own; the rest of the import still lands. The exit " +
			"code is non-zero when any item was refused.",
		ParamEnrich: common.DefaultParamEnricher(),
		InitFuncCtx: func(ctx *boa.HookContext, p *taskboardAddParams, _ *cobra.Command) error {
			boa.GetParamT(ctx, &p.Group).SetAlternativesFunc(completeGroupNames)
			boa.GetParamT(ctx, &p.AskHuman).SetAlternativesFunc(completeAskHumanDurations)
			return nil
		},
		RunFunc: func(p *taskboardAddParams, _ *cobra.Command, _ []string) {
			taskboardExit(runTaskboardAdd(p, os.Stdin, cliStdout, cliStderr))
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
	if p.FromAWB != "" || p.FromLinear != "" || p.FromGitHub != "" || p.LinesFile != "" {
		if p.Assignee != "" || p.Mine || p.Blocked != "" || p.Done || len(p.Attach) > 0 {
			fmt.Fprintln(stderr, "Error: an import takes only --next; --assignee, --mine, --blocked, --done and --attach are for one task")
			return rcInvalidArg
		}
		return runTaskboardImport(p, stdin, stdout, stderr)
	}
	if strings.TrimSpace(p.Title) == "" {
		fmt.Fprintln(stderr, "Error: give a title, or one of --from-awb, --from-linear, --from-github, --lines-file")
		return rcInvalidArg
	}
	body, _, err := readTaskBody(p.Body, p.BodyFile, stdin)
	if err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return rcInvalidArg
	}
	if p.Top && p.Before != "" {
		fmt.Fprintln(stderr, "Error: give --top or --before, not both")
		return rcInvalidArg
	}
	if p.Done && strings.TrimSpace(p.Blocked) != "" {
		fmt.Fprintln(stderr, "Error: give --blocked or --done, not both")
		return rcInvalidArg
	}
	req := map[string]any{"title": p.Title, "body": body, "ref": p.Ref, "ref_label": p.RefLabel,
		"after": splitTaskIDs(p.After), "position": map[string]any{"top": p.Top, "before": p.Before}}
	if p.Next && p.Idea {
		fmt.Fprintln(stderr, "Error: give --next or --idea, not both")
		return rcInvalidArg
	}
	assigned := p.Mine || strings.TrimSpace(p.Assignee) != ""
	switch {
	case p.Next:
		req["stage"] = "next"
	case p.Idea:
		req["stage"] = "idea"
	}
	if assigned && (p.Next || p.Idea) {
		req["start"] = false
	}
	if p.ResolveRef {
		req["resolve_ref"] = true
	}
	if p.Mine && strings.TrimSpace(p.Assignee) != "" {
		fmt.Fprintln(stderr, "Error: give --mine or --assignee, not both")
		return rcInvalidArg
	}
	if a := strings.TrimSpace(p.Assignee); a != "" {
		req["to"] = a
	}
	if p.Mine {
		req["mine"] = true
	}
	if b := strings.TrimSpace(p.Blocks); b != "" {
		req["blocks"] = b
	}
	switch {
	case strings.TrimSpace(p.Blocked) != "":
		req["status"], req["block_reason"] = "blocked", strings.TrimSpace(p.Blocked)
	case p.Done:
		req["status"] = "done"
	}
	path := "/v1/groups/" + url.PathEscape(strings.TrimSpace(p.Group)) + "/taskboard/tasks"
	if len(p.Attach) == 0 {
		return taskboardWrite(p.Peer, http.MethodPost, path, req, p.AskHuman, p.JSON, "added", stdout, stderr)
	}
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	ask, rc := parseAskHumanFlag(p.AskHuman, stdout, stderr)
	if rc != rcOK {
		return rc
	}
	var t taskboardTask
	if err := taskboardUploadFiles(p.Peer, path, req, p.Attach, &t, ask); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
	}
	if p.JSON {
		return writeJSONTo(stdout, t)
	}
	fmt.Fprintf(stdout, "added %s: %s  [%s, %s]\n", t.ID, t.Title, t.Stage, t.Status)
	printTaskboardAttachments(stdout, "  ", t.Attachments)
	return rcOK
}

// ---- ask ----

type taskboardAskParams struct {
	Group      string   `pos:"true" help:"Group whose board the tasks go on"`
	Prompt     string   `pos:"true" optional:"true" help:"What you want, in your own words"`
	To         string   `long:"to" help:"Member to ask (name or id): an active member of the group who may change its board"`
	PromptFile string   `long:"prompt-file" optional:"true" help:"Read the request from this file ('-' for stdin)"`
	Attach     []string `long:"attach" short:"a" optional:"true" help:"Send a file you can read with the request, as a mail attachment the member puts on its tasks (repeatable; at most 10, 4 MiB each)"`
	Peer       string   `long:"peer" optional:"true" help:"Ask a member on a linked node's board, under the grants it gave this node (taskboard.write)"`
	AskHuman   string   `long:"ask-human" optional:"true" help:"On permission denial, ask the human via popup with this timeout (e.g. '30s'). Capped at 300s. Timeout = deny."`
	JSON       bool     `long:"json" help:"Output JSON"`
}

func taskboardAskCmd() *cobra.Command {
	return boa.CmdT[taskboardAskParams]{
		Use:   "ask",
		Short: "Ask a member to turn a request into tasks on the board (inbox mail)",
		Long: "Mail a member of the group a request in prose. It is told to turn the request into well-formed tasks " +
			"with `tclaude agent taskboard add`, to reply when done, and not to start the work. The request is inbox " +
			"mail, never keys typed into its pane. It needs taskboard.write on the group, as add does, and the member " +
			"must be able to change the board (the writer lock). This is the dashboard's + New task › Ask an agent.",
		ParamEnrich: common.DefaultParamEnricher(),
		InitFuncCtx: func(ctx *boa.HookContext, p *taskboardAskParams, _ *cobra.Command) error {
			boa.GetParamT(ctx, &p.Group).SetAlternativesFunc(completeGroupNames)
			boa.GetParamT(ctx, &p.AskHuman).SetAlternativesFunc(completeAskHumanDurations)
			return nil
		},
		RunFunc: func(p *taskboardAskParams, _ *cobra.Command, _ []string) {
			taskboardExit(runTaskboardAsk(p, os.Stdin, cliStdout, cliStderr))
		},
	}.ToCobra()
}

func runTaskboardAsk(p *taskboardAskParams, stdin io.Reader, stdout, stderr io.Writer) int {
	if strings.TrimSpace(p.To) == "" {
		fmt.Fprintln(stderr, "Error: --to is required: the member to ask")
		return rcInvalidArg
	}
	if p.Prompt != "" && p.PromptFile != "" {
		fmt.Fprintln(stderr, "Error: give the request or --prompt-file, not both")
		return rcInvalidArg
	}
	prompt, _, err := readTaskBody(p.Prompt, p.PromptFile, stdin)
	if err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return rcInvalidArg
	}
	if strings.TrimSpace(prompt) == "" {
		fmt.Fprintln(stderr, "Error: give the request, as an argument or with --prompt-file")
		return rcInvalidArg
	}
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	ask, rc := parseAskHumanFlag(p.AskHuman, stdout, stderr)
	if rc != rcOK {
		return rc
	}
	var out struct {
		Group     string `json:"group"`
		To        string `json:"to"`
		MessageID int64  `json:"message_id"`
	}
	path := "/v1/groups/" + url.PathEscape(strings.TrimSpace(p.Group)) + "/taskboard/ask"
	req := map[string]any{"to": p.To, "prompt": prompt}
	if len(p.Attach) > 0 {
		err = taskboardUploadFiles(p.Peer, path, req, p.Attach, &out, ask)
	} else {
		err = remoteRequestOpts(p.Peer, http.MethodPost, path, req, &out, DaemonOpts{AskHuman: ask})
	}
	if err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
	}
	if p.JSON {
		return writeJSONTo(stdout, out)
	}
	fmt.Fprintf(stdout, "asked %s for tasks on %s (message #%d); it replies when they are on the board\n", out.To, out.Group, out.MessageID)
	return rcOK
}

// taskboardImportResult is one item of an import's answer.
type taskboardImportResult struct {
	Input    string         `json:"input"`
	Task     *taskboardTask `json:"task,omitempty"`
	Resolved bool           `json:"resolved,omitempty"`
	Note     string         `json:"note,omitempty"`
	Error    string         `json:"error,omitempty"`
	Code     string         `json:"code,omitempty"`
}

// importRefs splits a comma-separated --from-* value, giving each bare id the
// tracker prefix so the daemon cannot read it as another kind. URLs and
// GitHub's owner/repo#n pass through.
func importRefs(list, prefix string) []string {
	var out []string
	for _, ref := range splitTaskIDs(list) {
		lower := strings.ToLower(ref)
		if prefix != "" && !strings.HasPrefix(lower, "http://") && !strings.HasPrefix(lower, "https://") &&
			!strings.HasPrefix(lower, prefix) {
			ref = prefix + ref
		}
		out = append(out, ref)
	}
	return out
}

// readImportLines reads --lines-file ('-' is stdin), one task per line,
// skipping blank lines and lines starting with '#'.
func readImportLines(file string, stdin io.Reader) ([]string, error) {
	var (
		b   []byte
		err error
	)
	const limit = 48 << 10
	src := stdin
	if file != "-" {
		f, err := os.Open(file)
		if err != nil {
			return nil, err
		}
		defer func() { _ = f.Close() }()
		src = f
	}
	if b, err = io.ReadAll(io.LimitReader(src, limit+1)); err != nil {
		return nil, err
	}
	if len(b) > limit {
		return nil, fmt.Errorf("the lines are over %d KiB; an import is for the next few tasks", limit>>10)
	}
	var lines []string
	for line := range strings.SplitSeq(string(b), "\n") {
		// Blank lines and '#' comments are skipped, so a list can be kept
		// as a commented file.
		if line = strings.TrimSpace(line); line != "" && !strings.HasPrefix(line, "#") {
			lines = append(lines, line)
		}
	}
	return lines, nil
}

func runTaskboardImport(p *taskboardAddParams, stdin io.Reader, stdout, stderr io.Writer) int {
	if p.Title != "" || p.Body != "" || p.BodyFile != "" || p.Ref != "" || p.RefLabel != "" || p.After != "" ||
		p.Top || p.Before != "" {
		fmt.Fprintln(stderr, "Error: an import (--from-*, --lines-file) takes no title, --body, --body-file, --ref, "+
			"--ref-label, --after, --top or --before; only --next")
		return rcInvalidArg
	}
	refs := importRefs(p.FromAWB, "awb:")
	refs = append(refs, importRefs(p.FromLinear, "linear:")...)
	refs = append(refs, importRefs(p.FromGitHub, "")...)
	var lines []string
	if p.LinesFile != "" {
		var err error
		if lines, err = readImportLines(p.LinesFile, stdin); err != nil {
			fmt.Fprintf(stderr, "Error: %v\n", err)
			return rcIOFailure
		}
	}
	if len(refs) == 0 && len(lines) == 0 {
		fmt.Fprintln(stderr, "Error: nothing to import")
		return rcInvalidArg
	}
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	ask, rc := parseAskHumanFlag(p.AskHuman, stdout, stderr)
	if rc != rcOK {
		return rc
	}
	req := map[string]any{"refs": refs, "lines": lines}
	if p.Next {
		req["stage"] = "next"
	}
	var resp struct {
		Group   string                  `json:"group"`
		Created int                     `json:"created"`
		Results []taskboardImportResult `json:"results"`
	}
	path := "/v1/groups/" + url.PathEscape(strings.TrimSpace(p.Group)) + "/taskboard/tasks"
	// The daemon spends up to a minute on title lookups; wait past that.
	opts := DaemonOpts{AskHuman: ask, Timeout: 90 * time.Second}
	if err := remoteRequestOpts(p.Peer, http.MethodPost, path, req, &resp, opts); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
	}
	refused := 0
	for _, r := range resp.Results {
		if r.Error != "" {
			refused++
		}
	}
	if p.JSON {
		if rc := writeJSONTo(stdout, resp); rc != rcOK || refused == 0 {
			return rc
		}
		return rcInvalidArg
	}
	for _, r := range resp.Results {
		switch {
		case r.Error != "":
			fmt.Fprintf(stdout, "refused  %s: %s\n", r.Input, r.Error)
		case r.Task != nil:
			line := fmt.Sprintf("added %s: %s  [%s]", r.Task.ID, r.Task.Title, r.Task.Stage)
			if ref := r.Task.refText(); ref != "" && ref != r.Task.Title {
				line += "  " + ref
			}
			if r.Note != "" {
				line += "  (title not looked up: " + r.Note + ")"
			}
			fmt.Fprintln(stdout, line)
		}
	}
	fmt.Fprintf(stdout, "%d added to %s, %d refused\n", resp.Created, resp.Group, refused)
	if refused > 0 {
		return rcInvalidArg
	}
	return rcOK
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
	// ResolveRef is add's --resolve-ref, for a new ref or the task's own.
	ResolveRef bool   `long:"resolve-ref" help:"Look a bare awb or Linear ref up through its tracker proxy to fill in its link: the new --ref, or without --ref the task's own link-less one"`
	AddDep     string `long:"add-dep" optional:"true" help:"Comma-separated task ids to depend on"`
	RmDep      string `long:"rm-dep" optional:"true" help:"Comma-separated task ids to stop depending on"`
	Peer       string `long:"peer" optional:"true" help:"Act on a linked node's board instead, under the grants it gave this node (taskboard.read / taskboard.write)"`
	AskHuman   string `long:"ask-human" optional:"true" help:"On permission denial, ask the human via popup with this timeout (e.g. '30s'). Capped at 300s. Timeout = deny."`
	JSON       bool   `long:"json" help:"Output JSON"`
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
			taskboardExit(runTaskboardEdit(p, os.Stdin, cliStdout, cliStderr))
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
	if p.ResolveRef {
		if p.ClearRef {
			fmt.Fprintln(stderr, "Error: give --clear-ref or --resolve-ref, not both")
			return rcInvalidArg
		}
		req["resolve_ref"] = true
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
	return taskboardWrite(p.Peer, http.MethodPatch, taskboardPath(p.Task, ""), req, p.AskHuman, p.JSON, "edited", stdout, stderr)
}

// ---- move / stage / status ----

type taskboardMoveParams struct {
	Task     string `pos:"true" help:"Task id (tk_…)"`
	Top      bool   `long:"top" help:"To the top of its stage"`
	Bottom   bool   `long:"bottom" help:"To the end of its stage"`
	Before   string `long:"before" optional:"true" help:"Just before this task (crossing into the other stage promotes or demotes)"`
	After    string `long:"after" optional:"true" help:"Just after this task (crossing into the other stage promotes or demotes)"`
	Peer     string `long:"peer" optional:"true" help:"Act on a linked node's board instead, under the grants it gave this node (taskboard.read / taskboard.write)"`
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
			taskboardExit(runTaskboardMove(p, cliStdout, cliStderr))
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
	return taskboardWrite(p.Peer, http.MethodPost, taskboardPath(p.Task, "/move"), req, p.AskHuman, p.JSON, "moved", stdout, stderr)
}

// ---- move-to-group ----

type taskboardMoveToGroupParams struct {
	Task     string `pos:"true" help:"Task id (tk_…)"`
	Group    string `pos:"true" help:"Group to move the task to: a group on this node, or group@peer for one on a linked node"`
	Clone    bool   `long:"clone" help:"Clone it instead: a new task with the same title, brief and ref; the original stays"`
	Stage    string `long:"stage" optional:"true" help:"Where it lands: idea (the default) or next"`
	AskHuman string `long:"ask-human" optional:"true" help:"On permission denial, ask the human via popup with this timeout (e.g. '30s'). Capped at 300s. Timeout = deny."`
	JSON     bool   `long:"json" help:"Output JSON"`
}

// taskboardTransfer is POST /v1/taskboard/tasks/{id}/move-to-group's answer.
type taskboardTransfer struct {
	Task              taskboardTask `json:"task"`
	Group             string        `json:"group"`
	From              string        `json:"from"`
	Clone             bool          `json:"clone"`
	DroppedDeps       []string      `json:"dropped_deps,omitempty"`
	DroppedDependents []string      `json:"dropped_dependents,omitempty"`
	Unassigned        string        `json:"unassigned,omitempty"`
}

func taskboardMoveToGroupCmd() *cobra.Command {
	return boa.CmdT[taskboardMoveToGroupParams]{
		Use:   "move-to-group",
		Short: "Move or clone a task to another group's board",
		Long: "Move a task to another group's board, on this node or a linked one, or with --clone copy its title, brief and ref there.\n\n" +
			"A move keeps the task and its history. Its dependencies are dropped (they never cross groups), an assignee " +
			"who is not a member of the new group is unassigned, and anything but a done task lands open. A clone has no " +
			"assignee and no dependencies.\n\n" +
			"Needs taskboard.transfer on the target group (its owners have it). A move also needs taskboard.write on the " +
			"source when you hold the task, taskboard.manage there otherwise; a clone needs taskboard.read on the source.\n\n" +
			"GROUP@PEER moves it to a linked node's group, under the taskboard.transfer grant that node gave this one: the " +
			"task is created there first (title, brief and ref, with a note where it came from), and only then dropped here " +
			"with a \"moved to PEER/GROUP\" note. Its history, assignee and dependencies stay behind. If the create fails " +
			"nothing changes; if the drop fails it is retried once, then reported, with both copies live.",
		ParamEnrich: common.DefaultParamEnricher(),
		InitFuncCtx: func(ctx *boa.HookContext, p *taskboardMoveToGroupParams, _ *cobra.Command) error {
			boa.GetParamT(ctx, &p.AskHuman).SetAlternativesFunc(completeAskHumanDurations)
			return nil
		},
		RunFunc: func(p *taskboardMoveToGroupParams, _ *cobra.Command, _ []string) {
			taskboardExit(runTaskboardMoveToGroup(p, cliStdout, cliStderr))
		},
	}.ToCobra()
}

func runTaskboardMoveToGroup(p *taskboardMoveToGroupParams, stdout, stderr io.Writer) int {
	if p.Stage != "" && p.Stage != "idea" && p.Stage != "next" {
		fmt.Fprintln(stderr, "Error: --stage is idea or next")
		return rcInvalidArg
	}
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	ask, rc := parseAskHumanFlag(p.AskHuman, stdout, stderr)
	if rc != rcOK {
		return rc
	}
	if group, peer, ok := strings.Cut(p.Group, "@"); ok {
		return runTaskboardMoveToPeer(p, strings.TrimSpace(group), strings.TrimSpace(peer), ask, stdout, stderr)
	}
	req := map[string]any{"group": p.Group, "clone": p.Clone, "stage": p.Stage}
	var out taskboardTransfer
	if err := remoteRequestOpts("", http.MethodPost, taskboardPath(p.Task, "/move-to-group"), req, &out, DaemonOpts{AskHuman: ask}); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
	}
	if p.JSON {
		return writeJSONTo(stdout, out)
	}
	if out.Clone {
		fmt.Fprintf(stdout, "cloned %s to %s as %s: %s  [%s]\n", p.Task, out.Group, out.Task.ID, out.Task.Title, out.Task.Stage)
		return rcOK
	}
	fmt.Fprintf(stdout, "moved %s from %s to %s: %s  [%s, %s]\n", out.Task.ID, out.From, out.Group, out.Task.Title, out.Task.Stage, out.Task.Status)
	if dropped := append(append([]string{}, out.DroppedDeps...), out.DroppedDependents...); len(dropped) > 0 {
		fmt.Fprintf(stdout, "  dependencies dropped: %s\n", strings.Join(dropped, " "))
	}
	if out.Unassigned != "" {
		fmt.Fprintf(stdout, "  unassigned %s (not a member of %s)\n", out.Unassigned, out.Group)
	}
	return rcOK
}

// runTaskboardMoveToPeer moves or clones a task of this node to group on
// linked node peer: create there (transfer-in), then drop it here
// (moved-away).
func runTaskboardMoveToPeer(p *taskboardMoveToGroupParams, group, peer string, ask time.Duration, stdout, stderr io.Writer) int {
	if group == "" || peer == "" {
		fmt.Fprintln(stderr, "Error: give the target as group@peer")
		return rcInvalidArg
	}
	var t taskboardTask
	if err := remoteRequest("", http.MethodGet, taskboardPath(p.Task, ""), nil, &t); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
	}
	if !p.Clone && t.Status != "open" && t.Status != "blocked" {
		fmt.Fprintf(stderr, "Error: %s is %s; only an open or blocked task moves to another node (--clone copies it)\n", t.ID, t.Status)
		return rcInvalidArg
	}
	stage := p.Stage
	if stage == "" {
		stage = "idea"
	}
	in := map[string]any{"title": t.Title, "body": t.Body, "stage": stage, "clone": p.Clone,
		"origin": map[string]any{"group": t.Group, "task": t.ID}}
	if t.Ref != nil {
		switch {
		case t.Ref.Local && t.Ref.ID != "":
			// Linked on this node's awb server: the peer links the id on its own.
			in["ref"] = "awb:" + t.Ref.ID
			if t.Ref.Label != t.Ref.ID {
				in["ref_label"] = t.Ref.Label
			}
		case t.Ref.URL != "":
			in["ref"], in["ref_label"] = t.Ref.URL, t.Ref.Label
		default:
			in["ref"] = t.Ref.Label
		}
	}
	awayPath := taskboardPath(t.ID, "/moved-away")
	if !p.Clone {
		// Ask the drop's gates here first, so a refusal is known before the
		// task exists on the peer.
		if err := remoteRequestOpts("", http.MethodPost, awayPath, map[string]any{"check": true}, nil, DaemonOpts{AskHuman: ask}); err != nil {
			fmt.Fprintf(stderr, "Error: %s cannot be dropped here, so it is not moved (nothing changed): %v\n", t.ID, err)
			return MapDaemonErrorToRC(err)
		}
	}
	// The task's files go with it, in the same request.
	var files []taskboardFile
	for _, a := range t.Attachments {
		data, err := fetchTaskboardAttachment("", t.ID, a.ID)
		if err != nil {
			fmt.Fprintf(stderr, "Error: read file %s of %s (nothing changed): %v\n", a.Name, t.ID, err)
			return MapDaemonErrorToRC(err)
		}
		files = append(files, taskboardFile{Name: a.Name, Data: data.Data})
	}
	var created taskboardTask
	path := "/v1/groups/" + url.PathEscape(group) + "/taskboard/transfer-in"
	var err error
	if len(files) > 0 {
		err = taskboardUpload(peer, path, in, files, &created, ask)
	} else {
		err = remoteRequestOpts(peer, http.MethodPost, path, in, &created, DaemonOpts{AskHuman: ask})
	}
	if err != nil {
		fmt.Fprintf(stderr, "Error: %s on %s did not take it: %v\n", group, peer, err)
		var de *DaemonError
		if !errors.As(err, &de) || de.Status >= 500 {
			fmt.Fprintf(stderr, "If the request timed out, %s may have the task already: check it (taskboard ls %s --peer %s) before retrying.\n", peer, group, peer)
		} else {
			fmt.Fprintln(stderr, "Nothing changed.")
		}
		return MapDaemonErrorToRC(err)
	}
	out := map[string]any{"task": t.ID, "created": created, "group": group, "peer": peer, "clone": p.Clone}
	if p.Clone {
		if p.JSON {
			return writeJSONTo(stdout, out)
		}
		fmt.Fprintf(stdout, "cloned %s to %s on %s as %s: %s  [%s]\n", t.ID, group, peer, created.ID, created.Title, created.Stage)
		return rcOK
	}
	away := map[string]any{"to": map[string]any{"node": peer, "group": group, "task": created.ID}}
	for attempt := 0; attempt < 2; attempt++ {
		err = remoteRequestOpts("", http.MethodPost, awayPath, away, nil, DaemonOpts{AskHuman: ask})
		var de *DaemonError
		if err == nil {
			break
		}
		if errors.As(err, &de) {
			// A retry that finds it closed means the first try landed and
			// only its answer was lost.
			if attempt > 0 && de.Status == http.StatusConflict {
				err = nil
				break
			}
			if de.Status < 500 {
				break // a refusal: retrying will not change it
			}
		}
	}
	if err != nil {
		fmt.Fprintf(stderr, "Error: created %s on %s/%s, but could not drop %s here: %v\n"+
			"Both copies are live; drop this one with: tclaude agent taskboard drop %s --reason \"moved to %s/%s\"\n",
			created.ID, peer, group, t.ID, err, t.ID, peer, group)
		return MapDaemonErrorToRC(err)
	}
	if p.JSON {
		return writeJSONTo(stdout, out)
	}
	fmt.Fprintf(stdout, "moved %s to %s on %s as %s: %s  [%s]; dropped here\n", t.ID, group, peer, created.ID, created.Title, created.Stage)
	return rcOK
}

type taskboardTaskParams struct {
	Task     string `pos:"true" help:"Task id (tk_…)"`
	Peer     string `long:"peer" optional:"true" help:"Act on a linked node's board instead, under the grants it gave this node (taskboard.read / taskboard.write)"`
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
			taskboardExit(taskboardWrite(p.Peer, http.MethodPost, taskboardPath(p.Task, "/stage"), map[string]any{"stage": stage},
				p.AskHuman, p.JSON, use+"d", cliStdout, cliStderr))
		},
	}.ToCobra()
}

type taskboardStatusParams struct {
	Task     string `pos:"true" help:"Task id (tk_…)"`
	Reason   string `long:"reason" optional:"true" help:"Why (kept in the task's history)"`
	Peer     string `long:"peer" optional:"true" help:"Act on a linked node's board instead, under the grants it gave this node (taskboard.read / taskboard.write)"`
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
				taskboardExit(taskboardWrite(p.Peer, http.MethodPost, taskboardPath(p.Task, "/status"), map[string]any{"status": status},
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
			taskboardExit(taskboardWrite(p.Peer, http.MethodPost, taskboardPath(p.Task, "/status"),
				map[string]any{"status": status, "reason": p.Reason}, p.AskHuman, p.JSON, verbs[use], cliStdout, cliStderr))
		},
	}.ToCobra()
}

type taskboardBlockParams struct {
	Task     string `pos:"true" help:"Task id (tk_…)"`
	Reason   string `long:"reason" optional:"true" help:"Why the task is blocked"`
	On       string `long:"on" optional:"true" help:"Block it waiting on this task of the board (tk_…, typically one assigned to the operator), instead of a reason: it becomes a dependency, and closing it as done reopens this task"`
	Peer     string `long:"peer" optional:"true" help:"Act on a linked node's board instead, under the grants it gave this node (taskboard.read / taskboard.write)"`
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
			reason, on := strings.TrimSpace(p.Reason), strings.TrimSpace(p.On)
			if (reason == "") == (on == "") {
				fmt.Fprintln(cliStderr, "Error: give --reason or --on, one of them")
				taskboardExit(rcInvalidArg)
				return
			}
			req := map[string]any{"status": "blocked", "reason": reason}
			if on != "" {
				req = map[string]any{"status": "blocked", "on": on}
			}
			taskboardExit(taskboardWrite(p.Peer, http.MethodPost, taskboardPath(p.Task, "/status"),
				req, p.AskHuman, p.JSON, "blocked", cliStdout, cliStderr))
		},
	}.ToCobra()
}

type taskboardReopenParams struct {
	Task     string `pos:"true" help:"Task id (tk_…) of a done or dropped task"`
	Next     bool   `long:"next" help:"Reopen it in the queue (next) instead of the stage it had"`
	Idea     bool   `long:"idea" help:"Reopen it as an idea (an assigned task needs --unassign too)"`
	Assignee string `long:"assignee" optional:"true" help:"Reopen it assigned to this member (name or id), or operator; the default keeps whoever held it"`
	Unassign bool   `long:"unassign" help:"Reopen it with no assignee"`
	Peer     string `long:"peer" optional:"true" help:"Act on a linked node's board instead, under the grants it gave this node (taskboard.write; taskboard.assign to change the assignee)"`
	AskHuman string `long:"ask-human" optional:"true" help:"On permission denial, ask the human via popup with this timeout (e.g. '30s'). Capped at 300s. Timeout = deny."`
	JSON     bool   `long:"json" help:"Output JSON"`
}

func taskboardReopenCmd() *cobra.Command {
	return boa.CmdT[taskboardReopenParams]{
		Use:   "reopen",
		Short: "Reopen a done or dropped task, where it was or in another stage, and optionally with another assignee",
		Long: "Puts a done or dropped task back on the board: by default in the stage and place it had, with the assignee it had. " +
			"--next or --idea picks the stage, --assignee gives it to a member (or operator), --unassign takes the assignee off. " +
			"Reopening your own task needs taskboard.own; anyone else's, taskboard.write. Changing the assignee is gated as assign and unassign are.",
		ParamEnrich: common.DefaultParamEnricher(),
		InitFuncCtx: func(ctx *boa.HookContext, p *taskboardReopenParams, _ *cobra.Command) error {
			boa.GetParamT(ctx, &p.AskHuman).SetAlternativesFunc(completeAskHumanDurations)
			return nil
		},
		RunFunc: func(p *taskboardReopenParams, _ *cobra.Command, _ []string) {
			taskboardExit(runTaskboardReopen(p, cliStdout, cliStderr))
		},
	}.ToCobra()
}

func runTaskboardReopen(p *taskboardReopenParams, stdout, stderr io.Writer) int {
	assignee := strings.TrimSpace(p.Assignee)
	switch {
	case p.Next && p.Idea:
		fmt.Fprintln(stderr, "Error: give --next or --idea, not both")
		return rcInvalidArg
	case p.Unassign && assignee != "":
		fmt.Fprintln(stderr, "Error: give --assignee or --unassign, not both")
		return rcInvalidArg
	}
	req := map[string]any{}
	if p.Next {
		req["stage"] = "next"
	}
	if p.Idea {
		req["stage"] = "idea"
	}
	if assignee != "" {
		req["to"] = assignee
	}
	if p.Unassign {
		req["to"] = ""
	}
	return taskboardWrite(p.Peer, http.MethodPost, taskboardPath(p.Task, "/reopen"), req, p.AskHuman, p.JSON, "reopened", stdout, stderr)
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
	To       string `long:"to" help:"Agent to assign it to (name or id; an active member of the task's group), or operator for the operator of the board's node"`
	Hold     bool   `long:"hold" help:"Assign it without starting it: it stays in its stage (an idea to triage, or queued) in the assignee's lane; they start it"`
	Idea     bool   `long:"idea" help:"Hold it as an idea in the assignee's lane (implies --hold)"`
	Next     bool   `long:"next" help:"Hold it queued in the assignee's lane (implies --hold)"`
	Peer     string `long:"peer" optional:"true" help:"Assign on a linked node's board, to a member there, under the grants it gave this node (taskboard.assign)"`
	AskHuman string `long:"ask-human" optional:"true" help:"On permission denial, ask the human via popup with this timeout (e.g. '30s'). Capped at 300s. Timeout = deny."`
	JSON     bool   `long:"json" help:"Output JSON"`
}

func taskboardAssignCmd() *cobra.Command {
	return boa.CmdT[taskboardAssignParams]{
		Use:   "assign",
		Short: "Assign a task to a member of its group (they get an inbox brief)",
		Long: "Assign a task to an active member of the task's group. The task starts: an idea is promoted to the queue and it is in progress, " +
			"and the assignee gets an inbox message from you with the brief and the report commands. With --hold (or --idea / --next) " +
			"the assignee holds it without starting it, in its stage, and gets a short note instead; it starts with `taskboard start`. Assigning another " +
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
				taskboardExit(rcInvalidArg)
			}
			req := map[string]any{"to": p.To}
			if p.Idea && p.Next {
				fmt.Fprintln(cliStderr, "Error: give --idea or --next, not both")
				taskboardExit(rcInvalidArg)
			}
			if p.Hold || p.Idea || p.Next {
				req["start"] = false
				switch {
				case p.Idea:
					req["stage"] = "idea"
				case p.Next:
					req["stage"] = "next"
				}
			}
			taskboardExit(taskboardWrite(p.Peer, http.MethodPost, taskboardPath(p.Task, "/assign"), req,
				p.AskHuman, p.JSON, "assigned", cliStdout, cliStderr))
		},
	}.ToCobra()
}

func taskboardTakeCmd() *cobra.Command {
	return boa.CmdT[taskboardTaskParams]{
		Use:         "take",
		Short:       "Take an open, unassigned task of your group for yourself (an idea is promoted to the queue)",
		ParamEnrich: common.DefaultParamEnricher(),
		InitFuncCtx: func(ctx *boa.HookContext, p *taskboardTaskParams, _ *cobra.Command) error {
			boa.GetParamT(ctx, &p.AskHuman).SetAlternativesFunc(completeAskHumanDurations)
			return nil
		},
		RunFunc: func(p *taskboardTaskParams, _ *cobra.Command, _ []string) {
			if strings.TrimSpace(p.Peer) != "" {
				fmt.Fprintln(cliStderr, "Error: take acts on yourself, so it has no --peer; a linked node's tasks are assigned there")
				taskboardExit(rcInvalidArg)
			}
			taskboardExit(taskboardWrite("", http.MethodPost, taskboardPath(p.Task, "/assign"), map[string]any{"to": ""},
				p.AskHuman, p.JSON, "took", cliStdout, cliStderr))
		},
	}.ToCobra()
}

func taskboardStartCmd() *cobra.Command {
	return boa.CmdT[taskboardTaskParams]{
		Use:   "start",
		Short: "Start an assigned task its assignee holds (an idea to triage, or queued): it goes in progress",
		Long: "Start a task its assignee holds without having started it: an idea is promoted to the queue, and the task is " +
			"in progress, with the stall clock running. The assignee starts its own task under taskboard.own; starting " +
			"someone else's needs taskboard.assign and briefs them, as an assign does.",
		ParamEnrich: common.DefaultParamEnricher(),
		InitFuncCtx: func(ctx *boa.HookContext, p *taskboardTaskParams, _ *cobra.Command) error {
			boa.GetParamT(ctx, &p.AskHuman).SetAlternativesFunc(completeAskHumanDurations)
			return nil
		},
		RunFunc: func(p *taskboardTaskParams, _ *cobra.Command, _ []string) {
			taskboardExit(taskboardWrite(p.Peer, http.MethodPost, taskboardPath(p.Task, "/start"), map[string]any{"start": true},
				p.AskHuman, p.JSON, "started", cliStdout, cliStderr))
		},
	}.ToCobra()
}

type taskboardHoldParams struct {
	Task     string `pos:"true" help:"Task id (tk_…)"`
	Idea     bool   `long:"idea" help:"Hold it as an idea, to triage"`
	Next     bool   `long:"next" help:"Hold it queued"`
	Peer     string `long:"peer" optional:"true" help:"Act on a linked node's board instead, under the grants it gave this node (taskboard.assign)"`
	AskHuman string `long:"ask-human" optional:"true" help:"On permission denial, ask the human via popup with this timeout (e.g. '30s'). Capped at 300s. Timeout = deny."`
	JSON     bool   `long:"json" help:"Output JSON"`
}

func taskboardHoldCmd() *cobra.Command {
	return boa.CmdT[taskboardHoldParams]{
		Use:   "hold",
		Short: "Put a started task back, held by its assignee in its stage (or --idea / --next), not in progress",
		Long: "Un-start an assigned task: it keeps its assignee but leaves in progress, sitting in its stage (or the one " +
			"--idea or --next names) in the assignee's lane until it is started again. Same permissions as start.",
		ParamEnrich: common.DefaultParamEnricher(),
		InitFuncCtx: func(ctx *boa.HookContext, p *taskboardHoldParams, _ *cobra.Command) error {
			boa.GetParamT(ctx, &p.AskHuman).SetAlternativesFunc(completeAskHumanDurations)
			return nil
		},
		RunFunc: func(p *taskboardHoldParams, _ *cobra.Command, _ []string) {
			req := map[string]any{"start": false}
			switch {
			case p.Idea && p.Next:
				fmt.Fprintln(cliStderr, "Error: give --idea or --next, not both")
				taskboardExit(rcInvalidArg)
			case p.Idea:
				req["stage"] = "idea"
			case p.Next:
				req["stage"] = "next"
			}
			taskboardExit(taskboardWrite(p.Peer, http.MethodPost, taskboardPath(p.Task, "/start"), req,
				p.AskHuman, p.JSON, "held", cliStdout, cliStderr))
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
			if use == "release" && strings.TrimSpace(p.Peer) != "" {
				fmt.Fprintln(cliStderr, "Error: release acts on yourself, so it has no --peer")
				taskboardExit(rcInvalidArg)
			}
			taskboardExit(taskboardWrite(p.Peer, http.MethodPost, taskboardPath(p.Task, "/unassign"), map[string]any{"self": use == "release"},
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
			taskboardExit(runTaskboardReport(p, cliStdout, cliStderr))
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
	Peer     string   `long:"peer" optional:"true" help:"Lock a linked node's board instead, under the grants it gave this node (taskboard.manage)"`
	AskHuman string   `long:"ask-human" optional:"true" help:"On permission denial, ask the human via popup with this timeout (e.g. '30s'). Capped at 300s. Timeout = deny."`
}

func groupsSetTaskboardWritersCmd() *cobra.Command {
	return boa.CmdT[groupsTaskboardWritersParams]{
		Use:   "set-taskboard-writers",
		Short: "Lock a group's task board to listed writers, or open it to every member (--members)",
		Long: "By default every member of a group may track its own work on the task board (taskboard.own) and take its " +
			"tasks, and every holder of taskboard.write may change it. Listing writers " +
			"narrows that to them; owners, the operator, and agents with an explicit unscoped or group-scoped " +
			"taskboard.write, taskboard.assign or taskboard.own grant are not affected, and reporting on your own task is never locked. With no agents and no " +
			"--members the board is locked to owners only. Needs taskboard.manage (group owners have it).",
		ParamEnrich: common.DefaultParamEnricher(),
		InitFuncCtx: func(ctx *boa.HookContext, p *groupsTaskboardWritersParams, _ *cobra.Command) error {
			boa.GetParamT(ctx, &p.Group).SetAlternativesFunc(completeGroupNames)
			boa.GetParamT(ctx, &p.AskHuman).SetAlternativesFunc(completeAskHumanDurations)
			return nil
		},
		RunFunc: func(p *groupsTaskboardWritersParams, _ *cobra.Command, _ []string) {
			taskboardExit(runGroupsSetTaskboardWriters(p, cliStdout, cliStderr))
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
		if err := remoteRequest(p.Peer, http.MethodGet, path, nil, &resp); err != nil {
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
		if err := remoteRequestOpts(p.Peer, http.MethodPost, path, body, &resp, DaemonOpts{AskHuman: ask}); err != nil {
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

// nonEmpty is the non-empty strings among parts, in order.
func nonEmpty(parts ...string) []string {
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}
