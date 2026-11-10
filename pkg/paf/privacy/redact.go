package privacy

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/harness/transcript"
	"github.com/tofutools/tclaude/pkg/paf"
)

// Redaction kinds recorded in privacy.json.
const (
	RedactMaskSpans       = "mask_spans"
	RedactStubToolResults = "stub_tool_results"
	RedactDropMedia       = "drop_media"
	RedactDropReasoning   = "drop_reasoning"
	RedactDropNative      = "drop_native"
	RedactPaths           = "paths_to_placeholders"
	RedactStripIdentity   = "strip_identity"
	// RedactDropUnknown records content a redaction cannot rewrite safely
	// and dropped instead: extension data this tool does not understand,
	// unknown context lines, binary agent files.
	RedactDropUnknown = "drop_unredactable"
)

// LossRedacted is the loss item kind for a redaction.
const LossRedacted = "redacted"

// Stand-ins for removed content.
const (
	stubToolResult = "[tool result removed by redaction]"
	stubMedia      = "[media removed by redaction]"
	stubIdentity   = "[REDACTED:identity]"
)

// NativeDecoder decodes a native history blob into the neutral transcript.
type NativeDecoder func(n paf.Native, r io.Reader) (*transcript.Transcript, error)

// RedactOptions selects the redactions. Every redaction also drops the
// native blob, which is never edited.
type RedactOptions struct {
	// MaskFlagged replaces what the scan flags with [REDACTED:<kind>].
	MaskFlagged bool
	// StubToolResults replaces tool results older than the last KeepTurns
	// human turns with a stub.
	StubToolResults bool
	KeepTurns       int
	// DropMedia replaces images and documents with a stub.
	DropMedia bool
	// DropReasoning removes thinking blocks and provider reasoning state.
	DropReasoning bool
	// DropNative only drops the native blob.
	DropNative bool
	// PathsToPlaceholders replaces the home directory, the working
	// directory, recorded placeholder paths and the user name with
	// ${NAME} placeholders, and records the names without their values.
	PathsToPlaceholders bool
	// StripIdentity removes what identifies the agent, its node and its
	// conversations: agent and instance ids, the agent and conversation
	// titles, conversation ids (agent.json, transcript headers, lineage
	// sources), group memberships and media path hints. Extension data is
	// dropped, since it can carry identifiers this tool cannot tell
	// apart. It applies to one-agent PAFs only.
	StripIdentity bool

	// Native decodes a native blob for an agent with no context file, so
	// its history survives the drop. Without it such history is dropped.
	Native NativeDecoder
	// Limits bounds the output archive; zero uses the defaults.
	Limits paf.Limits
	// MaxAttachmentBytes bounds one attachment read for rewriting.
	MaxAttachmentBytes int64
}

func (o RedactOptions) any() bool {
	return o.MaskFlagged || o.StubToolResults || o.DropMedia || o.DropReasoning || o.DropNative || o.PathsToPlaceholders || o.StripIdentity
}

// RedactReport says what a redaction changed. It holds counts, kinds and
// names, never content.
type RedactReport struct {
	Redactions []paf.Redaction `json:"redactions"`
	// Placeholders are the placeholder names paths were replaced with.
	Placeholders []string `json:"placeholders,omitempty"`
	// Findings is the scan of the redacted output.
	Findings []paf.Finding `json:"findings"`
}

// Redact writes a redacted copy of a to out and reports what changed. With
// io.Discard as out it is a preview. The copy is a new file: the original
// signatures and consent records do not carry over, the scan is redone on
// the output, and the applied redactions are recorded in privacy.json and
// each agent's loss report.
func Redact(a *paf.Archive, out io.Writer, opts RedactOptions, producer paf.Producer, keys ...paf.SigningKey) (*RedactReport, error) {
	if !opts.any() {
		return nil, errors.New("no redaction selected")
	}
	if opts.MaxAttachmentBytes <= 0 {
		opts.MaxAttachmentBytes = 64 << 20
	}
	if opts.StripIdentity && (a.Manifest.Kind != paf.KindAgent || len(a.Manifest.Agents) != 1) {
		return nil, errors.New("strip_identity applies to a PAF of one agent")
	}
	kept, err := keptWorkflows(a, opts)
	if err != nil {
		return nil, err
	}
	kind := a.Manifest.Kind
	if kind == paf.KindBundle && len(kept) == 0 {
		if len(a.Manifest.Agents) != 1 {
			return nil, errors.New("this redaction drops the file's workflows, and several agents need a group or a workflow to travel together; redact without it, or fix the workflow")
		}
		kind = paf.KindAgent
	}
	r := &redactor{
		a:       a,
		opts:    opts,
		w:       paf.NewWriter(out, kind, opts.Limits),
		keptWF:  kept,
		scan:    NewCollector(),
		counts:  map[string]int{},
		masked:  map[string]int{},
		dropped: map[string]bool{},
		refs:    map[string]string{},
		keepExt: a.UnderstandsExtension(paf.ExtTclaude) && !opts.StripIdentity,
	}
	if opts.PathsToPlaceholders {
		r.paths = pathPlaceholders(a)
	}
	if opts.StripIdentity {
		r.idents = identifiers(a)
	}
	if len(kept) > 0 {
		keys = paf.RewriteKeys(a, keys)
	}
	return r.run(producer, keys)
}

type redactor struct {
	a    *paf.Archive
	opts RedactOptions
	w    *paf.Writer
	scan *Collector

	// counts is per redaction kind; masked is per credential kind.
	counts  map[string]int
	masked  map[string]int
	dropped map[string]bool
	// agentCounts is per redaction kind for the agent being written.
	agentCounts map[string]int
	// refs maps source attachment references to rewritten ones.
	refs  map[string]string
	paths []placeholder
	// idents are the identifiers strip_identity removes wherever they
	// appear in text, longest first.
	idents  []string
	keepExt bool
	// keptWF are the workflows that stay, by key (see keptWorkflows).
	keptWF map[string]bool
}

// workflowRecordLimit bounds one line of a workflow entry: a workflow file
// allows at most this much per entry.
const workflowRecordLimit = 1 << 20

// keptWorkflows says which workflows a redaction leaves in the copy. Templates
// are addressed by the hash of their content and subprocess steps pin those
// hashes, so a workflow cannot be rewritten: it stays byte for byte or it goes.
// Redactions of the conversation do not touch it. Masking keeps a workflow the
// scan finds nothing in. Paths and identity may be anywhere in a template, so
// those redactions always drop it.
func keptWorkflows(a *paf.Archive, opts RedactOptions) (map[string]bool, error) {
	kept := map[string]bool{}
	if opts.PathsToPlaceholders || opts.StripIdentity {
		return kept, nil
	}
	for _, wf := range a.Manifest.Workflows {
		if opts.MaskFlagged {
			c := NewCollector()
			for _, e := range workflowEntries(a, wf.Key) {
				if err := c.entry(a, e, workflowRecordLimit); err != nil {
					return nil, err
				}
			}
			if len(c.Findings()) > 0 {
				continue
			}
		}
		kept[wf.Key] = true
	}
	return kept, nil
}

func workflowEntries(a *paf.Archive, key string) []paf.Entry {
	var out []paf.Entry
	for _, e := range a.Manifest.Entries {
		if strings.HasPrefix(e.Path, paf.WorkflowPath(key, "")) {
			out = append(out, e)
		}
	}
	return out
}

// workflows copies the kept workflows and their links, scanned like any other
// content, and records the rest as dropped.
func (r *redactor) workflows() error {
	for _, wf := range r.a.Manifest.Workflows {
		if !r.keptWF[wf.Key] {
			r.drop("carried workflow")
			continue
		}
		if err := r.w.AddWorkflow(wf.Key, wf.Root, wf.Requires); err != nil {
			return err
		}
		for _, e := range workflowEntries(r.a, wf.Key) {
			data, err := r.a.ReadEntry(e.Path, r.opts.MaxAttachmentBytes)
			if err != nil {
				return err
			}
			if err := r.scan.entry(r.a, e, workflowRecordLimit); err != nil {
				return err
			}
			if err := r.w.WriteFile(e.Path, e.MediaType, data); err != nil {
				return err
			}
		}
	}
	for _, l := range r.a.Manifest.Links {
		if r.keptWF[l.Workflow] {
			r.w.AddLink(l)
		}
	}
	return nil
}

func (r *redactor) run(producer paf.Producer, keys []paf.SigningKey) (*RedactReport, error) {
	m := r.a.Manifest
	for _, f := range m.Requires {
		r.w.Require(f)
	}
	if d, ok := m.ExtensionsUsed[paf.ExtTclaude]; ok && r.keepExt {
		if err := r.w.DeclareExtension(paf.ExtTclaude, d, slices.Contains(m.ExtensionsRequired, paf.ExtTclaude)); err != nil {
			return nil, err
		}
	}
	for id := range m.ExtensionsUsed {
		if id != paf.ExtTclaude || !r.keepExt {
			r.drop("extension " + id)
		}
	}
	for _, key := range m.Agents {
		if err := r.agent(key); err != nil {
			return nil, fmt.Errorf("agent %s: %w", key, err)
		}
	}
	if g := r.a.Group(); g != nil {
		var out paf.Group
		if err := r.doc(g, &out); err != nil {
			return nil, err
		}
		if err := r.writeJSON(paf.GroupPath, out); err != nil {
			return nil, err
		}
	}
	for _, e := range m.Entries {
		if strings.HasPrefix(e.Path, paf.ExtDir) {
			if err := r.file(e); err != nil {
				return nil, err
			}
		}
	}
	if err := r.workflows(); err != nil {
		return nil, err
	}
	if len(m.Extensions) > 0 {
		ext := r.exts(m.Extensions)
		r.w.SetManifestExtensions(ext)
		if raw, err := json.Marshal(ext); err == nil && len(ext) > 0 {
			r.scan.JSON(raw, "manifest.json extensions")
		}
	}
	// The source's earlier redaction records and privacy extensions are
	// free text from the file: they are rewritten like any other content
	// and scanned, before the report is taken, so a credential cannot ride
	// along in privacy.json past a scan that reports none.
	var carried []paf.Redaction
	var carriedExt paf.Extensions
	old := r.a.Privacy()
	if old != nil {
		for _, red := range old.Redactions {
			red.Detail = r.text(red.Detail, false)
			carried = append(carried, red)
		}
		carriedExt = r.exts(old.Extensions)
		if raw, err := json.Marshal(map[string]any{"redactions": carried, "extensions": carriedExt}); err == nil {
			r.scan.JSON(raw, paf.PrivacyPath)
		}
	}
	rep := r.report()
	pr := paf.Privacy{Scan: &paf.Scan{Scanner: Scanner, Findings: rep.Findings}}
	now := time.Now().UTC().Truncate(time.Second)
	pr.Scan.At = &now
	if old != nil {
		pr.Sensitivity, pr.Audience, pr.Retention = old.Sensitivity, old.Audience, old.Retention
		pr.Redactions = append(pr.Redactions, carried...)
		pr.Extensions = carriedExt
	}
	pr.Redactions = append(pr.Redactions, rep.Redactions...)
	if err := r.w.WriteJSON(paf.PrivacyPath, pr); err != nil {
		return nil, err
	}
	if _, err := r.w.Close(producer, keys...); err != nil {
		return nil, err
	}
	return rep, nil
}

func (r *redactor) report() *RedactReport {
	rep := &RedactReport{Redactions: []paf.Redaction{}, Findings: r.scan.Findings()}
	add := func(kind string, count int, detail string) {
		if count > 0 {
			rep.Redactions = append(rep.Redactions, paf.Redaction{Kind: kind, Count: count, Detail: detail})
		}
	}
	add(RedactMaskSpans, r.counts[RedactMaskSpans], countsDetail(r.masked))
	add(RedactStubToolResults, r.counts[RedactStubToolResults], fmt.Sprintf("kept tool results of the last %d human turns", r.opts.KeepTurns))
	add(RedactDropMedia, r.counts[RedactDropMedia], "")
	add(RedactDropReasoning, r.counts[RedactDropReasoning], "")
	add(RedactDropNative, r.counts[RedactDropNative], "")
	var names []string
	for _, p := range r.paths {
		names = append(names, p.name)
	}
	rep.Placeholders = names
	add(RedactPaths, r.counts[RedactPaths], strings.Join(names, ", "))
	add(RedactStripIdentity, r.counts[RedactStripIdentity], "agent name, agent, instance and conversation ids and titles, groups, repository and task, media paths")
	var dropped []string
	for d := range r.dropped {
		dropped = append(dropped, d)
	}
	slices.Sort(dropped)
	add(RedactDropUnknown, len(dropped), truncate(strings.Join(dropped, ", "), 1000))
	return rep
}

func countsDetail(m map[string]int) string {
	var parts []string
	for k, n := range m {
		parts = append(parts, fmt.Sprintf("%s %d", k, n))
	}
	slices.Sort(parts)
	return strings.Join(parts, ", ")
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func (r *redactor) drop(what string) { r.dropped[what] = true }

func (r *redactor) count(kind string, n int) {
	r.counts[kind] += n
	if r.agentCounts != nil {
		r.agentCounts[kind] += n
	}
}

// --- text and JSON rewriting ---

// text applies the text redactions. structural fields (ids, opaque blobs)
// are masked for credentials but not for high-entropy tokens, as the scan
// treats them.
func (r *redactor) text(s string, structural bool) string {
	if s == "" {
		return s
	}
	if r.opts.MaskFlagged {
		before := sumCounts(r.masked)
		s = Mask(s, !structural, r.masked)
		r.count(RedactMaskSpans, sumCounts(r.masked)-before)
	}
	if len(r.paths) > 0 {
		var n int
		s, n = replacePaths(s, r.paths)
		r.count(RedactPaths, n)
	}
	for _, id := range r.idents {
		if n := strings.Count(s, id); n > 0 {
			s = strings.ReplaceAll(s, id, stubIdentity)
			r.count(RedactStripIdentity, n)
		}
	}
	return s
}

// minIdentLen keeps short values (a harness name, "main") from being
// treated as identifiers.
const minIdentLen = 6

// identifiers collects the ids and names strip_identity removes: the
// agent, instance and conversation ids, the instance name and the
// conversation ids its lineage and contexts record.
func identifiers(a *paf.Archive) []string {
	seen := map[string]bool{}
	add := func(v string) {
		if len(v) >= minIdentLen {
			seen[v] = true
		}
	}
	for _, key := range a.Manifest.Agents {
		ag, ok := a.Agent(key)
		if !ok {
			continue
		}
		add(ag.ID)
		add(ag.Title)
		// The name is usually the conversation title; a name that is
		// just the role stays readable in the text.
		if ag.Name != ag.Definition.Role {
			add(ag.Name)
		}
		for _, g := range ag.Groups {
			add(g.Name)
		}
		if ag.Instance != nil {
			add(ag.Instance.ID)
			add(ag.Instance.Name)
		}
		if ag.Conversation != nil {
			add(ag.Conversation.ID)
			add(ag.Conversation.Title)
		}
		for _, l := range ag.Lineage {
			if l.From != nil {
				add(l.From.Instance)
				add(l.From.Agent)
				add(l.From.Conversation)
			}
		}
		if ag.Native != nil {
			add(ag.Native.SourceConvID)
		}
		for _, scope := range []string{paf.ScopeEffective, paf.ScopeFull} {
			if !a.HasContext(key, scope) {
				continue
			}
			if cr, closer, err := a.Context(key, scope); err == nil {
				if cr.Header.Source != nil {
					add(cr.Header.Source.ConvID)
					add(cr.Header.Source.Title)
				}
				_ = closer.Close()
			}
		}
	}
	out := make([]string, 0, len(seen))
	for v := range seen {
		out = append(out, v)
	}
	slices.SortFunc(out, func(x, y string) int {
		if len(x) != len(y) {
			return len(y) - len(x)
		}
		return strings.Compare(x, y)
	})
	return out
}

func sumCounts(m map[string]int) int {
	n := 0
	for _, v := range m {
		n += v
	}
	return n
}

// doc rewrites a PAF metadata object through JSON: every string is
// redacted and extension data is kept only for extensions this tool
// understands.
func (r *redactor) doc(in, out any) error {
	raw, err := json.Marshal(in)
	if err != nil {
		return err
	}
	v, err := decodeJSON(raw)
	if err != nil {
		return err
	}
	raw, err = json.Marshal(r.walk(v, "", true))
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, out)
}

func decodeJSON(raw []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	return v, nil
}

// walk redacts the strings of a decoded JSON value. pafObject marks PAF
// metadata, where an "extensions" member holds extension data.
func (r *redactor) walk(v any, key string, pafObject bool) any {
	switch x := v.(type) {
	case string:
		if pafObject && key == "agent" {
			// An agent key names agents/<key>/ and the manifest's agent
			// list; it is an identifier, kept as is.
			return x
		}
		return r.text(x, structuralKeys[key])
	case map[string]any:
		for k, child := range x {
			if pafObject && k == "extensions" {
				ext, ok := child.(map[string]any)
				if !ok {
					delete(x, k)
					continue
				}
				for id, data := range ext {
					if id == paf.ExtTclaude && r.keepExt {
						ext[id] = r.walk(data, "", false)
						continue
					}
					r.drop("extension " + id)
					delete(ext, id)
				}
				if len(ext) == 0 {
					delete(x, k)
				}
				continue
			}
			x[k] = r.walk(child, k, pafObject)
		}
		return x
	case []any:
		for i, child := range x {
			x[i] = r.walk(child, key, pafObject)
		}
		return x
	}
	return v
}

// exts filters and redacts an extensions map.
func (r *redactor) exts(e paf.Extensions) paf.Extensions {
	var out paf.Extensions
	for id, raw := range e {
		if id != paf.ExtTclaude || !r.keepExt {
			r.drop("extension " + id)
			continue
		}
		v, err := decodeJSON(raw)
		if err != nil {
			r.drop("extension " + id)
			continue
		}
		b, err := json.Marshal(r.walk(v, "", false))
		if err != nil {
			r.drop("extension " + id)
			continue
		}
		if out == nil {
			out = paf.Extensions{}
		}
		out[id] = b
	}
	return out
}

// rawJSON redacts a JSON value that is not PAF metadata (tool input).
func (r *redactor) rawJSON(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return raw
	}
	v, err := decodeJSON(raw)
	if err != nil {
		return json.RawMessage(strconvQuote(r.text(string(raw), false)))
	}
	b, err := json.Marshal(r.walk(v, "", false))
	if err != nil {
		return nil
	}
	return b
}

func strconvQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// --- output with scanning ---

func (r *redactor) writeJSON(path string, v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	r.scan.JSON(raw, path)
	return r.w.WriteJSON(path, v)
}

func (r *redactor) writeFile(path, mediaType string, data []byte) error {
	if err := r.w.WriteFile(path, mediaType, data); err != nil {
		return err
	}
	return r.scanBytes(path, mediaType, data)
}

func (r *redactor) scanBytes(path, mediaType string, data []byte) error {
	switch {
	case strings.HasSuffix(path, ".jsonl") || mediaType == "application/jsonl" || mediaType == "application/x-ndjson":
		return r.scan.JSONLines(bytes.NewReader(data), path, max(len(data)+1, 64<<10))
	case strings.HasSuffix(path, ".json") || mediaType == "application/json":
		r.scan.JSON(data, path)
	case looksText(data):
		return r.scan.chunks(bytes.NewReader(data), path)
	}
	return nil
}

// file rewrites an entry that is neither agent metadata nor context: an
// extension file or an extra file in an agent directory. JSON is redacted
// field by field and text as text; anything else is dropped.
func (r *redactor) file(e paf.Entry) error {
	if rest, ok := strings.CutPrefix(e.Path, paf.ExtDir); ok {
		id, _, _ := strings.Cut(rest, "/")
		if id != paf.ExtTclaude || !r.keepExt {
			r.drop("extension " + id)
			return nil
		}
	}
	data, err := r.a.ReadEntry(e.Path, r.opts.MaxAttachmentBytes)
	if err != nil {
		r.drop(e.Path)
		return nil
	}
	switch {
	case strings.HasSuffix(e.Path, ".jsonl") || e.MediaType == "application/jsonl" || e.MediaType == "application/x-ndjson":
		var out bytes.Buffer
		for _, line := range bytes.Split(data, []byte("\n")) {
			if len(bytes.TrimSpace(line)) == 0 {
				continue
			}
			v, err := decodeJSON(line)
			if err != nil {
				r.drop(e.Path)
				return nil
			}
			b, err := json.Marshal(r.walk(v, "", false))
			if err != nil {
				return err
			}
			out.Write(b)
			out.WriteByte('\n')
		}
		return r.writeFile(r.outPath(e.Path), e.MediaType, out.Bytes())
	case strings.HasSuffix(e.Path, ".json") || e.MediaType == "application/json":
		v, err := decodeJSON(data)
		if err != nil {
			r.drop(e.Path)
			return nil
		}
		b, err := json.Marshal(r.walk(v, "", false))
		if err != nil {
			return err
		}
		return r.writeFile(r.outPath(e.Path), e.MediaType, b)
	case looksText(data):
		return r.writeFile(r.outPath(e.Path), e.MediaType, []byte(r.text(string(data), false)))
	}
	r.drop(e.Path)
	return nil
}

// --- agents ---

func (r *redactor) agent(key string) error {
	srcAg, ok := r.a.Agent(key)
	if !ok {
		return errors.New("missing agent.json")
	}
	srcEnv, _ := r.a.Environment(key)
	if srcEnv == nil {
		srcEnv = &paf.Environment{}
	}
	r.agentCounts = map[string]int{}
	defer func() { r.agentCounts = nil }()

	src := *srcAg
	native := src.Native
	src.Native = nil
	if native != nil {
		r.count(RedactDropNative, 1)
	}
	var ag paf.Agent
	if err := r.doc(src, &ag); err != nil {
		return err
	}
	var env paf.Environment
	if err := r.doc(srcEnv, &env); err != nil {
		return err
	}
	if r.opts.StripIdentity {
		r.stripAgent(&ag, &env)
	}
	r.placeholders(&env)
	if err := r.w.AddAgent(r.outKey(key), ag, env); err != nil {
		return err
	}
	r.scanAgentJSON(key, ag, env)

	contexts, effective := 0, false
	for _, scope := range []string{paf.ScopeEffective, paf.ScopeFull} {
		if !r.a.HasContext(key, scope) {
			continue
		}
		contexts++
		effective = effective || scope == paf.ScopeEffective
		if err := r.context(r.a, key, scope); err != nil {
			return err
		}
	}
	srcLosses, _ := r.a.Losses(key)
	if contexts == 0 && native != nil {
		t, err := r.decodeNative(*native)
		if err != nil {
			r.drop(native.Path + " (no context to carry the history: " + err.Error() + ")")
		} else {
			tmp, err := contextArchive(key, t)
			if err != nil {
				return err
			}
			if err := r.context(tmp, key, paf.ScopeEffective); err != nil {
				return err
			}
			l := paf.ChainLosses(srcLosses, paf.LossesFromReport(t.Losses))
			srcLosses, effective = &l, true
		}
	}
	// A report with no context to go with it describes a conversion the file
	// still records, so it stays too.
	if effective || srcLosses != nil {
		if err := r.losses(key, srcLosses); err != nil {
			return err
		}
	}
	prefix := paf.AgentPath(key, "")
	for _, e := range r.a.Manifest.Entries {
		rel, ok := strings.CutPrefix(e.Path, prefix)
		if !ok {
			continue
		}
		switch {
		case rel == "agent.json" || rel == "environment.json",
			rel == "context/transcript.jsonl" || rel == "context/full.jsonl" || rel == "context/losses.json",
			strings.HasPrefix(rel, "native/"):
			continue
		}
		if err := r.file(e); err != nil {
			return err
		}
	}
	return nil
}

// strippedName is the agent's name and key in a PAF with its identity
// stripped: tclaude names an agent after its conversation title.
const strippedName = "agent"

// outKey is the agent key an agent is written under.
func (r *redactor) outKey(key string) string {
	if r.opts.StripIdentity {
		return strippedName
	}
	return key
}

// outPath maps an agent entry's path to its output agent key.
func (r *redactor) outPath(path string) string {
	if !r.opts.StripIdentity {
		return path
	}
	for _, key := range r.a.Manifest.Agents {
		if rest, ok := strings.CutPrefix(path, paf.AgentPath(key, "")); ok {
			return paf.AgentPath(strippedName, rest)
		}
	}
	return path
}

// stripAgent removes the agent's identifying fields: its ids, its name and
// titles (tclaude names an agent after its conversation's title), its
// node, groups, lineage sources, and the repository and task it worked on.
func (r *redactor) stripAgent(ag *paf.Agent, env *paf.Environment) {
	n := 0
	blank := func(s *string) {
		if *s != "" {
			*s = ""
			n++
		}
	}
	blank(&ag.ID)
	blank(&ag.Title)
	if ag.Name != strippedName {
		ag.Name = strippedName
		n++
	}
	if ag.Definition.Task != nil {
		ag.Definition.Task = nil
		n++
	}
	if ws := env.Workspace; ws != nil {
		blank(&ws.RepoURL)
		blank(&ws.Branch)
		blank(&ws.Commit)
		blank(&ws.Worktree)
	}
	if ag.Instance != nil {
		ag.Instance = nil
		n++
	}
	if ag.Conversation != nil {
		blank(&ag.Conversation.ID)
		blank(&ag.Conversation.Title)
		if ag.Conversation.Harness == "" {
			ag.Conversation = nil
		}
	}
	if len(ag.Groups) > 0 {
		n += len(ag.Groups)
		ag.Groups = nil
	}
	for i := range ag.Lineage {
		if ag.Lineage[i].From != nil {
			ag.Lineage[i].From = nil
			n++
		}
	}
	r.count(RedactStripIdentity, n)
}

func (r *redactor) scanAgentJSON(key string, ag paf.Agent, env paf.Environment) {
	if raw, err := json.Marshal(ag); err == nil {
		r.scan.JSON(raw, paf.AgentPath(r.outKey(key), "agent.json"))
	}
	if raw, err := json.Marshal(env); err == nil {
		r.scan.JSON(raw, paf.AgentPath(r.outKey(key), "environment.json"))
	}
}

func (r *redactor) decodeNative(n paf.Native) (*transcript.Transcript, error) {
	if r.opts.Native == nil {
		return nil, errors.New("no decoder")
	}
	rc, err := r.a.OpenEntry(n.Path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rc.Close() }()
	return r.opts.Native(n, rc)
}

// contextArchive writes a decoded transcript into a scratch archive, so it
// is redacted through the same path as a stored context.
func contextArchive(key string, t *transcript.Transcript) (*paf.Archive, error) {
	var buf bytes.Buffer
	w := paf.NewWriter(&buf, paf.KindAgent, paf.Limits{})
	if err := w.AddAgent(key, paf.Agent{Name: key}, paf.Environment{Harness: paf.Harness{Name: t.Source.Harness}}); err != nil {
		return nil, err
	}
	if err := paf.WriteTranscript(w, key, paf.ScopeEffective, t); err != nil {
		return nil, err
	}
	if err := w.WriteJSON(paf.AgentPath(key, "context/losses.json"), paf.LossesFromReport(t.Losses)); err != nil {
		return nil, err
	}
	if _, err := w.Close(paf.Producer{Name: "tclaude"}); err != nil {
		return nil, err
	}
	return paf.Open(bytes.NewReader(buf.Bytes()), int64(buf.Len()), paf.Options{Purpose: paf.PurposeInspect})
}

// losses writes losses.json: the source report plus one redacted item per
// redaction applied to this agent's content.
func (r *redactor) losses(key string, src *paf.Losses) error {
	l := paf.Losses{Items: []paf.LossItem{}}
	if src != nil {
		if err := r.doc(src, &l); err != nil {
			return err
		}
		if l.Items == nil {
			l.Items = []paf.LossItem{}
		}
	}
	if l.Source == "" {
		if env, ok := r.a.Environment(key); ok {
			l.Source = env.Harness.Name
		}
	}
	for _, k := range []struct{ kind, severity string }{
		{RedactMaskSpans, "warn"}, {RedactStubToolResults, "warn"}, {RedactDropMedia, "warn"},
		{RedactDropReasoning, "info"}, {RedactPaths, "info"},
	} {
		if n := r.agentCounts[k.kind]; n > 0 {
			l.Items = append(l.Items, paf.LossItem{Kind: LossRedacted, Severity: k.severity, Count: n, Detail: k.kind})
		}
	}
	return r.writeJSON(paf.AgentPath(r.outKey(key), "context/losses.json"), l)
}

// --- context ---

func (r *redactor) context(a *paf.Archive, key, scope string) error {
	cr, closer, err := a.Context(key, scope)
	if err != nil {
		return err
	}
	defer func() { _ = closer.Close() }()
	var lines []paf.Line
	for {
		l, err := cr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		lines = append(lines, l)
	}
	// Tool results before the cutoff entry are stubbed: the cutoff is the
	// KeepTurns-th human turn from the end.
	cutoff := 0
	if r.opts.StubToolResults {
		cutoff = len(lines)
		seen := 0
		for i := len(lines) - 1; i >= 0 && seen < r.opts.KeepTurns; i-- {
			if lines[i].Type == paf.LineEntry && lines[i].Origin == "human" && lines[i].Role == "user" {
				seen++
				cutoff = i
			}
		}
		if r.opts.KeepTurns > 0 && seen < r.opts.KeepTurns {
			// Fewer human turns than kept: nothing is old enough.
			cutoff = 0
		}
	}
	h := *cr.Header.Source
	h.Cwd = r.text(h.Cwd, false)
	h.Title = r.text(h.Title, false)
	h.ConvID = r.text(h.ConvID, true)
	if r.opts.StripIdentity && (h.ConvID != "" || h.Title != "") {
		h.ConvID, h.Title = "", ""
		r.count(RedactStripIdentity, 1)
	}
	h.Extensions = r.exts(h.Extensions)

	var buf bytes.Buffer
	cw, err := paf.NewContextWriter(&buf, scope, h)
	if err != nil {
		return err
	}
	for i, l := range lines {
		switch l.Type {
		case paf.LineEntry:
			if !r.entry(a, &l, i < cutoff) {
				continue
			}
		case paf.LineBoundary:
		default:
			r.drop("context line type " + l.Type)
			continue
		}
		l.Extensions = r.exts(l.Extensions)
		l.Raw = nil
		if err := cw.Write(l); err != nil {
			return err
		}
	}
	name := "transcript.jsonl"
	if scope == paf.ScopeFull {
		name = "full.jsonl"
	}
	return r.writeFile(paf.AgentPath(r.outKey(key), "context/"+name), "application/jsonl", buf.Bytes())
}

// entry redacts one entry line in place and reports whether it still has
// content.
func (r *redactor) entry(a *paf.Archive, l *paf.Line, old bool) bool {
	l.Author = r.text(l.Author, false)
	l.ID = r.text(l.ID, true)
	var blocks []paf.Block
	for _, b := range l.Blocks {
		b.Extensions = r.exts(b.Extensions)
		switch b.Kind {
		case paf.BlockThinking:
			if r.opts.DropReasoning {
				r.count(RedactDropReasoning, 1)
				continue
			}
			text := r.text(b.Text, false)
			if text != b.Text && b.Opaque != nil {
				// A reasoning signature covers the text it was minted
				// for; over rewritten text it is only a liability.
				b.Opaque = nil
				r.count(RedactDropReasoning, 1)
			}
			b.Text = text
		case paf.BlockToolResult:
			if old {
				b.Text, b.Media = stubToolResult, nil
				r.count(RedactStubToolResults, 1)
			} else {
				b.Text = r.text(b.Text, false)
			}
		case paf.BlockMedia:
			if r.opts.DropMedia {
				r.count(RedactDropMedia, len(b.Media))
				b = paf.Block{Kind: paf.BlockText, Text: stubMedia}
			}
		default:
			b.Text = r.text(b.Text, false)
		}
		if b.Opaque != nil && r.opts.DropReasoning {
			b.Opaque = nil
			r.count(RedactDropReasoning, 1)
		}
		b.CallID = r.text(b.CallID, true)
		b.Input = r.rawJSON(b.Input)
		b.Media = r.media(a, b.Media)
		if b.Kind == paf.BlockMedia && len(b.Media) == 0 {
			continue
		}
		if (b.Kind == paf.BlockText || b.Kind == paf.BlockSummary) && b.Text == "" {
			continue
		}
		blocks = append(blocks, b)
	}
	l.Blocks = blocks
	return len(blocks) > 0
}

func (r *redactor) media(a *paf.Archive, ms []paf.Media) []paf.Media {
	if len(ms) == 0 {
		return nil
	}
	if r.opts.DropMedia {
		r.count(RedactDropMedia, len(ms))
		return nil
	}
	var out []paf.Media
	for _, m := range ms {
		m.Extensions = r.exts(m.Extensions)
		m.Name = r.text(m.Name, false)
		m.Path = r.text(m.Path, false)
		if r.opts.StripIdentity && m.Path != "" {
			m.Path = ""
			r.count(RedactStripIdentity, 1)
		}
		m.URL = r.text(m.URL, false)
		if m.Attachment != "" {
			ref, ok := r.attachment(a, m.Attachment, m.MediaType)
			if !ok {
				r.drop("attachment " + m.Attachment)
				continue
			}
			m.Attachment = ref
		}
		out = append(out, m)
	}
	return out
}

// attachment copies an attachment, redacting it when its bytes are text.
func (r *redactor) attachment(a *paf.Archive, ref, mediaType string) (string, bool) {
	key := fmt.Sprintf("%p:%s", a, ref)
	if out, ok := r.refs[key]; ok {
		return out, out != ""
	}
	data, err := a.Attachment(ref, r.opts.MaxAttachmentBytes)
	if err != nil {
		r.refs[key] = ""
		return "", false
	}
	if looksText(data) {
		data = []byte(r.text(string(data), false))
	}
	out, err := r.w.AddAttachment(data, mediaType)
	if err != nil {
		r.refs[key] = ""
		return "", false
	}
	_ = r.scanBytes(paf.AttachmentsDir+strings.TrimPrefix(out, paf.AttachmentPrefix), mediaType, data)
	r.refs[key] = out
	return out, true
}

// --- paths ---

type placeholder struct {
	name, original string
	word           bool
}

var homeRE = regexp.MustCompile(`^(/home/[^/]+|/Users/[^/]+|/root)(?:/|$)`)

// pathPlaceholders derives the path placeholders: recorded placeholder
// paths, each agent's working directory and worktree, the home directory
// above them and the user name it is named after. Longer values come first
// so a working directory wins over the home it is in.
func pathPlaceholders(a *paf.Archive) []placeholder {
	var ps []placeholder
	seen := map[string]bool{}
	names := map[string]bool{}
	add := func(name, original string, word bool) {
		if original == "" || seen[original] {
			return
		}
		if !word && (!filepath.IsAbs(original) || original == "/") {
			return
		}
		base := name
		for i := 2; names[name]; i++ {
			name = fmt.Sprintf("%s_%d", base, i)
		}
		seen[original], names[name] = true, true
		ps = append(ps, placeholder{name: name, original: original, word: word})
	}
	var dirs []string
	for _, key := range a.Manifest.Agents {
		env, ok := a.Environment(key)
		if !ok {
			continue
		}
		for _, p := range env.Placeholders {
			add(p.Name, p.Original, false)
		}
		if ws := env.Workspace; ws != nil {
			dirs = append(dirs, ws.Cwd, ws.Worktree)
		}
	}
	for _, key := range a.Manifest.Agents {
		for _, scope := range []string{paf.ScopeEffective, paf.ScopeFull} {
			if a.HasContext(key, scope) {
				if cr, c, err := a.Context(key, scope); err == nil {
					dirs = append(dirs, cr.Header.Source.Cwd)
					_ = c.Close()
				}
			}
		}
	}
	var homes []string
	for _, d := range dirs {
		d = filepath.Clean(d)
		if m := homeRE.FindStringSubmatch(d); m != nil && !slices.Contains(homes, m[1]) {
			homes = append(homes, m[1])
		}
	}
	for _, d := range dirs {
		if d != "" && !slices.Contains(homes, filepath.Clean(d)) {
			add("PROJECT", filepath.Clean(d), false)
		}
	}
	for _, h := range homes {
		add("HOME", h, false)
	}
	for _, h := range homes {
		// A short user name would match ordinary words.
		if u := filepath.Base(h); len(u) >= 4 && u != "root" {
			add("USER", u, true)
		}
	}
	slices.SortStableFunc(ps, func(x, y placeholder) int {
		if x.word != y.word {
			if x.word {
				return 1
			}
			return -1
		}
		return len(y.original) - len(x.original)
	})
	return ps
}

// replacePaths replaces placeholder values with ${NAME}. A path matches
// only as a whole path prefix; a user name only as a whole word.
func replacePaths(s string, ps []placeholder) (string, int) {
	n := 0
	for _, p := range ps {
		if !strings.Contains(s, p.original) {
			continue
		}
		var b strings.Builder
		rest := s
		for {
			i := strings.Index(rest, p.original)
			if i < 0 {
				b.WriteString(rest)
				break
			}
			end := i + len(p.original)
			okBefore := i == 0 || !wordByte(rest[i-1], p.word)
			okAfter := end == len(rest) || !wordByte(rest[end], p.word) || (!p.word && rest[end] == '/')
			if okBefore && okAfter {
				b.WriteString(rest[:i])
				b.WriteString("${" + p.name + "}")
				n++
			} else {
				b.WriteString(rest[:end])
			}
			rest = rest[end:]
		}
		s = b.String()
	}
	return s, n
}

func wordByte(c byte, word bool) bool {
	if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' {
		return true
	}
	return !word && (c == '.' || c == '-')
}

// placeholders records the path placeholders in an agent's environment:
// the derived names this agent's paths use, and recorded ones, all without
// their original values.
func (r *redactor) placeholders(env *paf.Environment) {
	if len(r.paths) == 0 {
		return
	}
	have := map[string]bool{}
	for i := range env.Placeholders {
		env.Placeholders[i].Original = ""
		have[env.Placeholders[i].Name] = true
	}
	raw, _ := json.Marshal(env)
	for _, p := range r.paths {
		if p.word || have[p.name] || !bytes.Contains(raw, []byte("${"+p.name+"}")) {
			continue
		}
		env.Placeholders = append(env.Placeholders, paf.Placeholder{Name: p.name, Description: "path removed by redaction"})
	}
}
