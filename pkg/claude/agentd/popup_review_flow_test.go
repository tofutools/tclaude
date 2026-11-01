package agentd_test

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"net/http"
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/agentbundle"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/paf"
	"github.com/tofutools/tclaude/pkg/testutil"
)

func reviewedAgentFile(t *testing.T) []byte {
	t.Helper()
	b := &agentbundle.Bundle{Manifest: agentbundle.Manifest{Format: agentbundle.Format, FormatVersion: 1, Agent: agentbundle.Definition{
		Name: "visitor", Role: "scout", Harness: "claude", Profile: json.RawMessage(`{}`),
		Paths: agentbundle.Paths{Cwd: testutil.CanonicalTempDir(t)}, CarryPermissions: true,
		Permissions: []agentbundle.Permission{{Slug: "self.rename", Effect: "grant", Source: "conversation"}},
	}}}
	b.SetHistory("jsonl", "source-conversation", []byte(`{"text":"history that stays out of the popup"}`+"\n"))
	raw, err := b.Encode()
	require.NoError(t, err)
	return raw
}

// resigned is file under a key this node has never seen.
func resigned(t *testing.T, file []byte) []byte {
	t.Helper()
	a, err := paf.Open(bytes.NewReader(file), int64(len(file)), paf.Options{Purpose: paf.PurposeImport})
	require.NoError(t, err)
	key := paf.SigningKey{Key: ed25519.NewKeyFromSeed(bytes.Repeat([]byte{9}, 32)), KeyID: "reviewed", Signer: &paf.Signer{Kind: "instance"}}
	var out bytes.Buffer
	require.NoError(t, paf.WithPrivacy(a, &out, paf.Privacy{}, paf.Limits{}, paf.Producer{Name: "test"}, key))
	return out.Bytes()
}

// The popup is the review a client asking first is shown, never the archive, and
// the handler still gets the file whole.
func TestAgentImportPopupShowsTheReview(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("receiver")
	f.HaveMember("receiver", "caller")
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	popups, restore := agentd.StubCapturingApprovalForTest(true)
	t.Cleanup(restore)
	file := reviewedAgentFile(t)

	rec := askHumanAs(f, "caller", "/v1/agent-bundle/import?keep_paths=true", "application/zip", file)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), `"visitor"`)
	require.Len(t, *popups, 1, "the review must not raise a popup of its own")
	preview := (*popups)[0]
	assert.Equal(t, "What approving does", preview.Label)
	assert.Contains(t, preview.Body, "Nothing is imported yet")
	assert.Contains(t, preview.Body, "Agent: visitor, on claude")
	assert.Contains(t, preview.Body, "Signature: none (unverified)")
	assert.Contains(t, preview.Body, "Lands without conversation history.")
	assert.Contains(t, preview.Body, "[archive omitted]")
	assert.NotContains(t, preview.Body, "PK", "no archive bytes")
	assert.NotContains(t, preview.Body, "history that stays out of the popup")

	cwd := testutil.CanonicalTempDir(t)
	// Past both gates this file cannot land unstarted, which is not what is tested.
	askHumanAs(f, "caller", "/v1/agent-bundle/import?apply=true&start=false&group=receiver&carry_permissions=true&cwd="+url.QueryEscape(cwd), "application/zip", file)
	require.Len(t, *popups, 3, "carrying permissions is a second gate, which shows the review too")
	for _, popup := range (*popups)[1:] {
		assert.Contains(t, popup.Body, "Approving lands this agent here")
		assert.Contains(t, popup.Body, "Agent: visitor, on claude")
	}

	rec = askHumanAs(f, "caller", "/v1/agent-bundle/import", "application/zip", []byte("not an archive"))
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	assert.Contains(t, (*popups)[len(*popups)-1].Body, "cannot be reviewed")
}

func TestAgentImportPopupShowsASignedPAF(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("receiver")
	f.HaveMember("receiver", "caller")
	const source = "019fe740-43a4-7023-b8ae-1ee64459f2c9"
	f.HaveAliveSession(source, "original", "original-pane", testutil.CanonicalTempDir(t))
	rec := profileReq(t, f, http.MethodGet, "/v1/agent-bundle/export?agent="+source+"&format=paf", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	file := resigned(t, rec.Body.Bytes())

	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	popups, restore := agentd.StubCapturingApprovalForTest(true)
	t.Cleanup(restore)
	rec = askHumanAs(f, "caller", "/v1/agent-bundle/import?keep_paths=true", "application/zip", file)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Len(t, *popups, 1)
	assert.Contains(t, (*popups)[0].Body, "Signature: valid, key reviewed (unknown key)")
	assert.NotContains(t, (*popups)[0].Body, "manifest.json")
}

// Reviewing for a human is not a way past the gate.
func TestAgentImportDeclinedPopupTellsTheCallerNothing(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("receiver")
	f.HaveMember("receiver", "caller")
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	popups, restore := agentd.StubCapturingApprovalForTest(false)
	t.Cleanup(restore)

	rec := askHumanAs(f, "caller", "/v1/agent-bundle/import", "application/zip", reviewedAgentFile(t))
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	require.Len(t, *popups, 1)
	assert.Contains(t, (*popups)[0].Body, "Agent: visitor")
	assert.NotContains(t, rec.Body.String(), "visitor")
}

func TestGroupImportPopupShowsTheReview(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("receiver")
	f.HaveMember("receiver", "caller")
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	popups, restore := agentd.StubCapturingApprovalForTest(true)
	t.Cleanup(restore)
	file := confirmGroupArchive(t, "visiting")

	rec := askHumanAs(f, "caller", "/v1/groups/import/inspect", "application/gzip", file)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Len(t, *popups, 1)
	preview := (*popups)[0]
	assert.Equal(t, "What approving does", preview.Label)
	assert.Contains(t, preview.Body, "Nothing is imported yet")
	assert.Contains(t, preview.Body, "Group: visiting, landing as visiting")
	assert.Contains(t, preview.Body, "Signature: none (unverified)")
	assert.Contains(t, preview.Body, "none of which this caller can keep")
	assert.Contains(t, preview.Body, "env:TOKENISH")
	assert.Contains(t, preview.Body, "[archive omitted]")
	assert.NotContains(t, preview.Body, confirmSecret)

	rec = askHumanAs(f, "caller", "/v1/groups/import?as=landed&into="+url.QueryEscape(testutil.CanonicalTempDir(t)), "application/gzip", file)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Len(t, *popups, 2)
	assert.Contains(t, (*popups)[1].Body, "Approving imports this group here")
	assert.Contains(t, (*popups)[1].Body, "landing as landed")
	g, err := db.GetAgentGroupByName("landed")
	require.NoError(t, err)
	assert.NotNil(t, g, "approval must not cost the handler its body")

	rec = askHumanAs(f, "caller", "/v1/groups/import?as=landed&into="+url.QueryEscape(testutil.CanonicalTempDir(t)), "application/gzip", file)
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	assert.Contains(t, (*popups)[2].Body, "This node refuses this file")

	rec = askHumanAs(f, "caller", "/v1/groups/import?into="+url.QueryEscape(testutil.CanonicalTempDir(t)), "application/gzip", []byte("not an archive"))
	require.NotEqual(t, http.StatusOK, rec.Code)
	assert.Contains(t, (*popups)[3].Body, "cannot be reviewed")
}

func TestGroupImportPopupShowsASignedPAF(t *testing.T) {
	f := newFlow(t)
	const conv = "cccccccc-1111-2222-3333-555555555555"
	f.HaveConvWithTitle(conv, "carol")
	f.HaveAliveSession(conv, "lbl-c", "tmux-c", f.TestCwd("work"))
	f.HaveGroup("crew")
	f.HaveMemberWithRole("crew", conv, "lead")
	f.HaveGroup("receiver")
	f.HaveMember("receiver", "caller")
	file := resigned(t, exportGroupAs(t, f, "crew", "paf"))

	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	popups, restore := agentd.StubCapturingApprovalForTest(true)
	t.Cleanup(restore)
	rec := askHumanAs(f, "caller", "/v1/groups/import/inspect?as=crew2", "application/zip", file)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Len(t, *popups, 1)
	assert.Contains(t, (*popups)[0].Body, "archive:        group PAF")
	assert.Contains(t, (*popups)[0].Body, "Signature: valid, key reviewed (unknown key)")
	assert.NotContains(t, (*popups)[0].Body, "manifest.json")
}
