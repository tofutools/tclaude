package common

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/claude/common/config"
)

// writeTmuxSocketNameConfig points HOME at a fixture home holding a config.json
// with the given tmux.socket_name (or no tmux block at all when name is ""),
// and clears the resolved-name cache so the next lookup reads it.
func writeTmuxSocketNameConfig(t *testing.T, name string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)

	cfg := &config.Config{}
	if name != "" {
		cfg.Tmux = &config.TmuxConfig{SocketName: name}
	}
	data, err := json.Marshal(cfg)
	require.NoError(t, err)
	dir := filepath.Join(home, ".tclaude", "data")
	require.NoError(t, os.MkdirAll(dir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config.json"), data, 0o600))

	t.Cleanup(SetTmuxSocketNameForTest(""))
}

// With no tmux block the socket name stays the historical "tclaude", so an
// unconfigured install keeps talking to exactly the server it always did.
func TestTmuxSocketNameDefaultsToTclaude(t *testing.T) {
	writeTmuxSocketNameConfig(t, "")
	assert.Equal(t, "tclaude", config.DefaultTmuxSocketName)
	assert.Equal(t, config.DefaultTmuxSocketName, TmuxSocketName())
}

// The end-to-end wiring: a name in the operator's config.json is what tmux is
// actually invoked with.
func TestTmuxSocketNameFromConfigFile(t *testing.T) {
	writeTmuxSocketNameConfig(t, "work-2")
	assert.Equal(t, "work-2", TmuxSocketName())
	assert.Equal(t, []string{"-L", "work-2", "list-sessions"}, TmuxArgs("list-sessions"))
}

// A name that cannot be a socket name must not reach tmux: resolution fails
// closed to the default rather than passing the value through.
func TestTmuxSocketNameRejectsUnusableConfigValue(t *testing.T) {
	writeTmuxSocketNameConfig(t, "../escape")
	assert.Equal(t, config.DefaultTmuxSocketName, TmuxSocketName())
}

// Every tmux invocation goes through TmuxArgs, so the resolved name has to
// reach the `-L` flag there rather than at each of the ~40 call sites.
func TestTmuxArgsUsesResolvedSocketName(t *testing.T) {
	defer SetTmuxSocketNameForTest("work-2")()

	assert.Equal(t, []string{"-L", "work-2"}, TmuxArgs())
	assert.Equal(t, []string{"tmux", "-L", "work-2", "has-session", "-t", "=x"},
		LiveTmux{}.Command("has-session", "-t", "=x").Args)
}

// The override is scoped: restoring puts the process back on the previous
// value, so one test cannot leak a socket name into the next.
func TestSetTmuxSocketNameForTestRestores(t *testing.T) {
	before := TmuxSocketName()
	restore := SetTmuxSocketNameForTest("temporary")
	assert.Equal(t, "temporary", TmuxSocketName())
	restore()
	assert.Equal(t, before, TmuxSocketName())
}
