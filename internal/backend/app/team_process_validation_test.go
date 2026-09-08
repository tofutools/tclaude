package app

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/model"
)

func TestTeamProcessValidatesGuidanceAndCombinedInitialSize(t *testing.T) {
	for _, phases := range [][]model.TeamPhase{
		{{Name: "Review"}, {Name: " review "}},
		{{Name: "Review\nNow"}},
		{{Name: "Review", Roles: []string{""}}},
		{{Name: "Review", Criteria: string([]byte{0xff})}},
	} {
		require.ErrorIs(t, validateTeamPhases(model.TeamDefinition{AdvisoryProcess: phases}), ErrInvalid)
	}
	require.ErrorIs(t, validateTeamPhases(model.TeamDefinition{AdvisoryPhases: []string{"Old"}, AdvisoryProcess: []model.TeamPhase{{Name: "New"}}}), ErrInvalid)
	team := model.TeamDefinition{Members: []model.TeamMemberSpec{{Key: "worker"}}, AdvisoryProcess: []model.TeamPhase{{Name: "Review", Roles: []string{"all"}, Criteria: strings.Repeat("x", 8192)}}}
	require.NoError(t, validateTeamPhases(team))
	_, err := resolveTeamMissionBriefings(team, strings.Repeat("m", 32760), true)
	require.ErrorIs(t, err, ErrInvalid)
	_, err = resolveTeamMissionBriefings(team, "Review this change", true)
	require.NoError(t, err)
}

func TestTeamRebriefUsesCurrentWorkPatternWithoutReplacingDeployedProcess(t *testing.T) {
	team := model.TeamDefinition{Members: []model.TeamMemberSpec{{Key: "worker", BriefingIDs: []string{"work"}}}, Briefings: []model.TeamBriefing{{ID: "work", Body: "Updated work pattern", Timing: model.BriefingAfterReady}}, AdvisoryProcess: []model.TeamPhase{{Name: "Changed phase", Criteria: "Different process"}}}
	require.Equal(t, "Updated work pattern", rebriefBody("worker", team))
}
