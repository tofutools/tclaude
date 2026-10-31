//ci:reads none

package agentd_test

import (
	"encoding/json"
	"net/http"
	"net/url"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
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
