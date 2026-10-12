// Package skillbundle carries one bounded skill directory without installing
// or executing publisher content during inspection.
package skillbundle

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/common/configbundle"
	"github.com/tofutools/tclaude/pkg/federation/proto"
	"golang.org/x/sys/unix"
)

const MaxFiles = 512
const Format = "tclaude-skill-bundle"

type File struct {
	Path       string `json:"path"`
	Data       []byte `json:"data"`
	Executable bool   `json:"executable,omitempty"`
}
type Bundle struct {
	Format string `json:"format"`
	Name   string `json:"name"`
	Files  []File `json:"files"`
}

func ValidName(s string) bool {
	return s != "" && len(s) <= 128 && s != "." && s != ".." && !strings.ContainsAny(s, "/\\\x00") && !strings.HasPrefix(s, ".")
}
func (b Bundle) Validate() error {
	if b.Format != Format || !ValidName(b.Name) || len(b.Files) == 0 || len(b.Files) > MaxFiles {
		return errors.New("invalid skill bundle name, format or file count")
	}
	seen := map[string]bool{}
	for _, f := range b.Files {
		if !fs.ValidPath(f.Path) || f.Path == "." || strings.ContainsAny(f.Path, "\\\x00") || seen[f.Path] {
			return fmt.Errorf("invalid or duplicate skill file %q", f.Path)
		}
		seen[f.Path] = true
		if len(configbundle.CredentialKinds(string(f.Data))) > 0 {
			return fmt.Errorf("suspected credentials in skill file %q; exclude them before publishing", f.Path)
		}
	}
	if !seen["SKILL.md"] {
		return errors.New("skill requires SKILL.md")
	}
	for p := range seen {
		for parent := path.Dir(p); parent != "."; parent = path.Dir(parent) {
			if seen[parent] {
				return errors.New("skill file conflicts with a directory")
			}
		}
	}
	raw, err := json.Marshal(b)
	if err != nil {
		return err
	}
	if len(raw) > proto.MaxBoardItemBytes {
		return errors.New("skill exceeds Board bundle limit (16 MiB)")
	}
	return nil
}
func Decode(raw []byte) (Bundle, error) {
	var b Bundle
	if len(raw) > proto.MaxBoardItemBytes {
		return b, errors.New("skill exceeds Board bundle limit (16 MiB)")
	}
	err := json.Unmarshal(raw, &b)
	if err == nil {
		err = b.Validate()
	}
	return b, err
}
func SecurityRelevant(f File) bool {
	if f.Executable || strings.HasPrefix(string(f.Data), "#!") {
		return true
	}
	switch strings.ToLower(path.Ext(f.Path)) {
	case ".sh", ".bash", ".zsh", ".fish", ".py", ".js", ".mjs", ".cjs", ".ts", ".rb", ".pl", ".php", ".lua", ".ps1", ".exe", ".bat", ".cmd":
		return true
	}
	return false
}

// ReadFile opens every directory component with no-follow, so a renamed or
// replaced directory cannot turn a bounded read into a symlink escape.
func ReadFile(root, relative string, limit int64) ([]byte, fs.FileMode, error) {
	if !fs.ValidPath(relative) || relative == "." {
		return nil, 0, errors.New("invalid relative file")
	}
	fd, err := unix.Open(root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = unix.Close(fd) }()
	parts := strings.Split(relative, "/")
	for _, part := range parts[:len(parts)-1] {
		next, e := unix.Openat(fd, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if e != nil {
			return nil, 0, e
		}
		_ = unix.Close(fd)
		fd = next
	}
	leaf, err := unix.Openat(fd, parts[len(parts)-1], unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, 0, err
	}
	f := os.NewFile(uintptr(leaf), relative)
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, 0, err
	}
	if !info.Mode().IsRegular() {
		return nil, 0, errors.New("skill contains a special file")
	}
	raw, err := io.ReadAll(io.LimitReader(f, limit+1))
	if int64(len(raw)) > limit {
		return nil, 0, errors.New("skill exceeds bounded size")
	}
	return raw, info.Mode(), err
}
func Export(root, name string) (Bundle, error) {
	return ExportDirectory(filepath.Join(root, name), name)
}

// ExportDirectory reads one explicitly selected skill directory. Its directory
// name need not equal the name chosen for the reusable library item.
func ExportDirectory(dir, name string) (Bundle, error) {
	b := Bundle{Format: Format, Name: name}
	if !ValidName(name) {
		return b, errors.New("invalid skill name")
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return b, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return b, errors.New("skill must be a directory, not a symlink")
	}
	var total int64
	err = filepath.WalkDir(dir, func(p string, d fs.DirEntry, e error) error {
		if e != nil {
			return e
		}
		if d.Type()&os.ModeSymlink != 0 {
			return errors.New("skills cannot contain symlinks")
		}
		if d.IsDir() {
			return nil
		}
		if len(b.Files) >= MaxFiles {
			return errors.New("skill has too many files (maximum 512)")
		}
		rel, e := filepath.Rel(dir, p)
		if e != nil {
			return e
		}
		raw, mode, e := ReadFile(dir, filepath.ToSlash(rel), proto.MaxBoardItemBytes-total)
		if e != nil {
			return e
		}
		total += int64(len(raw))
		b.Files = append(b.Files, File{Path: filepath.ToSlash(rel), Data: raw, Executable: mode&0111 != 0})
		return nil
	})
	if err == nil {
		sort.Slice(b.Files, func(i, j int) bool { return b.Files[i].Path < b.Files[j].Path })
		err = b.Validate()
	}
	return b, err
}
