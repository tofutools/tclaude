package session

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWorkDirFromToolUse(t *testing.T) {
	cases := []struct {
		name    string
		tool    string
		input   string
		cwd     string
		wantDir string
		wantOK  bool
	}{
		{
			name:    "Edit with absolute path",
			tool:    "Edit",
			input:   `{"file_path":"/home/u/git/repo/pkg/foo/bar.go"}`,
			wantDir: "/home/u/git/repo/pkg/foo",
			wantOK:  true,
		},
		{
			name:    "Write with absolute path",
			tool:    "Write",
			input:   `{"file_path":"/home/u/git/repo/main.go","content":"x"}`,
			wantDir: "/home/u/git/repo",
			wantOK:  true,
		},
		{
			name:    "NotebookEdit uses notebook_path",
			tool:    "NotebookEdit",
			input:   `{"notebook_path":"/home/u/nb/run.ipynb"}`,
			wantDir: "/home/u/nb",
			wantOK:  true,
		},
		{
			name:    "relative path resolves against cwd",
			tool:    "Edit",
			input:   `{"file_path":"pkg/foo/bar.go"}`,
			cwd:     "/home/u/git/repo",
			wantDir: "/home/u/git/repo/pkg/foo",
			wantOK:  true,
		},
		{
			name:   "relative path with no cwd is unusable",
			tool:   "Edit",
			input:  `{"file_path":"pkg/foo/bar.go"}`,
			wantOK: false,
		},
		{
			name:   "Read is not a build signal",
			tool:   "Read",
			input:  `{"file_path":"/home/u/git/repo/README.md"}`,
			wantOK: false,
		},
		{
			name:   "Bash is not a build signal",
			tool:   "Bash",
			input:  `{"command":"cd /somewhere && ls"}`,
			wantOK: false,
		},
		{
			name:   "Edit with no file_path",
			tool:   "Edit",
			input:  `{"old_string":"a","new_string":"b"}`,
			wantOK: false,
		},
		{
			name:   "empty tool input",
			tool:   "Edit",
			input:  ``,
			wantOK: false,
		},
		{
			name:   "malformed tool input",
			tool:   "Edit",
			input:  `{not json`,
			wantOK: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var raw json.RawMessage
			if tc.input != "" {
				raw = json.RawMessage(tc.input)
			}
			dir, ok := WorkDirFromToolUse(tc.tool, raw, tc.cwd)
			assert.Equal(t, tc.wantOK, ok, "ok")
			if tc.wantOK {
				assert.Equal(t, tc.wantDir, dir, "dir")
			}
		})
	}
}

func TestGitLocationOf(t *testing.T) {
	// Empty input and a dir outside any git repo both resolve to
	// ("", "") — "not in a repo", recorded faithfully, never an error.
	root, branch := GitLocationOf("")
	assert.Empty(t, root)
	assert.Empty(t, branch)

	root, branch = GitLocationOf(t.TempDir())
	assert.Empty(t, root, "worktree root of a non-repo dir")
	assert.Empty(t, branch, "branch of a non-repo dir")

	// A real repo on a known branch resolves both — including when
	// asked about a nested subdirectory rather than the repo root.
	repo := t.TempDir()
	repo, err := filepath.EvalSymlinks(repo)
	require.NoError(t, err, "resolve symlinks")
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		require.NoErrorf(t, cmd.Run(), "git %v", args)
	}
	git("init", "-b", "trunk")
	git("config", "user.email", "t@example.com")
	git("config", "user.name", "T")
	require.NoError(t, os.WriteFile(filepath.Join(repo, "f.txt"), []byte("x"), 0o644))
	git("add", ".")
	git("commit", "-m", "init")

	root, branch = GitLocationOf(repo)
	assert.Equal(t, repo, root, "repo root")
	assert.Equal(t, "trunk", branch, "branch")

	sub := filepath.Join(repo, "pkg", "deep")
	require.NoError(t, os.MkdirAll(sub, 0o755))
	root, branch = GitLocationOf(sub)
	assert.Equal(t, repo, root, "repo root resolved from a subdir")
	assert.Equal(t, "trunk", branch, "branch resolved from a subdir")
}
