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

func TestWhoamiReportsOnlyActiveMembershipsForImplicitSpawn(t *testing.T) {
	f := newFlow(t)
	const caller = "spawn-own-group-caller"
	f.HaveConvWithTitle(caller, "caller")
	f.HaveGroup("active")
	f.HaveGroup("archived")
	f.HaveMember("active", caller)
	f.HaveMember("archived", caller)
	require.NoError(t, db.ArchiveAgentGroup("archived"))

	rec := testharness.Serve(f.Mux, agentd.AsAgentPeer(
		testharness.JSONRequest(t, http.MethodGet, "/v1/whoami", nil), caller))
	require.Equal(t, http.StatusOK, rec.Code, "whoami: %s", rec.Body.String())
	var resp struct {
		Groups       []string `json:"groups"`
		ActiveGroups []string `json:"active_groups"`
	}
	testharness.DecodeJSON(t, rec, &resp)
	assert.Equal(t, []string{"active", "archived"}, resp.Groups)
	assert.Equal(t, []string{"active"}, resp.ActiveGroups)
}
