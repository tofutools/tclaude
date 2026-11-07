package agent

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// On the command line a permission denial names --ask-human, not the API's
// header.
func TestDaemonErrorNamesTheAskHumanFlag(t *testing.T) {
	err := &DaemonError{Status: 403, Msg: `caller is not granted permission "taskboard.write" (grant via …; or call again with X-Tclaude-Ask-Human: <duration> to ask the human via popup)`}
	assert.Contains(t, err.Error(), "or call again with --ask-human <duration> to ask the human")
	assert.NotContains(t, err.Error(), "X-Tclaude-Ask-Human")
}

// A scoped grant shows its scope in the effective list.
func TestPermissionsGrantShowsTheScope(t *testing.T) {
	prevAvail, prevReq := DaemonAvailableImpl, DaemonRequestImpl
	t.Cleanup(func() { DaemonAvailableImpl, DaemonRequestImpl = prevAvail, prevReq })
	DaemonAvailableImpl = func() bool { return true }
	DaemonRequestImpl = func(_, _ string, _, out any, _ DaemonOpts) error {
		*out.(*permissionsMutateResp) = permissionsMutateResp{Target: "opsbot", Slug: "taskboard.read",
			Scope: permissionScope{"group": {"crew"}}, Effective: []string{"taskboard.read", "self.rename"}}
		return nil
	}
	var stdout, stderr bytes.Buffer
	rc := runPermissionsMutate("/v1/permissions/grant", "Granted", "opsbot", "taskboard.read",
		[]string{"group=crew"}, "", &stdout, &stderr)
	require.Equal(t, rcOK, rc, stderr.String())
	assert.Contains(t, stdout.String(), "Effective: self.rename, taskboard.read [group=crew]")
}
