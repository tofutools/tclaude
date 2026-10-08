package agentd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/federation/bundletransfer"
	"github.com/tofutools/tclaude/pkg/federation/proto"
	"github.com/tofutools/tclaude/pkg/federation/stream"
)

// Logs use the shared descriptor, private spool and authenticated stream FIN.
// They are admitted only as a result of a locally submitted, matching job;
// unsolicited bundle offers never grant authority to receive job output.
var jobLogType = bundletransfer.Type{Name: "job-log", MaxBytes: 50 << 20, PendingLimit: 10, PendingBytes: 256 << 20}

func (rt *fedRuntime) persistJobLogs(j *db.FederationJob, res *proto.JobResult, out nonInteractiveSpawnResult) error {
	raw, e := json.Marshal(out)
	if e != nil {
		return e
	}
	d := bundletransfer.New(jobLogType, raw, "remote job output", j.ExpiresAt)
	if e = fedBundleSpool().Receive("out", j.Peer, d, bytes.NewReader(raw)); e != nil {
		return e
	}
	_, e = db.InsertFederationBundleOffer(db.FederationBundleOffer{Descriptor: d, Direction: "out", Peer: j.Peer, State: "ready"}, jobLogType)
	if e != nil {
		_ = fedBundleSpool().Remove("out", j.Peer, d.ID)
		return e
	}
	res.Logs, e = json.Marshal(d)
	return e
}
func (rt *fedRuntime) acceptJobResult(peer *db.FederationPeer, env *proto.Envelope) {
	var res proto.JobResult
	if env.From.Agent != "" || env.DecodePayload(&res) != nil {
		return
	}
	j, e := db.GetFederationJob(res.ID)
	if e != nil || j.Direction != "out" || j.Peer != peer.InstanceID || jobTerminal(j.State) {
		return
	}
	logKey := config.DataDir() + "/" + j.ID
	federationJobs.Lock()
	active := federationJobs.logs[logKey]
	federationJobs.Unlock()
	if j.State == "receiving_logs" {
		if active {
			return
		}
		if db.TransitionFederationJob(j.ID, "receiving_logs", "logs_pending", j.Result) != nil {
			return
		}
		j.State = "logs_pending"
	}
	// Never publish a terminal exit until all output has passed length/digest
	// validation and, for stream transfers, authenticated EOF verification.
	if !jobTerminal(res.State) {
		if res.State == "pending" || res.State == "preparing" || res.State == "running" || res.State == "unknown" || res.State == "stopping" {
			raw, _ := json.Marshal(res)
			_ = db.TransitionFederationJob(j.ID, j.State, res.State, raw)
		}
		return
	}
	raw, _ := json.Marshal(res)
	if len(res.Logs) == 0 {
		if res.State == "refused" || res.State == "canceled" || res.State == "interrupted" || res.State == "output_unavailable" {
			_ = db.TransitionFederationJob(j.ID, j.State, res.State, raw)
		}
		return
	}
	var d bundletransfer.Descriptor
	if json.Unmarshal(res.Logs, &d) != nil || d.Validate(jobLogType, time.Now()) != nil {
		return
	}
	federationJobs.Lock()
	if federationJobs.logs[logKey] {
		federationJobs.Unlock()
		return
	}
	if db.TransitionFederationJob(j.ID, j.State, "receiving_logs", raw) != nil {
		federationJobs.Unlock()
		return
	}
	federationJobs.logs[logKey] = true
	federationJobs.Unlock()
	rt.wg.Add(1)
	go func() {
		defer rt.wg.Done()
		defer func() { federationJobs.Lock(); delete(federationJobs.logs, logKey); federationJobs.Unlock() }()
		if rt.fetchJobLogs(rt.ctx, j.Peer, d) != nil {
			_ = db.TransitionFederationJob(j.ID, "receiving_logs", "logs_pending", raw)
			return
		}
		if db.TransitionFederationJob(j.ID, "receiving_logs", res.State, raw) == nil {
			rt.sendControl(j.Peer, proto.KindBundleResult, "", bundletransfer.Result{Offer: d.ID, State: "applied"})
		}
	}()
}
func (rt *fedRuntime) fetchJobLogs(parent context.Context, peer string, d bundletransfer.Descriptor) error {
	if len(d.Inline) > 0 {
		return fedBundleSpool().Receive("in", peer, d, bytes.NewReader(d.Inline))
	}
	key := "job-log/" + peer + "/" + d.ID
	if !rt.reserveBundleTransfer(key) {
		return errors.New("transfer busy")
	}
	defer rt.releaseBundleTransfer(key)
	ctx, cancel := context.WithTimeout(parent, 5*time.Minute)
	defer cancel()
	kp, e := stream.NewKeyPair()
	if e != nil {
		return e
	}
	sid := proto.NewEnvelopeID()
	ch := make(chan bundletransfer.Answer, 1)
	rt.bundleMu.Lock()
	if rt.bundleWaiters == nil {
		rt.bundleWaiters = map[string]fedBundleWaiter{}
	}
	rt.bundleWaiters[sid] = fedBundleWaiter{Peer: peer, Offer: d.ID, Digest: d.SHA256, Answer: ch}
	rt.bundleMu.Unlock()
	defer func() { rt.bundleMu.Lock(); delete(rt.bundleWaiters, sid); rt.bundleMu.Unlock() }()
	if !rt.sendControl(peer, proto.KindBundleFetch, "", bundletransfer.Request{Offer: d.ID, Stream: sid, SHA256: d.SHA256, Key: kp.Pub}) {
		return errors.New("log sender offline")
	}
	var a bundletransfer.Answer
	select {
	case a = <-ch:
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(30 * time.Second):
		return errors.New("log sender did not answer")
	}
	if !a.OK {
		return errors.New("log sender refused")
	}
	conn, e := rt.joinStream(ctx, peer, sid, kp, a.Key, true)
	if e != nil {
		return e
	}
	defer func() { _ = conn.Close() }()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Minute))
	if e = fedBundleSpool().Receive("in", peer, d, conn); e != nil {
		return e
	}
	p, e := db.GetFederationPeer(peer)
	if e != nil || p == nil {
		_ = fedBundleSpool().Remove("in", peer, d.ID)
		return errors.New("peer untrusted during log transfer")
	}
	return nil
}

func handleFederationJobLogs(w http.ResponseWriter, r *http.Request) {

	j, e := db.GetFederationJob(r.PathValue("id"))
	if e != nil || !jobTerminal(j.State) {
		writeError(w, 409, "job", "job output is not complete")
		return
	}
	if !authorizeJobAccess(w, r, j) {
		return
	}
	var res proto.JobResult
	var d bundletransfer.Descriptor
	if json.Unmarshal(j.Result, &res) != nil || json.Unmarshal(res.Logs, &d) != nil {
		writeError(w, 404, "logs", "job has no output artifact")
		return
	}
	direction := "out"
	if j.Direction == "out" {
		direction = "in"
	}
	raw, e := fedBundleSpool().Read(direction, j.Peer, d)
	if e != nil {
		writeError(w, 410, "logs", "job output expired or unavailable")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(raw)
}

func pruneFederationJobLogs() {
	jobs, e := db.ListFederationJobs(false)
	if e != nil {
		return
	}
	now := time.Now()
	for _, j := range jobs {
		if j.ExpiresAt.After(now) {
			continue
		}
		var res proto.JobResult
		var d bundletransfer.Descriptor
		if json.Unmarshal(j.Result, &res) != nil || json.Unmarshal(res.Logs, &d) != nil {
			continue
		}
		direction := "out"
		if j.Direction == "out" {
			direction = "in"
		}
		_ = fedBundleSpool().Remove(direction, j.Peer, d.ID)
	}
}
