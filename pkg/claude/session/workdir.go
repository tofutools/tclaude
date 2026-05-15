package session

import (
	"encoding/json"
	"os/exec"
	"path/filepath"
	"strings"
)

// fileMutatingTools are the Claude Code tool names that signal "the
// agent is building here". Read / Grep / Glob are deliberately absent:
// an agent reads and searches files all over the place while
// investigating, but it *edits* files in the repo it's actually
// working on. The edit is the signal we want.
var fileMutatingTools = map[string]bool{
	"Edit":         true,
	"Write":        true,
	"MultiEdit":    true,
	"NotebookEdit": true,
}

// WorkDirFromToolUse inspects a Claude Code PostToolUse payload and
// returns the directory the tool acted in, when it can tell.
//
// It looks only at the file-mutating tools (see fileMutatingTools) and
// returns the parent directory of the file they touched. Relative
// paths are resolved against cwd — Claude Code's launch directory, as
// reported in the hook payload. Returns ("", false) when the tool
// isn't a recognised file-mutator or carries no usable path.
//
// Pure function: no I/O, no globals beyond the static tool set, so the
// derivation is straightforward to unit-test.
func WorkDirFromToolUse(toolName string, toolInput json.RawMessage, cwd string) (string, bool) {
	if !fileMutatingTools[toolName] || len(toolInput) == 0 {
		return "", false
	}
	var in struct {
		FilePath     string `json:"file_path"`
		NotebookPath string `json:"notebook_path"`
	}
	if err := json.Unmarshal(toolInput, &in); err != nil {
		return "", false
	}
	path := in.FilePath
	if path == "" {
		path = in.NotebookPath
	}
	if path == "" {
		return "", false
	}
	if !filepath.IsAbs(path) {
		if cwd == "" {
			return "", false
		}
		path = filepath.Join(cwd, path)
	}
	return filepath.Dir(filepath.Clean(path)), true
}

// GitLocationOf resolves dir to its git worktree root and the branch
// checked out there. It shells out to git twice; both calls are
// best-effort — a dir outside any git repo (or a missing git binary)
// yields ("", ""), which is recorded faithfully as "not in a repo"
// rather than treated as an error. A detached HEAD reports an empty
// branch (there's no branch name to show).
//
// The PostToolUse hook calls this once per file edit so an agent's
// current branch is stamped into agent_workdir at edit time — read
// surfaces then never have to run git themselves.
func GitLocationOf(dir string) (worktreeRoot, branch string) {
	if dir == "" {
		return "", ""
	}
	if out, err := exec.Command("git", "-C", dir, "rev-parse", "--show-toplevel").Output(); err == nil {
		worktreeRoot = strings.TrimSpace(string(out))
	}
	if worktreeRoot == "" {
		return "", ""
	}
	if out, err := exec.Command("git", "-C", dir, "rev-parse", "--abbrev-ref", "HEAD").Output(); err == nil {
		branch = strings.TrimSpace(string(out))
		if branch == "HEAD" {
			branch = "" // detached HEAD — no branch name to report
		}
	}
	return worktreeRoot, branch
}
