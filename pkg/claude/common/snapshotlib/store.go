// Package snapshotlib is the local snapshot library: named, point-in-time
// agent PAFs under ~/.tclaude/data/snapshots/. Each snapshot is a directory
// holding the PAF (the source of truth) and a small metadata file for
// listing. A snapshot appears only once complete: it is written to a
// hidden temporary directory, synced, and renamed into place.
package snapshotlib

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/tofutools/tclaude/pkg/paf"
)

// File names inside a snapshot directory.
const (
	PAFFile  = "snapshot.paf"
	MetaFile = "snapshot.json"
)

// tmpPrefix marks directories still being written or removed. They are
// never listed, and leftovers older than staleAfter are swept.
const (
	tmpPrefix  = ".tmp-"
	staleAfter = time.Hour
)

// nameRE is the snapshot name charset: a lowercase letter or digit, then up
// to 63 of lowercase letters, digits, '.', '_' and '-'. It admits no path
// separator, and the leading character rules out "." and "..".
var nameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

// ValidName reports whether name is an acceptable snapshot name.
func ValidName(name string) bool { return nameRE.MatchString(name) }

// ErrExists and ErrNotFound are returned for name conflicts and misses.
var (
	ErrExists   = errors.New("a snapshot with that name already exists")
	ErrNotFound = errors.New("no such snapshot")
)

// Meta is a snapshot's listing metadata. The PAF holds the full record.
type Meta struct {
	Name      string        `json:"name"`
	Note      string        `json:"note,omitempty"`
	AgentID   string        `json:"agent_id,omitempty"`
	AgentName string        `json:"agent_name"`
	ConvID    string        `json:"conv_id,omitempty"`
	Harness   string        `json:"harness"`
	Model     string        `json:"model,omitempty"`
	State     string        `json:"state,omitempty"`
	CreatedAt time.Time     `json:"created_at"`
	ExpiresAt *time.Time    `json:"expires_at,omitempty"`
	Bytes     int64         `json:"bytes"`
	History   bool          `json:"history"`
	Findings  []paf.Finding `json:"findings"`
}

// Expired reports whether the snapshot is past its expiry at now.
func (m Meta) Expired(now time.Time) bool { return m.ExpiresAt != nil && !now.Before(*m.ExpiresAt) }

// Store is a snapshot library rooted at a directory. Its lock orders the
// steps that change which snapshots exist (the final rename of a create, a
// remove, a prune), so retention never races a concurrent create. Writing a
// snapshot's bytes happens outside the lock.
type Store struct {
	dir string
	mu  sync.Mutex
}

// New returns the store rooted at dir. The directory is created on first
// write.
func New(dir string) *Store { return &Store{dir: dir} }

// Dir is the library root.
func (s *Store) Dir() string { return s.dir }

// Path returns the directory of a named snapshot.
func (s *Store) Path(name string) (string, error) {
	if !ValidName(name) {
		return "", fmt.Errorf("invalid snapshot name %q: use 1-64 of a-z, 0-9, '.', '_' and '-', starting with a letter or digit", name)
	}
	return filepath.Join(s.dir, name), nil
}

// Create writes a new snapshot: write streams the PAF, and meta is stored
// beside it with Bytes filled in. Nothing is visible under name until the
// whole snapshot is on disk.
func (s *Store) Create(meta Meta, write func(io.Writer) error) (Meta, error) {
	final, err := s.Path(meta.Name)
	if err != nil {
		return Meta{}, err
	}
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return Meta{}, err
	}
	if _, err := os.Stat(final); err == nil {
		return Meta{}, ErrExists
	}
	tmp, err := s.tempDir()
	if err != nil {
		return Meta{}, err
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	n, err := writeSynced(filepath.Join(tmp, PAFFile), write)
	if err != nil {
		return Meta{}, err
	}
	meta.Bytes = n
	if meta.Findings == nil {
		meta.Findings = []paf.Finding{}
	}
	raw, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return Meta{}, err
	}
	if _, err := writeSynced(filepath.Join(tmp, MetaFile), func(w io.Writer) error { _, err := w.Write(raw); return err }); err != nil {
		return Meta{}, err
	}
	if err := syncDir(tmp); err != nil {
		return Meta{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := os.Stat(final); err == nil {
		return Meta{}, ErrExists
	}
	if err := os.Rename(tmp, final); err != nil {
		return Meta{}, err
	}
	return meta, syncDir(s.dir)
}

func (s *Store) tempDir() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	p := filepath.Join(s.dir, tmpPrefix+hex.EncodeToString(b[:]))
	return p, os.Mkdir(p, 0o700)
}

// writeSynced writes a new 0600 file and fsyncs it.
func writeSynced(path string, write func(io.Writer) error) (int64, error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return 0, err
	}
	cw := &countingWriter{w: f}
	err = write(cw)
	if err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	return cw.n, err
}

type countingWriter struct {
	w io.Writer
	n int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	err = d.Sync()
	if closeErr := d.Close(); err == nil {
		err = closeErr
	}
	return err
}

// List returns every complete snapshot, newest first. Directories still
// being written, and entries whose metadata cannot be read, are skipped.
func (s *Store) List() ([]Meta, error) {
	entries, err := os.ReadDir(s.dir)
	if errors.Is(err, os.ErrNotExist) {
		return []Meta{}, nil
	}
	if err != nil {
		return nil, err
	}
	out := []Meta{}
	for _, e := range entries {
		if !e.IsDir() || !ValidName(e.Name()) {
			continue
		}
		m, err := s.readMeta(e.Name())
		if err != nil {
			continue
		}
		out = append(out, m)
	}
	slices.SortFunc(out, func(a, b Meta) int { return b.CreatedAt.Compare(a.CreatedAt) })
	return out, nil
}

// Get returns a snapshot's metadata and the path of its PAF.
func (s *Store) Get(name string) (Meta, string, error) {
	dir, err := s.Path(name)
	if err != nil {
		return Meta{}, "", err
	}
	m, err := s.readMeta(name)
	if errors.Is(err, os.ErrNotExist) {
		return Meta{}, "", ErrNotFound
	}
	if err != nil {
		return Meta{}, "", err
	}
	return m, filepath.Join(dir, PAFFile), nil
}

func (s *Store) readMeta(name string) (Meta, error) {
	raw, err := os.ReadFile(filepath.Join(s.dir, name, MetaFile))
	if err != nil {
		return Meta{}, err
	}
	var m Meta
	if err := json.Unmarshal(raw, &m); err != nil {
		return Meta{}, err
	}
	if m.Name != name {
		return Meta{}, fmt.Errorf("snapshot %q: metadata names %q", name, m.Name)
	}
	return m, nil
}

// Remove deletes a snapshot. It disappears from listings at once: it is
// renamed to a temporary name before its files are removed.
func (s *Store) Remove(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.removeLocked(name)
}

func (s *Store) removeLocked(name string) error {
	dir, err := s.Path(name)
	if err != nil {
		return err
	}
	if _, err := os.Stat(dir); errors.Is(err, os.ErrNotExist) {
		return ErrNotFound
	}
	gone, err := s.tempName()
	if err != nil {
		return err
	}
	if err := os.Rename(dir, gone); err != nil {
		return err
	}
	return os.RemoveAll(gone)
}

func (s *Store) tempName() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return filepath.Join(s.dir, tmpPrefix+hex.EncodeToString(b[:])), nil
}

// Prune removes the snapshots expired at now and returns their names. It
// also sweeps temporary directories left by an interrupted create or
// remove. A create in progress is never touched: its directory is younger
// than the sweep age, and it becomes visible only under the lock.
func (s *Store) Prune(now time.Time) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entries, err := os.ReadDir(s.dir)
	if errors.Is(err, os.ErrNotExist) {
		return []string{}, nil
	}
	if err != nil {
		return nil, err
	}
	removed := []string{}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), tmpPrefix) {
			if info, err := e.Info(); err == nil && now.Sub(info.ModTime()) > staleAfter {
				_ = os.RemoveAll(filepath.Join(s.dir, e.Name()))
			}
			continue
		}
		if !e.IsDir() || !ValidName(e.Name()) {
			continue
		}
		m, err := s.readMeta(e.Name())
		if err != nil || !m.Expired(now) {
			continue
		}
		if err := s.removeLocked(e.Name()); err != nil {
			return removed, err
		}
		removed = append(removed, e.Name())
	}
	return removed, nil
}
