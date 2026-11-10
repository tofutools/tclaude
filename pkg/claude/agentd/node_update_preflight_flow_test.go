package agentd_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/selfupdate"
	"github.com/tofutools/tclaude/pkg/testharness"
	"github.com/tofutools/tclaude/pkg/testutil"
)

func decodePlan(t *testing.T, raw []byte) selfupdate.Plan {
	t.Helper()
	var p selfupdate.Plan
	require.NoError(t, json.Unmarshal(raw, &p))
	return p
}

// The preflight dry-runs Update now under the saved method: the operator
// sees what it would run, a peer needs node.update and only an unrestricted
// one sees the custom command, and nothing runs.
func TestNodeUpdatePreflightLocalAndPeer(t *testing.T) {
	fh := newFedHarness(t)
	host := &selfUpdateHost{}
	host.install(t)
	dir := testutil.CanonicalTempDir(t)
	selfUpdateService(t, dir, []selfupdate.Binary{{Name: "tclaude", Path: fakeBinary(t, dir, "bin/tclaude", "v1.0.0"), Version: "v1.0.0", Method: "unrecognized"}})
	putSettings(t, fh.f, map[string]any{"settings": map[string]any{"method": "custom", "custom": map[string]any{"command": "deploy --token s3cret"}}}, 200)

	// The node's operator, through the CLI's route.
	rec := fedHuman(t, fh.f, "GET", "/v1/node/update/preflight", nil)
	require.Equal(t, 200, rec.Code, rec.Body.String())
	p := decodePlan(t, rec.Body.Bytes())
	require.Equal(t, "custom", p.Method)
	require.True(t, p.Ready, "%+v", p.Checks)
	require.Contains(t, strings.Join(p.Steps, "\n"), "deploy --token s3cret")
	require.NotEmpty(t, p.NotChecked)
	require.Empty(t, host.commands(), "a preflight runs nothing")

	// An agent is refused: testing the update method is the operator's.
	rec = testharness.Serve(fh.f.Mux, agentd.AsAgentPeer(testharness.JSONRequest(t, "GET", "/v1/node/update/preflight", nil), "reader"))
	require.Equal(t, 403, rec.Code, rec.Body.String())

	// A peer without node.update is refused; with it, the command is withheld.
	peer := agentd.PeerViewHandler(fh.peer.id.ID())
	rec = testharness.Serve(peer, testharness.JSONRequest(t, "GET", "/api/node/update/preflight", nil))
	require.Equal(t, 403, rec.Code, rec.Body.String())
	require.NoError(t, db.UpsertFederationPeerGrant(db.FederationPeerGrant{Peer: fh.peer.id.ID(), Slug: agentd.PermNodeUpdate}))
	rec = testharness.Serve(peer, testharness.JSONRequest(t, "GET", "/api/node/update/preflight", nil))
	require.Equal(t, 200, rec.Code, rec.Body.String())
	require.NotContains(t, rec.Body.String(), "s3cret")
	require.True(t, decodePlan(t, rec.Body.Bytes()).Ready)

	setFedTrustLevel(t, fh, "unrestricted")
	rec = testharness.Serve(peer, testharness.JSONRequest(t, "GET", "/api/node/update/preflight", nil))
	require.Equal(t, 200, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), "deploy --token s3cret", "an unrestricted peer is the node's operator")
	require.Empty(t, host.commands())
}

// A preflight reports what the apply would refuse, under the saved method.
func TestNodeUpdatePreflightReportsRefusal(t *testing.T) {
	fh := newFedHarness(t)
	(&selfUpdateHost{}).install(t)
	dir := testutil.CanonicalTempDir(t)
	selfUpdateService(t, dir, []selfupdate.Binary{{Name: "tclaude", Path: fakeBinary(t, dir, "bin/tclaude", "v1.0.0"), Version: "v1.0.0", Method: "release"}})
	putSettings(t, fh.f, map[string]any{"settings": map[string]any{"method": "go_install"}}, 200)
	rec := fedHuman(t, fh.f, "GET", "/v1/node/update/preflight", nil)
	require.Equal(t, 200, rec.Code, rec.Body.String())
	p := decodePlan(t, rec.Body.Bytes())
	require.False(t, p.Ready)
	require.Contains(t, rec.Body.String(), "go is not on PATH")
}

// Update now with the custom method runs the command even when the node is
// already on the latest release: it may track a branch.
func TestNodeUpdateNowRunsCustomCommandAtLatest(t *testing.T) {
	fh := newFedHarness(t)
	host := &selfUpdateHost{}
	host.install(t)
	dir := testutil.CanonicalTempDir(t)
	selfUpdateService(t, dir, []selfupdate.Binary{{Name: "tclaude", Path: fakeBinary(t, dir, "bin/tclaude", "v1.1.0"), Version: "v1.1.0", Method: "unrecognized"}})
	putSettings(t, fh.f, map[string]any{"settings": map[string]any{"method": "custom", "custom": map[string]any{"command": "git pull && go install ."}}}, 200)
	rec := fedHuman(t, fh.f, "POST", "/v1/node/update", map[string]any{"action": "apply"})
	require.Equal(t, 202, rec.Code, rec.Body.String())
	var job selfupdate.Job
	testharness.DecodeJSON(t, rec, &job)
	job = waitNodeUpdateJob(t, fh.f, job.ID)
	require.Equal(t, "succeeded", job.State, job.Error)
	require.Contains(t, strings.Join(job.Warnings, "\n"), "already up to date", "an unchanged binary is up to date, not a failure")
	require.Equal(t, [][]string{{"/bin/sh", "-c", "git pull && go install ."}}, host.commands())
}

// waitNodeUpdateJob reads a job through the operator's route until it
// leaves running.
func waitNodeUpdateJob(t *testing.T, f *testharness.Flow, id string) selfupdate.Job {
	t.Helper()
	var job selfupdate.Job
	fedEventuallyWithin(t, "update job to finish", 5*time.Second, func() bool {
		rec := fedHuman(t, f, "GET", "/v1/node/update/jobs/"+id, nil)
		if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &job) != nil {
			return false
		}
		return job.State != "running"
	})
	return job
}
