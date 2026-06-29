package setup

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/claude/common/config"
)

// notifConfig loads the on-disk notifications block (or nil when no config
// file / block exists) — the assertion surface for the setup tests.
func notifConfig(t *testing.T) *config.NotificationConfig {
	t.Helper()
	if !config.NotificationsPresent() {
		return nil
	}
	cfg, err := config.Load()
	require.NoError(t, err)
	return cfg.Notifications
}

// withStdin points os.Stdin at a pipe pre-loaded with input for the
// duration of fn, so a non-assumeYes prompt reads a scripted answer.
func withStdin(t *testing.T, input string, fn func()) {
	t.Helper()
	orig := os.Stdin
	r, w, err := os.Pipe()
	require.NoError(t, err)
	_, err = w.WriteString(input)
	require.NoError(t, err)
	require.NoError(t, w.Close())
	os.Stdin = r
	defer func() { os.Stdin = orig }()
	fn()
}

// THE regression: a user who deliberately disabled notifications must keep
// them disabled across repeated `tclaude setup` runs — even under --yes,
// which previously force-flipped Enabled back to true.
func TestConfigureNotifications_DisabledStaysDisabled(t *testing.T) {
	tempHome(t)

	// Pre-seed a configured-but-disabled block (what a user who turned
	// notifications off looks like on disk).
	cfg := config.DefaultConfig()
	cfg.Notifications.Enabled = false
	require.NoError(t, config.Save(cfg))

	out := captureStdout(t, func() {
		configureNotifications(&Params{Yes: true})
	})
	assert.Contains(t, out, "leaving them off")
	assert.NotContains(t, out, "Notifications enabled")

	n := notifConfig(t)
	require.NotNil(t, n)
	assert.False(t, n.Enabled, "a deliberately disabled block must not be re-enabled by setup")
}

// An already-enabled configured block is left exactly as-is, and setup does
// not rewrite the file when there is nothing to merge.
func TestConfigureNotifications_EnabledPreserved(t *testing.T) {
	tempHome(t)

	cfg := config.DefaultConfig()
	cfg.Notifications.Enabled = true
	cfg.Notifications.CooldownSeconds = 42
	require.NoError(t, config.Save(cfg))

	out := captureStdout(t, func() {
		configureNotifications(&Params{Yes: true})
	})
	assert.Contains(t, out, "already enabled")

	n := notifConfig(t)
	require.NotNil(t, n)
	assert.True(t, n.Enabled)
	assert.Equal(t, 42, n.CooldownSeconds, "existing settings must survive")
}

// First run (no notifications block on disk) still offers to enable, and
// --yes opts in — the onboarding path is preserved.
func TestConfigureNotifications_FreshEnable(t *testing.T) {
	tempHome(t)
	require.False(t, config.NotificationsPresent(), "precondition: no block yet")

	out := captureStdout(t, func() {
		configureNotifications(&Params{Yes: true})
	})
	assert.Contains(t, out, "Notifications enabled")

	n := notifConfig(t)
	require.NotNil(t, n)
	assert.True(t, n.Enabled)
	for _, ty := range config.NotifyTypes {
		assert.True(t, n.NotifyTypeEnabled(ty), "fresh enable seeds default category %q", ty)
	}
}

// First run + decline writes nothing: the user is not opted in and no block
// is persisted behind their back.
func TestConfigureNotifications_FreshDeclineWritesNothing(t *testing.T) {
	tempHome(t)

	out := captureStdout(t, func() {
		withStdin(t, "n\n", func() {
			configureNotifications(&Params{Yes: false})
		})
	})
	assert.Contains(t, out, "not enabled")

	assert.False(t, config.NotificationsPresent(), "decline must not write a notifications block")
}

// The additive merge: a config predating a newer category gains that
// category while every existing rule (including a from-specific advanced
// rule), the cooldown and the Enabled state are preserved untouched.
func TestConfigureNotifications_MergesMissingCategory(t *testing.T) {
	tempHome(t)

	cfg := config.DefaultConfig()
	cfg.Notifications.Enabled = true
	cfg.Notifications.CooldownSeconds = 7
	// An "older" config: missing "exited", plus a custom advanced rule.
	cfg.Notifications.Transitions = []config.TransitionRule{
		{From: "*", To: "idle"},
		{From: "*", To: "awaiting_permission"},
		{From: "*", To: "awaiting_input"},
		{From: "*", To: "error"},
		{From: "working", To: "idle"}, // advanced rule, must round-trip
	}
	require.NoError(t, config.Save(cfg))

	out := captureStdout(t, func() {
		configureNotifications(&Params{Yes: true})
	})
	assert.Contains(t, out, "Added new notification category: exited")

	n := notifConfig(t)
	require.NotNil(t, n)
	assert.True(t, n.Enabled, "merge must not change Enabled")
	assert.Equal(t, 7, n.CooldownSeconds, "merge must not touch cooldown")
	assert.True(t, n.NotifyTypeEnabled("exited"), "missing category must be added")
	assert.Contains(t, n.Transitions, config.TransitionRule{From: "working", To: "idle"},
		"advanced rules must be preserved")
}

// Disabling-then-merging: a disabled block still picks up new categories
// (so re-enabling later includes them) without being flipped on.
func TestConfigureNotifications_MergesIntoDisabledBlock(t *testing.T) {
	tempHome(t)

	cfg := config.DefaultConfig()
	cfg.Notifications.Enabled = false
	cfg.Notifications.Transitions = []config.TransitionRule{
		{From: "*", To: "idle"}, // only one category
	}
	require.NoError(t, config.Save(cfg))

	configureNotifications(&Params{Yes: true})

	n := notifConfig(t)
	require.NotNil(t, n)
	assert.False(t, n.Enabled, "merge must not enable a disabled block")
	for _, ty := range config.NotifyTypes {
		assert.True(t, n.NotifyTypeEnabled(ty), "category %q merged into disabled block", ty)
	}
}
