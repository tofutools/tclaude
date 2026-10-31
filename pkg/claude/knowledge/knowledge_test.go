//ci:reads docs/

package knowledge

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/docs"
	"github.com/tofutools/tclaude/pkg/testutil"
)

const testCommit = "6e63f4ec14c977c7b4f464b47a6a31b32196bcb2"

var testDocs = fstest.MapFS{
	"index.md": {Data: []byte("# Home\n\nSee [the companion](companion.md#the-steward) and [the web](https://example.com/x.md).\n")},
	"companion.md": {Data: []byte("# Companion\n\nIntro to [home](index.md).\n\n## The Steward\n\nIt helps.\n\n" +
		"```bash\n# not a heading\n[not a link](index.md)\n```\n\n### Capture: seeing\n\nDeep, see [boards](learn/boards.md).\n\n## Next\n\nOther.\n")},
	"learn/boards.md": {Data: []byte("# Boards\n\nBack to [the companion](../companion.md).\n")},
}

func TestIdentify(t *testing.T) {
	settings := func(pairs ...string) []debug.BuildSetting {
		var out []debug.BuildSetting
		for i := 0; i < len(pairs); i += 2 {
			out = append(out, debug.BuildSetting{Key: pairs[i], Value: pairs[i+1]})
		}
		return out
	}
	cases := []struct {
		name     string
		info     debug.BuildInfo
		state    BuildState
		commit   string
		ref      string
		inLabel  string
		notLabel string
	}{
		{name: "clean checkout", info: debug.BuildInfo{Settings: settings("vcs.revision", testCommit, "vcs.modified", "false")},
			state: StateExact, commit: testCommit, ref: testCommit, inLabel: "exact", notLabel: "uncommitted"},
		{name: "dirty checkout", info: debug.BuildInfo{Settings: settings("vcs.revision", testCommit, "vcs.modified", "true")},
			state: StateDirty, commit: testCommit, ref: testCommit, inLabel: "uncommitted"},
		{name: "go install of a tag", info: debug.BuildInfo{Main: debug.Module{Version: "v1.2.3"}},
			state: StateExact, ref: "v1.2.3", inLabel: "v1.2.3"},
		{name: "go install of a commit", info: debug.BuildInfo{Main: debug.Module{Version: "v0.0.2475-0.20261031200100-6e63f4ec14c9"}},
			state: StateExact, commit: "6e63f4ec14c9", ref: "6e63f4ec14c9", inLabel: "6e63f4ec14c9"},
		{name: "plain build", info: debug.BuildInfo{Main: debug.Module{Version: "(devel)"}}, state: StateUnknown, inLabel: "no commit"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			build := identify(&tc.info, "v9")
			assert.Equal(t, tc.state, build.State)
			assert.Equal(t, tc.commit, build.Commit)
			assert.Equal(t, tc.ref, build.Ref)
			assert.Contains(t, build.Label(), tc.inLabel)
			if tc.notLabel != "" {
				assert.NotContains(t, build.Label(), tc.notLabel)
			}
		})
	}
}

func docsBase(t *testing.T, build Build) *Base {
	t.Helper()
	return New(build, testDocs, filepath.Join(testutil.CanonicalTempDir(t), "knowledge"))
}

func TestReadWalksTheGraph(t *testing.T) {
	base := docsBase(t, Build{State: StateUnknown})

	home, err := base.Read(ReadRequest{Path: "index.md"})
	require.NoError(t, err)
	assert.Equal(t, "docs/index.md", home.Path, "a bare page name means the docs")
	require.Equal(t, []Link{{Path: "docs/companion.md", Section: "the-steward", Text: "the companion"}}, home.Links, "links out of the docs are no edges")

	section, err := base.Read(ReadRequest{Path: home.Links[0].Path + "#" + home.Links[0].Section})
	require.NoError(t, err)
	assert.Contains(t, section.Content, "It helps.")
	assert.Contains(t, section.Content, "### Capture: seeing", "a section holds its subsections")
	assert.NotContains(t, section.Content, "Other.", "and stops at the next one at its level")
	require.Len(t, section.Links, 1, "a link in a code block is no edge")
	assert.Equal(t, "docs/learn/boards.md", section.Links[0].Path)
	assert.ElementsMatch(t, []Link{{Path: "docs/index.md"}, {Path: "docs/learn/boards.md"}}, section.LinkedFrom)

	nested, err := base.Read(ReadRequest{Path: "docs/companion.md#capture-seeing"})
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(nested.Content, "### Capture: seeing"), "the anchor drops punctuation as the docs site does")

	_, err = base.Read(ReadRequest{Path: "companion.md#nope"})
	require.ErrorIs(t, err, ErrNotFound)
	assert.Contains(t, err.Error(), "the-steward", "a miss lists what is there")
}

func TestReadStaysInside(t *testing.T) {
	base := docsBase(t, Build{State: StateUnknown})
	for _, path := range []string{"../secret", "/etc/passwd", "docs/../../secret", "a/../b"} {
		_, err := base.Read(ReadRequest{Path: path})
		require.ErrorIs(t, err, ErrBadRequest, path)
	}
	_, err := base.Read(ReadRequest{Path: "pkg/x.go"})
	require.ErrorIs(t, err, ErrNotFound)
	assert.Contains(t, err.Error(), "source is not available", "the reader learns why")
}

func TestReadLimits(t *testing.T) {
	long := strings.Repeat("line\n", maxReadLines+5)
	base := New(Build{}, fstest.MapFS{"long.md": {Data: []byte(long)}}, t.TempDir())
	page, err := base.Read(ReadRequest{Path: "long.md"})
	require.NoError(t, err)
	assert.True(t, page.Truncated)
	assert.Equal(t, maxReadLines, page.ToLine)
	assert.Equal(t, maxReadLines+5, page.TotalLines)

	rest, err := base.Read(ReadRequest{Path: "long.md", From: maxReadLines + 1, To: maxReadLines + 5})
	require.NoError(t, err)
	assert.False(t, rest.Truncated)
	assert.Equal(t, 5, strings.Count(rest.Content, "\n"))

	_, err = base.Read(ReadRequest{Path: "long.md", From: maxReadLines + 7})
	require.ErrorIs(t, err, ErrBadRequest)
}

type tarFile struct {
	name string
	data string
	link string
}

func githubArchive(t *testing.T, commit string, files ...tarFile) []byte {
	t.Helper()
	var out bytes.Buffer
	gz := gzip.NewWriter(&out)
	tw := tar.NewWriter(gz)
	require.NoError(t, tw.WriteHeader(&tar.Header{Typeflag: tar.TypeXGlobalHeader, Name: "pax_global_header", PAXRecords: map[string]string{"comment": commit}}))
	for _, file := range files {
		header := &tar.Header{Name: "tclaude-" + commit + "/" + file.name, Mode: 0o644, Size: int64(len(file.data)), Typeflag: tar.TypeReg}
		if file.link != "" {
			header.Typeflag, header.Linkname, header.Size = tar.TypeSymlink, file.link, 0
		}
		require.NoError(t, tw.WriteHeader(header))
		_, err := tw.Write([]byte(file.data))
		require.NoError(t, err)
	}
	require.NoError(t, tw.Close())
	require.NoError(t, gz.Close())
	return out.Bytes()
}

func serveArchive(t *testing.T, archive []byte) (url string, requests *int) {
	t.Helper()
	count := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count++
		_, _ = w.Write(archive)
	}))
	t.Cleanup(server.Close)
	return server.URL + "/", &count
}

func fetched(t *testing.T, base *Base) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	base.Prefetch()
	base.WaitForFetch(ctx)
}

func TestSourceIsFetchedByCommitAndFiltered(t *testing.T) {
	archive := githubArchive(t, testCommit,
		tarFile{name: "pkg/a.go", data: "package a // needle\n"},
		tarFile{name: "docs/index.md", data: "stale needle"},
		tarFile{name: ".claude/settings.json", data: "{}"},
		tarFile{name: "pkg/blob.bin", data: "a\x00b"},
		tarFile{name: "pkg/huge.txt", data: strings.Repeat("x", maxFileBytes+1)},
		tarFile{name: "escape", link: "/etc/passwd"},
	)
	url, requests := serveArchive(t, archive)
	base := docsBase(t, Build{State: StateExact, Commit: testCommit, Ref: testCommit}).WithSourceURL(url)
	require.NoError(t, os.MkdirAll(filepath.Join(base.root, "old-commit"), 0o700))

	assert.Equal(t, SourceFetching, base.Status().Source.State)
	fetched(t, base)
	status := base.Status()
	require.Equal(t, SourceReady, status.Source.State, status.Source.Detail)
	assert.Equal(t, 1, status.Source.Files, "only the text file outside docs/ and .claude/")
	assert.Equal(t, testCommit, status.Source.Commit)
	assert.NoDirExists(t, filepath.Join(base.root, "old-commit"), "an older build's tree goes")

	page, err := base.Read(ReadRequest{Path: "pkg/a.go"})
	require.NoError(t, err)
	assert.Contains(t, page.Content, "needle")
	assert.Empty(t, page.Links, "source is not part of the graph")

	found, err := base.Search(t.Context(), SearchRequest{Pattern: "NEEDLE", IgnoreCase: true})
	require.NoError(t, err)
	require.Len(t, found.Matches, 1, "docs come from the binary, not the archive")
	assert.Equal(t, Match{Path: "pkg/a.go", Line: 1, Text: "package a // needle"}, found.Matches[0])

	top, err := base.List("")
	require.NoError(t, err)
	assert.Equal(t, []Entry{{Path: "docs", Dir: true}, {Path: "pkg", Dir: true}}, top)

	base.Prefetch()
	assert.Equal(t, 1, *requests, "a stored tree is not fetched again")
}

func TestSourceFromAnotherCommitIsRefused(t *testing.T) {
	url, _ := serveArchive(t, githubArchive(t, strings.Repeat("a", 40), tarFile{name: "a.go", data: "package a\n"}))
	base := docsBase(t, Build{State: StateExact, Commit: testCommit, Ref: testCommit}).WithSourceURL(url)
	fetched(t, base)
	source := base.Status().Source
	assert.Equal(t, SourceFailed, source.State)
	assert.Contains(t, source.Detail, "not "+shortRef(testCommit))
	_, err := base.Read(ReadRequest{Path: "a.go"})
	require.ErrorIs(t, err, ErrNotFound)
}

func TestAFailedFetchLeavesDocsAndWaitsToRetry(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		http.NotFound(w, r)
	}))
	defer server.Close()
	base := docsBase(t, Build{State: StateDirty, Commit: testCommit, Ref: testCommit}).WithSourceURL(server.URL + "/")
	fetched(t, base)
	source := base.Status().Source
	assert.Equal(t, SourceFailed, source.State)
	assert.Contains(t, source.Detail, "not on GitHub")
	_, err := base.Read(ReadRequest{Path: "index.md"})
	require.NoError(t, err, "docs need no network")
	base.Prefetch()
	assert.Equal(t, 1, requests, "a dead link is not retried on every call")
}

func TestAnUnknownBuildFetchesNothing(t *testing.T) {
	url, requests := serveArchive(t, nil)
	base := docsBase(t, Build{State: StateUnknown}).WithSourceURL(url)
	base.Prefetch()
	base.WaitForFetch(t.Context())
	assert.Zero(t, *requests)
	assert.Equal(t, SourceUnavailable, base.Status().Source.State)
}

func TestSearchLimit(t *testing.T) {
	base := New(Build{}, fstest.MapFS{"a.md": {Data: []byte(strings.Repeat("hit\n", 5))}}, t.TempDir())
	found, err := base.Search(t.Context(), SearchRequest{Pattern: "hit", Limit: 3})
	require.NoError(t, err)
	assert.Len(t, found.Matches, 3)
	assert.True(t, found.Truncated)
	_, err = base.Search(t.Context(), SearchRequest{Pattern: "("})
	require.ErrorIs(t, err, ErrBadRequest)
}

// TestShippedDocs reads the embedded pages themselves: every page is there, the
// links form a graph, and what the Steward is pointed at resolves.
func TestShippedDocs(t *testing.T) {
	base := New(Build{}, docs.FS, t.TempDir())
	var want []string
	root := filepath.Join("..", "..", "..", "docs")
	require.NoError(t, filepath.WalkDir(root, func(name string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() && (entry.Name() == "v2-plan-and-ideas" || entry.Name() == "assets") {
			return filepath.SkipDir
		}
		if strings.HasSuffix(name, ".md") {
			rel, relErr := filepath.Rel(root, name)
			want = append(want, filepath.ToSlash(rel))
			return relErr
		}
		return nil
	}))
	for _, page := range want {
		_, err := base.Read(ReadRequest{Path: "docs/" + page})
		assert.NoError(t, err, "docs/embed.go leaves out %s", page)
	}
	for _, section := range []string{"capture", "marks", "saved-captures", "commands-behind-this"} {
		page, err := base.Read(ReadRequest{Path: "companion.md#" + section})
		require.NoError(t, err)
		assert.NotEmpty(t, page.Content)
	}
	page, err := base.Read(ReadRequest{Path: "companion.md"})
	require.NoError(t, err)
	assert.NotEmpty(t, page.Links)
	assert.NotEmpty(t, page.LinkedFrom)
}
