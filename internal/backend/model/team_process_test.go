package model

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestTeamProcessReadsLegacyPhaseNamesWithoutRewritingAuthoredJSON(t *testing.T) {
	legacy := TeamDefinition{AdvisoryPhases: []string{"Investigate", "Review"}}
	before, err := json.Marshal(legacy)
	require.NoError(t, err)
	require.Equal(t, []TeamPhase{{Name: "Investigate"}, {Name: "Review"}}, legacy.ProcessPhases())
	after, err := json.Marshal(legacy)
	require.NoError(t, err)
	require.Equal(t, string(before), string(after))
	require.NotContains(t, string(after), "AdvisoryProcess")
}
