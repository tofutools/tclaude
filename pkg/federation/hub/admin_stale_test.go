package hub

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/federation/proto"
)

func TestHubStaleAdmissionsPreviewAndRecheck(t *testing.T) {
	st, admin, token := adminTestStore(t)
	now := time.Now().UTC()
	require.NoError(t, st.ClaimAdmin(admin.ID(), admin.Pub, token, now))
	h, err := New(st, Config{})
	require.NoError(t, err)
	ids := map[string]*proto.Identity{}
	for _, name := range []string{"old", "reconnected", "promoted", "never_seen", "recent", "online", "stream", "already_revoked"} {
		id, e := proto.NewIdentity()
		require.NoError(t, e)
		ids[name] = id
		require.NoError(t, st.Admit(id.ID()))
		require.NoError(t, st.RecordSeen(id.ID(), id.Pub, name, "test", now.Add(-45*24*time.Hour)))
	}
	require.NoError(t, st.RecordSeen(ids["recent"].ID(), ids["recent"].Pub, "recent", "test", now))
	require.NoError(t, st.Revoke(ids["already_revoked"].ID()))
	_, err = st.db.Exec(`UPDATE instances SET last_seen='',admitted_at=? WHERE instance_id=?`, ts(now), ids["never_seen"].ID())
	require.NoError(t, err)
	require.NoError(t, st.RecordSeen(admin.ID(), admin.Pub, "admin", "test", now.Add(-45*24*time.Hour)))
	online := &conn{id: ids["online"].ID(), boardOnly: true, nonce: "board-test"}
	h.conns[online.registryKey()] = online
	h.streams = &streamState{sessions: map[string]map[*streamSession]bool{ids["stream"].ID(): {&streamSession{id: ids["stream"].ID()}: true}}}
	preview, err := h.cleanupStaleAdmissions(admin.ID(), adminParams{}, now)
	require.NoError(t, err)
	b, err := json.Marshal(preview)
	require.NoError(t, err)
	var plan struct {
		Days      int
		Cutoff    time.Time
		Instances []staleAdmission
	}
	require.NoError(t, json.Unmarshal(b, &plan))
	require.Equal(t, 30, plan.Days)
	selected := []string{}
	for _, row := range plan.Instances {
		selected = append(selected, row.Instance)
	}
	require.ElementsMatch(t, []string{ids["old"].ID(), ids["reconnected"].ID(), ids["promoted"].ID()}, selected)
	old, err := st.Get(ids["old"].ID())
	require.NoError(t, err)
	require.False(t, old.Revoked, "preview is read-only")
	require.NoError(t, st.RecordSeen(ids["reconnected"].ID(), ids["reconnected"].Pub, "reconnected", "test", now))
	require.NoError(t, st.SetAdmin(ids["promoted"].ID(), admin.ID(), []string{"hub.health.read"}))
	// Even explicit IDs in the bulk API cannot opt admins or the current node in.
	selected = append(selected, admin.ID(), ids["recent"].ID(), ids["online"].ID(), ids["stream"].ID())
	result, err := h.cleanupStaleAdmissions(admin.ID(), adminParams{Days: 30, Apply: true, Cutoff: plan.Cutoff, Instances: selected}, now.Add(time.Minute))
	require.NoError(t, err)
	require.Equal(t, []string{ids["old"].ID()}, result.(map[string]any)["revoked"])
	for name, id := range ids {
		row, e := st.Get(id.ID())
		require.NoError(t, e)
		require.Equal(t, name == "old" || name == "already_revoked", row.Revoked, name)
	}
	_, err = h.cleanupStaleAdmissions(admin.ID(), adminParams{Apply: true}, now)
	requireAdminCode(t, err, "confirmation_required")
	_, err = h.cleanupStaleAdmissions(admin.ID(), adminParams{Days: -1}, now)
	requireAdminCode(t, err, "days")
	require.Equal(t, "hub.admissions.manage", adminMethodCapability["admissions.cleanup"])
}

func TestHubStaleDisconnectKeepsProtectionUntilActivitySaved(t *testing.T) {
	for _, kind := range []string{"fleet", "board", "stream"} {
		t.Run(kind, func(t *testing.T) {
			boardOnly := kind == "board"
			st, id, _ := adminTestStore(t)
			old := time.Now().Add(-60 * 24 * time.Hour)
			require.NoError(t, st.RecordSeen(id.ID(), id.Pub, "original name", "original version", old))
			h, err := New(st, Config{})
			require.NoError(t, err)
			h.closed = true // No directory broadcasts during this isolated lifecycle check.
			c := &conn{id: id.ID(), pub: id.Pub, name: "original name", version: "original version", boardOnly: boardOnly, nonce: "fixture"}
			c.once.Do(func() {}) // Socket already closed, as at the end of the read loop.
			h.conns[c.registryKey()] = c
			stream := &streamSession{id: id.ID(), sid: "fixture"}
			stream.once.Do(func() {})
			if kind == "stream" {
				delete(h.conns, c.registryKey())
				h.streams = &streamState{sessions: map[string]map[*streamSession]bool{id.ID(): {stream: true}}}
			}
			st.db.SetMaxOpenConns(1)
			tx, err := st.db.Begin()
			require.NoError(t, err)
			t.Cleanup(func() { _ = tx.Rollback() })
			waits := st.db.Stats().WaitCount
			done := make(chan struct{})
			go func() {
				if kind == "stream" {
					h.dropStream(stream)
				} else {
					h.unregister(c)
				}
				close(done)
			}()
			require.Eventually(t, func() bool { return st.db.Stats().WaitCount > waits }, time.Second, time.Millisecond)
			h.mu.Lock()
			registered := h.conns[c.registryKey()] == c
			if kind == "stream" {
				registered = len(h.streams.sessions[id.ID()]) > 0
			}
			h.mu.Unlock()
			require.True(t, registered, "do not expose stale last_seen while disconnect activity is still being written")
			require.NoError(t, tx.Commit())
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("disconnect did not finish")
			}
			row, err := st.Get(id.ID())
			require.NoError(t, err)
			require.True(t, row.LastSeen.After(old.Add(24*time.Hour)))
			require.Equal(t, "original name", row.Name)
			require.Equal(t, "original version", row.Version)
			changed, err := st.revoke(id.ID(), time.Now().Add(-30*24*time.Hour))
			require.NoError(t, err)
			require.False(t, changed)
		})
	}
}
