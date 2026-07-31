package agentd_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/claude/session"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// networkEnginePolicy is one row of the proposal's §1.3 deployment table, in
// the authored shape an operator actually types.
type networkEnginePolicy struct {
	label string
	rules map[string]any
	// deploys is the engine this policy runs when the profile selects the proxy
	// engine. Unset means the policy asks for no distinction between
	// destinations, so no engine is deployed however it is authored.
	deploysUnderProxy sandboxpolicy.NetworkEngine
	// deploysUnselected is what the same policy runs with no engine authored,
	// which must be exactly what it ran before the field existed.
	deploysUnselected sandboxpolicy.NetworkEngine
}

func networkEnginePolicyTable() []networkEnginePolicy {
	return []networkEnginePolicy{
		{
			label:             "allow-all",
			rules:             map[string]any{"baseline": "allow"},
			deploysUnderProxy: sandboxpolicy.NetworkEngineUnset,
			deploysUnselected: sandboxpolicy.NetworkEngineUnset,
		},
		{
			label:             "closed",
			rules:             map[string]any{"baseline": "deny"},
			deploysUnderProxy: sandboxpolicy.NetworkEngineUnset,
			deploysUnselected: sandboxpolicy.NetworkEngineUnset,
		},
		{
			label: "loopback-only",
			rules: map[string]any{
				"baseline": "deny",
				"allow":    []any{map[string]any{"loopback": true, "ports": []int{11434}}},
			},
			deploysUnderProxy: sandboxpolicy.NetworkEngineUnset,
			deploysUnselected: sandboxpolicy.NetworkEngineUnset,
		},
		{
			label: "loopback-plus-one-domain",
			rules: map[string]any{
				"baseline": "deny",
				"allow": []any{
					map[string]any{"loopback": true, "ports": []int{11434}},
					map[string]any{"domain": "example.com", "ports": []int{443}},
				},
			},
			deploysUnderProxy: sandboxpolicy.NetworkEngineProxy,
			deploysUnselected: sandboxpolicy.NetworkEnginePacket,
		},
		{
			label: "deny-only-under-allow",
			rules: map[string]any{
				"baseline": "allow",
				"deny":     []any{map[string]any{"domain": "blocked.example"}},
			},
			deploysUnderProxy: sandboxpolicy.NetworkEngineProxy,
			deploysUnselected: sandboxpolicy.NetworkEnginePacket,
		},
	}
}

// TestNetworkEnginePreviewAndLaunchAgreeOnWhatDeploys is the milestone's parity
// gate. The preview an operator reads and the launch that actually runs must
// name the same filtering engine for the same policy, because a divergence here
// IS the disclosure-does-not-match-rendered-surface bug rather than a cosmetic
// one: the preview would describe a mechanism the sandbox does not run.
//
// Both sides are reached through their production entry points — the daemon's
// resolved effective snapshot feeding harness.PredictAccessEnforcement on one
// side, and session.BuildTclaudeLayerLaunchSpec rendering its mount plan on the
// other — so the test fails if either grows its own copy of the predicate.
func TestNetworkEnginePreviewAndLaunchAgreeOnWhatDeploys(t *testing.T) {
	f := newFlow(t)
	for _, policy := range networkEnginePolicyTable() {
		for _, selection := range []struct {
			engine   sandboxpolicy.NetworkEngine
			expected sandboxpolicy.NetworkEngine
		}{
			{sandboxpolicy.NetworkEngineUnset, policy.deploysUnselected},
			{sandboxpolicy.NetworkEngineProxy, policy.deploysUnderProxy},
		} {
			name := policy.label + "-engine-" + string(selection.engine)
			if selection.engine == sandboxpolicy.NetworkEngineUnset {
				name = policy.label + "-engine-unset"
			}
			t.Run(name, func(t *testing.T) {
				rules := map[string]any{}
				for key, value := range policy.rules {
					rules[key] = value
				}
				if selection.engine != sandboxpolicy.NetworkEngineUnset {
					rules["engine"] = string(selection.engine)
				}
				rec := profileReq(t, f, http.MethodPost, "/v1/sandbox-profiles",
					map[string]any{
						"name": name, "filesystem": []any{}, "environment": []any{},
						"network": rules,
					})
				require.Equalf(t, http.StatusCreated, rec.Code,
					"body=%s", rec.Body.String())

				snapshot, err := db.ResolveEffectiveSandboxSnapshot(0, name)
				require.NoError(t, err)
				axes, err := sandboxpolicy.PlannedEffectiveAccessAxes(snapshot.Effective)
				require.NoError(t, err)

				predicted, err := harness.PredictAccessEnforcement(
					harness.Default(), sandboxpolicy.ImplementationTclaudeLayer,
					axes, "", "linux",
				)
				require.NoError(t, err)

				spec, err := session.BuildTclaudeLayerLaunchSpec(
					session.TclaudeLayerLaunchInput{
						HarnessName: harness.DefaultName,
						Cwd:         t.TempDir(),
						StateRoot:   t.TempDir(),
						Snapshot:    &snapshot,
					})
				require.NoError(t, err)
				plan, err := sandboxpolicy.RenderMountPlanWithEngine(
					spec.Effective, spec.Contract.NetworkEngine)
				require.NoError(t, err)

				assert.Equal(t, selection.expected, predicted.NetworkEngine,
					"preview must name the engine this policy deploys")
				assert.Equal(t, predicted.NetworkEngine, plan.NetworkEngine,
					"preview and launch must name the same engine")
			})
		}
	}
}

// TestNetworkEngineDisclosureRendersThroughTheEnforcementAPI proves the engine
// reaches the surface an operator actually reads, not just the internal
// prediction value, and that an engine-unset profile's rendering is untouched.
func TestNetworkEngineDisclosureRendersThroughTheEnforcementAPI(t *testing.T) {
	f := newFlow(t)
	discriminating := []any{
		map[string]any{"domain": "example.com", "ports": []int{443}},
	}
	for _, profile := range []struct {
		name    string
		network map[string]any
	}{
		{"engine-unset-discriminating", map[string]any{
			"baseline": "deny", "allow": discriminating,
		}},
		{"engine-proxy-discriminating", map[string]any{
			"baseline": "deny", "allow": discriminating, "engine": "proxy",
		}},
		{"engine-proxy-latent", map[string]any{
			"baseline": "allow", "engine": "proxy",
		}},
	} {
		rec := profileReq(t, f, http.MethodPost, "/v1/sandbox-profiles",
			map[string]any{
				"name": profile.name, "filesystem": []any{}, "environment": []any{},
				"network": profile.network,
			})
		require.Equalf(t, http.StatusCreated, rec.Code, "body=%s", rec.Body.String())
	}

	// The target is a parameter rather than a constant because activation is
	// per harness: reading the rendered surface for only one of them would let
	// a second activated harness disclose something else entirely and nothing
	// here would notice.
	detailForTarget := func(t *testing.T, name, target string) string {
		t.Helper()
		rec := profileReq(t, f, http.MethodGet,
			"/v1/sandbox-profiles/"+name+"/enforcement?for="+target, nil)
		require.Equalf(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
		var got struct {
			Targets []struct {
				Axes harness.PredictedAccessAxes `json:"axes"`
			} `json:"targets"`
		}
		testharness.DecodeJSON(t, rec, &got)
		require.Len(t, got.Targets, 1)
		return got.Targets[0].Axes.Network.Detail
	}
	detailFor := func(t *testing.T, name string) string {
		t.Helper()
		return detailForTarget(t, name, "tclaude-layer%2Fclaude%2Flinux")
	}

	// Unset says nothing about an engine at all. This is the parity half: an
	// operator who never touched the field reads exactly what they read before.
	unset := detailFor(t, "engine-unset-discriminating")
	assert.NotContains(t, unset, "Filtering engine")
	assert.NotContains(t, unset, "Proxy filter")
	assert.Contains(t, unset, "pasta",
		"an unset engine still describes the packet gateway's launch checks")

	// A deployed and ACTIVATED proxy names the engine and discloses what it
	// carries. The not-yet-activated sentence has retired — leaving it beside
	// cells that now say Full would tell the operator nothing is enforced while
	// the row says otherwise — and the launch condition is the proxy floor's
	// own, which refuses rather than widens.
	proxy := detailFor(t, "engine-proxy-discriminating")
	assert.Contains(t, proxy, "Filtering engine: Proxy filter")
	assert.Contains(t, proxy, "SOCKS5")
	assert.Contains(t, proxy, "blocked rather than filtered")
	// The notice says "not activated for this target"; asserting on the
	// spelling it does not use would pass however this rendered.
	assert.NotContains(t, proxy, "not activated")
	assert.Contains(t, proxy, "enforces the network allow list",
		"an activated proxy engine states that it enforces")
	assert.Contains(t, proxy, harness.ProxyEngineLaunchCondition,
		"the launch condition must be the proxy floor's own")
	assert.NotContains(t, proxy, "supervised DNS/pasta/nftables",
		"a proxy-engine posture must not claim the packet gateway's mechanism")
	assert.NotContains(t, proxy, "outbound traffic is open",
		"this floor refuses a launch it cannot build; it does not widen it")

	// Codex is activated by TCL-888 on the evidence TCL-884 named, so the same
	// profile read for a Codex target must render the SAME activated surface.
	// Asserted through the API rather than inferred from the shared code path,
	// because the rendered surface is the thing an operator reads, and it is
	// what has to stop saying "not activated" the moment the cells flip.
	//
	// OpenCode is read beside it and must still carry the not-activated
	// sentence: that contrast is the activation rule visible at the surface,
	// and it is what would fail if a flip ever leaked past its record.
	codex := detailForTarget(t, "engine-proxy-discriminating",
		"tclaude-layer%2Fcodex%2Flinux")
	assert.Equal(t, proxy, codex,
		"an activated Codex target renders the same proxy-engine disclosure")
	assert.NotContains(t, codex, "not activated")

	// TCL-891 activates OpenCode from its own floor smoke, so the same profile
	// read for an OpenCode target must now render the activated surface too —
	// with ONE addition nobody else carries. The per-harness carriage sentence
	// is the measured fact that this client uses HTTP CONNECT only, which is
	// why the row is read here rather than assumed to equal its neighbours'.
	openCode := detailForTarget(t, "engine-proxy-discriminating",
		"tclaude-layer%2Fopencode%2Flinux")
	assert.NotContains(t, openCode, "not activated",
		"OpenCode has an activation record and must stop disclosing that it does not")
	assert.Contains(t, openCode, harness.ProxyEngineCarriageNotice)
	assert.Contains(t, openCode, harness.ProxyEngineOpenCodeCarriageNotice,
		"the per-harness carriage fact must reach the surface an operator reads")
	assert.NotEqual(t, proxy, openCode,
		"OpenCode's surface differs from the plain-CLI harnesses' by exactly that sentence")
	assert.NotContains(t, codex, harness.ProxyEngineOpenCodeCarriageNotice,
		"a harness whose tool egress is measured over both carriages must not claim SOCKS is unreachable")

	// The not-activated sentence must still be REACHABLE through this API, or
	// the assertion above degenerates into "nothing ever says it". Darwin is
	// the real, permanent subject for that until the M3 Seatbelt smokes exist:
	// no harness has a Darwin record, and the platform is part of the target.
	darwin := detailForTarget(t, "engine-proxy-discriminating",
		"tclaude-layer%2Fopencode%2Fdarwin")
	assert.Contains(t, darwin, "not activated",
		"an unactivated platform must still disclose that through the same surface")

	// A selection on a policy that needs no filtering is latent, not an error.
	latent := detailFor(t, "engine-proxy-latent")
	assert.Contains(t, latent, "needs no filtering engine")
	assert.Contains(t, latent, "would apply if you add")
}
