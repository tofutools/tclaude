package agentd_test

import (
	"encoding/base64"
	"encoding/json"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/testutil"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/federation/bundletransfer"
	"github.com/tofutools/tclaude/pkg/federation/proto"
	"github.com/tofutools/tclaude/pkg/federation/stream"
	"github.com/tofutools/tclaude/pkg/testharness"
)

func TestDashboardFederationIncrementalJobOutput(t *testing.T) {
	fh := newFedHarness(t)
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://localhost:12345"))
	fh.peer.send(fh.peer.envelope(proto.KindCatalog, proto.Endpoint{}, proto.CatalogPayload{JobOutput: true, Groups: []proto.CatalogGroup{{Name: "team", Caps: []string{proto.CapJobs}}}}))
	fedEventually(t, "incremental output catalog", func() bool {
		raw, _, _ := db.GetFederationCatalog(fh.peer.id.ID())
		return strings.Contains(raw, `"job_output":true`)
	})
	h := agentd.BuildDashboardHandlerForTest()
	rec := testharness.Serve(h, testharness.JSONRequest(t, "POST", "/api/federation/jobs", map[string]any{"node": "bob", "repo": "project", "ref": "main", "group": "team", "command": "make test", "timeout_seconds": 10}))
	require.Equal(t, 200, rec.Code, rec.Body.String())
	var sent struct {
		Job db.FederationJob `json:"job"`
	}
	testharness.DecodeJSON(t, rec, &sent)
	path := "/api/federation/jobs/" + sent.Job.ID + "/output"
	for _, query := range []string{"?cursor=invalid", "?max_bytes=1", "?max_bytes=262145"} {
		rec = testharness.Serve(h, testharness.JSONRequest(t, "GET", path+query, nil))
		require.Equal(t, 400, rec.Code, rec.Body.String())
	}
	result := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		result <- testharness.Serve(h, testharness.JSONRequest(t, "GET", path+"?max_bytes=65536", nil))
	}()
	fedEventually(t, "bounded output request", func() bool { return len(fh.peer.envelopes(proto.KindJobOutput)) > 0 })
	var req struct {
		bundletransfer.Request
		Cursor   map[string]any `json:"cursor"`
		MaxBytes int            `json:"max_bytes"`
	}
	require.NoError(t, fh.peer.envelopes(proto.KindJobOutput)[0].DecodePayload(&req))
	require.Equal(t, 65536, req.MaxBytes)
	require.Equal(t, sent.Job.ID, req.Cursor["job"])
	require.Equal(t, float64(0), req.Cursor["offset"])
	kp, err := stream.NewKeyPair()
	require.NoError(t, err)
	a := fh.peer.envelope(proto.KindJobFollowAnswer, proto.Endpoint{}, bundletransfer.Answer{Request: bundletransfer.Request{Offer: req.Offer, Stream: req.Stream, SHA256: req.SHA256, Key: kp.Pub}, OK: true})
	a.From.Agent = ""
	fh.peer.send(a)
	conn := fedPeerStream(t, fh.peer, req.Stream, kp, req.Key, false)
	req.Cursor["offset"] = 17
	req.Cursor["stdout"] = 4
	raw, err := json.Marshal(req.Cursor)
	require.NoError(t, err)
	cursor := base64.RawURLEncoding.EncodeToString(raw)
	require.NoError(t, json.NewEncoder(conn).Encode(map[string]any{"chunks": []map[string]string{{"stream": "stdout", "data": base64.StdEncoding.EncodeToString([]byte("live")), "encoding": "base64"}}, "cursor": cursor, "done": true, "state": "completed"}))
	require.NoError(t, conn.CloseWrite())
	defer conn.Close()
	select {
	case rec = <-result:
	case <-time.After(5 * time.Second):
		t.Fatal("bounded output did not close on authenticated FIN")
	}
	require.Equal(t, 200, rec.Code, rec.Body.String())
	require.Contains(t, rec.Header().Get("Cache-Control"), "no-store")
	var got struct {
		Chunks        []struct{ Stream, Data string }
		Cursor, State string
		Done          bool
	}
	testharness.DecodeJSON(t, rec, &got)
	require.Len(t, got.Chunks, 1)
	require.Equal(t, base64.StdEncoding.EncodeToString([]byte("live")), got.Chunks[0].Data)
	require.Equal(t, cursor, got.Cursor)
	require.False(t, got.Done, "remote output cannot authenticate terminal job receipt")
	require.Equal(t, "submitted", got.State)
	// The next cursor is passed back unchanged; reject one bound to another job.
	req.Cursor["job"] = "other"
	raw, err = json.Marshal(req.Cursor)
	require.NoError(t, err)
	rec = testharness.Serve(h, testharness.JSONRequest(t, "GET", path+"?cursor="+url.QueryEscape(base64.RawURLEncoding.EncodeToString(raw)), nil))
	require.Equal(t, 400, rec.Code)
	peer, err := db.GetFederationPeer(fh.peer.id.ID())
	require.NoError(t, err)
	peer.TrustLevel = db.FederationTrustUnrestricted
	require.NoError(t, db.TrustFederationPeer(*peer))
	rec = testharness.Serve(agentd.PeerViewHandler(peer.InstanceID), testharness.JSONRequest(t, "GET", path, nil))
	require.Equal(t, 403, rec.Code)
	// CLI agents cannot observe another operator's outbound job.
	fh.f.HaveConvWithTitle("other-job-reader", "reader")
	rec = testharness.Serve(fh.f.Mux, agentd.AsAgentPeer(testharness.JSONRequest(t, "GET", "/v1/federation/jobs/"+sent.Job.ID+"/output", nil), "other-job-reader"))
	require.Equal(t, 403, rec.Code, rec.Body.String())
	// Old peers fail immediately rather than timing out on a new protocol kind.
	fh.peer.send(fh.peer.envelope(proto.KindCatalog, proto.Endpoint{}, proto.CatalogPayload{Groups: []proto.CatalogGroup{{Name: "team", Caps: []string{proto.CapJobs}}}}))
	fedEventually(t, "legacy catalog", func() bool {
		raw, _, _ := db.GetFederationCatalog(fh.peer.id.ID())
		return !strings.Contains(raw, `"job_output"`)
	})
	rec = testharness.Serve(h, testharness.JSONRequest(t, "GET", path, nil))
	require.Equal(t, 409, rec.Code)
	require.Contains(t, rec.Body.String(), "unsupported_peer")
	require.NoError(t, db.TransitionFederationJob(sent.Job.ID, "submitted", "refused", nil))
	rec = testharness.Serve(h, testharness.JSONRequest(t, "GET", path, nil))
	require.Equal(t, 200, rec.Code)
	require.Contains(t, rec.Body.String(), `"done":true`)
	rawMux := http.NewServeMux()
	agentd.RegisterDashboardRoutesForTest(rawMux)
	rec = testharness.Serve(rawMux, testharness.JSONRequest(t, "GET", path, nil))
	require.Equal(t, 403, rec.Code)
}

func TestDashboardFederationIdentityTransitionReadOnly(t *testing.T) {
	fh := newFedHarness(t)
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://localhost:12345"))
	next, err := proto.NewIdentity()
	require.NoError(t, err)
	now := time.Now()
	rotation, err := proto.NewRotation(fh.peer.id, next, "", 1, now, time.Hour)
	require.NoError(t, err)
	_, err = db.ObserveFederationRotation(rotation, now, time.Hour)
	require.NoError(t, err)
	h := agentd.BuildDashboardHandlerForTest()
	read := func(path string) []byte {
		t.Helper()
		rec := testharness.Serve(h, testharness.JSONRequest(t, "GET", path, nil))
		require.Equal(t, 200, rec.Code, rec.Body.String())
		return rec.Body.Bytes()
	}
	for _, query := range []string{"", "?summary=1"} {
		// Inspect the wire names directly, including the summary shape.
		raw := read("/api/federation/status" + query)
		require.Contains(t, string(raw), `"identity_transition":{"state":"pending"`)
		require.Contains(t, string(raw), `"new_id":"`+next.ID()+`"`)
		require.Contains(t, string(raw), proto.Fingerprint(next.Pub))
	}
	require.Contains(t, string(read("/api/federation/identity/rotations")), "pending")
	conflicting, err := proto.NewIdentity()
	require.NoError(t, err)
	other, err := proto.NewRotation(fh.peer.id, conflicting, "", 1, now, time.Hour)
	require.NoError(t, err)
	_, err = db.ObserveFederationRotation(other, now, time.Hour)
	require.NoError(t, err)
	require.Contains(t, string(read("/api/federation/status?summary=1")), `"state":"conflict"`)
	for _, path := range []string{"/api/federation/status", "/api/federation/identity/rotations"} {
		rec := testharness.Serve(agentd.PeerViewHandler(fh.peer.id.ID()), testharness.JSONRequest(t, "GET", path, nil))
		require.Equal(t, 403, rec.Code)
	}
	fh.f.HaveConvWithTitle("identity-observer", "observer")
	rec := testharness.Serve(fh.f.Mux, agentd.AsAgentPeer(testharness.JSONRequest(t, "GET", "/v1/federation/identity/rotations", nil), "identity-observer"))
	require.Equal(t, 403, rec.Code)
	rec = testharness.Serve(h, testharness.JSONRequest(t, "POST", "/api/federation/identity/rotations", nil))
	require.Equal(t, 404, rec.Code)
}

// The other half of the route uses the real worker spool and encrypted hub
// stream. A read never reruns the job or waits for future worker output.
func TestFederationIncrementalJobOutputProducer(t *testing.T) {
	fh := newFedHarness(t)
	fh.f.HaveGroup("team")
	fedJobRepo(t, fh)
	fedAllowJobs(t, fh)
	t.Cleanup(agentd.SetRemoteJobDirectRunnerForTest())
	fifo := filepath.Join(testutil.CanonicalTempDir(t), "finish")
	require.NoError(t, syscall.Mkfifo(fifo, 0600))
	t.Cleanup(func() {
		fd, err := syscall.Open(fifo, syscall.O_WRONLY|syscall.O_NONBLOCK, 0)
		if err == nil {
			_, _ = syscall.Write(fd, []byte("finish\n"))
			_ = syscall.Close(fd)
		}
	})
	q := proto.JobRequest{ID: proto.NewEnvelopeID(), Repo: "project", Ref: "main", Group: "team", Harness: "shell", Command: "printf hello; printf warning >&2; read ignored < '" + fifo + "'", Timeout: 10}
	env := fh.peer.envelope(proto.KindJobRequest, proto.Endpoint{}, q)
	env.From.Agent = ""
	fh.peer.send(env)
	var j *db.FederationJob
	fedEventually(t, "worker output ready", func() bool {
		var err error
		j, err = db.GetFederationJob(q.ID)
		if err != nil || j.State != "running" {
			return false
		}
		info, err := os.Stat(filepath.Join(config.DataDir(), "federation", "job-live", q.ID+".frames"))
		return err == nil && info.Size() >= 38
	})
	cursor := map[string]any{"job": j.ID, "fingerprint": j.Fingerprint, "offset": 0, "stdout": 0, "stderr": 0}
	poll := func() struct {
		Chunks []struct{ Stream, Data, Encoding string }
		Cursor string
	} {
		t.Helper()
		kp, err := stream.NewKeyPair()
		require.NoError(t, err)
		req := bundletransfer.Request{Offer: j.ID, Stream: proto.NewEnvelopeID(), SHA256: j.Fingerprint, Key: kp.Pub}
		env := fh.peer.envelope(proto.KindJobOutput, proto.Endpoint{}, map[string]any{"offer": req.Offer, "stream": req.Stream, "sha256": req.SHA256, "key": req.Key, "cursor": cursor, "max_bytes": 65536})
		env.From.Agent = ""
		fh.peer.send(env)
		var answer bundletransfer.Answer
		fedEventually(t, "incremental producer answer", func() bool {
			for _, e := range fh.peer.envelopes(proto.KindJobFollowAnswer) {
				var a bundletransfer.Answer
				if e.DecodePayload(&a) == nil && a.Stream == req.Stream {
					answer = a
					return true
				}
			}
			return false
		})
		require.True(t, answer.OK, answer.Reason)
		conn := fedPeerStream(t, fh.peer, req.Stream, kp, answer.Key, true)
		defer conn.Close()
		require.NoError(t, conn.SetDeadline(time.Now().Add(5*time.Second)))
		raw, err := io.ReadAll(conn)
		require.NoError(t, err, "bounded reply must have authenticated FIN")
		var out struct {
			Chunks []struct{ Stream, Data, Encoding string }
			Cursor string
		}
		require.NoError(t, json.Unmarshal(raw, &out))
		raw, err = base64.RawURLEncoding.DecodeString(out.Cursor)
		require.NoError(t, err)
		require.NoError(t, json.Unmarshal(raw, &cursor))
		return out
	}
	first := poll()
	var stdout, stderr strings.Builder
	for _, chunk := range first.Chunks {
		require.Equal(t, "base64", chunk.Encoding)
		b, err := base64.StdEncoding.DecodeString(chunk.Data)
		require.NoError(t, err)
		if chunk.Stream == "stdout" {
			stdout.Write(b)
		} else {
			stderr.Write(b)
		}
	}
	require.Equal(t, "hello", stdout.String())
	require.Equal(t, "warning", stderr.String())
	require.Empty(t, poll().Chunks, "cursor reads must not replay retained output")
	current, err := db.GetFederationJob(j.ID)
	require.NoError(t, err)
	require.Equal(t, j.WorkerID, current.WorkerID)
}
