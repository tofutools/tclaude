package harness

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// conv_index synchronization for Copilot.
//
// The cache exists so tclaude-side verbs can NAME a Copilot conversation.
// `conv archive` is the concrete one: archiving has nowhere to live but
// `conv_index.archived_at`, and before this sync a conversation tclaude could
// list was not necessarily a conversation tclaude could archive.

func TestCopilotListingPopulatesConvIndex(t *testing.T) {
	home := copilotTestHome(t)
	cwd := t.TempDir()
	copilotSession(t, home, copilotTestID,
		workspaceYAML(copilotTestID, cwd, "widgets summary", false,
			"2026-08-03T19:08:12.442Z", "2026-08-03T19:08:13.219Z"),
		copilotStartEvent(copilotTestID, cwd, "gpt-5.4"),
		copilotSystemEvent(),
		copilotUserEvent("first prompt about widgets"))

	store := copilotConvStore{home: home}
	_, err := store.ListConvs("")
	require.NoError(t, err)

	row, err := db.GetConvIndex(copilotTestID)
	require.NoError(t, err)
	require.NotNil(t, row, "a listed Copilot conversation must be nameable by conv_index")
	assert.Equal(t, CopilotName, row.Harness)
	assert.Equal(t, cwd, row.ProjectPath)
	assert.Equal(t, "widgets summary", row.Summary)
	assert.Equal(t, "first prompt about widgets", row.FirstPrompt)
	assert.Equal(t, 1, row.MessageCount)
	assert.Equal(t, "probe-branch", row.GitBranch)
}

// TestCopilotListingPreservesArchivedState is the column the cache does NOT
// own. A full upsert that re-sent archived_at would unarchive on every listing.
func TestCopilotListingPreservesArchivedState(t *testing.T) {
	home := copilotTestHome(t)
	cwd := t.TempDir()
	copilotSession(t, home, copilotTestID,
		workspaceYAML(copilotTestID, cwd, "widgets", false,
			"2026-08-03T19:08:12.442Z", "2026-08-03T19:08:13.219Z"),
		copilotStartEvent(copilotTestID, cwd, "gpt-5.4"))

	store := copilotConvStore{home: home}
	_, err := store.ListConvs("")
	require.NoError(t, err)
	require.NoError(t, db.SetConvIndexArchived(copilotTestID, true))

	entries, err := store.ListConvs("")
	require.NoError(t, err)
	require.Len(t, entries, 1)
	assert.True(t, entries[0].IsArchived(),
		"the archived overlay must survive the listing that refreshes the cache")

	row, err := db.GetConvIndex(copilotTestID)
	require.NoError(t, err)
	require.NotNil(t, row)
	assert.True(t, row.IsArchived(),
		"a refreshing listing must not clear the one column it does not own")
}

// TestCopilotResolveDoesNotWriteTheCache pins the split between the two read
// paths. Resolve deliberately skips the event scan, so it does not know
// FirstPrompt, MessageCount or Model — and must therefore not upsert, or every
// `conv resume <prefix>` would blank those columns.
func TestCopilotResolveDoesNotWriteTheCache(t *testing.T) {
	home := copilotTestHome(t)
	cwd := t.TempDir()
	copilotSession(t, home, copilotTestID,
		workspaceYAML(copilotTestID, cwd, "widgets", false,
			"2026-08-03T19:08:12.442Z", "2026-08-03T19:08:13.219Z"),
		copilotStartEvent(copilotTestID, cwd, "gpt-5.4"),
		copilotUserEvent("first prompt about widgets"))

	store := copilotConvStore{home: home}
	_, err := store.ListConvs("")
	require.NoError(t, err)

	ref, err := store.Resolve(copilotTestID[:8], "", true)
	require.NoError(t, err)
	require.NotNil(t, ref)

	row, err := db.GetConvIndex(copilotTestID)
	require.NoError(t, err)
	require.NotNil(t, row)
	assert.Equal(t, "first prompt about widgets", row.FirstPrompt,
		"the events-free resolve must not overwrite what the full listing recorded")
	assert.Equal(t, 1, row.MessageCount)
}

// TestCopilotCwdScopedListingSyncsEveryProject is the trap a cwd-scoped sync
// would fall into: refreshing only the filtered subset would make the cache's
// contents depend on which directory the last command ran in.
func TestCopilotCwdScopedListingSyncsEveryProject(t *testing.T) {
	home := copilotTestHome(t)
	mine, other := t.TempDir(), t.TempDir()
	copilotSession(t, home, copilotTestID,
		workspaceYAML(copilotTestID, mine, "mine", false,
			"2026-08-03T19:08:12.442Z", "2026-08-03T19:08:13.219Z"),
		copilotStartEvent(copilotTestID, mine, "gpt-5.4"))
	copilotSession(t, home, copilotOtherID,
		workspaceYAML(copilotOtherID, other, "theirs", false,
			"2026-08-03T19:08:12.442Z", "2026-08-03T19:08:13.219Z"),
		copilotStartEvent(copilotOtherID, other, "gpt-5.4"))

	store := copilotConvStore{home: home}
	entries, err := store.ListConvs(mine)
	require.NoError(t, err)
	require.Len(t, entries, 1, "the RETURNED listing stays scoped to the requested cwd")

	for _, id := range []string{copilotTestID, copilotOtherID} {
		row, err := db.GetConvIndex(id)
		require.NoError(t, err)
		assert.NotNil(t, row,
			"conv %s must be cached regardless of which project the listing was scoped to", id)
	}
}

// TestCopilotListingKeepsUnrelatedCacheRows is the reason this sync does not
// evict. Copilot's listing only describes the CURRENT COPILOT_HOME, so
// treating absence as deletion would let a repointed home destroy the archived
// state of conversations under the real one.
func TestCopilotListingKeepsUnrelatedCacheRows(t *testing.T) {
	home := copilotTestHome(t)
	cwd := t.TempDir()

	stale := &db.ConvIndexRow{
		ConvID: copilotOtherID, ProjectDir: cwd, ProjectPath: cwd,
		IndexedAt: time.Now(), Harness: CopilotName,
	}
	require.NoError(t, db.UpsertConvIndex(stale))
	require.NoError(t, db.SetConvIndexArchived(copilotOtherID, true))

	copilotSession(t, home, copilotTestID,
		workspaceYAML(copilotTestID, cwd, "widgets", false,
			"2026-08-03T19:08:12.442Z", "2026-08-03T19:08:13.219Z"),
		copilotStartEvent(copilotTestID, cwd, "gpt-5.4"))

	store := copilotConvStore{home: home}
	_, err := store.ListConvs("")
	require.NoError(t, err)

	row, err := db.GetConvIndex(copilotOtherID)
	require.NoError(t, err)
	require.NotNil(t, row, "a conversation absent from THIS home must not be evicted")
	assert.True(t, row.IsArchived())
}
