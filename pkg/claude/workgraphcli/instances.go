package workgraphcli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/table"
	"github.com/tofutools/tclaude/pkg/claude/workgraph"
	"github.com/tofutools/tclaude/pkg/common"
)

// The instance verbs are thin clients over the daemon's peer-cred
// /v1/workgraphs* surface (owned by the agentd side). Every daemon call goes
// through agent.DaemonRequest so CLI tests can stand in a transport stub via
// agent.DaemonRequestImpl without binding a real socket.

// --- wire shapes (decoded from the frozen /v1/workgraphs* responses) ---

// wfInstanceRow is one row of GET /v1/workgraphs {"instances":[...]} — the
// snapshot the daemon builds from collectWorkgraphsSnapshot.
type wfInstanceRow struct {
	ID           int64  `json:"id"`
	Title        string `json:"title"`
	TemplateRef  string `json:"template_ref"`
	TemplateName string `json:"template_name"`
	Status       string `json:"status"`
	GroupID      int64  `json:"group_id"`
	GroupName    string `json:"group_name,omitempty"`
	Total        int    `json:"total"`
	Done         int    `json:"done"`
	Failed       int    `json:"failed"`
	Running      int    `json:"running"`
	CreatedAt    string `json:"created_at,omitempty"`
	UpdatedAt    string `json:"updated_at,omitempty"`
	CompletedAt  string `json:"completed_at,omitempty"`
}

// wfNode mirrors agentd's workgraphNodeJSON.
type wfNode struct {
	NodeID          string   `json:"node_id"`
	Label           string   `json:"label"`
	ExecutorKind    string   `json:"executor_kind"`
	Agent           string   `json:"agent,omitempty"`
	VerifyKind      string   `json:"verify_kind,omitempty"`
	Status          string   `json:"status"`
	Outcome         string   `json:"outcome,omitempty"`
	Assignee        string   `json:"assignee,omitempty"`
	Visits          int64    `json:"visits"`
	Output          string   `json:"output,omitempty"`
	StartedAt       string   `json:"started_at,omitempty"`
	FinishedAt      string   `json:"finished_at,omitempty"`
	AllowedOutcomes []string `json:"allowed_outcomes,omitempty"`
}

// wfEvent mirrors agentd's workgraphEventJSON.
type wfEvent struct {
	ID      int64  `json:"id"`
	NodeID  string `json:"node_id,omitempty"`
	Kind    string `json:"kind"`
	Message string `json:"message,omitempty"`
	At      string `json:"at"`
}

// wfInstanceMeta is the instance header shared by detail + where.
type wfInstanceMeta struct {
	ID           int64  `json:"id"`
	Title        string `json:"title"`
	TemplateRef  string `json:"template_ref"`
	TemplateName string `json:"template_name"`
	Status       string `json:"status"`
	GroupID      int64  `json:"group_id"`
	GroupName    string `json:"group_name,omitempty"`
	CreatedAt    string `json:"created_at,omitempty"`
	UpdatedAt    string `json:"updated_at,omitempty"`
	CompletedAt  string `json:"completed_at,omitempty"`
}

// wfDetail is GET /v1/workgraphs/{id}.
type wfDetail struct {
	Instance wfInstanceMeta  `json:"instance"`
	Mermaid  string          `json:"mermaid"`
	Params   json.RawMessage `json:"params"`
	Vars     json.RawMessage `json:"vars"`
	Nodes    []wfNode        `json:"nodes"`
	Events   []wfEvent       `json:"events"`
	Warnings []string        `json:"warnings"`
}

// wfSelfView mirrors agentd's workgraphSelfView (JOH-15 Slice A): the embedded
// agent's task with inputs interpolated, its allowed outcomes, and where each
// outcome leads — resolved server-side so the agent never parses mermaid.
type wfSelfView struct {
	Task             string        `json:"task"`
	TaskInterpolated string        `json:"task_interpolated"`
	MissingRefs      []string      `json:"missing_refs"`
	AllowedOutcomes  []string      `json:"allowed_outcomes"`
	Successors       []wfSuccessor `json:"successors"`
}

// wfSuccessor is one outcome→target edge in the self-view.
type wfSuccessor struct {
	Outcome string `json:"outcome"`
	To      string `json:"to"`
	ToLabel string `json:"to_label"`
}

// wfAssignment / wfWhere are GET /v1/workgraphs/where.
type wfAssignment struct {
	Instance wfInstanceMeta `json:"instance"`
	Node     wfNode         `json:"node"`
	SelfView wfSelfView     `json:"self_view"`
}

type wfWhere struct {
	Caller      string         `json:"caller"`
	Assignments []wfAssignment `json:"assignments"`
}

// --- daemon helper ---

// daemonGet performs a GET against the daemon via the stubbable transport.
func daemonGet(path string, out any) error {
	return agent.DaemonRequest(http.MethodGet, path, nil, out, agent.DaemonOpts{})
}

// rcForDaemonErr maps a daemon error to a CLI exit code, refining
// agent.MapDaemonErrorToRC for the workgraph surface: the /v1/workgraphs*
// handlers reject an unauthorised caller (e.g. settling a node you're not the
// assignee of, or instantiating without owning the bound group) with a
// "forbidden" code the shared mapper doesn't know — treat it as rcAuth so a
// permission refusal is distinguishable from a transport failure. Everything
// else defers to the shared mapper.
func rcForDaemonErr(err error) int {
	if de, ok := err.(*agent.DaemonError); ok && de.Code == "forbidden" {
		return rcAuth
	}
	return agent.MapDaemonErrorToRC(err)
}

// --- ls ---

type lsParams struct {
	JSON bool `long:"json" help:"Output JSON"`
}

func lsCmd() *cobra.Command {
	return boa.CmdT[lsParams]{
		Use:         "ls",
		Short:       "List workgraph instances (and discoverable templates)",
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(p *lsParams, _ *cobra.Command, _ []string) {
			os.Exit(runLs(p, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

func runLs(p *lsParams, stdout, stderr io.Writer) int {
	if rc := agent.RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	var resp struct {
		Instances []wfInstanceRow `json:"instances"`
	}
	if err := daemonGet("/v1/workgraphs", &resp); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return rcForDaemonErr(err)
	}
	templates := workgraph.List(projectDirs()...)

	if p.JSON {
		return writeJSON(stdout, stderr, map[string]any{
			"instances": resp.Instances,
			"templates": templates,
		})
	}

	fmt.Fprintln(stdout, "INSTANCES")
	renderInstanceRows(resp.Instances, stdout)
	fmt.Fprintln(stdout, "\nTEMPLATES")
	renderTemplateList(templates, stdout)
	return rcOK
}

func renderInstanceRows(rows []wfInstanceRow, w io.Writer) {
	if len(rows) == 0 {
		fmt.Fprintln(w, "  (no workgraph instances)")
		return
	}
	tbl := table.New(
		table.Column{Header: "ID", Width: 4},
		table.Column{Header: "TITLE", MinWidth: 8, Weight: 1.0, Truncate: true},
		table.Column{Header: "TEMPLATE", MinWidth: 8, Weight: 0.8, Truncate: true},
		table.Column{Header: "STATUS", Width: 9},
		table.Column{Header: "PROGRESS", Width: 8},
		table.Column{Header: "GROUP", MinWidth: 6, Weight: 0.6, Truncate: true},
	)
	tbl.SetTerminalWidth(table.GetTerminalWidth())
	for _, r := range rows {
		tbl.AddRow(table.Row{Cells: []string{
			strconv.FormatInt(r.ID, 10),
			r.Title,
			r.TemplateName,
			r.Status,
			progressCell(r),
			r.GroupName,
		}})
	}
	fmt.Fprintln(w, tbl.Render())
}

// progressCell summarises node progress: "done/total", with a failed/running
// hint appended when either is non-zero.
func progressCell(r wfInstanceRow) string {
	s := fmt.Sprintf("%d/%d", r.Done, r.Total)
	var extra []string
	if r.Running > 0 {
		extra = append(extra, fmt.Sprintf("%d▶", r.Running))
	}
	if r.Failed > 0 {
		extra = append(extra, fmt.Sprintf("%d✗", r.Failed))
	}
	if len(extra) > 0 {
		s += " " + strings.Join(extra, " ")
	}
	return s
}

// --- status ---

type statusParams struct {
	Instance string `pos:"true" help:"Workgraph instance id"`
	JSON     bool   `long:"json" help:"Output JSON"`
}

func statusCmd() *cobra.Command {
	return boa.CmdT[statusParams]{
		Use:         "status",
		Short:       "Show a workgraph instance: per-node status, outcomes, vars, recent events",
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(p *statusParams, _ *cobra.Command, _ []string) {
			os.Exit(runStatus(p, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

func runStatus(p *statusParams, stdout, stderr io.Writer) int {
	id, rc := parseInstanceID(p.Instance, stderr)
	if rc != rcOK {
		return rc
	}
	if rc := agent.RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	var d wfDetail
	if err := daemonGet("/v1/workgraphs/"+strconv.FormatInt(id, 10), &d); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return rcForDaemonErr(err)
	}
	if p.JSON {
		return writeJSON(stdout, stderr, d)
	}
	renderDetail(&d, stdout)
	return rcOK
}

func renderDetail(d *wfDetail, w io.Writer) {
	m := d.Instance
	fmt.Fprintf(w, "#%d  %s\n", m.ID, m.Title)
	fmt.Fprintf(w, "  template: %s\n", refOrName(m.TemplateRef, m.TemplateName))
	fmt.Fprintf(w, "  status:   %s\n", m.Status)
	if m.GroupName != "" {
		fmt.Fprintf(w, "  group:    %s\n", m.GroupName)
	}
	if m.CreatedAt != "" {
		fmt.Fprintf(w, "  created:  %s\n", shortTime(m.CreatedAt))
	}
	if m.CompletedAt != "" {
		fmt.Fprintf(w, "  finished: %s\n", shortTime(m.CompletedAt))
	}
	if pv := compactJSON(d.Params); pv != "" && pv != "{}" {
		fmt.Fprintf(w, "  params:   %s\n", pv)
	}
	if vv := compactJSON(d.Vars); vv != "" && vv != "{}" {
		fmt.Fprintf(w, "  vars:     %s\n", vv)
	}

	if len(d.Warnings) > 0 {
		fmt.Fprintln(w, "\n⚠ warnings:")
		for _, warn := range d.Warnings {
			fmt.Fprintf(w, "  - %s\n", warn)
		}
	}

	fmt.Fprintln(w, "\nnodes:")
	renderNodeTable(d.Nodes, w)

	if len(d.Events) > 0 {
		const recent = 10
		if len(d.Events) > recent {
			fmt.Fprintf(w, "\nrecent events (last %d of %d — `workgraph events %d` for the full timeline):\n",
				recent, len(d.Events), m.ID)
		} else {
			fmt.Fprintln(w, "\nrecent events:")
		}
		renderEventTable(lastN(d.Events, recent), w)
	}
}

func renderNodeTable(nodes []wfNode, w io.Writer) {
	tbl := table.New(
		table.Column{Header: "NODE", MinWidth: 6, Weight: 0.7, Truncate: true},
		table.Column{Header: "LABEL", MinWidth: 8, Weight: 1.0, Truncate: true},
		table.Column{Header: "EXECUTOR", Width: 10},
		table.Column{Header: "STATUS", Width: 10},
		table.Column{Header: "OUTCOME", Width: 8},
		table.Column{Header: "ASSIGNEE", MinWidth: 8, Weight: 0.6, Truncate: true},
	)
	tbl.SetTerminalWidth(table.GetTerminalWidth())
	for _, n := range nodes {
		exec := n.ExecutorKind
		if n.Agent != "" {
			exec += ":" + n.Agent
		}
		tbl.AddRow(table.Row{Cells: []string{
			n.NodeID, n.Label, exec, n.Status, n.Outcome, shortID(n.Assignee),
		}})
	}
	fmt.Fprintln(w, tbl.Render())
}

// --- events ---

type eventsParams struct {
	Instance string `pos:"true" help:"Workgraph instance id"`
	Node     string `pos:"true" optional:"true" help:"Optional node id to filter the timeline"`
	JSON     bool   `long:"json" help:"Output JSON"`
}

func eventsCmd() *cobra.Command {
	return boa.CmdT[eventsParams]{
		Use:         "events",
		Short:       "Show a workgraph instance's audit timeline (optionally one node's)",
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(p *eventsParams, _ *cobra.Command, _ []string) {
			os.Exit(runEvents(p, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

func runEvents(p *eventsParams, stdout, stderr io.Writer) int {
	id, rc := parseInstanceID(p.Instance, stderr)
	if rc != rcOK {
		return rc
	}
	if rc := agent.RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	path := "/v1/workgraphs/" + strconv.FormatInt(id, 10) + "/events"
	if strings.TrimSpace(p.Node) != "" {
		path += "?node=" + url.QueryEscape(p.Node)
	}
	var resp struct {
		Events []wfEvent `json:"events"`
	}
	if err := daemonGet(path, &resp); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return rcForDaemonErr(err)
	}
	if p.JSON {
		return writeJSON(stdout, stderr, resp)
	}
	if len(resp.Events) == 0 {
		fmt.Fprintln(stdout, "(no events)")
		return rcOK
	}
	renderEventTable(resp.Events, stdout)
	return rcOK
}

func renderEventTable(events []wfEvent, w io.Writer) {
	tbl := table.New(
		table.Column{Header: "TIME", Width: 16},
		table.Column{Header: "NODE", MinWidth: 6, Weight: 0.7, Truncate: true},
		table.Column{Header: "KIND", MinWidth: 10, Weight: 0.7, Truncate: true},
		table.Column{Header: "MESSAGE", MinWidth: 10, Weight: 1.4, Truncate: true},
	)
	tbl.SetTerminalWidth(table.GetTerminalWidth())
	for _, e := range events {
		tbl.AddRow(table.Row{Cells: []string{shortTime(e.At), e.NodeID, e.Kind, e.Message}})
	}
	fmt.Fprintln(w, tbl.Render())
}

// --- where ---

type whereParams struct {
	Instance string `long:"instance" optional:"true" help:"Limit to one instance id"`
	All      bool   `long:"all" help:"Include assignments in finished instances / settled nodes"`
	JSON     bool   `long:"json" help:"Output JSON"`
}

func whereCmd() *cobra.Command {
	return boa.CmdT[whereParams]{
		Use:         "where",
		Short:       "Show which workgraph node(s) you (the caller) are assigned to",
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(p *whereParams, _ *cobra.Command, _ []string) {
			os.Exit(runWhere(p, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

func runWhere(p *whereParams, stdout, stderr io.Writer) int {
	if rc := agent.RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	path := "/v1/workgraphs/where"
	if strings.TrimSpace(p.Instance) != "" {
		path += "?instance=" + url.QueryEscape(p.Instance)
	}
	var resp wfWhere
	if err := daemonGet(path, &resp); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return rcForDaemonErr(err)
	}

	// Default to live assignments; --all keeps everything the server returned.
	// Non-nil either way (the daemon already emits [], but normalise the --all
	// passthrough too) so --json never serialises "assignments": null.
	assignments := resp.Assignments
	if assignments == nil {
		assignments = []wfAssignment{}
	}
	if !p.All {
		live := make([]wfAssignment, 0, len(assignments))
		for _, a := range assignments {
			if isLiveAssignment(a) {
				live = append(live, a)
			}
		}
		assignments = live
	}

	if p.JSON {
		return writeJSON(stdout, stderr, wfWhere{Caller: resp.Caller, Assignments: assignments})
	}

	if resp.Caller == "" {
		fmt.Fprintln(stdout, "(no caller identity — workgraph assignments are per-agent; humans use `workgraph ls`)")
		return rcOK
	}
	if len(assignments) == 0 {
		hint := ""
		if !p.All {
			hint = " (try --all for finished instances / settled nodes)"
		}
		fmt.Fprintf(stdout, "(you are not assigned to any live workgraph node)%s\n", hint)
		return rcOK
	}
	for _, a := range assignments {
		renderAssignment(a, stdout)
	}
	return rcOK
}

// isLiveAssignment reports whether an assignment is currently actionable: the
// instance is still running and the node is on the live frontier (not settled).
func isLiveAssignment(a wfAssignment) bool {
	if a.Instance.Status != "running" {
		return false
	}
	switch a.Node.Status {
	case "ready", "running", "awaiting_verify":
		return true
	default:
		return false
	}
}

func renderAssignment(a wfAssignment, w io.Writer) {
	fmt.Fprintf(w, "instance #%d  %s  [%s]\n", a.Instance.ID, a.Instance.Title, a.Instance.Status)
	fmt.Fprintf(w, "  template: %s\n", refOrName(a.Instance.TemplateRef, a.Instance.TemplateName))
	fmt.Fprintf(w, "  node:     %s  %q  [%s]\n", a.Node.NodeID, a.Node.Label, a.Node.Status)
	sv := a.SelfView
	if task := sv.TaskInterpolated; task != "" {
		fmt.Fprintf(w, "  task:     %s\n", task)
	}
	if len(sv.MissingRefs) > 0 {
		fmt.Fprintf(w, "  unresolved inputs: %s\n", strings.Join(sv.MissingRefs, ", "))
	}
	if len(a.Node.AllowedOutcomes) > 0 {
		fmt.Fprintf(w, "  outcomes: %s\n", strings.Join(a.Node.AllowedOutcomes, ", "))
	}
	for _, s := range sv.Successors {
		toLabel := s.ToLabel
		if toLabel == "" || toLabel == s.To {
			fmt.Fprintf(w, "  on %q → %s\n", s.Outcome, s.To)
		} else {
			fmt.Fprintf(w, "  on %q → %s (%s)\n", s.Outcome, s.To, toLabel)
		}
	}
	if a.Node.Output != "" {
		fmt.Fprintf(w, "  output:   %s\n", a.Node.Output)
	}
	fmt.Fprintln(w)
}

// --- small helpers ---

// parseInstanceID validates the positional instance id, writing a clear error
// and returning rcInvalidArg when it isn't a positive integer.
func parseInstanceID(s string, stderr io.Writer) (int64, int) {
	id, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil || id <= 0 {
		fmt.Fprintf(stderr, "Error: instance id must be a positive integer, got %q\n", s)
		return 0, rcInvalidArg
	}
	return id, rcOK
}

func refOrName(ref, name string) string {
	if ref != "" {
		return ref
	}
	return name
}

// shortID truncates a UUID-length conv-id to its 8-char prefix for compact
// table display. Shorter values — a human handle, the engine-owner sentinel, an
// already-abbreviated id — are left as-is; the full value is always in --json.
func shortID(s string) string {
	if len(s) >= 32 { // a conv-id is a 36-char UUID
		return s[:8]
	}
	return s
}

// shortTime renders an RFC3339 timestamp as "2006-01-02 15:04", falling back to
// the raw string when it doesn't parse.
func shortTime(s string) string {
	if s == "" {
		return ""
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return s
	}
	return t.Local().Format("2006-01-02 15:04")
}

// compactJSON renders a JSON blob as a single line for inline display via
// json.Compact, which strips only insignificant whitespace — unlike a
// fields-collapse it never touches spaces inside string values. Returns "" for
// empty input, and the trimmed raw text if the blob somehow doesn't parse.
func compactJSON(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return strings.TrimSpace(string(raw))
	}
	return buf.String()
}

func lastN[T any](s []T, n int) []T {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}
