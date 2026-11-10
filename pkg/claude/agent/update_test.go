package agent

import (
	"bytes"
	"encoding/json"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/selfupdate"
	"testing"
	"time"
)

func TestUpdateCLIRoutesValidationAndJobOutput(t *testing.T) {
	oldAvail, oldReq := DaemonAvailableImpl, DaemonRequestImpl
	t.Cleanup(func() { DaemonAvailableImpl, DaemonRequestImpl = oldAvail, oldReq })
	DaemonAvailableImpl = func() bool { return true }
	calls := 0
	DaemonRequestImpl = func(method, path string, in, out any, opts DaemonOpts) error {
		calls++
		require.True(t, opts.NoRetry)
		require.Equal(t, 20*time.Second, opts.Timeout)
		require.Equal(t, "POST", method)
		require.Equal(t, "/v1/federation/peer/bob%20node/node/update", path)
		require.Equal(t, selfupdate.Request{Action: "apply", Version: "v1.2.3"}, in)
		return json.Unmarshal([]byte(`{"id":"0123456789abcdef0123456789abcdef","action":"apply","version":"v1.2.3","state":"running","warnings":["source\u001b[31m\nwarning"],"peer_view":{"included":["node.update"]}}`), out)
	}
	var stdout, stderr bytes.Buffer
	require.Equal(t, rcInvalidArg, runUpdate(&updateParams{Apply: true, Rollback: true}, &stdout, &stderr))
	require.Zero(t, calls)
	require.Equal(t, rcInvalidArg, runUpdate(&updateParams{Apply: true, Version: "latest"}, &stdout, &stderr))
	require.Zero(t, calls)
	stdout.Reset()
	stderr.Reset()
	require.Zero(t, runUpdate(&updateParams{Node: "bob node", Apply: true, Version: "v1.2.3", NoWait: true, JSON: true}, &stdout, &stderr))
	require.Contains(t, stdout.String(), "peer_view")
	require.NotContains(t, stderr.String(), "\x1b")
	DaemonRequestImpl = func(method, path string, in, out any, opts DaemonOpts) error {
		require.Equal(t, "GET", method)
		require.Equal(t, "/v1/node/update/jobs/0123456789abcdef0123456789abcdef", path)
		return json.Unmarshal([]byte(`{"id":"0123456789abcdef0123456789abcdef","action":"check","version":"v1.2.3","current_version":"v1.2.2","update_available":true,"state":"succeeded"}`), out)
	}
	stdout.Reset()
	stderr.Reset()
	require.Zero(t, runUpdate(&updateParams{Job: "0123456789abcdef0123456789abcdef"}, &stdout, &stderr))
	require.Contains(t, stdout.String(), "update available: true")
}

func TestUpdateCLIDryRunPrintsThePlan(t *testing.T) {
	oldAvail, oldReq := DaemonAvailableImpl, DaemonRequestImpl
	t.Cleanup(func() { DaemonAvailableImpl, DaemonRequestImpl = oldAvail, oldReq })
	DaemonAvailableImpl = func() bool { return true }
	ready := false
	DaemonRequestImpl = func(method, path string, in, out any, opts DaemonOpts) error {
		require.Equal(t, "GET", method)
		require.Equal(t, "/v1/federation/peer/bob/node/update/preflight", path)
		body, _ := json.Marshal(selfupdate.Plan{Method: "custom", Ready: ready, CurrentVersion: "v1.0.0",
			Checks:     []selfupdate.PlanCheck{{Name: "Command", State: "fail", Detail: "No custom command is set"}},
			Steps:      []string{"Run the node's custom command"},
			NotChecked: []string{"What the custom command does"}})
		return json.Unmarshal(body, out)
	}
	var stdout, stderr bytes.Buffer
	require.Equal(t, rcInvalidArg, runUpdate(&updateParams{DryRun: true, Apply: true}, &stdout, &stderr))
	require.Equal(t, rcIOFailure, runUpdate(&updateParams{Node: "bob", DryRun: true}, &stdout, &stderr), "a failed check exits non-zero")
	out := stdout.String()
	require.Contains(t, out, "custom: would fail")
	require.Contains(t, out, "[FAIL] Command: No custom command is set")
	require.Contains(t, out, "1. Run the node's custom command")
	require.Contains(t, out, "Not checked:")
	ready = true
	stdout.Reset()
	require.Zero(t, runUpdate(&updateParams{Node: "bob", DryRun: true}, &stdout, &stderr))
	require.Contains(t, stdout.String(), "custom: ready")
}
