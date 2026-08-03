package agentd_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// TestSandboxProfilePreviewResolvesLaunchDefaultsAndNamesComposedLayers pins the
// two halves of the TCL-865 vocabulary at their source, so the dashboard's copy
// is describing something the daemon actually does.
//
// The halves are deliberately different mechanisms and the test asserts both:
//
//   - RESOLVED DEFAULTS: with no explicit target, the preview walks the same
//     launch chain a real spawn walks — the group's default spawn profile
//     outranks the global one, which outranks the harness default — and says
//     which tier it took.
//   - COMPOSED SANDBOX LAYERS: the global, group, and explicit sandbox profiles
//     all appear in the effective context, by scope and by name. This is what
//     the editor's always-visible layer row renders, so a preview that silently
//     dropped a layer would leave that row lying.
func TestSandboxProfilePreviewResolvesLaunchDefaultsAndNamesComposedLayers(t *testing.T) {
	f := newFlow(t)
	_, err := db.CreateAgentGroup("crew", "")
	require.NoError(t, err)

	for _, name := range []string{"house-rules", "crew-rules"} {
		rec := profileReq(t, f, http.MethodPost, "/v1/sandbox-profiles", map[string]any{
			"name": name, "filesystem": []any{}, "environment": []any{},
			"darwin_allow_mach_register": name == "house-rules",
		})
		require.Equalf(t, http.StatusCreated, rec.Code, "body=%s", rec.Body.String())
	}
	require.NoError(t, db.SetGlobalSandboxProfile("house-rules"))
	_, err = db.SetAgentGroupSandboxProfile("crew", "crew-rules")
	require.NoError(t, err)

	// Both spawn-profile tiers are populated, with DIFFERENT harnesses, so the
	// assertion below can distinguish all three outcomes: group wins (codex),
	// global wins (opencode), or neither was consulted and the harness default
	// (claude) stood. A test that left the global tier empty would pass just as
	// happily with the precedence inverted.
	_, err = db.CreateSpawnProfile(&db.SpawnProfile{Name: "crew-launch", Harness: "codex"})
	require.NoError(t, err)
	_, err = db.CreateSpawnProfile(&db.SpawnProfile{Name: "house-launch", Harness: "opencode"})
	require.NoError(t, err)
	rec := profileReq(t, f, http.MethodPut, "/v1/spawn-profile-default",
		map[string]any{"name": "house-launch"})
	require.Equalf(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	_, err = db.SetAgentGroupDefaultProfile("crew", "crew-launch")
	require.NoError(t, err)

	rec = profileReq(t, f, http.MethodPost, "/v1/sandbox-profile-enforcement", map[string]any{
		"draft": map[string]any{
			"name": "scratch-draft", "filesystem": []any{}, "environment": []any{},
		},
		"context": map[string]any{"group": "crew"},
	})
	require.Equalf(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	var got struct {
		Targets []struct {
			Target struct {
				Harness string `json:"harness"`
			} `json:"target"`
			ResolvedBy string `json:"resolved_by"`
		} `json:"targets"`
		Contexts []struct {
			Context                 map[string]string `json:"context"`
			DarwinAllowMachRegister bool              `json:"darwin_allow_mach_register"`
		} `json:"contexts"`
	}
	testharness.DecodeJSON(t, rec, &got)

	require.Len(t, got.Targets, 1)
	assert.Equal(t, "codex", got.Targets[0].Target.Harness,
		"the preview must resolve the launch target the way a real spawn into this group would")
	assert.Contains(t, got.Targets[0].ResolvedBy, `group default profile "crew-launch"`,
		"the preview has to name the tier it resolved from; the dashboard renders this verbatim")
	assert.NotContains(t, got.Targets[0].ResolvedBy, "house-launch",
		"the group tier outranks the global one; the global profile must not be the source")

	require.Len(t, got.Contexts, 1)
	assert.Equal(t, map[string]string{
		"global":     "house-rules",
		"group":      "crew-rules",
		"group_name": "crew",
		"explicit":   "scratch-draft",
	}, got.Contexts[0].Context,
		"every composed sandbox-profile layer must be identifiable by scope and name")
	assert.True(t, got.Contexts[0].DarwinAllowMachRegister,
		"the effective preview must preserve the composed macOS compatibility capability")
}
