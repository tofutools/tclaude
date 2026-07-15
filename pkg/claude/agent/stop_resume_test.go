package agent

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRunStopPassesAskHumanToDaemon(t *testing.T) {
	prevAvail, prevReq := DaemonAvailableImpl, DaemonRequestImpl
	t.Cleanup(func() { DaemonAvailableImpl, DaemonRequestImpl = prevAvail, prevReq })
	DaemonAvailableImpl = func() bool { return true }

	var gotOpts DaemonOpts
	DaemonRequestImpl = func(method, path string, _ any, out any, opts DaemonOpts) error {
		assert.Equal(t, http.MethodPost, method)
		assert.Equal(t, "/v1/agent/worker/stop", path)
		gotOpts = opts
		return json.Unmarshal([]byte(`{"conv_id":"worker-conv","action":"soft_stopped"}`), out)
	}

	var stdout, stderr bytes.Buffer
	rc := runStop(&stopParams{Selector: "worker", AskHuman: "30s"}, &stdout, &stderr)
	require.Equal(t, rcOK, rc, "stderr=%s", stderr.String())
	assert.Equal(t, 30*time.Second, gotOpts.AskHuman)
	assert.Contains(t, stdout.String(), "Waiting up to 30s for human approval")
}

func TestRunResumeAnswersWriteProofChallenge(t *testing.T) {
	prevAvail, prevReq := DaemonAvailableImpl, DaemonRequestImpl
	t.Cleanup(func() { DaemonAvailableImpl, DaemonRequestImpl = prevAvail, prevReq })
	DaemonAvailableImpl = func() bool { return true }

	dir := t.TempDir()
	const token = "resume-proof-123"
	filename := ".tclaude-write-proof-" + token
	challenge, err := json.Marshal(map[string]any{
		"code": WriteProofRequiredCode,
		"write_proof": map[string]any{
			"token": token, "filename": filename, "dirs": []string{dir},
		},
	})
	require.NoError(t, err)

	calls := 0
	DaemonRequestImpl = func(method, path string, in, out any, _ DaemonOpts) error {
		calls++
		assert.Equal(t, http.MethodPost, method)
		assert.Equal(t, "/v1/agent/worker/resume", path)
		body, ok := in.(map[string]any)
		require.True(t, ok)
		if calls == 1 {
			assert.Empty(t, body)
			return &DaemonError{Status: http.StatusForbidden, Code: WriteProofRequiredCode, Raw: challenge}
		}
		assert.Equal(t, token, body["write_proof_token"])
		return json.Unmarshal([]byte(`{"conv_id":"worker-conv","action":"resumed"}`), out)
	}

	var stdout, stderr bytes.Buffer
	rc := runResume(&resumeParams{Selector: "worker"}, &stdout, &stderr)
	require.Equal(t, rcOK, rc, "stderr=%s", stderr.String())
	assert.Equal(t, 2, calls)
	assert.Contains(t, stdout.String(), "worker-c: resumed")
	assert.FileExists(t, filepath.Join(dir, filename))
	require.NoError(t, os.Remove(filepath.Join(dir, filename)))
}
