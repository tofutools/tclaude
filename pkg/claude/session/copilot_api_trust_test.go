package session

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// copilotAPITrustEnv builds the launch environment entries that relocate
// COPILOT_HOME and HOME, so every case below is decided against a disposable
// store rather than the developer's own.
func copilotAPITrustEnv(home string) []sandboxpolicy.EnvironmentEntry {
	return []sandboxpolicy.EnvironmentEntry{
		{Name: harness.CopilotHomeEnvVar, Value: home},
		{Name: "HOME", Value: home},
	}
}

func copilotHarness(t *testing.T) *harness.Harness {
	t.Helper()
	h, err := harness.Resolve(harness.CopilotName)
	require.NoError(t, err)
	return h
}

// The gate exists for the API drive and nothing else. A send-keys Copilot agent
// in an untrusted directory has always been allowed to launch and stop on the
// modal — a human can clear it, and the dashboard focus button exists for
// exactly that — so widening this to every Copilot launch would break a
// long-standing workflow to solve a problem that workflow does not have.
func TestCopilotAPIFolderTrustIgnoresTheSendKeysDrive(t *testing.T) {
	home := t.TempDir()
	assert.NoError(t, ValidateCopilotAPIFolderTrust(
		copilotHarness(t), false, false, t.TempDir(), copilotAPITrustEnv(home)))
}

// A launch that is about to seed the directory is admitted on that promise.
// session.New performs the seed a few lines after this check, so refusing here
// would refuse every --trust-dir spawn — the one shape that is guaranteed not
// to hit the modal.
func TestCopilotAPIFolderTrustAdmitsALaunchThatWillSeed(t *testing.T) {
	home := t.TempDir()
	assert.NoError(t, ValidateCopilotAPIFolderTrust(
		copilotHarness(t), true, true, t.TempDir(), copilotAPITrustEnv(home)))
}

// The refusal itself, and the reason it exists: an untrusted directory under
// the API drive produces a working, invisible agent rather than a failure.
func TestCopilotAPIFolderTrustRefusesAnUntrustedDir(t *testing.T) {
	home := t.TempDir()
	cwd := t.TempDir()

	err := ValidateCopilotAPIFolderTrust(
		copilotHarness(t), true, false, cwd, copilotAPITrustEnv(home))
	require.Error(t, err)
	assert.Contains(t, err.Error(), cwd,
		"the refusal must name the directory, since that is what the operator has to act on")
	assert.Contains(t, err.Error(), "--trust-dir",
		"the refusal must name the remedy rather than only the problem")
}

// Already-trusted needs no opt-in. The gate asks whether the launch will stop
// on the modal, not whether this particular spawn requested a seed, so a
// directory the operator cleared by hand once is as good as a seeded one.
func TestCopilotAPIFolderTrustAdmitsAnAlreadyTrustedDir(t *testing.T) {
	home := t.TempDir()
	cwd := t.TempDir()
	require.NoError(t, harness.EnsureCopilotDirTrustedForLaunch(
		func(name string) string {
			if name == harness.CopilotHomeEnvVar {
				return home
			}
			return ""
		}, home, cwd))

	assert.NoError(t, ValidateCopilotAPIFolderTrust(
		copilotHarness(t), true, false, cwd, copilotAPITrustEnv(home)))
}

// The store the check reads must be the store the LAUNCH reads. A profile that
// relocates COPILOT_HOME moves the file that governs the modal, and a gate
// reading the ambient one would refuse a launch whose directory is trusted
// exactly where it matters — a confident refusal derived from the wrong file.
func TestCopilotAPIFolderTrustReadsTheLaunchesOwnStore(t *testing.T) {
	launchHome := t.TempDir()
	otherHome := t.TempDir()
	cwd := t.TempDir()

	require.NoError(t, harness.EnsureCopilotDirTrustedForLaunch(
		func(name string) string {
			if name == harness.CopilotHomeEnvVar {
				return launchHome
			}
			return ""
		}, launchHome, cwd))

	assert.NoError(t, ValidateCopilotAPIFolderTrust(
		copilotHarness(t), true, false, cwd, copilotAPITrustEnv(launchHome)),
		"the launch's own store trusts this directory")
	assert.Error(t, ValidateCopilotAPIFolderTrust(
		copilotHarness(t), true, false, cwd, copilotAPITrustEnv(otherHome)),
		"a different store must produce a different answer, or the check is not "+
			"reading the file it claims to")
}

// A launch with no cwd inherits tclaude's own directory. There is no directory
// the operator chose to name in a refusal, so naming one would send them to
// edit trust for a path they never asked for.
func TestCopilotAPIFolderTrustIsQuietWithoutACwd(t *testing.T) {
	home := t.TempDir()
	assert.NoError(t, ValidateCopilotAPIFolderTrust(
		copilotHarness(t), true, false, "  ", copilotAPITrustEnv(home)))
}

// Other harnesses have their own trust stores and no embedded server, so the
// question does not apply to them at all.
func TestCopilotAPIFolderTrustIgnoresOtherHarnesses(t *testing.T) {
	home := t.TempDir()
	for _, name := range []string{harness.DefaultName, harness.CodexName} {
		h, err := harness.Resolve(name)
		require.NoError(t, err)
		assert.NoError(t, ValidateCopilotAPIFolderTrust(
			h, true, false, filepath.Join(t.TempDir(), "x"), copilotAPITrustEnv(home)),
			"harness %s has no embedded server to be invisibly driven over", name)
	}
}
