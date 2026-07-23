package processcmd

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"

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

	want := []string{"advance", "apply", "observe", "preview", "reconcile", "record-outcome", "reissue", "repair", "report", "resolve", "resume", "run", "runs", "show", "unblock", "verify", "worklist"}
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
