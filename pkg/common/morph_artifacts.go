package common

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// MorphArtifactsBase is the daemon-owned parent of the per-agent directories
// a cross-harness morph writes its transcript, briefing, loss report and
// media into. Like SpawnAttachmentsPrivateBase it lives in the agent-reachable
// API tree, because the morphed agent is pointed at these files, and is hidden
// from every tclaude-layer launch; each launch reopens only its own agent's
// child.
func MorphArtifactsBase() string {
	return filepath.Join(TclaudeAPIDir(), "morph")
}

// MorphArtifactsDir returns the stable directory for one agent. It is keyed
// by the agent id, not the session, because a morphed agent reads it from
// every later launch.
func MorphArtifactsDir(agentID string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(agentID)))
	return filepath.Join(MorphArtifactsBase(), hex.EncodeToString(sum[:]))
}

// ExistingMorphArtifactsDir returns the agent's morph directory when a morph
// has created it as a real directory, else "". A launch grants it only then.
func ExistingMorphArtifactsDir(agentID string) string {
	if strings.TrimSpace(agentID) == "" {
		return ""
	}
	base, path := MorphArtifactsBase(), MorphArtifactsDir(agentID)
	for _, p := range []string{base, path} {
		info, err := os.Lstat(p)
		if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return ""
		}
	}
	return path
}

// PrepareMorphArtifactsDir materializes the agent's morph directory without
// accepting a symlink in place of the parent or the child.
func PrepareMorphArtifactsDir(agentID string) (string, error) {
	if strings.TrimSpace(agentID) == "" {
		return "", fmt.Errorf("morph artifacts need an agent id")
	}
	base := MorphArtifactsBase()
	if !filepath.IsAbs(base) {
		return "", fmt.Errorf("morph artifact parent is not absolute")
	}
	if err := os.MkdirAll(base, 0o700); err != nil {
		return "", fmt.Errorf("create morph artifact parent: %w", err)
	}
	if info, err := os.Lstat(base); err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return "", fmt.Errorf("morph artifact parent is not a real directory")
	}
	path := MorphArtifactsDir(agentID)
	info, err := os.Lstat(path)
	switch {
	case os.IsNotExist(err):
		if err := os.Mkdir(path, 0o700); err != nil {
			return "", fmt.Errorf("create morph artifact directory: %w", err)
		}
	case err != nil:
		return "", fmt.Errorf("inspect morph artifact directory: %w", err)
	case info.Mode()&os.ModeSymlink != 0 || !info.IsDir():
		return "", fmt.Errorf("morph artifact directory is not a real directory")
	}
	if err := os.Chmod(path, 0o700); err != nil {
		return "", fmt.Errorf("secure morph artifact directory: %w", err)
	}
	return path, nil
}
