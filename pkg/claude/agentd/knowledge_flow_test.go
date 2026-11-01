//ci:reads none

package agentd_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/knowledge"
	"github.com/tofutools/tclaude/pkg/testharness"
)

const knowledgeTestCommit = "6e63f4ec14c977c7b4f464b47a6a31b32196bcb2"

func knowledgeTestDocs() fstest.MapFS {
	return fstest.MapFS{
		"index.md":     {Data: []byte("# Home\n\n[Companion](companion.md#capture)\n")},
		"companion.md": {Data: []byte("# Companion\n\n## Capture\n\nSee the screen.\n\n## Marks\n\nDraw on it.\n")},
	}
}

// useKnowledge gives the daemon a base with no reachable source: a fetch fails
// fast and docs still answer.
func useKnowledge(t *testing.T, build knowledge.Build) {
	t.Helper()
	base := knowledge.New(build, knowledgeTestDocs(), t.TempDir()).WithSourceURL("http://127.0.0.1:1/")
	t.Cleanup(agentd.SetKnowledgeBaseForTest(base))
}

// The Steward is told about the base and holds what reading it takes; another
// agent does not, and a write is not a thing the API has.
func TestKnowledge_StewardReadsPinnedDocs(t *testing.T) {
	useKnowledge(t, knowledge.Build{Version: "v1", State: knowledge.StateExact, Commit: knowledgeTestCommit, Ref: knowledgeTestCommit})
	f, _ := processAuthoringFlow(t)
	enableCollab(t)
	stubScribeTerminal(t)
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))

	summon := testharness.Serve(f.Mux, agentd.AsHumanPeer(testharness.JSONRequest(t, http.MethodPost, "/v1/scribe", map[string]any{
		"name": "steward", "slugs": []string{agentd.PermNodeExec}, "scope": map[string]any{"kind": "steward"}, "brief": "Help.",
	})))
	require.Equalf(t, http.StatusOK, summon.Code, "summon body=%s", summon.Body.String())
	steward := decodeScribeResp(t, summon).ConvID

	messages, err := db.ListAgentMessagesForConv(steward, 10)
	require.NoError(t, err)
	require.NotEmpty(t, messages)
	assert.Contains(t, messages[len(messages)-1].Body, "tclaude agent knowledge search", "the brief says it exists")

	get := func(conv, path string) (int, map[string]any) {
		rec := testharness.Serve(f.Mux, agentd.AsAgentPeer(testharness.JSONRequest(t, http.MethodGet, path, nil), conv))
		var body map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		return rec.Code, body
	}

	code, status := get(steward, "/v1/knowledge/status")
	require.Equal(t, http.StatusOK, code)
	assert.Contains(t, status["label"], "exact")
	assert.Equal(t, float64(2), status["pages"])

	code, home := get(steward, "/v1/knowledge/read?path=index.md")
	require.Equal(t, http.StatusOK, code)
	links, ok := home["links"].([]any)
	require.True(t, ok, "%v", home)
	require.Len(t, links, 1)
	link := links[0].(map[string]any)
	assert.Equal(t, "docs/companion.md", link["path"])

	code, section := get(steward, "/v1/knowledge/read?path="+url.QueryEscape(link["path"].(string)+"#"+link["section"].(string)))
	require.Equal(t, http.StatusOK, code)
	assert.Contains(t, section["content"], "See the screen.")
	assert.NotContains(t, section["content"], "Draw on it.")
	assert.Contains(t, section["label"], "exact", "every answer carries the pin")

	code, found := get(steward, "/v1/knowledge/search?q=screen&i=1")
	require.Equal(t, http.StatusOK, code)
	assert.Len(t, found["matches"], 1)

	code, _ = get(steward, "/v1/knowledge/ls?path=docs")
	assert.Equal(t, http.StatusOK, code)

	code, _ = get(steward, "/v1/knowledge/read?path="+url.QueryEscape("../config.json"))
	assert.Equal(t, http.StatusBadRequest, code, "the base is the whole world")
	code, _ = get(steward, "/v1/knowledge/read?path=nope.md")
	assert.Equal(t, http.StatusNotFound, code)
	code, _ = get(steward, "/v1/knowledge/read?path=index.md&lines=x")
	assert.Equal(t, http.StatusBadRequest, code)

	const other = "know-aaaa-bbbb-cccc-000000000001"
	f.HaveGroup("team")
	f.HaveMember("team", other)
	f.HaveAliveSession(other, "know", "tclaude-know", f.TestCwd("work"))
	code, _ = get(other, "/v1/knowledge/status")
	assert.Equal(t, http.StatusForbidden, code, "only a Steward by default")

	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		rec := testharness.Serve(f.Mux, agentd.AsAgentPeer(testharness.JSONRequest(t, method, "/v1/knowledge/read?path=index.md", map[string]any{}), steward))
		assert.Equal(t, http.StatusMethodNotAllowed, rec.Code, method)
	}
}

// A download is the operator's call: summoning a Steward, a daemon start and every
// read leave the network alone, and the Steward is told the source is not there.
func TestKnowledge_NothingDownloadsSourceOnItsOwn(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests.Add(1) }))
	t.Cleanup(server.Close)
	build := knowledge.Build{Version: "v1", State: knowledge.StateExact, Commit: knowledgeTestCommit, Ref: knowledgeTestCommit}
	base := knowledge.New(build, knowledgeTestDocs(), t.TempDir()).WithSourceURL(server.URL + "/")
	t.Cleanup(agentd.SetKnowledgeBaseForTest(base))
	f, _ := processAuthoringFlow(t)
	enableCollab(t)
	stubScribeTerminal(t)
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))

	summon := testharness.Serve(f.Mux, agentd.AsHumanPeer(testharness.JSONRequest(t, http.MethodPost, "/v1/scribe", map[string]any{
		"name": "steward", "slugs": []string{agentd.PermNodeExec}, "scope": map[string]any{"kind": "steward"}, "brief": "Help.",
	})))
	require.Equalf(t, http.StatusOK, summon.Code, "summon body=%s", summon.Body.String())
	steward := decodeScribeResp(t, summon).ConvID

	agentd.ReconcileStewardAccessForTest()

	for _, path := range []string{"/v1/knowledge/status", "/v1/knowledge/ls", "/v1/knowledge/read?path=index.md", "/v1/knowledge/search?q=home"} {
		rec := testharness.Serve(f.Mux, agentd.AsAgentPeer(testharness.JSONRequest(t, http.MethodGet, path, nil), steward))
		require.Equalf(t, http.StatusOK, rec.Code, "%s body=%s", path, rec.Body.String())
		var body struct {
			Source knowledge.SourceStatus `json:"source"`
		}
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
		assert.Equal(t, knowledge.SourceUnavailable, body.Source.State, path)
		assert.Equal(t, knowledge.SourceNotDownloaded, body.Source.Detail, path)
	}
	base.WaitForFetch(t.Context())
	assert.Zero(t, requests.Load(), "nothing went to the network")
}

// The source flow for one test: a Steward, and a base fed by a counting server.
type knowledgeSourceFlow struct {
	t        *testing.T
	base     *knowledge.Base
	requests *atomic.Int32
	dash     http.Handler
	mux      http.Handler
	steward  string
}

func newKnowledgeSourceFlow(t *testing.T) *knowledgeSourceFlow {
	t.Helper()
	var requests atomic.Int32
	archive := knowledgeArchive(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		_, _ = w.Write(archive)
	}))
	t.Cleanup(server.Close)
	build := knowledge.Build{Version: "v1.2.3", State: knowledge.StateExact, Commit: knowledgeTestCommit, Ref: knowledgeTestCommit}
	base := knowledge.New(build, knowledgeTestDocs(), t.TempDir()).WithSourceURL(server.URL + "/")
	t.Cleanup(agentd.SetKnowledgeBaseForTest(base))
	f, _ := processAuthoringFlow(t)
	enableCollab(t)
	stubScribeTerminal(t)
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	summon := testharness.Serve(f.Mux, agentd.AsHumanPeer(testharness.JSONRequest(t, http.MethodPost, "/v1/scribe", map[string]any{
		"name": "steward", "slugs": []string{agentd.PermNodeExec}, "scope": map[string]any{"kind": "steward"}, "brief": "Help.",
	})))
	require.Equalf(t, http.StatusOK, summon.Code, "summon body=%s", summon.Body.String())
	return &knowledgeSourceFlow{
		t: t, base: base, requests: &requests, dash: dashMessageMux(t), mux: f.Mux, steward: decodeScribeResp(t, summon).ConvID,
	}
}

func knowledgeArchive(t *testing.T) []byte {
	t.Helper()
	var out bytes.Buffer
	gz := gzip.NewWriter(&out)
	tw := tar.NewWriter(gz)
	require.NoError(t, tw.WriteHeader(&tar.Header{Typeflag: tar.TypeXGlobalHeader, Name: "pax_global_header", PAXRecords: map[string]string{"comment": knowledgeTestCommit}}))
	const file = "package a // needle\n"
	require.NoError(t, tw.WriteHeader(&tar.Header{Name: "tclaude-x/pkg/a.go", Mode: 0o644, Size: int64(len(file)), Typeflag: tar.TypeReg}))
	_, err := tw.Write([]byte(file))
	require.NoError(t, err)
	require.NoError(t, tw.Close())
	require.NoError(t, gz.Close())
	return out.Bytes()
}

type knowledgeSourceBody struct {
	knowledge.SourceStatus
	Ask    bool   `json:"ask"`
	Policy string `json:"policy"`
	Origin string `json:"origin"`
}

func (k *knowledgeSourceFlow) view() knowledgeSourceBody {
	k.t.Helper()
	rec := testharness.Serve(k.dash, testharness.JSONRequest(k.t, http.MethodGet, "/api/knowledge/source", nil))
	require.Equalf(k.t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	var body knowledgeSourceBody
	require.NoError(k.t, json.Unmarshal(rec.Body.Bytes(), &body))
	return body
}

func (k *knowledgeSourceFlow) answer(action string) int {
	k.t.Helper()
	rec := testharness.Serve(k.dash, testharness.JSONRequest(k.t, http.MethodPost, "/api/knowledge/source", map[string]any{"action": action}))
	k.base.WaitForFetch(k.t.Context())
	return rec.Code
}

func (k *knowledgeSourceFlow) savedPolicy() string {
	k.t.Helper()
	cfg, err := config.Load()
	require.NoError(k.t, err)
	return cfg.StewardSourceDownload()
}

// The question goes to the operator once, only while a Steward could use the
// source, and "this version" leaves the choice open for the next one.
func TestKnowledgeSource_AskOnceThenFetchThisVersion(t *testing.T) {
	k := newKnowledgeSourceFlow(t)
	first := k.view()
	assert.True(t, first.Ask, "a Steward with no source: ask")
	assert.Equal(t, config.StewardSourceAsk, first.Policy)
	assert.Equal(t, knowledge.SourceOrigin, first.Origin)
	assert.Zero(t, k.requests.Load(), "asking fetches nothing")

	require.Equal(t, http.StatusOK, k.answer("once"))
	assert.EqualValues(t, 1, k.requests.Load())
	after := k.view()
	assert.False(t, after.Ask, "answered")
	assert.Equal(t, knowledge.SourceReady, after.State)
	assert.Equal(t, 1, after.Files)
	assert.Equal(t, config.StewardSourceAsk, k.savedPolicy(), "a new version asks again")

	rec := testharness.Serve(k.mux, agentd.AsAgentPeer(testharness.JSONRequest(t, http.MethodGet, "/v1/knowledge/read?path=pkg/a.go", nil), k.steward))
	require.Equalf(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "needle")

	require.Equal(t, http.StatusOK, k.answer("once"))
	agentd.ReconcileStewardAccessForTest()
	assert.EqualValues(t, 1, k.requests.Load(), "a version already held is not fetched again")
}

// "Every new version" is kept in Config and acts by itself from then on, and
// "no" is the end of the question.
func TestKnowledgeSource_ChoicesAreKeptInConfig(t *testing.T) {
	k := newKnowledgeSourceFlow(t)
	require.Equal(t, http.StatusOK, k.answer("never"))
	assert.Equal(t, config.StewardSourceNever, k.savedPolicy())
	assert.False(t, k.view().Ask, "no means no more asking")
	assert.Zero(t, k.requests.Load())
	agentd.ReconcileStewardAccessForTest()
	assert.Zero(t, k.requests.Load())

	_, err := config.Update(func(cfg *config.Config, _ error) error {
		cfg.Steward.SourceDownload = config.StewardSourceEachVersion
		return nil
	})
	require.NoError(t, err)
	agentd.ReconcileStewardAccessForTest()
	require.Eventually(t, func() bool { return k.view().State == knowledge.SourceReady }, 5*time.Second, 10*time.Millisecond,
		"a daemon start fetches the version it doesn't hold")
	assert.EqualValues(t, 1, k.requests.Load())

	require.Equal(t, http.StatusOK, k.answer("remove"))
	assert.Equal(t, config.StewardSourceNever, k.savedPolicy(), "removing doesn't bring the question or a download back")
	removed := k.view()
	assert.False(t, removed.Ask)
	assert.Equal(t, knowledge.SourceUnavailable, removed.State)

	assert.Equal(t, http.StatusBadRequest, k.answer("sometime"))
}

// An agent has no way to the answer: the API it can reach only reads.
func TestKnowledgeSource_AnAgentCannotStartADownload(t *testing.T) {
	k := newKnowledgeSourceFlow(t)
	for _, method := range []string{http.MethodPost, http.MethodPut} {
		rec := testharness.Serve(k.mux, agentd.AsAgentPeer(testharness.JSONRequest(t, method, "/api/knowledge/source", map[string]any{"action": "once"}), k.steward))
		assert.Equal(t, http.StatusNotFound, rec.Code, method)
		rec = testharness.Serve(k.mux, agentd.AsAgentPeer(testharness.JSONRequest(t, method, "/v1/knowledge/status", map[string]any{"action": "once"}), k.steward))
		assert.Equal(t, http.StatusMethodNotAllowed, rec.Code, method)
	}
	k.base.WaitForFetch(t.Context())
	assert.Zero(t, k.requests.Load())
}

// A dirty or unknown build reads as that, on every answer.
func TestKnowledge_InexactBuildsAreLabelled(t *testing.T) {
	for _, tc := range []struct {
		build knowledge.Build
		want  string
	}{
		{knowledge.Build{State: knowledge.StateDirty, Commit: knowledgeTestCommit, Ref: knowledgeTestCommit}, "uncommitted changes"},
		{knowledge.Build{State: knowledge.StateUnknown}, "names no commit"},
	} {
		t.Run(string(tc.build.State), func(t *testing.T) {
			useKnowledge(t, tc.build)
			f, _ := processAuthoringFlow(t)
			enableCollab(t)
			rec := testharness.Serve(f.Mux, agentd.AsHumanPeer(testharness.JSONRequest(t, http.MethodGet, "/v1/knowledge/read?path=index.md", nil)))
			require.Equalf(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
			var body struct {
				Label string               `json:"label"`
				State knowledge.BuildState `json:"state"`
			}
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
			assert.Equal(t, tc.build.State, body.State)
			assert.Contains(t, body.Label, tc.want)
			assert.NotContains(t, body.Label, "exact")
		})
	}
}
