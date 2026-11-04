package agentd_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// tcl-bmhpir: an explicit --harness-config posture is a launch contract
// (applySpawnHarnessConfig), not a profile tier. Every relaunch that rebuilds
// the policy from the profile chain must keep it: a read pin over a write
// chain must not widen to write, and a write selection over the floor must
// not silently revert.
func TestSpawnHarnessConfigSurvivesRelaunch(t *testing.T) {
	relaunches := []struct {
		name string
		run  func(f *testharness.Flow, spawn testharness.SpawnResp) string
	}{
		{name: "resume", run: func(f *testharness.Flow, spawn testharness.SpawnResp) string {
			f.MarkOffline(spawn.TmuxSession)
			f.AssertResumeSpawned(f.AsHuman().Resume(spawn.ConvID))
			return spawn.ConvID
		}},
		{name: "reincarnate", run: func(f *testharness.Flow, spawn testharness.SpawnResp) string {
			return f.AsHuman().Reincarnate(spawn.ConvID, "continue").NewConv
		}},
		{name: "clone", run: func(f *testharness.Flow, spawn testharness.SpawnResp) string {
			return f.AsHuman().CloneFresh(spawn.ConvID).NewConv
		}},
	}
	postures := []struct {
		name     string
		chain    sandboxpolicy.HarnessConfigAccess // the global profile's posture
		explicit string
		want     sandboxpolicy.HarnessConfigAccess
	}{
		{name: "read-pin-over-write-chain", chain: sandboxpolicy.HarnessConfigAccessWrite, explicit: "read", want: sandboxpolicy.HarnessConfigAccessRead},
		{name: "write-over-floor", chain: sandboxpolicy.HarnessConfigAccessDefault, explicit: "write", want: sandboxpolicy.HarnessConfigAccessWrite},
		{name: "write-over-read-chain", chain: sandboxpolicy.HarnessConfigAccessRead, explicit: "write", want: sandboxpolicy.HarnessConfigAccessWrite},
	}
	// Codex under the managed profile takes the paths that rebuild the policy
	// from the profile chain on clone too; Claude's clone inherits the record.
	harnesses := []struct {
		name string
		body map[string]any
	}{
		{name: "claude", body: map[string]any{}},
		{name: "codex-managed", body: map[string]any{"harness": "codex", "sandbox": "tclaude-agent"}},
	}
	for _, h := range harnesses {
		for _, posture := range postures {
			for _, relaunch := range relaunches {
				t.Run(h.name+"/"+posture.name+"/"+relaunch.name, func(t *testing.T) {
					f := newFlow(t)
					f.HaveGroup("crew")
					_, err := db.CreateSandboxProfile(&db.SandboxProfile{Name: "chain", HarnessConfig: posture.chain})
					require.NoError(t, err)
					require.NoError(t, db.SetGlobalSandboxProfile("chain"))

					body := map[string]any{"name": "worker", "harness_config": posture.explicit, "cwd": t.TempDir()}
					for k, v := range h.body {
						body[k] = v
					}
					spawn := f.AsHuman().SpawnWith("crew", body)
					require.Equalf(t, http.StatusOK, spawn.Code, "spawn body=%s", spawn.Raw)
					born, ok := f.World.SpawnSandboxPolicy(spawn.ConvID)
					require.True(t, ok)
					require.Equal(t, posture.want, born.Effective.HarnessConfig, "precondition: the launch took the explicit posture")

					conv := relaunch.run(f, spawn)
					require.NotEmpty(t, conv)
					relaunched, ok := f.World.SpawnSandboxPolicy(conv)
					require.True(t, ok, "the relaunch reached the spawner")
					require.NotNil(t, relaunched)
					assert.Equal(t, posture.want, relaunched.Effective.HarnessConfig, "the explicit posture survives %s", relaunch.name)
					persisted, err := db.AgentEffectiveSandboxConfigForConv(conv)
					require.NoError(t, err)
					require.NotNil(t, persisted)
					assert.Equal(t, posture.want, persisted.Effective.HarnessConfig, "and is what the agent's record says")

					// And again: the relaunched snapshot keeps it for the next one.
					if relaunch.name == "resume" {
						for _, session := range f.World.Tmux.Sessions() {
							f.MarkOffline(session)
						}
						f.AssertResumeSpawned(f.AsHuman().Resume(conv))
						again, ok := f.World.SpawnSandboxPolicy(conv)
						require.True(t, ok)
						assert.Equal(t, posture.want, again.Effective.HarnessConfig, "a second resume keeps it too")
					}
				})
			}
		}
	}
}
