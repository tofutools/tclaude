package agentd_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/federation/proto"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// A local agent reading a linked node's board context through an agent peer
// view gets a board's options summary only where it holds
// taskboard.options.read for that peer and group: the linked node decides on
// its grants to this node as a whole, this node on the agent's own. The
// tasks, and their queue facts, stay.
func TestPeerViewAgentContextOptionsSummary(t *testing.T) {
	fh := newFedHarness(t)
	f, p := fh.f, fh.peer
	agentd.ResetAgentPeerViewLimitsForTest()
	t.Cleanup(agentd.ResetAgentPeerViewLimitsForTest)
	inst := p.id.ID()
	const caller = "019fe740-43a4-7023-b8ae-0000000a6e77"
	f.HaveEnrolledAgent(caller)
	before, _, _ := db.GetFederationCatalog(inst)
	p.send(p.envelope(proto.KindCatalog, proto.Endpoint{}, proto.CatalogPayload{PeerViewAgents: true, Groups: []proto.CatalogGroup{{Name: "lab"}, {Name: "ops"}}}))
	fedEventually(t, "catalog stored", func() bool {
		after, _, _ := db.GetFederationCatalog(inst)
		return after != before && after != ""
	})
	require.NoError(t, db.GrantAgentPermissionWithScope(caller, agentd.PermTaskboardRead, `{"peer":["`+inst+`"]}`, "test"))
	require.NoError(t, db.GrantAgentPermissionWithScope(caller, agentd.PermTaskboardOptionsRead, `{"peer":["`+inst+`/lab"]}`, "test"))

	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		done <- testharness.Serve(f.Mux, agentd.AsAgentPeer(testharness.JSONRequest(t, "GET", "/v1/federation/peer/bob/v1/taskboard/context", nil), caller))
	}()
	conn := acceptOutboundPeerView(t, p, 0)
	wire := readPeerViewWire(t, conn)
	require.Equal(t, "/v1/taskboard/context", wire.URI)
	entry := func(group string) map[string]any {
		return map[string]any{"group": group, "options": map[string]any{"version": 1, "set": []string{"limits.in_progress.max"}},
			"tasks": []any{map[string]any{"id": "tk_" + group, "queued": true, "queued_reason": "In progress is full, 1st in line"}}}
	}
	body, err := json.Marshal(map[string]any{"groups": []any{entry("lab"), entry("ops")}})
	require.NoError(t, err)
	writePeerViewWire(t, conn, peerViewWire{Status: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: body})
	rec := <-done
	_ = conn.Close()
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var got struct {
		Groups []struct {
			Group   string           `json:"group"`
			Options *json.RawMessage `json:"options"`
			Tasks   []map[string]any `json:"tasks"`
		} `json:"groups"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got), rec.Body.String())
	require.Len(t, got.Groups, 2)
	for _, g := range got.Groups {
		require.Len(t, g.Tasks, 1, "%s keeps its tasks", g.Group)
		assert.Equal(t, true, g.Tasks[0]["queued"], "%s keeps a task's queue facts", g.Group)
		switch g.Group {
		case "lab":
			assert.NotNil(t, g.Options, "options.read peer=<inst>/lab keeps lab's summary")
		case "ops":
			assert.Nil(t, g.Options, "no options.read on ops: its summary is dropped")
		}
	}
}
