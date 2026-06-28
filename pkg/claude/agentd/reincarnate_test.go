package agentd

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// JOH-319 naming: the living successor keeps its plain base name, and the
// retiring predecessor gets the `-x` archive marker — `<prev>-x`, or
// `<prev>-x-<N>` (N >= 2) when an earlier retired generation already holds
// the bare form. The `-r-<N>` scheme (the OLD successor marker) is gone.

func upsertCustomTitle(t *testing.T, convID, title string) {
	t.Helper()
	require.NoError(t, db.UpsertConvIndex(&db.ConvIndexRow{
		ConvID:      convID,
		CustomTitle: title,
	}), "UpsertConvIndex(%q)", convID)
}

// reincarnateBase strips a legacy `-r-<N>` / `-reincarnate-<N>` successor
// suffix so a transition living name falls back to its plain base; a
// title with no such suffix (including a hand-numbered `worker-1`) is
// unchanged.
func TestReincarnateBase(t *testing.T) {
	cases := []struct{ in, want string }{
		{"worker", "worker"},
		{"worker-r-6", "worker"},
		{"worker-reincarnate-3", "worker"},
		{"worker-1", "worker-1"}, // plain numeric tail is NOT a -r-N suffix
		{"worker-x", "worker-x"}, // archive marker is not stripped
		{"", ""},
	}
	for _, c := range cases {
		assert.Equalf(t, c.want, reincarnateBase(c.in), "reincarnateBase(%q)", c.in)
	}
}

func TestRetiredGenerationTitle_FirstRetirementKeepsBareX(t *testing.T) {
	setupTestDB(t)
	// The living gen `worker` is being retired; no prior archive exists.
	upsertCustomTitle(t, "a", "worker")

	got, ok := retiredGenerationTitle("worker")
	assert.True(t, ok)
	assert.Equal(t, "worker-x", got, "first retirement keeps the historical bare -x form")
}

func TestRetiredGenerationTitle_RepeatRetirementAddsCounter(t *testing.T) {
	setupTestDB(t)
	// A prior generation already retired as `worker-x`; the living gen
	// keeps the base name, so this retirement collides on the bare form
	// and must take the next free `-x-<N>`.
	upsertCustomTitle(t, "old1", "worker-x")

	got, ok := retiredGenerationTitle("worker")
	assert.True(t, ok)
	assert.Equal(t, "worker-x-2", got, "second retirement disambiguates with -x-2")
}

func TestRetiredGenerationTitle_CounterFindsSmallestFree(t *testing.T) {
	setupTestDB(t)
	upsertCustomTitle(t, "old1", "worker-x")
	upsertCustomTitle(t, "old2", "worker-x-2")
	upsertCustomTitle(t, "old4", "worker-x-4") // a hole at 3

	got, ok := retiredGenerationTitle("worker")
	assert.True(t, ok)
	assert.Equal(t, "worker-x-3", got, "the counter fills the smallest free slot")
}

// A legacy old-scheme living name (`worker-r-6`, seen only during the
// changeover) keeps its FULL title and just gains `-x` — byte-identical
// to the pre-JOH-319 predecessor naming, so the transition is seamless.
func TestRetiredGenerationTitle_LegacyNumberedPredecessorKeepsItsName(t *testing.T) {
	setupTestDB(t)
	upsertCustomTitle(t, "a", "worker-r-6")

	got, ok := retiredGenerationTitle("worker-r-6")
	assert.True(t, ok)
	assert.Equal(t, "worker-r-6-x", got, "legacy numbered predecessor archives as <prev>-x")
}

// Independent bases don't share a counter namespace: `frontend-x` rows
// must not push `worker`'s first retirement off the bare form.
func TestRetiredGenerationTitle_DifferentBasesIndependent(t *testing.T) {
	setupTestDB(t)
	upsertCustomTitle(t, "f1", "frontend-x")
	upsertCustomTitle(t, "f2", "frontend-x-2")

	got, ok := retiredGenerationTitle("worker")
	assert.True(t, ok)
	assert.Equal(t, "worker-x", got, "another base's archives don't reserve worker's slot")
}

// A hand-numbered worker (`worker-1`) is a base in its own right: its
// trailing `-1` is not a `-r-N` suffix, so it archives as `worker-1-x`.
func TestRetiredGenerationTitle_NumericBaseName(t *testing.T) {
	setupTestDB(t)
	upsertCustomTitle(t, "a", "worker-1")

	got, ok := retiredGenerationTitle("worker-1")
	assert.True(t, ok)
	assert.Equal(t, "worker-1-x", got, "the -1 is part of the base, not a successor suffix")
}

func TestRetiredGenerationTitle_EmptyTitleSkipsRename(t *testing.T) {
	setupTestDB(t)
	_, ok := retiredGenerationTitle("")
	assert.False(t, ok, "an untitled predecessor has nothing to archive-mark")
}

// A LIVING gen named with a trailing `-x` (unusual — `-x` is the archive
// marker) still archives: appending `-x` yields a title distinct from the
// successor's un-suffixed base name, so the retiring predecessor never
// collides with the live successor.
func TestRetiredGenerationTitle_XEndingNameStillArchivesWithoutCollision(t *testing.T) {
	setupTestDB(t)
	upsertCustomTitle(t, "a", "project-x")

	got, ok := retiredGenerationTitle("project-x")
	assert.True(t, ok, "a -x-ending predecessor is still archived")
	assert.Equal(t, "project-x-x", got)
	// The invariant that matters: the retired title differs from the base
	// name the living successor keeps.
	assert.NotEqual(t, reincarnateBase("project-x"), got,
		"retired predecessor title must differ from the successor's base name")
}
