package session

import (
	"strings"
	"testing"
)

// TestBuildClaudeCmd_Effort is the acceptance check for the regular
// `tclaude session new` surface: an unset effort must NOT add --effort
// to the claude invocation (claude keeps its own default), and an
// explicit level must append `--effort <level>` verbatim.
func TestBuildClaudeCmd_Effort(t *testing.T) {
	// Unset → no --effort anywhere in the command.
	if got := buildClaudeCmd("", "", "", nil); strings.Contains(got, "--effort") {
		t.Fatalf("unset effort must omit --effort, got %q", got)
	}

	// Set → `--effort <level>` appended.
	if got := buildClaudeCmd("", "", "high", nil); !strings.Contains(got, "--effort high") {
		t.Fatalf("set effort must append --effort high, got %q", got)
	}

	// Coexists with --resume and post-`--` passthrough args.
	got := buildClaudeCmd("", "conv-123", "max", []string{"--foo", "bar baz"})
	if !strings.Contains(got, "--resume conv-123") {
		t.Fatalf("expected --resume conv-123, got %q", got)
	}
	if !strings.Contains(got, "--effort max") {
		t.Fatalf("expected --effort max, got %q", got)
	}
}
