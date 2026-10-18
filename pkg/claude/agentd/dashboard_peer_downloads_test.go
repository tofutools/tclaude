package agentd

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/federation/proto"
)

// A peer's download reaches this node's dashboard staged on the peer and read
// chunk by chunk over the peer view, only under unrestricted trust, and is
// served as an opaque, sandboxed attachment. The "peer" here is this same
// daemon answering through its real peer-view handler.
func TestDashboardPeerDownloadStagesAndStreamsUnderUnrestrictedTrust(t *testing.T) {
	setupTestDB(t)
	withDashboardAuth(t)
	id, err := proto.NewIdentity()
	require.NoError(t, err)
	require.NoError(t, db.TrustFederationPeer(db.FederationPeer{InstanceID: id.ID(), PubKey: id.Pub, Label: "forge", Name: "forge", TrustLevel: db.FederationTrustRestricted}))
	_, err = db.CreateGroupTemplate(&db.GroupTemplate{Name: "crew", Descr: "the build crew"})
	require.NoError(t, err)

	peerView := func(method, uri string, body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(method, uri, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		PeerViewHandler(id.ID()).ServeHTTP(rec, req)
		require.LessOrEqual(t, rec.Body.Len(), fedPeerViewResponseBodyLimit, "every reply must fit one peer-view exchange")
		return rec
	}
	previousStage, previousChunk := stagePeerDownload, fetchPeerDownloadChunk
	t.Cleanup(func() { stagePeerDownload, fetchPeerDownloadChunk = previousStage, previousChunk })
	stagePeerDownload = func(_ context.Context, peer *db.FederationPeer, target string) (peerDownloadStaged, int, error) {
		raw, _ := json.Marshal(map[string]string{"path": target})
		rec := peerView(http.MethodPost, "/api/peer-downloads", string(raw))
		status, body := rec.Code, rec.Body.Bytes()
		if status == http.StatusAccepted {
			var started struct {
				Job string `json:"peer_job"`
			}
			require.NoError(t, json.Unmarshal(body, &started))
			require.Eventually(t, func() bool {
				var state peerViewJobState
				require.NoError(t, json.Unmarshal(peerView(http.MethodGet, "/api/peer-view-jobs/"+started.Job, "").Body.Bytes(), &state))
				if state.Status == "done" {
					status, body = state.Result.Status, state.Result.Body
				}
				return state.Status == "done"
			}, 10*time.Second, 20*time.Millisecond)
		}
		if status != http.StatusOK {
			return peerDownloadStaged{}, status, &peerDownloadRefused{body: body}
		}
		var staged peerDownloadStaged
		require.NoError(t, json.Unmarshal(body, &staged))
		return staged, http.StatusOK, nil
	}
	chunks := 0
	fetchPeerDownloadChunk = func(_ context.Context, _ *db.FederationPeer, staged string, offset int64) (humanAttachmentChunk, int, error) {
		chunks++
		rec := peerView(http.MethodGet, "/api/peer-downloads/"+staged+"?offset="+strconv.FormatInt(offset, 10), "")
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var c humanAttachmentChunk
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &c))
		return c, http.StatusOK, nil
	}
	mux := http.NewServeMux()
	registerDashboardPeerDownloadRoutes(mux)
	download := func(target string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, dashboardRequest(http.MethodGet, "/api/peer-download/"+id.ID()+"?path="+target, ""))
		return rec
	}

	// A restricted peer stages nothing.
	rec := download("/api/templates/crew/export")
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())

	require.NoError(t, db.TrustFederationPeer(db.FederationPeer{InstanceID: id.ID(), PubKey: id.Pub, Label: "forge", Name: "forge", TrustLevel: db.FederationTrustUnrestricted}))
	rec = download("/api/templates/crew/export")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), "the build crew", "the peer's own export arrives intact")
	require.Equal(t, "application/octet-stream", rec.Header().Get("Content-Type"), "a peer's bytes never run on this origin")
	require.Contains(t, rec.Header().Get("Content-Disposition"), "attachment")
	require.Equal(t, "nosniff", rec.Header().Get("X-Content-Type-Options"))
	require.Contains(t, rec.Header().Get("Content-Security-Policy"), "sandbox")
	require.Equal(t, 1, chunks)

	// Only download routes stage, and the route's own refusal comes back.
	rec = download("/api/snapshots")
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	rec = download("/api/templates/nobody/export")
	require.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), "no such template", "as the route answers it locally")

	// An unknown peer is refused before anything is asked of it.
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, dashboardRequest(http.MethodGet, "/api/peer-download/inst_nobody123?path=/api/templates/crew/export", ""))
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
}

// Every stageable download is a route the peer view already serves only
// under unrestricted trust, never one that stays with its own node.
func TestPeerDownloadRoutesAreUnrestrictedLocalRoutes(t *testing.T) {
	rules := peerViewRules()
	for pattern := range peerDownloadRoutes {
		require.Equal(t, "local_dashboard", rules[pattern].feature, pattern)
		require.True(t, rules[pattern].unrestrictedOnly, pattern)
	}
}
