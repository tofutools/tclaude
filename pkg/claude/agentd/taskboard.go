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
// Access: taskboard.read is group-scoped and conferred by active membership
// and by ownership of the group; taskboard.write by ownership only, so a
// member changes the board under taskboard.own (taskboard_own.go) unless it
// holds write through a grant (a group grant restores it for every member).
// Anyone else needs a grant, scoped with group=<name> or unscoped. write
// implies read. A task id in a
// group the caller cannot read answers 404, so existence does not leak.

const (
	PermTaskboardRead   = "taskboard.read"
	PermTaskboardWrite  = "taskboard.write"
	PermTaskboardManage = "taskboard.manage"
	// PermTaskboardColumns manages a board's columns: add, rename, reorder,
	// remove, hide, show, cordon and uncordon (taskboard_columns.go).
	// taskboard.manage implies it (permissionUmbrellaFor).
	PermTaskboardColumns = "taskboard.columns"
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
	mux.HandleFunc("PATCH /v1/taskboard/tasks/{id}", handleTaskboardTask(taskboardOwnGate, handleTaskboardEdit))
	mux.HandleFunc("POST /v1/taskboard/tasks/{id}/move", handleTaskboardTask(taskboardWriteAccess, handleTaskboardMove))
	mux.HandleFunc("POST /v1/taskboard/tasks/{id}/stage", handleTaskboardTask(taskboardOwnGate, handleTaskboardStage))
	registerTaskboardColumnRoutes(mux)
	mux.HandleFunc("POST /v1/taskboard/tasks/{id}/status", handleTaskboardTask(taskboardOwnGate, handleTaskboardStatus))
	mux.HandleFunc("POST /v1/taskboard/tasks/{id}/reopen", handleTaskboardTask(taskboardOwnGate, handleTaskboardReopen))
	mux.HandleFunc("POST /v1/taskboard/tasks/{id}/archive", handleTaskboardTask(taskboardOwnGate, handleTaskboardArchive))
	mux.HandleFunc("POST /v1/taskboard/tasks/{id}/unarchive", handleTaskboardTask(taskboardOwnGate, handleTaskboardUnarchive))
	mux.HandleFunc("POST /v1/groups/{name}/taskboard/archive-done", v1GroupRoute(handleTaskboardArchiveDone))
	mux.HandleFunc("POST /v1/groups/{name}/taskboard/archive-closed", v1GroupRoute(handleTaskboardArchiveClosed))
	mux.HandleFunc("POST /v1/taskboard/tasks/{id}/move-to-group", handleTaskboardTask(taskboardOwnGate, handleTaskboardMoveToGroup))
	mux.HandleFunc("POST /v1/taskboard/tasks/{id}/moved-away", handleTaskboardTask(taskboardOwnGate, handleTaskboardMovedAway))
	mux.HandleFunc("POST /v1/groups/{name}/taskboard/transfer-in", v1GroupRoute(handleTaskboardTransferIn))
	mux.HandleFunc("POST /v1/groups/{name}/taskboard/ask", v1GroupRoute(handleTaskboardAsk))
	mux.HandleFunc("POST /v1/groups/{name}/taskboard/ref-lookup", v1GroupRoute(handleTaskboardRefLookup))
	registerTaskboardAssignRoutes(mux)
	registerTaskboardQuestionRoutes(mux)
	registerTaskboardAttachmentRoutes(mux)
	registerTaskboardDrawingRoutes(mux)
	registerTaskboardCommentRoutes(mux)
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
// whose scope the group alone satisfies; an explicit taskboard.own grant passes too.
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
	for _, slug := range []string{PermTaskboardWrite, PermTaskboardAssign, PermTaskboardOwn} {
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
	// Local marks an awb ref whose link is this node's awb server's (the
	// configured agent.awb_proxy.url, which may be a local address). A move
	// to another node sends the id instead (ID), and that node links it on
	// its own awb server.
	Local bool   `json:"local,omitempty"`
	ID    string `json:"id,omitempty"`
}

type taskboardTaskView struct {
	ID          string `json:"id"`
	Group       string `json:"group"`
	Title       string `json:"title"`
	Body        string `json:"body,omitempty"`
	Stage       string `json:"stage"`
	Status      string `json:"status"`
	Rank        string `json:"rank"`
	BlockReason string `json:"block_reason,omitempty"`
	// BlockedOn is the task a blocked task waits on (block --on, add
	// --blocks): closing it as done reopens this one.
	BlockedOn string        `json:"blocked_on,omitempty"`
	Ref       *taskboardRef `json:"ref,omitempty"`
	DependsOn []string      `json:"depends_on"`
	// Unmet lists the dependencies not yet done; an open task with any is
	// queued.
	Unmet  []string `json:"unmet,omitempty"`
	Queued bool     `json:"queued,omitempty"`
	// Assignee is the task's one assignee; absent when unassigned.
	Assignee   *taskboardAgentRef `json:"assignee,omitempty"`
	AssignedBy string             `json:"assigned_by,omitempty"`
	AssignedAt string             `json:"assigned_at,omitempty"`
	// Held marks a live task its assignee holds without starting it: it
	// sits in its stage in the assignee's lane, not in progress
	// (tk_3826822a). StartedAt is when a started one was started. A node
	// that predates holding sends neither: its assigned tasks are started.
	Held      bool   `json:"held,omitempty"`
	StartedAt string `json:"started_at,omitempty"`
	// Column is the custom column (its slug) a live task of stage next
	// stands in, such as in-review; absent for none (tcl-plby5h). A task in
	// a custom column never stalls.
	Column string `json:"column,omitempty"`
	// Questions are the questions asked on the task, in order, each with
	// its state and answer (tk_8b2c86f7); OpenQuestions counts those still
	// waiting on the operator. Question is a question task's own question
	// (ask-operator <group>, tk_aa049a67): its first. Waiting marks a task
	// whose asker is polling for an answer (ask-operator --wait) right now.
	Questions     []*taskboardQuestionView `json:"questions,omitempty"`
	OpenQuestions int                      `json:"open_questions,omitempty"`
	Question      *taskboardQuestionView   `json:"question,omitempty"`
	Waiting       bool                     `json:"waiting,omitempty"`
	Outcome       string                   `json:"outcome,omitempty"`
	OutcomeNote   string                   `json:"outcome_note,omitempty"`
	Failures      int                      `json:"failures,omitempty"`
	// Stalled marks an open task whose assignee has sat idle past
	// taskboard.idle_grace without reporting; IdleSince is since when.
	// Derived on read: a new turn or a report clears it.
	Stalled   bool   `json:"stalled,omitempty"`
	IdleSince string `json:"idle_since,omitempty"`
	// Attachments are the task's files, in the order they were added.
	Attachments []taskboardAttachmentView `json:"attachments,omitempty"`
	// CommentCount counts the comments not deleted; LastEditedBy (with
	// LastEditedByName for an agent) and LastEditedAt are the latest edit
	// of the task's text, ref or dependencies (tcl-jipq70).
	CommentCount     int    `json:"comment_count"`
	LastEditedBy     string `json:"last_edited_by,omitempty"`
	LastEditedByName string `json:"last_edited_by_name,omitempty"`
	LastEditedAt     string `json:"last_edited_at,omitempty"`
	CreatedBy        string `json:"created_by"`
	CreatedByName    string `json:"created_by_name,omitempty"`
	CreatedAt        string `json:"created_at"`
	UpdatedAt        string `json:"updated_at"`
	ClosedAt         string `json:"closed_at,omitempty"`
	// ArchivedAt is when a closed task was archived (by hand or by the
	// sweep); an archived task is on no list but ls --archived.
	ArchivedAt string `json:"archived_at,omitempty"`
	// URL is the task's dashboard link (taskboard_link.go), on show and
	// add only, for a caller on this node: a linked node's caller could not
	// open this node's dashboard listener.
	URL string `json:"url,omitempty"`
}

// taskboardAgentRef names an agent on the wire.
type taskboardAgentRef struct {
	AgentID string `json:"agent_id"`
	Name    string `json:"name,omitempty"`
	// Operator marks the operator of the board's node as the assignee
	// (agent_id and name are "operator").
	Operator bool `json:"operator,omitempty"`
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
	ID     int64  `json:"id,omitempty"`
	At     string `json:"at"`
	Actor  string `json:"actor"`
	Kind   string `json:"kind"`
	Detail string `json:"detail,omitempty"`
	// ActorName and DetailText read the actor and the detail with agents
	// named (taskboardLabeler); DetailText is absent when it says the same.
	ActorName  string `json:"actor_name,omitempty"`
	DetailText string `json:"detail_text,omitempty"`
	// Changes are a change entry's fields, before and after; Action names
	// an action on a resource (attached, assigned, …); Comment is a
	// comment's current state, and a comment_edited entry names the
	// comment and the body it replaced (tcl-jipq70).
	Changes      []taskboardChangeView `json:"changes,omitempty"`
	Action       string                `json:"action,omitempty"`
	Comment      *taskboardCommentView `json:"comment,omitempty"`
	CommentID    int64                 `json:"comment_id,omitempty"`
	PreviousBody string                `json:"previous_body,omitempty"`
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
	ids := make([]string, 0, len(tasks))
	for _, t := range tasks {
		ids = append(ids, t.ID)
	}
	files, err := db.ListGroupTaskAttachmentsBatch(ids)
	if err != nil {
		return nil, err
	}
	activity, err := db.GroupTaskActivitySummaries(ids)
	if err != nil {
		return nil, err
	}
	questions, err := db.ListGroupTaskQuestions(ids)
	if err != nil {
		return nil, err
	}
	names := map[string]string{}
	actorName := func(actor string) string {
		id, ok := strings.CutPrefix(actor, "agent:")
		if !ok {
			return ""
		}
		if n, seen := names[id]; seen {
			return n
		}
		names[id] = taskboardAgentName(id)
		return names[id]
	}
	out := make([]taskboardTaskView, 0, len(tasks))
	now := time.Now()
	staller := newTaskboardStaller(now)
	var awbBase *string // read once, and only when a url-less awb ref needs it
	for _, t := range tasks {
		v := taskboardTaskView{
			ID: t.ID, Group: group, Title: t.Title, Body: t.Body, Stage: t.Stage, Status: t.Status, Rank: t.Rank,
			BlockReason: t.BlockReason, DependsOn: t.DependsOn, CreatedBy: t.CreatedBy,
			CreatedAt: rfc3339(t.CreatedAt), UpdatedAt: rfc3339(t.UpdatedAt), ClosedAt: rfc3339(t.ClosedAt),
			ArchivedAt: rfc3339(t.ArchivedAt),
		}
		if v.DependsOn == nil {
			v.DependsOn = []string{}
		}
		if t.RefKind != "" {
			v.Ref = &taskboardRef{Kind: t.RefKind, URL: t.RefURL, Label: t.RefLabel}
			// An awb ref stored before refs were linked on write (its label
			// the id) is linked as it is read, so every awb chip can be followed.
			if v.Ref.Kind == "awb" {
				if awbBase == nil {
					b := awbBaseURL()
					awbBase = &b
				}
				linkLocalAWBRef(v.Ref, *awbBase)
			}
		}
		for _, dep := range t.DependsOn {
			if s, ok := statuses[dep]; ok && s != db.GroupTaskDone {
				v.Unmet = append(v.Unmet, dep)
			}
		}
		v.Queued = t.Status == db.GroupTaskOpen && len(v.Unmet) > 0
		switch t.AssigneeID {
		case "":
		case db.OperatorAssignee:
			v.Assignee = &taskboardAgentRef{AgentID: db.OperatorAssignee, Name: db.OperatorAssignee, Operator: true}
		default:
			v.Assignee = &taskboardAgentRef{AgentID: t.AssigneeID, Name: taskboardAgentName(t.AssigneeID)}
		}
		if t.AssigneeID != "" {
			v.AssignedBy, v.AssignedAt = t.AssignedBy, rfc3339(t.AssignedAt)
			v.Held, v.StartedAt = t.Live() && !t.Started(), rfc3339(t.StartedAt)
		}
		for _, q := range questions[t.ID] {
			qv := taskboardQuestionViewOf(t.ID, q, now)
			v.Questions = append(v.Questions, qv)
			if q.Open() {
				v.OpenQuestions++
			}
			v.Waiting = v.Waiting || qv.Waiting
		}
		if t.IsQuestionTask() && len(v.Questions) > 0 {
			v.Question = v.Questions[0]
		}
		v.BlockedOn, v.Column = t.BlockedOn, t.Column
		v.Outcome, v.OutcomeNote, v.Failures = t.Outcome, t.OutcomeNote, t.Failures
		if atts := files[t.ID]; len(atts) > 0 {
			v.Attachments = taskboardAttachmentViews(atts)
		}
		if since, ok := staller.stalled(t); ok {
			v.Stalled, v.IdleSince = true, rfc3339(since)
		}
		a := activity[t.ID]
		v.CommentCount = a.Comments
		v.LastEditedBy, v.LastEditedAt = a.LastEditedBy, rfc3339(a.LastEditedAt)
		v.LastEditedByName, v.CreatedByName = actorName(a.LastEditedBy), actorName(t.CreatedBy)
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

// writeTaskboardTaskLinked is writeTaskboardTask with the task's dashboard
// link, for show and add: a caller on this node gets it, a linked node's
// does not (its operator opens the task from its own dashboard).
func writeTaskboardTaskLinked(w http.ResponseWriter, r *http.Request, status int, g *db.AgentGroup, t *db.GroupTask) {
	views, err := taskboardViews(g.Name, []*db.GroupTask{t})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "db", err.Error())
		return
	}
	if taskboardPeerAuthor(r) == "" {
		views[0].URL = taskboardTaskURL(g.Name, t.ID)
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
// prefixed awb: or linear:. The URL is the ref's main datum: an awb id takes
// its link from the configured awb server (awbIssueURL); a Linear id has
// none until a lookup gives it one (resolve_ref, an import).
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
	return "awb", awbIssueURL(norm), firstNonEmpty(label, norm), nil
}

// awbBaseURL is the configured awb server's web base, or "" when none is
// configured; a variable so a test can point it somewhere.
var awbBaseURL = func() string {
	cfg, _ := config.Load()
	base, fault := validateAWBBaseURL(cfg.ResolvedAWBProxy().URL)
	if fault != nil {
		return ""
	}
	return base
}

// linkLocalAWBRef links a link-less awb ref (its label the id) on the awb
// server at base, and marks a ref linked there as Local, with its id.
func linkLocalAWBRef(ref *taskboardRef, base string) {
	if base == "" {
		return
	}
	if ref.URL == "" {
		id, fault := validateAWBIssueRef(ref.Label)
		if fault != nil {
			return
		}
		u := base + "/#/issues/" + id
		if validateTaskRefURL(u) != nil {
			return
		}
		ref.URL = u
	}
	if strings.HasPrefix(ref.URL, base+"/#/issues/") {
		if id, fault := validateAWBIssueRef(deriveTaskLabel(ref.URL)); fault == nil {
			ref.Local, ref.ID = true, id
		}
	}
}

// awbIssueURL is an awb issue's link on the configured awb server, as
// deriveTaskLabel reads it back: <base>/#/issues/<id>. It is "" without a
// configured server or for something that is not an awb id.
func awbIssueURL(id string) string {
	norm, fault := validateAWBIssueRef(id)
	if fault != nil {
		return ""
	}
	base := awbBaseURL()
	if base == "" {
		return ""
	}
	u := base + "/#/issues/" + norm
	if validateTaskRefURL(u) != nil {
		return ""
	}
	return u
}

func decodeTaskboardBody(w http.ResponseWriter, r *http.Request, v any) bool {
	return decodeTaskboardBodyLimit(w, r, v, 64<<10)
}

// decodeTaskboardBodyLimit is decodeTaskboardBody for a body of up to limit
// bytes.
func decodeTaskboardBodyLimit(w http.ResponseWriter, r *http.Request, v any, limit int64) bool {
	r.Body = http.MaxBytesReader(w, r.Body, limit)
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
	all, archived := r.URL.Query().Get("all"), r.URL.Query().Get("archived")
	tasks, err := db.ListGroupTasks(g.ID, db.GroupTaskListOpts{Stage: stage, Closed: all == "1" || all == "true",
		Archived: archived == "1" || archived == "true"})
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
	columns, err := taskboardColumnViews(g)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "db", err.Error())
		return
	}
	out := map[string]any{"group": g.Name, "tasks": views, "locked": lock.Locked, "columns": columns}
	if builtins := taskboardBuiltinColumnStates(g); len(builtins) > 0 {
		out["builtin_columns"] = builtins
	}
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
	if !taskboardEnabled(w) {
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
		// done, who did it. With "start": false the assignee holds it in
		// stage (idea, the default, or next) without starting it.
		Status      string `json:"status"`
		BlockReason string `json:"block_reason"`
		To          string `json:"to"`
		Start       *bool  `json:"start"`
		// Mine assigns the task to the calling agent (`add --mine`).
		Mine bool `json:"mine"`
		// Blocks names a live task of the board that waits on the new
		// operator task (`add --assignee operator --blocks tk_…`): in the
		// same write it gains the new task as a dependency and is blocked on
		// it. It is gated as a change of that task.
		Blocks string `json:"blocks"`
		// Question makes the task a question for the operator (`taskboard
		// ask-operator <group>`), its first question: to is the operator,
		// implied when left out.
		Question *db.GroupTaskQuestion `json:"question"`
	}
	// A multipart create carries its files beside the JSON (a "task" part);
	// the task and its files land in one transaction.
	files, ok := decodeTaskboardRequest(w, r, &body)
	if !ok {
		return
	}
	// An agent that owns the board but does not write it creates its own
	// tasks only: each is assigned to it as it is created. What taskboard.own
	// does not cover (an import, an unassigned task, a place in the queue)
	// goes through the write gate, so --ask-human can still ask for write.
	needsWrite := len(body.Refs) > 0 || len(body.Lines) > 0 || body.Position != (taskboardPositionBody{}) ||
		(strings.TrimSpace(body.To) == "" && !body.Mine && body.Stage != "")
	ownOnly := !needsWrite && taskboardOwnOnly(r, g)
	if ownOnly {
		if !requireTaskboardOwn(w, r, g, true) {
			return
		}
	} else if !requireTaskboard(w, r, g, true) {
		return
	}
	if !requireGroupActive(w, g) {
		return
	}
	if len(body.Refs) > 0 || len(body.Lines) > 0 {
		if body.Title != "" || body.Body != "" || body.Ref != "" || body.RefLabel != "" || len(body.After) > 0 ||
			body.Position != (taskboardPositionBody{}) || body.ResolveRef || body.Status != "" || body.BlockReason != "" || body.To != "" || body.Mine || body.Blocks != "" || body.Start != nil || body.Question != nil || len(files) > 0 {
			writeError(w, http.StatusBadRequest, "invalid_arg",
				"an import (refs or lines) takes only stage; title, body, ref, after, position and files are for one task")
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
	// A custom column of the board stands in next (tcl-plby5h): the task
	// starts there in the same write.
	var column string
	if err == nil {
		var bad error
		if body.Stage, column, bad, err = taskboardStageColumn(g, strings.TrimSpace(body.Stage)); err != nil {
			writeError(w, http.StatusInternalServerError, "db", err.Error())
			return
		}
		err = bad
	}
	// A board that hides its Ideas starts a new task in next unless the
	// caller names the stage (tk_bf5d2b02).
	if err == nil && body.Stage == "" && taskboardIdeasHidden(g) {
		body.Stage = db.GroupTaskStageNext
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
	// on its own taskboard.own, anyone else on taskboard.assign; it must be
	// an active member.
	to := strings.TrimSpace(body.To)
	if body.Question != nil {
		switch {
		case body.Mine || (to != "" && to != db.OperatorAssignee):
			err = errors.New("a question is for the operator (to: operator)")
		case status != "" && status != db.GroupTaskOpen:
			err = errors.New("a question starts open")
		}
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_arg", err.Error())
			return
		}
		if err := db.NormalizeGroupTaskQuestion(body.Question); err != nil {
			writeTaskboardError(w, err)
			return
		}
		// Who asked is the daemon's to say, never the body's.
		body.Question.AskedBy, body.Question.AskedVia, body.Question.Source = "", taskboardPeerAskerPrincipal(r), ""
		to = db.OperatorAssignee
	}
	if body.Mine {
		self, _ := callerAgentID(r)
		switch {
		case to != "":
			err = errors.New("give to or mine, not both")
		case self == "":
			err = errors.New("mine is for an agent: name the assignee (to)")
		}
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_arg", err.Error())
			return
		}
		to = self
	}
	if ownOnly && to == "" {
		to, _ = callerAgentID(r)
	}
	var assignee, assigneeConv string
	if to != "" {
		if assignee, assigneeConv, err = resolveTaskboardAssignee(g, to); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_arg", err.Error())
			return
		}
		if assignee == db.OperatorAssignee {
			if !requireTaskboardOperatorAssign(w, r, g, nil) {
				return
			}
		} else if !requireTaskboardAssigneeAction(w, r, g, assignee, true) {
			return
		}
	}
	hold := ""
	if body.Start != nil && !*body.Start {
		switch {
		case assignee == "":
			err = errors.New("start is for a task with an assignee (to)")
		case status == db.GroupTaskDone:
			err = errors.New("a task logged as done is not held")
		}
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_arg", err.Error())
			return
		}
		hold = firstNonEmpty(body.Stage, db.GroupTaskStageIdea)
	}
	// A task that waits on the new operator task is changed in the same
	// write, so its change is gated here too.
	var blocks *db.GroupTask
	if b := strings.TrimSpace(body.Blocks); b != "" {
		switch {
		case assignee != db.OperatorAssignee:
			err = errors.New("blocks is for a task assigned to the operator (to: operator)")
		case status != "" && status != db.GroupTaskOpen:
			err = errors.New("a task that blocks another starts open")
		}
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_arg", err.Error())
			return
		}
		var ok bool
		if blocks, ok = requireTaskboardWaitOn(w, g, b); !ok || !requireTaskboardTaskChange(w, r, g, blocks) {
			return
		}
	}
	if body.ResolveRef {
		refURL = resolveTaskboardRefLink(r, kind, refURL, refLabel)
	}
	actor := taskboardActor(r)
	atts, ok := storeTaskUploadsOrFail(w, files, actor)
	if !ok {
		return
	}
	in := db.NewGroupTask{GroupID: g.ID, Title: title, Body: body.Body, Stage: body.Stage,
		RefKind: kind, RefURL: refURL, RefLabel: refLabel, DependsOn: deps, Position: pos, Actor: actor,
		Status: status, BlockReason: reason, AssigneeID: assignee, AssignedBy: actor, Hold: hold, Attachments: atts,
		Question: body.Question, Column: column}
	if blocks != nil {
		in.Blocks, in.BlocksExpect = blocks.ID, blocks.AssigneeID
	}
	t, err := db.CreateGroupTask(in)
	if err != nil {
		writeTaskboardError(w, err)
		return
	}
	setAuditDetail(r, "created "+t.ID+" in "+g.Name)
	// Someone else's new task is briefed as an assign is; work logged as
	// done tells nobody.
	if self, _ := callerAgentID(r); assignee != "" && assignee != self && assignee != db.OperatorAssignee && t.Status != db.GroupTaskDone {
		setAuditTargetConv(r, assigneeConv)
		by := taskboardCallerName(r)
		if !t.Started() {
			subject, text := taskboardHeldNote(g, t, by)
			mailTaskboard(r, g, assigneeConv, subject, text)
			writeTaskboardTaskLinked(w, r, http.StatusCreated, g, t)
			return
		}
		brief := taskboardBrief(g, t, by)
		if t.Status == db.GroupTaskBlocked {
			brief = "It starts blocked: " + t.BlockReason + "\n" + brief
		}
		mailTaskboard(r, g, assigneeConv, taskboardSubject(g, t, "assigned to you by "+by), taskboardLinked(g, t, brief))
	}
	writeTaskboardTaskLinked(w, r, http.StatusCreated, g, t)
}

func handleTaskboardShow(w http.ResponseWriter, r *http.Request, t *db.GroupTask, g *db.AgentGroup) {
	writeTaskboardTaskLinked(w, r, http.StatusOK, g, t)
}

// GET /v1/taskboard/tasks/{id}/events: the activity stream, as "events"
// (taskboard_comments.go).
func handleTaskboardEvents(w http.ResponseWriter, r *http.Request, t *db.GroupTask, g *db.AgentGroup) {
	handleTaskboardActivity(w, r, t, g)
}

// PATCH /v1/taskboard/tasks/{id} {title?, body?, ref?, ref_label?,
// clear_ref?, resolve_ref?, add_deps?, rm_deps?} — resolve_ref looks a bare
// awb or Linear id up through its tracker proxy to fill its link, as an add
// does: the new ref's, or with no ref given the task's own link-less one.
func handleTaskboardEdit(w http.ResponseWriter, r *http.Request, t *db.GroupTask, g *db.AgentGroup) {
	var body struct {
		Title      *string  `json:"title"`
		Body       *string  `json:"body"`
		Ref        *string  `json:"ref"`
		RefLabel   *string  `json:"ref_label"`
		ClearRef   bool     `json:"clear_ref"`
		ResolveRef bool     `json:"resolve_ref"`
		AddDeps    []string `json:"add_deps"`
		RmDeps     []string `json:"rm_deps"`
	}
	if !requireTaskboardTaskChange(w, r, g, t) || !decodeTaskboardBody(w, r, &body) {
		return
	}
	if body.Title != nil || body.Body != nil {
		// A question task made for a mirrored question shows its original
		// (the access request the operator decides on it, say): only the
		// original says what it asks (taskboard_mirror.go).
		if mirrored, err := db.IsMirroredQuestionTask(t); err != nil {
			writeError(w, http.StatusInternalServerError, "db", err.Error())
			return
		} else if mirrored {
			writeError(w, http.StatusConflict, "mirrored", "this task mirrors a question asked in Messages or an access request; its title and text are not edited")
			return
		}
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
				if body.ResolveRef {
					refURL = resolveTaskboardRefLink(r, kind, refURL, refLabel)
				}
				edit.RefKind, edit.RefURL, edit.RefLabel = &kind, &refURL, &refLabel
			}
		case body.ResolveRef && body.RefLabel != nil:
			err = errors.New("resolve_ref links a ref: give it with ref, or alone for the task's own")
		case body.ResolveRef:
			// The task's own ref, linked now if it has no link yet: the id
			// is its label (an awb or Linear ref stored without a link).
			if t.RefKind == "" {
				err = errors.New("the task has no ref to resolve")
			} else if t.RefURL == "" {
				if kind, refURL, _, perr := parseTaskRef(t.RefKind+":"+t.RefLabel, ""); perr == nil && kind == t.RefKind {
					if refURL == "" {
						refURL = resolveTaskboardRefLink(r, kind, refURL, t.RefLabel)
					}
					if refURL != "" {
						edit.RefURL = &refURL
					}
				}
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
	if !requireTaskboardTaskChange(w, r, g, t) || !decodeTaskboardBody(w, r, &body) {
		return
	}
	stage := strings.TrimSpace(body.Stage)
	var out *db.GroupTask
	var err error
	switch stage {
	case "":
		writeError(w, http.StatusBadRequest, "invalid_arg", "stage is idea, next or a custom column of the board")
		return
	case db.GroupTaskStageNext:
		// Back to next from a custom column (In progress for a started task).
		if t.Column != "" {
			out, err = db.SetGroupTaskColumn(t.ID, "", taskboardActor(r), t.AssigneeID)
			break
		}
		out, err = db.SetGroupTaskStage(t.ID, stage, taskboardActor(r))
	case db.GroupTaskStageIdea:
		out, err = db.SetGroupTaskStage(t.ID, stage, taskboardActor(r))
	default:
		// A custom column of the board (tcl-plby5h), by slug or label.
		col, ok, ferr := findTaskboardColumn(g, stage)
		if ferr != nil {
			writeError(w, http.StatusInternalServerError, "db", ferr.Error())
			return
		}
		if !ok {
			writeError(w, http.StatusBadRequest, "invalid_arg",
				fmt.Sprintf("stage is idea, next or a custom column of the board; %s has no column %q", g.Name, guideText(stage, 40)))
			return
		}
		stage = col.Slug
		out, err = db.SetGroupTaskColumn(t.ID, col.Slug, taskboardActor(r), t.AssigneeID)
	}
	if err != nil {
		writeTaskboardError(w, err)
		return
	}
	setAuditDetail(r, t.ID+" to "+stage)
	writeTaskboardTask(w, http.StatusOK, g, out)
}

// POST /v1/taskboard/tasks/{id}/status {"status":"blocked","reason":…} |
// {"status":"open"} (unblock, or reopen a closed task where it was) |
// {"status":"done"|"dropped","reason"?}
func handleTaskboardStatus(w http.ResponseWriter, r *http.Request, t *db.GroupTask, g *db.AgentGroup) {
	var body struct {
		Status string `json:"status"`
		Reason string `json:"reason"`
		// On, with status blocked, blocks the task waiting on another task
		// of the board (`block --on tk_…`): it becomes a dependency, the
		// reason says "waiting on operator: <title>", and closing it as
		// done reopens this task.
		On string `json:"on"`
	}
	if !decodeTaskboardBody(w, r, &body) {
		return
	}
	if on := strings.TrimSpace(body.On); on != "" {
		handleTaskboardBlockOn(w, r, t, g, on, body.Status, body.Reason)
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
	// (taskboard.own) or an assigner's (taskboard.assign); everything else is
	// a write, or the assignee's own change of its task.
	closing := body.Status == db.GroupTaskDone || body.Status == db.GroupTaskDropped
	if closing && body.Status == db.GroupTaskDone && t.AssigneeID == db.OperatorAssignee && t.Live() {
		// Only the operator, or an assigner, closes an operator task as done:
		// the agent that asked may withdraw it (drop), not answer it.
		if _, ok := requireGroupPermission(w, r, PermTaskboardAssign, g, ActionContext{Group: g.Name}); !ok {
			return
		}
	} else if closing && t.AssigneeID != "" && t.Live() {
		if !requireTaskboardHolderAction(w, r, g, t, false) {
			return
		}
	} else if !requireTaskboardTaskChange(w, r, g, t) {
		return
	}
	var waiters []string
	if closing {
		waiters = taskboardWaitersBefore(t)
	}
	out, err := db.SetGroupTaskStatusFor(t.ID, body.Status, reason, taskboardActor(r), t.AssigneeID)
	if err != nil {
		writeTaskboardError(w, err)
		return
	}
	setAuditDetail(r, t.ID+" "+body.Status)
	if closing && t.Live() {
		mailTaskboardWaiters(r, g, out, body.Status, reason, waiters, t.AssigneeID == db.OperatorAssignee)
	}
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
	// A purged task took its files' rows with it; their blobs go here.
	collectTaskBlobs(now)
}
