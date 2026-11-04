package agentd_test

import (
	"net/http"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// A template deploy resolves sandbox mode, approval, tool governance and the
// SSH workaround over the same tiers a direct spawn does: the template's own
// profiles, then the group's and the global default profile, then the
// harness default (tcl-a2e791 slice 5, TCL-1119). Before, the deploy applied
// the harness default right after its own tiers, so the default profiles
// never spoke for these fields.

type launchPosture struct {
	Sandbox, Approval, Tools string
	SSHWorkaround            bool
}

func postureOf(t *testing.T, f *testharness.Flow, conv string) launchPosture {
	t.Helper()
	var out launchPosture
	var ok bool
	out.Sandbox, ok = f.World.SpawnSandbox(conv)
	require.Truef(t, ok, "no spawn recorded for %s", conv)
	out.Approval, _ = f.World.SpawnApproval(conv)
	rp, err := db.AgentRelaunchProfileForConv(conv)
	require.NoError(t, err)
	require.NotNil(t, rp, "relaunch profile for %s", conv)
	if rp.ToolGovernance != nil {
		out.Tools = *rp.ToolGovernance
	}
	if rp.SSHWorkaround != nil {
		out.SSHWorkaround = *rp.SSHWorkaround
	}
	return out
}

// directPosture spawns into "alpha" with nothing named, as the comparison
// for a template member under the same defaults.
func directPosture(t *testing.T, f *testharness.Flow, body map[string]any) launchPosture {
	t.Helper()
	f.HaveGroup("alpha")
	body["name"], body["initial_message"] = "direct", "Do it."
	spawn := f.AsHuman().SpawnWith("alpha", body)
	require.Equalf(t, http.StatusOK, spawn.Code, "spawn body=%s", spawn.Raw)
	agentd.WaitForBackgroundForTest()
	return postureOf(t, f, spawn.ConvID)
}

func globalDefault(t *testing.T, f *testharness.Flow, body map[string]any) {
	t.Helper()
	body["name"] = "workspace-default"
	require.Equal(t, http.StatusCreated, createProfile(t, f, body).Code)
	require.Equal(t, http.StatusOK, setGlobalProfile(t, f, "workspace-default").Code)
}

// axes drops the SSH workaround, which only Codex launches use; the stored
// flag already differed between deploys and direct spawns on other harnesses.
func (p launchPosture) axes() launchPosture {
	p.SSHWorkaround = false
	return p
}

// Pinned at the values a deploy got before this change: the harness's own
// secure defaults.
func TestTemplateDefaultTiers_NothingConfiguredUnchanged(t *testing.T) {
	for h, want := range map[string]launchPosture{
		"claude": {Sandbox: "inherit", Approval: "auto"},
		// harness.CanSSHWorkaround is Linux-only, so the default is off on macOS.
		"codex":    {Sandbox: "tclaude-agent", Approval: "never", SSHWorkaround: runtime.GOOS == "linux"},
		"opencode": {Sandbox: "access-control", Approval: "deny", Tools: "allow"},
	} {
		t.Run(h, func(t *testing.T) {
			f := newFlow(t)
			assert.Equal(t, want, postureOf(t, f, deployUniformTemplate(t, f, map[string]any{"harness": h})))
		})
	}
}

func TestTemplateDefaultTiers_GlobalDefaultSetsSandboxAndApproval(t *testing.T) {
	f := newFlow(t)
	globalDefault(t, f, map[string]any{"harness": "claude", "sandbox": "on", "approval": "plan"})
	member := postureOf(t, f, deployUniformTemplate(t, f, map[string]any{}))
	assert.Equal(t, "on", member.Sandbox, "the workspace default profile's sandbox mode applies")
	assert.Equal(t, "plan", member.Approval)
	assert.Equal(t, directPosture(t, f, map[string]any{}).axes(), member.axes(), "as on a direct spawn")
}

func TestTemplateDefaultTiers_GlobalDefaultSetsToolGovernance(t *testing.T) {
	f := newFlow(t)
	globalDefault(t, f, map[string]any{"harness": "opencode", "tools": "ask"})
	member := postureOf(t, f, deployUniformTemplate(t, f, map[string]any{}))
	assert.Equal(t, "ask", member.Tools)
	assert.Equal(t, directPosture(t, f, map[string]any{}).axes(), member.axes())
}

func TestTemplateDefaultTiers_GlobalDefaultSetsSSHWorkaround(t *testing.T) {
	f := newFlow(t)
	globalDefault(t, f, map[string]any{"harness": "codex", "ssh_workaround": false})
	member := postureOf(t, f, deployUniformTemplate(t, f, map[string]any{}))
	assert.Equal(t, directPosture(t, f, map[string]any{}), member)
	assert.False(t, member.SSHWorkaround, "the workspace default profile turns the workaround off")
}

// The template's own profile still wins over the defaults.
func TestTemplateDefaultTiers_ReferencedProfileWins(t *testing.T) {
	f := newFlow(t)
	globalDefault(t, f, map[string]any{"harness": "claude", "sandbox": "on", "approval": "plan"})
	require.Equal(t, http.StatusCreated, createProfile(t, f, map[string]any{
		"name": "member-profile", "harness": "claude", "sandbox": "off", "approval": "default"}).Code)
	member := postureOf(t, f, deployUniformTemplate(t, f, map[string]any{"spawn_profile": "member-profile"}))
	assert.Equal(t, "off", member.Sandbox)
	assert.Equal(t, "default", member.Approval)
}

// A default-tier value skipped for the deploy's harness is disclosed once,
// though both the template walk and the spawn walk the default tiers.
func TestTemplateDefaultTiers_SkipNoteDisclosedOnce(t *testing.T) {
	f := newFlow(t)
	globalDefault(t, f, map[string]any{"harness": "claude", "model": "opus"})
	require.Equal(t, http.StatusCreated, humanReq(t, f, http.MethodPost, "/v1/templates", map[string]any{
		"name": "notes-team", "agents": []map[string]any{{"name": "member", "role": "dev",
			"harness": "codex", "initial_message": "Do it."}},
	}).Code)
	rec := humanReq(t, f, http.MethodPost, "/v1/templates/notes-team/deploy", map[string]any{"group_name": "notes-force"})
	require.Equalf(t, http.StatusCreated, rec.Code, "deploy: %s", rec.Body.String())
	var res instantiateResult
	testharness.DecodeJSON(t, rec, &res)
	require.Len(t, res.Agents, 1)
	require.NotNil(t, res.Agents[0].Resolved)
	seen := map[string]int{}
	for _, n := range res.Agents[0].Resolved.Notes {
		seen[n]++
	}
	found := false
	for n, c := range seen {
		assert.Equalf(t, 1, c, "note %q repeated", n)
		if strings.Contains(n, "model ignored") {
			found = true
		}
	}
	assert.True(t, found, "the skipped model is disclosed: %v", res.Agents[0].Resolved.Notes)
}
