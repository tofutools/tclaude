package agent

import (
	"bytes"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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
