package paf

import (
	"archive/zip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"sort"
	"strings"
)

// Purpose says what the caller will do with an archive. It decides how
// features the reader does not understand are handled.
type Purpose int

const (
	// PurposeImport refuses unknown required features and extensions:
	// importing, resuming or rewriting needs every one of them.
	PurposeImport Purpose = iota
	// PurposeInspect degrades instead: read-only inspection and rendering
	// show what they understand and report the rest in Archive.Unsupported.
	PurposeInspect
)

// Options configures Open.
type Options struct {
	Limits  Limits
	Purpose Purpose
}

// knownFeatures are the base requires[] features this package understands.
var knownFeatures = map[string]bool{}

// knownExtensions are the extensions this package understands, by id and
// the newest version it knows.
var knownExtensions = map[string][2]int{ExtTclaude: {0, 1}, "org.tclaude.managed-instructions": {1, 0}}

// versionReadable reports whether a MAJOR.MINOR version can be read by a
// reader of the given major version. Minor versions only add, from 0.1 on
// (see "Compatibility policy" in docs/paf.md), so any minor of the same
// major is read; a deliberate break is marked with min_reader_version.
func versionReadable(v string, major int) bool {
	gotMajor, _, err := ParseVersion(v)
	return err == nil && gotMajor == major
}

// ReadVersions are the PAF versions this package reads natively, which an
// instance advertises to its federation peers.
func ReadVersions() []string { return []string{Version, EnvelopeVersion} }

// writeVersions are the versions this package can write, newest first.
var writeVersions = []string{Version}

// NegotiateVersion picks the version to send a peer that reads peer: the
// newest version this package writes that the peer lists, or "" when they
// share none and the legacy carrier must be used.
func NegotiateVersion(peer []string) string {
	for _, v := range writeVersions {
		for _, p := range peer {
			if p == v {
				return v
			}
		}
	}
	return ""
}

// versionBefore reports whether MAJOR.MINOR a is older than b. Invalid
// versions compare as not older.
func versionBefore(a, b string) bool {
	aMajor, aMinor, errA := ParseVersion(a)
	bMajor, bMinor, errB := ParseVersion(b)
	if errA != nil || errB != nil {
		return false
	}
	return aMajor < bMajor || aMajor == bMajor && aMinor < bMinor
}

// maxWarnings bounds the best-effort warnings one Open collects.
const maxWarnings = 20

// ErrNotPAF reports an archive that is not a PAF file.
var ErrNotPAF = errors.New("paf: not a PAF archive")

// VersionError reports a PAF this package cannot import: another major
// version, or a min_reader_version newer than this reader. It still carries
// what the manifest says, so a user knows what to upgrade.
type VersionError struct {
	PafVersion       string
	MinReaderVersion string
	Producer         Producer
}

func (e *VersionError) Error() string {
	p := e.Producer.Name
	if e.Producer.Version != "" {
		p += " " + e.Producer.Version
	}
	if e.MinReaderVersion != "" {
		return fmt.Sprintf("paf: this file needs PAF >= %s (written by %s as PAF %s); this reader supports PAF %s", e.MinReaderVersion, p, e.PafVersion, EnvelopeVersion)
	}
	return fmt.Sprintf("paf: unsupported PAF version %s (written by %s; this reader supports %d.x)", e.PafVersion, p, Major)
}

// Archive is a verified, opened PAF.
type Archive struct {
	Manifest    Manifest
	ManifestRaw []byte
	Signatures  []SignatureResult
	// Unsupported lists required features and extensions this reader does
	// not understand. It is only ever non-empty for PurposeInspect.
	Unsupported []string
	// Warnings says why an inspection read the file best effort: it needs
	// a newer reader, and what did not check out. Only PurposeInspect.
	Warnings []string

	// bestEffort reports schema mismatches as warnings instead of
	// refusing: set for an inspected file that needs a newer reader.
	bestEffort bool

	limits    Limits
	listedSet map[string]bool
	files     map[string]*zip.File
	agents    map[string]*Agent
	envs      map[string]*Environment
	losses    map[string]*Losses
	group     *Group
	privacy   *Privacy
}

// Open reads and fully verifies a PAF: container rules, manifest, entry
// sizes and hashes, schemas, semantic rules and signatures. A returned
// Archive has passed every check its Purpose requires.
func Open(r io.ReaderAt, size int64, opts Options) (*Archive, error) {
	lim := opts.Limits.orDefault()
	if size > lim.MaxArchiveBytes {
		return nil, fmt.Errorf("paf: archive exceeds %d bytes", lim.MaxArchiveBytes)
	}
	zr, err := zip.NewReader(r, size)
	if err != nil {
		return nil, fmt.Errorf("paf: invalid ZIP: %w", err)
	}
	a := &Archive{limits: lim, files: map[string]*zip.File{}, agents: map[string]*Agent{}, envs: map[string]*Environment{}, losses: map[string]*Losses{}}
	if err := a.indexFiles(r, size, zr.File); err != nil {
		return nil, err
	}
	if err := a.readManifest(opts.Purpose); err != nil {
		return nil, err
	}
	if err := a.verifyEntries(); err != nil {
		return nil, err
	}
	if err := a.checkSemantics(); err != nil {
		return nil, err
	}
	if err := a.readSignatures(); err != nil {
		return nil, err
	}
	return a, nil
}

func (a *Archive) indexFiles(ra io.ReaderAt, size int64, files []*zip.File) error {
	indexed, err := IndexZip(ra, size, files, a.limits)
	if err != nil {
		return err
	}
	a.files = indexed
	return nil
}

// IndexZip applies the container rules every reader of this family of
// formats enforces (PAF, PWF): regular files only, valid unique names,
// stored or deflated, bounded counts and sizes, and local headers that agree
// with the central directory. It returns the files by name. manifest.json and
// signatures/ are bounded by the metadata limit instead of the content total.
func IndexZip(ra io.ReaderAt, size int64, files []*zip.File, limits Limits) (map[string]*zip.File, error) {
	limits = limits.orDefault()
	out := map[string]*zip.File{}
	if len(files) > limits.MaxEntries+1+64 {
		return nil, errors.New("paf: too many archive entries")
	}
	offsets, err := localHeaderOffsets(ra, size, len(files))
	if err != nil {
		return nil, err
	}
	var total uint64
	for _, f := range files {
		if f.FileInfo().IsDir() {
			continue
		}
		if !f.Mode().IsRegular() {
			return nil, fmt.Errorf("paf: entry %q is not a regular file", f.Name)
		}
		if !ValidEntryPath(f.Name) {
			return nil, fmt.Errorf("paf: invalid entry name %q", f.Name)
		}
		if _, dup := out[f.Name]; dup {
			return nil, fmt.Errorf("paf: duplicate entry %q", f.Name)
		}
		if f.Method != zip.Store && f.Method != zip.Deflate {
			return nil, fmt.Errorf("paf: entry %q uses an unsupported compression method", f.Name)
		}
		// The manifest and signatures are bounded by the metadata limit
		// instead, matching what the writer counts.
		if f.Name != ManifestPath && !strings.HasPrefix(f.Name, SignaturesDir) {
			total += f.UncompressedSize64
			if total > uint64(limits.MaxArchiveBytes) {
				return nil, fmt.Errorf("paf: uncompressed content exceeds %d bytes", limits.MaxArchiveBytes)
			}
		}
		off, ok := offsets[f.Name]
		if !ok {
			return nil, fmt.Errorf("paf: entry %q: local header does not match the central directory", f.Name)
		}
		if err := checkLocalHeader(ra, size, f, off); err != nil {
			return nil, err
		}
		out[f.Name] = f
	}
	return out, nil
}

// readBounded reads a whole entry, refusing more than limit bytes and
// verifying the decompressed size.
func (a *Archive) readBounded(f *zip.File, limit int64) ([]byte, error) { return ReadBounded(f, limit) }

// ReadBounded reads a whole entry, refusing more than limit bytes and
// verifying the decompressed size.
func ReadBounded(f *zip.File, limit int64) ([]byte, error) {
	if f.UncompressedSize64 > uint64(limit) {
		return nil, fmt.Errorf("paf: entry %q exceeds %d bytes", f.Name, limit)
	}
	rc, err := f.Open()
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	b, err := io.ReadAll(io.LimitReader(rc, limit+1))
	if err != nil {
		return nil, fmt.Errorf("paf: entry %q: %w", f.Name, err)
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("paf: entry %q exceeds %d bytes", f.Name, limit)
	}
	return b, nil
}

func (a *Archive) readManifest(purpose Purpose) error {
	f := a.files[ManifestPath]
	if f == nil {
		return ErrNotPAF
	}
	raw, err := a.readBounded(f, a.limits.MaxMetadataBytes)
	if err != nil {
		return err
	}
	if err := checkNoDuplicateKeys(raw); err != nil {
		return fmt.Errorf("paf: manifest.json: %w", err)
	}
	var head struct {
		Format           string   `json:"format"`
		PafVersion       string   `json:"paf_version"`
		MinReaderVersion string   `json:"min_reader_version"`
		Producer         Producer `json:"producer"`
	}
	if err := json.Unmarshal(raw, &head); err != nil || head.Format != Format {
		return ErrNotPAF
	}
	var verr *VersionError
	switch {
	case !versionReadable(head.PafVersion, Major):
		verr = &VersionError{PafVersion: head.PafVersion, Producer: head.Producer}
	case head.MinReaderVersion != "" && versionBefore(EnvelopeVersion, head.MinReaderVersion):
		verr = &VersionError{PafVersion: head.PafVersion, MinReaderVersion: head.MinReaderVersion, Producer: head.Producer}
	}
	if verr != nil {
		// Import and resume refuse a file that needs a newer reader;
		// inspection and rendering still try, and say so.
		if purpose != PurposeInspect {
			return verr
		}
		a.bestEffort = true
		a.warn(strings.TrimPrefix(verr.Error(), "paf: ") + "; read best effort, and parts may be missing or misread")
	}
	if err := a.lenient("manifest.json", validateJSON(SchemaManifest, raw)); err != nil {
		return fmt.Errorf("paf: manifest.json: %w", err)
	}
	if err := json.Unmarshal(raw, &a.Manifest); err != nil {
		return fmt.Errorf("paf: manifest.json: %w", err)
	}
	a.ManifestRaw = raw
	m := &a.Manifest
	for id, d := range m.ExtensionsUsed {
		if !wellFormedExtensionID(id) {
			return fmt.Errorf("paf: invalid extension id %q", id)
		}
		if _, _, err := ParseVersion(d.Version); err != nil {
			return fmt.Errorf("paf: extension %q: %w", id, err)
		}
	}
	for _, id := range m.ExtensionsRequired {
		if _, ok := m.ExtensionsUsed[id]; !ok {
			return fmt.Errorf("paf: required extension %q is not declared", id)
		}
	}
	var unsupported []string
	for _, f := range m.Requires {
		if !knownFeatures[f] {
			unsupported = append(unsupported, f)
		}
	}
	for _, id := range m.ExtensionsRequired {
		if !a.UnderstandsExtension(id) {
			unsupported = append(unsupported, "ext:"+id)
		}
	}
	if len(unsupported) > 0 {
		if purpose != PurposeInspect {
			return fmt.Errorf("paf: archive requires features this reader does not support: %s", strings.Join(unsupported, ", "))
		}
		a.Unsupported = unsupported
	}
	return nil
}

// warn records a best-effort warning, up to maxWarnings.
func (a *Archive) warn(msg string) {
	switch {
	case len(a.Warnings) < maxWarnings:
		a.Warnings = append(a.Warnings, msg)
	case len(a.Warnings) == maxWarnings:
		a.Warnings = append(a.Warnings, "more warnings not shown")
	}
}

// lenient passes a schema error through, or, reading best effort, records
// it as a warning and returns nil.
func (a *Archive) lenient(where string, err error) error {
	if err == nil || !a.bestEffort {
		return err
	}
	a.warn(where + ": " + err.Error())
	return nil
}

// UnderstandsExtension reports whether this package knows a declared
// extension at its declared major version.
func (a *Archive) UnderstandsExtension(id string) bool {
	d, ok := a.Manifest.ExtensionsUsed[id]
	if !ok {
		return false
	}
	known, ok := knownExtensions[id]
	return ok && versionReadable(d.Version, known[0])
}

func (a *Archive) verifyEntries() error {
	listed := map[string]Entry{}
	a.listedSet = map[string]bool{}
	for _, e := range a.Manifest.Entries {
		a.listedSet[e.Path] = true
		if e.Path == ManifestPath || strings.HasPrefix(e.Path, SignaturesDir) {
			return fmt.Errorf("paf: manifest lists reserved entry %q", e.Path)
		}
		if _, dup := listed[e.Path]; dup {
			return fmt.Errorf("paf: manifest lists %q twice", e.Path)
		}
		listed[e.Path] = e
	}
	for name := range a.files {
		if name == ManifestPath || strings.HasPrefix(name, SignaturesDir) {
			continue
		}
		if _, ok := listed[name]; !ok {
			return fmt.Errorf("paf: entry %q is not listed in the manifest", name)
		}
	}
	paths := make([]string, 0, len(listed))
	for p := range listed {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		if err := a.verifyEntry(listed[p]); err != nil {
			return err
		}
	}
	return nil
}

// verifyEntry checks one entry's size and hash and, for metadata and
// context files, its schema.
func (a *Archive) verifyEntry(e Entry) error {
	f := a.files[e.Path]
	if f == nil {
		return fmt.Errorf("paf: listed entry %q is missing", e.Path)
	}
	if f.UncompressedSize64 != uint64(e.Bytes) {
		return fmt.Errorf("paf: entry %q size does not match the manifest", e.Path)
	}
	schema := metadataSchema(e.Path)
	if schema != "" {
		raw, err := a.readBounded(f, a.limits.MaxMetadataBytes)
		if err != nil {
			return err
		}
		if hexSHA256(raw) != e.SHA256 {
			return fmt.Errorf("paf: entry %q hash does not match the manifest", e.Path)
		}
		return a.parseMetadata(e.Path, schema, raw)
	}
	rc, err := f.Open()
	if err != nil {
		return err
	}
	defer rc.Close()
	h := sha256.New()
	src := io.TeeReader(io.LimitReader(rc, e.Bytes+1), h)
	if isContextFile(e.Path) {
		if err := a.checkContext(e.Path, src); err != nil {
			return err
		}
	}
	if _, err := io.Copy(io.Discard, src); err != nil {
		return fmt.Errorf("paf: entry %q: %w", e.Path, err)
	}
	if hex.EncodeToString(h.Sum(nil)) != e.SHA256 {
		return fmt.Errorf("paf: entry %q hash does not match the manifest", e.Path)
	}
	return nil
}

// metadataSchema returns the schema of a JSON metadata entry, or "".
func metadataSchema(p string) string {
	switch p {
	case PrivacyPath:
		return SchemaPrivacy
	case GroupPath:
		return SchemaGroup
	}
	key, rel, ok := splitAgentPath(p)
	if !ok || !ValidAgentKey(key) {
		return ""
	}
	switch rel {
	case "agent.json":
		return SchemaAgent
	case "environment.json":
		return SchemaEnvironment
	case "context/losses.json":
		return SchemaLosses
	}
	return ""
}

func splitAgentPath(p string) (key, rel string, ok bool) {
	rest, ok := strings.CutPrefix(p, AgentsDir)
	if !ok {
		return "", "", false
	}
	return strings.Cut(rest, "/")
}

func isContextFile(p string) bool {
	_, rel, ok := splitAgentPath(p)
	return ok && (rel == "context/transcript.jsonl" || rel == "context/full.jsonl")
}

func (a *Archive) parseMetadata(p, schema string, raw []byte) error {
	if err := checkNoDuplicateKeys(raw); err != nil {
		return fmt.Errorf("paf: %s: %w", p, err)
	}
	if err := a.lenient(p, validateJSON(schema, raw)); err != nil {
		return fmt.Errorf("paf: %s: %w", p, err)
	}
	if err := a.checkDeclared(raw); err != nil {
		return fmt.Errorf("paf: %s: %w", p, err)
	}
	key, _, _ := splitAgentPath(p)
	var err error
	switch schema {
	case SchemaPrivacy:
		a.privacy = &Privacy{}
		err = json.Unmarshal(raw, a.privacy)
	case SchemaGroup:
		a.group = &Group{}
		err = json.Unmarshal(raw, a.group)
	case SchemaAgent:
		v := &Agent{}
		err = json.Unmarshal(raw, v)
		a.agents[key] = v
	case SchemaEnvironment:
		v := &Environment{}
		err = json.Unmarshal(raw, v)
		a.envs[key] = v
	case SchemaLosses:
		v := &Losses{}
		err = json.Unmarshal(raw, v)
		a.losses[key] = v
	}
	if err != nil {
		return fmt.Errorf("paf: %s: %w", p, err)
	}
	return nil
}

// checkDeclared refuses extension ids used in raw that the manifest does
// not declare.
func (a *Archive) checkDeclared(raw []byte) error {
	return forEachExtension(raw, a.declaredExtension)
}

// checkDeclaredDoc is checkDeclared on an already decoded document.
func (a *Archive) checkDeclaredDoc(doc any) error {
	return walkExtensions(doc, a.declaredExtension)
}

func (a *Archive) declaredExtension(id string, data map[string]any) error {
	if _, ok := a.Manifest.ExtensionsUsed[id]; !ok {
		return fmt.Errorf("extension %q is used but not declared", id)
	}
	if schema := extensionSchemas[id]; schema != "" && a.UnderstandsExtension(id) {
		if err := a.lenient("extension "+id, ValidateSchema(schema, any(data))); err != nil {
			return fmt.Errorf("extension %q: %w", id, err)
		}
	}
	return nil
}

// extensionSchemas are the bundled schemas of extensions this package
// understands.
var extensionSchemas = map[string]string{ExtTclaude: SchemaExtTclaude}

// checkContext validates every line of a context file: its schema, the
// closed rules, declared extensions and attachment references.
func (a *Archive) checkContext(p string, r io.Reader) error {
	cr, err := NewContextReader(r, a.limits.MaxRecordBytes)
	if err != nil {
		return fmt.Errorf("paf: %s: %w", p, err)
	}
	wantScope := ScopeEffective
	if strings.HasSuffix(p, "/full.jsonl") {
		wantScope = ScopeFull
	}
	if cr.Header.Scope != wantScope {
		return fmt.Errorf("paf: %s: header scope %q, want %q", p, cr.Header.Scope, wantScope)
	}
	check := func(l Line) error {
		// One decode serves the schema and the extension walk. The walk is
		// structural: a key spelled with escapes is still an "extensions"
		// key, so no byte search may stand in for it.
		var doc any
		if err := json.Unmarshal(l.Raw, &doc); err != nil {
			return err
		}
		if err := a.lenient(p, ValidateSchema(SchemaLine, doc)); err != nil {
			return err
		}
		if err := a.checkDeclaredDoc(doc); err != nil {
			return err
		}
		for _, b := range l.Blocks {
			for _, m := range b.Media {
				if m.Attachment == "" {
					continue
				}
				ap, _ := AttachmentPath(m.Attachment)
				if !a.listed(ap) {
					return fmt.Errorf("attachment %s is not in the archive", m.Attachment)
				}
			}
		}
		return nil
	}
	if err := check(cr.Header); err != nil {
		return fmt.Errorf("paf: %s: header: %w", p, err)
	}
	for {
		l, err := cr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("paf: %s: %w", p, err)
		}
		if err := check(l); err != nil {
			return fmt.Errorf("paf: %s line %d: %w", p, cr.line, err)
		}
	}
}

func (a *Archive) listed(p string) bool { return a.listedSet[p] }

func (a *Archive) checkSemantics() error {
	m := a.Manifest
	if m.Kind == KindGroup && a.group == nil {
		return errors.New("paf: group PAF has no group.json")
	}
	if m.Kind != KindGroup && a.group != nil {
		return errors.New("paf: group.json in an agent PAF")
	}
	if m.Kind == KindBundle && len(m.Workflows) == 0 {
		return errors.New("paf: bundle PAF has no workflow")
	}
	for _, key := range m.Agents {
		ag := a.agents[key]
		if ag == nil || a.envs[key] == nil {
			return fmt.Errorf("paf: agent %q has no agent.json or environment.json", key)
		}
		if ag.Native != nil {
			if !strings.HasPrefix(ag.Native.Path, AgentPath(key, "native/")) {
				return fmt.Errorf("paf: agent %q: native path %q is outside its native directory", key, ag.Native.Path)
			}
			if !a.listed(ag.Native.Path) {
				return fmt.Errorf("paf: agent %q: native blob %q is missing", key, ag.Native.Path)
			}
		}
		if a.listed(AgentPath(key, "context/transcript.jsonl")) && a.losses[key] == nil {
			return fmt.Errorf("paf: agent %q has context but no losses.json", key)
		}
	}
	for key := range a.agents {
		if !slices.Contains(m.Agents, key) {
			return fmt.Errorf("paf: agent directory %q is not listed in the manifest", key)
		}
	}
	if a.group != nil {
		for _, mem := range a.group.Members {
			if !slices.Contains(m.Agents, mem.Agent) {
				return fmt.Errorf("paf: group member %q is not an agent in the archive", mem.Agent)
			}
		}
	}
	for _, e := range m.Entries {
		if key, _, ok := splitAgentPath(e.Path); ok && !slices.Contains(m.Agents, key) {
			return fmt.Errorf("paf: entry %q belongs to an agent the manifest does not list", e.Path)
		}
		if strings.HasPrefix(e.Path, ExtDir) {
			id, _, _ := strings.Cut(strings.TrimPrefix(e.Path, ExtDir), "/")
			if _, ok := m.ExtensionsUsed[id]; !ok {
				return fmt.Errorf("paf: entry %q belongs to undeclared extension %q", e.Path, id)
			}
		}
		if strings.HasPrefix(e.Path, AttachmentsDir) {
			if h := strings.TrimPrefix(e.Path, AttachmentsDir); h != e.SHA256 {
				return fmt.Errorf("paf: attachment %q is not named by its hash", e.Path)
			}
		}
	}
	if err := a.checkEnvelope(); err != nil {
		if a.lenient("envelope", err) != nil {
			return err
		}
	}
	return a.checkDeclared(a.ManifestRaw)
}

// checkEnvelope verifies the workflow items and links: every key resolves,
// every digest matches, and each workflow's entries belong to it. What a
// workflow holds is checked by pkg/pwf, which reads it. A file with no
// workflow items has no envelope, whatever its listed entries are called,
// since a reader ignores a listed entry it does not understand.
func (a *Archive) checkEnvelope() error {
	m := a.Manifest
	if len(m.Workflows) == 0 {
		if len(m.Links) > 0 {
			return errors.New("paf: links in a file with no workflows")
		}
		return nil
	}
	items := map[string]WorkflowItem{}
	for _, wf := range m.Workflows {
		if !ValidAgentKey(wf.Key) {
			return fmt.Errorf("paf: invalid workflow key %q", wf.Key)
		}
		if _, dup := items[wf.Key]; dup {
			return fmt.Errorf("paf: workflow %q is listed twice", wf.Key)
		}
		items[wf.Key] = wf
		if !a.listed(WorkflowPath(wf.Key, "workflow.json")) {
			return fmt.Errorf("paf: workflow %q has no workflow.json", wf.Key)
		}
		if got := SectionDigest(m.Entries, WorkflowPath(wf.Key, "")); got != wf.Digest {
			return fmt.Errorf("paf: workflow %q digest does not match its entries", wf.Key)
		}
	}
	for _, e := range m.Entries {
		if rest, ok := strings.CutPrefix(e.Path, WorkflowsDir); ok {
			key, _, _ := strings.Cut(rest, "/")
			if _, listed := items[key]; !listed {
				return fmt.Errorf("paf: entry %q belongs to a workflow the manifest does not list", e.Path)
			}
		}
	}
	roles := map[[2]string]bool{}
	for _, l := range m.Links {
		switch l.Type {
		case LinkPerformer, LinkCarries:
		default:
			// An open enumeration: a reader skips a link type it does not know.
			continue
		}
		wf, ok := items[l.Workflow]
		if !ok {
			return fmt.Errorf("paf: %s link names unknown workflow %q", l.Type, l.Workflow)
		}
		if !slices.Contains(m.Agents, l.Agent) {
			return fmt.Errorf("paf: %s link names unknown agent %q", l.Type, l.Agent)
		}
		want := wf.Digest
		if l.Type == LinkPerformer {
			want = SectionDigest(m.Entries, AgentPath(l.Agent, ""))
			if l.Role == "" {
				return errors.New("paf: performer link has no role")
			}
			slot := [2]string{l.Workflow, l.Role}
			if roles[slot] {
				return fmt.Errorf("paf: workflow %q has two performers for role %q", l.Workflow, l.Role)
			}
			roles[slot] = true
		}
		if l.Digest != want {
			return fmt.Errorf("paf: %s link digest does not match what it names", l.Type)
		}
	}
	return nil
}

func (a *Archive) readSignatures() error {
	var names []string
	for name := range a.files {
		if strings.HasPrefix(name, SignaturesDir) {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	for _, name := range names {
		raw, err := a.readBounded(a.files[name], a.limits.MaxMetadataBytes)
		if err != nil {
			return err
		}
		res := SignatureResult{Path: name}
		var alg struct {
			Alg string `json:"alg"`
		}
		var s Signature
		if err := json.Unmarshal(raw, &alg); err == nil && alg.Alg != AlgEd25519 {
			res.Status = SignatureUnsupported
			res.Signature.Alg = alg.Alg
			if !algRE.MatchString(alg.Alg) {
				res.Signature.Alg = "unrecognised"
			}
		} else if err := decodeStrict(raw, &s); err != nil {
			res.Status, res.Err = SignatureInvalid, err
		} else {
			res.Signature = s
			res.Status, res.Err = Verify(a.ManifestRaw, s)
		}
		a.Signatures = append(a.Signatures, res)
	}
	return nil
}

// Agent returns an agent's agent.json.
func (a *Archive) Agent(key string) (*Agent, bool) { v, ok := a.agents[key]; return v, ok }

// Environment returns an agent's environment.json.
func (a *Archive) Environment(key string) (*Environment, bool) { v, ok := a.envs[key]; return v, ok }

// Losses returns an agent's context/losses.json, if it has context.
func (a *Archive) Losses(key string) (*Losses, bool) { v, ok := a.losses[key]; return v, ok }

// Group returns group.json for a group PAF.
func (a *Archive) Group() *Group { return a.group }

// Performers lists the agents a workflow ships, from its performer links.
func (a *Archive) Performers(workflow string) []Link {
	var out []Link
	for _, l := range a.Manifest.Links {
		if l.Type == LinkPerformer && l.Workflow == workflow {
			out = append(out, l)
		}
	}
	return out
}

// Privacy returns privacy.json, or nil when absent (which means private).
func (a *Archive) Privacy() *Privacy { return a.privacy }

// HasEntry reports whether a listed entry exists.
func (a *Archive) HasEntry(p string) bool { return a.listed(p) }

// OpenEntry opens a listed entry. The archive was verified by Open; the
// reader still enforces the declared size.
func (a *Archive) OpenEntry(p string) (io.ReadCloser, error) {
	if !a.listed(p) {
		return nil, fmt.Errorf("paf: no entry %q", p)
	}
	return a.files[p].Open()
}

// ReadEntry reads a listed entry whole, up to limit bytes.
func (a *Archive) ReadEntry(p string, limit int64) ([]byte, error) {
	if !a.listed(p) {
		return nil, fmt.Errorf("paf: no entry %q", p)
	}
	return a.readBounded(a.files[p], limit)
}

// Attachment reads an attachment by its media reference.
func (a *Archive) Attachment(ref string, limit int64) ([]byte, error) {
	p, ok := AttachmentPath(ref)
	if !ok {
		return nil, fmt.Errorf("paf: invalid attachment reference %q", ref)
	}
	return a.ReadEntry(p, limit)
}

// HasContext reports whether an agent carries a context of the scope.
func (a *Archive) HasContext(key, scope string) bool {
	return a.listed(contextPath(key, scope))
}

func contextPath(key, scope string) string {
	if scope == ScopeFull {
		return AgentPath(key, "context/full.jsonl")
	}
	return AgentPath(key, "context/transcript.jsonl")
}

// Context opens an agent's context file. Close the returned closer when done.
func (a *Archive) Context(key, scope string) (*ContextReader, io.Closer, error) {
	rc, err := a.OpenEntry(contextPath(key, scope))
	if err != nil {
		return nil, nil, err
	}
	cr, err := NewContextReader(rc, a.limits.MaxRecordBytes)
	if err != nil {
		rc.Close()
		return nil, nil, err
	}
	return cr, rc, nil
}
