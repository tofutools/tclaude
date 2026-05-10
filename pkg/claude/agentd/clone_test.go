package agentd

import (
	"testing"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// uniqueCloneAlias always appends `-c-N` (the shortened suffix scheme;
// `-c-` pairs with `-r-` for reincarnations). N is the smallest
// integer not already used by any matching alias system-wide; clone-
// of-a-clone strips the existing `-c-N` (or legacy `-clone-N`) before
// recomputing.

func TestUniqueCloneAlias_NoExistingCloneStartsAt1(t *testing.T) {
	setupTestDB(t)
	gID, _ := db.CreateAgentGroup("team", "")
	_ = db.AddAgentGroupMember(&db.AgentGroupMember{GroupID: gID, ConvID: "a", Alias: "worker"})

	got := uniqueCloneAlias("worker")
	if got != "worker-c-1" {
		t.Errorf("first clone: got %q, want %q", got, "worker-c-1")
	}
}

func TestUniqueCloneAlias_GlobalCounterAcrossGroups(t *testing.T) {
	// The new clone should pick the smallest free N regardless of
	// which groups the existing clones live in.
	setupTestDB(t)
	g1, _ := db.CreateAgentGroup("team-a", "")
	g2, _ := db.CreateAgentGroup("team-b", "")
	_ = db.AddAgentGroupMember(&db.AgentGroupMember{GroupID: g1, ConvID: "a", Alias: "worker"})
	_ = db.AddAgentGroupMember(&db.AgentGroupMember{GroupID: g1, ConvID: "b", Alias: "worker-c-1"})
	// "worker-c-2" lives in a DIFFERENT group; the next clone must
	// still skip past it.
	_ = db.AddAgentGroupMember(&db.AgentGroupMember{GroupID: g2, ConvID: "c", Alias: "worker-c-2"})

	got := uniqueCloneAlias("worker")
	if got != "worker-c-3" {
		t.Errorf("global counter: got %q, want %q", got, "worker-c-3")
	}
}

func TestUniqueCloneAlias_FillsHoles(t *testing.T) {
	// If 1 and 3 are taken but 2 is free, pick 2.
	setupTestDB(t)
	gID, _ := db.CreateAgentGroup("team", "")
	_ = db.AddAgentGroupMember(&db.AgentGroupMember{GroupID: gID, ConvID: "a", Alias: "worker-c-1"})
	_ = db.AddAgentGroupMember(&db.AgentGroupMember{GroupID: gID, ConvID: "b", Alias: "worker-c-3"})

	got := uniqueCloneAlias("worker")
	if got != "worker-c-2" {
		t.Errorf("hole-filling: got %q, want %q", got, "worker-c-2")
	}
}

func TestUniqueCloneAlias_CloneOfACloneStripsSuffix(t *testing.T) {
	// Cloning `worker-c-3` should yield `worker-c-N` (bumped), not
	// `worker-c-3-c-1` (nested). 1 and 2 are free, so we expect 1.
	setupTestDB(t)
	gID, _ := db.CreateAgentGroup("team", "")
	_ = db.AddAgentGroupMember(&db.AgentGroupMember{GroupID: gID, ConvID: "a", Alias: "worker"})
	_ = db.AddAgentGroupMember(&db.AgentGroupMember{GroupID: gID, ConvID: "b", Alias: "worker-c-3"})

	got := uniqueCloneAlias("worker-c-3")
	if got != "worker-c-1" {
		t.Errorf("clone-of-clone: got %q, want %q", got, "worker-c-1")
	}
}

func TestUniqueCloneAlias_EmptyOriginalAliasUsesPrefix(t *testing.T) {
	// Original had no alias in this group. The clone gets `c-N`
	// (no leading dash, no base name).
	setupTestDB(t)
	gID, _ := db.CreateAgentGroup("team", "")
	_ = db.AddAgentGroupMember(&db.AgentGroupMember{GroupID: gID, ConvID: "a", Alias: "c-1"})

	got := uniqueCloneAlias("")
	if got != "c-2" {
		t.Errorf("empty base: got %q, want %q", got, "c-2")
	}
}

func TestUniqueCloneAlias_DifferentBasesIndependent(t *testing.T) {
	// Existing aliases for "frontend-c-N" don't affect counter
	// for "worker-c-N".
	setupTestDB(t)
	gID, _ := db.CreateAgentGroup("team", "")
	_ = db.AddAgentGroupMember(&db.AgentGroupMember{GroupID: gID, ConvID: "a", Alias: "frontend-c-1"})
	_ = db.AddAgentGroupMember(&db.AgentGroupMember{GroupID: gID, ConvID: "b", Alias: "frontend-c-2"})

	got := uniqueCloneAlias("worker")
	if got != "worker-c-1" {
		t.Errorf("independent base: got %q, want %q", got, "worker-c-1")
	}
}

// TestUniqueCloneAlias_LegacyFormStripsCleanlyOnCloneOfClone covers
// the changeover guarantee: cloning a legacy `-clone-<N>` alias
// strips the legacy suffix and produces a NEW `-c-<N>` alias rather
// than nesting (`worker-clone-3-c-1`). Without the alternation in
// cloneSuffixRegex the legacy form would not be stripped.
func TestUniqueCloneAlias_LegacyFormStripsCleanlyOnCloneOfClone(t *testing.T) {
	setupTestDB(t)
	got := uniqueCloneAlias("worker-clone-3")
	if got != "worker-c-1" {
		t.Errorf("legacy-form clone-of-clone: got %q, want %q", got, "worker-c-1")
	}
}

// TestUniqueCloneAlias_LegacyAliasesDoNotReserveNewN documents the
// "two namespaces, no cross-reservation" rule: existing legacy
// `-clone-<N>` aliases do NOT block matching `-c-<N>` numbers in
// the new scheme. This avoids surprising holes immediately after a
// changeover (everyone starts cleanly at -c-1).
func TestUniqueCloneAlias_LegacyAliasesDoNotReserveNewN(t *testing.T) {
	setupTestDB(t)
	gID, _ := db.CreateAgentGroup("team", "")
	_ = db.AddAgentGroupMember(&db.AgentGroupMember{GroupID: gID, ConvID: "a", Alias: "worker-clone-1"})
	_ = db.AddAgentGroupMember(&db.AgentGroupMember{GroupID: gID, ConvID: "b", Alias: "worker-clone-2"})

	got := uniqueCloneAlias("worker")
	if got != "worker-c-1" {
		t.Errorf("legacy aliases must not reserve new N: got %q, want %q", got, "worker-c-1")
	}
}

// TestUniqueCloneAlias_NumericSuffixInBaseName covers the same
// gotcha as the reincarnate side: aliases like `worker-1`,
// `worker-2` (common when humans hand-name multiple workers) must
// NOT have their trailing `-N` mistaken for a `-c-N` suffix. The
// regex requires `-c-` or `-clone-` literal between the base and
// the digits, so `worker-1` (just `dash + digit`) doesn't match
// and the base stays whole.
func TestUniqueCloneAlias_NumericSuffixInBaseName(t *testing.T) {
	setupTestDB(t)
	gID, _ := db.CreateAgentGroup("team", "")
	_ = db.AddAgentGroupMember(&db.AgentGroupMember{GroupID: gID, ConvID: "a", Alias: "worker-1"})

	// Cloning worker-1: the "1" stays as part of the base.
	got := uniqueCloneAlias("worker-1")
	if got != "worker-1-c-1" {
		t.Errorf("worker-1 first clone: got %q, want %q",
			got, "worker-1-c-1")
	}

	// After one clone, the next bump anchors on `worker-1`. Mirroring
	// the reincarnate test: register the prior clone, then ask again.
	_ = db.AddAgentGroupMember(&db.AgentGroupMember{GroupID: gID, ConvID: "b", Alias: "worker-1-c-1"})
	got = uniqueCloneAlias("worker-1-c-1")
	if got != "worker-1-c-2" {
		t.Errorf("worker-1 second clone: got %q, want %q",
			got, "worker-1-c-2")
	}

	// worker-2's namespace is independent from worker-1's.
	_ = db.AddAgentGroupMember(&db.AgentGroupMember{GroupID: gID, ConvID: "c", Alias: "worker-2"})
	got = uniqueCloneAlias("worker-2")
	if got != "worker-2-c-1" {
		t.Errorf("worker-2 first clone: got %q, want %q",
			got, "worker-2-c-1")
	}
}
