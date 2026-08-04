package copilotfixture_test

import (
	"os"
	"slices"
	"testing"

	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/claude/harness/copilotfixture"
)

// TestCopilotEffortCatalogMatchesPinnedHelpFixture keeps the production
// catalog tied to the sanitized evidence. The fixture smoke test below then
// compares that evidence with a live 1.0.77 binary when the optional CLI job
// is enabled.
func TestCopilotEffortCatalogMatchesPinnedHelpFixture(t *testing.T) {
	help, err := os.ReadFile(copilotfixture.PinnedEffortHelpFixture)
	if err != nil {
		t.Fatal(err)
	}
	advertised, err := copilotfixture.EffortLevelsFromHelp(help)
	if err != nil {
		t.Fatal(err)
	}
	h := harness.MustGet(harness.CopilotName)
	if got := h.Models.EffortLevels(); !slices.Equal(got, advertised) {
		t.Fatalf("Copilot EffortLevels() = %v, want pinned help choices %v", got, advertised)
	}
}
