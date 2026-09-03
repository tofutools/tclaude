package agentd

import (
	"encoding/json"
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
// be, how the workspace gate prunes a tree, and the compact renderer — the last
// of which is a second copy of a format awb owns, so it is pinned field by
// field.

// --- issue references ---

// TestValidateAWBIssueRef is the pre-call half of the workspace gate, and the
// reason it can be a pre-call half at all: a reference that carries no workspace
// key could only be judged after fetching the issue.
func TestValidateAWBIssueRef(t *testing.T) {
	t.Run("a full id", func(t *testing.T) {
		ref, fault := validateAWBIssueRef("awb-a3f9c1")
		require.Nil(t, fault)
		assert.Equal(t, "awb-a3f9c1", ref)
		assert.Equal(t, "awb", workspaceKeyOf(ref))
	})

	t.Run("a hash PREFIX still carries the workspace", func(t *testing.T) {
		ref, fault := validateAWBIssueRef("awb-a3f")
		require.Nil(t, fault)
		assert.Equal(t, "awb-a3f", ref)
		assert.Equal(t, "awb", workspaceKeyOf(ref))
	})

	t.Run("capitals resolve, as they do in awb", func(t *testing.T) {
		ref, fault := validateAWBIssueRef("AWB-A3F9C1")
		require.Nil(t, fault)
		assert.Equal(t, "awb-a3f9c1", ref)
	})

	t.Run("a workspace key may contain hyphens, so the split is on the LAST one", func(t *testing.T) {
		ref, fault := validateAWBIssueRef("my-proj-a3f9c1")
		require.Nil(t, fault)
		assert.Equal(t, "my-proj", workspaceKeyOf(ref))
	})

	t.Run("a BARE hash is refused", func(t *testing.T) {
		_, fault := validateAWBIssueRef("a3f9c1")
		require.NotNil(t, fault)
		assert.Equal(t, http.StatusBadRequest, fault.Status)
		assert.Contains(t, fault.Msg, "names no workspace",
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

// TestAWBWorkspaceKeyShape pins the rule the permission-scope parser borrows, so
// a matcher an operator can write is always a key a request could also carry.
func TestAWBWorkspaceKeyShape(t *testing.T) {
	for _, ok := range []string{"awb", "a", "my-proj", "web2", "a-1-b"} {
		assert.NoError(t, awbWorkspaceKeyShapeErr(ok), "%q is a valid AWB workspace key", ok)
	}
	for _, bad := range []string{"", "  ", "1web", "-web", "WEB", "we b", "web_x", "web/x", "*",
		"averyveryverylongkey"} {
		assert.Error(t, awbWorkspaceKeyShapeErr(bad), "%q must be refused", bad)
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

// --- the workspace gate ---

func awbTestSession(workspaces ...string) *awbProxySession {
	return &awbProxySession{
		policy:     config.AWBProxyConfig{AllowedWorkspaces: workspaces},
		base:       "https://awb.example",
		workspaces: workspaces,
	}
}

// TestEnforceIssueWorkspace is the SECOND half of the gate: the one that checks
// what was actually reached rather than what was asked for.
func TestEnforceIssueWorkspace(t *testing.T) {
	s := awbTestSession("awb")

	assert.Nil(t, s.enforceIssueWorkspace(&awbIssue{ID: "awb-1", Workspace: "awb"}))

	fault := s.enforceIssueWorkspace(&awbIssue{ID: "secret-1", Workspace: "secret"})
	require.NotNil(t, fault)
	assert.Equal(t, http.StatusForbidden, fault.Status)

	fault = s.enforceIssueWorkspace(&awbIssue{ID: "awb-1"})
	require.NotNil(t, fault, "an issue carrying no workspace cannot be gated, so it must not pass")
	assert.Equal(t, "workspace_unresolved", fault.Code)

	fault = s.enforceIssueWorkspace(nil)
	require.NotNil(t, fault)
	assert.Equal(t, http.StatusNotFound, fault.Status)
}

// TestPruneTree covers the one read that can reach outside the gate on its own:
// AWB follows children across workspace boundaries by design.
func TestPruneTree(t *testing.T) {
	s := awbTestSession("awb")
	tree := &awbIssueTree{
		awbIssue: awbIssue{ID: "awb-root", Workspace: "awb"},
		Children: []awbIssueTree{
			{awbIssue: awbIssue{ID: "awb-a", Workspace: "awb"}},
			{
				awbIssue: awbIssue{ID: "secret-b", Workspace: "secret", Description: "confidential"},
				Children: []awbIssueTree{
					{awbIssue: awbIssue{ID: "secret-c", Workspace: "secret"}},
					// Reachable, but only through an unreachable parent. It goes
					// too: the path to it is what a caller may not see.
					{awbIssue: awbIssue{ID: "awb-d", Workspace: "awb"}},
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
			awbIssue: awbIssue{ID: "secret-1", Workspace: "secret"}})
		assert.Nil(t, kept)
		assert.Equal(t, 1, pruned)
	})
}

// TestEnforceIssueList drops rather than refuses, so one unexpected row cannot
// deny the agent the rest of a legitimate listing.
func TestEnforceIssueList(t *testing.T) {
	s := awbTestSession("awb", "web")
	kept := s.enforceIssueList([]awbIssue{
		{ID: "awb-1", Workspace: "awb"},
		{ID: "secret-1", Workspace: "secret"},
		{ID: "web-1", Workspace: "web"},
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

// TestRequireAllowedWorkspaceNamesTheRightList is what lets an agent tell its
// human which of the two lists to widen instead of guessing from a 403.
func TestRequireAllowedWorkspaceNamesTheRightList(t *testing.T) {
	t.Run("the operator's list excluded it", func(t *testing.T) {
		s := awbTestSession("awb")
		fault := s.requireAllowedWorkspace("secret")
		require.NotNil(t, fault)
		assert.Equal(t, "workspace_not_allowed", fault.Code)
		assert.Contains(t, fault.Msg, "allowed_workspaces")
	})

	t.Run("the caller's own grant scope excluded it", func(t *testing.T) {
		s := &awbProxySession{
			// The operator allows both; this caller's grant reaches only one.
			policy:          config.AWBProxyConfig{AllowedWorkspaces: []string{"awb", "web"}},
			workspaces:      []string{"awb"},
			grantWorkspaces: []string{"awb"},
		}
		fault := s.requireAllowedWorkspace("web")
		require.NotNil(t, fault)
		assert.Equal(t, awbWorkspaceOutOfScopeCode, fault.Code)
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

func TestValidateAWBImplementationFields(t *testing.T) {
	for _, ok := range []string{"", "01234567", strings.Repeat("A", maxAWBCommitHashLen)} {
		got, fault := validateAWBCommitHash(ok)
		assert.Nil(t, fault, ok)
		assert.Equal(t, ok, got, "commit hash case is preserved")
	}
	for _, bad := range []string{"1234567", strings.Repeat("a", maxAWBCommitHashLen+1), "not-a-hash"} {
		_, fault := validateAWBCommitHash(bad)
		assert.NotNil(t, fault, bad)
	}

	for _, ok := range []string{"", "http://example.com/pull/1", "https://user:token@example.com/pull/1"} {
		got, fault := validateAWBPullRequestURL(ok)
		assert.Nil(t, fault, ok)
		assert.Equal(t, ok, got)
	}
	for _, bad := range []string{"ssh://example.com/1", "https://example.com/pull/ 1", "https://"} {
		_, fault := validateAWBPullRequestURL(bad)
		assert.NotNil(t, fault, bad)
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

	issue.Assignees = []string{"claude-1", "claude-2"}
	issue.Labels = []string{"tokeniser", "parser"}
	issue.Blocked = true
	issue.Blockers = []string{"awb-000001", "awb-000002"}
	assert.Equal(t,
		`awb-5c1d84 P1 in_progress bug "Tokeniser drops the trailing newline" `+
			`@claude-1 @claude-2 #tokeniser #parser !blocked`,
		awbCompactLine(issue, false),
		"optional tokens in their fixed order, and no blockers unless asked for")

	assert.Equal(t,
		`awb-5c1d84 P1 in_progress bug "Tokeniser drops the trailing newline" `+
			`@claude-1 @claude-2 #tokeniser #parser !blocked blocked-by:awb-000001 blocked-by:awb-000002`,
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

// TestAWBCompactActivityLine pins the timeline format, which is the one an
// agent reads a discussion out of. The body is a JSON string so that a comment
// containing line breaks still occupies exactly one line — the property that
// makes a timeline splittable on newlines.
func TestAWBCompactActivityLine(t *testing.T) {
	t.Run("an ordinary comment carries no action", func(t *testing.T) {
		assert.Equal(t,
			`42 2026-08-26T09:12:03.412Z comment @claude-1 "Reproduced with an empty token stream."`,
			awbCompactActivityLine(&awbActivity{
				ID: 42, CreatedAt: "2026-08-26T09:12:03.412Z", Kind: "comment",
				Actor: "claude-1", Body: "Reproduced with an empty token stream.",
			}))
	})

	t.Run("a close reason is a comment whose action is closed", func(t *testing.T) {
		assert.Equal(t,
			`43 2026-08-26T09:13:00.000Z comment @claude-1 closed "Guard against empty token stream"`,
			awbCompactActivityLine(&awbActivity{
				ID: 43, CreatedAt: "2026-08-26T09:13:00.000Z", Kind: "comment",
				Actor: "claude-1", Action: "closed", Body: "Guard against empty token stream",
			}))
	})

	t.Run("a change carries its action bare, and its field changes as JSON", func(t *testing.T) {
		assert.Equal(t,
			`44 2026-08-26T09:14:00.000Z change @claude-1 closed `+
				`[{"field":"status","from":"open","to":"closed"}]`,
			awbCompactActivityLine(&awbActivity{
				ID: 44, CreatedAt: "2026-08-26T09:14:00.000Z", Kind: "change",
				Actor: "claude-1", Action: "closed",
				Changes: []awbActivityChange{{
					Field: "status",
					From:  json.RawMessage(`"open"`),
					To:    json.RawMessage(`"closed"`),
				}},
			}))
	})

	t.Run("an unknown actor is simply absent", func(t *testing.T) {
		assert.Equal(t,
			`45 2026-08-26T09:15:00.000Z comment "no identity was configured"`,
			awbCompactActivityLine(&awbActivity{
				ID: 45, CreatedAt: "2026-08-26T09:15:00.000Z", Kind: "comment",
				Body: "no identity was configured",
			}))
	})

	t.Run("a multi-line body stays on ONE line", func(t *testing.T) {
		line := awbCompactActivityLine(&awbActivity{
			ID: 46, CreatedAt: "2026-08-26T09:16:00.000Z", Kind: "comment",
			Body: "first\nsecond\tthird",
		})
		assert.NotContains(t, line, "\n", "a timeline must stay splittable on newlines")
		assert.Contains(t, line, `"first\nsecond\tthird"`)
	})
}

// TestValidateAWBComment covers the one rule a comment has that a description
// does not: it may not be blank.
func TestValidateAWBComment(t *testing.T) {
	assert.Nil(t, validateAWBComment("Reproduced with an empty token stream."))
	assert.Nil(t, validateAWBComment("line one\nline two\ttabbed\r\n"),
		"the whitespace controls Markdown needs are allowed")

	for _, blank := range []string{"", "   ", "\n\t "} {
		fault := validateAWBComment(blank)
		require.NotNil(t, fault, "%q is not a comment", blank)
		assert.Equal(t, http.StatusBadRequest, fault.Status)
	}

	fault := validateAWBComment("bell\x07here")
	require.NotNil(t, fault)
	assert.Contains(t, fault.Msg, "control character")

	fault = validateAWBComment(strings.Repeat("x", maxAWBCommentBytes+1))
	require.NotNil(t, fault)
	assert.Contains(t, fault.Msg, "maximum")
}

func TestValidateAWBOffset(t *testing.T) {
	got, fault := validateAWBOffset(0)
	require.Nil(t, fault)
	assert.Equal(t, 0, got)

	got, fault = validateAWBOffset(25)
	require.Nil(t, fault)
	assert.Equal(t, 25, got)

	_, fault = validateAWBOffset(-1)
	assert.NotNil(t, fault)
	_, fault = validateAWBOffset(maxAWBOffset + 1)
	assert.NotNil(t, fault)
}

// TestAWBIssueCarriesNoCloseReason pins a REMOVAL.
//
// AWB 0.6 took close_reason off the issue entirely — a close reason is now a
// typed comment. Keeping the field would have reported `"close_reason": ""` on
// every issue, which reads as "no reason recorded" for a concept the tracker no
// longer has, and would go on doing so silently.
func TestAWBIssueCarriesNoCloseReason(t *testing.T) {
	encoded, err := json.Marshal(&awbIssue{ID: "awb-1", Workspace: "awb"})
	require.NoError(t, err)
	assert.NotContains(t, string(encoded), "close_reason",
		"close_reason is not a field AWB has any more")
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

// TestVisiblePermissionRegistryGatesEachProxyFamily pins what the slug catalog
// advertises.
//
// The catalog exists so an agent does not mistake an unconfigured optional
// feature for missing authority. One OR-combined flag defeated that: a git-only
// host advertised proxy.awb.*, and an AWB-only host advertised the git family.
// Hiding a slug never withdraws a grant made under it — the full registry still
// backs validation and stored-grant resolution — so this is about advertising,
// not enforcement.
func TestVisiblePermissionRegistryGatesEachProxyFamily(t *testing.T) {
	slugs := func(vis proxyVisibility) map[string]bool {
		out := map[string]bool{}
		for _, p := range visiblePermissionRegistry(vis) {
			out[p.Slug] = true
		}
		return out
	}

	t.Run("git only", func(t *testing.T) {
		got := slugs(proxyVisibility{git: true})
		assert.True(t, got[PermGitRead])
		assert.True(t, got[PermGitHubWrite])
		assert.False(t, got[PermLinearRead],
			"a git-only host has no Linear key, so advertising the slug sends an agent after a "+
				"tracker that is not there")
		assert.False(t, got[PermLinearWrite])
		assert.False(t, got[PermAWBRead], "a git-only host has no AWB server to reach")
		assert.False(t, got[PermAWBWrite])
	})

	// A Linear-only host: the case that used to hide both Linear slugs, leaving
	// an operator unable to grant a capability their host actually had.
	t.Run("linear only", func(t *testing.T) {
		got := slugs(proxyVisibility{linear: true})
		assert.True(t, got[PermLinearRead],
			"a slug missing from the catalog is one nobody can grant")
		assert.True(t, got[PermLinearWrite])
		assert.False(t, got[PermGitRead])
		assert.False(t, got[PermAWBRead])
	})

	t.Run("every family at once", func(t *testing.T) {
		got := slugs(proxyVisibility{git: true, linear: true, awb: true})
		for _, slug := range []string{
			PermGitRead, PermGitPush, PermGitHubRead, PermGitHubWrite, PermGitHubMerge,
			PermLinearRead, PermLinearWrite, PermAWBRead, PermAWBWrite,
		} {
			assert.True(t, got[slug], "%s", slug)
		}
	})

	t.Run("awb only", func(t *testing.T) {
		got := slugs(proxyVisibility{awb: true})
		assert.True(t, got[PermAWBRead])
		assert.True(t, got[PermAWBWrite])
		assert.False(t, got[PermGitRead], "an AWB-only host has no git proxy configured")
		assert.False(t, got[PermGitHubMerge])
		assert.False(t, got[PermLinearRead])
	})

	t.Run("neither", func(t *testing.T) {
		got := slugs(proxyVisibility{})
		for _, slug := range []string{
			PermGitRead, PermGitPush, PermGitHubRead, PermGitHubWrite, PermGitHubMerge,
			PermLinearRead, PermLinearWrite, PermAWBRead, PermAWBWrite,
		} {
			assert.False(t, got[slug], "%s must not be advertised with no proxy configured", slug)
		}
		assert.NotEmpty(t, got, "every non-proxy slug is still listed")
	})

	t.Run("a non-proxy slug is never filtered", func(t *testing.T) {
		for _, vis := range []proxyVisibility{{}, {git: true}, {awb: true}, {git: true, awb: true}} {
			assert.True(t, slugs(vis)[PermPermissionsGrant])
		}
	})

	t.Run("every semantic-proxy slug is answered by exactly one family", func(t *testing.T) {
		// The guard that keeps a slug added later from defaulting to visible:
		// with both families off, no semantic-proxy slug may appear.
		got := slugs(proxyVisibility{})
		for _, p := range permissionRegistry {
			if isSemanticProxyPermission(p.Slug) {
				assert.False(t, got[p.Slug],
					"%s is a semantic-proxy slug but is not gated by any family", p.Slug)
			}
		}
	})
}

func TestValidateAWBActivityKind(t *testing.T) {
	got, fault := validateAWBActivityKind("")
	require.Nil(t, fault)
	assert.Empty(t, got, "no kind means the whole timeline")

	got, fault = validateAWBActivityKind("  COMMENT ")
	require.Nil(t, fault)
	assert.Equal(t, "comment", got)

	got, fault = validateAWBActivityKind("change")
	require.Nil(t, fault)
	assert.Equal(t, "change", got)

	_, fault = validateAWBActivityKind("changes")
	require.NotNil(t, fault)
	assert.Contains(t, fault.Msg, "comment, change", "the refusal lists the whole vocabulary")
}

// TestEveryAdvertisedFamilyAlsoRegistersTheCommand is the coherence rule
// between the two halves of "is this proxy available here".
//
// The permission catalog and the command tree answer for different surfaces —
// what an operator can grant, and what an agent can run — but they must not
// disagree. A host that advertises a family's slugs while `tclaude proxy`
// itself is unregistered lets an operator grant a capability with no way to
// exercise it; the reverse hides a command whose slugs are grantable. Both
// halves read proxyVisibility, so the rule holds by construction and this
// pins it.
func TestEveryAdvertisedFamilyAlsoRegistersTheCommand(t *testing.T) {
	for _, vis := range []proxyVisibility{
		{git: true}, {linear: true}, {awb: true},
		{git: true, linear: true}, {linear: true, awb: true},
		{git: true, linear: true, awb: true},
	} {
		advertised := false
		for _, p := range visiblePermissionRegistry(vis) {
			if isSemanticProxyPermission(p.Slug) {
				advertised = true
				break
			}
		}
		registered := vis.git || vis.linear || vis.awb
		assert.Equal(t, registered, advertised,
			"%+v: the catalog and the command tree must agree", vis)
	}

	t.Run("and nothing configured advertises nothing", func(t *testing.T) {
		for _, p := range visiblePermissionRegistry(proxyVisibility{}) {
			assert.False(t, isSemanticProxyPermission(p.Slug), "%s", p.Slug)
		}
	})
}
