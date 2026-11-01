// Package legacy converts tclaude's pre-PAF carriers into PAF, so callers
// can read one model whatever arrived. It lives outside package paf because
// the legacy types pull in tclaude's database layer.
package legacy

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/agentbundle"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/groupexport"
	"github.com/tofutools/tclaude/pkg/claude/harness/transcript"
	"github.com/tofutools/tclaude/pkg/paf"
	"github.com/tofutools/tclaude/pkg/paf/privacy"
)

// SourceAgentBundleV1 names agent bundle v1 input.
const SourceAgentBundleV1 = "agent-bundle-v1"

// MailLedgerPath is where the tclaude extension keeps a continuation's mail
// delivery ledger.
const MailLedgerPath = paf.ExtDir + paf.ExtTclaude + "/mail-deliveries.json"

// TclaudeExtension is the declaration tclaude writes for its extension.
func TclaudeExtension(systemVersion string) paf.ExtensionDecl {
	return paf.ExtensionDecl{
		Version:       "0.1",
		System:        "tclaude",
		SystemVersion: systemVersion,
		Spec:          "https://github.com/tofutools/tclaude/blob/main/docs/paf.md#the-tclaude-extension",
		Schema:        paf.SchemaBaseURI + paf.SchemaExtTclaude,
		Copy:          "unsafe",
	}
}

// AgentExt is the tclaude extension on agent.json.
type AgentExt struct {
	SpawnProfile *SpawnProfile `json:"spawn_profile,omitempty"`
	Permissions  []Permission  `json:"permissions,omitempty"`
	Transfer     *Transfer     `json:"transfer,omitempty"`
}

type SpawnProfile struct {
	Name    string          `json:"name,omitempty"`
	Profile json.RawMessage `json:"profile"`
}

type Permission struct {
	Slug   string          `json:"slug"`
	Effect string          `json:"effect,omitempty"`
	Scope  json.RawMessage `json:"scope,omitempty"`
	Source string          `json:"source,omitempty"`
}

// Transfer is the continuation state an agent bundle carries for move and
// teleport: a stable federation identity, the permission carry opt-in, the
// origin trigger and source warnings.
type Transfer struct {
	Identity         *db.FederationIdentity `json:"identity,omitempty"`
	CarryPermissions bool                   `json:"carry_permissions,omitempty"`
	Trigger          string                 `json:"trigger,omitempty"`
	Warnings         []string               `json:"warnings,omitempty"`
}

// PlaceholderExt is the tclaude extension on an environment placeholder:
// which configuration field the placeholder stands in.
type PlaceholderExt struct {
	Item  string `json:"item,omitempty"`
	Field string `json:"field,omitempty"`
}

// ConvertedFrom is the tclaude extension on the manifest root of a PAF
// converted from a legacy carrier.
type ConvertedFrom struct {
	Format         string `json:"format"`
	FormatVersion  int    `json:"format_version"`
	TclaudeVersion string `json:"tclaude_version,omitempty"`
	CreatedAt      string `json:"created_at,omitempty"`
}

var keyRE = regexp.MustCompile(`[^a-z0-9-]+`)

// AgentKey derives an archive agent key from a name.
func AgentKey(name string) string {
	k := strings.Trim(keyRE.ReplaceAllString(strings.ToLower(name), "-"), "-")
	if len(k) > 64 {
		k = strings.Trim(k[:64], "-")
	}
	if k == "" {
		return "agent"
	}
	return k
}

// FromAgentBundle writes an agent bundle v1 as an agent PAF. Everything the
// bundle carries is kept: base fields where PAF has them, the tclaude
// extension for the rest, and the native history byte for byte. A v1 bundle
// has no neutral context; a cross-harness landing decodes the native blob.
func FromAgentBundle(b *agentbundle.Bundle, out io.Writer, producer paf.Producer, keys ...paf.SigningKey) error {
	return WriteAgentBundle(b, out, WriteOptions{Producer: producer, Keys: keys})
}

// WriteOptions configures WriteAgentBundle.
type WriteOptions struct {
	Producer paf.Producer
	Keys     []paf.SigningKey
	// Context, when set, is written as the agent's neutral context beside
	// the native blob, with its loss report.
	Context *transcript.Transcript
	// MaxRecordBytes bounds one context line; 0 uses the PAF default.
	MaxRecordBytes int
	// OmitNative leaves the native history out, so the conversation travels
	// only as the neutral Context, which must then be set.
	OmitNative bool
	// AllowedFindings records that a human explicitly allowed the scan
	// findings.
	AllowedFindings bool
	// Snapshot marks the PAF as a named snapshot (agent.json snapshot).
	Snapshot *paf.Snapshot
	// Lineage is appended to agent.json lineage.
	Lineage []paf.LineageStep
	// Handoff, when set, is written as context/handoff.md: the agent's own
	// summary of its earlier work, for a landing that has to cut history.
	Handoff string
	// EarlierLosses is the conversation's stored loss report, or nil. It goes
	// into the new report's prior, and stands as losses.json when there is no
	// Context to report on.
	EarlierLosses *paf.Losses
	// LossCountsOnly writes losses.json without free text, for an archive
	// leaving this node.
	LossCountsOnly bool
	// AddContent, when set, adds to the archive before it closes, such as
	// the workflows an agent carries. It gets the agent keys.
	AddContent func(w *paf.Writer, agents []string) error
	// ContentFindings are the scan findings of what AddContent adds, recorded
	// in privacy.json with the agents' own.
	ContentFindings []agentbundle.Finding
}

func writeLosses(w *paf.Writer, key string, l paf.Losses, opts WriteOptions) error {
	if opts.LossCountsOnly {
		l = paf.CountsOnly(l)
	}
	return w.WriteJSON(paf.AgentPath(key, "context/losses.json"), l)
}

// WriteAgentBundle writes an agent bundle as an agent PAF, optionally with
// a neutral context.
func WriteAgentBundle(b *agentbundle.Bundle, out io.Writer, opts WriteOptions) error {
	m := b.Manifest
	w := paf.NewWriter(out, paf.KindAgent, paf.Limits{MaxArchiveBytes: ConvertedLimit(b.Limit()), MaxRecordBytes: opts.MaxRecordBytes})
	if t, err := time.Parse(time.RFC3339, m.CreatedAt); err == nil {
		w.SetCreatedAt(t)
	}
	if err := w.DeclareExtension(paf.ExtTclaude, TclaudeExtension(opts.Producer.Version), false); err != nil {
		return err
	}
	key := AgentKey(m.Agent.Name)
	if err := writeAgent(w, key, b, opts); err != nil {
		return err
	}
	if err := writeBundlePrivacy(w, append(slices.Clone(m.Findings), opts.ContentFindings...), opts.AllowedFindings); err != nil {
		return err
	}
	if opts.AddContent != nil {
		if err := opts.AddContent(w, []string{key}); err != nil {
			return err
		}
	}
	var root paf.Extensions
	if err := root.Set(paf.ExtTclaude, map[string]any{"converted_from": ConvertedFrom{Format: m.Format, FormatVersion: m.FormatVersion, TclaudeVersion: m.TclaudeVersion, CreatedAt: m.CreatedAt}}); err != nil {
		return err
	}
	w.SetManifestExtensions(root)
	_, err := w.Close(opts.Producer, opts.Keys...)
	return err
}

// BundleAgent is one agent of a bundle PAF, with how to write it.
type BundleAgent struct {
	Bundle  *agentbundle.Bundle
	Options WriteOptions
}

// WriteBundlePAF writes a bundle PAF: agents with no group, which opts.AddContent
// ties to the workflows it adds. AddContent gets the keys in the order of agents.
func WriteBundlePAF(agents []BundleAgent, out io.Writer, opts WriteOptions, limit int64) error {
	if len(agents) == 0 {
		return errors.New("legacy: a bundle PAF needs an agent")
	}
	w := paf.NewWriter(out, paf.KindBundle, paf.Limits{MaxArchiveBytes: ConvertedLimit(limit), MaxRecordBytes: opts.MaxRecordBytes})
	if err := w.DeclareExtension(paf.ExtTclaude, TclaudeExtension(opts.Producer.Version), false); err != nil {
		return err
	}
	used := map[string]bool{}
	keys := make([]string, 0, len(agents))
	findings := slices.Clone(opts.ContentFindings)
	for _, a := range agents {
		key := uniqueKey(AgentKey(a.Bundle.Manifest.Agent.Name), used)
		keys = append(keys, key)
		if err := writeAgent(w, key, a.Bundle, a.Options); err != nil {
			return err
		}
		findings = append(findings, a.Bundle.Manifest.Findings...)
	}
	if err := writeBundlePrivacy(w, findings, opts.AllowedFindings); err != nil {
		return err
	}
	if opts.AddContent != nil {
		if err := opts.AddContent(w, keys); err != nil {
			return err
		}
	}
	_, err := w.Close(opts.Producer, opts.Keys...)
	return err
}

func writeBundlePrivacy(w *paf.Writer, findings []agentbundle.Finding, allowed bool) error {
	if len(findings) == 0 {
		return nil
	}
	pr := paf.Privacy{Scan: &paf.Scan{Scanner: privacy.Scanner, Findings: []paf.Finding{}, Allowed: allowed}}
	for _, f := range findings {
		pr.Scan.Findings = append(pr.Scan.Findings, paf.Finding{Kind: f.Kind, Count: f.Count, Locations: f.Locations})
	}
	return w.WriteJSON(paf.PrivacyPath, pr)
}

// writeAgent writes one agent into w under key.
func writeAgent(w *paf.Writer, key string, b *agentbundle.Bundle, opts WriteOptions) error {
	m := b.Manifest
	d := m.Agent

	ag := paf.Agent{Name: d.Name, Definition: paf.Definition{
		Role:           d.Role,
		Description:    d.Description,
		StartupContext: d.StartupContext,
		InitialMessage: d.InitialMessage,
	}}
	ag.Snapshot = opts.Snapshot
	ag.Lineage = append(ag.Lineage, opts.Lineage...)
	if d.TaskURL != "" || d.TaskLabel != "" {
		ag.Definition.Task = &paf.Task{URL: d.TaskURL, Label: d.TaskLabel}
	}
	for _, g := range d.Groups {
		ag.Groups = append(ag.Groups, paf.GroupRef{Name: g.Name, Role: g.Role})
	}
	env := paf.Environment{Harness: paf.Harness{Name: d.Harness}}
	ws := &paf.Workspace{RepoURL: d.Paths.RepoURL, Cwd: d.Paths.Cwd, Worktree: d.Paths.Worktree, Branch: d.Paths.Branch}
	ext := AgentExt{SpawnProfile: &SpawnProfile{Name: d.ProfileName, Profile: d.Profile}}
	if o := d.Origin; o != nil {
		ag.ID = o.Agent
		if o.Instance != "" {
			ag.Instance = &paf.Instance{ID: o.Instance}
		}
		if o.Model != "" {
			env.Model = &paf.Model{ID: o.Model}
		}
		ws.Commit, ws.Dirty = o.Commit, o.Dirty
	}
	if ws.RepoURL != "" || ws.Cwd != "" || ws.Worktree != "" || ws.Branch != "" || ws.Commit != "" || ws.Dirty != nil {
		env.Workspace = ws
	}
	for _, p := range d.Permissions {
		ext.Permissions = append(ext.Permissions, Permission{Slug: p.Slug, Effect: p.Effect, Scope: p.Scope, Source: p.Source})
	}
	tr := Transfer{Identity: d.Identity, CarryPermissions: d.CarryPermissions, Warnings: m.Warnings}
	if d.Origin != nil {
		tr.Trigger = d.Origin.Trigger
	}
	if tr.Identity != nil || tr.CarryPermissions || tr.Trigger != "" || len(tr.Warnings) > 0 {
		ext.Transfer = &tr
	}
	if err := ag.Extensions.Set(paf.ExtTclaude, ext); err != nil {
		return err
	}
	for _, p := range m.Placeholders {
		ph := paf.Placeholder{Name: p.Name, Original: p.Original}
		if err := ph.Extensions.Set(paf.ExtTclaude, PlaceholderExt{Item: p.Item, Field: p.Field}); err != nil {
			return err
		}
		env.Placeholders = append(env.Placeholders, ph)
	}

	if opts.OmitNative && m.History != nil && opts.Context == nil {
		return errors.New("legacy: leaving out the native history needs a neutral context to carry instead")
	}
	if h := m.History; h != nil && !opts.OmitNative {
		if !paf.ValidEntryPath(h.Format) || strings.Contains(h.Format, "/") {
			return fmt.Errorf("legacy: invalid history format %q", h.Format)
		}
		ag.Native = &paf.Native{Format: h.Format, Path: paf.AgentPath(key, "native/"+h.Format+"/history.jsonl"), Harness: d.Harness, SourceConvID: h.SourceConvID}
	}
	if ag.Conversation == nil && m.History != nil {
		ag.Conversation = &paf.Conversation{ID: m.History.SourceConvID, Harness: d.Harness}
	}
	if err := writeManagedInstructions(w, &ag, &env, b.ManagedInstructions); err != nil {
		return err
	}
	if err := w.AddAgent(key, ag, env); err != nil {
		return err
	}
	if ag.Native != nil {
		hw, err := w.Create(ag.Native.Path, "application/jsonl")
		if err != nil {
			return err
		}
		r, err := b.OpenHistory()
		if err != nil {
			return err
		}
		_, err = io.Copy(hw, r)
		closeErr := r.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
		if err := hw.Close(); err != nil {
			return err
		}
	}
	if m.MailLedger {
		if err := w.WriteJSON(MailLedgerPath, b.MailDeliveries); err != nil {
			return err
		}
	}
	if opts.Handoff != "" {
		if err := w.WriteFile(paf.AgentPath(key, "context/handoff.md"), "text/markdown", []byte(opts.Handoff)); err != nil {
			return err
		}
	}
	if opts.Context != nil {
		if err := paf.WriteTranscript(w, key, paf.ScopeEffective, opts.Context); err != nil {
			return err
		}
		if err := writeLosses(w, key, paf.ChainLosses(opts.EarlierLosses, paf.LossesFromReport(opts.Context.Losses)), opts); err != nil {
			return err
		}
	} else if opts.EarlierLosses != nil {
		if err := writeLosses(w, key, *opts.EarlierLosses, opts); err != nil {
			return err
		}
	}
	return nil
}

// ConvertedLimit is the archive limit for a PAF converted from a bundle
// whose own limit is bundleLimit: the history may use the whole bundle
// limit, so the PAF gets headroom for the mail ledger and metadata.
func ConvertedLimit(bundleLimit int64) int64 {
	return bundleLimit + agentbundle.MaxMailLedgerBytes + 16<<20
}

// Opened is an archive opened by OpenFile.
type Opened struct {
	*paf.Archive
	// Source is "paf", SourceAgentBundleV1 or SourceGroupExport.
	Source string
	close  func() error
}

// Close releases temporary files a conversion created.
func (o *Opened) Close() error {
	if o.close == nil {
		return nil
	}
	return o.close()
}

// OpenFile opens a .paf, a legacy agent bundle v1 or a group export as a
// verified PAF. A legacy file is validated by its own decoder, converted into
// a private temporary PAF under dir, and opened from there.
func OpenFile(path, dir string, producer paf.Producer, opts paf.Options) (*Opened, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	keepOpen := false
	defer func() {
		if !keepOpen {
			f.Close()
		}
	}()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	format, err := manifestFormat(f, st.Size())
	if err != nil {
		return nil, err
	}
	switch format {
	case paf.Format:
		a, err := paf.Open(f, st.Size(), opts)
		if err != nil {
			return nil, err
		}
		// The archive reads entries lazily from f.
		keepOpen = true
		return &Opened{Archive: a, Source: paf.Format, close: f.Close}, nil
	case agentbundle.Format:
		limit := opts.Limits.MaxArchiveBytes
		if limit <= 0 {
			limit = paf.DefaultLimits().MaxArchiveBytes
		}
		b, err := agentbundle.DecodeFile(f, limit, dir)
		if err != nil {
			return nil, err
		}
		defer func() { _ = b.Close() }()
		return convertedOpen(dir, opts, ConvertedLimit(limit), SourceAgentBundleV1, func(w io.Writer) error {
			return FromAgentBundle(b, w, producer)
		})
	case "":
		// A group export's manifest has no format marker, only its
		// format_version.
		limit := opts.Limits.MaxArchiveBytes
		if limit <= 0 {
			limit = paf.DefaultLimits().MaxArchiveBytes
		}
		if st.Size() > limit {
			return nil, fmt.Errorf("legacy: archive is %d bytes, over the %d byte limit", st.Size(), limit)
		}
		raw := make([]byte, st.Size())
		if _, err := f.ReadAt(raw, 0); err != nil {
			return nil, err
		}
		// The zip reader refuses an entry that inflates past its declared
		// size, so bounding the declared total bounds the decompression.
		if err := checkUncompressed(raw, limit); err != nil {
			return nil, err
		}
		exp, err := groupexport.Unmarshal(raw)
		if err != nil {
			return nil, err
		}
		return convertedOpen(dir, opts, ConvertedLimit(limit), SourceGroupExport, func(w io.Writer) error {
			return WriteGroupExport(exp, w, GroupWriteOptions{Producer: producer, MaxArchiveBytes: ConvertedLimit(limit)})
		})
	}
	return nil, fmt.Errorf("legacy: unrecognised archive format %q", format)
}

// convertedOpen writes a converted PAF into a private temporary file under
// dir and opens it from there.
func convertedOpen(dir string, opts paf.Options, limit int64, source string, write func(io.Writer) error) (*Opened, error) {
	tmp, err := os.CreateTemp(dir, ".paf-legacy-")
	if err != nil {
		return nil, err
	}
	cleanup := func() error {
		tmp.Close()
		return os.Remove(tmp.Name())
	}
	if err := write(tmp); err != nil {
		_ = cleanup()
		return nil, err
	}
	size, err := tmp.Seek(0, io.SeekCurrent)
	if err != nil {
		_ = cleanup()
		return nil, err
	}
	convOpts := opts
	convOpts.Limits.MaxArchiveBytes = limit
	a, err := paf.Open(tmp, size, convOpts)
	if err != nil {
		_ = cleanup()
		return nil, err
	}
	return &Opened{Archive: a, Source: source, close: cleanup}, nil
}

// manifestFormat reads manifest.json's format marker.
func manifestFormat(r io.ReaderAt, size int64) (string, error) {
	zr, err := zip.NewReader(r, size)
	if err != nil {
		return "", fmt.Errorf("not a ZIP archive: %w", err)
	}
	for _, f := range zr.File {
		if f.Name != paf.ManifestPath {
			continue
		}
		if f.UncompressedSize64 > uint64(paf.DefaultLimits().MaxMetadataBytes) {
			// Too large for a PAF or agent bundle manifest; a group
			// export's can be this large, and it has no format marker.
			return "", nil
		}
		rc, err := f.Open()
		if err != nil {
			return "", err
		}
		defer rc.Close()
		var head struct {
			Format string `json:"format"`
		}
		if err := json.NewDecoder(io.LimitReader(rc, paf.DefaultLimits().MaxMetadataBytes)).Decode(&head); err != nil {
			return "", fmt.Errorf("manifest.json: %w", err)
		}
		return head.Format, nil
	}
	return "", errors.New("archive has no manifest.json")
}

// checkUncompressed refuses a ZIP archive whose entries declare more than
// limit bytes in total.
func checkUncompressed(raw []byte, limit int64) error {
	zr, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		return fmt.Errorf("not a ZIP archive: %w", err)
	}
	var total uint64
	for _, f := range zr.File {
		total += f.UncompressedSize64
		if total > uint64(limit) {
			return fmt.Errorf("legacy: archive expands past the %d byte limit", limit)
		}
	}
	return nil
}
