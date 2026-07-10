package common

import (
	"os"
	"path/filepath"
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
