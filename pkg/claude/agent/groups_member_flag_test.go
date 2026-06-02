package agent

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// A single multi-field --member spec must survive cobra parsing intact —
// the boa StringSlice comma-split regression (rationale in groups.go's
// groupsCreateCmd InitFuncCtx). Pins that the whole spec reaches
// parseMemberSpec as one string with all four fields set.
func TestMemberFlag_MultiFieldSpecParsesAsOneMember(t *testing.T) {
	cmd := groupsCreateCmd()

	err := cmd.Flags().Parse([]string{
		"--member", "name=lead,role=tech-lead,descr=Owns the diff,cwd=/tmp/x",
	})
	require.NoError(t, err, "parsing --member")

	got, err := cmd.Flags().GetStringArray("member")
	require.NoError(t, err, "--member must be a StringArray flag")
	require.Len(t, got, 1, "comma-separated pairs must stay in ONE --member value, not be split")

	spec, err := parseMemberSpec(got[0])
	require.NoError(t, err, "parseMemberSpec on the intact spec")
	require.Equal(t, "lead", spec.Name)
	require.Equal(t, "tech-lead", spec.Role)
	require.Equal(t, "Owns the diff", spec.Descr)
	require.Equal(t, "/tmp/x", spec.Cwd)
}

// Repeated --member flags still accumulate (StringArray appends), so a team
// of N members is bootstrappable in one create call.
func TestMemberFlag_RepeatedFlagsAccumulate(t *testing.T) {
	cmd := groupsCreateCmd()

	err := cmd.Flags().Parse([]string{
		"--member", "name=lead,role=tech-lead",
		"--member", "name=tester,role=test-runner",
	})
	require.NoError(t, err, "parsing repeated --member")

	got, err := cmd.Flags().GetStringArray("member")
	require.NoError(t, err)
	require.Equal(t, []string{"name=lead,role=tech-lead", "name=tester,role=test-runner"}, got)
}
