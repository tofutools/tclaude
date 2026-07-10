package common

import (
	"net"
	"os"
	"path/filepath"
	"time"
)

// tclaude's on-disk home is split by ACCESS CLASS so a sandboxed agent can be
// denied the private daemon state as one subtree while still reaching the
// agent-facing socket:
//
//	~/.tclaude/
//	  data/   private/internal — denied (read+write) to sandboxed agents.
//	          db.sqlite (+ -wal/-shm/*.bak), operator_token, processes/,
//	          output.log, debug.log, config.json, and all other daemon state.
//	  api/    agent-reachable surface — the agentd Unix socket lives here.
//
// The Claude/Codex sandbox resolves paths deny-before-allow, so a whole-tree
// deny on ~/.tclaude could not be allow-carved back to expose api/. Keeping the
// socket reachable while state stays denied REQUIRES the deny to be narrowed to
// ~/.tclaude/data — which is only sound if everything sensitive lives under
// data/. These three helpers are the single source of truth for that layout.
//
// They live in pkg/common (a leaf package that imports no other tclaude
// package) so every state-owning package — including pkg/common's own logging,
// which cannot import the higher-level config package without an import cycle —
// can resolve the same paths. config.DataDir()/APIDir() delegate here.

// TclaudeDir returns the tclaude root directory (~/.tclaude), or "" if the home
// directory cannot be resolved.
func TclaudeDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".tclaude")
}

// TclaudeDataDir returns the private state directory (~/.tclaude/data) that is
// denied to sandboxed agents, or "" if the home directory cannot be resolved.
func TclaudeDataDir() string {
	root := TclaudeDir()
	if root == "" {
		return ""
	}
	return filepath.Join(root, "data")
}

// TclaudeAPIDir returns the agent-reachable directory (~/.tclaude/api) that
// holds the agentd socket, or "" if the home directory cannot be resolved.
func TclaudeAPIDir() string {
	root := TclaudeDir()
	if root == "" {
		return ""
	}
	return filepath.Join(root, "api")
}

// TclaudeStatePath returns the active location for one private state entry.
// Normally that is data/<name>. While a pre-split daemon is live — or when a
// legacy entry still exists and its destination does not — callers stay on the
// legacy root path. The existence fallback closes the race where the old
// daemon exits after a command's migration preflight but before the command
// resolves its own path.
func TclaudeStatePath(name string) string {
	root := TclaudeDir()
	dataDir := TclaudeDataDir()
	if root == "" || dataDir == "" {
		return ""
	}
	oldPath := filepath.Join(root, name)
	newPath := filepath.Join(dataDir, name)
	if PreSplitAgentdReachable() {
		return oldPath
	}
	if _, oldErr := os.Lstat(oldPath); oldErr == nil {
		if _, newErr := os.Lstat(newPath); os.IsNotExist(newErr) {
			return oldPath
		}
	}
	return newPath
}

// PreSplitAgentdReachable reports whether an older daemon is reachable only
// through one of the pre-split socket paths. Migration code must not rename
// state out from under such a daemon: open file descriptors survive rename on
// Unix, but the daemon can subsequently reopen/recreate a sidecar or state file
// at the old name and split the live state across both layouts.
func PreSplitAgentdReachable() bool {
	root := TclaudeDir()
	home, err := os.UserHomeDir()
	if err != nil || root == "" {
		return false
	}
	canonical := filepath.Join(TclaudeAPIDir(), "agentd.sock")
	if unixSocketReachable(canonical) {
		return false
	}
	return unixSocketReachable(filepath.Join(home, ".tclaude-agentd.sock")) ||
		unixSocketReachable(filepath.Join(root, "agentd.sock"))
}

func unixSocketReachable(path string) bool {
	if path == "" {
		return false
	}
	c, err := net.DialTimeout("unix", path, 100*time.Millisecond)
	if err != nil {
		return false
	}
	_ = c.Close()
	return true
}
