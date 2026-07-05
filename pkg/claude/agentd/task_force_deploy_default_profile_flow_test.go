package agentd_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// The deploy dialog's "Default profile" picker resolves each roster member with
// NO profile of its own to a sensible default (the group / dashboard default) and
// sends the result per-member in the deploy request's `agent_profiles` map. The
// server applies the map only to members that carried no profile — it never
// server-defaults and never displaces an explicit per-member template choice.
// These flow tests assert the resolved launch shape at the Spawner boundary
// (World.SpawnModel), the same surface the per-role-profile tests use.

// Scenario: a 2-member template — a lead that REFERENCES a profile and a worker
// left blank — is deployed with an agent_profiles override. The blank worker
// spawns on the override profile's model; the lead keeps its own profile EVEN
// THOUGH the map also names it (the override only fills a blank).
func TestTaskForceDeploy_DefaultProfile_FillsBlankMemberOnly(t *testing.T) {
	f := newFlow(t)

	require.Equalf(t, http.StatusCreated,
		createProfile(t, f, map[string]any{"name": "fancy", "model": "opus"}).Code, "create fancy")
	require.Equalf(t, http.StatusCreated,
		createProfile(t, f, map[string]any{"name": "cheap", "model": "haiku"}).Code, "create cheap")

	createBody := map[string]any{
		"name": "team",
		"agents": []map[string]any{
			{"name": "lead", "role": "lead", "spawn_profile": "fancy"},
			{"name": "worker", "role": "dev"}, // no profile of its own
		},
	}
	require.Equalf(t, http.StatusCreated,
		humanReq(t, f, http.MethodPost, "/v1/templates", createBody).Code, "create template")

	// Deploy resolving the blank worker to "cheap" — and also (deliberately)
	// naming the lead, which the server must ignore because the lead has its own.
	rec := humanReq(t, f, http.MethodPost, "/v1/templates/team/deploy", map[string]any{
		"group_name":     "phoenix",
		"agent_profiles": map[string]any{"lead": "cheap", "worker": "cheap"},
	})
	require.Equalf(t, http.StatusCreated, rec.Code, "deploy: %s", rec.Body.String())
	var res instantiateResult
	testharness.DecodeJSON(t, rec, &res)
	require.Equal(t, 2, res.Spawned, "both members spawned")
	require.Equal(t, 0, res.Failed, "no spawn failures: %+v", res.Agents)
	agentd.WaitForBackgroundForTest()

	convByName := map[string]string{}
	for _, a := range res.Agents {
		require.Emptyf(t, a.Error, "member %s spawned cleanly", a.Name)
		convByName[a.Name] = a.ConvID
	}

	leadModel, ok := f.World.SpawnModel(convByName["lead"])
	require.Truef(t, ok, "no spawn recorded for lead conv %s", convByName["lead"])
	assert.Equal(t, "opus", leadModel, "the lead keeps its own profile — the override never displaces an explicit choice")

	workerModel, ok := f.World.SpawnModel(convByName["worker"])
	require.Truef(t, ok, "no spawn recorded for worker conv %s", convByName["worker"])
	assert.Equal(t, "haiku", workerModel, "the blank worker adopts the deploy default profile")
}

// Scenario: the server never applies the default to a member that carries its own
// launch config even when the map names it — an inline model, or a referenced
// role, is the member's own setting and wins. (The client omits such members from
// the map; this asserts the server's own eligibility guard as defence-in-depth.)
func TestTaskForceDeploy_DefaultProfile_SkipsConfiguredMembers(t *testing.T) {
	f := newFlow(t)

	require.Equalf(t, http.StatusCreated,
		createProfile(t, f, map[string]any{"name": "cheap", "model": "haiku"}).Code, "create cheap")
	// A role carrying its own launch model.
	require.Equalf(t, http.StatusCreated,
		humanReq(t, f, http.MethodPost, "/v1/roles",
			map[string]any{"name": "tf-default-reviewer", "model": "sonnet"}).Code, "create role")

	createBody := map[string]any{
		"name": "team",
		"agents": []map[string]any{
			{"name": "inliner", "role": "dev", "model": "opus"}, // inline model, blank profile
			{"name": "roled", "role": "qa", "role_ref": "tf-default-reviewer"}, // launch via role
		},
	}
	require.Equalf(t, http.StatusCreated,
		humanReq(t, f, http.MethodPost, "/v1/templates", createBody).Code, "create template")

	// The map (wrongly) names both — the server must ignore both.
	rec := humanReq(t, f, http.MethodPost, "/v1/templates/team/deploy", map[string]any{
		"group_name":     "phoenix",
		"agent_profiles": map[string]any{"inliner": "cheap", "roled": "cheap"},
	})
	require.Equalf(t, http.StatusCreated, rec.Code, "deploy: %s", rec.Body.String())
	var res instantiateResult
	testharness.DecodeJSON(t, rec, &res)
	require.Equal(t, 2, res.Spawned)
	require.Equal(t, 0, res.Failed, "no spawn failures: %+v", res.Agents)
	agentd.WaitForBackgroundForTest()

	convByName := map[string]string{}
	for _, a := range res.Agents {
		convByName[a.Name] = a.ConvID
	}
	inlinerModel, _ := f.World.SpawnModel(convByName["inliner"])
	assert.Equal(t, "opus", inlinerModel, "an inline model wins over the deploy default")
	roledModel, _ := f.World.SpawnModel(convByName["roled"])
	assert.Equal(t, "sonnet", roledModel, "a role's own launch model wins over the deploy default")
}

// Scenario: no agent_profiles sent — the deploy is byte-identical to before this
// feature (a blank member spawns on the harness default, not some inferred one).
func TestTaskForceDeploy_DefaultProfile_NoOverrideIsNoOp(t *testing.T) {
	f := newFlow(t)

	require.Equalf(t, http.StatusCreated,
		createProfile(t, f, map[string]any{"name": "cheap", "model": "haiku"}).Code, "create cheap")

	createBody := map[string]any{
		"name": "team",
		"agents": []map[string]any{
			{"name": "worker", "role": "dev"}, // blank
		},
	}
	require.Equalf(t, http.StatusCreated,
		humanReq(t, f, http.MethodPost, "/v1/templates", createBody).Code, "create template")

	rec := humanReq(t, f, http.MethodPost, "/v1/templates/team/deploy",
		map[string]any{"group_name": "phoenix"})
	require.Equalf(t, http.StatusCreated, rec.Code, "deploy: %s", rec.Body.String())
	var res instantiateResult
	testharness.DecodeJSON(t, rec, &res)
	require.Equal(t, 1, res.Spawned)
	require.Equal(t, 0, res.Failed, "no spawn failures: %+v", res.Agents)
	agentd.WaitForBackgroundForTest()

	// The worker did NOT inherit "cheap" — nothing was sent, so nothing is applied.
	model, _ := f.World.SpawnModel(res.Agents[0].ConvID)
	assert.NotEqual(t, "haiku", model, "with no agent_profiles sent, the blank member is not defaulted server-side")
}
