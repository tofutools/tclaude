package harness

import (
	"slices"
	"testing"
)

func TestOpenCodeAskerBuildAskArgv(t *testing.T) {
	tests := []struct {
		name string
		spec AskSpec
		want []string
	}{
		{
			name: "fresh capture with model and effort",
			spec: AskSpec{
				Print: true, Model: "openai/gpt-5.2-codex", Effort: "high", Prompt: "explain this",
			},
			want: []string{
				"opencode", "run", "--agent", "plan",
				"--model", "openai/gpt-5.2-codex", "--variant", "high",
				"--", "explain this",
			},
		},
		{
			name: "resume capture without model or effort",
			spec: AskSpec{
				Print: true, ResumeID: "ses_resume123", Prompt: "follow up",
			},
			want: []string{
				"opencode", "run", "--agent", "plan",
				"--session", "ses_resume123", "--", "follow up",
			},
		},
		{
			name: "fresh interactive inherits agent defaults and omits effort",
			spec: AskSpec{
				Model: "openai/gpt-5.2-codex", Effort: "max", Prompt: "pair with me",
			},
			want: []string{
				"opencode", "--model", "openai/gpt-5.2-codex",
				"--prompt", "pair with me",
			},
		},
		{
			name: "resume interactive",
			spec: AskSpec{
				ResumeID: "ses_resume456", Prompt: "continue",
			},
			want: []string{
				"opencode", "--session", "ses_resume456",
				"--prompt", "continue",
			},
		},
		{
			name: "capture guards leading dash prompt",
			spec: AskSpec{
				Print: true, Prompt: "--looks-like-a-flag",
			},
			want: []string{
				"opencode", "run", "--agent", "plan",
				"--", "--looks-like-a-flag",
			},
		},
		{
			name: "interactive prompt is one option value",
			spec: AskSpec{
				Prompt: "--looks-like-a-flag",
			},
			want: []string{
				"opencode", "--prompt", "--looks-like-a-flag",
			},
		},
		{
			name: "capture without prompt omits guard and positional",
			spec: AskSpec{
				Print: true,
			},
			want: []string{
				"opencode", "run", "--agent", "plan",
			},
		},
		{
			name: "interactive without prompt omits prompt option",
			spec: AskSpec{},
			want: []string{
				"opencode",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := openCodeAsker{}.BuildAskArgv(tt.spec)
			if !slices.Equal(got, tt.want) {
				t.Fatalf("BuildAskArgv:\n got %q\nwant %q", got, tt.want)
			}
		})
	}
}

func TestOpenCodeAskerCaptureSafetyPosture(t *testing.T) {
	argv := openCodeAsker{}.BuildAskArgv(AskSpec{
		Print: true, Prompt: "inspect this repository",
	})
	if !slices.Contains(argv, "--agent") || !slices.Contains(argv, "plan") {
		t.Fatalf("capture must force the best-effort Plan posture, got %q", argv)
	}
	for _, unsafe := range []string{"--auto", "--yolo", "--dangerously-skip-permissions"} {
		if slices.Contains(argv, unsafe) {
			t.Fatalf("capture must not auto-approve permissions via %q, got %q", unsafe, argv)
		}
	}
	if slices.Contains(argv, "--format") {
		t.Fatalf("buffered v1 uses default human-readable output, got %q", argv)
	}
}

func TestOpenCodeAskerIgnoresSessionID(t *testing.T) {
	for _, printMode := range []bool{true, false} {
		withID := openCodeAsker{}.BuildAskArgv(AskSpec{
			Print: printMode, SessionID: "uuid-shaped-but-unsupported", Prompt: "q",
		})
		withoutID := openCodeAsker{}.BuildAskArgv(AskSpec{
			Print: printMode, Prompt: "q",
		})
		if !slices.Equal(withID, withoutID) {
			t.Fatalf("Print=%v: SessionID must not change OpenCode argv:\n with %q\n w/o  %q",
				printMode, withID, withoutID)
		}
	}
}

func TestOpenCodeAskerCapabilities(t *testing.T) {
	h, ok := Get(OpenCodeName)
	if !ok {
		t.Fatal("OpenCode harness must be registered")
	}
	if !h.SupportsAsk() {
		t.Fatal("OpenCode harness must report SupportsAsk")
	}
	if h.SupportsAskStream() {
		t.Fatal("buffered OpenCode v1 must not report SupportsAskStream")
	}
	if h.PreMintsAskConvID() || h.Ask.PreMintsConvID() {
		t.Fatal("OpenCode ask must discover its server-minted ses_ id after the turn")
	}
	if h.Ask.NoisyCaptureStderr() {
		t.Fatal("OpenCode TTY answers share stderr with its UI and must not be hidden")
	}
}
