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

	// A switch clears the link: neither a re-read nor a record from before
	// the switch writes it back; one written after the switch does.
	require.NoError(t, db.ClearSessionRCLink("s1"))
	wrote, _ = db.RecordSessionRCLink(conv, "https://claude.ai/code/session_a", at)
	require.False(t, wrote, "a cleared link is not written back")
	wrote, _ = db.RecordSessionRCLink(conv, "https://claude.ai/code/session_late", time.Now().Add(-time.Second))
	require.False(t, wrote, "a record from before the switch, reached late, is not this link")
	require.Empty(t, link("s1"))
	after := time.Now().Add(time.Second)
	wrote, _ = db.RecordSessionRCLink(conv, "https://claude.ai/code/session_b", after)
	require.True(t, wrote, "a record after the switch is")

	// The record heals a drifted flag: Remote Access is on.
	require.NoError(t, db.SetSessionRemoteControl("s1", false))
	wrote, _ = db.RecordSessionRCLink(conv, "https://claude.ai/code/session_b2", after.Add(time.Second))
	require.True(t, wrote)
	rows, err := db.FindSessionsByConvID(conv)
	require.NoError(t, err)
	require.True(t, rows[0].RemoteControl, "a bridge_status record heals the flag to on")

	// A launch on a new row starts with no link, and an older record is not
	// its link; the next record is.
	relaunch := after.Add(10 * time.Minute)
	require.NoError(t, db.SaveSession(&db.SessionRow{ID: "s2", ConvID: conv, TmuxSession: "t1", Status: "idle", CreatedAt: relaunch, UpdatedAt: relaunch}))
	require.Empty(t, link("s2"))
	wrote, _ = db.RecordSessionRCLink(conv, "https://claude.ai/code/session_b3", relaunch.Add(-time.Second))
	require.True(t, wrote, "the old row may take it; it is not shown for a dead launch")
	require.Empty(t, link("s2"), "a record from before the launch is not its link")
	wrote, _ = db.RecordSessionRCLink(conv, "https://claude.ai/code/session_c", relaunch.Add(time.Second))
	require.True(t, wrote)
	require.Equal(t, "https://claude.ai/code/session_c", link("s2"))

	// A resume reuses its row: the launch save drops the dead link, and a
	// record from before the resume is not its link.
	resume := relaunch.Add(10 * time.Minute)
	require.NoError(t, db.SaveSession(&db.SessionRow{ID: "s2", ConvID: conv, TmuxSession: "t1", Status: "idle",
		CreatedAt: resume, UpdatedAt: resume, ExitLaunchGeneration: "gen-2"}))
	require.Empty(t, link("s2"), "a resume starts without the last launch's link")
	wrote, _ = db.RecordSessionRCLink(conv, "https://claude.ai/code/session_c2", resume.Add(-time.Second))
	require.Empty(t, link("s2"))
	wrote, _ = db.RecordSessionRCLink(conv, "https://claude.ai/code/session_d", resume.Add(time.Second))
	require.True(t, wrote)
	require.Equal(t, "https://claude.ai/code/session_d", link("s2"))

	// A plain status save (a hook tick) keeps the link.
	require.NoError(t, db.SaveSession(&db.SessionRow{ID: "s2", ConvID: conv, TmuxSession: "t1", Status: "working"}))
	require.Equal(t, "https://claude.ai/code/session_d", link("s2"))
}
