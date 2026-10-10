// Package agentbundle defines the portable archive shared by local transfer
// and federation. Archive entries are data, never paths to extract onto a host.
package agentbundle

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/tofutools/tclaude/pkg/claude/common/configbundle"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"io"
	"strings"
	"time"
)

const Format = "tclaude-agent-bundle"
const MaxBytes = 2 << 30
const MaxManifestBytes = 1 << 20
const ManifestFile = "manifest.json"
const MailLedgerFile = "continuation/mail-deliveries.json"
const MaxMailLedgerBytes = 64 << 20
const HistoryFile = "history/transcript.jsonl"

type Group struct {
	Name string `json:"name"`
	Role string `json:"role,omitempty"`
}
type Permission struct {
	Slug   string          `json:"slug"`
	Effect string          `json:"effect"`
	Scope  json.RawMessage `json:"scope,omitempty"`
	Source string          `json:"source"`
}
type Paths struct {
	RepoURL  string `json:"repo_url,omitempty"`
	Cwd      string `json:"cwd,omitempty"`
	Worktree string `json:"worktree,omitempty"`
	Branch   string `json:"branch,omitempty"`
}

// Origin is advisory source-reported context, never receiver launch authority.
type Origin struct {
	Instance string `json:"instance,omitempty"`
	Agent    string `json:"agent,omitempty"`
	Trigger  string `json:"trigger,omitempty"`
	Model    string `json:"model,omitempty"`
	Commit   string `json:"commit,omitempty"`
	Dirty    *bool  `json:"dirty,omitempty"`
}
type Definition struct {
	Identity         *db.FederationIdentity `json:"identity,omitempty"`
	Origin           *Origin                `json:"origin,omitempty"`
	CarryPermissions bool                   `json:"carry_permissions,omitempty"`
	Name             string                 `json:"name"`
	Role             string                 `json:"role,omitempty"`
	Description      string                 `json:"description,omitempty"`
	Harness          string                 `json:"harness"`
	ProfileName      string                 `json:"profile_name,omitempty"`
	Profile          json.RawMessage        `json:"profile"`
	Groups           []Group                `json:"groups,omitempty"`
	StartupContext   string                 `json:"startup_context,omitempty"`
	InitialMessage   string                 `json:"initial_message,omitempty"`
	TaskURL          string                 `json:"task_url,omitempty"`
	TaskLabel        string                 `json:"task_label,omitempty"`
	Paths            Paths                  `json:"paths"`
	// Permission rows are advisory. Only explicit carry opt-in plus receiver
	// authorization can promote them to grants; ownership never travels.

	Permissions []Permission `json:"permissions,omitempty"`
}
type History struct {
	Format       string `json:"format"`
	SourceConvID string `json:"source_conv_id"`
	Bytes        int64  `json:"bytes"`
	SHA256       string `json:"sha256"`
}
type Finding struct {
	Kind      string   `json:"kind"`
	Count     int      `json:"count"`
	Locations []string `json:"locations"`
}
type Manifest struct {
	MailLedger     bool                       `json:"mail_ledger,omitempty"`
	Placeholders   []configbundle.Placeholder `json:"placeholders,omitempty"`
	Format         string                     `json:"format"`
	FormatVersion  int                        `json:"format_version"`
	CreatedAt      string                     `json:"created_at"`
	TclaudeVersion string                     `json:"tclaude_version"`
	Agent          Definition                 `json:"agent"`
	History        *History                   `json:"history,omitempty"`
	Findings       []Finding                  `json:"findings,omitempty"`
	Warnings       []string                   `json:"warnings,omitempty"`
}
type Bundle struct {
	MailDeliveries []db.FederationMailDelivery
	Manifest       Manifest
	Transcript     []byte
	// TranscriptPath is local-only; it is never an archive entry or a wire path.
	TranscriptPath  string
	MaxBytes        int64
	ownedTranscript bool
}

func (b *Bundle) Validate() error {
	m := b.Manifest
	if m.MailLedger && m.Agent.Identity == nil {
		return errors.New("mail ledger requires a stable continuation")
	}
	if len(b.MailDeliveries) > 500000 {
		return errors.New("mail delivery ledger exceeds 500000 entries")
	}
	if !m.MailLedger && len(b.MailDeliveries) != 0 {
		return errors.New("mail delivery ledger is not declared")
	}
	for _, r := range b.MailDeliveries {
		if r.Sender == "" || len(r.Sender) > 128 || r.Envelope == "" || len(r.Envelope) > 128 || r.ExpiresAt.IsZero() || r.ExpiresAt.After(time.Now().Add(9*24*time.Hour)) {
			return errors.New("invalid mail delivery ledger entry")
		}
	}
	if m.Format != Format {
		return fmt.Errorf("expected format %q", Format)
	}
	if m.FormatVersion != 1 {
		return fmt.Errorf("unsupported agent bundle format_version %d (supported: 1)", m.FormatVersion)
	}
	if strings.TrimSpace(m.Agent.Name) == "" || m.Agent.Harness == "" || !json.Valid(m.Agent.Profile) {
		return errors.New("agent name, harness and valid inline profile are required")
	}
	if len(m.Agent.Paths.RepoURL) > 4096 || strings.ContainsAny(m.Agent.Paths.RepoURL, "\x00\r\n") {
		return errors.New("invalid repository URL hint")
	}
	r, err := b.OpenHistory()
	if err != nil {
		return err
	}
	defer r.Close()
	sum := sha256.New()
	n, err := io.Copy(sum, io.LimitReader(r, b.Limit()+1))
	if err != nil {
		return err
	}
	if n > b.Limit() {
		return b.LimitError("history")
	}
	if m.History != nil {
		if m.History.Bytes != n || m.History.SHA256 != hex.EncodeToString(sum.Sum(nil)) {
			return errors.New("history length or checksum mismatch")
		}
		if n == 0 || m.History.SourceConvID == "" {
			return errors.New("history metadata is incomplete")
		}
	} else if n > 0 {
		return errors.New("history entry has no manifest declaration")
	}

	return nil
}
func (b *Bundle) SetHistory(format, source string, raw []byte) {
	sum := sha256.Sum256(raw)
	b.Transcript = raw
	b.Manifest.History = &History{format, source, int64(len(raw)), hex.EncodeToString(sum[:])}
}
func (b *Bundle) Encode() ([]byte, error) {
	var out bytes.Buffer
	if err := b.EncodeTo(&out); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}
func (b *Bundle) EncodeTo(out io.Writer) error {
	if err := b.Validate(); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(b.Manifest, "", "  ")
	if err != nil {
		return err
	}
	if len(raw) > MaxManifestBytes {
		return errors.New("manifest exceeds 1 MiB")
	}
	bounded := &limitWriter{Writer: out, left: b.Limit()}
	zw := zip.NewWriter(bounded)
	hdr := &zip.FileHeader{Name: ManifestFile, Method: zip.Deflate}
	hdr.SetMode(0600)
	w, err := zw.CreateHeader(hdr)
	if err != nil {
		return err
	}
	if _, err = w.Write(raw); err != nil {
		return err
	}
	if b.Manifest.MailLedger {
		hdr = &zip.FileHeader{Name: MailLedgerFile, Method: zip.Deflate}
		hdr.SetMode(0600)
		w, err = zw.CreateHeader(hdr)
		if err != nil {
			return err
		}
		if err = json.NewEncoder(&limitWriter{Writer: w, left: MaxMailLedgerBytes}).Encode(b.MailDeliveries); err != nil {
			return err
		}
	}
	if b.Manifest.History != nil {
		hdr = &zip.FileHeader{Name: HistoryFile, Method: zip.Deflate}
		hdr.SetMode(0600)
		w, err = zw.CreateHeader(hdr)
		if err != nil {
			return err
		}
		r, err := b.OpenHistory()
		if err != nil {
			return err
		}
		_, err = io.Copy(w, r)
		closeErr := r.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
	}
	if err = zw.Close(); err != nil {
		return fmt.Errorf("write archive (federation.agent_transfer_max_bytes=%d): %w", b.Limit(), err)
	}
	return nil
}
func Decode(raw []byte) (*Bundle, error) {
	if len(raw) > MaxBytes {
		return nil, errors.New("archive exceeds federation.agent_transfer_max_bytes=2147483648; raise that node setting")
	}
	zr, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		return nil, fmt.Errorf("invalid agent bundle ZIP: %w", err)
	}
	if len(zr.File) > 2 {
		return nil, errors.New("unexpected archive entries")
	}
	b := &Bundle{}
	seen := map[string]bool{}
	for _, f := range zr.File {
		limit := MaxBytes
		switch f.Name {
		case ManifestFile:
			limit = MaxManifestBytes
		case HistoryFile:
		case MailLedgerFile:
			limit = MaxMailLedgerBytes
		default:
			return nil, fmt.Errorf("unexpected archive entry %q", f.Name)
		}
		if seen[f.Name] || !f.Mode().IsRegular() {
			return nil, fmt.Errorf("duplicate or non-regular entry %q", f.Name)
		}
		seen[f.Name] = true
		if f.UncompressedSize64 > uint64(limit) {
			return nil, fmt.Errorf("entry %q is too large", f.Name)
		}
		r, err := f.Open()
		if err != nil {
			return nil, err
		}
		data, readErr := io.ReadAll(io.LimitReader(r, int64(limit)+1))
		closeErr := r.Close()
		if readErr != nil {
			return nil, readErr
		}
		if closeErr != nil {
			return nil, closeErr
		}
		if len(data) > limit {
			return nil, errors.New("uncompressed entry is too large")
		}
		if f.Name == ManifestFile {
			if err := json.Unmarshal(data, &b.Manifest); err != nil {
				return nil, err
			}
		} else if f.Name == MailLedgerFile {
			if err := json.Unmarshal(data, &b.MailDeliveries); err != nil {
				return nil, err
			}
		} else {
			b.Transcript = data
		}
	}
	if !seen[ManifestFile] {
		return nil, errors.New("archive has no manifest.json")
	}
	if b.Manifest.MailLedger != seen[MailLedgerFile] {
		return nil, errors.New("mail ledger declaration does not match archive")
	}
	if b.Manifest.History != nil && !seen[HistoryFile] {
		return nil, errors.New("declared history entry is missing")
	}
	if err := b.Validate(); err != nil {
		return nil, err
	}
	return b, nil
}
