package session

import (
	"strings"
	"testing"
)

// TestBuildClaudeCmd_Model is the acceptance check for the regular
// `tclaude session new` surface: an unset model must NOT add --model
// to the claude invocation (claude keeps its own default), and an
// explicit alias must append `--model <alias>`.
func TestBuildClaudeCmd_Model(t *testing.T) {
	// Unset → no --model anywhere in the command.
	if got := buildClaudeCmd("", "", "", "", nil); strings.Contains(got, "--model") {
		t.Fatalf("unset model must omit --model, got %q", got)
	}

	// Set → `--model <alias>` appended.
	if got := buildClaudeCmd("", "", "", "opus", nil); !strings.Contains(got, "--model opus") {
		t.Fatalf("set model must append --model opus, got %q", got)
	}

	// The [1m] aliases contain sh glob characters; the command is run
	// via `sh -c`, so they must arrive quoted.
	got := buildClaudeCmd("", "", "", "sonnet[1m]", nil)
	if !strings.Contains(got, `--model 'sonnet[1m]'`) && !strings.Contains(got, `--model "sonnet[1m]"`) {
		t.Fatalf("[1m] model must be shell-quoted, got %q", got)
	}

	// Coexists with --resume, --effort and post-`--` passthrough args.
	got = buildClaudeCmd("", "conv-123", "max", "fable", []string{"--foo", "bar baz"})
	if !strings.Contains(got, "--resume conv-123") {
		t.Fatalf("expected --resume conv-123, got %q", got)
	}
	if !strings.Contains(got, "--effort max") {
		t.Fatalf("expected --effort max, got %q", got)
	}
	if !strings.Contains(got, "--model fable") {
		t.Fatalf("expected --model fable, got %q", got)
	}
}
