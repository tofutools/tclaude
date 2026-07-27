//go:build linux

package session

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The heuristic decides only whether a surface may say "likely", so its job is
// to stay quiet on every host shape that is not the confirmed one — including
// the shapes an operator creates by acting on the guide it links to.
func TestLikelyAppArmorNestedBwrapBlockHostShapes(t *testing.T) {
	shape := func(t *testing.T) (string, string, string, string) {
		t.Helper()
		root := t.TempDir()
		enabled := filepath.Join(root, "enabled")
		policy := filepath.Join(root, "bwrap-userns-restrict")
		disabled := filepath.Join(root, "disable", "bwrap-userns-restrict")
		complain := filepath.Join(root, "force-complain", "bwrap-userns-restrict")
		require.NoError(t, os.WriteFile(enabled, []byte("Y\n"), 0o600))
		require.NoError(t, os.WriteFile(policy, []byte("profile bwrap …\n"), 0o600))
		require.NoError(t, os.MkdirAll(filepath.Dir(disabled), 0o700))
		require.NoError(t, os.MkdirAll(filepath.Dir(complain), 0o700))
		return enabled, policy, disabled, complain
	}

	t.Run("stock Ubuntu shape", func(t *testing.T) {
		enabled, policy, disabled, complain := shape(t)
		assert.True(t, likelyAppArmorNestedBwrapBlock(enabled, policy, disabled, complain))
	})

	t.Run("AppArmor off", func(t *testing.T) {
		enabled, policy, disabled, complain := shape(t)
		require.NoError(t, os.WriteFile(enabled, []byte("N\n"), 0o600))
		assert.False(t, likelyAppArmorNestedBwrapBlock(enabled, policy, disabled, complain))
	})

	t.Run("no AppArmor at all", func(t *testing.T) {
		enabled, policy, disabled, complain := shape(t)
		require.NoError(t, os.Remove(enabled))
		assert.False(t, likelyAppArmorNestedBwrapBlock(enabled, policy, disabled, complain))
	})

	t.Run("policy not shipped", func(t *testing.T) {
		enabled, policy, disabled, complain := shape(t)
		require.NoError(t, os.Remove(policy))
		assert.False(t, likelyAppArmorNestedBwrapBlock(enabled, policy, disabled, complain))
	})

	// The workaround the docs publish: a symlink into disable/ plus an
	// apparmor_parser -R. A dangling symlink still expresses the intent, and
	// this runs unprivileged, so lstat rather than stat is the load-bearing
	// choice here.
	t.Run("operator unloaded the policy", func(t *testing.T) {
		enabled, policy, disabled, complain := shape(t)
		require.NoError(t, os.Symlink(policy, disabled))
		assert.False(t, likelyAppArmorNestedBwrapBlock(enabled, policy, disabled, complain))
		require.NoError(t, os.Remove(disabled))
		require.NoError(t, os.Symlink(filepath.Join(t.TempDir(), "gone"), disabled))
		assert.False(t, likelyAppArmorNestedBwrapBlock(enabled, policy, disabled, complain),
			"a dangling disable/ entry is still an operator saying unload it")
	})

	t.Run("operator put the policy in complain mode", func(t *testing.T) {
		enabled, policy, disabled, complain := shape(t)
		require.NoError(t, os.Symlink(policy, complain))
		assert.False(t, likelyAppArmorNestedBwrapBlock(enabled, policy, disabled, complain))
	})

	// The temporary form the guide publishes is `aa-complain`, which edits the
	// flag list in the profile itself rather than dropping a symlink. A
	// stat-only heuristic would keep warning at an operator who had already
	// done exactly what it told them to do.
	t.Run("operator ran aa-complain", func(t *testing.T) {
		enabled, policy, disabled, complain := shape(t)
		require.NoError(t, os.WriteFile(policy, []byte(
			"profile bwrap /usr/bin/bwrap flags=(attach_disconnected,mediate_deleted,complain) {\n}\n",
		), 0o600))
		assert.False(t, likelyAppArmorNestedBwrapBlock(enabled, policy, disabled, complain))
	})
}

func TestAppArmorProfileInComplainMode(t *testing.T) {
	assert.True(t, appArmorProfileInComplainMode("profile bwrap flags=(complain) {\n}\n"))
	assert.True(t, appArmorProfileInComplainMode(
		"profile bwrap flags=(attach_disconnected, complain, mediate_deleted) {\n}\n"))
	assert.True(t, appArmorProfileInComplainMode(
		"profile bwrap flags=(attach_disconnected) {\n}\nprofile unpriv_bwrap flags=(complain) {\n}\n"),
		"a later profile in the same file still counts")

	assert.False(t, appArmorProfileInComplainMode(
		"profile bwrap flags=(attach_disconnected,mediate_deleted) {\n}\n"))
	assert.False(t, appArmorProfileInComplainMode(
		"# complain mode is not set here\nprofile bwrap {\n}\n"),
		"the word alone, outside a flag list, is not the flag")
	assert.False(t, appArmorProfileInComplainMode("profile bwrap flags=(complain\n"),
		"an unterminated flag list is not a claim either way")
}
