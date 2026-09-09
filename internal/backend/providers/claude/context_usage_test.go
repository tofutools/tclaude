package claude

import (
	"context"
	"encoding/json"
	"fmt"
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

func TestMain(m *testing.M) {
	if len(os.Args) == 2 && os.Args[1] == StatusLineCommand {
		if err := RunStatusLine(os.Stdin, os.Stdout, os.Getenv("TCLAUDE_OBSERVATION_SPOOL"), os.Getenv(model.AutoCompactWindowEnvVar)); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

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
	command.Env = append(os.Environ(), "TCLAUDE_OBSERVATION_SPOOL="+spool.Directory(), model.AutoCompactWindowEnvVar+"=450000")
	command.Stdin = strings.NewReader(`{"session_id":"` + id + `","context_window":{"context_window_size":1000000,"used_percentage":21}}`)
	output, err := command.CombinedOutput()
	require.NoError(t, err, string(output))
	require.Contains(t, string(output), "47%")
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

func TestAutoCompactWindowAmbientStatusSurvivesEvidenceRecovery(t *testing.T) {
	for _, selected := range []string{"", "450000"} {
		t.Run("admitted_"+selected, func(t *testing.T) {
			root := t.TempDir()
			spool, err := host.PrepareObservationSpool(filepath.Join(root, "events"))
			require.NoError(t, err)
			id := "43e874eb-4827-4b22-b1b8-376a5e5e553f"
			raw := `{"session_id":"` + id + `","model":{"display_name":"Opus","id":"claude-opus"},"context_window":{"context_window_size":1000000,"used_percentage":21},"cost":{"total_cost_usd":1.25},"effort":{"level":"high"},"workspace":{"current_dir":"/workspace/project"}}`
			command := exec.Command("/bin/sh", "-c", claudeStatusLineCommand())
			command.Env = append(os.Environ(), "TCLAUDE_OBSERVATION_SPOOL="+spool.Directory(), model.AutoCompactWindowEnvVar+"=450000")
			command.Stdin = strings.NewReader(raw)
			output, err := command.CombinedOutput()
			require.NoError(t, err, string(output))
			for _, expected := range []string{"Opus", "450k", "47%", "$1.25", "high", "/workspace/project"} {
				require.Contains(t, string(output), expected)
			}
			pending, err := spool.ReadPending()
			require.NoError(t, err)
			require.Len(t, pending, 1)
			runtime := &Runtime{executionID: "execution", attempt: 1, nativeID: id, contextReady: true, observations: &observationSink{}, spool: spool, autoCompactWindow: model.AutoCompactWindow(selected)}
			_, err = runtime.consumeObservationEvents(context.Background(), nil)
			require.NoError(t, err)
			require.NotNil(t, runtime.contextUsage)
			require.Equal(t, int64(450000), runtime.contextUsage.EffectiveWindow)
			require.InDelta(t, 46.6666667, runtime.contextUsage.UsedPercent, 0.00001)
			persisted, err := encodeEvidence(evidence{ExecutionID: "execution", NativeID: id, ContextReady: true, ContextUsage: runtime.contextUsage})
			require.NoError(t, err)
			// Recovery reads the effective native observation, even with no authored pin.
			recovered, err := decodeEvidence(persisted)
			require.NoError(t, err)
			require.Equal(t, runtime.contextUsage, recovered.ContextUsage)
			execution := model.Execution{ID: "execution", ContextReadiness: model.ContextReadinessReady, Evidence: persisted, NativeConversation: nativeEvidence(id)}
			require.Equal(t, recovered.ContextUsage, (&Provider{}).ProjectContextUsage(execution))
		})
	}
}

func TestAutoCompactWindowStatusLimitsUnknownAndInvalidPayload(t *testing.T) {
	var out strings.Builder
	root := t.TempDir()
	spool, err := host.PrepareObservationSpool(filepath.Join(root, "events"))
	require.NoError(t, err)
	raw := `{"session_id":"session","model":{"display_name":"Opus"},"context_window":{"context_window_size":1000000,"used_percentage":21},"rate_limits":{"five_hour":{"used_percentage":25,"resets_at":4102444800},"seven_day":{"used_percentage":50,"resets_at":4102444800}},"cost":{"total_cost_usd":9}}`
	require.NoError(t, RunStatusLine(strings.NewReader(raw), &out, spool.Directory(), "invalid"))
	require.Contains(t, out.String(), "context unknown")
	require.Contains(t, out.String(), "5h [")
	require.Contains(t, out.String(), "7d [")
	require.NotContains(t, out.String(), "$9")
	pending, err := spool.ReadPending()
	require.NoError(t, err)
	require.Len(t, pending, 1)
	require.Nil(t, parseContextUsage(pending[0].Payload, "session", "", time.Now()))
	for _, invalid := range []string{"null", "[]", "{", strings.Repeat(" ", 1<<20)} {
		require.Error(t, RunStatusLine(strings.NewReader(invalid), &out, spool.Directory(), ""))
	}
	require.NotContains(t, statusText("a\x1b[2J\nb"), "\x1b")
}
