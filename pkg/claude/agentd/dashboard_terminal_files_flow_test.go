package agentd_test

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/federation/bundletransfer"
	"github.com/tofutools/tclaude/pkg/federation/proto"
	"github.com/tofutools/tclaude/pkg/federation/stream"
	"github.com/tofutools/tclaude/pkg/testharness"
)

func TestDashboardTerminalFileVerifiedDownloadAndHead(t *testing.T) {
	fh := newFedHarness(t)
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://localhost:12345"))
	ws, _, open := dashboardTerminalOpen(t, fh, "watch")
	defer ws.Close()
	path := "/api/federation/terminal?" + url.Values{"peer": {fh.peer.id.ID()}, "agent": {open.Agent}, "mode": {"watch"}}.Encode()
	endpoint := "/api/federation/terminal-file?" + url.Values{"terminal": {path}, "viewer": {open.Stream}, "path": {"report.txt"}}.Encode()
	for _, test := range []struct {
		method  string
		corrupt bool
		status  int
		list    bool
	}{{"GET", false, 200, false}, {"HEAD", false, 200, false}, {"GET", true, 403, false}, {"GET", false, 200, true}} {
		before := len(fh.peer.envelopes(proto.KindTerminalFile))
		done := make(chan *httptest.ResponseRecorder, 1)
		go func() {
			done <- testharness.Serve(agentd.BuildDashboardHandlerForTest(), httptest.NewRequest(test.method, endpoint+"&list="+fmt.Sprint(test.list), nil))
		}()
		var env *proto.Envelope
		fedEventually(t, "browser file request", func() bool {
			es := fh.peer.envelopes(proto.KindTerminalFile)
			if len(es) > before {
				env = es[len(es)-1]
				return true
			}
			return false
		})
		var req struct {
			bundletransfer.Request
			Viewer, Path string
			Head         bool
			List         bool
		}
		require.NoError(t, env.DecodePayload(&req))
		require.Equal(t, open.Stream, req.Viewer)
		require.Equal(t, "report.txt", req.Path)
		require.Equal(t, test.method == "HEAD", req.Head)
		require.Equal(t, test.list, req.List)
		kp, err := stream.NewKeyPair()
		require.NoError(t, err)
		answer := bundletransfer.Answer{Request: req.Request, OK: true}
		answer.Key = kp.Pub
		reply := fh.peer.envelope(proto.KindBundleAnswer, proto.Endpoint{}, answer)
		reply.From.Agent = ""
		reply.InReplyTo = env.ID
		fh.peer.send(reply)
		conn := fedPeerStream(t, fh.peer, req.Stream, kp, req.Key, false)
		body := []byte("binary report\x00\xff")
		if test.list {
			body = []byte(`{"entries":[{"path":"report.txt","kind":"file","size":9}],"truncated":false}`)
		}
		hash := sha256.Sum256(body)
		digest := hex.EncodeToString(hash[:])
		if test.corrupt {
			digest = "bad"
		}
		raw, err := json.Marshal(map[string]any{"status": 200, "bytes": len(body), "sha256": digest})
		require.NoError(t, err)
		var prefix [4]byte
		binary.BigEndian.PutUint32(prefix[:], uint32(len(raw)))
		_, err = conn.Write(append(prefix[:], raw...))
		require.NoError(t, err)
		if !req.Head {
			_, err = conn.Write(body)
			require.NoError(t, err)
		}
		require.NoError(t, conn.CloseWrite())
		rec := <-done
		conn.Close()
		require.Equal(t, test.status, rec.Code, rec.Body.String())
		if test.status == 200 {
			require.Equal(t, "nosniff", rec.Header().Get("X-Content-Type-Options"))
			require.Equal(t, "sandbox", rec.Header().Get("Content-Security-Policy"))
			if test.list {
				require.Equal(t, "application/json", rec.Header().Get("Content-Type"))
				require.Empty(t, rec.Header().Get("Content-Disposition"))
			} else {
				require.Contains(t, rec.Header().Get("Content-Disposition"), "attachment")
			}
			if req.Head {
				require.Empty(t, rec.Body.Bytes())
			} else {
				require.Equal(t, body, rec.Body.Bytes())
			}
		} else {
			require.NotContains(t, rec.Body.String(), string(body))
		}
	}
	bad := testharness.Serve(agentd.BuildDashboardHandlerForTest(), httptest.NewRequest("GET", strings.Replace(endpoint, "viewer="+open.Stream, "viewer=invalid", 1), nil))
	require.Equal(t, 403, bad.Code)
	rawMux := http.NewServeMux()
	agentd.RegisterDashboardRoutesForTest(rawMux)
	refused := testharness.Serve(rawMux, httptest.NewRequest("GET", endpoint, nil))
	require.Equal(t, 403, refused.Code)
	peer, err := db.GetFederationPeer(fh.peer.id.ID())
	require.NoError(t, err)
	peer.TrustLevel = db.FederationTrustUnrestricted
	require.NoError(t, db.TrustFederationPeer(*peer))
	refused = testharness.Serve(agentd.PeerViewHandler(peer.InstanceID), httptest.NewRequest("GET", endpoint, nil))
	require.Equal(t, 403, refused.Code)
	cli := agentd.AsAgentPeer(httptest.NewRequest("GET", "/v1/federation/file?target=unknown&path=report.txt", nil), "test-agent")
	refused = testharness.Serve(agentd.BuildHandlerForTest(), cli)
	require.Equal(t, 403, refused.Code)
}

func TestFederationFileCLIUsesTemporaryPinnedViewer(t *testing.T) {
	cliFileTransferFlow(t, false, false)
}
func TestFederationFileCLIStopsOnViewerReset(t *testing.T) { cliFileTransferFlow(t, true, false) }
func TestFederationFileCLIListsTemporaryPinnedViewer(t *testing.T) {
	cliFileTransferFlow(t, false, true)
}
func cliFileTransferFlow(t *testing.T, reset, list bool) {
	fh := newFedHarness(t)
	row := dashboardRemoteTerminalCatalog(t, fh, proto.CapSessionsWatch, proto.CapSessionsFilesRead)
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		done <- fedHuman(t, fh.f, "GET", "/v1/federation/file?"+url.Values{"target": {row.Agent + "@bob"}, "path": {"empty.txt"}, "list": {fmt.Sprint(list)}}.Encode(), nil)
	}()
	var env *proto.Envelope
	fedEventually(t, "CLI viewer open", func() bool {
		es := fh.peer.envelopes(proto.KindSessionOpen)
		if len(es) > 0 {
			env = es[len(es)-1]
			return true
		}
		return false
	})
	var open proto.SessionOpenPayload
	require.NoError(t, env.DecodePayload(&open))
	require.True(t, open.FixedSize)
	require.True(t, open.ReadOnly)
	kp, err := stream.NewKeyPair()
	require.NoError(t, err)
	answerEnv := fh.peer.envelope(proto.KindSessionAnswer, proto.Endpoint{}, proto.SessionAnswerPayload{Stream: open.Stream, OK: true, Key: kp.Pub, Cols: 80, Rows: 24, Files: true})
	answerEnv.InReplyTo = env.ID
	fh.peer.send(answerEnv)
	viewer := fedPeerStream(t, fh.peer, open.Stream, kp, open.Key, false)
	defer viewer.Close()
	fedEventually(t, "CLI file request", func() bool {
		es := fh.peer.envelopes(proto.KindTerminalFile)
		if len(es) > 0 {
			env = es[len(es)-1]
			return true
		}
		return false
	})
	var req struct {
		bundletransfer.Request
		Viewer, Path string
		List         bool
	}
	require.NoError(t, env.DecodePayload(&req))
	require.Equal(t, open.Stream, req.Viewer)
	require.Equal(t, "empty.txt", req.Path)
	require.Equal(t, list, req.List)
	fileKey, err := stream.NewKeyPair()
	require.NoError(t, err)
	answer := bundletransfer.Answer{Request: req.Request, OK: true}
	answer.Key = fileKey.Pub
	reply := fh.peer.envelope(proto.KindBundleAnswer, proto.Endpoint{}, answer)
	reply.From.Agent = ""
	reply.InReplyTo = env.ID
	fh.peer.send(reply)
	file := fedPeerStream(t, fh.peer, req.Stream, fileKey, req.Key, false)
	var body []byte
	if list {
		body = []byte(`{"entries":[],"truncated":false}`)
	}
	hash := sha256.Sum256(body)
	size := len(body)
	if reset {
		size = 1
	}
	raw, err := json.Marshal(map[string]any{"status": 200, "bytes": size, "sha256": hex.EncodeToString(hash[:])})
	require.NoError(t, err)
	var prefix [4]byte
	binary.BigEndian.PutUint32(prefix[:], uint32(len(raw)))
	_, err = file.Write(append(prefix[:], raw...))
	require.NoError(t, err)
	if reset {
		require.NoError(t, viewer.Close())
		select {
		case rec := <-done:
			require.NotEqual(t, 200, rec.Code)
			require.NotContains(t, rec.Body.String(), "binary report")
		case <-time.After(5 * time.Second):
			t.Fatal("download survived closed temporary viewer")
		}
		file.Close()
		return
	}
	if len(body) > 0 {
		_, err = file.Write(body)
		require.NoError(t, err)
	}
	require.NoError(t, file.CloseWrite())
	rec := <-done
	file.Close()
	require.Equal(t, 200, rec.Code, rec.Body.String())
	require.Equal(t, string(body), rec.Body.String())
	if list {
		require.Equal(t, "application/json", rec.Header().Get("Content-Type"))
	} else {
		require.Contains(t, rec.Header().Get("Content-Disposition"), "empty.txt")
	}
	viewers := fedHuman(t, fh.f, "GET", "/v1/federation/viewers", nil)
	require.NotContains(t, viewers.Body.String(), open.Stream)
}

func TestFederationFileCLINameAddressGuidance(t *testing.T) {
	fh := newFedHarness(t)
	dashboardRemoteTerminalCatalog(t, fh, proto.CapSessionsWatch, proto.CapSessionsFilesRead)
	rec := fedHuman(t, fh.f, "GET", "/v1/federation/file?target=reviewer-1@bob&path=notes.txt", nil)
	require.Equal(t, 403, rec.Code)
	require.Contains(t, rec.Body.String(), "unknown session")
	require.Contains(t, rec.Body.String(), "federation sessions")
	require.NotContains(t, rec.Body.String(), "watch/attach capability")
}
