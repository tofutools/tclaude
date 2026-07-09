package processcmd

import (
	"bytes"
	"context"
	"strings"
	"testing"

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
		"semantic:",
		"running_attempt_without_command_or_actor",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("report missing %q:\n%s", want, text)
		}
	}
}
