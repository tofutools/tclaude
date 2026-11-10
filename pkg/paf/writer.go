package paf

import (
	"archive/zip"
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"slices"
	"sort"
	"strings"
	"time"
)

// SigningKey signs a manifest. KeyID names the signature file; Signer labels
// it. Both are self-asserted: verifiers trust only the public key.
type SigningKey struct {
	Key    ed25519.PrivateKey
	KeyID  string
	Signer *Signer
}

// Writer streams a PAF archive. Entries are written as they are added; the
// manifest and signatures are written by Close, which is the only point
// where the archive becomes valid.
type Writer struct {
	zw       *zip.Writer
	limits   Limits
	kind     string
	total    int64
	entries  []Entry
	seen     map[string]bool
	agents   []string
	open     bool
	closed   bool
	created  time.Time
	requires []string
	extUsed  map[string]ExtensionDecl
	extReq   []string
	extSeen  map[string]bool
	manExt   Extensions
	natives  []string
	members  []string
	// workflows and links are the envelope; their digests are filled in by
	// Close, once every entry is written.
	workflows []WorkflowItem
	links     []Link
}

// NewWriter starts an archive of the given kind.
func NewWriter(w io.Writer, kind string, limits Limits) *Writer {
	return &Writer{
		zw:      zip.NewWriter(w),
		limits:  limits.orDefault(),
		kind:    kind,
		seen:    map[string]bool{},
		created: time.Now().UTC().Truncate(time.Second),
		extUsed: map[string]ExtensionDecl{},
		extSeen: map[string]bool{},
	}
}

// SetCreatedAt overrides the manifest's created_at (for reproducible output).
func (w *Writer) SetCreatedAt(t time.Time) { w.created = t.UTC() }

// Require adds a base feature to manifest.requires.
func (w *Writer) Require(feature string) {
	if !slices.Contains(w.requires, feature) {
		w.requires = append(w.requires, feature)
	}
}

// DeclareExtension declares an extension in manifest.extensions_used.
// required also lists it in extensions_required.
func (w *Writer) DeclareExtension(id string, d ExtensionDecl, required bool) error {
	if !ValidExtensionID(id) {
		return fmt.Errorf("paf: invalid extension id %q", id)
	}
	if d.Version == "" || d.System == "" || d.Spec == "" {
		return fmt.Errorf("paf: extension %q needs version, system and spec", id)
	}
	if _, _, err := ParseVersion(d.Version); err != nil {
		return fmt.Errorf("paf: extension %q: %w", id, err)
	}
	w.extUsed[id] = d
	if required && !slices.Contains(w.extReq, id) {
		w.extReq = append(w.extReq, id)
	}
	return nil
}

// SetManifestExtensions sets extension data on the manifest root.
func (w *Writer) SetManifestExtensions(e Extensions) { w.manExt = e }

// Entries returns the entries written so far.
func (w *Writer) Entries() []Entry { return slices.Clone(w.entries) }

// ContentDigest is the consent digest over the entries written so far.
// Write privacy.json after every other entry, then call it.
func (w *Writer) ContentDigest() string { return ContentDigest(w.entries) }

// Create starts an entry and returns a writer for its bytes. The entry is
// recorded when the returned writer is closed. One entry is open at a time.
func (w *Writer) Create(path, mediaType string) (io.WriteCloser, error) {
	if w.closed {
		return nil, errors.New("paf: writer is closed")
	}
	if w.open {
		return nil, errors.New("paf: another entry is still open")
	}
	if !ValidEntryPath(path) || path == ManifestPath || strings.HasPrefix(path, SignaturesDir) {
		return nil, fmt.Errorf("paf: invalid entry path %q", path)
	}
	if w.seen[path] {
		return nil, fmt.Errorf("paf: duplicate entry %q", path)
	}
	if len(w.entries) >= w.limits.MaxEntries {
		return nil, errors.New("paf: too many entries")
	}
	if strings.HasPrefix(path, ExtDir) {
		id, _, _ := strings.Cut(strings.TrimPrefix(path, ExtDir), "/")
		w.extSeen[id] = true
	}
	zf, err := w.createZip(path)
	if err != nil {
		return nil, err
	}
	w.seen[path] = true
	w.open = true
	return &entryWriter{w: w, zf: zf, h: sha256.New(), path: path, mediaType: mediaType}, nil
}

func (w *Writer) createZip(path string) (io.Writer, error) {
	hdr := &zip.FileHeader{Name: path, Method: zip.Deflate}
	hdr.SetMode(0o600)
	return w.zw.CreateHeader(hdr)
}

type entryWriter struct {
	w         *Writer
	zf        io.Writer
	h         hash.Hash
	n         int64
	path      string
	mediaType string
	done      bool
}

func (e *entryWriter) Write(p []byte) (int, error) {
	if e.done {
		return 0, errors.New("paf: entry is closed")
	}
	if e.w.total+int64(len(p)) > e.w.limits.MaxArchiveBytes {
		return 0, fmt.Errorf("paf: archive exceeds %d bytes", e.w.limits.MaxArchiveBytes)
	}
	n, err := e.zf.Write(p)
	e.h.Write(p[:n])
	e.n += int64(n)
	e.w.total += int64(n)
	return n, err
}

func (e *entryWriter) Close() error {
	if e.done {
		return nil
	}
	e.done = true
	e.w.open = false
	e.w.entries = append(e.w.entries, Entry{Path: e.path, Bytes: e.n, SHA256: hex.EncodeToString(e.h.Sum(nil)), MediaType: e.mediaType})
	return nil
}

// WriteFile writes a whole entry.
func (w *Writer) WriteFile(path, mediaType string, data []byte) error {
	ew, err := w.Create(path, mediaType)
	if err != nil {
		return err
	}
	if _, err := ew.Write(data); err != nil {
		return err
	}
	return ew.Close()
}

// WriteJSON writes a JSON metadata entry, recording the extension ids it uses.
func (w *Writer) WriteJSON(path string, v any) error {
	b, err := marshalJSON(v)
	if err != nil {
		return err
	}
	if int64(len(b)) > w.limits.MaxMetadataBytes {
		return fmt.Errorf("paf: %s exceeds %d bytes", path, w.limits.MaxMetadataBytes)
	}
	if schema := metadataSchema(path); schema != "" {
		if err := validateJSON(schema, b); err != nil {
			return fmt.Errorf("paf: %s: %w", path, err)
		}
	}
	if err := collectExtensionIDs(b, w.extSeen); err != nil {
		return fmt.Errorf("paf: %s: %w", path, err)
	}
	if path == GroupPath {
		var g Group
		if err := json.Unmarshal(b, &g); err != nil {
			return err
		}
		for _, m := range g.Members {
			w.members = append(w.members, m.Agent)
		}
	}
	return w.WriteFile(path, "application/json", b)
}

// AddAttachment stores data content-addressed and returns its media
// reference. Identical data is stored once.
func (w *Writer) AddAttachment(data []byte, mediaType string) (string, error) {
	h := hexSHA256(data)
	path := AttachmentsDir + h
	if !w.seen[path] {
		if err := w.WriteFile(path, mediaType, data); err != nil {
			return "", err
		}
	}
	return AttachmentRef(h), nil
}

// AgentPath returns the entry path of a file in an agent's directory.
func AgentPath(key, rel string) string { return AgentsDir + key + "/" + rel }

// AddAgent writes an agent's agent.json and environment.json.
func (w *Writer) AddAgent(key string, a Agent, env Environment) error {
	if !ValidAgentKey(key) {
		return fmt.Errorf("paf: invalid agent key %q", key)
	}
	if slices.Contains(w.agents, key) {
		return fmt.Errorf("paf: duplicate agent key %q", key)
	}
	if w.kind == KindAgent && len(w.agents) > 0 {
		return errors.New("paf: an agent PAF holds exactly one agent")
	}
	if strings.TrimSpace(a.Name) == "" || env.Harness.Name == "" {
		return errors.New("paf: agent name and harness name are required")
	}
	if a.Native != nil && !strings.HasPrefix(a.Native.Path, AgentPath(key, "native/")) {
		return fmt.Errorf("paf: native path %q is outside agent %q", a.Native.Path, key)
	}
	if err := w.WriteJSON(AgentPath(key, "agent.json"), a); err != nil {
		return err
	}
	if err := w.WriteJSON(AgentPath(key, "environment.json"), env); err != nil {
		return err
	}
	w.agents = append(w.agents, key)
	if a.Native != nil {
		w.natives = append(w.natives, a.Native.Path)
	}
	return nil
}

// AddWorkflow records a workflow whose entries the caller writes under
// workflows/<key>/ (see pwf.WriteEmbedded).
func (w *Writer) AddWorkflow(key, root string, requires []string) error {
	if !ValidAgentKey(key) {
		return fmt.Errorf("paf: invalid workflow key %q", key)
	}
	if slices.ContainsFunc(w.workflows, func(wf WorkflowItem) bool { return wf.Key == key }) {
		return fmt.Errorf("paf: duplicate workflow key %q", key)
	}
	w.workflows = append(w.workflows, WorkflowItem{Key: key, Root: root, Requires: append([]string{}, requires...)})
	return nil
}

// AddLink records a reference between an agent and a workflow. Close fills
// in its digest.
func (w *Writer) AddLink(l Link) { w.links = append(w.links, l) }

// CreateContext starts an agent's context file (scope effective writes
// transcript.jsonl, scope full writes full.jsonl). Close the returned closer
// after the last line.
func (w *Writer) CreateContext(key, scope string, src Source) (*ContextWriter, io.Closer, error) {
	name := "transcript.jsonl"
	if scope == ScopeFull {
		name = "full.jsonl"
	} else if scope != ScopeEffective {
		return nil, nil, fmt.Errorf("paf: unknown scope %q", scope)
	}
	ew, err := w.Create(AgentPath(key, "context/"+name), "application/jsonl")
	if err != nil {
		return nil, nil, err
	}
	cw, err := NewContextWriter(&extScanWriter{w: ew, seen: w.extSeen, max: w.limits.MaxRecordBytes}, scope, src)
	if err != nil {
		return nil, nil, err
	}
	return cw, ew, nil
}

// extScanWriter records extension ids used on context lines and refuses a
// line over the record limit a reader would apply. Each Write from
// json.Encoder is one complete line.
type extScanWriter struct {
	w    io.Writer
	seen map[string]bool
	max  int
}

// ErrRecordTooLarge reports a context line over Limits.MaxRecordBytes.
var ErrRecordTooLarge = errors.New("paf: context line exceeds the record limit")

func (e *extScanWriter) Write(p []byte) (int, error) {
	if len(p) > e.max {
		return 0, ErrRecordTooLarge
	}
	if bytes.Contains(p, []byte(`"extensions"`)) {
		if err := collectExtensionIDs(bytes.TrimSpace(p), e.seen); err != nil {
			return 0, err
		}
	}
	return e.w.Write(p)
}

// Close writes the manifest and signatures and finishes the archive. It
// returns the exact manifest bytes.
func (w *Writer) Close(producer Producer, keys ...SigningKey) ([]byte, error) {
	if w.closed {
		return nil, errors.New("paf: writer is closed")
	}
	if w.open {
		return nil, errors.New("paf: an entry is still open")
	}
	if len(w.agents) == 0 {
		return nil, errors.New("paf: archive has no agent")
	}
	if w.kind == KindGroup && !w.seen[GroupPath] {
		return nil, errors.New("paf: group PAF has no group.json")
	}
	if w.kind != KindGroup && w.seen[GroupPath] {
		return nil, errors.New("paf: group.json in an agent PAF")
	}
	if w.kind == KindBundle && len(w.workflows) == 0 {
		return nil, errors.New("paf: bundle PAF has no workflow")
	}
	for _, key := range w.agents {
		if w.seen[AgentPath(key, "context/transcript.jsonl")] && !w.seen[AgentPath(key, "context/losses.json")] {
			return nil, fmt.Errorf("paf: agent %q has context but no losses.json", key)
		}
	}
	for _, p := range w.natives {
		if !w.seen[p] {
			return nil, fmt.Errorf("paf: native blob %q was declared but not written", p)
		}
	}
	for _, m := range w.members {
		if !slices.Contains(w.agents, m) {
			return nil, fmt.Errorf("paf: group member %q is not an agent in the archive", m)
		}
	}
	for p := range w.seen {
		if key, _, ok := splitAgentPath(p); ok && !slices.Contains(w.agents, key) {
			return nil, fmt.Errorf("paf: entry %q belongs to an agent that was not added", p)
		}
	}
	if w.manExt != nil {
		raw, err := marshalJSON(map[string]any{"extensions": w.manExt})
		if err != nil {
			return nil, err
		}
		if err := collectExtensionIDs(raw, w.extSeen); err != nil {
			return nil, err
		}
	}
	for id := range w.extSeen {
		if _, ok := w.extUsed[id]; !ok {
			return nil, fmt.Errorf("paf: extension %q is used but not declared", id)
		}
	}
	sort.Slice(w.entries, func(i, j int) bool { return w.entries[i].Path < w.entries[j].Path })
	m := Manifest{
		Format:             Format,
		PafVersion:         Version,
		Kind:               w.kind,
		CreatedAt:          w.created,
		Producer:           producer,
		Requires:           append([]string{}, w.requires...),
		ExtensionsRequired: w.extReq,
		Agents:             w.agents,
		Entries:            w.entries,
		Extensions:         w.manExt,
	}
	if len(w.workflows) > 0 {
		// An older reader would import the agents and silently lose the
		// workflows, so a file with an envelope says it needs a newer one.
		m.PafVersion, m.MinReaderVersion = EnvelopeVersion, EnvelopeVersion
		for i := range w.workflows {
			w.workflows[i].Digest = SectionDigest(w.entries, WorkflowPath(w.workflows[i].Key, ""))
		}
		m.Workflows = w.workflows
		for i := range w.links {
			l := &w.links[i]
			if l.Type == LinkPerformer {
				l.Digest = SectionDigest(w.entries, AgentPath(l.Agent, ""))
			} else {
				l.Digest = SectionDigest(w.entries, WorkflowPath(l.Workflow, ""))
			}
		}
		m.Links = w.links
	}
	if len(w.extUsed) > 0 {
		m.ExtensionsUsed = w.extUsed
	}
	raw, err := marshalJSON(m)
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > w.limits.MaxMetadataBytes {
		return nil, errors.New("paf: manifest exceeds the metadata limit")
	}
	zf, err := w.createZip(ManifestPath)
	if err != nil {
		return nil, err
	}
	if _, err := zf.Write(raw); err != nil {
		return nil, err
	}
	keyIDs := map[string]bool{}
	for _, k := range keys {
		if keyIDs[k.KeyID] {
			return nil, fmt.Errorf("paf: duplicate signing key id %q", k.KeyID)
		}
		keyIDs[k.KeyID] = true
		sig, err := Sign(raw, k)
		if err != nil {
			return nil, err
		}
		b, err := marshalJSON(sig)
		if err != nil {
			return nil, err
		}
		zf, err := w.createZip(SignaturesDir + sig.KeyID + ".json")
		if err != nil {
			return nil, err
		}
		if _, err := zf.Write(b); err != nil {
			return nil, err
		}
	}
	w.closed = true
	if err := w.zw.Close(); err != nil {
		return nil, err
	}
	return raw, nil
}
