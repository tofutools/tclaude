package agentd

import (
	"bytes"
	"context"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/federation/proto"
	"github.com/tofutools/tclaude/pkg/testutil"
)

// A spawn's attachment larger than one peer view request reaches the peer in
// chunks, staged there and handed to its upload route whole, only under
// unrestricted trust. The "peer" here is this same daemon answering through
// its real peer-view handler.
func TestDashboardPeerUploadSendsChunksUnderUnrestrictedTrust(t *testing.T) {
	setupTestDB(t)
	withDashboardAuth(t)
	previousBase := spawnAttachmentsBase
	spawnAttachmentsBase = testutil.CanonicalTempDir(t)
	t.Cleanup(func() { spawnAttachmentsBase = previousBase })
	id, err := proto.NewIdentity()
	require.NoError(t, err)
	trust := func(level string) {
		require.NoError(t, db.TrustFederationPeer(db.FederationPeer{InstanceID: id.ID(), PubKey: id.Pub, Label: "forge", Name: "forge", TrustLevel: level}))
	}
	trust(db.FederationTrustRestricted)

	chunks := 0
	previous := peerUploadExchange
	t.Cleanup(func() { peerUploadExchange = previous })
	peerUploadExchange = func(_ context.Context, _ *db.FederationPeer, req fedPeerViewRequest, out any) (int, error) {
		require.LessOrEqual(t, len(req.Body), peerViewBodyLimit(pathOf(req.URI)), "every request fits its peer view limit")
		if pathOf(req.URI) == peerUploadChunkPath {
			chunks++
		}
		serve := func(method, uri string, body []byte) *httptest.ResponseRecorder {
			rec := httptest.NewRecorder()
			r := httptest.NewRequest(method, uri, bytes.NewReader(body))
			for k, v := range req.Header {
				r.Header[k] = v
			}
			PeerViewHandler(id.ID()).ServeHTTP(rec, r)
			return rec
		}
		rec := serve(req.Method, req.URI, req.Body)
		status, body := rec.Code, rec.Body.Bytes()
		if status == http.StatusAccepted && req.URI == peerUploadFinishPath {
			var started struct {
				Job string `json:"peer_job"`
			}
			require.NoError(t, json.Unmarshal(body, &started))
			require.Eventually(t, func() bool {
				var state peerViewJobState
				require.NoError(t, json.Unmarshal(serve(http.MethodGet, "/api/peer-view-jobs/"+started.Job, nil).Body.Bytes(), &state))
				if state.Status == "done" {
					status, body = state.Result.Status, state.Result.Body
				}
				return state.Status == "done"
			}, 10*time.Second, 20*time.Millisecond)
		}
		if status != http.StatusOK {
			return status, &peerDownloadRefused{body: body}
		}
		if out != nil {
			require.NoError(t, json.Unmarshal(body, out))
		}
		return http.StatusOK, nil
	}
	mux := http.NewServeMux()
	registerDashboardPeerUploadRoutes(mux)
	content := make([]byte, 2*peerUploadChunk+12345)
	for i := range content {
		content[i] = byte(i % 251)
	}
	upload := func(path string) *httptest.ResponseRecorder {
		var form bytes.Buffer
		mw := multipart.NewWriter(&form)
		part, err := mw.CreateFormFile("file", "trace.bin")
		require.NoError(t, err)
		_, _ = part.Write(content)
		require.NoError(t, mw.Close())
		r := dashboardRequest(http.MethodPost, "/api/peer-upload/"+id.ID()+"?path="+path, form.String())
		r.Header.Set("Content-Type", mw.FormDataContentType())
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, r)
		return rec
	}

	rec := upload("/api/spawn-attachments")
	require.Equal(t, http.StatusForbidden, rec.Code, "a restricted peer takes no upload: %s", rec.Body.String())

	trust(db.FederationTrustUnrestricted)
	rec = upload("/api/spawn-attachments")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, 3, chunks, "the file crossed in 512 KiB chunks")
	var stored spawnAttachmentsResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &stored))
	require.Len(t, stored.Files, 1)
	got, err := os.ReadFile(stored.Files[0].Path)
	require.NoError(t, err)
	require.True(t, bytes.Equal(content, got), "the peer's route got the whole file")

	// Only upload routes take a staged upload.
	rec = upload("/api/groups")
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), "not an upload")
}

func pathOf(uri string) string {
	if i := bytes.IndexByte([]byte(uri), '?'); i >= 0 {
		return uri[:i]
	}
	return uri
}

// Only the chunk route's request may carry more than a control request.
func TestPeerViewBodyLimitOpensOnlyForUploadChunks(t *testing.T) {
	require.Equal(t, peerUploadChunk, peerViewBodyLimit(peerUploadChunkPath))
	require.Equal(t, fedPeerViewBodyLimit, peerViewBodyLimit("/api/spawn-attachments"))
	require.Equal(t, fedPeerViewBodyLimit, peerViewBodyLimit(peerUploadFinishPath))
	// A chunk, base64-encoded in its JSON frame, fits the frame limit.
	require.Less(t, (peerUploadChunk+2)/3*4+8192, fedPeerViewRequestLimit)
}
