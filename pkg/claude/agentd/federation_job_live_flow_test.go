package agentd_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/federation/bundletransfer"
	"github.com/tofutools/tclaude/pkg/federation/jobstream"
	"github.com/tofutools/tclaude/pkg/federation/proto"
	"github.com/tofutools/tclaude/pkg/federation/stream"
	"github.com/tofutools/tclaude/pkg/testharness"
)

func publishJobNode(t *testing.T, p *fedPeer, load float64) {
	t.Helper()
	at := time.Now().UTC()
	p.send(p.envelope(proto.KindCatalog, proto.Endpoint{}, proto.CatalogPayload{Node: placementNode(load), NodeAt: at, Groups: []proto.CatalogGroup{{Name: "team", Caps: []string{proto.CapJobs}}}}))
	fedEventually(t, "job node published", func() bool {
		raw, _, e := db.GetFederationCatalog(p.id.ID())
		return e == nil && strings.Contains(raw, `"jobs"`)
	})
}
func TestFederation_JobsFanoutPinsBeforeAnySubmission(t *testing.T) {
	fh := newFedHarness(t)
	other := addPlacementPeer(t, fh, "charlie")
	publishJobNode(t, fh.peer, 2)
	publishJobNode(t, other, 1)
	body := map[string]any{"nodes": []string{"bob", "charlie"}, "repo": "project", "ref": "main", "group": "team", "command": "make test", "timeout_seconds": 10}
	rec := fedHuman(t, fh.f, http.MethodPost, "/v1/federation/jobs", body)
	require.Equal(t, 400, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), "--ref")
	var refusal struct {
		Error string `json:"error"`
	}
	testharness.DecodeJSON(t, rec, &refusal)
	require.Contains(t, refusal.Error, "git rev-parse origin/<branch>")
	jobs, e := db.ListFederationJobs(false)
	require.NoError(t, e)
	require.Empty(t, jobs)
	body["ref"] = strings.Repeat("a", 40)
	rec = fedHuman(t, fh.f, http.MethodPost, "/v1/federation/jobs", body)
	require.Equal(t, 200, rec.Code, rec.Body.String())
	var sent struct {
		Results []struct {
			Job db.FederationJob `json:"job"`
		} `json:"results"`
	}
	testharness.DecodeJSON(t, rec, &sent)
	require.Len(t, sent.Results, 2)
	require.NotEqual(t, sent.Results[0].Job.ID, sent.Results[1].Job.ID)
	for _, r := range sent.Results {
		var q proto.JobRequest
		require.NoError(t, json.Unmarshal(r.Job.Request, &q))
		require.Equal(t, body["ref"], q.Ref)
	}
}
func TestFederation_JobsAutoSelectsOneAndDoesNotRetryBusy(t *testing.T) {
	fh := newFedHarness(t)
	other := addPlacementPeer(t, fh, "charlie")
	publishJobNode(t, fh.peer, 4)
	publishJobNode(t, other, 1)
	rec := fedHuman(t, fh.f, http.MethodPost, "/v1/federation/jobs", map[string]any{"node": "auto", "repo": "project", "ref": "main", "group": "team", "command": "make test", "timeout_seconds": 10})
	require.Equal(t, 200, rec.Code, rec.Body.String())
	var sent struct {
		Job db.FederationJob `json:"job"`
	}
	testharness.DecodeJSON(t, rec, &sent)
	require.Equal(t, other.id.ID(), sent.Job.Peer)
	fedEventually(t, "chosen job arrives", func() bool { return len(other.envelopes(proto.KindJobRequest)) == 1 })
	reply := other.envelope(proto.KindJobResult, proto.Endpoint{}, proto.JobResult{ID: sent.Job.ID, State: "refused", Code: "node_busy", ExitCode: 1})
	reply.From.Agent = ""
	other.send(reply)
	fedEventually(t, "busy refusal stored", func() bool { j, e := db.GetFederationJob(sent.Job.ID); return e == nil && j.State == "refused" })
	require.Empty(t, fh.peer.envelopes(proto.KindJobRequest))
	jobs, e := db.ListFederationJobs(false)
	require.NoError(t, e)
	require.Len(t, jobs, 1)
}
func TestFederation_JobsLiveFollowEncryptedReconnectDoesNotRerun(t *testing.T) {
	fh := newFedHarness(t)
	fh.f.HaveGroup("team")
	fedJobRepo(t, fh)
	fedAllowJobs(t, fh)
	t.Cleanup(agentd.SetRemoteJobDirectRunnerForTest())
	q := proto.JobRequest{ID: proto.NewEnvelopeID(), Repo: "project", Ref: "main", Group: "team", Harness: "shell", Command: "printf early; sleep 3; printf late >&2", Timeout: 10}
	env := fh.peer.envelope(proto.KindJobRequest, proto.Endpoint{}, q)
	env.From.Agent = ""
	fh.peer.send(env)
	var j *db.FederationJob
	fedEventually(t, "job running", func() bool { var e error; j, e = db.GetFederationJob(q.ID); return e == nil && j.State == "running" })
	cursor := jobstream.Cursor{}
	var stdout, stderr bytes.Buffer
	follow := func() io.ReadWriteCloser {
		kp, e := stream.NewKeyPair()
		require.NoError(t, e)
		req := bundletransfer.Request{Offer: q.ID, Stream: proto.NewEnvelopeID(), SHA256: j.Fingerprint, Key: kp.Pub}
		env := fh.peer.envelope(proto.KindJobFollow, proto.Endpoint{}, req)
		env.From.Agent = ""
		fh.peer.send(env)
		var a bundletransfer.Answer
		fedEventually(t, "follow answer", func() bool {
			for _, env := range fh.peer.envelopes(proto.KindJobFollowAnswer) {
				var next bundletransfer.Answer
				if env.DecodePayload(&next) == nil && next.Stream == req.Stream {
					a = next
					return true
				}
			}
			return false
		})
		require.True(t, a.OK, a.Reason)
		return fedPeerStream(t, fh.peer, req.Stream, kp, a.Key, true)
	}
	first := follow()
	f, e := jobstream.Read(first)
	require.NoError(t, e)
	require.NoError(t, cursor.Apply(f, &stdout, &stderr))
	require.Equal(t, "early", stdout.String())
	_ = first.Close()
	second := follow()
	defer second.Close()
	for {
		f, e = jobstream.Read(second)
		if e == io.EOF {
			break
		}
		require.NoError(t, e)
		require.NoError(t, cursor.Apply(f, &stdout, &stderr))
	}
	require.Equal(t, "early", stdout.String())
	require.Equal(t, "late", stderr.String())
	finished := fedWaitJob(t, fh, q.ID)
	require.Equal(t, j.WorkerID, finished.WorkerID)
	require.Equal(t, "completed", finished.State)
}

func TestFederation_JobsFollowAPIRequiresFramesAndLeavesExitPending(t *testing.T) {
	fh := newFedHarness(t)
	publishJobNode(t, fh.peer, 1)
	rec := fedHuman(t, fh.f, http.MethodPost, "/v1/federation/jobs", map[string]any{"node": "bob", "repo": "project", "ref": "main", "group": "team", "command": "make test", "timeout_seconds": 10})
	require.Equal(t, 200, rec.Code, rec.Body.String())
	var sent struct {
		Job db.FederationJob `json:"job"`
	}
	testharness.DecodeJSON(t, rec, &sent)
	result := make(chan *httptest.ResponseRecorder, 1)
	go func() { result <- fedHuman(t, fh.f, http.MethodGet, "/v1/federation/jobs/"+sent.Job.ID+"/follow", nil) }()
	fedEventually(t, "follow request", func() bool { return len(fh.peer.envelopes(proto.KindJobFollow)) > 0 })
	var req bundletransfer.Request
	require.NoError(t, fh.peer.envelopes(proto.KindJobFollow)[0].DecodePayload(&req))
	require.Equal(t, sent.Job.ID, req.Offer)
	require.Equal(t, sent.Job.Fingerprint, req.SHA256)
	kp, e := stream.NewKeyPair()
	require.NoError(t, e)
	a := fh.peer.envelope(proto.KindJobFollowAnswer, proto.Endpoint{}, bundletransfer.Answer{Request: bundletransfer.Request{Offer: req.Offer, Stream: req.Stream, SHA256: req.SHA256, Key: kp.Pub}, OK: true})
	a.From.Agent = ""
	fh.peer.send(a)
	conn := fedPeerStream(t, fh.peer, req.Stream, kp, req.Key, false)
	defer conn.Close()
	enc := jobstream.NewEncoder(conn)
	_, e = enc.Write(jobstream.Stdout, []byte("live"))
	require.NoError(t, e)
	require.NoError(t, conn.CloseWrite())
	select {
	case rec = <-result:
	case <-time.After(5 * time.Second):
		t.Fatal("follow API did not finish on authenticated FIN")
	}
	require.Equal(t, 200, rec.Code, rec.Body.String())
	frame, e := jobstream.Read(rec.Body)
	require.NoError(t, e)
	require.Equal(t, "live", string(frame.Data))
	_, e = jobstream.Read(rec.Body)
	require.ErrorIs(t, e, io.EOF)
	j, e := db.GetFederationJob(sent.Job.ID)
	require.NoError(t, e)
	require.Equal(t, "submitted", j.State, "stream FIN cannot publish a terminal worker exit")
}
