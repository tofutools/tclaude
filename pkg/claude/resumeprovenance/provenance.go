package resumeprovenance

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/harness"
)

const (
	Version       = 1
	MaxEncodedLen = 16 << 10

	RepositoryNone = "none"
	RepositoryGit  = "git"
)

// PathIdentity binds an absolute canonical directory pathname to the physical
// filesystem object that occupied it at a trustworthy lifecycle boundary.
type PathIdentity struct {
	Path   string `json:"path"`
	Device uint64 `json:"device"`
	Inode  uint64 `json:"inode"`
}

type GitIdentity struct {
	Dir       PathIdentity `json:"dir"`
	CommonDir PathIdentity `json:"common_dir"`
}

// Provenance is the daemon-private resume identity stored on one session row.
// RepositoryState is explicit so malformed values cannot reinterpret a missing
// repository object as a trustworthy non-repository launch.
type Provenance struct {
	Version         int          `json:"version"`
	Cwd             PathIdentity `json:"cwd"`
	RepositoryState string       `json:"repository_state"`
	Repository      *GitIdentity `json:"repository,omitempty"`
}

// Capture observes cwd's current physical directory and Git metadata identity.
// Callers decide whether the boundary is trustworthy (live pane, human recovery,
// or a launch whose proof guard has already completed).
func Capture(cwd string) (Provenance, error) {
	cwdID, err := capturePath(cwd)
	if err != nil {
		return Provenance{}, fmt.Errorf("capture cwd identity: %w", err)
	}
	commonDir, err := harness.GitCommonDir(cwdID.Path)
	if err != nil {
		return Provenance{}, err
	}
	gitDir, err := harness.GitDir(cwdID.Path)
	if err != nil {
		return Provenance{}, err
	}
	p := Provenance{Version: Version, Cwd: cwdID, RepositoryState: RepositoryNone}
	if commonDir == "" && gitDir == "" {
		return p, nil
	}
	if commonDir == "" || gitDir == "" {
		return Provenance{}, fmt.Errorf("inconsistent Git identity for %s: git-dir=%q common-dir=%q",
			cwdID.Path, gitDir, commonDir)
	}
	commonID, err := capturePath(commonDir)
	if err != nil {
		return Provenance{}, fmt.Errorf("capture Git common-dir identity: %w", err)
	}
	gitID, err := capturePath(gitDir)
	if err != nil {
		return Provenance{}, fmt.Errorf("capture Git dir identity: %w", err)
	}
	p.RepositoryState = RepositoryGit
	p.Repository = &GitIdentity{Dir: gitID, CommonDir: commonID}
	return p, nil
}

func capturePath(path string) (PathIdentity, error) {
	path = strings.TrimSpace(path)
	if path == "" || !filepath.IsAbs(path) {
		return PathIdentity{}, fmt.Errorf("path must be absolute, got %q", path)
	}
	physical, err := filepath.EvalSymlinks(path)
	if err != nil {
		return PathIdentity{}, fmt.Errorf("resolve %s: %w", path, err)
	}
	physical = filepath.Clean(physical)
	info, err := os.Stat(physical)
	if err != nil {
		return PathIdentity{}, fmt.Errorf("stat %s: %w", physical, err)
	}
	if !info.IsDir() {
		return PathIdentity{}, fmt.Errorf("%s is not a directory", physical)
	}
	device, inode, err := platformFileIdentity(info)
	if err != nil {
		return PathIdentity{}, fmt.Errorf("identify %s: %w", physical, err)
	}
	return PathIdentity{Path: physical, Device: device, Inode: inode}, nil
}

func Encode(p Provenance) (string, error) {
	if err := p.Validate(); err != nil {
		return "", err
	}
	raw, err := json.Marshal(p)
	if err != nil {
		return "", err
	}
	if len(raw) > MaxEncodedLen {
		return "", fmt.Errorf("resume provenance exceeds %d bytes", MaxEncodedLen)
	}
	return string(raw), nil
}

// Decode is deliberately strict because an empty, malformed, newer, or
// internally inconsistent row must never become launch authority.
func Decode(raw string) (Provenance, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return Provenance{}, errors.New("resume provenance is missing")
	}
	if len(raw) > MaxEncodedLen {
		return Provenance{}, fmt.Errorf("resume provenance exceeds %d bytes", MaxEncodedLen)
	}
	dec := json.NewDecoder(bytes.NewBufferString(raw))
	dec.DisallowUnknownFields()
	var p Provenance
	if err := dec.Decode(&p); err != nil {
		return Provenance{}, fmt.Errorf("decode resume provenance: %w", err)
	}
	if err := ensureJSONEOF(dec); err != nil {
		return Provenance{}, err
	}
	if err := p.Validate(); err != nil {
		return Provenance{}, err
	}
	return p, nil
}

func ensureJSONEOF(dec *json.Decoder) error {
	var extra any
	err := dec.Decode(&extra)
	if errors.Is(err, io.EOF) {
		return nil
	}
	if err == nil {
		return errors.New("decode resume provenance: trailing JSON value")
	}
	return fmt.Errorf("decode resume provenance trailing data: %w", err)
}

func (p Provenance) Validate() error {
	if p.Version != Version {
		return fmt.Errorf("unsupported resume provenance version %d", p.Version)
	}
	if err := p.Cwd.validate("cwd"); err != nil {
		return err
	}
	switch p.RepositoryState {
	case RepositoryNone:
		if p.Repository != nil {
			return errors.New("resume provenance repository_state none has repository identity")
		}
	case RepositoryGit:
		if p.Repository == nil {
			return errors.New("resume provenance repository_state git is missing repository identity")
		}
		if err := p.Repository.Dir.validate("repository.dir"); err != nil {
			return err
		}
		if err := p.Repository.CommonDir.validate("repository.common_dir"); err != nil {
			return err
		}
	default:
		return fmt.Errorf("invalid resume provenance repository_state %q", p.RepositoryState)
	}
	return nil
}

func (p PathIdentity) validate(field string) error {
	if p.Path == "" || !filepath.IsAbs(p.Path) || filepath.Clean(p.Path) != p.Path {
		return fmt.Errorf("invalid resume provenance %s path %q", field, p.Path)
	}
	if p.Device == 0 || p.Inode == 0 {
		return fmt.Errorf("invalid resume provenance %s filesystem identity", field)
	}
	return nil
}

// Compare reports which durable identity changed. Callers use this diagnostic
// without ever deriving launch grants from the newly observed value.
func Compare(expected, actual Provenance) error {
	if err := expected.Validate(); err != nil {
		return err
	}
	if err := actual.Validate(); err != nil {
		return err
	}
	if err := comparePath("cwd", expected.Cwd, actual.Cwd); err != nil {
		return err
	}
	if expected.RepositoryState != actual.RepositoryState {
		return fmt.Errorf("repository identity changed: expected %s, found %s",
			expected.RepositoryState, actual.RepositoryState)
	}
	if expected.RepositoryState == RepositoryGit {
		if err := comparePath("Git dir", expected.Repository.Dir, actual.Repository.Dir); err != nil {
			return err
		}
		if err := comparePath("Git common dir", expected.Repository.CommonDir, actual.Repository.CommonDir); err != nil {
			return err
		}
	}
	return nil
}

func comparePath(label string, expected, actual PathIdentity) error {
	if expected.Path != actual.Path {
		return fmt.Errorf("%s path changed: expected %s, found %s", label, expected.Path, actual.Path)
	}
	if expected.Device != actual.Device || expected.Inode != actual.Inode {
		return fmt.Errorf("%s filesystem identity changed at %s", label, expected.Path)
	}
	return nil
}
