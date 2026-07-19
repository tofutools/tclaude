package agentd

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/tofutools/tclaude/pkg/claude/process/model"
	"github.com/tofutools/tclaude/pkg/claude/process/state"
	"github.com/tofutools/tclaude/pkg/claude/process/store"
)

const maxProcessEditBody = model.MaxProcessTemplateSourceBytes

type processTemplateVersionView struct {
	Ref          string         `json:"ref"`
	SemanticHash string         `json:"semanticHash"`
	SourceHash   string         `json:"sourceHash"`
	StoredAt     time.Time      `json:"storedAt"`
	Actor        state.ActorRef `json:"actor,omitempty"`
	AuthoredAt   *time.Time     `json:"authoredAt,omitempty"`
}

type processTemplateListView struct {
	ID            string                       `json:"id"`
	Name          string                       `json:"name,omitempty"`
	Description   string                       `json:"description,omitempty"`
	LatestVersion processTemplateVersionView   `json:"latestVersion"`
	VersionCount  int                          `json:"versionCount"`
	Versions      []processTemplateVersionView `json:"versions"`
}

// processTemplateHeadView is the bounded polling shape plus provenance for
// that exact committed generation. Attribution stays optional: legacy or
// hand-written versions may have none, and callers must not infer an identity.
// Ref + SourceHash remain the generation authority.
type processTemplateHeadView struct {
	ID         string         `json:"id"`
	Ref        string         `json:"ref"`
	SourceHash string         `json:"sourceHash"`
	Actor      state.ActorRef `json:"actor,omitempty"`
	AuthoredAt *time.Time     `json:"authoredAt,omitempty"`
}

// processTemplateEditView is the editor's lossless wire model. Template holds
// semantic fields only; layout stays separate because it is authoring metadata
// and deliberately does not contribute to SemanticHash. Edges are normalized
// out of nodes[*].next so a graph editor need not reverse-engineer that shape.
type processTemplateEditView struct {
	Template      *model.Template            `json:"template"`
	Edges         []model.Edge               `json:"edges,omitempty"`
	Layout        *model.Layout              `json:"layout,omitempty"`
	SourceHash    string                     `json:"sourceHash,omitempty"`
	SemanticHash  string                     `json:"semanticHash,omitempty"`
	CurrentRef    string                     `json:"currentRef,omitempty"`
	Source        string                     `json:"source,omitempty"`
	Actor         state.ActorRef             `json:"actor,omitempty"`
	AuthoredAt    *time.Time                 `json:"authoredAt,omitempty"`
	Diagnostics   []processEditDiag          `json:"diagnostics,omitempty"`
	Authorship    []store.TemplateAuthorship `json:"authorship,omitempty"`
	layoutPresent bool
}

type processEditDiag struct {
	Scope    string         `json:"scope"`
	TargetID string         `json:"targetId,omitempty"`
	Severity model.Severity `json:"severity"`
	Code     string         `json:"code"`
	Message  string         `json:"message"`
}

type processEditModelError struct{ err error }

func (e *processEditModelError) Error() string { return e.err.Error() }
func (e *processEditModelError) Unwrap() error { return e.err }

type processTemplateStoredValidationError struct {
	diagnostics model.Diagnostics
}

func (e *processTemplateStoredValidationError) Error() string {
	return "stored process template is incompatible with current validation limits"
}

func handleProcessTemplates(w http.ResponseWriter, r *http.Request) {
	if _, ok := requirePermission(w, r, PermProcessTemplatesRead); !ok {
		return
	}
	fs, err := store.NewFS(processStoreRoot())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "process_store", err.Error())
		return
	}
	// This endpoint is loaded on an explicit dashboard tab visit, not polled.
	// Its per-template version scans favor a simple durable store layout; add an
	// index if template counts make this observable in real workloads.
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
		head, headErr := fs.GetTemplateHead(r.Context(), id)
		if headErr != nil {
			writeError(w, http.StatusInternalServerError, "process_template_head", headErr.Error())
			return
		}
		// A writer publishes immutable version content before atomically moving
		// the head pointer. If that happens between the collection snapshot above
		// and this head read, include the newly selected head so latestVersion,
		// versions, and description still describe one internally consistent view.
		headListed := false
		for _, version := range versions {
			if version.Ref == head.Ref {
				headListed = true
				break
			}
		}
		if !headListed {
			versions = append(versions, head)
		}
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
		for _, version := range view.Versions {
			if version.Ref == head.Ref {
				view.LatestVersion = version
				break
			}
		}
		latest, loadErr := fs.GetTemplate(r.Context(), head.Ref)
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

func handleProcessTemplateHeads(w http.ResponseWriter, r *http.Request) {
	if _, ok := requirePermission(w, r, PermProcessTemplatesRead); !ok {
		return
	}
	fs, err := store.NewFS(processStoreRoot())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "process_store", err.Error())
		return
	}
	heads, err := fs.ListTemplateHeads(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "process_template_heads", err.Error())
		return
	}
	views := make([]processTemplateHeadView, 0, len(heads))
	for _, head := range heads {
		views = append(views, processTemplateHeadView{
			ID: head.ID, Ref: head.Ref, SourceHash: head.SourceHash,
			Actor: head.Actor, AuthoredAt: head.AuthoredAt,
		})
	}
	writeProcessJSON(w, http.StatusOK, map[string]any{"heads": views})
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
	case http.MethodDelete:
		handleProcessTemplateDelete(w, r, fs)
	default:
		w.Header().Set("Allow", "GET, POST, DELETE")
		writeError(w, http.StatusMethodNotAllowed, "method", "method not allowed")
	}
}

// handleProcessTemplateDelete removes a template and its whole version history.
// The store refuses while unfinished runs still reference it, which surfaces
// here as 409 plus the blocking run ids so the caller can act on them.
func handleProcessTemplateDelete(w http.ResponseWriter, r *http.Request, fs *store.FS) {
	if _, ok := requirePermission(w, r, PermProcessTemplatesManage); !ok {
		return
	}
	id := r.PathValue("id")
	// Classify a malformed id as client error before it reaches the filesystem,
	// where it would otherwise surface as an apparent store fault.
	if err := store.ValidateTemplateID(id); err != nil {
		writeError(w, http.StatusBadRequest, "process_template_id", err.Error())
		return
	}
	err := fs.DeleteTemplate(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	var inUse *store.TemplateInUseError
	if errors.As(err, &inUse) {
		writeProcessJSON(w, http.StatusConflict, map[string]any{
			"error":            inUse.Error(),
			"code":             "process_template_in_use",
			"runIds":           inUse.RunIDs,
			"unreadableRunIds": inUse.UnreadableRunIDs,
		})
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "process_template_delete", err.Error())
		return
	}
	writeProcessJSON(w, http.StatusOK, map[string]any{"deleted": id})
}

func handleProcessTemplateGet(w http.ResponseWriter, r *http.Request, fs *store.FS) {
	if _, ok := requirePermission(w, r, PermProcessTemplatesRead); !ok {
		return
	}
	version := r.URL.Query().Get("version")
	includeAuthorship := r.URL.Query().Get("authorship") != "omit"
	var exactHead *store.TemplateHead
	var record store.TemplateRecord
	var err error
	if !includeAuthorship && version == "" {
		head, headErr := fs.GetTemplateHeadGeneration(r.Context(), r.PathValue("id"))
		if headErr == nil {
			exactHead = &head
			record = store.TemplateRecord{ID: head.ID, Ref: head.Ref}
		}
		err = headErr
	} else {
		record, err = resolveProcessTemplateVersion(r, fs, r.PathValue("id"), version)
	}
	if errors.Is(err, store.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, "process_template_version", err.Error())
		return
	}
	view, err := loadProcessTemplateEditView(r, fs, record.Ref, includeAuthorship)
	var storedValidationErr *processTemplateStoredValidationError
	if errors.As(err, &storedValidationErr) {
		writeProcessJSON(w, http.StatusUnprocessableEntity, map[string]any{
			"error":       storedValidationErr.Error(),
			"code":        "process_template_invalid",
			"diagnostics": diagnosticsForEditor(storedValidationErr.diagnostics, nil),
		})
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "process_template", err.Error())
		return
	}
	// The source read and head observation use separate short locks so writers
	// are not blocked while YAML is parsed. Publish attribution only when the
	// returned source still matches the exact observed ref+sourceHash pair.
	if exactHead != nil && view.CurrentRef == exactHead.Ref && view.SourceHash == exactHead.SourceHash {
		view.Actor = exactHead.Actor
		view.AuthoredAt = exactHead.AuthoredAt
	}
	writeProcessJSON(w, http.StatusOK, view)
}

// handleProcessTemplateCreate mints the id for a brand-new template. Ids are a
// permanent store key embedded in every id@sha256:<hash> ref, so they are
// generated here rather than typed by an operator: a hand-picked id is a
// naming decision that cannot be taken back, and the display name -- which is
// renameable -- is the thing a human should be choosing. A compact lowercase
// hex UUID satisfies the existing id grammar with no validation change.
//
// The authored YAML/CLI path deliberately keeps supplying its own ids: refs are
// content-addressed against them and docs/examples rely on stable names. This
// endpoint is creation-from-the-dashboard only.
func handleProcessTemplateCreate(w http.ResponseWriter, r *http.Request) {
	caller, ok := requirePermission(w, r, PermProcessTemplatesManage)
	if !ok {
		return
	}
	fs, err := store.NewFS(processStoreRoot())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "process_store", err.Error())
		return
	}
	body, err := decodeProcessEditView(w, r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "json", err.Error())
		return
	}
	if body.Template == nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", "template is required")
		return
	}
	if strings.TrimSpace(body.Template.ID) != "" {
		writeError(w, http.StatusBadRequest, "invalid_arg",
			"template ids are generated; omit template.id when creating and POST to /v1/process/templates/{id} to update an existing template")
		return
	}
	if body.SourceHash != "" {
		writeError(w, http.StatusBadRequest, "invalid_arg", "a new template has no prior version to save against")
		return
	}
	body.Template.ID = newProcessTemplateID()
	parsed, err := parseProcessEditView(body)
	if err != nil {
		writeProcessEditParseError(w, err)
		return
	}
	if model.PreflightNormalizedGraphCardinality(parsed.Template).HasErrors() ||
		parsed.Diagnostics.HasNormalizedGraphBudgetError() {
		writeProcessJSON(w, http.StatusUnprocessableEntity, map[string]any{
			"error":       "process template has validation errors; run process-templates validate and fix them before saving",
			"code":        "process_template_invalid",
			"diagnostics": diagnosticsForEditor(parsed.Diagnostics, parsed.Template),
		})
		return
	}
	if err := store.ValidateTemplateID(parsed.Template.ID); err != nil {
		writeError(w, http.StatusInternalServerError, "process_template_invalid_id", err.Error())
		return
	}
	actor, err := processTemplateAuthor(caller)
	if err != nil {
		writeError(w, http.StatusForbidden, "auth", err.Error())
		return
	}
	// An empty expectation means "must not exist", so a generated id that
	// somehow collided surfaces as a conflict rather than stacking a version
	// onto an unrelated template.
	commit, err := fs.PutTemplateEditorSourceAttributed(r.Context(), parsed.Template, "", actor)
	var conflict *store.TemplateSourceConflictError
	if errors.As(err, &conflict) {
		writeError(w, http.StatusConflict, "process_template_conflict", "generated template id already exists; retry")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "process_template_store", err.Error())
		return
	}
	writeProcessJSON(w, http.StatusCreated, map[string]any{
		"id":           parsed.Template.ID,
		"ref":          commit.Ref,
		"semanticHash": commit.SemanticHash,
		"sourceHash":   commit.SourceHash,
		"actor":        commit.Actor,
		"authoredAt":   commit.AuthoredAt,
		"diagnostics":  diagnosticsForEditor(parsed.Diagnostics, parsed.Template),
	})
}

// newProcessTemplateID returns a compact (dashless) lowercase hex UUID, which
// is already a valid template id under ^[a-z0-9][a-z0-9._-]*$.
func newProcessTemplateID() string {
	return strings.ReplaceAll(uuid.NewString(), "-", "")
}

func handleProcessTemplateSave(w http.ResponseWriter, r *http.Request, fs *store.FS) {
	caller, ok := requirePermission(w, r, PermProcessTemplatesManage)
	if !ok {
		return
	}
	body, err := decodeProcessEditView(w, r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "json", err.Error())
		return
	}
	if body.Template == nil && strings.TrimSpace(body.Source) == "" {
		writeError(w, http.StatusBadRequest, "invalid_arg", "template or source is required")
		return
	}
	id := r.PathValue("id")
	if body.Template != nil && body.Template.ID == "" {
		// Preserve the dashboard edit-wire convenience: the path supplies an
		// omitted id before canonicalization/validation. Raw YAML is a complete
		// document and must carry its own id.
		body.Template.ID = id
	}
	rawSource := body.Template == nil
	parsed, err := parseProcessEditView(body)
	if err != nil {
		writeProcessEditParseError(w, err)
		return
	}
	cardinalityErrors := model.PreflightNormalizedGraphCardinality(parsed.Template).HasErrors()
	resourceErrors := parsed.Diagnostics.HasNormalizedGraphBudgetError()
	if (rawSource && parsed.Diagnostics.HasErrors()) || cardinalityErrors || resourceErrors {
		writeProcessJSON(w, http.StatusUnprocessableEntity, map[string]any{
			"error":       "process template has validation errors; run process-templates validate and fix them before saving",
			"code":        "process_template_invalid",
			"diagnostics": diagnosticsForEditor(parsed.Diagnostics, parsed.Template),
		})
		return
	}
	body.Template = parsed.Template
	if body.Template.ID != id {
		writeError(w, http.StatusBadRequest, "invalid_arg", "template.id must match the path id")
		return
	}
	if err := store.ValidateTemplateID(id); err != nil {
		writeError(w, http.StatusUnprocessableEntity, "process_template_invalid_id", err.Error())
		return
	}
	// Validation findings are deliberately advisory for editor saves. The
	// draft remains serializable and content-addressed, so persisting it lets a
	// human fix multi-step graph edits without the server discarding their work.
	// Only malformed JSON, a model CanonicalYAML cannot represent, or a template
	// identity the content-addressed store cannot safely key is blocked.
	actor, err := processTemplateAuthor(caller)
	if err != nil {
		writeError(w, http.StatusForbidden, "auth", err.Error())
		return
	}
	commit, err := fs.PutTemplateEditorSourceAttributed(r.Context(), parsed.Template, body.SourceHash, actor)
	var conflict *store.TemplateSourceConflictError
	if errors.As(err, &conflict) {
		writeProcessJSON(w, http.StatusConflict, map[string]any{
			"error":             "template head changed since it was opened",
			"code":              "process_template_conflict",
			"currentSourceHash": conflict.CurrentSourceHash,
			"currentRef":        conflict.CurrentRef,
		})
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "process_template_store", err.Error())
		return
	}
	writeProcessJSON(w, http.StatusCreated, map[string]any{
		"ref":          commit.Ref,
		"semanticHash": commit.SemanticHash,
		"sourceHash":   commit.SourceHash,
		"actor":        commit.Actor,
		"authoredAt":   commit.AuthoredAt,
		"diagnostics":  diagnosticsForEditor(parsed.Diagnostics, parsed.Template),
	})
}

func handleProcessValidate(w http.ResponseWriter, r *http.Request) {
	if _, ok := requirePermission(w, r, PermProcessTemplatesRead); !ok {
		return
	}
	body, err := decodeProcessEditView(w, r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "json", err.Error())
		return
	}
	if body.Template == nil && strings.TrimSpace(body.Source) == "" {
		writeError(w, http.StatusBadRequest, "invalid_arg", "template or source is required")
		return
	}
	parsed, err := parseProcessEditView(body)
	if err != nil {
		writeProcessEditParseError(w, err)
		return
	}
	writeProcessJSON(w, http.StatusOK, map[string]any{
		"sourceHash": parsed.SourceHash, "semanticHash": parsed.SemanticHash,
		"diagnostics": diagnosticsForEditor(parsed.Diagnostics, parsed.Template),
	})
}

func parseProcessEditView(body *processTemplateEditView) (*model.ParsedTemplate, error) {
	if body == nil {
		return nil, fmt.Errorf("template is required")
	}
	if body.Template == nil {
		if strings.TrimSpace(body.Source) == "" {
			return nil, fmt.Errorf("template or source is required")
		}
		return model.Parse([]byte(body.Source))
	}
	if err := assembleProcessEditModel(body); err != nil {
		return nil, &processEditModelError{err: err}
	}
	edges, cardinalityDiagnostics := model.NormalizeEdgesWithinBudget(body.Template)
	if cardinalityDiagnostics.HasErrors() {
		return &model.ParsedTemplate{
			Template: body.Template, Edges: edges, Diagnostics: cardinalityDiagnostics,
		}, nil
	}
	canonical, err := model.CanonicalYAML(body.Template)
	if err != nil {
		return nil, err
	}
	return model.Parse(canonical)
}

func writeProcessEditParseError(w http.ResponseWriter, err error) {
	var editErr *processEditModelError
	if errors.As(err, &editErr) {
		writeError(w, http.StatusUnprocessableEntity, "process_template_edit_model", editErr.Error())
		return
	}
	writeError(w, http.StatusUnprocessableEntity, "process_template_unserializable", err.Error())
}

func processTemplateAuthor(callerConv string) (state.ActorRef, error) {
	if callerConv == "" {
		return state.ActorRef("human:operator"), nil
	}
	agentID := peerAgentID(callerConv)
	if agentID == "" {
		return "", fmt.Errorf("caller has no stable agent identity")
	}
	return state.ActorRef("agent:" + agentID), nil
}

func decodeProcessEditView(w http.ResponseWriter, r *http.Request) (*processTemplateEditView, error) {
	r.Body = http.MaxBytesReader(w, r.Body, maxProcessEditBody)
	data, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(data))
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
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return nil, err
	}
	_, body.layoutPresent = fields["layout"]
	return &body, nil
}

func assembleProcessEditModel(body *processTemplateEditView) error {
	// A top-level layout is the editor wire model's authoritative value when
	// present. For API clients that send a complete Template directly, retain
	// template.layout when the top-level field is omitted instead of clearing it.
	if body.layoutPresent {
		body.Template.Layout = body.Layout
	} else {
		body.Layout = body.Template.Layout
	}
	if body.Edges == nil {
		return nil
	}
	for id, node := range body.Template.Nodes {
		node.Next = nil
		body.Template.Nodes[id] = node
	}
	body.Template.Start = ""
	seen := make(map[string]struct{}, len(body.Edges))
	for _, edge := range body.Edges {
		key := edge.From + "\x00" + edge.Outcome
		if _, exists := seen[key]; exists {
			return fmt.Errorf("duplicate edge for from %q and outcome %q", edge.From, edge.Outcome)
		}
		seen[key] = struct{}{}
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
	return nil
}

func loadProcessTemplateEditView(r *http.Request, fs *store.FS, ref string, includeAuthorship bool) (*processTemplateEditView, error) {
	var source []byte
	var authorship []store.TemplateAuthorship
	if includeAuthorship {
		snapshot, err := fs.GetTemplateAuthoringSnapshot(r.Context(), ref)
		if err != nil {
			return nil, err
		}
		source, authorship = snapshot.Source, snapshot.Authorship
	} else {
		var err error
		source, err = fs.GetTemplateSource(r.Context(), ref)
		if err != nil {
			return nil, err
		}
	}
	parsed, err := model.Parse(source)
	if err != nil {
		return nil, err
	}
	if parsed.Template == nil || parsed.Diagnostics.HasNormalizedGraphBudgetError() {
		return nil, &processTemplateStoredValidationError{diagnostics: parsed.Diagnostics}
	}
	semantic := *parsed.Template
	semantic.Layout = nil
	return &processTemplateEditView{
		Template:     &semantic,
		Edges:        parsed.Edges,
		Layout:       parsed.Template.Layout,
		SourceHash:   parsed.SourceHash,
		SemanticHash: parsed.SemanticHash,
		CurrentRef:   ref,
		Source:       string(source),
		Diagnostics:  diagnosticsForEditor(parsed.Diagnostics, parsed.Template),
		Authorship:   authorship,
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
		return fs.GetTemplateHead(r.Context(), id)
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
	snapshot, err := fs.GetTemplateAuthoringSnapshot(r.Context(), record.Ref)
	if err != nil {
		return processTemplateVersionView{}, err
	}
	view := processTemplateVersionView{
		Ref: record.Ref, SemanticHash: record.SemanticHash,
		SourceHash: processSourceHash(snapshot.Source), StoredAt: record.StoredAt,
	}
	if len(snapshot.Authorship) > 0 {
		latest := snapshot.Authorship[len(snapshot.Authorship)-1]
		view.Actor = latest.Actor
		view.AuthoredAt = &latest.AuthoredAt
	}
	return view, nil
}

func processSourceHash(source []byte) string {
	sum := sha256.Sum256(source)
	return hex.EncodeToString(sum[:])
}

func diagnosticsForEditor(diagnostics model.Diagnostics, tmpl *model.Template) []processEditDiag {
	out := make([]processEditDiag, 0, len(diagnostics))
	for _, diagnostic := range diagnostics {
		scope, target := diagnosticEditorTarget(diagnostic.Path, tmpl)
		out = append(out, processEditDiag{
			Scope: scope, TargetID: target, Severity: diagnostic.Severity,
			Code: diagnostic.Code, Message: diagnostic.Message,
		})
	}
	encoded, err := json.Marshal(out)
	if err != nil || len(encoded) > model.MaxTemplateDiagnosticWireBytes {
		sentinel := model.TemplateDiagnosticBudgetDiagnostic()
		return []processEditDiag{{
			Scope: "template", Severity: sentinel.Severity,
			Code: sentinel.Code, Message: sentinel.Message,
		}}
	}
	return out
}

func diagnosticEditorTarget(path string, tmpl *model.Template) (string, string) {
	if rest, ok := strings.CutPrefix(path, "layout.nodes."); ok && rest != "" {
		return "node", rest
	}
	rest, ok := strings.CutPrefix(path, "nodes.")
	if !ok || rest == "" {
		return "template", ""
	}
	nodeID, field := splitDiagnosticNodePath(rest, tmpl)
	if outcome, isEdge := strings.CutPrefix(field, "next."); isEdge {
		return "edge", nodeID + ":" + outcome
	}
	return "node", nodeID
}

// splitDiagnosticNodePath separates a diagnostic path's node id from its field
// path. Node ids may themselves contain dots ("a.test"), so a naive split on
// the first dot misanchors badges; the declared node set disambiguates — the
// longest declared id that prefixes the path wins. Undeclared ids (e.g. a
// derived child-stage id in an expansion-collision diagnostic) fall back to
// treating everything before a known field-ish suffix as unavailable and use
// the first segment.
func splitDiagnosticNodePath(rest string, tmpl *model.Template) (string, string) {
	best := ""
	if tmpl != nil {
		for id := range tmpl.Nodes {
			if len(id) <= len(best) {
				continue
			}
			if rest == id || (strings.HasPrefix(rest, id) && rest[len(id)] == '.') {
				best = id
			}
		}
	}
	if best == "" {
		best, _, _ = strings.Cut(rest, ".")
	}
	return best, strings.TrimPrefix(strings.TrimPrefix(rest, best), ".")
}
