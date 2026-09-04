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

// TestCopilotModelCatalogMatchesPinnedHelpFixture keeps the production model
// suggestions tied to the sanitized evidence. `auto` is tclaude's stable
// convenience choice and therefore intentionally precedes the concrete ids
// documented by Copilot. Announced models may lead their vendor's released
// choices before the pinned CLI fixture advertises them; name those exceptions
// explicitly so the rest of the catalog remains exact.
func TestCopilotModelCatalogMatchesPinnedHelpFixture(t *testing.T) {
	help, err := os.ReadFile(copilotfixture.PinnedModelHelpFixture)
	if err != nil {
		t.Fatal(err)
	}
	advertised, err := copilotfixture.ModelsFromHelp(help)
	if err != nil {
		t.Fatal(err)
	}
	want := append([]string{"auto"}, advertised...)
	h := harness.MustGet(harness.CopilotName)
	got := h.Models.Models()
	for _, announced := range []string{"gpt-6-astra"} {
		if !slices.Contains(got, announced) {
			t.Fatalf("Copilot Models() = %v, missing announced model %q", got, announced)
		}
		got = slices.DeleteFunc(got, func(model string) bool { return model == announced })
	}
	if !slices.Equal(got, want) {
		t.Fatalf("Copilot Models() = %v, want pinned help choices plus auto %v", got, want)
	}
}
