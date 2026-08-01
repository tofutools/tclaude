//go:build linux || darwin

package agentd

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TCL-923: these refusals decided one thing and described another. Each test
// below pins the sentence the code can now defend AND keeps the retired
// spelling as a negative needle, because re-introducing wording already removed
// elsewhere in this file is its demonstrated failure mode — the audit found the
// same defect in an arm no delta had touched.
//
// Every assertion here was mutation-checked: the wording was reverted and the
// test confirmed to fail. A test that passes against both spellings pins
// nothing.

// The gate is TrimSpace + Clean + IsAbs, so it canonicalizes nothing. Four
// inputs whose single fault is non-absoluteness, all of which used to report a
// canonicality verdict with no value attached.
func TestOpenCodeFilteredProviderSourcesNamesNonAbsoluteAsWhatItIs(t *testing.T) {
	for _, spelled := range []string{"", "   ", "relative/path", "."} {
		err := validateOpenCodeFilteredProviderSources(spelled)
		require.Error(t, err, "a non-absolute state root must still be refused")
		assert.Contains(t, err.Error(), "is not an absolute path",
			"the sentence must name the predicate that actually ran")
		assert.NotContains(t, err.Error(), "state root is not canonical",
			"the retired canonicality wording must not come back")
		// The value, which the old sentence omitted entirely — an operator
		// given neither a mechanism nor a path has nothing to act on.
		assert.Contains(t, err.Error(), `"`+spelled+`"`,
			"the refused spelling must be quoted back")
	}
}

// One directory, three arms, three different mechanisms — and the old wording
// named the wrong one twice. The subtests share a fixture shape deliberately:
// what distinguishes them is only which arm fires.
func TestOpenCodeFilteredProviderDirectoryNamesTheArmThatFired(t *testing.T) {
	t.Run("MissingDirectoryIsNotACanonicalityVerdict", func(t *testing.T) {
		err := validateOpenCodeFilteredProviderDirectory(
			filepath.Join(t.TempDir(), "absent"), "XDG config directory")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "could not be inspected or is not a directory")
		// The arm that tests canonicality is the NEXT one. Sending a reader to
		// the symlink question for a missing directory is the defect.
		assert.NotContains(t, err.Error(), "is not a canonical directory",
			"the retired wording named a mechanism this arm does not run")
	})

	t.Run("EmptyDirectoryIsNotToldItIsNotEmpty", func(t *testing.T) {
		// resolvedTestPath, not the raw t.TempDir(). The entry-count arm is
		// the THIRD one, and the canonicality arm before it refuses any path
		// that differs from its own resolution — which on macOS every temp
		// path does, because /var is reached through a symlink to
		// /private/var. Passing the raw path fired the wrong arm there while
		// passing on Linux, so this subtest asserted nothing about the
		// condition it names on half the matrix. Caught by CI, not by
		// inspection.
		dir := resolvedTestPath(t, t.TempDir())
		err := validateOpenCodeFilteredProviderDirectory(dir, "XDG config directory")
		require.Error(t, err, "an empty directory has no marker and must be refused")
		assert.Contains(t, err.Error(), "does not hold exactly one entry")
		// The sentence an operator would act on by going to look for contents
		// that are not there.
		assert.NotContains(t, err.Error(), "is not provider-empty",
			"an empty directory must not be told it is not empty")
	})

	// Reached through a symlinked ANCESTOR, not a symlinked leaf. A symlinked
	// leaf never gets here: Lstat reports the link itself, !IsDir fires, and
	// the arm above answers. The canonicality arm is only reachable when the
	// path Lstats as a real directory and still differs from its resolution,
	// which is what an ancestor link produces.
	t.Run("NonCanonicalPathNamesBothArms", func(t *testing.T) {
		// Resolved for the same macOS reason as above, but here it protects a
		// PASSING assertion rather than a failing one: a raw temp root is
		// already non-canonical on macOS, so the arm would fire for the
		// platform's /var symlink instead of the one this fixture builds, and
		// the subtest would pass while testing nothing it names.
		root := resolvedTestPath(t, t.TempDir())
		require.NoError(t, os.MkdirAll(filepath.Join(root, "real", "opencode"), 0o700))
		require.NoError(t, os.Symlink(
			filepath.Join(root, "real"), filepath.Join(root, "link")))
		reached := filepath.Join(root, "link", "opencode")
		require.DirExists(t, reached, "the fixture must reach a real directory")

		err := validateOpenCodeFilteredProviderDirectory(reached, "XDG config directory")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "could not be resolved or is not canonical",
			"the EvalSymlinks error is a disjunct and the sentence must say so")
	})
}

// The worst one in the audit: the decision runs on the RESOLVED path and the
// sentence quoted the unresolved one, so with a symlinked leaf the refusal
// printed a path that visibly HAS the shape it was being told it lacks. It read
// as a bug in the validator and hid the symlink.
func TestOpenCodeAllocatedConfigDirShowsTheValueItDecidedOn(t *testing.T) {
	root := t.TempDir()
	configBase := filepath.Join(root, "config")
	require.NoError(t, os.MkdirAll(filepath.Join(configBase, "elsewhere"), 0o700))
	spelled := filepath.Join(configBase, "opencode")
	require.NoError(t, os.Symlink(filepath.Join(configBase, "elsewhere"), spelled))

	_, err := requireOpenCodeAllocatedConfigDir(spelled)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "does not have the per-agent",
		"the shape refusal is still the one that fires")
	// The spelling the operator handed in still appears — it is what they
	// typed — but it can no longer appear ALONE, which is what made the
	// sentence contradict itself.
	assert.Contains(t, err.Error(), "(tested as ",
		"the value the decision ran on must be shown beside the one quoted")
	assert.Contains(t, err.Error(), "elsewhere",
		"and the tested value is the resolved leaf, which is where the shape broke")
}

// The owner half of an ownership refusal. The !ok arm is the one worth pinning:
// a nil *syscall.Stat_t rendered as uid 0 would name root as the owner of a
// directory whose owner was never read.
func TestOpenCodeStatOwnerDescriptionDoesNotInventAnOwner(t *testing.T) {
	assert.Equal(t, "no readable owner", openCodeStatOwnerDescription(nil, false),
		"a failed Sys() assertion has no owner to report")
	assert.Equal(t, "no readable owner", openCodeStatOwnerDescription(nil, true),
		"a nil stat must not be dereferenced into a confident zero")
	assert.Equal(t, "uid 4294967294",
		openCodeStatOwnerDescription(&syscall.Stat_t{Uid: 4294967294}, true),
		"a real owner is rendered as the uid it is")
}

// The allocated-state-root renderer, pinned directly because the value it
// guards against is produced by the standard library rather than by this
// package: Clean("") is ".", EvalSymlinks(".") succeeds, and a legacy-shared
// allocation is REQUIRED to record an empty state root. Printing the resolved
// form would name "." as a recorded root that does not exist.
func TestOpenCodeRecordedStateRootDescriptionDoesNotInventARoot(t *testing.T) {
	// The precondition, asserted rather than assumed — if resolution of an
	// empty path ever stopped yielding ".", this renderer's whole reason to
	// exist would change and the test should say so first.
	require.Equal(t, ".", resolvedOpenCodeSeedPath(""),
		"an empty recorded root still resolves to the current directory")

	assert.Equal(t, "none recorded",
		openCodeRecordedStateRootDescription("", resolvedOpenCodeSeedPath("")))
	assert.Equal(t, "none recorded",
		openCodeRecordedStateRootDescription("   ", resolvedOpenCodeSeedPath("   ")))
	assert.Equal(t, `"/real/root"`,
		openCodeRecordedStateRootDescription("/real/root", "/real/root"),
		"a recorded root is quoted in its resolved form")
}
