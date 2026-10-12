package agentd_test

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/groupexport"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/paf"
	"github.com/tofutools/tclaude/pkg/testharness"
	"github.com/tofutools/tclaude/pkg/testutil"
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

type groupLandingView struct {
	ConvID        string   `json:"conv_id"`
	Title         string   `json:"title"`
	SourceHarness string   `json:"source_harness"`
	Harness       string   `json:"harness"`
	Model         string   `json:"model"`
	Rung          string   `json:"rung"`
	Warnings      []string `json:"warnings"`
}

func importGroupWith(t *testing.T, f *testharness.Flow, archive []byte, query url.Values) (*httptest.ResponseRecorder, []groupLandingView) {
	t.Helper()
	r := agentd.AsHumanPeer(httptest.NewRequest(http.MethodPost, "/v1/groups/import?"+query.Encode(), bytes.NewReader(archive)))
	rec := testharness.Serve(f.Mux, r)
	var out struct {
		Landings []groupLandingView `json:"landings"`
	}
	if rec.Code == http.StatusOK {
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	}
	return rec, out.Landings
}

// A group PAF lands its members on another harness: each conversation is
// translated, installed through that harness's own history import under a
// fresh conversation id, and the group's rows follow it. Exported again as
// PAF, the members carry their new harness's native history, which lands
// natively on that harness.
func TestGroupImport_PAFMembersLandOnAnotherHarness(t *testing.T) {
	f := newFlow(t)
	t.Setenv("CODEX_HOME", "")
	const a = "0f0e0d0c-0b0a-4908-8706-0504030201a1"
	const b = "0f0e0d0c-0b0a-4908-8706-0504030201b2"
	cwd := testutil.CanonicalTempDir(t)
	seedClaudeConversation(t, f, a, "grp-a", cwd)
	seedClaudeConversation(t, f, b, "grp-b", cwd)
	require.NoError(t, db.SetConvIndexCustomTitle(a, "alice", "claude"))
	require.NoError(t, db.SetConvIndexCustomTitle(b, "bob", "claude"))
	src := f.HaveGroup("crew")
	f.HaveMemberWithRole("crew", a, "lead")
	f.HaveMemberWithRole("crew", b, "worker")
	_, err := db.InsertAgentMessage(&db.AgentMessage{GroupID: src.ID, FromConv: a, ToConv: b, Body: "hello bob"})
	require.NoError(t, err)
	archive := exportGroupAs(t, f, "crew", "paf")

	dest := testutil.CanonicalTempDir(t)
	q := url.Values{"into": {dest}, "as": {"crew-codex"}, "harness": {"codex"}, "model": {"gpt-5.5"}, "member_model": {"bob=gpt-5.5-mini"}}

	// The dry run plans every member without writing anything.
	r := agentd.AsHumanPeer(httptest.NewRequest(http.MethodPost, "/v1/groups/import/inspect?"+q.Encode(), bytes.NewReader(archive)))
	rec := testharness.Serve(f.Mux, r)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var insp struct {
		Landings []groupLandingView `json:"landings"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &insp))
	require.Len(t, insp.Landings, 2)
	for _, l := range insp.Landings {
		assert.Equal(t, "claude", l.SourceHarness)
		assert.Equal(t, "codex", l.Harness)
		assert.Equal(t, "translated", l.Rung, l.Warnings)
	}
	g, err := db.GetAgentGroupByName("crew-codex")
	require.NoError(t, err)
	require.Nil(t, g, "a dry run writes nothing")

	rec, landings := importGroupWith(t, f, archive, q)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Len(t, landings, 2)
	g, err = db.GetAgentGroupByName("crew-codex")
	require.NoError(t, err)
	require.NotNil(t, g)
	members, err := db.ListAgentGroupMembers(g.ID)
	require.NoError(t, err)
	require.Len(t, members, 2)
	codex := mustGetHarness(t, harness.CodexName)
	byRole := map[string]string{}
	for _, m := range members {
		byRole[m.Role] = m.ConvID
		assert.NotContains(t, []string{a, b}, m.ConvID, "a harness import mints a fresh conversation id")
		row, err := db.GetConvIndex(m.ConvID)
		require.NoError(t, err)
		require.NotNil(t, row)
		assert.Equal(t, "codex", row.Harness)
		assert.Contains(t, historyText(t, codex, m.ConvID, dest), morphSecret)
	}
	for _, l := range landings {
		if l.Title == "bob" {
			assert.Equal(t, "gpt-5.5-mini", l.Model)
		}
	}
	if row, _ := db.GetConvIndex(byRole["lead"]); assert.NotNil(t, row) {
		// The source still exists on this machine, so the copy is retitled
		// as any same-machine import is.
		assert.True(t, strings.HasPrefix(row.CustomTitle, "alice-i-"), row.CustomTitle)
	}
	msgs, err := db.ListAgentMessagesForConv(byRole["worker"], 10)
	require.NoError(t, err)
	require.NotEmpty(t, msgs)
	assert.Equal(t, byRole["lead"], msgs[0].FromConv, "messages follow the minted conversation ids")

	// Exported again, the members carry Codex rollouts and land natively.
	again := exportGroupAs(t, f, "crew-codex", "paf")
	pa, err := paf.Open(bytes.NewReader(again), int64(len(again)), paf.Options{Purpose: paf.PurposeImport})
	require.NoError(t, err)
	for _, key := range pa.Manifest.Agents {
		ag, _ := pa.Agent(key)
		require.NotNil(t, ag.Native, key)
		assert.Equal(t, codex.History.Format(), ag.Native.Format)
	}
	rec, landings = importGroupWith(t, f, again, url.Values{"into": {dest}, "as": {"crew-back"}})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	for _, l := range landings {
		assert.Equal(t, "native", l.Rung)
		assert.Equal(t, "codex", l.Harness)
	}
	// The zip carries Claude Code conversations only.
	zipped, err := groupexport.Unmarshal(exportGroupAs(t, f, "crew-codex", "zip"))
	require.NoError(t, err)
	for _, c := range zipped.Convs {
		assert.True(t, c.Missing)
	}
}

func TestGroupImport_UnknownMemberOverrideRefused(t *testing.T) {
	f := newFlow(t)
	const conv = "eeeeeeee-1111-2222-3333-444444444444"
	f.HaveConvWithTitle(conv, "erin")
	f.HaveAliveSession(conv, "lbl-e", "tmux-e", f.TestCwd("work"))
	f.HaveGroup("solo")
	f.HaveMemberWithRole("solo", conv, "lead")
	archive := exportGroupAs(t, f, "solo", "paf")
	rec, _ := importGroupWith(t, f, archive, url.Values{"into": {f.TestCwd("dst")}, "as": {"solo2"}, "member_harness": {"nobody=codex"}})
	assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), `no member \"nobody\"`)
}

// A member whose conversation does not travel still records its harness,
// so it resumes there; an override naming an unknown harness is refused.
func TestGroupImport_ConfigOnlyMemberKeepsItsHarness(t *testing.T) {
	f := newFlow(t)
	var buf bytes.Buffer
	w := paf.NewWriter(&buf, paf.KindGroup, paf.Limits{})
	require.NoError(t, w.AddAgent("cody", paf.Agent{Name: "cody", Conversation: &paf.Conversation{ID: "ffffffff-1111-2222-3333-444444444444"}}, paf.Environment{Harness: paf.Harness{Name: "codex"}}))
	require.NoError(t, w.WriteJSON(paf.GroupPath, paf.Group{Name: "cfg", Members: []paf.GroupMember{{Agent: "cody", Role: "dev"}}}))
	_, err := w.Close(paf.Producer{Name: "other-tool"})
	require.NoError(t, err)

	rec, _ := importGroupWith(t, f, buf.Bytes(), url.Values{"into": {f.TestCwd("dst")}, "member_harness": {"cody=bogus"}})
	assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())

	rec, landings := importGroupWith(t, f, buf.Bytes(), url.Values{"into": {f.TestCwd("dst")}})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Len(t, landings, 1)
	assert.Equal(t, "codex", landings[0].Harness)
	assert.Equal(t, "none", landings[0].Rung)
	row, err := db.GetConvIndex("ffffffff-1111-2222-3333-444444444444")
	require.NoError(t, err)
	require.NotNil(t, row)
	assert.Equal(t, "codex", row.Harness)
	assert.Equal(t, "cody", row.CustomTitle)
}

// A group PAF export scans the group's messages and conversations: a
// credential refuses the export unless allowed, the allowance is recorded in
// privacy.json, and the import preview rescans on this node.
func TestGroupExport_PAFScanRefusesUnlessAllowed(t *testing.T) {
	f := newFlow(t)
	const conv = "abababab-1111-2222-3333-444444444444"
	f.HaveConvWithTitle(conv, "sam")
	f.HaveAliveSession(conv, "lbl-s", "tmux-s", f.TestCwd("work"))
	g := f.HaveGroup("vault")
	f.HaveMemberWithRole("vault", conv, "lead")
	_, err := db.InsertAgentMessage(&db.AgentMessage{GroupID: g.ID, FromConv: conv, ToConv: conv, Body: "deploy with ghp_abcdefghijklmnopqrstuvwxyz0123"})
	require.NoError(t, err)

	r := agentd.AsHumanPeer(httptest.NewRequest(http.MethodGet, "/v1/groups/vault/export?format=paf", nil))
	rec := testharness.Serve(f.Mux, r)
	require.Equal(t, http.StatusUnprocessableEntity, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "github-token")
	assert.NotContains(t, rec.Body.String(), "ghp_abcdef", "findings never carry the matched text")
	// The zip export is unchanged: it does not scan.
	exportGroupAs(t, f, "vault", "zip")

	archive := exportGroupAs(t, f, "vault", "paf&allow_flagged=true")
	a, err := paf.Open(bytes.NewReader(archive), int64(len(archive)), paf.Options{Purpose: paf.PurposeImport})
	require.NoError(t, err)
	p := a.Privacy()
	require.NotNil(t, p)
	require.NotNil(t, p.Scan)
	assert.True(t, p.Scan.Allowed)
	require.NotEmpty(t, p.Scan.Findings)
	assert.Equal(t, "github-token", p.Scan.Findings[0].Kind)

	r = agentd.AsHumanPeer(httptest.NewRequest(http.MethodPost, "/v1/groups/import/inspect?as=vault2", bytes.NewReader(archive)))
	rec = testharness.Serve(f.Mux, r)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var insp struct {
		Findings []paf.Finding `json:"findings"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &insp))
	require.NotEmpty(t, insp.Findings)
	assert.Equal(t, "github-token", insp.Findings[0].Kind)
	assert.Contains(t, insp.Findings[0].Locations[0], "ext/tclaude/group-state.json")
}
