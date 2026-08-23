package session

import (
	"reflect"
	"testing"

	"github.com/tofutools/tclaude/pkg/claude/harness"
)

func TestConfigureTmuxPassthrough(t *testing.T) {
	tests := []struct {
		name    string
		harness *harness.Harness
		want    [][]string
	}{
		{
			name:    "copilot",
			harness: mustTestHarness(t, harness.CopilotName),
			want: [][]string{{
				"set-option", "-t", "=sess-harness:", "allow-passthrough", "on",
			}},
		},
		{
			name:    "opencode",
			harness: mustTestHarness(t, harness.OpenCodeName),
			want: [][]string{{
				"set-option", "-t", "=sess-harness:", "allow-passthrough", "on",
			}},
		},
		{name: "claude", harness: harness.Default()},
		{name: "codex", harness: mustTestHarness(t, harness.CodexName)},
		{name: "bare", harness: &harness.Harness{Name: "bare"}},
		{name: "nil", harness: nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := withRecordingTmux(t)
			ConfigureTmuxPassthrough("sess-harness", tt.harness)
			if !reflect.DeepEqual(rec.calls, tt.want) {
				t.Fatalf("tmux passthrough config = %v, want %v", rec.calls, tt.want)
			}
		})
	}
}

func mustTestHarness(t *testing.T, name string) *harness.Harness {
	t.Helper()
	h, ok := harness.Get(name)
	if !ok {
		t.Fatalf("%s harness not registered", name)
	}
	return h
}
