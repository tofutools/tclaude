package agentd

import (
	"testing"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// uniqueReincarnateTitle always returns `<base>-r-N` (the shortened
// suffix scheme; `-r-` pairs with `-c-` for clones). N is the
// smallest integer not already used by any conv_index.custom_title
// matching the new prefix; reincarnating-a-reincarnate strips the
// existing `-r-N` (or legacy `-reincarnate-N`) before recomputing.
// Mirrors uniqueCloneAlias's contract exactly, just on a different
// namespace.

func upsertCustomTitle(t *testing.T, convID, title string) {
	t.Helper()
	if err := db.UpsertConvIndex(&db.ConvIndexRow{
		ConvID:      convID,
		CustomTitle: title,
	}); err != nil {
		t.Fatalf("UpsertConvIndex(%q): %v", convID, err)
	}
}

func TestUniqueReincarnateTitle_NoExistingStartsAt1(t *testing.T) {
	setupTestDB(t)
	upsertCustomTitle(t, "a", "worker")

	got := uniqueReincarnateTitle("worker")
	if got != "worker-r-1" {
		t.Errorf("first reincarnation: got %q, want %q", got, "worker-r-1")
	}
}

func TestUniqueReincarnateTitle_GlobalCounter(t *testing.T) {
	setupTestDB(t)
	upsertCustomTitle(t, "a", "worker")
	upsertCustomTitle(t, "b", "worker-r-1")
	upsertCustomTitle(t, "c", "worker-r-2")

	got := uniqueReincarnateTitle("worker")
	if got != "worker-r-3" {
		t.Errorf("global counter: got %q, want %q", got, "worker-r-3")
	}
}

func TestUniqueReincarnateTitle_FillsHoles(t *testing.T) {
	setupTestDB(t)
	upsertCustomTitle(t, "a", "worker-r-1")
	upsertCustomTitle(t, "b", "worker-r-3")

	got := uniqueReincarnateTitle("worker")
	if got != "worker-r-2" {
		t.Errorf("hole-filling: got %q, want %q", got, "worker-r-2")
	}
}

func TestUniqueReincarnateTitle_StripsExistingSuffix(t *testing.T) {
	// Reincarnating `worker-r-3` should yield `worker-r-N` (bumped),
	// not `worker-r-3-r-1` (nested). 1 and 2 are free, so we expect 1.
	setupTestDB(t)
	upsertCustomTitle(t, "a", "worker")
	upsertCustomTitle(t, "b", "worker-r-3")

	got := uniqueReincarnateTitle("worker-r-3")
	if got != "worker-r-1" {
		t.Errorf("reincarnate-of-reincarnate: got %q, want %q", got, "worker-r-1")
	}
}

func TestUniqueReincarnateTitle_EmptyPrevUsesPrefix(t *testing.T) {
	setupTestDB(t)
	upsertCustomTitle(t, "a", "r-1")

	got := uniqueReincarnateTitle("")
	if got != "r-2" {
		t.Errorf("empty base: got %q, want %q", got, "r-2")
	}
}

func TestUniqueReincarnateTitle_DifferentBasesIndependent(t *testing.T) {
	setupTestDB(t)
	upsertCustomTitle(t, "a", "frontend-r-1")
	upsertCustomTitle(t, "b", "frontend-r-2")

	got := uniqueReincarnateTitle("worker")
	if got != "worker-r-1" {
		t.Errorf("base independence: got %q, want %q", got, "worker-r-1")
	}
}

// TestUniqueReincarnateTitle_LegacyFormStripsCleanlyOnReincarnateOfReincarnate
// covers the changeover guarantee: reincarnating a legacy
// `-reincarnate-<N>` title strips the legacy suffix and produces a
// NEW `-r-<N>` title rather than nesting
// (`worker-reincarnate-3-r-1`). Without the alternation in
// reincarnateSuffixRegex the legacy form would not be stripped.
func TestUniqueReincarnateTitle_LegacyFormStripsCleanlyOnReincarnateOfReincarnate(t *testing.T) {
	setupTestDB(t)
	got := uniqueReincarnateTitle("worker-reincarnate-3")
	if got != "worker-r-1" {
		t.Errorf("legacy-form reincarnate-of-reincarnate: got %q, want %q",
			got, "worker-r-1")
	}
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
	if got != "worker-r-1" {
		t.Errorf("legacy titles must not reserve new N: got %q, want %q",
			got, "worker-r-1")
	}
}
