//go:build linux || darwin

package agentd

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

// TCL-908, end to end through the production seed. The source is reached
// through a symlinked INTERMEDIATE component — the one O_NOFOLLOW never
// constrained, since it applies to the final component only — and that symlink
// is repointed at a victim directory at the exact instant between validation
// and use.
//
// Post-fix the write follows the descriptor opened before validation, so it
// lands in the directory that was validated. Pre-fix it followed the path,
// which by then named the victim.
func TestPrepareOpenCodeReadOnlyConfigWritesThroughTheValidatedDirectory(t *testing.T) {
	stateRoot, realConfig := allocatedOpenCodeConfigDir(t)

	// The seed is handed <link>/opencode, where <link> currently points at the
	// real per-agent config base. Nothing about the source's own name changes
	// during the test; only what an intermediate component means.
	realBase := filepath.Dir(realConfig)
	link := filepath.Join(t.TempDir(), "config-base-link")
	require.NoError(t, os.Symlink(realBase, link))
	source := filepath.Join(link, "opencode")

	victimBase := filepath.Join(t.TempDir(), "victim-base")
	require.NoError(t, os.MkdirAll(filepath.Join(victimBase, "opencode"), 0o700))

	swapped := false
	openCodeSeedWindowHookForTest = func() {
		require.NoError(t, os.Remove(link))
		require.NoError(t, os.Symlink(victimBase, link))
		swapped = true
	}
	t.Cleanup(func() { openCodeSeedWindowHookForTest = nil })

	spec := openCodeConfigBootstrapSpec(stateRoot, realConfig, source)
	require.NoError(t, prepareOpenCodeReadOnlyConfig(spec, "Linux"))

	// The hook is the fixture: if it never ran, everything below is satisfied by
	// the window simply not having been reached, which is indistinguishable from
	// the window being closed.
	require.True(t, swapped,
		"the seed did not reach the check-then-use window; this test proves nothing unless it did")
	require.Equal(t, victimBase, mustReadlink(t, link),
		"the path really does name the victim by the time the write happens")

	raw, err := os.ReadFile(filepath.Join(realConfig, openCodeInstallBootstrapFile))
	require.NoError(t, err,
		"the write must land in the directory that was validated, not the one the path now names")
	assert.Equal(t, openCodeInstallGitignore, string(raw))
	assert.NoFileExists(t,
		filepath.Join(victimBase, "opencode", openCodeInstallBootstrapFile),
		"a daemon-side write reached a directory the validation never saw")
}

// The same property at the primitive, without the production hook: a descriptor
// pinned before an intermediate component is repointed still writes into the
// object it was opened on.
//
// The second half is the discriminating control. It is the SAME swap addressed
// by path instead of by descriptor, and it lands in the victim — so the first
// half is evidence about the addressing mode, not about the fixture being too
// weak to move anything.
func TestEnsureOpenCodeBootstrapGitignoreAtIgnoresAnIntermediateSwap(t *testing.T) {
	real := t.TempDir()
	realApp := filepath.Join(real, "opencode")
	require.NoError(t, os.MkdirAll(realApp, 0o700))
	victim := t.TempDir()
	victimApp := filepath.Join(victim, "opencode")
	require.NoError(t, os.MkdirAll(victimApp, 0o700))

	link := filepath.Join(t.TempDir(), "base")
	require.NoError(t, os.Symlink(real, link))
	source := filepath.Join(link, "opencode")

	dirFD, err := openOpenCodeBootstrapDirectory(source, "config")
	require.NoError(t, err)
	defer func() { _ = unix.Close(dirFD) }()

	require.NoError(t, os.Remove(link))
	require.NoError(t, os.Symlink(victim, link))
	require.Equal(t, victim, mustReadlink(t, link))

	created, err := ensureOpenCodeBootstrapGitignoreAt(dirFD, source, "config")
	require.NoError(t, err)
	assert.True(t, created)
	assert.FileExists(t, filepath.Join(realApp, openCodeInstallBootstrapFile))
	assert.NoFileExists(t, filepath.Join(victimApp, openCodeInstallBootstrapFile))

	// Control: the path-addressed form of the identical situation follows the
	// swap. Without this, "the file landed in real" would be consistent with a
	// swap that never took effect at all.
	pathCreated, err := ensureOpenCodeBootstrapGitignore(source, "config")
	require.NoError(t, err)
	assert.True(t, pathCreated)
	assert.FileExists(t, filepath.Join(victimApp, openCodeInstallBootstrapFile),
		"addressing by path is what made the window exploitable; if this does not move, the fixture is wrong")
}

// The descriptor is compared to the daemon's candidates by kernel identity, so
// a source that merely SPELLS its way to an accepted directory is accepted, and
// one that spells its way anywhere else is refused — with the refusal naming
// the reason rather than any error.
func TestValidateOpenCodeReadOnlyConfigSeedSourceAtComparesIdentity(t *testing.T) {
	_, configDir := allocatedOpenCodeConfigDir(t)
	alias := filepath.Join(t.TempDir(), "alias")
	require.NoError(t, os.Symlink(configDir, alias))

	aliasFD, err := openOpenCodeBootstrapDirectory(alias, "config")
	require.NoError(t, err)
	defer func() { _ = unix.Close(aliasFD) }()
	require.NoError(t,
		validateOpenCodeReadOnlyConfigSeedSourceAt(aliasFD, alias, configDir),
		"a legitimate directory reached through another spelling is the same directory")

	foreign := t.TempDir()
	foreignFD, err := openOpenCodeBootstrapDirectory(foreign, "config")
	require.NoError(t, err)
	defer func() { _ = unix.Close(foreignFD) }()
	require.ErrorContains(t,
		validateOpenCodeReadOnlyConfigSeedSourceAt(foreignFD, foreign, configDir),
		"is neither an allocated per-agent config directory nor this host's ambient OpenCode config")
}

// A non-directory source is refused at the descriptor, before any decision is
// made about it — O_DIRECTORY is doing that, and it is worth pinning because
// the previous shape only ever opened the file underneath.
func TestOpenOpenCodeBootstrapDirectoryRefusesANonDirectory(t *testing.T) {
	file := filepath.Join(t.TempDir(), "not-a-directory")
	require.NoError(t, os.WriteFile(file, []byte("x"), 0o600))

	_, err := openOpenCodeBootstrapDirectory(file, "config")
	require.ErrorContains(t, err, "open OpenCode config bootstrap directory")
	require.ErrorIs(t, err, unix.ENOTDIR)
}

func mustReadlink(t *testing.T, path string) string {
	t.Helper()
	target, err := os.Readlink(path)
	require.NoError(t, err)
	return target
}
