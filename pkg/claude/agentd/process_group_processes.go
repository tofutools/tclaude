package agentd

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"maps"
	"net/http"
	"slices"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/process/model"
	"github.com/tofutools/tclaude/pkg/claude/process/store"
)

// A group's saved processes and the template library (tcl-8o4rjj).
//
// Everything stays a stored template: one versioned artifact, whoever saved
// it. A group keeps an ordered list of the processes saved to it (pins), and
// the library lists a template unless it was saved to a group without being
// published. Both live in tclaude's database beside the template store, never
// in template content, so a template's hashes and its export never carry a
// group or a listing.
//
//   - "Save to group" is a template save with pinGroup (and, for a new one,
//     listed: false): POST /v1/process/templates[/{id}].
//   - "Publish" lists it: POST /v1/process/templates/{id}/listing, or a save
//     with listed: true.
//   - Pin and unpin an existing template: POST and DELETE
//     /v1/process/groups/{group}/processes[/{templateId}].
//   - The process context lists each group's saved processes with the inputs
//     a start asks for.

// processTemplatePin is a resolved, authorized pinGroup of a save request.
type processTemplatePin struct {
	group *db.AgentGroup
}

// resolveProcessGroupForPin resolves a group by name for a pin change: it must
// exist and be live, and the caller must be the human, or an agent that is a
// member or an owner of that group. A pin is a change to the group, so seeing
// every group's runs (process.runs.read) is not enough to make one.
func resolveProcessGroupForPin(w http.ResponseWriter, r *http.Request, name string) (*db.AgentGroup, bool) {
	group, err := db.GetAgentGroupByName(strings.TrimSpace(name))
	if err != nil || group == nil || group.IsArchived() {
		writeError(w, http.StatusNotFound, "not_found", "no such group")
		return nil, false
	}
	p := peerFromContext(r.Context())
	if classify(p) == classHuman {
		return group, true
	}
	if p.ConvID != "" {
		if member, _ := db.FindMemberInGroup(group.ID, p.ConvID); member != nil {
			return group, true
		}
		if owner, _ := db.IsAgentGroupOwner(group.ID, p.ConvID); owner {
			return group, true
		}
	}
	writeError(w, http.StatusNotFound, "not_found", "no such group")
	return nil, false
}

func resolveProcessTemplatePin(w http.ResponseWriter, r *http.Request, name string) (*processTemplatePin, bool) {
	if strings.TrimSpace(name) == "" {
		return nil, true
	}
	group, ok := resolveProcessGroupForPin(w, r, name)
	if !ok {
		return nil, false
	}
	// Refuse a full group before the template version is written, so a
	// failed pin never leaves an unlisted template nobody can find.
	if full, err := db.ProcessGroupPinsFull(group.ID, r.PathValue("id")); err != nil {
		writeError(w, http.StatusInternalServerError, "process_group_pin", err.Error())
		return nil, false
	} else if full {
		writeError(w, http.StatusConflict, "process_group_pin", db.ErrProcessGroupPinsFull.Error())
		return nil, false
	}
	return &processTemplatePin{group: group}, true
}

// applyProcessTemplateMeta records a save's listing and pin after the
// template version is committed.
// A failure after the save still names the saved template (id and ref), so
// the caller can retry the listing or pin instead of saving again.
func applyProcessTemplateMeta(w http.ResponseWriter, templateID, ref string, listed *bool, pin *processTemplatePin) bool {
	fail := func(status int, code, message string) bool {
		body := map[string]any{"error": message, "code": code, "id": templateID}
		if ref != "" {
			body["ref"] = ref
			body["error"] = "the template was saved, but " + message
		}
		writeProcessJSON(w, status, body)
		return false
	}
	if listed != nil {
		if err := db.SetProcessTemplateListed(templateID, *listed); err != nil {
			return fail(http.StatusInternalServerError, "process_template_listing", "recording its library listing failed: "+err.Error())
		}
	}
	if pin != nil {
		if err := db.PinProcessToGroup(pin.group.ID, templateID); err != nil {
			status := http.StatusInternalServerError
			if errors.Is(err, db.ErrProcessGroupPinsFull) {
				status = http.StatusConflict
			}
			return fail(status, "process_group_pin", "pinning it to the group failed: "+err.Error())
		}
	}
	return true
}

// POST /v1/process/templates/{id}/listing {listed}
func handleProcessTemplateListing(w http.ResponseWriter, r *http.Request) {
	if _, ok := requirePermission(w, r, PermProcessTemplatesManage); !ok {
		return
	}
	id := r.PathValue("id")
	if err := store.ValidateTemplateID(id); err != nil {
		writeError(w, http.StatusBadRequest, "process_template_id", err.Error())
		return
	}
	var body struct {
		Listed *bool `json:"listed"`
	}
	if err := decodeStrictProcessJSON(w, r, &body); err != nil || body.Listed == nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", "body must be {\"listed\": true|false}")
		return
	}
	if !processTemplateExists(w, r, id) {
		return
	}
	if err := db.SetProcessTemplateListed(id, *body.Listed); err != nil {
		writeError(w, http.StatusInternalServerError, "process_template_listing", err.Error())
		return
	}
	writeProcessJSON(w, http.StatusOK, map[string]any{"id": id, "listed": *body.Listed})
}

// POST /v1/process/groups/{group}/processes {templateId}
func handleProcessGroupPin(w http.ResponseWriter, r *http.Request) {
	if _, ok := requirePermission(w, r, PermProcessTemplatesManage); !ok {
		return
	}
	var body struct {
		TemplateID string `json:"templateId"`
	}
	if err := decodeStrictProcessJSON(w, r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "json", err.Error())
		return
	}
	if err := store.ValidateTemplateID(body.TemplateID); err != nil {
		writeError(w, http.StatusBadRequest, "process_template_id", err.Error())
		return
	}
	group, ok := resolveProcessGroupForPin(w, r, r.PathValue("group"))
	if !ok || !processTemplateExists(w, r, body.TemplateID) {
		return
	}
	if !applyProcessTemplateMeta(w, body.TemplateID, "", nil, &processTemplatePin{group: group}) {
		return
	}
	writeProcessJSON(w, http.StatusOK, map[string]any{"group": group.Name, "templateId": body.TemplateID, "pinned": true})
}

// DELETE /v1/process/groups/{group}/processes/{templateId}
func handleProcessGroupUnpin(w http.ResponseWriter, r *http.Request) {
	if _, ok := requirePermission(w, r, PermProcessTemplatesManage); !ok {
		return
	}
	group, ok := resolveProcessGroupForPin(w, r, r.PathValue("group"))
	if !ok {
		return
	}
	id := r.PathValue("templateId")
	removed, err := db.UnpinProcessFromGroup(group.ID, id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "process_group_pin", err.Error())
		return
	}
	if !removed {
		writeError(w, http.StatusNotFound, "not_found", "the template is not saved to this group")
		return
	}
	writeProcessJSON(w, http.StatusOK, map[string]any{"group": group.Name, "templateId": id, "pinned": false})
}

func processTemplateExists(w http.ResponseWriter, r *http.Request, id string) bool {
	fs, err := store.NewFS(processStoreRoot())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "process_store", err.Error())
		return false
	}
	if _, err := fs.GetTemplateHead(r.Context(), id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not_found", "no such process template")
		} else {
			writeError(w, http.StatusInternalServerError, "process_template_head", err.Error())
		}
		return false
	}
	return true
}

func decodeStrictProcessJSON(w http.ResponseWriter, r *http.Request, target any) error {
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	return dec.Decode(target)
}

// processGroupProcess is one saved process of a group, with what a one-click
// start needs: the start inputs it asks for and the program profiles the
// start must authorize.
type processGroupProcess struct {
	TemplateID      string                     `json:"templateId"`
	Name            string                     `json:"name"`
	Ref             string                     `json:"ref"`
	Listed          bool                       `json:"listed"`
	Steps           int                        `json:"steps"`
	Params          []processGroupProcessParam `json:"params"`
	ProgramProfiles []string                   `json:"programProfiles"`
}

type processGroupProcessParam struct {
	Name        string `json:"name"`
	Type        string `json:"type"`
	Required    bool   `json:"required"`
	Default     any    `json:"default,omitempty"`
	Description string `json:"description,omitempty"`
}

// processSavedTemplateStore is what listing groups' saved processes reads
// once per context build: the template store and the unlisted set.
type processSavedTemplateStore struct {
	fs       *store.FS
	unlisted map[string]bool
	// read memoizes each template read in this build: a template pinned to
	// several groups is read once.
	read map[string]*processGroupProcess
}

// processSavedTemplates opens the store for a context build. A store or
// listing that cannot be read leaves every group's saved processes empty
// rather than failing the whole context.
func processSavedTemplates() processSavedTemplateStore {
	fs, err := store.NewFS(processStoreRoot())
	if err != nil {
		slog.Warn("process context: template store unavailable", "error", err)
		return processSavedTemplateStore{}
	}
	unlisted, err := db.ProcessTemplateUnlisted()
	if err != nil {
		slog.Warn("process context: template listing unavailable", "error", err)
		return processSavedTemplateStore{}
	}
	return processSavedTemplateStore{fs: fs, unlisted: unlisted, read: map[string]*processGroupProcess{}}
}

// groupProcesses lists a group's saved processes in pin order. A pin whose
// template can no longer be read is skipped.
func (s processSavedTemplateStore) groupProcesses(ctx context.Context, groupID int64) []processGroupProcess {
	ids, err := db.ListProcessGroupPins(groupID)
	if err != nil || len(ids) == 0 {
		return []processGroupProcess{}
	}
	out := make([]processGroupProcess, 0, len(ids))
	for _, id := range ids {
		view, seen := s.read[id]
		if !seen {
			view = s.load(ctx, id)
			s.read[id] = view
		}
		if view != nil {
			out = append(out, *view)
		}
	}
	return out
}

// load reads one saved template's head; nil when it can no longer be read.
func (s processSavedTemplateStore) load(ctx context.Context, id string) *processGroupProcess {
	head, err := s.fs.GetTemplateHead(ctx, id)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			slog.Warn("process context: saved process unreadable", "template", id, "error", err)
		}
		return nil
	}
	tmpl, err := s.fs.GetTemplate(ctx, head.Ref)
	if err != nil || tmpl == nil {
		return nil
	}
	view := processGroupProcessOf(tmpl, head.Ref, !s.unlisted[id])
	return &view
}

func processGroupProcessOf(tmpl *model.Template, ref string, listed bool) processGroupProcess {
	view := processGroupProcess{TemplateID: tmpl.ID, Name: tmpl.Name, Ref: ref, Listed: listed,
		Params: []processGroupProcessParam{}, ProgramProfiles: missingProcessProgramAuthorizations(tmpl, nil)}
	if view.Name == "" {
		view.Name = tmpl.ID
	}
	for _, node := range tmpl.Nodes {
		if node.Type == model.NodeTypeTask || node.Type == model.NodeTypeDecision {
			view.Steps++
		}
	}
	// A start must supply a param that has no default and is not explicitly
	// optional, the same rule a subprocess binding applies.
	for _, name := range slices.Sorted(maps.Keys(tmpl.Params)) {
		param := tmpl.Params[name]
		view.Params = append(view.Params, processGroupProcessParam{
			Name: name, Type: param.Type, Required: param.Default == nil && (param.Required == nil || *param.Required),
			Default: param.Default, Description: param.Description,
		})
	}
	return view
}
