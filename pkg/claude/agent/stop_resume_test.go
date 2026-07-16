package agent

import (
	"bytes"
	"encoding/json"
	"net/http"
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

func TestRunResumePassesAskHumanToDaemon(t *testing.T) {
	prevAvail, prevReq := DaemonAvailableImpl, DaemonRequestImpl
	t.Cleanup(func() { DaemonAvailableImpl, DaemonRequestImpl = prevAvail, prevReq })
	DaemonAvailableImpl = func() bool { return true }

	var gotOpts DaemonOpts
	DaemonRequestImpl = func(method, path string, in, out any, opts DaemonOpts) error {
		assert.Equal(t, http.MethodPost, method)
		assert.Equal(t, "/v1/agent/worker/resume", path)
		assert.Nil(t, in)
		gotOpts = opts
		return json.Unmarshal([]byte(`{"conv_id":"worker-conv","action":"resumed"}`), out)
	}

	var stdout, stderr bytes.Buffer
	rc := runResume(&resumeParams{Selector: "worker", AskHuman: "30s"}, &stdout, &stderr)
	require.Equal(t, rcOK, rc, "stderr=%s", stderr.String())
	assert.Equal(t, 30*time.Second, gotOpts.AskHuman)
	assert.Contains(t, stdout.String(), "worker-c: resumed")
	assert.NotContains(t, stdout.String(), "Waiting up to",
		"resume must not claim an approval request exists before the daemon actually denies")
}

func TestResumeCmdExposesAskHumanFlag(t *testing.T) {
	flag := resumeCmd().Flags().Lookup("ask-human")
	require.NotNil(t, flag)
	assert.Contains(t, flag.Usage, "permission denial")
}

func TestRunGroupsResumePassesAskHumanWithoutCallerProof(t *testing.T) {
	prevAvail, prevReq := DaemonAvailableImpl, DaemonRequestImpl
	t.Cleanup(func() { DaemonAvailableImpl, DaemonRequestImpl = prevAvail, prevReq })
	DaemonAvailableImpl = func() bool { return true }

	var gotOpts DaemonOpts
	DaemonRequestImpl = func(method, path string, in, out any, opts DaemonOpts) error {
		assert.Equal(t, http.MethodPost, method)
		assert.Equal(t, "/v1/groups/team/resume", path)
		assert.Nil(t, in)
		gotOpts = opts
		return json.Unmarshal([]byte(`{"group":"team","action":"resume","members":[{"conv_id":"worker-conv","action":"resumed"}]}`), out)
	}

	var stdout, stderr bytes.Buffer
	rc := runGroupsResume(&groupsResumeParams{Name: "team", AskHuman: "30s"}, &stdout, &stderr)
	require.Equal(t, rcOK, rc, "stderr=%s", stderr.String())
	assert.Equal(t, 30*time.Second, gotOpts.AskHuman)
	assert.Contains(t, stdout.String(), "resumed")
	assert.NotContains(t, stdout.String(), "Waiting up to")
}
