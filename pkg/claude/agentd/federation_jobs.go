package agentd

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/federation/jobrepo"
	"github.com/tofutools/tclaude/pkg/federation/proto"
)

var federationJobs = struct {
	sync.Mutex
	cancels map[string]context.CancelFunc
	logs    map[string]bool
}{cancels: map[string]context.CancelFunc{}, logs: map[string]bool{}}

func registerFederationJobRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/federation/jobs", handleFederationJobSend)
	mux.HandleFunc("GET /v1/federation/jobs", handleFederationJobs)
	mux.HandleFunc("GET /v1/federation/jobs/{id}", handleFederationJobs)
	mux.HandleFunc("GET /v1/federation/jobs/{id}/logs", handleFederationJobLogs)
	mux.HandleFunc("POST /v1/federation/jobs/{id}/cancel", handleFederationJobCancel)
	mux.HandleFunc("POST /v1/federation/jobs/{id}/approve", handleFederationJobApprove)
	mux.HandleFunc("POST /v1/federation/jobs/{id}/retry", handleFederationJobRetry)
	mux.HandleFunc("POST /v1/federation/jobs/{id}/acknowledge-stopped", handleFederationJobAcknowledgeStopped)
}
func validateJob(q *proto.JobRequest) error {
	if !proto.ValidStreamID(q.ID) || !jobrepo.ValidName(q.Repo) || q.Group == "" || len(q.Group) > 256 {
		return errors.New("invalid job identity, repository or group")
	}
	if q.Command == "" || len(q.Command) > proto.MaxSpawnBrief {
		return errors.New("command must be nonempty and within the brief limit")
	}
	if q.Timeout == 0 {
		q.Timeout = 3600
	}
	if q.Timeout < 1 || q.Timeout > 86400 {
		return errors.New("timeout must be 1..86400 seconds")
	}
	_, e := jobrepo.NormalizeRef(context.Background(), q.Ref)
	if e != nil {
		return e
	}
	_, e = proto.ParseNodeMatch(q.Require)
	return e
}
func jobFingerprint(q proto.JobRequest) string {
	b, _ := json.Marshal(q)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
func jobTerminal(state string) bool {
	return state == "completed" || state == "failed" || state == "canceled" || state == "timeout" || state == "refused" || state == "interrupted" || state == "output_unavailable"
}
func jobCallerAllowed(w http.ResponseWriter, r *http.Request, peer, group string) bool {
	_, human, ok := authedCaller(w, r)
	if !ok {
		return false
	}
	if human {
		return true
	}
	_, ok = requirePermission(w, r, PermJobsRun, ActionContext{RemotePeer: peer, RemoteGroup: group})
	return ok
}
func handleFederationJobSend(w http.ResponseWriter, r *http.Request) {
	var in struct {
		proto.JobRequest
		Node string `json:"node"`
		Peer string `json:"peer"`
	}
	if e := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&in); e != nil {
		writeError(w, 400, "json", e.Error())
		return
	}
	in.ID = proto.NewEnvelopeID()
	if e := validateJob(&in.JobRequest); e != nil {
		writeError(w, 400, "job", e.Error())
		return
	}
	if in.Node != "" {
		in.Peer = in.Node
	}
	if in.Peer == "auto" || strings.HasPrefix(in.Peer, "group:") {
		writeError(w, 400, "placement", "automatic job placement is available with the follow/fan-out increment; select one explicit peer")
		return
	}
	peer, e := resolveFederationPeerOpt(in.Peer, false)
	if e != nil {
		writeFedErr(w, e)
		return
	}
	if !jobCallerAllowed(w, r, peer.InstanceID, in.Group) {
		return
	}
	cat, _, e := fedCatalogFor(peer.InstanceID)
	found := false
	if e == nil && cat != nil {
		for _, g := range cat.Groups {
			if g.Name == in.Group && g.HasCap(proto.CapJobs) {
				found = true
			}
		}
	}
	if !found {
		writeError(w, 403, "not_exported", "peer does not export this group for jobs")
		return
	}
	rt := currentFederation()
	if rt == nil {
		writeError(w, 503, "offline", "federation is offline")
		return
	}
	raw, _ := json.Marshal(in.JobRequest)
	j := db.FederationJob{ID: in.ID, Direction: "out", Peer: peer.InstanceID, Fingerprint: jobFingerprint(in.JobRequest), State: "submitted", Request: raw, ExpiresAt: time.Now().Add(72 * time.Hour)}
	caller, human, _ := authedCaller(w, r)
	if !human {
		j.CallerAgent, _ = db.AgentIDForConv(caller)
	}
	if e = db.InsertFederationJob(&j); e != nil {
		writeFedErr(w, e)
		return
	}
	// An undelivered/ambiguous send never causes failover: querying or retrying
	// this same ID is the only safe recovery once a request may have been seen.
	delivered := rt.sendControl(peer.InstanceID, proto.KindJobRequest, "", in.JobRequest)
	writeJSON(w, 200, map[string]any{"job": j, "delivered": delivered})
}
func handleFederationJobs(w http.ResponseWriter, r *http.Request) {
	caller, human, ok := authedCaller(w, r)
	if !ok {
		return
	}
	actor := ""
	if !human {
		actor, _ = db.AgentIDForConv(caller)
	}
	id := r.PathValue("id")
	if id == "" {
		rows, e := db.ListFederationJobs(false)
		if e != nil {
			writeFedErr(w, e)
			return
		}
		if !human {
			filtered := []db.FederationJob{}
			for _, j := range rows {
				if j.Direction == "out" && j.CallerAgent == actor {
					filtered = append(filtered, j)
				}
			}
			rows = filtered
		}
		writeJSON(w, 200, map[string]any{"jobs": rows})
		return
	}
	j, e := db.GetFederationJob(id)
	if e != nil {
		writeError(w, 404, "job", "job not found")
		return
	}
	if !human && (j.Direction != "out" || j.CallerAgent != actor) {
		writeError(w, 403, "job", "job belongs to another caller")
		return
	}
	if !human {
		var q proto.JobRequest
		_ = json.Unmarshal(j.Request, &q)
		if !jobCallerAllowed(w, r, j.Peer, q.Group) {
			return
		}
	}
	rt := currentFederation()
	if j.Direction == "out" && !jobTerminal(j.State) && rt != nil {
		rt.sendControl(j.Peer, proto.KindJobStatus, "", proto.JobControl{ID: j.ID})
	}
	writeJSON(w, 200, j)
}
func handleFederationJobCancel(w http.ResponseWriter, r *http.Request) {

	j, e := db.GetFederationJob(r.PathValue("id"))
	if e != nil {
		writeError(w, 404, "job", "job not found")
		return
	}
	if !authorizeJobAccess(w, r, j) {
		return
	}
	if j.Direction == "out" {
		if rt := currentFederation(); rt != nil {
			rt.sendControl(j.Peer, proto.KindJobCancel, "", proto.JobControl{ID: j.ID})
		}
	} else {
		cancelFederationJob(j)
	}
	writeJSON(w, 200, map[string]any{"id": j.ID, "cancel_requested": true})
}
func cancelFederationJob(j *db.FederationJob) {
	federationJobs.Lock()
	cancel := federationJobs.cancels[config.DataDir()+"/"+j.ID]
	federationJobs.Unlock()
	if cancel != nil {
		_ = db.TransitionFederationJob(j.ID, j.State, "stopping", nil)
		cancel()
		return
	}
	if j.State == "pending" {
		res, _ := json.Marshal(proto.JobResult{ID: j.ID, State: "canceled", ExitCode: 130})
		_ = db.TransitionFederationJob(j.ID, "pending", "canceled", res)
	}
}
func (rt *fedRuntime) acceptJobControl(peer *db.FederationPeer, env *proto.Envelope) {
	var q proto.JobControl
	if env.From.Agent != "" || env.DecodePayload(&q) != nil {
		return
	}
	j, e := db.GetFederationJob(q.ID)
	if e != nil || j.Direction != "in" || j.Peer != peer.InstanceID {
		return
	}
	if env.Kind == proto.KindJobCancel {
		cancelFederationJob(j)
		j, _ = db.GetFederationJob(q.ID)
	}
	rt.sendJobState(j)
}
func (rt *fedRuntime) sendJobState(j *db.FederationJob) {
	if j == nil {
		return
	}
	res := proto.JobResult{ID: j.ID, State: j.State}
	if len(j.Result) > 2 {
		_ = json.Unmarshal(j.Result, &res)
	}
	rt.sendControl(j.Peer, proto.KindJobResult, "", res)
}
func (rt *fedRuntime) acceptJobRequest(peer *db.FederationPeer, env *proto.Envelope) {
	var q proto.JobRequest
	if env.From.Agent != "" || env.To.Agent != "" || env.DecodePayload(&q) != nil || validateJob(&q) != nil {
		return
	}
	if !rt.allowInbound(peer.InstanceID) {
		return
	}
	nodeAdmission.Lock()
	old, e := db.GetFederationJob(q.ID)
	if e == nil {
		nodeAdmission.Unlock()
		if old.Peer == peer.InstanceID && old.Direction == "in" && old.Fingerprint == jobFingerprint(q) {
			rt.sendJobState(old)
		}
		return
	}
	if !errors.Is(e, sql.ErrNoRows) {
		nodeAdmission.Unlock()
		return
	}
	g, _ := db.GetAgentGroupByName(q.Group)
	repo, _ := db.GetFederationRepo(q.Repo)
	grant := (*db.FederationPeerGrant)(nil)
	if g != nil {
		grant = fedPeerGroupGrant(peer.InstanceID, g.ID, PermJobsRun)
	}
	state, code := "pending", ""
	match, _ := proto.ParseNodeMatch(q.Require)
	if !match.Matches(localNodeMetadata()) {
		state, code = "refused", "node_incompatible"
	}
	allowed := false
	if repo != nil && repo.Enabled && g != nil && !g.IsArchived() && grant != nil {
		for _, id := range repo.Definition.Groups {
			if id == g.ID {
				allowed = true
			}
		}
	}
	if !allowed {
		state, code = "refused", "not_exported"
	}
	if state == "pending" {
		limit, e := nodeCapacityLimit()
		if e != nil {
			state, code = "refused", "node_capacity_unknown"
		} else if limit > 0 {
			used, e := nodeCapacityUsed("")
			if e != nil {
				state, code = "refused", "node_capacity_unknown"
			} else if used >= limit {
				state, code = "refused", fedCodeNodeBusy
			}
		}
	}
	if state == "pending" {
		rows, e := db.ListFederationJobs(true)
		if e != nil {
			state, code = "refused", "internal"
		} else {
			count := 0
			for _, j := range rows {
				if j.Peer == peer.InstanceID {
					count++
				}
			}
			workers, e := db.ListFederationAutoWorkers(peer.InstanceID)
			if e != nil {
				state, code = "refused", "internal"
			} else {
				live := 0
				reserved := map[string]bool{}
				requests, requestErr := db.ListFederationSpawnReservations()
				if requestErr != nil {
					state, code = "refused", "internal"
				} else {
					for _, req := range requests {
						if req.FromInstance == peer.InstanceID && !req.Expired(time.Now()) {
							id := req.ResultAgent
							if id == "" {
								id = "request:" + req.EnvelopeID
							}
							reserved[id] = true
						}
					}
				}
				for _, id := range workers {
					a, e := db.GetAgent(id)
					if e != nil {
						state, code = "refused", "internal"
						break
					}
					if a != nil && a.Active() && isConvOnline(a.CurrentConvID) {
						reserved[id] = true
					}
				}
				live = len(reserved)
				cap := grant.SpawnPolicy.MaxLive
				if cap <= 0 {
					cap = 2
				}
				if count+live >= cap {
					state, code = "refused", "peer_capacity"
				}
			}
		}
	}
	raw, _ := json.Marshal(q)
	result, _ := json.Marshal(proto.JobResult{ID: q.ID, State: state, Code: code})
	j := db.FederationJob{ID: q.ID, Direction: "in", Peer: peer.InstanceID, Fingerprint: jobFingerprint(q), State: state, Request: raw, WorkerID: db.NewAgentID(), Result: result, ExpiresAt: time.Now().Add(72 * time.Hour)}
	if repo != nil {
		j.RepoID = repo.ID
		j.RepoRevision = repo.Revision
	}
	e = db.InsertFederationJob(&j)
	nodeAdmission.Unlock()
	if e != nil {
		return
	}
	rt.sendJobState(&j)
	if state == "pending" && grant != nil && grant.SpawnPolicy.JobApproval != "manual" {
		rt.startJob(&j)
	}
}
func handleFederationJobApprove(w http.ResponseWriter, r *http.Request) {
	if !requireHuman(w, r, "approve remote job execution") {
		return
	}
	j, e := db.GetFederationJob(r.PathValue("id"))
	if e != nil || j.Direction != "in" || j.State != "pending" {
		writeError(w, 409, "job", "job is not pending here")
		return
	}
	rt := currentFederation()
	if rt == nil {
		writeError(w, 503, "offline", "federation offline")
		return
	}
	rt.startJob(j)
	writeJSON(w, 200, map[string]any{"id": j.ID})
}
func (rt *fedRuntime) startJob(j *db.FederationJob) {
	if !j.ExpiresAt.After(time.Now()) {
		return
	}
	federationJobs.Lock()
	if db.TransitionFederationJob(j.ID, "pending", "preparing", nil) != nil {
		federationJobs.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(rt.ctx)
	key := config.DataDir() + "/" + j.ID
	federationJobs.cancels[key] = cancel
	federationJobs.Unlock()
	rt.wg.Add(1)
	go func() {
		defer rt.wg.Done()
		defer cancel()
		defer func() { federationJobs.Lock(); delete(federationJobs.cancels, key); federationJobs.Unlock() }()
		rt.runJob(ctx, j)
	}()
}
func (rt *fedRuntime) runJob(parent context.Context, j *db.FederationJob) {
	var q proto.JobRequest
	_ = json.Unmarshal(j.Request, &q)
	ctx, cancel := context.WithTimeout(parent, time.Duration(q.Timeout)*time.Second)
	defer cancel()
	result := proto.JobResult{ID: j.ID, State: "failed", ExitCode: 1}
	out := nonInteractiveSpawnResult{}
	finish := func() {
		if ctx.Err() != nil && result.State != "unknown" {
			result.State = "canceled"
			result.ExitCode = 130
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				result.State = "timeout"
				result.ExitCode = 124
			}
		}
		if e := rt.persistJobLogs(j, &result, out); e != nil {
			if result.State != "unknown" {
				result.ExitCode = 1
				result.State = "output_unavailable"
			}
			result.Code = "log_storage"
		}
		raw, _ := json.Marshal(result)
		current, e := db.GetFederationJob(j.ID)
		if e == nil {
			_ = db.TransitionFederationJob(j.ID, current.State, result.State, raw)
		}
		if result.State != "unknown" {
			_, _ = db.RetireAgentByID(j.WorkerID, "remote-job", "one-shot finished")
		}
		current, _ = db.GetFederationJob(j.ID)
		rt.sendJobState(current)
	}
	defer finish()
	trusted, e := db.GetFederationPeer(j.Peer)
	if e != nil || trusted == nil {
		result.Code = "untrusted"
		return
	}
	repo, e := db.GetFederationRepo(j.RepoID)
	if e != nil || !repo.Enabled || repo.Revision != j.RepoRevision {
		result.Code = "repository_changed"
		return
	}
	g, e := db.GetAgentGroupByName(q.Group)
	if e != nil || g == nil || !jobRepoAllows(repo, g.ID) || fedPeerGroupGrant(j.Peer, g.ID, PermJobsRun) == nil {
		result.Code = "grant_revoked"
		return
	}
	cache, e := os.UserCacheDir()
	if e != nil {
		result.Code = "checkout"
		return
	}
	root := filepath.Join(cache, "tclaude", "remote-jobs")
	if e = os.MkdirAll(root, 0700); e != nil {
		result.Code = "checkout"
		return
	}
	checkout, e := jobrepo.Prepare(ctx, repo.Definition, filepath.Join(root, j.ID), q.Ref)
	if e != nil {
		result.Code = "checkout"
		return
	}
	defer func() {
		if result.State != "unknown" {
			_ = os.RemoveAll(checkout.Root)
		}
	}()
	result.Commit = checkout.Commit
	defaults, e := db.ResolveFederationWorkerDefaults(j.Peer)
	if e != nil {
		result.Code = "worker_defaults"
		return
	}
	if e = db.RecordFederationWorkerDefaults(j.WorkerID, defaults); e != nil {
		result.Code = "worker_defaults"
		return
	}
	// Authority and the immutable allowlist revision are rechecked immediately
	// before crossing the ordinary receiving-group launch boundary.
	repo, e = db.GetFederationRepo(j.RepoID)
	if e != nil || !repo.Enabled || repo.Revision != j.RepoRevision || !jobRepoAllows(repo, g.ID) || fedPeerGroupGrant(j.Peer, g.ID, PermJobsRun) == nil {
		result.Code = "authority_changed"
		return
	}
	if e = db.TransitionFederationJob(j.ID, "preparing", "running", nil); e != nil {
		return
	}
	launch := &federationJobLaunch{Peer: j.Peer, RepoID: j.RepoID, RepoRevision: j.RepoRevision, ID: j.ID, WorkerID: j.WorkerID, GroupID: g.ID, Cwd: checkout.Path, Harness: q.Harness, Defaults: defaults}
	grant := fedPeerGroupGrant(j.Peer, g.ID, PermJobsRun)
	if grant == nil {
		result.Code = "authority_changed"
		return
	}
	policy := grant.SpawnPolicy
	if policy.Harness != "" && q.Harness != "" && policy.Harness != q.Harness {
		result.Code = "harness_policy"
		return
	}
	body := agent.SpawnRequest{Profile: policy.Profile, Model: policy.Model, Cwd: checkout.Path, Harness: fedFirst(policy.Harness, q.Harness), InitialMessage: q.Command, NonInteractive: true, RunTimeoutSeconds: q.Timeout}
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(raw))
	req = req.WithContext(context.WithValue(ctx, federationJobContextKey{}, launch))
	req = req.WithContext(context.WithValue(req.Context(), reservedAgentIDContextKey{}, j.WorkerID))
	req = req.WithContext(context.WithValue(req.Context(), peerKey{}, &peer{PID: 1, HumanTokenValid: true}))
	rec := httptest.NewRecorder()
	handleGroupSpawn(rec, req, g)
	if rec.Code != 200 {
		result.Code = "launch_refused"
		var failure struct {
			Code string `json:"code"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &failure)
		if failure.Code == "remote_job_unknown" {
			result.State = "unknown"
			result.Code = "execution_uncertain"
		}
		out.Stderr = rec.Body.String()
		return
	}
	if e = json.Unmarshal(rec.Body.Bytes(), &out); e != nil {
		result.Code = "result_invalid"
		return
	}
	result.ExitCode = out.ExitCode
	result.State = "completed"
	if out.ExitCode != 0 {
		result.State = "failed"
	}
	if out.ExitCode == 124 {
		result.State = "timeout"
	}
}

func authorizeJobAccess(w http.ResponseWriter, r *http.Request, j *db.FederationJob) bool {
	caller, human, ok := authedCaller(w, r)
	if !ok {
		return false
	}
	if human {
		return true
	}
	actor, e := db.AgentIDForConv(caller)
	if e != nil || j.Direction != "out" || actor == "" || j.CallerAgent != actor {
		writeError(w, 403, "job", "job belongs to another caller")
		return false
	}
	var q proto.JobRequest
	_ = json.Unmarshal(j.Request, &q)
	return jobCallerAllowed(w, r, j.Peer, q.Group)
}

func jobRepoAllows(repo *db.FederationRepo, group int64) bool {
	if repo == nil || !repo.Enabled {
		return false
	}
	for _, id := range repo.Definition.Groups {
		if id == group {
			return true
		}
	}
	return false
}

// Active runners are canceled on untrust. A reservation left by a previous
// daemon is explicitly unknown: it must never become a second execution.
func reconcileFederationJobs() {
	rows, e := db.ListFederationJobs(true)
	if e != nil {
		return
	}
	for _, j := range rows {
		if j.State == "pending" && !j.ExpiresAt.After(time.Now()) {
			raw, _ := json.Marshal(proto.JobResult{ID: j.ID, State: "refused", Code: "expired", ExitCode: 1})
			_ = db.TransitionFederationJob(j.ID, "pending", "refused", raw)
			continue
		}
		federationJobs.Lock()
		cancel := federationJobs.cancels[config.DataDir()+"/"+j.ID]
		federationJobs.Unlock()
		p, e := db.GetFederationPeer(j.Peer)
		if e != nil {
			continue
		}
		if p == nil {
			if cancel != nil {
				cancel()
			} else if j.State == "pending" {
				cancelFederationJob(&j)
			}
			continue
		}
		if cancel == nil && (j.State == "preparing" || j.State == "running" || j.State == "stopping") {
			raw, _ := json.Marshal(proto.JobResult{ID: j.ID, State: "unknown", Code: "daemon_interrupted", ExitCode: 1})
			_ = db.TransitionFederationJob(j.ID, j.State, "unknown", raw)
		}
	}
}

func handleFederationJobRetry(w http.ResponseWriter, r *http.Request) {
	j, e := db.GetFederationJob(r.PathValue("id"))
	if e != nil || j.Direction != "out" {
		writeError(w, 404, "job", "submitted job not found")
		return
	}
	if !authorizeJobAccess(w, r, j) {
		return
	}
	rt := currentFederation()
	if rt == nil {
		writeError(w, 503, "offline", "federation offline")
		return
	}
	var q proto.JobRequest
	if json.Unmarshal(j.Request, &q) != nil {
		writeError(w, 500, "job", "stored request unavailable")
		return
	}
	delivered := rt.sendControl(j.Peer, proto.KindJobRequest, "", q)
	writeJSON(w, 200, map[string]any{"id": j.ID, "delivered": delivered})
}

func handleFederationJobAcknowledgeStopped(w http.ResponseWriter, r *http.Request) {
	if !requireHuman(w, r, "acknowledge an uncertain job has stopped") {
		return
	}
	var body struct {
		Acknowledged bool `json:"acknowledge_stopped"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body) != nil || !body.Acknowledged {
		writeError(w, 400, "acknowledgement", "explicit acknowledge_stopped=true is required after confirming the workload stopped")
		return
	}
	j, e := db.GetFederationJob(r.PathValue("id"))
	if e != nil || j.Direction != "in" || j.State != "unknown" {
		writeError(w, 409, "job", "job is not an uncertain local execution")
		return
	}
	if isConvOnline("job-" + j.ID) {
		writeError(w, 409, "job_alive", "job pane is still alive; stop it before acknowledging")
		return
	}
	result, _ := json.Marshal(proto.JobResult{ID: j.ID, State: "interrupted", Code: "operator_acknowledged_stopped", ExitCode: 1})
	if e = db.TransitionFederationJob(j.ID, "unknown", "interrupted", result); e != nil {
		writeFedErr(w, e)
		return
	}
	_, _ = db.RetireAgentByID(j.WorkerID, "operator", "acknowledged uncertain job stopped")
	if rt := currentFederation(); rt != nil {
		current, _ := db.GetFederationJob(j.ID)
		rt.sendJobState(current)
	}
	writeJSON(w, 200, map[string]any{"id": j.ID, "state": "interrupted"})
}
