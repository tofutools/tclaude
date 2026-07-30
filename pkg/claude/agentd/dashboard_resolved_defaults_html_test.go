package agentd

import (
	"io/fs"
	"path"
	"strings"
	"testing"
)

// resolvedDefaultsCanonicalModule owns every phrase for the dashboard's two
// resolution concepts.
const resolvedDefaultsCanonicalModule = "js/resolved-defaults.js"

// resolvedDefaultsConsumers are the surfaces the TCL-865 ticket names. Each one
// must take its copy from the canonical module rather than writing its own.
var resolvedDefaultsConsumers = []string{
	"js/agent-spawn-island.js",            // launch/spawn dialog
	"js/management-island.js",             // spawn-profile + sandbox-profile editors
	"js/sandbox-profile-preview.js",       // the launch dialog's composed-policy line
	"js/groups-list.js",                   // group default-profile / sandbox-profile chips
	"js/toolbar-profile-picker-island.js", // global default-profile / sandbox-profile chips
	"js/profiles.js",                      // spawn-profile summaries
	"js/roles.js",                         // role summaries
	"js/template-management-island.js",    // template roster summaries
}

// TestDashboardHTML_ResolvedDefaultsVocabulary pins the two sentences the
// dashboard is allowed to use for its two resolution concepts, and the fact
// that every surface takes them from one module rather than writing its own.
//
// The failure this guards against is not a typo. Before TCL-865 the same
// mechanism was called one thing in one selector and something else in the next,
// while a bare `inherit` stood for "Claude's settings decide" — so an operator
// could not tell which "default" was a LAUNCH PARAMETER and which was a COMPOSED
// SANDBOX RULE. A surface that reintroduces private copy for either concept
// fails here.
func TestDashboardHTML_ResolvedDefaultsVocabulary(t *testing.T) {
	for needle, why := range map[string]string{
		`export const RESOLVED_DEFAULTS_LABEL = 'Resolved defaults';`:                                                                   "one canonical phrase for launch-parameter resolution",
		`explicit launch choice → named spawn profile → group default spawn profile → global default spawn profile → harness default.`:  "the launch chain is stated in full tier order",
		`group default spawn profile → global default spawn profile → harness default.`:                                                 "the preview states the shorter chain it can actually walk",
		`the global sandbox profile, the group sandbox profile, and an explicit sandbox profile when one is chosen all apply together.`: "every applicable sandbox layer applies, none wins outright",
		`export const SANDBOX_PROFILE_LAYERS_LABEL = 'Composed sandbox-profile layers';`:                                                "one canonical phrase for the composed layers",

		// The launch dialog and the profile editor both read the canonical copy.
		`<option value="">${RESOLVED_DEFAULTS_LABEL}</option>`:        "the evaluation harness selector names resolved defaults",
		`${RESOLVED_DEFAULTS_LABEL} (this host)`:                      "the platform selector narrows the same phrase instead of coining another",
		`${resolvedDefaultOption(resolvedSandboxImplLabel)}`:          "the spawn dialog's implementation row names the resolved implementation, not the chain that produced it",
		"`— ${RESOLVED_DEFAULT_LABEL} (${answer}) —`":                 "a named resolved default reads as one value, not a mechanism",
		`launchDefaults?.implementation`:                              "the named value comes from the daemon, not from a client-side guess",
		"`Unset (${RESOLVED_DEFAULTS_LABEL.toLowerCase()} at spawn)`": "the profile editor's implementation row names resolved defaults",
		`<label title=${EVALUATION_TARGET_TITLE}>Agent harness`:       "the target controls carry the explanation as a tooltip, not a paragraph",

		// Sandbox-policy composition stays distinguishable from those defaults.
		`'— composed global + group sandbox profiles —'`:     "the spawn dialog's sandbox-profile option names composition, not a default",
		`id="sandbox-profile-editor-policy-layers"`:          "the composed layers are visible without opening a disclosure",
		`sandboxProfileLayersText(selectedEffective.context`: "the visible layer row names each scope's profile",
		`sandboxProfileLayersInline(`:                        "the flat preview line brackets its layer list so it cannot run into the next section",

		// `inherit` never stands alone as if it explained itself.
		`export const CLAUDE_INHERIT_SANDBOX_LABEL = 'Claude settings decide (inherit)';`: "Claude's sandbox mode reads as plain language, token retained as detail",
		`sandboxModeOptionLabel(draft.harness, mode, recommended)`:                        "the spawn dialog's sandbox modes go through the labeller",
		`label: sandboxModeOptionLabel(draft.harness, value, hEntry.default_sandbox)`:     "the profile editor's sandbox modes go through the same labeller",
		`sandboxModeDetail(target.target.harness, target.target.sandbox)`:                 "a reported resolved mode is explained, not echoed",
		`' (enabled only if Claude settings enable it)'`:                                  "naming the implementation owner never asserts the sandbox is on",
		"`sandbox ${sandboxModeLabel(rl.harness || 'claude', rl.sandbox)}`":               "role summaries explain the mode they print",
		"`sandbox ${sandboxModeLabel(agent.harness || 'claude', agent.sandbox)}`":         "template roster summaries explain the mode they print",
		`text('sandbox', sandboxModeLabel(p.harness || 'claude', p.sandbox))`:             "spawn-profile detail chips explain the mode they print",
	} {
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("dashboard missing %q (%s)", needle, why)
		}
	}

	// The retired phrasings. These are plain literals with no canonical source,
	// so re-adding one is exactly the regression they name.
	for _, retired := range []string{
		`Resolved default target`,
		`Resolved with harness`,
		`Resolved current host`,
		`Dashboard default spawn profile`,
	} {
		if strings.Contains(dashboardAssets, retired) {
			t.Errorf("retired resolution wording remains: %q", retired)
		}
	}

	// Every named surface IMPORTS the canonical copy. File-scoped, because a
	// match anywhere in the concatenated assets would pass even if one surface
	// had quietly inlined its own string.
	for _, module := range resolvedDefaultsConsumers {
		if !strings.Contains(dashboardAssetFile(t, module), "from './resolved-defaults.js'") {
			t.Errorf("%s no longer takes its resolution copy from the canonical module", module)
		}
	}
}

// TestDashboardHTML_ResolvedDefaultsPhrasesHaveOneHome is the tripwire the
// import check alone cannot be: importing the module does not stop a file from
// ALSO hand-writing one of its phrases a few lines later. Each canonical phrase
// may appear as a literal in exactly one place — the module that defines it.
func TestDashboardHTML_ResolvedDefaultsPhrasesHaveOneHome(t *testing.T) {
	phrases := []string{
		"Resolved defaults",
		"Composed sandbox-profile layers",
		"Claude settings decide",
	}
	err := fs.WalkDir(dashboardAssetsFS, "js", func(name string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() || path.Ext(name) != ".js" || name == resolvedDefaultsCanonicalModule {
			return err
		}
		body := dashboardAssetFile(t, name)
		for _, phrase := range phrases {
			if strings.Contains(body, phrase) {
				t.Errorf("%s writes the canonical phrase %q itself; import it from %s instead",
					name, phrase, resolvedDefaultsCanonicalModule)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk dashboard js: %v", err)
	}
}
