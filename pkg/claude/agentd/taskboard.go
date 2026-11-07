package agentd

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// Group task board (tcl-uzmtah, docs/taskboard.md): a short-term, ordered list
// of tasks per agent group, on unless features.taskboard is false. This is the
// store: list, capture, edit, reorder, promote/demote, block/unblock, drop,
// close and history; assignment and reports are in taskboard_assign.go.
//
// Access: taskboard.read / taskboard.write are group-scoped and conferred by
// active membership and by ownership of the group; anyone else needs a grant,
// scoped with group=<name> or unscoped. write implies read. A task id in a
// group the caller cannot read answers 404, so existence does not leak.

const (
	PermTaskboardRead   = "taskboard.read"
	PermTaskboardWrite  = "taskboard.write"
	PermTaskboardManage = "taskboard.manage"
	// PermTaskboardTransfer moves or clones a task into a group's board from
	// another group (tclaude agent taskboard move-to-group).
	PermTaskboardTransfer = "taskboard.transfer"
	PermSelfTaskboard     = "self.taskboard"
)

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
		"the task board is off; features.taskboard is false in ~/.tclaude/data/config.json")
	return false
}

func registerTaskboardRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/groups/{name}/taskboard", v1GroupRoute(handleTaskboardList))
	mux.HandleFunc("GET /v1/taskboard", handleTaskboardListOwnGroup)
	mux.HandleFunc("GET /v1/taskboard/context", handleTaskboardContext)
	mux.HandleFunc("POST /v1/groups/{name}/taskboard/tasks", v1GroupRoute(handleTaskboardCreate))
	mux.HandleFunc("GET /v1/taskboard/tasks/{id}", handleTaskboardTask(taskboardReadAccess, handleTaskboardShow))
	mux.HandleFunc("GET /v1/taskboard/tasks/{id}/events", handleTaskboardTask(taskboardReadAccess, handleTaskboardEvents))
	mux.HandleFunc("PATCH /v1/taskboard/tasks/{id}", handleTaskboardTask(taskboardWriteAccess, handleTaskboardEdit))
	mux.HandleFunc("POST /v1/taskboard/tasks/{id}/move", handleTaskboardTask(taskboardWriteAccess, handleTaskboardMove))
	mux.HandleFunc("POST /v1/taskboard/tasks/{id}/stage", handleTaskboardTask(taskboardWriteAccess, handleTaskboardStage))
	mux.HandleFunc("POST /v1/taskboard/tasks/{id}/status", handleTaskboardTask(taskboardOwnGate, handleTaskboardStatus))
	mux.HandleFunc("POST /v1/taskboard/tasks/{id}/move-to-group", handleTaskboardTask(taskboardOwnGate, handleTaskboardMoveToGroup))
	mux.HandleFunc("POST /v1/taskboard/tasks/{id}/moved-away", handleTaskboardTask(taskboardOwnGate, handleTaskboardMovedAway))
	mux.HandleFunc("POST /v1/groups/{name}/taskboard/transfer-in", v1GroupRoute(handleTaskboardTransferIn))
	mux.HandleFunc("POST /v1/groups/{name}/taskboard/ask", v1GroupRoute(handleTaskboardAsk))
	registerTaskboardAssignRoutes(mux)
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
	if _, ok := requireGroupPermission(w, r, PermTaskboardWrite, g); !ok {
		return false
	}
	return requireTaskboardUnlocked(w, r, g)
}

// requireTaskboardUnlocked refuses a confirmed agent's change of a locked
// board unless it is a listed writer, an owner of the group, or holds the
// write or assign slug through an explicit grant (not through membership)
// whose scope the group alone satisfies.
// The operator and other callers are not narrowed by the lock.
func requireTaskboardUnlocked(w http.ResponseWriter, r *http.Request, g *db.AgentGroup) bool {
	conv := confirmedAgentConv(r)
	if conv == "" {
		return true
	}
	passes, err := taskboardLockPasses(r, g, conv)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "db", err.Error())
		return false
	}
	if passes {
		return true
	}
	writeError(w, http.StatusForbidden, "taskboard_locked",
		"the "+g.Name+" task board is locked to its listed writers; ask a group owner to add you (tclaude agent groups set-taskboard-writers)")
	return false
}

// taskboardLockPasses reports whether the agent behind conv passes g's writer
// lock: the board is open, or the agent is a listed writer, an owner of the
// group, or holds the write or assign slug through an explicit grant whose
// scope the group alone satisfies.
func taskboardLockPasses(r *http.Request, g *db.AgentGroup, conv string) (bool, error) {
	lock, err := db.GetTaskboardLock(g.ID)
	if err != nil {
		return false, err
	}
	if !lock.Locked {
		return true, nil
	}
	if agentID, err := db.AgentIDForConv(conv); err == nil && agentID != "" && slices.Contains(lock.Writers, agentID) {
		return true, nil
	}
	if owner, err := db.IsAgentGroupOwner(g.ID, conv); err == nil && owner {
		return true, nil
	}
	ctx := ActionContext{Group: g.Name}
	for _, slug := range []string{PermTaskboardWrite, PermTaskboardAssign} {
		v := resolvePermissionVerdictForRequest(r, conv, slug)
		if v.Resolution == permAllow && evalPermissionScope(v, conv, ctx).Satisfied {
			return true, nil
		}
	}
	return false, nil
}

// taskboardActor names the caller in a task's history. A linked node's
// change is recorded as its own, never as this node's operator's.
func taskboardActor(r *http.Request) string {
	if author := taskboardPeerAuthor(r); author != "" {
		return "peer: " + author
	}
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
// taskboardAccess is what a task-id route's wrapper gates before the handler.
type taskboardAccess int

const (
	taskboardReadAccess taskboardAccess = iota
	taskboardWriteAccess
	// taskboardOwnGate: the wrapper gates read (the 404 rule) and an active
	// group; the handler gates the change itself.
	taskboardOwnGate
)

func handleTaskboardTask(access taskboardAccess, fn func(http.ResponseWriter, *http.Request, *db.GroupTask, *db.AgentGroup)) http.HandlerFunc {
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
		write := access == taskboardWriteAccess
		if !requireTaskboard(gate, r, g, write) {
			if conv == "" && !callerIsHuman(r) {
				notFound()
				return
			}
			gate.replay(w)
			return
		}
		if access != taskboardReadAccess && !requireGroupActive(w, g) {
			return
		}
		setAuditGroup(r, g.Name)
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
	Unmet  []string `json:"unmet,omitempty"`
	Queued bool     `json:"queued,omitempty"`
	// Assignee is the task's one assignee; absent when unassigned.
	Assignee    *taskboardAgentRef `json:"assignee,omitempty"`
	AssignedBy  string             `json:"assigned_by,omitempty"`
	AssignedAt  string             `json:"assigned_at,omitempty"`
	Outcome     string             `json:"outcome,omitempty"`
	OutcomeNote string             `json:"outcome_note,omitempty"`
	Failures    int                `json:"failures,omitempty"`
	// Stalled marks an open task whose assignee has sat idle past
	// taskboard.idle_grace without reporting; IdleSince is since when.
	// Derived on read: a new turn or a report clears it.
	Stalled   bool   `json:"stalled,omitempty"`
	IdleSince string `json:"idle_since,omitempty"`
	CreatedBy string `json:"created_by"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
	ClosedAt  string `json:"closed_at,omitempty"`
}

// taskboardAgentRef names an agent on the wire.
type taskboardAgentRef struct {
	AgentID string `json:"agent_id"`
	Name    string `json:"name,omitempty"`
}

// taskboardAgentName is an agent's display name, "" when unknown.
func taskboardAgentName(agentID string) string {
	conv, err := db.CurrentConvForAgent(agentID)
	if err != nil || conv == "" {
		return ""
	}
	if t := agent.CachedTitle(conv); t != "" && t != agent.UnknownTitle {
		return t
	}
	return ""
}

type taskboardEventView struct {
	At     string `json:"at"`
	Actor  string `json:"actor"`
	Kind   string `json:"kind"`
	Detail string `json:"detail,omitempty"`
	// ActorName and DetailText read the actor and the detail with agents
	// named (taskboardLabeler); DetailText is absent when it says the same.
	ActorName  string `json:"actor_name,omitempty"`
	DetailText string `json:"detail_text,omitempty"`
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
	staller := newTaskboardStaller(time.Now())
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
		if t.AssigneeID != "" {
			v.Assignee = &taskboardAgentRef{AgentID: t.AssigneeID, Name: taskboardAgentName(t.AssigneeID)}
			v.AssignedBy, v.AssignedAt = t.AssignedBy, rfc3339(t.AssignedAt)
		}
		v.Outcome, v.OutcomeNote, v.Failures = t.Outcome, t.OutcomeNote, t.Failures
		if since, ok := staller.stalled(t); ok {
			v.Stalled, v.IdleSince = true, rfc3339(since)
		}
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
	lock, err := db.GetTaskboardLock(g.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "db", err.Error())
		return
	}
	out := map[string]any{"group": g.Name, "tasks": views, "locked": lock.Locked}
	if lock.Locked {
		out["writers"] = lock.Writers
		// writer_names reads the same list as a person would say it.
		labels := newTaskboardLabeler(g)
		for _, id := range lock.Writers {
			labels.learn(id)
		}
		names := make([]string, 0, len(lock.Writers))
		for _, id := range lock.Writers {
			names = append(names, labels.label(id))
		}
		out["writer_names"] = names
	}
	writeJSON(w, http.StatusOK, out)
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
// after:[deps], position:{top|bottom|before|after}, resolve_ref} — capture
// lands in ideas unless stage is next. resolve_ref fills the link of a bare
// awb or Linear id through the tracker proxy, as an import does (the title
// stays the one given). {refs:[…]} and/or {lines:[…]} (with an optional stage)
// import a batch instead; see handleTaskboardImport.
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
		// ResolveRef looks a bare awb or Linear id up, as the caller, to fill
		// its link (`taskboard add --resolve-ref`).
		ResolveRef bool `json:"resolve_ref"`
		// Refs and Lines make the request an import (taskboard_import.go):
		// one task per ref or line, refused per item.
		Refs  []string `json:"refs"`
		Lines []string `json:"lines"`
		// The task's start state, in this one create: status open (the
		// default), blocked (with block_reason) or done (logged after the
		// fact); to is its assignee: who holds it (in progress, so next) or,
		// done, who did it.
		Status      string `json:"status"`
		BlockReason string `json:"block_reason"`
		To          string `json:"to"`
	}
	if !decodeTaskboardBody(w, r, &body) {
		return
	}
	if len(body.Refs) > 0 || len(body.Lines) > 0 {
		if body.Title != "" || body.Body != "" || body.Ref != "" || body.RefLabel != "" || len(body.After) > 0 ||
			body.Position != (taskboardPositionBody{}) || body.ResolveRef || body.Status != "" || body.BlockReason != "" || body.To != "" {
			writeError(w, http.StatusBadRequest, "invalid_arg",
				"an import (refs or lines) takes only stage; title, body, ref, after and position are for one task")
			return
		}
		if body.Stage != "" && body.Stage != db.GroupTaskStageIdea && body.Stage != db.GroupTaskStageNext {
			writeError(w, http.StatusBadRequest, "invalid_arg", "stage is idea or next")
			return
		}
		handleTaskboardImport(w, r, g, body.Refs, body.Lines, body.Stage)
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
	status := strings.TrimSpace(body.Status)
	reason := strings.TrimSpace(body.BlockReason)
	if err == nil {
		switch {
		case status != "" && status != db.GroupTaskOpen && status != db.GroupTaskBlocked && status != db.GroupTaskDone:
			err = errors.New("a new task's status is open, blocked or done")
		case status == db.GroupTaskBlocked && reason == "":
			err = errors.New("a blocked task needs block_reason")
		case reason != "" && status != db.GroupTaskBlocked:
			err = errors.New("block_reason is for status blocked")
		}
	}
	if err == nil && reason != "" {
		reason, err = validateTaskText("the block reason", reason, maxTaskReasonLen)
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", err.Error())
		return
	}
	// The assignee is gated as an assign is: the caller itself (taking it)
	// on its own self.taskboard, anyone else on taskboard.assign; it must be
	// an active member.
	var assignee, assigneeConv string
	if to := strings.TrimSpace(body.To); to != "" {
		if assignee, assigneeConv, err = resolveTaskboardMember(g, to); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_arg", err.Error())
			return
		}
		if !requireTaskboardAssigneeAction(w, r, g, assignee, true) {
			return
		}
	}
	if body.ResolveRef {
		refURL = resolveTaskboardRefLink(r, kind, refURL, refLabel)
	}
	actor := taskboardActor(r)
	t, err := db.CreateGroupTask(db.NewGroupTask{GroupID: g.ID, Title: title, Body: body.Body, Stage: body.Stage,
		RefKind: kind, RefURL: refURL, RefLabel: refLabel, DependsOn: deps, Position: pos, Actor: actor,
		Status: status, BlockReason: reason, AssigneeID: assignee, AssignedBy: actor})
	if err != nil {
		writeTaskboardError(w, err)
		return
	}
	setAuditDetail(r, "created "+t.ID+" in "+g.Name)
	// Someone else's new task is briefed as an assign is; work logged as
	// done tells nobody.
	if self, _ := callerAgentID(r); assignee != "" && assignee != self && t.Status != db.GroupTaskDone {
		setAuditTargetConv(r, assigneeConv)
		by := taskboardCallerName(r)
		brief := taskboardBrief(g, t, by)
		if t.Status == db.GroupTaskBlocked {
			brief = "It starts blocked: " + t.BlockReason + "\n" + brief
		}
		mailTaskboard(r, g, assigneeConv, taskboardSubject(g, t, "assigned to you by "+by), brief)
	}
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
	labels := newTaskboardLabeler(g)
	for _, e := range events {
		labels.learnEvent(e.Actor, e.Kind, e.Detail)
	}
	for _, e := range events {
		v := taskboardEventView{At: rfc3339(e.At), Actor: e.Actor, Kind: e.Kind, Detail: e.Detail,
			ActorName: labels.actor(e.Actor)}
		if text := labels.detail(e.Kind, e.Detail); text != e.Detail {
			v.DetailText = text
		}
		out = append(out, v)
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
	// Closing an assigned task (done or dropped) is the assignee's own
	// (self.taskboard) or an assigner's (taskboard.assign); everything else is
	// a write.
	closing := body.Status == db.GroupTaskDone || body.Status == db.GroupTaskDropped
	if closing && t.AssigneeID != "" && t.Live() {
		if !requireTaskboardAssigneeAction(w, r, g, t.AssigneeID, false) {
			return
		}
	} else if !requireTaskboard(w, r, g, true) {
		return
	}
	out, err := db.SetGroupTaskStatusFor(t.ID, body.Status, reason, taskboardActor(r), t.AssigneeID)
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
