package agent

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRunDirRepairUsesSelfOnlyEndpoint(t *testing.T) {
	prevAvail := DaemonAvailableImpl
	prevReq := DaemonRequestImpl
	t.Cleanup(func() {
		DaemonAvailableImpl = prevAvail
		DaemonRequestImpl = prevReq
	})
	DaemonAvailableImpl = func() bool { return true }

	var method, path string
	DaemonRequestImpl = func(gotMethod, gotPath string, _ /*in*/, out any, _ DaemonOpts) error {
		method, path = gotMethod, gotPath
		resp := out.(*struct {
			Dir      string `json:"dir"`
			Repaired bool   `json:"repaired"`
		})
		resp.Dir = "/tmp/agent-startup"
		resp.Repaired = true
		return nil
	}

	var stdout, stderr bytes.Buffer
	rc := runDir(&dirParams{Repair: true}, &stdout, &stderr)
	require.Equal(t, rcOK, rc, "stderr=%q", stderr.String())
	assert.Equal(t, "POST", method)
	assert.Equal(t, "/v1/whoami/dir/repair", path)
	assert.Contains(t, stdout.String(), "Recreated startup directory: /tmp/agent-startup")
}

func TestRunDirRepairRejectsAnyAlternateTarget(t *testing.T) {
	for _, params := range []*dirParams{
		{Repair: true, Selector: "another-agent"},
		{Repair: true, Open: true},
		{Repair: true, Start: true},
		{Repair: true, Worktree: true},
	} {
		var stdout, stderr bytes.Buffer
		rc := runDir(params, &stdout, &stderr)
		assert.Equal(t, rcInvalidArg, rc)
		assert.Contains(t, stderr.String(), "targets only this agent's recorded startup directory")
	}
}
