package agentd_test

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/noderun"
	"github.com/tofutools/tclaude/pkg/testharness"
)

func TestNodeRunTwoLocksOwnershipAndRevocation(t *testing.T) {
	fh := newFedHarness(t)
	started := make(chan string, 2)
	cleanup, err := agentd.SetNodeRunExecutorForTest(func(ctx context.Context, path, _ string, _ int64) noderun.Result {
		raw, err := os.ReadFile(path)
		if err != nil {
			return noderun.Result{ExitCode: 125, Error: err.Error()}
		}
		started <- string(raw)
		if string(raw) == "local fixture" {
			return noderun.Result{Stdout: "local output"}
		}
		<-ctx.Done()
		return noderun.Result{ExitCode: 130, Stdout: "secret output"}
	})
	require.NoError(t, err)
	t.Cleanup(cleanup)
	h := agentd.PeerViewHandler(fh.peer.id.ID())
	payload := noderun.Request{Script: "remote fixture secret", TimeoutSeconds: 30}
	post := func() int {
		return testharness.Serve(h, testharness.JSONRequest(t, "POST", "/api/node/run", payload)).Code
	}
	require.Equal(t, 403, post())
	require.NoError(t, db.UpsertFederationPeerGrant(db.FederationPeerGrant{Peer: fh.peer.id.ID(), Slug: agentd.PermNodeExec}))
	require.Equal(t, 403, post(), "grant alone is insufficient")
	setFedTrustLevel(t, fh, "unrestricted")
	require.Equal(t, 403, post(), "unrestricted still requires the receiver switch")
	for _, method := range []string{"GET", "PUT"} {
		rec := testharness.Serve(h, testharness.JSONRequest(t, method, "/api/node/run/settings", map[string]any{"accept_remote_scripts": true}))
		require.Equal(t, 403, rec.Code, "settings are local-only")
	}
	// Local runs need neither remote lock and retain their full output.
	rec := fedHuman(t, fh.f, "POST", "/v1/node/run", noderun.Request{Script: "local fixture", TimeoutSeconds: 30})
	require.Equal(t, 202, rec.Code, rec.Body.String())
	var local noderun.Job
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &local))
	require.Eventually(t, func() bool {
		rec = fedHuman(t, fh.f, "GET", "/v1/node/run/jobs/"+local.ID, nil)
		return rec.Code == 200 && json.Unmarshal(rec.Body.Bytes(), &local) == nil && local.State == "completed"
	}, 3*time.Second, 10*time.Millisecond)
	rec = fedHuman(t, fh.f, "GET", "/v1/node/run/jobs/"+local.ID+"/logs?stream=stdout", nil)
	require.Equal(t, 200, rec.Code)
	var chunk noderun.LogChunk
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &chunk))
	require.Equal(t, "local output", string(chunk.Data))
	rec = fedHuman(t, fh.f, "PUT", "/v1/node/run/settings", map[string]any{"accept_remote_scripts": true})
	require.Equal(t, 200, rec.Code, rec.Body.String())
	rec = testharness.Serve(h, testharness.JSONRequest(t, "GET", "/api/node/run/jobs/"+local.ID, nil))
	require.Equal(t, 404, rec.Code, "peer cannot read a local operator's job")
	rec = testharness.Serve(h, testharness.JSONRequest(t, "POST", "/api/node/run", payload))
	require.Equal(t, 202, rec.Code, rec.Body.String())
	var remote noderun.Job
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &remote))
	require.Equal(t, fh.peer.id.ID(), remote.Peer)
	// Wait for both executions before revoking the independent receiver lock.
	for range 2 {
		select {
		case <-started:
		case <-time.After(3 * time.Second):
			t.Fatal("execution not started")
		}
	}
	rec = fedHuman(t, fh.f, "PUT", "/v1/node/run/settings", map[string]any{"accept_remote_scripts": false})
	require.Equal(t, 200, rec.Code)
	require.Eventually(t, func() bool {
		rec = fedHuman(t, fh.f, "GET", "/v1/node/run/jobs/"+remote.ID, nil)
		return rec.Code == 200 && json.Unmarshal(rec.Body.Bytes(), &remote) == nil && remote.State == "canceled"
	}, 3*time.Second, 10*time.Millisecond)
	require.Equal(t, 130, remote.ExitCode)
	rec = testharness.Serve(h, testharness.JSONRequest(t, "GET", "/api/node/run/jobs/"+remote.ID, nil))
	require.Equal(t, 403, rec.Code)
	rec = testharness.Serve(fh.f.Mux, agentd.AsAgentPeer(testharness.JSONRequest(t, "POST", "/v1/node/run", payload), "reader"))
	require.Equal(t, 403, rec.Code, "local agents cannot execute operator scripts")
	rec = fedHuman(t, fh.f, "GET", "/v1/federation/audit", nil)
	require.Equal(t, 200, rec.Code)
	require.Contains(t, rec.Body.String(), "federation.node.exec")
	entries, err := db.ListAuditLog(db.AuditLogFilter{Verb: "federation.node.exec"})
	require.NoError(t, err)
	raw, err := json.Marshal(entries)
	require.NoError(t, err)
	require.Contains(t, string(raw), remote.ScriptSHA256)
	require.NotContains(t, string(raw), "remote fixture secret")
	require.NotContains(t, string(raw), "secret output")
	require.NotContains(t, rec.Body.String(), "remote fixture secret")
	require.NotContains(t, rec.Body.String(), "secret output")
}
