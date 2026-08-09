package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRewriteCodexAppServerClientMessagePreservesManagedToolProfile(t *testing.T) {
	const profile = "tclaude-agent-0123456789abcdef"
	tests := []struct {
		name       string
		input      string
		wantField  string
		wantAbsent string
		unchanged  bool
	}{
		{
			name:      "fresh remote thread",
			input:     `{"id":1,"method":"thread/start","params":{"cwd":"/repo","sandbox":"workspace-write"}}`,
			wantField: "permissions", wantAbsent: "sandbox",
		},
		{
			name:      "remote resume",
			input:     `{"id":2,"method":"thread/resume","params":{"threadId":"thread","sandbox":"read-only"}}`,
			wantField: "permissions", wantAbsent: "sandbox",
		},
		{
			name:      "turn legacy override",
			input:     `{"id":3,"method":"turn/start","params":{"threadId":"thread","input":[],"sandboxPolicy":{"type":"workspaceWrite"}}}`,
			wantField: "permissions", wantAbsent: "sandboxPolicy",
		},
		{
			name:      "named turn override is clamped",
			input:     `{"id":4,"method":"thread/settings/update","params":{"threadId":"thread","permissions":"other"}}`,
			wantField: "permissions",
		},
		{
			name:      "typed agentd turn restates managed tool profile",
			input:     `{"id":5,"method":"turn/start","params":{"threadId":"thread","input":[{"type":"text","text":"hello"}]}}`,
			wantField: "permissions",
		},
		{
			name:      "unrelated call",
			input:     `{"id":6,"method":"thread/read","params":{"threadId":"thread"}}`,
			unchanged: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := rewriteCodexAppServerClientMessage([]byte(tc.input), profile)
			require.NoError(t, err)
			if tc.unchanged {
				assert.Equal(t, tc.input, string(got))
				return
			}
			var request struct {
				Params map[string]json.RawMessage `json:"params"`
			}
			require.NoError(t, json.Unmarshal(got, &request))
			require.Contains(t, request.Params, tc.wantField)
			assert.JSONEq(t, `"`+profile+`"`, string(request.Params[tc.wantField]))
			if tc.wantAbsent != "" {
				assert.NotContains(t, request.Params, tc.wantAbsent)
			}
		})
	}
}

func TestConsumeCodexAppServerTokenIsOneShotAndRejectsUnsafeFiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "capability")
	require.NoError(t, os.WriteFile(path, []byte("launch-secret\n"), 0o600))

	token, err := consumeCodexAppServerToken(path)
	require.NoError(t, err)
	assert.Equal(t, "launch-secret", token)
	_, err = consumeCodexAppServerToken(path)
	assert.Error(t, err, "the handoff must be consumed exactly once")

	unsafe := filepath.Join(dir, "unsafe")
	require.NoError(t, os.WriteFile(unsafe, []byte("secret"), 0o644))
	_, err = consumeCodexAppServerToken(unsafe)
	assert.ErrorContains(t, err, "owned private file")

	target := filepath.Join(dir, "target")
	require.NoError(t, os.WriteFile(target, []byte("secret"), 0o600))
	symlink := filepath.Join(dir, "symlink")
	require.NoError(t, os.Symlink(target, symlink))
	_, err = consumeCodexAppServerToken(symlink)
	assert.ErrorContains(t, err, "owned private file")
}
