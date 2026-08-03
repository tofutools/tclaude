package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/claude/common/agentipc"
)

// The socket name reaches tmux argv, a filepath.Join for the sandbox
// socket-deny path, and shell command strings — so the gate is narrow on
// purpose. Anything that could turn the name into a different path component,
// or that a shell would treat as more than one word, must be refused rather
// than escaped.
func TestValidTmuxSocketName(t *testing.T) {
	for _, tc := range []struct {
		name  string
		input string
		want  bool
	}{
		{"default", DefaultTmuxSocketName, true},
		{"dots underscores hyphens and digits", "tclaude_work-2.0", true},
		{"single char", "t", true},
		{"max length", strings.Repeat("a", maxTmuxSocketNameLen), true},
		{"empty", "", false},
		{"too long", strings.Repeat("a", maxTmuxSocketNameLen+1), false},
		{"dot entry", ".", false},
		{"dotdot entry", "..", false},
		{"path separator", "sub/tclaude", false},
		{"parent traversal", "../tclaude", false},
		{"absolute path", "/tmp/tclaude", false},
		{"space", "my socket", false},
		{"shell metacharacter", "tclaude;id", false},
		{"single quote", "tclaude'", false},
		{"newline", "tclaude\n", false},
		{"nul", "tclaude\x00", false},
		{"non-ascii", "tclaudé", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, ValidTmuxSocketName(tc.input))
		})
	}
}

// Resolution is nil-safe and fails closed: an unusable name degrades to the
// historical "tclaude" socket rather than pointing tclaude at a server it
// cannot reach.
func TestResolvedTmuxSocketName(t *testing.T) {
	assert.Equal(t, DefaultTmuxSocketName, (*Config)(nil).ResolvedTmuxSocketName(), "nil config")
	assert.Equal(t, DefaultTmuxSocketName, (&Config{}).ResolvedTmuxSocketName(), "absent tmux block")
	assert.Equal(t, DefaultTmuxSocketName, (&Config{Tmux: &TmuxConfig{}}).ResolvedTmuxSocketName(), "empty name")
	assert.Equal(t, DefaultTmuxSocketName, (&Config{Tmux: &TmuxConfig{SocketName: "   "}}).ResolvedTmuxSocketName(), "blank name")
	assert.Equal(t, DefaultTmuxSocketName, (&Config{Tmux: &TmuxConfig{SocketName: "../escape"}}).ResolvedTmuxSocketName(), "invalid name fails closed")
	assert.Equal(t, "work", (&Config{Tmux: &TmuxConfig{SocketName: "work"}}).ResolvedTmuxSocketName())
	assert.Equal(t, "work", (&Config{Tmux: &TmuxConfig{SocketName: "  work  "}}).ResolvedTmuxSocketName(), "surrounding space trimmed")
}

// Validate refuses a non-blank name that can never take effect, so the
// dashboard's config editor reports the typo instead of saving a value every
// process would silently ignore.
func TestValidateTmuxSocketName(t *testing.T) {
	assert.Empty(t, Validate(&Config{}), "absent tmux block")
	assert.Empty(t, Validate(&Config{Tmux: &TmuxConfig{}}), "blank means default")
	assert.Empty(t, Validate(&Config{Tmux: &TmuxConfig{SocketName: "work-2"}}))

	errs := Validate(&Config{Tmux: &TmuxConfig{SocketName: "../escape"}})
	assert.Len(t, errs, 1)
	assert.Contains(t, errs[0], "tmux.socket_name")
}

// PrivateConfigUnreadable answers "is every value Load gave me a default
// because I never got to look?" — callers fail closed on it, so a false
// positive changes behavior for a process that is not blind at all.
func TestPrivateConfigUnreadable(t *testing.T) {
	writeConfig := func(t *testing.T) string {
		t.Helper()
		home := t.TempDir()
		t.Setenv("HOME", home)
		t.Setenv("USERPROFILE", home)
		return home
	}

	t.Run("ordinary process", func(t *testing.T) {
		writeConfig(t)
		t.Setenv(agentipc.SocketEnv, "")
		assert.False(t, PrivateConfigUnreadable(), "no agent marker, nothing to probe")
	})

	t.Run("sandboxed agent that cannot reach the config", func(t *testing.T) {
		home := writeConfig(t)
		t.Setenv(agentipc.SocketEnv, filepath.Join(home, ".tclaude", "api", "agentd.sock"))
		assert.True(t, PrivateConfigUnreadable())
	})

	t.Run("daemon with a custom socket that can", func(t *testing.T) {
		home := writeConfig(t)
		dir := filepath.Join(home, ".tclaude", "data")
		require.NoError(t, os.MkdirAll(dir, 0o700))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{}`), 0o600))
		t.Setenv(agentipc.SocketEnv, filepath.Join(home, "custom-agentd.sock"))
		assert.False(t, PrivateConfigUnreadable(),
			"--socket exports the same marker but the config is right there")
	})
}

// A configured socket name must survive the JSON round trip the config file and
// the dashboard's editor both go through, and an absent block must stay absent
// rather than being written back as an empty object.
func TestTmuxConfigRoundTrip(t *testing.T) {
	var cfg Config
	require.NoError(t, json.Unmarshal([]byte(`{"tmux":{"socket_name":"work"}}`), &cfg))
	Normalize(&cfg)
	assert.Equal(t, "work", cfg.ResolvedTmuxSocketName())

	configured, err := json.MarshalIndent(&Config{Tmux: &TmuxConfig{SocketName: "work"}}, "", "  ")
	require.NoError(t, err)
	assert.Contains(t, string(configured), `"socket_name": "work"`)

	absent, err := json.MarshalIndent(&Config{}, "", "  ")
	require.NoError(t, err)
	assert.NotContains(t, string(absent), "tmux")
}
