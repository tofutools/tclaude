package harness

import (
	"bytes"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"

	"github.com/tofutools/tclaude/pkg/common"
)

// pi's live status comes from tclaude's own pi extension (piextension/
// tclaude-pi.ts), not from installed hooks: pi has no hook configuration, and
// tclaude never writes into ~/.pi. The extension is embedded in the binary,
// written read-only under ~/.tclaude/pi-extension at a path named by its
// content hash, checked byte-for-byte before every launch, and loaded with
// `pi -e <path>`. It runs the hook callback named by PiHookBinEnvVar.
//
// Not under the private data directory: tclaude's sandbox hides that from the
// pane, and the pane is where pi loads the extension. tclaude's sandbox mounts
// the hash directory read-only, and the per-launch check replaces a file that
// differs before pi reads it.

//go:embed piextension/tclaude-pi.ts
var piExtensionSource []byte

// The `ask` posture's permission extension (pi_approval.go) ships the same
// way, in the same directory: its hash covers both files.
//
//go:embed piextension/tclaude-pi-permissions.ts
var piPermissionExtensionSource []byte

const (
	// PiHookBinEnvVar carries the absolute tclaude path the extension runs.
	PiHookBinEnvVar           = "TCLAUDE_PI_HOOK_BIN"
	piExtensionFile           = "tclaude-pi.ts"
	piPermissionExtensionFile = "tclaude-pi-permissions.ts"
)

// piExtensionFiles is what the extension directory holds.
func piExtensionFiles() map[string][]byte {
	return map[string][]byte{
		piExtensionFile:           piExtensionSource,
		piPermissionExtensionFile: piPermissionExtensionSource,
	}
}

// PiPermissionExtensionPath is the permission extension materialized next to
// the status extension at statusExtension (MaterializePiExtension).
func PiPermissionExtensionPath(statusExtension string) string {
	return filepath.Join(filepath.Dir(statusExtension), piPermissionExtensionFile)
}

// PiExtensionSource returns the embedded extension.
func PiExtensionSource() []byte { return bytes.Clone(piExtensionSource) }

// piExtensionCacheDir is a variable so tests can point it at a temp dir.
var piExtensionCacheDir = func() (string, error) {
	dir := common.TclaudeDir()
	if dir == "" {
		return "", errors.New("cannot resolve tclaude's directory")
	}
	return filepath.Join(dir, "pi-extension"), nil
}

// MaterializePiExtension returns the path of the embedded status extension
// on disk, writing the extension files first when absent. They live in a
// directory named by their content hash, are read-only, and are verified
// against the embedded bytes on every call; a file that differs (tampered
// with, or truncated by a crash) is replaced rather than loaded.
func MaterializePiExtension() (string, error) {
	root, err := piExtensionCacheDir()
	if err != nil {
		return "", fmt.Errorf("pi extension: %w", err)
	}
	files := piExtensionFiles()
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	hash := sha256.New()
	for _, name := range names {
		sum := sha256.Sum256(files[name])
		hash.Write([]byte(name))
		hash.Write(sum[:])
	}
	dir := filepath.Join(root, hex.EncodeToString(hash.Sum(nil)[:8]))
	// The hash directory itself must be a real directory: a symlink there
	// would point the verified path somewhere else.
	if info, err := os.Lstat(dir); err == nil && !info.IsDir() {
		if err := os.Remove(dir); err != nil {
			return "", fmt.Errorf("pi extension: replace %s: %w", dir, err)
		}
	}
	for _, name := range names {
		if err := materializePiExtensionFile(dir, name, files[name]); err != nil {
			return "", err
		}
	}
	return filepath.Join(dir, piExtensionFile), nil
}

func materializePiExtensionFile(dir, name string, source []byte) error {
	path := filepath.Join(dir, name)
	if current, err := readNoFollow(path); err == nil && bytes.Equal(current, source) {
		return nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("pi extension: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".tclaude-pi-*.ts")
	if err != nil {
		return fmt.Errorf("pi extension: %w", err)
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(source); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("pi extension: %w", err)
	}
	if err := tmp.Chmod(0o444); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("pi extension: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("pi extension: %w", err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("pi extension: %w", err)
	}
	return nil
}

// readNoFollow reads a regular file, refusing a symlink in its place.
func readNoFollow(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, &fs.PathError{Op: "read", Path: path, Err: errors.New("not a regular file")}
	}
	return os.ReadFile(path)
}
