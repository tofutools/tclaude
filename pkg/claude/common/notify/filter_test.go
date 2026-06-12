package notify

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// setupFilterDB points the db package at a fresh temp HOME so each
// test gets its own SQLite store — same pattern as the db package's
// own setupTestDB.
func setupFilterDB(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	db.ResetForTest()
}

func addMember(t *testing.T, groupID int64, convID string) {
	t.Helper()
	require.NoError(t, db.AddAgentGroupMember(&db.AgentGroupMember{
		GroupID:  groupID,
		ConvID:   convID,
		JoinedAt: time.Now(),
	}))
}

// TestAllowedForConv covers the whole decision ladder: agent pref wins
// outright, any muted active group silences an inheriting agent,
// archived groups don't count, and everything else (including DB-less
// edge cases) fails open.
func TestAllowedForConv(t *testing.T) {
	setupFilterDB(t)

	gid, err := db.CreateAgentGroup("team", "")
	require.NoError(t, err)
	addMember(t, gid, "conv-in-group")

	t.Run("empty conv id fails open", func(t *testing.T) {
		assert.True(t, AllowedForConv(""))
	})
	t.Run("ungrouped conv with no pref is allowed", func(t *testing.T) {
		assert.True(t, AllowedForConv("conv-loose"))
	})
	t.Run("member of an unmuted group is allowed", func(t *testing.T) {
		assert.True(t, AllowedForConv("conv-in-group"))
	})

	t.Run("muting the group silences inheriting members", func(t *testing.T) {
		_, err := db.SetAgentGroupNotifyEnabled("team", false)
		require.NoError(t, err)
		assert.False(t, AllowedForConv("conv-in-group"))
	})
	t.Run("a per-agent 'on' pref overrides the group mute", func(t *testing.T) {
		require.NoError(t, db.SetConvNotifyPref("conv-in-group", db.NotifyPrefOn))
		assert.True(t, AllowedForConv("conv-in-group"))
	})
	t.Run("dropping the pref falls back to the muted group", func(t *testing.T) {
		require.NoError(t, db.SetConvNotifyPref("conv-in-group", db.NotifyPrefInherit))
		assert.False(t, AllowedForConv("conv-in-group"))
	})
	t.Run("unmuting the group restores members", func(t *testing.T) {
		_, err := db.SetAgentGroupNotifyEnabled("team", true)
		require.NoError(t, err)
		assert.True(t, AllowedForConv("conv-in-group"))
	})

	t.Run("a per-agent 'off' pref silences regardless of groups", func(t *testing.T) {
		require.NoError(t, db.SetConvNotifyPref("conv-in-group", db.NotifyPrefOff))
		assert.False(t, AllowedForConv("conv-in-group"))
		require.NoError(t, db.SetConvNotifyPref("conv-loose", db.NotifyPrefOff))
		assert.False(t, AllowedForConv("conv-loose"))
	})

	t.Run("an archived muted group does not silence", func(t *testing.T) {
		gid2, err := db.CreateAgentGroup("old-team", "")
		require.NoError(t, err)
		addMember(t, gid2, "conv-archived")
		_, err = db.SetAgentGroupNotifyEnabled("old-team", false)
		require.NoError(t, err)
		require.NoError(t, db.ArchiveAgentGroup("old-team"))
		assert.True(t, AllowedForConv("conv-archived"))
	})

	t.Run("one muted group among several silences", func(t *testing.T) {
		gidA, err := db.CreateAgentGroup("a-team", "")
		require.NoError(t, err)
		gidB, err := db.CreateAgentGroup("b-team", "")
		require.NoError(t, err)
		addMember(t, gidA, "conv-multi")
		addMember(t, gidB, "conv-multi")
		_, err = db.SetAgentGroupNotifyEnabled("b-team", false)
		require.NoError(t, err)
		assert.False(t, AllowedForConv("conv-multi"))
	})
}
