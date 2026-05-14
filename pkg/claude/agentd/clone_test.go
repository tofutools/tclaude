package agentd

import (
	"testing"

	"github.com/stretchr/testify/assert"
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
	assert.Equal(t, "worker-c-1", got, "first clone")
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
	assert.Equal(t, "worker-c-3", got, "global counter")
}

func TestUniqueCloneAlias_FillsHoles(t *testing.T) {
	// If 1 and 3 are taken but 2 is free, pick 2.
	setupTestDB(t)
	gID, _ := db.CreateAgentGroup("team", "")
	_ = db.AddAgentGroupMember(&db.AgentGroupMember{GroupID: gID, ConvID: "a", Alias: "worker-c-1"})
	_ = db.AddAgentGroupMember(&db.AgentGroupMember{GroupID: gID, ConvID: "b", Alias: "worker-c-3"})

	got := uniqueCloneAlias("worker")
	assert.Equal(t, "worker-c-2", got, "hole-filling")
}

func TestUniqueCloneAlias_CloneOfACloneStripsSuffix(t *testing.T) {
	// Cloning `worker-c-3` strips the suffix to anchor the base (so we
	// don't nest into `worker-c-3-c-1`), but the new N is MONOTONIC
	// w.r.t. the previous clone: prevN=3 → start at 4. We don't loop
	// back to a "free" 1/2 even when their member rows are missing —
	// chronological lineage matters more than slot economy. Same
	// policy as uniqueReincarnateTitle.
	setupTestDB(t)
	gID, _ := db.CreateAgentGroup("team", "")
	_ = db.AddAgentGroupMember(&db.AgentGroupMember{GroupID: gID, ConvID: "a", Alias: "worker"})
	_ = db.AddAgentGroupMember(&db.AgentGroupMember{GroupID: gID, ConvID: "b", Alias: "worker-c-3"})

	got := uniqueCloneAlias("worker-c-3")
	assert.Equal(t, "worker-c-4", got, "clone-of-clone")
}

// TestUniqueCloneAlias_MonotonicFromPrev_PrunedAncestor mirrors the
// reincarnate test of the same shape: when the chronologically-prev
// clone (worker-c-2) is the seed and its predecessor (worker-c-1) is
// not in the index — member removed, group deleted, etc. — the new
// clone must NOT recycle N=1. It bumps to N=3 instead.
func TestUniqueCloneAlias_MonotonicFromPrev_PrunedAncestor(t *testing.T) {
	setupTestDB(t)
	gID, _ := db.CreateAgentGroup("team", "")
	_ = db.AddAgentGroupMember(&db.AgentGroupMember{GroupID: gID, ConvID: "a", Alias: "worker-c-2"})

	got := uniqueCloneAlias("worker-c-2")
	assert.Equal(t, "worker-c-3", got, "monotonic-from-prev")
}

func TestUniqueCloneAlias_EmptyOriginalAliasUsesPrefix(t *testing.T) {
	// Original had no alias in this group. The clone gets `c-N`
	// (no leading dash, no base name).
	setupTestDB(t)
	gID, _ := db.CreateAgentGroup("team", "")
	_ = db.AddAgentGroupMember(&db.AgentGroupMember{GroupID: gID, ConvID: "a", Alias: "c-1"})

	got := uniqueCloneAlias("")
	assert.Equal(t, "c-2", got, "empty base")
}

func TestUniqueCloneAlias_DifferentBasesIndependent(t *testing.T) {
	// Existing aliases for "frontend-c-N" don't affect counter
	// for "worker-c-N".
	setupTestDB(t)
	gID, _ := db.CreateAgentGroup("team", "")
	_ = db.AddAgentGroupMember(&db.AgentGroupMember{GroupID: gID, ConvID: "a", Alias: "frontend-c-1"})
	_ = db.AddAgentGroupMember(&db.AgentGroupMember{GroupID: gID, ConvID: "b", Alias: "frontend-c-2"})

	got := uniqueCloneAlias("worker")
	assert.Equal(t, "worker-c-1", got, "independent base")
}

// TestUniqueCloneAlias_LegacyFormStripsCleanlyOnCloneOfClone covers
// the changeover guarantee: cloning a legacy `-clone-<N>` alias
// strips the legacy suffix and produces a NEW `-c-<N>` alias rather
// than nesting (`worker-clone-3-c-1`). Without the alternation in
// cloneSuffixRegex the legacy form would not be stripped.
//
// Note on N: prev was `-clone-3` so prevN=3 and the monotonic floor
// lifts the new N to 4. We treat the legacy suffix's N as
// chronologically meaningful — same call as the reincarnate side.
func TestUniqueCloneAlias_LegacyFormStripsCleanlyOnCloneOfClone(t *testing.T) {
	setupTestDB(t)
	got := uniqueCloneAlias("worker-clone-3")
	assert.Equal(t, "worker-c-4", got, "legacy-form clone-of-clone")
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
	assert.Equal(t, "worker-c-1", got, "legacy aliases must not reserve new N")
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
	assert.Equal(t, "worker-1-c-1", got, "worker-1 first clone")

	// After one clone, the next bump anchors on `worker-1`. Mirroring
	// the reincarnate test: register the prior clone, then ask again.
	_ = db.AddAgentGroupMember(&db.AgentGroupMember{GroupID: gID, ConvID: "b", Alias: "worker-1-c-1"})
	got = uniqueCloneAlias("worker-1-c-1")
	assert.Equal(t, "worker-1-c-2", got, "worker-1 second clone")

	// worker-2's namespace is independent from worker-1's.
	_ = db.AddAgentGroupMember(&db.AgentGroupMember{GroupID: gID, ConvID: "c", Alias: "worker-2"})
	got = uniqueCloneAlias("worker-2")
	assert.Equal(t, "worker-2-c-1", got, "worker-2 first clone")
}
