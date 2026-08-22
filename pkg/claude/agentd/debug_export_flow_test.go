package agentd_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/session"
)

func TestDashboardDebugExportCarriesExactRecordedConfiguration(t *testing.T) {
	const convID = "debug-1111-2222-3333-4444"
	const launchGeneration = "11111111111111111111111111111111"
	f := newFlow(t)
	f.HaveGroup("debuggers")
	f.HaveAliveSession(convID, "spwn-debug", "tmux-debug", f.TestCwd("repo"))
	f.HaveMember("debuggers", convID)
	agentID, err := db.AgentIDForConv(convID)
	require.NoError(t, err)
	require.NotEmpty(t, agentID)

	require.NoError(t, db.SetAgentInitialSpawnConfig(agentID,
		`{"harness":"codex","sandbox_profile":"developer","initial_message":"private brief","write_proof_token":"one-time"}`))
	mode, implementation, model := "tclaude-agent", "harness-builtin", "gpt-5.6-sol"
	require.NoError(t, db.SetAgentRelaunchProfile(agentID, db.AgentRelaunchProfile{
		Version: db.RelaunchProfileVersion, HarnessBuiltinMode: &mode,
		SandboxImplementation: &implementation, ModelID: &model,
	}))
	snapshot := sandboxpolicy.NewSnapshot(sandboxpolicy.EffectiveProfile{
		Filesystem:       []sandboxpolicy.FilesystemGrant{},
		Environment:      []sandboxpolicy.EnvironmentEntry{{Name: "HOME", Value: "/home/helena"}},
		AgentDirectories: []string{},
		HarnessConfig:    sandboxpolicy.HarnessConfigAccessRead,
	}, []sandboxpolicy.AppliedProfile{{Scope: sandboxpolicy.ScopeGlobal, ID: 7, Name: "developer"}})
	require.NoError(t, db.SetAgentEffectiveSandboxConfig(agentID, &snapshot))
	require.NoError(t, db.SaveSession(&db.SessionRow{
		ID: "spwn-debug", TmuxSession: "tmux-debug", ConvID: convID,
		PID: os.Getpid(), Cwd: f.TestCwd("repo"), Status: "running", Harness: "codex",
		HarnessBuiltinMode: mode, SandboxImplementation: implementation,
		ApprovalPolicy: "never", EffectiveSandbox: &snapshot,
	}))
	// An older launch can receive a delayed hook and become most recently
	// updated. Running diagnostics must still select the newest launch.
	require.NoError(t, db.SaveSession(&db.SessionRow{
		ID: "spwn-debug-old", TmuxSession: "tmux-debug-old", ConvID: convID,
		Cwd: f.TestCwd("old-repo"), Status: "idle", Harness: "claude",
		HarnessBuiltinMode: "default", SandboxImplementation: "harness-builtin",
	}))
	d, err := db.Open()
	require.NoError(t, err)
	_, err = d.Exec(`UPDATE sessions SET created_at = CASE id
		WHEN 'spwn-debug-old' THEN 1000000000
		ELSE 2000000000 END,
		updated_at = CASE id
		WHEN 'spwn-debug-old' THEN 4000000000
		ELSE 3000000000 END
		WHERE id IN ('spwn-debug', 'spwn-debug-old')`)
	require.NoError(t, err)
	require.NoError(t, db.UpdateSessionModelSlug("spwn-debug", model))
	require.NoError(t, db.UpdateSessionEffort("spwn-debug", "high"))
	require.NoError(t, db.SetSessionExitLaunchGeneration("spwn-debug", launchGeneration))
	current, err := db.GetSessionExitLaunchIdentity("spwn-debug")
	require.NoError(t, err)
	require.Equal(t, launchGeneration, current.Generation)
	boundaryRaw, err := json.Marshal(session.ExecutionBoundary{
		Version: session.ExecutionBoundaryVersion, LaunchGeneration: launchGeneration,
		SandboxImplementation: implementation,
		Platform:              "linux",
		Harness: session.ExecutionHarness{
			Name: "codex", LookupName: "codex", HostPath: "/opt/codex/bin/codex",
			SandboxPath: "/opt/codex/bin/codex", Resolution: "absolute executable resolved before sandbox launch",
		},
		Tclaude: &session.ExecutionBinary{
			Name: "tclaude", HostPath: "/home/helena/go/bin/tclaude",
			SandboxPath: "/.tclaude/bin/tclaude", Exposure: "single-file read-only bind",
		},
		PATH: session.ExecutionPATH{
			Host: "/usr/bin", LaunchBase: "/usr/bin", BeforePreLaunch: "/.tclaude/bin:/usr/bin",
			Construction: []string{"prepend /.tclaude/bin"}, FinalValueKnown: true,
		},
		Identity: session.ExecutionIdentityMapping{
			Host:          session.ExecutionUnixIdentity{UID: 1000, GID: 1000},
			Sandbox:       session.ExecutionUnixIdentity{UID: 0, GID: 0},
			UserNamespace: true, Mapping: "host uid/gid mapped to namespace 0:0",
		},
		RootMode: "constructed", AutomaticEntries: []session.ExecutionNamespaceEntry{},
	})
	require.NoError(t, err)
	require.NoError(t, db.SetSessionExecutionBoundary("spwn-debug", string(boundaryRaw)))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/agents/"+agentID+"/debug-export", nil)
	req.Header.Set("Origin", "http://localhost")
	agentd.BuildDashboardHandlerForTest().ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Header().Get("Content-Disposition"), "attachment")
	assert.Contains(t, rec.Body.String(), "\n  \"format\": \"tclaude-agent-debug\",")

	var payload map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &payload))
	assert.Equal(t, "tclaude-agent-debug", payload["format"])
	configurations := payload["configurations"].(map[string]any)
	requested := configurations["requested"].(map[string]any)
	assert.Equal(t, true, requested["recorded"])
	spawn := requested["parameters"].(map[string]any)
	assert.Equal(t, "codex", spawn["harness"])
	assert.Equal(t, "<redacted: 13 bytes>", spawn["initial_message"])
	assert.NotContains(t, spawn, "write_proof_token")
	resolved := configurations["resolved"].(map[string]any)
	relaunch := resolved["durable_relaunch"].(map[string]any)
	assert.Equal(t, model, relaunch["model_id"])
	effective := resolved["effective_sandbox"].(map[string]any)["effective"].(map[string]any)
	assert.Equal(t, "read", effective["harness_config"])
	environment := effective["environment"].([]any)
	assert.Equal(t, "/home/helena", environment[0].(map[string]any)["value"])
	running := configurations["running"].(map[string]any)
	assert.Equal(t, true, running["recorded"])
	latest := running["launch"].(map[string]any)
	assert.Equal(t, f.TestCwd("repo"), latest["cwd"])
	assert.Equal(t, implementation, latest["sandbox_implementation"])
	assert.Equal(t, model, latest["model_id"])
	assert.Equal(t, "high", latest["effort"])
	assert.Equal(t, true, latest["execution_boundary_recorded"])
	boundary := latest["execution_boundary"].(map[string]any)
	assert.Equal(t, "/.tclaude/bin:/usr/bin", boundary["path"].(map[string]any)["before_pre_launch"])
	assert.Equal(t, "/.tclaude/bin/tclaude", boundary["tclaude"].(map[string]any)["sandbox_path"])
	assert.Equal(t, float64(0), boundary["identity"].(map[string]any)["sandbox"].(map[string]any)["uid"])
	live := latest["live_process"].(map[string]any)
	if runtime.GOOS == "linux" {
		assert.Equal(t, "unavailable", live["status"],
			"the flow fixture PID is the test process, not a validated harness")
	} else {
		assert.Equal(t, "unsupported", live["status"])
	}
	agentdIdentity := payload["tclaude"].(map[string]any)["agentd_identity"].(map[string]any)
	assert.Equal(t, float64(os.Getuid()), agentdIdentity["uid"])
	assert.Equal(t, float64(os.Getgid()), agentdIdentity["gid"])
}
