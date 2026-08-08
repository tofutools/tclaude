package db

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAgentLineageDirectTransitiveAndRetirementPersistence(t *testing.T) {
	setupTestDB(t)
	now := time.Now().UTC()
	parent, _, err := EnsureAgentForConv("lineage-parent", "spawn")
	require.NoError(t, err)
	child, _, err := EnsureAgentForConv("lineage-child", "spawn")
	require.NoError(t, err)
	grandchild, _, err := EnsureAgentForConv("lineage-grandchild", "spawn")
	require.NoError(t, err)
	unrelated, _, err := EnsureAgentForConv("lineage-unrelated", "spawn")
	require.NoError(t, err)
	require.NoError(t, RecordAgentLineage(child, parent, now))
	require.NoError(t, RecordAgentLineage(grandchild, child, now.Add(time.Second)))
	require.NoError(t, RecordAgentLineage(child, parent, now.Add(time.Hour)), "same fact is idempotent")
	require.Error(t, RecordAgentLineage(child, unrelated, now), "birth parent is immutable")

	direct, err := IsDirectAgentChild(parent, child)
	require.NoError(t, err)
	assert.True(t, direct)
	direct, err = IsDirectAgentChild(parent, grandchild)
	require.NoError(t, err)
	assert.False(t, direct)
	for _, tc := range []struct {
		target string
		want   bool
	}{{child, true}, {grandchild, true}, {unrelated, false}, {parent, false}} {
		got, err := IsAgentDescendant(parent, tc.target)
		require.NoError(t, err)
		assert.Equal(t, tc.want, got, tc.target)
	}

	retired, err := RetireAgent("lineage-child", "test", "lineage persistence")
	require.NoError(t, err)
	require.True(t, retired)
	direct, err = IsDirectAgentChild(parent, child)
	require.NoError(t, err)
	assert.True(t, direct, "retirement must not erase birth facts")
}

func TestDeleteAgentLineageForChildRemovesOnlyThatBirthEdge(t *testing.T) {
	setupTestDB(t)
	parent := "agt_lineage_delete_parent"
	child := "agt_lineage_delete_child"
	grandchild := "agt_lineage_delete_grandchild"
	require.NoError(t, RecordAgentLineage(child, parent, time.Now()))
	require.NoError(t, RecordAgentLineage(grandchild, child, time.Now()))

	n, err := DeleteAgentLineageForChild(child)
	require.NoError(t, err)
	assert.EqualValues(t, 1, n)
	direct, err := IsDirectAgentChild(parent, child)
	require.NoError(t, err)
	assert.False(t, direct)
	direct, err = IsDirectAgentChild(child, grandchild)
	require.NoError(t, err)
	assert.True(t, direct, "rollback deletion must not sweep a real child's outgoing edges")
}

func TestAgentLineageReincarnationKeepsStableParentActor(t *testing.T) {
	setupTestDB(t)
	parent, _, err := EnsureAgentForConv("lineage-parent-old", "spawn")
	require.NoError(t, err)
	child, _, err := EnsureAgentForConv("lineage-child", "spawn")
	require.NoError(t, err)
	require.NoError(t, RecordAgentLineage(child, parent, time.Now()))

	// The real rotation path may first see a bare self-registered successor;
	// RotateAgentConv absorbs it and advances the predecessor's stable actor.
	_, _, err = EnsureAgentForConv("lineage-parent-new", "session-start")
	require.NoError(t, err)
	_, err = RotateAgentConv("lineage-parent-old", "lineage-parent-new", "reincarnate")
	require.NoError(t, err)
	successorActor, err := AgentIDForConv("lineage-parent-new")
	require.NoError(t, err)
	assert.Equal(t, parent, successorActor, "reincarnation preserves the parent agent_id")
	matched, err := IsAgentDescendant(successorActor, child)
	require.NoError(t, err)
	assert.True(t, matched, "the stable successor inherits the predecessor's children")
}

func TestAgentLineageWalkIsCycleSafeAndDepthBounded(t *testing.T) {
	setupTestDB(t)
	now := time.Now()
	ids := make([]string, MaxAgentLineageDepth+2)
	for i := range ids {
		ids[i] = fmt.Sprintf("agt_depth_%03d", i)
	}
	for i := 1; i < len(ids); i++ {
		require.NoError(t, RecordAgentLineage(ids[i], ids[i-1], now.Add(time.Duration(i))))
	}
	atBound, err := IsAgentDescendant(ids[0], ids[MaxAgentLineageDepth])
	require.NoError(t, err)
	assert.True(t, atBound)
	beyondBound, err := IsAgentDescendant(ids[0], ids[MaxAgentLineageDepth+1])
	require.NoError(t, err)
	assert.False(t, beyondBound, "paths beyond the authorization bound fail closed")

	// A separate malformed cycle terminates and never makes an actor its own
	// descendant. Direct SQL models corruption impossible through the writer.
	d, err := Open()
	require.NoError(t, err)
	mustExec(t, d, `INSERT INTO agent_lineage(child_agent_id, parent_agent_id, spawned_at)
		VALUES ('agt_cycle_b', 'agt_cycle_a', 1),
		       ('agt_cycle_c', 'agt_cycle_b', 2),
		       ('agt_cycle_a', 'agt_cycle_c', 3)`)
	self, err := IsAgentDescendant("agt_cycle_a", "agt_cycle_a")
	require.NoError(t, err)
	assert.False(t, self)
}
