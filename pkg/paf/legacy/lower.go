package legacy

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/agentbundle"
	"github.com/tofutools/tclaude/pkg/claude/common/configbundle"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/paf"
)

// WarningContextOnly is the bundle warning for a PAF whose history arrived
// only in neutral form, which this release imports as configuration only.
const WarningContextOnly = "conversation history arrived only in harness-neutral PAF form; it is not imported by this tclaude release"

// Info describes what DecodeBundleFile read.
type Info struct {
	// Source is paf.Format or SourceAgentBundleV1.
	Source string
	// Signatures are the PAF's checked signatures (none for v1).
	Signatures []paf.SignatureResult
	// HasContext reports a neutral context in the PAF.
	HasContext bool
}

// DecodeLimits bounds DecodeBundleFile.
type DecodeLimits struct {
	// MaxBytes is the archive limit (federation.agent_transfer_max_bytes).
	MaxBytes int64
	// MaxRecordBytes bounds one PAF context line. Callers importing from
	// peers pass a tighter bound than the native history record limit,
	// because validating a context line costs several times its size.
	MaxRecordBytes int
}

// DecodeBundleFile reads an agent bundle v1 or an agent PAF from f into the
// agent bundle model the import, share, move and teleport paths use. A PAF
// is fully verified, refused when it requires features this release does
// not understand, and lowered losslessly; its native history is extracted
// to a private temporary file under dir that Bundle.Close removes.
func DecodeBundleFile(f *os.File, lim DecodeLimits, dir string) (*agentbundle.Bundle, Info, error) {
	st, err := f.Stat()
	if err != nil {
		return nil, Info{}, err
	}
	format, err := manifestFormat(f, st.Size())
	if err != nil {
		return nil, Info{}, fmt.Errorf("invalid agent bundle: %w", err)
	}
	switch format {
	case agentbundle.Format:
		b, err := agentbundle.DecodeFile(f, lim.MaxBytes, dir)
		return b, Info{Source: SourceAgentBundleV1}, err
	case paf.Format:
		a, err := paf.Open(f, st.Size(), paf.Options{Purpose: paf.PurposeImport, Limits: paf.Limits{MaxArchiveBytes: ConvertedLimit(lim.MaxBytes), MaxRecordBytes: lim.MaxRecordBytes}})
		if err != nil {
			return nil, Info{}, err
		}
		b, err := ToAgentBundle(a, lim.MaxBytes, dir)
		if err != nil {
			return nil, Info{}, err
		}
		key := a.Manifest.Agents[0]
		return b, Info{Source: paf.Format, Signatures: a.Signatures, HasContext: a.HasContext(key, paf.ScopeEffective)}, nil
	}
	return nil, Info{}, fmt.Errorf("invalid agent bundle: unrecognised archive format %q", format)
}

// DecodeBundleBytes is DecodeBundleFile for an in-memory archive.
func DecodeBundleBytes(raw []byte, lim DecodeLimits, dir string) (*agentbundle.Bundle, Info, error) {
	if format, err := manifestFormat(bytes.NewReader(raw), int64(len(raw))); err == nil && format == agentbundle.Format {
		b, err := agentbundle.Decode(raw)
		return b, Info{Source: SourceAgentBundleV1}, err
	}
	tmp, err := os.CreateTemp(dir, ".paf-decode-")
	if err != nil {
		return nil, Info{}, err
	}
	defer func() { _ = tmp.Close(); _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(raw); err != nil {
		return nil, Info{}, err
	}
	return DecodeBundleFile(tmp, lim, dir)
}

// ToAgentBundle lowers an agent PAF to the agent bundle v1 model. Fields
// the tclaude extension carries come back exactly; a PAF from another
// producer lowers to its base fields. A PAF whose history is only neutral
// context lowers to configuration only, with a warning.
func ToAgentBundle(a *paf.Archive, limit int64, dir string) (*agentbundle.Bundle, error) {
	if a.Manifest.Kind != paf.KindAgent || len(a.Manifest.Agents) != 1 {
		return nil, errors.New("an agent bundle import needs an agent PAF with exactly one agent")
	}
	key := a.Manifest.Agents[0]
	ag, _ := a.Agent(key)
	env, _ := a.Environment(key)
	var ext AgentExt
	if a.UnderstandsExtension(paf.ExtTclaude) {
		if _, err := ag.Extensions.Decode(paf.ExtTclaude, &ext); err != nil {
			return nil, fmt.Errorf("tclaude extension: %w", err)
		}
	}
	var conv struct {
		ConvertedFrom *ConvertedFrom `json:"converted_from"`
	}
	if a.UnderstandsExtension(paf.ExtTclaude) {
		if _, err := a.Manifest.Extensions.Decode(paf.ExtTclaude, &conv); err != nil {
			return nil, fmt.Errorf("tclaude extension: %w", err)
		}
	}

	b := &agentbundle.Bundle{MaxBytes: limit}
	m := &b.Manifest
	m.Format, m.FormatVersion = agentbundle.Format, 1
	m.CreatedAt = a.Manifest.CreatedAt.UTC().Format(time.RFC3339)
	m.TclaudeVersion = a.Manifest.Producer.Version
	if c := conv.ConvertedFrom; c != nil {
		m.TclaudeVersion = c.TclaudeVersion
		if c.CreatedAt != "" {
			m.CreatedAt = c.CreatedAt
		}
	}

	d := &m.Agent
	d.Name = ag.Name
	d.Role = ag.Definition.Role
	d.Description = ag.Definition.Description
	d.StartupContext = ag.Definition.StartupContext
	d.InitialMessage = ag.Definition.InitialMessage
	if t := ag.Definition.Task; t != nil {
		d.TaskURL, d.TaskLabel = t.URL, t.Label
	}
	d.Harness = env.Harness.Name
	d.Profile = json.RawMessage(`{}`)
	if sp := ext.SpawnProfile; sp != nil {
		d.ProfileName = sp.Name
		if len(sp.Profile) > 0 {
			d.Profile = sp.Profile
		}
	}
	for _, g := range ag.Groups {
		d.Groups = append(d.Groups, agentbundle.Group{Name: g.Name, Role: g.Role})
	}
	for _, p := range ext.Permissions {
		d.Permissions = append(d.Permissions, agentbundle.Permission{Slug: p.Slug, Effect: p.Effect, Scope: p.Scope, Source: p.Source})
	}
	if ws := env.Workspace; ws != nil {
		d.Paths = agentbundle.Paths{RepoURL: ws.RepoURL, Cwd: ws.Cwd, Worktree: ws.Worktree, Branch: ws.Branch}
	}
	origin := agentbundle.Origin{Agent: ag.ID}
	if ag.Instance != nil {
		origin.Instance = ag.Instance.ID
	}
	if env.Model != nil {
		origin.Model = env.Model.ID
	}
	if ws := env.Workspace; ws != nil {
		origin.Commit, origin.Dirty = ws.Commit, ws.Dirty
	}
	if tr := ext.Transfer; tr != nil {
		d.Identity = tr.Identity
		d.CarryPermissions = tr.CarryPermissions
		origin.Trigger = tr.Trigger
		m.Warnings = append(m.Warnings, tr.Warnings...)
	}
	if origin != (agentbundle.Origin{}) {
		d.Origin = &origin
	}
	for _, p := range env.Placeholders {
		var px PlaceholderExt
		if a.UnderstandsExtension(paf.ExtTclaude) {
			if _, err := p.Extensions.Decode(paf.ExtTclaude, &px); err != nil {
				return nil, fmt.Errorf("tclaude extension: %w", err)
			}
		}
		m.Placeholders = append(m.Placeholders, configbundle.Placeholder{Name: p.Name, Item: px.Item, Field: px.Field, Original: p.Original})
	}
	if pr := a.Privacy(); pr != nil && pr.Scan != nil {
		for _, f := range pr.Scan.Findings {
			m.Findings = append(m.Findings, agentbundle.Finding{Kind: f.Kind, Count: f.Count, Locations: f.Locations})
		}
	}
	if a.HasEntry(MailLedgerPath) && a.UnderstandsExtension(paf.ExtTclaude) {
		raw, err := a.ReadEntry(MailLedgerPath, agentbundle.MaxMailLedgerBytes)
		if err != nil {
			return nil, err
		}
		var ledger []db.FederationMailDelivery
		if err := json.Unmarshal(raw, &ledger); err != nil {
			return nil, fmt.Errorf("mail ledger: %w", err)
		}
		b.MailDeliveries = ledger
		m.MailLedger = true
	}

	ok := false
	defer func() {
		if !ok {
			_ = b.Close()
		}
	}()
	switch {
	case ag.Native != nil:
		source := ag.Native.SourceConvID
		if source == "" && ag.Conversation != nil {
			source = ag.Conversation.ID
		}
		if source == "" {
			return nil, errors.New("native history has no source conversation id")
		}
		if err := extractNative(a, b, ag.Native, source, dir); err != nil {
			return nil, err
		}
	case a.HasContext(key, paf.ScopeEffective):
		m.Warnings = append(m.Warnings, WarningContextOnly)
	}
	if err := b.Validate(); err != nil {
		return nil, err
	}
	ok = true
	return b, nil
}

func extractNative(a *paf.Archive, b *agentbundle.Bundle, n *paf.Native, source, dir string) error {
	rc, err := a.OpenEntry(n.Path)
	if err != nil {
		return err
	}
	defer rc.Close()
	tmp, err := os.CreateTemp(dir, ".agent-history-")
	if err != nil {
		return err
	}
	_, err = io.Copy(tmp, io.LimitReader(rc, b.Limit()+1))
	closeErr := tmp.Close()
	if err == nil {
		err = closeErr
	}
	if err == nil {
		err = b.SetHistoryFile(n.Format, source, tmp.Name(), true)
	}
	if err != nil {
		_ = os.Remove(tmp.Name())
	}
	return err
}
