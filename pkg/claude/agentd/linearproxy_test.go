package agentd

import (
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
)

// linearproxy_test.go covers the pieces the flow tests reach only indirectly:
// the identifier and charset gates, the filter the daemon builds, and the
// comment renderer's bound.

func testLinearSession(teams ...string) *linearProxySession {
	cfg := &config.Config{Agent: &config.AgentConfig{
		LinearProxy: &config.LinearProxyConfig{AllowedTeams: teams},
	}}
	return &linearProxySession{policy: cfg.ResolvedLinearProxy(), key: "test-key"}
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
		filter := s.linearIssueFilter("", "", false)
		clauses, ok := filter["and"].([]any)
		require.True(t, ok)
		require.Len(t, clauses, 1)
		alternatives, ok := clauses[0].(map[string]any)["or"].([]any)
		require.True(t, ok, "the team clause must be an or-of-teams")
		assert.Len(t, alternatives, 2)
	})

	t.Run("a named team narrows rather than widens", func(t *testing.T) {
		filter := s.linearIssueFilter("TCL", "", false)
		clauses := filter["and"].([]any)
		require.Len(t, clauses, 1)
		team := clauses[0].(map[string]any)["team"].(map[string]any)
		assert.Equal(t, "TCL", team["key"].(map[string]any)["eqIgnoreCase"])
	})

	t.Run("state and assignee are ANDed on top, never instead", func(t *testing.T) {
		filter := s.linearIssueFilter("", "In Review", true)
		clauses := filter["and"].([]any)
		require.Len(t, clauses, 3, "the team clause must survive alongside the others")
		_, hasTeamClause := clauses[0].(map[string]any)["or"]
		assert.True(t, hasTeamClause, "the team clause must still be first")
	})
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
