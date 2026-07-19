package agentd

import (
	"encoding/json"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// The spawn and profile dialogs collapse per-mode help behind a [?] but keep
// the text from a "⚠" onward permanently visible, so the marker's placement is
// load-bearing copy, not decoration. These tests pin the convention on the Go
// side and hand the real strings to the browser tests, which would otherwise
// assert against hand-typed literals that drift from the harness.

const modeHelpFixturePath = "jstest/mode-help-fixture.json"

// collectModeHelp returns every mode-help string the dashboard can render,
// keyed harness/axis/mode, straight off the descriptors the catalog serves.
func collectModeHelp(t *testing.T) map[string]string {
	t.Helper()
	help := map[string]string{}
	for _, entry := range buildHarnessCatalog() {
		for mode, text := range entry.SandboxModeHelp {
			help[entry.Name+"/sandbox/"+mode] = text
		}
		for mode, text := range entry.ApprovalModeHelp {
			help[entry.Name+"/approval/"+mode] = text
		}
		for mode, text := range entry.AskTimeoutModeHelp {
			help[entry.Name+"/ask_timeout/"+mode] = text
		}
	}
	require.NotEmpty(t, help, "the catalog must expose mode help")
	return help
}

// TestModeHelpCaveatConvention is the guard the dashboard relies on: the
// dashboard shows everything from the ⚠ to the end of the string, so a second
// marker or neutral prose trailing the warning would either split the caveat
// or end a warning on a reassuring note.
func TestModeHelpCaveatConvention(t *testing.T) {
	for key, text := range collectModeHelp(t) {
		if strings.TrimSpace(text) == "" {
			t.Errorf("%s: mode help is empty", key)
			continue
		}
		if n := strings.Count(text, "⚠"); n > 1 {
			t.Errorf("%s: %d ⚠ markers; the dashboard shows from the first to the end, so a caveat must be a single trailing run: %q", key, n, text)
		}
		marker := strings.Index(text, "⚠")
		if marker < 0 {
			continue
		}
		// The caveat runs to the end of the string. Anything after the last
		// sentence-ending punctuation that reads as a fresh recommendation
		// would be shown as part of the warning.
		caveat := strings.TrimSpace(text[marker:])
		if len(caveat) < 3 {
			t.Errorf("%s: ⚠ carries no warning text: %q", key, text)
		}
		for _, reassuring := range []string{"Recommended", "recommended for"} {
			if strings.Contains(caveat, reassuring) {
				t.Errorf("%s: the ⚠ caveat ends by recommending the mode, which the dialog shows verbatim: %q", key, caveat)
			}
		}
	}
}

// TestModeHelpFixtureMatchesHarness keeps jstest/mode-help-fixture.json in step
// with the harness descriptors. The browser tests parse it so their helpCaveat
// assertions run against production copy; regenerate with -update when the help
// changes on purpose.
func TestModeHelpFixtureMatchesHarness(t *testing.T) {
	help := collectModeHelp(t)
	keys := make([]string, 0, len(help))
	for key := range help {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	ordered := make(map[string]string, len(help))
	for _, key := range keys {
		ordered[key] = help[key]
	}
	want, err := json.MarshalIndent(ordered, "", "  ")
	require.NoError(t, err)
	want = append(want, '\n')

	got, err := os.ReadFile(modeHelpFixturePath)
	if os.Getenv("UPDATE_MODE_HELP_FIXTURE") != "" {
		require.NoError(t, os.WriteFile(modeHelpFixturePath, want, 0o644))
		return
	}
	require.NoError(t, err, "run with UPDATE_MODE_HELP_FIXTURE=1 to create it")
	if string(got) != string(want) {
		t.Fatalf("%s is stale — rerun with UPDATE_MODE_HELP_FIXTURE=1", modeHelpFixturePath)
	}
}
