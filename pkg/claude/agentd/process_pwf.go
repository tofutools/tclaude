package agentd

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"regexp"
	"slices"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/process/model"
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

// GET /v1/process/templates/{id}/export?version=&title=&summary=&author=&licence=[&check=1]
// [&assignee=<agent>=<role>...][&assignees=roles][&strict=1]
// [&performer=<role>=<agent>...[&performer_history=1][&allow_flagged=true]]
//
// A template that fails validation exports as it is, as the store saves it:
// the file carries it and every reader reports its problems. With check=1
// the response is those problems and the machine-specific paths the file
// would carry as JSON instead of the file, so the dashboard can ask before
// sharing a workflow that cannot run yet or that reveals a username. It also
// lists the steps naming a specific agent, with the agent's role here when it
// has exactly one.
//
// assignee= and assignees=roles export those steps with a role instead (see
// process_pwf_export.go). strict=1 refuses to export anything a check would
// point out.
//
// performer= ships an agent for a role, which makes the file a .paf that holds
// both (see process_envelope.go).
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
	bundle.Title, bundle.Summary, bundle.Author, bundle.Licence = q.Get("title"), q.Get("summary"), q.Get("author"), q.Get("licence")
	if q.Get("check") == "1" {
		problems, err := pwf.Problems(bundle)
		if err != nil {
			writeError(w, http.StatusUnprocessableEntity, "process_export", err.Error())
			return
		}
		if problems == nil {
			problems = []string{}
		}
		paths, err := pwf.ScanPaths(bundle)
		if err != nil {
			writeError(w, http.StatusUnprocessableEntity, "process_export", err.Error())
			return
		}
		if paths == nil {
			paths = []pwf.PathFinding{}
		}
		assignees, err := pwfAssigneesWithRoles(r, bundle)
		if err != nil {
			writeError(w, http.StatusUnprocessableEntity, "process_export", err.Error())
			return
		}
		writeProcessJSON(w, http.StatusOK, map[string]any{"problems": problems, "paths": paths, "assignees": assignees})
		return
	}
	roles, err := pwfExportRoles(r, bundle)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", err.Error())
		return
	}
	if bundle, err = pwfConvertAssignees(bundle, roles); err != nil {
		writeError(w, http.StatusUnprocessableEntity, "process_export", err.Error())
		return
	}
	if q.Get("strict") == "1" {
		issues, err := pwfStrictIssues(bundle)
		if err != nil {
			writeError(w, http.StatusUnprocessableEntity, "process_export", err.Error())
			return
		}
		if len(issues) > 0 {
			writeError(w, http.StatusUnprocessableEntity, "process_export_not_portable",
				"not exported, because the workflow is not portable:\n  "+strings.Join(issues, "\n  "))
			return
		}
	}
	performers, err := parseWorkflowPerformers(q, bundle)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", err.Error())
		return
	}
	if len(performers) > 0 {
		serveWorkflowWithPerformers(w, r, bundle, performers, r.PathValue("id"))
		return
	}
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
// importing node). A reference with nothing stored behind it is left out;
// the file reports it as a problem rather than refusing the export.
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
		if depth > 0 && isPWFNotFound(err) {
			return nil
		}
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
			if !wellFormedPWFRef(sp) {
				// A half-typed reference in a draft names nothing the store
				// could hold; the file reports it as not carried.
				continue
			}
			if sp.Version != "" {
				if err := load(sp.TemplateRef+"@"+sp.Version, false, depth+1); err != nil {
					return err
				}
				continue
			}
			child, err := fs.GetTemplateHead(ctx, sp.TemplateRef)
			if isPWFNotFound(err) {
				continue
			}
			if err != nil {
				return fmt.Errorf("template %q references %q: %w", tmpl.ID, sp.TemplateRef, err)
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

// pwfVersionRE is a subprocess pin's version: sha256:<64 lowercase hex>.
var pwfVersionRE = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

func wellFormedPWFRef(sp model.Subprocess) bool {
	if store.ValidateTemplateID(sp.TemplateRef) != nil {
		return false
	}
	return sp.Version == "" || pwfVersionRE.MatchString(sp.Version)
}

func isPWFNotFound(err error) bool {
	return errors.Is(err, store.ErrNotFound) || errors.Is(err, os.ErrNotExist)
}

// pwfImportView is the review an importer acts on: what the workflow is,
// who signed it, what it needs, and what importing would change locally.
type pwfImportView struct {
	Title      string             `json:"title"`
	Summary    string             `json:"summary,omitempty"`
	Author     string             `json:"author,omitempty"`
	Licence    string             `json:"licence,omitempty"`
	Root       string             `json:"root"`
	RootID     string             `json:"rootId"`
	Readme     string             `json:"readme,omitempty"`
	Signatures []pwfSignatureView `json:"signatures"`
	Params     []pwf.Param        `json:"params"`
	Roles      []pwf.Use          `json:"roles"`
	Programs   []pwf.Program      `json:"programs"`
	Assignees  []pwf.Use          `json:"assignees"`
	Humans     []string           `json:"humans"`
	// Problems keep the workflow from running as carried (pwf
	// Archive.Problems): it imports all the same, and a run refuses to start
	// until they are fixed here.
	Problems []string `json:"problems"`
	// Paths are machine-specific paths the file carries (pwf Archive.Paths).
	Paths     []pwf.PathFinding     `json:"paths"`
	Templates []pwfTemplateImported `json:"templates"`
	// RoleMap lists every role name the agent steps use (the role they ask
	// for and the profile a fallback spawns) and what they use after the
	// import; LocalRoles are the names a role can be mapped to here.
	RoleMap    []pwfRoleMapping `json:"roleMap"`
	LocalRoles []string         `json:"localRoles"`
	// Envelope is set for a workflow read out of a .paf.
	Envelope *pwfEnvelopeView `json:"envelope,omitempty"`
	// Rename and Group echo the importer's choices; Pinned reports that an
	// applied import pinned the root to Group.
	Rename bool   `json:"rename,omitempty"`
	Group  string `json:"group,omitempty"`
	Pinned bool   `json:"pinned,omitempty"`
	// Refused says why this node's policy refuses to import the file; a review
	// still shows it, and an apply is refused.
	Refused string `json:"refused,omitempty"`
	// Updates counts templates whose local head the import would move; an
	// import with updates needs replace.
	Updates int  `json:"updates"`
	Applied bool `json:"applied"`
	// ReviewDigest binds an apply to this exact review: the file and what
	// importing it would change here. Apply must send it back as ?review=,
	// and is refused if anything moved in between (e.g. a local save).
	ReviewDigest string `json:"reviewDigest"`
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
	Ref string `json:"ref"`
	ID  string `json:"id"`
	// ImportRef and ImportID are the template as it is stored here when the
	// role mapping or a rename rewrote it; empty when stored as carried.
	ImportRef string `json:"importRef,omitempty"`
	ImportID  string `json:"importId,omitempty"`
	Name      string `json:"name,omitempty"`
	Head      bool   `json:"head"`
	Status    string `json:"status"`
	// CurrentRef is the local head when Status is update.
	CurrentRef string `json:"currentRef,omitempty"`
}

// POST /v1/process/import[?apply=1&review=<digest>[&replace=1]] with a .pwf
// body, plus the importer's choices on both the preview and the apply:
// map=<role>=<local> (repeated), rename=1 and group=<name> (see
// pwfImportOptions). Without apply it is a preview and changes nothing; an
// apply must carry the reviewDigest of the preview it acts on.
func handleProcessImport(w http.ResponseWriter, r *http.Request) {
	// A .paf carries whole conversations, so the body is spooled to disk under
	// the agent transfer limit; a .pwf keeps its own, smaller bound.
	r.Body = http.MaxBytesReader(w, r.Body, max(maxPWFUpload, agentTransferLimit()))
	r = withApprovalFacts(r, func(body []byte) string { return processImportApprovalFacts(r, body) })
	caller, ok := requirePermission(w, r, PermProcessTemplatesManage)
	if !ok {
		return
	}
	file, err := os.CreateTemp("", ".workflow-import-archive-")
	if err != nil {
		writeError(w, http.StatusInternalServerError, "process_import", err.Error())
		return
	}
	defer func() { _ = file.Close(); _ = os.Remove(file.Name()) }()
	hash := sha256.New()
	size, err := io.Copy(io.MultiWriter(file, hash), r.Body)
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, "too_large", fmt.Sprintf("a workflow file is at most %d bytes", maxPWFUpload))
		return
	}
	q := r.URL.Query()
	archive, envelope, envelopeKey, err := openWorkflowFile(file, size, q.Get("workflow"))
	if errors.Is(err, errPWFTooLarge) {
		writeError(w, http.StatusRequestEntityTooLarge, "too_large", err.Error())
		return
	}
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, "invalid_pwf", err.Error())
		return
	}
	opts, err := parsePWFImportOptions(q)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", err.Error())
		return
	}
	uses := pwfRoleUses(archive)
	for from := range opts.RoleMap {
		if uses[from] == nil {
			writeError(w, http.StatusBadRequest, "invalid_arg", fmt.Sprintf("the workflow asks for no role %q", from))
			return
		}
	}
	var group *db.AgentGroup
	if opts.Group != "" {
		if group, ok = resolveProcessGroupForPin(w, r, opts.Group); !ok {
			return
		}
		opts.Group = group.Name
	}
	fs, err := store.NewFS(processStoreRoot())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "process_store", err.Error())
		return
	}
	planned, _, err := pwfImportPlan(r.Context(), fs, archive, opts)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, "process_import", err.Error())
		return
	}
	local, err := pwfLocalRoleNames(r)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "process_import", err.Error())
		return
	}
	opts.Workflow = envelopeKey
	view, err := pwfImportPreview(r.Context(), fs, archive, hash.Sum(nil), planned, opts, uses, local)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "process_import", err.Error())
		return
	}
	if envelope != nil {
		view.Envelope = pwfEnvelopeSummary(envelope, envelopeKey, opts)
	}
	if group != nil {
		// Refuse a full group before anything is stored.
		if full, err := db.ProcessGroupPinsFull(group.ID, view.RootID); err != nil {
			writeError(w, http.StatusInternalServerError, "process_group_pin", err.Error())
			return
		} else if full {
			writeError(w, http.StatusConflict, "process_group_pin", db.ErrProcessGroupPinsFull.Error())
			return
		}
	}
	if q.Get("apply") != "1" {
		writeProcessJSON(w, http.StatusOK, view)
		return
	}
	if view.Refused != "" {
		writeError(w, http.StatusForbidden, "process_import_unsigned", view.Refused)
		return
	}
	if q.Get("review") != view.ReviewDigest {
		writeProcessJSON(w, http.StatusConflict, map[string]any{
			"error":   "process_import_review_stale",
			"message": "what this import would change is not what was reviewed (a template changed here since); review it again",
			"preview": view,
		})
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
	// Heads first: on a node without the id, a pinned older version stored
	// first would become the head and make the imported head look like a
	// conflict.
	rows := make([]*pwfTemplateImported, 0, len(view.Templates))
	for i := range view.Templates {
		if view.Templates[i].Head {
			rows = append(rows, &view.Templates[i])
		}
	}
	for i := range view.Templates {
		if !view.Templates[i].Head {
			rows = append(rows, &view.Templates[i])
		}
	}
	for _, row := range rows {
		tmpl := planned[row.Ref].Tmpl
		switch row.Status {
		case "identical":
			continue
		case "version":
			_, err = fs.PutTemplateVersion(r.Context(), tmpl)
		case "new", "update":
			expected := ""
			if row.Status == "update" {
				// Replace only the head that was reviewed.
				head, herr := fs.GetTemplateHeadGeneration(r.Context(), tmpl.ID)
				if herr != nil {
					err = herr
					break
				}
				if head.Ref != row.CurrentRef {
					err = fmt.Errorf("the local head moved to %s since the review", head.Ref)
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
	if group != nil {
		if err := db.PinProcessToGroup(group.ID, view.RootID); err != nil {
			writeProcessJSON(w, http.StatusConflict, map[string]any{
				"error":   "process_group_pin",
				"message": "the workflow was imported, but pinning it to the group failed: " + err.Error(),
				"preview": view,
			})
			return
		}
		view.Pinned = true
	}
	slog.Info("process workflow imported", "root", view.Root, "templates", len(view.Templates), "actor", actor, "group", view.Group)
	writeProcessJSON(w, http.StatusOK, view)
}

// processImportApprovalFacts is the review a popup shows for an import, the one
// the CLI prints: the file itself says nothing a person can judge. The handler
// checks everything again and refuses what this cannot read.
func processImportApprovalFacts(r *http.Request, data []byte) string {
	review, err := processImportReview(r, data)
	if err != nil {
		return "This workflow file cannot be reviewed: " + guideText(err.Error(), 300)
	}
	q := r.URL.Query()
	return processImportApprovalText(review, q.Get("apply") == "1", q.Get("replace") == "1")
}

// processImportApprovalText states what approving does for a workflow import.
func processImportApprovalText(review agent.ImportReview, apply, replace bool) string {
	lead := "Approving lets the agent read this review. Nothing is imported yet."
	switch {
	case apply && review.Refused != "":
		lead = "This node refuses this file, so approving imports nothing: " + review.Refused
	case apply:
		lead = "Approving imports this workflow. It stores templates and starts no run."
		if replace {
			lead += " Imported versions become current where this node already has the template."
		}
	case review.Refused != "":
		lead += " This node would refuse it: " + review.Refused
	}
	return approvalReviewText(lead, review)
}

func processImportReview(r *http.Request, data []byte) (agent.ImportReview, error) {
	q := r.URL.Query()
	archive, envelope, envelopeKey, err := openWorkflowFile(bytes.NewReader(data), int64(len(data)), q.Get("workflow"))
	if err != nil {
		return agent.ImportReview{}, err
	}
	opts, err := parsePWFImportOptions(q)
	if err != nil {
		return agent.ImportReview{}, err
	}
	fs, err := store.NewFS(processStoreRoot())
	if err != nil {
		return agent.ImportReview{}, err
	}
	planned, _, err := pwfImportPlan(r.Context(), fs, archive, opts)
	if err != nil {
		return agent.ImportReview{}, err
	}
	local, err := pwfLocalRoleNames(r)
	if err != nil {
		return agent.ImportReview{}, err
	}
	opts.Workflow = envelopeKey
	fileDigest := sha256.Sum256(data)
	view, err := pwfImportPreview(r.Context(), fs, archive, fileDigest[:], planned, opts, pwfRoleUses(archive), local)
	if err != nil {
		return agent.ImportReview{}, err
	}
	if envelope != nil {
		view.Envelope = pwfEnvelopeSummary(envelope, envelopeKey, opts)
	}
	preview, err := json.Marshal(view)
	if err != nil {
		return agent.ImportReview{}, err
	}
	return agent.WorkflowImportReview(preview)
}

// pwfImportPreview says what importing the planned templates changes here.
// RootID is the root's id as stored (renamed, when it was).
func pwfImportPreview(ctx context.Context, fs *store.FS, a *pwf.Archive, fileDigest []byte, planned map[string]*pwfPlanned,
	opts pwfImportOptions, uses map[string][]string, local []string) (pwfImportView, error) {
	wf := a.Workflow
	view := pwfImportView{
		Title: wf.Title, Summary: wf.Summary, Author: wf.Author, Licence: wf.Licence, Root: wf.Root, RootID: planned[wf.Root].ID, Readme: string(a.Readme),
		Params: wf.Params, Roles: wf.Roles, Programs: wf.Programs, Assignees: wf.Assignees, Humans: wf.Humans,
		Problems:   append([]string{}, a.Problems...),
		Paths:      append([]pwf.PathFinding{}, a.Paths...),
		Signatures: pwfSignatureViews(a.Signatures), RoleMap: []pwfRoleMapping{}, LocalRoles: local,
		Rename: opts.Rename, Group: opts.Group,
	}
	if pwfRefusedUnsigned(a) {
		view.Refused = "this node refuses workflows without a valid signature (processes.refuse_unsigned_imports)"
	}
	known := map[string]bool{}
	for _, name := range local {
		known[name] = true
	}
	for _, name := range sortedKeysOf(uses) {
		target, mapped := opts.RoleMap[name]
		if !mapped {
			target = name
		}
		view.RoleMap = append(view.RoleMap, pwfRoleMapping{Name: name, Steps: uses[name], Target: target, Mapped: mapped, Known: known[target]})
	}
	for _, fileRef := range a.Order {
		t := a.Templates[fileRef]
		p := planned[fileRef]
		ref := p.Ref
		row := pwfTemplateImported{Ref: fileRef, ID: t.Parsed.ID, Name: t.Parsed.Name, Head: t.Head}
		if ref != fileRef {
			row.ImportRef, row.ImportID = ref, p.ID
		}
		_, storedErr := fs.GetTemplate(ctx, ref)
		stored := storedErr == nil
		if storedErr != nil && !errors.Is(storedErr, store.ErrNotFound) && !errors.Is(storedErr, os.ErrNotExist) {
			return view, storedErr
		}
		head, headErr := fs.GetTemplateHead(ctx, p.ID)
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
	digest := sha256.New()
	digest.Write(fileDigest)
	for _, row := range view.Templates {
		fmt.Fprintf(digest, "\n%s %s %s %s", row.Ref, row.ImportRef, row.Status, row.CurrentRef)
	}
	fmt.Fprintf(digest, "\n%s", opts.digestKey())
	view.ReviewDigest = hex.EncodeToString(digest.Sum(nil))
	return view, nil
}

// pwfRefusedUnsigned applies the operator's policy. A signature that does not
// verify proves nothing, so it counts as none; a valid one by an unknown key
// passes, since the policy asks for a signature, not for a known signer. A
// config that cannot be read never gets here: processRoute takes the route down.
func pwfRefusedUnsigned(a *pwf.Archive) bool {
	cfg, err := config.Load()
	if err != nil || !cfg.RefuseUnsignedWorkflows() {
		return false
	}
	return !slices.ContainsFunc(a.Signatures, func(s paf.SignatureResult) bool { return s.Status == paf.SignatureValid })
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
