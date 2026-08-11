package agentd

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
)

// linearproxy_test.go covers the pieces the flow tests reach only indirectly:
// the identifier and charset gates, the filter the daemon builds, and the
// comment renderer's bound.

// testLinearSession builds a session with an UNSCOPED caller, whose effective
// team set is therefore exactly the operator's allow-list. The scoped-grant
// narrowing is exercised end to end in linearproxy_flow_test.go, where the grant
// row and the permission resolver are real.
func testLinearSession(teams ...string) *linearProxySession {
	cfg := &config.Config{Agent: &config.AgentConfig{
		LinearProxy: &config.LinearProxyConfig{AllowedTeams: teams},
	}}
	policy := cfg.ResolvedLinearProxy()
	return withTestRoutes(&linearProxySession{policy: policy, teams: policy.AllowedTeams})
}

// withTestRoutes gives a hand-built session the credential routing a real one
// resolves in newLinearProxySession, so a test can exercise scanTargets and the
// filter it feeds. These policies configure no workspaces, so every team routes
// to the single default credential and linearRoutes cannot fail.
func withTestRoutes(s *linearProxySession) *linearProxySession {
	routes, byTeam, fault := linearRoutes(s.policy, s.teams)
	if fault != nil {
		panic("test policy could not be routed: " + fault.Msg)
	}
	s.routes, s.routeByTeam = routes, byTeam
	return s
}

// testScopedLinearSession is testLinearSession with a team-scoped caller: the
// effective set is the intersection, and grantTeams is what the grant admits.
func testScopedLinearSession(operator, granted []string) *linearProxySession {
	cfg := &config.Config{Agent: &config.AgentConfig{
		LinearProxy: &config.LinearProxyConfig{AllowedTeams: operator},
	}}
	policy := cfg.ResolvedLinearProxy()
	s := &linearProxySession{policy: policy, grantTeams: lowerTeamKeys(granted)}
	for _, key := range policy.AllowedTeams {
		if slices.Contains(s.grantTeams, key) {
			s.teams = append(s.teams, key)
		}
	}
	return withTestRoutes(s)
}

// testLinearFilter builds the filter one call of a team-spanning verb would
// send, going through the real scanTargets → linearIssueFilter path rather than
// around it.
func testLinearFilter(t *testing.T, s *linearProxySession, team, state string, assignedMe bool) map[string]any {
	t.Helper()
	targets, fault := s.scanTargets(team)
	require.Nil(t, fault)
	require.Len(t, targets, 1, "a single-workspace policy must resolve to one call")
	return linearIssueFilter(targets[0].teams, state, assignedMe)
}

func TestValidateLinearIdentifier(t *testing.T) {
	t.Run("accepts and normalises", func(t *testing.T) {
		for in, want := range map[string]string{
			"TCL-568":   "TCL-568",
			"tcl-568":   "TCL-568",
			"  TCL-1  ": "TCL-1",
			"JOH-99999": "JOH-99999",
			"A1-7":      "A1-7",
		} {
			got, fault := validateLinearIdentifier(in)
			require.Nil(t, fault, "%q should be accepted", in)
			assert.Equal(t, want, got)
		}
	})

	t.Run("refuses anything that is not TEAM-123", func(t *testing.T) {
		for _, in := range []string{
			"",
			"TCL",
			"TCL-",
			"-568",
			"TCL-abc",
			"TCL-5-6",
			"TCL 568",
			"TCL-1234567890", // more digits than any real issue number
			"8a7d5f6e-1234-4a2b-9c8d-1122334455ff",
			"TCL-568; DROP",
			"--TCL-1",
		} {
			_, fault := validateLinearIdentifier(in)
			require.NotNil(t, fault, "%q should be refused", in)
			assert.Equal(t, http.StatusBadRequest, fault.Status)
		}
	})

	// A UUID is refused not because it is malformed but because it carries no
	// team key — there would be nothing to check the allow-list against before
	// spending the operator's credential.
	t.Run("a UUID is refused even though Linear would accept it", func(t *testing.T) {
		_, fault := validateLinearIdentifier("8a7d5f6e-1234-4a2b-9c8d-1122334455ff")
		require.NotNil(t, fault)
		assert.Contains(t, fault.Msg, "TEAM-123")
	})
}

func TestTeamKeyOf(t *testing.T) {
	assert.Equal(t, "TCL", teamKeyOf("TCL-568"))
	assert.Equal(t, "JOH", teamKeyOf("  JOH-1 "))
	assert.Empty(t, teamKeyOf("TCL"))
	assert.Empty(t, teamKeyOf("-1"))
	assert.Empty(t, teamKeyOf("TCL-"))
	assert.Empty(t, teamKeyOf(""))
}

// TestLinearTeamAllowedIsExactNotPrefix is the rule that keeps the flat team
// namespace safe: unlike the git proxy's remote patterns, where a shorter
// pattern deliberately matches as a prefix, a team key must match whole.
func TestLinearTeamAllowedIsExactNotPrefix(t *testing.T) {
	s := testLinearSession("TCL", "JOH")

	for _, allowed := range []string{"TCL", "tcl", "  TCL  ", "JOH"} {
		assert.Nil(t, s.requireAllowedTeam(allowed), "%q should be allowed", allowed)
	}
	for _, refused := range []string{"TCLX", "TC", "SECRET", "", "T", "JOHN"} {
		fault := s.requireAllowedTeam(refused)
		require.NotNil(t, fault, "%q must be refused", refused)
		assert.Equal(t, "team_not_allowed", fault.Code)
	}
}

// TestRequireAllowedTeamNamesTheAllowList — an agent that cannot see the
// operator's config needs the refusal to say what to ask for.
func TestRequireAllowedTeamNamesTheAllowList(t *testing.T) {
	fault := testLinearSession("TCL", "JOH").requireAllowedTeam("SECRET")
	require.NotNil(t, fault)
	assert.Contains(t, fault.Msg, "allowed_teams")
	assert.Contains(t, fault.Msg, "tcl")
	assert.Contains(t, fault.Msg, "joh")
}

// TestRequireAllowedTeamDistinguishesTheTwoLists — with an operator allow-list
// AND a per-agent team scope, a refusal has two possible causes and only one
// fix each. Naming the wrong list would send the agent's human to change a
// setting that was never the problem.
func TestRequireAllowedTeamDistinguishesTheTwoLists(t *testing.T) {
	// The operator allows TCL and JOH; this agent's grant covers TCL only.
	s := testScopedLinearSession([]string{"TCL", "JOH"}, []string{"TCL"})
	assert.Equal(t, []string{"tcl"}, s.teams)

	assert.Nil(t, s.requireAllowedTeam("TCL"))

	t.Run("allowed by the operator, refused by the grant", func(t *testing.T) {
		fault := s.requireAllowedTeam("JOH")
		require.NotNil(t, fault)
		assert.Equal(t, linearTeamOutOfScopeCode, fault.Code)
		assert.Contains(t, fault.Msg, "scope")
		assert.Contains(t, fault.Msg, "tcl", "the refusal must name what the grant DOES cover")
	})

	t.Run("refused by the operator wins the explanation", func(t *testing.T) {
		// SECRET is outside both lists. The operator's is the one to name: a
		// widened grant alone would still not reach it.
		fault := s.requireAllowedTeam("SECRET")
		require.NotNil(t, fault)
		assert.Equal(t, "team_not_allowed", fault.Code)
		assert.Contains(t, fault.Msg, "allowed_teams")
	})
}

// TestScopedSessionNarrowsTheListingFilter — the filter and the row-level drop
// must read the same effective set the identifier gate does, or a scoped agent
// would be refused one issue by name and handed the whole team in a listing.
func TestScopedSessionNarrowsTheListingFilter(t *testing.T) {
	s := testScopedLinearSession([]string{"TCL", "JOH"}, []string{"JOH"})

	filter := testLinearFilter(t, s, "", "", false)
	assert.Equal(t, []string{"joh"}, filterTeamKeys(t, filter),
		"the filter must carry the scoped set, not the operator's")

	kept := s.enforceIssueList([]linearIssue{
		{Identifier: "JOH-1", Team: linearTeamRef{Key: "JOH"}},
		{Identifier: "TCL-1", Team: linearTeamRef{Key: "TCL"}},
	})
	require.Len(t, kept, 1, "a row from an operator-allowed but out-of-scope team must be dropped")
	assert.Equal(t, "JOH-1", kept[0].Identifier)
}

// TestScopedSessionReChecksLinearsAnswer — the load-bearing second gate must be
// the SCOPED set too. An issue moved into a team the operator allows but this
// agent's grant does not is exactly the case a set-intersection bug would leak.
func TestScopedSessionReChecksLinearsAnswer(t *testing.T) {
	s := testScopedLinearSession([]string{"TCL", "JOH"}, []string{"TCL"})

	assert.Nil(t, s.enforceIssueTeam(&linearIssue{Team: linearTeamRef{Key: "TCL"}}))

	fault := s.enforceIssueTeam(&linearIssue{
		Identifier: "JOH-9", Title: "moved", Team: linearTeamRef{Key: "JOH"},
	})
	require.NotNil(t, fault, "an out-of-scope team on Linear's own answer must be refused")
	assert.Equal(t, linearTeamOutOfScopeCode, fault.Code)
	assert.NotContains(t, fault.Msg, "moved", "the refusal must not carry the issue's contents")
}

// TestEnforceIssueTeamRefusesAnUncheckableIssue — a response with no team is a
// programming error in this package, and the safe reading of it is "refuse",
// never "let it through".
func TestEnforceIssueTeamRefusesAnUncheckableIssue(t *testing.T) {
	s := testLinearSession("TCL")

	assert.Nil(t, s.enforceIssueTeam(&linearIssue{Team: linearTeamRef{Key: "TCL"}}))

	fault := s.enforceIssueTeam(&linearIssue{Identifier: "TCL-1"})
	require.NotNil(t, fault, "an issue with no team must not be returned")
	assert.Equal(t, "team_unresolved", fault.Code)

	fault = s.enforceIssueTeam(nil)
	require.NotNil(t, fault)
	assert.Equal(t, http.StatusNotFound, fault.Status)
}

// TestLinearIssueFilterAlwaysCarriesTheAllowList — every list-shaped verb goes
// through this builder, so an unfiltered listing must not be something a
// handler can produce by omission.
func TestLinearIssueFilterAlwaysCarriesTheAllowList(t *testing.T) {
	s := testLinearSession("TCL", "JOH")

	t.Run("no team named: every allow-listed team", func(t *testing.T) {
		filter := testLinearFilter(t, s, "", "", false)
		clauses, ok := filter["and"].([]any)
		require.True(t, ok)
		require.Len(t, clauses, 1)
		assert.Equal(t, []string{"tcl", "joh"}, filterTeamKeys(t, filter))
	})

	t.Run("a named team narrows rather than widens", func(t *testing.T) {
		filter := testLinearFilter(t, s, "TCL", "", false)
		clauses := filter["and"].([]any)
		require.Len(t, clauses, 1)
		assert.Equal(t, []string{"TCL"}, filterTeamKeys(t, filter))
	})

	t.Run("state and assignee are ANDed on top, never instead", func(t *testing.T) {
		filter := testLinearFilter(t, s, "", "In Review", true)
		clauses := filter["and"].([]any)
		require.Len(t, clauses, 3, "the team clause must survive alongside the others")
		assert.Equal(t, []string{"tcl", "joh"}, filterTeamKeys(t, filter),
			"the team clause must still be first, and still carry the whole set")
	})
}

// TestLinearTeamClauseNeverMatchesEverything pins the shape of the one input
// the clause builder should never see. An empty team list has no honest filter,
// and both obvious spellings of one — an omitted clause, an empty `or` — mean
// "every issue in the workspace" to Linear.
func TestLinearTeamClauseNeverMatchesEverything(t *testing.T) {
	clause := linearTeamClause(nil)
	team, ok := clause["team"].(map[string]any)
	require.True(t, ok, "an empty team list must still produce a team constraint")
	assert.Equal(t, "", team["key"].(map[string]any)["eq"],
		"no team key is empty, so this matches nothing")
}

// filterTeamKeys extracts the teams a filter constrains to, whichever shape the
// clause took: one team is a direct constraint, several are an `or` over them.
func filterTeamKeys(t *testing.T, filter map[string]any) []string {
	t.Helper()
	clauses, ok := filter["and"].([]any)
	require.True(t, ok, "a filter must AND its clauses explicitly")
	require.NotEmpty(t, clauses)
	clause, ok := clauses[0].(map[string]any)
	require.True(t, ok, "the team clause must come first")

	if alternatives, isOr := clause["or"].([]any); isOr {
		keys := make([]string, 0, len(alternatives))
		for _, alt := range alternatives {
			keys = append(keys, teamKeyInClause(t, alt.(map[string]any)))
		}
		return keys
	}
	return []string{teamKeyInClause(t, clause)}
}

func teamKeyInClause(t *testing.T, clause map[string]any) string {
	t.Helper()
	team, ok := clause["team"].(map[string]any)
	require.True(t, ok, "every alternative must constrain a team")
	key, ok := team["key"].(map[string]any)["eqIgnoreCase"].(string)
	require.True(t, ok, "teams are matched case-insensitively")
	return key
}

func TestValidateLinearTitle(t *testing.T) {
	assert.Nil(t, validateLinearTitle("Fix the flaky dashsnap test"))
	assert.Nil(t, validateLinearTitle("Ärendet är löst"), "non-ASCII titles are fine")

	// Unlike the GitHub half a leading "-" is harmless here: the title never
	// reaches an argv. It must still be accepted, or the two proxies would
	// refuse different things for no reason a caller could see.
	assert.Nil(t, validateLinearTitle("-fix the thing"))

	for _, bad := range []string{
		"",
		"   ",
		"line one\nline two",
		"tab\there",
		strings.Repeat("x", maxLinearTitleLen+1),
		// U+202E RIGHT-TO-LEFT OVERRIDE, escaped rather than literal so the
		// test source itself does not render deceptively — which is the very
		// thing the validator refuses.
		"safe\u202eelbisiver",
	} {
		fault := validateLinearTitle(bad)
		require.NotNil(t, fault, "%q should be refused", bad)
		assert.Equal(t, http.StatusBadRequest, fault.Status)
	}
}

// TestValidateLinearTitleCountsRunesNotBytes — a byte count would refuse a
// perfectly legal non-ASCII title at a third of the real limit.
func TestValidateLinearTitleCountsRunesNotBytes(t *testing.T) {
	assert.Nil(t, validateLinearTitle(strings.Repeat("ä", maxLinearTitleLen)))
	assert.NotNil(t, validateLinearTitle(strings.Repeat("ä", maxLinearTitleLen+1)))
}

func TestValidateLinearAttachmentURL(t *testing.T) {
	for _, ok := range []string{
		"https://github.com/tofutools/tclaude/pull/42",
		"http://internal.example/build/7",
		"HTTPS://EXAMPLE.COM/x",
	} {
		_, fault := validateLinearAttachmentURL(ok)
		assert.Nil(t, fault, "%q should be accepted", ok)
	}
	for _, bad := range []string{
		"",
		"javascript:alert(1)",
		"data:text/html,<script>alert(1)</script>",
		"file:///etc/passwd",
		"ftp://example.com/x",
		"//example.com/x",
		"https://example.com/a b",
		"https://example.com/\nHost: evil",
		"https://" + strings.Repeat("x", 2048),
	} {
		_, fault := validateLinearAttachmentURL(bad)
		require.NotNil(t, fault, "%q should be refused", bad)
	}
}

func TestValidateLinearPriorityAndLimit(t *testing.T) {
	for p := 0; p <= 4; p++ {
		assert.Nil(t, validateLinearPriority(p))
	}
	assert.NotNil(t, validateLinearPriority(-1))
	assert.NotNil(t, validateLinearPriority(5))

	// 0 means "unset" for a limit and takes the default, which is why the
	// update handler needs a *int for priority but not for this.
	got, fault := validateLinearLimit(0)
	require.Nil(t, fault)
	assert.Equal(t, defaultLinearLimit, got)

	_, fault = validateLinearLimit(maxLinearLimit + 1)
	assert.NotNil(t, fault)
	_, fault = validateLinearLimit(-1)
	assert.NotNil(t, fault)
}

func TestResolveStateIDIsExactAndListsOnMiss(t *testing.T) {
	meta := &linearTeamMeta{Key: "TCL"}
	meta.States.Nodes = []linearStateRef{
		{ID: "s1", Name: "Todo"},
		{ID: "s2", Name: "In Review"},
		{ID: "s3", Name: "Done"},
	}

	id, fault := resolveStateID(meta, "in review")
	require.Nil(t, fault, "matching must be case-insensitive")
	assert.Equal(t, "s2", id)

	// "In Revue" is one letter from "In Review". A fuzzy matcher would move
	// the ticket anyway; this one must refuse and say what the options are.
	_, fault = resolveStateID(meta, "In Revue")
	require.NotNil(t, fault)
	assert.Equal(t, "unknown_state", fault.Code)
	assert.Contains(t, fault.Msg, "In Review")
	assert.Contains(t, fault.Msg, "Todo")
}

func TestRenderLinearCommentsKeepsTheTail(t *testing.T) {
	t.Run("empty thread says so", func(t *testing.T) {
		out := renderLinearComments("TCL-1", "A thing", "https://linear.app/x", nil)
		assert.Contains(t, out, "(no comments)")
	})

	// The input is deliberately NEWEST-FIRST, which is the order Linear
	// actually returns. A test that hand-built its slice oldest-first would
	// assert a property of the renderer that its real caller never supplies —
	// and would keep passing if the ordering broke.
	t.Run("sorts to oldest-first whatever order it is given", func(t *testing.T) {
		out := renderLinearComments("TCL-1", "t", "u", []linearComment{
			{Body: "THIRD", CreatedAt: "2026-08-03T00:00:00Z"},
			{Body: "SECOND", CreatedAt: "2026-08-02T00:00:00Z"},
			{Body: "FIRST", CreatedAt: "2026-08-01T00:00:00Z"},
		})
		first, second, third := strings.Index(out, "FIRST"), strings.Index(out, "SECOND"), strings.Index(out, "THIRD")
		require.NotEqual(t, -1, first)
		assert.Less(t, first, second)
		assert.Less(t, second, third)
	})

	t.Run("the kept tail is the newest end", func(t *testing.T) {
		comments := []linearComment{
			// Newest first, as Linear sends it. The huge one is the OLDEST, so
			// a renderer that both sorts and keeps the tail must drop it and
			// retain the short newest comment.
			{Body: "THE-NEWEST-COMMENT", CreatedAt: "2026-08-08T00:00:00Z"},
			{Body: strings.Repeat("old ", maxLinearCommentsTextBytes/4), CreatedAt: "2026-01-01T00:00:00Z"},
		}
		out := renderLinearComments("TCL-1", "A thing", "https://linear.app/x", comments)
		assert.LessOrEqual(t, len(out), maxLinearCommentsTextBytes+len("(earlier comments truncated)\n"))
		assert.Contains(t, out, "THE-NEWEST-COMMENT",
			"the tail is what is kept, because that is where the newest comments are")
		assert.Contains(t, out, "(earlier comments truncated)")
	})

	t.Run("an unattributed comment still renders", func(t *testing.T) {
		out := renderLinearComments("TCL-1", "t", "u", []linearComment{{Body: "hi", CreatedAt: "now"}})
		assert.Contains(t, out, "(unknown)")
		assert.Contains(t, out, "hi")
	})
}

// TestLinearGraphQLFaultClassification — the codes are what an agent branches
// on, so a misclassified error means a retry loop against a call that can
// never succeed.
func TestLinearGraphQLFaultClassification(t *testing.T) {
	mk := func(code, presentable string) []linearError {
		var e linearError
		e.Message = "raw"
		e.Extensions.Code = code
		e.Extensions.UserPresentableMessage = presentable
		return []linearError{e}
	}

	fault := linearGraphQLFault(200, mk("AUTHENTICATION_ERROR", "You need to authenticate."))
	assert.Equal(t, "linear_auth", fault.Code)
	assert.Equal(t, http.StatusServiceUnavailable, fault.Status)
	assert.Contains(t, fault.Msg, "You need to authenticate.")

	fault = linearGraphQLFault(400, mk("GRAPHQL_VALIDATION_FAILED", "Cannot query field"))
	assert.Equal(t, "linear_schema_drift", fault.Code)
	assert.Contains(t, fault.Msg, "tclaude bug")

	fault = linearGraphQLFault(429, mk("RATELIMITED", "Slow down"))
	assert.Equal(t, http.StatusTooManyRequests, fault.Status)

	// No userPresentableMessage: the terser `message` must still surface.
	fault = linearGraphQLFault(200, mk("SOMETHING_ELSE", ""))
	assert.Equal(t, "linear_failed", fault.Code)
	assert.Contains(t, fault.Msg, "raw")
}

// TestLinearProxyConfigIsFailClosed pins the one rule that must never be
// relaxed: no allow-list means nothing is reachable.
func TestLinearProxyConfigIsFailClosed(t *testing.T) {
	for name, cfg := range map[string]*config.Config{
		"nil config":       nil,
		"no agent block":   {},
		"no linear block":  {Agent: &config.AgentConfig{}},
		"empty allow-list": {Agent: &config.AgentConfig{LinearProxy: &config.LinearProxyConfig{}}},
		"blank entries": {Agent: &config.AgentConfig{
			LinearProxy: &config.LinearProxyConfig{AllowedTeams: []string{"", "  "}},
		}},
	} {
		assert.False(t, cfg.LinearProxyEnabled(), "%s must leave the proxy disabled", name)
	}

	enabled := &config.Config{Agent: &config.AgentConfig{
		LinearProxy: &config.LinearProxyConfig{AllowedTeams: []string{"TCL"}},
	}}
	assert.True(t, enabled.LinearProxyEnabled())
}

// TestLinearRoutesGroupTeamsByCredential — the routing a team-spanning verb
// fans out over. Teams no workspaces entry claims share the default route, and
// routes come out in the order teams first appear, so a fan-out is
// deterministic rather than map-ordered.
func TestLinearRoutesGroupTeamsByCredential(t *testing.T) {
	policy := config.LinearProxyConfig{
		AllowedTeams: []string{"tcl", "acm", "joh", "ops"},
		APIKeyFile:   "/tmp/default.key",
		Workspaces: []config.LinearWorkspaceConfig{
			{Name: "acme", APIKeyFile: "/tmp/acme.key", Teams: []string{"acm", "ops"}},
		},
	}
	routes, byTeam, fault := linearRoutes(policy, policy.AllowedTeams)
	require.Nil(t, fault)
	require.Len(t, routes, 2)

	assert.Equal(t, "default", routes[0].name)
	assert.Equal(t, []string{"tcl", "joh"}, routes[0].teams)
	assert.Equal(t, "acme", routes[1].name)
	assert.Equal(t, []string{"acm", "ops"}, routes[1].teams)

	assert.Same(t, routes[1], byTeam["acm"], "a claimed team must resolve to its own workspace")
	assert.Same(t, routes[0], byTeam["joh"], "an unclaimed team falls back to the default key")
}

// TestFanoutIsBoundedButSingleTeamVerbsAreNot — a team-spanning verb spends one
// credential per workspace inside a single budget, so it is bounded. A verb that
// names a team spends one credential whatever the operator configured, and
// refusing it for the shape of a listing it is not performing would be a
// restriction with no cause behind it.
func TestFanoutIsBoundedButSingleTeamVerbsAreNot(t *testing.T) {
	policy := config.LinearProxyConfig{APIKeyFile: "/tmp/default.key"}
	var teams []string
	for i := 0; i <= maxLinearFanout; i++ {
		key := fmt.Sprintf("t%d", i)
		teams = append(teams, key)
		policy.Workspaces = append(policy.Workspaces, config.LinearWorkspaceConfig{
			Name: key, APIKeyFile: "/tmp/" + key + ".key", Teams: []string{key},
		})
	}
	policy.AllowedTeams = teams

	// The policy itself resolves: too many workspaces is not a broken policy.
	routes, byTeam, fault := linearRoutes(policy, teams)
	require.Nil(t, fault)
	require.Len(t, routes, maxLinearFanout+1)
	s := &linearProxySession{policy: policy, teams: teams, routes: routes, routeByTeam: byTeam}

	_, fault = s.scanTargets("")
	require.NotNil(t, fault, "a fan-out past the bound must be named, not silently attempted")
	assert.Equal(t, linearMisconfiguredCode, fault.Code)

	targets, fault := s.scanTargets("t0")
	require.Nil(t, fault, "naming a team spends one credential, so the bound does not apply")
	require.Len(t, targets, 1)
	assert.Equal(t, "t0", targets[0].route.name)

	// One workspace fewer and the fan-out is allowed, so the bound is on the
	// fan-out rather than on how many workspaces an operator may configure.
	routes, byTeam, fault = linearRoutes(policy, teams[:maxLinearFanout])
	require.Nil(t, fault)
	s = &linearProxySession{
		policy: policy, teams: teams[:maxLinearFanout], routes: routes, routeByTeam: byTeam,
	}
	_, fault = s.scanTargets("")
	assert.Nil(t, fault)
}

// TestLinearRoutesRefuseAWorkspaceNamedDefault — `whoami` reports routes by
// name, and the credential every unclaimed team uses is already called
// "default". Two rows by that name in the verb an operator runs to work out
// which key answered is worse than making them pick another label.
func TestLinearRoutesRefuseAWorkspaceNamedDefault(t *testing.T) {
	policy := config.LinearProxyConfig{
		AllowedTeams: []string{"tcl"},
		APIKeyFile:   "/tmp/default.key",
		Workspaces: []config.LinearWorkspaceConfig{
			{Name: "Default", APIKeyFile: "/tmp/other.key", Teams: []string{"acm"}},
		},
	}
	_, _, fault := linearRoutes(policy, policy.AllowedTeams)
	require.NotNil(t, fault, "the default route's name is reserved, case included")
	assert.Equal(t, linearMisconfiguredCode, fault.Code)
	assert.Contains(t, fault.Msg, "different name")
}

// TestMergeByUpdatedIsNewestFirstAndNeverNil — the merge restores across
// workspaces what orderBy: updatedAt promises within one, and an empty result
// must render as `[]` however many workspaces produced it. A nil slice here
// would make the response's JSON type depend on operator configuration the
// agent cannot see.
func TestMergeByUpdatedIsNewestFirstAndNeverNil(t *testing.T) {
	row := func(id, updated string) linearIssue {
		return linearIssue{Identifier: id, UpdatedAt: updated}
	}

	t.Run("newest first across groups", func(t *testing.T) {
		merged := mergeByUpdated([][]linearIssue{
			{row("A-2", "2026-08-09"), row("A-1", "2026-08-01")},
			{row("B-9", "2026-08-10"), row("B-8", "2026-08-05")},
			{row("C-1", "2026-08-07")},
		}, 25)
		assert.Equal(t, []string{"B-9", "A-2", "C-1", "B-8", "A-1"}, identifiersOf(merged))
	})

	t.Run("the limit bounds the merged result", func(t *testing.T) {
		merged := mergeByUpdated([][]linearIssue{
			{row("A-1", "2026-08-01")},
			{row("B-1", "2026-08-02")},
			{row("C-1", "2026-08-03")},
		}, 2)
		assert.Equal(t, []string{"C-1", "B-1"}, identifiersOf(merged))
	})

	t.Run("a tie keeps workspace order", func(t *testing.T) {
		merged := mergeByUpdated([][]linearIssue{
			{row("A-1", "2026-08-01")},
			{row("B-1", "2026-08-01")},
		}, 25)
		assert.Equal(t, []string{"A-1", "B-1"}, identifiersOf(merged),
			"a stable sort must not reorder rows Linear ranked equally")
	})

	t.Run("empty is [] whatever the workspace count", func(t *testing.T) {
		for name, groups := range map[string][][]linearIssue{
			"one workspace":  {{}},
			"two workspaces": {{}, {}},
		} {
			out, err := json.Marshal(mergeByUpdated(groups, 25))
			require.NoError(t, err)
			assert.Equal(t, "[]", string(out), "%s must render an empty listing the same way", name)
		}
	})

	t.Run("a single group is passed through untouched", func(t *testing.T) {
		// Not re-sorted: Linear already ordered it, and re-sorting could only
		// introduce differences.
		merged := mergeByUpdated([][]linearIssue{{row("A-1", ""), row("A-2", "2026-08-09")}}, 25)
		assert.Equal(t, []string{"A-1", "A-2"}, identifiersOf(merged))
	})
}

// TestMergeByRelevanceTakesTurns — relevance ranks from two responses are not
// comparable, so a bounded search result must not be filled by whichever
// workspace happens to be first.
func TestMergeByRelevanceTakesTurns(t *testing.T) {
	row := func(id string) linearIssue { return linearIssue{Identifier: id} }

	t.Run("round-robin across groups", func(t *testing.T) {
		merged := mergeByRelevance([][]linearIssue{
			{row("A-1"), row("A-2")},
			{row("B-1"), row("B-2")},
			{row("C-1"), row("C-2")},
		}, 25)
		assert.Equal(t, []string{"A-1", "B-1", "C-1", "A-2", "B-2", "C-2"}, identifiersOf(merged))
	})

	t.Run("ragged groups drain rather than stall", func(t *testing.T) {
		merged := mergeByRelevance([][]linearIssue{
			{row("A-1")},
			{row("B-1"), row("B-2"), row("B-3")},
		}, 25)
		assert.Equal(t, []string{"A-1", "B-1", "B-2", "B-3"}, identifiersOf(merged),
			"an exhausted group must not end the merge while another still has rows")
	})

	t.Run("the limit can cut mid-round", func(t *testing.T) {
		merged := mergeByRelevance([][]linearIssue{
			{row("A-1"), row("A-2")},
			{row("B-1"), row("B-2")},
		}, 3)
		assert.Equal(t, []string{"A-1", "B-1", "A-2"}, identifiersOf(merged))
	})

	t.Run("empty is [] whatever the workspace count", func(t *testing.T) {
		out, err := json.Marshal(mergeByRelevance([][]linearIssue{{}, {}}, 25))
		require.NoError(t, err)
		assert.Equal(t, "[]", string(out))
	})
}

func identifiersOf(issues []linearIssue) []string {
	ids := make([]string, 0, len(issues))
	for _, issue := range issues {
		ids = append(ids, issue.Identifier)
	}
	return ids
}

// TestLinearRouteKeyEnvFallbackIsTheDefaultRouteAlone — one LINEAR_API_KEY names
// one workspace's key. Letting a second route borrow it would answer that
// route's teams with the wrong workspace's credential, which Linear reports as
// a missing issue rather than as an error.
func TestLinearRouteKeyEnvFallbackIsTheDefaultRouteAlone(t *testing.T) {
	t.Setenv("LINEAR_API_KEY", "lin_api_env")
	s := &linearProxySession{}

	key, fault := s.routeKey(&linearRoute{name: "default", isDefault: true, teams: []string{"tcl"}})
	require.Nil(t, fault)
	assert.Equal(t, "lin_api_env", key)

	// linearRoutes already refuses a workspace entry with no key file, so this
	// route cannot be built from a real policy. The rule is enforced here too
	// because the two would have to be wrong TOGETHER for the environment key
	// to answer a workspace it does not belong to.
	_, fault = s.routeKey(&linearRoute{name: "acme", teams: []string{"acm"}})
	require.NotNil(t, fault, "a workspace route must never borrow the environment key")
	assert.Equal(t, linearMisconfiguredCode, fault.Code)
}

// TestLinearWriteCeilingIsIndependentOfTheSlug — allow_write is the operator's
// ceiling and defaults off, so a granted agent still cannot write until the
// operator says any agent may.
func TestLinearWriteCeilingIsIndependentOfTheSlug(t *testing.T) {
	s := testLinearSession("TCL")
	fault := s.requireWrite()
	require.NotNil(t, fault, "allow_write defaults to off")
	assert.Equal(t, "linear_write_disabled", fault.Code)

	cfg := &config.Config{Agent: &config.AgentConfig{
		LinearProxy: &config.LinearProxyConfig{AllowedTeams: []string{"TCL"}, AllowWrite: true},
	}}
	s = &linearProxySession{policy: cfg.ResolvedLinearProxy()}
	assert.Nil(t, s.requireWrite())
}

// TestLinearProxyDocumentsAreConstants is a structural guard on the invariant
// the whole design rests on. It cannot prove the documents were not built at
// runtime — the Go compiler already guarantees that for a `const` — but it does
// catch the two things that would silently weaken them: a document that stopped
// naming its operation, and an issue selection that lost the `team { key }`
// the allow-list is enforced on.
// TestRequireMutationBudgetRefusesRatherThanWriteHalfway — the reads a write
// verb makes first can eat the whole budget when Linear is degraded, and a
// mutation sent with a sliver of deadline left gets cut off mid-flight. That is
// the one outcome the budget exists to prevent: the agent cannot tell whether
// the write landed, and a retry writes twice under the operator's name.
func TestRequireMutationBudgetRefusesRatherThanWriteHalfway(t *testing.T) {
	s := testLinearSession("TCL")

	// A session built by hand has no deadline, which means unbounded — the unit
	// tests rely on that, so it must not start refusing writes.
	assert.Nil(t, s.requireMutationBudget(), "an unbounded session has nothing to run out of")

	s.deadline = time.Now().Add(linearProxyBudget)
	assert.Nil(t, s.requireMutationBudget(), "a fresh budget must allow the write")

	s.deadline = time.Now().Add(linearMutationHeadroom / 2)
	fault := s.requireMutationBudget()
	require.NotNil(t, fault)
	assert.Equal(t, "linear_budget_spent", fault.Code)
	assert.Contains(t, fault.Msg, "nothing was written",
		"the refusal has to say the write did not happen, or a retry is a coin toss")
}

func TestLinearProxyDocumentsAreConstants(t *testing.T) {
	require.NotEmpty(t, linearProxyDocuments)
	for name, doc := range linearProxyDocuments {
		assert.True(t,
			strings.Contains(doc, "query ") || strings.Contains(doc, "mutation "),
			"document %q names no operation", name)
		assert.NotContains(t, doc, "%s", "document %q looks like a format string", name)
		assert.NotContains(t, doc, "%v", "document %q looks like a format string", name)
	}

	// Every document that can return an issue must ask for its team, because
	// enforceIssueTeam refuses an issue it cannot check.
	for _, name := range []string{"issue", "issues", "search", "comments", "issueCreate", "issueUpdate"} {
		assert.Contains(t, linearProxyDocuments[name], "team {",
			"document %q can return an issue but does not select its team, so the allow-list "+
				"could not be enforced on it", name)
	}
}
