package agentd_test

import (
	"net/url"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// tcl-mzx9jm: archived operator mail (from_conv "") is kept, since genuine
// archives carry it. But an agent's inbox reads an empty sender as "From:
// human operator", so a restricted peer's file could forge this node's
// human's word. A restricted peer's import lands it from a fresh, unnamed
// sender instead; a human import keeps it.

// operatorMailSquad seeds squad with one agent and one message to it from the
// operator, and returns the archive.
func operatorMailSquad(t *testing.T, h *peerProcessHarness) []byte {
	t.Helper()
	squad := h.f.HaveGroup("squad")
	conv := uuid.NewString()
	havePerformerAgent(t, h.f, "squad", conv, "sq-dev", "dev", "offline")
	_, err := db.InsertAgentMessage(&db.AgentMessage{GroupID: squad.ID, FromConv: "", ToConv: conv, Subject: "from-operator", Body: "ship it", OperatorAuthored: true})
	require.NoError(t, err)
	return exportSquad(t, h)
}

// landedFrom is the sender of the imported group's "from-operator" message.
func landedFrom(t *testing.T, group string) (fromConv, fromAgent string) {
	t.Helper()
	found := 0
	for _, m := range getMailbox(t, dashHandlerForTest(t), "all") {
		if m.Subject == "from-operator" && m.Group == group {
			found++
			fromConv, fromAgent = m.FromConv, m.FromAgent
		}
	}
	require.Equal(t, 1, found, "the operator's message landed in %s", group)
	return fromConv, fromAgent
}

func TestPeerGroupImportLandsOperatorMailFromAnUnnamedSender(t *testing.T) {
	h := newPeerProcessHarness(t)
	archive := operatorMailSquad(t, h)
	for _, slug := range []string{agentd.PermGroupsImport, agentd.PermNodeGroupsCreate, agentd.PermGroupsMembersSpawn, agentd.PermGroupsMembersSpawnCwd} {
		h.grant(t, slug, "")
	}
	inspectCode, inspect := h.importAs(t, archive, "/api/groups/import/inspect?"+url.Values{"into": {t.TempDir()}, "as": {"squad2"}}.Encode())
	require.Equal(t, 200, inspectCode, inspect)
	assert.Contains(t, inspect, "operator", "the preview says the operator's mail lands from another sender: %s", inspect)

	code, body := h.importAs(t, archive, "/api/groups/import?"+url.Values{"into": {t.TempDir()}, "as": {"squad2"}}.Encode())
	require.Equal(t, 200, code, body)
	from, agentID := landedFrom(t, "squad2")
	assert.NotEmpty(t, from, "a peer's file does not speak as this node's operator: inbox read shows an empty sender as the human operator")
	assert.Empty(t, agentID, "the fresh sender is no agent")
	assert.True(t, strings.Contains(body, "1 archived message") && strings.Contains(body, "operator"), "the reply lists it: %s", body)
	assert.NotContains(t, body, "ship it", "no message text in the warning")
}

func TestGroupImport_HumanKeepsArchivedOperatorMail(t *testing.T) {
	h := newPeerProcessHarness(t)
	archive := operatorMailSquad(t, h)
	res := importArchiveOK(t, h.f, archive, t.TempDir(), "squad2")
	from, _ := landedFrom(t, "squad2")
	assert.Empty(t, from, "this node's human imports genuine operator history as the operator's")
	assert.NotContains(t, strings.Join(res.FileWarnings, "\n"), "operator")
}
