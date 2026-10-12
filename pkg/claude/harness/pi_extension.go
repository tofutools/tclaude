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

	"github.com/tofutools/tclaude/pkg/common"
)

// pi's live status comes from tclaude's own pi extension (piextension/
// tclaude-pi.ts), not from installed hooks: pi has no hook configuration, and
// tclaude never writes into ~/.pi. The extension is embedded in the binary,
// written read-only under tclaude's private data directory (denied to
// sandboxed agents, so a confined agent cannot swap the code the next
// unconfined pi will run) at a path named by its content hash, checked
// byte-for-byte before every launch, and loaded with `pi -e <path>`. It runs
// the hook callback named by PiHookBinEnvVar.

//go:embed piextension/tclaude-pi.ts
var piExtensionSource []byte

const (
	// PiHookBinEnvVar carries the absolute tclaude path the extension runs.
	PiHookBinEnvVar = "TCLAUDE_PI_HOOK_BIN"
	piExtensionFile = "tclaude-pi.ts"
)

// PiExtensionSource returns the embedded extension.
func PiExtensionSource() []byte { return bytes.Clone(piExtensionSource) }

// piExtensionCacheDir is a variable so tests can point it at a temp dir.
var piExtensionCacheDir = func() (string, error) {
	dir := common.TclaudeDataDir()
	if dir == "" {
		return "", errors.New("cannot resolve tclaude's data directory")
	}
	return filepath.Join(dir, "pi-extension"), nil
}

// MaterializePiExtension returns the path of the embedded extension on disk,
// writing it first when absent. The file lives in a directory named by the
// content hash, is read-only, and is verified against the embedded bytes on
// every call; a file that differs (tampered with, or truncated by a crash) is
// replaced rather than loaded.
func MaterializePiExtension() (string, error) {
	root, err := piExtensionCacheDir()
	if err != nil {
		return "", fmt.Errorf("pi extension: %w", err)
	}
	sum := sha256.Sum256(piExtensionSource)
	dir := filepath.Join(root, hex.EncodeToString(sum[:8]))
	path := filepath.Join(dir, piExtensionFile)
	// The hash directory itself must be a real directory: a symlink there
	// would point the verified path somewhere else.
	if info, err := os.Lstat(dir); err == nil && !info.IsDir() {
		if err := os.Remove(dir); err != nil {
			return "", fmt.Errorf("pi extension: replace %s: %w", dir, err)
		}
	}
	if current, err := readNoFollow(path); err == nil && bytes.Equal(current, piExtensionSource) {
		return path, nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("pi extension: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".tclaude-pi-*.ts")
	if err != nil {
		return "", fmt.Errorf("pi extension: %w", err)
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(piExtensionSource); err != nil {
		_ = tmp.Close()
		return "", fmt.Errorf("pi extension: %w", err)
	}
	if err := tmp.Chmod(0o444); err != nil {
		_ = tmp.Close()
		return "", fmt.Errorf("pi extension: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("pi extension: %w", err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return "", fmt.Errorf("pi extension: %w", err)
	}
	return path, nil
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
