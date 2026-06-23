package agentd_test

import (
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// Scenario: every session/conv tclaude spawns is tagged with the harness
// it belongs to (schema v56). Until a non-Claude harness exists, that tag
// must be "claude" everywhere — the column default + the empty→claude
// coalescing on the write path — so no existing reader changes behavior.
//
// This pins the tag at the two real read surfaces:
//   - the session row (db.FindSessionsByConvID — what agentd's tmux
//     nudge / identity resolution walk), and
//   - the conv_index row as the dashboard refreshes it
//     (agent.FreshConvRowResolved).
//
// It's the end-to-end counterpart to the v56 migration round-trip unit
// tests: those exercise SaveSession / UpsertConvIndex in isolation; this
// runs the production spawn → SaveSession → .jsonl-scan → read path
// unchanged and confirms "claude" falls out at both surfaces.
func TestSpawn_TagsHarnessClaude(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFlow(t)

		f.HaveGroup("alpha")
		spawn := f.AsHuman().Spawn("alpha", "worker")

		// Let the post-spawn /rename land so the conv has a scannable title
		// turn in its .jsonl (the FreshConvRowResolved scan needs content).
		f.AssertGroupMember("alpha", spawn.ConvID, "worker", 5*time.Second)

		// Session side: the row the simSpawner wrote via the production
		// db.SaveSession path carries the default harness.
		sessions, err := db.FindSessionsByConvID(spawn.ConvID)
		require.NoError(t, err, "FindSessionsByConvID")
		require.NotEmpty(t, sessions, "spawned session row should exist")
		assert.Equal(t, "claude", sessions[0].Harness, "spawned session is tagged claude")

		// Conv-index side: the row the dashboard refreshes through, after the
		// .jsonl scan upserts it. The scan leaves SessionEntry.Harness empty;
		// UpsertConvIndex coalesces it to claude.
		row := agent.FreshConvRowResolved(spawn.ConvID)
		require.NotNil(t, row, "conv_index row should resolve after the scan")
		assert.Equal(t, "claude", row.Harness, "scanned conv is tagged claude")
	})
}
