package agentd

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// githubapi_test.go covers the pieces of the direct GitHub client that have no
// HTTP round trip in them — URL construction, error rendering, pagination
// parsing, credential discovery and archive extraction. Each is a place where
// a mistake is invisible from the outside until it matters.

// TestGHRequestURL_RefusesAnythingOffHost is the load-bearing one.
//
// A pagination `next` link is GITHUB-SUPPLIED DATA that this client follows
// with the operator's token attached. Go strips a sensitive header across a
// REDIRECT to another host, but a Link header is not a redirect — it is a URL
// handed straight to Do — so nothing in net/http would stop a token going
// somewhere it was never meant to.
func TestGHRequestURL_RefusesAnythingOffHost(t *testing.T) {
	t.Run("a relative path resolves under the API root", func(t *testing.T) {
		got, err := ghRequestURL(ghAPIRequest{Path: "repos/o/r/pulls"})
		require.NoError(t, err)
		assert.Equal(t, "https://api.github.com/repos/o/r/pulls", got)
	})

	t.Run("a leading slash is not a second root", func(t *testing.T) {
		got, err := ghRequestURL(ghAPIRequest{Path: "/repos/o/r"})
		require.NoError(t, err)
		assert.Equal(t, "https://api.github.com/repos/o/r", got)
	})

	t.Run("query values are escaped rather than concatenated", func(t *testing.T) {
		got, err := ghRequestURL(ghAPIRequest{
			Path:  "repos/o/r/actions/runs",
			Query: url.Values{"branch": []string{"feat/a b&c"}},
		})
		require.NoError(t, err)
		assert.Contains(t, got, "branch=feat%2Fa+b%26c")
	})

	t.Run("an absolute api.github.com link is followed", func(t *testing.T) {
		got, err := ghRequestURL(ghAPIRequest{
			Path: "https://api.github.com/repositories/1/issues/2/comments?page=3",
		})
		require.NoError(t, err)
		assert.Equal(t, "https://api.github.com/repositories/1/issues/2/comments?page=3", got)
	})

	for _, bad := range []string{
		"https://api.github.com.attacker.net/repos/o/r",
		"https://attacker.net/repos/o/r",
		"http://api.github.com/repos/o/r",
	} {
		t.Run("refused: "+bad, func(t *testing.T) {
			_, err := ghRequestURL(ghAPIRequest{Path: bad})
			require.Error(t, err)
			assert.Contains(t, err.Error(), "outside api.github.com")
		})
	}
}

// TestGHNextLink parses the pagination cursor GitHub actually sends. Parsing it
// rather than incrementing a page counter is what makes a walk terminate on
// GitHub's terms.
func TestGHNextLink(t *testing.T) {
	for _, tc := range []struct {
		name   string
		header string
		want   string
	}{
		{"next then last", `<https://api.github.com/x?page=2>; rel="next", <https://api.github.com/x?page=9>; rel="last"`,
			"https://api.github.com/x?page=2"},
		{"prev then next", `<https://api.github.com/x?page=1>; rel="prev", <https://api.github.com/x?page=3>; rel="next"`,
			"https://api.github.com/x?page=3"},
		{"the last page offers no next", `<https://api.github.com/x?page=1>; rel="first", <https://api.github.com/x?page=8>; rel="prev"`, ""},
		{"absent", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := http.Header{}
			if tc.header != "" {
				h.Set("Link", tc.header)
			}
			assert.Equal(t, tc.want, ghNextLink(h))
		})
	}
}

// TestGHTailText keeps the END, because that is where a failing step's error
// and a thread's newest comment are — and it starts at a line boundary, so a
// truncated log does not open on a fragment that reads like corruption.
func TestGHTailText(t *testing.T) {
	t.Run("under the bound is untouched", func(t *testing.T) {
		got, truncated := ghTailText("short\n", 100)
		assert.Equal(t, "short\n", got)
		assert.False(t, truncated)
	})

	// The slice is by bytes, and the line-boundary trim only repairs a split
	// rune when the tail happens to contain a newline. A long line of non-Latin
	// prose has neither.
	t.Run("a tail that splits a rune is still valid UTF-8", func(t *testing.T) {
		text := strings.Repeat("日", 100) // three bytes per rune, no newline
		got, truncated := ghTailText(text, 100)
		assert.True(t, truncated)
		assert.True(t, utf8.ValidString(got), "got %q", got)
	})

	t.Run("over the bound keeps the tail, aligned to a line", func(t *testing.T) {
		text := "aaaa\nbbbb\ncccc\ndddd\n"
		got, truncated := ghTailText(text, 12)
		assert.True(t, truncated)
		assert.Equal(t, "cccc\ndddd\n", got)
		assert.False(t, strings.HasPrefix(got, "b"), "a mid-line start reads as corrupted output")
	})
}

// TestGHErrorText renders GitHub's own words, which are the actionable part of
// every failure this proxy reports.
func TestGHErrorText(t *testing.T) {
	t.Run("a REST error carries its field detail", func(t *testing.T) {
		got := ghErrorText(ghAPIResult{Status: 422, Body: []byte(`{
			"message":"Validation Failed",
			"errors":[{"resource":"PullRequest","field":"base","code":"invalid",
			           "message":"No commits between main and feat/x"}]}`)})
		assert.Contains(t, got, "Validation Failed")
		assert.Contains(t, got, "No commits between main and feat/x")
	})

	t.Run("a field error with no message still says which field", func(t *testing.T) {
		got := ghErrorText(ghAPIResult{Status: 422, Body: []byte(
			`{"message":"Validation Failed","errors":[{"resource":"Issue","field":"title","code":"missing_field"}]}`)})
		assert.Contains(t, got, "Issue.title: missing_field")
	})

	t.Run("a GraphQL error document is recognised too", func(t *testing.T) {
		got := ghErrorText(ghAPIResult{Status: 200, Body: []byte(
			`{"errors":[{"type":"FORBIDDEN","message":"Resource not accessible"}]}`)})
		assert.Contains(t, got, "FORBIDDEN")
		assert.Contains(t, got, "Resource not accessible")
	})

	// A bare 404 is the single most common failure here — a private repository
	// the token cannot see looks exactly like one that does not exist — and an
	// empty body would otherwise produce an empty diagnosis.
	t.Run("an empty 404 says what it probably means", func(t *testing.T) {
		got := ghErrorText(ghAPIResult{Status: 404})
		assert.Contains(t, got, "not visible to the token")
	})

	t.Run("anything else names the status", func(t *testing.T) {
		assert.Contains(t, ghErrorText(ghAPIResult{Status: 502, Body: []byte("<html>")}), "HTTP 502")
	})
}

// TestValidateGHTokenShape — a stray newline in a token file is an ordinary
// editor accident, and net/http rejects the resulting header with a message
// that names the header rather than the cause.
func TestValidateGHTokenShape(t *testing.T) {
	assert.Nil(t, validateGHTokenShape("ghp_ordinary_token", "test"))

	for _, bad := range []string{"ghp_a\nghp_b", "ghp_a\ttab", "ghp_a\rcr"} {
		fault := validateGHTokenShape(bad, "test")
		require.NotNil(t, fault, "token %q must be refused", bad)
		assert.Contains(t, fault.Msg, "control character")
	}
}

// TestGHPathf escapes every argument as a path segment, so a value that got
// past a gate cannot become extra path.
func TestGHPathf(t *testing.T) {
	assert.Equal(t, "repos/o/r/pulls/7", ghPathf("repos/%s/%s/pulls/%s", "o", "r", 7))
	assert.Equal(t, "repos/o%2F..%2Fx/r/pulls",
		ghPathf("repos/%s/%s/pulls", "o/../x", "r"),
		"a slash in an argument must not become a path separator")
}

// TestGHArtifactSubdir sanitizes a name that came from GITHUB rather than from
// the caller — on a public repository, from whoever opened the pull request
// whose job uploaded it.
func TestGHArtifactSubdir(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"coverage", "coverage"},
		{"coverage (ubuntu/latest)", "coverage (ubuntu_latest)"},
		{"..", "artifact"},
		{"", "artifact"},
		{"a\x00b", "a_b"},
		{`we:i<r>d|na*me?"`, "we_i_r_d_na_me__"},
	} {
		assert.Equal(t, tc.want, ghArtifactSubdir(tc.in), "input %q", tc.in)
	}
}

// TestExtractZip is the untrusted-input boundary: an artifact archive is
// written by CI, and on a public repository by anyone who can open a pull
// request. The daemon unpacks it as the operator.
func TestExtractZip(t *testing.T) {
	build := func(t *testing.T, files map[string]string) string {
		t.Helper()
		var buf bytes.Buffer
		w := zip.NewWriter(&buf)
		for name, content := range files {
			f, err := w.Create(name)
			require.NoError(t, err)
			_, err = f.Write([]byte(content))
			require.NoError(t, err)
		}
		require.NoError(t, w.Close())
		path := filepath.Join(t.TempDir(), "a.zip")
		require.NoError(t, os.WriteFile(path, buf.Bytes(), 0o600))
		return path
	}

	t.Run("ordinary entries land where they say", func(t *testing.T) {
		root := t.TempDir()
		archive := build(t, map[string]string{"a/b.txt": "hello", "c.txt": "hi"})
		n, err := extractZip(archive, root, "", 1<<20)
		require.NoError(t, err)
		assert.EqualValues(t, 7, n)
		assert.FileExists(t, filepath.Join(root, "a", "b.txt"))
		assert.FileExists(t, filepath.Join(root, "c.txt"))
	})

	t.Run("a subdirectory keeps two artifacts apart", func(t *testing.T) {
		root := t.TempDir()
		archive := build(t, map[string]string{"report.txt": "x"})
		_, err := extractZip(archive, root, "coverage", 1<<20)
		require.NoError(t, err)
		assert.FileExists(t, filepath.Join(root, "coverage", "report.txt"))
	})

	// The one that matters. An absolute or traversing entry name has to land
	// inside the destination rather than escaping it.
	t.Run("a traversing entry cannot escape", func(t *testing.T) {
		root := t.TempDir()
		outside := filepath.Join(t.TempDir(), "victim")
		archive := build(t, map[string]string{
			"../../../../../../../../etc/passwd": "pwned",
			"/absolute/path.txt":                 "pwned",
		})
		_, err := extractZip(archive, root, "", 1<<20)
		require.NoError(t, err)
		assert.NoFileExists(t, outside)
		assert.FileExists(t, filepath.Join(root, "etc", "passwd"))
		assert.FileExists(t, filepath.Join(root, "absolute", "path.txt"))
	})

	// The budget is checked AS BYTES ARE WRITTEN. A declared uncompressed size
	// in a zip header is attacker-controlled, and measuring afterwards means
	// the disk is already full.
	t.Run("the budget stops an oversized unpack", func(t *testing.T) {
		root := t.TempDir()
		archive := build(t, map[string]string{"big.bin": strings.Repeat("x", 4096)})
		_, err := extractZip(archive, root, "", 1024)
		require.ErrorIs(t, err, errArtifactUnpackTooLarge)
	})
}

// TestProjectAuthor renders the four keys this proxy has always answered with,
// and labels a bot as one — "was this reviewed by a human?" is a question an
// agent acts on.
func TestProjectAuthor(t *testing.T) {
	t.Run("a user", func(t *testing.T) {
		got := (&ghGQLAuthor{TypeName: "User", Login: "mikes", ID: "U_1", Name: "Mikael"}).project()
		doc, err := json.Marshal(got)
		require.NoError(t, err)
		assert.JSONEq(t, `{"id":"U_1","is_bot":false,"login":"mikes","name":"Mikael"}`, string(doc))
	})

	t.Run("a bot", func(t *testing.T) {
		got := (&ghGQLAuthor{TypeName: "Bot", Login: "coderabbitai"}).project()
		doc, err := json.Marshal(got)
		require.NoError(t, err)
		assert.JSONEq(t, `{"id":"","is_bot":true,"login":"app/coderabbitai","name":""}`, string(doc))
	})

	// GitHub returns a null author for content whose account was deleted. A
	// zero-valued author would read as a real one with an empty login.
	t.Run("a deleted account", func(t *testing.T) {
		var absent *ghGQLAuthor
		assert.Nil(t, absent.project())
		assert.Nil(t, (&ghGQLAuthor{}).project())
	})
}

// TestGHLogArchiveFind resolves a step's log entry despite GitHub renaming both
// halves of the path on its way into the archive.
func TestGHLogArchiveFind(t *testing.T) {
	archiveWith := func(names ...string) *ghLogArchive {
		a := &ghLogArchive{files: map[string]*zip.File{}}
		for _, n := range names {
			a.files[n] = &zip.File{FileHeader: zip.FileHeader{Name: n}}
		}
		return a
	}

	t.Run("the literal name", func(t *testing.T) {
		a := archiveWith("build/3_Run tests.txt")
		require.NotNil(t, a.find("build", 3))
	})

	t.Run("a job name GitHub had to sanitize", func(t *testing.T) {
		a := archiveWith("test (ubuntu_latest)/2_Build.txt")
		require.NotNil(t, a.find("test (ubuntu/latest)", 2))
	})

	// In a matrix build several jobs have a step 3, so several entries with the
	// right step number under the WRONG job are an ambiguity, not an answer:
	// attributing one leg's failure to another is worse than falling back to
	// the whole job log.
	t.Run("several jobs' step of the same number is an ambiguity", func(t *testing.T) {
		a := archiveWith("test (ubuntu)/3_Run tests.txt", "test (macos)/3_Run tests.txt")
		assert.Nil(t, a.find("build", 3))
	})

	// The truncated-job-name case: GitHub shortened the directory past
	// recognition, but exactly one entry in the archive carries this step
	// number, so there is nothing to confuse it with.
	t.Run("the only entry with that step number, under an unrecognisable job", func(t *testing.T) {
		a := archiveWith("a-very-long-job-name-truncated-by-git/3_Run tests.txt",
			"other-job/1_Checkout.txt")
		require.NotNil(t, a.find("a-very-long-job-name-that-github-shortened", 3))
	})

	// Ordering must not depend on map iteration: two candidates that both
	// match the literal prefix have to resolve the same way every run.
	t.Run("a tie resolves deterministically", func(t *testing.T) {
		a := archiveWith("build/3_a.txt", "build/3_b.txt")
		first := a.find("build", 3)
		require.NotNil(t, first)
		for range 20 {
			assert.Same(t, first, a.find("build", 3))
		}
	})

	t.Run("a missing step is a normal nil, not a panic", func(t *testing.T) {
		var absent *ghLogArchive
		assert.Nil(t, absent.find("build", 1))
		assert.Nil(t, archiveWith().find("build", 1))
	})
}

// TestGHIsFailure decides which conclusions earn a log read. A cancelled or
// timed-out job leaves one that says why, and a timeout in particular is the
// case where the log is the entire diagnosis.
func TestGHIsFailure(t *testing.T) {
	for _, yes := range []string{"failure", "FAILURE", "timed_out", "cancelled", "startup_failure"} {
		assert.True(t, ghIsFailure(yes), "conclusion %q", yes)
	}
	for _, no := range []string{"success", "skipped", "neutral", ""} {
		assert.False(t, ghIsFailure(no), "conclusion %q", no)
	}
}

// TestGraphQLDocumentsAreInternallyConsistent is a cheap structural check on
// every document this proxy sends.
//
// It cannot validate field names against GitHub's schema — nothing offline can
// — but it catches the class of mistake that the flow tests, which answer from
// canned fixtures, are blind to by construction: a document that is malformed,
// or that uses a variable it never declared. Both fail only on a real call.
//
// The variable check is the load-bearing half. Every caller-supplied value
// reaches GraphQL as a variable, so a renamed or misspelled one is exactly the
// path by which a caller's value would stop being applied — a `pr ls --state
// merged` that silently listed everything, say — with no other symptom.
func TestGraphQLDocumentsAreInternallyConsistent(t *testing.T) {
	docs := map[string]string{
		"ghPRListQuery":     ghPRListQuery,
		"ghPRViewQuery":     ghPRViewQuery,
		"ghPRChecksQuery":   ghPRChecksQuery,
		"ghIssueListQuery":  ghIssueListQuery,
		"ghIssueViewQuery":  ghIssueViewQuery,
		"ghPRStateQuery":   ghPRStateQuery,
		"ghPRReadyMutation": ghPRReadyMutation,
	}
	declared := regexp.MustCompile(`\$(\w+):`)
	used := regexp.MustCompile(`\$(\w+)`)

	for name, doc := range docs {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, strings.Count(doc, "{"), strings.Count(doc, "}"),
				"unbalanced braces")
			assert.Equal(t, strings.Count(doc, "("), strings.Count(doc, ")"),
				"unbalanced parentheses")

			// The signature is everything up to the first newline after the
			// operation name, which is where every document declares its
			// variables.
			signature, _, _ := strings.Cut(strings.TrimSpace(doc), "\n")
			declaredSet := map[string]bool{}
			for _, m := range declared.FindAllStringSubmatch(signature, -1) {
				declaredSet[m[1]] = true
			}
			require.NotEmpty(t, declaredSet, "every document here is parameterised")

			for _, m := range used.FindAllStringSubmatch(doc, -1) {
				assert.True(t, declaredSet[m[1]],
					"$%s is used but never declared in the operation signature", m[1])
			}

			// No caller value may be baked into a document. A `%` would mean
			// somebody reached for fmt.Sprintf, which is the one way a
			// caller's string could become syntax rather than data.
			assert.NotContains(t, doc, "%", "a document must never be built by formatting")
		})
	}
}

// TestGraphQLVariablesMatchTheDocuments pins the other half: that the variables
// the handlers actually send are the ones the documents declare.
//
// A document and its call site live in different files, so a variable renamed
// in one and not the other type-checks perfectly and fails only against
// GitHub.
func TestGraphQLVariablesMatchTheDocuments(t *testing.T) {
	declared := regexp.MustCompile(`\$(\w+):`)
	for _, tc := range []struct {
		name string
		doc  string
		vars []string
	}{
		{"pr list", ghPRListQuery, []string{"owner", "name", "limit", "states"}},
		{"pr view", ghPRViewQuery, []string{"owner", "name", "number"}},
		{"pr checks", ghPRChecksQuery, []string{"owner", "name", "number"}},
		{"issue list", ghIssueListQuery, []string{"owner", "name", "limit", "states"}},
		{"issue view", ghIssueViewQuery, []string{"owner", "name", "number"}},
		{"pr state", ghPRStateQuery, []string{"owner", "name", "number"}},
		{"pr ready", ghPRReadyMutation, []string{"id"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			signature, _, _ := strings.Cut(strings.TrimSpace(tc.doc), "\n")
			var got []string
			for _, m := range declared.FindAllStringSubmatch(signature, -1) {
				got = append(got, m[1])
			}
			assert.ElementsMatch(t, tc.vars, got,
				"the document declares different variables from the ones its caller sends")
		})
	}
}

// TestSetProxyBinariesForTestRestoresARealPath — the override must not leave
// lazy resolution consumed but empty.
//
// Consuming proxyBinaries.once with a no-op would mark resolution done with
// git == "" and err == nil, so the RESTORE would hand the next caller an empty
// path with no error — which reads as success and is then exec'd. This runs
// the override the way a test does and checks what is left behind.
func TestSetProxyBinariesForTestRestoresARealPath(t *testing.T) {
	restore := SetProxyBinariesForTest("/pinned/git")
	pinned, err := proxyBinary("git")
	require.NoError(t, err)
	assert.Equal(t, "/pinned/git", pinned)
	restore()

	after, err := proxyBinary("git")
	if err != nil {
		// A runner without git: the recorded failure has to survive too, or
		// the next caller execs "".
		assert.Empty(t, after)
		return
	}
	assert.NotEmpty(t, after, "restoring must not leave an empty path with a nil error")
	assert.True(t, filepath.IsAbs(after), "and it must still be the absolute path, got %q", after)
}
