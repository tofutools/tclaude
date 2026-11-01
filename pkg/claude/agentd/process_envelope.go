package agentd

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/agentbundle"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/process/store"
	"github.com/tofutools/tclaude/pkg/common/buildversion"
	"github.com/tofutools/tclaude/pkg/federation/proto"
	"github.com/tofutools/tclaude/pkg/paf"
	"github.com/tofutools/tclaude/pkg/paf/legacy"
	"github.com/tofutools/tclaude/pkg/paf/privacy"
	"github.com/tofutools/tclaude/pkg/pwf"
)

// The PAF envelope on the daemon side (docs/paf.md, "The envelope"). An agent,
// a group or a snapshot can carry workflows, and a workflow can ship agents for
// its roles. Each side keeps its own gates: carrying a workflow needs what a
// workflow export needs, shipping an agent needs what an agent export needs,
// and either kind of content is scanned before it leaves. Importing never takes
// both at once. A workflow goes through the workflow review, an agent through
// the agent import, so neither review is weakened by the file holding the other.

// carriedWorkflow names what an export should carry.
type carriedWorkflows struct {
	IDs []string
	// Own adds the workflows the exported thing has: a group's saved
	// processes, or the templates of an agent's active runs.
	Own bool
}

func carriedWorkflowsFromQuery(q url.Values) carriedWorkflows {
	return carriedWorkflows{IDs: q["workflow"], Own: q.Get("workflows") == "own"}
}

func (c carriedWorkflows) any() bool { return c.Own || len(c.IDs) > 0 }

// loadCarriedWorkflows answers an export's request to carry workflows: the
// feature must be on and the caller allowed to export them, whatever it holds
// on the agent or group. own lists the exported thing's own template ids. It
// writes the error itself.
func loadCarriedWorkflows(w http.ResponseWriter, r *http.Request, c carriedWorkflows, own func() ([]string, error)) ([]pwf.Bundle, bool) {
	if !c.any() {
		return nil, true
	}
	if !processRoutesEnabled() {
		writeError(w, http.StatusBadRequest, processDisabledCode, "processes are off here, so an export cannot carry workflows")
		return nil, false
	}
	if _, ok := requirePermission(w, r, PermProcessTemplatesRead); !ok {
		return nil, false
	}
	ids := slices.Clone(c.IDs)
	explicit := len(ids)
	if c.Own {
		mine, err := own()
		if err != nil {
			writeError(w, http.StatusInternalServerError, "process_export", err.Error())
			return nil, false
		}
		ids = append(ids, mine...)
	}
	fs, err := store.NewFS(processStoreRoot())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "process_store", err.Error())
		return nil, false
	}
	seen := map[string]bool{}
	var bundles []pwf.Bundle
	for i, id := range ids {
		id = strings.TrimSpace(id)
		if seen[id] {
			continue
		}
		seen[id] = true
		bundle, err := collectPWFBundle(r, fs, id, "")
		switch {
		case isPWFNotFound(err) && i >= explicit:
			// A saved process whose template is gone has nothing to carry.
			slog.Warn("export: carried workflow not found", "template", id)
			continue
		case isPWFNotFound(err):
			writeError(w, http.StatusNotFound, "not_found", fmt.Sprintf("process template %q not found", id))
			return nil, false
		case err != nil:
			writeError(w, http.StatusUnprocessableEntity, "process_export", err.Error())
			return nil, false
		}
		bundles = append(bundles, bundle)
	}
	return bundles, true
}

// agentOwnWorkflows lists the templates of an agent's active runs.
func agentOwnWorkflows(convID string) func() ([]string, error) {
	return func() ([]string, error) {
		id, err := db.AgentIDForConv(convID)
		if err != nil {
			return nil, err
		}
		runs, err := db.ListScopedActiveProcessRuns(db.ProcessRunContextQuery{AgentID: id, PerScope: db.MaxProcessRunContextRuns})
		if err != nil {
			return nil, err
		}
		var ids []string
		for _, run := range runs {
			template, _, _ := strings.Cut(run.TemplateRef, "@")
			ids = append(ids, template)
		}
		return ids, nil
	}
}

// groupOwnWorkflows lists a group's saved processes.
func groupOwnWorkflows(groupID int64) func() ([]string, error) {
	return func() ([]string, error) { return db.ListProcessGroupPins(groupID) }
}

// scanWorkflowBundles scans what carrying the workflows would add to a file.
func scanWorkflowBundles(bundles []pwf.Bundle) []paf.Finding {
	c := privacy.NewCollector()
	for _, b := range bundles {
		for _, t := range b.Templates {
			c.Text(string(t.Source), "workflow template "+t.Ref)
		}
		if len(b.Readme) > 0 {
			c.Text(string(b.Readme), "workflow README")
		}
	}
	return c.Findings()
}

var envelopeKeyChars = regexp.MustCompile(`[^a-z0-9-]+`)

// envelopeWorkflowKey names a workflow inside the archive, after its root
// template. The key is short because a template's own path sits under it.
func envelopeWorkflowKey(root string, used map[string]bool) string {
	id, _, _ := strings.Cut(root, "@")
	base := strings.Trim(envelopeKeyChars.ReplaceAllString(id, "-"), "-")
	if len(base) > 28 {
		base = strings.TrimRight(base[:28], "-")
	}
	if base == "" {
		base = "workflow"
	}
	key := base
	for i := 2; used[key]; i++ {
		key = fmt.Sprintf("%s-%d", base, i)
	}
	used[key] = true
	return key
}

// writeCarriedWorkflows adds workflows to an agent or group file. A file of one
// agent links each workflow to it; a group holds its workflows itself.
func writeCarriedWorkflows(w *paf.Writer, bundles []pwf.Bundle, agents []string, linked bool) error {
	used := map[string]bool{}
	for _, b := range bundles {
		key := envelopeWorkflowKey(b.Root, used)
		if err := pwf.WriteEmbedded(w, key, b); err != nil {
			return err
		}
		if linked {
			w.AddLink(paf.Link{Type: paf.LinkCarries, Workflow: key, Agent: agents[0]})
		}
	}
	return nil
}

// workflowPerformer is one performer= of a workflow export: an agent shipped
// for a role the workflow asks for.
type workflowPerformer struct {
	Role   string
	ConvID string
}

// parseWorkflowPerformers reads performer=<role>=<agent> and checks each role
// is one the workflow asks for. An error is the caller's mistake.
func parseWorkflowPerformers(q url.Values, bundle pwf.Bundle) ([]workflowPerformer, error) {
	if len(q["performer"]) == 0 {
		return nil, nil
	}
	roles, err := pwf.Roles(bundle)
	if err != nil {
		return nil, err
	}
	var out []workflowPerformer
	for _, spec := range q["performer"] {
		role, selector, ok := strings.Cut(spec, "=")
		role, selector = strings.TrimSpace(role), strings.TrimSpace(selector)
		if !ok || role == "" || selector == "" {
			return nil, fmt.Errorf("performer %q is not <role>=<agent>", spec)
		}
		if !slices.ContainsFunc(roles, func(u pwf.Use) bool { return u.Name == role }) {
			return nil, fmt.Errorf("the workflow asks for no role %q", role)
		}
		if slices.ContainsFunc(out, func(p workflowPerformer) bool { return p.Role == role }) {
			return nil, fmt.Errorf("role %q is given two performers", role)
		}
		res, _, err := agent.ResolveSelector(selector)
		if err != nil {
			return nil, err
		}
		out = append(out, workflowPerformer{Role: role, ConvID: res.ConvID})
	}
	return out, nil
}

// serveWorkflowWithPerformers writes the workflow and its performers as a bundle
// PAF. Each performer is exported the way `paf export` exports it, under the
// same permission and credential scan, and the conversation travels only when
// asked. It writes the response itself.
func serveWorkflowWithPerformers(w http.ResponseWriter, r *http.Request, bundle pwf.Bundle, performers []workflowPerformer, id string) {
	q := r.URL.Query()
	allowFlagged := q.Get("allow_flagged") == "true"
	withHistory := q.Get("performer_history") == "1"
	byConv := map[string]*agentbundle.Bundle{}
	var agents []legacy.BundleAgent
	index := map[string]int{}
	defer func() {
		for _, b := range byConv {
			_ = b.Close()
		}
	}()
	findings := scanWorkflowBundles([]pwf.Bundle{bundle})
	for _, p := range performers {
		if _, ok := requireCrossAgentPermission(w, r, PermAgentBundleExport, p.ConvID); !ok {
			return
		}
		if byConv[p.ConvID] != nil {
			continue
		}
		b, err := collectAgentBundle(p.ConvID, withHistory)
		if err != nil {
			writeError(w, http.StatusBadRequest, "bundle_export", err.Error())
			return
		}
		byConv[p.ConvID] = b
		if caller, human, ok := authedCaller(w, r); ok && b.Manifest.Agent.Origin != nil {
			b.Manifest.Agent.Origin.Trigger = arrivalTrigger(caller, p.ConvID, human)
		}
		for _, f := range b.Manifest.Findings {
			findings = append(findings, paf.Finding{Kind: f.Kind, Count: f.Count, Locations: f.Locations})
		}
		item := legacy.WriteOptions{MaxRecordBytes: pafContextRecordLimit(), Context: neutralContext(b)}
		if b.Manifest.History != nil {
			item.EarlierLosses = storedConvLosses(b.Manifest.History.SourceConvID)
		}
		index[p.ConvID] = len(agents)
		agents = append(agents, legacy.BundleAgent{Bundle: b, Options: item})
	}
	if privacy.Blocking(findings) && !allowFlagged {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"error": "suspected credentials: use --allow-flagged or export without --performer-history", "code": "flagged_credentials", "findings": findings})
		return
	}
	opts := legacy.WriteOptions{
		Producer:        paf.Producer{Name: "tclaude", Version: buildversion.AppVersion()},
		MaxRecordBytes:  pafContextRecordLimit(),
		AllowedFindings: classify(peerFromContext(r.Context())) == classHuman && privacy.Blocking(findings),
		AddContent: func(pw *paf.Writer, keys []string) error {
			key := envelopeWorkflowKey(bundle.Root, map[string]bool{})
			if err := pwf.WriteEmbedded(pw, key, bundle); err != nil {
				return err
			}
			for _, p := range performers {
				pw.AddLink(paf.Link{Type: paf.LinkPerformer, Workflow: key, Agent: keys[index[p.ConvID]], Role: p.Role})
			}
			return nil
		},
	}
	if identity, err := proto.LoadIdentity(FederationKeyPath()); err == nil {
		opts.Keys = []paf.SigningKey{{Key: identity.Priv, KeyID: identity.ID(), Signer: &paf.Signer{Kind: "instance"}}}
	}
	file, err := os.CreateTemp("", ".workflow-export-archive-")
	if err != nil {
		writeError(w, http.StatusInternalServerError, "process_export", err.Error())
		return
	}
	defer func() { _ = file.Close(); _ = os.Remove(file.Name()) }()
	if err := legacy.WriteBundlePAF(agents, file, opts, agentTransferLimit()); err != nil {
		writeError(w, http.StatusUnprocessableEntity, "process_export", err.Error())
		return
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		writeError(w, http.StatusInternalServerError, "process_export", err.Error())
		return
	}
	w.Header().Set("Content-Type", paf.MediaType)
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s.paf"`, id))
	_, _ = io.Copy(w, file)
}

// pwfEnvelopeView is what a workflow review adds when the workflow came out of
// a .paf: which of the file's workflows this is, and the agents it ships.
type pwfEnvelopeView struct {
	Workflow   string             `json:"workflow"`
	Workflows  []pwfEnvelopeItem  `json:"workflows"`
	Performers []pwfPerformerView `json:"performers"`
	CarriedBy  []string           `json:"carriedBy,omitempty"`
}

type pwfEnvelopeItem struct {
	Key  string `json:"key"`
	Root string `json:"root"`
}

// pwfPerformerView is an agent the workflow ships for a role. Nothing in it
// lands by importing the workflow: AgentKey is what an agent import of the same
// file names to land it, under that import's own review.
type pwfPerformerView struct {
	Role string `json:"role"`
	// Target is the role after the importer's mapping.
	Target   string `json:"target"`
	AgentKey string `json:"agentKey"`
	Name     string `json:"name"`
	Harness  string `json:"harness"`
	Model    string `json:"model,omitempty"`
	History  bool   `json:"history"`
}

func pwfEnvelopeSummary(env *paf.Archive, key string, opts pwfImportOptions) *pwfEnvelopeView {
	view := &pwfEnvelopeView{Workflow: key, Workflows: []pwfEnvelopeItem{}, Performers: []pwfPerformerView{}}
	for _, wf := range env.Manifest.Workflows {
		view.Workflows = append(view.Workflows, pwfEnvelopeItem{Key: wf.Key, Root: wf.Root})
	}
	for _, l := range env.Performers(key) {
		ag, _ := env.Agent(l.Agent)
		e, _ := env.Environment(l.Agent)
		row := pwfPerformerView{Role: l.Role, Target: l.Role, AgentKey: l.Agent, Name: ag.Name, Harness: e.Harness.Name,
			History: ag.Native != nil || env.HasContext(l.Agent, paf.ScopeEffective)}
		if to, ok := opts.RoleMap[l.Role]; ok {
			row.Target = to
		}
		if e.Model != nil {
			row.Model = e.Model.ID
		}
		view.Performers = append(view.Performers, row)
	}
	for _, l := range env.Manifest.Links {
		if l.Type == paf.LinkCarries && l.Workflow == key {
			view.CarriedBy = append(view.CarriedBy, l.Agent)
		}
	}
	return view
}

// openWorkflowFile reads an upload as a .pwf or, when it is a PAF, as the
// workflow key names in its envelope (the first one when key is empty). The
// second result is the envelope, nil for a .pwf; a caller keeps file open while
// it uses it.
func openWorkflowFile(file io.ReaderAt, size int64, key string) (*pwf.Archive, *paf.Archive, string, error) {
	if !legacy.IsPAF(file, size) {
		if key != "" {
			return nil, nil, "", errors.New("workflow= names a workflow inside a .paf; this file holds one workflow")
		}
		if size > maxPWFUpload {
			return nil, nil, "", errPWFTooLarge
		}
		data, err := io.ReadAll(io.NewSectionReader(file, 0, size))
		if err != nil {
			return nil, nil, "", err
		}
		a, err := pwf.Open(bytes.NewReader(data), size, pwf.DefaultLimits())
		return a, nil, "", err
	}
	env, err := paf.Open(file, size, paf.Options{Purpose: paf.PurposeImport, Limits: paf.Limits{MaxArchiveBytes: legacy.ConvertedLimit(agentTransferLimit()), MaxRecordBytes: pafContextRecordLimit()}})
	if err != nil {
		return nil, nil, "", err
	}
	a, chosen, err := pwf.OpenEnvelope(env, key)
	return a, env, chosen, err
}

var errPWFTooLarge = fmt.Errorf("a workflow file is at most %d bytes", maxPWFUpload)
