package agentd_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
	"github.com/tofutools/tclaude/pkg/testutil"
)

const openCodeImportSecret = "the launch code is heron-lantern"

// fakeOpenCodeStore puts a stand-in `opencode` on PATH: the session store
// contract tclaude's history transfer uses, one JSON file per session. The
// real binary runs in the OpenCode group import smoke workflow.
func fakeOpenCodeStore(t *testing.T) string {
	t.Helper()
	dir := testutil.CanonicalTempDir(t)
	store := filepath.Join(dir, "store")
	require.NoError(t, os.Mkdir(store, 0o700))
	// A launch's private state arrives as XDG_DATA_HOME: that conversation goes
	// in the store under it, so a test can tell the two stores apart.
	script := `#!/bin/sh
store='` + store + `'
case "$XDG_DATA_HOME" in
*/tclaude/opencode-agents/*) store="$XDG_DATA_HOME/fake-store"; mkdir -p "$store" ;;
esac
field() { sed -n "s|.*\"$1\":\"\($2[^\"]*\)\".*|\1|p" "$3" | head -n 1; }
case "$1" in
export) cat "$store/$2.json" ;;
import) cp "$2" "$store/$(field id ses_ "$2").json" ;;
session)
	case "$2" in
	delete) rm "$store/$3.json" ;;
	list)
		sep=''
		printf '['
		for f in "$store"/*.json; do
			[ -e "$f" ] || continue
			printf '%s{"id":"%s","directory":"%s","title":"t","created":1,"updated":1}' \
				"$sep" "$(field id ses_ "$f")" "$(field directory / "$f")"
			sep=,
		done
		printf ']\n'
		;;
	esac
	;;
*) exit 1 ;;
esac
`
	require.NoError(t, os.WriteFile(filepath.Join(dir, "opencode"), []byte(script), 0o755))
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return store
}

// seedOpenCodeSession writes a native session into the fake store and indexes
// it as an OpenCode conversation.
func seedOpenCodeSession(t *testing.T, store, id, title, cwd string) {
	t.Helper()
	const message = "msg_0123456789abcdefghijklmn"
	native, err := json.Marshal(map[string]any{
		"info": map[string]any{"id": id, "slug": "s", "projectID": "global", "directory": cwd, "title": title, "version": "1.18.6", "time": map[string]int64{"created": 1, "updated": 1}},
		"messages": []map[string]any{{
			"info":  map[string]any{"id": message, "sessionID": id, "role": "user", "time": map[string]int64{"created": 1}, "agent": "build", "model": map[string]string{"providerID": "test", "modelID": "fixture"}},
			"parts": []map[string]any{{"id": "prt_0123456789abcdefghijklmn", "sessionID": id, "messageID": message, "type": "text", "text": openCodeImportSecret}},
		}},
	})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(store, id+".json"), native, 0o600))
	require.NoError(t, db.UpsertConvIndex(&db.ConvIndexRow{ConvID: id, ProjectDir: cwd, ProjectPath: cwd, Harness: "opencode", CustomTitle: title}))
}

func storedOpenCodeSessions(t *testing.T, store string) map[string]string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(store, "*.json"))
	require.NoError(t, err)
	out := map[string]string{}
	for _, f := range files {
		raw, err := os.ReadFile(f)
		require.NoError(t, err)
		out[strings.TrimSuffix(filepath.Base(f), ".json")] = string(raw)
	}
	return out
}

// A group member landing on OpenCode keeps its conversation: the dry run says
// so, the import installs it into the store the member's resume reads, and the
// group's rows follow the id that install minted.
func TestGroupImport_OpenCodeMemberKeepsItsHistory(t *testing.T) {
	f := newFlow(t)
	store := fakeOpenCodeStore(t)
	const source = "ses_0123456789abcdefghijklmnop"
	cwd := testutil.CanonicalTempDir(t)
	seedOpenCodeSession(t, store, source, "scout", cwd)
	f.HaveGroup("crew")
	f.HaveMemberWithRole("crew", source, "lead")
	archive := exportGroupAs(t, f, "crew", "paf")

	// --into names a directory the import has to create for the opencode CLI.
	dest := filepath.Join(testutil.CanonicalTempDir(t), "landing")
	q := url.Values{"into": {dest}, "as": {"crew-oc"}}
	r := agentd.AsHumanPeer(httptest.NewRequest(http.MethodPost, "/v1/groups/import/inspect?"+q.Encode(), bytes.NewReader(archive)))
	rec := testharness.Serve(f.Mux, r)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var insp struct {
		Landings []groupLandingView `json:"landings"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &insp))
	require.Len(t, insp.Landings, 1)
	assert.Equal(t, "opencode", insp.Landings[0].Harness)
	assert.Equal(t, "native", insp.Landings[0].Rung, insp.Landings[0].Warnings)
	assert.Empty(t, insp.Landings[0].Warnings)
	assert.Len(t, storedOpenCodeSessions(t, store), 1, "a dry run installs nothing")

	rec, landings := importGroupWith(t, f, archive, q)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Len(t, landings, 1)
	assert.Equal(t, "native", landings[0].Rung)
	var result struct {
		FileWarnings []string `json:"file_warnings"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &result))
	assert.NotContains(t, strings.Join(result.FileWarnings, "\n"), "config only")

	g, err := db.GetAgentGroupByName("crew-oc")
	require.NoError(t, err)
	require.NotNil(t, g)
	members, err := db.ListAgentGroupMembers(g.ID)
	require.NoError(t, err)
	require.Len(t, members, 1)
	landed := members[0].ConvID
	assert.NotEqual(t, source, landed, "the history import mints a fresh session id")

	sessions := storedOpenCodeSessions(t, store)
	require.Contains(t, sessions, landed)
	assert.Contains(t, sessions[landed], openCodeImportSecret)
	assert.Contains(t, sessions[landed], fmt.Sprintf("%q:%q", "directory", dest))
	row, err := db.GetConvIndex(landed)
	require.NoError(t, err)
	require.NotNil(t, row)
	assert.Equal(t, "opencode", row.Harness)
	profile, err := db.ConversationResumeProfileForConv(landed)
	require.NoError(t, err)
	require.NotNil(t, profile, "a resume cannot find the conversation from the daemon's directory")
	assert.Equal(t, "opencode", profile.Harness)
	assert.Equal(t, dest, profile.Cwd)
	agentID, err := db.AgentIDForConv(landed)
	require.NoError(t, err)
	require.NotEmpty(t, agentID)
	allocation, err := db.GetOpenCodeAgentStateAllocation(agentID)
	require.NoError(t, err)
	assert.Nil(t, allocation, "the member resumes on the ambient store, so no private state is allocated")
	require.NotNil(t, landings[0].Posture)
	assert.Equal(t, "ambient", landings[0].Posture.OpenCodeState)
}

// Without the opencode binary the conversation cannot install, and the member
// still imports as configuration, as it can on any host.
func TestGroupImport_OpenCodeMemberWithoutOpenCodeImportsConfigOnly(t *testing.T) {
	f := newFlow(t)
	store := fakeOpenCodeStore(t)
	const source = "ses_0123456789abcdefghijklmnoq"
	cwd := testutil.CanonicalTempDir(t)
	seedOpenCodeSession(t, store, source, "scout", cwd)
	f.HaveGroup("crew")
	f.HaveMember("crew", source)
	archive := exportGroupAs(t, f, "crew", "paf")

	t.Setenv("PATH", testutil.CanonicalTempDir(t))
	t.Setenv("HOME", testutil.CanonicalTempDir(t))
	rec, landings := importGroupWith(t, f, archive, url.Values{"into": {f.TestCwd("dst")}, "as": {"crew-cfg"}})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Len(t, landings, 1)
	assert.Equal(t, "opencode", landings[0].Harness)
	assert.Equal(t, "none", landings[0].Rung)
	assert.Contains(t, strings.Join(landings[0].Warnings, " "), "not installed")
}

// A zip carries no OpenCode history and names no harness. Its member still
// imports as configuration on OpenCode, not as a Claude Code stub.
func TestGroupImport_OpenCodeMemberInZipKeepsItsHarness(t *testing.T) {
	f := newFlow(t)
	store := fakeOpenCodeStore(t)
	const source = "ses_0123456789abcdefghijklmnor"
	cwd := testutil.CanonicalTempDir(t)
	seedOpenCodeSession(t, store, source, "scout", cwd)
	f.HaveGroup("crew")
	f.HaveMember("crew", source)
	archive := exportGroupAs(t, f, "crew", "zip")

	rec, landings := importGroupWith(t, f, archive, url.Values{"into": {f.TestCwd("dst")}, "as": {"crew-zip"}})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Len(t, landings, 1)
	assert.Equal(t, "opencode", landings[0].Harness)
	assert.Equal(t, "none", landings[0].Rung)
}
