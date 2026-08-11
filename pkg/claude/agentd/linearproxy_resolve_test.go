package agentd

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// linearproxy_resolve_test.go covers the name→UUID resolvers directly.
//
// The flow tests exercise these through the daemon mux, which is where the
// team gate and the mutation input are worth checking. What is easier to pin
// here is the MATCHING itself: which of several same-named things wins, which
// collisions are refused, and what a refusal says — all of which are decided
// from one Linear response and need no daemon around them.

// stubLinear replays one scripted response per document, keyed by a substring
// of the query, and records the variables each call carried.
type stubLinear struct {
	byQuery map[string]string
	calls   []linearRequest
}

func (s *stubLinear) install(t *testing.T) {
	t.Helper()
	t.Cleanup(SetLinearTransportForTest(
		func(_ context.Context, _ string, req linearRequest) (linearHTTPResult, error) {
			s.calls = append(s.calls, req)
			for needle, body := range s.byQuery {
				if strings.Contains(req.Query, needle) {
					return linearHTTPResult{Status: http.StatusOK, Body: []byte(body), Headers: http.Header{}}, nil
				}
			}
			return linearHTTPResult{Status: http.StatusOK, Body: []byte(`{"data":{}}`), Headers: http.Header{}}, nil
		}))
}

// resolveSession is a session for one team, with the transport stubbed and a
// key in the environment so routeKey resolves.
func resolveSession(t *testing.T, team string, responses map[string]string) (*linearProxySession, *stubLinear, *linearRoute) {
	t.Helper()
	t.Setenv("LINEAR_API_KEY", "lin_api_testkey")
	stub := &stubLinear{byQuery: responses}
	stub.install(t)
	s := testLinearSession(team)
	rt, fault := s.routeFor(team)
	require.Nil(t, fault)
	return s, stub, rt
}

// lastFilter is the `filter` variable of the most recent call, which is where
// every caller-supplied name travels.
func (s *stubLinear) lastFilter(t *testing.T) map[string]any {
	t.Helper()
	require.NotEmpty(t, s.calls)
	filter, ok := s.calls[len(s.calls)-1].Variables["filter"].(map[string]any)
	require.True(t, ok, "the resolver must send its filter as a variable")
	return filter
}

// --- labels ---

const labelResolveDoc = "query LabelResolve"

// TestResolveLabelIDsMatchesEveryNameInOneCall is the shape the resolver
// promises: one call for the whole set, and each requested name decided against
// what came back rather than against row order.
func TestResolveLabelIDsMatchesEveryNameInOneCall(t *testing.T) {
	s, stub, rt := resolveSession(t, "TCL", map[string]string{
		labelResolveDoc: `{"data":{"issueLabels":{"nodes":[
			{"id":"lbl-bug","name":"Bug","team":{"key":"TCL"}},
			{"id":"lbl-perf","name":"performance","team":null}
		]}}}`,
	})

	ids, fault := s.resolveLabelIDs(t.Context(), rt, "TCL", []string{"performance", "bug"})
	require.Nil(t, fault)
	// The caller's order, not Linear's: the request describes a set, and
	// returning it in the order it was asked for is the least surprising thing
	// to put in front of a human reading the audit trail.
	assert.Equal(t, []string{"lbl-perf", "lbl-bug"}, ids)
	assert.Len(t, stub.calls, 1, "a multi-label set must cost one call, not one per label")
}

// TestResolveLabelIDsRefusesUnknownNamesByName — a label is never created on
// demand, so a miss has to say WHICH name missed or the agent cannot fix it.
func TestResolveLabelIDsRefusesUnknownNamesByName(t *testing.T) {
	s, _, rt := resolveSession(t, "TCL", map[string]string{
		labelResolveDoc: `{"data":{"issueLabels":{"nodes":[
			{"id":"lbl-bug","name":"Bug","team":{"key":"TCL"}}
		]}}}`,
	})

	_, fault := s.resolveLabelIDs(t.Context(), rt, "TCL", []string{"bug", "regressionn"})
	require.NotNil(t, fault)
	assert.Equal(t, "unknown_label", fault.Code)
	assert.Contains(t, fault.Msg, "regressionn", "the refusal must name the label that missed")
	assert.NotContains(t, fault.Msg, `"bug"`, "the label that resolved is not part of the problem")
}

// TestResolveLabelIDsPrefersTheTeamsOwnLabel is the single place these
// resolvers prefer rather than refuse. A team that has defined its own "bug"
// has said what the word means for its issues; the workspace-wide one is what
// it was defined instead of.
func TestResolveLabelIDsPrefersTheTeamsOwnLabel(t *testing.T) {
	s, _, rt := resolveSession(t, "TCL", map[string]string{
		labelResolveDoc: `{"data":{"issueLabels":{"nodes":[
			{"id":"lbl-global","name":"Bug","team":null},
			{"id":"lbl-team","name":"Bug","team":{"key":"TCL","name":"Tclaude"}}
		]}}}`,
	})

	ids, fault := s.resolveLabelIDs(t.Context(), rt, "TCL", []string{"bug"})
	require.Nil(t, fault)
	assert.Equal(t, []string{"lbl-team"}, ids)
}

// TestResolveLabelIDsRefusesATieWithinOneScope — two labels in different groups
// share a name routinely in Linear, and nothing distinguishes them. Picking one
// would hang the wrong label on the ticket, silently, under the operator's name.
func TestResolveLabelIDsRefusesATieWithinOneScope(t *testing.T) {
	s, _, rt := resolveSession(t, "TCL", map[string]string{
		labelResolveDoc: `{"data":{"issueLabels":{"nodes":[
			{"id":"lbl-a","name":"Bug","team":{"key":"TCL"},"parent":{"name":"Type"}},
			{"id":"lbl-b","name":"Bug","team":{"key":"TCL"},"parent":{"name":"Source"}}
		]}}}`,
	})

	_, fault := s.resolveLabelIDs(t.Context(), rt, "TCL", []string{"Bug"})
	require.NotNil(t, fault)
	assert.Equal(t, "ambiguous_label", fault.Code)
	assert.Contains(t, fault.Msg, "Type", "the refusal must name the groups that collide")
	assert.Contains(t, fault.Msg, "Source")
}

// TestResolveLabelIDsDropsARepeatedName — "Bug" and "bug" are one label, and a
// set with a repeated member is not what the caller described.
func TestResolveLabelIDsDropsARepeatedName(t *testing.T) {
	s, _, rt := resolveSession(t, "TCL", map[string]string{
		labelResolveDoc: `{"data":{"issueLabels":{"nodes":[
			{"id":"lbl-bug","name":"Bug","team":{"key":"TCL"}}
		]}}}`,
	})

	ids, fault := s.resolveLabelIDs(t.Context(), rt, "TCL", []string{"Bug", "bug"})
	require.Nil(t, fault)
	assert.Equal(t, []string{"lbl-bug"}, ids)
}

// TestResolveLabelIDsFilterCarriesTheTeamAndExcludesGroups pins the filter
// itself, which no response can reveal: the lookup must stay inside the gated
// team (plus the workspace-wide labels, which belong to no team), and a label
// GROUP is a container an issue cannot carry.
func TestResolveLabelIDsFilterCarriesTheTeamAndExcludesGroups(t *testing.T) {
	s, stub, rt := resolveSession(t, "TCL", map[string]string{
		labelResolveDoc: `{"data":{"issueLabels":{"nodes":[
			{"id":"lbl-bug","name":"Bug","team":{"key":"TCL"}}
		]}}}`,
	})

	_, fault := s.resolveLabelIDs(t.Context(), rt, "TCL", []string{"Bug"})
	require.Nil(t, fault)

	encoded, err := json.Marshal(stub.lastFilter(t))
	require.NoError(t, err)
	filter := string(encoded)
	assert.Contains(t, filter, `"eqIgnoreCase":"TCL"`, "the lookup must be scoped to the gated team")
	assert.Contains(t, filter, `"null":true`, "workspace-wide labels belong to no team and must still match")
	assert.Contains(t, filter, `"isGroup":{"eq":false}`, "a label group is not something an issue can carry")
	assert.Contains(t, filter, `"eqIgnoreCase":"Bug"`, "the name must travel in the filter, not in the document")
}

// TestResolveLabelIDsBoundsTheSet — the cap is enforced before a credential is
// spent. What the cap has to EQUAL is pinned separately, by
// TestLabelCapMatchesWhatIssueViewReadsBack.
func TestResolveLabelIDsBoundsTheSet(t *testing.T) {
	s, stub, rt := resolveSession(t, "TCL", nil)

	names := make([]string, maxLinearIssueLabels+1)
	for i := range names {
		names[i] = string(rune('a' + i))
	}
	_, fault := s.resolveLabelIDs(t.Context(), rt, "TCL", names)
	require.NotNil(t, fault)
	assert.Equal(t, http.StatusBadRequest, fault.Status)
	assert.Empty(t, stub.calls, "an oversize set must be refused before a credential is spent")
}

// --- assignee ---

const userResolveDoc = "query UserResolve"

// TestResolveAssigneeAcceptsThreeSpellings — which of these an agent has
// depends on where it read the person's name, and it cannot tell which field it
// is looking at.
func TestResolveAssigneeAcceptsThreeSpellings(t *testing.T) {
	s, stub, rt := resolveSession(t, "TCL", map[string]string{
		userResolveDoc: `{"data":{"users":{"nodes":[
			{"id":"usr-1","name":"Mikael","displayName":"mikael","email":"m@example.com","active":true}
		]}}}`,
	})

	for _, spelling := range []string{"mikael", "Mikael", "m@example.com"} {
		id, fault := s.resolveAssigneeID(t.Context(), rt, spelling)
		require.Nil(t, fault, "spelling %q", spelling)
		assert.Equal(t, "usr-1", id)
	}
	encoded, err := json.Marshal(stub.lastFilter(t))
	require.NoError(t, err)
	for _, field := range []string{"displayName", "name", "email"} {
		assert.Contains(t, string(encoded), field, "all three spellings must be asked about at once")
	}
}

// TestResolveAssigneeDropsDeactivatedAccountsWhenAnActiveOneMatches — names get
// reused as people leave and join, and a former colleague must not make a
// current one unassignable.
func TestResolveAssigneeDropsDeactivatedAccountsWhenAnActiveOneMatches(t *testing.T) {
	s, _, rt := resolveSession(t, "TCL", map[string]string{
		userResolveDoc: `{"data":{"users":{"nodes":[
			{"id":"usr-old","displayName":"sam","email":"sam@old.example","active":false},
			{"id":"usr-new","displayName":"sam","email":"sam@example.com","active":true}
		]}}}`,
	})

	id, fault := s.resolveAssigneeID(t.Context(), rt, "sam")
	require.Nil(t, fault)
	assert.Equal(t, "usr-new", id)
}

// TestResolveAssigneeResolvesAnOnlyMatchThatIsDeactivated — dropping the
// inactive rows is a tie-break, not a filter. A workspace whose only match is
// deactivated still gets a specific answer rather than "no such user", which
// would send the agent looking for a typo that is not there.
func TestResolveAssigneeResolvesAnOnlyMatchThatIsDeactivated(t *testing.T) {
	s, _, rt := resolveSession(t, "TCL", map[string]string{
		userResolveDoc: `{"data":{"users":{"nodes":[
			{"id":"usr-old","displayName":"sam","email":"sam@old.example","active":false}
		]}}}`,
	})

	id, fault := s.resolveAssigneeID(t.Context(), rt, "sam")
	require.Nil(t, fault)
	assert.Equal(t, "usr-old", id)
}

// TestResolveAssigneeRefusesAmbiguityAndOffersTheEmail — the email is the thing
// that makes the next attempt work, so the refusal has to carry it.
func TestResolveAssigneeRefusesAmbiguityAndOffersTheEmail(t *testing.T) {
	s, _, rt := resolveSession(t, "TCL", map[string]string{
		userResolveDoc: `{"data":{"users":{"nodes":[
			{"id":"usr-1","displayName":"sam","email":"sam.a@example.com","active":true},
			{"id":"usr-2","displayName":"sam","email":"sam.b@example.com","active":true}
		]}}}`,
	})

	_, fault := s.resolveAssigneeID(t.Context(), rt, "sam")
	require.NotNil(t, fault)
	assert.Equal(t, "ambiguous_assignee", fault.Code)
	assert.Contains(t, fault.Msg, "sam.a@example.com")
	assert.Contains(t, fault.Msg, "sam.b@example.com")
}

// TestResolveAssigneeMissDoesNotEnumerateTheWorkspace — a misspelling must not
// turn into a directory dump of everyone the operator's key can see.
func TestResolveAssigneeMissDoesNotEnumerateTheWorkspace(t *testing.T) {
	s, _, rt := resolveSession(t, "TCL", map[string]string{
		userResolveDoc: `{"data":{"users":{"nodes":[]}}}`,
	})

	_, fault := s.resolveAssigneeID(t.Context(), rt, "nobody")
	require.NotNil(t, fault)
	assert.Equal(t, "unknown_assignee", fault.Code)
	assert.Contains(t, fault.Msg, "nobody")
}

// --- project and milestone ---

// TestResolveProjectScopesTheLookupToTheTeam — a project name is only meaningful
// inside the gate; searching the workspace for one would reach past it.
func TestResolveProjectScopesTheLookupToTheTeam(t *testing.T) {
	s, stub, rt := resolveSession(t, "TCL", map[string]string{
		"query ProjectResolve": `{"data":{"projects":{"nodes":[{"id":"prj-1","name":"tclaude"}]}}}`,
	})

	id, fault := s.resolveProjectID(t.Context(), rt, "TCL", "tclaude")
	require.Nil(t, fault)
	assert.Equal(t, "prj-1", id)

	encoded, err := json.Marshal(stub.lastFilter(t))
	require.NoError(t, err)
	assert.Contains(t, string(encoded), `"accessibleTeams"`)
	assert.Contains(t, string(encoded), `"eqIgnoreCase":"TCL"`)
}

// TestResolveProjectRefusesAMissAndATie.
func TestResolveProjectRefusesAMissAndATie(t *testing.T) {
	t.Run("miss", func(t *testing.T) {
		s, _, rt := resolveSession(t, "TCL", map[string]string{
			"query ProjectResolve": `{"data":{"projects":{"nodes":[]}}}`,
		})
		_, fault := s.resolveProjectID(t.Context(), rt, "TCL", "nope")
		require.NotNil(t, fault)
		assert.Equal(t, "unknown_project", fault.Code)
	})
	t.Run("tie", func(t *testing.T) {
		s, _, rt := resolveSession(t, "TCL", map[string]string{
			"query ProjectResolve": `{"data":{"projects":{"nodes":[
				{"id":"p1","name":"Q3"},{"id":"p2","name":"q3"}]}}}`,
		})
		_, fault := s.resolveProjectID(t.Context(), rt, "TCL", "Q3")
		require.NotNil(t, fault)
		assert.Equal(t, "ambiguous_project", fault.Code)
	})
}

// TestResolveMilestoneRefusesAMissAndATie — neither branch had coverage, and
// the miss path is the one that renders the project by name (or, with no rows
// to take a name from, by the id the lookup used).
func TestResolveMilestoneRefusesAMissAndATie(t *testing.T) {
	t.Run("miss names the project it searched", func(t *testing.T) {
		s, _, rt := resolveSession(t, "TCL", map[string]string{
			"query MilestoneResolve": `{"data":{"projectMilestones":{"nodes":[]}}}`,
		})
		_, fault := s.resolveMilestoneID(t.Context(), rt, "prj-1", "Current", "Beta")
		require.NotNil(t, fault)
		assert.Equal(t, "unknown_milestone", fault.Code)
		assert.Contains(t, fault.Msg, "Beta")
		// A miss returns no rows, so there is no project name in Linear's answer
		// to use — the one the CALLER supplied has to carry the message instead.
		// Live testing caught this: before the name was threaded through, a
		// typo'd milestone was refused with the project's raw UUID, which is the
		// one string the caller cannot match against anything they typed.
		assert.Contains(t, fault.Msg, "Current")
		assert.NotContains(t, fault.Msg, "prj-1", "a UUID is not something the caller can act on")
	})

	t.Run("miss with no name anywhere falls back to the id", func(t *testing.T) {
		s, _, rt := resolveSession(t, "TCL", map[string]string{
			"query MilestoneResolve": `{"data":{"projectMilestones":{"nodes":[]}}}`,
		})
		_, fault := s.resolveMilestoneID(t.Context(), rt, "prj-1", "", "Beta")
		require.NotNil(t, fault)
		// Not good, but better than naming nothing at all.
		assert.Contains(t, fault.Msg, "prj-1")
	})
	t.Run("tie", func(t *testing.T) {
		s, _, rt := resolveSession(t, "TCL", map[string]string{
			"query MilestoneResolve": `{"data":{"projectMilestones":{"nodes":[
				{"id":"ms-1","name":"Beta","project":{"id":"prj-1","name":"Current"}},
				{"id":"ms-2","name":"beta","project":{"id":"prj-1","name":"Current"}}]}}}`,
		})
		_, fault := s.resolveMilestoneID(t.Context(), rt, "prj-1", "Current", "Beta")
		require.NotNil(t, fault)
		assert.Equal(t, "ambiguous_milestone", fault.Code)
		assert.Contains(t, fault.Msg, "Current", "the project is named when Linear gave one")
	})
}

// TestResolveMilestoneIsScopedToOneProject — milestone names are unique only
// within a project, so the project id has to be in the filter or the answer is
// a coin toss between projects.
func TestResolveMilestoneIsScopedToOneProject(t *testing.T) {
	s, stub, rt := resolveSession(t, "TCL", map[string]string{
		"query MilestoneResolve": `{"data":{"projectMilestones":{"nodes":[
			{"id":"ms-1","name":"Beta","project":{"id":"prj-1","name":"tclaude"}}]}}}`,
	})

	id, fault := s.resolveMilestoneID(t.Context(), rt, "prj-1", "Current", "beta")
	require.Nil(t, fault)
	assert.Equal(t, "ms-1", id)

	encoded, err := json.Marshal(stub.lastFilter(t))
	require.NoError(t, err)
	assert.Contains(t, string(encoded), `"eq":"prj-1"`)
}

// --- the field applier ---

// TestApplyIssueNameFieldsDistinguishesAbsentFromCleared is the whole reason
// these fields are pointers: omitting a flag and clearing a field are different
// requests, and only one of them may touch the mutation input.
func TestApplyIssueNameFieldsDistinguishesAbsentFromCleared(t *testing.T) {
	s, stub, rt := resolveSession(t, "TCL", nil)

	t.Run("absent touches nothing", func(t *testing.T) {
		input := map[string]any{}
		fault := s.applyIssueNameFields(t.Context(), rt, "TCL",
			linearIssuePlacement{}, linearIssueNameFields{}, input)
		require.Nil(t, fault)
		assert.Empty(t, input)
		assert.Empty(t, stub.calls, "nothing to resolve means no credential spent")
	})

	t.Run("empty clears, without a lookup", func(t *testing.T) {
		input := map[string]any{}
		empty, noLabels := "", []string{}
		fault := s.applyIssueNameFields(t.Context(), rt, "TCL",
			linearIssuePlacement{ProjectID: "prj-1"}, linearIssueNameFields{
				Project: &empty, Milestone: &empty, Assignee: &empty, Labels: &noLabels,
			}, input)
		require.Nil(t, fault)
		// Explicit nulls: Linear reads an absent key as "leave it" and a null as
		// "unset it", which is exactly the difference being asked for.
		require.Contains(t, input, "projectId")
		assert.Nil(t, input["projectId"])
		assert.Nil(t, input["projectMilestoneId"])
		assert.Nil(t, input["assigneeId"])
		assert.Equal(t, []string{}, input["labelIds"])
		assert.Empty(t, stub.calls, "a clear names nothing, so there is nothing to look up")
	})
}

// TestApplyIssueNameFieldsRefusesAMilestoneWithNoProject — a milestone is a
// child of a project. Without one there is nothing to resolve the name within,
// and the whole workspace is not an answer.
func TestApplyIssueNameFieldsRefusesAMilestoneWithNoProject(t *testing.T) {
	s, stub, rt := resolveSession(t, "TCL", nil)

	milestone := "Beta"
	fault := s.applyIssueNameFields(t.Context(), rt, "TCL",
		linearIssuePlacement{}, linearIssueNameFields{Milestone: &milestone}, map[string]any{})
	require.NotNil(t, fault)
	assert.Equal(t, "milestone_needs_project", fault.Code)
	assert.Empty(t, stub.calls)
}

// TestApplyIssueNameFieldsClearingTheProjectAlsoStrandsAMilestone — the same
// refusal, reached the other way: a call that takes the issue out of its
// project and names a milestone is asking for a milestone of no project.
func TestApplyIssueNameFieldsClearingTheProjectAlsoStrandsAMilestone(t *testing.T) {
	s, _, rt := resolveSession(t, "TCL", nil)

	empty, milestone := "", "Beta"
	fault := s.applyIssueNameFields(t.Context(), rt, "TCL",
		linearIssuePlacement{ProjectID: "prj-current"},
		linearIssueNameFields{Project: &empty, Milestone: &milestone}, map[string]any{})
	require.NotNil(t, fault)
	assert.Equal(t, "milestone_needs_project", fault.Code)
}

// TestApplyIssueNameFieldsDropsAStrandedMilestone is the case a caller does not
// think about: moving or clearing the project on an issue that HAS a milestone,
// without mentioning the milestone at all.
//
// A milestone belongs to exactly one project, so it cannot come along. Leaving
// projectMilestoneId untouched would either have Linear refuse the whole update
// — a 502 the agent is told not to retry, naming nothing it could act on — or
// leave the ticket pointing into a project it is no longer in.
func TestApplyIssueNameFieldsDropsAStrandedMilestone(t *testing.T) {
	current := linearIssuePlacement{ProjectID: "prj-old", ProjectName: "Current", MilestoneID: "ms-old"}

	t.Run("moving to another project", func(t *testing.T) {
		s, _, rt := resolveSession(t, "TCL", map[string]string{
			"query ProjectResolve": `{"data":{"projects":{"nodes":[{"id":"prj-new","name":"Next"}]}}}`,
		})
		project := "Next"
		input := map[string]any{}
		fault := s.applyIssueNameFields(t.Context(), rt, "TCL", current,
			linearIssueNameFields{Project: &project}, input)
		require.Nil(t, fault)
		assert.Equal(t, "prj-new", input["projectId"])
		require.Contains(t, input, "projectMilestoneId")
		assert.Nil(t, input["projectMilestoneId"])
	})

	t.Run("clearing the project", func(t *testing.T) {
		s, _, rt := resolveSession(t, "TCL", nil)
		empty := ""
		input := map[string]any{}
		fault := s.applyIssueNameFields(t.Context(), rt, "TCL", current,
			linearIssueNameFields{Project: &empty}, input)
		require.Nil(t, fault)
		assert.Nil(t, input["projectId"])
		require.Contains(t, input, "projectMilestoneId")
		assert.Nil(t, input["projectMilestoneId"])
	})

	// The other half of the rule, and the one that keeps it from being
	// destructive: naming the project an issue is already in is not a move, so
	// the milestone must survive it.
	t.Run("naming the project it is already in changes nothing", func(t *testing.T) {
		s, _, rt := resolveSession(t, "TCL", map[string]string{
			"query ProjectResolve": `{"data":{"projects":{"nodes":[{"id":"prj-old","name":"Current"}]}}}`,
		})
		project := "Current"
		input := map[string]any{}
		fault := s.applyIssueNameFields(t.Context(), rt, "TCL", current,
			linearIssueNameFields{Project: &project}, input)
		require.Nil(t, fault)
		assert.Equal(t, "prj-old", input["projectId"])
		assert.NotContains(t, input, "projectMilestoneId",
			"an issue that has not moved keeps its milestone")
	})

	// An issue with no milestone has nothing to strand, so a move must not send
	// a null that says something the caller did not.
	t.Run("no milestone to strand", func(t *testing.T) {
		s, _, rt := resolveSession(t, "TCL", map[string]string{
			"query ProjectResolve": `{"data":{"projects":{"nodes":[{"id":"prj-new","name":"Next"}]}}}`,
		})
		project := "Next"
		input := map[string]any{}
		fault := s.applyIssueNameFields(t.Context(), rt, "TCL",
			linearIssuePlacement{ProjectID: "prj-old"},
			linearIssueNameFields{Project: &project}, input)
		require.Nil(t, fault)
		assert.NotContains(t, input, "projectMilestoneId")
	})
}

// TestApplyIssueNameFieldsResolvesTheMilestoneInTheNewProject — a call that
// moves an issue and names a milestone means the milestone of the project it is
// moving TO. Resolving against the old one could attach a same-named milestone
// belonging to a project the issue is leaving.
func TestApplyIssueNameFieldsResolvesTheMilestoneInTheNewProject(t *testing.T) {
	s, stub, rt := resolveSession(t, "TCL", map[string]string{
		"query ProjectResolve": `{"data":{"projects":{"nodes":[{"id":"prj-new","name":"Next"}]}}}`,
		"query MilestoneResolve": `{"data":{"projectMilestones":{"nodes":[
			{"id":"ms-new","name":"Beta","project":{"id":"prj-new","name":"Next"}}]}}}`,
	})

	project, milestone := "Next", "Beta"
	input := map[string]any{}
	fault := s.applyIssueNameFields(t.Context(), rt, "TCL",
		linearIssuePlacement{ProjectID: "prj-old"},
		linearIssueNameFields{Project: &project, Milestone: &milestone}, input)
	require.Nil(t, fault)
	assert.Equal(t, "prj-new", input["projectId"])
	assert.Equal(t, "ms-new", input["projectMilestoneId"])

	require.Len(t, stub.calls, 2)
	encoded, err := json.Marshal(stub.calls[1].Variables["filter"])
	require.NoError(t, err)
	assert.Contains(t, string(encoded), `"eq":"prj-new"`,
		"the milestone must be resolved within the project the issue is moving to")
}

// --- shared helpers ---

func TestValidateLinearName(t *testing.T) {
	assert.Nil(t, validateLinearName("Q3 planning", "project"))
	require.NotNil(t, validateLinearName("", "project"))
	require.NotNil(t, validateLinearName(strings.Repeat("x", maxLinearNameLen+1), "label"))

	fault := validateLinearName("bad\nname", "label")
	require.NotNil(t, fault)
	assert.Contains(t, fault.Msg, "control character")
	assert.Contains(t, fault.Msg, "label", "the refusal must name the field it is about")
}

// TestRequireCompletePageRefusesATruncatedLookup — "exactly one match" drawn
// from a page that filled is not a conclusion, and these resolvers never
// resolve from a set they cannot see all of.
func TestRequireCompletePageRefusesATruncatedLookup(t *testing.T) {
	assert.Nil(t, requireCompletePage(linearResolvePageSize-1, "label", "bug"))
	fault := requireCompletePage(linearResolvePageSize, "label", "bug")
	require.NotNil(t, fault)
	assert.Contains(t, fault.Msg, "unique")
}

// TestNormalizedForCreateFoldsClearsIntoAbsent — a new issue has nothing to
// clear, so the spelling that means "unset it" on an update must not reach a
// resolver here.
func TestNormalizedForCreateFoldsClearsIntoAbsent(t *testing.T) {
	empty, blanks := " ", []string{"", "  "}
	body := linearCreateRequest{linearIssueNameFields: linearIssueNameFields{
		Project: &empty, Milestone: &empty, Assignee: &empty, Labels: &blanks,
	}}
	assert.False(t, body.normalizedForCreate().any(),
		"empty values on a create must read as 'not asked for'")

	name := "tclaude"
	body.Project = &name
	assert.True(t, body.normalizedForCreate().any())
}

// TestLabelCapMatchesWhatIssueViewReadsBack pins the invariant maxLinearIssueLabels
// exists for. `--label` replaces the whole set, so an agent adds a label by
// reading the issue and resending its labels plus one. If the view showed fewer
// labels than a call may set, that loop would silently delete the ones it could
// not see — so the two numbers have to move together, and one of them lives
// inside a GraphQL document where no compiler will check it.
func TestLabelCapMatchesWhatIssueViewReadsBack(t *testing.T) {
	assert.Contains(t, linearQueryIssue, fmt.Sprintf("labels(first: %d)", maxLinearIssueLabels),
		"issue view must read back at least as many labels as one call may set")
}

func TestPlacementOf(t *testing.T) {
	assert.Equal(t, linearIssuePlacement{}, placementOf(nil))
	assert.Equal(t, linearIssuePlacement{}, placementOf(&linearIssue{}))
	assert.Equal(t,
		linearIssuePlacement{ProjectID: "prj-1", ProjectName: "tclaude", MilestoneID: "ms-1"},
		placementOf(&linearIssue{
			Project:          &linearProjectRef{ID: " prj-1 ", Name: "tclaude"},
			ProjectMilestone: &linearMilestoneRef{ID: "ms-1", Name: "Beta"},
		}))
}
