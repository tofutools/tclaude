package agentd_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// A long operation through the peer view runs as a job: the request answers
// its job at once, and the job answers the handler's own reply to the peer
// that started it.
func TestPeerViewLongOperationRunsAsJob(t *testing.T) {
	fh := newFedHarness(t)
	h := agentd.PeerViewHandler(fh.peer.id.ID())
	cleanup := func() *httptest.ResponseRecorder {
		return testharness.Serve(h, testharness.JSONRequest(t, "POST", "/api/worktrees/cleanup", map[string]any{"paths": []string{}}))
	}
	rec := cleanup()
	require.Equal(t, 403, rec.Code, "a restricted peer starts no job: %s", rec.Body.String())

	setFedTrustLevel(t, fh, db.FederationTrustUnrestricted)
	rec = cleanup()
	require.Equal(t, 202, rec.Code, rec.Body.String())
	var started struct {
		Job string `json:"peer_job"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &started))
	require.NotEmpty(t, started.Job)

	var state struct {
		Status string `json:"status"`
		Result *struct {
			Status      int    `json:"status"`
			ContentType string `json:"content_type"`
			Body        []byte `json:"body"`
		} `json:"result"`
	}
	require.Eventually(t, func() bool {
		rec := testharness.Serve(h, testharness.JSONRequest(t, "GET", "/api/peer-view-jobs/"+started.Job, nil))
		require.Equal(t, 200, rec.Code, rec.Body.String())
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &state))
		return state.Status == "done"
	}, 10*time.Second, 50*time.Millisecond)
	require.Equal(t, 200, state.Result.Status, string(state.Result.Body))
	require.Contains(t, string(state.Result.Body), "outcomes")

	other := agentd.PeerViewHandler("inst_someoneelse0000")
	rec = testharness.Serve(other, testharness.JSONRequest(t, "GET", "/api/peer-view-jobs/"+started.Job, nil))
	require.NotEqual(t, 200, rec.Code, "another peer never reads the job")
}

// The requesting node's proxy waits on a peer's job and answers its reply,
// which may be a stream's lines rather than one JSON value.
func TestFederationPeerViewProxyAwaitsAJob(t *testing.T) {
	fh := newFedHarness(t)
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://localhost:12345"))
	p := fh.peer
	h := agentd.BuildDashboardHandlerForTest()
	ch := make(chan *httptest.ResponseRecorder, 1)
	req := testharness.JSONRequest(t, "POST", "/api/peer/"+p.id.ID()+"/git-repositories/batch", map[string]any{"requests": []any{}})
	go func() { ch <- testharness.Serve(h, req) }()

	conn := acceptOutboundPeerView(t, p, 0)
	wire := readPeerViewWire(t, conn)
	require.Equal(t, "/api/git-repositories/batch", wire.URI)
	writePeerViewWire(t, conn, peerViewWire{Status: 202, Header: http.Header{"Content-Type": {"application/json"}}, Body: []byte(`{"peer_job":"pvj_abc"}`)})
	_ = conn.Close()

	conn = acceptOutboundPeerView(t, p, 1)
	wire = readPeerViewWire(t, conn)
	require.Equal(t, "GET", wire.Method)
	require.Equal(t, "/api/peer-view-jobs/pvj_abc", wire.URI)
	lines := "{\"path\":\"/r\",\"status\":\"updated\"}\n{\"status\":\"complete\"}\n"
	done, err := json.Marshal(map[string]any{"status": "done", "result": map[string]any{"status": 200, "content_type": "application/x-ndjson", "body": []byte(lines)}})
	require.NoError(t, err)
	writePeerViewWire(t, conn, peerViewWire{Status: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: done})
	_ = conn.Close()

	select {
	case rec := <-ch:
		require.Equal(t, 200, rec.Code, rec.Body.String())
		require.Equal(t, lines, rec.Body.String())
		require.Equal(t, "application/x-ndjson", rec.Header().Get("Content-Type"))
	case <-time.After(10 * time.Second):
		t.Fatal("proxy request did not finish")
	}
}
