package agentd_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/claude/worktree"
)

// Flow coverage for the opt-in dir-trust flag threading from the spawn body
// through to the forked `tclaude session new --trust-dir` — JOH-205 inc4 Part B
// for Codex, extended to Claude Code once it grew the same opt-in.
//
// The simulator spawns in-process, so it does NOT perform the real
// ~/.codex/config.toml or ~/.claude.json write (those are covered exhaustively
// by the harness package's editor unit tests). What these scenarios pin is the
// PLUMBING: that the `trust_dir` body field is gated on the harness having a
// trust dialog at all, restricted for agent callers outside a verified sibling
// worktree, and threaded to the spawner — captured here via
// World.SpawnTrustDir. They are the trust-dir analog of the
// sandbox/approval/auto-review body-contract tests.
//
// Both harnesses are asserted in pairs so the two cannot silently diverge.

// Scenario: a Codex spawn that ticks the dashboard's "pre-trust this dir"
// checkbox sends {"trust_dir": true}; the daemon threads it to the spawner so
// the forked session would write the trust entry before launch.
func TestCodexSpawn_TrustDirThreadsWhenOptedIn(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)
	f.HaveGroup("squad")

	spawn := f.SpawnWith("squad", map[string]any{
		"name":      "cdx-trusted",
		"harness":   "codex",
		"trust_dir": true,
	})
	require.Equal(t, 200, spawn.Code, "spawn body=%s", string(spawn.Raw))
	require.NotEmpty(t, spawn.ConvID, "spawn returned a conv id")

	got, ok := f.World.SpawnTrustDir(spawn.ConvID)
	require.True(t, ok, "the spawner recorded a trust-dir flag for the new conv")
	assert.True(t, got, "trust_dir:true must thread `--trust-dir` to the spawner")
}

// Scenario: the default — a Codex spawn that does NOT request dir-trust never
// gets it. Pins "never auto-defaulted": the flag is false unless explicitly set.
func TestCodexSpawn_TrustDirOffByDefault(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)
	f.HaveGroup("squad")

	spawn := f.SpawnWith("squad", map[string]any{
		"name":    "cdx-untrusted",
		"harness": "codex",
	})
	require.Equal(t, 200, spawn.Code, "spawn body=%s", string(spawn.Raw))

	got, ok := f.World.SpawnTrustDir(spawn.ConvID)
	require.True(t, ok, "the spawner recorded a trust-dir flag for the new conv")
	assert.False(t, got, "an unrequested dir-trust must default off — never auto-trusted")
}

// Scenario: Claude Code takes the SAME opt-in. It has its own trust-folder
// dialog (recorded in ~/.claude.json), so the checkbox threads through
// identically rather than being rejected as it was while the flag was
// Codex-only. This is the parity assertion for the Codex case above.
func TestClaudeSpawn_TrustDirThreadsWhenOptedIn(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)
	f.HaveGroup("squad")

	spawn := f.SpawnWith("squad", map[string]any{
		"name":      "cc-trusted",
		"trust_dir": true, // no harness → Claude Code
	})
	require.Equal(t, 200, spawn.Code, "trust_dir for Claude Code must be accepted; body=%s", string(spawn.Raw))
	require.NotEmpty(t, spawn.ConvID, "spawn returned a conv id")

	got, ok := f.World.SpawnTrustDir(spawn.ConvID)
	require.True(t, ok, "the spawner recorded a trust-dir flag for the new conv")
	assert.True(t, got, "trust_dir:true must thread `--trust-dir` to the spawner for claude too")
}

// Scenario: the never-auto-defaulted guarantee holds for Claude Code as well —
// pre-trusting edits ~/.claude.json, so an unrequested spawn must not.
func TestClaudeSpawn_TrustDirOffByDefault(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)
	f.HaveGroup("squad")

	spawn := f.SpawnWith("squad", map[string]any{"name": "cc-untrusted"})
	require.Equal(t, 200, spawn.Code, "spawn body=%s", string(spawn.Raw))

	got, ok := f.World.SpawnTrustDir(spawn.ConvID)
	require.True(t, ok, "the spawner recorded a trust-dir flag for the new conv")
	assert.False(t, got, "an unrequested dir-trust must default off — never auto-trusted")
}

// Scenario: the agent-caller restriction is harness-neutral. A Claude Code
// child spawned by an AGENT into an arbitrary dir still cannot request
// pre-trust — only tclaude's own verified sibling worktrees are exempt (the
// test below), so an agent can never talk the daemon into trusting a path it
// merely named.
func TestClaudeSpawn_TrustDirRejectedForAgentCallerOutsideSiblingWorktree(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("squad")
	const parent = "parent-trst-cc-bbbb-cccc-111111111111"
	haveSpawnCapableSandboxParent(t, f, "squad", parent, harness.DefaultName, "")

	rec := agentReqProof(t, f, parent, http.MethodPost, "/v1/groups/squad/spawn", map[string]any{
		"name":      "cc-trusted",
		"cwd":       t.TempDir(),
		"trust_dir": true,
	})
	require.Equal(t, http.StatusForbidden, rec.Code, "body=%s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "trust_dir_restricted")
}

// Scenario: a Claude Code child spawned into a verified default sibling
// worktree is auto-trusted with no trust_dir field, exactly as the Codex case
// below. This is the half that makes agent-spawned CC worktree children work
// unattended — without it the pane stops on the trust dialog.
func TestClaudeSpawn_DefaultSiblingWorktreeAutoTrustedForAgentCaller(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("squad")
	const parent = "parent-trst-cc-sibling-cccc-1111111111"
	haveSpawnCapableSandboxParent(t, f, "squad", parent, harness.DefaultName, "")

	repo, _ := initRepoOnMain(t)
	worktreeDir, err := worktree.AddWorktreeIn(repo, "agent-child", "main", "")
	require.NoError(t, err)

	rec := agentReqProof(t, f, parent, http.MethodPost, "/v1/groups/squad/spawn", map[string]any{
		"name": "cc-sibling",
		"cwd":  worktreeDir,
	})
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	var resp struct {
		ConvID string `json:"conv_id"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	got, ok := f.World.SpawnTrustDir(resp.ConvID)
	require.True(t, ok)
	assert.True(t, got, "a default sibling worktree must be pre-trusted for claude too")
}

func TestCodexSpawn_TrustDirRejectedForAgentCallerOutsideSiblingWorktree(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("squad")
	const parent = "parent-trst-aaaa-bbbb-cccc-111111111111"
	haveSpawnCapableSandboxParent(t, f, "squad", parent, harness.CodexName, harness.SandboxManagedProfile)

	rec := agentReqProof(t, f, parent, http.MethodPost, "/v1/groups/squad/spawn", map[string]any{
		"name":      "cdx-trusted",
		"cwd":       t.TempDir(),
		"harness":   "codex",
		"trust_dir": true,
	})
	require.Equal(t, http.StatusForbidden, rec.Code, "body=%s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "trust_dir_restricted")
}

func TestCodexSpawn_DefaultSiblingWorktreeAutoTrustedForAgentCaller(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("squad")
	const parent = "parent-trst-sibling-cccc-111111111111"
	haveSpawnCapableSandboxParent(t, f, "squad", parent, harness.CodexName, harness.SandboxManagedProfile)

	repo, repoParent := initRepoOnMain(t)
	worktreeDir, err := worktree.AddWorktreeIn(repo, "agent-child", "main", "")
	require.NoError(t, err)
	gitDir, err := harness.GitDir(worktreeDir)
	require.NoError(t, err)

	// No trust_dir field: a verified default ../<repo>-<branch> worktree is
	// trusted automatically so the detached Codex child cannot freeze on its
	// onboarding modal. The agent caller proves every repository path the child
	// receives (container, shared Git metadata, and its exact admin dir).
	rec := agentReqProof(t, f, parent, http.MethodPost, "/v1/groups/squad/spawn", map[string]any{
		"name":    "cdx-sibling",
		"cwd":     worktreeDir,
		"harness": "codex",
	})
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	var resp struct {
		ConvID string `json:"conv_id"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	got, ok := f.World.SpawnTrustDir(resp.ConvID)
	require.True(t, ok)
	assert.True(t, got, "default sibling worktree must always be pre-trusted")
	writeDirs, ok := f.World.SpawnGitWorktreeWriteDirs(resp.ConvID)
	require.True(t, ok)
	assert.Equal(t, []string{repoParent, gitDir}, writeDirs,
		"linked-worktree launch must grant the exact checkout admin dir")
}
