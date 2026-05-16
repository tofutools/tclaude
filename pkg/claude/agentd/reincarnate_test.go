package agentd

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// uniqueReincarnateTitle always returns `<base>-r-N` (the shortened
// suffix scheme; `-r-` pairs with `-c-` for clones). N is the
// smallest integer not already used by any conv_index.custom_title
// matching the new prefix; reincarnating-a-reincarnate strips the
// existing `-r-N` (or legacy `-reincarnate-N`) before recomputing.
// Mirrors uniqueCloneTitle's contract exactly, just on a different
// namespace.

func upsertCustomTitle(t *testing.T, convID, title string) {
	t.Helper()
	require.NoError(t, db.UpsertConvIndex(&db.ConvIndexRow{
		ConvID:      convID,
		CustomTitle: title,
	}), "UpsertConvIndex(%q)", convID)
}

func TestUniqueReincarnateTitle_NoExistingStartsAt1(t *testing.T) {
	setupTestDB(t)
	upsertCustomTitle(t, "a", "worker")

	got := uniqueReincarnateTitle("worker")
	assert.Equal(t, "worker-r-1", got, "first reincarnation")
}

func TestUniqueReincarnateTitle_GlobalCounter(t *testing.T) {
	setupTestDB(t)
	upsertCustomTitle(t, "a", "worker")
	upsertCustomTitle(t, "b", "worker-r-1")
	upsertCustomTitle(t, "c", "worker-r-2")

	got := uniqueReincarnateTitle("worker")
	assert.Equal(t, "worker-r-3", got, "global counter")
}

func TestUniqueReincarnateTitle_FillsHoles(t *testing.T) {
	setupTestDB(t)
	upsertCustomTitle(t, "a", "worker-r-1")
	upsertCustomTitle(t, "b", "worker-r-3")

	got := uniqueReincarnateTitle("worker")
	assert.Equal(t, "worker-r-2", got, "hole-filling")
}

func TestUniqueReincarnateTitle_StripsExistingSuffix(t *testing.T) {
	// Reincarnating `worker-r-3` strips the suffix to anchor the base
	// (so we don't nest into `worker-r-3-r-1`), but the new N is
	// MONOTONIC w.r.t. the previous instance: prevN=3 → start at 4.
	// We don't loop back to a "free" 1/2 even when their conv_index
	// rows are missing — chronological lineage matters more than slot
	// economy.
	setupTestDB(t)
	upsertCustomTitle(t, "a", "worker")
	upsertCustomTitle(t, "b", "worker-r-3")

	got := uniqueReincarnateTitle("worker-r-3")
	assert.Equal(t, "worker-r-4", got, "reincarnate-of-reincarnate")
}

// TestUniqueReincarnateTitle_MonotonicFromPrev_PrunedAncestor pins the
// real bug we hit in production: prev was `tclaude-dev-r-2`, but the
// `-r-1` row was no longer in conv_index (pruned / retitled), so the
// old smallest-free policy reused N=1 — the new instance got titled
// `r-1` even though it's a descendant of `r-2`. Monotonic-from-prev
// makes the result `r-3` regardless of what's missing.
func TestUniqueReincarnateTitle_MonotonicFromPrev_PrunedAncestor(t *testing.T) {
	setupTestDB(t)
	// Only the parent's row is in conv_index; the "r-1" ancestor that
	// chronologically came before is gone.
	upsertCustomTitle(t, "parent", "tclaude-dev-r-2")

	got := uniqueReincarnateTitle("tclaude-dev-r-2")
	assert.Equal(t, "tclaude-dev-r-3", got, "monotonic-from-prev")
}

func TestUniqueReincarnateTitle_EmptyPrevUsesPrefix(t *testing.T) {
	setupTestDB(t)
	upsertCustomTitle(t, "a", "r-1")

	got := uniqueReincarnateTitle("")
	assert.Equal(t, "r-2", got, "empty base")
}

func TestUniqueReincarnateTitle_DifferentBasesIndependent(t *testing.T) {
	setupTestDB(t)
	upsertCustomTitle(t, "a", "frontend-r-1")
	upsertCustomTitle(t, "b", "frontend-r-2")

	got := uniqueReincarnateTitle("worker")
	assert.Equal(t, "worker-r-1", got, "base independence")
}

// TestUniqueReincarnateTitle_LegacyFormStripsCleanlyOnReincarnateOfReincarnate
// covers the changeover guarantee: reincarnating a legacy
// `-reincarnate-<N>` title strips the legacy suffix and produces a
// NEW `-r-<N>` title rather than nesting
// (`worker-reincarnate-3-r-1`). Without the alternation in
// reincarnateSuffixRegex the legacy form would not be stripped.
//
// Note on N: prev was `-reincarnate-3` so prevN=3, and the monotonic
// floor lifts the new N to 4. We treat the legacy suffix's N as
// chronologically meaningful even though it lived in a different
// namespace — the alternative ("legacy resets to 1") was a holdover
// from the pre-monotonic policy and would visibly under-count the
// lineage on the rare migration paths still in flight.
func TestUniqueReincarnateTitle_LegacyFormStripsCleanlyOnReincarnateOfReincarnate(t *testing.T) {
	setupTestDB(t)
	got := uniqueReincarnateTitle("worker-reincarnate-3")
	assert.Equal(t, "worker-r-4", got, "legacy-form reincarnate-of-reincarnate")
}

// TestUniqueReincarnateTitle_LegacyTitlesDoNotReserveNewN documents
// the "two namespaces, no cross-reservation" rule: existing legacy
// `-reincarnate-<N>` titles do NOT block matching `-r-<N>` numbers
// in the new scheme. This avoids surprising holes immediately
// after a changeover (everyone starts cleanly at -r-1).
func TestUniqueReincarnateTitle_LegacyTitlesDoNotReserveNewN(t *testing.T) {
	setupTestDB(t)
	upsertCustomTitle(t, "a", "worker-reincarnate-1")
	upsertCustomTitle(t, "b", "worker-reincarnate-2")

	got := uniqueReincarnateTitle("worker")
	assert.Equal(t, "worker-r-1", got, "legacy titles must not reserve new N")
}

// TestUniqueReincarnateTitle_NumericSuffixInBaseName covers the
// gotcha: titles like `worker-1`, `worker-2` (common when humans
// hand-name multiple workers) must NOT have their trailing `-N`
// mistaken for a `-r-N` suffix. The regex requires `-r-` or
// `-reincarnate-` literal between the base and the digits, so
// `worker-1` (just `dash + digit`) doesn't match and the base
// stays whole. Each numbered worker gets its own independent
// `-r-N` counter.
func TestUniqueReincarnateTitle_NumericSuffixInBaseName(t *testing.T) {
	setupTestDB(t)

	// First reincarnation of worker-1 keeps the "1" as part of the base.
	got := uniqueReincarnateTitle("worker-1")
	assert.Equal(t, "worker-1-r-1", got, "worker-1 first reincarnation")

	// After one reincarnation: the "-r-1" IS recognised and stripped, so
	// the next bump still anchors on `worker-1` as base.
	upsertCustomTitle(t, "a", "worker-1-r-1")
	got = uniqueReincarnateTitle("worker-1-r-1")
	assert.Equal(t, "worker-1-r-2", got, "worker-1 second reincarnation")

	// worker-2's namespace is independent from worker-1's. The
	// `worker-1-r-1` row sitting in the DB doesn't reserve N=1 for
	// `worker-2-r-N`.
	got = uniqueReincarnateTitle("worker-2")
	assert.Equal(t, "worker-2-r-1", got, "worker-2 first reincarnation")
}
