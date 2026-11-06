package agent

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// tcl-xrs9v5: holding the lock in a group does not reach a template pinned to
// another group too, and resources ls says which.
func TestResourcesLsNamesTheTemplatesTheGroupsLockDoesNotReach(t *testing.T) {
	var resp groupResourcesResp
	require.NoError(t, json.Unmarshal([]byte(`{"group":"crew","canLock":true,"processes":[
		{"templateId":"ship","name":"Ship","canLock":true},
		{"templateId":"shared","name":"Shared","canLock":false}]}`), &resp))
	var out bytes.Buffer
	printGroupResources(&out, resp)
	assert.Contains(t, out.String(), "You hold groups.resources.lock here")
	assert.Contains(t, out.String(), "Not process shared: also pinned to a group where you do not hold it.")
	assert.NotContains(t, out.String(), "Not process ship")

	resp.Processes[1].CanLock = true
	out.Reset()
	printGroupResources(&out, resp)
	assert.NotContains(t, out.String(), "Not process")

	resp.CanLock = false
	out.Reset()
	printGroupResources(&out, resp)
	assert.NotContains(t, out.String(), "groups.resources.lock")
}
