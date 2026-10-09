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
	"io"
	"strings"
)

const Format = "tclaude-agent-bundle"
const MaxBytes = 256 << 20
const MaxManifestBytes = 1 << 20
const ManifestFile = "manifest.json"
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
	Cwd      string `json:"cwd,omitempty"`
	Worktree string `json:"worktree,omitempty"`
	Branch   string `json:"branch,omitempty"`
}
type Definition struct {
	Name           string          `json:"name"`
	Role           string          `json:"role,omitempty"`
	Description    string          `json:"description,omitempty"`
	Harness        string          `json:"harness"`
	ProfileName    string          `json:"profile_name,omitempty"`
	Profile        json.RawMessage `json:"profile"`
	Groups         []Group         `json:"groups,omitempty"`
	StartupContext string          `json:"startup_context,omitempty"`
	InitialMessage string          `json:"initial_message,omitempty"`
	TaskURL        string          `json:"task_url,omitempty"`
	TaskLabel      string          `json:"task_label,omitempty"`
	Paths          Paths           `json:"paths"`
	// Permissions is documentation for deliberate local grants, never input to
	// spawn authorization. Source group roles/profiles are likewise advisory.
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
	Manifest   Manifest
	Transcript []byte
}

func (b *Bundle) Validate() error {
	m := b.Manifest
	if m.Format != Format {
		return fmt.Errorf("expected format %q", Format)
	}
	if m.FormatVersion != 1 {
		return fmt.Errorf("unsupported agent bundle format_version %d (supported: 1)", m.FormatVersion)
	}
	if strings.TrimSpace(m.Agent.Name) == "" || m.Agent.Harness == "" || !json.Valid(m.Agent.Profile) {
		return errors.New("agent name, harness and valid inline profile are required")
	}
	if len(b.Transcript) > MaxBytes {
		return errors.New("history exceeds 256 MiB")
	}
	if m.History != nil {
		sum := sha256.Sum256(b.Transcript)
		if m.History.Bytes != int64(len(b.Transcript)) || m.History.SHA256 != hex.EncodeToString(sum[:]) {
			return errors.New("history length or checksum mismatch")
		}
		if len(b.Transcript) == 0 || m.History.SourceConvID == "" {
			return errors.New("history metadata is incomplete")
		}
	} else if len(b.Transcript) > 0 {
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
	if err := b.Validate(); err != nil {
		return nil, err
	}
	raw, err := json.MarshalIndent(b.Manifest, "", "  ")
	if err != nil {
		return nil, err
	}
	if len(raw) > MaxManifestBytes {
		return nil, errors.New("manifest exceeds 1 MiB")
	}
	var out bytes.Buffer
	zw := zip.NewWriter(&out)
	for _, entry := range []struct {
		name string
		data []byte
	}{{ManifestFile, raw}, {HistoryFile, b.Transcript}} {
		if entry.name == HistoryFile && b.Manifest.History == nil {
			continue
		}
		hdr := &zip.FileHeader{Name: entry.name, Method: zip.Deflate}
		hdr.SetMode(0600)
		w, err := zw.CreateHeader(hdr)
		if err != nil {
			return nil, err
		}
		if _, err = w.Write(entry.data); err != nil {
			return nil, err
		}
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	if out.Len() > MaxBytes {
		return nil, errors.New("archive exceeds 256 MiB")
	}
	return out.Bytes(), nil
}
func Decode(raw []byte) (*Bundle, error) {
	if len(raw) > MaxBytes {
		return nil, errors.New("archive exceeds 256 MiB")
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
		} else {
			b.Transcript = data
		}
	}
	if !seen[ManifestFile] {
		return nil, errors.New("archive has no manifest.json")
	}
	if b.Manifest.History != nil && !seen[HistoryFile] {
		return nil, errors.New("declared history entry is missing")
	}
	if err := b.Validate(); err != nil {
		return nil, err
	}
	return b, nil
}
