package agentd

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// Group task board (tcl-uzmtah, docs/taskboard.md): a short-term, ordered list
// of tasks per agent group, behind features.taskboard. This slice is the plain
// store: list, capture, edit, reorder, promote/demote, block/unblock, drop,
// close and history. Assignment and reports arrive with a later slice.
//
// Access: taskboard.read / taskboard.write are group-scoped and conferred by
// active membership and by ownership of the group; anyone else needs a grant,
// scoped with group=<name> or unscoped. write implies read. A task id in a
// group the caller cannot read answers 404, so existence does not leak.

const (
	PermTaskboardRead   = "taskboard.read"
	PermTaskboardWrite  = "taskboard.write"
	PermTaskboardManage = "taskboard.manage"
	PermSelfTaskboard   = "self.taskboard"
)

const peerTaskboardWhy = "the federation read path of a group's task board is a later slice; until then a peer does not reach it"

const (
	maxTaskTitleBytes = 200
	maxTaskBodyBytes  = 8 << 10
	maxTaskReasonLen  = 400
)

// taskboardEnabled answers 404 when features.taskboard is off.
func taskboardEnabled(w http.ResponseWriter) bool {
	cfg, _ := config.Load()
	if cfg.TaskboardEnabled() {
		return true
	}
	writeError(w, http.StatusNotFound, "feature_disabled",
		"the task board is off; enable it with features.taskboard in ~/.tclaude/config.json")
	return false
}

func registerTaskboardRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/groups/{name}/taskboard", v1GroupRoute(handleTaskboardList))
	mux.HandleFunc("GET /v1/taskboard", handleTaskboardListOwnGroup)
	mux.HandleFunc("POST /v1/groups/{name}/taskboard/tasks", v1GroupRoute(handleTaskboardCreate))
	mux.HandleFunc("GET /v1/taskboard/tasks/{id}", handleTaskboardTask(false, handleTaskboardShow))
	mux.HandleFunc("GET /v1/taskboard/tasks/{id}/events", handleTaskboardTask(false, handleTaskboardEvents))
	mux.HandleFunc("PATCH /v1/taskboard/tasks/{id}", handleTaskboardTask(true, handleTaskboardEdit))
	mux.HandleFunc("POST /v1/taskboard/tasks/{id}/move", handleTaskboardTask(true, handleTaskboardMove))
	mux.HandleFunc("POST /v1/taskboard/tasks/{id}/stage", handleTaskboardTask(true, handleTaskboardStage))
	mux.HandleFunc("POST /v1/taskboard/tasks/{id}/status", handleTaskboardTask(true, handleTaskboardStatus))
}

// agentReadsTaskboard reports, silently, whether the agent reads group's
// board: taskboard.read, or taskboard.write unless read is explicitly denied.
func agentReadsTaskboard(r *http.Request, conv string, g *db.AgentGroup) bool {
	ctx := ActionContext{Group: g.Name, structuralGroup: g.Name}
	if silentAgentAllows(r, conv, PermTaskboardRead, ctx) {
		return true
	}
	if resolvePermissionVerdictForRequest(r, conv, PermTaskboardRead).Resolution == permDeny {
		return false
	}
	return silentAgentAllows(r, conv, PermTaskboardWrite, ctx)
}

// requireTaskboard gates an access to group's board. The operator passes; an
// agent needs the slug (read is also satisfied by write); anyone else gets
// the shared gate's answer.
func requireTaskboard(w http.ResponseWriter, r *http.Request, g *db.AgentGroup, write bool) bool {
	clearAuthorizedPermission(r)
	if !write {
		if conv := confirmedAgentConv(r); conv != "" && agentReadsTaskboard(r, conv, g) {
			return true
		}
		_, ok := requireGroupPermission(w, r, PermTaskboardRead, g)
		return ok
	}
	_, ok := requireGroupPermission(w, r, PermTaskboardWrite, g)
	return ok
}

// taskboardActor names the caller in a task's history.
func taskboardActor(r *http.Request) string {
	if conv := confirmedAgentConv(r); conv != "" {
		if id, err := db.AgentIDForConv(conv); err == nil && id != "" {
			return "agent:" + id
		}
		return "agent:" + conv
	}
	if callerIsHuman(r) {
		return "human:operator"
	}
	return "unknown"
}

// handleTaskboardTask resolves {id} to a task and its group, answers 404 for
// a task in a group the calling agent cannot read, and gates the access.
func handleTaskboardTask(write bool, fn func(http.ResponseWriter, *http.Request, *db.GroupTask, *db.AgentGroup)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !taskboardEnabled(w) {
			return
		}
		id := strings.TrimSpace(r.PathValue("id"))
		notFound := func() { writeError(w, http.StatusNotFound, "not_found", "no such task "+guideText(id, 40)) }
		if !db.IsGroupTaskID(id) {
			notFound()
			return
		}
		t, err := db.GetGroupTask(id)
		if errors.Is(err, db.ErrGroupTaskNotFound) {
			notFound()
			return
		}
		if err != nil {
			writeError(w, http.StatusInternalServerError, "db", err.Error())
			return
		}
		g, err := db.GetAgentGroupByID(t.GroupID)
		if err != nil || g == nil {
			notFound()
			return
		}
		conv := confirmedAgentConv(r)
		if conv != "" && !agentReadsTaskboard(r, conv, g) {
			notFound()
			return
		}
		// A refused caller that is neither the operator nor a confirmed agent
		// (a peer, an unidentified or retired caller) gets the same 404 as for
		// a missing task, so it cannot confirm that an id exists. An agent
		// that reads the board but may not write it keeps the 403.
		gate := &taskboardGateWriter{header: http.Header{}}
		if !requireTaskboard(gate, r, g, write) {
			if conv == "" && !callerIsHuman(r) {
				notFound()
				return
			}
			gate.replay(w)
			return
		}
		if write && !requireGroupActive(w, g) {
			return
		}
		fn(w, r, t, g)
	}
}

// taskboardGateWriter holds a gate's refusal so the caller can replace it.
type taskboardGateWriter struct {
	header http.Header
	status int
	body   []byte
}

func (g *taskboardGateWriter) Header() http.Header { return g.header }
func (g *taskboardGateWriter) WriteHeader(status int) {
	if g.status == 0 {
		g.status = status
	}
}
func (g *taskboardGateWriter) Write(b []byte) (int, error) {
	if g.status == 0 {
		g.status = http.StatusOK
	}
	g.body = append(g.body, b...)
	return len(b), nil
}

func (g *taskboardGateWriter) replay(w http.ResponseWriter) {
	for k, v := range g.header {
		w.Header()[k] = v
	}
	if g.status == 0 {
		g.status = http.StatusForbidden
	}
	w.WriteHeader(g.status)
	_, _ = w.Write(g.body)
}

// taskTextForbidden reports a rune a task's text refuses: control
// characters (but newline and tab when multiline), line and paragraph
// separators, and format characters such as bidi overrides.
func taskTextForbidden(r rune, multiline bool) bool {
	if multiline && (r == '\n' || r == '\t') {
		return false
	}
	return unicode.IsControl(r) || unicode.In(r, unicode.Zl, unicode.Zp, unicode.Cf)
}

// ---- wire shapes ----

type taskboardRef struct {
	Kind  string `json:"kind"`
	URL   string `json:"url,omitempty"`
	Label string `json:"label,omitempty"`
}

type taskboardTaskView struct {
	ID          string        `json:"id"`
	Group       string        `json:"group"`
	Title       string        `json:"title"`
	Body        string        `json:"body,omitempty"`
	Stage       string        `json:"stage"`
	Status      string        `json:"status"`
	Rank        string        `json:"rank"`
	BlockReason string        `json:"block_reason,omitempty"`
	Ref         *taskboardRef `json:"ref,omitempty"`
	DependsOn   []string      `json:"depends_on"`
	// Unmet lists the dependencies not yet done; an open task with any is
	// queued.
	Unmet     []string `json:"unmet,omitempty"`
	Queued    bool     `json:"queued,omitempty"`
	CreatedBy string   `json:"created_by"`
	CreatedAt string   `json:"created_at"`
	UpdatedAt string   `json:"updated_at"`
	ClosedAt  string   `json:"closed_at,omitempty"`
}

type taskboardEventView struct {
	At     string `json:"at"`
	Actor  string `json:"actor"`
	Kind   string `json:"kind"`
	Detail string `json:"detail,omitempty"`
}

func rfc3339(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

func taskboardViews(group string, tasks []*db.GroupTask) ([]taskboardTaskView, error) {
	var depIDs []string
	for _, t := range tasks {
		depIDs = append(depIDs, t.DependsOn...)
	}
	statuses, err := db.GroupTaskStatuses(depIDs)
	if err != nil {
		return nil, err
	}
	out := make([]taskboardTaskView, 0, len(tasks))
	for _, t := range tasks {
		v := taskboardTaskView{
			ID: t.ID, Group: group, Title: t.Title, Body: t.Body, Stage: t.Stage, Status: t.Status, Rank: t.Rank,
			BlockReason: t.BlockReason, DependsOn: t.DependsOn, CreatedBy: t.CreatedBy,
			CreatedAt: rfc3339(t.CreatedAt), UpdatedAt: rfc3339(t.UpdatedAt), ClosedAt: rfc3339(t.ClosedAt),
		}
		if v.DependsOn == nil {
			v.DependsOn = []string{}
		}
		if t.RefKind != "" {
			v.Ref = &taskboardRef{Kind: t.RefKind, URL: t.RefURL, Label: t.RefLabel}
		}
		for _, dep := range t.DependsOn {
			if s, ok := statuses[dep]; ok && s != db.GroupTaskDone {
				v.Unmet = append(v.Unmet, dep)
			}
		}
		v.Queued = t.Status == db.GroupTaskOpen && len(v.Unmet) > 0
		out = append(out, v)
	}
	return out, nil
}

func writeTaskboardTask(w http.ResponseWriter, status int, g *db.AgentGroup, t *db.GroupTask) {
	views, err := taskboardViews(g.Name, []*db.GroupTask{t})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "db", err.Error())
		return
	}
	writeJSON(w, status, views[0])
}

// writeTaskboardError maps a store error onto the wire.
func writeTaskboardError(w http.ResponseWriter, err error) {
	var conflict *db.GroupTaskConflict
	switch {
	case errors.Is(err, db.ErrGroupTaskNotFound):
		writeError(w, http.StatusNotFound, "not_found", err.Error())
	case errors.Is(err, db.ErrGroupTaskCap):
		writeError(w, http.StatusConflict, "cap", err.Error())
	case errors.As(err, &conflict):
		status := http.StatusConflict
		if conflict.Code == "invalid_arg" {
			status = http.StatusBadRequest
		}
		writeError(w, status, conflict.Code, conflict.Msg)
	default:
		writeError(w, http.StatusInternalServerError, "db", err.Error())
	}
}

// ---- validation ----

// validateTaskTitle: one line of valid UTF-8 with no control characters,
// 1-200 bytes. A title is shown in listings and inbox mail and never reaches
// tmux send-keys; a future path that sends one toward a pane must still run
// the rename-title gate at that sink.
func validateTaskTitle(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", errors.New("a title is required")
	}
	if len(s) > maxTaskTitleBytes {
		return "", fmt.Errorf("a title is at most %d bytes", maxTaskTitleBytes)
	}
	if !utf8.ValidString(s) {
		return "", errors.New("a title must be valid UTF-8")
	}
	for _, r := range s {
		if taskTextForbidden(r, false) {
			return "", errors.New("a title is one line with no control or format characters")
		}
	}
	return s, nil
}

// validateTaskText bounds a body or a reason: valid UTF-8, no control
// characters but newline and tab.
func validateTaskText(what, s string, max int) (string, error) {
	if len(s) > max {
		return "", fmt.Errorf("%s is at most %d bytes", what, max)
	}
	if !utf8.ValidString(s) {
		return "", fmt.Errorf("%s must be valid UTF-8", what)
	}
	for _, r := range s {
		if taskTextForbidden(r, true) {
			return "", fmt.Errorf("%s has a control or format character", what)
		}
	}
	return s, nil
}

var (
	taskRefIDRe     = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_-]*-[A-Za-z0-9]+$`)
	taskRefLinearRe = regexp.MustCompile(`^[A-Z][A-Z0-9]*-[0-9]+$`)
	taskRefGitHubRe = regexp.MustCompile(`^([A-Za-z0-9][A-Za-z0-9-]*)/([A-Za-z0-9._-]+)#([0-9]+)$`)
)

// parseTaskRef reads a task's external reference: an http(s) URL (its kind
// derived from the host), owner/repo#n for GitHub, or a bare tracker id —
// UPPER-123 is Linear, lower-case <workspace>-<hash> is awb — optionally
// prefixed awb: or linear:. A bare id has no URL until a later slice resolves
// it through the tracker proxies.
func parseTaskRef(raw, label string) (kind, refURL, refLabel string, err error) {
	raw, label = strings.TrimSpace(raw), strings.TrimSpace(label)
	if err := validateTaskRefLabel(label); err != nil {
		return "", "", "", err
	}
	if raw == "" {
		return "", "", "", nil
	}
	lower := strings.ToLower(raw)
	if strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "https://") {
		if err := validateTaskRefURL(raw); err != nil {
			return "", "", "", err
		}
		derived := deriveTaskLabel(raw)
		host := ""
		if u, perr := url.Parse(raw); perr == nil {
			host = strings.ToLower(strings.TrimPrefix(u.Hostname(), "www."))
		}
		kind = "url"
		switch {
		case (host == "linear.app" || strings.HasSuffix(host, ".linear.app")) && linearIssueRe.MatchString(derived):
			kind = "linear"
		case host == "github.com":
			kind = "github"
		case derived != "" && derived != host && strings.Contains(raw, "#/issues/"):
			kind = "awb"
		}
		return kind, raw, firstNonEmpty(label, derived), nil
	}
	if m := taskRefGitHubRe.FindStringSubmatch(raw); m != nil {
		u := "https://github.com/" + m[1] + "/" + m[2] + "/issues/" + m[3]
		return "github", u, firstNonEmpty(label, m[1]+"/"+m[2]+"#"+m[3]), nil
	}
	id, explicit := raw, ""
	if p, rest, ok := strings.Cut(raw, ":"); ok && (strings.EqualFold(p, "awb") || strings.EqualFold(p, "linear")) {
		id, explicit = strings.TrimSpace(rest), strings.ToLower(p)
	}
	if !taskRefIDRe.MatchString(id) {
		return "", "", "", fmt.Errorf("%q is not a ref: give an http(s) URL, owner/repo#n, or a tracker id such as tcl-a1b2c3 or LIN-123", raw)
	}
	if explicit == "linear" || explicit == "" && taskRefLinearRe.MatchString(id) {
		id = strings.ToUpper(id)
		if !taskRefLinearRe.MatchString(id) {
			return "", "", "", fmt.Errorf("%q is not a Linear issue id (TEAM-123)", raw)
		}
		return "linear", "", firstNonEmpty(label, id), nil
	}
	norm, fault := validateAWBIssueRef(id)
	if fault != nil {
		return "", "", "", errors.New(fault.Msg)
	}
	return "awb", "", firstNonEmpty(label, norm), nil
}

func decodeTaskboardBody(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", "invalid JSON body: "+err.Error())
		return false
	}
	return true
}

type taskboardPositionBody struct {
	Top    bool   `json:"top,omitempty"`
	Bottom bool   `json:"bottom,omitempty"`
	Before string `json:"before,omitempty"`
	After  string `json:"after,omitempty"`
}

func (p taskboardPositionBody) position() (db.GroupTaskPosition, error) {
	n := 0
	for _, set := range []bool{p.Top, p.Bottom, p.Before != "", p.After != ""} {
		if set {
			n++
		}
	}
	if n > 1 {
		return db.GroupTaskPosition{}, errors.New("give at most one of top, bottom, before, after")
	}
	return db.GroupTaskPosition{Top: p.Top, Bottom: p.Bottom,
		Before: strings.TrimSpace(p.Before), After: strings.TrimSpace(p.After)}, nil
}

func cleanTaskIDs(ids []string) ([]string, error) {
	var out []string
	seen := map[string]bool{}
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" || seen[id] {
			continue
		}
		if !db.IsGroupTaskID(id) {
			return nil, fmt.Errorf("%q is not a task id (tk_ and 8 hex digits)", guideText(id, 40))
		}
		seen[id] = true
		out = append(out, id)
	}
	if len(out) > db.GroupTaskDepsCap {
		return nil, fmt.Errorf("a task depends on at most %d tasks", db.GroupTaskDepsCap)
	}
	return out, nil
}

// ---- handlers ----

// GET /v1/groups/{name}/taskboard[?stage=idea|next][&all=1] — the board in
// the operator's order: next, then ideas; with all, the done and dropped
// tasks not yet archived.
func handleTaskboardList(w http.ResponseWriter, r *http.Request, g *db.AgentGroup) {
	if !taskboardEnabled(w) || !requireTaskboard(w, r, g, false) {
		return
	}
	stage := r.URL.Query().Get("stage")
	if stage != "" && stage != db.GroupTaskStageIdea && stage != db.GroupTaskStageNext {
		writeError(w, http.StatusBadRequest, "invalid_arg", "stage is idea or next")
		return
	}
	all := r.URL.Query().Get("all")
	tasks, err := db.ListGroupTasks(g.ID, db.GroupTaskListOpts{Stage: stage, Closed: all == "1" || all == "true"})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "db", err.Error())
		return
	}
	views, err := taskboardViews(g.Name, tasks)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "db", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"group": g.Name, "tasks": views})
}

// GET /v1/taskboard — the board of the calling agent's group, when it is an
// active member of exactly one group; `taskboard ls` with no group.
func handleTaskboardListOwnGroup(w http.ResponseWriter, r *http.Request) {
	if !taskboardEnabled(w) {
		return
	}
	conv := confirmedAgentConv(r)
	if conv == "" {
		writeError(w, http.StatusBadRequest, "invalid_arg", "name a group")
		return
	}
	groups, err := db.ListGroupsForConv(conv)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "db", err.Error())
		return
	}
	var active []*db.AgentGroup
	for _, g := range groups {
		if !g.IsArchived() {
			active = append(active, g)
		}
	}
	if len(active) != 1 {
		writeError(w, http.StatusBadRequest, "invalid_arg",
			fmt.Sprintf("you are in %d active groups; name the group", len(active)))
		return
	}
	handleTaskboardList(w, r, active[0])
}

// POST /v1/groups/{name}/taskboard/tasks {title, body, ref, ref_label, stage,
// after:[deps], position:{top|bottom|before|after}} — capture lands in ideas
// unless stage is next.
func handleTaskboardCreate(w http.ResponseWriter, r *http.Request, g *db.AgentGroup) {
	if !taskboardEnabled(w) || !requireTaskboard(w, r, g, true) || !requireGroupActive(w, g) {
		return
	}
	var body struct {
		Title    string                `json:"title"`
		Body     string                `json:"body"`
		Ref      string                `json:"ref"`
		RefLabel string                `json:"ref_label"`
		Stage    string                `json:"stage"`
		After    []string              `json:"after"`
		Position taskboardPositionBody `json:"position"`
	}
	if !decodeTaskboardBody(w, r, &body) {
		return
	}
	title, err := validateTaskTitle(body.Title)
	if err == nil {
		body.Body, err = validateTaskText("the body", body.Body, maxTaskBodyBytes)
	}
	var kind, refURL, refLabel string
	if err == nil {
		kind, refURL, refLabel, err = parseTaskRef(body.Ref, body.RefLabel)
	}
	var deps []string
	if err == nil {
		deps, err = cleanTaskIDs(body.After)
	}
	var pos db.GroupTaskPosition
	if err == nil {
		pos, err = body.Position.position()
	}
	if err == nil && body.Stage != "" && body.Stage != db.GroupTaskStageIdea && body.Stage != db.GroupTaskStageNext {
		err = errors.New("stage is idea or next")
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", err.Error())
		return
	}
	t, err := db.CreateGroupTask(db.NewGroupTask{GroupID: g.ID, Title: title, Body: body.Body, Stage: body.Stage,
		RefKind: kind, RefURL: refURL, RefLabel: refLabel, DependsOn: deps, Position: pos, Actor: taskboardActor(r)})
	if err != nil {
		writeTaskboardError(w, err)
		return
	}
	setAuditDetail(r, "created "+t.ID+" in "+g.Name)
	writeTaskboardTask(w, http.StatusCreated, g, t)
}

func handleTaskboardShow(w http.ResponseWriter, _ *http.Request, t *db.GroupTask, g *db.AgentGroup) {
	writeTaskboardTask(w, http.StatusOK, g, t)
}

func handleTaskboardEvents(w http.ResponseWriter, _ *http.Request, t *db.GroupTask, g *db.AgentGroup) {
	events, err := db.ListGroupTaskEvents(t.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "db", err.Error())
		return
	}
	out := make([]taskboardEventView, 0, len(events))
	for _, e := range events {
		out = append(out, taskboardEventView{At: rfc3339(e.At), Actor: e.Actor, Kind: e.Kind, Detail: e.Detail})
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": t.ID, "group": g.Name, "title": t.Title, "events": out})
}

// PATCH /v1/taskboard/tasks/{id} {title?, body?, ref?, ref_label?,
// clear_ref?, add_deps?, rm_deps?}
func handleTaskboardEdit(w http.ResponseWriter, r *http.Request, t *db.GroupTask, g *db.AgentGroup) {
	var body struct {
		Title    *string  `json:"title"`
		Body     *string  `json:"body"`
		Ref      *string  `json:"ref"`
		RefLabel *string  `json:"ref_label"`
		ClearRef bool     `json:"clear_ref"`
		AddDeps  []string `json:"add_deps"`
		RmDeps   []string `json:"rm_deps"`
	}
	if !decodeTaskboardBody(w, r, &body) {
		return
	}
	edit := db.GroupTaskEdit{Actor: taskboardActor(r)}
	var err error
	if body.Title != nil {
		var title string
		if title, err = validateTaskTitle(*body.Title); err == nil {
			edit.Title = &title
		}
	}
	if err == nil && body.Body != nil {
		var text string
		if text, err = validateTaskText("the body", *body.Body, maxTaskBodyBytes); err == nil {
			edit.Body = &text
		}
	}
	if err == nil {
		switch {
		case body.ClearRef:
			empty := ""
			edit.RefKind, edit.RefURL, edit.RefLabel = &empty, &empty, &empty
		case body.Ref != nil:
			label := ""
			if body.RefLabel != nil {
				label = *body.RefLabel
			}
			var kind, refURL, refLabel string
			if kind, refURL, refLabel, err = parseTaskRef(*body.Ref, label); err == nil {
				edit.RefKind, edit.RefURL, edit.RefLabel = &kind, &refURL, &refLabel
			}
		case body.RefLabel != nil:
			if t.RefKind == "" {
				err = errors.New("the task has no ref to label")
			} else if err = validateTaskRefLabel(*body.RefLabel); err == nil {
				label := strings.TrimSpace(*body.RefLabel)
				edit.RefLabel = &label
			}
		}
	}
	if err == nil {
		edit.AddDeps, err = cleanTaskIDs(body.AddDeps)
	}
	if err == nil {
		edit.RmDeps, err = cleanTaskIDs(body.RmDeps)
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", err.Error())
		return
	}
	out, err := db.EditGroupTask(t.ID, edit)
	if err != nil {
		writeTaskboardError(w, err)
		return
	}
	setAuditDetail(r, "edited "+t.ID)
	writeTaskboardTask(w, http.StatusOK, g, out)
}

// POST /v1/taskboard/tasks/{id}/move {top|bottom|before|after} — crossing the
// ideas divider (before/after a task of the other stage) promotes or demotes.
func handleTaskboardMove(w http.ResponseWriter, r *http.Request, t *db.GroupTask, g *db.AgentGroup) {
	var body taskboardPositionBody
	if !decodeTaskboardBody(w, r, &body) {
		return
	}
	pos, err := body.position()
	if err == nil && !pos.Top && !pos.Bottom && pos.Before == "" && pos.After == "" {
		err = errors.New("give one of top, bottom, before, after")
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", err.Error())
		return
	}
	out, err := db.MoveGroupTask(t.ID, pos, taskboardActor(r))
	if err != nil {
		writeTaskboardError(w, err)
		return
	}
	setAuditDetail(r, "moved "+t.ID)
	writeTaskboardTask(w, http.StatusOK, g, out)
}

// POST /v1/taskboard/tasks/{id}/stage {"stage":"next"|"idea"} — promote to
// the end of the queue, or demote to the top of the ideas.
func handleTaskboardStage(w http.ResponseWriter, r *http.Request, t *db.GroupTask, g *db.AgentGroup) {
	var body struct {
		Stage string `json:"stage"`
	}
	if !decodeTaskboardBody(w, r, &body) {
		return
	}
	if body.Stage != db.GroupTaskStageIdea && body.Stage != db.GroupTaskStageNext {
		writeError(w, http.StatusBadRequest, "invalid_arg", "stage is idea or next")
		return
	}
	out, err := db.SetGroupTaskStage(t.ID, body.Stage, taskboardActor(r))
	if err != nil {
		writeTaskboardError(w, err)
		return
	}
	setAuditDetail(r, t.ID+" to "+body.Stage)
	writeTaskboardTask(w, http.StatusOK, g, out)
}

// POST /v1/taskboard/tasks/{id}/status {"status":"blocked","reason":…} |
// {"status":"open"} (unblock, or reopen a closed task where it was) |
// {"status":"done"|"dropped","reason"?}
func handleTaskboardStatus(w http.ResponseWriter, r *http.Request, t *db.GroupTask, g *db.AgentGroup) {
	var body struct {
		Status string `json:"status"`
		Reason string `json:"reason"`
	}
	if !decodeTaskboardBody(w, r, &body) {
		return
	}
	reason, err := validateTaskText("the reason", strings.TrimSpace(body.Reason), maxTaskReasonLen)
	switch {
	case err != nil:
	case body.Status == db.GroupTaskBlocked && reason == "":
		err = errors.New("a block needs a reason")
	case body.Status != db.GroupTaskOpen && body.Status != db.GroupTaskBlocked &&
		body.Status != db.GroupTaskDone && body.Status != db.GroupTaskDropped:
		err = errors.New("status is open, blocked, done or dropped")
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", err.Error())
		return
	}
	out, err := db.SetGroupTaskStatus(t.ID, body.Status, reason, taskboardActor(r))
	if err != nil {
		writeTaskboardError(w, err)
		return
	}
	setAuditDetail(r, t.ID+" "+body.Status)
	writeTaskboardTask(w, http.StatusOK, g, out)
}

// taskboardSweepInterval is how often done and dropped tasks are archived and
// purged.
const taskboardSweepInterval = time.Hour

// startTaskboardSweep runs sweepTaskboard at start and then hourly.
func startTaskboardSweep(stop <-chan struct{}) {
	go func() {
		sweepTaskboard(time.Now())
		t := time.NewTicker(taskboardSweepInterval)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				sweepTaskboard(time.Now())
			}
		}
	}()
}

// sweepTaskboard archives the done and dropped tasks closed longer ago than
// taskboard.archive_after_days and purges those past taskboard.retention_days.
// It does nothing while features.taskboard is off.
func sweepTaskboard(now time.Time) {
	cfg, _ := config.Load()
	if !cfg.TaskboardEnabled() {
		return
	}
	archiveDays, retentionDays := cfg.TaskboardRetention()
	day := 24 * time.Hour
	archived, purged, err := db.SweepGroupTasks(now.Add(-time.Duration(archiveDays)*day),
		now.Add(-time.Duration(retentionDays)*day))
	if err != nil {
		slog.Warn("taskboard sweep failed", "error", err)
		return
	}
	if archived > 0 || purged > 0 {
		slog.Info("taskboard sweep", "archived", archived, "purged", purged)
	}
}
