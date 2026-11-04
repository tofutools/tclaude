package agentd_test

import (
	"bytes"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// `tclaude agent spawn --profile X` sends only the flags the user typed, and
// the daemon resolves X itself, so X's values are the agent's spawn copy and
// not its explicit choices. A flag typed on top stays explicit. Without this a
// re-applied copy could never change a CLI-spawned agent.
func TestCLIProfileValuesAreNotRecordedAsExplicit(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")
	rec := createProfile(t, f, map[string]any{"name": "kit", "role": "qa",
		"ask_user_question_timeout": "5m", "include_group_default_context": false})
	require.Equalf(t, http.StatusCreated, rec.Code, "create profile body=%s", rec.Body.String())
	bridgeAgentClientToMux(t, f.Mux)
	chdirTo(t, resolveSym(t, t.TempDir()))

	for _, tc := range []struct {
		params       agent.SpawnParams
		wantExplicit []string
	}{
		// The CLI always sends the caller's directory, and --profile itself is
		// the user's choice of copy; both are explicit.
		{agent.SpawnParams{Group: "alpha", Name: "copied", Profile: "kit"},
			[]string{"directory", "spawn_profile"}},
		{agent.SpawnParams{Group: "alpha", Name: "flagged", Profile: "kit", Model: "sonnet"},
			[]string{"directory", "spawn_profile", "model"}},
	} {
		stderr := new(bytes.Buffer)
		resp, rc := agent.RunSpawn(&tc.params, new(bytes.Buffer), stderr, new(bytes.Buffer))
		require.Equal(t, 0, rc, "RunSpawn stderr=%s", stderr.String())
		require.NotNil(t, resp)

		got, ok := f.World.SpawnAskTimeout(resp.ConvID)
		require.True(t, ok)
		assert.Equal(t, "5m", got, "%s: the profile still decides the launch", tc.params.Name)

		layers, err := db.AgentLaunchLayersForConv(resp.ConvID)
		require.NoError(t, err)
		require.NotNil(t, layers)
		var explicit []string
		for key := range layers.Explicit {
			explicit = append(explicit, key)
		}
		assert.ElementsMatchf(t, tc.wantExplicit, explicit, "%s: explicit launch keys", tc.params.Name)
		require.NotNil(t, layers.SpawnCopy)
		require.Len(t, layers.SpawnCopy.Tiers, 1)
		assert.Equal(t, "kit", layers.SpawnCopy.Tiers[0].ProfileName)
	}
}
