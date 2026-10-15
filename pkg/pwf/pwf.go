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
)

var (
	templateIDRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,127}$`)
	hashRE       = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

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
// A reader recomputes everything but Title and Summary and refuses a file
// whose workflow.json disagrees with its templates.
type Workflow struct {
	Title   string `json:"title"`
	Summary string `json:"summary,omitempty"`
	Root    string `json:"root"`
	// Templates lists every template in the file, root first. Head marks the
	// version an unpinned subprocess reference to that id resolves to.
	Templates []TemplateEntry `json:"templates"`
	Params    []Param         `json:"params"`
	// Roles are the agent roles steps ask for (Performer.Profile on agent
	// performers); an importer maps each to a local role.
	Roles []Use `json:"roles"`
	// Programs are every command a step would run. Nothing runs on import;
	// starting a run asks the operator to authorize each program profile.
	Programs []Program `json:"programs"`
	// Assignees are steps that name one specific agent. They are not
	// portable: the importing node's agents differ.
	Assignees []Use `json:"assignees"`
	// Humans are steps a person performs (questions and sign-offs).
	Humans []string `json:"humans"`
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
}

// Bundle is what Write packs: the root and every template it references.
type Bundle struct {
	Root      string
	Title     string
	Summary   string
	Readme    []byte
	Templates []Template
}

type WriteOptions struct {
	Producer  paf.Producer
	CreatedAt time.Time
	Keys      []paf.SigningKey
	Limits    paf.Limits
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
// Sources with validation errors are refused.
func ParseTemplate(source []byte) (*model.Template, string, error) {
	parsed, err := model.Parse(source)
	if err != nil {
		return nil, "", err
	}
	if parsed.Template == nil {
		return nil, "", errors.New("not a process template")
	}
	if parsed.Diagnostics.HasErrors() {
		return nil, "", fmt.Errorf("template %q has validation errors", parsed.Template.ID)
	}
	if !templateIDRE.MatchString(parsed.Template.ID) {
		return nil, "", fmt.Errorf("template id %q is not portable", parsed.Template.ID)
	}
	return parsed.Template, parsed.Ref, nil
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
// head per id, the root is present, and every reference resolves inside the
// file. A workflow must arrive complete.
func checkClosure(root string, byRef map[string]*Template, order []string) error {
	if byRef[root] == nil {
		return fmt.Errorf("root template %q is not in the file", root)
	}
	heads := map[string]bool{}
	for _, ref := range order {
		t := byRef[ref]
		if t.Head {
			if heads[t.Parsed.ID] {
				return fmt.Errorf("template %q has more than one head", t.Parsed.ID)
			}
			heads[t.Parsed.ID] = true
		}
	}
	for _, ref := range order {
		for _, sp := range SubprocessRefs(byRef[ref].Parsed) {
			if resolveIn(sp, byRef, order) == nil {
				want := sp.TemplateRef
				if sp.Version != "" {
					want += "@" + sp.Version
				}
				return fmt.Errorf("template %q references %q, which the file does not carry", byRef[ref].Parsed.ID, want)
			}
		}
	}
	return nil
}

// Summarize derives workflow.json's computed fields from the templates.
func Summarize(root string, byRef map[string]*Template, order []string) Workflow {
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

// Write packs a bundle into a PWF file. The bundle must be complete: the
// root and every template its subprocess steps reference.
func Write(out io.Writer, b Bundle, opts WriteOptions) error {
	byRef := map[string]*Template{}
	var order []string
	for i := range b.Templates {
		t := b.Templates[i]
		parsed, ref, err := ParseTemplate(t.Source)
		if err != nil {
			return fmt.Errorf("pwf: %w", err)
		}
		if t.Ref != "" && t.Ref != ref {
			return fmt.Errorf("pwf: template source hashes to %s, not %s", ref, t.Ref)
		}
		if byRef[ref] != nil {
			return fmt.Errorf("pwf: template %s given twice", ref)
		}
		t.Ref, t.Parsed = ref, parsed
		byRef[ref] = &t
		if ref == b.Root {
			order = append([]string{ref}, order...)
		} else {
			order = append(order, ref)
		}
	}
	if err := checkClosure(b.Root, byRef, order); err != nil {
		return fmt.Errorf("pwf: %w", err)
	}
	wf := Summarize(b.Root, byRef, order)
	wf.Title = strings.TrimSpace(b.Title)
	if wf.Title == "" {
		wf.Title = byRef[b.Root].Parsed.Name
	}
	if wf.Title == "" {
		wf.Title = byRef[b.Root].Parsed.ID
	}
	wf.Summary = strings.TrimSpace(b.Summary)
	if wf.Summary == "" {
		wf.Summary = byRef[b.Root].Parsed.Description
	}
	if err := checkText(wf.Title, wf.Summary); err != nil {
		return err
	}
	workflow, err := marshal(wf)
	if err != nil {
		return err
	}

	created := opts.CreatedAt
	if created.IsZero() {
		created = time.Now()
	}
	limits := opts.Limits
	if limits == (paf.Limits{}) {
		limits = paf.DefaultLimits()
	}
	zw := zip.NewWriter(out)
	man := Manifest{Format: Format, PwfVersion: Version, Kind: KindWorkflow, CreatedAt: created.UTC().Truncate(time.Second).Format(time.RFC3339),
		Producer: opts.Producer, Requires: []string{}, Root: b.Root}
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
	if err := add(WorkflowPath, "application/json", workflow); err != nil {
		return err
	}
	if len(b.Readme) > 0 {
		if err := add(ReadmePath, "text/markdown", b.Readme); err != nil {
			return err
		}
	}
	for _, ref := range order {
		name, err := templatePath(ref)
		if err != nil {
			return fmt.Errorf("pwf: %w", err)
		}
		if err := add(name, TemplateMediaType, byRef[ref].Source); err != nil {
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

// Open reads and fully verifies a PWF file: container rules, manifest,
// entry sizes and hashes, every template's validity and ref, the closure,
// workflow.json against the templates, and signatures. Signature trust is
// the caller's decision: an unsigned file is valid but unverified.
func Open(r io.ReaderAt, size int64, limits paf.Limits) (*Archive, error) {
	if limits == (paf.Limits{}) {
		limits = paf.DefaultLimits()
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
	if len(m.Requires) > 0 {
		return nil, fmt.Errorf("pwf: file requires features this reader does not support: %s", strings.Join(m.Requires, ", "))
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
		switch {
		case p == WorkflowPath, p == ReadmePath, strings.HasPrefix(p, TemplatesDir):
		default:
			return nil, fmt.Errorf("pwf: unknown entry %q", p)
		}
		contents[p] = data
	}
	raw, ok := contents[WorkflowPath]
	if !ok {
		return nil, errors.New("pwf: workflow.json is missing")
	}
	if err := paf.DecodeStrict(raw, &a.Workflow); err != nil {
		return nil, fmt.Errorf("pwf: workflow.json: %w", err)
	}
	if err := checkText(a.Workflow.Title, a.Workflow.Summary); err != nil {
		return nil, err
	}
	a.Readme = contents[ReadmePath]
	carried := map[string]bool{}
	for _, te := range a.Workflow.Templates {
		name, err := templatePath(te.Ref)
		if err != nil {
			return nil, fmt.Errorf("pwf: workflow.json: %w", err)
		}
		source, ok := contents[name]
		if !ok {
			return nil, fmt.Errorf("pwf: template %s is not in the file", te.Ref)
		}
		if a.Templates[te.Ref] != nil {
			return nil, fmt.Errorf("pwf: template %s listed twice", te.Ref)
		}
		parsed, ref, err := ParseTemplate(source)
		if err != nil {
			return nil, fmt.Errorf("pwf: %s: %w", name, err)
		}
		if ref != te.Ref {
			return nil, fmt.Errorf("pwf: %s hashes to %s", name, ref)
		}
		a.Templates[ref] = &Template{Ref: ref, Head: te.Head, Source: source, Parsed: parsed}
		a.Order = append(a.Order, ref)
		carried[name] = true
	}
	for name := range contents {
		if strings.HasPrefix(name, TemplatesDir) && !carried[name] {
			return nil, fmt.Errorf("pwf: %s is not listed in workflow.json", name)
		}
	}
	if len(a.Order) == 0 || a.Order[0] != m.Root || a.Workflow.Root != m.Root {
		return nil, errors.New("pwf: the root template must be listed first in workflow.json and match the manifest")
	}
	if err := checkClosure(m.Root, a.Templates, a.Order); err != nil {
		return nil, fmt.Errorf("pwf: %w", err)
	}
	derived := Summarize(m.Root, a.Templates, a.Order)
	derived.Title, derived.Summary = a.Workflow.Title, a.Workflow.Summary
	got, _ := marshal(a.Workflow)
	want, _ := marshal(derived)
	if !bytes.Equal(got, want) {
		return nil, errors.New("pwf: workflow.json does not match its templates")
	}
	if err := a.readSignatures(files, read); err != nil {
		return nil, err
	}
	return a, nil
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

func checkText(title, summary string) error {
	if title == "" || len(title) > maxTitle {
		return fmt.Errorf("pwf: title must be 1..%d bytes", maxTitle)
	}
	if len(summary) > maxSummary {
		return fmt.Errorf("pwf: summary exceeds %d bytes", maxSummary)
	}
	for _, r := range title {
		if r < 0x20 || r == 0x7f {
			return errors.New("pwf: title has control characters")
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
