package agent

import (
	"bytes"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCronCLI_AddSpawnContract(t *testing.T) {
	prevAvail, prevReq := DaemonAvailableImpl, DaemonRequestImpl
	t.Cleanup(func() { DaemonAvailableImpl, DaemonRequestImpl = prevAvail, prevReq })
	DaemonAvailableImpl = func() bool { return true }
	var sent map[string]any
	DaemonRequestImpl = func(method, path string, body, out any, _ DaemonOpts) error {
		require.Equal(t, http.MethodPost, method)
		require.Equal(t, "/v1/cron", path)
		sent = body.(map[string]any)
		resp := out.(*cronJobJSON)
		*resp = cronJobJSON{ID: 7, TargetKind: "group", GroupName: "maintainers", IntervalSeconds: 8 * 3600,
			ActionKind: "spawn", SpawnProfile: "flaky-fixer", SpawnConcurrencyPolicy: "Allow",
			SpawnMaxLiveWorkers: 2, SpawnWorkerDeadlineSeconds: 7200}
		return nil
	}
	var stdout, stderr bytes.Buffer
	rc := runCronAdd(&cronAddParams{Action: "spawn", Target: "group:maintainers", Interval: "8h",
		SpawnProfile: "flaky-fixer", SpawnRoles: []string{"builder", "linear"},
		SpawnNameTemplate: "flaky-scanner-{{fire_time}}", Instruction: "fix one flaky ticket",
		Concurrency: "Allow", MaxLiveWorkers: 2, WorkerDeadline: "2h"},
		bytes.NewReader(nil), &stdout, &stderr)
	require.Equal(t, rcOK, rc, stderr.String())
	assert.Equal(t, "spawn", sent["action_kind"])
	assert.Equal(t, "fix one flaky ticket", sent["spawn_instruction_template"])
	assert.Equal(t, []string{"builder", "linear"}, sent["spawn_roles"])
	assert.Equal(t, int64(7200), sent["spawn_worker_deadline_seconds"])
	assert.Contains(t, stdout.String(), "Spawn profile \"flaky-fixer\"")
}

func TestCronCLI_SpawnRejectsSubsecondDeadline(t *testing.T) {
	var stdout, stderr bytes.Buffer
	rc := runCronAdd(&cronAddParams{Action: "spawn", Target: "group:maintainers", Interval: "8h",
		SpawnProfile: "scanner", Instruction: "scan", WorkerDeadline: "500ms"},
		bytes.NewReader(nil), &stdout, &stderr)
	assert.Equal(t, rcInvalidArg, rc)
	assert.Contains(t, stderr.String(), "between 1s and 8760h")
}

func TestCronCLI_RetiredOwnerDenialIsAnError(t *testing.T) {
	prevAvail, prevReq := DaemonAvailableImpl, DaemonRequestImpl
	t.Cleanup(func() { DaemonAvailableImpl, DaemonRequestImpl = prevAvail, prevReq })
	DaemonAvailableImpl = func() bool { return true }
	DaemonRequestImpl = func(method, path string, _, _ any, _ DaemonOpts) error {
		require.Equal(t, http.MethodPost, method)
		assert.Contains(t, []string{"/v1/cron/7/enable", "/v1/cron/7/run-now"}, path)
		return &DaemonError{
			Status: http.StatusConflict,
			Code:   "not_runnable",
			Msg:    "cron job owner is retired; the requested action was not applied",
		}
	}

	for _, tc := range []struct {
		name string
		run  func(stdout, stderr *bytes.Buffer) int
	}{
		{name: "enable", run: func(stdout, stderr *bytes.Buffer) int {
			return runCronEnable(&cronIDOnlyParams{ID: "7"}, true, stdout, stderr)
		}},
		{name: "run now", run: func(stdout, stderr *bytes.Buffer) int {
			return runCronRunNow(&cronIDOnlyParams{ID: "7"}, stdout, stderr)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			rc := tc.run(&stdout, &stderr)
			assert.NotEqual(t, rcOK, rc)
			assert.Empty(t, stdout.String(), "a denied mutation must not print success")
			assert.Equal(t,
				"Error: cron job owner is retired; the requested action was not applied\n",
				stderr.String())
		})
	}
}
