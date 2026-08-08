package agentd_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// linearproxy_flow_test.go drives the Linear proxy through the real daemon mux
// with the outbound-HTTP boundary stubbed.
//
// The assertions concentrate on what would fail SILENTLY: that the team
// allow-list actually bounds what an agent can reach (in both the request the
// daemon builds and the answer it will return), that reading does not confer
// writing, that the operator's own allow_write ceiling is independent of the
// permission slug, and that no caller value can reach the GraphQL document.

const linearProxyTestConv = "conv-linear-proxy"

// linearRecorder captures every GraphQL request the daemon builds and replays
// a scripted response. Capturing the document AND the variables separately is
// the point: it is what lets a test assert that a caller's string landed in
// `variables` and never in the document.
type linearRecorder struct {
	mu       sync.Mutex
	calls    []linearCall
	response func(call linearCall) (int, string)
	err      error
}

type linearCall struct {
	Key       string
	Query     string
	Variables map[string]any
}

func (r *linearRecorder) do(_ context.Context, key string, req any) (int, []byte, error) {
	// req is agentd.linearRequest, unexported — round-trip it through JSON,
	// which is also exactly what the real transport does with it.
	raw, err := json.Marshal(req)
	if err != nil {
		return 0, nil, err
	}
	var decoded struct {
		Query     string         `json:"query"`
		Variables map[string]any `json:"variables"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return 0, nil, err
	}
	call := linearCall{Key: key, Query: decoded.Query, Variables: decoded.Variables}

	r.mu.Lock()
	r.calls = append(r.calls, call)
	respond := r.response
	stubErr := r.err
	r.mu.Unlock()

	if stubErr != nil {
		return 0, nil, stubErr
	}
	if respond == nil {
		return http.StatusOK, []byte(`{"data":{}}`), nil
	}
	status, body := respond(call)
	return status, []byte(body), nil
}

func (r *linearRecorder) snapshot() []linearCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]linearCall(nil), r.calls...)
}

func (r *linearRecorder) count() int { return len(r.snapshot()) }

func (r *linearRecorder) sawAnyCall() bool { return r.count() > 0 }

// only returns the single call the recorder saw.
func (r *linearRecorder) only(t *testing.T) linearCall {
	t.Helper()
	calls := r.snapshot()
	require.Len(t, calls, 1, "expected exactly one Linear call")
	return calls[0]
}

// linearWorld builds a daemon world with an enrolled agent and an operator
// policy. allowed == nil writes no linear_proxy block at all, which is the
// fail-closed "operator never opted in" case.
func linearWorld(t *testing.T, allowed []string, tweak ...func(*config.LinearProxyConfig)) (*testharness.Flow, *linearRecorder) {
	t.Helper()
	f := newFlow(t)
	f.HaveConvWithTitle(linearProxyTestConv, "ticket-worker")
	f.HaveEnrolledAgent(linearProxyTestConv)

	if allowed == nil {
		require.NoError(t, config.Save(&config.Config{Agent: &config.AgentConfig{}}))
	} else {
		writeLinearConfig(t, allowed, tweak...)
	}

	rec := &linearRecorder{}
	t.Cleanup(agentd.SetLinearTransportForTestJSON(rec.do))
	// A key in the environment, so the tests exercise the same resolution path
	// an operator without an api_key_file uses.
	t.Setenv("LINEAR_API_KEY", "lin_api_testkey")
	return f, rec
}

func writeLinearConfig(t *testing.T, allowed []string, tweak ...func(*config.LinearProxyConfig)) {
	t.Helper()
	proxy := &config.LinearProxyConfig{AllowedTeams: allowed}
	for _, fn := range tweak {
		fn(proxy)
	}
	require.NoError(t, config.Save(&config.Config{Agent: &config.AgentConfig{LinearProxy: proxy}}))
}

func linearPost(t *testing.T, f *testharness.Flow, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	return testharness.Serve(f.Mux,
		agentd.AsAgentPeer(testharness.JSONRequest(t, http.MethodPost, path, body), linearProxyTestConv))
}

// issueJSON builds a scripted `issue` response for a given identifier/team.
// It carries an `id` because the real linearQueryIssue selects one — the write
// verbs mutate by that UUID rather than by the identifier.
func issueJSON(identifier, teamKey string) string {
	return `{"data":{"issue":{"id":"` + issueUUIDFor(identifier) + `","identifier":"` + identifier +
		`","title":"A thing","url":"https://linear.app/acme/issue/` + identifier +
		`","team":{"key":"` + teamKey + `","name":"Team"}}}}`
}

// issueUUIDFor is a stable fake UUID per identifier, so a test can assert that
// the mutation carried the UUID and not the identifier.
func issueUUIDFor(identifier string) string {
	return "uuid-of-" + strings.ToLower(identifier)
}

// --- fail-closed configuration ---

// TestLinearProxy_DisabledUntilTeamsAllowListed is the fail-closed contract:
// an operator who has not configured the proxy has not enabled it, and no
// grant can change that.
func TestLinearProxy_DisabledUntilTeamsAllowListed(t *testing.T) {
	t.Run("no linear_proxy block at all", func(t *testing.T) {
		f, rec := linearWorld(t, nil)
		require.NoError(t, db.GrantAgentPermission(linearProxyTestConv, agentd.PermLinearRead, "test"))

		res := linearPost(t, f, "/v1/linear/issue/view", map[string]any{"identifier": "TCL-1"})
		assert.Equal(t, http.StatusServiceUnavailable, res.Code, "body=%s", res.Body.String())
		assert.Contains(t, res.Body.String(), "linear_proxy_disabled")
		assert.False(t, rec.sawAnyCall(), "a disabled proxy must not reach Linear at all")
	})

	t.Run("an explicitly empty allow-list is still disabled", func(t *testing.T) {
		f, rec := linearWorld(t, []string{})
		require.NoError(t, db.GrantAgentPermission(linearProxyTestConv, agentd.PermLinearRead, "test"))

		res := linearPost(t, f, "/v1/linear/issue/view", map[string]any{"identifier": "TCL-1"})
		assert.Equal(t, http.StatusServiceUnavailable, res.Code, "body=%s", res.Body.String())
		assert.False(t, rec.sawAnyCall())
	})
}

// --- permission gating ---

// TestLinearProxy_WriteRequiresItsOwnSlug — reading a ticket must not confer
// the ability to write to the workspace under the operator's name.
func TestLinearProxy_WriteRequiresItsOwnSlug(t *testing.T) {
	t.Run("ungranted", func(t *testing.T) {
		f, rec := linearWorld(t, []string{"TCL"})
		res := linearPost(t, f, "/v1/linear/issue/comment",
			map[string]any{"identifier": "TCL-1", "body": "done"})
		assert.Equal(t, http.StatusForbidden, res.Code, "body=%s", res.Body.String())
		assert.False(t, rec.sawAnyCall(), "a denied caller must not reach Linear — not even a probe")
	})

	t.Run("linear.read does not confer linear.write", func(t *testing.T) {
		f, rec := linearWorld(t, []string{"TCL"})
		require.NoError(t, db.GrantAgentPermission(linearProxyTestConv, agentd.PermLinearRead, "test"))
		res := linearPost(t, f, "/v1/linear/issue/comment",
			map[string]any{"identifier": "TCL-1", "body": "done"})
		assert.Equal(t, http.StatusForbidden, res.Code, "body=%s", res.Body.String())
		assert.False(t, rec.sawAnyCall())
	})
}

// TestLinearProxy_WriteAlsoNeedsTheOperatorCeiling is the second half of the
// write gate, and the one that is easy to lose: the slug says THIS AGENT may
// write, allow_write says the OPERATOR wants any agent to be able to. A grant
// must not be able to override the operator's own setting.
func TestLinearProxy_WriteAlsoNeedsTheOperatorCeiling(t *testing.T) {
	f, rec := linearWorld(t, []string{"TCL"}) // allow_write defaults to false
	require.NoError(t, db.GrantAgentPermission(linearProxyTestConv, agentd.PermLinearWrite, "test"))

	res := linearPost(t, f, "/v1/linear/issue/comment",
		map[string]any{"identifier": "TCL-1", "body": "done"})
	assert.Equal(t, http.StatusForbidden, res.Code, "body=%s", res.Body.String())
	assert.Contains(t, res.Body.String(), "linear_write_disabled")
	assert.False(t, rec.sawAnyCall(),
		"the operator's ceiling must be checked before anything reaches Linear")
}

// --- the team gate ---

// TestLinearProxy_TeamAllowListBoundsTheIdentifier refuses a team the operator
// never allow-listed, before any credential is spent.
func TestLinearProxy_TeamAllowListBoundsTheIdentifier(t *testing.T) {
	f, rec := linearWorld(t, []string{"TCL"})
	require.NoError(t, db.GrantAgentPermission(linearProxyTestConv, agentd.PermLinearRead, "test"))

	res := linearPost(t, f, "/v1/linear/issue/view", map[string]any{"identifier": "SECRET-1"})
	assert.Equal(t, http.StatusForbidden, res.Code, "body=%s", res.Body.String())
	assert.Contains(t, res.Body.String(), "team_not_allowed")
	assert.False(t, rec.sawAnyCall(),
		"the cheap gate must run before the network, not after it")
}

// TestLinearProxy_TeamGateReChecksLinearsAnswer is the load-bearing half of the
// team gate.
//
// The identifier check is a check on a string the CALLER supplied. Linear can
// legitimately resolve that identifier to an issue in a different team — an
// issue moved between teams keeps answering to its old identifier — so a proxy
// that trusted the prefix alone would hand over an issue the operator never
// allow-listed. The daemon must refuse on the team LINEAR REPORTED, and the
// issue's contents must not appear in the response.
func TestLinearProxy_TeamGateReChecksLinearsAnswer(t *testing.T) {
	f, rec := linearWorld(t, []string{"TCL"})
	require.NoError(t, db.GrantAgentPermission(linearProxyTestConv, agentd.PermLinearRead, "test"))
	rec.response = func(linearCall) (int, string) {
		// The caller asked for TCL-1; Linear answers with an issue in SECRET.
		return http.StatusOK, `{"data":{"issue":{"identifier":"SECRET-9","title":"Acquisition plan",` +
			`"description":"confidential","team":{"key":"SECRET","name":"Secret"}}}}`
	}

	res := linearPost(t, f, "/v1/linear/issue/view", map[string]any{"identifier": "TCL-1"})
	assert.Equal(t, http.StatusForbidden, res.Code, "body=%s", res.Body.String())
	assert.NotContains(t, res.Body.String(), "Acquisition plan",
		"an issue outside the allow-list must not leak through the refusal")
	assert.NotContains(t, res.Body.String(), "confidential")
}

// TestLinearProxy_ListIsBoundedByTheAllowList checks both halves of the
// listing gate: the filter the daemon SENDS, and the rows it will RETURN.
func TestLinearProxy_ListIsBoundedByTheAllowList(t *testing.T) {
	f, rec := linearWorld(t, []string{"TCL", "JOH"})
	require.NoError(t, db.GrantAgentPermission(linearProxyTestConv, agentd.PermLinearRead, "test"))
	rec.response = func(linearCall) (int, string) {
		// Linear returns a row the filter should have excluded. The daemon's
		// own check, not the filter, is the gate.
		return http.StatusOK, `{"data":{"issues":{"nodes":[
			{"identifier":"TCL-1","title":"mine","team":{"key":"TCL"}},
			{"identifier":"SECRET-1","title":"not mine","team":{"key":"SECRET"}}
		]}}}`
	}

	res := linearPost(t, f, "/v1/linear/issue/list", map[string]any{})
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	assert.Contains(t, res.Body.String(), "TCL-1")
	assert.NotContains(t, res.Body.String(), "SECRET-1",
		"a row outside the allow-list must be dropped even when Linear returns it")

	// And the request itself carried the allow-list, so the common case does
	// not depend on the post-filter above.
	call := rec.only(t)
	vars, _ := json.Marshal(call.Variables)
	assert.Contains(t, strings.ToLower(string(vars)), "tcl")
	assert.Contains(t, strings.ToLower(string(vars)), "joh")
}

// --- the document/variables invariant ---

// TestLinearProxy_CallerValuesNeverReachTheDocument is the structural property
// this whole design rests on: a caller can change what an operation is asked
// ABOUT, never which operation runs.
//
// It is asserted rather than assumed because the failure is invisible — a
// document built by string concatenation looks identical in every test that
// only checks the response.
func TestLinearProxy_CallerValuesNeverReachTheDocument(t *testing.T) {
	f, rec := linearWorld(t, []string{"TCL"})
	require.NoError(t, db.GrantAgentPermission(linearProxyTestConv, agentd.PermLinearRead, "test"))
	rec.response = func(linearCall) (int, string) { return http.StatusOK, issueJSON("TCL-1", "TCL") }

	// A search term that would be catastrophic if it were interpolated into a
	// GraphQL document rather than carried as a variable.
	const hostile = `x") { id } } mutation { issueDelete(id: "TCL-1`
	res := linearPost(t, f, "/v1/linear/issue/search", map[string]any{"term": hostile})
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())

	call := rec.only(t)
	assert.NotContains(t, call.Query, "issueDelete",
		"the caller's text reached the GraphQL document — it must only ever reach variables")
	assert.NotContains(t, call.Query, hostile)
	assert.Equal(t, hostile, call.Variables["term"],
		"the term must arrive intact as a variable")
	assert.Contains(t, call.Query, "query IssueSearch(",
		"the document must be the shipped constant, unchanged")
}

// TestLinearProxy_IdentifierMustBeInIdentifierForm — a raw UUID carries no team
// key, so accepting one would mean there was nothing to check the allow-list
// against before spending the credential.
func TestLinearProxy_IdentifierMustBeInIdentifierForm(t *testing.T) {
	f, rec := linearWorld(t, []string{"TCL"})
	require.NoError(t, db.GrantAgentPermission(linearProxyTestConv, agentd.PermLinearRead, "test"))

	for _, bad := range []string{
		"8a7d5f6e-1234-4a2b-9c8d-1122334455ff", // a real Linear UUID
		"TCL",
		"-1",
		"TCL-",
		"TCL-abc",
		"TCL-1-2",
	} {
		res := linearPost(t, f, "/v1/linear/issue/view", map[string]any{"identifier": bad})
		assert.Equal(t, http.StatusBadRequest, res.Code, "%q should be refused; body=%s", bad, res.Body.String())
	}
	assert.False(t, rec.sawAnyCall(), "a malformed identifier must not reach Linear")
}

// --- writes ---

// TestLinearProxy_CommentConfirmsTheTeamBeforeWriting — commentCreate takes an
// issue reference and would happily accept one outside the allow-list, so the
// daemon reads the issue first and refuses on Linear's own answer. Nothing may
// be written in that case.
func TestLinearProxy_CommentConfirmsTheTeamBeforeWriting(t *testing.T) {
	f, rec := linearWorld(t, []string{"TCL"}, func(c *config.LinearProxyConfig) { c.AllowWrite = true })
	require.NoError(t, db.GrantAgentPermission(linearProxyTestConv, agentd.PermLinearWrite, "test"))
	rec.response = func(call linearCall) (int, string) {
		if strings.Contains(call.Query, "query IssueView") {
			return http.StatusOK, issueJSON("SECRET-9", "SECRET")
		}
		return http.StatusOK, `{"data":{"commentCreate":{"success":true,"comment":{"url":"u","createdAt":"t"}}}}`
	}

	res := linearPost(t, f, "/v1/linear/issue/comment",
		map[string]any{"identifier": "TCL-1", "body": "progress"})
	assert.Equal(t, http.StatusForbidden, res.Code, "body=%s", res.Body.String())

	for _, call := range rec.snapshot() {
		assert.NotContains(t, call.Query, "mutation CommentCreate",
			"a refused write must not have run the mutation")
	}
}

// TestLinearProxy_CommentSucceedsOnAnAllowedTeam is the happy path, and it
// pins the body into variables rather than the document.
func TestLinearProxy_CommentSucceedsOnAnAllowedTeam(t *testing.T) {
	f, rec := linearWorld(t, []string{"TCL"}, func(c *config.LinearProxyConfig) { c.AllowWrite = true })
	require.NoError(t, db.GrantAgentPermission(linearProxyTestConv, agentd.PermLinearWrite, "test"))
	rec.response = func(call linearCall) (int, string) {
		if strings.Contains(call.Query, "query IssueView") {
			return http.StatusOK, issueJSON("TCL-1", "TCL")
		}
		return http.StatusOK, `{"data":{"commentCreate":{"success":true,"comment":{"url":"https://linear.app/c/1","createdAt":"2026-08-08"}}}}`
	}

	const body = "Pushed the fix; CI is green."
	res := linearPost(t, f, "/v1/linear/issue/comment",
		map[string]any{"identifier": "TCL-1", "body": body})
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())

	calls := rec.snapshot()
	require.Len(t, calls, 2, "expected a confirming read then the mutation")
	mutation := calls[1]
	assert.Contains(t, mutation.Query, "mutation CommentCreate(")
	assert.NotContains(t, mutation.Query, body, "the comment body must not reach the document")
	input, ok := mutation.Variables["input"].(map[string]any)
	require.True(t, ok, "the mutation must carry an input variable")
	assert.Equal(t, body, input["body"])
	assert.Equal(t, "TCL-1", input["issueId"])
}

// TestLinearProxy_UpdateRefusesAnUnknownState — a fuzzy match here would move a
// ticket to the wrong column silently, so a miss must be an error that lists
// the real states.
func TestLinearProxy_UpdateRefusesAnUnknownState(t *testing.T) {
	f, rec := linearWorld(t, []string{"TCL"}, func(c *config.LinearProxyConfig) { c.AllowWrite = true })
	require.NoError(t, db.GrantAgentPermission(linearProxyTestConv, agentd.PermLinearWrite, "test"))
	rec.response = func(call linearCall) (int, string) {
		switch {
		case strings.Contains(call.Query, "query IssueView"):
			return http.StatusOK, issueJSON("TCL-1", "TCL")
		case strings.Contains(call.Query, "query TeamMeta"):
			return http.StatusOK, `{"data":{"teams":{"nodes":[{"id":"team-uuid","key":"TCL","name":"Tclaude",
				"states":{"nodes":[{"id":"s1","name":"Todo"},{"id":"s2","name":"In Review"}]}}]}}}`
		}
		return http.StatusOK, `{"data":{"issueUpdate":{"success":true,"issue":{"identifier":"TCL-1","team":{"key":"TCL"}}}}}`
	}

	res := linearPost(t, f, "/v1/linear/issue/update",
		map[string]any{"identifier": "TCL-1", "state": "In Revue"})
	assert.Equal(t, http.StatusBadRequest, res.Code, "body=%s", res.Body.String())
	assert.Contains(t, res.Body.String(), "In Review", "the refusal must list the real states")

	for _, call := range rec.snapshot() {
		assert.NotContains(t, call.Query, "mutation IssueUpdate",
			"an unresolved state must not reach the mutation")
	}
}

// TestLinearProxy_UpdateResolvesStateCaseInsensitively is the matching happy
// path: exact but case-insensitive, never fuzzy.
func TestLinearProxy_UpdateResolvesStateCaseInsensitively(t *testing.T) {
	f, rec := linearWorld(t, []string{"TCL"}, func(c *config.LinearProxyConfig) { c.AllowWrite = true })
	require.NoError(t, db.GrantAgentPermission(linearProxyTestConv, agentd.PermLinearWrite, "test"))
	rec.response = func(call linearCall) (int, string) {
		switch {
		case strings.Contains(call.Query, "query IssueView"):
			return http.StatusOK, issueJSON("TCL-1", "TCL")
		case strings.Contains(call.Query, "query TeamMeta"):
			return http.StatusOK, `{"data":{"teams":{"nodes":[{"id":"team-uuid","key":"TCL","name":"Tclaude",
				"states":{"nodes":[{"id":"s2","name":"In Review"}]}}]}}}`
		}
		return http.StatusOK, `{"data":{"issueUpdate":{"success":true,"issue":{"identifier":"TCL-1","team":{"key":"TCL"}}}}}`
	}

	res := linearPost(t, f, "/v1/linear/issue/update",
		map[string]any{"identifier": "TCL-1", "state": "in review"})
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())

	calls := rec.snapshot()
	last := calls[len(calls)-1]
	require.Contains(t, last.Query, "mutation IssueUpdate(")
	input, ok := last.Variables["input"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "s2", input["stateId"], "the state name must have been resolved to its UUID")

	// issueUpdate is the one write path Linear does not document as accepting
	// an identifier, so the mutation must carry the UUID the confirming read
	// returned rather than "TCL-1".
	assert.Equal(t, issueUUIDFor("TCL-1"), last.Variables["id"],
		"issue update must mutate by the issue's UUID, not by its identifier")
}

// TestLinearProxy_LargeBodyIsAccepted — the skill tells agents to use
// --body-file "for anything multi-line", and a progress report is routinely
// several kilobytes. A transport-level cap below the validator's own limit
// would reject those with a raw "request body too large" that names neither
// the real limit nor the field.
func TestLinearProxy_LargeBodyIsAccepted(t *testing.T) {
	f, rec := linearWorld(t, []string{"TCL"}, func(c *config.LinearProxyConfig) { c.AllowWrite = true })
	require.NoError(t, db.GrantAgentPermission(linearProxyTestConv, agentd.PermLinearWrite, "test"))
	rec.response = func(call linearCall) (int, string) {
		if strings.Contains(call.Query, "query IssueView") {
			return http.StatusOK, issueJSON("TCL-1", "TCL")
		}
		return http.StatusOK, `{"data":{"commentCreate":{"success":true,"comment":{"url":"u","createdAt":"t"}}}}`
	}

	body := strings.Repeat("progress report line\n", 3000) // ~60 KB
	res := linearPost(t, f, "/v1/linear/issue/comment",
		map[string]any{"identifier": "TCL-1", "body": body})
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())

	calls := rec.snapshot()
	input := calls[len(calls)-1].Variables["input"].(map[string]any)
	assert.Equal(t, body, input["body"], "the body must arrive intact")
}

// TestLinearProxy_OversizeBodyIsRefusedByName — past the real limit the
// refusal must be the validator's, which names the limit, not the transport's.
func TestLinearProxy_OversizeBodyIsRefusedByName(t *testing.T) {
	f, _ := linearWorld(t, []string{"TCL"}, func(c *config.LinearProxyConfig) { c.AllowWrite = true })
	require.NoError(t, db.GrantAgentPermission(linearProxyTestConv, agentd.PermLinearWrite, "test"))

	res := linearPost(t, f, "/v1/linear/issue/comment", map[string]any{
		"identifier": "TCL-1",
		"body":       strings.Repeat("x", 300*1024),
	})
	assert.Equal(t, http.StatusBadRequest, res.Code, "body=%s", res.Body.String())
	assert.Contains(t, res.Body.String(), "maximum",
		"the refusal should state the limit rather than report a transport failure")
}

// TestLinearProxy_CommentsAreRenderedOldestFirst — Linear's connections return
// the newest first (there is no direction control on `first:`), which is the
// right SET but the wrong ORDER to read a discussion in. Reversing the meaning
// of a "then we decided…" exchange is a silent failure, so the ordering is
// pinned here against a response in the order Linear really sends.
func TestLinearProxy_CommentsAreRenderedOldestFirst(t *testing.T) {
	f, rec := linearWorld(t, []string{"TCL"})
	require.NoError(t, db.GrantAgentPermission(linearProxyTestConv, agentd.PermLinearRead, "test"))
	rec.response = func(linearCall) (int, string) {
		// Newest first, as Linear sends it.
		return http.StatusOK, `{"data":{"issue":{"identifier":"TCL-1","title":"t","url":"u",
			"team":{"key":"TCL"},"comments":{"nodes":[
				{"body":"THIRD","createdAt":"2026-08-03T00:00:00Z"},
				{"body":"SECOND","createdAt":"2026-08-02T00:00:00Z"},
				{"body":"FIRST","createdAt":"2026-08-01T00:00:00Z"}
			]}}}}`
	}

	res := linearPost(t, f, "/v1/linear/issue/comments", map[string]any{"identifier": "TCL-1"})
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())

	out := res.Body.String()
	first, second, third := strings.Index(out, "FIRST"), strings.Index(out, "SECOND"), strings.Index(out, "THIRD")
	require.NotEqual(t, -1, first)
	assert.Less(t, first, second, "the oldest comment must be rendered first")
	assert.Less(t, second, third, "the thread must read in chronological order")
}

// TestLinearProxy_MissingIssueIsNotFoundNotABug — 502 is documented as "a
// tclaude bug, do not retry" or "could not reach Linear". A typo'd issue
// number is neither, and telling an agent to escalate one wastes a human.
func TestLinearProxy_MissingIssueIsNotFoundNotABug(t *testing.T) {
	f, rec := linearWorld(t, []string{"TCL"})
	require.NoError(t, db.GrantAgentPermission(linearProxyTestConv, agentd.PermLinearRead, "test"))
	rec.response = func(linearCall) (int, string) {
		return http.StatusBadRequest, `{"errors":[{"message":"Entity not found: Issue - Could not find referenced Issue.",
			"extensions":{"type":"invalid input","code":"INVALID_INPUT"}}]}`
	}

	res := linearPost(t, f, "/v1/linear/issue/view", map[string]any{"identifier": "TCL-99999"})
	assert.Equal(t, http.StatusNotFound, res.Code, "body=%s", res.Body.String())
	assert.Contains(t, res.Body.String(), "not_found")
	assert.NotContains(t, res.Body.String(), "linear_schema_drift")
}

// TestLinearProxy_WriteVerbsConfirmTheTeamBeforeWriting generalises the
// comment-path test to the other two write verbs, which run the same
// read-then-refuse sequence and had no coverage of their own.
func TestLinearProxy_WriteVerbsConfirmTheTeamBeforeWriting(t *testing.T) {
	for _, tc := range []struct {
		name, path, mutation string
		body                 map[string]any
	}{
		{
			name: "update", path: "/v1/linear/issue/update", mutation: "mutation IssueUpdate",
			body: map[string]any{"identifier": "TCL-1", "title": "Renamed"},
		},
		{
			name: "link", path: "/v1/linear/issue/link", mutation: "mutation AttachmentLink",
			body: map[string]any{"identifier": "TCL-1", "url": "https://example.com/pr/1"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, rec := linearWorld(t, []string{"TCL"}, func(c *config.LinearProxyConfig) { c.AllowWrite = true })
			require.NoError(t, db.GrantAgentPermission(linearProxyTestConv, agentd.PermLinearWrite, "test"))
			rec.response = func(call linearCall) (int, string) {
				if strings.Contains(call.Query, "query IssueView") {
					// The identifier says TCL; Linear says otherwise.
					return http.StatusOK, issueJSON("SECRET-9", "SECRET")
				}
				return http.StatusOK, `{"data":{}}`
			}

			res := linearPost(t, f, tc.path, tc.body)
			assert.Equal(t, http.StatusForbidden, res.Code, "body=%s", res.Body.String())
			for _, call := range rec.snapshot() {
				assert.NotContains(t, call.Query, tc.mutation,
					"a refused write must not have run the mutation")
			}
		})
	}
}

// TestLinearProxy_LinkRefusesNonHTTPURLs — an attachment renders as a
// clickable link in the operator's workspace.
func TestLinearProxy_LinkRefusesNonHTTPURLs(t *testing.T) {
	f, rec := linearWorld(t, []string{"TCL"}, func(c *config.LinearProxyConfig) { c.AllowWrite = true })
	require.NoError(t, db.GrantAgentPermission(linearProxyTestConv, agentd.PermLinearWrite, "test"))

	for _, bad := range []string{
		"javascript:alert(1)",
		"data:text/html,<script>",
		"file:///etc/passwd",
		"ftp://example.com/x",
	} {
		res := linearPost(t, f, "/v1/linear/issue/link",
			map[string]any{"identifier": "TCL-1", "url": bad})
		assert.Equal(t, http.StatusBadRequest, res.Code, "%q should be refused; body=%s", bad, res.Body.String())
	}
	assert.False(t, rec.sawAnyCall())
}

// TestLinearProxy_CreateRequiresAnAllowListedTeam — `issue create` names its
// team directly, which makes it the one verb where the allow-list is the only
// thing standing between an agent and any team in the workspace.
func TestLinearProxy_CreateRequiresAnAllowListedTeam(t *testing.T) {
	f, rec := linearWorld(t, []string{"TCL"}, func(c *config.LinearProxyConfig) { c.AllowWrite = true })
	require.NoError(t, db.GrantAgentPermission(linearProxyTestConv, agentd.PermLinearWrite, "test"))

	res := linearPost(t, f, "/v1/linear/issue/create",
		map[string]any{"team": "SECRET", "title": "Backdoor"})
	assert.Equal(t, http.StatusForbidden, res.Code, "body=%s", res.Body.String())
	assert.Contains(t, res.Body.String(), "team_not_allowed")
	assert.False(t, rec.sawAnyCall())
}

// --- error surfacing ---

// TestLinearProxy_GraphQLErrorsSurfaceUsefully — a GraphQL error arrives with
// HTTP 200, so a proxy that only checked the status would report success on a
// failed call.
func TestLinearProxy_GraphQLErrorsSurfaceUsefully(t *testing.T) {
	f, rec := linearWorld(t, []string{"TCL"})
	require.NoError(t, db.GrantAgentPermission(linearProxyTestConv, agentd.PermLinearRead, "test"))
	rec.response = func(linearCall) (int, string) {
		return http.StatusOK, `{"errors":[{"message":"Authentication required, not authenticated",
			"extensions":{"code":"AUTHENTICATION_ERROR","userPresentableMessage":"You need to authenticate."}}]}`
	}

	res := linearPost(t, f, "/v1/linear/issue/view", map[string]any{"identifier": "TCL-1"})
	assert.Equal(t, http.StatusServiceUnavailable, res.Code, "body=%s", res.Body.String())
	assert.Contains(t, res.Body.String(), "linear_auth")
	assert.Contains(t, res.Body.String(), "You need to authenticate.")
}

// TestLinearProxy_SchemaDriftIsNamedAsSuch — a validation failure is a tclaude
// bug, not a bad request, and saying so is what stops an agent retrying a call
// that can never succeed.
func TestLinearProxy_SchemaDriftIsNamedAsSuch(t *testing.T) {
	f, rec := linearWorld(t, []string{"TCL"})
	require.NoError(t, db.GrantAgentPermission(linearProxyTestConv, agentd.PermLinearRead, "test"))
	rec.response = func(linearCall) (int, string) {
		return http.StatusBadRequest, `{"errors":[{"message":"Cannot query field \"titel\" on type \"Issue\".",
			"extensions":{"code":"GRAPHQL_VALIDATION_FAILED"}}]}`
	}

	res := linearPost(t, f, "/v1/linear/issue/view", map[string]any{"identifier": "TCL-1"})
	assert.Equal(t, http.StatusBadGateway, res.Code, "body=%s", res.Body.String())
	assert.Contains(t, res.Body.String(), "linear_schema_drift")
	assert.Contains(t, res.Body.String(), "tclaude bug")
}

// TestLinearProxy_KeyIsSentButNeverReturned — the whole point of the proxy is
// that the agent does not hold the credential, so it must not come back in a
// response either.
func TestLinearProxy_KeyIsSentButNeverReturned(t *testing.T) {
	f, rec := linearWorld(t, []string{"TCL"})
	require.NoError(t, db.GrantAgentPermission(linearProxyTestConv, agentd.PermLinearRead, "test"))
	rec.response = func(linearCall) (int, string) { return http.StatusOK, issueJSON("TCL-1", "TCL") }

	res := linearPost(t, f, "/v1/linear/issue/view", map[string]any{"identifier": "TCL-1"})
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())

	assert.Equal(t, "lin_api_testkey", rec.only(t).Key, "the daemon must authenticate with the operator's key")
	assert.NotContains(t, res.Body.String(), "lin_api_testkey",
		"the key must never reach the agent")
}
