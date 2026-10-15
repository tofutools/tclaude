package agentd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/process/store"
	"github.com/tofutools/tclaude/pkg/common/buildversion"
	"github.com/tofutools/tclaude/pkg/federation/proto"
	"github.com/tofutools/tclaude/pkg/paf"
	"github.com/tofutools/tclaude/pkg/pwf"
)

// Process workflow sharing (docs/pwf.md). Export packs a stored template and
// every template its subprocess steps reference into a .pwf file; import
// previews one and, when asked, stores its templates. Neither ever starts a
// run: a run asks the operator to authorize each program profile when it
// starts, so nothing a file carries can execute on import.

// maxPWFUpload bounds an uploaded .pwf. Templates are small; a workflow of
// many templates still fits comfortably.
const maxPWFUpload = 32 << 20

// GET /v1/process/templates/{id}/export?version=&title=&summary=
func handleProcessTemplateExport(w http.ResponseWriter, r *http.Request) {
	if _, ok := requirePermission(w, r, PermProcessTemplatesRead); !ok {
		return
	}
	fs, err := store.NewFS(processStoreRoot())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "process_store", err.Error())
		return
	}
	q := r.URL.Query()
	bundle, err := collectPWFBundle(r, fs, r.PathValue("id"), q.Get("version"))
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "process template not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, "process_export", err.Error())
		return
	}
	bundle.Title, bundle.Summary = q.Get("title"), q.Get("summary")
	opts := pwf.WriteOptions{Producer: paf.Producer{Name: "tclaude", Version: buildversion.AppVersion()}}
	// Signed only when this node already has a federation identity; exporting
	// never creates one (the same rule as a local PAF export).
	if id, err := proto.LoadIdentity(FederationKeyPath()); err == nil {
		opts.Keys = []paf.SigningKey{{Key: id.Priv, KeyID: id.ID(), Signer: &paf.Signer{Kind: "instance"}}}
	}
	var buf bytes.Buffer
	if err := pwf.Write(&buf, bundle, opts); err != nil {
		writeError(w, http.StatusUnprocessableEntity, "process_export", err.Error())
		return
	}
	w.Header().Set("Content-Type", pwf.MediaType)
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s.pwf"`, r.PathValue("id")))
	_, _ = w.Write(buf.Bytes())
}

// collectPWFBundle loads a template version and, transitively, every stored
// template its subprocess steps reference: the pinned version, or the head
// for an unpinned reference (marked head so it resolves the same way on the
// importing node).
func collectPWFBundle(r *http.Request, fs *store.FS, id, version string) (pwf.Bundle, error) {
	root, err := resolveProcessTemplateVersion(r, fs, id, version)
	if err != nil {
		return pwf.Bundle{}, err
	}
	ctx := r.Context()
	byRef := map[string]*pwf.Template{}
	var order []string
	var load func(ref string, head bool, depth int) error
	load = func(ref string, head bool, depth int) error {
		if t := byRef[ref]; t != nil {
			t.Head = t.Head || head
			return nil
		}
		if depth > 64 {
			return errors.New("subprocess references nest too deeply")
		}
		source, err := fs.GetTemplateSource(ctx, ref)
		if err != nil {
			return fmt.Errorf("load %s: %w", ref, err)
		}
		tmpl, parsedRef, err := pwf.ParseTemplate(source)
		if err != nil {
			return err
		}
		if parsedRef != ref {
			return fmt.Errorf("stored template %s hashes to %s", ref, parsedRef)
		}
		byRef[ref] = &pwf.Template{Ref: ref, Head: head, Source: source}
		order = append(order, ref)
		for _, sp := range pwf.SubprocessRefs(tmpl) {
			if sp.Version != "" {
				if err := load(sp.TemplateRef+"@"+sp.Version, false, depth+1); err != nil {
					return err
				}
				continue
			}
			child, err := fs.GetTemplateHead(ctx, sp.TemplateRef)
			if err != nil {
				return fmt.Errorf("template %q references %q, which has no saved version: %w", tmpl.ID, sp.TemplateRef, err)
			}
			if err := load(child.Ref, true, depth+1); err != nil {
				return err
			}
		}
		return nil
	}
	if err := load(root.Ref, true, 0); err != nil {
		return pwf.Bundle{}, err
	}
	bundle := pwf.Bundle{Root: root.Ref}
	for _, ref := range order {
		bundle.Templates = append(bundle.Templates, *byRef[ref])
	}
	return bundle, nil
}

// pwfImportView is the review an importer acts on: what the workflow is,
// who signed it, what it needs, and what importing would change locally.
type pwfImportView struct {
	Title      string                `json:"title"`
	Summary    string                `json:"summary,omitempty"`
	Root       string                `json:"root"`
	RootID     string                `json:"rootId"`
	Readme     string                `json:"readme,omitempty"`
	Signatures []pwfSignatureView    `json:"signatures"`
	Params     []pwf.Param           `json:"params"`
	Roles      []pwf.Use             `json:"roles"`
	Programs   []pwf.Program         `json:"programs"`
	Assignees  []pwf.Use             `json:"assignees"`
	Humans     []string              `json:"humans"`
	Templates  []pwfTemplateImported `json:"templates"`
	// Updates counts templates whose local head the import would move; an
	// import with updates needs replace.
	Updates int  `json:"updates"`
	Applied bool `json:"applied"`
}

type pwfSignatureView struct {
	KeyID  string `json:"keyId"`
	Status string `json:"status"`
	// Trust is "this node", "known peer <name>" or "unknown key", decided by
	// the public key alone; empty unless the signature is valid.
	Trust string `json:"trust,omitempty"`
}

// pwfTemplateImported is one template's import status:
//   - new: not stored here; it is stored (as head when Head).
//   - identical: this exact version is already stored (and is the head when
//     Head), so nothing changes.
//   - update: the id exists here with a different head; importing moves
//     the head to this version, and needs replace.
//   - version: a pinned non-head version; it is stored without touching
//     the local head.
type pwfTemplateImported struct {
	Ref    string `json:"ref"`
	ID     string `json:"id"`
	Name   string `json:"name,omitempty"`
	Head   bool   `json:"head"`
	Status string `json:"status"`
	// CurrentRef is the local head when Status is update.
	CurrentRef string `json:"currentRef,omitempty"`
}

// POST /v1/process/import[?apply=1[&replace=1]] with a .pwf body. Without
// apply it is a preview and changes nothing.
func handleProcessImport(w http.ResponseWriter, r *http.Request) {
	caller, ok := requirePermission(w, r, PermProcessTemplatesManage)
	if !ok {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxPWFUpload)
	data, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, "too_large", fmt.Sprintf("a workflow file is at most %d bytes", maxPWFUpload))
		return
	}
	archive, err := pwf.Open(bytes.NewReader(data), int64(len(data)), paf.Limits{})
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, "invalid_pwf", err.Error())
		return
	}
	fs, err := store.NewFS(processStoreRoot())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "process_store", err.Error())
		return
	}
	view, err := pwfImportPreview(r.Context(), fs, archive)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "process_import", err.Error())
		return
	}
	q := r.URL.Query()
	if q.Get("apply") != "1" {
		writeProcessJSON(w, http.StatusOK, view)
		return
	}
	if view.Updates > 0 && q.Get("replace") != "1" {
		writeProcessJSON(w, http.StatusConflict, map[string]any{
			"error":   "process_import_conflict",
			"message": fmt.Sprintf("%d template(s) already exist here with different content; import with replace to make the imported versions current", view.Updates),
			"preview": view,
		})
		return
	}
	actor, err := processTemplateAuthor(caller)
	if err != nil {
		writeError(w, http.StatusForbidden, "process_actor", err.Error())
		return
	}
	for i := range view.Templates {
		row := &view.Templates[i]
		tmpl := archive.Templates[row.Ref].Parsed
		switch row.Status {
		case "identical":
			continue
		case "version":
			_, err = fs.PutTemplateVersion(r.Context(), tmpl)
		case "new", "update":
			expected := ""
			if row.Status == "update" {
				head, herr := fs.GetTemplateHeadGeneration(r.Context(), row.ID)
				if herr != nil {
					err = herr
					break
				}
				expected = head.SourceHash
			}
			_, err = fs.PutTemplateEditorSourceAttributed(r.Context(), tmpl, expected, actor)
		}
		if err != nil {
			writeError(w, http.StatusConflict, "process_import", fmt.Sprintf("store %s: %v (templates before it were imported)", row.Ref, err))
			return
		}
	}
	view.Applied = true
	slog.Info("process workflow imported", "root", view.Root, "templates", len(view.Templates), "actor", actor)
	writeProcessJSON(w, http.StatusOK, view)
}

func pwfImportPreview(ctx context.Context, fs *store.FS, a *pwf.Archive) (pwfImportView, error) {
	wf := a.Workflow
	view := pwfImportView{
		Title: wf.Title, Summary: wf.Summary, Root: wf.Root, RootID: a.Root().Parsed.ID, Readme: string(a.Readme),
		Params: wf.Params, Roles: wf.Roles, Programs: wf.Programs, Assignees: wf.Assignees, Humans: wf.Humans,
		Signatures: pwfSignatureViews(a.Signatures),
	}
	for _, ref := range a.Order {
		t := a.Templates[ref]
		row := pwfTemplateImported{Ref: ref, ID: t.Parsed.ID, Name: t.Parsed.Name, Head: t.Head}
		_, storedErr := fs.GetTemplate(ctx, ref)
		stored := storedErr == nil
		if storedErr != nil && !errors.Is(storedErr, store.ErrNotFound) && !errors.Is(storedErr, os.ErrNotExist) {
			return view, storedErr
		}
		head, headErr := fs.GetTemplateHead(ctx, t.Parsed.ID)
		hasHead := headErr == nil
		if headErr != nil && !errors.Is(headErr, store.ErrNotFound) && !errors.Is(headErr, os.ErrNotExist) {
			return view, headErr
		}
		switch {
		case !t.Head && stored:
			row.Status = "identical"
		case !t.Head:
			row.Status = "version"
		case hasHead && head.Ref == ref:
			row.Status = "identical"
		case hasHead:
			row.Status, row.CurrentRef = "update", head.Ref
			view.Updates++
		default:
			row.Status = "new"
		}
		view.Templates = append(view.Templates, row)
	}
	return view, nil
}

// pwfSignatureViews labels valid signatures by key: this node, a known
// federation peer, or unknown. Labels the file asserts are never trusted.
func pwfSignatureViews(results []paf.SignatureResult) []pwfSignatureView {
	views := []pwfSignatureView{}
	var self string
	if id, err := proto.LoadIdentity(FederationKeyPath()); err == nil {
		self = id.ID()
	}
	var peers []db.FederationPeer
	if _, err := os.Stat(db.DBPath()); err == nil {
		peers, _ = db.ListFederationPeers()
	}
	for _, res := range results {
		v := pwfSignatureView{KeyID: res.Signature.KeyID, Status: string(res.Status)}
		if res.Status == paf.SignatureValid {
			pub := res.Signature.PublicKey
			id := proto.InstanceID(pub)
			v.Trust = "unknown key"
			if id == self {
				v.Trust = "this node"
			}
			for _, p := range peers {
				if p.InstanceID == id && bytes.Equal(p.PubKey, pub) {
					name := p.Label
					if name == "" {
						name = p.Name
					}
					v.Trust = strings.TrimSpace("known peer " + name)
				}
			}
		}
		views = append(views, v)
	}
	return views
}
