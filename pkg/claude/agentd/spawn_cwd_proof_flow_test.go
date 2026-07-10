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
	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

type cwdProofResponse struct {
	Required   bool   `json:"required"`
	Proof      string `json:"proof"`
	Cwd        string `json:"cwd"`
	MarkerPath string `json:"marker_path"`
}

type cwdSwapSpawner struct {
	inner     agentd.Spawner
	target    string
	moved     string
	forbidden string
}

func (s *cwdSwapSpawner) SpawnNew(args clcommon.SpawnArgs) error {
	if err := os.Rename(s.target, s.moved); err != nil {
		return err
	}
	if err := os.Symlink(s.forbidden, s.target); err != nil {
		return err
	}
	return s.inner.SpawnNew(args)
}

func (s *cwdSwapSpawner) SpawnResume(args clcommon.SpawnArgs) error {
	return s.inner.SpawnResume(args)
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
	require.Equal(t, filepath.Join(proof.Cwd, clcommon.SpawnCwdProofPrefix+proof.Proof), proof.MarkerPath)
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

func TestSpawnCwdProof_PathSwapBeforePaneLaunchIsRefused(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")
	const lead = "lead-proof-dddd-eeee-ffff-444444444444"
	spawnProofCapableAgent(t, f, "alpha", lead)

	root := t.TempDir()
	target := filepath.Join(root, "target")
	moved := filepath.Join(root, "proved-target")
	forbidden := filepath.Join(root, "forbidden")
	require.NoError(t, os.Mkdir(target, 0o700))
	require.NoError(t, os.Mkdir(forbidden, 0o700))
	proof := issueCwdProof(t, f, lead, target)
	materialiseCwdProof(t, proof)

	inner := agentd.Spawn
	agentd.Spawn = &cwdSwapSpawner{inner: inner, target: target, moved: moved, forbidden: forbidden}
	t.Cleanup(func() { agentd.Spawn = inner })

	req := agentd.AsAgentPeer(testharness.JSONRequest(t, http.MethodPost,
		"/v1/groups/alpha/spawn", map[string]any{
			"name": "worker", "cwd": target, "cwd_write_proof": proof.Proof,
		}), lead)
	rec := testharness.Serve(f.Mux, req)
	require.Equalf(t, http.StatusInternalServerError, rec.Code, "spawn response: %s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "failed to launch")
	assert.Len(t, f.ListGroupMembers("alpha"), 1, "swapped cwd must not launch or enroll a child")

	// Restore the temp tree so cleanup can remove it and so the proof marker is
	// visible at its original path for materialiseCwdProof's cleanup.
	require.NoError(t, os.Remove(target))
	require.NoError(t, os.Rename(moved, target))
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

func TestSpawnCwdProof_AgentCannotEditGlobalCodexTrust(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")
	const lead = "lead-proof-eeee-ffff-aaaa-555555555555"
	spawnProofCapableAgent(t, f, "alpha", lead)

	resp := f.AsAgent(lead).SpawnWith("alpha", map[string]any{
		"name": "worker", "cwd": t.TempDir(), "harness": "codex", "trust_dir": true,
	})
	require.Equalf(t, http.StatusForbidden, resp.Code, "spawn response: %s", resp.Raw)
	assert.Contains(t, string(resp.Raw), "trust_dir_restricted")
	assert.Len(t, f.ListGroupMembers("alpha"), 1, "trust-dir refusal must not enroll a child")
}
