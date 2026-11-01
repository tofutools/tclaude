// Package pwf reads and writes the Portable Workflow Format: a shareable,
// verifiable file that carries a process template and every stored template
// it references through subprocess steps. It is PAF's sibling (docs/pwf.md):
// the same ZIP container rules, entry hashing and Ed25519 signatures, under
// its own manifest and its own signature domain.
package pwf

import (
	"archive/zip"
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/process/model"
	"github.com/tofutools/tclaude/pkg/paf"
)

const (
	Format = "pwf"
	// Version is the PWF version this package writes and reads.
	Version      = "0.1"
	KindWorkflow = "workflow"
	// MediaType is the file's media type; the extension is .pwf.
	MediaType = "application/vnd.tclaude.pwf+zip"
	// TemplateMediaType marks a template source entry (YAML or JSON).
	TemplateMediaType = "application/vnd.tclaude.process-template"

	ManifestPath  = "manifest.json"
	WorkflowPath  = "workflow.json"
	ReadmePath    = "README.md"
	TemplatesDir  = "templates/"
	SignaturesDir = "signatures/"

	// signatureContext separates PWF signatures from PAF's, so a signature
	// made for one format never verifies a file of the other.
	signatureContext = "pwf-manifest-signature-v1\n"

	maxTitle   = 200
	maxSummary = 2000
	// maxCredit bounds Author and Licence.
	maxCredit = 200
)

var (
	templateIDRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,127}$`)
	hashRE       = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// FeatureLoops marks a workflow with bounded loops (Workflow.Loops). A reader
// that predates loops cannot show them in its review, so the marker makes it
// refuse the file by name instead. Files written before the marker existed
// carry loops without it and still open.
const FeatureLoops = "loops"

// knownFeatures are the requires values this reader understands.
var knownFeatures = map[string]bool{FeatureLoops: true}

// Manifest is manifest.json: the format header and every entry's hash.
type Manifest struct {
	Format     string       `json:"format"`
	PwfVersion string       `json:"pwf_version"`
	Kind       string       `json:"kind"`
	CreatedAt  string       `json:"created_at"`
	Producer   paf.Producer `json:"producer"`
	Requires   []string     `json:"requires"`
	// Root is the ref (id@sha256:<hex>) of the workflow's entry template.
	Root    string      `json:"root"`
	Entries []paf.Entry `json:"entries"`
}

// Workflow is workflow.json: what the workflow is and what it needs, derived
// from the templates so a reader can review it without walking the graph.
// A reader recomputes everything but the exporter's text (Title, Summary,
// Author and Licence) and refuses a file
// whose workflow.json disagrees with its templates.
type Workflow struct {
	Title   string `json:"title"`
	Summary string `json:"summary,omitempty"`
	// Author and Licence are what the exporter says about the workflow. Only a
	// signature ties a file to anyone, so neither is proof of who wrote it.
	Author  string `json:"author,omitempty"`
	Licence string `json:"licence,omitempty"`
	Root    string `json:"root"`
	// Templates lists every template in the file, root first. Head marks the
	// version an unpinned subprocess reference to that id resolves to.
	Templates []TemplateEntry `json:"templates"`
	Params    []Param         `json:"params"`
	// Roles are the agent roles steps ask for (Performer.Profile or
	// Performer.Role on agent performers); an importer maps each to a local
	// role.
	Roles []Use `json:"roles"`
	// Programs are every command a step would run. Nothing runs on import;
	// starting a run asks the operator to authorize each program profile.
	Programs []Program `json:"programs"`
	// Assignees are steps that name one specific agent. They are not
	// portable: the importing node's agents differ.
	Assignees []Use `json:"assignees"`
	// Humans are steps a person performs (questions and sign-offs).
	Humans []string `json:"humans"`
	// Loops are the decision outcomes that send work back for another round,
	// with their round limits (tcl-eu70x9). Omitted when there are none, so a
	// workflow without loops reads exactly as before.
	Loops []Loop `json:"loops,omitempty"`
}

// Loop is one bounded back edge: Step's Outcome goes back to To for at most
// MaxIterations rounds, then follows OnExhausted (or is refused without one).
type Loop struct {
	Step          string `json:"step"`
	Outcome       string `json:"outcome"`
	To            string `json:"to"`
	MaxIterations int    `json:"maxIterations"`
	OnExhausted   string `json:"onExhausted,omitempty"`
}

type TemplateEntry struct {
	Ref  string `json:"ref"`
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`
	Head bool   `json:"head"`
}

type Param struct {
	Name        string `json:"name"`
	Type        string `json:"type"`
	Required    bool   `json:"required"`
	Description string `json:"description,omitempty"`
}

// Use is one name (a role, an assignee) and the steps that use it.
type Use struct {
	Name  string   `json:"name"`
	Steps []string `json:"steps"`
}

type Program struct {
	Step    string   `json:"step"`
	Profile string   `json:"profile"`
	Run     string   `json:"run"`
	Args    []string `json:"args,omitempty"`
}

// Template is one template carried in a file.
type Template struct {
	Ref    string
	Head   bool
	Source []byte
	Parsed *model.Template
	// Problems are the template's validation errors. A template that fails
	// validation is still carried, the way the store keeps an unfinished
	// draft; it just cannot run until someone fixes it.
	Problems []string
}

// Bundle is what Write packs: the root and every template it references.
type Bundle struct {
	Root      string
	Title     string
	Summary   string
	Author    string
	Licence   string
	Readme    []byte
	Templates []Template
}

type WriteOptions struct {
	Producer  paf.Producer
	CreatedAt time.Time
	Keys      []paf.SigningKey
	Limits    paf.Limits
}

// DefaultLimits bound what a PWF reader accepts. A workflow is a handful of
// small text files, so these are far below PAF's (which carries whole
// conversations): a small, highly compressed file can never make a reader
// allocate more than MaxArchiveBytes.
func DefaultLimits() paf.Limits {
	return paf.Limits{MaxArchiveBytes: 32 << 20, MaxMetadataBytes: 1 << 20, MaxRecordBytes: 1 << 20, MaxEntries: 1000}
}

// Archive is an opened and fully verified PWF file.
type Archive struct {
	Manifest    Manifest
	ManifestRaw []byte
	Workflow    Workflow
	Readme      []byte
	// Templates by ref; Order is the workflow.json order (root first).
	Templates  map[string]*Template
	Order      []string
	Signatures []paf.SignatureResult
	// Problems is everything that keeps the workflow from running as
	// carried: each template's validation errors and every reference the
	// file does not carry. Readers recompute it rather than trusting the
	// file, so it is not part of workflow.json.
	Problems []string
	// Paths are the machine-specific paths the file carries (see pathscan.go),
	// recomputed the same way.
	Paths []PathFinding
}

// Root returns the root template.
func (a *Archive) Root() *Template { return a.Templates[a.Manifest.Root] }

// Head returns the template an unpinned reference to id resolves to.
func (a *Archive) Head(id string) *Template {
	for _, ref := range a.Order {
		if t := a.Templates[ref]; t.Head && t.Parsed.ID == id {
			return t
		}
	}
	return nil
}

// ParseTemplate parses one template source and returns it with its ref.
// It accepts what the template store accepts as a draft: validation errors
// are advisory (see TemplateProblems), but a source the store could not keep
// as written (an unknown field, a duplicate or merge key) or a graph past the
// size budget is refused.
func ParseTemplate(source []byte) (*model.Template, string, error) {
	tmpl, ref, _, err := parseTemplate(source)
	return tmpl, ref, err
}

func parseTemplate(source []byte) (*model.Template, string, []string, error) {
	parsed, err := model.Parse(source)
	if err != nil {
		return nil, "", nil, err
	}
	if parsed.Template == nil {
		return nil, "", nil, errors.New("not a process template")
	}
	if !templateIDRE.MatchString(parsed.Template.ID) {
		return nil, "", nil, fmt.Errorf("template id %q is not portable", parsed.Template.ID)
	}
	blocking := model.PreflightNormalizedGraphCardinality(parsed.Template).Errors()
	if parsed.Diagnostics.HasSourceFidelityError() || parsed.Diagnostics.HasNormalizedGraphBudgetError() {
		blocking = append(blocking, parsed.Diagnostics.Errors()...)
	}
	if len(blocking) > 0 {
		return nil, "", nil, fmt.Errorf("template %q cannot be carried: %s", parsed.Template.ID, problemText(blocking[0]))
	}
	var problems []string
	for _, d := range parsed.Diagnostics.Errors() {
		problems = append(problems, problemText(d))
	}
	return parsed.Template, parsed.Ref, problems, nil
}

func problemText(d model.Diagnostic) string {
	if d.Path != "" {
		return d.Path + ": " + d.Message
	}
	return d.Message
}

// templatePath is a template's entry name: templates/<id>/<semantic hash>.
func templatePath(ref string) (string, error) {
	id, hash, ok := strings.Cut(ref, "@sha256:")
	if !ok || !templateIDRE.MatchString(id) || !hashRE.MatchString(hash) {
		return "", fmt.Errorf("invalid template ref %q", ref)
	}
	return TemplatesDir + id + "/" + hash, nil
}

// SubprocessRefs lists the stored-template references a template makes:
// every subprocess step naming a templateRef, inline bodies included.
func SubprocessRefs(tmpl *model.Template) []model.Subprocess {
	var refs []model.Subprocess
	model.WalkNodes(tmpl, func(_ string, node model.Node) {
		if node.Type == model.NodeTypeSubprocess && node.Subprocess != nil && node.Subprocess.TemplateRef != "" {
			refs = append(refs, *node.Subprocess)
		}
	})
	return refs
}

// resolveIn finds the template a reference resolves to among templates: the
// pinned version, or the id's head when unpinned.
func resolveIn(sp model.Subprocess, byRef map[string]*Template, order []string) *Template {
	if sp.Version != "" {
		return byRef[sp.TemplateRef+"@"+sp.Version]
	}
	for _, ref := range order {
		if t := byRef[ref]; t.Head && t.Parsed.ID == sp.TemplateRef {
			return t
		}
	}
	return nil
}

// checkClosure verifies the template set: refs match content, at most one
// head per id, and the root is present. A reference that does not resolve
// inside the file is not an error, since an unfinished draft may point at a
// template nobody has written yet; it is returned as a problem.
func checkClosure(root string, byRef map[string]*Template, order []string) ([]string, error) {
	if byRef[root] == nil {
		return nil, fmt.Errorf("root template %q is not in the file", root)
	}
	heads := map[string]bool{}
	for _, ref := range order {
		t := byRef[ref]
		if t.Head {
			if heads[t.Parsed.ID] {
				return nil, fmt.Errorf("template %q has more than one head", t.Parsed.ID)
			}
			heads[t.Parsed.ID] = true
		}
	}
	var problems []string
	for _, ref := range order {
		t := byRef[ref]
		for _, p := range t.Problems {
			problems = append(problems, fmt.Sprintf("template %q: %s", t.Parsed.ID, p))
		}
		for _, sp := range SubprocessRefs(t.Parsed) {
			if resolveIn(sp, byRef, order) == nil {
				want := sp.TemplateRef
				if sp.Version != "" {
					want += "@" + sp.Version
				}
				problems = append(problems, fmt.Sprintf("template %q references %q, which the file does not carry", t.Parsed.ID, want))
			}
		}
	}
	return problems, nil
}

// Summarize derives workflow.json's computed fields from the templates.
func Summarize(root string, byRef map[string]*Template, order []string) Workflow {
	return summarize(root, byRef, order, true)
}

// summarize leaves role selectors out of Roles when roleSelectors is false,
// which is how files written before they were listed derive.
func summarize(root string, byRef map[string]*Template, order []string, roleSelectors bool) Workflow {
	w := Workflow{Root: root, Params: []Param{}, Roles: []Use{}, Programs: []Program{}, Assignees: []Use{}, Humans: []string{}}
	for _, ref := range order {
		t := byRef[ref]
		w.Templates = append(w.Templates, TemplateEntry{Ref: ref, ID: t.Parsed.ID, Name: t.Parsed.Name, Head: t.Head})
	}
	rootT := byRef[root].Parsed
	for _, name := range sortedKeys(rootT.Params) {
		p := rootT.Params[name]
		w.Params = append(w.Params, Param{Name: name, Type: p.Type, Required: p.Required != nil && *p.Required, Description: p.Description})
	}
	roles := map[string][]string{}
	assignees := map[string][]string{}
	for _, ref := range order {
		t := byRef[ref].Parsed
		model.WalkNodes(t, func(id string, node model.Node) {
			step := t.ID + ":" + id
			visit := func(stage string, p *model.Performer) {
				if p == nil {
					return
				}
				where := step + stage
				switch p.Kind {
				case model.PerformerAgent:
					if p.Profile != "" {
						roles[p.Profile] = append(roles[p.Profile], where)
					}
					if roleSelectors && p.Role != "" && p.Role != p.Profile {
						roles[p.Role] = append(roles[p.Role], where)
					}
					if p.Assignee != "" {
						assignees[p.Assignee] = append(assignees[p.Assignee], where)
					}
				case model.PerformerProgram:
					w.Programs = append(w.Programs, Program{Step: where, Profile: p.Profile, Run: p.Run, Args: p.Args})
				case model.PerformerHuman:
					w.Humans = append(w.Humans, where)
				}
			}
			visit("", node.Performer)
			if node.Plan != nil {
				visit("/plan", &node.Plan.Performer)
			}
			for i := range node.Checks {
				visit(fmt.Sprintf("/check%d", i+1), &node.Checks[i].Performer)
			}
			if node.Review != nil {
				visit("/review", &node.Review.Performer)
			}
			// A target in a subprocess body is namespaced like the step.
			scope := t.ID + ":" + id[:strings.LastIndex(id, model.SubprocessNodeIDSeparator)+1]
			for _, outcome := range sortedKeys(node.Loops) {
				bound := node.Loops[outcome]
				w.Loops = append(w.Loops, Loop{Step: step, Outcome: outcome, To: scope + node.Next[outcome],
					MaxIterations: bound.MaxIterations, OnExhausted: bound.OnExhausted})
			}
		})
	}
	w.Roles = uses(roles)
	w.Assignees = uses(assignees)
	return w
}

func uses(m map[string][]string) []Use {
	out := []Use{}
	for _, name := range sortedKeys(m) {
		out = append(out, Use{Name: name, Steps: m[name]})
	}
	return out
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// prepare parses a bundle's templates in file order (root first) and checks
// the set, returning what keeps the workflow from running as carried.
func prepare(b Bundle) (map[string]*Template, []string, []string, error) {
	byRef := map[string]*Template{}
	var order []string
	for i := range b.Templates {
		t := b.Templates[i]
		parsed, ref, problems, err := parseTemplate(t.Source)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("pwf: %w", err)
		}
		if t.Ref != "" && t.Ref != ref {
			return nil, nil, nil, fmt.Errorf("pwf: template source hashes to %s, not %s", ref, t.Ref)
		}
		if byRef[ref] != nil {
			return nil, nil, nil, fmt.Errorf("pwf: template %s given twice", ref)
		}
		t.Ref, t.Parsed, t.Problems = ref, parsed, problems
		byRef[ref] = &t
		if ref == b.Root {
			order = append([]string{ref}, order...)
		} else {
			order = append(order, ref)
		}
	}
	problems, err := checkClosure(b.Root, byRef, order)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("pwf: %w", err)
	}
	return byRef, order, problems, nil
}

// Problems says what would keep a bundle's workflow from running as
// carried, without writing it: the same list a reader of the written file
// gets as Archive.Problems. A bundle with problems can still be written.
func Problems(b Bundle) ([]string, error) {
	_, _, problems, err := prepare(b)
	return problems, err
}

// Assignees lists the steps of a bundle that name one specific agent.
func Assignees(b Bundle) ([]Use, error) {
	byRef, order, _, err := prepare(b)
	if err != nil {
		return nil, err
	}
	return Summarize(b.Root, byRef, order).Assignees, nil
}

// content is what a bundle writes, with paths relative to the workflow, in
// either container.
type content struct {
	workflow Workflow
	files    []contentFile
}

type contentFile struct {
	name, mediaType string
	data            []byte
}

func bundleContent(b Bundle) (*content, error) {
	byRef, order, _, err := prepare(b)
	if err != nil {
		return nil, err
	}
	wf := Summarize(b.Root, byRef, order)
	wf.Title, wf.Summary = workflowText(b, byRef[b.Root].Parsed)
	wf.Author, wf.Licence = strings.TrimSpace(b.Author), strings.TrimSpace(b.Licence)
	if err := wf.checkText(); err != nil {
		return nil, err
	}
	workflow, err := marshal(wf)
	if err != nil {
		return nil, err
	}
	c := &content{workflow: wf, files: []contentFile{{WorkflowPath, "application/json", workflow}}}
	if len(b.Readme) > 0 {
		c.files = append(c.files, contentFile{ReadmePath, "text/markdown", b.Readme})
	}
	for _, ref := range order {
		name, err := templatePath(ref)
		if err != nil {
			return nil, fmt.Errorf("pwf: %w", err)
		}
		c.files = append(c.files, contentFile{name, TemplateMediaType, byRef[ref].Source})
	}
	return c, nil
}

// Roles lists the agent roles a bundle's steps ask for.
func Roles(b Bundle) ([]Use, error) {
	byRef, order, _, err := prepare(b)
	if err != nil {
		return nil, err
	}
	return Summarize(b.Root, byRef, order).Roles, nil
}

// Write packs a bundle into a PWF file. The bundle should be complete (the
// root and every template its subprocess steps reference), but templates
// with validation errors and references it does not carry are written as
// they are and reported to readers as Archive.Problems.
func Write(out io.Writer, b Bundle, opts WriteOptions) error {
	c, err := bundleContent(b)
	if err != nil {
		return err
	}

	created := opts.CreatedAt
	if created.IsZero() {
		created = time.Now()
	}
	limits := opts.Limits
	if limits == (paf.Limits{}) {
		limits = DefaultLimits()
	}
	zw := zip.NewWriter(out)
	man := Manifest{Format: Format, PwfVersion: Version, Kind: KindWorkflow, CreatedAt: created.UTC().Truncate(time.Second).Format(time.RFC3339),
		Producer: opts.Producer, Requires: requiredFeatures(c.workflow), Root: b.Root}
	add := func(name, mediaType string, data []byte) error {
		if int64(len(data)) > limits.MaxMetadataBytes {
			return fmt.Errorf("pwf: entry %q exceeds %d bytes", name, limits.MaxMetadataBytes)
		}
		w, err := zw.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Deflate, Modified: created.UTC()})
		if err != nil {
			return err
		}
		if _, err := w.Write(data); err != nil {
			return err
		}
		sum := sha256.Sum256(data)
		man.Entries = append(man.Entries, paf.Entry{Path: name, Bytes: int64(len(data)), SHA256: hex.EncodeToString(sum[:]), MediaType: mediaType})
		return nil
	}
	for _, f := range c.files {
		if err := add(f.name, f.mediaType, f.data); err != nil {
			return err
		}
	}
	manifest, err := marshal(man)
	if err != nil {
		return err
	}
	w, err := zw.CreateHeader(&zip.FileHeader{Name: ManifestPath, Method: zip.Deflate, Modified: created.UTC()})
	if err != nil {
		return err
	}
	if _, err := w.Write(manifest); err != nil {
		return err
	}
	for _, key := range opts.Keys {
		sig, err := paf.SignContext(signatureContext, manifest, key)
		if err != nil {
			return fmt.Errorf("pwf: sign: %w", err)
		}
		raw, err := marshal(sig)
		if err != nil {
			return err
		}
		w, err := zw.CreateHeader(&zip.FileHeader{Name: SignaturesDir + key.KeyID + ".json", Method: zip.Deflate, Modified: created.UTC()})
		if err != nil {
			return err
		}
		if _, err := w.Write(raw); err != nil {
			return err
		}
	}
	return zw.Close()
}

// workflowText is the title and summary a bundle is written with: the
// exporter's, or the root template's when it gave none.
func workflowText(b Bundle, root *model.Template) (string, string) {
	title, summary := strings.TrimSpace(b.Title), strings.TrimSpace(b.Summary)
	if title == "" {
		title = root.Name
	}
	if title == "" {
		title = root.ID
	}
	if summary == "" {
		summary = root.Description
	}
	return title, summary
}

// requiredFeatures is the manifest's requires list for a workflow. It is never
// nil: requires is a required field of the manifest.
func requiredFeatures(w Workflow) []string {
	features := []string{}
	if len(w.Loops) > 0 {
		features = append(features, FeatureLoops)
	}
	return features
}

func unknownFeatures(requires []string) []string {
	var unknown []string
	for _, feature := range requires {
		if !knownFeatures[feature] {
			unknown = append(unknown, feature)
		}
	}
	return unknown
}

// Open reads and fully verifies a PWF file: container rules, manifest,
// entry sizes and hashes, every template's validity and ref, the closure,
// workflow.json against the templates, and signatures. Signature trust is
// the caller's decision: an unsigned file is valid but unverified.
func Open(r io.ReaderAt, size int64, limits paf.Limits) (*Archive, error) {
	if limits == (paf.Limits{}) {
		limits = DefaultLimits()
	}
	if size > limits.MaxArchiveBytes {
		return nil, fmt.Errorf("pwf: file exceeds %d bytes", limits.MaxArchiveBytes)
	}
	zr, err := zip.NewReader(r, size)
	if err != nil {
		return nil, fmt.Errorf("pwf: invalid ZIP: %w", err)
	}
	files, err := paf.IndexZip(r, size, zr.File, limits)
	if err != nil {
		return nil, errors.New("pwf: " + strings.TrimPrefix(err.Error(), "paf: "))
	}
	read := func(name string) ([]byte, error) {
		f := files[name]
		if f == nil {
			return nil, fmt.Errorf("pwf: %s is missing", name)
		}
		b, err := paf.ReadBounded(f, limits.MaxMetadataBytes)
		if err != nil {
			return nil, errors.New("pwf: " + strings.TrimPrefix(err.Error(), "paf: "))
		}
		return b, nil
	}
	a := &Archive{Templates: map[string]*Template{}}
	if a.ManifestRaw, err = read(ManifestPath); err != nil {
		return nil, err
	}
	if err := paf.DecodeStrict(a.ManifestRaw, &a.Manifest); err != nil {
		return nil, fmt.Errorf("pwf: manifest.json: %w", err)
	}
	m := &a.Manifest
	if m.Format != Format {
		return nil, errors.New("pwf: not a PWF file")
	}
	if m.PwfVersion != Version {
		return nil, fmt.Errorf("pwf: unsupported PWF version %q (this reader supports %s)", m.PwfVersion, Version)
	}
	if m.Kind != KindWorkflow {
		return nil, fmt.Errorf("pwf: unsupported kind %q", m.Kind)
	}
	if unknown := unknownFeatures(m.Requires); len(unknown) > 0 {
		return nil, fmt.Errorf("pwf: file requires features this reader does not support: %s", strings.Join(unknown, ", "))
	}
	listed := map[string]paf.Entry{}
	for _, e := range m.Entries {
		if e.Path == ManifestPath || strings.HasPrefix(e.Path, SignaturesDir) {
			return nil, fmt.Errorf("pwf: manifest lists reserved entry %q", e.Path)
		}
		if _, dup := listed[e.Path]; dup {
			return nil, fmt.Errorf("pwf: manifest lists %q twice", e.Path)
		}
		listed[e.Path] = e
	}
	for name := range files {
		if name == ManifestPath || strings.HasPrefix(name, SignaturesDir) {
			continue
		}
		if _, ok := listed[name]; !ok {
			return nil, fmt.Errorf("pwf: entry %q is not listed in the manifest", name)
		}
	}
	contents := map[string][]byte{}
	for _, p := range sortedKeys(listed) {
		e := listed[p]
		f := files[p]
		if f == nil {
			return nil, fmt.Errorf("pwf: listed entry %q is missing", p)
		}
		if f.UncompressedSize64 != uint64(e.Bytes) {
			return nil, fmt.Errorf("pwf: entry %q size does not match the manifest", p)
		}
		data, err := read(p)
		if err != nil {
			return nil, err
		}
		sum := sha256.Sum256(data)
		if hex.EncodeToString(sum[:]) != e.SHA256 {
			return nil, fmt.Errorf("pwf: entry %q hash does not match the manifest", p)
		}
		contents[p] = data
	}
	if err := a.loadContents(contents); err != nil {
		return nil, err
	}
	if err := a.readSignatures(files, read); err != nil {
		return nil, err
	}
	return a, nil
}

// loadContents checks a workflow's files, whichever container held them,
// against a.Manifest and fills in everything but the signatures.
func (a *Archive) loadContents(contents map[string][]byte) error {
	for p := range contents {
		if p != WorkflowPath && p != ReadmePath && !strings.HasPrefix(p, TemplatesDir) {
			return fmt.Errorf("pwf: unknown entry %q", p)
		}
	}
	m := &a.Manifest
	raw, ok := contents[WorkflowPath]
	if !ok {
		return errors.New("pwf: workflow.json is missing")
	}
	var err error
	if err = paf.DecodeStrict(raw, &a.Workflow); err != nil {
		return fmt.Errorf("pwf: workflow.json: %w", err)
	}
	if err := a.Workflow.checkText(); err != nil {
		return err
	}
	a.Readme = contents[ReadmePath]
	carried := map[string]bool{}
	for _, te := range a.Workflow.Templates {
		name, err := templatePath(te.Ref)
		if err != nil {
			return fmt.Errorf("pwf: workflow.json: %w", err)
		}
		source, ok := contents[name]
		if !ok {
			return fmt.Errorf("pwf: template %s is not in the file", te.Ref)
		}
		if a.Templates[te.Ref] != nil {
			return fmt.Errorf("pwf: template %s listed twice", te.Ref)
		}
		parsed, ref, problems, err := parseTemplate(source)
		if err != nil {
			return fmt.Errorf("pwf: %s: %w", name, err)
		}
		if ref != te.Ref {
			return fmt.Errorf("pwf: %s hashes to %s", name, ref)
		}
		a.Templates[ref] = &Template{Ref: ref, Head: te.Head, Source: source, Parsed: parsed, Problems: problems}
		a.Order = append(a.Order, ref)
		carried[name] = true
	}
	for name := range contents {
		if strings.HasPrefix(name, TemplatesDir) && !carried[name] {
			return fmt.Errorf("pwf: %s is not listed in workflow.json", name)
		}
	}
	if len(a.Order) == 0 || a.Order[0] != m.Root || a.Workflow.Root != m.Root {
		return errors.New("pwf: the root template must be listed first in workflow.json and match the manifest")
	}
	if a.Problems, err = checkClosure(m.Root, a.Templates, a.Order); err != nil {
		return fmt.Errorf("pwf: %w", err)
	}
	derived := Summarize(m.Root, a.Templates, a.Order)
	derived.Title, derived.Summary, derived.Author, derived.Licence = a.Workflow.Title, a.Workflow.Summary, a.Workflow.Author, a.Workflow.Licence
	got, _ := marshal(a.Workflow)
	want, _ := marshal(derived)
	if !bytes.Equal(got, want) {
		// A file written before role selectors were listed still opens, and
		// reads as listing them, so its review matches a newer file's.
		legacy := summarize(m.Root, a.Templates, a.Order, false)
		legacy.Title, legacy.Summary, legacy.Author, legacy.Licence = a.Workflow.Title, a.Workflow.Summary, a.Workflow.Author, a.Workflow.Licence
		if want, _ = marshal(legacy); !bytes.Equal(got, want) {
			return errors.New("pwf: workflow.json does not match its templates")
		}
		a.Workflow.Roles = derived.Roles
	}
	if a.Paths, err = scanPaths(a.Workflow, a.Readme, inOrder(a.Templates, a.Order)); err != nil {
		return err
	}
	return nil
}

func (a *Archive) readSignatures(files map[string]*zip.File, read func(string) ([]byte, error)) error {
	var names []string
	for name := range files {
		if strings.HasPrefix(name, SignaturesDir) {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	for _, name := range names {
		if path.Dir(name)+"/" != SignaturesDir {
			return fmt.Errorf("pwf: unexpected signature entry %q", name)
		}
		raw, err := read(name)
		if err != nil {
			return err
		}
		res := paf.SignatureResult{Path: name}
		var s paf.Signature
		if err := paf.DecodeStrict(raw, &s); err != nil {
			res.Status, res.Err = paf.SignatureInvalid, err
		} else {
			res.Signature = s
			res.Status, res.Err = paf.VerifyContext(signatureContext, a.ManifestRaw, s)
		}
		a.Signatures = append(a.Signatures, res)
	}
	return nil
}

// SignedBy reports whether a valid signature carries pub.
func (a *Archive) SignedBy(pub ed25519.PublicKey) bool { return paf.SignedBy(a.Signatures, pub) }

func (w Workflow) checkText() error {
	if w.Title == "" || len(w.Title) > maxTitle {
		return fmt.Errorf("pwf: title must be 1..%d bytes", maxTitle)
	}
	if len(w.Summary) > maxSummary {
		return fmt.Errorf("pwf: summary exceeds %d bytes", maxSummary)
	}
	for _, r := range w.Summary {
		if (r < 0x20 && r != '\n' && r != '\t') || r == 0x7f || (r >= 0x80 && r < 0xa0) {
			return errors.New("pwf: summary has control characters")
		}
	}
	for name, text := range map[string]string{"title": w.Title, "author": w.Author, "licence": w.Licence} {
		if name != "title" && len(text) > maxCredit {
			return fmt.Errorf("pwf: %s exceeds %d bytes", name, maxCredit)
		}
		for _, r := range text {
			if r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) {
				return fmt.Errorf("pwf: %s has control characters", name)
			}
		}
	}
	return nil
}

func marshal(v any) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}
