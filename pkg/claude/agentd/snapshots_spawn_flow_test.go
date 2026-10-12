package agentd_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testutil"
)

func TestSnapshotSpawnFlow(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("receiver")
	const source = "019fe740-43a4-7023-b8ae-1ee64459f2d1"
	const other = "019fe740-43a4-7023-b8ae-1ee64459f2d2"
	cwd := testutil.CanonicalTempDir(t)
	f.HaveAliveSession(source, "original", "original-pane", cwd)
	f.HaveAliveSession(other, "bystander", "bystander-pane", testutil.CanonicalTempDir(t))
	f.HaveMember("receiver", other)

	post := func(path string, body any, asAgent string) *httptest.ResponseRecorder {
		var buf bytes.Buffer
		require.NoError(t, json.NewEncoder(&buf).Encode(body))
		r := httptest.NewRequest(http.MethodPost, path, &buf)
		if asAgent == "" {
			r = agentd.AsHumanPeer(r)
		} else {
			r = agentd.AsAgentPeer(r, asAgent)
		}
		rec := httptest.NewRecorder()
		f.Mux.ServeHTTP(rec, r)
		return rec
	}

	rec := post("/v1/snapshots", map[string]any{"agent": source, "name": "known-good"}, "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	rec = post("/v1/snapshots/known-good/spawn", map[string]any{"group": "receiver", "count": 21}, "")
	assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	rec = post("/v1/snapshots/missing/spawn", map[string]any{"group": "receiver"}, "")
	assert.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())

	// An agent without access to the snapshot's content cannot spawn from it.
	rec = post("/v1/snapshots/known-good/spawn", map[string]any{"group": "receiver", "keep_paths": true}, other)
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())

	rec = post("/v1/snapshots/known-good/spawn", map[string]any{"group": "receiver", "name": "copy", "count": 2, "keep_paths": true}, "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var res struct {
		Snapshot string `json:"snapshot"`
		Spawned  []struct {
			ConvID  string `json:"conv_id"`
			AgentID string `json:"agent_id"`
		} `json:"spawned"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &res))
	assert.Equal(t, "known-good", res.Snapshot)
	require.Len(t, res.Spawned, 2)
	assert.NotEqual(t, res.Spawned[0].ConvID, res.Spawned[1].ConvID)
	receiver, err := db.GetAgentGroupByName("receiver")
	require.NoError(t, err)
	for _, s := range res.Spawned {
		assert.NotEqual(t, source, s.ConvID, "each spawn is a new agent")
		member, err := db.FindMemberInGroup(receiver.ID, s.ConvID)
		require.NoError(t, err)
		assert.NotNil(t, member)
	}

	// The from-snapshot link is in the audit log.
	entries, err := db.ListAuditLog(db.AuditLogFilter{Verb: "snapshot.spawn"})
	require.NoError(t, err)
	require.Len(t, entries, 2)
	for _, e := range entries {
		assert.Equal(t, "from_snapshot known-good", e.Detail)
		assert.Equal(t, db.AuditActorHuman, e.ActorKind)
	}
}
