package agentd_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// Everything an agent launches must fit inside the agent's own sandbox: a
// child it spawns, the members of a template it deploys, and a worker a
// trigger it owns starts (tcl-w0rr5d). All three are capped by one rule
// (childSandboxParent.contain). These pin, for each surface, what the caller
// sees when the launch is refused because the caller predates effective
// sandbox snapshots, when it is refused because the launch would be wider
// than the caller, and that a launch that fits is admitted.

// containmentCase is one launch attempt's outcome as its surface reports it:
// the HTTP status and error body for spawn and deploy, the action outcome and
// detail for a trigger.
type containmentCase struct {
	code   int
	kind   string
	detail string
}

// legacyCaller is an agent with no stored launch snapshot.
const legacyCaller = "legacy-c0n7-a1n0-0000-000000000001"

// widenGroup assigns group a profile that grants internet access, which no
// caller here has.
func widenGroup(t *testing.T, group string) {
	t.Helper()
	if p, err := db.GetSandboxProfile("pins-internet"); err != nil || p == nil {
		_, err := db.CreateSandboxProfile(&db.SandboxProfile{
			Name: "pins-internet", NetworkAccess: sandboxpolicy.NetworkAccessInternet,
		})
		require.NoError(t, err)
	}
	_, err := db.SetAgentGroupSandboxProfile(group, "pins-internet")
	require.NoError(t, err)
}

// currentCaller spawns, as the human, an agent into home with a stored launch
// snapshot and the launch posture the lineage guard admits children from.
func currentCaller(t *testing.T, f *testharness.Flow, home string) string {
	t.Helper()
	parent := f.AsHuman().SpawnWith(home, map[string]any{
		"name": "caller", "harness": "codex", "sandbox": harness.SandboxManagedProfile,
	})
	require.Equalf(t, http.StatusOK, parent.Code, "spawn body=%s", parent.Raw)
	sess, err := db.FindSessionByConvID(parent.ConvID)
	require.NoError(t, err)
	require.NotNil(t, sess)
	sess.HarnessBuiltinMode = harness.SandboxManagedProfile
	require.NoError(t, db.SaveSession(sess))
	return parent.ConvID
}

func grantAll(t *testing.T, conv string, slugs ...string) {
	t.Helper()
	for _, s := range slugs {
		require.NoError(t, db.GrantAgentPermission(conv, s, "test"))
	}
}

func spawnAs(t *testing.T, f *testharness.Flow, caller, group string) containmentCase {
	t.Helper()
	grantAll(t, caller, agentd.PermGroupsMembersSpawn, agentd.PermAgentSpawn)
	resp := f.AsAgent(caller).SpawnWith(group, map[string]any{
		"name": "child", "harness": "codex", "sandbox": harness.SandboxManagedProfile,
	})
	return httpCase(resp.Code, resp.Raw)
}

func reinforceAs(t *testing.T, f *testharness.Flow, caller, group string) containmentCase {
	t.Helper()
	grantAll(t, caller, agentd.PermGroupsMembersSpawn, agentd.PermAgentSpawn, agentd.PermTemplatesUse)
	if tmpl, err := db.GetGroupTemplate("pins-roster"); err != nil || tmpl == nil {
		require.Equal(t, http.StatusCreated, humanReq(t, f, http.MethodPost, "/v1/templates",
			map[string]any{"name": "pins-roster", "agents": []map[string]any{
				{"name": "member", "role": "dev", "harness": "codex", "sandbox": harness.SandboxManagedProfile},
			}}).Code)
	}
	rec := agentReqProof(t, f, caller, http.MethodPost, "/v1/templates/pins-roster/reinforce",
		map[string]any{"group_name": group, "task": "t", "cwd": t.TempDir()})
	return httpCase(rec.Code, rec.Body.Bytes())
}

var pinPRNumber = 100

func triggerAs(t *testing.T, f *testharness.Flow, caller string) containmentCase {
	t.Helper()
	f.HaveMember("alpha", caller)
	grantAll(t, caller, agentd.PermGroupsTriggersManage, agentd.PermGroupsMembersSpawn)
	if p, err := db.GetSpawnProfile("reviewer"); err != nil || p == nil {
		_, err := db.CreateSpawnProfile(&db.SpawnProfile{Name: "reviewer", Harness: "codex", Sandbox: harness.SandboxManagedProfile})
		require.NoError(t, err)
	}
	ownerAgent, _, err := db.EnsureAgentForConv(caller, "test")
	require.NoError(t, err)
	ruleID := refusalTrigger(t, f, ownerAgent)
	pinPRNumber++
	out := openPR(t, ruleID, pinPRNumber)
	return containmentCase{code: 0, kind: out.Outcome, detail: out.Detail}
}

// httpCase is an HTTP surface's outcome: its status, and the error code and
// message of a refusal (the raw body when it is not one).
func httpCase(code int, raw []byte) containmentCase {
	var body struct {
		Code  string `json:"code"`
		Error string `json:"error"`
	}
	if json.Unmarshal(raw, &body) != nil || body.Code == "" {
		return containmentCase{code: code, detail: string(raw)}
	}
	return containmentCase{code: code, kind: body.Code, detail: body.Error}
}

// The refusals each surface reports today. The containment message is
// sandboxpolicy.RequireContained's, verbatim.
const (
	pinsContainmentMsg = `network access "internet" is not contained by parent access ""`
	pinsLegacyPrefix   = "this parent predates effective sandbox snapshots and may not inherit custom capabilities; relaunch it under current policy or ask the human to "
)

func TestChildSandboxContainmentPins(t *testing.T) {
	type outcomes struct{ legacy, wider, fits containmentCase }
	surfaces := []struct {
		name   string
		flow   func(t *testing.T) *testharness.Flow
		launch func(t *testing.T, f *testharness.Flow, caller string) containmentCase
		want   outcomes
	}{
		{
			name: "spawn",
			flow: func(t *testing.T) *testharness.Flow { f := newFlow(t); f.HaveGroup("alpha"); return f },
			launch: func(t *testing.T, f *testharness.Flow, caller string) containmentCase {
				return spawnAs(t, f, caller, "alpha")
			},
			want: outcomes{
				legacy: containmentCase{http.StatusForbidden, "sandbox_profile_restricted", pinsLegacyPrefix + "spawn the child"},
				wider:  containmentCase{http.StatusForbidden, "sandbox_profile_restricted", pinsContainmentMsg},
				fits:   containmentCase{code: http.StatusOK},
			},
		},
		{
			name: "template deploy",
			flow: func(t *testing.T) *testharness.Flow { f := newFlow(t); f.HaveGroup("alpha"); return f },
			launch: func(t *testing.T, f *testharness.Flow, caller string) containmentCase {
				return reinforceAs(t, f, caller, "alpha")
			},
			want: outcomes{
				legacy: containmentCase{http.StatusForbidden, "sandbox_profile_restricted", pinsLegacyPrefix + "instantiate the template"},
				wider:  containmentCase{http.StatusForbidden, "sandbox_profile_restricted", pinsContainmentMsg},
				fits:   containmentCase{code: http.StatusCreated},
			},
		},
		{
			// A trigger records its refusal as the firing's action outcome.
			name: "trigger worker",
			flow: uniformTriggerGroup,
			launch: func(t *testing.T, f *testharness.Flow, caller string) containmentCase {
				return triggerAs(t, f, caller)
			},
			want: outcomes{
				legacy: containmentCase{kind: "permission_denied", detail: pinsLegacyPrefix + "start the worker"},
				wider:  containmentCase{kind: "permission_denied", detail: pinsContainmentMsg},
				fits:   containmentCase{kind: "spawned"},
			},
		},
	}
	check := func(t *testing.T, want, got containmentCase) {
		t.Helper()
		assert.Equal(t, want.code, got.code, "status; body=%s", got.detail)
		assert.Equal(t, want.kind, got.kind, "code; body=%s", got.detail)
		if want.detail != "" {
			assert.Contains(t, got.detail, want.detail)
		}
	}
	for _, s := range surfaces {
		t.Run(s.name, func(t *testing.T) {
			t.Run("caller predates snapshots", func(t *testing.T) {
				f := s.flow(t)
				f.HaveMember("alpha", legacyCaller)
				widenGroup(t, "alpha")
				check(t, s.want.legacy, s.launch(t, f, legacyCaller))
			})
			t.Run("launch wider than caller", func(t *testing.T) {
				f := s.flow(t)
				f.HaveGroup("crew")
				caller := currentCaller(t, f, "crew")
				widenGroup(t, "alpha")
				check(t, s.want.wider, s.launch(t, f, caller))
			})
			t.Run("launch fits", func(t *testing.T) {
				f := s.flow(t)
				f.HaveGroup("crew")
				caller := currentCaller(t, f, "crew")
				check(t, s.want.fits, s.launch(t, f, caller))
			})
		})
	}
}
