package processcmd

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/process/store/storetest"
)

func TestRunVerifyRendersEvidenceCrashReport(t *testing.T) {
	fixture := storetest.BuildCrashFixture(t, storetest.CrashAfterManifestBeforeState)
	var out bytes.Buffer

	err := runVerify(context.Background(), &verifyParams{RunID: fixture.RunID, StoreRoot: fixture.Root}, &out)
	if err == nil {
		t.Fatal("expected verify error")
	}
	text := out.String()
	for _, want := range []string{
		"Effective status: inconsistent",
		"evidence:",
		"state_behind_manifest",
		"crash between manifest append and state checkpoint write",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("report missing %q:\n%s", want, text)
		}
	}
}

func TestRunVerifyRendersSemanticDirtyReport(t *testing.T) {
	fixture := storetest.BuildSemanticViolationFixture(t)
	var out bytes.Buffer

	err := runVerify(context.Background(), &verifyParams{RunID: fixture.RunID, StoreRoot: fixture.Root}, &out)
	if err == nil {
		t.Fatal("expected verify error")
	}
	text := out.String()
	for _, want := range []string{
		"Dirty: yes",
		"Effective status: dirty",
		"semantic:",
		"running_attempt_without_command_or_actor",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("report missing %q:\n%s", want, text)
		}
	}
}

func TestRunVerifyRendersLogAheadManifestCrashReport(t *testing.T) {
	fixture := storetest.BuildCrashFixture(t, storetest.CrashAfterLogBeforeManifest)
	var out bytes.Buffer

	err := runVerify(context.Background(), &verifyParams{RunID: fixture.RunID, StoreRoot: fixture.Root}, &out)
	if err == nil {
		t.Fatal("expected verify error")
	}
	text := out.String()
	for _, want := range []string{
		"Effective status: inconsistent",
		"log_ahead_of_manifest",
		"crash between node log append and manifest append",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("report missing %q:\n%s", want, text)
		}
	}
}

func TestRunVerifyMissingStoreRootDoesNotCreateDirectory(t *testing.T) {
	root := filepath.Join(t.TempDir(), "missing")
	err := runVerify(context.Background(), &verifyParams{RunID: "run", StoreRoot: root}, ioDiscard{})
	if err == nil || !strings.Contains(err.Error(), "process store root does not exist") {
		t.Fatalf("expected missing root error, got %v", err)
	}
	if _, statErr := os.Stat(root); !os.IsNotExist(statErr) {
		t.Fatalf("verify created missing store root or unexpected stat error: %v", statErr)
	}
}

func TestProcessFeatureGateFailsClosed(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if err := requireProcessesEnabled(); err == nil {
		t.Fatal("expected process feature gate to fail closed")
	}

	if err := os.MkdirAll(filepath.Dir(config.ConfigPath()), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(config.ConfigPath(), []byte(`{"features":{"processes":true}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := requireProcessesEnabled(); err != nil {
		t.Fatalf("expected enabled process feature gate, got %v", err)
	}
}

type ioDiscard struct{}

func (ioDiscard) Write(p []byte) (int, error) {
	return len(p), nil
}
