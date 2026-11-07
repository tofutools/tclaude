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
	// Summary is the dashboard's counts for every board you can read, one
	// line per group (GET /v1/taskboard/context).
	Summary bool `long:"summary" help:"Counts for every group board you can read, one line per group"`
	JSON    bool `long:"json" help:"Output JSON"`
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
	if p.Summary {
		if strings.TrimSpace(p.Group) != "" || p.Stage != "" || p.All {
			fmt.Fprintln(stderr, "Error: --summary covers every board you can read; it takes no group, --stage or --all")
			return rcInvalidArg
		}
		return RunTaskboardSummary(p.JSON, stdout, stderr)
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
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	var resp struct {
		Group string          `json:"group"`
		Tasks []taskboardTask `json:"tasks"`
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
		if t.BlockReason != "" {
			line += "  — " + t.BlockReason
		}
		fmt.Fprintln(stdout, line)
	}
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
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	var resp struct {
		Groups []TaskboardSummaryGroup `json:"groups"`
	}
	if err := DaemonGet("/v1/taskboard/context", &resp); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
	}
	if resp.Groups == nil {
		resp.Groups = []TaskboardSummaryGroup{}
	}
	if asJSON {
		return writeJSONTo(stdout, resp)
	}
	if len(resp.Groups) == 0 {
		fmt.Fprintln(stdout, "no task boards you can read")
		return rcOK
	}
	for _, g := range resp.Groups {
		fmt.Fprintf(stdout, "%s: %s\n", g.Group, taskboardCountsLine(g.Counts))
	}
	return rcOK
}

// taskboardCountsLine reads as the dashboard's folded section:
// "9 · 1 needs you · 2 in progress · 6 next · 3 ideas · 12 done this week".
func taskboardCountsLine(c TaskboardCounts) string {
	parts := []string{fmt.Sprintf("%d", c.Live)}
	add := func(n int, label string) {
		if n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", n, label))
		}
	}
	add(c.NeedsYou, "needs you")
	add(c.InProgress, "in progress")
	add(c.Next, "next")
	if c.Ideas == 1 {
		add(c.Ideas, "idea")
	} else {
		add(c.Ideas, "ideas")
	}
	add(c.DoneThisWeek, "done this week")
	add(c.Dropped, "dropped")
	return strings.Join(parts, " · ")
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
	Title    string `pos:"true" optional:"true" help:"Task title (one line, up to 200 bytes); not with an import flag"`
	Body     string `long:"body" optional:"true" help:"Task brief (up to 8 KiB)"`
	BodyFile string `long:"body-file" optional:"true" help:"Read the brief from this file ('-' for stdin)"`
	Ref      string `long:"ref" optional:"true" help:"External ref: an http(s) URL, owner/repo#n, or a tracker id (tcl-a1b2c3 for awb, LIN-123 for Linear; prefix awb: or linear: to say which)"`
	RefLabel string `long:"ref-label" optional:"true" help:"Display label for the ref (default: derived)"`
	Next     bool   `long:"next" help:"Add to the queue (next) instead of the ideas"`
	After    string `long:"after" optional:"true" help:"Comma-separated task ids this task depends on"`
	Top      bool   `long:"top" help:"Place at the top of its stage instead of the end"`
	Before   string `long:"before" optional:"true" help:"Place just before this task (and in its stage)"`
	// Import flags: one task per ref or line, titles looked up through the
	// tracker proxies.
	FromAWB    string `long:"from-awb" optional:"true" help:"Import awb issues: comma-separated ids (tcl-a1b2c3,…), one task each"`
	FromLinear string `long:"from-linear" optional:"true" help:"Import Linear issues: comma-separated ids (LIN-1,…), one task each"`
	FromGitHub string `long:"from-github" optional:"true" help:"Import GitHub issues or pull requests: comma-separated owner/repo#n or URLs, one task each"`
	LinesFile  string `long:"lines-file" optional:"true" help:"Add one task per line of this file ('-' for stdin); a line that is only an awb, Linear or GitHub id becomes a task with that ref"`
	AskHuman   string `long:"ask-human" optional:"true" help:"On permission denial, ask the human via popup with this timeout (e.g. '30s'). Capped at 300s. Timeout = deny."`
	JSON       bool   `long:"json" help:"Output JSON"`
}

func taskboardAddCmd() *cobra.Command {
	return boa.CmdT[taskboardAddParams]{
		Use:   "add",
		Short: "Add a task, or import refs or lines (lands in the ideas unless --next)",
		Long: "Add one task with a title, or import several: --from-awb, --from-linear and --from-github add one task per " +
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
	if p.FromAWB != "" || p.FromLinear != "" || p.FromGitHub != "" || p.LinesFile != "" {
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
	req := map[string]any{"title": p.Title, "body": body, "ref": p.Ref, "ref_label": p.RefLabel,
		"after": splitTaskIDs(p.After), "position": map[string]any{"top": p.Top, "before": p.Before}}
	if p.Next {
		req["stage"] = "next"
	}
	path := "/v1/groups/" + url.PathEscape(strings.TrimSpace(p.Group)) + "/taskboard/tasks"
	return taskboardWrite(http.MethodPost, path, req, p.AskHuman, p.JSON, "added", stdout, stderr)
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

// readImportLines reads --lines-file ('-' is stdin), one task per line.
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
		if line = strings.TrimSpace(line); line != "" {
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
	if err := DaemonRequest(http.MethodPost, path, req, &resp, opts); err != nil {
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
