package agentd

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// Custom stage columns (tcl-plby5h, docs/taskboard.md): a group may name up
// to db.GroupTaskColumnsCap columns of its own, such as "In review" or "In
// CI". They sit between In progress and Blocked; the built-in columns are
// fixed. Managing them is taskboard.manage (owners hold it); putting a task in
// one is a change of that task (POST /v1/taskboard/tasks/{id}/stage with the
// column's slug), so a member moves its own task there under taskboard.own.
//
//	GET    /v1/groups/{name}/taskboard/columns          the columns (taskboard.read)
//	POST   /v1/groups/{name}/taskboard/columns          {label, slug?, before?}
//	PATCH  /v1/groups/{name}/taskboard/columns/{slug}   {label?, before?, to_end?}
//	DELETE /v1/groups/{name}/taskboard/columns/{slug}   its tasks go back to next

const (
	maxTaskColumnLabelBytes = 40
	maxTaskColumnSlugBytes  = 32
)

// taskboardReservedColumnSlugs are the built-in columns' and states' names;
// a custom column may not take one, so --stage and the filters stay
// unambiguous.
var taskboardReservedColumnSlugs = map[string]bool{
	"idea": true, "ideas": true, "next": true, "progress": true, "in-progress": true, "started": true,
	"blocked": true, "done": true, "dropped": true, "open": true, "queued": true, "stalled": true,
	"waiting": true, "held": true, "closed": true, "history": true, "all": true,
}

type taskboardColumnView struct {
	Slug  string `json:"slug"`
	Label string `json:"label"`
}

func registerTaskboardColumnRoutes(mux *http.ServeMux) {
	for _, route := range taskboardColumnRoutes {
		mux.HandleFunc(route.pattern, route.handler)
	}
}

var taskboardColumnRoutes = []struct {
	pattern string
	handler http.HandlerFunc
}{
	{"GET /v1/groups/{name}/taskboard/columns", v1GroupRoute(handleTaskboardColumnsGet)},
	{"POST /v1/groups/{name}/taskboard/columns", v1GroupRoute(handleTaskboardColumnAdd)},
	{"PATCH /v1/groups/{name}/taskboard/columns/{slug}", v1GroupRoute(handleTaskboardColumnChange)},
	{"DELETE /v1/groups/{name}/taskboard/columns/{slug}", v1GroupRoute(handleTaskboardColumnRemove)},
}

func taskboardColumnViews(g *db.AgentGroup) ([]taskboardColumnView, error) {
	cols, err := db.ListGroupTaskColumns(g.ID)
	if err != nil {
		return nil, err
	}
	out := make([]taskboardColumnView, 0, len(cols))
	for _, c := range cols {
		out = append(out, taskboardColumnView{Slug: c.Slug, Label: c.Label})
	}
	return out, nil
}

// findTaskboardColumn finds g's custom column by slug, or by its label
// (case-insensitive), or by the slug its label would make ("In review").
func findTaskboardColumn(g *db.AgentGroup, name string) (db.GroupTaskColumn, bool) {
	cols, err := db.ListGroupTaskColumns(g.ID)
	if err != nil {
		return db.GroupTaskColumn{}, false
	}
	slug := taskboardColumnSlug(name)
	for _, c := range cols {
		if c.Slug == name || c.Slug == slug || strings.EqualFold(c.Label, name) {
			return c, true
		}
	}
	return db.GroupTaskColumn{}, false
}

// taskboardColumnSlug makes a slug of a label: lower-case ASCII letters and
// digits, other runs as one "-" ("In CI (macOS)" → "in-ci-macos").
func taskboardColumnSlug(label string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(label) {
		switch {
		case r >= 'a' && r <= 'z' || r >= '0' && r <= '9':
			if dash && b.Len() > 0 {
				b.WriteByte('-')
			}
			dash = false
			b.WriteRune(r)
		default:
			dash = true
		}
	}
	s := b.String()
	if len(s) > maxTaskColumnSlugBytes {
		s = strings.TrimRight(s[:maxTaskColumnSlugBytes], "-")
	}
	return s
}

func validateTaskColumnLabel(s string) (string, error) {
	s = strings.TrimSpace(s)
	switch {
	case s == "":
		return "", errors.New("a column needs a label")
	case len(s) > maxTaskColumnLabelBytes:
		return "", fmt.Errorf("a column label is at most %d bytes", maxTaskColumnLabelBytes)
	case !utf8.ValidString(s):
		return "", errors.New("a column label must be valid UTF-8")
	}
	for _, r := range s {
		if taskTextForbidden(r, false) {
			return "", errors.New("a column label is one line with no control or format characters")
		}
	}
	return s, nil
}

func validateTaskColumnSlug(s string) error {
	switch {
	case s == "":
		return errors.New("a column slug needs a letter or digit: give one with slug")
	case taskboardColumnSlug(s) != s:
		return fmt.Errorf("a column slug is lower-case letters, digits and single dashes, at most %d bytes", maxTaskColumnSlugBytes)
	case taskboardReservedColumnSlugs[s]:
		return fmt.Errorf("%s names a built-in column or state; pick another", s)
	}
	return nil
}

func writeTaskboardColumns(w http.ResponseWriter, status int, g *db.AgentGroup) {
	cols, err := taskboardColumnViews(g)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "db", err.Error())
		return
	}
	writeJSON(w, status, map[string]any{"group": g.Name, "columns": cols, "cap": db.GroupTaskColumnsCap})
}

func writeTaskboardColumnError(w http.ResponseWriter, err error) {
	if errors.Is(err, db.ErrGroupTaskColumnNotFound) {
		writeError(w, http.StatusNotFound, "not_found", err.Error())
		return
	}
	writeTaskboardError(w, err)
}

// requireTaskboardColumnsManage gates a change of g's columns.
func requireTaskboardColumnsManage(w http.ResponseWriter, r *http.Request, g *db.AgentGroup) bool {
	if !taskboardEnabled(w) {
		return false
	}
	if _, ok := requireGroupPermission(w, r, PermTaskboardManage, g); !ok {
		return false
	}
	return requireGroupActive(w, g)
}

// GET /v1/groups/{name}/taskboard/columns
func handleTaskboardColumnsGet(w http.ResponseWriter, r *http.Request, g *db.AgentGroup) {
	if !taskboardEnabled(w) || !requireTaskboard(w, r, g, false) {
		return
	}
	writeTaskboardColumns(w, http.StatusOK, g)
}

// POST /v1/groups/{name}/taskboard/columns {label, slug?, before?}
func handleTaskboardColumnAdd(w http.ResponseWriter, r *http.Request, g *db.AgentGroup) {
	if !requireTaskboardColumnsManage(w, r, g) {
		return
	}
	var body struct {
		Label  string `json:"label"`
		Slug   string `json:"slug"`
		Before string `json:"before"`
	}
	if !decodeTaskboardBody(w, r, &body) {
		return
	}
	label, err := validateTaskColumnLabel(body.Label)
	slug := strings.TrimSpace(body.Slug)
	if err == nil {
		if slug == "" {
			slug = taskboardColumnSlug(label)
		}
		err = validateTaskColumnSlug(slug)
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", err.Error())
		return
	}
	if _, err := db.AddGroupTaskColumn(g.ID, slug, label, strings.TrimSpace(body.Before), taskboardActor(r)); err != nil {
		writeTaskboardColumnError(w, err)
		return
	}
	setAuditDetail(r, "added column "+slug)
	writeTaskboardColumns(w, http.StatusCreated, g)
}

// PATCH /v1/groups/{name}/taskboard/columns/{slug} {label?, before?, to_end?}
func handleTaskboardColumnChange(w http.ResponseWriter, r *http.Request, g *db.AgentGroup) {
	if !requireTaskboardColumnsManage(w, r, g) {
		return
	}
	var body struct {
		Label  string `json:"label"`
		Before string `json:"before"`
		ToEnd  bool   `json:"to_end"`
	}
	if !decodeTaskboardBody(w, r, &body) {
		return
	}
	ch := db.GroupTaskColumnChange{Before: strings.TrimSpace(body.Before), ToEnd: body.ToEnd}
	var err error
	switch {
	case ch.Before != "" && ch.ToEnd:
		err = errors.New("give before or to_end, not both")
	case strings.TrimSpace(body.Label) == "" && ch.Before == "" && !ch.ToEnd:
		err = errors.New("give a new label, before or to_end")
	case strings.TrimSpace(body.Label) != "":
		ch.Label, err = validateTaskColumnLabel(body.Label)
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", err.Error())
		return
	}
	slug := strings.TrimSpace(r.PathValue("slug"))
	if _, err := db.ChangeGroupTaskColumn(g.ID, slug, ch); err != nil {
		writeTaskboardColumnError(w, err)
		return
	}
	setAuditDetail(r, "changed column "+slug)
	writeTaskboardColumns(w, http.StatusOK, g)
}

// DELETE /v1/groups/{name}/taskboard/columns/{slug} — the tasks in it go
// back to next (In progress for a started one); "moved" lists them.
func handleTaskboardColumnRemove(w http.ResponseWriter, r *http.Request, g *db.AgentGroup) {
	if !requireTaskboardColumnsManage(w, r, g) {
		return
	}
	slug := strings.TrimSpace(r.PathValue("slug"))
	moved, err := db.RemoveGroupTaskColumn(g.ID, slug, taskboardActor(r))
	if err != nil {
		writeTaskboardColumnError(w, err)
		return
	}
	setAuditDetail(r, fmt.Sprintf("removed column %s (%d tasks back to next)", slug, len(moved)))
	cols, err := taskboardColumnViews(g)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "db", err.Error())
		return
	}
	if moved == nil {
		moved = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"group": g.Name, "columns": cols, "cap": db.GroupTaskColumnsCap, "moved": moved})
}
