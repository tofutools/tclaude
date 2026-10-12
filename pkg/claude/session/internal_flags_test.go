package session

import (
	"strings"
	"testing"
)

func TestNewCmdHelpOmitsInternalFlags(t *testing.T) {
	cmd := NewCmd()
	usage := cmd.UsageString()
	if strings.Contains(usage, internalFlagPrefix) {
		t.Fatalf("session new --help shows internal flags:\n%s", usage)
	}
	if !strings.Contains(usage, "--harness") {
		t.Fatalf("session new --help lost its operator flags:\n%s", usage)
	}
	// Hidden flags must still parse.
	if err := cmd.Flags().Parse([]string{"--managed-launch", "--sandbox-chosen-by", "explicit"}); err != nil {
		t.Fatalf("internal flags no longer parse: %v", err)
	}
}
