package agentd_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

type codexDriveDiagnostic struct {
	ConvID          string `json:"conv_id"`
	Drive           string `json:"drive"`
	DriveSource     string `json:"drive_source"`
	Health          string `json:"health"`
	RuntimeState    string `json:"runtime_state"`
	SocketIdentity  string `json:"socket_identity"`
	MessageDelivery string `json:"message_delivery"`
	Detail          string `json:"detail"`
	Rollback        string `json:"rollback"`
	CallerConv      string `json:"caller_conv"`
}

func TestCodexAppServerDiagnosticsSelfAndOwnerTargetRoutes(t *testing.T) {
	f := newFlow(t)
	const worker = "019ec111-4250-79b1-9ade-ebaea4170180"
	const lead = "019ec222-4250-79b1-9ade-ebaea4170180"
	f.HaveGroup("rollout")
	f.HaveAliveCodexSession(worker, "codex-worker", "tmux-codex-worker", f.TestCwd("worker"))
	f.HaveMember("rollout", worker)
	group, err := db.GetAgentGroupByName("rollout")
	require.NoError(t, err)
	require.NoError(t, db.AddAgentGroupOwner(group.ID, lead, "test"))
	_, _, err = db.EnsureAgentForConv(worker, "test")
	require.NoError(t, err)
	require.NoError(t, db.SetAgentCodexAppServerSelectionForConv(worker, true, "group default profile rollout"))
	require.NoError(t, db.UpsertCodexAppServerRuntime(db.CodexAppServerRuntime{
		Generation: "flow-generation", LaunchID: "flow-launch", AgentID: "flow-agent",
		ConvID: worker, ThreadID: worker, SocketPath: "/home/operator/private/app.sock",
		CodexVersion: "0.147.2", State: db.CodexAppServerUnavailable,
		Detail: "dial /home/operator/private/app.sock: connection refused", CreatedAt: time.Now().UTC(),
	}))

	read := func(req *http.Request) codexDriveDiagnostic {
		rec := testharness.Serve(f.Mux, req)
		require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
		var got codexDriveDiagnostic
		testharness.DecodeJSON(t, rec, &got)
		return got
	}
	self := read(agentd.AsAgentPeer(
		testharness.JSONRequest(t, http.MethodGet, "/v1/whoami/codex-app-server", nil), worker))
	assert.Equal(t, "app-server", self.Drive)
	assert.Equal(t, "group default profile rollout", self.DriveSource)
	assert.Equal(t, "disconnected", self.Health)
	assert.Equal(t, db.CodexAppServerUnavailable, self.RuntimeState)
	assert.Contains(t, self.SocketIdentity, "path withheld")
	assert.Contains(t, self.MessageDelivery, "held")
	assert.Contains(t, self.Rollback, "--send-keys")
	assert.NotContains(t, self.Detail, "/home/operator")

	targeted := read(agentd.AsAgentPeer(testharness.JSONRequest(t, http.MethodGet,
		"/v1/agent/"+worker+"/codex-app-server", nil), lead))
	assert.Equal(t, worker, targeted.ConvID)
	assert.Equal(t, lead, targeted.CallerConv)
}
