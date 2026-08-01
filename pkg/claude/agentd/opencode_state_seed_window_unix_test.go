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

	// The victim is read FIRST and non-fatally. Ordered the other way, reverting
	// the fix aborts on the missing file in the real directory and never states
	// where the write actually went — a failure report about the return path
	// rather than about the damage.
	assert.NoFileExists(t,
		filepath.Join(victimBase, "opencode", openCodeInstallBootstrapFile),
		"a daemon-side write reached a directory the validation never saw")
	raw, err := os.ReadFile(filepath.Join(realConfig, openCodeInstallBootstrapFile))
	require.NoError(t, err,
		"the write must land in the directory that was validated, not the one the path now names")
	assert.Equal(t, openCodeInstallGitignore, string(raw))
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

// Acceptance now rests on ONE comparison — the pinned descriptor against the
// path this function returns — so what that path is made of decides whether the
// residual window #1834's cold review demonstrated stays shut. Handed an alias,
// it must answer with the resolved directory, never with the argument as given:
// echoing it back would put a caller-supplied string on the deciding side.
//
// SCOPE, ESTABLISHED BY MUTATION, NOT BY READING. A first version of this test
// claimed the stronger property "the answer is derived from the STORE rather
// than from the argument", and mutating the return to the argument's own
// resolved form did not fail it. That is not a weak assertion — the two are the
// same string by construction: the function has already proven
// resolvedOpenCodeSeedPath(allocation.StateRoot) == stateRoot, and the two
// basenames are pinned to config/opencode just above, so joining them back
// reproduces the resolved argument exactly. Absent a concurrent flip no test can
// separate them, and the claim was withdrawn rather than restated.
//
// What genuinely closed the window is therefore NOT provable here: it is that
// configDir is read exactly once, so there is no second read to race. That is a
// property of the code's shape, checkable by inspection, and this test does not
// carry it.
func TestRequireOpenCodeAllocatedConfigDirAnswersInResolvedForm(t *testing.T) {
	stateRoot, configDir := allocatedOpenCodeConfigDir(t)
	alias := filepath.Join(t.TempDir(), "alias-root")
	require.NoError(t, os.Symlink(stateRoot, alias))
	aliasConfig := filepath.Join(alias, "config", "opencode")

	allocated, err := requireOpenCodeAllocatedConfigDir(aliasConfig)
	require.NoError(t, err)
	assert.Equal(t, configDir, allocated,
		"the answer must be the resolved directory")
	assert.NotEqual(t, aliasConfig, allocated,
		"echoing the argument back would put a caller-supplied string on the deciding side")

	// And it still refuses what it should, so the equality above is not simply
	// a function that returns a path for anything.
	_, err = requireOpenCodeAllocatedConfigDir(
		filepath.Join(t.TempDir(), "config", "opencode"))
	require.ErrorContains(t, err, "names")
}

// The EEXIST branch inspects the existing file through the same descriptor. A
// cold-review mutation showed it reverting to a path re-open with no test
// failing, so the branch is pinned here: with the intermediate component
// repointed at a victim that holds a NON-REGULAR .gitignore, a path re-walk
// would inspect the victim's and refuse, while the descriptor sees the real
// directory's regular file and accepts.
func TestValidateExistingOpenCodeBootstrapInspectsThroughTheDescriptor(t *testing.T) {
	real := t.TempDir()
	realApp := filepath.Join(real, "opencode")
	require.NoError(t, os.MkdirAll(realApp, 0o700))
	require.NoError(t, os.WriteFile(
		filepath.Join(realApp, openCodeInstallBootstrapFile),
		[]byte(openCodeInstallGitignore), 0o600))

	victim := t.TempDir()
	victimApp := filepath.Join(victim, "opencode")
	require.NoError(t, os.MkdirAll(victimApp, 0o700))
	// A non-regular entry, which the existing-file inspection refuses.
	require.NoError(t, os.Mkdir(
		filepath.Join(victimApp, openCodeInstallBootstrapFile), 0o700))

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
	require.NoError(t, err,
		"the existing-file inspection must read the descriptor's directory, not the path's")
	assert.False(t, created, "the file already exists in the pinned directory")

	// Discriminating control: the path-addressed form of the same situation
	// reaches the victim's non-regular entry and refuses. Without it, NoError
	// above is equally consistent with the swap never having taken effect.
	_, pathErr := ensureOpenCodeBootstrapGitignore(source, "config")
	require.ErrorContains(t, pathErr, "is not a regular file",
		"addressing by path is what let the swap change which file was inspected")
}

// The allocated-but-wrong branch's wording. It said the source "does not
// resolve to" this host's ambient config, which was true before the descriptor
// rewrite — the test really was resolvedOpenCodeSeedPath(source) against the
// ambient path. Acceptance is now a device/inode comparison and this function
// resolves nothing, so the sentence outlived its mechanism.
//
// The discriminating case needs no symlink at all: point the ambient config
// somewhere that does not exist. "Does not resolve to <path>" then implies
// <path> exists and is elsewhere, when it does not exist.
func TestValidateOpenCodeReadOnlyConfigSeedSourceAtDoesNotClaimResolution(t *testing.T) {
	_, _ = allocatedOpenCodeConfigDir(t)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(t.TempDir(), "absent-config-base"))

	// The branch under test needs the allocation lookup to FAIL while the
	// descriptor IS the contract's config directory, so the source is
	// self-bound to an unallocated target. Pointing at a foreign directory
	// instead lands in the other refusal and proves nothing about this wording.
	unallocated := filepath.Join(t.TempDir(), "config", "opencode")
	require.NoError(t, os.MkdirAll(unallocated, 0o700))
	fd, err := openOpenCodeBootstrapDirectory(unallocated, "config")
	require.NoError(t, err)
	defer func() { _ = unix.Close(fd) }()

	err = validateOpenCodeReadOnlyConfigSeedSourceAt(fd, unallocated, unallocated)
	require.Error(t, err)
	require.NotContains(t, err.Error(), "does not resolve to",
		"nothing here resolves the source; claiming resolution describes a mechanism the code no longer performs")
	require.Contains(t, err.Error(), "is not this host's ambient OpenCode config",
		"the claim must be about WHAT the directory is, not how it was determined")
}
