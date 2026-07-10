package agentd

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/process/model"
	"github.com/tofutools/tclaude/pkg/claude/process/store"
)

const maxProcessEditBody = 4 << 20

var processTemplateSaveMu sync.Mutex

type processTemplateVersionView struct {
	Ref          string    `json:"ref"`
	SemanticHash string    `json:"semanticHash"`
	SourceHash   string    `json:"sourceHash"`
	StoredAt     time.Time `json:"storedAt"`
}

type processTemplateListView struct {
	ID            string                       `json:"id"`
	Name          string                       `json:"name,omitempty"`
	Description   string                       `json:"description,omitempty"`
	LatestVersion processTemplateVersionView   `json:"latestVersion"`
	VersionCount  int                          `json:"versionCount"`
	Versions      []processTemplateVersionView `json:"versions"`
}

// processTemplateEditView is the editor's lossless wire model. Template holds
// semantic fields only; layout stays separate because it is authoring metadata
// and deliberately does not contribute to SemanticHash. Edges are normalized
// out of nodes[*].next so a graph editor need not reverse-engineer that shape.
type processTemplateEditView struct {
	Template     *model.Template   `json:"template"`
	Edges        []model.Edge      `json:"edges,omitempty"`
	Layout       *model.Layout     `json:"layout,omitempty"`
	SourceHash   string            `json:"sourceHash,omitempty"`
	SemanticHash string            `json:"semanticHash,omitempty"`
	Source       string            `json:"source,omitempty"`
	Diagnostics  []processEditDiag `json:"diagnostics,omitempty"`
}

type processEditDiag struct {
	Scope    string         `json:"scope"`
	TargetID string         `json:"targetId,omitempty"`
	Severity model.Severity `json:"severity"`
	Code     string         `json:"code"`
	Message  string         `json:"message"`
}

func handleProcessTemplates(w http.ResponseWriter, r *http.Request) {
	fs, err := store.NewFS(processStoreRoot())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "process_store", err.Error())
		return
	}
	records, err := fs.ListTemplates(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "process_templates", err.Error())
		return
	}
	grouped := make(map[string][]store.TemplateRecord)
	for _, record := range records {
		grouped[record.ID] = append(grouped[record.ID], record)
	}
	ids := make([]string, 0, len(grouped))
	for id := range grouped {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	views := make([]processTemplateListView, 0, len(ids))
	for _, id := range ids {
		versions := grouped[id]
		slices.SortFunc(versions, compareTemplateRecordsNewest)
		view := processTemplateListView{
			ID:           id,
			VersionCount: len(versions),
			Versions:     make([]processTemplateVersionView, 0, len(versions)),
		}
		for _, record := range versions {
			version, versionErr := processVersionView(r, fs, record)
			if versionErr != nil {
				writeError(w, http.StatusInternalServerError, "process_template", versionErr.Error())
				return
			}
			view.Versions = append(view.Versions, version)
		}
		view.LatestVersion = view.Versions[0]
		latest, loadErr := fs.GetTemplate(r.Context(), versions[0].Ref)
		if loadErr != nil {
			writeError(w, http.StatusInternalServerError, "process_template", loadErr.Error())
			return
		}
		view.Name = latest.Name
		view.Description = latest.Description
		views = append(views, view)
	}
	writeProcessJSON(w, http.StatusOK, map[string]any{"templates": views})
}

func handleProcessTemplate(w http.ResponseWriter, r *http.Request) {
	fs, err := store.NewFS(processStoreRoot())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "process_store", err.Error())
		return
	}
	switch r.Method {
	case http.MethodGet:
		handleProcessTemplateGet(w, r, fs)
	case http.MethodPost:
		handleProcessTemplateSave(w, r, fs)
	default:
		w.Header().Set("Allow", "GET, POST")
		writeError(w, http.StatusMethodNotAllowed, "method", "method not allowed")
	}
}

func handleProcessTemplateGet(w http.ResponseWriter, r *http.Request, fs *store.FS) {
	record, err := resolveProcessTemplateVersion(r, fs, r.PathValue("id"), r.URL.Query().Get("version"))
	if errors.Is(err, store.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, "process_template_version", err.Error())
		return
	}
	view, err := loadProcessTemplateEditView(r, fs, record.Ref)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "process_template", err.Error())
		return
	}
	writeProcessJSON(w, http.StatusOK, view)
}

func handleProcessTemplateSave(w http.ResponseWriter, r *http.Request, fs *store.FS) {
	body, err := decodeProcessEditView(w, r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "json", err.Error())
		return
	}
	if body.Template == nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", "template is required")
		return
	}
	id := r.PathValue("id")
	if body.Template.ID == "" {
		body.Template.ID = id
	}
	if body.Template.ID != id {
		writeError(w, http.StatusBadRequest, "invalid_arg", "template.id must match the path id")
		return
	}
	assembleProcessEditModel(body)
	processTemplateSaveMu.Lock()
	defer processTemplateSaveMu.Unlock()

	// Optimistic concurrency is checked against the current head's canonical
	// source, not its semantic hash: layout-only edits and canonicalized source
	// changes must conflict just like semantic edits.
	records, err := processTemplateRecords(r, fs, id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "process_templates", err.Error())
		return
	}
	if len(records) > 0 {
		headSource, sourceErr := fs.GetTemplateSource(r.Context(), records[0].Ref)
		if sourceErr != nil {
			writeError(w, http.StatusInternalServerError, "process_template", sourceErr.Error())
			return
		}
		currentHash := processSourceHash(headSource)
		if body.SourceHash != currentHash {
			writeProcessJSON(w, http.StatusConflict, map[string]any{
				"error":             "process_template_conflict",
				"message":           "template head changed since it was opened",
				"currentSourceHash": currentHash,
				"currentRef":        records[0].Ref,
			})
			return
		}
	} else if body.SourceHash != "" {
		writeProcessJSON(w, http.StatusConflict, map[string]any{
			"error":   "process_template_conflict",
			"message": "template no longer has the head version that was opened",
		})
		return
	}

	canonical, err := model.CanonicalYAML(body.Template)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, "process_template_unserializable", err.Error())
		return
	}
	parsed, err := model.Parse(canonical)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, "process_template_unserializable", err.Error())
		return
	}
	// Validation findings are deliberately advisory for editor saves. The
	// draft remains serializable and content-addressed, so persisting it lets a
	// human fix multi-step graph edits without the server discarding their work.
	// Only malformed JSON / a model CanonicalYAML cannot represent is blocked.
	record, err := fs.PutTemplateEditorSource(r.Context(), parsed.Template)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, "process_template_unserializable", err.Error())
		return
	}
	view, err := loadProcessTemplateEditView(r, fs, record.Ref)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "process_template", err.Error())
		return
	}
	writeProcessJSON(w, http.StatusCreated, map[string]any{
		"ref":          record.Ref,
		"semanticHash": view.SemanticHash,
		"sourceHash":   view.SourceHash,
		"diagnostics":  diagnosticsForEditor(parsed.Diagnostics),
	})
}

func handleProcessValidate(w http.ResponseWriter, r *http.Request) {
	body, err := decodeProcessEditView(w, r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "json", err.Error())
		return
	}
	if body.Template == nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", "template is required")
		return
	}
	assembleProcessEditModel(body)
	canonical, err := model.CanonicalYAML(body.Template)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, "process_template_unserializable", err.Error())
		return
	}
	parsed, err := model.Parse(canonical)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, "process_template_unserializable", err.Error())
		return
	}
	writeProcessJSON(w, http.StatusOK, map[string]any{"diagnostics": diagnosticsForEditor(parsed.Diagnostics)})
}

func decodeProcessEditView(w http.ResponseWriter, r *http.Request) (*processTemplateEditView, error) {
	r.Body = http.MaxBytesReader(w, r.Body, maxProcessEditBody)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	var body processTemplateEditView
	if err := dec.Decode(&body); err != nil {
		return nil, err
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, fmt.Errorf("request must contain one JSON value")
		}
		return nil, err
	}
	return &body, nil
}

func assembleProcessEditModel(body *processTemplateEditView) {
	body.Template.Layout = body.Layout
	if body.Edges == nil {
		return
	}
	for id, node := range body.Template.Nodes {
		node.Next = nil
		body.Template.Nodes[id] = node
	}
	body.Template.Start = ""
	for _, edge := range body.Edges {
		if edge.From == "" {
			if edge.Outcome == "start" {
				body.Template.Start = edge.To
			}
			continue
		}
		node, ok := body.Template.Nodes[edge.From]
		if !ok {
			continue
		}
		if node.Next == nil {
			node.Next = model.Next{}
		}
		node.Next[edge.Outcome] = edge.To
		body.Template.Nodes[edge.From] = node
	}
}

func loadProcessTemplateEditView(r *http.Request, fs *store.FS, ref string) (*processTemplateEditView, error) {
	source, err := fs.GetTemplateSource(r.Context(), ref)
	if err != nil {
		return nil, err
	}
	parsed, err := model.Parse(source)
	if err != nil {
		return nil, err
	}
	semantic := *parsed.Template
	semantic.Layout = nil
	return &processTemplateEditView{
		Template:     &semantic,
		Edges:        parsed.Edges,
		Layout:       parsed.Template.Layout,
		SourceHash:   parsed.SourceHash,
		SemanticHash: parsed.SemanticHash,
		Source:       string(source),
		Diagnostics:  diagnosticsForEditor(parsed.Diagnostics),
	}, nil
}

func processTemplateRecords(r *http.Request, fs *store.FS, id string) ([]store.TemplateRecord, error) {
	all, err := fs.ListTemplates(r.Context())
	if err != nil {
		return nil, err
	}
	records := make([]store.TemplateRecord, 0)
	for _, record := range all {
		if record.ID == id {
			records = append(records, record)
		}
	}
	slices.SortFunc(records, compareTemplateRecordsNewest)
	return records, nil
}

func resolveProcessTemplateVersion(r *http.Request, fs *store.FS, id, version string) (store.TemplateRecord, error) {
	records, err := processTemplateRecords(r, fs, id)
	if err != nil {
		return store.TemplateRecord{}, err
	}
	if len(records) == 0 {
		return store.TemplateRecord{}, store.ErrNotFound
	}
	version = strings.TrimSpace(version)
	if version == "" {
		return records[0], nil
	}
	wantHash := version
	if refID, refHash, ok := strings.Cut(version, "@sha256:"); ok {
		if refID != id {
			return store.TemplateRecord{}, fmt.Errorf("version ref belongs to template %q", refID)
		}
		wantHash = refHash
	} else {
		wantHash = strings.TrimPrefix(wantHash, "sha256:")
	}
	for _, record := range records {
		if record.SemanticHash == wantHash {
			return record, nil
		}
	}
	return store.TemplateRecord{}, store.ErrNotFound
}

func compareTemplateRecordsNewest(a, b store.TemplateRecord) int {
	if !a.StoredAt.Equal(b.StoredAt) {
		if a.StoredAt.After(b.StoredAt) {
			return -1
		}
		return 1
	}
	return strings.Compare(b.SemanticHash, a.SemanticHash)
}

func processVersionView(r *http.Request, fs *store.FS, record store.TemplateRecord) (processTemplateVersionView, error) {
	source, err := fs.GetTemplateSource(r.Context(), record.Ref)
	if err != nil {
		return processTemplateVersionView{}, err
	}
	return processTemplateVersionView{
		Ref: record.Ref, SemanticHash: record.SemanticHash,
		SourceHash: processSourceHash(source), StoredAt: record.StoredAt,
	}, nil
}

func processSourceHash(source []byte) string {
	sum := sha256.Sum256(source)
	return hex.EncodeToString(sum[:])
}

func diagnosticsForEditor(diagnostics model.Diagnostics) []processEditDiag {
	out := make([]processEditDiag, 0, len(diagnostics))
	for _, diagnostic := range diagnostics {
		scope, target := diagnosticEditorTarget(diagnostic.Path)
		out = append(out, processEditDiag{
			Scope: scope, TargetID: target, Severity: diagnostic.Severity,
			Code: diagnostic.Code, Message: diagnostic.Message,
		})
	}
	return out
}

func diagnosticEditorTarget(path string) (string, string) {
	parts := strings.Split(path, ".")
	if len(parts) >= 4 && parts[0] == "nodes" && parts[2] == "next" {
		return "edge", parts[1] + ":" + strings.Join(parts[3:], ".")
	}
	if len(parts) >= 2 && parts[0] == "nodes" {
		return "node", parts[1]
	}
	if len(parts) >= 3 && parts[0] == "layout" && parts[1] == "nodes" {
		return "node", parts[2]
	}
	return "template", ""
}
