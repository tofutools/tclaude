package harness_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// copilotHomeEnv builds the getenv a launch with a relocated COPILOT_HOME
// would see, so the reader is exercised through the same resolution the seeder
// uses rather than against the ambient home.
func copilotHomeEnv(home string) func(string) string {
	return func(name string) string {
		if name == harness.CopilotHomeEnvVar {
			return home
		}
		return ""
	}
}

// The read-side companion to the seeder: the same store, asked rather than
// written. Seeding then reading is the round-trip every caller depends on, and
// it is worth asserting directly — a reader that looked at a different key or a
// different file would answer "untrusted" for a directory the CLI will accept,
// and the launch would be refused for no reason.
func TestCopilotDirTrustedForLaunchSeesTheSeededDir(t *testing.T) {
	stateHome := t.TempDir()
	projectDir := t.TempDir()

	trusted, err := harness.CopilotDirTrustedForLaunch(copilotHomeEnv(stateHome), stateHome, projectDir)
	require.NoError(t, err)
	assert.False(t, trusted, "a fresh COPILOT_HOME trusts nothing")

	require.NoError(t, harness.EnsureCopilotDirTrustedForLaunch(
		copilotHomeEnv(stateHome), stateHome, projectDir))

	trusted, err = harness.CopilotDirTrustedForLaunch(copilotHomeEnv(stateHome), stateHome, projectDir)
	require.NoError(t, err)
	assert.True(t, trusted, "the reader must see what the seeder wrote")
}

// A directory nobody seeded stays untrusted even when the store exists and
// holds other entries. The failure this guards against is a reader that
// answers "is this file non-empty" instead of "is this directory listed".
func TestCopilotDirTrustedForLaunchIsPerDirectory(t *testing.T) {
	stateHome := t.TempDir()
	seeded := t.TempDir()
	other := t.TempDir()

	require.NoError(t, harness.EnsureCopilotDirTrustedForLaunch(
		copilotHomeEnv(stateHome), stateHome, seeded))

	trusted, err := harness.CopilotDirTrustedForLaunch(copilotHomeEnv(stateHome), stateHome, other)
	require.NoError(t, err)
	assert.False(t, trusted)
}

// An absent config file is the ordinary state of a fresh COPILOT_HOME, so it
// must be "not trusted" and not an error: a launch into a brand-new state
// directory would otherwise fail its gate with an I/O complaint instead of the
// actionable refusal it deserves.
func TestCopilotDirTrustedForLaunchTreatsAMissingStoreAsUntrusted(t *testing.T) {
	home := t.TempDir()
	trusted, err := harness.CopilotDirTrustedForLaunch(
		copilotHomeEnv(filepath.Join(home, "never-created")), home, t.TempDir())
	require.NoError(t, err)
	assert.False(t, trusted)
}

// A store shape the seeder refuses to edit is reported as an error rather than
// as "untrusted". Answering "untrusted" would send the caller into a --trust-dir
// seed that is going to fail on the same file, so the operator would be told to
// do the one thing that cannot work.
func TestCopilotDirTrustedForLaunchRefusesAShapeItCannotRead(t *testing.T) {
	stateHome := t.TempDir()
	require.NoError(t, os.WriteFile(
		filepath.Join(stateHome, harness.CopilotConfigFileName),
		[]byte(`{"trustedFolders": "not-an-array"}`), 0o600))

	_, err := harness.CopilotDirTrustedForLaunch(copilotHomeEnv(stateHome), stateHome, t.TempDir())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "trustedFolders")
}

// A relative project dir is refused rather than resolved against whatever
// directory this process happens to be in: tclaude and Copilot would resolve it
// against different working directories, and the answer would be about neither.
func TestCopilotDirTrustedForLaunchRefusesARelativeDir(t *testing.T) {
	home := t.TempDir()
	_, err := harness.CopilotDirTrustedForLaunch(copilotHomeEnv(home), home, "relative/dir")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not absolute")
}
