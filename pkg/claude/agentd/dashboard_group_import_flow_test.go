package agentd_test

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// Flow coverage for the dashboard group-import surface — the "⤒ import"
// button on the Groups page. The dashboard cannot stream a raw body the
// way the CLI does, so it posts a multipart/form-data upload to two new
// endpoints, both of which funnel through the SAME permission-checked
// handlers the /v1 CLI routes use:
//
//	POST /api/groups/import          → handleGroupImport
//	POST /api/groups/import/inspect  → handleGroupImportInspect (dry run)
//
// These scenarios drive the real dashboard mux (BuildDashboardHandlerForTest)
// so the cookie auth + asDashboardHumanPeer wrap + requirePermission gate
// all run exactly as in production.

// importInspectionResult mirrors the daemon's importInspection JSON —
// the dry-run analysis the dashboard preview panel renders. The
// production type is unexported, so the test decodes into its own copy.
type importInspectionResult struct {
	SourceGroup     string `json:"source_group"`
	FormatVersion   int    `json:"format_version"`
	SourceHome      string `json:"source_home"`
	SourceOS        string `json:"source_os"`
	ExportedAt      string `json:"exported_at"`
	AgentCount      int    `json:"agent_count"`
	MessageCount    int    `json:"message_count"`
	ConvCount       int    `json:"conv_count"`
	MissingConvs    int    `json:"missing_convs"`
	TargetName      string `json:"target_name"`
	TargetNameValid bool   `json:"target_name_valid"`
	TargetNameError string `json:"target_name_error"`
	GroupNameTaken  bool   `json:"group_name_taken"`
	ConvCollisions  []struct {
		ConvID string `json:"conv_id"`
		Title  string `json:"title"`
	} `json:"conv_collisions"`
}

// dashboardImportUpload builds the multipart/form-data POST a browser
// issues for an import or its preview: an "archive" file part plus the
// "into" / "as" form fields (each omitted when empty). It mirrors the
// FormData the dashboard's fetch() assembles.
func dashboardImportUpload(t *testing.T, path string, archive []byte, into, as string) *http.Request {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if into != "" {
		require.NoError(t, mw.WriteField("into", into))
	}
	if as != "" {
		require.NoError(t, mw.WriteField("as", as))
	}
	fw, err := mw.CreateFormFile("archive", "group.zip")
	require.NoError(t, err)
	_, err = fw.Write(archive)
	require.NoError(t, err)
	require.NoError(t, mw.Close())

	r := httptest.NewRequest(http.MethodPost, path, &buf)
	r.Header.Set("Content-Type", mw.FormDataContentType())
	return r
}

// TestDashboardGroupImport_UploadRecreatesGroup is the core happy path:
// a group is exported, the machine is wiped clean, and the archive is
// uploaded back through the dashboard's multipart import endpoint — the
// group, its members and their roles all return.
//
// Reaching HTTP 200 here also pins the permission wiring: the dashboard
// route wraps the cookie-authed request with asDashboardHumanPeer before
// calling the shared, permission-checked handleGroupImport. Without that
// wrap requirePermission would see PID==0 and 401 — exactly the guard
// TestDashboardGroupExport relies on for the export route.
func TestDashboardGroupImport_UploadRecreatesGroup(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)
	home := f.World.HomeDir
	dash := agentd.BuildDashboardHandlerForTest()
	const aConv = "a1a1a1a1-1111-2222-3333-444444444444"
	const bConv = "b2b2b2b2-1111-2222-3333-444444444444"
	const srcCwd = "/tmp/dash-import-src"

	f.HaveConvWithTitle(aConv, "alice")
	f.HaveConvWithTitle(bConv, "bob")
	f.HaveAliveSession(aConv, "lbl-a", "tmux-dgi-a", srcCwd)
	f.HaveAliveSession(bConv, "lbl-b", "tmux-dgi-b", srcCwd)
	require.NoError(t, f.World.CCs.GetByConvID(aConv).WriteCustomTitle("alice"))
	require.NoError(t, f.World.CCs.GetByConvID(aConv).WriteUserTurn("editing things"))
	require.NoError(t, f.World.CCs.GetByConvID(bConv).WriteCustomTitle("bob"))

	src := f.HaveGroup("team")
	f.HaveMemberWithRole("team", aConv, "lead")
	f.HaveMemberWithRole("team", bConv, "worker")
	require.NoError(t, db.AddAgentGroupOwner(src.ID, aConv, "test"))

	archive := exportGroup(t, f, "team")
	require.NotEmpty(t, archive)
	wipeForFreshImport(t, "team", []string{aConv, bConv}, srcCwd, home)

	// Preview first (what the dashboard does the moment the .zip is
	// picked): the machine is clean, so the dry run must report no
	// collisions and a free, valid target name.
	const dstCwd = "/tmp/dash-import-dst"
	insRec := testharness.Serve(dash, dashboardImportUpload(t,
		"/api/groups/import/inspect", archive, "", ""))
	require.Equal(t, http.StatusOK, insRec.Code, "inspect: body=%s", insRec.Body.String())
	var ins importInspectionResult
	require.NoError(t, json.Unmarshal(insRec.Body.Bytes(), &ins))
	assert.Equal(t, "team", ins.SourceGroup)
	assert.Equal(t, 2, ins.AgentCount)
	assert.True(t, ins.TargetNameValid, "exported name is a valid group name")
	assert.False(t, ins.GroupNameTaken, "machine was wiped — the name is free")
	assert.Empty(t, ins.ConvCollisions, "machine was wiped — no conv-id collides")

	// Commit the import through the dashboard's multipart endpoint.
	impRec := testharness.Serve(dash, dashboardImportUpload(t,
		"/api/groups/import", archive, dstCwd, ""))
	require.Equal(t, http.StatusOK, impRec.Code, "import: body=%s", impRec.Body.String())
	var res importResult
	require.NoError(t, json.Unmarshal(impRec.Body.Bytes(), &res))
	assert.Equal(t, "team", res.Group)
	assert.Equal(t, 2, res.AgentCount)
	assert.Empty(t, res.ConvRemaps, "no local collision — conv-ids preserved")

	// The group and its members are back, verified at the same surface
	// `tclaude agent groups members` reads.
	g, err := db.GetAgentGroupByName("team")
	require.NoError(t, err)
	require.NotNil(t, g, "group recreated by the dashboard import")
	members, err := db.ListAgentGroupMembers(g.ID)
	require.NoError(t, err)
	roles := map[string]string{}
	for _, m := range members {
		roles[m.ConvID] = m.Role
	}
	assert.Equal(t, "lead", roles[aConv])
	assert.Equal(t, "worker", roles[bConv])

	owners, err := db.ListAgentGroupOwners(g.ID)
	require.NoError(t, err)
	require.Len(t, owners, 1)
	assert.Equal(t, aConv, owners[0].ConvID, "owner restored via the dashboard import")
}

// TestDashboardGroupImport_InspectReportsCollisionsWithoutWriting drives
// the dry-run endpoint against a group that still exists locally: the
// group name collides and every conv-id collides. The inspection must
// report both — and write absolutely nothing.
func TestDashboardGroupImport_InspectReportsCollisionsWithoutWriting(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)
	dash := agentd.BuildDashboardHandlerForTest()
	const aConv = "c3c3c3c3-1111-2222-3333-444444444444"
	const bConv = "d4d4d4d4-1111-2222-3333-444444444444"
	const srcCwd = "/tmp/dash-import-live"

	f.HaveConvWithTitle(aConv, "alice")
	f.HaveConvWithTitle(bConv, "bob")
	f.HaveAliveSession(aConv, "lbl-a", "tmux-dgi2-a", srcCwd)
	f.HaveAliveSession(bConv, "lbl-b", "tmux-dgi2-b", srcCwd)
	require.NoError(t, f.World.CCs.GetByConvID(aConv).WriteCustomTitle("alice"))
	require.NoError(t, f.World.CCs.GetByConvID(bConv).WriteCustomTitle("bob"))

	f.HaveGroup("team")
	f.HaveMember("team", aConv)
	f.HaveMember("team", bConv)

	archive := exportGroup(t, f, "team")

	// Preview with NO --as: the source name "team" is still taken and
	// both conv-ids still exist locally.
	rec := testharness.Serve(dash, dashboardImportUpload(t,
		"/api/groups/import/inspect", archive, "", ""))
	require.Equal(t, http.StatusOK, rec.Code, "inspect: body=%s", rec.Body.String())
	var ins importInspectionResult
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &ins))
	assert.Equal(t, "team", ins.SourceGroup)
	assert.Equal(t, "team", ins.TargetName)
	assert.Equal(t, 2, ins.AgentCount)
	assert.Equal(t, 1, ins.FormatVersion)
	assert.NotEmpty(t, ins.SourceOS, "the manifest records the source OS")
	assert.True(t, ins.TargetNameValid)
	assert.True(t, ins.GroupNameTaken, "the exported name 'team' already exists locally")
	require.Len(t, ins.ConvCollisions, 2, "both conv-ids already exist locally")
	collided := map[string]bool{}
	for _, c := range ins.ConvCollisions {
		collided[c.ConvID] = true
		assert.NotEmpty(t, c.Title, "a collision carries the agent's title")
	}
	assert.True(t, collided[aConv] && collided[bConv], "both members are flagged as collisions")

	// Preview WITH --as=team-copy: a free name clears the group-name
	// collision; the conv-id collisions are independent of the name and
	// still report.
	rec = testharness.Serve(dash, dashboardImportUpload(t,
		"/api/groups/import/inspect", archive, "", "team-copy"))
	require.Equal(t, http.StatusOK, rec.Code, "inspect --as: body=%s", rec.Body.String())
	var insAs importInspectionResult
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &insAs))
	assert.Equal(t, "team-copy", insAs.TargetName)
	assert.True(t, insAs.TargetNameValid)
	assert.False(t, insAs.GroupNameTaken, "'team-copy' is a free name")
	assert.Len(t, insAs.ConvCollisions, 2, "conv-id collisions do not depend on the group name")

	// The dry run wrote nothing: still exactly one "team" group, no
	// second group from the --as preview, and no import row in the
	// transfer log (only the earlier export).
	groups, err := db.ListAgentGroups()
	require.NoError(t, err)
	teamCount := 0
	for _, g := range groups {
		assert.NotEqual(t, "team-copy", g.Name, "the dry run must not create the --as group")
		if g.Name == "team" {
			teamCount++
		}
	}
	assert.Equal(t, 1, teamCount, "the dry run must not duplicate the group")

	log, err := db.ListTransferLog(0)
	require.NoError(t, err)
	for _, e := range log {
		assert.NotEqual(t, db.TransferKindImport, e.Kind,
			"a dry run must never record an import in the transfer log")
	}
}

// TestDashboardGroupImport_RejectsMalformedUpload pins that a corrupt or
// non-archive upload is refused cleanly with a 400 at BOTH the preview
// and the commit step — the dashboard surfaces the error and blocks the
// confirm rather than letting the human walk into a failing import.
func TestDashboardGroupImport_RejectsMalformedUpload(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	newFlow(t) // stands up the test DB + mocks; no Flow surface needed here
	dash := agentd.BuildDashboardHandlerForTest()
	junk := []byte("this is definitely not a zip archive")

	// Preview rejects it — the dashboard shows the error, no preview.
	rec := testharness.Serve(dash, dashboardImportUpload(t,
		"/api/groups/import/inspect", junk, "", ""))
	assert.Equal(t, http.StatusBadRequest, rec.Code,
		"a malformed archive must be rejected by the preview: body=%s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "error", "the rejection carries an error message")

	// The commit endpoint rejects it too (into supplied so the failure
	// is the archive, not a missing field).
	rec = testharness.Serve(dash, dashboardImportUpload(t,
		"/api/groups/import", junk, "/tmp/dash-import-junk", ""))
	assert.Equal(t, http.StatusBadRequest, rec.Code,
		"a malformed archive must be rejected by the import: body=%s", rec.Body.String())

	// A request with no "archive" file part at all is a clean 400, not a
	// panic — the daemon must tolerate a malformed multipart form.
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	require.NoError(t, mw.WriteField("into", "/tmp/dash-import-junk"))
	require.NoError(t, mw.Close())
	noFile := httptest.NewRequest(http.MethodPost, "/api/groups/import", &buf)
	noFile.Header.Set("Content-Type", mw.FormDataContentType())
	rec = testharness.Serve(dash, noFile)
	assert.Equal(t, http.StatusBadRequest, rec.Code,
		"an upload with no archive part must be a clean 400: body=%s", rec.Body.String())

	// Nothing was created by any of the rejected requests.
	groups, err := db.ListAgentGroups()
	require.NoError(t, err)
	assert.Empty(t, groups, "rejected uploads must not create a group")
}
