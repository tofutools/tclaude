package sandboxpolicy

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

const maxSnapshotFileBytes = 1 << 20

// WriteSnapshotFile writes a private one-shot handoff for the detached
// session wrapper. Only the path and digest travel in argv; environment values
// remain out of process listings. The receiving wrapper owns deletion.
func WriteSnapshotFile(dir string, snapshot Snapshot) (path, digest string, err error) {
	validated, err := RevalidateSnapshot(snapshot)
	if err != nil {
		return "", "", err
	}
	payload, err := json.Marshal(validated)
	if err != nil {
		return "", "", fmt.Errorf("marshal sandbox snapshot handoff: %w", err)
	}
	if len(payload) > maxSnapshotFileBytes {
		return "", "", fmt.Errorf("sandbox snapshot handoff exceeds %d bytes", maxSnapshotFileBytes)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", "", fmt.Errorf("create sandbox snapshot handoff directory: %w", err)
	}
	f, err := os.CreateTemp(dir, "sandbox-snapshot-*.json")
	if err != nil {
		return "", "", fmt.Errorf("create sandbox snapshot handoff: %w", err)
	}
	path = f.Name()
	ok := false
	defer func() {
		_ = f.Close()
		if !ok {
			_ = os.Remove(path)
		}
	}()
	if err := f.Chmod(0o600); err != nil {
		return "", "", fmt.Errorf("secure sandbox snapshot handoff: %w", err)
	}
	if _, err := f.Write(payload); err != nil {
		return "", "", fmt.Errorf("write sandbox snapshot handoff: %w", err)
	}
	if err := f.Sync(); err != nil {
		return "", "", fmt.Errorf("sync sandbox snapshot handoff: %w", err)
	}
	if err := f.Close(); err != nil {
		return "", "", fmt.Errorf("close sandbox snapshot handoff: %w", err)
	}
	sum := sha256.Sum256(payload)
	ok = true
	return path, hex.EncodeToString(sum[:]), nil
}

// ReadSnapshotFile verifies and consumes a one-shot handoff. The caller should
// remove path after this returns, including on error.
func ReadSnapshotFile(path, expectedDigest string) (Snapshot, error) {
	if !filepath.IsAbs(path) {
		return Snapshot{}, fmt.Errorf("sandbox snapshot handoff path must be absolute")
	}
	info, err := os.Lstat(path)
	if err != nil {
		return Snapshot{}, fmt.Errorf("stat sandbox snapshot handoff: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return Snapshot{}, fmt.Errorf("sandbox snapshot handoff must be a regular non-symlink file")
	}
	if info.Mode().Perm() != 0o600 {
		return Snapshot{}, fmt.Errorf("sandbox snapshot handoff has mode %04o, want 0600", info.Mode().Perm())
	}
	f, err := os.Open(path)
	if err != nil {
		return Snapshot{}, fmt.Errorf("open sandbox snapshot handoff: %w", err)
	}
	defer func() { _ = f.Close() }()
	payload, err := io.ReadAll(io.LimitReader(f, maxSnapshotFileBytes+1))
	if err != nil {
		return Snapshot{}, fmt.Errorf("read sandbox snapshot handoff: %w", err)
	}
	if len(payload) > maxSnapshotFileBytes {
		return Snapshot{}, fmt.Errorf("sandbox snapshot handoff exceeds %d bytes", maxSnapshotFileBytes)
	}
	sum := sha256.Sum256(payload)
	if hex.EncodeToString(sum[:]) != expectedDigest {
		return Snapshot{}, fmt.Errorf("sandbox snapshot handoff digest mismatch")
	}
	var snapshot Snapshot
	if err := json.Unmarshal(payload, &snapshot); err != nil {
		return Snapshot{}, fmt.Errorf("decode sandbox snapshot handoff: %w", err)
	}
	return RevalidateSnapshot(snapshot)
}
