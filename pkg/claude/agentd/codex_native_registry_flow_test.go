package agentd_test

import (
	"encoding/json"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/claude/session"
)

func TestCodexNativeRegistryPreflightIsStructuredAndScoped(t *testing.T) {
	t.Cleanup(agentd.SetAwaitCodexAppServerLaunchReadyForTest(func(string, string) bool { return true }))
	var readinessCalls atomic.Int32
	restore := agentd.SetCodexNativeRegistryReadinessForTest(func() error {
		readinessCalls.Add(1)
		return &session.CodexNativeRegistryError{
			Code: session.CodexNativeRegistryWrongTarget, Detail: "/etc/codex points elsewhere",
		}
	})
	t.Cleanup(restore)

	t.Run("builtin app-server fails closed", func(t *testing.T) {
		f := newFlow(t)
		f.HaveGroup("crew")
		response := f.AsHuman().SpawnWith("crew", map[string]any{
			"name": "native-missing", "harness": harness.CodexName,
			"codex_app_server": true, "sandbox": harness.SandboxManagedProfile,
			"sandbox_implementation": string(sandboxpolicy.ImplementationHarnessBuiltin),
		})
		require.Equal(t, http.StatusPreconditionFailed, response.Code)
		var failure struct {
			Code  string `json:"code"`
			Error string `json:"error"`
		}
		require.NoError(t, json.Unmarshal(response.Raw, &failure))
		assert.Equal(t, session.CodexNativeRegistryWrongTarget, failure.Code)
		assert.Contains(t, failure.Error, session.CodexNativeRegistrySetupDoc)
		assert.Empty(t, response.ConvID)
	})

	callsAfterApplicable := readinessCalls.Load()
	t.Run("send-keys is unchanged", func(t *testing.T) {
		f := newFlow(t)
		f.HaveGroup("crew")
		response := f.AsHuman().SpawnWith("crew", map[string]any{
			"name": "send-keys", "harness": harness.CodexName,
			"codex_app_server": false, "sandbox": harness.SandboxManagedProfile,
			"sandbox_implementation": string(sandboxpolicy.ImplementationHarnessBuiltin),
		})
		require.Equalf(t, http.StatusOK, response.Code, "body=%s", response.Raw)
	})
	assert.Equal(t, callsAfterApplicable, readinessCalls.Load())

	t.Run("tclaude outer sandbox is unchanged", func(t *testing.T) {
		f := newFlow(t)
		f.HaveGroup("crew")
		response := f.AsHuman().SpawnWith("crew", map[string]any{
			"name": "outer-layer", "harness": harness.CodexName,
			"codex_app_server": true, "sandbox": harness.SandboxManagedProfile,
			"sandbox_implementation": string(sandboxpolicy.ImplementationTclaudeLayer),
		})
		require.Equalf(t, http.StatusOK, response.Code, "body=%s", response.Raw)
	})
	assert.Equal(t, callsAfterApplicable, readinessCalls.Load())
}
