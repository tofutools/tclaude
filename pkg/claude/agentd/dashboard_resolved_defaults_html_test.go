package agentd

import (
	"strings"
	"testing"
)

// TestDashboardHTML_ResolvedDefaultsVocabulary pins the two sentences the
// dashboard is allowed to use for its two resolution concepts, and the fact
// that both the launch dialog and the sandbox-profile editor take them from one
// module rather than each writing its own.
//
// The failure this guards against is not a typo. Before TCL-865 the same
// mechanism was called "Resolved default target" in one selector, "Resolved
// with harness" in the next, and "Resolved current host" in the third, while a
// bare `inherit` stood for "Claude's settings decide" — so an operator could not
// tell which "default" was a LAUNCH PARAMETER and which was a COMPOSED SANDBOX
// RULE. Any surface that reintroduces private copy for either concept fails
// here.
func TestDashboardHTML_ResolvedDefaultsVocabulary(t *testing.T) {
	for needle, why := range map[string]string{
		`export const RESOLVED_DEFAULTS_LABEL = 'Resolved defaults';`:                                                                   "one canonical phrase for launch-parameter resolution",
		`explicit launch choice → named spawn profile → group default spawn profile → global default spawn profile → harness default.`:  "the launch chain is stated in full tier order",
		`the global sandbox profile, the group sandbox profile, and an explicit sandbox profile when one is chosen all apply together.`: "every applicable sandbox layer applies, none wins outright",
		`export const SANDBOX_PROFILE_LAYERS_LABEL = 'Composed sandbox-profile layers';`:                                                "one canonical phrase for the composed layers",

		// The launch dialog and the profile editor both read the canonical copy.
		`<option value="">${RESOLVED_DEFAULTS_LABEL}</option>`:             "the evaluation harness selector names resolved defaults",
		`${RESOLVED_DEFAULTS_LABEL} (this host)`:                           "the platform selector narrows the same phrase instead of coining another",
		`— ${RESOLVED_DEFAULTS_LABEL} (${view.sandboxImplInheritLabel}) —`: "the spawn dialog's implementation row names resolved defaults",
		"`Unset (${RESOLVED_DEFAULTS_LABEL.toLowerCase()} at spawn)`":      "the profile editor's implementation row names resolved defaults",
		`id="sandbox-profile-editor-evaluate-intro"`:                       "the target controls carry the launch-chain explanation",

		// Sandbox-policy composition stays distinguishable from those defaults.
		`'— composed global + group sandbox profiles —'`:     "the spawn dialog's sandbox-profile option names composition, not a default",
		`id="sandbox-profile-editor-policy-layers"`:          "the composed layers are visible without opening a disclosure",
		`sandboxProfileLayersText(selectedEffective.context`: "the visible layer row names each scope's profile",

		// `inherit` never stands alone as if it explained itself.
		`export const CLAUDE_INHERIT_SANDBOX_LABEL = 'Claude settings decide (inherit)';`: "Claude's sandbox mode reads as plain language, token retained as detail",
		`labelFor: (mode) => sandboxModeLabel(draft.harness, mode)`:                       "the spawn dialog's sandbox modes go through the labeller",
		`label: sandboxModeLabel(draft.harness, value)`:                                   "the profile editor's sandbox modes go through the same labeller",
		`sandboxModeDetail(target.target.harness, target.target.sandbox)`:                 "a reported resolved mode is explained, not echoed",
		`' (enabled only if Claude settings enable it)'`:                                  "naming the implementation owner never asserts the sandbox is on",
	} {
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("dashboard missing %q (%s)", needle, why)
		}
	}

	// The retired phrasings. Each one described a resolution the canonical copy
	// now covers; a surface that brings one back has forked the vocabulary again.
	for _, retired := range []string{
		`Resolved default target`,
		`Resolved with harness`,
		`Resolved current host`,
		`— inherit global + group profiles —`,
		`Unset (inherit at spawn)`,
	} {
		if strings.Contains(dashboardAssets, retired) {
			t.Errorf("retired resolution wording remains: %q", retired)
		}
	}

	// Both dialogs must IMPORT the canonical copy. A file-scoped check, because a
	// match anywhere in the concatenated assets would pass even if one surface
	// had quietly inlined its own string.
	for _, module := range []string{"js/agent-spawn-island.js", "js/management-island.js"} {
		if !strings.Contains(dashboardAssetFile(t, module), "from './resolved-defaults.js'") {
			t.Errorf("%s no longer takes its resolution copy from the canonical module", module)
		}
	}
}
