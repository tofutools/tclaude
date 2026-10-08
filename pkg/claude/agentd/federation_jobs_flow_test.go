package agentd_test

import (
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/federation/bundletransfer"
	"github.com/tofutools/tclaude/pkg/federation/proto"
	"github.com/tofutools/tclaude/pkg/federation/stream"
	"github.com/tofutools/tclaude/pkg/testharness"
	"github.com/tofutools/tclaude/pkg/testutil"
)

func fedJobRepo(t *testing.T, fh *fedHarness) string {
	t.Helper()
	root := testutil.CanonicalTempDir(t)
	git := func(args ...string) string {
		c := exec.Command("git", args...)
		c.Dir = root
		c.Env = append(os.Environ(), "GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.test", "GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.test")
		b, e := c.CombinedOutput()
		require.NoError(t, e, string(b))
		return string(b)
	}
	git("init", "-b", "main")
	require.NoError(t, os.WriteFile(filepath.Join(root, "hello"), []byte("from git\n"), 0600))
	git("add", ".")
	git("commit", "-m", "first")
	rec := fedHuman(t, fh.f, http.MethodPost, "/v1/federation/repos", map[string]any{"name": "project", "url": (&url.URL{Scheme: "file", Path: root}).String(), "clone": root, "groups": []string{"team"}})
	require.Equal(t, 200, rec.Code, rec.Body.String())
	return root
}
func fedAllowJobs(t *testing.T, fh *fedHarness) {
	t.Helper()
	rec := fedHuman(t, fh.f, http.MethodPost, "/v1/federation/grants", map[string]any{"peer": "bob", "slug": agentd.PermJobsRun, "scope": "group=team", "spawn_policy": map[string]any{"max_live": 2}})
	require.Equal(t, 200, rec.Code, rec.Body.String())
}
func fedWaitJob(t *testing.T, fh *fedHarness, id string) *db.FederationJob {
	t.Helper()
	var job db.FederationJob
	fedEventually(t, "job terminal", func() bool {
		rec := fedHuman(t, fh.f, http.MethodGet, "/v1/federation/jobs/"+id, nil)
		if rec.Code != 200 {
			return false
		}
		testharness.DecodeJSON(t, rec, &job)
		return job.State == "completed" || job.State == "failed" || job.State == "refused" || job.State == "canceled" || job.State == "timeout"
	})
	return &job
}
func TestFederation_JobsExactCheckoutNonzeroAndReplay(t *testing.T) {
	fh := newFedHarness(t)
	fh.f.HaveGroup("team")
	fedJobRepo(t, fh)
	fedAllowJobs(t, fh)
	t.Cleanup(agentd.SetRemoteJobDirectRunnerForTest())
	q := proto.JobRequest{ID: proto.NewEnvelopeID(), Repo: "project", Ref: "main", Group: "team", Harness: "shell", Command: "cat hello; printf warning >&2; exit 7", Timeout: 10}
	env := fh.peer.envelope(proto.KindJobRequest, proto.Endpoint{}, q)
	env.From.Agent = ""
	fh.peer.send(env)
	j := fedWaitJob(t, fh, q.ID)
	require.Equal(t, "failed", j.State, string(j.Result))
	var res proto.JobResult
	require.NoError(t, json.Unmarshal(j.Result, &res))
	require.Equal(t, 7, res.ExitCode)
	require.Len(t, res.Commit, 40)
	rec := fedHuman(t, fh.f, http.MethodGet, "/v1/federation/jobs/"+q.ID+"/logs", nil)
	require.Equal(t, 200, rec.Code, rec.Body.String())
	var logs map[string]any
	testharness.DecodeJSON(t, rec, &logs)
	require.Equal(t, "from git\n", logs["stdout"])
	require.Equal(t, "warning", logs["stderr"])
	worker := j.WorkerID
	env = fh.peer.envelope(proto.KindJobRequest, proto.Endpoint{}, q)
	env.From.Agent = ""
	fh.peer.send(env)
	fedEventually(t, "replayed terminal receipt", func() bool {
		for _, e := range fh.peer.envelopes(proto.KindJobResult) {
			var r proto.JobResult
			if e.DecodePayload(&r) == nil && r.ID == q.ID && r.State == "failed" {
				return true
			}
		}
		return false
	})
	again, e := db.GetFederationJob(q.ID)
	require.NoError(t, e)
	require.Equal(t, worker, again.WorkerID)
}
func TestFederation_JobsRequireDedicatedGrantAndAllowedRepo(t *testing.T) {
	fh := newFedHarness(t)
	fh.f.HaveGroup("team")
	fedJobRepo(t, fh)
	for _, repo := range []string{"project", "unknown"} {
		q := proto.JobRequest{ID: proto.NewEnvelopeID(), Repo: repo, Ref: "main", Group: "team", Harness: "shell", Command: "echo forbidden", Timeout: 10}
		env := fh.peer.envelope(proto.KindJobRequest, proto.Endpoint{}, q)
		env.From.Agent = ""
		fh.peer.send(env)
		j := fedWaitJob(t, fh, q.ID)
		require.Equal(t, "refused", j.State)
	}
}

func TestFederation_JobsWorkerDefaultsInstalledBeforeExec(t *testing.T) {
	fh := newFedHarness(t)
	fh.f.HaveGroup("team")
	fedJobRepo(t, fh)
	profile := fedNodeProfile(t, fh, "worker-defaults", db.FederationNodeProfileSpec{PeerGrants: []db.FederationPeerGrant{{Slug: agentd.PermJobsRun, Scope: "group=team", SpawnPolicy: db.FederationSpawnPolicy{MaxLive: 2}}}, WorkerPermissions: map[string]db.PermissionOverride{agentd.PermGroupsCreate: {Effect: db.PermEffectDeny}}})
	fedApplyNodeProfile(t, fh, profile)
	checked := make(chan string, 1)
	t.Cleanup(agentd.SetRemoteJobDirectRunnerForTest(func(id string) {
		snapshot, e := db.GetFederationWorkerDefaults(id)
		require.NoError(t, e)
		require.NotNil(t, snapshot)
		require.Equal(t, profile.ID, snapshot.ProfileID)
		actor, e := db.GetAgent(id)
		require.NoError(t, e)
		require.NotNil(t, actor)
		grants, e := db.ListAgentPermissionOverridesForConv(actor.CurrentConvID)
		require.NoError(t, e)
		require.Equal(t, db.PermEffectDeny, grants[agentd.PermGroupsCreate])
		checked <- id
	}))
	q := proto.JobRequest{ID: proto.NewEnvelopeID(), Repo: "project", Ref: "main", Group: "team", Harness: "shell", Command: "printf done", Timeout: 10}
	env := fh.peer.envelope(proto.KindJobRequest, proto.Endpoint{}, q)
	env.From.Agent = ""
	fh.peer.send(env)
	job := fedWaitJob(t, fh, q.ID)
	require.Equal(t, "completed", job.State, string(job.Result))
	require.Equal(t, job.WorkerID, <-checked)
}
func TestFederation_JobsShareNodeAdmissionAndCancel(t *testing.T) {
	fh := newFedHarness(t)
	fh.f.HaveGroup("team")
	fedJobRepo(t, fh)
	fedAllowJobs(t, fh)
	t.Cleanup(agentd.SetRemoteJobDirectRunnerForTest())
	cfg, e := config.Load()
	require.NoError(t, e)
	cfg.Federation.MaxLiveAgents = 1
	require.NoError(t, config.Save(cfg))
	q := proto.JobRequest{ID: proto.NewEnvelopeID(), Repo: "project", Ref: "main", Group: "team", Harness: "shell", Command: "sleep 30", Timeout: 40}
	send := func(q proto.JobRequest) {
		env := fh.peer.envelope(proto.KindJobRequest, proto.Endpoint{}, q)
		env.From.Agent = ""
		fh.peer.send(env)
	}
	send(q)
	fedEventually(t, "job running", func() bool { j, e := db.GetFederationJob(q.ID); return e == nil && j.State == "running" })
	next := q
	next.ID = proto.NewEnvelopeID()
	send(next)
	busy := fedWaitJob(t, fh, next.ID)
	require.Equal(t, "refused", busy.State)
	var result proto.JobResult
	require.NoError(t, json.Unmarshal(busy.Result, &result))
	require.Equal(t, "node_busy", result.Code)
	rec := fedHuman(t, fh.f, http.MethodPost, "/v1/federation/jobs/"+q.ID+"/cancel", nil)
	require.Equal(t, 200, rec.Code, rec.Body.String())
	stopped := fedWaitJob(t, fh, q.ID)
	require.Equal(t, "canceled", stopped.State, string(stopped.Result))
	next.ID = proto.NewEnvelopeID()
	next.Command = "echo released"
	send(next)
	require.Equal(t, "completed", fedWaitJob(t, fh, next.ID).State)
}

func TestFederation_JobsLargeLogsRequireDigestAndAuthenticatedFIN(t *testing.T) {
	fh := newFedHarness(t)
	p := fh.peer
	cat := p.envelope(proto.KindCatalog, proto.Endpoint{}, proto.CatalogPayload{Groups: []proto.CatalogGroup{{Name: "team", Caps: []string{proto.CapJobs}}}})
	p.send(cat)
	fedEventually(t, "job capability publication", func() bool {
		raw, _, e := db.GetFederationCatalog(p.id.ID())
		return e == nil && strings.Contains(raw, `"jobs"`)
	})
	rec := fedHuman(t, fh.f, http.MethodPost, "/v1/federation/jobs", map[string]any{"node": "bob", "repo": "project", "ref": "main", "group": "team", "harness": "shell", "command": "echo result", "timeout_seconds": 10})
	require.Equal(t, 200, rec.Code, rec.Body.String())
	var sent struct {
		Job db.FederationJob `json:"job"`
	}
	testharness.DecodeJSON(t, rec, &sent)
	output := strings.Repeat("output ", 50000)
	raw, e := json.Marshal(map[string]any{"stdout": output, "stderr": "", "exit_code": 0})
	require.NoError(t, e)
	kind := bundletransfer.Type{Name: "job-log", MaxBytes: 50 << 20}
	d := bundletransfer.New(kind, raw, "job logs", time.Now().Add(time.Hour))
	require.Empty(t, d.Inline)
	descriptor, e := json.Marshal(d)
	require.NoError(t, e)
	for attempt := 0; attempt < 3; attempt++ {
		result := proto.JobResult{ID: sent.Job.ID, State: "completed", ExitCode: 0, Logs: descriptor}
		env := p.envelope(proto.KindJobResult, proto.Endpoint{}, result)
		env.From.Agent = ""
		p.send(env)
		fedEventually(t, "job log fetch", func() bool { return len(p.envelopes(proto.KindBundleFetch)) > attempt })
		var req bundletransfer.Request
		require.NoError(t, p.envelopes(proto.KindBundleFetch)[attempt].DecodePayload(&req))
		kp, e := stream.NewKeyPair()
		require.NoError(t, e)
		answer := p.envelope(proto.KindBundleAnswer, proto.Endpoint{}, bundletransfer.Answer{Request: bundletransfer.Request{Offer: req.Offer, Stream: req.Stream, SHA256: req.SHA256, Key: kp.Pub}, OK: true})
		answer.From.Agent = ""
		p.send(answer)
		conn := fedPeerStream(t, p, req.Stream, kp, req.Key, false)
		payload := append([]byte{}, raw...)
		if attempt == 1 {
			payload[len(payload)-1] ^= 1
		}
		_, e = conn.Write(payload)
		require.NoError(t, e)
		if attempt == 0 {
			require.NoError(t, conn.Close())
		} else {
			require.NoError(t, conn.CloseWrite())
		}
		if attempt < 2 {
			fedEventually(t, "rejected incomplete or corrupt logs", func() bool { j, e := db.GetFederationJob(sent.Job.ID); return e == nil && j.State == "logs_pending" })
		} else {
			j := fedWaitJob(t, fh, sent.Job.ID)
			require.Equal(t, "completed", j.State)
		}
		_ = conn.Close()
	}
	rec = fedHuman(t, fh.f, http.MethodGet, "/v1/federation/jobs/"+sent.Job.ID+"/logs", nil)
	require.Equal(t, 200, rec.Code, rec.Body.String())
	var logs struct {
		Stdout string `json:"stdout"`
	}
	testharness.DecodeJSON(t, rec, &logs)
	require.Equal(t, output, logs.Stdout)
}

func TestFederation_JobsRestartKeepsUncertainReservation(t *testing.T) {
	fh := newFedHarness(t)
	fh.f.HaveGroup("team")
	fedJobRepo(t, fh)
	fedAllowJobs(t, fh)
	cfg, e := config.Load()
	require.NoError(t, e)
	cfg.Federation.MaxLiveAgents = 1
	require.NoError(t, config.Save(cfg))
	abandoned := db.FederationJob{ID: proto.NewEnvelopeID(), Direction: "in", Peer: fh.peer.id.ID(), Fingerprint: "original", State: "running", Request: json.RawMessage(`{}`), WorkerID: db.NewAgentID(), ExpiresAt: time.Now().Add(time.Hour)}
	require.NoError(t, db.InsertFederationJob(&abandoned))
	fedEventually(t, "interrupted daemon reservation becomes unknown", func() bool { j, e := db.GetFederationJob(abandoned.ID); return e == nil && j.State == "unknown" })
	q := proto.JobRequest{ID: proto.NewEnvelopeID(), Repo: "project", Ref: "main", Group: "team", Harness: "shell", Command: "echo unsafe-duplicate", Timeout: 10}
	env := fh.peer.envelope(proto.KindJobRequest, proto.Endpoint{}, q)
	env.From.Agent = ""
	fh.peer.send(env)
	j := fedWaitJob(t, fh, q.ID)
	require.Equal(t, "refused", j.State)
	path := "/v1/federation/jobs/" + abandoned.ID + "/acknowledge-stopped"
	rec := fedHuman(t, fh.f, http.MethodPost, path, map[string]any{})
	require.Equal(t, 400, rec.Code)
	rec = fedHuman(t, fh.f, http.MethodPost, path, map[string]any{"acknowledge_stopped": true})
	require.Equal(t, 200, rec.Code, rec.Body.String())
	saved, e := db.GetFederationJob(abandoned.ID)
	require.NoError(t, e)
	require.Equal(t, "interrupted", saved.State)
}
func TestFederation_JobsTimeoutAndRepoChangeRefuseExecution(t *testing.T) {
	fh := newFedHarness(t)
	fh.f.HaveGroup("team")
	fedJobRepo(t, fh)
	fedAllowJobs(t, fh)
	t.Cleanup(agentd.SetRemoteJobDirectRunnerForTest())
	q := proto.JobRequest{ID: proto.NewEnvelopeID(), Repo: "project", Ref: "main", Group: "team", Harness: "shell", Command: "sleep 30", Timeout: 1}
	env := fh.peer.envelope(proto.KindJobRequest, proto.Endpoint{}, q)
	env.From.Agent = ""
	fh.peer.send(env)
	job := fedWaitJob(t, fh, q.ID)
	require.Equal(t, "timeout", job.State, string(job.Result))
	rec := fedHuman(t, fh.f, http.MethodDelete, "/v1/federation/repos/project", nil)
	require.Equal(t, 200, rec.Code)
	q.ID = proto.NewEnvelopeID()
	q.Command = "echo prohibited"
	env = fh.peer.envelope(proto.KindJobRequest, proto.Endpoint{}, q)
	env.From.Agent = ""
	fh.peer.send(env)
	require.Equal(t, "refused", fedWaitJob(t, fh, q.ID).State)
}

func TestFederation_JobsManualApprovalRechecksRepositoryRevision(t *testing.T) {
	fh := newFedHarness(t)
	fh.f.HaveGroup("team")
	fedJobRepo(t, fh)
	rec := fedHuman(t, fh.f, http.MethodPost, "/v1/federation/grants", map[string]any{"peer": "bob", "slug": agentd.PermJobsRun, "scope": "group=team", "spawn_policy": map[string]any{"max_live": 2, "job_approval": "manual"}})
	require.Equal(t, 200, rec.Code)
	q := proto.JobRequest{ID: proto.NewEnvelopeID(), Repo: "project", Ref: "main", Group: "team", Harness: "shell", Command: "echo must-not-run", Timeout: 10}
	env := fh.peer.envelope(proto.KindJobRequest, proto.Endpoint{}, q)
	env.From.Agent = ""
	fh.peer.send(env)
	fedEventually(t, "manual approval pending", func() bool { j, e := db.GetFederationJob(q.ID); return e == nil && j.State == "pending" })
	rec = fedHuman(t, fh.f, http.MethodDelete, "/v1/federation/repos/project", nil)
	require.Equal(t, 200, rec.Code)
	rec = fedHuman(t, fh.f, http.MethodPost, "/v1/federation/jobs/"+q.ID+"/approve", nil)
	require.Equal(t, 200, rec.Code)
	j := fedWaitJob(t, fh, q.ID)
	require.Equal(t, "failed", j.State)
	var result proto.JobResult
	require.NoError(t, json.Unmarshal(j.Result, &result))
	require.Equal(t, "repository_changed", result.Code)
}
