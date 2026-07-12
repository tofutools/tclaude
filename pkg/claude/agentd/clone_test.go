package agentd

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
)

func TestInheritEffectiveSandboxSnapshotPreservesPrePersistedCloneSnapshot(t *testing.T) {
	setupTestDB(t)
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	effective, err := sandboxpolicy.Resolve(sandboxpolicy.Scopes{Global: &sandboxpolicy.Profile{
		Name: "cache", AgentDirectories: []string{"GOCACHE"},
	}})
	require.NoError(t, err)
	source, sourceCleanup, err := materializeAgentDirectories(sandboxpolicy.NewSnapshot(effective, nil), "source")
	require.NoError(t, err)
	t.Cleanup(sourceCleanup)
	target, targetCleanup, err := materializeAgentDirectories(source, "target")
	require.NoError(t, err)
	t.Cleanup(targetCleanup)

	for conv, snapshot := range map[string]*sandboxpolicy.Snapshot{"source-conv": &source, "target-conv": &target} {
		agentID, _, ensureErr := db.EnsureAgentForConv(conv, "test")
		require.NoError(t, ensureErr)
		require.NoError(t, db.SetAgentEffectiveSandboxConfig(agentID, snapshot))
	}
	require.NoError(t, inheritEffectiveSandboxSnapshot("source-conv", "target-conv"))
	persisted, err := db.AgentEffectiveSandboxConfigForConv("target-conv")
	require.NoError(t, err)
	require.NotNil(t, persisted)
	assert.Equal(t, target.Effective.Environment, persisted.Effective.Environment)
	assert.NotEqual(t, source.Effective.Environment, persisted.Effective.Environment)
}

// uniqueCloneTitle always appends `-c-N` (the shortened suffix scheme).
// N is the smallest integer not already used by any
// conv_index.custom_title matching the new prefix; clone-of-a-clone
// strips the existing `-c-N` (or legacy `-clone-N`) before recomputing.
// The upsertCustomTitle helper lives in reincarnate_test.go (same
// package).

func TestUniqueCloneTitle_NoExistingCloneStartsAt1(t *testing.T) {
	setupTestDB(t)
	upsertCustomTitle(t, "a", "worker")

	got := uniqueCloneTitle("worker")
	assert.Equal(t, "worker-c-1", got, "first clone")
}

func TestUniqueCloneTitle_GlobalCounter(t *testing.T) {
	// The new clone should pick the smallest free N across all
	// conv_index titles.
	setupTestDB(t)
	upsertCustomTitle(t, "a", "worker")
	upsertCustomTitle(t, "b", "worker-c-1")
	upsertCustomTitle(t, "c", "worker-c-2")

	got := uniqueCloneTitle("worker")
	assert.Equal(t, "worker-c-3", got, "global counter")
}

func TestUniqueCloneTitle_FillsHoles(t *testing.T) {
	// If 1 and 3 are taken but 2 is free, pick 2.
	setupTestDB(t)
	upsertCustomTitle(t, "a", "worker-c-1")
	upsertCustomTitle(t, "b", "worker-c-3")

	got := uniqueCloneTitle("worker")
	assert.Equal(t, "worker-c-2", got, "hole-filling")
}

func TestUniqueCloneTitle_CloneOfACloneStripsSuffix(t *testing.T) {
	// Cloning `worker-c-3` strips the suffix to anchor the base (so we
	// don't nest into `worker-c-3-c-1`), but the new N is MONOTONIC
	// w.r.t. the previous clone: prevN=3 → start at 4. We don't loop
	// back to a "free" 1/2 even when their conv_index rows are missing
	// — chronological lineage matters more than slot economy.
	setupTestDB(t)
	upsertCustomTitle(t, "a", "worker")
	upsertCustomTitle(t, "b", "worker-c-3")

	got := uniqueCloneTitle("worker-c-3")
	assert.Equal(t, "worker-c-4", got, "clone-of-clone")
}

// TestUniqueCloneTitle_MonotonicFromPrev_PrunedAncestor mirrors the
// reincarnate test of the same shape: when the chronologically-prev
// clone (worker-c-2) is the seed and its predecessor (worker-c-1) is
// not in the index — pruned, retitled, etc. — the new clone must NOT
// recycle N=1. It bumps to N=3 instead.
func TestUniqueCloneTitle_MonotonicFromPrev_PrunedAncestor(t *testing.T) {
	setupTestDB(t)
	upsertCustomTitle(t, "a", "worker-c-2")

	got := uniqueCloneTitle("worker-c-2")
	assert.Equal(t, "worker-c-3", got, "monotonic-from-prev")
}

func TestUniqueCloneTitle_EmptyOriginalTitleUsesPrefix(t *testing.T) {
	// Original had no title. The clone gets `c-N` (no leading dash,
	// no base name).
	setupTestDB(t)
	upsertCustomTitle(t, "a", "c-1")

	got := uniqueCloneTitle("")
	assert.Equal(t, "c-2", got, "empty base")
}

func TestUniqueCloneTitle_DifferentBasesIndependent(t *testing.T) {
	// Existing titles for "frontend-c-N" don't affect the counter
	// for "worker-c-N".
	setupTestDB(t)
	upsertCustomTitle(t, "a", "frontend-c-1")
	upsertCustomTitle(t, "b", "frontend-c-2")

	got := uniqueCloneTitle("worker")
	assert.Equal(t, "worker-c-1", got, "independent base")
}

// TestUniqueCloneTitle_LegacyFormStripsCleanlyOnCloneOfClone covers
// the changeover guarantee: cloning a legacy `-clone-<N>` title
// strips the legacy suffix and produces a NEW `-c-<N>` title rather
// than nesting (`worker-clone-3-c-1`). Without the alternation in
// cloneSuffixRegex the legacy form would not be stripped.
//
// Note on N: prev was `-clone-3` so prevN=3 and the monotonic floor
// lifts the new N to 4. We treat the legacy suffix's N as
// chronologically meaningful — same call as the reincarnate side.
func TestUniqueCloneTitle_LegacyFormStripsCleanlyOnCloneOfClone(t *testing.T) {
	setupTestDB(t)
	got := uniqueCloneTitle("worker-clone-3")
	assert.Equal(t, "worker-c-4", got, "legacy-form clone-of-clone")
}

// TestUniqueCloneTitle_LegacyTitlesDoNotReserveNewN documents the
// "two namespaces, no cross-reservation" rule: existing legacy
// `-clone-<N>` titles do NOT block matching `-c-<N>` numbers in
// the new scheme. This avoids surprising holes immediately after a
// changeover (everyone starts cleanly at -c-1).
func TestUniqueCloneTitle_LegacyTitlesDoNotReserveNewN(t *testing.T) {
	setupTestDB(t)
	upsertCustomTitle(t, "a", "worker-clone-1")
	upsertCustomTitle(t, "b", "worker-clone-2")

	got := uniqueCloneTitle("worker")
	assert.Equal(t, "worker-c-1", got, "legacy titles must not reserve new N")
}

// TestUniqueCloneTitle_NumericSuffixInBaseName covers the same
// gotcha as the reincarnate side: titles like `worker-1`,
// `worker-2` (common when humans hand-name multiple workers) must
// NOT have their trailing `-N` mistaken for a `-c-N` suffix. The
// regex requires `-c-` or `-clone-` literal between the base and
// the digits, so `worker-1` (just `dash + digit`) doesn't match
// and the base stays whole.
func TestUniqueCloneTitle_NumericSuffixInBaseName(t *testing.T) {
	setupTestDB(t)
	upsertCustomTitle(t, "a", "worker-1")

	// Cloning worker-1: the "1" stays as part of the base.
	got := uniqueCloneTitle("worker-1")
	assert.Equal(t, "worker-1-c-1", got, "worker-1 first clone")

	// After one clone, the next bump anchors on `worker-1`. Mirroring
	// the reincarnate test: register the prior clone, then ask again.
	upsertCustomTitle(t, "b", "worker-1-c-1")
	got = uniqueCloneTitle("worker-1-c-1")
	assert.Equal(t, "worker-1-c-2", got, "worker-1 second clone")

	// worker-2's namespace is independent from worker-1's.
	upsertCustomTitle(t, "c", "worker-2")
	got = uniqueCloneTitle("worker-2")
	assert.Equal(t, "worker-2-c-1", got, "worker-2 first clone")
}
