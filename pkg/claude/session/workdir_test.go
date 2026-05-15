package session

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
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
