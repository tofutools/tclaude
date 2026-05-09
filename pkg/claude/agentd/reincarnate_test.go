package agentd

import (
	"testing"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// uniqueReincarnateTitle always returns `<base>-reincarnate-N`. N is
// the smallest integer not already used by any conv_index.custom_title;
// reincarnating-a-reincarnate strips the existing `-reincarnate-N`
// before recomputing. Mirrors uniqueCloneAlias's contract exactly,
// just on a different namespace.

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
	if got != "worker-reincarnate-1" {
		t.Errorf("first reincarnation: got %q, want %q", got, "worker-reincarnate-1")
	}
}

func TestUniqueReincarnateTitle_GlobalCounter(t *testing.T) {
	setupTestDB(t)
	upsertCustomTitle(t, "a", "worker")
	upsertCustomTitle(t, "b", "worker-reincarnate-1")
	upsertCustomTitle(t, "c", "worker-reincarnate-2")

	got := uniqueReincarnateTitle("worker")
	if got != "worker-reincarnate-3" {
		t.Errorf("global counter: got %q, want %q", got, "worker-reincarnate-3")
	}
}

func TestUniqueReincarnateTitle_FillsHoles(t *testing.T) {
	setupTestDB(t)
	upsertCustomTitle(t, "a", "worker-reincarnate-1")
	upsertCustomTitle(t, "b", "worker-reincarnate-3")

	got := uniqueReincarnateTitle("worker")
	if got != "worker-reincarnate-2" {
		t.Errorf("hole-filling: got %q, want %q", got, "worker-reincarnate-2")
	}
}

func TestUniqueReincarnateTitle_StripsExistingSuffix(t *testing.T) {
	// Reincarnating `worker-reincarnate-3` should yield
	// `worker-reincarnate-N` (bumped), not
	// `worker-reincarnate-3-reincarnate-1` (nested).
	setupTestDB(t)
	upsertCustomTitle(t, "a", "worker")
	upsertCustomTitle(t, "b", "worker-reincarnate-3")

	got := uniqueReincarnateTitle("worker-reincarnate-3")
	if got != "worker-reincarnate-1" {
		t.Errorf("reincarnate-of-reincarnate: got %q, want %q", got, "worker-reincarnate-1")
	}
}

func TestUniqueReincarnateTitle_EmptyPrevUsesPrefix(t *testing.T) {
	setupTestDB(t)
	upsertCustomTitle(t, "a", "reincarnate-1")

	got := uniqueReincarnateTitle("")
	if got != "reincarnate-2" {
		t.Errorf("empty base: got %q, want %q", got, "reincarnate-2")
	}
}

func TestUniqueReincarnateTitle_DifferentBasesIndependent(t *testing.T) {
	setupTestDB(t)
	upsertCustomTitle(t, "a", "frontend-reincarnate-1")
	upsertCustomTitle(t, "b", "frontend-reincarnate-2")

	got := uniqueReincarnateTitle("worker")
	if got != "worker-reincarnate-1" {
		t.Errorf("base independence: got %q, want %q", got, "worker-reincarnate-1")
	}
}
