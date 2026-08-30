package agentd

import (
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
)

// awbproxy_test.go covers the pieces of the AWB proxy that are decisions rather
// than plumbing: what an issue reference may be, what the operator's URL may
// be, how the project gate prunes a tree, and the compact renderer — the last
// of which is a second copy of a format awb owns, so it is pinned field by
// field.

// --- issue references ---

// TestValidateAWBIssueRef is the pre-call half of the project gate, and the
// reason it can be a pre-call half at all: a reference that carries no project
// key could only be judged after fetching the issue.
func TestValidateAWBIssueRef(t *testing.T) {
	t.Run("a full id", func(t *testing.T) {
		ref, fault := validateAWBIssueRef("awb-a3f9c1")
		require.Nil(t, fault)
		assert.Equal(t, "awb-a3f9c1", ref)
		assert.Equal(t, "awb", projectKeyOf(ref))
	})

	t.Run("a hash PREFIX still carries the project", func(t *testing.T) {
		ref, fault := validateAWBIssueRef("awb-a3f")
		require.Nil(t, fault)
		assert.Equal(t, "awb-a3f", ref)
		assert.Equal(t, "awb", projectKeyOf(ref))
	})

	t.Run("capitals resolve, as they do in awb", func(t *testing.T) {
		ref, fault := validateAWBIssueRef("AWB-A3F9C1")
		require.Nil(t, fault)
		assert.Equal(t, "awb-a3f9c1", ref)
	})

	t.Run("a project key may contain hyphens, so the split is on the LAST one", func(t *testing.T) {
		ref, fault := validateAWBIssueRef("my-proj-a3f9c1")
		require.Nil(t, fault)
		assert.Equal(t, "my-proj", projectKeyOf(ref))
	})

	t.Run("a BARE hash is refused", func(t *testing.T) {
		_, fault := validateAWBIssueRef("a3f9c1")
		require.NotNil(t, fault)
		assert.Equal(t, http.StatusBadRequest, fault.Status)
		assert.Contains(t, fault.Msg, "names no project",
			"the refusal has to say WHY a form awb itself accepts is refused here")
	})

	t.Run("a non-hex tail is not an id", func(t *testing.T) {
		_, fault := validateAWBIssueRef("awb-zzzz")
		require.NotNil(t, fault)
		assert.Contains(t, fault.Msg, "hexadecimal")
	})

	t.Run("a path separator cannot ride in", func(t *testing.T) {
		for _, bad := range []string{"awb-a3/../../admin", "awb/a3f9c1", "../awb-a3f9c1"} {
			_, fault := validateAWBIssueRef(bad)
			assert.NotNil(t, fault, "%q must not validate", bad)
		}
	})

	t.Run("empty", func(t *testing.T) {
		_, fault := validateAWBIssueRef("   ")
		require.NotNil(t, fault)
	})
}

// TestAWBProjectKeyShape pins the rule the permission-scope parser borrows, so
// a matcher an operator can write is always a key a request could also carry.
func TestAWBProjectKeyShape(t *testing.T) {
	for _, ok := range []string{"awb", "a", "my-proj", "web2", "a-1-b"} {
		assert.NoError(t, awbProjectKeyShapeErr(ok), "%q is a valid AWB project key", ok)
	}
	for _, bad := range []string{"", "  ", "1web", "-web", "WEB", "we b", "web_x", "web/x", "*",
		"averyveryverylongkey"} {
		assert.Error(t, awbProjectKeyShapeErr(bad), "%q must be refused", bad)
	}
}

// --- the operator's URL ---

func TestValidateAWBBaseURL(t *testing.T) {
	t.Run("http and https, trailing slash trimmed", func(t *testing.T) {
		for raw, want := range map[string]string{
			"https://awb.example":      "https://awb.example",
			"https://awb.example/":     "https://awb.example",
			"http://127.0.0.1:8080":    "http://127.0.0.1:8080",
			"https://example.com/awb/": "https://example.com/awb",
		} {
			got, fault := validateAWBBaseURL(raw)
			require.Nil(t, fault, "%q", raw)
			assert.Equal(t, want, got)
		}
	})

	t.Run("unconfigured is its own answer", func(t *testing.T) {
		_, fault := validateAWBBaseURL("")
		require.NotNil(t, fault)
		assert.Equal(t, awbNotConfiguredCode, fault.Code,
			"'no server' must be distinguishable from 'a bad server'")
	})

	t.Run("credentials in the URL are refused, not stripped", func(t *testing.T) {
		_, fault := validateAWBBaseURL("https://bot:hunter2@awb.example")
		require.NotNil(t, fault)
		assert.Equal(t, awbMisconfiguredCode, fault.Code)
		assert.Contains(t, fault.Msg, "password_file",
			"the refusal must point at where the password belongs")
	})

	t.Run("other schemes", func(t *testing.T) {
		for _, bad := range []string{"file:///etc/passwd", "ftp://awb.example", "awb.example"} {
			_, fault := validateAWBBaseURL(bad)
			assert.NotNil(t, fault, "%q must be refused", bad)
		}
	})
}

// --- the project gate ---

func awbTestSession(projects ...string) *awbProxySession {
	return &awbProxySession{
		policy:   config.AWBProxyConfig{AllowedProjects: projects},
		base:     "https://awb.example",
		projects: projects,
	}
}

// TestEnforceIssueProject is the SECOND half of the gate: the one that checks
// what was actually reached rather than what was asked for.
func TestEnforceIssueProject(t *testing.T) {
	s := awbTestSession("awb")

	assert.Nil(t, s.enforceIssueProject(&awbIssue{ID: "awb-1", Project: "awb"}))

	fault := s.enforceIssueProject(&awbIssue{ID: "secret-1", Project: "secret"})
	require.NotNil(t, fault)
	assert.Equal(t, http.StatusForbidden, fault.Status)

	fault = s.enforceIssueProject(&awbIssue{ID: "awb-1"})
	require.NotNil(t, fault, "an issue carrying no project cannot be gated, so it must not pass")
	assert.Equal(t, "project_unresolved", fault.Code)

	fault = s.enforceIssueProject(nil)
	require.NotNil(t, fault)
	assert.Equal(t, http.StatusNotFound, fault.Status)
}

// TestPruneTree covers the one read that can reach outside the gate on its own:
// AWB follows children across project boundaries by design.
func TestPruneTree(t *testing.T) {
	s := awbTestSession("awb")
	tree := &awbIssueTree{
		awbIssue: awbIssue{ID: "awb-root", Project: "awb"},
		Children: []awbIssueTree{
			{awbIssue: awbIssue{ID: "awb-a", Project: "awb"}},
			{
				awbIssue: awbIssue{ID: "secret-b", Project: "secret", Description: "confidential"},
				Children: []awbIssueTree{
					{awbIssue: awbIssue{ID: "secret-c", Project: "secret"}},
					// Reachable, but only through an unreachable parent. It goes
					// too: the path to it is what a caller may not see.
					{awbIssue: awbIssue{ID: "awb-d", Project: "awb"}},
				},
			},
		},
	}

	kept, pruned := s.pruneTree(tree)
	require.NotNil(t, kept)
	assert.Equal(t, 3, pruned, "the out-of-scope node and its whole subtree")
	require.Len(t, kept.Children, 1)
	assert.Equal(t, "awb-a", kept.Children[0].ID)

	t.Run("an out-of-scope root leaves nothing", func(t *testing.T) {
		kept, pruned := s.pruneTree(&awbIssueTree{
			awbIssue: awbIssue{ID: "secret-1", Project: "secret"}})
		assert.Nil(t, kept)
		assert.Equal(t, 1, pruned)
	})
}

// TestEnforceIssueList drops rather than refuses, so one unexpected row cannot
// deny the agent the rest of a legitimate listing.
func TestEnforceIssueList(t *testing.T) {
	s := awbTestSession("awb", "web")
	kept := s.enforceIssueList([]awbIssue{
		{ID: "awb-1", Project: "awb"},
		{ID: "secret-1", Project: "secret"},
		{ID: "web-1", Project: "web"},
		{ID: "orphan-1"},
	})
	require.Len(t, kept, 2)
	assert.Equal(t, "awb-1", kept[0].ID)
	assert.Equal(t, "web-1", kept[1].ID)

	t.Run("an empty result is a slice, never nil", func(t *testing.T) {
		assert.NotNil(t, s.enforceIssueList(nil),
			"a nil would render as JSON null and make 'did I get rows' depend on which empty it was")
	})
}

// TestRequireAllowedProjectNamesTheRightList is what lets an agent tell its
// human which of the two lists to widen instead of guessing from a 403.
func TestRequireAllowedProjectNamesTheRightList(t *testing.T) {
	t.Run("the operator's list excluded it", func(t *testing.T) {
		s := awbTestSession("awb")
		fault := s.requireAllowedProject("secret")
		require.NotNil(t, fault)
		assert.Equal(t, "project_not_allowed", fault.Code)
		assert.Contains(t, fault.Msg, "allowed_projects")
	})

	t.Run("the caller's own grant scope excluded it", func(t *testing.T) {
		s := &awbProxySession{
			// The operator allows both; this caller's grant reaches only one.
			policy:        config.AWBProxyConfig{AllowedProjects: []string{"awb", "web"}},
			projects:      []string{"awb"},
			grantProjects: []string{"awb"},
		}
		fault := s.requireAllowedProject("web")
		require.NotNil(t, fault)
		assert.Equal(t, awbProjectOutOfScopeCode, fault.Code)
		assert.Contains(t, fault.Msg, "this grant covers")
	})
}

// --- validation ---

func TestValidateAWBAttachmentName(t *testing.T) {
	name, fault := validateAWBAttachmentName("notes.md")
	require.Nil(t, fault)
	assert.Equal(t, "notes.md", name)

	for _, bad := range []string{"", ".", "..", "a/b", `a\b`, "a\x00b", "a\nb",
		strings.Repeat("x", 256)} {
		_, fault := validateAWBAttachmentName(bad)
		assert.NotNil(t, fault, "%q is not an attachment name", bad)
	}
}

func TestValidateAWBTitle(t *testing.T) {
	title, fault := validateAWBTitle("  Parser crashes on empty input  ")
	require.Nil(t, fault)
	assert.Equal(t, "Parser crashes on empty input", title)

	_, fault = validateAWBTitle("")
	assert.NotNil(t, fault)

	_, fault = validateAWBTitle("looks\u202egnip.exe")
	require.NotNil(t, fault)
	assert.Contains(t, fault.Msg, "U+202E",
		"a bidi override reorders how a title renders without changing what it says")

	_, fault = validateAWBTitle("line\nbreak")
	assert.NotNil(t, fault)

	_, fault = validateAWBTitle(strings.Repeat("x", maxAWBTitleLen+1))
	assert.NotNil(t, fault)
}

func TestValidateAWBNameTokens(t *testing.T) {
	for _, ok := range []string{"parser", "team/backend", "a.b", "a_b", "a-b", "claude-1"} {
		_, fault := validateAWBLabel(ok)
		assert.Nil(t, fault, "%q is a valid label", ok)
	}
	for _, bad := range []string{"", "Parser", "a b", "a,b", "a#b", strings.Repeat("x", 65)} {
		_, fault := validateAWBLabel(bad)
		assert.NotNil(t, fault, "%q must be refused rather than normalised", bad)
	}
}

func TestValidateAWBVocabularies(t *testing.T) {
	t.Run("type", func(t *testing.T) {
		got, fault := validateAWBType("BUG")
		require.Nil(t, fault)
		assert.Equal(t, "bug", got)
		_, fault = validateAWBType("epicish")
		require.NotNil(t, fault)
		assert.Contains(t, fault.Msg, "chore", "the refusal lists the whole vocabulary")
	})

	t.Run("relation", func(t *testing.T) {
		got, fault := validateAWBRelationType("blocked-by")
		require.Nil(t, fault)
		assert.Equal(t, "blocked-by", got)
		_, fault = validateAWBRelationType("blocks")
		assert.NotNil(t, fault)
	})

	t.Run("sort, and relevance only where it applies", func(t *testing.T) {
		_, fault := validateAWBSort("relevance", false)
		assert.NotNil(t, fault, "only search can order by relevance")
		got, fault := validateAWBSort("relevance", true)
		require.Nil(t, fault)
		assert.Equal(t, "relevance", got)
		got, fault = validateAWBSort("", false)
		require.Nil(t, fault)
		assert.Empty(t, got)
	})

	t.Run("priority", func(t *testing.T) {
		assert.Nil(t, validateAWBPriority(0))
		assert.Nil(t, validateAWBPriority(4))
		assert.NotNil(t, validateAWBPriority(5))
		assert.NotNil(t, validateAWBPriority(-1))
	})
}

// TestValidateAWBLimit pins the one place the proxy deliberately differs from
// awb: awb returns every row by default, and the proxy bounds it.
func TestValidateAWBLimit(t *testing.T) {
	limit, fault := validateAWBLimit(0)
	require.Nil(t, fault)
	assert.Equal(t, defaultAWBLimit, limit, "an omitted limit must be bounded, not unbounded")

	limit, fault = validateAWBLimit(7)
	require.Nil(t, fault)
	assert.Equal(t, 7, limit)

	_, fault = validateAWBLimit(maxAWBLimit + 1)
	assert.NotNil(t, fault)
	_, fault = validateAWBLimit(-1)
	assert.NotNil(t, fault)
}

func TestValidateAWBSearchTerms(t *testing.T) {
	terms, fault := validateAWBSearchTerms([]string{" parser ", "", "crash"})
	require.Nil(t, fault)
	assert.Equal(t, []string{"parser", "crash"}, terms)

	_, fault = validateAWBSearchTerms([]string{"  "})
	assert.NotNil(t, fault, "a term list that reduces to nothing is not a search")

	many := make([]string, maxAWBSearchTerms+1)
	for i := range many {
		many[i] = "x"
	}
	_, fault = validateAWBSearchTerms(many)
	assert.NotNil(t, fault)
}

// --- the operator's credential ---

func TestResolveAWBPassword(t *testing.T) {
	t.Run("a server with no users needs none", func(t *testing.T) {
		s := &awbProxySession{policy: config.AWBProxyConfig{}}
		user, password, fault := s.credentials()
		require.Nil(t, fault)
		assert.Empty(t, user)
		assert.Empty(t, password)
	})

	t.Run("a username with no password is half a credential", func(t *testing.T) {
		t.Setenv("AWB_PASSWORD", "")
		s := &awbProxySession{policy: config.AWBProxyConfig{Username: "bot"}}
		_, _, fault := s.credentials()
		require.NotNil(t, fault)
		assert.Equal(t, "password_missing", fault.Code)
	})

	t.Run("the environment fallback", func(t *testing.T) {
		t.Setenv("AWB_PASSWORD", "hunter2")
		s := &awbProxySession{policy: config.AWBProxyConfig{Username: "bot"}}
		user, password, fault := s.credentials()
		require.Nil(t, fault)
		assert.Equal(t, "bot", user)
		assert.Equal(t, "hunter2", password)
	})

	t.Run("a file wins, and its trailing newline is not part of the password", func(t *testing.T) {
		t.Setenv("AWB_PASSWORD", "from-env")
		path := t.TempDir() + "/password"
		require.NoError(t, os.WriteFile(path, []byte("from-file\n"), 0o600))
		s := &awbProxySession{policy: config.AWBProxyConfig{Username: "bot", PasswordFile: path}}
		_, password, fault := s.credentials()
		require.Nil(t, fault)
		assert.Equal(t, "from-file", password)
	})

	t.Run("an unreadable file is a refusal, not a fallback", func(t *testing.T) {
		t.Setenv("AWB_PASSWORD", "from-env")
		s := &awbProxySession{
			policy: config.AWBProxyConfig{Username: "bot", PasswordFile: t.TempDir() + "/absent"}}
		_, _, fault := s.credentials()
		require.NotNil(t, fault)
		assert.Equal(t, "password_unreadable", fault.Code,
			"falling back to the environment would authenticate as something the operator did not ask for")
	})
}

// --- AWB's own failures ---

// TestAWBErrorFault keeps AWB's status classification, which is the whole point
// of a tracker that documents each status as a category.
func TestAWBErrorFault(t *testing.T) {
	for _, tc := range []struct {
		status   int
		wantCode string
		wantHTTP int
	}{
		{http.StatusBadRequest, "invalid_arg", http.StatusBadRequest},
		{http.StatusUnauthorized, "awb_auth", http.StatusServiceUnavailable},
		{http.StatusForbidden, "awb_forbidden", http.StatusForbidden},
		{http.StatusNotFound, "not_found", http.StatusNotFound},
		{http.StatusConflict, "awb_conflict", http.StatusConflict},
		{http.StatusInternalServerError, "awb_failed", http.StatusBadGateway},
		{http.StatusUnsupportedMediaType, "awb_schema_drift", http.StatusBadGateway},
	} {
		fault := awbErrorFault(awbHTTPResult{
			Status: tc.status, Body: []byte(`{"error":"no such issue"}`)})
		require.NotNil(t, fault)
		assert.Equal(t, tc.wantCode, fault.Code, "HTTP %d", tc.status)
		assert.Equal(t, tc.wantHTTP, fault.Status, "HTTP %d", tc.status)
		assert.Contains(t, fault.Msg, "no such issue",
			"AWB's own message is the useful half and must survive")
	}

	t.Run("a body that is not AWB's error shape still says something", func(t *testing.T) {
		fault := awbErrorFault(awbHTTPResult{Status: 502, Body: []byte("<html>bad gateway</html>")})
		require.NotNil(t, fault)
		assert.Contains(t, fault.Msg, "502")
	})
}

// --- the compact renderer ---

// TestAWBCompactLine pins every field and every optional token of a format awb
// owns. This is a second copy of that format, so it is worth pinning to the
// specification rather than to whatever the code currently does.
func TestAWBCompactLine(t *testing.T) {
	issue := &awbIssue{
		ID: "awb-5c1d84", Priority: 1, Status: "in_progress", Type: "bug",
		Title: "Tokeniser drops the trailing newline",
	}
	assert.Equal(t,
		`awb-5c1d84 P1 in_progress bug "Tokeniser drops the trailing newline"`,
		awbCompactLine(issue, false),
		"the five mandatory positional fields, the title as a JSON string")

	issue.Assignee = "claude-1"
	issue.Labels = []string{"tokeniser", "parser"}
	issue.Blocked = true
	issue.Blockers = []string{"awb-000001", "awb-000002"}
	assert.Equal(t,
		`awb-5c1d84 P1 in_progress bug "Tokeniser drops the trailing newline" `+
			`@claude-1 #tokeniser #parser !blocked`,
		awbCompactLine(issue, false),
		"optional tokens in their fixed order, and no blockers unless asked for")

	assert.Equal(t,
		`awb-5c1d84 P1 in_progress bug "Tokeniser drops the trailing newline" `+
			`@claude-1 #tokeniser #parser !blocked blocked-by:awb-000001 blocked-by:awb-000002`,
		awbCompactLine(issue, true),
		"blocked's own listing carries the blockers, which are its point")

	t.Run("the title's quoting is awb's, HTML escaping off", func(t *testing.T) {
		assert.Equal(t,
			`awb-1 P2 open task "<b> & \"quoted\""`,
			awbCompactLine(&awbIssue{
				ID: "awb-1", Priority: 2, Status: "open", Type: "task",
				Title: `<b> & "quoted"`,
			}, false),
			"angle brackets and ampersands stay literal, so the line is cheap to read")
	})
}

func TestAWBCompactTree(t *testing.T) {
	tree := &awbIssueTree{
		awbIssue: awbIssue{ID: "awb-root", Priority: 0, Status: "open", Type: "epic", Title: "Root"},
		Children: []awbIssueTree{{
			awbIssue: awbIssue{ID: "awb-kid", Priority: 2, Status: "open", Type: "task", Title: "Kid"},
			Children: []awbIssueTree{{
				awbIssue: awbIssue{
					ID: "awb-gk", Priority: 3, Status: "closed", Type: "chore", Title: "Grandkid"},
			}},
		}},
	}
	var b strings.Builder
	awbCompactTree(tree, 0, &b)
	assert.Equal(t,
		"awb-root P0 open epic \"Root\"\n"+
			"  awb-kid P2 open task \"Kid\"\n"+
			"    awb-gk P3 closed chore \"Grandkid\"\n",
		b.String(),
		"two spaces per level of depth, the root unindented")
}

func TestAWBCompactAttachmentLine(t *testing.T) {
	assert.Equal(t,
		`awb-5c1d84 12345 9f86d0 "text/markdown; charset=utf-8" "notes.md"`,
		awbCompactAttachmentLine(&awbAttachment{
			Issue: "awb-5c1d84", Size: 12345, SHA256: "9f86d0",
			ContentType: "text/markdown; charset=utf-8", Name: "notes.md",
		}),
		"the content type is quoted because it may carry a parameter with a space in it")
}
