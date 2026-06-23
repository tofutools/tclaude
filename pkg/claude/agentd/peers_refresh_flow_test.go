package agentd_test

import (
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/conv"
)

// Scenario: a human runs `tclaude agent ls` and one of the listed
// agents was spawned (and renamed) but never picked up by a `conv ls`
// indexing pass — so conv_index has no row for it.
//
// The paper-cut this guards: handlePeers used to read titles straight
// from db.GetConvIndex, so a never-indexed agent rendered as the bare
// "(unknown)" placeholder. handlePeers now resolves names through
// agent.FreshTitle, which refreshes the conv_index row from the
// underlying .jsonl first — the same source-of-truth refresh the
// dashboard already did. The name surfaces with no manual `conv ls`.
func TestPeers_RefreshesNeverIndexedMemberFromSource(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFlow(t)

		// 36-char UUID shape — ScanAndUpsertFile gates on len==36.
		const bobConv = "bbbbbbb1-aaaa-bbbb-cccc-000000000001"

		f.HaveGroup("alpha")
		f.HaveMember("alpha", bobConv)
		// HaveAliveSession materialises Bob's .jsonl + session row but does
		// NOT index him into conv_index.
		f.HaveAliveSession(bobConv, "spwn-bob", "tmux-bob", "/tmp/bob")

		cc := f.World.CCs.GetByConvID(bobConv)
		require.NotNil(t, cc, "CCSim for bob")
		require.NoError(t, cc.WriteCustomTitle("Bob The Builder"), "rename bob on disk")

		// Precondition: conv_index genuinely has no row for Bob — the
		// "(unknown)" trigger the old code path hit.
		if row, _ := db.GetConvIndex(bobConv); row != nil {
			t.Fatalf("precondition: conv_index should have no row for bob, got %+v", row)
		}

		peer := f.AsHuman().FindPeer(bobConv)
		require.NotNil(t, peer, "human's `agent ls` should list bob")
		// NAME comes from the .jsonl refresh — an agent's single name.
		assert.Equal(t, "Bob The Builder", peer.Title, "name refreshed from source")
	})
}

// Scenario: an agent was indexed once, then renamed again afterwards.
// conv_index carries the stale title; the .jsonl on disk carries the
// current one. `tclaude agent ls` must reflect the source, not the
// stale cache — "files that may have been updated since the last db
// update" verbatim.
func TestPeers_RefreshesStaleTitleFromSource(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFlow(t)

		// 36-char UUID shape — ScanAndUpsertFile gates on len==36.
		const bobConv = "bbbbbbb2-aaaa-bbbb-cccc-000000000002"

		f.HaveGroup("beta")
		f.HaveMember("beta", bobConv)
		f.HaveAliveSession(bobConv, "spwn-bob", "tmux-bob", "/tmp/bob")

		cc := f.World.CCs.GetByConvID(bobConv)
		require.NotNil(t, cc, "CCSim for bob")

		// First rename, then index it — this is the "last db update".
		require.NoError(t, cc.WriteCustomTitle("Old Name"), "first rename")
		require.NotNil(t, conv.ScanAndUpsertFile(cc.JsonlPath), "index bob")
		row, _ := db.GetConvIndex(bobConv)
		require.NotNil(t, row, "conv_index row after scan")
		require.Equal(t, "Old Name", row.CustomTitle, "stale title cached")

		// Rename again — the .jsonl moves on; conv_index does not.
		require.NoError(t, cc.WriteCustomTitle("New Name"), "second rename")

		peer := f.AsHuman().FindPeer(bobConv)
		require.NotNil(t, peer, "human's `agent ls` should list bob")
		assert.Equal(t, "New Name", peer.Title, "name reflects source, not stale db row")
	})
}
