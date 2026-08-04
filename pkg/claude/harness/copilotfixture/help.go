package copilotfixture

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// PinnedEffortHelpFixture is the sanitized excerpt captured from
// `copilot --help` for PinnedCLIVersion. Keeping the path beside the version
// pin makes a CLI bump require an explicit fixture update.
const PinnedEffortHelpFixture = "testdata/" + PinnedCLIVersion + "/help.txt"

var (
	effortChoicesRE = regexp.MustCompile(`(?s)--effort,\s+--reasoning-effort\s+<level>.*?\(choices:\s*((?:"[^"]+"\s*,?\s*)+)\)`)
	quotedValueRE   = regexp.MustCompile(`"([^"]+)"`)
)

// EffortLevelsFromHelp extracts the advertised choices for Copilot's
// --effort option from a help page. It intentionally parses the CLI-shaped
// evidence instead of repeating the list as a second test-only constant.
func EffortLevelsFromHelp(help []byte) ([]string, error) {
	match := effortChoicesRE.FindSubmatch(help)
	if len(match) != 2 {
		return nil, fmt.Errorf("Copilot help does not contain an --effort choices list")
	}

	quoted := quotedValueRE.FindAllSubmatch(match[1], -1)
	if len(quoted) == 0 {
		return nil, fmt.Errorf("Copilot --effort choices list is empty")
	}
	levels := make([]string, 0, len(quoted))
	for _, value := range quoted {
		level := string(value[1])
		if slices.Contains(levels, level) {
			return nil, fmt.Errorf("Copilot --effort choices list repeats %q", level)
		}
		levels = append(levels, level)
	}
	return levels, nil
}

// ValidateHelpEffortLevels compares a live help page with the pinned
// sanitized fixture. The caller can use it in a smoke test after asserting
// the installed CLI version.
func ValidateHelpEffortLevels(live, fixture []byte) error {
	liveLevels, err := EffortLevelsFromHelp(live)
	if err != nil {
		return fmt.Errorf("parse live Copilot help: %w", err)
	}
	fixtureLevels, err := EffortLevelsFromHelp(fixture)
	if err != nil {
		return fmt.Errorf("parse pinned Copilot help fixture: %w", err)
	}
	if !slices.Equal(liveLevels, fixtureLevels) {
		return fmt.Errorf("Copilot --effort choices drifted: live %s, fixture %s",
			strings.Join(liveLevels, ", "), strings.Join(fixtureLevels, ", "))
	}
	return nil
}
