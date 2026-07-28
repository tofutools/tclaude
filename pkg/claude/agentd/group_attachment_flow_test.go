package agentd_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

type groupAttachmentResp struct {
	Group         string `json:"group"`
	URL           string `json:"attachment_url"`
	Label         string `json:"attachment_label"`
	LabelOverride string `json:"attachment_label_override"`
	Cleared       bool   `json:"cleared"`
}

func TestGroupAttachment_SetRenderAndClear(t *testing.T) {
	f := newFlow(t)
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f.HaveGroup("alpha")

	const refURL = "https://linear.app/acme/project/platform"
	rec := testharness.Serve(f.Mux, agentd.AsHumanPeer(testharness.JSONRequest(
		t, http.MethodPost, "/v1/groups/alpha/attachment",
		map[string]any{"url": refURL, "label": "Platform project"})))
	require.Equalf(t, http.StatusOK, rec.Code, "set body=%s", rec.Body.String())
	var set groupAttachmentResp
	testharness.DecodeJSON(t, rec, &set)
	assert.Equal(t, refURL, set.URL)
	assert.Equal(t, "Platform project", set.Label)
	assert.Equal(t, "Platform project", set.LabelOverride)

	g, err := db.GetAgentGroupByName("alpha")
	require.NoError(t, err)
	require.NotNil(t, g)
	assert.Equal(t, refURL, g.AttachmentURL)
	assert.Equal(t, "Platform project", g.AttachmentLabel)

	snap := fetchDashSnapshot(t, agentd.BuildDashboardHandlerForTest())
	require.Len(t, snap.Groups, 1)
	assert.Equal(t, refURL, snap.Groups[0].AttachmentURL)
	assert.Equal(t, "Platform project", snap.Groups[0].AttachmentLabel)
	assert.Equal(t, "Platform project", snap.Groups[0].AttachmentLabelOverride)

	rec = testharness.Serve(f.Mux, agentd.AsHumanPeer(testharness.JSONRequest(
		t, http.MethodPost, "/v1/groups/alpha/attachment", map[string]any{"clear": true})))
	require.Equalf(t, http.StatusOK, rec.Code, "clear body=%s", rec.Body.String())
	var cleared groupAttachmentResp
	testharness.DecodeJSON(t, rec, &cleared)
	assert.True(t, cleared.Cleared)
	assert.Empty(t, cleared.URL)

	g, err = db.GetAgentGroupByName("alpha")
	require.NoError(t, err)
	assert.Empty(t, g.AttachmentURL)
	assert.Empty(t, g.AttachmentLabel)
}

func TestGroupAttachment_DerivesLabelAndRejectsUnsafeURL(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")

	rec := testharness.Serve(f.Mux, agentd.AsHumanPeer(testharness.JSONRequest(
		t, http.MethodPost, "/v1/groups/alpha/attachment",
		map[string]any{"url": "https://github.com/tofutools/tclaude/issues/801"})))
	require.Equalf(t, http.StatusOK, rec.Code, "set body=%s", rec.Body.String())
	var set groupAttachmentResp
	testharness.DecodeJSON(t, rec, &set)
	assert.Equal(t, "#801", set.Label)
	assert.Empty(t, set.LabelOverride)

	rec = testharness.Serve(f.Mux, agentd.AsHumanPeer(testharness.JSONRequest(
		t, http.MethodPost, "/v1/groups/alpha/attachment",
		map[string]any{"url": "javascript:alert(1)"})))
	assert.Equal(t, http.StatusBadRequest, rec.Code, "unsafe href must be rejected; body=%s", rec.Body.String())
}

func TestGroupAttachment_OwnerMayWriteButUnrelatedAgentMayNot(t *testing.T) {
	f := newFlow(t)
	const owner = "gatt-aaaa-bbbb-cccc-dddd"
	const stranger = "gatt-eeee-ffff-gggg-hhhh"
	g := f.HaveGroup("alpha")
	f.HaveEnrolledAgent(owner)
	f.HaveEnrolledAgent(stranger)
	require.NoError(t, db.AddAgentGroupOwner(g.ID, owner, "test"))

	rec := testharness.Serve(f.Mux, agentd.AsAgentPeer(testharness.JSONRequest(
		t, http.MethodPost, "/v1/groups/alpha/attachment",
		map[string]any{"url": "https://example.com/project"}), owner))
	require.Equalf(t, http.StatusOK, rec.Code, "owner write body=%s", rec.Body.String())

	rec = testharness.Serve(f.Mux, agentd.AsAgentPeer(testharness.JSONRequest(
		t, http.MethodPost, "/v1/groups/alpha/attachment",
		map[string]any{"url": "https://example.com/other"}), stranger))
	assert.Equal(t, http.StatusForbidden, rec.Code, "unrelated agent must need groups.rename")

	rec = testharness.Serve(f.Mux, agentd.AsAgentPeer(testharness.JSONRequest(
		t, http.MethodGet, "/v1/groups/alpha/attachment", nil), stranger))
	assert.Equal(t, http.StatusOK, rec.Code, "read-only group references are open like groups ls")
}
