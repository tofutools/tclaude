// Package bundletransfer holds the format and bounded private spool shared by
// config and agent offers. Payloads are data; remote names never become paths.
package bundletransfer

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/federation/proto"
)

const InlineLimit = 256 << 10
const DefaultTTL = 72 * time.Hour
const PendingLimit = 10
const PendingBytes = 64 << 20

// Type is local policy, never supplied by the peer. Another bundle type plugs
// in its own cap/admission slug and validator/importer at the daemon boundary.
type Type struct {
	Name          string
	MaxBytes      int64
	AdmissionSlug string
	GroupScoped   bool
	PendingLimit  int
	PendingBytes  int64
}

var Agent = Type{Name: "agent", MaxBytes: 2 << 30, AdmissionSlug: "agents.receive", GroupScoped: true, PendingLimit: 10, PendingBytes: 4 << 30}

var Config = Type{Name: "config", MaxBytes: 16 << 20, AdmissionSlug: "config.offer", PendingLimit: PendingLimit, PendingBytes: PendingBytes}

type MoveIntent struct {
	DirectIfAllowed bool   `json:"direct_if_allowed,omitempty"`
	Cwd             string `json:"cwd,omitempty"`
	Landing         string `json:"landing,omitempty"`
	SourceAgent     string `json:"source_agent"`
	SourceConv      string `json:"source_conv"`
}
type MoveConfirmation struct {
	Cwd         string    `json:"cwd,omitempty"`
	ObservedAt  time.Time `json:"observed_at"`
	Offer       string    `json:"offer"`
	SHA256      string    `json:"sha256"`
	SourceAgent string    `json:"source_agent"`
	SourceConv  string    `json:"source_conv"`
	TargetAgent string    `json:"target_agent"`
	TargetConv  string    `json:"target_conv"`
}

type Descriptor struct {
	Teleport  *TeleportIntent `json:"teleport,omitempty"`
	Move      *MoveIntent     `json:"move,omitempty"`
	ID        string          `json:"id"`
	Type      string          `json:"type"`
	Bytes     int64           `json:"bytes"`
	SHA256    string          `json:"sha256"`
	ExpiresAt time.Time       `json:"expires_at"`
	Summary   string          `json:"summary"`
	Group     string          `json:"group,omitempty"`
	Inline    []byte          `json:"inline,omitempty"`
}

func New(kind Type, raw []byte, summary string, expiry time.Time) Descriptor {
	sum := sha256.Sum256(raw)
	d := Descriptor{ID: proto.NewEnvelopeID(), Type: kind.Name, Bytes: int64(len(raw)), SHA256: hex.EncodeToString(sum[:]), ExpiresAt: expiry, Summary: summary}
	if len(raw) <= InlineLimit {
		d.Inline = raw
	}
	return d
}

// NewFile hashes a disk-backed archive without loading it into memory.
// The file is rewound for the caller's subsequent spool write.
func NewFile(kind Type, f *os.File, summary string, expiry time.Time) (Descriptor, error) {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return Descriptor{}, err
	}
	digest := sha256.New()
	n, err := io.Copy(digest, io.LimitReader(f, kind.MaxBytes+1))
	if err != nil {
		return Descriptor{}, err
	}
	if n > kind.MaxBytes {
		return Descriptor{}, fmt.Errorf("bundle exceeds federation.agent_transfer_max_bytes=%d; raise that node setting", kind.MaxBytes)
	}
	d := Descriptor{ID: proto.NewEnvelopeID(), Type: kind.Name, Bytes: n, SHA256: hex.EncodeToString(digest.Sum(nil)), ExpiresAt: expiry, Summary: summary}
	if _, err = f.Seek(0, io.SeekStart); err != nil {
		return Descriptor{}, err
	}
	if n <= InlineLimit {
		d.Inline, err = io.ReadAll(f)
		if err != nil {
			return Descriptor{}, err
		}
		_, err = f.Seek(0, io.SeekStart)
	}
	return d, err
}
func (d Descriptor) VerifyReader(r io.Reader) error {
	digest := sha256.New()
	n, err := io.Copy(digest, io.LimitReader(r, d.Bytes+1))
	if err != nil {
		return err
	}
	if n != d.Bytes || hex.EncodeToString(digest.Sum(nil)) != d.SHA256 {
		return errors.New("bundle length or SHA-256 mismatch")
	}
	return nil
}

func (d Descriptor) Validate(kind Type, now time.Time) error {
	if d.Teleport != nil {
		if kind.Name != Agent.Name {
			return errors.New("teleport requires an agent bundle")
		}
		if err := d.Teleport.Validate(); err != nil {
			return err
		}
	}
	if d.Move != nil && (kind.Name != Agent.Name || !proto.ValidAgentRef(d.Move.SourceAgent) || len(d.Move.SourceConv) != 36) {
		return errors.New("invalid agent move identity")
	}
	if d.Move != nil && (len(d.Move.Cwd) > 4096 || len(d.Move.Landing) > 256 || d.Move.DirectIfAllowed && d.Teleport != nil) {
		return errors.New("invalid direct move landing request")
	}
	if !proto.ValidStreamID(d.ID) || d.Type != kind.Name || d.Bytes <= 0 || d.Bytes > kind.MaxBytes {
		return errors.New("invalid bundle offer identity, type or size")
	}
	hash, err := hex.DecodeString(d.SHA256)
	if err != nil || len(hash) != sha256.Size {
		return errors.New("invalid bundle digest")
	}
	if !d.ExpiresAt.After(now) || d.ExpiresAt.After(now.Add(DefaultTTL+time.Hour)) {
		return errors.New("offer expired or TTL exceeds 72 hours")
	}
	if len(d.Summary) > 512 {
		return errors.New("offer summary exceeds 512 bytes")
	}
	if kind.GroupScoped && (d.Group == "" || len(d.Group) > 256) {
		return errors.New("receiving group is required for this bundle type")
	}
	if !kind.GroupScoped && d.Group != "" {
		return errors.New("this bundle type is not group-scoped")
	}
	if len(d.Inline) > 0 {
		if len(d.Inline) > InlineLimit {
			return errors.New("inline bundle exceeds 256 KiB")
		}
		return d.Verify(d.Inline)
	}
	return nil
}
func (d Descriptor) Verify(raw []byte) error {
	sum := sha256.Sum256(raw)
	if int64(len(raw)) != d.Bytes || hex.EncodeToString(sum[:]) != d.SHA256 {
		return errors.New("bundle length or SHA-256 mismatch")
	}
	return nil
}

// Request/Answer travel in sealed envelopes; the peer, offer, stream and digest
// must all match the locally reserved waiter before deriving stream keys.
type Request struct {
	Chunked bool   `json:"chunked,omitempty"`
	Offset  int64  `json:"offset,omitempty"`
	Bytes   int64  `json:"bytes,omitempty"`
	Offer   string `json:"offer"`
	Stream  string `json:"stream"`
	SHA256  string `json:"sha256"`
	Key     []byte `json:"key"`
}
type Answer struct {
	Request
	OK          bool   `json:"ok"`
	Reason      string `json:"reason,omitempty"`
	ChunkSHA256 string `json:"chunk_sha256,omitempty"`
}
type Result struct {
	SHA256      string `json:"sha256,omitempty"`
	BytesDone   int64  `json:"bytes_done,omitempty"`
	BytesTotal  int64  `json:"bytes_total,omitempty"`
	Disposition string `json:"disposition,omitempty"`
	Offer       string `json:"offer"`
	State       string `json:"state"`
}

type Spool struct{ Root string }

func (s Spool) path(direction, peer, id string) (string, error) {
	if (direction != "in" && direction != "out") || !proto.ValidInstanceID(peer) || !proto.ValidStreamID(id) {
		return "", errors.New("invalid private spool key")
	}
	return filepath.Join(s.Root, direction+"-"+peer+"-"+id+".bundle"), nil
}
func (s Spool) Open(direction, peer string, d Descriptor) (*os.File, error) {
	path, err := s.path(direction, peer, d.ID)
	if err != nil {
		return nil, err
	}
	f, err := openSpoolFile(path)
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() || st.Size() != d.Bytes {
		f.Close()
		return nil, errors.New("invalid spool file")
	}
	return f, nil
}
func (s Spool) Read(direction, peer string, d Descriptor) ([]byte, error) {
	f, err := s.Open(direction, peer, d)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, d.Bytes+1))
	if err != nil {
		return nil, err
	}
	return raw, d.Verify(raw)
}

// Receive consumes exactly the declared payload plus authenticated EOF. A
// stream.Conn reports a missing FIN as UnexpectedEOF, so partial delivery can
// never become a ready bundle. Rename publishes only the verified file.
func (s Spool) Receive(direction, peer string, d Descriptor, r io.Reader) error {
	path, err := s.path(direction, peer, d.ID)
	if err != nil {
		return err
	}
	if err = os.MkdirAll(s.Root, 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(s.Root, ".receiving-")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() { _ = os.Remove(tmp) }()
	digest := sha256.New()
	n, copyErr := io.Copy(io.MultiWriter(f, digest), io.LimitReader(r, d.Bytes+1))
	closeErr := f.Close()
	if copyErr != nil {
		return copyErr
	}
	if closeErr != nil {
		return closeErr
	}
	if n != d.Bytes || hex.EncodeToString(digest.Sum(nil)) != d.SHA256 {
		return errors.New("bundle length or SHA-256 mismatch")
	}
	// LimitReader only returns EOF early if the authenticated input ends. Since
	// it retains one spare byte here, a missing FIN or trailing byte is rejected.
	if err = os.Rename(tmp, path); err != nil {
		return fmt.Errorf("publish bundle: %w", err)
	}
	return nil
}
func (s Spool) Remove(direction, peer, id string) error {
	path, err := s.path(direction, peer, id)
	if err != nil {
		return err
	}
	// Partial transfers are private spool entries too, and expire with the offer.
	entries, _ := os.ReadDir(s.Root)
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasPrefix(name, filepath.Base(path)+".") && strings.HasSuffix(name, ".partial") && entry.Type().IsRegular() {
			_ = os.Remove(filepath.Join(s.Root, name))
		}
	}
	err = os.Remove(path)
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// PruneTemporary removes abandoned transfers left by a process crash. Active
// transfers have a five minute deadline; an hour leaves a generous margin.
func (s Spool) PruneTemporary(now time.Time) {
	entries, _ := os.ReadDir(s.Root)
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), ".receiving-") {
			continue
		}
		st, err := e.Info()
		if err == nil && st.Mode().IsRegular() && now.Sub(st.ModTime()) > time.Hour {
			_ = os.Remove(filepath.Join(s.Root, e.Name()))
		}
	}
}
