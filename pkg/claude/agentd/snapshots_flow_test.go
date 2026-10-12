package agentd_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/snapshotlib"
	"github.com/tofutools/tclaude/pkg/paf"
)

func TestSnapshotLibraryFlow(t *testing.T) {
	f := newFlow(t)
	const source = "019fe740-43a4-7023-b8ae-1ee64459f2c1"
	const other = "019fe740-43a4-7023-b8ae-1ee64459f2c2"
	f.HaveAliveSession(source, "original", "original-pane", t.TempDir())
	f.HaveAliveSession(other, "bystander", "bystander-pane", t.TempDir())

	serve := func(r *http.Request) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		f.Mux.ServeHTTP(rec, r)
		return rec
	}
	human := func(method, path string, body any) *httptest.ResponseRecorder {
		var buf bytes.Buffer
		if body != nil {
			require.NoError(t, json.NewEncoder(&buf).Encode(body))
		}
		return serve(agentd.AsHumanPeer(httptest.NewRequest(method, path, &buf)))
	}
	asAgent := func(conv, method, path string) *httptest.ResponseRecorder {
		return serve(agentd.AsAgentPeer(httptest.NewRequest(method, path, nil), conv))
	}

	rec := human(http.MethodPost, "/v1/snapshots", map[string]any{"agent": source, "name": "../escape"})
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())

	rec = human(http.MethodPost, "/v1/snapshots", map[string]any{"agent": source, "name": "before-refactor", "note": "known good", "expires": "30d"})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var meta snapshotlib.Meta
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &meta))
	assert.Equal(t, "before-refactor", meta.Name)
	assert.Equal(t, "claude", meta.Harness)
	assert.True(t, meta.History)
	assert.Positive(t, meta.Bytes)
	require.NotNil(t, meta.ExpiresAt)
	assert.WithinDuration(t, time.Now().Add(30*24*time.Hour), *meta.ExpiresAt, time.Minute)

	rec = human(http.MethodPost, "/v1/snapshots", map[string]any{"agent": source, "name": "before-refactor"})
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())

	rec = human(http.MethodGet, "/v1/snapshots", nil)
	require.Equal(t, http.StatusOK, rec.Code)
	var list struct {
		Snapshots []snapshotlib.Meta `json:"snapshots"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &list))
	require.Len(t, list.Snapshots, 1)

	// The stored PAF records the snapshot and its lineage.
	rec = human(http.MethodGet, "/v1/snapshots/before-refactor/export", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	raw := rec.Body.Bytes()
	a, err := paf.Open(bytes.NewReader(raw), int64(len(raw)), paf.Options{})
	require.NoError(t, err)
	ag, ok := a.Agent(a.Manifest.Agents[0])
	require.True(t, ok)
	require.NotNil(t, ag.Snapshot)
	assert.Equal(t, "before-refactor", ag.Snapshot.Name)
	assert.Equal(t, "known good", ag.Snapshot.Note)
	assert.Contains(t, []string{"idle", "last_complete_turn"}, ag.Snapshot.State)
	require.NotEmpty(t, ag.Lineage)
	last := ag.Lineage[len(ag.Lineage)-1]
	assert.Equal(t, paf.OpSnapshot, last.Op)
	require.NotNil(t, last.From)
	assert.Equal(t, source, last.From.Conversation)
	assert.NotNil(t, ag.Native, "the conversation is captured")

	// An agent without agent.bundle.export on the source can neither read
	// the snapshot's content nor remove it.
	rec = asAgent(other, http.MethodGet, "/v1/snapshots/before-refactor/export")
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	rec = asAgent(other, http.MethodDelete, "/v1/snapshots/before-refactor")
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	rec = asAgent(other, http.MethodPost, "/v1/snapshots/prune")
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())

	// An expired snapshot disappears from the library.
	rec = human(http.MethodPost, "/v1/snapshots", map[string]any{"agent": source, "name": "brief", "expires": "1ms", "no_history": true})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	time.Sleep(5 * time.Millisecond)
	rec = human(http.MethodGet, "/v1/snapshots/brief", nil)
	assert.Equal(t, http.StatusNotFound, rec.Code)
	rec = human(http.MethodGet, "/v1/snapshots", nil)
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &list))
	assert.Len(t, list.Snapshots, 1)

	rec = human(http.MethodDelete, "/v1/snapshots/before-refactor", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	rec = human(http.MethodGet, "/v1/snapshots/before-refactor", nil)
	assert.Equal(t, http.StatusNotFound, rec.Code)

	// A default name is derived from the agent.
	rec = human(http.MethodPost, "/v1/snapshots", map[string]any{"agent": source, "no_history": true})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &meta))
	assert.True(t, snapshotlib.ValidName(meta.Name), meta.Name)

	// An agent's snapshots are bounded and it sees less of them.
	require.NoError(t, db.GrantAgentPermission(other, agentd.PermAgentBundleExport, "test"))
	var body bytes.Buffer
	require.NoError(t, json.NewEncoder(&body).Encode(map[string]any{"agent": source, "name": "by-agent", "note": "private note", "expires": "never", "no_history": true}))
	rec = serve(agentd.AsAgentPeer(httptest.NewRequest(http.MethodPost, "/v1/snapshots", &body), other))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &meta))
	require.NotNil(t, meta.ExpiresAt, "an agent's snapshot always expires")
	assert.WithinDuration(t, time.Now().Add(30*24*time.Hour), *meta.ExpiresAt, time.Minute)
	rec = asAgent(other, http.MethodGet, "/v1/snapshots/by-agent")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var seen snapshotlib.Meta
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &seen))
	assert.Empty(t, seen.Note)
	rec = human(http.MethodGet, "/v1/snapshots/by-agent", nil)
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &meta))
	assert.Equal(t, "private note", meta.Note)
}
