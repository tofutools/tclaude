package claude

import (
	"context"
	"encoding/json"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/host"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/ports"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAutoCompactWindowContextUsesNativePercentageAndBinding(t *testing.T) {
	id := "43e874eb-4827-4b22-b1b8-376a5e5e553f"
	at := time.Now().UTC()
	raw := []byte(`{"session_id":"` + id + `","context_window":{"context_window_size":1000000,"used_percentage":21}}`)
	usage := parseContextUsage(raw, id, "450000", at)
	require.NotNil(t, usage)
	require.InDelta(t, 46.6666667, usage.UsedPercent, 0.00001)
	require.Equal(t, int64(450000), usage.EffectiveWindow)
	require.Equal(t, float64(21), parseContextUsage(raw, id, "", at).UsedPercent)
	require.Equal(t, int64(1000000), parseContextUsage(raw, id, "2000000", at).EffectiveWindow)
	for _, bad := range []string{strings.ReplaceAll(string(raw), id, "other"), `{"session_id":"` + id + `","context_window":{}}`, strings.ReplaceAll(string(raw), `"used_percentage":21`, `"used_percentage":null`), strings.ReplaceAll(string(raw), `"used_percentage":21`, `"used_percentage":-1`), strings.ReplaceAll(string(raw), `"context_window":`, `"agent_id":"nested","context_window":`)} {
		require.Nil(t, parseContextUsage([]byte(bad), id, "450000", at))
	}
}

func TestAutoCompactWindowStatusLineSpoolAndPersistedProjection(t *testing.T) {
	root := t.TempDir()
	spool, err := host.PrepareObservationSpool(filepath.Join(root, "observations"))
	require.NoError(t, err)
	id := "43e874eb-4827-4b22-b1b8-376a5e5e553f"
	runtime := &Runtime{executionID: "execution", attempt: 1, nativeID: id, intent: ports.StartFresh, observations: &observationSink{}, spool: spool, autoCompactWindow: "450000"}
	writeClaudeHookEvent(t, spool.Directory(), sessionStartEvent{SessionID: id, HookEventName: "SessionStart", Source: "startup"})
	_, err = runtime.consumeObservationEvents(context.Background(), nil)
	require.NoError(t, err)
	// Execute the exact statusLine command exposed to Claude with native JSON stdin.
	prepared := &prepared{request: ports.PreparationRequest{Spec: model.ResolvedExecutionSpec{WorkingDirectory: root, AutoCompactWindow: "450000"}}}
	args := prepared.argv()
	var settings map[string]json.RawMessage
	for i, arg := range args {
		if arg == "--settings" {
			require.NoError(t, json.Unmarshal([]byte(args[i+1]), &settings))
			break
		}
	}
	var status struct {
		Command string `json:"command"`
	}
	require.NoError(t, json.Unmarshal(settings["statusLine"], &status))
	command := exec.Command("/bin/sh", "-c", status.Command)
	command.Env = append(os.Environ(), "TCLAUDE_OBSERVATION_SPOOL="+spool.Directory())
	command.Stdin = strings.NewReader(`{"session_id":"` + id + `","context_window":{"context_window_size":1000000,"used_percentage":21}}`)
	require.NoError(t, command.Run())
	_, err = runtime.consumeObservationEvents(context.Background(), nil)
	require.NoError(t, err)
	require.NotNil(t, runtime.contextUsage)
	persisted, err := encodeEvidence(evidence{ExecutionID: "execution", NativeID: id, ContextReady: true, ContextUsage: runtime.contextUsage})
	require.NoError(t, err)
	execution := model.Execution{ID: "execution", ContextReadiness: model.ContextReadinessReady, Evidence: persisted, NativeConversation: nativeEvidence(id)}
	provider := &Provider{}
	require.Equal(t, runtime.contextUsage, provider.ProjectContextUsage(execution))
	execution.ID = "other"
	require.Nil(t, provider.ProjectContextUsage(execution))
	execution.ID = "execution"
	execution.ContextReadiness = model.ContextReadinessUnresolved
	require.Nil(t, provider.ProjectContextUsage(execution))
	// A compaction invalidates the prior meter until native status reports anew.
	writeClaudeHookEvent(t, spool.Directory(), sessionStartEvent{SessionID: id, HookEventName: "SessionStart", Source: "compact"})
	_, err = runtime.consumeObservationEvents(context.Background(), nil)
	require.NoError(t, err)
	require.Nil(t, runtime.contextUsage)
}
