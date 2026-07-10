package agentd_test

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

type cwdProofResponse struct {
	Required   bool   `json:"required"`
	Proof      string `json:"proof"`
	Cwd        string `json:"cwd"`
	MarkerPath string `json:"marker_path"`
}

func spawnProofCapableAgent(t *testing.T, f *testharness.Flow, group, conv string) {
	t.Helper()
	f.HaveMember(group, conv)
	require.NoError(t, db.GrantAgentPermission(conv, agentd.PermGroupsSpawn, "test"))
}

func issueCwdProof(t *testing.T, f *testharness.Flow, conv, cwd string) cwdProofResponse {
	t.Helper()
	req := agentd.AsAgentPeer(testharness.JSONRequest(t, http.MethodPost,
		"/v1/spawn-cwd-proof", map[string]string{"cwd": cwd}), conv)
	rec := testharness.Serve(f.Mux, req)
	require.Equalf(t, http.StatusOK, rec.Code, "issue proof: %s", rec.Body.String())
	var proof cwdProofResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &proof))
	require.True(t, proof.Required)
	require.NotEmpty(t, proof.Proof)
	return proof
}

func materialiseCwdProof(t *testing.T, proof cwdProofResponse) {
	t.Helper()
	require.Equal(t, filepath.Join(proof.Cwd, ".tclaude-spawn-cwd-proof-"+proof.Proof), proof.MarkerPath)
	f, err := os.OpenFile(proof.MarkerPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	require.NoError(t, err)
	require.NoError(t, f.Close())
	t.Cleanup(func() { _ = os.Remove(proof.MarkerPath) })
}

func TestSpawnCwdProof_AgentWithoutProofIsRefused(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")
	const lead = "lead-proof-aaaa-bbbb-cccc-111111111111"
	spawnProofCapableAgent(t, f, "alpha", lead)

	req := agentd.AsAgentPeer(testharness.JSONRequest(t, http.MethodPost,
		"/v1/groups/alpha/spawn", map[string]any{"name": "worker", "cwd": t.TempDir()}), lead)
	rec := testharness.Serve(f.Mux, req)
	require.Equalf(t, http.StatusForbidden, rec.Code, "spawn response: %s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "cwd_proof_required")
	assert.Len(t, f.ListGroupMembers("alpha"), 1, "no child should be enrolled")
}

func TestSpawnCwdProof_MarkerAllowsSpawnAndIsConsumed(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")
	const lead = "lead-proof-bbbb-cccc-dddd-222222222222"
	spawnProofCapableAgent(t, f, "alpha", lead)
	cwd := t.TempDir()
	proof := issueCwdProof(t, f, lead, cwd)
	materialiseCwdProof(t, proof)

	req := agentd.AsAgentPeer(testharness.JSONRequest(t, http.MethodPost,
		"/v1/groups/alpha/spawn", map[string]any{
			"name": "worker", "cwd": cwd, "cwd_write_proof": proof.Proof,
		}), lead)
	rec := testharness.Serve(f.Mux, req)
	require.Equalf(t, http.StatusOK, rec.Code, "spawn response: %s", rec.Body.String())
	_, err := os.Lstat(proof.MarkerPath)
	assert.True(t, os.IsNotExist(err), "daemon should remove the consumed marker; err=%v", err)
	assert.Len(t, f.ListGroupMembers("alpha"), 2, "child should be enrolled")
}

func TestSpawnCwdProof_IsBoundToCanonicalDirectory(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")
	const lead = "lead-proof-cccc-dddd-eeee-333333333333"
	spawnProofCapableAgent(t, f, "alpha", lead)
	proof := issueCwdProof(t, f, lead, t.TempDir())
	materialiseCwdProof(t, proof)

	req := agentd.AsAgentPeer(testharness.JSONRequest(t, http.MethodPost,
		"/v1/groups/alpha/spawn", map[string]any{
			"name": "worker", "cwd": t.TempDir(), "cwd_write_proof": proof.Proof,
		}), lead)
	rec := testharness.Serve(f.Mux, req)
	require.Equalf(t, http.StatusForbidden, rec.Code, "spawn response: %s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "invalid_cwd_proof")
	assert.Len(t, f.ListGroupMembers("alpha"), 1, "no child should be enrolled")
}

func TestSpawnCwdProof_HumanDoesNotNeedProof(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")
	req := agentd.AsHumanPeer(testharness.JSONRequest(t, http.MethodPost,
		"/v1/spawn-cwd-proof", map[string]string{"cwd": t.TempDir()}))
	rec := testharness.Serve(f.Mux, req)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.JSONEq(t, `{"required":false}`, rec.Body.String())
}
