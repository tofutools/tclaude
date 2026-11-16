package convops

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// bridgeLine is Claude Code's record of Remote Access coming up, in the
// shape it writes (tk_5ef87574).
func bridgeLine(ts, url string) string {
	return fmt.Sprintf(`{"parentUuid":null,"isSidechain":false,"type":"system","subtype":"bridge_status",`+
		`"content":"/remote-control is active · Continue here, on your phone, or at %s","url":%q,"isMeta":false,`+
		`"timestamp":%q,"cwd":"/proj","sessionId":%q}`, url, url, ts, followerTestConvID) + "\n"
}

// The follower reads the Remote Access link from bridge_status records: the
// latest wins, a link on another origin is ignored, and a full reparse
// agrees with the incremental read.
func TestConvFollower_RemoteAccessLink(t *testing.T) {
	path := filepath.Join(t.TempDir(), followerTestConvID+".jsonl")
	require.NoError(t, os.WriteFile(path, []byte(userLine("main", "2026-03-01T10:00:00Z", "hi")), 0o600))
	f := newConvFollower(followerTestConvID)
	entry, _, err := f.refresh(path, statOf(t, path))
	require.NoError(t, err)
	require.Empty(t, entry.RCSessionURL, "no link before Remote Access came up")

	const first, second = "https://claude.ai/code/session_01AAA", "https://claude.ai/code/session_01BBB"
	for _, step := range []struct{ line, url, at string }{
		{bridgeLine("2026-03-01T10:05:00.123Z", first), first, "2026-03-01T10:05:00.123Z"},
		{bridgeLine("2026-03-01T10:06:00Z", "https://evil.example/code/session_x"), first, "2026-03-01T10:05:00.123Z"},
		{bridgeLine("2026-03-01T10:09:00Z", second), second, "2026-03-01T10:09:00Z"},
	} {
		appendToFile(t, path, step.line)
		entry, _, err = f.refresh(path, statOf(t, path))
		require.NoError(t, err)
		require.Equal(t, step.url, entry.RCSessionURL)
		at, _ := time.Parse(time.RFC3339Nano, step.at)
		require.True(t, entry.RCSessionURLAt.Equal(at), "%s vs %s", entry.RCSessionURLAt, at)
	}
	full, _ := parseJSONLSession(path, followerTestConvID)
	require.Equal(t, entry.RCSessionURL, full.RCSessionURL, "a full reparse agrees")
}

// The link lands on the launch the record belongs to: a record from before
// a launch started is not its link, a newer record replaces an older one, and
// a link cleared by a switch is not written back by a re-read.
func TestRecordSessionRCLink_PerLaunch(t *testing.T) {
	setupTestDB(t)
	const conv = followerTestConvID
	launch := time.Now().Add(-time.Hour).UTC()
	require.NoError(t, db.SaveSession(&db.SessionRow{ID: "s1", ConvID: conv, TmuxSession: "t1", Status: "idle", CreatedAt: launch, UpdatedAt: launch}))
	link := func(id string) string {
		rows, err := db.FindSessionsByConvID(conv)
		require.NoError(t, err)
		for _, r := range rows {
			if r.ID == id {
				return r.RCSessionURL
			}
		}
		t.Fatalf("no session %s", id)
		return ""
	}

	wrote, err := db.RecordSessionRCLink(conv, "https://claude.ai/code/session_old", launch.Add(-time.Minute))
	require.NoError(t, err)
	require.False(t, wrote, "a record from before the launch is not its link")

	at := launch.Add(time.Minute)
	wrote, err = db.RecordSessionRCLink(conv, "https://claude.ai/code/session_a", at)
	require.NoError(t, err)
	require.True(t, wrote)
	require.Equal(t, "https://claude.ai/code/session_a", link("s1"))
	wrote, _ = db.RecordSessionRCLink(conv, "https://claude.ai/code/session_a", at)
	require.False(t, wrote, "a re-read writes nothing new")
	wrote, _ = db.RecordSessionRCLink(conv, "https://other.example/x", at.Add(time.Minute))
	require.False(t, wrote, "another origin is ignored")

	require.NoError(t, db.ClearSessionRCLink("s1"))
	wrote, _ = db.RecordSessionRCLink(conv, "https://claude.ai/code/session_a", at)
	require.False(t, wrote, "a cleared link is not written back")
	require.Empty(t, link("s1"))
	wrote, _ = db.RecordSessionRCLink(conv, "https://claude.ai/code/session_b", at.Add(time.Minute))
	require.True(t, wrote, "a newer record is")

	// A relaunch: the next record is the new launch's, the old launch keeps
	// what it had.
	relaunch := at.Add(10 * time.Minute)
	require.NoError(t, db.SaveSession(&db.SessionRow{ID: "s2", ConvID: conv, TmuxSession: "t1", Status: "idle", CreatedAt: relaunch, UpdatedAt: relaunch}))
	wrote, _ = db.RecordSessionRCLink(conv, "https://claude.ai/code/session_c", relaunch.Add(time.Second))
	require.True(t, wrote)
	require.Equal(t, "https://claude.ai/code/session_c", link("s2"))
	require.Equal(t, "https://claude.ai/code/session_b", link("s1"))
}
