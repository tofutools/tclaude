package agentd

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/common/boardopts"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// Board options (docs/taskboard.md "Board options"): how one group's board
// behaves, stored with the board (db.GroupTaskboardOptions), modelled and
// resolved in pkg/claude/common/boardopts. A board stores only what it sets;
// the rest is inherited from the operator's global taskboard.* config or the
// built-in default, and every read carries the values in force with where
// each came from.
//
//	GET   /v1/groups/{name}/taskboard/options   stored + resolved (taskboard.options.read)
//	PUT   /v1/groups/{name}/taskboard/options   {options, version}: replace the
//	      whole document; version is the one read, a newer one is 409 stale
//	PATCH /v1/groups/{name}/taskboard/options   {set: {key: value}, reset: [key]}:
//	      change single keys atomically (the CLI's options set / reset)
//
// Reading them is taskboard.options.read (or .own.read, which every member
// holds on its own group's board), changing them taskboard.options.write (or
// .own.write); owners hold both. A linked node reads and changes them under
// the same peer grants (peer_taskboard.go). See taskboard_options_access.go.

func registerTaskboardOptionRoutes(mux *http.ServeMux) {
	for _, route := range taskboardOptionRoutes {
		mux.HandleFunc(route.pattern, route.handler)
	}
}

// taskboardOptionRoutes are served on the agent API and, for the dashboard
// and linked nodes, on the dashboard mux (registerDashboardTaskboardRoutes).
var taskboardOptionRoutes = []struct {
	pattern string
	handler http.HandlerFunc
}{
	{"GET /v1/groups/{name}/taskboard/options", v1GroupRoute(handleTaskboardOptionsGet)},
	{"PUT /v1/groups/{name}/taskboard/options", v1GroupRoute(handleTaskboardOptionsPut)},
	{"PATCH /v1/groups/{name}/taskboard/options", v1GroupRoute(handleTaskboardOptionsPatch)},
}

// taskboardOptionsGlobals are the global settings every board inherits.
func taskboardOptionsGlobals() boardopts.Globals {
	cfg, _ := config.Load()
	g := boardopts.Globals{IdleCheck: cfg.TaskboardIdleCheck()}
	if d, ok := cfg.TaskboardIdleGraceSetting(); ok {
		g.IdleGrace = &d
	}
	return g
}

// taskboardColumnSlugs are g's custom column slugs, in order.
func taskboardColumnSlugs(groupID int64) ([]string, error) {
	cols, err := db.ListGroupTaskColumns(groupID)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(cols))
	for _, c := range cols {
		out = append(out, c.Slug)
	}
	return out, nil
}

// taskboardOptions reads a board's stored options, dropping what names a
// column the board no longer has. A document that does not parse (written
// by a newer tclaude, say) reads as no options, so the board falls back to
// the inherited behaviour rather than failing every read.
func taskboardOptions(groupID int64) (db.GroupTaskboardOptions, boardopts.Options, []string, error) {
	row, err := db.GetGroupTaskboardOptions(groupID)
	if err != nil {
		return row, boardopts.Options{}, nil, err
	}
	cols, err := taskboardColumnSlugs(groupID)
	if err != nil {
		return row, boardopts.Options{}, nil, err
	}
	o, err := boardopts.Parse(row.Options)
	if err != nil {
		return row, boardopts.Options{}, cols, nil
	}
	o.Prune(cols)
	return row, o, cols, nil
}

type taskboardOptionsGlobalsView struct {
	// IdleCheck is taskboard.idle_check; absent when unset.
	IdleCheck *bool `json:"idle_check,omitempty"`
	// IdleGrace is taskboard.idle_grace as set ("0" turns the check off
	// everywhere); absent when unset.
	IdleGrace string `json:"idle_grace,omitempty"`
}

type taskboardOptionsView struct {
	Group string `json:"group"`
	// Options is what the board itself sets.
	Options boardopts.Options `json:"options"`
	// Resolved is what is in force, each value with its source.
	Resolved boardopts.Resolved `json:"resolved"`
	// Inherited is what each value would be if the board set nothing: the
	// value a reset falls back to (the dashboard shows it before saving).
	Inherited boardopts.Resolved          `json:"inherited"`
	Globals   taskboardOptionsGlobalsView `json:"globals"`
	Version   int64                       `json:"version"`
	UpdatedBy string                      `json:"updated_by,omitempty"`
	UpdatedAt string                      `json:"updated_at,omitempty"`
	// Keys are the option keys PATCH and the CLI take.
	Keys []string `json:"keys"`
}

func taskboardOptionsViewOf(g *db.AgentGroup) (taskboardOptionsView, error) {
	row, o, cols, err := taskboardOptions(g.ID)
	if err != nil {
		return taskboardOptionsView{}, err
	}
	globals := taskboardOptionsGlobals()
	v := taskboardOptionsView{
		Group: g.Name, Options: o, Resolved: boardopts.Resolve(o, globals, cols),
		Inherited: boardopts.Resolve(boardopts.Options{}, globals, cols),
		Version:   row.Version, UpdatedBy: row.UpdatedBy, UpdatedAt: rfc3339(row.UpdatedAt), Keys: boardopts.Keys,
	}
	v.Globals.IdleCheck = globals.IdleCheck
	if globals.IdleGrace != nil {
		v.Globals.IdleGrace = boardopts.DurationString(*globals.IdleGrace).String()
		if *globals.IdleGrace == 0 {
			v.Globals.IdleGrace = "0"
		}
	}
	return v, nil
}

func writeTaskboardOptions(w http.ResponseWriter, status int, g *db.AgentGroup) {
	v, err := taskboardOptionsViewOf(g)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "db", err.Error())
		return
	}
	writeJSON(w, status, v)
}

// GET /v1/groups/{name}/taskboard/options
func handleTaskboardOptionsGet(w http.ResponseWriter, r *http.Request, g *db.AgentGroup) {
	if !taskboardEnabled(w) || !requireTaskboardOptionsRead(w, r, g) {
		return
	}
	writeTaskboardOptions(w, http.StatusOK, g)
}

// requireTaskboardOptionsWrite gates a change of g's options
// (taskboard_options_access.go).
func requireTaskboardOptionsWrite(w http.ResponseWriter, r *http.Request, g *db.AgentGroup) bool {
	return taskboardEnabled(w) && requireTaskboardOptionsChange(w, r, g) && requireGroupActive(w, g)
}

// PUT /v1/groups/{name}/taskboard/options {options, version}
func handleTaskboardOptionsPut(w http.ResponseWriter, r *http.Request, g *db.AgentGroup) {
	if !requireTaskboardOptionsWrite(w, r, g) {
		return
	}
	var body struct {
		Options json.RawMessage `json:"options"`
		Version *int64          `json:"version"`
	}
	if !decodeTaskboardBody(w, r, &body) {
		return
	}
	if len(body.Options) == 0 || string(body.Options) == "null" || body.Version == nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", "give options and the version they were read at ({} and 0 for a board that sets nothing)")
		return
	}
	// Strictly: a misspelled key is refused, not dropped.
	next, err := boardopts.Parse(string(body.Options))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", err.Error())
		return
	}
	updateTaskboardOptions(w, r, g, *body.Version, func(_ boardopts.Options) (boardopts.Options, error) { return next, nil },
		"replaced board options", false)
}

// PATCH /v1/groups/{name}/taskboard/options {set: {key: value}, reset: [key], version?}
func handleTaskboardOptionsPatch(w http.ResponseWriter, r *http.Request, g *db.AgentGroup) {
	if !requireTaskboardOptionsWrite(w, r, g) {
		return
	}
	var body struct {
		Set     map[string]string `json:"set"`
		Reset   []string          `json:"reset"`
		Version *int64            `json:"version"`
	}
	if !decodeTaskboardBody(w, r, &body) {
		return
	}
	if len(body.Set) == 0 && len(body.Reset) == 0 {
		writeError(w, http.StatusBadRequest, "invalid_arg", "give set {key: value} and/or reset [key]; the keys are "+strings.Join(boardopts.Keys, ", "))
		return
	}
	expect := int64(db.AnyTaskboardOptionsVersion)
	if body.Version != nil {
		expect = *body.Version
	}
	// Resets first, then sets in key order, so a request reads the same
	// whatever order its map arrived in.
	keys := make([]string, 0, len(body.Set))
	for k := range body.Set {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	var detail []string
	for _, k := range body.Reset {
		detail = append(detail, "reset "+k)
	}
	for _, k := range keys {
		detail = append(detail, k+"="+body.Set[k])
	}
	updateTaskboardOptions(w, r, g, expect, func(o boardopts.Options) (boardopts.Options, error) {
		for _, k := range body.Reset {
			if err := o.Reset(strings.TrimSpace(k)); err != nil {
				return o, err
			}
		}
		for _, k := range keys {
			if err := o.Set(strings.TrimSpace(k), body.Set[k]); err != nil {
				return o, err
			}
		}
		return o, nil
	}, "board options: "+strings.Join(detail, ", "), true)
}

// errTaskboardOptionsInvalid wraps a document the board refuses.
type errTaskboardOptionsInvalid struct{ err error }

func (e errTaskboardOptionsInvalid) Error() string { return e.err.Error() }

// errTaskboardOptionsUnreadable is a stored document this daemon cannot read
// (one a newer tclaude wrote): a change of single keys would lose the rest.
var errTaskboardOptionsUnreadable = errors.New("unreadable board options")

// updateTaskboardOptions applies change to g's stored options (pruned of
// removed columns), validates the result against the board and writes it.
// merge is set for a change of single keys, which refuses to write over a
// stored document this daemon cannot read; a whole-document PUT replaces it.
func updateTaskboardOptions(w http.ResponseWriter, r *http.Request, g *db.AgentGroup, expect int64,
	change func(boardopts.Options) (boardopts.Options, error), audit string, merge bool) {
	cols, err := taskboardColumnSlugs(g.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "db", err.Error())
		return
	}
	_, err = db.UpdateGroupTaskboardOptions(g.ID, expect, taskboardActor(r), func(current string) (string, error) {
		o, err := boardopts.Parse(current)
		if err != nil {
			if merge {
				return "", errTaskboardOptionsUnreadable
			}
			o = boardopts.Options{}
		}
		o.Prune(cols)
		next, err := change(o)
		if err == nil {
			err = next.Validate(cols)
		}
		if err != nil {
			return "", errTaskboardOptionsInvalid{err}
		}
		return next.Encode()
	})
	var invalid errTaskboardOptionsInvalid
	switch {
	case errors.Is(err, db.ErrTaskboardOptionsStale):
		v, verr := taskboardOptionsViewOf(g)
		if verr != nil {
			writeError(w, http.StatusInternalServerError, "db", verr.Error())
			return
		}
		writeJSON(w, http.StatusConflict, map[string]any{
			"code": "stale", "error": fmt.Sprintf(
				"the %s board options changed since you read them (now version %d); read them again and reapply your change", g.Name, v.Version),
			"current": v,
		})
		return
	case errors.Is(err, errTaskboardOptionsUnreadable):
		writeError(w, http.StatusConflict, "unreadable", "the "+g.Name+
			" board options were written by a newer tclaude and this daemon cannot read them; change them there, or replace them whole (PUT)")
		return
	case errors.As(err, &invalid):
		writeError(w, http.StatusBadRequest, "invalid_arg", invalid.Error())
		return
	case err != nil:
		writeError(w, http.StatusInternalServerError, "db", err.Error())
		return
	}
	setAuditDetail(r, audit)
	// A limit raised or lifted lets the first in line in.
	taskboardFillLanes(g)
	writeTaskboardOptions(w, http.StatusOK, g)
}

// pruneTaskboardOptionsColumn drops a removed column from g's stored
// options, so a later column of the same slug does not inherit its place
// in idle.check_columns or its limit.
func pruneTaskboardOptionsColumn(g *db.AgentGroup, actor string) {
	cols, err := taskboardColumnSlugs(g.ID)
	if err != nil {
		return
	}
	_, err = db.UpdateGroupTaskboardOptions(g.ID, db.AnyTaskboardOptionsVersion, actor, func(current string) (string, error) {
		o, err := boardopts.Parse(current)
		if err != nil || !o.Prune(cols) {
			return current, nil
		}
		return o.Encode()
	})
	if err != nil {
		slog.Warn("taskboard: pruning board options after a column removal failed", "group", g.Name, "error", err)
	}
}
