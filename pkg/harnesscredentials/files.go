// Package harnesscredentials copies only explicitly selected standard auth
// files. No read API exposes these contents; callers send bundles over the
// encrypted peer channel and persist only metadata in jobs/audits.
package harnesscredentials

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"golang.org/x/sys/unix"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const MaxBytes = 18 << 10

var mu sync.Mutex
var ErrExists = errors.New("credentials already exist; explicit overwrite confirmation required")

type File struct {
	Name string `json:"name"`
	Data []byte `json:"data"`
}
type Bundle struct {
	Harness string `json:"harness"`
	Files   []File `json:"files"`
}
type Receipt struct {
	BackupID       string `json:"backup_id"`
	BackupLocation string `json:"backup_location"`
	Copied         bool   `json:"copied"`
}
type savedFile struct {
	Name   string `json:"name"`
	Exists bool   `json:"exists"`
	Data   []byte `json:"data,omitempty"`
}
type backup struct {
	CreatedAt time.Time   `json:"created_at"`
	Harness   string      `json:"harness"`
	Files     []savedFile `json:"files"`
}

func location(home, name string, receiving bool) (string, []string, error) {
	switch name {
	case "claude":
		dir := os.Getenv("CLAUDE_CONFIG_DIR")
		if receiving {
			dir = ""
		}
		if dir == "" {
			dir = filepath.Join(home, ".claude")
		}
		return dir, []string{".credentials.json"}, nil
	case "codex":
		dir := os.Getenv("CODEX_HOME")
		if dir == "" {
			dir = filepath.Join(home, ".codex")
		}
		return dir, []string{"auth.json"}, nil
	case "opencode":
		dir := os.Getenv("XDG_DATA_HOME")
		if dir == "" {
			dir = filepath.Join(home, ".local", "share")
		}
		return filepath.Join(dir, "opencode"), []string{"auth.json"}, nil
	case "gemini":
		if configured := os.Getenv("GEMINI_CLI_HOME"); configured != "" {
			home = configured
		}
		return filepath.Join(home, ".gemini"), []string{"oauth_creds.json", "google_accounts.json"}, nil
	default:
		return "", nil, fmt.Errorf("file credential copy unsupported for this harness; use its login flow (keychains and environment secrets are not copied)")
	}
}

// canonicalDirectory resolves only daemon-configured local directory paths,
// never a peer-selected path or a credential file. Missing suffixes are appended
// to the canonical existing ancestor so new directories can be created safely.
func canonicalDirectory(path string) (string, error) {
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("credential directory must be absolute")
	}
	ancestor := filepath.Clean(path)
	suffix := []string{}
	for {
		_, err := os.Lstat(ancestor)
		if err == nil {
			break
		}
		if !os.IsNotExist(err) || ancestor == string(os.PathSeparator) {
			return "", fmt.Errorf("credential directory unavailable")
		}
		suffix = append(suffix, filepath.Base(ancestor))
		ancestor = filepath.Dir(ancestor)
	}
	canonical, err := filepath.EvalSymlinks(ancestor)
	if err != nil {
		return "", fmt.Errorf("credential directory unavailable")
	}
	for i := len(suffix) - 1; i >= 0; i-- {
		canonical = filepath.Join(canonical, suffix[i])
	}
	return canonical, nil
}
func openDirectory(path string, create bool) (*os.Root, error) {
	// HOME and deliberately linked harness roots are local operator choices.
	// Resolve their aliases once, then reject symlinks introduced below that
	// canonical path and pin the opened root to its observed directory identity.
	path, err := canonicalDirectory(path)
	if err != nil {
		return nil, err
	}
	var expected os.FileInfo
	current := string(os.PathSeparator)
	for _, part := range strings.Split(strings.TrimPrefix(path, string(os.PathSeparator)), string(os.PathSeparator)) {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if os.IsNotExist(err) && create {
			err = os.Mkdir(current, 0700)
			if err == nil {
				info, err = os.Lstat(current)
			}
		}
		if err != nil {
			return nil, fmt.Errorf("credential directory unavailable")
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("credential directory symlinks are refused")
		}
		expected = info
	}
	root, err := os.OpenRoot(path)
	if err != nil {
		return nil, err
	}
	dir, err := root.Open(".")
	if err != nil {
		_ = root.Close()
		return nil, err
	}
	opened, err := dir.Stat()
	_ = dir.Close()
	if err != nil || expected == nil || !os.SameFile(expected, opened) {
		_ = root.Close()
		return nil, fmt.Errorf("credential directory changed while opening")
	}
	return root, nil
}
func readFile(root *os.Root, name string) ([]byte, error) {
	return readBoundedFile(root, name, MaxBytes)
}
func readBoundedFile(root *os.Root, name string, limit int) ([]byte, error) {
	before, err := root.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !before.Mode().IsRegular() || before.Size() > int64(limit) {
		return nil, fmt.Errorf("credential file must be a bounded regular file")
	}
	f, err := root.OpenFile(name, os.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, fmt.Errorf("credential file unavailable")
	}
	defer func() { _ = f.Close() }()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(before, opened) {
		return nil, fmt.Errorf("credential file changed while opening")
	}
	raw, err := io.ReadAll(io.LimitReader(f, int64(limit)+1))
	if err != nil || len(raw) > limit {
		return nil, fmt.Errorf("credential file exceeds limit")
	}
	return raw, nil
}
func Capture(home, name string) (Bundle, error) {
	mu.Lock()
	defer mu.Unlock()
	dir, names, err := location(home, name, false)
	if err != nil {
		return Bundle{}, err
	}
	root, err := openDirectory(dir, false)
	if err != nil {
		return Bundle{}, err
	}
	defer func() { _ = root.Close() }()
	b := Bundle{Harness: name, Files: []File{}}
	for _, file := range names {
		raw, err := readFile(root, file)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return Bundle{}, err
		}
		b.Files = append(b.Files, File{file, raw})
	}
	if err := b.Validate(); err != nil {
		return Bundle{}, err
	}
	return b, nil
}
func (b Bundle) Validate() error {
	_, names, err := location("/unused", b.Harness, false)
	if err != nil {
		return err
	}
	if len(b.Files) == 0 {
		return fmt.Errorf("no file-based credentials available; log in locally or use the target login flow")
	}
	count := 0
	seen := map[string]bool{}
	for _, f := range b.Files {
		valid := false
		for _, n := range names {
			valid = valid || n == f.Name
		}
		if !valid || seen[f.Name] || len(f.Data) == 0 || !json.Valid(f.Data) {
			return fmt.Errorf("invalid credential bundle")
		}
		seen[f.Name] = true
		count += len(f.Data)
	}
	if count > MaxBytes {
		return fmt.Errorf("credential bundle exceeds limit")
	}
	return nil
}
func atomicFile(root *os.Root, name string, raw []byte) error {
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return err
	}
	temp := ".tclaude-credentials-" + hex.EncodeToString(nonce[:])
	f, err := root.OpenFile(temp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return fmt.Errorf("could not create private credential file")
	}
	defer func() { _ = root.Remove(temp) }()
	_, e1 := f.Write(raw)
	e2 := f.Sync()
	e3 := f.Close()
	if errors.Join(e1, e2, e3) != nil {
		return fmt.Errorf("could not persist credential file")
	}
	if err := root.Rename(temp, name); err != nil {
		return fmt.Errorf("could not replace credential file")
	}
	d, err := root.Open(".")
	if err != nil {
		return err
	}
	defer func() { _ = d.Close() }()
	return d.Sync()
}
func Receive(home, privateDir string, b Bundle, overwrite bool, authorize func() bool) (Receipt, error) {
	mu.Lock()
	defer mu.Unlock()
	if err := b.Validate(); err != nil {
		return Receipt{}, err
	}
	if !authorize() {
		return Receipt{}, fmt.Errorf("credential receiving authority revoked")
	}
	dir, _, err := location(home, b.Harness, true)
	if err != nil {
		return Receipt{}, err
	}
	root, err := openDirectory(dir, true)
	if err != nil {
		return Receipt{}, err
	}
	defer func() { _ = root.Close() }()
	previous := backup{Harness: b.Harness, Files: []savedFile{}}
	for _, f := range b.Files {
		raw, err := readFile(root, f.Name)
		if err != nil && !os.IsNotExist(err) {
			return Receipt{}, err
		}
		exists := err == nil
		if exists && !overwrite {
			return Receipt{}, ErrExists
		}
		previous.Files = append(previous.Files, savedFile{Name: f.Name, Exists: exists, Data: raw})
	}
	receipt, err := saveBackup(privateDir, previous)
	if err != nil {
		return Receipt{}, err
	}

	for i, f := range b.Files {
		if !authorize() {
			return receipt, errors.Join(fmt.Errorf("credential receiving authority revoked"), restoreFiles(root, previous.Files[:i]))
		}
		current, err := readFile(root, f.Name)
		p := previous.Files[i]
		if (p.Exists && (err != nil || string(current) != string(p.Data))) || (!p.Exists && !os.IsNotExist(err)) {
			return receipt, errors.Join(fmt.Errorf("credentials changed before replacement"), restoreFiles(root, previous.Files[:i]))
		}
		if err := atomicFile(root, f.Name, f.Data); err != nil {
			return receipt, errors.Join(err, restoreFiles(root, previous.Files[:i+1]))
		}
	}
	receipt.Copied = true
	return receipt, nil
}
func restoreFiles(root *os.Root, files []savedFile) error {
	var result error
	for i := len(files) - 1; i >= 0; i-- {
		f := files[i]
		if f.Exists {
			result = errors.Join(result, atomicFile(root, f.Name, f.Data))
		} else {
			err := root.Remove(f.Name)
			if !os.IsNotExist(err) {
				result = errors.Join(result, err)
			}
			d, e := root.Open(".")
			if e == nil {
				e = d.Sync()
				_ = d.Close()
			}
			result = errors.Join(result, e)
		}
	}
	return result
}

// Presence reports only bounded regular-file presence. Missing files remain
// unknown because a keychain or ambient provider may authenticate the harness.
func Presence(home, name string) *bool {
	dir, names, err := location(home, name, true)
	if err != nil {
		return nil
	}
	root, err := openDirectory(dir, false)
	if err != nil {
		return nil
	}
	defer func() { _ = root.Close() }()
	for _, file := range names {
		info, err := root.Lstat(file)
		if err == nil && info.Mode().IsRegular() && info.Size() > 0 && info.Size() <= MaxBytes {
			yes := true
			return &yes
		}
	}
	return nil
}

// StandardSecretPaths is the relative credential-path inventory used by file
// sharing guards. Keep it next to the credential capture locations.
func StandardSecretPaths() []string {
	return []string{".claude/.credentials.json", ".codex/auth.json", ".local/share/opencode/auth.json", ".gemini/oauth_creds.json", ".gemini/google_accounts.json"}
}

// ConfiguredSecretPaths includes operator-selected credential locations, which
// may lie inside a project despite not having a standard harness directory name.
func ConfiguredSecretPaths(home string) []string {
	var paths []string
	for _, name := range []string{"claude", "codex", "opencode", "gemini"} {
		for _, receiving := range []bool{false, true} {
			dir, files, err := location(home, name, receiving)
			if err != nil {
				continue
			}
			dir, err = canonicalDirectory(dir)
			if err != nil {
				continue
			}
			for _, file := range files {
				paths = append(paths, filepath.Join(dir, file))
			}
		}
	}
	return paths
}
