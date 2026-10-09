package agentd

import (
	"encoding/json"
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// gitlabproxy_test.go covers the pieces of the GitLab proxy that are pure
// functions: URL construction against a pinned base, the draft-title marker
// GitLab uses in place of a flag, the project-path gate, and the error
// document shapes.

func TestGLRequestURL(t *testing.T) {
	base := "https://gitlab.example.com:6080"

	t.Run("relative paths join under the API prefix, port kept", func(t *testing.T) {
		got, err := glRequestURL(base, glAPIRequest{Path: "projects/a%2Fb/merge_requests", Query: url.Values{"state": {"opened"}}})
		require.NoError(t, err)
		assert.Equal(t, "https://gitlab.example.com:6080/api/v4/projects/a%2Fb/merge_requests?state=opened", got)
	})
	t.Run("a pagination link on the same instance is followed as given", func(t *testing.T) {
		got, err := glRequestURL(base, glAPIRequest{Path: "https://gitlab.example.com:6080/api/v4/projects/1/notes?page=2&per_page=100"})
		require.NoError(t, err)
		assert.Equal(t, "https://gitlab.example.com:6080/api/v4/projects/1/notes?page=2&per_page=100", got)
	})
	t.Run("a link to another host is refused", func(t *testing.T) {
		_, err := glRequestURL(base, glAPIRequest{Path: "https://gitlab.com/api/v4/projects/1/notes?page=2"})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "outside gitlab.example.com:6080")
	})
	t.Run("the same host on another port is another instance", func(t *testing.T) {
		_, err := glRequestURL(base, glAPIRequest{Path: "https://gitlab.example.com/api/v4/projects/1"})
		require.Error(t, err)
	})
	t.Run("an http base is refused", func(t *testing.T) {
		_, err := glRequestURL("http://gitlab.example.com", glAPIRequest{Path: "projects"})
		require.Error(t, err)
	})
	t.Run("a caller query key replaces one the link carried", func(t *testing.T) {
		got, err := glRequestURL(base, glAPIRequest{
			Path:  "https://gitlab.example.com:6080/api/v4/projects/1/notes?page=2",
			Query: url.Values{"page": {"3"}},
		})
		require.NoError(t, err)
		assert.Equal(t, "https://gitlab.example.com:6080/api/v4/projects/1/notes?page=3", got)
	})
}

func TestGLStripDraftPrefix(t *testing.T) {
	cases := map[string]string{
		"Draft: Add a thing":         "Add a thing",
		"draft:Add a thing":          "Add a thing",
		"[Draft] Add a thing":        "Add a thing",
		"(Draft) Add a thing":        "Add a thing",
		"WIP: Add a thing":           "Add a thing",
		"Draft: [Draft] Add a thing": "Add a thing",
		"Add a thing":                "Add a thing",
		"Drafting the thing":         "Drafting the thing",
		"  Draft:   Add a thing   ":  "Add a thing",
		"Draft: ":                    "",
	}
	for in, want := range cases {
		assert.Equal(t, want, glStripDraftPrefix(in), "%q", in)
	}
	assert.Equal(t, "Draft: Add a thing", glDraftTitle("Draft: Add a thing"), "marking a draft twice adds one marker")
}

func TestIsGitLabPathSegment(t *testing.T) {
	for _, ok := range []string{"yamzz", "previewplayback-dashboard", "glfield_plugin", "a.b", "1337", "_x"} {
		assert.True(t, isGitLabPathSegment(ok), ok)
	}
	for _, bad := range []string{"", "-lead", ".lead", "sp ace", "a/b", "ümlaut", "a%2Fb", "x;y"} {
		assert.False(t, isGitLabPathSegment(bad), bad)
	}
}

func TestGLMessageText(t *testing.T) {
	cases := []struct {
		raw  string
		want string
	}{
		{`"404 Project Not Found"`, "404 Project Not Found"},
		{`["a", "b"]`, "a; b"},
		{`{"title":["can't be blank"],"base":["is invalid","is protected"]}`, "base: is invalid; is protected; title: can't be blank"},
		{`null`, ""},
		{``, ""},
	}
	for _, tc := range cases {
		assert.Equal(t, tc.want, glMessageText(json.RawMessage(tc.raw)), tc.raw)
	}
	assert.Equal(t, "405 Method Not Allowed", glErrorText(glAPIResult{Status: 405, Body: []byte(`{"message":"405 Method Not Allowed"}`)}))
	assert.Equal(t, "insufficient_scope", glErrorText(glAPIResult{Status: 403, Body: []byte(`{"error":"insufficient_scope"}`)}))
	assert.Contains(t, glErrorText(glAPIResult{Status: 404}), "Not Found")
	assert.Equal(t, "GitLab returned HTTP 500", glErrorText(glAPIResult{Status: 500, Body: []byte("<html>")}))
}

func TestGLMergePreflight(t *testing.T) {
	assert.Nil(t, glMergePreflight(&glMR{IID: 1, State: "opened", DetailedMergeStatus: "mergeable"}))
	assert.Nil(t, glMergePreflight(&glMR{IID: 1, State: "opened", DetailedMergeStatus: "ci_still_running"}),
		"a running pipeline is GitLab's to judge at merge time")

	merged := glMergePreflight(&glMR{IID: 1, State: "merged"})
	require.NotNil(t, merged)
	assert.Equal(t, 0, merged.ExitCode, "already merged is the state asked for")

	for name, mr := range map[string]*glMR{
		"closed":          {IID: 1, State: "closed"},
		"draft":           {IID: 1, State: "opened", Draft: true},
		"conflicts":       {IID: 1, State: "opened", HasConflicts: true},
		"conflict status": {IID: 1, State: "opened", DetailedMergeStatus: "conflict"},
	} {
		res := glMergePreflight(mr)
		require.NotNil(t, res, name)
		assert.Equal(t, 1, res.ExitCode, name)
		assert.Contains(t, res.Stderr, "!1", name)
	}
}
