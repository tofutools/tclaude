package copilotfixture

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestEffortLevelsFromHelpFixture(t *testing.T) {
	help, err := os.ReadFile(filepath.Join("testdata", PinnedCLIVersion, "help.txt"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := EffortLevelsFromHelp(help)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"none", "minimal", "low", "medium", "high", "xhigh", "max"}
	if !slices.Equal(got, want) {
		t.Fatalf("EffortLevelsFromHelp() = %v, want %v", got, want)
	}
}

func TestEffortLevelsFromHelpRejectsMissingFlag(t *testing.T) {
	if _, err := EffortLevelsFromHelp([]byte("Usage: copilot [options]")); err == nil {
		t.Fatal("EffortLevelsFromHelp() accepted help without --effort")
	}
}

func TestValidateHelpEffortLevels(t *testing.T) {
	help, err := os.ReadFile(filepath.Join("testdata", PinnedCLIVersion, "help.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateHelpEffortLevels(help, help); err != nil {
		t.Fatal(err)
	}
	changed := []byte(strings.Replace(string(help), `"none"`, `"unexpected"`, 1))
	if err := ValidateHelpEffortLevels(changed, help); err == nil {
		t.Fatal("ValidateHelpEffortLevels() accepted changed choices")
	}
}
