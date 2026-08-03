package harness

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The cwd-spelling cases. Copilot records a RESOLVED cwd; a caller supplies
// whatever its environment spells. On macOS those are routinely two spellings
// of one physical directory (`/var/folders/...` vs `/private/var/folders/...`),
// and the smoke test in pkg/claude/harness/copilotfixture drives that exact
// shape through the real binary. The symlink staged below is the same shape,
// reproducible on any platform, so these run everywhere.

// copilotSymlinkedProject stages `<tmp>/physical/work` plus an `<tmp>/alias`
// symlink to `<tmp>/physical`, and returns the two spellings of the one
// directory.
//
// The two sides are treated ASYMMETRICALLY, mirroring production: `physical` is
// resolved, because that is the side Copilot writes into workspace.yaml, while
// `alias` is left exactly as constructed, because that is the side a caller
// supplies. Resolving the caller's side too is what would make these tests
// measure nothing.
//
// Resolving `physical` is not cosmetic, and it is what the tmp root's OWN
// spelling makes necessary: on macOS t.TempDir hands back /var/folders/… for a
// directory the kernel calls /private/var/folders/…, so a `physical` built by
// Join alone would be an unresolved spelling of the record side and every
// assertion about "what the store recorded" or "what resolution returns" would
// be off by that prefix on macOS while passing on Linux.
func copilotSymlinkedProject(t *testing.T) (physical, alias string) {
	t.Helper()
	// The ROOT is canonicalized once, before anything is derived from it, so
	// `physical` is genuinely physical. Without this the helper returns two
	// ALIASES on a host whose temp root is itself symlinked, and every
	// assertion about what resolution returns is off by that prefix — which is
	// how these tests passed on Linux and failed on macOS.
	root, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	physical = filepath.Join(root, "physical", "work")
	require.NoError(t, os.MkdirAll(physical, 0o755))
	if err := os.Symlink(filepath.Join(root, "physical"), filepath.Join(root, "alias")); err != nil {
		t.Skipf("symlinks unavailable here: %v", err)
	}
	return physical, filepath.Join(root, "alias", "work")
}

// TestCopilotConvStoreMatchesCwdSpelledThroughASymlink is the defect: the
// conversation carries the resolved spelling and the caller supplies the
// symlinked one, so a lexical comparison drops it from a cwd-scoped listing.
func TestCopilotConvStoreMatchesCwdSpelledThroughASymlink(t *testing.T) {
	home := copilotTestHome(t)
	physical, alias := copilotSymlinkedProject(t)
	require.NotEqual(t, physical, alias, "the two spellings must differ for this to test anything")

	copilotSession(t, home, copilotTestID,
		workspaceYAML(copilotTestID, physical, "mine", false,
			"2026-08-03T19:08:12.442Z", "2026-08-03T19:08:13.219Z"))
	store := copilotConvStore{home: home}

	entries, err := store.ListConvs(alias)
	require.NoError(t, err)
	require.Len(t, entries, 1, "the symlinked spelling names the same physical directory")
	assert.Equal(t, copilotTestID, entries[0].SessionID)
	assert.Equal(t, physical, entries[0].ProjectPath,
		"the entry keeps the spelling Copilot recorded; only the MATCH is physical")

	// Resolve rides the same filter, so an id prefix scoped to the symlinked
	// spelling must find the conversation too.
	ref, err := store.Resolve(copilotTestID[:8], alias, false)
	require.NoError(t, err)
	require.NotNil(t, ref)
	assert.Equal(t, copilotTestID, ref.ConvID)
}

// TestCopilotConvStoreMatchesResolvedCwdAgainstASymlinkedRecord is the same
// contract from the other side: a conversation recorded under the symlinked
// spelling must be found from the resolved one. Copilot does not write this
// shape today, but the filter must not be one-directional — a future CLI, or an
// operator moving a project behind a link, would otherwise flip the answer.
func TestCopilotConvStoreMatchesResolvedCwdAgainstASymlinkedRecord(t *testing.T) {
	home := copilotTestHome(t)
	physical, alias := copilotSymlinkedProject(t)

	copilotSession(t, home, copilotTestID,
		workspaceYAML(copilotTestID, alias, "mine", false,
			"2026-08-03T19:08:12.442Z", "2026-08-03T19:08:13.219Z"))

	entries, err := copilotConvStore{home: home}.ListConvs(physical)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	assert.Equal(t, copilotTestID, entries[0].SessionID)
}

// TestCopilotConvStoreKeepsDistinctDirectoriesDistinct pins that the
// canonicalization only ever collapses spellings of ONE directory. Two real
// directories — including two that merely share a parent, and one that is a
// prefix of the other as a string — stay separate projects.
func TestCopilotConvStoreKeepsDistinctDirectoriesDistinct(t *testing.T) {
	home := copilotTestHome(t)
	root := t.TempDir()
	mine := filepath.Join(root, "work")
	sibling := filepath.Join(root, "work-2")
	child := filepath.Join(root, "work", "nested")
	for _, dir := range []string{mine, sibling, child} {
		require.NoError(t, os.MkdirAll(dir, 0o755))
	}
	copilotSession(t, home, copilotTestID,
		workspaceYAML(copilotTestID, mine, "mine", false,
			"2026-08-03T19:08:12.442Z", "2026-08-03T19:08:13.219Z"))
	copilotSession(t, home, copilotTestID2,
		workspaceYAML(copilotTestID2, sibling, "sibling", false,
			"2026-08-03T19:08:12.442Z", "2026-08-03T19:08:13.219Z"))
	copilotSession(t, home, copilotOtherID,
		workspaceYAML(copilotOtherID, child, "child", false,
			"2026-08-03T19:08:12.442Z", "2026-08-03T19:08:13.219Z"))
	store := copilotConvStore{home: home}

	for _, tc := range []struct {
		cwd  string
		want string
	}{
		{mine, copilotTestID},
		{sibling, copilotTestID2},
		{child, copilotOtherID},
	} {
		entries, err := store.ListConvs(tc.cwd)
		require.NoError(t, err)
		require.Len(t, entries, 1, "cwd %s", tc.cwd)
		assert.Equal(t, tc.want, entries[0].SessionID, "cwd %s", tc.cwd)
	}
}

// TestCopilotConvStoreMatchesMissingDirectoriesLexically pins the degraded
// cases. A cwd that does not exist has no physical spelling, and a conversation
// whose project directory has been deleted has none either — both must still
// match on the recorded spelling, and must never match a different one.
func TestCopilotConvStoreMatchesMissingDirectoriesLexically(t *testing.T) {
	home := copilotTestHome(t)
	gone := filepath.Join(t.TempDir(), "deleted-project")
	copilotSession(t, home, copilotTestID,
		workspaceYAML(copilotTestID, gone, "mine", false,
			"2026-08-03T19:08:12.442Z", "2026-08-03T19:08:13.219Z"))
	store := copilotConvStore{home: home}

	entries, err := store.ListConvs(gone)
	require.NoError(t, err)
	require.Len(t, entries, 1, "a deleted project still lists under the spelling it recorded")

	entries, err = store.ListConvs(filepath.Join(t.TempDir(), "other-missing"))
	require.NoError(t, err)
	assert.Empty(t, entries, "two paths that both fail to resolve are not the same directory")

	// An existing conversation must not be dragged in by a cwd that cannot be
	// resolved — that would be the filter guessing rather than comparing.
	entries, err = store.ListConvs(filepath.Join(gone, "..", "deleted-project", "deeper"))
	require.NoError(t, err)
	assert.Empty(t, entries)
}

// TestCopilotConvStoreDoesNotResolveRelativeCwds pins that a relative cwd stays
// lexical. filepath.EvalSymlinks would interpret it against the PROCESS's
// working directory, which is not the caller's; the honest answer is the string
// comparison.
func TestCopilotConvStoreDoesNotResolveRelativeCwds(t *testing.T) {
	home := copilotTestHome(t)
	copilotSession(t, home, copilotTestID,
		workspaceYAML(copilotTestID, "relative/project", "mine", false,
			"2026-08-03T19:08:12.442Z", "2026-08-03T19:08:13.219Z"))
	store := copilotConvStore{home: home}

	entries, err := store.ListConvs("relative/project")
	require.NoError(t, err)
	assert.Len(t, entries, 1, "an identical relative spelling still matches itself")

	entries, err = store.ListConvs("./relative/project/")
	require.NoError(t, err)
	assert.Len(t, entries, 1, "cleaning is lexical and still applies")

	entries, err = store.ListConvs(filepath.Join(t.TempDir(), "relative", "project"))
	require.NoError(t, err)
	assert.Empty(t, entries, "an absolute path is not a relative one that ends the same way")
}

// TestCopilotCwdFilterResolvesEachDirectoryOnce pins the memo. Without it a
// home holding many sessions in one project would EvalSymlinks that project
// once per session, turning a cold listing into a filesystem sweep.
func TestCopilotCwdFilterResolvesEachDirectoryOnce(t *testing.T) {
	physical, alias := copilotSymlinkedProject(t)
	filter := newCopilotCwdFilter(alias)

	require.True(t, filter.matches(physical))
	require.True(t, filter.matches(physical))
	assert.Len(t, filter.real, 0,
		"the entry equals the caller's RESOLVED cwd, so it never needs resolving itself")

	// A directory that matches neither spelling is resolved — once.
	other := t.TempDir()
	assert.False(t, filter.matches(other))
	assert.False(t, filter.matches(other))
	assert.Len(t, filter.real, 1)
}

// TestCopilotCwdFilterSkipsEntryProbesForAnUnresolvableCwd pins the bound on
// the probe: when the caller's own cwd cannot be resolved there is no physical
// directory to compare against, so no entry is touched at all.
func TestCopilotCwdFilterSkipsEntryProbesForAnUnresolvableCwd(t *testing.T) {
	filter := newCopilotCwdFilter(filepath.Join(t.TempDir(), "missing"))
	require.Empty(t, filter.wantReal)

	assert.False(t, filter.matches(t.TempDir()))
	assert.Empty(t, filter.real, "an unresolvable cwd must not provoke a filesystem probe")
}

func TestPhysicalDirSpelling(t *testing.T) {
	physical, alias := copilotSymlinkedProject(t)

	resolved, ok := physicalDirSpelling(alias)
	require.True(t, ok)
	assert.Equal(t, physical, resolved)

	resolved, ok = physicalDirSpelling(physical)
	require.True(t, ok)
	assert.Equal(t, physical, resolved)

	_, ok = physicalDirSpelling(filepath.Join(physical, "missing"))
	assert.False(t, ok, "a path that does not exist has no physical spelling")

	_, ok = physicalDirSpelling("relative/path")
	assert.False(t, ok, "a relative path would resolve against the process cwd")

	_, ok = physicalDirSpelling("")
	assert.False(t, ok)
}
