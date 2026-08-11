package proxy

import (
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// linear_test.go covers what `tclaude proxy linear issue create/update` decides
// to SEND.
//
// The distinction worth pinning is "clear it" versus "leave it alone". On the
// wire those differ only in whether a key is present in the request body, and
// which one a caller gets is decided entirely by cobra's Changed() — a detail
// no amount of reading the params struct can confirm. So these parse argv
// through the PRODUCTION flag set, which is also what keeps a renamed or
// dropped flag from passing silently here.

// parsedUpdateCmd parses argv against the real `issue update` flag set and
// hands back the command, so Changed() answers what a caller actually typed.
//
// The params struct is supplied separately because boa binds the flags to an
// instance a test cannot reach; the values are the same ones argv carries, and
// what is under test is which of them the builder decides to send.
func parsedUpdateCmd(t *testing.T, argv ...string) *cobra.Command {
	t.Helper()
	cmd := linearIssueUpdateCmd()
	require.NoError(t, cmd.Flags().Parse(argv))
	return cmd
}

// TestLinearUpdateOmittedFlagsAreNotSent — every field the caller did not type
// must be absent from the body, or the daemon would read it as an instruction
// to change that field.
func TestLinearUpdateOmittedFlagsAreNotSent(t *testing.T) {
	cmd := parsedUpdateCmd(t, "TCL-1", "--state", "In Review")
	p := &linearUpdateParams{Identifier: "TCL-1", State: "In Review"}

	body, rc := buildLinearUpdateBody(p, cmd, strings.NewReader(""), &strings.Builder{})
	require.Equal(t, rcOK, rc)

	assert.Equal(t, "In Review", body["state"])
	for _, key := range []string{"description", "project", "milestone", "assignee", "labels", "priority"} {
		assert.NotContains(t, body, key, "an untyped flag must not reach the daemon")
	}
}

// TestLinearUpdateEmptyValuesAreSentAsClears is the other half: an empty string
// typed at the flag is a request to unset the field, and it has to survive as
// far as the daemon to mean anything.
func TestLinearUpdateEmptyValuesAreSentAsClears(t *testing.T) {
	cmd := parsedUpdateCmd(t, "TCL-1",
		"--assignee", "", "--project", "", "--milestone", "", "--description", "", "--label", "")
	p := &linearUpdateParams{Identifier: "TCL-1"}

	body, rc := buildLinearUpdateBody(p, cmd, strings.NewReader(""), &strings.Builder{})
	require.Equal(t, rcOK, rc)

	for _, key := range []string{"assignee", "project", "milestone", "description"} {
		require.Contains(t, body, key, "a typed empty value must be sent, not dropped")
		assert.Equal(t, "", body[key])
	}
	require.Contains(t, body, "labels")
	assert.Empty(t, body["labels"], "an empty label set is the clear")
}

// TestLinearUpdateLabelsAreTheWholeSet — `--label` replaces rather than adds,
// and repeating it builds the set. Getting this wrong would silently drop the
// labels a ticket already carries.
func TestLinearUpdateLabelsAreTheWholeSet(t *testing.T) {
	cmd := parsedUpdateCmd(t, "TCL-1", "--label", "bug", "--label", "needs review")
	p := &linearUpdateParams{Identifier: "TCL-1", Labels: []string{"bug", "needs review"}}

	body, rc := buildLinearUpdateBody(p, cmd, strings.NewReader(""), &strings.Builder{})
	require.Equal(t, rcOK, rc)
	assert.Equal(t, []string{"bug", "needs review"}, body["labels"])
}

// TestLinearUpdateRefusesAnEmptyChangeSet — a request that changes nothing
// should not spend the operator's credential to be told so.
func TestLinearUpdateRefusesAnEmptyChangeSet(t *testing.T) {
	var stderr strings.Builder
	cmd := parsedUpdateCmd(t, "TCL-1")

	_, rc := buildLinearUpdateBody(&linearUpdateParams{Identifier: "TCL-1"}, cmd,
		strings.NewReader(""), &stderr)
	assert.Equal(t, rcInvalidArg, rc)
	assert.Contains(t, stderr.String(), "--assignee", "the refusal should list what CAN be updated")
}

// TestLinearUpdateDescriptionCanComeFromAFile — the skill tells agents to use
// --description-file for anything multi-line, so that flag has to count as
// "the caller asked to change the description" on its own.
func TestLinearUpdateDescriptionCanComeFromAFile(t *testing.T) {
	cmd := parsedUpdateCmd(t, "TCL-1", "--description-file", "-")
	p := &linearUpdateParams{Identifier: "TCL-1", DescriptionFile: "-"}

	body, rc := buildLinearUpdateBody(p, cmd, strings.NewReader("a new body\n"), &strings.Builder{})
	require.Equal(t, rcOK, rc)
	assert.Equal(t, "a new body\n", body["description"])
}

// TestLinearCreateRequiresProjectForMilestone — a milestone is a child of a
// project, and saying so before the call spares a round trip.
func TestLinearCreateRequiresProjectForMilestone(t *testing.T) {
	var stderr strings.Builder
	p := &linearCreateParams{Team: "TCL", Title: "A thing", Milestone: "Beta"}

	_, rc := buildLinearCreateBody(p, strings.NewReader(""), &stderr)
	assert.Equal(t, rcInvalidArg, rc)
	assert.Contains(t, stderr.String(), "--project")
}

// TestLinearCreateSendsOnlyWhatWasGiven — a create has nothing to clear, so an
// empty value is "not asked for" rather than a null on the wire.
func TestLinearCreateSendsOnlyWhatWasGiven(t *testing.T) {
	p := &linearCreateParams{
		Team: "TCL", Title: "A thing",
		Project: "tclaude", Assignee: "mikael", Labels: []string{"bug"},
	}

	body, rc := buildLinearCreateBody(p, strings.NewReader(""), &strings.Builder{})
	require.Equal(t, rcOK, rc)

	assert.Equal(t, "tclaude", body["project"])
	assert.Equal(t, "mikael", body["assignee"])
	assert.Equal(t, []string{"bug"}, body["labels"])
	assert.NotContains(t, body, "milestone", "an untyped field is left out entirely")
}
