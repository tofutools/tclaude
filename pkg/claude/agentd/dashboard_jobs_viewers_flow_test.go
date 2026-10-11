package agentd_test

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/federation/proto"
	"github.com/tofutools/tclaude/pkg/federation/stream"
	"github.com/tofutools/tclaude/pkg/testharness"
)

func TestDashboardFederationJobsAndRepos(t *testing.T) {
	fh := newFedHarness(t)
	fh.f.HaveGroup("team")
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://localhost:12345"))
	root := fedJobRepo(t, fh)
	fedAllowJobs(t, fh)
	t.Cleanup(agentd.SetRemoteJobDirectRunnerForTest())
	h := agentd.BuildDashboardHandlerForTest()
	call := func(method, tail string, body any, status int) []byte {
		t.Helper()
		rec := testharness.Serve(h, testharness.JSONRequest(t, method, "/api/federation/"+tail, body))
		require.Equal(t, status, rec.Code, rec.Body.String())
		require.Contains(t, rec.Header().Get("Cache-Control"), "no-store")
		return rec.Body.Bytes()
	}
	var repos struct {
		Repos []db.FederationRepo `json:"repos"`
	}
	require.NoError(t, json.Unmarshal(call("GET", "repos", nil, 200), &repos))
	require.Len(t, repos.Repos, 1)
	body := map[string]any{"name": "project", "url": (&url.URL{Scheme: "file", Path: root}).String(), "clone": root, "groups": []string{"team"}, "revision": repos.Repos[0].Revision}
	call("HEAD", "repos", body, 200)
	call("PUT", "repos/project", body, 200)
	call("PUT", "repos/project", body, 409)
	body["name"] = "second"
	delete(body, "revision")
	call("POST", "repos", body, 200)
	q := proto.JobRequest{ID: proto.NewEnvelopeID(), Repo: "project", Ref: "main", Group: "team", Harness: "shell", Command: "cat hello", Timeout: 10}
	env := fh.peer.envelope(proto.KindJobRequest, proto.Endpoint{}, q)
	env.From.Agent = ""
	fh.peer.send(env)
	job := fedWaitJob(t, fh, q.ID)
	require.Equal(t, "completed", job.State, string(job.Result))
	call("GET", "jobs", nil, 200)
	call("GET", "jobs/"+q.ID, nil, 200)
	require.Contains(t, string(call("GET", "jobs/"+q.ID+"/logs", nil, 200)), "from git")
	call("GET", "jobs/"+q.ID+"/follow", nil, 409)
	call("POST", "jobs/"+q.ID+"/approve", nil, 409)
	call("POST", "jobs/"+q.ID+"/acknowledge-stopped", map[string]bool{}, 400)
	// Outgoing job submission and cancellation preserve the durable CLI identity.
	fh.peer.send(fh.peer.envelope(proto.KindCatalog, proto.Endpoint{}, proto.CatalogPayload{Groups: []proto.CatalogGroup{{Name: "team", Caps: []string{proto.CapJobs}}}}))
	fedEventually(t, "job catalog", func() bool {
		raw, _, _ := db.GetFederationCatalog(fh.peer.id.ID())
		return strings.Contains(raw, `"jobs"`)
	})
	var sent struct {
		Job db.FederationJob `json:"job"`
	}
	require.NoError(t, json.Unmarshal(call("POST", "jobs", map[string]any{"peer": "bob", "repo": "project", "ref": "main", "group": "team", "harness": "shell", "command": "echo remote", "timeout_seconds": 10}, 200), &sent))
	require.NotEmpty(t, sent.Job.ID)
	call("POST", "jobs/"+sent.Job.ID+"/retry", nil, 200)
	call("POST", "jobs/"+sent.Job.ID+"/cancel", nil, 200)
	call("DELETE", "repos/second", nil, 200)
	peer, err := db.GetFederationPeer(fh.peer.id.ID())
	require.NoError(t, err)
	peer.TrustLevel = db.FederationTrustUnrestricted
	require.NoError(t, db.TrustFederationPeer(*peer))
	for _, route := range []struct{ method, tail string }{{"POST", "jobs"}, {"GET", "jobs"}, {"GET", "jobs/" + q.ID}, {"GET", "jobs/" + q.ID + "/logs"}, {"GET", "jobs/" + q.ID + "/follow"}, {"POST", "jobs/" + q.ID + "/cancel"}, {"POST", "jobs/" + q.ID + "/approve"}, {"POST", "jobs/" + q.ID + "/retry"}, {"POST", "jobs/" + q.ID + "/acknowledge-stopped"}, {"GET", "repos"}, {"POST", "repos"}, {"PUT", "repos/project"}, {"DELETE", "repos/project"}} {
		rec := testharness.Serve(agentd.PeerViewHandler(peer.InstanceID), testharness.JSONRequest(t, route.method, "/api/federation/"+route.tail, nil))
		require.Equal(t, 403, rec.Code, rec.Body.String())
	}
	raw := http.NewServeMux()
	agentd.RegisterDashboardRoutesForTest(raw)
	rec := testharness.Serve(raw, testharness.JSONRequest(t, "GET", "/api/federation/jobs", nil))
	require.Equal(t, 403, rec.Code, rec.Body.String())
}

func TestDashboardFederationRepositoryGroupNames(t *testing.T) {
	fh := newFedHarness(t)
	for _, name := range []string{"team", "alpha", "zeta"} {
		fh.f.HaveGroup(name)
	}
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://localhost:12345"))
	root := fedJobRepo(t, fh)
	h := agentd.BuildDashboardHandlerForTest()
	type repoResponse struct {
		db.FederationRepo
		GroupNames []string `json:"group_names"`
	}
	body := map[string]any{"name": "ordered", "url": (&url.URL{Scheme: "file", Path: root}).String(), "clone": root, "groups": []string{"zeta", "alpha", "zeta"}}
	rec := testharness.Serve(h, testharness.JSONRequest(t, "POST", "/api/federation/repos", body))
	require.Equal(t, 200, rec.Code, rec.Body.String())
	var created repoResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &created))
	require.Equal(t, []string{"zeta", "alpha"}, created.GroupNames)
	require.Len(t, created.Definition.Groups, 2)
	body["revision"] = created.Revision
	rec = testharness.Serve(h, testharness.JSONRequest(t, "PUT", "/api/federation/repos/ordered", body))
	require.Equal(t, 200, rec.Code, rec.Body.String())
	var updated repoResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &updated))
	require.Equal(t, created.GroupNames, updated.GroupNames)

	read := func(cli bool, want []string) {
		t.Helper()
		var raw []byte
		if cli {
			rec := fedHuman(t, fh.f, "GET", "/v1/federation/repos", nil)
			require.Equal(t, 200, rec.Code, rec.Body.String())
			raw = rec.Body.Bytes()
		} else {
			rec := testharness.Serve(h, testharness.JSONRequest(t, "GET", "/api/federation/repos", nil))
			require.Equal(t, 200, rec.Code, rec.Body.String())
			raw = rec.Body.Bytes()
		}
		var result struct {
			Repos []repoResponse `json:"repos"`
		}
		require.NoError(t, json.Unmarshal(raw, &result))
		for _, row := range result.Repos {
			if row.Name == "ordered" {
				require.Equal(t, want, row.GroupNames)
				require.Equal(t, created.Definition.Groups, row.Definition.Groups, "names must not change the stored authority")
				return
			}
		}
		t.Fatal("repository missing")
	}
	read(false, []string{"zeta", "alpha"})
	rec = testharness.Serve(h, testharness.JSONRequest(t, "POST", "/api/groups/zeta/rename", map[string]string{"new_name": "renamed"}))
	require.Equal(t, 200, rec.Code, rec.Body.String())
	read(false, []string{"renamed", "alpha"})
	rec = testharness.Serve(h, testharness.JSONRequest(t, "DELETE", "/api/groups/alpha", nil))
	require.Equal(t, 204, rec.Code, rec.Body.String())
	read(false, []string{"renamed"})
	read(true, []string{"renamed"})
	rec = testharness.Serve(h, testharness.JSONRequest(t, "DELETE", "/api/groups/renamed", nil))
	require.Equal(t, 204, rec.Code, rec.Body.String())
	read(false, []string{})
}

func TestDashboardFederationIncomingViewerKick(t *testing.T) {
	fh := newFedHarness(t)
	f, p := fh.f, fh.peer
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://localhost:12345"))
	const conv = "dashboard-viewed-agent"
	f.HaveGroup("team")
	f.HaveConvWithTitle(conv, "viewed-agent")
	f.HaveMember("team", conv)
	f.HaveAliveSession(conv, "view-runtime", "tclaude-view-runtime", f.TestCwd("work"))
	aid, err := db.AgentIDForConv(conv)
	require.NoError(t, err)
	original := clcommon.Default
	mock := &terminalTmux{Tmux: original, options: map[string]string{"pane-border-format": "original"}, pane: "%1", windows: "1", version: "tmux 3.4"}
	clcommon.Default = mock
	t.Cleanup(func() { agentd.ResetFederationForTest(); clcommon.Default = original })
	incarnation := terminalCatalogIncarnation(t, fh, aid)
	rec := fedHuman(t, f, "POST", "/v1/federation/grants", map[string]any{"peer": "bob", "slug": agentd.PermSessionsWatch, "scope": "group=team"})
	require.Equal(t, 200, rec.Code, rec.Body.String())
	kp, err := stream.NewKeyPair()
	require.NoError(t, err)
	opening := proto.SessionOpenPayload{Agent: aid, Session: "view-runtime", Incarnation: incarnation, Group: "team", Stream: proto.NewEnvelopeID(), Key: kp.Pub, ReadOnly: true, Cols: 80, Rows: 24}
	p.send(p.envelope(proto.KindSessionOpen, proto.Endpoint{}, opening))
	answer := terminalAnswer(t, p, opening.Stream)
	require.True(t, answer.OK, answer.Reason)
	conn := fedPeerStream(t, p, opening.Stream, kp, answer.Key, true)
	defer func() { _ = conn.Close() }()
	terminalRead(t, conn)
	h := agentd.BuildDashboardHandlerForTest()
	rec = testharness.Serve(h, testharness.JSONRequest(t, "GET", "/api/federation/viewers?session="+aid, nil))
	require.Equal(t, 200, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), opening.Stream)
	require.Contains(t, rec.Body.String(), `"read_only":true`)
	rec = testharness.Serve(h, testharness.JSONRequest(t, "GET", "/api/federation/viewers?session=other", nil))
	require.Equal(t, 200, rec.Code)
	require.JSONEq(t, `[]`, rec.Body.String())
	rec = testharness.Serve(h, testharness.JSONRequest(t, "POST", "/api/federation/viewers/"+opening.Stream+"/kick", nil))
	require.Equal(t, 200, rec.Code, rec.Body.String())
	fedEventually(t, "viewer removed", func() bool {
		rec = testharness.Serve(h, testharness.JSONRequest(t, "GET", "/api/federation/viewers", nil))
		return !strings.Contains(rec.Body.String(), opening.Stream)
	})
	peer, err := db.GetFederationPeer(p.id.ID())
	require.NoError(t, err)
	peer.TrustLevel = db.FederationTrustUnrestricted
	require.NoError(t, db.TrustFederationPeer(*peer))
	for _, route := range []struct{ method, tail string }{{"GET", "viewers"}, {"POST", "viewers/" + opening.Stream + "/kick"}} {
		rec = testharness.Serve(agentd.PeerViewHandler(peer.InstanceID), testharness.JSONRequest(t, route.method, "/api/federation/"+route.tail, nil))
		require.Equal(t, 403, rec.Code, rec.Body.String())
	}
	raw := http.NewServeMux()
	agentd.RegisterDashboardRoutesForTest(raw)
	rec = testharness.Serve(raw, testharness.JSONRequest(t, "GET", "/api/federation/viewers", nil))
	require.Equal(t, 403, rec.Code, rec.Body.String())
}
