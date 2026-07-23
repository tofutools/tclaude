package processcmd

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
)

func TestRuntimeMVPVerbsAndDeferredVerbsRemainDiscoverable(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	require.NoError(t, config.Save(&config.Config{
		Features: &config.FeaturesConfig{Processes: true},
	}))

	want := []string{"advance", "apply", "events", "observe", "preview", "reconcile", "record-outcome", "reissue", "repair", "report", "resolve", "resume", "run", "runs", "show", "unblock", "verify", "worklist"}
	root := Cmd()
	for _, name := range want {
		command, _, err := root.Find([]string{name})
		require.NoError(t, err, name)
		assert.Equal(t, name, command.Name())
	}

	root = Cmd()
	root.SilenceUsage = true
	root.SilenceErrors = true
	root.SetArgs([]string{"advance", "legacy-run", "--store-root", t.TempDir()})
	err := root.Execute()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "temporarily unavailable: no engine is installed")
}

func TestRuntimeMutationUsesDaemonAndNeverOpensSQLite(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	require.NoError(t, config.Save(&config.Config{Features: &config.FeaturesConfig{Processes: true}}))
	previousAvailable := agent.DaemonAvailableImpl
	agent.DaemonAvailableImpl = func() bool { return true }
	t.Cleanup(func() { agent.DaemonAvailableImpl = previousAvailable })
	previousRequest := agent.DaemonRequestImpl
	var method, path string
	var body any
	agent.DaemonRequestImpl = func(gotMethod, gotPath string, in, out any, _ agent.DaemonOpts) error {
		method, path, body = gotMethod, gotPath, in
		encoded, err := json.Marshal(processRunJSON{ID: "run_daemon", TemplateRef: "demo@sha256:abc", Status: "running", Action: "executing", StateVersion: 2})
		if err != nil {
			return err
		}
		return json.Unmarshal(encoded, out)
	}
	t.Cleanup(func() { agent.DaemonRequestImpl = previousRequest })

	var stdout, stderr bytes.Buffer
	err := runProcessRun(&processRunParams{
		TemplateID: "demo", Params: []string{"branch=main"}, Authorize: []string{"safe"},
	}, &stdout, &stderr)
	require.NoError(t, err)
	assert.Equal(t, http.MethodPost, method)
	assert.Equal(t, "/v1/process/runs", path)
	assert.Contains(t, stdout.String(), "run_daemon")
	assert.Contains(t, stdout.String(), "tclaude process show run_daemon")
	assert.Contains(t, stdout.String(), "tclaude process events run_daemon")
	encodedBody, err := json.Marshal(body)
	require.NoError(t, err)
	assert.JSONEq(t, `{"templateId":"demo","params":{"branch":"main"},"authorizeProgramProfiles":["safe"]}`, string(encodedBody))
	_, err = os.Stat(filepath.Join(home, ".tclaude", "data", "db.sqlite"))
	assert.ErrorIs(t, err, os.ErrNotExist, "runtime CLI must not open SQLite")
}

func TestRuntimeVerbIsDiscoverableAndFeatureGatedByDefault(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	root := Cmd()
	assert.False(t, root.Hidden)
	command, _, err := root.Find([]string{"run"})
	require.NoError(t, err)
	assert.Equal(t, "run", command.Name())
	err = runProcessRun(&processRunParams{TemplateID: "saved-template"}, &bytes.Buffer{}, &bytes.Buffer{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "features.processes=true")
}

func TestRuntimeEventsUsesBoundedDaemonPageAndRendersPayload(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	require.NoError(t, config.Save(&config.Config{Features: &config.FeaturesConfig{Processes: true}}))
	previousAvailable := agent.DaemonAvailableImpl
	agent.DaemonAvailableImpl = func() bool { return true }
	t.Cleanup(func() { agent.DaemonAvailableImpl = previousAvailable })
	previousRequest := agent.DaemonRequestImpl
	var method, path string
	agent.DaemonRequestImpl = func(gotMethod, gotPath string, in, out any, _ agent.DaemonOpts) error {
		method, path = gotMethod, gotPath
		assert.Nil(t, in)
		response := struct {
			Events []processRunEventJSON `json:"events"`
			Next   int64                 `json:"next"`
		}{
			Events: []processRunEventJSON{
				{
					Sequence: 5, OccurredAt: time.Date(2026, 7, 23, 12, 34, 56, 0, time.Local),
					Kind: "run_created", Payload: json.RawMessage("{\n \"message\": \"hello\", \"ok\": true\n}"),
					Actor: "operator",
				},
				{
					Sequence: 6, OccurredAt: time.Date(2026, 7, 23, 12, 35, 0, 0, time.Local),
					NodeID: "task-01", Kind: "program_prepared", Payload: json.RawMessage(`{"command":"true"}`),
				},
			},
			Next: 6,
		}
		encoded, err := json.Marshal(response)
		if err != nil {
			return err
		}
		return json.Unmarshal(encoded, out)
	}
	t.Cleanup(func() { agent.DaemonRequestImpl = previousRequest })

	var stdout, stderr bytes.Buffer
	err := runProcessEvents(&processEventsParams{RunID: " run_daemon ", After: 4, Limit: 2}, &stdout, &stderr)
	require.NoError(t, err)
	assert.Equal(t, http.MethodGet, method)
	assert.Equal(t, "/v1/process/runs/run_daemon/events?after=4&limit=2", path)
	output := stdout.String()
	assert.Contains(t, output, "SEQ")
	assert.Contains(t, output, "run_created")
	assert.Contains(t, output, `{"message":"hello","ok":true}`)
	assert.Contains(t, output, "task-01")
	assert.Contains(t, output, "next=6")
}

func TestRuntimeEventsEmptyAndInvalidInputs(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	require.NoError(t, config.Save(&config.Config{Features: &config.FeaturesConfig{Processes: true}}))
	previousAvailable := agent.DaemonAvailableImpl
	agent.DaemonAvailableImpl = func() bool { return true }
	t.Cleanup(func() { agent.DaemonAvailableImpl = previousAvailable })
	previousRequest := agent.DaemonRequestImpl
	var calls int
	agent.DaemonRequestImpl = func(_, _ string, _, _ any, _ agent.DaemonOpts) error {
		calls++
		return nil
	}
	t.Cleanup(func() { agent.DaemonRequestImpl = previousRequest })

	var stdout bytes.Buffer
	require.NoError(t, runProcessEvents(&processEventsParams{RunID: "run_empty", Limit: 32}, &stdout, &bytes.Buffer{}))
	assert.Equal(t, "No evidence recorded for process run run_empty.\n", stdout.String())
	assert.Equal(t, 1, calls)

	for _, params := range []*processEventsParams{
		{RunID: "", Limit: 32},
		{RunID: "run_invalid", After: -1, Limit: 32},
		{RunID: "run_invalid", Limit: 0},
		{RunID: "run_invalid", Limit: 257},
	} {
		err := runProcessEvents(params, &bytes.Buffer{}, &bytes.Buffer{})
		require.Error(t, err)
	}
	assert.Equal(t, 1, calls, "locally invalid inputs must not reach the daemon")
}

func TestProcessEventPayloadRenderingIsUTF8AndByteBounded(t *testing.T) {
	payload := json.RawMessage(`{"text":"` + strings.Repeat("é", 400) + `"}`)
	rendered := formatProcessEventPayload(payload)
	assert.LessOrEqual(t, len(rendered), maxProcessEventPayloadDisplayBytes)
	assert.True(t, utf8.ValidString(rendered))
	assert.True(t, strings.HasSuffix(rendered, "..."))
	assert.Equal(t, "<invalid JSON>", formatProcessEventPayload(json.RawMessage(`{`)))
}
