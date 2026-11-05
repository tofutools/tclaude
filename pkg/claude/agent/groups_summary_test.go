package agent

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// `groups ls --json` decodes the daemon listing into groupSummary and prints
// it again, so a field the struct lacks is silently lost on the way through.
func TestGroupSummaryKeepsAutoJoinFields(t *testing.T) {
	var g groupSummary
	require.NoError(t, json.Unmarshal([]byte(`{"name":"kivra","default_cwd":"/repo","default_spawn_group":true}`), &g))
	out, err := json.Marshal(g)
	require.NoError(t, err)
	assert.Contains(t, string(out), `"default_cwd":"/repo"`)
	assert.Contains(t, string(out), `"default_spawn_group":true`)
}
