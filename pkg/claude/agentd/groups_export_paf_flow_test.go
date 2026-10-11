package agentd_test

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/paf"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// A group PAF previews like a zip, naming its source, and a tampered one is
// refused before anything is written.
func TestGroupImport_PAFInspectAndTamperRefused(t *testing.T) {
	f := newFlow(t)
	const conv = "cccccccc-1111-2222-3333-444444444444"
	f.HaveConvWithTitle(conv, "carol")
	f.HaveAliveSession(conv, "lbl-c", "tmux-c", f.TestCwd("work"))
	f.HaveGroup("crew")
	f.HaveMemberWithRole("crew", conv, "lead")

	archive := exportGroupAs(t, f, "crew", "paf")
	a, err := paf.Open(bytes.NewReader(archive), int64(len(archive)), paf.Options{Purpose: paf.PurposeImport})
	require.NoError(t, err)
	require.Equal(t, paf.KindGroup, a.Manifest.Kind)
	require.NotNil(t, a.Group())
	assert.Equal(t, "crew", a.Group().Name)
	require.Len(t, a.Group().Members, 1)
	assert.Equal(t, "lead", a.Group().Members[0].Role)

	r := agentd.AsHumanPeer(httptest.NewRequest(http.MethodPost, "/v1/groups/import/inspect?as=crew2", bytes.NewReader(archive)))
	rec := testharness.Serve(f.Mux, r)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var insp struct {
		Source      string `json:"source"`
		SourceGroup string `json:"source_group"`
		AgentCount  int    `json:"agent_count"`
		ConvCount   int    `json:"conv_count"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &insp))
	assert.Equal(t, "paf", insp.Source)
	assert.Equal(t, "crew", insp.SourceGroup)
	assert.Equal(t, 1, insp.AgentCount)
	assert.Equal(t, 1, insp.ConvCount)

	tampered := rewriteZipEntry(t, archive, paf.GroupPath, func(b []byte) []byte {
		return bytes.Replace(b, []byte(`"lead"`), []byte(`"boss"`), 1)
	})
	rec = importArchive(f, tampered, f.TestCwd("dst"), "crew3")
	assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	g, err := db.GetAgentGroupByName("crew3")
	require.NoError(t, err)
	assert.Nil(t, g, "a refused import writes nothing")
}

// A group PAF another tool wrote, with no tclaude extension, imports its
// group, members and their Claude Code histories.
func TestGroupImport_ForeignGroupPAF(t *testing.T) {
	f := newFlow(t)
	const conv = "dddddddd-1111-2222-3333-444444444444"
	history := `{"type":"user","uuid":"u1","sessionId":"` + conv + `","timestamp":"2026-10-11T10:00:00Z","cwd":"/elsewhere/w","message":{"role":"user","content":"hello from elsewhere"}}` + "\n"

	var buf bytes.Buffer
	w := paf.NewWriter(&buf, paf.KindGroup, paf.Limits{})
	native := paf.AgentPath("dana", "native/claude-jsonl/history.jsonl")
	require.NoError(t, w.AddAgent("dana", paf.Agent{Name: "dana", Native: &paf.Native{Format: "claude-jsonl", Path: native, Harness: "claude", SourceConvID: conv}},
		paf.Environment{Harness: paf.Harness{Name: "claude"}, Workspace: &paf.Workspace{Cwd: "/elsewhere/w"}}))
	require.NoError(t, w.WriteFile(native, "application/jsonl", []byte(history)))
	require.NoError(t, w.WriteJSON(paf.GroupPath, paf.Group{Name: "visitors", Description: "from another tool", Members: []paf.GroupMember{{Agent: "dana", Role: "scout"}}}))
	_, err := w.Close(paf.Producer{Name: "other-tool"})
	require.NoError(t, err)

	dst := f.TestCwd("dst")
	res := importArchiveOK(t, f, buf.Bytes(), dst, "")
	assert.Equal(t, "visitors", res.Group)
	assert.Equal(t, 1, res.AgentCount)

	g, err := db.GetAgentGroupByName("visitors")
	require.NoError(t, err)
	require.NotNil(t, g)
	members, err := db.ListAgentGroupMembers(g.ID)
	require.NoError(t, err)
	require.Len(t, members, 1)
	assert.Equal(t, conv, members[0].ConvID)
	assert.Equal(t, "scout", members[0].Role)
	restored, err := os.ReadFile(convJSONLPath(f.World.HomeDir, res.TargetDir, conv))
	require.NoError(t, err)
	assert.Contains(t, string(restored), "hello from elsewhere")
	assert.Contains(t, string(restored), res.TargetDir)
}

// rewriteZipEntry copies a zip archive, passing one entry through edit.
func rewriteZipEntry(t *testing.T, raw []byte, name string, edit func([]byte) []byte) []byte {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	require.NoError(t, err)
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	edited := false
	for _, f := range zr.File {
		rc, err := f.Open()
		require.NoError(t, err)
		data, err := io.ReadAll(rc)
		require.NoError(t, err)
		_ = rc.Close()
		if f.Name == name {
			next := edit(data)
			edited = !bytes.Equal(next, data)
			data = next
		}
		w, err := zw.Create(f.Name)
		require.NoError(t, err)
		_, err = w.Write(data)
		require.NoError(t, err)
	}
	require.NoError(t, zw.Close())
	require.True(t, edited, "the edit changed %s", name)
	return buf.Bytes()
}

func TestGroupExport_RejectsUnknownFormat(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("crew")
	r := agentd.AsHumanPeer(httptest.NewRequest(http.MethodGet, "/v1/groups/crew/export?format=tar", nil))
	rec := testharness.Serve(f.Mux, r)
	assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
}
