package agentd

import (
	"strings"
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

// TCL-731: the successor's launch prompt is the reincarnation twin of
// buildSpawnLaunchPrompt — inline the handoff when it fits, otherwise point at
// the inbox copy — and it always orients the agent about what it just became.
func TestBuildReincarnationLaunchPrompt(t *testing.T) {
	const handoff = "Continue the TCL-731 migration; the branch is reinc-fix."

	t.Run("short handoff is inlined and notes the inbox copy", func(t *testing.T) {
		got := buildReincarnationLaunchPrompt("worker", "", 42, handoff, 2000)
		assert.Contains(t, got, "[system:", "opens with the system orientation")
		assert.Contains(t, got, `"worker"`, "names the title it was launched with")
		assert.Contains(t, got, "tclaude agent", "keeps the coordination pointer")
		assert.Contains(t, got, "message #42", "notes the inbox copy by id")
		assert.Contains(t, got, handoff, "the handoff rides inline")
		assert.NotContains(t, got, "inbox read", "an inlined handoff needs no round-trip")
	})

	t.Run("multi-line handoff survives verbatim", func(t *testing.T) {
		body := "Where I got to:\n\n- pkg/foo done\n- pkg/bar next\n"
		got := buildReincarnationLaunchPrompt("worker", "", 7, body, 2000)
		assert.Contains(t, got, strings.TrimSpace(body),
			"argv carries newlines a send-keys nudge could not")
	})

	t.Run("over-cap handoff falls back to the inbox pointer", func(t *testing.T) {
		body := strings.Repeat("x", 5000)
		got := buildReincarnationLaunchPrompt("worker", "", 9, body, 2000)
		assert.NotContains(t, got, body, "an over-cap handoff stays in the inbox")
		assert.Contains(t, got, "inbox read 9", "tells the agent how to fetch it")
	})

	t.Run("over-cap handoff inlines anyway when the inbox copy failed", func(t *testing.T) {
		body := strings.Repeat("y", 5000)
		got := buildReincarnationLaunchPrompt("worker", "", 0, body, 2000)
		assert.Contains(t, got, body,
			"with no inbox row to point at, inlining is the only way the successor gets the handoff")
		assert.NotContains(t, got, "message #", "never claims a message that does not exist")
	})

	t.Run("a cross-agent reincarnation names the handoff's author", func(t *testing.T) {
		got := buildReincarnationLaunchPrompt("worker", "manager", 3, handoff, 2000)
		assert.Contains(t, got, "written by manager")
	})

	t.Run("a rejected title is not echoed back as the agent's identity", func(t *testing.T) {
		got := buildReincarnationLaunchPrompt("", "", 3, handoff, 2000)
		assert.Contains(t, got, "you are a fresh reincarnation:",
			"an unnamed successor still gets the orientation, just no name")
	})
}

// A self-reincarnation needs no attribution; a cross-agent one names the
// manager that triggered it.
func TestReincarnationHandoffAuthor(t *testing.T) {
	assert.Equal(t, "", reincarnationHandoffAuthor("same", "same"), "self-reincarnation")
	assert.Equal(t, "", reincarnationHandoffAuthor("", "target"), "human-triggered")
}
