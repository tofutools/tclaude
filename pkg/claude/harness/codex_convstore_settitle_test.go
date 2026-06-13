package harness_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// JOH-161: the Codex out-of-band rename. ConvStore.SetTitle writes
// threads.title in ~/.codex/state_5.sqlite (Codex has no in-pane rename
// command), and the reads (Title / ListConvs) must surface the new name.
// These exercise the real ConvStore surface against a temp HOME — the same
// store agentd's deliverRename routes a Codex rename through.

func TestCodexConvStore_SetTitle_UpdatesThreadsTitle(t *testing.T) {
	home := codexTestHome(t)
	cwd := "/home/u/proj"
	const id = "019ec004-4250-79b1-9ade-ebaea4135453"
	cs := codexConvs(t)

	// A live Codex session: rollout on disk + the threads row Codex
	// creates, whose title starts as the derived first message.
	startCodexSim(t, home, id, cwd, "hello", "hi")
	writeCodexThread(t, home, codexThreadSeed{
		ID: id, Cwd: cwd, Title: "hello", FirstUserMessage: "hello",
		CreatedAt: 1781337965, UpdatedAt: 1781337965,
	})

	// Out-of-band rename.
	require.NoError(t, cs.SetTitle(id, "Renamed By User"))

	// Read-back surfaces: Title() and the SessionEntry CustomTitle both
	// reflect the rename (a title != first message reads as a real rename).
	title, err := cs.Title(id)
	require.NoError(t, err)
	assert.Equal(t, "Renamed By User", title, "Title reflects the rename")

	entries, err := cs.ListConvs("")
	require.NoError(t, err)
	e, ok := findEntry(entries, id)
	require.True(t, ok)
	assert.Equal(t, "Renamed By User", e.CustomTitle, "ListConvs surfaces the rename as CustomTitle")
}

func TestCodexConvStore_SetTitle_NoStateDB(t *testing.T) {
	codexTestHome(t) // fresh HOME, no ~/.codex/state_5.sqlite
	err := codexConvs(t).SetTitle("019ec004-4250-79b1-9ade-ebaea4135453", "x")
	require.Error(t, err, "rename with no state DB must fail (conversation not renameable)")
	assert.Contains(t, err.Error(), "no state DB")
}

func TestCodexConvStore_SetTitle_NoRow(t *testing.T) {
	home := codexTestHome(t)
	// The DB + table exist (some other conv has a row), but not for the id
	// we rename — SetTitle is an UPDATE, never an insert.
	writeCodexThread(t, home, codexThreadSeed{
		ID: "aaaa1111-1111-1111-1111-111111111111", Cwd: "/p", Title: "other",
	})
	err := codexConvs(t).SetTitle("bbbb2222-2222-2222-2222-222222222222", "x")
	require.Error(t, err, "rename of a conv with no threads row must fail")
	assert.Contains(t, err.Error(), "no threads row")
}

func TestCodexConvStore_SetTitle_RejectsEmpty(t *testing.T) {
	home := codexTestHome(t)
	const id = "019ec004-4250-79b1-9ade-ebaea4135453"
	writeCodexThread(t, home, codexThreadSeed{ID: id, Cwd: "/p", Title: "keep"})
	cs := codexConvs(t)

	require.Error(t, cs.SetTitle(id, ""), "empty title must be rejected (would blank the title)")
	require.Error(t, cs.SetTitle("", "x"), "empty conv id must be rejected")

	// The guarded write left the existing title intact.
	title, err := cs.Title(id)
	require.NoError(t, err)
	assert.Equal(t, "keep", title)
}
