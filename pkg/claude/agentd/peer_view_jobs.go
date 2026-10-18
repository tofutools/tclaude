package agentd

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// A peer view request is one exchange of at most fedPeerViewTimeout, and its
// reply one JSON value. The routes in peerViewJobRoutes do longer work (a
// git pull or sync over many checkouts, a worktree cleanup) and the git batch
// streams its progress, so through the peer view they run as jobs: the
// request answers 202 {"peer_job": id} at once, the handler runs in the
// background against a buffering writer, and GET /api/peer-view-jobs/{id}
// answers {"status": "running"} until it carries the handler's reply. The
// requesting node's proxy (servePeerViewProxyWith) waits on the job, so its
// caller sees the handler's own reply, as for any other route.
var peerViewJobRoutes = map[string]bool{
	"/api/git-repositories/batch": true,
	"/api/worktrees/cleanup":      true,
}

const (
	peerViewJobTimeout  = 30 * time.Minute
	peerViewJobKeep     = 10 * time.Minute
	peerViewJobsPerPeer = 2
	peerViewJobPoll     = time.Second
)

type peerViewJob struct {
	peer     string
	done     chan struct{}
	out      *peerViewResponse
	finished time.Time
}

var peerViewJobs = struct {
	sync.Mutex
	byID map[string]*peerViewJob
}{byID: map[string]*peerViewJob{}}

// peerViewJobRoute reports whether a request on pattern runs as a job.
func peerViewJobRoute(pattern string, r *http.Request) bool {
	return r.Method == http.MethodPost && peerViewJobRoutes[pattern]
}

// startPeerViewJob runs serve for peer in the background on a copy of r
// detached from the exchange's deadline, and answers its job ID. It refuses
// (false) while the peer already runs peerViewJobsPerPeer jobs.
func startPeerViewJob(peer string, r *http.Request, serve func(*peerViewResponse, *http.Request)) (string, bool) {
	body, _ := io.ReadAll(io.LimitReader(r.Body, fedPeerViewBodyLimit+1))
	raw := make([]byte, 12)
	_, _ = rand.Read(raw)
	id := "pvj_" + hex.EncodeToString(raw)
	job := &peerViewJob{peer: peer, done: make(chan struct{}), out: &peerViewResponse{header: make(http.Header)}}

	peerViewJobs.Lock()
	running := 0
	for key, other := range peerViewJobs.byID {
		select {
		case <-other.done:
			if time.Since(other.finished) > peerViewJobKeep {
				delete(peerViewJobs.byID, key)
			}
		default:
			if other.peer == peer {
				running++
			}
		}
	}
	if running >= peerViewJobsPerPeer {
		peerViewJobs.Unlock()
		return "", false
	}
	peerViewJobs.byID[id] = job
	peerViewJobs.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), peerViewJobTimeout)
	req := r.Clone(ctx)
	req.Body = io.NopCloser(bytes.NewReader(body))
	go func() {
		defer cancel()
		serve(job.out, req)
		peerViewJobs.Lock()
		job.finished = time.Now()
		peerViewJobs.Unlock()
		close(job.done)
	}()
	return id, true
}

type peerViewJobResult struct {
	Status      int    `json:"status"`
	ContentType string `json:"content_type,omitempty"`
	Body        []byte `json:"body,omitempty"`
}

type peerViewJobState struct {
	Status string             `json:"status"`
	Result *peerViewJobResult `json:"result,omitempty"`
}

// servePeerViewJob answers a job's state to the peer that started it.
func servePeerViewJob(w http.ResponseWriter, r *http.Request, v *peerView, _ peerViewRule) {
	peerViewJobs.Lock()
	job := peerViewJobs.byID[r.PathValue("id")]
	peerViewJobs.Unlock()
	if job == nil || job.peer != v.peer.InstanceID {
		writeError(w, http.StatusNotFound, "not_found", "no such job")
		return
	}
	select {
	case <-job.done:
		writeJSON(w, http.StatusOK, peerViewJobState{Status: "done", Result: &peerViewJobResult{
			Status: job.out.statusCode(), ContentType: job.out.Header().Get("Content-Type"), Body: job.out.body.Bytes(),
		}})
	default:
		writeJSON(w, http.StatusOK, peerViewJobState{Status: "running"})
	}
}

// awaitPeerViewJob turns a peer's 202 {"peer_job": id} reply into the job's
// own reply, polling the peer until it is done. Any other reply is returned
// unchanged (ok false).
func awaitPeerViewJob(ctx context.Context, rt *fedRuntime, peer *db.FederationPeer, reply fedPeerViewReply) (fedPeerViewReply, bool, error) {
	if reply.Status != http.StatusAccepted {
		return reply, false, nil
	}
	var started struct {
		Job string `json:"peer_job"`
	}
	if json.Unmarshal(reply.Body, &started) != nil || started.Job == "" {
		return reply, false, nil
	}
	ctx, cancel := context.WithTimeout(ctx, peerViewJobTimeout+time.Minute)
	defer cancel()
	poll := fedPeerViewRequest{Method: http.MethodGet, URI: "/api/peer-view-jobs/" + started.Job, Header: http.Header{}}
	for {
		select {
		case <-ctx.Done():
			return reply, true, errPeerViewTimeout
		case <-time.After(peerViewJobPoll):
		}
		answer, err := exchangePeerView(ctx, rt, peer, poll)
		if err != nil {
			if errors.Is(err, errPeerViewBusy) {
				continue
			}
			return reply, true, err
		}
		var state peerViewJobState
		if answer.Status != http.StatusOK || json.Unmarshal(answer.Body, &state) != nil {
			return fedPeerViewReply{Status: answer.Status, Header: answer.Header, Body: answer.Body}, true, nil
		}
		if state.Status == "done" && state.Result != nil {
			header := http.Header{}
			if state.Result.ContentType != "" {
				header.Set("Content-Type", state.Result.ContentType)
			}
			return fedPeerViewReply{Status: state.Result.Status, Header: header, Body: state.Result.Body}, true, nil
		}
	}
}
