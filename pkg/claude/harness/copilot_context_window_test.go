package harness

import "testing"

func TestCopilotContextWindowDefault(t *testing.T) {
	tests := []struct {
		model string
		want  int64
	}{
		{"gpt-5.6-sol", 272_000},
		{"gpt-5.6-custom", 272_000},
		{"claude-sonnet-4.6", 1_000_000},
		{"claude-opus-5", 1_000_000},
		{"gpt-5.5", 200_000},
		{"gemini-3.1-pro-preview", 200_000},
		{"", 0},
	}
	for _, tt := range tests {
		if got := CopilotContextWindowDefault(tt.model); got != tt.want {
			t.Errorf("CopilotContextWindowDefault(%q) = %d; want %d", tt.model, got, tt.want)
		}
	}
}

func TestResolveCopilotContextWindowHarnessGate(t *testing.T) {
	copilot, err := Resolve(CopilotName)
	if err != nil {
		t.Fatal(err)
	}
	claude, err := Resolve(DefaultName)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := ResolveCopilotContextWindow(copilot, 272_000); err != nil || got != 272_000 {
		t.Fatalf("configured Copilot max = %d, %v; want 272000, nil", got, err)
	}
	if got, err := ResolveCopilotContextWindow(copilot, 0); err != nil || got != 0 {
		t.Fatalf("unset Copilot max = %d, %v; want 0, nil", got, err)
	}
	if _, err := ResolveCopilotContextWindow(claude, 272_000); err == nil {
		t.Fatal("non-Copilot context max unexpectedly accepted")
	}
}
