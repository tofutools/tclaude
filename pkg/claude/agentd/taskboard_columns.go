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
// fixed. Managing them is taskboard.columns (taskboard.manage covers it;
// owners hold both); putting a task in
// one is a change of that task (POST /v1/taskboard/tasks/{id}/stage with the
// column's slug), so a member moves its own task there under taskboard.own.
//
//	GET    /v1/groups/{name}/taskboard/columns          the columns (taskboard.read)
//	POST   /v1/groups/{name}/taskboard/columns          {label, slug?, before?}
//	PATCH  /v1/groups/{name}/taskboard/columns/{slug}   {label?, before?, to_end?}
//	DELETE /v1/groups/{name}/taskboard/columns/{slug}   its tasks go back to next
//	POST   /v1/groups/{name}/taskboard/columns/{slug}/state {state}
//	       shown, hidden or cordoned (tk_bf5d2b02); slug ideas or done for
//	       those built-in columns, which may be hidden. A route of its own,
//	       not a PATCH field, so a linked node that predates it answers 404
//	       rather than ignoring the field.

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
	// State is hidden or cordoned; absent for shown (tk_bf5d2b02).
	State string `json:"state,omitempty"`
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
	{"POST /v1/groups/{name}/taskboard/columns/{slug}/state", v1GroupRoute(handleTaskboardColumnState)},
}

func taskboardColumnViews(g *db.AgentGroup) ([]taskboardColumnView, error) {
	cols, err := db.ListGroupTaskColumns(g.ID)
	if err != nil {
		return nil, err
	}
	states, err := db.ListGroupTaskColumnStates(g.ID)
	if err != nil {
		return nil, err
	}
	out := make([]taskboardColumnView, 0, len(cols))
	for _, c := range cols {
		out = append(out, taskboardColumnView{Slug: c.Slug, Label: c.Label, State: states[c.Slug]})
	}
	return out, nil
}

// taskboardBuiltinColumnStates is the state of g's built-in columns that are
// not shown (ideas, done: hidden), by slug; nil when every one is shown.
func taskboardBuiltinColumnStates(g *db.AgentGroup) map[string]string {
	states, err := db.ListGroupTaskColumnStates(g.ID)
	if err != nil {
		return nil
	}
	var out map[string]string
	for _, slug := range []string{db.GroupTaskColumnIdeas, db.GroupTaskColumnDone} {
		if s := states[slug]; s != "" {
			if out == nil {
				out = map[string]string{}
			}
			out[slug] = s
		}
	}
	return out
}

// taskboardIdeasHidden reports whether g's board hides its Ideas column: a
// new task then starts in next unless its stage is named.
func taskboardIdeasHidden(g *db.AgentGroup) bool {
	return taskboardBuiltinColumnStates(g)[db.GroupTaskColumnIdeas] == db.GroupTaskColumnHidden
}

// findTaskboardColumn finds g's custom column by its slug first, then by its
// label (case-insensitive), then by the slug its label would make ("In
// review" → in-review).
func findTaskboardColumn(g *db.AgentGroup, name string) (db.GroupTaskColumn, bool, error) {
	cols, err := db.ListGroupTaskColumns(g.ID)
	if err != nil {
		return db.GroupTaskColumn{}, false, err
	}
	slug := taskboardColumnSlug(name)
	for _, match := range []func(db.GroupTaskColumn) bool{
		func(c db.GroupTaskColumn) bool { return c.Slug == name },
		func(c db.GroupTaskColumn) bool { return strings.EqualFold(c.Label, name) },
		func(c db.GroupTaskColumn) bool { return c.Slug == slug },
	} {
		for _, c := range cols {
			if match(c) {
				return c, true, nil
			}
		}
	}
	return db.GroupTaskColumn{}, false, nil
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

func taskboardColumnExists(g *db.AgentGroup, slug string) bool {
	cols, err := db.ListGroupTaskColumns(g.ID)
	if err != nil {
		return true
	}
	for _, c := range cols {
		if c.Slug == slug {
			return true
		}
	}
	return false
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
	out := map[string]any{"group": g.Name, "columns": cols, "cap": db.GroupTaskColumnsCap}
	if builtins := taskboardBuiltinColumnStates(g); len(builtins) > 0 {
		out["builtin_columns"] = builtins
	}
	writeJSON(w, status, out)
}

// writeTaskboardColumnError maps a column store error; a missing column
// named in the path is 404, one named in the body (before) a bad argument.
func writeTaskboardColumnError(w http.ResponseWriter, err error, inPath bool) {
	if errors.Is(err, db.ErrGroupTaskColumnNotFound) {
		if inPath {
			writeError(w, http.StatusNotFound, "not_found", err.Error())
		} else {
			writeError(w, http.StatusBadRequest, "invalid_arg", "before names no custom column of the board")
		}
		return
	}
	writeTaskboardError(w, err)
}

// requireTaskboardColumnsManage gates a change of g's columns.
func requireTaskboardColumnsManage(w http.ResponseWriter, r *http.Request, g *db.AgentGroup) bool {
	if !taskboardEnabled(w) {
		return false
	}
	if _, ok := requireGroupPermission(w, r, PermTaskboardColumns, g); !ok {
		return false
	}
	return requireGroupActive(w, g)
}

// POST /v1/groups/{name}/taskboard/columns/{slug}/state {state} — show,
// hide or cordon a column for the whole board (tk_bf5d2b02). slug is a
// custom column's (by slug or label), or ideas / done; a cordon is for a
// custom column only.
func handleTaskboardColumnState(w http.ResponseWriter, r *http.Request, g *db.AgentGroup) {
	if !requireTaskboardColumnsManage(w, r, g) {
		return
	}
	var body struct {
		State string `json:"state"`
	}
	if !decodeTaskboardBody(w, r, &body) {
		return
	}
	state := strings.TrimSpace(body.State)
	name := strings.TrimSpace(r.PathValue("slug"))
	slug := name
	switch name {
	case db.GroupTaskColumnIdeas, "idea":
		slug = db.GroupTaskColumnIdeas
	case db.GroupTaskColumnDone:
	default:
		col, ok, err := findTaskboardColumn(g, name)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "db", err.Error())
			return
		}
		if !ok {
			if taskboardReservedColumnSlugs[name] {
				writeError(w, http.StatusBadRequest, "invalid_arg",
					fmt.Sprintf("%s is always shown; of the built-in columns only ideas and done can be hidden", name))
				return
			}
			writeError(w, http.StatusNotFound, "not_found", fmt.Sprintf("%s has no column %q", g.Name, guideText(name, 40)))
			return
		}
		slug = col.Slug
	}
	if err := db.SetGroupTaskColumnState(g.ID, slug, state); err != nil {
		writeTaskboardColumnError(w, err, true)
		return
	}
	setAuditDetail(r, "column "+slug+" "+state)
	// A column no longer cordoned takes the first in line for it.
	taskboardFillLanes(g)
	writeTaskboardColumns(w, http.StatusOK, g)
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
		writeTaskboardColumnError(w, err, false)
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
		// The path's column is checked first; a miss after that is before's.
		writeTaskboardColumnError(w, err, !taskboardColumnExists(g, slug))
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
		writeTaskboardColumnError(w, err, true)
		return
	}
	pruneTaskboardOptionsColumn(g, taskboardActor(r))
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

// taskboardStageColumn splits a requested stage into a stage and a custom
// column: idea and next are stages; anything else names one of the board's
// columns (by slug or label, as move --stage takes it), which stands in
// next. A column the board lacks is a bad request (bad); a lookup failure is
// err.
func taskboardStageColumn(g *db.AgentGroup, stage string) (outStage, column string, bad, err error) {
	switch stage {
	case "", db.GroupTaskStageIdea, db.GroupTaskStageNext:
		return stage, "", nil, nil
	}
	col, ok, err := findTaskboardColumn(g, stage)
	if err != nil {
		return "", "", nil, err
	}
	if !ok {
		return "", "", fmt.Errorf("stage is idea, next or a custom column of the board; %s has no column %q", g.Name, guideText(stage, 40)), nil
	}
	return db.GroupTaskStageNext, col.Slug, nil, nil
}
