package agentd_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/claude/agentd"
	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
)

// A clone has two launch branches: no-copy starts a fresh `session new`, and
// copy resumes a copied conversation. Both must hand the spawner the same
// launch posture, the source's, and differ only in the fields that name what
// is launched. These pins read that from the SpawnArgs the spawner receives.

// spawnArgsRecorder wraps the flow's simulator spawner and keeps every
// SpawnArgs it is handed. fail, when set, refuses every launch with it.
type spawnArgsRecorder struct {
	inner   agentd.Spawner
	fail    error
	news    []clcommon.SpawnArgs
	resumes []clcommon.SpawnArgs
}

func (r *spawnArgsRecorder) SpawnNew(args clcommon.SpawnArgs) error {
	r.news = append(r.news, args)
	if r.fail != nil {
		return r.fail
	}
	return r.inner.SpawnNew(args)
}

func (r *spawnArgsRecorder) SpawnResume(args clcommon.SpawnArgs) error {
	r.resumes = append(r.resumes, args)
	if r.fail != nil {
		return r.fail
	}
	return r.inner.SpawnResume(args)
}

func recordSpawnArgs(t *testing.T) *spawnArgsRecorder {
	t.Helper()
	r := &spawnArgsRecorder{inner: agentd.Spawn}
	agentd.Spawn = r
	t.Cleanup(func() { agentd.Spawn = r.inner })
	return r
}

// clonePosture is a clone launch's SpawnArgs with the fields that name what is
// launched blanked: the label or conv it is launched as, its preset session
// id, its follow-up prompt (which cites its own inbox message), and the
// no-copy continuation flag. The effective sandbox is compared by value.
func clonePosture(t *testing.T, a clcommon.SpawnArgs) (clcommon.SpawnArgs, sandboxpolicy.Snapshot) {
	t.Helper()
	require.NotNil(t, a.EffectiveSandbox, "a clone launches under an effective sandbox")
	sandbox := *a.EffectiveSandbox
	a.EffectiveSandbox = nil
	a.Label, a.ConvID, a.SessionID, a.InitialPrompt = "", "", "", ""
	a.SandboxContinuation = false
	return a, sandbox
}

func cloneBothWays(t *testing.T, spawnBody map[string]any) (noCopy, copied clcommon.SpawnArgs, noCopyConv, copiedConv string) {
	t.Helper()
	f := newFlow(t)
	f.HaveGroup("crew")
	spawn := f.AsHuman().SpawnWith("crew", spawnBody)
	require.Equalf(t, http.StatusOK, spawn.Code, "spawn body=%s", spawn.Raw)
	rec := recordSpawnArgs(t)

	nc := f.AsHuman().CloneWith(spawn.ConvID, map[string]any{"no_copy_conv": true})
	require.Equalf(t, http.StatusOK, nc.Code, "no-copy clone body=%s", nc.Raw)
	require.False(t, nc.CopyConv)
	cp := f.AsHuman().CloneWith(spawn.ConvID, map[string]any{})
	require.Equalf(t, http.StatusOK, cp.Code, "copy clone body=%s", cp.Raw)
	require.True(t, cp.CopyConv)

	require.Len(t, rec.news, 1, "the no-copy clone is a fresh session new")
	require.Len(t, rec.resumes, 1, "the copy clone resumes its copied conversation")
	return rec.news[0], rec.resumes[0], nc.NewConv, cp.NewConv
}

func assertCloneBranchesShareThePosture(t *testing.T, noCopy, copied clcommon.SpawnArgs, noCopyConv, copiedConv string) {
	t.Helper()
	// What names each launch.
	assert.NotEmpty(t, noCopy.Label, "no-copy launches under a fresh label")
	assert.Empty(t, noCopy.ConvID)
	assert.True(t, noCopy.SandboxContinuation, "no-copy is a continuation of the source's posture")
	assert.Empty(t, copied.Label)
	assert.Equal(t, copiedConv, copied.ConvID, "copy resumes the copied conversation")
	assert.Empty(t, copied.SessionID)
	assert.False(t, copied.SandboxContinuation)

	// Everything else is the same posture. The sandboxes compare equal only
	// because these sources declare no per-agent directories: each branch
	// materializes those under its own launch key (label, or the copied conv).
	ncPosture, ncSandbox := clonePosture(t, noCopy)
	cpPosture, cpSandbox := clonePosture(t, copied)
	assert.Equal(t, ncPosture, cpPosture, "both clone branches launch with the same posture")
	assert.Equal(t, ncSandbox, cpSandbox, "both clone branches launch under the same sandbox")
}

func TestCloneSpawnArgs_CodexBranchesShareTheSourcesPosture(t *testing.T) {
	noCopy, copied, ncConv, cpConv := cloneBothWays(t, map[string]any{
		"name": "worker", "harness": "codex", "model": "gpt-5.6-terra", "effort": "high",
		"approval": "never", "sandbox": "workspace-write", "auto_review": true,
	})
	assertCloneBranchesShareThePosture(t, noCopy, copied, ncConv, cpConv)
	assert.Empty(t, noCopy.SessionID, "Codex mints its own id: a no-copy Codex clone presets none")
	for _, a := range []clcommon.SpawnArgs{noCopy, copied} {
		assert.Equal(t, "codex", a.Harness)
		assert.Equal(t, "gpt-5.6-terra", a.Model)
		assert.Equal(t, "high", a.Effort)
		assert.Equal(t, "never", a.Approval)
		assert.True(t, a.AutoReview)
		assert.Equal(t, "workspace-write", a.Sandbox)
		assert.NotEmpty(t, a.SandboxImplementation)
		assert.NotEmpty(t, a.CodexStateRoot, "a Codex clone keeps its source's state root")
	}
}

func TestCloneSpawnArgs_ClaudeBranchesShareTheSourcesPosture(t *testing.T) {
	noCopy, copied, ncConv, cpConv := cloneBothWays(t, map[string]any{
		"name": "worker", "model": "opus", "effort": "high", "remote_control": true,
		"auto_memory": false, "ask_user_question_timeout": "5m",
	})
	assertCloneBranchesShareThePosture(t, noCopy, copied, ncConv, cpConv)
	assert.Equal(t, ncConv, noCopy.SessionID, "a launch-enrolled no-copy clone presets its conv as the session id")
	for _, a := range []clcommon.SpawnArgs{noCopy, copied} {
		assert.Equal(t, "opus", a.Model)
		assert.Equal(t, "high", a.Effort)
		assert.True(t, a.RemoteControl, "an armed source's clone is armed too")
		assert.Equal(t, "5m", a.AskUserQuestionTimeout)
		assert.NotEmpty(t, a.Name, "the clone's name rides the launch")
		assert.NotEmpty(t, a.InitialPrompt, "the clone's arrival rides the launch")
	}
}

// A refused launch is a typed spawn error on either branch, and the clone
// response names no new agent.
func TestCloneSpawnArgs_RefusedLaunchIsATypedSpawnError(t *testing.T) {
	for _, tc := range []struct {
		name string
		body map[string]any
	}{
		{"no-copy", map[string]any{"no_copy_conv": true}},
		{"copy", map[string]any{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFlow(t)
			f.HaveGroup("crew")
			spawn := f.AsHuman().SpawnWith("crew", map[string]any{"name": "worker"})
			require.Equalf(t, http.StatusOK, spawn.Code, "spawn body=%s", spawn.Raw)
			rec := recordSpawnArgs(t)
			rec.fail = errors.New("pinned launch refusal")

			clone := f.AsHuman().CloneWith(spawn.ConvID, tc.body)
			require.Equal(t, 1, len(rec.news)+len(rec.resumes), "exactly one launch was attempted")
			assert.Equal(t, http.StatusInternalServerError, clone.Code, "body=%s", clone.Raw)
			var body struct {
				Code  string `json:"code"`
				Error string `json:"error"`
			}
			require.NoError(t, json.Unmarshal(clone.Raw, &body), "body=%s", clone.Raw)
			assert.Equal(t, "spawn", body.Code)
			assert.Contains(t, body.Error, "pinned launch refusal")
			assert.Empty(t, clone.NewConv, "a refused clone names no new agent")
		})
	}
}
