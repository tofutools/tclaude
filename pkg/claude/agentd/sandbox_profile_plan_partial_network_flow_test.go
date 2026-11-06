package agentd_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// The reported case, generically: the group profile allows the model
// endpoint, the global profile's own allow list does not, and the lists
// intersect. `sandbox-profiles plan` must say which tier dropped what, while
// the predicted rules stay exactly the intersection.
func TestSandboxProfilePlanNamesTierThatDroppedNetworkEntries(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("crew")
	list := func(hosts ...string) *sandboxpolicy.NetworkRules {
		rules := &sandboxpolicy.NetworkRules{Baseline: sandboxpolicy.NetworkBaselineDeny}
		for _, host := range hosts {
			rules.Allow = append(rules.Allow, sandboxpolicy.NetworkAllowEntry{Host: host, Ports: []int{443}})
		}
		return rules
	}
	_, err := db.CreateSandboxProfile(&db.SandboxProfile{Name: "base", Network: list("git.example")})
	require.NoError(t, err)
	_, err = db.CreateSandboxProfile(&db.SandboxProfile{Name: "dev", Network: list("git.example", "api.anthropic.com")})
	require.NoError(t, err)
	require.NoError(t, db.SetGlobalSandboxProfile("base"))
	_, err = db.SetAgentGroupSandboxProfile("crew", "dev")
	require.NoError(t, err)

	req := testharness.JSONRequest(t, http.MethodPost, "/v1/sandbox-profile-plan", map[string]any{
		"group": "crew",
		"cwd":   f.World.HomeDir,
		"for":   "tclaude-layer/claude",
	})
	rec := testharness.Serve(f.Mux, agentd.AsHumanPeer(req))
	require.Equalf(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	var response struct {
		Notices []sandboxpolicy.AccessNotice `json:"notices"`
	}
	testharness.DecodeJSON(t, rec, &response)

	var details []string
	for _, notice := range response.Notices {
		if notice.Reason == sandboxpolicy.AccessNoticeReasonPartialIntersection {
			details = append(details, notice.Detail)
		}
	}
	require.Lenf(t, details, 1, "notices: %+v", response.Notices)
	assert.Contains(t, details[0], `global "base" ∩ group "dev"`)
	assert.Contains(t, details[0], `group "dev": api.anthropic.com:443`)
}
