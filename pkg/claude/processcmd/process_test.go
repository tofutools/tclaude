package processcmd

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/spf13/cobra"
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

func TestRuntimeReadCommandsExposeAskHumanFlagAndCompletion(t *testing.T) {
	for name, command := range map[string]*cobra.Command{
		"runs ls":   processRunsLsCmd(),
		"show":      processShowCmd(),
		"events":    processEventsCmd(),
		"reconcile": processReconcileCmd(),
	} {
		t.Run(name, func(t *testing.T) {
			flag := command.Flags().Lookup("ask-human")
			require.NotNil(t, flag)
			assert.Empty(t, flag.DefValue)
			assert.Contains(t, flag.Usage, "permission denial")

			complete, ok := command.GetFlagCompletionFunc("ask-human")
			require.True(t, ok)
			suggestions, _ := complete(command, nil, "3")
			assert.Contains(t, suggestions, "30s")
		})
	}
}

func TestRuntimeReadCommandsPassAskHumanAndRetryOutput(t *testing.T) {
	tests := map[string]struct {
		run      func(stdout, stderr *bytes.Buffer) error
		wantPath string
	}{
		"runs ls": {
			run: func(stdout, stderr *bytes.Buffer) error {
				return runProcessRunsLs(&processRunsLsParams{AskHuman: "30s"}, stdout, stderr)
			},
			wantPath: "/v1/process/runs",
		},
		"show": {
			run: func(stdout, stderr *bytes.Buffer) error {
				return runProcessShow("run_1", "30s", false, false, stdout, stderr)
			},
			wantPath: "/v1/process/runs/run_1",
		},
		"events": {
			run: func(stdout, stderr *bytes.Buffer) error {
				return runProcessEvents(&processEventsParams{
					RunID: "run_1", Limit: 16, PayloadBytes: defaultProcessEventPayloadDisplayBytes, AskHuman: "30s",
				}, stdout, stderr)
			},
			wantPath: "/v1/process/runs/run_1/events?limit=16",
		},
		"reconcile": {
			run: func(stdout, stderr *bytes.Buffer) error {
				return runProcessShow("run_1", "30s", true, false, stdout, stderr)
			},
			wantPath: "/v1/process/runs/run_1",
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			var gotPath string
			var gotOpts agent.DaemonOpts
			stubProcessRuntime(t, func(_ string, path string, _ any, out any, opts agent.DaemonOpts) error {
				gotPath, gotOpts = path, opts
				switch typed := out.(type) {
				case *processRunJSON:
					typed.ID = "run_1"
					typed.Action = "complete"
				}
				return nil
			})
			var stdout, stderr bytes.Buffer

			require.NoError(t, test.run(&stdout, &stderr))

			assert.Equal(t, test.wantPath, gotPath)
			assert.Equal(t, 30*time.Second, gotOpts.AskHuman)
			assert.Same(t, &stderr, gotOpts.RetryOutput)
		})
	}
}

func TestRuntimeReadCommandsRejectInvalidAskHumanBeforeRequest(t *testing.T) {
	tests := map[string]func(stdout, stderr *bytes.Buffer) error{
		"runs ls": func(stdout, stderr *bytes.Buffer) error {
			return runProcessRunsLs(&processRunsLsParams{AskHuman: "later"}, stdout, stderr)
		},
		"show": func(stdout, stderr *bytes.Buffer) error {
			return runProcessShow("run_1", "later", false, false, stdout, stderr)
		},
		"events": func(stdout, stderr *bytes.Buffer) error {
			return runProcessEvents(&processEventsParams{
				RunID: "run_1", Limit: 16, PayloadBytes: defaultProcessEventPayloadDisplayBytes, AskHuman: "later",
			}, stdout, stderr)
		},
		"reconcile": func(stdout, stderr *bytes.Buffer) error {
			return runProcessShow("run_1", "later", true, false, stdout, stderr)
		},
	}
	for name, run := range tests {
		t.Run(name, func(t *testing.T) {
			var calls int
			stubProcessRuntime(t, func(_ string, _ string, _ any, _ any, _ agent.DaemonOpts) error {
				calls++
				return nil
			})

			err := run(&bytes.Buffer{}, &bytes.Buffer{})

			require.Error(t, err)
			assert.Contains(t, err.Error(), "invalid --ask-human value")
			assert.Zero(t, calls)
		})
	}
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
	err := runProcessEvents(&processEventsParams{
		RunID: " run_daemon ", After: 4, Limit: 2,
		PayloadBytes: defaultProcessEventPayloadDisplayBytes,
	}, &stdout, &stderr)
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

func TestRuntimeShowJSONEmitsCompleteRunSnapshot(t *testing.T) {
	want := processRunJSON{
		ID: "run_json", TemplateRef: "demo@sha256:abc",
		Params: map[string]string{"branch": "main"}, ProgramAuthorizations: []string{"safe"},
		Status: "blocked", StateVersion: 7, Action: "needs_reconcile", NeedsReconcile: true,
		CreatedAt: time.Date(2026, 7, 23, 10, 0, 0, 0, time.UTC),
		UpdatedAt: time.Date(2026, 7, 23, 10, 5, 0, 0, time.UTC),
	}
	stubProcessRuntime(t, func(method, path string, in, out any, _ agent.DaemonOpts) error {
		assert.Equal(t, http.MethodGet, method)
		assert.Equal(t, "/v1/process/runs/run_json", path)
		assert.Nil(t, in)
		return copyProcessJSON(want, out)
	})

	var stdout, stderr bytes.Buffer
	require.NoError(t, runProcessShow(" run_json ", "", false, true, &stdout, &stderr))
	assert.Empty(t, stderr.String())
	expected, err := json.Marshal(want)
	require.NoError(t, err)
	assert.JSONEq(t, string(expected), stdout.String())

	var fields map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &fields))
	assert.ElementsMatch(t, []string{
		"id", "templateRef", "params", "programAuthorizations", "status", "stateVersion",
		"checkpoint", "action", "needsReconcile", "createdAt", "updatedAt",
	}, mapKeys(fields))
}

func TestRuntimeEventsJSONAndJSONLinesPreserveEventsAndCursors(t *testing.T) {
	events := []processRunEventJSON{
		{
			Sequence: 5, OccurredAt: time.Date(2026, 7, 23, 12, 34, 56, 0, time.UTC),
			NodeID: "task-01", Kind: "program_prepared",
			Payload: json.RawMessage("{\n \"command\": [\"sh\", \"-c\", \"true\"], \"ok\": true\n}"),
			Actor:   "operator",
		},
		{
			Sequence: 6, OccurredAt: time.Date(2026, 7, 23, 12, 35, 0, 0, time.UTC),
			Kind: "program_finished", Payload: json.RawMessage(`{"exitCode":0}`),
		},
	}
	next := int64(6)
	var paths []string
	stubProcessRuntime(t, func(method, path string, in, out any, _ agent.DaemonOpts) error {
		assert.Equal(t, http.MethodGet, method)
		assert.Nil(t, in)
		paths = append(paths, path)
		return copyProcessJSON(struct {
			Events []processRunEventJSON `json:"events"`
			Next   int64                 `json:"next"`
		}{Events: events, Next: next}, out)
	})

	var stdout, stderr bytes.Buffer
	require.NoError(t, runProcessEvents(&processEventsParams{
		RunID: "run_json", After: 4, Limit: 2, JSON: true,
		PayloadBytes: defaultProcessEventPayloadDisplayBytes,
	}, &stdout, &stderr))
	assert.Empty(t, stderr.String())
	var page struct {
		Events []processRunEventJSON `json:"events"`
		Next   int64                 `json:"next"`
	}
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &page))
	require.Len(t, page.Events, 2)
	assert.Equal(t, int64(6), page.Next)
	assert.JSONEq(t, `{"command":["sh","-c","true"],"ok":true}`, string(page.Events[0].Payload))

	stdout.Reset()
	require.NoError(t, runProcessEvents(&processEventsParams{
		RunID: "run_json", After: 4, Limit: 2, JSONLines: true,
		PayloadBytes: defaultProcessEventPayloadDisplayBytes,
	}, &stdout, &stderr))
	lines := strings.Split(strings.TrimSuffix(stdout.String(), "\n"), "\n")
	require.Len(t, lines, 2)
	for i, line := range lines {
		var event processRunEventJSON
		require.NoError(t, json.Unmarshal([]byte(line), &event))
		assert.Equal(t, events[i].Sequence, event.Sequence)
		assert.JSONEq(t, string(events[i].Payload), string(event.Payload))
	}
	assert.NotContains(t, stdout.String(), `"next"`)

	events = []processRunEventJSON{}
	next = 0
	stdout.Reset()
	require.NoError(t, runProcessEvents(&processEventsParams{
		RunID: "run_json", After: 6, Limit: 2, JSONLines: true,
		PayloadBytes: defaultProcessEventPayloadDisplayBytes,
	}, &stdout, &stderr))
	assert.Empty(t, stdout.String())
	assert.Equal(t, []string{
		"/v1/process/runs/run_json/events?after=4&limit=2",
		"/v1/process/runs/run_json/events?after=4&limit=2",
		"/v1/process/runs/run_json/events?after=6&limit=2",
	}, paths)
}

func TestRuntimeEventsPayloadPreviewModes(t *testing.T) {
	payload := json.RawMessage(`{"text":"` + strings.Repeat("é", 100) + `"}`)
	stubProcessRuntime(t, func(_, _ string, _, out any, _ agent.DaemonOpts) error {
		return copyProcessJSON(struct {
			Events []processRunEventJSON `json:"events"`
			Next   int64                 `json:"next"`
		}{Events: []processRunEventJSON{{
			Sequence: 1, OccurredAt: time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC),
			Kind: "evidence", Payload: payload,
		}}}, out)
	})

	command := processEventsCmd()
	flag := command.Flags().Lookup("payload-bytes")
	require.NotNil(t, flag)
	assert.Equal(t, "120", flag.DefValue)

	var stdout bytes.Buffer
	require.NoError(t, runProcessEvents(&processEventsParams{
		RunID: "run_payload", Limit: 16, PayloadBytes: defaultProcessEventPayloadDisplayBytes,
	}, &stdout, &bytes.Buffer{}))
	assert.Contains(t, stdout.String(), "PAYLOAD")
	assert.Contains(t, stdout.String(), formatProcessEventPayload(payload, defaultProcessEventPayloadDisplayBytes))

	stdout.Reset()
	require.NoError(t, runProcessEvents(&processEventsParams{
		RunID: "run_payload", Limit: 16, PayloadBytes: 0,
	}, &stdout, &bytes.Buffer{}))
	assert.NotContains(t, stdout.String(), "PAYLOAD")
	assert.NotContains(t, stdout.String(), `{"text"`)

	stdout.Reset()
	require.NoError(t, runProcessEvents(&processEventsParams{
		RunID: "run_payload", Limit: 16, PayloadBytes: 24,
	}, &stdout, &bytes.Buffer{}))
	assert.Contains(t, stdout.String(), formatProcessEventPayload(payload, 24))
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
	require.NoError(t, runProcessEvents(&processEventsParams{RunID: "run_empty", Limit: 16}, &stdout, &bytes.Buffer{}))
	assert.Equal(t, "No evidence recorded for process run run_empty.\n", stdout.String())
	assert.Equal(t, 1, calls)

	stdout.Reset()
	require.NoError(t, runProcessEvents(&processEventsParams{
		RunID: "run_empty", After: 9, Limit: 16,
	}, &stdout, &bytes.Buffer{}))
	assert.Equal(t, "No evidence after sequence 9 for process run run_empty.\n", stdout.String())
	assert.Equal(t, 2, calls)

	for name, params := range map[string]*processEventsParams{
		"json with hidden payload": {
			RunID: "run_invalid", Limit: 16, JSON: true, PayloadBytes: 0,
		},
		"json lines with custom payload": {
			RunID: "run_invalid", Limit: 16, JSONLines: true, PayloadBytes: 24,
		},
	} {
		t.Run(name, func(t *testing.T) {
			err := runProcessEvents(params, &bytes.Buffer{}, &bytes.Buffer{})
			require.Error(t, err)
			assert.Contains(t, err.Error(), "--payload-bytes is only valid for table output")
			assert.Equal(t, 2, calls, "invalid output mode must not reach the daemon")
		})
	}

	for _, params := range []*processEventsParams{
		{RunID: "", Limit: 16},
		{RunID: "run_invalid", After: -1, Limit: 16},
		{RunID: "run_invalid", Limit: 0},
		{RunID: "run_invalid", Limit: 17},
		{RunID: "run_invalid", Limit: 16, JSON: true, JSONLines: true},
		{RunID: "run_invalid", Limit: 16, PayloadBytes: -1},
		{RunID: "run_invalid", Limit: 16, PayloadBytes: maxProcessEventPayloadDisplayBytes + 1},
	} {
		err := runProcessEvents(params, &bytes.Buffer{}, &bytes.Buffer{})
		require.Error(t, err)
	}
	assert.Equal(t, 2, calls, "locally invalid inputs must not reach the daemon")
}

func TestProcessEventPayloadRenderingIsUTF8AndByteBounded(t *testing.T) {
	payload := json.RawMessage(`{"text":"` + strings.Repeat("é", 400) + `"}`)
	rendered := formatProcessEventPayload(payload, maxProcessEventPayloadDisplayBytes)
	assert.LessOrEqual(t, len(rendered), maxProcessEventPayloadDisplayBytes)
	assert.True(t, utf8.ValidString(rendered))
	assert.True(t, strings.HasSuffix(rendered, "..."))
	assert.Equal(t, "<invalid JSON>", formatProcessEventPayload(json.RawMessage(`{`), 20))

	for limit := 1; limit <= 4; limit++ {
		rendered = formatProcessEventPayload(payload, limit)
		assert.LessOrEqual(t, len(rendered), limit)
		assert.True(t, utf8.ValidString(rendered))
	}
}

func TestRuntimeJSONOutputReturnsWriteErrors(t *testing.T) {
	stubProcessRuntime(t, func(_, _ string, _, out any, _ agent.DaemonOpts) error {
		return copyProcessJSON(struct {
			Events []processRunEventJSON `json:"events"`
			Next   int64                 `json:"next"`
		}{Events: []processRunEventJSON{{
			Sequence: 1, Kind: "evidence", Payload: json.RawMessage(`{"ok":true}`),
		}}}, out)
	})

	err := runProcessEvents(&processEventsParams{
		RunID: "run_write_error", Limit: 16, JSON: true,
		PayloadBytes: defaultProcessEventPayloadDisplayBytes,
	}, processFailingWriter{}, &bytes.Buffer{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "write failed")
}

type processFailingWriter struct{}

func (processFailingWriter) Write([]byte) (int, error) {
	return 0, errors.New("write failed")
}

func stubProcessRuntime(
	t *testing.T,
	request func(method, path string, in, out any, opts agent.DaemonOpts) error,
) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	require.NoError(t, config.Save(&config.Config{Features: &config.FeaturesConfig{Processes: true}}))
	previousAvailable := agent.DaemonAvailableImpl
	agent.DaemonAvailableImpl = func() bool { return true }
	t.Cleanup(func() { agent.DaemonAvailableImpl = previousAvailable })
	previousRequest := agent.DaemonRequestImpl
	agent.DaemonRequestImpl = request
	t.Cleanup(func() { agent.DaemonRequestImpl = previousRequest })
}

func copyProcessJSON(value, out any) error {
	encoded, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return json.Unmarshal(encoded, out)
}

func mapKeys[V any](values map[string]V) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	return keys
}
