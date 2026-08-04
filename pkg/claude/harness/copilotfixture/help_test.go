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

func TestModelsFromHelpFixture(t *testing.T) {
	help, err := os.ReadFile(filepath.Join("testdata", PinnedCLIVersion, "help.txt"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := ModelsFromHelp(help)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"claude-sonnet-5", "claude-sonnet-4.6", "claude-sonnet-4.5",
		"claude-haiku-4.5", "claude-fable-5", "claude-opus-5",
		"claude-opus-4.8", "claude-opus-4.8-fast", "claude-opus-4.7",
		"claude-opus-4.6", "claude-opus-4.5", "gpt-5.6-sol", "gpt-5.6-terra",
		"gpt-5.6-luna", "gpt-5.5", "gpt-5.4", "gpt-5.3-codex", "gpt-5.4-mini",
		"gpt-5-mini", "mai-code-1-flash-picker", "gemini-3.1-pro-preview",
		"gemini-3.6-flash", "gemini-3.5-flash", "grok-4.5", "kimi-k2.7-code",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("ModelsFromHelp() = %v, want %v", got, want)
	}
}

func TestModelsFromHelpRejectsMissingConfigKey(t *testing.T) {
	if _, err := ModelsFromHelp([]byte("Usage: copilot [options]")); err == nil {
		t.Fatal("ModelsFromHelp() accepted help without a model config key")
	}
}

func TestValidateHelpModels(t *testing.T) {
	help, err := os.ReadFile(filepath.Join("testdata", PinnedCLIVersion, "help.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateHelpModels(help, help); err != nil {
		t.Fatal(err)
	}
	changed := []byte(strings.Replace(string(help), `"claude-sonnet-5"`, `"unexpected-model"`, 1))
	if err := ValidateHelpModels(changed, help); err == nil {
		t.Fatal("ValidateHelpModels() accepted changed choices")
	}
}
