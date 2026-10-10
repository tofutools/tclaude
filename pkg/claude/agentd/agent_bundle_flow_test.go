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
	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/agentbundle"
	"github.com/tofutools/tclaude/pkg/claude/common/convops"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/testharness"
	"github.com/tofutools/tclaude/pkg/testutil"
)

func postAgentBundle(t *testing.T, f *testharness.Flow, b *agentbundle.Bundle, query string, asAgent string) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := b.Encode()
	require.NoError(t, err)
	r := httptest.NewRequest(http.MethodPost, "/v1/agent-bundle/import?"+query, bytes.NewReader(raw))
	if asAgent == "" {
		r = agentd.AsHumanPeer(r)
	} else {
		r = agentd.AsAgentPeer(r, asAgent)
	}
	rec := httptest.NewRecorder()
	f.Mux.ServeHTTP(rec, r)
	return rec
}
func TestAgentBundleNativeRoundTrip(t *testing.T) {
	for _, name := range []string{"claude", "codex"} {
		t.Run(name, func(t *testing.T) {
			f := newFlow(t)
			t.Setenv("CODEX_HOME", "")
			f.HaveGroup("source")
			f.HaveGroup("receiver")
			const source = "019fe740-43a4-7023-b8ae-1ee64459f2a1"
			cwd := testutil.CanonicalTempDir(t)
			if name == "claude" {
				f.HaveAliveSession(source, "original", "original-pane", cwd)
			} else {
				f.HaveAliveCodexSession(source, "original", "original-pane", cwd)
			}
			f.HaveMemberWithRole("source", source, "reviewer")
			require.NoError(t, db.GrantAgentPermission(source, "config.import", "human"))
			rec := profileReq(t, f, http.MethodGet, "/v1/agent-bundle/export?agent="+source+"&history=true", nil)
			require.Equal(t, 200, rec.Code, rec.Body.String())
			b, err := agentbundle.Decode(rec.Body.Bytes())
			require.NoError(t, err)
			require.NotNil(t, b.Manifest.History)
			assert.Equal(t, name, b.Manifest.Agent.Harness)
			assert.Equal(t, cwd, b.Manifest.Agent.Paths.Cwd)
			require.NotEmpty(t, b.Manifest.Agent.Permissions)
			h, _ := harness.Get(name)
			before, err := h.History.Export(source, cwd)
			require.NoError(t, err)
			rec = postAgentBundle(t, f, b, "keep_paths=true", "")
			require.Equal(t, 200, rec.Code, rec.Body.String())
			assert.Contains(t, rec.Body.String(), `"applied":false`)
			assert.Contains(t, rec.Body.String(), `"history":true`)
			receiver, err := db.GetAgentGroupByName("receiver")
			require.NoError(t, err)
			members, err := db.ListAgentGroupMembers(receiver.ID)
			require.NoError(t, err)
			assert.Empty(t, members)
			// Even a forged inline profile cannot import source grants or ownership.
			b.Manifest.Agent.Profile = json.RawMessage(`{"is_owner":true,"permission_overrides":{"config.import":{"effect":"grant"}},"role_refs":["nonexistent-source-role"]}`)
			dest := testutil.CanonicalTempDir(t)
			rec = postAgentBundle(t, f, b, "apply=true&group=receiver&name=receiving-agent&cwd="+url.QueryEscape(dest), "")
			require.Equal(t, 200, rec.Code, rec.Body.String())
			var result struct {
				Applied bool `json:"applied"`
				Spawn   struct {
					ConvID string `json:"conv_id"`
				} `json:"spawn"`
			}
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &result))
			require.True(t, result.Applied)
			require.NotEmpty(t, result.Spawn.ConvID)
			assert.NotEqual(t, source, result.Spawn.ConvID)
			member, err := db.FindMemberInGroup(receiver.ID, result.Spawn.ConvID)
			require.NoError(t, err)
			require.NotNil(t, member)
			assert.Equal(t, "reviewer", member.Role)
			owner, err := db.IsAgentGroupOwner(receiver.ID, result.Spawn.ConvID)
			require.NoError(t, err)
			assert.False(t, owner)
			grants, err := db.ListAgentPermissionOverrideRowsForConv(result.Spawn.ConvID)
			require.NoError(t, err)
			assert.Empty(t, grants)
			sourceMembership, err := db.GetAgentGroupByName("source")
			require.NoError(t, err)
			member, err = db.FindMemberInGroup(sourceMembership.ID, result.Spawn.ConvID)
			require.NoError(t, err)
			assert.Nil(t, member)
			imported, err := h.History.Export(result.Spawn.ConvID, dest)
			require.NoError(t, err)
			assert.Contains(t, string(imported), dest)
			after, err := h.History.Export(source, cwd)
			require.NoError(t, err)
			assert.Equal(t, before, after)
			ref, err := h.Convs.Resolve(result.Spawn.ConvID, dest, false)
			require.NoError(t, err)
			require.NotNil(t, ref)
			assert.Equal(t, dest, ref.ProjectPath)
		})
	}
}
func TestAgentBundlePathsAndGates(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("receiver")
	b := &agentbundle.Bundle{Manifest: agentbundle.Manifest{Format: agentbundle.Format, FormatVersion: 1, Agent: agentbundle.Definition{Name: "portable", Harness: "claude", Profile: json.RawMessage(`{}`), Paths: agentbundle.Paths{Cwd: "/missing/portable/project"}}}}
	rec := postAgentBundle(t, f, b, "keep_paths=true", "")
	require.Equal(t, 200, rec.Code)
	assert.Contains(t, rec.Body.String(), "use --cwd")
	rec = postAgentBundle(t, f, b, "apply=true&group=receiver&keep_paths=true", "")
	require.Equal(t, 409, rec.Code)
	cwd := testutil.CanonicalTempDir(t)
	b.Manifest.Agent.Paths.Cwd = cwd
	rec = postAgentBundle(t, f, b, "apply=true&group=receiver&keep_paths=true", "")
	require.Equal(t, 200, rec.Code, rec.Body.String())
	f.HaveMember("receiver", "caller")
	rec = postAgentBundle(t, f, b, "cwd="+url.QueryEscape(cwd), "caller")
	require.Equal(t, 403, rec.Code, rec.Body.String())
	// Import permission alone does not confer spawn authority.
	require.NoError(t, db.GrantAgentPermission("caller", agentd.PermAgentBundleImport, "human"))
	rec = postAgentBundle(t, f, b, "apply=true&group=receiver&cwd="+url.QueryEscape(cwd), "caller")
	require.Equal(t, 403, rec.Code, rec.Body.String())
	rec = profileReq(t, f, http.MethodGet, "/v1/agent-bundle/export?agent=caller", nil)
	require.Equal(t, 200, rec.Code, rec.Body.String())
	r := agentd.AsAgentPeer(httptest.NewRequest(http.MethodGet, "/v1/agent-bundle/export?agent=caller", nil), "caller")
	rec = httptest.NewRecorder()
	f.Mux.ServeHTTP(rec, r)
	require.Equal(t, 403, rec.Code, rec.Body.String())
}
func TestAgentBundleCredentialCountsAndConfigOnly(t *testing.T) {
	f := newFlow(t)
	t.Setenv("CODEX_HOME", "")
	const source = "019fe740-43a4-7023-b8ae-1ee64459f2a1"
	cwd := testutil.CanonicalTempDir(t)
	f.HaveAliveSession(source, "original", "original-pane", cwd)
	f.HaveGroup("source")
	f.HaveMember("source", source)
	transcriptPath := filepath.Join(convops.GetClaudeProjectPath(cwd), source+".jsonl")
	file, err := os.OpenFile(transcriptPath, os.O_APPEND|os.O_WRONLY, 0600)
	require.NoError(t, err)
	for i := 0; i < 8; i++ {
		_, err = fmt.Fprintf(file, "{\"type\":\"user\",\"sessionId\":%q,\"message\":{\"content\":\"api_key=keepverbatim123456789\"}}\n", source)
		require.NoError(t, err)
	}
	require.NoError(t, file.Close())
	rec := profileReq(t, f, http.MethodGet, "/v1/agent-bundle/export?agent="+source+"&history=true", nil)
	require.Equal(t, 422, rec.Code, rec.Body.String())
	assert.NotContains(t, rec.Body.String(), "keepverbatim")
	var report struct {
		Findings []agentbundle.Finding `json:"findings"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &report))
	require.Len(t, report.Findings, 1)
	assert.Equal(t, 8, report.Findings[0].Count)
	assert.Len(t, report.Findings[0].Locations, 3)
	rec = profileReq(t, f, http.MethodGet, "/v1/agent-bundle/export?agent="+source+"&history=true&allow_flagged=true", nil)
	require.Equal(t, 200, rec.Code, rec.Body.String())
	b, err := agentbundle.Decode(rec.Body.Bytes())
	require.NoError(t, err)
	assert.Equal(t, 8, strings.Count(string(b.Transcript), "keepverbatim123456789"))
	// Missing optional native tools carry the complete config with a visible warning.
	t.Setenv("PATH", testutil.CanonicalTempDir(t))
	cx := "ses_portable-config"
	require.NoError(t, db.UpsertConvIndex(&db.ConvIndexRow{ConvID: cx, Harness: "opencode", ProjectPath: cwd, FullPath: filepath.Join(cwd, "unused")}))
	f.HaveMember("source", cx)
	rec = profileReq(t, f, http.MethodGet, "/v1/agent-bundle/export?agent="+cx+"&history=true", nil)
	require.Equal(t, 200, rec.Code, rec.Body.String())
	b, err = agentbundle.Decode(rec.Body.Bytes())
	require.NoError(t, err)
	assert.Nil(t, b.Manifest.History)
	assert.Contains(t, strings.Join(b.Manifest.Warnings, " "), "config only")
	// Skipping history also works with no OpenCode binary installed.
	rec = profileReq(t, f, http.MethodGet, "/v1/agent-bundle/export?agent="+cx, nil)
	require.Equal(t, 200, rec.Code, rec.Body.String())
	b, err = agentbundle.Decode(rec.Body.Bytes())
	require.NoError(t, err)
	assert.Nil(t, b.Manifest.History)
	assert.Equal(t, "opencode", b.Manifest.Agent.Harness)
}

type failingBundleSpawner struct {
	importedID     string
	transcriptPath string
}

func (s *failingBundleSpawner) SpawnNew(clcommon.SpawnArgs) error {
	return fmt.Errorf("unexpected fresh spawn")
}
func (s *failingBundleSpawner) SpawnResume(args clcommon.SpawnArgs) error {
	s.importedID = args.ConvID
	row, err := db.GetConvIndex(args.ConvID)
	if err != nil {
		return err
	}
	if row != nil {
		s.transcriptPath = row.FullPath
	}
	return fmt.Errorf("imported launch failed")
}
func TestAgentBundleFailedLaunchRemovesImportedHistory(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("receiver")
	cwd := testutil.CanonicalTempDir(t)
	b := &agentbundle.Bundle{Manifest: agentbundle.Manifest{Format: agentbundle.Format, FormatVersion: 1, Agent: agentbundle.Definition{Name: "portable", Harness: "claude", Profile: json.RawMessage(`{}`), Paths: agentbundle.Paths{Cwd: cwd}}}}
	const id = "019fe740-43a4-7023-b8ae-1ee64459f2a1"
	b.SetHistory("claude-jsonl", id, []byte(`{"type":"user","sessionId":"`+id+`","cwd":"/source","message":{"content":"original text"}}`+"\n"))
	spawner := &failingBundleSpawner{}
	previous := agentd.Spawn
	agentd.Spawn = spawner
	t.Cleanup(func() { agentd.Spawn = previous })
	rec := postAgentBundle(t, f, b, "apply=true&group=receiver&keep_paths=true", "")
	require.Equal(t, 500, rec.Code, rec.Body.String())
	require.NotEmpty(t, spawner.importedID)
	require.NotEmpty(t, spawner.transcriptPath)
	assert.NoFileExists(t, spawner.transcriptPath)
	row, err := db.GetConvIndex(spawner.importedID)
	require.NoError(t, err)
	assert.Nil(t, row)
	actor, err := db.GetAgentByConv(spawner.importedID)
	require.NoError(t, err)
	assert.Nil(t, actor)
}
