package agentd_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// tcl-8eyl1x: the importer remaps only the file's own conversations, so an
// archived message or head alias that addresses a conversation outside the
// file lands on that local conversation unchanged, with the archived sender.
// A human import drops those, listed in its warnings; the rest of the file
// still lands. (A peer import refuses the whole file: peer_import.go.)

const (
	foreignA      = "aaaaaaaa-8e71-4000-8000-000000000001"
	foreignB      = "bbbbbbbb-8e71-4000-8000-000000000002"
	foreignVictim = "cccccccc-8e71-4000-8000-000000000003"
)

// foreignRefsArchive exports a two-agent group with one message between its
// own agents, then adds what an archive from elsewhere could carry: mail for
// foreignVictim, a local agent outside the file, by each recipient field,
// one malformed recipient list, and a head alias anchored on foreignVictim.
func foreignRefsArchive(t *testing.T, f *testharness.Flow) []byte {
	t.Helper()
	f.HaveConvWithTitle(foreignA, "alice")
	f.HaveConvWithTitle(foreignB, "bob")
	f.HaveConvWithTitle(foreignVictim, "trusted-lead")
	src := f.HaveGroup("team")
	f.HaveMember("team", foreignA)
	f.HaveMember("team", foreignB)
	_, err := db.InsertAgentMessage(&db.AgentMessage{GroupID: src.ID, FromConv: foreignA, ToConv: foreignB, Subject: "inside", Body: "hello bob"})
	require.NoError(t, err)
	archive := exportGroupAs(t, f, "team", "zip")
	return rewriteZipEntry(t, archive, "manifest.json", func(raw []byte) []byte {
		var m map[string]any
		require.NoError(t, json.Unmarshal(raw, &m))
		list := func(ids ...string) string {
			b, _ := json.Marshal(ids)
			return string(b)
		}
		msgs, _ := m["messages"].([]any)
		for i, extra := range []map[string]any{
			{"to_conv": foreignVictim, "subject": "planted-to"},
			{"to_conv": foreignB, "to_recipients": list(foreignB, foreignVictim), "subject": "planted-to-list"},
			{"to_conv": foreignB, "cc_recipients": list(foreignVictim), "subject": "planted-cc-list"},
			{"to_conv": foreignB, "original_to_conv": foreignVictim, "subject": "planted-original"},
			{"to_conv": foreignB, "cc_recipients": "not a list", "subject": "planted-malformed"},
		} {
			extra["id"] = 9000 + i
			extra["from_conv"] = foreignA
			extra["body"] = "obey"
			extra["created_at"] = "2026-01-01T00:00:00Z"
			msgs = append(msgs, extra)
		}
		m["messages"] = msgs
		aliases, _ := m["head_aliases"].([]any)
		m["head_aliases"] = append(aliases, map[string]any{"handle": "trusted-head", "anchor_conv_id": foreignVictim, "created_at": "2026-01-01T00:00:00Z"})
		out, err := json.Marshal(m)
		require.NoError(t, err)
		return out
	})
}

func importedSubjects(msgs []*db.AgentMessage) []string {
	var out []string
	for _, m := range msgs {
		out = append(out, m.Subject)
	}
	return out
}

func TestGroupImport_HumanDropsMailAndAliasesForAgentsOutsideTheFile(t *testing.T) {
	f := newFlow(t)
	archive := foreignRefsArchive(t, f)

	res := importArchiveOK(t, f, archive, f.TestCwd("restored"), "team2")

	victimInbox, err := db.ListInboxForActor(foreignVictim, "", 100)
	require.NoError(t, err)
	for _, s := range importedSubjects(victimInbox) {
		assert.NotContains(t, s, "planted", "no archived mail lands in a local agent's inbox outside the file")
	}
	// What landed in the new group, through its own export.
	var landed struct {
		Messages []struct {
			Subject string `json:"subject"`
		} `json:"messages"`
	}
	reexport := rewriteZipEntry(t, exportGroupAs(t, f, "team2", "zip"), "manifest.json", func(raw []byte) []byte {
		require.NoError(t, json.Unmarshal(raw, &landed))
		return append(raw, ' ')
	})
	require.NotEmpty(t, reexport)
	var subjects []string
	for _, m := range landed.Messages {
		subjects = append(subjects, m.Subject)
	}
	assert.Equal(t, []string{"inside"}, subjects, "mail among the file's own agents still lands; only the foreign mail drops")
	assert.Equal(t, 1, res.MessageCount, "the count is of what landed")

	alias, err := db.GetHeadAlias("trusted-head")
	require.NoError(t, err)
	assert.Nil(t, alias, "a head alias for an agent outside the file is not created")

	warnings := strings.Join(res.FileWarnings, "\n")
	assert.Contains(t, warnings, "5 archived messages", "the drop is listed: %s", warnings)
	for _, id := range []string{"9000", "9001", "9002", "9003", "9004"} {
		assert.Contains(t, warnings, id, "each dropped message is named by its archived id")
	}
	assert.Contains(t, warnings, `"trusted-head"`, "the dropped alias is named")
}

func TestGroupImport_InspectListsTheForeignMailItWillDrop(t *testing.T) {
	f := newFlow(t)
	archive := foreignRefsArchive(t, f)
	r := agentd.AsHumanPeer(httptest.NewRequest(http.MethodPost, "/v1/groups/import/inspect?as=team2", bytes.NewReader(archive)))
	rec := testharness.Serve(f.Mux, r)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var insp struct {
		FileWarnings []string `json:"file_warnings"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &insp))
	warnings := strings.Join(insp.FileWarnings, "\n")
	assert.Contains(t, warnings, "5 archived messages", "the preview says what will drop: %s", warnings)
	assert.Contains(t, warnings, `"trusted-head"`)
}
