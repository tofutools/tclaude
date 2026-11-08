package agentd

import (
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// A task's comments and activity stream (tcl-jipq70, docs/taskboard.md),
// after awb's activity design (db/group_task_comments.go).
//
//	GET    /v1/taskboard/tasks/{id}/activity        the stream, oldest first (taskboard.read)
//	GET    /v1/taskboard/tasks/{id}/comments        the comments (taskboard.read)
//	POST   /v1/taskboard/tasks/{id}/comments        {body, key?}; multipart: a "comment" part and "file" parts
//	PATCH  /v1/taskboard/tasks/{id}/comments/{cid}  {body}: the author's edit
//	DELETE /v1/taskboard/tasks/{id}/comments/{cid}  the author's delete (a tombstone)
//
// Commenting takes taskboard.write, or taskboard.own on one's own group's
// board; editing and deleting take the same and being the comment's author.
// GET …/events answers the same stream (`taskboard history`).

// taskboardChangeView is one field's change on the wire.
type taskboardChangeView struct {
	Field  string `json:"field"`
	Before string `json:"before"`
	After  string `json:"after"`
}

// taskboardCommentView is a comment on the wire. Body is always its current
// body; a tombstone has none.
type taskboardCommentView struct {
	ID          int64                     `json:"id"`
	Kind        string                    `json:"kind"`
	Body        string                    `json:"body"`
	Key         string                    `json:"key,omitempty"`
	Author      string                    `json:"author"`
	AuthorName  string                    `json:"author_name,omitempty"`
	At          string                    `json:"at"`
	EditedAt    string                    `json:"edited_at,omitempty"`
	DeletedAt   string                    `json:"deleted_at,omitempty"`
	Attachments []taskboardAttachmentView `json:"attachments,omitempty"`
}

// taskboardResourceActions are the change kinds that are an action on a
// resource rather than a field change; an entry names it as its action.
var taskboardResourceActions = map[string]bool{
	db.GroupTaskEventAttached: true, db.GroupTaskEventDetached: true, db.GroupTaskEventRegrouped: true,
	db.GroupTaskEventCloned: true, db.GroupTaskEventAssigned: true, db.GroupTaskEventTaken: true,
	db.GroupTaskEventUnassigned: true, db.GroupTaskEventReleased: true, db.GroupTaskEventReported: true,
	db.GroupTaskEventImported: true, db.GroupTaskEventArchived: true,
}

func commentView(e *db.GroupTaskEvent, files []db.GroupTaskAttachment, labels *taskboardLabeler) *taskboardCommentView {
	v := &taskboardCommentView{ID: e.ID, Kind: e.CommentKind, Body: e.Body, Key: e.ClientKey, Author: e.Actor,
		At: rfc3339(e.At), EditedAt: rfc3339(e.EditedAt), DeletedAt: rfc3339(e.DeletedAt)}
	if name := peerPrincipalName(e.Actor); name != "" {
		v.AuthorName = name
	} else if labels != nil {
		if name := labels.actor(e.Actor); name != e.Actor {
			v.AuthorName = name
		}
	}
	if len(files) > 0 {
		v.Attachments = taskboardAttachmentViews(files)
	}
	return v
}

// taskboardActivityEntries are the task's stream as the wire shows it.
func taskboardActivityEntries(t *db.GroupTask, g *db.AgentGroup) ([]taskboardEventView, error) {
	events, err := db.ListGroupTaskEvents(t.ID)
	if err != nil {
		return nil, err
	}
	files, err := db.ListGroupTaskCommentAttachments(t.ID)
	if err != nil {
		return nil, err
	}
	labels := newTaskboardLabeler(g)
	for _, e := range events {
		labels.learnEvent(e.Actor, e.Kind, e.Detail)
	}
	out := make([]taskboardEventView, 0, len(events))
	for _, e := range events {
		v := taskboardEventView{ID: e.ID, At: rfc3339(e.At), Actor: e.Actor, Kind: e.Kind, Detail: e.Detail,
			ActorName: labels.actor(e.Actor)}
		if name := peerPrincipalName(e.Actor); name != "" {
			v.ActorName = name
		}
		if text := labels.detail(e.Kind, e.Detail); text != e.Detail {
			v.DetailText = text
		}
		for _, c := range e.Changes {
			v.Changes = append(v.Changes, taskboardChangeView(c))
		}
		if taskboardResourceActions[e.Kind] {
			v.Action = e.Kind
		}
		switch e.Kind {
		case db.GroupTaskEventComment:
			v.Comment = commentView(e, files[e.ID], labels)
		case db.GroupTaskEventCommentEdited:
			v.CommentID, v.PreviousBody = e.CommentID, e.Body
		}
		out = append(out, v)
	}
	return out, nil
}

func handleTaskboardActivity(w http.ResponseWriter, _ *http.Request, t *db.GroupTask, g *db.AgentGroup) {
	entries, err := taskboardActivityEntries(t, g)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "db", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": t.ID, "group": g.Name, "title": t.Title, "entries": entries, "events": entries})
}

func registerTaskboardCommentRoutes(mux *http.ServeMux) {
	for _, route := range taskboardCommentRoutes {
		mux.HandleFunc(route.pattern, route.handler)
	}
}

// taskboardCommentRoutes serve the socket and the dashboard alike.
var taskboardCommentRoutes = []struct {
	pattern string
	handler http.HandlerFunc
}{
	{"GET /v1/taskboard/tasks/{id}/activity", handleTaskboardTask(taskboardReadAccess, handleTaskboardActivity)},
	{"GET /v1/taskboard/tasks/{id}/comments", handleTaskboardTask(taskboardReadAccess, handleTaskboardComments)},
	{"POST /v1/taskboard/tasks/{id}/comments", handleTaskboardTask(taskboardOwnGate, handleTaskboardCommentAdd)},
	{"PATCH /v1/taskboard/tasks/{id}/comments/{cid}", handleTaskboardTask(taskboardOwnGate, handleTaskboardCommentEdit)},
	{"DELETE /v1/taskboard/tasks/{id}/comments/{cid}", handleTaskboardTask(taskboardOwnGate, handleTaskboardCommentDelete)},
}

// requireTaskboardCommenter gates commenting on any task of g's board: the
// operator, a taskboard.write holder, or an active member of g under
// taskboard.own (commenting is not claiming, so any task of its own group's
// board). An explicit deny of own (or its alias) refuses the member; a
// linked node needs write. Both are narrowed by the writer lock.
func requireTaskboardCommenter(w http.ResponseWriter, r *http.Request, g *db.AgentGroup) bool {
	if taskboardOwnOnly(r, g) {
		return requireTaskboardOwn(w, r, g, true)
	}
	return requireTaskboard(w, r, g, true)
}

// validateTaskComment bounds a comment's Markdown: 1..16 KiB of valid UTF-8,
// no control characters but newline and tab.
func validateTaskComment(body string) (string, error) {
	body = strings.TrimSpace(body)
	if body == "" {
		return "", errors.New("a comment needs a body")
	}
	if !utf8.ValidString(body) {
		return "", errors.New("a comment must be valid UTF-8")
	}
	return validateTaskText("a comment", body, db.GroupTaskCommentMaxBytes)
}

func validateTaskCommentKey(key string) (string, error) {
	key = strings.TrimSpace(key)
	if len(key) > db.GroupTaskCommentKeyMax {
		return "", fmt.Errorf("a key is at most %d bytes", db.GroupTaskCommentKeyMax)
	}
	for _, r := range key {
		if r <= ' ' || r == 0x7f {
			return "", errors.New("a key has no spaces or control characters")
		}
	}
	return key, nil
}

// writeTaskboardCommentError maps a comment's store error onto the wire.
func writeTaskboardCommentError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, db.ErrGroupTaskCommentNotFound):
		writeError(w, http.StatusNotFound, "not_found", err.Error())
	case errors.Is(err, db.ErrGroupTaskNotCommentAuthor):
		writeError(w, http.StatusForbidden, "not_author", err.Error())
	default:
		writeTaskboardError(w, err)
	}
}

func writeTaskboardComment(w http.ResponseWriter, status int, t *db.GroupTask, g *db.AgentGroup, c *db.GroupTaskEvent) {
	files, err := db.ListGroupTaskCommentAttachments(t.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "db", err.Error())
		return
	}
	writeJSON(w, status, map[string]any{"id": t.ID, "group": g.Name,
		"comment": commentView(c, files[c.ID], newTaskboardLabeler(g))})
}

// GET /v1/taskboard/tasks/{id}/comments
func handleTaskboardComments(w http.ResponseWriter, _ *http.Request, t *db.GroupTask, g *db.AgentGroup) {
	comments, err := db.ListGroupTaskComments(t.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "db", err.Error())
		return
	}
	files, err := db.ListGroupTaskCommentAttachments(t.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "db", err.Error())
		return
	}
	labels := newTaskboardLabeler(g)
	for _, c := range comments {
		labels.learnEvent(c.Actor, c.Kind, "")
	}
	out := make([]*taskboardCommentView, 0, len(comments))
	for _, c := range comments {
		out = append(out, commentView(c, files[c.ID], labels))
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": t.ID, "group": g.Name, "comments": out})
}

// POST /v1/taskboard/tasks/{id}/comments
func handleTaskboardCommentAdd(w http.ResponseWriter, r *http.Request, t *db.GroupTask, g *db.AgentGroup) {
	if !requireTaskboardCommenter(w, r, g) {
		return
	}
	var body struct {
		Body string `json:"body"`
		Key  string `json:"key"`
	}
	files, ok := decodeTaskboardRequest(w, r, &body)
	if !ok {
		return
	}
	text, err := validateTaskComment(body.Body)
	var key string
	if err == nil {
		key, err = validateTaskCommentKey(body.Key)
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", err.Error())
		return
	}
	actor := taskboardCommentAuthor(r)
	atts, ok := storeTaskUploadsOrFail(w, files, actor)
	if !ok {
		return
	}
	c, created, err := db.AddGroupTaskComment(db.NewGroupTaskComment{TaskID: t.ID, Actor: actor, Body: text, Key: key, Attachments: atts})
	if err != nil {
		writeTaskboardCommentError(w, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
		setAuditDetail(r, fmt.Sprintf("commented on %s (#%d)", t.ID, c.ID))
		mailTaskboardComment(r, g, t, c)
	}
	writeTaskboardComment(w, status, t, g, c)
}

// taskboardCommentOf resolves {cid}, writing the 404 itself.
func taskboardCommentOf(w http.ResponseWriter, r *http.Request) (int64, bool) {
	raw := r.PathValue("cid")
	cid, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || cid <= 0 {
		writeError(w, http.StatusNotFound, "not_found", "no such comment "+guideText(raw, 20))
		return 0, false
	}
	return cid, true
}

// PATCH /v1/taskboard/tasks/{id}/comments/{cid} {body}
func handleTaskboardCommentEdit(w http.ResponseWriter, r *http.Request, t *db.GroupTask, g *db.AgentGroup) {
	cid, ok := taskboardCommentOf(w, r)
	if !ok || !requireTaskboardCommenter(w, r, g) {
		return
	}
	var body struct {
		Body string `json:"body"`
	}
	if !decodeTaskboardBody(w, r, &body) {
		return
	}
	text, err := validateTaskComment(body.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", err.Error())
		return
	}
	c, err := db.EditGroupTaskComment(t.ID, cid, taskboardCommentAuthor(r), text)
	if err != nil {
		writeTaskboardCommentError(w, err)
		return
	}
	setAuditDetail(r, fmt.Sprintf("edited comment #%d on %s", cid, t.ID))
	writeTaskboardComment(w, http.StatusOK, t, g, c)
}

// DELETE /v1/taskboard/tasks/{id}/comments/{cid}
func handleTaskboardCommentDelete(w http.ResponseWriter, r *http.Request, t *db.GroupTask, g *db.AgentGroup) {
	cid, ok := taskboardCommentOf(w, r)
	if !ok || !requireTaskboardCommenter(w, r, g) {
		return
	}
	c, err := db.DeleteGroupTaskComment(t.ID, cid, taskboardCommentAuthor(r))
	if err != nil {
		writeTaskboardCommentError(w, err)
		return
	}
	setAuditDetail(r, fmt.Sprintf("deleted comment #%d on %s", cid, t.ID))
	writeTaskboardComment(w, http.StatusOK, t, g, c)
}

// mailTaskboardComment tells the task's assignee of someone else's comment,
// and its creator too when that is an agent other than the commenter and
// the assignee.
func mailTaskboardComment(r *http.Request, g *db.AgentGroup, t *db.GroupTask, c *db.GroupTaskEvent) {
	self, _ := callerAgentID(r)
	var to []string
	if t.AssigneeID != "" && t.AssigneeID != self {
		to = append(to, t.AssigneeID)
	}
	if creator, ok := strings.CutPrefix(t.CreatedBy, "agent:"); ok && creator != self && creator != t.AssigneeID {
		to = append(to, creator)
	}
	// An operator task's comments (the operator's answer, say) reach the
	// agent that asked.
	if asker, ok := strings.CutPrefix(t.AssignedBy, "agent:"); ok && t.AssigneeID == db.OperatorAssignee &&
		asker != self && !slices.Contains(to, asker) {
		to = append(to, asker)
	}
	if len(to) == 0 {
		return
	}
	by := taskboardCallerName(r)
	var b strings.Builder
	if author := taskboardPeerAuthor(r); author != "" {
		b.WriteString(peerFollowUpBanner(author))
	}
	fmt.Fprintf(&b, "%s commented on task %s %q in %s:\n\n%s\n\n", by, t.ID, t.Title, g.Name, c.Body)
	if files, err := db.ListGroupTaskCommentAttachments(t.ID); err == nil && len(files[c.ID]) > 0 {
		names := make([]string, 0, len(files[c.ID]))
		for _, f := range files[c.ID] {
			names = append(names, f.Name)
		}
		fmt.Fprintf(&b, "Files: %s (tclaude agent taskboard comment ls %s --save <dir>)\n", strings.Join(names, ", "), t.ID)
	}
	fmt.Fprintf(&b, "Reply: tclaude agent taskboard comment add %s \"…\"\nThe whole stream: tclaude agent taskboard activity %s\n", t.ID, t.ID)
	for _, id := range to {
		if conv, _ := db.CurrentConvForAgent(id); conv != "" {
			mailTaskboard(r, g, conv, taskboardSubject(g, t, "comment by "+by), b.String())
		}
	}
}
