package agentd_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// These tests exercise the spawn-dir write-proof (spawn_dir_proof.go) at the
// raw wire surface — agentReq/humanReq bypass the Flow.do harness that
// answers challenges transparently, so the 403 challenge/verify handshake
// itself is visible. The transparent path (what a CLI user experiences) is
// covered by TestSpawnDirProof_HarnessAnswersTransparently and by every
// pre-existing agent-caller spawn flow test, which now ride the same dance.

// writeProofChallengeResp mirrors the daemon's 403 challenge body.
type writeProofChallengeResp struct {
	Code       string `json:"code"`
	Error      string `json:"error"`
	WriteProof struct {
		Token    string   `json:"token"`
		Filename string   `json:"filename"`
		Dirs     []string `json:"dirs"`
	} `json:"write_proof"`
}

// codexCopyCompatSpawner lets copy-path tests use the existing Claude
// conversation-copy fixture while still asserting the Codex launch args. The
// production args are recorded unchanged; only the simulator delegate sees a
// Claude harness so it can reopen the copied Claude-format fixture.
type codexCopyCompatSpawner struct {
	inner agentd.Spawner
}

func (s codexCopyCompatSpawner) SpawnNew(args clcommon.SpawnArgs) error {
	return s.inner.SpawnNew(args)
}

func (s codexCopyCompatSpawner) SpawnResume(args clcommon.SpawnArgs) error {
	delegated := args
	delegated.Harness = harness.DefaultName
	delegated.Sandbox = harness.ClaudeSandboxInherit
	delegated.Approval = ""
	return s.inner.SpawnResume(delegated)
}

func installCodexCopyCompatSpawner(t *testing.T) {
	t.Helper()
	prev := agentd.Spawn
	agentd.Spawn = codexCopyCompatSpawner{inner: prev}
	t.Cleanup(func() { agentd.Spawn = prev })
}

func markSessionAsCodex(t *testing.T, label string) {
	t.Helper()
	row, err := db.LoadSession(label)
	require.NoError(t, err)
	require.NotNil(t, row)
	row.Harness = harness.CodexName
	require.NoError(t, db.SaveSession(row))
}

// decodeWriteProofChallenge asserts rec is a write_proof_required 403 and
// returns the parsed challenge.
func decodeWriteProofChallenge(t *testing.T, rec *httptest.ResponseRecorder) writeProofChallengeResp {
	t.Helper()
	require.Equalf(t, http.StatusForbidden, rec.Code, "expected challenge; body=%s", rec.Body.String())
	var ch writeProofChallengeResp
	testharness.DecodeJSON(t, rec, &ch)
	require.Equal(t, "write_proof_required", ch.Code, "body=%s", rec.Body.String())
	require.NotEmpty(t, ch.WriteProof.Token)
	require.Equal(t, ".tclaude-write-proof-"+ch.WriteProof.Token, ch.WriteProof.Filename)
	require.NotEmpty(t, ch.WriteProof.Dirs)
	return ch
}

// answerChallenge creates the proof file in every challenged dir — what the
// CLI does from inside the calling agent's sandbox.
func answerChallenge(t *testing.T, ch writeProofChallengeResp) {
	t.Helper()
	for _, dir := range ch.WriteProof.Dirs {
		p := filepath.Join(dir, ch.WriteProof.Filename)
		require.NoError(t, os.WriteFile(p, nil, 0o600))
		t.Cleanup(func() { _ = os.Remove(p) })
	}
}

// agentReqProof issues an agent-caller request and, if the daemon answers
// with a dir write-proof challenge, creates the proof files and retries once
// with the token folded into the body — the raw-wire equivalent of the CLI's
// transparent handling. body must be a map[string]any. Use it for agent
// callers on the proof-gated surfaces (spawn / clone / template instantiate /
// deploy / reinforce) when the test cares about what happens AFTER the proof,
// not the challenge itself.
func agentReqProof(t *testing.T, f *testharness.Flow, convID, method, path string, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	rec := agentReq(t, f, convID, method, path, body)
	if rec.Code != http.StatusForbidden {
		return rec
	}
	var ch writeProofChallengeResp
	if json.Unmarshal(rec.Body.Bytes(), &ch) != nil || ch.Code != "write_proof_required" {
		return rec
	}
	answerChallenge(t, ch)
	retry := make(map[string]any, len(body)+1)
	for k, v := range body {
		retry[k] = v
	}
	retry["write_proof_token"] = ch.WriteProof.Token
	return agentReq(t, f, convID, method, path, retry)
}

// Scenario: a sandboxed agent spawns a worker into a directory of its
// choosing. The daemon first refuses with a write-proof challenge; after the
// agent creates the token-named file there, the identical request with the
// token succeeds, the spawned session lands in the (symlink-resolved)
// challenged dir, and the daemon has consumed the proof file.
func TestSpawnDirProof_ChallengeThenVerifiedSpawn(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")
	const parent = "parent-wrp1-aaaa-bbbb-cccc-111111111111"
	haveSpawnCapableSandboxParent(t, f, "alpha", parent, harness.DefaultName, harness.ClaudeSandboxInherit)

	dir := t.TempDir()
	body := map[string]any{"name": "worker", "cwd": dir}

	rec := agentReq(t, f, parent, http.MethodPost, "/v1/groups/alpha/spawn", body)
	ch := decodeWriteProofChallenge(t, rec)
	resolvedDir, err := filepath.EvalSymlinks(dir)
	require.NoError(t, err)
	require.Equal(t, []string{resolvedDir}, ch.WriteProof.Dirs,
		"challenge must name the symlink-resolved spawn dir")

	answerChallenge(t, ch)
	body["write_proof_token"] = ch.WriteProof.Token
	rec = agentReq(t, f, parent, http.MethodPost, "/v1/groups/alpha/spawn", body)
	require.Equalf(t, http.StatusOK, rec.Code, "proved retry must spawn; body=%s", rec.Body.String())

	var resp struct {
		ConvID string `json:"conv_id"`
	}
	testharness.DecodeJSON(t, rec, &resp)
	require.NotEmpty(t, resp.ConvID)
	sess, err := db.FindSessionByConvID(resp.ConvID)
	require.NoError(t, err)
	require.NotNil(t, sess)
	assert.Equal(t, resolvedDir, sess.Cwd, "spawn must be pinned to the verified resolved dir")

	_, statErr := os.Lstat(filepath.Join(resolvedDir, ch.WriteProof.Filename))
	assert.True(t, os.IsNotExist(statErr), "daemon must consume the proof file")
}

// Scenario: the agent obtains a challenge but cannot (or does not) create
// the proof file — the exact posture of a sandboxed agent aiming a child at
// a directory outside its own write set. The retry is refused.
func TestSpawnDirProof_MissingProofRefused(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")
	const parent = "parent-wrp2-aaaa-bbbb-cccc-111111111111"
	haveSpawnCapableSandboxParent(t, f, "alpha", parent, harness.DefaultName, harness.ClaudeSandboxInherit)

	dir := t.TempDir()
	body := map[string]any{"name": "worker", "cwd": dir}
	ch := decodeWriteProofChallenge(t,
		agentReq(t, f, parent, http.MethodPost, "/v1/groups/alpha/spawn", body))

	body["write_proof_token"] = ch.WriteProof.Token
	rec := agentReq(t, f, parent, http.MethodPost, "/v1/groups/alpha/spawn", body)
	require.Equalf(t, http.StatusForbidden, rec.Code, "body=%s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "write_proof_failed")
	assert.Equal(t, 1, memberCount(t, "alpha"),
		"refused spawn must not enroll anyone beyond the pre-existing parent")
}

// Scenario: agent B tries to ride agent A's token (with the proof file in
// place). The token is bound to the conv it was minted for, so B gets a
// fresh challenge of its own instead of a spawn.
func TestSpawnDirProof_TokenBoundToCaller(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")
	const parentA = "parent-wrpa-aaaa-bbbb-cccc-111111111111"
	const parentB = "parent-wrpb-aaaa-bbbb-cccc-111111111111"
	haveSpawnCapableSandboxParent(t, f, "alpha", parentA, harness.DefaultName, harness.ClaudeSandboxInherit)
	haveSpawnCapableSandboxParent(t, f, "alpha", parentB, harness.DefaultName, harness.ClaudeSandboxInherit)

	dir := t.TempDir()
	body := map[string]any{"name": "worker", "cwd": dir}
	ch := decodeWriteProofChallenge(t,
		agentReq(t, f, parentA, http.MethodPost, "/v1/groups/alpha/spawn", body))
	answerChallenge(t, ch)

	body["write_proof_token"] = ch.WriteProof.Token
	rec := agentReq(t, f, parentB, http.MethodPost, "/v1/groups/alpha/spawn", body)
	fresh := decodeWriteProofChallenge(t, rec)
	assert.NotEqual(t, ch.WriteProof.Token, fresh.WriteProof.Token,
		"a foreign token must yield a fresh challenge, not a spawn")
}

// Scenario: a verified token is replayed for a second spawn. Tokens are
// single-use — the replay gets a fresh challenge.
func TestSpawnDirProof_TokenSingleUse(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")
	const parent = "parent-wrp3-aaaa-bbbb-cccc-111111111111"
	haveSpawnCapableSandboxParent(t, f, "alpha", parent, harness.DefaultName, harness.ClaudeSandboxInherit)

	dir := t.TempDir()
	body := map[string]any{"name": "worker", "cwd": dir}
	ch := decodeWriteProofChallenge(t,
		agentReq(t, f, parent, http.MethodPost, "/v1/groups/alpha/spawn", body))
	answerChallenge(t, ch)
	body["write_proof_token"] = ch.WriteProof.Token
	rec := agentReq(t, f, parent, http.MethodPost, "/v1/groups/alpha/spawn", body)
	require.Equalf(t, http.StatusOK, rec.Code, "first proved spawn; body=%s", rec.Body.String())

	answerChallenge(t, ch) // re-create the file; the token must still be dead
	rec = agentReq(t, f, parent, http.MethodPost, "/v1/groups/alpha/spawn", body)
	decodeWriteProofChallenge(t, rec)
}

// Scenario: the challenged path is swapped from one symlink target to
// another between challenge and retry. Verification re-resolves the request
// dirs and compares them to the challenged set, so the swap yields a fresh
// challenge for the NEW target — never a spawn pinned by a stale proof.
func TestSpawnDirProof_SymlinkSwapRefused(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")
	const parent = "parent-wrp4-aaaa-bbbb-cccc-111111111111"
	haveSpawnCapableSandboxParent(t, f, "alpha", parent, harness.DefaultName, harness.ClaudeSandboxInherit)

	base := t.TempDir()
	dirA := filepath.Join(base, "a")
	dirB := filepath.Join(base, "b")
	link := filepath.Join(base, "dir")
	require.NoError(t, os.Mkdir(dirA, 0o755))
	require.NoError(t, os.Mkdir(dirB, 0o755))
	require.NoError(t, os.Symlink(dirA, link))

	body := map[string]any{"name": "worker", "cwd": link}
	ch := decodeWriteProofChallenge(t,
		agentReq(t, f, parent, http.MethodPost, "/v1/groups/alpha/spawn", body))
	resolvedA, err := filepath.EvalSymlinks(dirA)
	require.NoError(t, err)
	require.Equal(t, []string{resolvedA}, ch.WriteProof.Dirs)

	// Swap the link to B and place the proof where the attacker CAN write.
	require.NoError(t, os.Remove(link))
	require.NoError(t, os.Symlink(dirB, link))
	require.NoError(t, os.WriteFile(filepath.Join(dirB, ch.WriteProof.Filename), nil, 0o600))

	body["write_proof_token"] = ch.WriteProof.Token
	rec := agentReq(t, f, parent, http.MethodPost, "/v1/groups/alpha/spawn", body)
	fresh := decodeWriteProofChallenge(t, rec)
	resolvedB, err := filepath.EvalSymlinks(dirB)
	require.NoError(t, err)
	assert.Equal(t, []string{resolvedB}, fresh.WriteProof.Dirs,
		"the swap must surface as a fresh challenge for the new target")
}

// Scenario: a spawn that names a worktree dir alongside the cwd must prove
// both — the worktree is where the welcome points the child's code work.
func TestSpawnDirProof_WorktreeDirAlsoChallenged(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")
	const parent = "parent-wrp5-aaaa-bbbb-cccc-111111111111"
	haveSpawnCapableSandboxParent(t, f, "alpha", parent, harness.DefaultName, harness.ClaudeSandboxInherit)

	cwd, wt := t.TempDir(), t.TempDir()
	body := map[string]any{"name": "worker", "cwd": cwd, "worktree_path": wt, "worktree_branch": "feat"}
	ch := decodeWriteProofChallenge(t,
		agentReq(t, f, parent, http.MethodPost, "/v1/groups/alpha/spawn", body))
	rc, err := filepath.EvalSymlinks(cwd)
	require.NoError(t, err)
	rw, err := filepath.EvalSymlinks(wt)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{rc, rw}, ch.WriteProof.Dirs)

	answerChallenge(t, ch)
	body["write_proof_token"] = ch.WriteProof.Token
	rec := agentReq(t, f, parent, http.MethodPost, "/v1/groups/alpha/spawn", body)
	require.Equalf(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
}

// Scenario: the human spawns into an arbitrary dir — no challenge, exactly
// as before. Humans are the trust root.
func TestSpawnDirProof_HumanExempt(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")
	rec := humanReq(t, f, http.MethodPost, "/v1/groups/alpha/spawn",
		map[string]any{"name": "worker", "cwd": t.TempDir()})
	require.Equalf(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
}

// Scenario: a parent whose own launch sandbox is fully open (Claude `off`)
// can already write anywhere — no challenge.
func TestSpawnDirProof_UnsandboxedParentExempt(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")
	const parent = "parent-wrp6-aaaa-bbbb-cccc-111111111111"
	haveSpawnCapableSandboxParent(t, f, "alpha", parent, harness.DefaultName, harness.ClaudeSandboxOff)

	rec := agentReq(t, f, parent, http.MethodPost, "/v1/groups/alpha/spawn",
		map[string]any{"name": "worker", "cwd": t.TempDir()})
	require.Equalf(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
}

// Scenario: the child is Codex read-only — it gets no write access at its
// cwd, so there is no write grant to prove. No challenge.
func TestSpawnDirProof_ReadOnlyCodexChildExempt(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")
	const parent = "parent-wrp7-aaaa-bbbb-cccc-111111111111"
	haveSpawnCapableSandboxParent(t, f, "alpha", parent, harness.CodexName, harness.SandboxManagedProfile)

	rec := agentReq(t, f, parent, http.MethodPost, "/v1/groups/alpha/spawn",
		map[string]any{"name": "worker", "cwd": t.TempDir(),
			"harness": harness.CodexName, "sandbox": harness.SandboxReadOnly})
	require.Equalf(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
}

func TestSpawnDirProof_CodexManagedSpawnProvesAndPinsGitCommonDir(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")
	const parent = "parent-wrgc-aaaa-bbbb-cccc-111111111111"
	haveSpawnCapableSandboxParent(t, f, "alpha", parent, harness.DefaultName, harness.ClaudeSandboxInherit)
	repo, repoParent := initRepoOnMain(t)
	commonDir, err := harness.CodexGitCommonDir(repo)
	require.NoError(t, err)
	require.NotEmpty(t, commonDir)

	body := map[string]any{"name": "codex-worker", "cwd": repo, "harness": harness.CodexName}
	ch := decodeWriteProofChallenge(t,
		agentReq(t, f, parent, http.MethodPost, "/v1/groups/alpha/spawn", body))
	assert.ElementsMatch(t, []string{repoParent, repo, commonDir}, ch.WriteProof.Dirs)
	answerChallenge(t, ch)
	body["write_proof_token"] = ch.WriteProof.Token
	rec := agentReq(t, f, parent, http.MethodPost, "/v1/groups/alpha/spawn", body)
	require.Equalf(t, http.StatusOK, rec.Code, "proved codex spawn; body=%s", rec.Body.String())

	var resp struct {
		ConvID string `json:"conv_id"`
	}
	testharness.DecodeJSON(t, rec, &resp)
	require.NotEmpty(t, resp.ConvID)
	got, ok := f.World.SpawnCodexGitCommonDir(resp.ConvID)
	require.True(t, ok)
	assert.Equal(t, commonDir, got, "child launch must use the daemon-pinned common dir")
	pinned, ok := f.World.SpawnCodexGitCommonDirPinned(resp.ConvID)
	require.True(t, ok)
	assert.True(t, pinned, "managed Codex spawn must carry pin-presence")
	assertNoDirWriteProofMarkers(t, repo)
	assertNoDirWriteProofMarkers(t, commonDir)
	assertNoDirWriteProofMarkers(t, repoParent)
}

func TestSpawnDirProof_ClaudeSpawnProvesAndPinsWorktreeWriteDirs(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")
	const parent = "parent-wrcc-aaaa-bbbb-cccc-111111111111"
	haveSpawnCapableSandboxParent(t, f, "alpha", parent, harness.DefaultName, harness.ClaudeSandboxInherit)
	repo, repoParent := initRepoOnMain(t)
	commonDir, err := harness.GitCommonDir(repo)
	require.NoError(t, err)

	body := map[string]any{
		"name": "claude-worker", "cwd": repo,
		"harness": harness.DefaultName, "sandbox": harness.ClaudeSandboxOn,
	}
	ch := decodeWriteProofChallenge(t,
		agentReq(t, f, parent, http.MethodPost, "/v1/groups/alpha/spawn", body))
	assert.ElementsMatch(t, []string{repoParent, repo, commonDir}, ch.WriteProof.Dirs)
	answerChallenge(t, ch)
	body["write_proof_token"] = ch.WriteProof.Token
	rec := agentReq(t, f, parent, http.MethodPost, "/v1/groups/alpha/spawn", body)
	require.Equalf(t, http.StatusOK, rec.Code, "proved claude spawn; body=%s", rec.Body.String())

	var resp struct {
		ConvID string `json:"conv_id"`
	}
	testharness.DecodeJSON(t, rec, &resp)
	got, ok := f.World.SpawnCodexGitCommonDir(resp.ConvID)
	require.True(t, ok)
	assert.Equal(t, commonDir, got, "Claude launch must receive the same pinned repository layout")
	pinned, ok := f.World.SpawnCodexGitCommonDirPinned(resp.ConvID)
	require.True(t, ok)
	assert.True(t, pinned)
}

func TestSpawnDirProof_CodexManagedSpawnPinsEmptyGitCommonDir(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")
	const parent = "parent-empty-aaaa-bbbb-cccc-111111111111"
	haveSpawnCapableSandboxParent(t, f, "alpha", parent, harness.DefaultName, harness.ClaudeSandboxInherit)
	cwd := t.TempDir()

	body := map[string]any{"name": "codex-worker", "cwd": cwd, "harness": harness.CodexName}
	ch := decodeWriteProofChallenge(t,
		agentReq(t, f, parent, http.MethodPost, "/v1/groups/alpha/spawn", body))
	resolvedCwd, err := filepath.EvalSymlinks(cwd)
	require.NoError(t, err)
	assert.Equal(t, []string{resolvedCwd}, ch.WriteProof.Dirs)
	answerChallenge(t, ch)
	body["write_proof_token"] = ch.WriteProof.Token
	rec := agentReq(t, f, parent, http.MethodPost, "/v1/groups/alpha/spawn", body)
	require.Equalf(t, http.StatusOK, rec.Code, "proved codex spawn; body=%s", rec.Body.String())

	var resp struct {
		ConvID string `json:"conv_id"`
	}
	testharness.DecodeJSON(t, rec, &resp)
	got, ok := f.World.SpawnCodexGitCommonDir(resp.ConvID)
	require.True(t, ok)
	assert.Empty(t, got)
	pinned, ok := f.World.SpawnCodexGitCommonDirPinned(resp.ConvID)
	require.True(t, ok)
	assert.True(t, pinned, "proved absence must remain distinguishable from an unpinned launch")
}

// Scenario: the challenge round-trip must not burn a spawn-rate slot — with
// a budget of ONE spawn per window, challenge + proved retry still lands,
// and only a SECOND spawn attempt is rate-limited (on its proved retry; the
// pre-claim proof gate answers the unproved attempt with a challenge).
func TestSpawnDirProof_ChallengeDoesNotBurnRateSlot(t *testing.T) {
	prev := agentd.SpawnMaxPerWindow
	agentd.SpawnMaxPerWindow = 1
	t.Cleanup(func() { agentd.SpawnMaxPerWindow = prev })

	f := newFlow(t)
	f.HaveGroup("alpha")
	const parent = "parent-wrp8-aaaa-bbbb-cccc-111111111111"
	haveSpawnCapableSandboxParent(t, f, "alpha", parent, harness.DefaultName, harness.ClaudeSandboxInherit)

	dir := t.TempDir()
	body := map[string]any{"name": "worker", "cwd": dir}
	ch := decodeWriteProofChallenge(t,
		agentReq(t, f, parent, http.MethodPost, "/v1/groups/alpha/spawn", body))
	answerChallenge(t, ch)
	body["write_proof_token"] = ch.WriteProof.Token
	rec := agentReq(t, f, parent, http.MethodPost, "/v1/groups/alpha/spawn", body)
	require.Equalf(t, http.StatusOK, rec.Code,
		"the challenge must not have consumed the single slot; body=%s", rec.Body.String())

	// Second spawn: fresh challenge, then the proved retry hits the limit.
	body2 := map[string]any{"name": "worker2", "cwd": dir}
	ch2 := decodeWriteProofChallenge(t,
		agentReq(t, f, parent, http.MethodPost, "/v1/groups/alpha/spawn", body2))
	answerChallenge(t, ch2)
	body2["write_proof_token"] = ch2.WriteProof.Token
	rec = agentReq(t, f, parent, http.MethodPost, "/v1/groups/alpha/spawn", body2)
	require.Equalf(t, http.StatusTooManyRequests, rec.Code, "body=%s", rec.Body.String())
}

// Scenario: the Flow harness (mirroring the CLI's transparent handling)
// makes an agent-caller spawn into a writable dir Just Work — the user-
// visible behaviour of `tclaude agent spawn` inside a sandbox that allows
// the target dir.
func TestSpawnDirProof_HarnessAnswersTransparently(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")
	const parent = "parent-wrp9-aaaa-bbbb-cccc-111111111111"
	haveSpawnCapableSandboxParent(t, f, "alpha", parent, harness.DefaultName, harness.ClaudeSandboxInherit)

	dir := t.TempDir()
	resp := f.AsAgent(parent).SpawnWith("alpha", map[string]any{"name": "worker", "cwd": dir})
	require.Equalf(t, http.StatusOK, resp.Code, "body=%s", resp.Raw)

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	for _, e := range entries {
		assert.False(t, strings.HasPrefix(e.Name(), ".tclaude-write-proof-"),
			"no proof litter may remain after the dance")
	}
}

// Scenario: an agent with templates.instantiate deploys a whole team into a
// directory it cannot write. Without a proof answer the instantiate is
// refused with a challenge — closing the template bypass. This is the
// template twin of TestSpawnDirProof_MissingProofRefused.
func TestSpawnDirProof_TemplateInstantiateChallenged(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")
	const parent = "parent-wtpl-aaaa-bbbb-cccc-111111111111"
	haveSpawnCapableSandboxParent(t, f, "alpha", parent, harness.DefaultName, harness.ClaudeSandboxInherit)
	require.NoError(t, db.GrantAgentPermission(parent, agentd.PermTemplatesUse, "test"))

	createBody := map[string]any{
		"name":   "team",
		"agents": []map[string]any{{"name": "worker"}},
	}
	require.Equalf(t, http.StatusCreated,
		humanReq(t, f, http.MethodPost, "/v1/templates", createBody).Code, "create template")

	dir := t.TempDir()
	body := map[string]any{"group_name": "team-cast", "cwd": dir}

	// Unanswered → challenge, and nothing is created.
	ch := decodeWriteProofChallenge(t,
		agentReq(t, f, parent, http.MethodPost, "/v1/templates/team/instantiate", body))
	resolvedDir, err := filepath.EvalSymlinks(dir)
	require.NoError(t, err)
	assert.Equal(t, []string{resolvedDir}, ch.WriteProof.Dirs)
	assert.Nil(t, func() *db.AgentGroup { g, _ := db.GetAgentGroupByName("team-cast"); return g }(),
		"a challenged instantiate must not create the group")

	// Answered → the cast lands.
	rec := agentReqProof(t, f, parent, http.MethodPost, "/v1/templates/team/instantiate", body)
	require.Equalf(t, http.StatusCreated, rec.Code, "proved instantiate; body=%s", rec.Body.String())
	assert.Equal(t, 1, memberCount(t, "team-cast"))
}

func TestSpawnDirProof_TemplatePerAgentWorktreeRejectsUnprovedGitAdminDirs(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")
	const parentConv = "parent-wtpa-aaaa-bbbb-cccc-111111111111"
	haveSpawnCapableSandboxParent(t, f, "alpha", parentConv, harness.DefaultName, harness.ClaudeSandboxInherit)
	require.NoError(t, db.GrantAgentPermission(parentConv, agentd.PermTemplatesUse, "test"))
	repo, parentDir := initRepoOnMain(t)
	commonDir, err := harness.CodexGitCommonDir(repo)
	require.NoError(t, err)
	require.NotEmpty(t, commonDir)

	createBody := map[string]any{
		"name": "wt-team-proof",
		"agents": []map[string]any{
			{"name": "lead"},
			{"name": "dev"},
		},
	}
	require.Equalf(t, http.StatusCreated,
		humanReq(t, f, http.MethodPost, "/v1/templates", createBody).Code, "create template")

	body := map[string]any{
		"group_name": "wt-proof-cast",
		"cwd":        repo,
		"per_agent_worktrees": map[string]any{
			"repo":            repo,
			"branch_prefix":   "wtproof",
			"from_branch":     "main",
			"worktree_as_cwd": true,
		},
	}
	ch := decodeWriteProofChallenge(t,
		agentReq(t, f, parentConv, http.MethodPost, "/v1/templates/wt-team-proof/instantiate", body))
	assert.ElementsMatch(t, []string{repo, parentDir, commonDir}, ch.WriteProof.Dirs,
		"per-agent worktrees launch under sibling dirs, so the repo parent must be proofed too")
	answerChallenge(t, ch)
	body["write_proof_token"] = ch.WriteProof.Token
	rec := agentReq(t, f, parentConv, http.MethodPost, "/v1/templates/wt-team-proof/instantiate", body)
	require.Equalf(t, http.StatusCreated, rec.Code, "proved instantiate; body=%s", rec.Body.String())

	var res instantiateResult
	testharness.DecodeJSON(t, rec, &res)
	require.Equal(t, 0, res.Spawned)
	require.Equal(t, 2, res.Failed)
	want := map[string]string{
		"lead": filepath.Join(parentDir, "repo-wtproof-lead"),
		"dev":  filepath.Join(parentDir, "repo-wtproof-dev"),
	}
	for _, a := range res.Agents {
		wantPath := want[a.Name]
		require.NotEmpty(t, wantPath, "unexpected agent %s", a.Name)
		assert.Empty(t, a.ConvID)
		assert.Contains(t, a.Error, "unproved path",
			"daemon-created Git admin dirs must not be relabelled as caller-proved")
		assertNoDirWriteProofMarkers(t, wantPath)
	}
	assertNoDirWriteProofMarkers(t, repo)
	assertNoDirWriteProofMarkers(t, parentDir)
	assertNoDirWriteProofMarkers(t, commonDir)
}

func assertNoDirWriteProofMarkers(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	for _, e := range entries {
		assert.False(t, strings.HasPrefix(e.Name(), ".tclaude-write-proof-"),
			"proof marker leaked in %s: %s", dir, e.Name())
	}
}

func TestSpawnDirProof_CodexCloneCwdOverrideProvesAndPinsGitCommonDir(t *testing.T) {
	prevCooldown := agentd.CloneCooldown
	agentd.CloneCooldown = 0
	t.Cleanup(func() { agentd.CloneCooldown = prevCooldown })

	f := newFlow(t)
	const caller = "self-wrgc-aaaa-bbbb-cccc-111111111111"
	f.HaveGroup("alpha")
	f.HaveMember("alpha", caller)
	f.HaveAliveCodexSession(caller, "spwn-codex-clone", "tclaude-spwn-codex-clone", t.TempDir())
	require.NoError(t, db.GrantAgentPermission(caller, agentd.PermSelfClone, "test"))
	repo, repoParent := initRepoOnMain(t)
	commonDir, err := harness.CodexGitCommonDir(repo)
	require.NoError(t, err)
	require.NotEmpty(t, commonDir)

	body := map[string]any{"no_copy_conv": true, "cwd": repo}
	ch := decodeWriteProofChallenge(t,
		agentReq(t, f, caller, http.MethodPost, "/v1/whoami/clone", body))
	assert.ElementsMatch(t, []string{repoParent, repo, commonDir}, ch.WriteProof.Dirs)
	answerChallenge(t, ch)
	body["write_proof_token"] = ch.WriteProof.Token
	rec := agentReq(t, f, caller, http.MethodPost, "/v1/whoami/clone", body)
	require.Equalf(t, http.StatusOK, rec.Code, "proved clone; body=%s", rec.Body.String())

	var resp struct {
		NewConv string `json:"new_conv"`
	}
	testharness.DecodeJSON(t, rec, &resp)
	require.NotEmpty(t, resp.NewConv)
	got, ok := f.World.SpawnCodexGitCommonDir(resp.NewConv)
	require.True(t, ok)
	assert.Equal(t, commonDir, got, "clone launch must use the daemon-pinned common dir")
	pinned, ok := f.World.SpawnCodexGitCommonDirPinned(resp.NewConv)
	require.True(t, ok)
	assert.True(t, pinned, "no-copy clone must carry pin-presence")
	assertNoDirWriteProofMarkers(t, repo)
	assertNoDirWriteProofMarkers(t, commonDir)
	assertNoDirWriteProofMarkers(t, repoParent)
}

func TestSpawnDirProof_CodexCloneCopyForwardsPinnedGitCommonDirOnResume(t *testing.T) {
	prevCooldown := agentd.CloneCooldown
	agentd.CloneCooldown = 0
	t.Cleanup(func() { agentd.CloneCooldown = prevCooldown })

	f := newFlow(t)
	const caller = "8b72f5ca-2480-4f9f-a693-b29fefc12201"
	f.HaveGroup("alpha")
	f.HaveMember("alpha", caller)
	sourceCwd := t.TempDir()
	f.HaveAliveSession(caller, "spwn-codex-copy", "tclaude-spwn-codex-copy", sourceCwd)
	markSessionAsCodex(t, "spwn-codex-copy")
	installCodexCopyCompatSpawner(t)
	require.NoError(t, db.GrantAgentPermission(caller, agentd.PermSelfClone, "test"))
	repo, repoParent := initRepoOnMain(t)
	commonDir, err := harness.CodexGitCommonDir(repo)
	require.NoError(t, err)

	body := map[string]any{"cwd": repo}
	ch := decodeWriteProofChallenge(t,
		agentReq(t, f, caller, http.MethodPost, "/v1/whoami/clone", body))
	assert.ElementsMatch(t, []string{repoParent, repo, commonDir}, ch.WriteProof.Dirs)
	answerChallenge(t, ch)
	body["write_proof_token"] = ch.WriteProof.Token
	rec := agentReq(t, f, caller, http.MethodPost, "/v1/whoami/clone", body)
	require.Equalf(t, http.StatusOK, rec.Code, "proved copy clone; body=%s", rec.Body.String())

	var resp struct {
		NewConv string `json:"new_conv"`
	}
	testharness.DecodeJSON(t, rec, &resp)
	require.NotEmpty(t, resp.NewConv)
	got, ok := f.World.SpawnCodexGitCommonDir(resp.NewConv)
	require.True(t, ok)
	assert.Equal(t, commonDir, got)
	pinned, ok := f.World.SpawnCodexGitCommonDirPinned(resp.NewConv)
	require.True(t, ok)
	assert.True(t, pinned, "copy clone resume must carry pin-presence")
	assertNoDirWriteProofMarkers(t, repoParent)
	assertNoDirWriteProofMarkers(t, repo)
	assertNoDirWriteProofMarkers(t, commonDir)
}

func TestSpawnDirProof_CodexCloneInheritedCwdProvesNewRepositoryRoot(t *testing.T) {
	prevCooldown := agentd.CloneCooldown
	agentd.CloneCooldown = 0
	t.Cleanup(func() { agentd.CloneCooldown = prevCooldown })

	f := newFlow(t)
	const caller = "self-grant-clone-aaaa-bbbb-111111111111"
	f.HaveGroup("alpha")
	f.HaveMember("alpha", caller)
	repo, repoParent := initRepoOnMain(t)
	f.HaveAliveCodexSession(caller, "spwn-grant-clone", "tmux-grant-clone", repo)
	require.NoError(t, db.GrantAgentPermission(caller, agentd.PermSelfClone, "test"))

	body := map[string]any{"no_copy_conv": true}
	ch := decodeWriteProofChallenge(t,
		agentReq(t, f, caller, http.MethodPost, "/v1/whoami/clone", body))
	gitDir := filepath.Join(repo, ".git")
	assert.ElementsMatch(t, []string{repo, repoParent, gitDir}, ch.WriteProof.Dirs)
	answerChallenge(t, ch)
	body["write_proof_token"] = ch.WriteProof.Token
	rec := agentReq(t, f, caller, http.MethodPost, "/v1/whoami/clone", body)
	require.Equalf(t, http.StatusOK, rec.Code, "proved inherited-cwd clone; body=%s", rec.Body.String())
	var resp struct {
		NewConv string `json:"new_conv"`
	}
	testharness.DecodeJSON(t, rec, &resp)
	dirs, ok := f.World.SpawnGitWorktreeWriteDirs(resp.NewConv)
	require.True(t, ok)
	assert.Equal(t, []string{repoParent, gitDir}, dirs)
	dirProof, ok := f.World.SpawnDirWriteProof(resp.NewConv)
	require.True(t, ok)
	assert.Empty(t, dirProof)
	cwdProof, ok := f.World.SpawnCwdWriteProof(resp.NewConv)
	require.True(t, ok)
	assert.Equal(t, ch.WriteProof.Token, cwdProof)
}

func TestSpawnDirProof_CodexReincarnateProvesNewRepositoryRoot(t *testing.T) {
	f := newFlow(t)
	const caller = "self-grant-reinc-aaaa-bbbb-111111111111"
	f.HaveGroup("alpha")
	f.HaveMember("alpha", caller)
	repo, repoParent := initRepoOnMain(t)
	f.HaveAliveCodexSession(caller, "spwn-grant-reinc", "tmux-grant-reinc", repo)
	require.NoError(t, db.GrantAgentPermission(caller, agentd.PermSelfReincarnate, "test"))

	body := map[string]any{"follow_up": "continue"}
	ch := decodeWriteProofChallenge(t,
		agentReq(t, f, caller, http.MethodPost, "/v1/whoami/reincarnate", body))
	gitDir := filepath.Join(repo, ".git")
	assert.ElementsMatch(t, []string{repo, repoParent, gitDir}, ch.WriteProof.Dirs)
	answerChallenge(t, ch)
	body["write_proof_token"] = ch.WriteProof.Token
	rec := agentReq(t, f, caller, http.MethodPost, "/v1/whoami/reincarnate", body)
	require.Equalf(t, http.StatusOK, rec.Code, "proved reincarnate; body=%s", rec.Body.String())
	var resp struct {
		NewConv string `json:"new_conv"`
	}
	testharness.DecodeJSON(t, rec, &resp)
	dirs, ok := f.World.SpawnGitWorktreeWriteDirs(resp.NewConv)
	require.True(t, ok)
	assert.Equal(t, []string{repoParent, gitDir}, dirs)
	dirProof, ok := f.World.SpawnDirWriteProof(resp.NewConv)
	require.True(t, ok)
	assert.Empty(t, dirProof)
	cwdProof, ok := f.World.SpawnCwdWriteProof(resp.NewConv)
	require.True(t, ok)
	assert.Equal(t, ch.WriteProof.Token, cwdProof)
}

func TestSpawnDirProof_CodexResumeProvesNewRepositoryRoot(t *testing.T) {
	f := newFlow(t)
	const caller = "resume-manager-aaaa-bbbb-111111111111"
	const target = "019ec663-3bef-7c41-abf8-ad956ed94a01"
	f.HaveGroup("alpha")
	f.HaveMember("alpha", target)
	repo, repoParent := initRepoOnMain(t)
	f.HaveAliveCodexSession(target, "spwn-grant-resume", "tmux-grant-resume", repo)
	f.MarkOffline("tmux-grant-resume")
	require.NoError(t, db.GrantAgentPermission(caller, agentd.PermAgentResume, "test"))
	haveSpawnCapableSandboxParent(t, f, "alpha", caller, harness.DefaultName, harness.ClaudeSandboxInherit)

	body := map[string]any{}
	ch := decodeWriteProofChallenge(t,
		agentReq(t, f, caller, http.MethodPost, "/v1/agent/"+target+"/resume", body))
	gitDir := filepath.Join(repo, ".git")
	assert.ElementsMatch(t, []string{repo, repoParent, gitDir}, ch.WriteProof.Dirs)
	answerChallenge(t, ch)
	body["write_proof_token"] = ch.WriteProof.Token
	rec := agentReq(t, f, caller, http.MethodPost, "/v1/agent/"+target+"/resume", body)
	require.Equalf(t, http.StatusOK, rec.Code, "proved resume; body=%s", rec.Body.String())
	dirs, ok := f.World.SpawnGitWorktreeWriteDirs(target)
	require.True(t, ok)
	assert.Equal(t, []string{repoParent, gitDir}, dirs)
	dirProof, ok := f.World.SpawnDirWriteProof(target)
	require.True(t, ok)
	assert.Empty(t, dirProof)
	cwdProof, ok := f.World.SpawnCwdWriteProof(target)
	require.True(t, ok)
	assert.Equal(t, ch.WriteProof.Token, cwdProof)
}

func TestSpawnDirProof_CodexTemplateProvesAndPinsGitCommonDir(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")
	const parent = "parent-tpgc-aaaa-bbbb-cccc-111111111111"
	haveSpawnCapableSandboxParent(t, f, "alpha", parent, harness.DefaultName, harness.ClaudeSandboxInherit)
	require.NoError(t, db.GrantAgentPermission(parent, agentd.PermTemplatesUse, "test"))
	repo, repoParent := initRepoOnMain(t)
	commonDir, err := harness.CodexGitCommonDir(repo)
	require.NoError(t, err)
	require.NotEmpty(t, commonDir)

	createBody := map[string]any{
		"name": "codex-template",
		"agents": []map[string]any{
			{"name": "worker", "harness": harness.CodexName},
		},
	}
	require.Equalf(t, http.StatusCreated,
		humanReq(t, f, http.MethodPost, "/v1/templates", createBody).Code, "create template")

	body := map[string]any{"group_name": "codex-cast", "cwd": repo}
	ch := decodeWriteProofChallenge(t,
		agentReq(t, f, parent, http.MethodPost, "/v1/templates/codex-template/instantiate", body))
	assert.ElementsMatch(t, []string{repoParent, repo, commonDir}, ch.WriteProof.Dirs)
	answerChallenge(t, ch)
	body["write_proof_token"] = ch.WriteProof.Token
	rec := agentReq(t, f, parent, http.MethodPost, "/v1/templates/codex-template/instantiate", body)
	require.Equalf(t, http.StatusCreated, rec.Code, "proved template; body=%s", rec.Body.String())

	var res instantiateResult
	testharness.DecodeJSON(t, rec, &res)
	require.Equal(t, 1, res.Spawned)
	require.Len(t, res.Agents, 1)
	got, ok := f.World.SpawnCodexGitCommonDir(res.Agents[0].ConvID)
	require.True(t, ok)
	assert.Equal(t, commonDir, got, "template child launch must use the daemon-pinned common dir")
	pinned, ok := f.World.SpawnCodexGitCommonDirPinned(res.Agents[0].ConvID)
	require.True(t, ok)
	assert.True(t, pinned, "template child must carry pin-presence")
	assertNoDirWriteProofMarkers(t, repo)
	assertNoDirWriteProofMarkers(t, commonDir)
	assertNoDirWriteProofMarkers(t, repoParent)
}

// Scenario: an agent self-clones with a cwd override — the same dir-granting
// power as a spawn, so the same proof handshake applies; a clone that
// inherits the source's cwd still proves and child-binds that cwd.
func TestSpawnDirProof_CloneCwdOverride(t *testing.T) {
	// Two agent-initiated clones of the same source in one test — disable
	// the cooldown so the second isn't refused for unrelated reasons.
	prevCooldown := agentd.CloneCooldown
	agentd.CloneCooldown = 0
	t.Cleanup(func() { agentd.CloneCooldown = prevCooldown })

	f := newFlow(t)
	const caller = "self-wrpc-aaaa-bbbb-cccc-111111111111"
	sourceDir, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	f.HaveConvWithTitle(caller, "worker")
	f.HaveAliveSession(caller, "spwn-wrpc-1", "tclaude-spwn-wrpc-1", sourceDir)
	f.HaveGroup("alpha")
	f.HaveMember("alpha", caller)
	require.NoError(t, db.GrantAgentPermission(caller, agentd.PermSelfClone, "test"))

	// Inherit-cwd clone: prove the cwd even though it is not overridden.
	inheritBody := map[string]any{"no_copy_conv": true}
	inheritChallenge := decodeWriteProofChallenge(t,
		agentReq(t, f, caller, http.MethodPost, "/v1/whoami/clone", inheritBody))
	assert.Equal(t, []string{sourceDir}, inheritChallenge.WriteProof.Dirs)
	answerChallenge(t, inheritChallenge)
	inheritBody["write_proof_token"] = inheritChallenge.WriteProof.Token
	rec := agentReq(t, f, caller, http.MethodPost, "/v1/whoami/clone", inheritBody)
	require.Equalf(t, http.StatusOK, rec.Code, "inherit-cwd clone; body=%s", rec.Body.String())

	// Override clone: challenge, then the proved retry lands there.
	dir := t.TempDir()
	body := map[string]any{"no_copy_conv": true, "cwd": dir}
	ch := decodeWriteProofChallenge(t,
		agentReq(t, f, caller, http.MethodPost, "/v1/whoami/clone", body))
	answerChallenge(t, ch)
	body["write_proof_token"] = ch.WriteProof.Token
	rec = agentReq(t, f, caller, http.MethodPost, "/v1/whoami/clone", body)
	require.Equalf(t, http.StatusOK, rec.Code, "proved clone; body=%s", rec.Body.String())

	var resp struct {
		NewConv string `json:"new_conv"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.NotEmpty(t, resp.NewConv)
	resolvedDir, err := filepath.EvalSymlinks(dir)
	require.NoError(t, err)
	sess, err := db.FindSessionByConvID(resp.NewConv)
	require.NoError(t, err)
	require.NotNil(t, sess)
	assert.Equal(t, resolvedDir, sess.Cwd, "clone must land in the verified resolved dir")
}
