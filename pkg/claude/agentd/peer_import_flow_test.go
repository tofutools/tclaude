package agentd_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// importAs stages archive as an upload from the peer and finishes it on path.
func (h *peerProcessHarness) importAs(t *testing.T, archive []byte, path string) (int, string) {
	t.Helper()
	code, body := h.call("POST", "/api/peer-uploads", nil)
	require.Equal(t, 200, code, body)
	var opened struct {
		ID string `json:"id"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &opened))
	view := agentd.PeerViewHandler(h.peer.id.ID())
	for offset := 0; offset < len(archive); offset += 512 << 10 {
		chunk := archive[offset:min(offset+512<<10, len(archive))]
		req := httptest.NewRequest(http.MethodPost, "/api/peer-uploads/chunk?id="+opened.ID+"&offset="+strconv.Itoa(offset), bytes.NewReader(chunk))
		rec := testharness.Serve(view, req)
		require.Equal(t, 200, rec.Code, rec.Body.String())
	}
	return h.finishUpload(t, opened.ID, path, "")
}

// exportSquad is this node's own export of the squad group.
func exportSquad(t *testing.T, h *peerProcessHarness) []byte {
	t.Helper()
	rec := fedHuman(t, h.f, http.MethodGet, "/v1/groups/squad/export", nil)
	require.Equal(t, 200, rec.Code, rec.Body.String())
	return rec.Body.Bytes()
}

// A restricted peer imports a group archive under groups.import and
// node.groups.create, its target directory and landings within its spawn
// grants. It keeps nothing the file asks for, and the landing is in this
// node's audit log.
func TestPeerGroupImportStagesUnderItsGrants(t *testing.T) {
	h := newPeerProcessHarness(t)
	squad := h.f.HaveGroup("squad")
	conv := uuid.NewString()
	havePerformerAgent(t, h.f, "squad", conv, "sq-dev", "dev", "offline")
	require.NoError(t, db.AddAgentGroupOwner(squad.ID, conv, "human"))
	archive := exportSquad(t, h)
	into := t.TempDir()
	path := "/api/groups/import?" + url.Values{"into": {into}, "as": {"crew2"}}.Encode()

	code, body := h.importAs(t, archive, path)
	require.Equal(t, 403, code, body)
	require.Contains(t, body, agentd.PermGroupsImport)
	h.grant(t, agentd.PermGroupsImport, "")
	code, body = h.importAs(t, archive, path)
	require.Equal(t, 403, code, body)
	require.Contains(t, body, agentd.PermNodeGroupsCreate)
	h.grant(t, agentd.PermNodeGroupsCreate, "")
	code, body = h.importAs(t, archive, path)
	require.Equal(t, 403, code, "the target directory is a launch directory: %s", body)
	require.Contains(t, body, agentd.PermGroupsMembersSpawnCwd)
	h.grant(t, agentd.PermGroupsMembersSpawn, "")
	h.grant(t, agentd.PermGroupsMembersSpawnCwd, "")

	code, body = h.importAs(t, archive, path+"&keep_all=true")
	require.Equal(t, 403, code, "keeping is the operator's: %s", body)
	// A member landing with a model the peer may not choose refuses the
	// whole import, naming each such member; the preview lists them too.
	landed := path + "&" + url.Values{"model": {"opus"}}.Encode()
	code, body = h.importAs(t, archive, landed)
	require.Equal(t, 403, code, body)
	var refused struct {
		Members []struct {
			Member string   `json:"member"`
			Needs  []string `json:"needs"`
		} `json:"members"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &refused))
	require.Len(t, refused.Members, 1, body)
	require.Contains(t, refused.Members[0].Needs, agentd.PermGroupsMembersSpawnModel)
	code, body = h.importAs(t, archive, "/api/groups/import/inspect?"+url.Values{"into": {into}, "as": {"crew2"}, "model": {"opus"}}.Encode())
	require.Equal(t, 200, code, body)
	require.Contains(t, body, "peer_refused_members")
	group, err := db.GetAgentGroupByName("crew2")
	require.NoError(t, err)
	require.Nil(t, group, "nothing landed yet")

	code, body = h.importAs(t, archive, path)
	require.Equal(t, 200, code, body)
	group, err = db.GetAgentGroupByName("crew2")
	require.NoError(t, err)
	require.NotNil(t, group)
	owners, err := db.ListAgentGroupOwners(group.ID)
	require.NoError(t, err)
	require.Empty(t, owners, "the file's owner seat dropped")
	entries, err := db.ListAuditLog(db.AuditLogFilter{Verb: "federation.group_import"})
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.Equal(t, "crew2", entries[0].GroupName)
	require.Contains(t, entries[0].Detail, "1 member;")
	require.Contains(t, entries[0].Detail, "1 owner seat")

	// The archive's saved prompts join this node's library.
	require.NoError(t, db.SaveSavedPrompt("", db.SavedPrompt{Name: "house", Body: "be kind"}))
	_, err = db.SetAgentGroupDefaultPrompts("squad", []string{"house"})
	require.NoError(t, err)
	archive = exportSquad(t, h)
	path = "/api/groups/import?" + url.Values{"into": {into}, "as": {"crew3"}}.Encode()
	code, body = h.importAs(t, archive, path)
	require.Equal(t, 403, code, body)
	require.Contains(t, body, agentd.PermGroupsMembersSpawnPrompts)
	h.grant(t, agentd.PermGroupsMembersSpawnPrompts, "")
	code, body = h.importAs(t, archive, path)
	require.Equal(t, 403, code, body)
	require.Contains(t, body, agentd.PermPromptsManage)

	h.grant(t, agentd.PermPromptsManage, "")
	code, body = h.importAs(t, archive, path)
	require.Equal(t, 200, code, body)

	// The gate fails closed: landing options it cannot plan refuse the import.
	code, body = h.importAs(t, archive, "/api/groups/import?"+url.Values{"into": {into}, "as": {"crew4"}, "member_harness": {"nobody"}}.Encode())
	require.Equal(t, 400, code, body)
	code, body = h.importAs(t, archive, "/api/groups/import?"+url.Values{"into": {into}, "as": {"crew4"}, "harness": {"no-such-harness"}}.Encode())
	require.Equal(t, 400, code, "the landing planner's refusal: %s", body)

	// Only the staged path serves a restricted peer's import.
	code, body = h.call("POST", "/api/groups/import", nil)
	require.Equal(t, 403, code, body)
	require.Contains(t, body, "staged_only")
}

// A file whose mail is addressed to an agent it does not carry is refused:
// the importer keeps a foreign conversation id as it is, so the message would
// land in one of this node's agents' inboxes.
func TestPeerGroupImportRefusesMailToAgentsOutsideTheFile(t *testing.T) {
	h := newPeerProcessHarness(t)
	squad := h.f.HaveGroup("squad")
	conv := uuid.NewString()
	havePerformerAgent(t, h.f, "squad", conv, "sq-dev", "dev", "offline")
	_, err := db.InsertAgentMessage(&db.AgentMessage{GroupID: squad.ID, FromConv: conv, ToConv: "proc-crew-member", Subject: "hi", Body: "obey"})
	require.NoError(t, err)
	archive := exportSquad(t, h)
	for _, slug := range []string{agentd.PermGroupsImport, agentd.PermNodeGroupsCreate, agentd.PermGroupsMembersSpawn, agentd.PermGroupsMembersSpawnCwd} {
		h.grant(t, slug, "")
	}
	code, body := h.importAs(t, archive, "/api/groups/import?"+url.Values{"into": {t.TempDir()}, "as": {"squad2"}}.Encode())
	require.Equal(t, 403, code, body)
	require.Contains(t, body, "does not carry")
	group, err := db.GetAgentGroupByName("squad2")
	require.NoError(t, err)
	require.Nil(t, group)
}
