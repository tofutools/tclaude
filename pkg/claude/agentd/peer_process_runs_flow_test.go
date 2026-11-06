package agentd_test

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// peerProcessHarness is a federation harness whose node has a process
// template store with a human-gate template ("gate") and a program one
// ("prog"), a group "crew" with a member and a three-phase process.
type peerProcessHarness struct {
	*fedHarness
	group  *db.AgentGroup
	member string // agent ID of crew's member
	call   func(method, path string, body any) (int, string)
}

func newPeerProcessHarness(t *testing.T) *peerProcessHarness {
	t.Helper()
	fh := newFedHarness(t)
	root := filepath.Join(fh.f.World.HomeDir, ".tclaude", "process-runtime-authoring")
	t.Cleanup(agentd.SetProcessStoreRootForTest(root))
	t.Cleanup(agentd.ResetProcessRunRuntimeForTest())
	putProcessRuntimeTemplate(t, root, processHumanGateTemplate("gate"))
	putProcessRuntimeTemplate(t, root, processRuntimeTemplate("prog", 1))
	g := fh.f.HaveGroup("crew")
	fh.f.HaveConvWithTitle("proc-crew-member", "worker")
	fh.f.HaveAliveSession("proc-crew-member", "spwn-proc-crew", "tmux-proc-crew", fh.f.World.HomeDir)
	fh.f.HaveMemberWithRole("crew", "proc-crew-member", "dev")
	require.NoError(t, db.InitGroupProcess(g.ID, []db.ProcessPhase{
		{Name: "design", Roles: []string{"architect"}},
		{Name: "build", Roles: []string{"dev"}},
		{Name: "review", Roles: []string{"reviewer"}},
	}, "human"))
	h := agentd.PeerViewHandler(fh.peer.id.ID())
	return &peerProcessHarness{
		fedHarness: fh, group: g, member: agentIDOf(t, "proc-crew-member"),
		call: func(method, path string, body any) (int, string) {
			t.Helper()
			rec := testharness.Serve(h, testharness.JSONRequest(t, method, path, body))
			return rec.Code, rec.Body.String()
		},
	}
}

func (h *peerProcessHarness) grant(t *testing.T, slug, scope string) {
	t.Helper()
	body := map[string]any{"peer": "bob", "slug": slug}
	if scope != "" {
		body["scope"] = scope
	}
	rec := fedHuman(t, h.f, http.MethodPost, "/v1/federation/grants", body)
	require.Equal(t, 200, rec.Code, rec.Body.String())
}

func (h *peerProcessHarness) trustUnrestricted(t *testing.T) {
	t.Helper()
	peer, err := db.GetFederationPeer(h.peer.id.ID())
	require.NoError(t, err)
	peer.TrustLevel = db.FederationTrustUnrestricted
	require.NoError(t, db.TrustFederationPeer(*peer))
}

// Pins: a restricted peer reaches none of this node's process runs or its
// group process, whatever it is granted, while unrestricted trust runs them
// through this node's own handlers as its operator.
func TestPeerProcessRunPins(t *testing.T) {
	h := newPeerProcessHarness(t)
	h.grant(t, agentd.PermGroupsRosterRead, "group=crew")
	start := map[string]any{"templateId": "gate", "group": "crew", "authorizeProgramProfiles": []string{}}
	for _, c := range []struct {
		method, path string
		body         any
	}{
		{"GET", "/v1/process/runs", nil},
		{"POST", "/v1/process/runs", start},
		{"GET", "/api/groups/crew/process", nil},
		{"POST", "/api/groups/crew/process/advance", map[string]any{}},
	} {
		code, body := h.call(c.method, c.path, c.body)
		require.Equal(t, 403, code, "%s %s: %s", c.method, c.path, body)
	}

	h.trustUnrestricted(t)
	code, body := h.call("POST", "/v1/process/runs", start)
	require.Equal(t, http.StatusCreated, code, body)
	var run processScopedRunView
	require.NoError(t, json.Unmarshal([]byte(body), &run))
	require.Equal(t, "human:operator", run.Scope.Owner, "unrestricted trust acts as this node's operator")
	code, body = h.call("GET", "/v1/process/runs/"+run.ID, nil)
	require.Equal(t, 200, code, body)
	code, body = h.call("POST", "/api/groups/crew/process/advance", map[string]any{})
	require.Equal(t, 200, code, body)
	require.Contains(t, body, `"to":"build"`)
}
