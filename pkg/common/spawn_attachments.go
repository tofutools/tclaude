package common

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// SpawnAttachmentsPrivateBase is the daemon-owned parent hidden from every
// tclaude-layer launch. It lives in the agent-reachable API tree because the
// attachment paths are handed to agents; keeping them below the protected data
// root makes those paths unusable from an outer/native sandbox that enforces
// the protected-root deny. Each launch still reopens only its own hashed child.
func SpawnAttachmentsPrivateBase() string {
	return filepath.Join(TclaudeAPIDir(), "spawn-attachments")
}

// LegacySpawnAttachmentsPrivateBase returns the pre-relocation parent. It is
// retained only so agentd can serve and eventually sweep attachment roots that
// belong to tclaude-layer sessions still running across an upgrade.
func LegacySpawnAttachmentsPrivateBase() string {
	return filepath.Join(TclaudeDataDir(), "spawn-attachments")
}

// SpawnAttachmentsPrivateDir returns the stable, path-safe private directory
// for one session-row identity. Hashing avoids turning a caller-controlled
// session label into path syntax while keeping agentd and the launch seam in
// exact agreement without another persisted field.
func SpawnAttachmentsPrivateDir(sessionID string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(sessionID)))
	return filepath.Join(SpawnAttachmentsPrivateBase(), hex.EncodeToString(sum[:]))
}

// LegacySpawnAttachmentsPrivateDir returns the old root for a session that
// may still have that exact path mounted from a pre-relocation launch.
func LegacySpawnAttachmentsPrivateDir(sessionID string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(sessionID)))
	return filepath.Join(LegacySpawnAttachmentsPrivateBase(), hex.EncodeToString(sum[:]))
}

// PrepareSpawnAttachmentsPrivateDir materializes the stable private root for
// one session without ever accepting a symlink in place of the daemon-owned
// parent or child. created reports whether this call created the child.
func PrepareSpawnAttachmentsPrivateDir(sessionID string) (path string, created bool, err error) {
	base := SpawnAttachmentsPrivateBase()
	if !filepath.IsAbs(base) {
		return "", false, fmt.Errorf("private attachment parent is not absolute")
	}
	if err := os.MkdirAll(base, 0o700); err != nil {
		return "", false, fmt.Errorf("create private attachment parent: %w", err)
	}
	baseInfo, err := os.Lstat(base)
	if err != nil || baseInfo.Mode()&os.ModeSymlink != 0 || !baseInfo.IsDir() {
		return "", false, fmt.Errorf("private attachment parent is not a real directory")
	}
	path = SpawnAttachmentsPrivateDir(sessionID)
	info, err := os.Lstat(path)
	switch {
	case os.IsNotExist(err):
		if err := os.Mkdir(path, 0o700); err != nil {
			return "", false, fmt.Errorf("create private attachment root: %w", err)
		}
		created = true
	case err != nil:
		return "", false, fmt.Errorf("inspect private attachment root: %w", err)
	case info.Mode()&os.ModeSymlink != 0 || !info.IsDir():
		return "", false, fmt.Errorf("private attachment root is not a real directory")
	}
	if err := os.Chmod(path, 0o700); err != nil {
		if created {
			_ = os.Remove(path)
		}
		return "", false, fmt.Errorf("secure private attachment root: %w", err)
	}
	return path, created, nil
}
