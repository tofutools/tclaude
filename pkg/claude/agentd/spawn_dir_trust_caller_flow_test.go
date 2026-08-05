package agentd_test

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// Flow coverage for the caller-owned-dir trust exemption (spawn_dir_trust.go).
//
// Before it, the ONLY dir an agent could pre-trust a child into was a worktree
// at the default <repo>-<branch> sibling path — a git-layout predicate that
// cannot describe a multi-repo workspace ROOT, which is not a repo and so is
// not a sibling of anything. An agent launched at such a root (deliberately,
// because its work spans repos) had no permitted `--cwd` at all: not its own
// start dir, and not the documented "leave --cwd off and inherit mine", since
// the CLI fills that in with the same path. Every child had to be spawned by
// the human.
//
// These scenarios pin the widened predicate through the real spawn path: a
// child may be pre-trusted into the caller's own start dir or any subdirectory
// of it, and nowhere else. The "nowhere else" half lives next door in
// codex_trust_dir_spawn_flow_test.go, which still asserts a 403 for an
// unrelated dir.

// haveAgentCallerInDir sets up a spawn-capable agent parent whose recorded
// launch dir is a real directory the test controls, so the caller-owned
// containment test has something to match against.
func haveAgentCallerInDir(t *testing.T, f *testharness.Flow, group, convID, dir string) {
	t.Helper()
	haveSpawnCapableSandboxParent(t, f, group, convID, harness.DefaultName, "")
	sess, err := db.FindSessionByConvID(convID)
	require.NoError(t, err)
	require.NotNil(t, sess)
	sess.Cwd = dir
	require.NoError(t, db.SaveSession(sess))
}

// Scenario: the caller asks for a child in its OWN start directory. That is the
// repro from the bug report — a workspace root holding several repo clones side
// by side, which no worktree predicate can describe. Pointing a child at it
// grants the child nothing the caller does not already hold, so it is allowed
// and the flag threads through.
func TestClaudeSpawn_TrustDirAllowedForAgentCallerOwnStartDir(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("squad")
	const parent = "parent-trst-owndir-aaaa-1111111111"

	workspace, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	haveAgentCallerInDir(t, f, "squad", parent, workspace)

	rec := agentReqProof(t, f, parent, http.MethodPost, "/v1/groups/squad/spawn", map[string]any{
		"name":      "cc-own-dir",
		"cwd":       workspace,
		"trust_dir": true,
	})
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	var resp struct {
		ConvID string `json:"conv_id"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	got, ok := f.World.SpawnTrustDir(resp.ConvID)
	require.True(t, ok, "the spawner recorded a trust-dir flag for the new conv")
	assert.True(t, got, "the caller's own start dir must thread `--trust-dir`")
}

// Scenario: a SUBDIRECTORY of the caller's start dir — the common multi-repo
// shape, where the workspace root holds the clone the child should work in.
// Containment is the predicate, so this is allowed on the same terms; a
// subdirectory is strictly narrower than the root the caller already has.
func TestClaudeSpawn_TrustDirAllowedForAgentCallerSubdir(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("squad")
	const parent = "parent-trst-subdir-bbbb-1111111111"

	workspace, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	child := filepath.Join(workspace, "service-repo")
	require.NoError(t, os.MkdirAll(child, 0o755))
	haveAgentCallerInDir(t, f, "squad", parent, workspace)

	rec := agentReqProof(t, f, parent, http.MethodPost, "/v1/groups/squad/spawn", map[string]any{
		"name":      "cc-subdir",
		"cwd":       child,
		"trust_dir": true,
	})
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	var resp struct {
		ConvID string `json:"conv_id"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	got, ok := f.World.SpawnTrustDir(resp.ConvID)
	require.True(t, ok, "the spawner recorded a trust-dir flag for the new conv")
	assert.True(t, got, "a subdirectory of the caller's start dir must thread `--trust-dir`")
}

// Scenario: the sibling of the caller's start dir is NOT covered. Containment
// is one-directional — the caller may narrow, never step out — so a peer
// directory that merely shares a parent stays refused. This is the boundary
// case the subdir scenario above could otherwise be read as widening.
func TestClaudeSpawn_TrustDirRejectedForAgentCallerSiblingDir(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("squad")
	const parent = "parent-trst-sibdir-cccc-1111111111"

	root, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	mine := filepath.Join(root, "mine")
	theirs := filepath.Join(root, "theirs")
	require.NoError(t, os.MkdirAll(mine, 0o755))
	require.NoError(t, os.MkdirAll(theirs, 0o755))
	haveAgentCallerInDir(t, f, "squad", parent, mine)

	rec := agentReqProof(t, f, parent, http.MethodPost, "/v1/groups/squad/spawn", map[string]any{
		"name":      "cc-sibling-dir",
		"cwd":       theirs,
		"trust_dir": true,
	})
	require.Equal(t, http.StatusForbidden, rec.Code, "body=%s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "trust_dir_restricted")
}

// Scenario: the recorded EDIT dir is NOT a trust root, even though tclaude
// tracks it and reports it as the agent's "current dir".
//
// agent_workdir is written from a tool-use payload by the PostToolUse hook —
// including its FAILURE arm — so an agent can nominate any path simply by
// attempting an edit its own sandbox denies. Were it a root, that attempt would
// buy the caller a pre-trust for a directory it cannot touch. The exemption is
// confined to the daemon-recorded launch dir for exactly this reason.
func TestClaudeSpawn_TrustDirRejectedForAgentCallerRecordedEditDir(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("squad")
	const parent = "parent-trst-editdir-dddd-1111111111"

	root, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	launch := filepath.Join(root, "launch")
	elsewhere := filepath.Join(root, "elsewhere")
	require.NoError(t, os.MkdirAll(launch, 0o755))
	require.NoError(t, os.MkdirAll(elsewhere, 0o755))
	haveAgentCallerInDir(t, f, "squad", parent, launch)
	// What a denied edit at `elsewhere` would leave behind.
	require.NoError(t, db.UpsertAgentWorkdir(parent, elsewhere, "", ""))

	rec := agentReqProof(t, f, parent, http.MethodPost, "/v1/groups/squad/spawn", map[string]any{
		"name":      "cc-edit-dir",
		"cwd":       elsewhere,
		"trust_dir": true,
	})
	require.Equal(t, http.StatusForbidden, rec.Code, "body=%s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "trust_dir_restricted")
}

// Scenario: a child whose own sandbox grants no cwd write (Codex `read-only`)
// normally skips the dir write-proof — there is no write access to anchor. But
// the caller-owned exemption justifies itself with a WRITE capability, so it
// must not ride on that skip: a read-only caller can by construction write
// nothing, and would otherwise seed the human's trust store off an unproven
// claim. Asking for pre-trust must therefore pull the proof in.
func TestCodexSpawn_TrustDirInCallerOwnDirDemandsProofEvenForReadOnlyChild(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("squad")
	const parent = "parent-trst-readonly-eeee-1111111111"

	workspace, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	haveSpawnCapableSandboxParent(t, f, "squad", parent, harness.CodexName, harness.SandboxManagedProfile)
	sess, err := db.FindSessionByConvID(parent)
	require.NoError(t, err)
	sess.Cwd = workspace
	require.NoError(t, db.SaveSession(sess))

	body := map[string]any{
		"name":      "cdx-readonly-trusted",
		"cwd":       workspace,
		"harness":   "codex",
		"sandbox":   harness.SandboxReadOnly,
		"trust_dir": true,
	}

	// Unanswered: the request is challenged rather than waved through.
	rec := agentReq(t, f, parent, http.MethodPost, "/v1/groups/squad/spawn", body)
	require.Equal(t, http.StatusForbidden, rec.Code, "body=%s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "write_proof_required",
		"pre-trust must demand the write-proof even for a read-only child")

	// Answered: the same request succeeds, so the proof is a gate, not a wall.
	rec = agentReqProof(t, f, parent, http.MethodPost, "/v1/groups/squad/spawn", body)
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	var resp struct {
		ConvID string `json:"conv_id"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	got, ok := f.World.SpawnTrustDir(resp.ConvID)
	require.True(t, ok)
	assert.True(t, got, "a proved caller-owned dir must still thread `--trust-dir`")
}
