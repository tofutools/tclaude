package agentd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

func TestCodexApprovalProfileOwnedByLivePane_RequiresExactWrapperPath(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "Codex Home $x 'quote' \\ `tick`")
	require.NoError(t, os.MkdirAll(dir, 0o700))
	path := filepath.Join(dir, "tclaude-agent-1111111111111111.config.toml")
	profile := "tclaude-agent-1111111111111111"
	quotedPath := clcommon.ShellQuoteArg(path)
	legacy := renderTmuxPaneStart(`codex -p ` + profile + `; tclaude_launch_status=$?; rm -f -- ` + quotedPath + `; exit $tclaude_launch_status`)
	current := renderTmuxPaneStart(`codex resume conv -p ` + profile + `; tclaude_resume_status=$?; cleanup --path ` + quotedPath +
		` || { cleanup --help || rm -f -- ` + quotedPath + `; }; exit $tclaude_resume_status`)

	assert.True(t, codexApprovalProfileOwnedByLivePane(path, []string{legacy}))
	assert.True(t, codexApprovalProfileOwnedByLivePane(path, []string{current}))

	otherHome := filepath.Join(t.TempDir(), filepath.Base(path))
	wrongHome := renderTmuxPaneStart(`codex -p ` + profile + `; tclaude_launch_status=$?; rm -f -- ` +
		clcommon.ShellQuoteArg(otherHome) + `; exit $tclaude_launch_status`)
	assert.False(t, codexApprovalProfileOwnedByLivePane(path, []string{wrongHome}),
		"a same-named profile under another CODEX_HOME must not match")

	spoof := renderTmuxPaneStart(`printf ' -p ` + profile + `'; tclaude_launch_status=$?; rm -f -- ` +
		quotedPath + `; exit $tclaude_launch_status`)
	assert.False(t, codexApprovalProfileOwnedByLivePane(path, []string{spoof}),
		"an unrelated pane with a wrapper-shaped tail must not match")
	quotedCodexSpoof := renderTmuxPaneStart(`printf '; codex -p ` + profile + `'; tclaude_launch_status=$?; rm -f -- ` +
		quotedPath + `; exit $tclaude_launch_status`)
	assert.False(t, codexApprovalProfileOwnedByLivePane(path, []string{quotedCodexSpoof}),
		"a quoted discussion of a Codex invocation must not match")

	otherPath := filepath.Join(dir, "tclaude-agent-2222222222222222.config.toml")
	promptThenOtherCleanup := renderTmuxPaneStart(`codex -p ` + profile + ` 'prompt says rm -f -- ` + quotedPath +
		`'; tclaude_launch_status=$?; rm -f -- ` + otherPath + `; exit $tclaude_launch_status`)
	assert.False(t, codexApprovalProfileOwnedByLivePane(path, []string{promptThenOtherCleanup}),
		"a prompt mention must not outrank the wrapper's final cleanup path")
}

func renderTmuxPaneStart(raw string) string {
	escaped := strings.NewReplacer(
		`\`, `\\`,
		`"`, `\"`,
		`$`, `\$`,
		"`", "\\`",
	).Replace(raw)
	return `sh -c "` + escaped + `"`
}

func TestCodexApprovalLivePaneStarts_ExcludesDeadPanes(t *testing.T) {
	got := codexApprovalLivePaneStarts("0\tsh -c live\n1\tsh -c dead\n\tmalformed\n")
	assert.Equal(t, []string{"sh -c live"}, got)
}

func TestCodexApprovalMonitor_StartupReconcilesOnlyLivePaneProfile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", filepath.Join(home, ".codex"))
	liveName, livePath, err := harness.EnsureCodexAgentLaunchProfile(nil, "1111111111111111")
	require.NoError(t, err)
	_, stalePath, err := harness.EnsureCodexAgentLaunchProfile(nil, "2222222222222222")
	require.NoError(t, err)
	appendApproval := func(path, tool string) {
		t.Helper()
		f, openErr := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
		require.NoError(t, openErr)
		_, writeErr := f.WriteString("\n[apps.asdk_app_69a089a326dc8191b32a3f2553f5be2c.tools.\"" + tool + "\"]\n" +
			"approval_mode = \"approve\"\n")
		require.NoError(t, writeErr)
		require.NoError(t, f.Close())
	}
	appendApproval(livePath, "linear.live_issue")
	appendApproval(stalePath, "linear.stale_issue")

	previousPaneStarts := codexApprovalPaneStartCommands
	codexApprovalPaneStartCommands = func() ([]byte, error) {
		command := "0\t" + renderTmuxPaneStart("codex -p "+liveName+"; tclaude_launch_status=$?; rm -f -- "+
			clcommon.ShellQuoteArg(livePath)+"; exit $tclaude_launch_status") + "\n"
		return []byte(command), nil
	}
	t.Cleanup(func() { codexApprovalPaneStartCommands = previousPaneStarts })

	stop := make(chan struct{})
	monitor := startCodexApprovalMonitor(stop)
	if monitor == nil {
		t.Skip("fsnotify watcher unavailable in this environment")
	}
	t.Cleanup(func() {
		close(stop)
		monitor.wait()
	})

	configPath := filepath.Join(home, ".codex", "config.toml")
	require.Eventually(t, func() bool {
		data, readErr := os.ReadFile(configPath)
		return readErr == nil && strings.Contains(string(data), "linear.live_issue")
	}, 10*time.Second, 10*time.Millisecond)
	data, err := os.ReadFile(configPath)
	require.NoError(t, err)
	assert.NotContains(t, string(data), "linear.stale_issue")
}

// Script-launch pane shape (`sh <launch-script> <profile-path>`): the whole
// bootstrap lives in a self-deleting script, so the profile path rides the
// pane argv as an inert marker (session.CodexProfileMarkerArgs) and startup
// recovery must match it there. The inline `sh -c "…"` shape above remains
// recognized for panes launched by a pre-script tclaude.
func TestCodexApprovalProfileOwnership_ScriptLaunchShape(t *testing.T) {
	dir := "/Users/u/.codex"
	profile := filepath.Join(dir, "tclaude-agent-1111111111111111.config.toml")
	script := "/Users/u/.tclaude/data/launch-scripts/launch-123456789"

	assert.True(t, codexApprovalProfileOwnedByLivePane(profile,
		[]string{"sh " + script + " " + profile}),
		"plain script-launch argv must claim its profile")

	// tmux double-quotes argv words containing specials (args_escape), with
	// backslash escapes for \ " $ and backtick — a CODEX_HOME with a space.
	spaceyProfile := "/Users/u/My Codex/tclaude-agent-2222222222222222.config.toml"
	assert.True(t, codexApprovalProfileOwnedByLivePane(spaceyProfile,
		[]string{`sh ` + script + ` "/Users/u/My Codex/tclaude-agent-2222222222222222.config.toml"`}),
		"a tmux-quoted profile word must decode and match")

	assert.False(t, codexApprovalProfileOwnedByLivePane(profile,
		[]string{"sh " + script + " " + filepath.Join(dir, "tclaude-agent-3333333333333333.config.toml")}),
		"a different profile's marker must not claim this one")

	assert.False(t, codexApprovalProfileOwnedByLivePane(profile,
		[]string{"sh /tmp/evil-script " + profile}),
		"only a tclaude launch-script word may carry a claim")

	assert.False(t, codexApprovalProfileOwnedByLivePane(profile,
		[]string{"bash " + script + " " + profile}),
		"a non-sh word 0 must not match")

	assert.False(t, codexApprovalProfileOwnedByLivePane(profile,
		[]string{"sh " + script}),
		"a script launch without a marker claims nothing")

	// A mixed fleet: one legacy inline pane, one script pane — each shape
	// claims exactly its own profile.
	legacy := renderTmuxPaneStart(`codex -p tclaude-agent-1111111111111111; tclaude_launch_status=$?; rm -f -- ` +
		clcommon.ShellQuoteArg(profile) + `; exit $tclaude_launch_status`)
	assert.True(t, codexApprovalProfileOwnedByLivePane(profile, []string{legacy, "sh " + script}),
		"legacy inline shape must still be recognized alongside script panes")
}
