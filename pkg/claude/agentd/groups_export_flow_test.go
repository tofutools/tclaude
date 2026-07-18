package agentd_test

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/convops"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/groupexport"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// importResult mirrors the daemon's importResponse JSON.
type importResult struct {
	Group          string            `json:"group"`
	GroupID        int64             `json:"group_id"`
	TargetDir      string            `json:"target_dir"`
	AgentCount     int               `json:"agent_count"`
	MessageCount   int               `json:"message_count"`
	ConvRemaps     map[string]string `json:"conv_remaps"`
	Retitled       map[string]string `json:"retitled"`
	SkippedAliases []string          `json:"skipped_head_aliases"`
	FileWarnings   []string          `json:"file_warnings"`
}

// exportGroup drives GET /v1/groups/{name}/export as the human peer and
// returns the raw .zip bytes.
func exportGroup(t *testing.T, f *testharness.Flow, group string) []byte {
	t.Helper()
	r := agentd.AsHumanPeer(httptest.NewRequest(http.MethodGet,
		"/v1/groups/"+url.PathEscape(group)+"/export", nil))
	rec := testharness.Serve(f.Mux, r)
	require.Equal(t, http.StatusOK, rec.Code,
		"export %q: body=%s", group, rec.Body.String())
	return rec.Body.Bytes()
}

// importArchive drives POST /v1/groups/import as the human peer. as may
// be empty. It returns the recorder so error-path tests can inspect the
// status; importArchiveOK is the success-asserting wrapper.
func importArchive(f *testharness.Flow, archive []byte, into, as string) *httptest.ResponseRecorder {
	path := "/v1/groups/import?into=" + url.QueryEscape(into)
	if as != "" {
		path += "&as=" + url.QueryEscape(as)
	}
	r := agentd.AsHumanPeer(httptest.NewRequest(http.MethodPost, path, bytes.NewReader(archive)))
	return testharness.Serve(f.Mux, r)
}

func importArchiveOK(t *testing.T, f *testharness.Flow, archive []byte, into, as string) importResult {
	t.Helper()
	rec := importArchive(f, archive, into, as)
	require.Equal(t, http.StatusOK, rec.Code, "import: body=%s", rec.Body.String())
	var out importResult
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out), "decode import: %s", rec.Body.String())
	return out
}

// convJSONLPath is where a conv's .jsonl lives on disk for a given cwd.
func convJSONLPath(home, cwd, convID string) string {
	return filepath.Join(home, ".claude", "projects",
		convops.PathToProjectDir(cwd), convID+".jsonl")
}

// TestGroupExportImport_RoundTripPreservesEverything is the core safety
// net: a group with members, roles, an owner, grant+deny permissions and
// real conversation .jsonl files is exported, the source machine is then
// wiped clean, and the archive is imported back. Every piece must return
// intact, and — because nothing collides on the now-clean machine — the
// original conv-ids are preserved.
func TestGroupExportImport_RoundTripPreservesEverything(t *testing.T) {
	f := newFlow(t)
	home := f.World.HomeDir
	const aConv = "aaaaaaaa-1111-2222-3333-444444444444"
	const bConv = "bbbbbbbb-1111-2222-3333-444444444444"
	srcCwd := f.TestCwd("work")

	f.HaveConvWithTitle(aConv, "alice")
	f.HaveConvWithTitle(bConv, "bob")
	f.HaveAliveSession(aConv, "lbl-a", "tmux-a", srcCwd)
	f.HaveAliveSession(bConv, "lbl-b", "tmux-b", srcCwd)
	// Put the titles + a path-bearing user turn into the .jsonl itself,
	// the way a real /rename and a real turn would — so the export
	// carries genuine content to round-trip.
	ccA := f.World.CCs.GetByConvID(aConv)
	require.NotNil(t, ccA)
	require.NoError(t, ccA.WriteCustomTitle("alice"))
	require.NoError(t, ccA.WriteUserTurn("editing things"))
	ccB := f.World.CCs.GetByConvID(bConv)
	require.NotNil(t, ccB)
	require.NoError(t, ccB.WriteCustomTitle("bob"))

	src := f.HaveGroup("team")
	f.HaveMemberWithRole("team", aConv, "lead")
	f.HaveMemberWithRole("team", bConv, "worker")
	require.NoError(t, db.AddAgentGroupOwner(src.ID, aConv, "test"))
	require.NoError(t, db.SetAgentPermissionOverride(aConv, "groups.spawn", db.PermEffectGrant, "test"))
	require.NoError(t, db.SetAgentPermissionOverride(bConv, "self.rename", db.PermEffectDeny, "test"))
	// A parent/child message pair so message + parent_id remap is exercised.
	parentID, err := db.InsertAgentMessage(&db.AgentMessage{
		GroupID: src.ID, FromConv: aConv, ToConv: bConv, Body: "parent",
	})
	require.NoError(t, err)
	_, err = db.InsertAgentMessage(&db.AgentMessage{
		GroupID: src.ID, FromConv: bConv, ToConv: aConv, Body: "reply", ParentID: parentID,
	})
	require.NoError(t, err)

	archive := exportGroup(t, f, "team")
	require.NotEmpty(t, archive)

	// Capture the source .jsonl content, then wipe the machine clean.
	srcContentA, err := os.ReadFile(convJSONLPath(home, srcCwd, aConv))
	require.NoError(t, err)
	assert.Contains(t, string(srcContentA), srcCwd, "fixture .jsonl carries the source cwd")
	wipeForFreshImport(t, "team", []string{aConv, bConv}, srcCwd, home)

	// Import into a fresh directory.
	dstCwd := f.TestCwd("restored")
	res := importArchiveOK(t, f, archive, dstCwd, "")
	assert.Equal(t, "team", res.Group)
	assert.Equal(t, 2, res.AgentCount)
	assert.Equal(t, 2, res.MessageCount)
	assert.Empty(t, res.ConvRemaps, "no local collision — conv-ids must be preserved")

	// Group + membership + roles.
	g, err := db.GetAgentGroupByName("team")
	require.NoError(t, err)
	require.NotNil(t, g, "group restored")
	members, err := db.ListAgentGroupMembers(g.ID)
	require.NoError(t, err)
	roles := map[string]string{}
	for _, m := range members {
		roles[m.ConvID] = m.Role
	}
	assert.Equal(t, "lead", roles[aConv])
	assert.Equal(t, "worker", roles[bConv])

	// Owner status.
	owners, err := db.ListAgentGroupOwners(g.ID)
	require.NoError(t, err)
	require.Len(t, owners, 1)
	assert.Equal(t, aConv, owners[0].ConvID, "owner restored as the same conv-id")

	// Messages keep their stable actor refs across import (JOH-27 PR3a): the
	// imported rows carry from_agent/to_agent resolved to the imported agents,
	// not left empty — the same denormalisation the send path and v76 backfill
	// produce.
	agA, err := db.GetAgentByConv(aConv)
	require.NoError(t, err)
	require.NotNil(t, agA, "agent restored for aConv")
	agB, err := db.GetAgentByConv(bConv)
	require.NoError(t, err)
	require.NotNil(t, agB, "agent restored for bConv")
	toB, err := db.ListAgentMessagesForConv(bConv, 10) // the parent (a→b)
	require.NoError(t, err)
	require.NotEmpty(t, toB, "the parent message survives import")
	assert.Equal(t, agA.AgentID, toB[0].FromAgent, "imported message keeps the sender's stable agent_id")
	assert.Equal(t, agB.AgentID, toB[0].ToAgent, "imported message keeps the recipient's stable agent_id")

	// Permissions — both the grant and the deny.
	permA, err := db.ListAgentPermissionOverridesForConv(aConv)
	require.NoError(t, err)
	assert.Equal(t, db.PermEffectGrant, permA["groups.spawn"], "grant override restored")
	permB, err := db.ListAgentPermissionOverridesForConv(bConv)
	require.NoError(t, err)
	assert.Equal(t, db.PermEffectDeny, permB["self.rename"], "deny override restored")

	// .jsonl content restored at the new location, with the source cwd
	// rewritten to the import target and no stale source path left.
	// res.TargetDir is the daemon's absolute-resolved --into, so reading
	// and asserting through it stays correct on every OS.
	restored, err := os.ReadFile(convJSONLPath(home, res.TargetDir, aConv))
	require.NoError(t, err, "imported .jsonl should exist at the target project dir")
	assert.Contains(t, string(restored), res.TargetDir, "cwd rewritten to the import target")
	assert.NotContains(t, string(restored), srcCwd, "no stale source cwd remains in the .jsonl")
	assert.Contains(t, string(restored), "editing things", "conversation content preserved")

	// The custom-title turn in the .jsonl means a conv_index scan (run by
	// the importer) resolves the agent's name again.
	if row, _ := db.GetConvIndex(aConv); assert.NotNil(t, row) {
		assert.Equal(t, "alice", row.CustomTitle, "title round-trips via the .jsonl")
	}

	// Both the export and the import are recorded in the audit log,
	// readable via GET /v1/groups/transfers.
	tr := agentd.AsHumanPeer(httptest.NewRequest(http.MethodGet, "/v1/groups/transfers", nil))
	trRec := testharness.Serve(f.Mux, tr)
	require.Equal(t, http.StatusOK, trRec.Code, "transfers: body=%s", trRec.Body.String())
	var log []struct {
		Kind         string `json:"kind"`
		ResultGroup  string `json:"result_group"`
		AgentCount   int    `json:"agent_count"`
		MessageCount int    `json:"message_count"`
	}
	require.NoError(t, json.Unmarshal(trRec.Body.Bytes(), &log))
	kinds := map[string]bool{}
	for _, e := range log {
		kinds[e.Kind] = true
	}
	assert.True(t, kinds["export"], "the export is in the transfer log")
	assert.True(t, kinds["import"], "the import is in the transfer log")
}

// TestGroupImport_TildeIntoExpandsToHome pins the import target's tilde
// handling: importing into "~/sub" must land under the human's home
// directory, not a literal "~" segment beneath the daemon's cwd. The
// spawn, group-default and worktree-picker paths all expand tilde, so the
// import "into" path must match them. Before the fix, filepath.Abs("~/x")
// produced "<daemon-cwd>/~/x" and the imported agents were stranded in an
// unreachable directory.
func TestGroupImport_TildeIntoExpandsToHome(t *testing.T) {
	f := newFlow(t)
	home := f.World.HomeDir // the testharness World points HOME at its tmp home
	const aConv = "eeeeeeee-1111-2222-3333-444444444444"
	srcCwd := f.TestCwd("tilde-src")

	f.HaveConvWithTitle(aConv, "alice")
	f.HaveAliveSession(aConv, "lbl-a", "tmux-tilde-a", srcCwd)
	ccA := f.World.CCs.GetByConvID(aConv)
	require.NotNil(t, ccA)
	require.NoError(t, ccA.WriteCustomTitle("alice"))
	require.NoError(t, ccA.WriteUserTurn("editing things"))

	f.HaveGroup("team")
	f.HaveMember("team", aConv)

	archive := exportGroup(t, f, "team")
	require.NotEmpty(t, archive)
	wipeForFreshImport(t, "team", []string{aConv}, srcCwd, home)

	// Import into a tilde path: the daemon must expand "~" to $HOME.
	res := importArchiveOK(t, f, archive, "~/imported-tilde", "")

	wantDir := filepath.Join(home, "imported-tilde")
	assert.Equal(t, wantDir, res.TargetDir,
		"import target must expand ~ to the home directory")
	assert.NotContains(t, res.TargetDir, "~",
		"no literal tilde may survive into the resolved target dir")

	// The imported .jsonl actually lands at the expanded location, and its
	// rewritten cwd points there rather than at any literal "~".
	restored, err := os.ReadFile(convJSONLPath(home, res.TargetDir, aConv))
	require.NoError(t, err, "imported .jsonl should exist under the expanded home path")
	assert.Contains(t, string(restored), wantDir, "cwd rewritten to the expanded target")
}

// TestGroupExportImport_SameMachineReimportRemaps imports a group that
// still exists locally. Every conv-id collides, so each is minted fresh
// and its agent retitled "-i-N"; every foreign key is remapped; and the
// ORIGINAL group is left completely untouched.
func TestGroupExportImport_SameMachineReimportRemaps(t *testing.T) {
	f := newFlow(t)
	home := f.World.HomeDir
	const aConv = "cccccccc-1111-2222-3333-444444444444"
	const bConv = "dddddddd-1111-2222-3333-444444444444"
	srcCwd := f.TestCwd("live")

	f.HaveConvWithTitle(aConv, "alice")
	f.HaveConvWithTitle(bConv, "bob")
	f.HaveAliveSession(aConv, "lbl-a", "tmux-a", srcCwd)
	f.HaveAliveSession(bConv, "lbl-b", "tmux-b", srcCwd)
	require.NoError(t, f.World.CCs.GetByConvID(aConv).WriteCustomTitle("alice"))
	require.NoError(t, f.World.CCs.GetByConvID(bConv).WriteCustomTitle("bob"))

	src := f.HaveGroup("team")
	f.HaveMemberWithRole("team", aConv, "lead")
	f.HaveMemberWithRole("team", bConv, "worker")
	require.NoError(t, db.SetAgentPermissionOverride(aConv, "groups.spawn", db.PermEffectGrant, "test"))

	archive := exportGroup(t, f, "team")
	srcContentA, err := os.ReadFile(convJSONLPath(home, srcCwd, aConv))
	require.NoError(t, err)

	// The group name is taken, so --as is required; every conv-id is also
	// taken, so every one must be remapped.
	res := importArchiveOK(t, f, archive, f.TestCwd("copy"), "team-copy")
	assert.Equal(t, "team-copy", res.Group)
	require.Len(t, res.ConvRemaps, 2, "both conv-ids collided and were remapped")
	require.Len(t, res.Retitled, 2)
	for _, fresh := range res.ConvRemaps {
		assert.NotEqual(t, aConv, fresh)
		assert.NotEqual(t, bConv, fresh)
		assert.Contains(t, res.Retitled[fresh], "-i-", "remapped agent gets an -i-N title")
	}

	// The new group holds the FRESH conv-ids.
	newG, err := db.GetAgentGroupByName("team-copy")
	require.NoError(t, err)
	require.NotNil(t, newG)
	newMembers, err := db.ListAgentGroupMembers(newG.ID)
	require.NoError(t, err)
	require.Len(t, newMembers, 2)
	for _, m := range newMembers {
		assert.NotEqual(t, aConv, m.ConvID, "new member must not reuse a source conv-id")
		assert.NotEqual(t, bConv, m.ConvID)
		assert.Equal(t, res.ConvRemaps[remapSource(res.ConvRemaps, m.ConvID)], m.ConvID)
	}
	// The permission grant followed conv-id A to its fresh id.
	freshA := res.ConvRemaps[aConv]
	require.NotEmpty(t, freshA)
	permFresh, err := db.ListAgentPermissionOverridesForConv(freshA)
	require.NoError(t, err)
	assert.Equal(t, db.PermEffectGrant, permFresh["groups.spawn"],
		"permission remapped onto the fresh conv-id")

	// The ORIGINAL group is untouched.
	origMembers, err := db.ListAgentGroupMembers(src.ID)
	require.NoError(t, err)
	origIDs := map[string]bool{}
	for _, m := range origMembers {
		origIDs[m.ConvID] = true
	}
	assert.True(t, origIDs[aConv] && origIDs[bConv], "original group keeps its original members")
	origNow, err := os.ReadFile(convJSONLPath(home, srcCwd, aConv))
	require.NoError(t, err)
	assert.Equal(t, srcContentA, origNow, "original .jsonl left byte-for-byte untouched")

	// The fresh agent's title resolves to the -i-N name via its .jsonl.
	if row, _ := db.GetConvIndex(freshA); assert.NotNil(t, row) {
		assert.Equal(t, res.Retitled[freshA], row.CustomTitle)
	}
}

// TestGroupImport_GroupNameCollisionRefused pins that a name clash is
// refused (not auto-renamed) — a group name is a human-meaningful
// identity, so the human must resolve it with --as.
func TestGroupImport_GroupNameCollisionRefused(t *testing.T) {
	f := newFlow(t)
	const aConv = "eeeeeeee-1111-2222-3333-444444444444"
	f.HaveConvWithTitle(aConv, "alice")
	f.HaveAliveSession(aConv, "lbl-a", "tmux-a", f.TestCwd("x"))
	f.HaveGroup("team")
	f.HaveMember("team", aConv)

	archive := exportGroup(t, f, "team")

	rec := importArchive(f, archive, f.TestCwd("dst"), "")
	assert.Equal(t, http.StatusConflict, rec.Code,
		"import under an existing group name must be refused: body=%s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "--as", "the error tells the human how to resolve it")

	// And no second group was created.
	groups, err := db.ListAgentGroups()
	require.NoError(t, err)
	count := 0
	for _, g := range groups {
		if g.Name == "team" {
			count++
		}
	}
	assert.Equal(t, 1, count, "the refused import created nothing")
}

// TestGroupImport_RejectsMalformedArchive pins that junk uploads are
// rejected cleanly with a 400, importing nothing.
func TestGroupImport_RejectsMalformedArchive(t *testing.T) {
	f := newFlow(t)

	rec := importArchive(f, []byte("this is not a zip archive at all"), f.TestCwd("dst"), "")
	assert.Equal(t, http.StatusBadRequest, rec.Code, "garbage upload: body=%s", rec.Body.String())

	// A structurally valid zip with no manifest is equally rejected.
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create("projects/whatever.jsonl")
	require.NoError(t, err)
	_, _ = w.Write([]byte("{}"))
	require.NoError(t, zw.Close())
	rec = importArchive(f, buf.Bytes(), f.TestCwd("dst"), "")
	assert.Equal(t, http.StatusBadRequest, rec.Code, "no-manifest zip: body=%s", rec.Body.String())
}

// TestGroupImport_RejectsUnsupportedVersion pins that an archive whose
// manifest declares a newer format version is refused — the
// forward-compatibility guard the source-control use case relies on.
func TestGroupImport_RejectsUnsupportedVersion(t *testing.T) {
	f := newFlow(t)

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create("manifest.json")
	require.NoError(t, err)
	_, _ = w.Write([]byte(`{"format_version":9999,"source_group":"team"}`))
	require.NoError(t, zw.Close())

	rec := importArchive(f, buf.Bytes(), f.TestCwd("dst"), "")
	assert.Equal(t, http.StatusBadRequest, rec.Code,
		"a too-new format version must be refused: body=%s", rec.Body.String())
}

// TestGroupImport_CrossHomePathRewrite imports an archive crafted as if
// exported by a different user on a different machine (a foreign home
// dir) and asserts every source path is rewritten out of the .jsonl —
// the "move to another worker machine" requirement.
func TestGroupImport_CrossHomePathRewrite(t *testing.T) {
	f := newFlow(t)
	home := f.World.HomeDir
	const convID = "11111111-aaaa-bbbb-cccc-222222222222"
	const srcHome = "/home/sourceuser"
	const srcCwd = "/home/sourceuser/projects/app"

	jsonl := `{"type":"user","sessionId":"` + convID + `","cwd":"` + srcCwd +
		`","message":{"role":"user","content":"touched ` + srcCwd +
		`/main.go and ` + srcHome + `/.config/tclaude/x"}}` + "\n"

	exp := &groupexport.Export{
		FormatVersion: groupexport.FormatVersion,
		SourceGroup:   "ported",
		SourceHome:    srcHome,
		SourceOS:      "linux",
		Members:       []groupexport.Member{{ConvID: convID, Role: "lead"}},
		Convs: []groupexport.Conv{{
			ConvID:    convID,
			SourceCwd: srcCwd,
			Title:     "ported-agent",
			Content:   []byte(jsonl),
		}},
	}
	archive, err := groupexport.Marshal(exp)
	require.NoError(t, err)

	dstCwd := f.TestCwd("landing/app")
	res := importArchiveOK(t, f, archive, dstCwd, "")
	assert.Equal(t, "ported", res.Group)
	assert.Empty(t, res.ConvRemaps, "a foreign conv-id does not collide locally")

	// res.TargetDir is the daemon's absolute-resolved --into — use it for
	// both the readback path and the rewritten-prefix assertion so the
	// test holds on every OS (Windows filepath.Abs prepends a drive).
	imported, err := os.ReadFile(convJSONLPath(home, res.TargetDir, convID))
	require.NoError(t, err, "imported .jsonl present at the target")
	got := string(imported)
	assert.NotContains(t, got, srcHome, "no trace of the source home dir")
	assert.NotContains(t, got, srcCwd, "no trace of the source cwd")
	assert.Contains(t, got, res.TargetDir+"/main.go", "source cwd rewritten to the import target")
	assert.Contains(t, got, home+"/.config/tclaude/x", "source home rewritten to the local home")
}

func TestGroupImport_MissingConversationLateFileCollisionRollsBack(t *testing.T) {
	f := newFlow(t)
	const convID = "88888888-dead-beef-cafe-000000000000"
	targetCwd := f.TestCwd("missing-race")
	exp := &groupexport.Export{
		FormatVersion: groupexport.FormatVersion,
		SourceGroup:   "missing-race",
		Members:       []groupexport.Member{{ConvID: convID, Role: "lead"}},
		Convs:         []groupexport.Conv{{ConvID: convID, Missing: true}},
	}
	archive, err := groupexport.Marshal(exp)
	require.NoError(t, err)
	dst := convJSONLPath(f.World.HomeDir, targetCwd, convID)
	restore := agentd.SetGroupImportAfterCollisionCheckForTest(func() {
		require.NoError(t, os.MkdirAll(filepath.Dir(dst), 0o755))
		require.NoError(t, os.WriteFile(dst, []byte("late local conversation"), 0o600))
	})
	t.Cleanup(restore)

	rec := importArchive(f, archive, targetCwd, "")
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	got, err := os.ReadFile(dst)
	require.NoError(t, err)
	assert.Equal(t, []byte("late local conversation"), got)
	group, err := db.GetAgentGroupByName("missing-race")
	require.NoError(t, err)
	assert.Nil(t, group, "filesystem collision happens before the DB import")
}

func TestGroupImport_LatePlainConversationIndexCollisionRollsBackAuthority(t *testing.T) {
	f := newFlow(t)
	const convID = "77777777-dead-beef-cafe-000000000000"
	targetCwd := f.TestCwd("index-race")
	foreignPath := f.TestCwd("other-project/77777777-dead-beef-cafe-000000000000.jsonl")
	exp := &groupexport.Export{
		FormatVersion: groupexport.FormatVersion,
		SourceGroup:   "index-race",
		Members:       []groupexport.Member{{ConvID: convID, Role: "lead"}},
		Owners:        []groupexport.Owner{{ConvID: convID, GrantedBy: "human"}},
		Permissions: []groupexport.Permission{{
			ConvID: convID, Slug: "groups.spawn", Effect: db.PermEffectGrant, GrantedBy: "human",
		}},
		SudoGrants: []groupexport.SudoGrant{{
			ConvID: convID, Slug: "human.notify", GrantedAt: "2026-01-01T00:00:00Z",
			ExpiresAt: "2099-01-01T00:00:00Z", GrantedBy: "human",
		}},
		CronJobs: []groupexport.CronJob{{
			ID: 1, Name: "authority", OwnerConv: convID, TargetConv: convID,
			IntervalSeconds: 60, Body: "must not attach", Enabled: 1,
		}},
		Convs: []groupexport.Conv{{ConvID: convID, Missing: true}},
	}
	archive, err := groupexport.Marshal(exp)
	require.NoError(t, err)

	// Deterministically create a non-agent conversation in another project
	// after the daemon's collision preflight. The import transaction must see
	// this SQLite identity and abort before replaying any archived authority.
	restore := agentd.SetGroupImportAfterCollisionCheckForTest(func() {
		require.NoError(t, db.UpsertConvIndex(&db.ConvIndexRow{
			ConvID: convID, ProjectDir: f.TestCwd("other-project"), FullPath: foreignPath,
		}))
	})
	t.Cleanup(restore)

	rec := importArchive(f, archive, targetCwd, "")
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "import conversation collision")

	row, err := db.GetConvIndex(convID)
	require.NoError(t, err)
	require.NotNil(t, row, "late local conversation remains indexed")
	assert.Equal(t, foreignPath, row.FullPath, "import does not rewrite the local identity")
	group, err := db.GetAgentGroupByName("index-race")
	require.NoError(t, err)
	assert.Nil(t, group, "collision rolls back the imported group")
	groups, err := db.ListGroupsForConv(convID)
	require.NoError(t, err)
	assert.Empty(t, groups, "membership and ownership authority are not attached")
	agent, err := db.GetAgentByConv(convID)
	require.NoError(t, err)
	assert.Nil(t, agent, "plain conversation is not enrolled as the imported actor")
	overrides, err := db.ListAgentPermissionOverridesForConv(convID)
	require.NoError(t, err)
	assert.Empty(t, overrides, "permanent authority is not attached")
	sudo, err := db.ListActiveSudoGrants(convID)
	require.NoError(t, err)
	assert.Empty(t, sudo, "sudo authority is not attached")
	jobs, err := db.ListAgentCronJobs()
	require.NoError(t, err)
	for _, job := range jobs {
		assert.NotEqual(t, convID, job.OwnerConv, "scheduled authority is not attached")
	}
	_, err = os.Stat(convJSONLPath(f.World.HomeDir, targetCwd, convID))
	assert.ErrorIs(t, err, os.ErrNotExist, "missing-conversation reservation is cleaned up")
}

func TestGroupImport_PresentConversationSucceedsWithLiveIndexMonitor(t *testing.T) {
	f := newFlow(t)
	const convID = "66666666-dead-beef-cafe-000000000000"
	targetCwd := f.TestCwd("monitor-present")
	dst := convJSONLPath(f.World.HomeDir, targetCwd, convID)
	require.NoError(t, os.MkdirAll(filepath.Dir(dst), 0o755))
	monitor := agentd.StartConvMonitorForTest(t, 5*time.Millisecond)
	if monitor == nil {
		t.Skip("fsnotify watcher unavailable")
	}
	agentd.WaitForConvMonitorStartupForTest(t, monitor)

	exp := &groupexport.Export{
		FormatVersion: groupexport.FormatVersion,
		SourceGroup:   "monitor-present",
		Members:       []groupexport.Member{{ConvID: convID, Role: "lead"}},
		Convs: []groupexport.Conv{{
			ConvID: convID, Content: []byte(`{"type":"summary","summary":"imported"}` + "\n"),
		}},
	}
	archive, err := groupexport.Marshal(exp)
	require.NoError(t, err)
	res := importArchiveOK(t, f, archive, targetCwd, "")
	assert.Equal(t, "monitor-present", res.Group)
	require.Eventually(t, func() bool {
		row, _ := db.GetConvIndex(convID)
		return row != nil && row.FullPath == dst
	}, 3*time.Second, 10*time.Millisecond, "the live monitor indexes the import-owned path")
}

func TestGroupImport_MissingConversationSucceedsWithLiveIndexMonitor(t *testing.T) {
	f := newFlow(t)
	const convID = "55555555-dead-beef-cafe-000000000000"
	targetCwd := f.TestCwd("monitor-missing")
	dst := convJSONLPath(f.World.HomeDir, targetCwd, convID)
	require.NoError(t, os.MkdirAll(filepath.Dir(dst), 0o755))
	monitor := agentd.StartConvMonitorForTest(t, 5*time.Millisecond)
	if monitor == nil {
		t.Skip("fsnotify watcher unavailable")
	}
	agentd.WaitForConvMonitorStartupForTest(t, monitor)

	exp := &groupexport.Export{
		FormatVersion: groupexport.FormatVersion,
		SourceGroup:   "monitor-missing",
		Members:       []groupexport.Member{{ConvID: convID, Role: "lead"}},
		Convs:         []groupexport.Conv{{ConvID: convID, Missing: true}},
	}
	archive, err := groupexport.Marshal(exp)
	require.NoError(t, err)
	res := importArchiveOK(t, f, archive, targetCwd, "")
	assert.Equal(t, "monitor-missing", res.Group)
	require.Len(t, res.FileWarnings, 1)
	_, err = os.Stat(dst)
	assert.ErrorIs(t, err, os.ErrNotExist, "missing reservation is removed after commit")
}

func TestGroupImport_RejectsMalformedMissingConversationIDBeforeFilesystem(t *testing.T) {
	f := newFlow(t)
	exp := &groupexport.Export{
		FormatVersion: groupexport.FormatVersion,
		SourceGroup:   "bad-conv-id",
		Convs:         []groupexport.Conv{{ConvID: "../escape/aaaaaaaa-1111-2222-3333-444444444444", Missing: true}},
	}
	archive, err := groupexport.Marshal(exp)
	require.NoError(t, err)
	rec := importArchive(f, archive, f.TestCwd("bad-conv-id"), "")
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "not a canonical UUID")
	group, err := db.GetAgentGroupByName("bad-conv-id")
	require.NoError(t, err)
	assert.Nil(t, group)
}

func TestGroupImport_RemapsOmittedSuccessionEndpointWithoutRedirectingLocalConv(t *testing.T) {
	f := newFlow(t)
	const localConv = "33333333-dead-beef-cafe-000000000000"
	const importedConv = "22222222-dead-beef-cafe-000000000000"
	localPath := f.TestCwd("local-succession/33333333-dead-beef-cafe-000000000000.jsonl")
	require.NoError(t, db.UpsertConvIndex(&db.ConvIndexRow{
		ConvID: localConv, ProjectDir: f.TestCwd("local-succession"), FullPath: localPath,
	}))
	exp := &groupexport.Export{
		FormatVersion: groupexport.FormatVersion,
		SourceGroup:   "succession-remap",
		Members:       []groupexport.Member{{ConvID: importedConv, Role: "lead"}},
		Convs:         []groupexport.Conv{{ConvID: importedConv, Missing: true}},
		Successions: []groupexport.Succession{{
			OldConvID: localConv, NewConvID: importedConv, Reason: "reincarnate",
		}},
	}
	archive, err := groupexport.Marshal(exp)
	require.NoError(t, err)
	res := importArchiveOK(t, f, archive, f.TestCwd("succession-remap"), "")
	remappedOld := res.ConvRemaps[localConv]
	require.NotEmpty(t, remappedOld, "omitted historical endpoint is collision-remapped")
	assert.NotEqual(t, localConv, remappedOld)
	successor, err := db.GetConvSuccessor(localConv)
	require.NoError(t, err)
	assert.Empty(t, successor, "import cannot redirect the unrelated local conversation")
	successor, err = db.GetConvSuccessor(remappedOld)
	require.NoError(t, err)
	assert.Equal(t, importedConv, successor, "the imported history keeps its remapped edge")
	row, err := db.GetConvIndex(localConv)
	require.NoError(t, err)
	require.NotNil(t, row)
	assert.Equal(t, localPath, row.FullPath)
}

func TestGroupImport_LateOmittedSuccessionCollisionRollsBackAllSideEffects(t *testing.T) {
	f := newFlow(t)
	const lateConv = "11111111-dead-beef-cafe-000000000000"
	const importedConv = "00000000-dead-beef-cafe-000000000001"
	exp := &groupexport.Export{
		FormatVersion: groupexport.FormatVersion,
		SourceGroup:   "succession-race",
		Members:       []groupexport.Member{{ConvID: importedConv, Role: "lead"}},
		Owners:        []groupexport.Owner{{ConvID: importedConv, GrantedBy: "human"}},
		Permissions: []groupexport.Permission{{
			ConvID: importedConv, Slug: "groups.spawn", Effect: db.PermEffectGrant,
		}},
		SudoGrants: []groupexport.SudoGrant{{
			ConvID: importedConv, Slug: "human.notify", GrantedAt: "2026-01-01T00:00:00Z",
			ExpiresAt: "2099-01-01T00:00:00Z",
		}},
		CronJobs: []groupexport.CronJob{{
			ID: 1, Name: "must-rollback", OwnerConv: importedConv, TargetConv: importedConv,
			IntervalSeconds: 60, Body: "no side effect", Enabled: 1,
		}},
		Convs: []groupexport.Conv{{ConvID: importedConv, Missing: true}},
		Successions: []groupexport.Succession{{
			OldConvID: lateConv, NewConvID: importedConv, Reason: "reincarnate",
		}},
	}
	archive, err := groupexport.Marshal(exp)
	require.NoError(t, err)
	restore := agentd.SetGroupImportAfterCollisionCheckForTest(func() {
		require.NoError(t, db.UpsertConvIndex(&db.ConvIndexRow{
			ConvID: lateConv, ProjectDir: f.TestCwd("late-succession"),
			FullPath: f.TestCwd("late-succession/11111111-dead-beef-cafe-000000000000.jsonl"),
		}))
	})
	t.Cleanup(restore)

	rec := importArchive(f, archive, f.TestCwd("succession-race"), "")
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	group, err := db.GetAgentGroupByName("succession-race")
	require.NoError(t, err)
	assert.Nil(t, group)
	successor, err := db.GetConvSuccessor(lateConv)
	require.NoError(t, err)
	assert.Empty(t, successor, "late collision cannot install a redirect")
	agent, err := db.GetAgentByConv(importedConv)
	require.NoError(t, err)
	assert.Nil(t, agent, "actor and membership roll back")
	overrides, err := db.ListAgentPermissionOverridesForConv(importedConv)
	require.NoError(t, err)
	assert.Empty(t, overrides)
	sudo, err := db.ListActiveSudoGrants(importedConv)
	require.NoError(t, err)
	assert.Empty(t, sudo)
	jobs, err := db.ListAgentCronJobs()
	require.NoError(t, err)
	for _, job := range jobs {
		assert.NotEqual(t, importedConv, job.OwnerConv)
	}
}

func TestGroupImport_RejectsMalformedOmittedSuccessionEndpoint(t *testing.T) {
	f := newFlow(t)
	const importedConv = "99999999-dead-beef-cafe-000000000001"
	exp := &groupexport.Export{
		FormatVersion: groupexport.FormatVersion,
		SourceGroup:   "bad-succession-id",
		Convs:         []groupexport.Conv{{ConvID: importedConv, Missing: true}},
		Successions: []groupexport.Succession{{
			OldConvID: "../escape", NewConvID: importedConv,
		}},
	}
	archive, err := groupexport.Marshal(exp)
	require.NoError(t, err)
	rec := importArchive(f, archive, f.TestCwd("bad-succession-id"), "")
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "not a canonical UUID")
	group, err := db.GetAgentGroupByName("bad-succession-id")
	require.NoError(t, err)
	assert.Nil(t, group)
}

// TestGroupImport_FailedImportLeavesNothing forces a mid-import failure
// (an archive with two members sharing one conv-id violates the
// membership primary key) and asserts the system is left exactly as it
// was: no group, no staged files, no audit-log row.
func TestGroupImport_FailedImportLeavesNothing(t *testing.T) {
	f := newFlow(t)
	home := f.World.HomeDir
	const dupConv = "99999999-dead-beef-cafe-000000000000"

	exp := &groupexport.Export{
		FormatVersion: groupexport.FormatVersion,
		SourceGroup:   "doomed",
		SourceHome:    "/home/x",
		SourceOS:      "linux",
		Members: []groupexport.Member{
			{ConvID: dupConv, Role: "one"},
			{ConvID: dupConv, Role: "two"}, // duplicate PK → tx fails
		},
		Convs: []groupexport.Conv{
			{ConvID: dupConv, Content: []byte(`{"type":"summary"}` + "\n")},
		},
	}
	archive, err := groupexport.Marshal(exp)
	require.NoError(t, err)

	rec := importArchive(f, archive, f.TestCwd("doomed"), "")
	require.GreaterOrEqual(t, rec.Code, 400, "the import must fail: body=%s", rec.Body.String())

	// No group.
	g, err := db.GetAgentGroupByName("doomed")
	require.NoError(t, err)
	assert.Nil(t, g, "a failed import must not leave a half-created group")

	// No audit-log row — the transfer-log insert is inside the rolled-back
	// transaction, and nothing was exported in this test.
	entries, err := db.ListTransferLog(0)
	require.NoError(t, err)
	assert.Empty(t, entries, "a failed import must not log a transfer")

	// No staging directory left behind under ~/.claude. The directory
	// must be readable — a skipped loop would let this check pass falsely.
	claudeDir := filepath.Join(home, ".claude")
	dirEntries, err := os.ReadDir(claudeDir)
	require.NoError(t, err, "~/.claude should be readable")
	for _, e := range dirEntries {
		assert.False(t, strings.HasPrefix(e.Name(), ".tclaude-import-staging-"),
			"staging dir %q must have been wiped", e.Name())
	}
}

// remapSource returns the source conv-id that maps to fresh in remap.
func remapSource(remap map[string]string, fresh string) string {
	for src, dst := range remap {
		if dst == fresh {
			return src
		}
	}
	return ""
}

// wipeForFreshImport simulates moving to a clean machine: it deletes the
// group and every trace of the listed convs — actor identity, conv_index,
// permissions and the .jsonl files — so a subsequent import sees no
// collision and preserves the original conv-ids.
func wipeForFreshImport(t *testing.T, group string, convs []string, cwd, home string) {
	t.Helper()
	require.NoError(t, db.DeleteAgentGroup(group))
	for _, c := range convs {
		// Revoke permissions FIRST, while the conv still resolves to its actor.
		// RevokeAllAgentPermissionsForConv looks the actor up through the
		// conv→agent link; DeleteAgentByConvID below only *unlinks* a rotated
		// predecessor generation (it leaves the live actor's permission rows
		// untouched), so a revoke run afterwards would resolve to no actor and
		// silently no-op, stranding actor-scoped rows.
		if _, err := db.RevokeAllAgentPermissionsForConv(c); err != nil {
			t.Fatalf("wipe: revoke permissions %s: %v", c, err)
		}
		// DeleteAgentByConvID tears down the actor + its conv-scoped DB rows
		// (conv_index, sessions, permissions via the actor) — the agents-table
		// successor to deleting the enrollment row (JOH-26 PR3c).
		if _, err := db.DeleteAgentByConvID(c); err != nil {
			t.Fatalf("wipe: delete agent %s: %v", c, err)
		}
		if err := db.DeleteConvIndex(c); err != nil {
			t.Fatalf("wipe: delete conv_index %s: %v", c, err)
		}
		_ = os.Remove(convJSONLPath(home, cwd, c))
	}
}
