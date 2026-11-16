package agentd

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/process/builtin"
	"github.com/tofutools/tclaude/pkg/claude/process/model"
)

// Built-in ways of working (tk_d02b0f4c): templates compiled into the binary.
// They are read from here, never stored; a group that uses one gets its own
// copy, a stored template pinned to the group, made through the ordinary
// create path (its gates, its id, its pin), so the group's edits stay its own.
//
//   - GET  /v1/process/builtins           every built-in, with its steps
//   - GET  /v1/process/builtins/{id}      one, as the editor reads a template
//   - POST /v1/process/builtins/{id}/use  {group, name?}: copy it into group

// processBuiltinSummary is a built-in as a list shows it.
type processBuiltinSummary struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	// Steps are its steps in the order a run reaches them, for a one-line
	// "brief → plan → …" summary.
	Steps []string `json:"steps"`
}

func handleProcessBuiltins(w http.ResponseWriter, _ *http.Request) {
	all, err := builtin.List()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "process_builtin", err.Error())
		return
	}
	out := make([]processBuiltinSummary, 0, len(all))
	for _, b := range all {
		out = append(out, processBuiltinSummary{ID: b.ID, Name: b.Name, Description: b.Description, Steps: processBuiltinSteps(b.Parsed)})
	}
	writeProcessJSON(w, http.StatusOK, map[string]any{"builtins": out})
}

// processBuiltinSteps are a template's step nodes (not start or end) in the
// order a run first reaches them.
func processBuiltinSteps(parsed *model.ParsedTemplate) []string {
	tmpl := parsed.Template
	next := map[string][]string{}
	for _, edge := range parsed.Edges {
		next[edge.From] = append(next[edge.From], edge.To)
	}
	seen := map[string]bool{tmpl.Start: true}
	queue := []string{tmpl.Start}
	var steps []string
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		if node, ok := tmpl.Nodes[id]; ok && node.Type != model.NodeTypeStart && node.Type != model.NodeTypeEnd {
			steps = append(steps, id)
		}
		for _, to := range next[id] {
			if !seen[to] {
				seen[to] = true
				queue = append(queue, to)
			}
		}
	}
	return steps
}

func processBuiltinFor(w http.ResponseWriter, r *http.Request) (*builtin.Builtin, bool) {
	b, err := builtin.Get(r.PathValue("id"))
	if errors.Is(err, builtin.ErrNotFound) {
		writeError(w, http.StatusNotFound, "process_builtin_not_found", err.Error()+" (the built-ins: "+strings.Join(builtin.IDs(), ", ")+")")
		return nil, false
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "process_builtin", err.Error())
		return nil, false
	}
	return b, true
}

// processBuiltinView is a built-in in the editor's shape (a template's GET),
// with no store metadata: it is not stored.
func processBuiltinView(b *builtin.Builtin) *processTemplateEditView {
	semantic := *b.Parsed.Template
	semantic.Layout = nil
	return &processTemplateEditView{
		Template: &semantic, Edges: b.Parsed.Edges, Layout: b.Parsed.Template.Layout,
		SourceHash: b.Parsed.SourceHash, SemanticHash: b.Parsed.SemanticHash, Source: string(b.Source),
		Diagnostics: diagnosticsForEditor(b.Parsed.Diagnostics, b.Parsed.Template),
	}
}

func handleProcessBuiltin(w http.ResponseWriter, r *http.Request) {
	b, ok := processBuiltinFor(w, r)
	if !ok {
		return
	}
	writeProcessJSON(w, http.StatusOK, processBuiltinView(b))
}

// handleProcessBuiltinUse copies a built-in into a group: a new template,
// unlisted and pinned to the group, made by the create route itself, so it
// takes what saving a process to the group takes.
func handleProcessBuiltinUse(w http.ResponseWriter, r *http.Request) {
	b, ok := processBuiltinFor(w, r)
	if !ok {
		return
	}
	var body struct {
		Group string `json:"group"`
		Name  string `json:"name"`
	}
	if err := decodeStrictProcessJSON(w, r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "json", err.Error())
		return
	}
	if strings.TrimSpace(body.Group) == "" {
		writeError(w, http.StatusBadRequest, "invalid_arg", "group is required: a built-in is copied into the group that uses it")
		return
	}
	view := processBuiltinView(b)
	tmpl := *view.Template
	tmpl.ID = ""
	if name := strings.TrimSpace(body.Name); name != "" {
		tmpl.Name = name
	}
	listed := false
	create := processTemplateEditView{Template: &tmpl, Edges: view.Edges, Layout: view.Layout, Listed: &listed, PinGroup: body.Group}
	payload, err := json.Marshal(&create)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "json", err.Error())
		return
	}
	inner := r.Clone(r.Context())
	// The create route has no {id}: a built-in id must not read as the
	// template a pin check is about.
	inner.SetPathValue("id", "")
	inner.Body = io.NopCloser(bytes.NewReader(payload))
	inner.ContentLength = int64(len(payload))
	inner.Header.Set("Content-Type", "application/json")
	setAuditDetail(r, "use built-in "+b.ID+" in "+body.Group)
	handleProcessTemplateCreate(w, inner)
}
