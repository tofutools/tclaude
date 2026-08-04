package harness

import (
	"slices"
	"strings"
	"testing"
)

func TestCopilotAskerBuildAskArgv(t *testing.T) {
	const (
		convID  = "11111111-2222-4333-8444-555555555555"
		freshID = "66666666-7777-4888-8999-aaaaaaaaaaaa"
	)
	tests := []struct {
		name string
		spec AskSpec
		want []string
	}{
		{
			name: "fresh capture pins the conv id and keeps the streams clean",
			spec: AskSpec{Print: true, SessionID: freshID, Prompt: "explain this"},
			want: []string{
				"copilot", "--session-id", freshID,
				"--no-color", "--log-level", "none",
				"-p", "explain this",
			},
		},
		{
			name: "resume capture uses the = form",
			spec: AskSpec{Print: true, ResumeID: convID, Prompt: "follow up"},
			want: []string{
				"copilot", "--resume=" + convID,
				"--no-color", "--log-level", "none",
				"-p", "follow up",
			},
		},
		{
			name: "capture with model and effort",
			spec: AskSpec{
				Print: true, SessionID: freshID,
				Model: "claude-sonnet-4.5", Effort: "high", Prompt: "which model?",
			},
			want: []string{
				"copilot", "--session-id", freshID,
				"--model=claude-sonnet-4.5", "--effort=high",
				"--no-color", "--log-level", "none",
				"-p", "which model?",
			},
		},
		{
			name: "fresh interactive submits the question at launch",
			spec: AskSpec{SessionID: freshID, Prompt: "pair with me"},
			want: []string{"copilot", "--session-id", freshID, "-i", "pair with me"},
		},
		{
			name: "resume interactive",
			spec: AskSpec{ResumeID: convID, Model: "claude-sonnet-4.5", Prompt: "continue"},
			want: []string{
				"copilot", "--resume=" + convID, "--model=claude-sonnet-4.5",
				"-i", "continue",
			},
		},
		{
			name: "a leading-dash prompt stays one argv element",
			spec: AskSpec{Print: true, Prompt: "--- piped input (stdin) ---"},
			want: []string{
				"copilot", "--no-color", "--log-level", "none",
				"-p", "--- piped input (stdin) ---",
			},
		},
		{
			// The ask flow never sends both, and the CLI documents them as
			// incompatible: sending both would silently pick one, so a resume
			// must win over a stale pre-minted id rather than emit a pair the
			// CLI rejects.
			name: "resume wins when both ids are set",
			spec: AskSpec{Print: true, ResumeID: convID, SessionID: freshID, Prompt: "q"},
			want: []string{
				"copilot", "--resume=" + convID,
				"--no-color", "--log-level", "none",
				"-p", "q",
			},
		},
		{
			// An unset field must omit its flag entirely, so "unset" reliably
			// means "let Copilot use its own default".
			name: "nothing set emits nothing extra",
			spec: AskSpec{Print: true, Prompt: "bare"},
			want: []string{
				"copilot", "--no-color", "--log-level", "none", "-p", "bare",
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := copilotAsker{}.BuildAskArgv(tc.spec)
			if !slices.Equal(got, tc.want) {
				t.Errorf("BuildAskArgv() =\n  %q\nwant\n  %q", got, tc.want)
			}
		})
	}
}

// TestCopilotAskerNeverPromotesPermissions pins the capture posture as an
// assertion rather than a comment. Headless, Copilot denies a tool call it
// cannot get permission for, which is what makes an unattended capture
// read-only-ish by construction — and `--allow-all-tools` (or its stronger
// ambient twin) is precisely the input that would undo that. A future edit that
// added one to "make ask more useful" would silently give every one-shot
// question the ability to write the workspace.
func TestCopilotAskerNeverPromotesPermissions(t *testing.T) {
	specs := []AskSpec{
		{Print: true, Prompt: "q"},
		{Print: true, ResumeID: "11111111-2222-4333-8444-555555555555", Prompt: "q"},
		{Prompt: "q"},
	}
	forbidden := []string{
		copilotFlagAllowAllTools,
		copilotAllowAllEnv,
		"--allow-all",
		"--allow-all-paths",
		"--deny-tool",
		"--allow-tool",
	}
	for _, spec := range specs {
		argv := copilotAsker{}.BuildAskArgv(spec)
		joined := strings.Join(argv, " ")
		for _, flag := range forbidden {
			if strings.Contains(joined, flag) {
				t.Errorf("BuildAskArgv(%+v) emitted %q: %q", spec, flag, argv)
			}
		}
	}
}

// TestCopilotAskerCaptureAndInteractiveDoNotMix guards the one confusion that
// would break the surface outright: `-p` exits after the turn while `-i` keeps
// the TUI open, so a capture that emitted `-i` would never return an answer and
// an interactive turn that emitted `-p` would close the pane the human is in.
func TestCopilotAskerCaptureAndInteractiveDoNotMix(t *testing.T) {
	capture := copilotAsker{}.BuildAskArgv(AskSpec{Print: true, Prompt: "q"})
	if slices.Contains(capture, "-i") || !slices.Contains(capture, "-p") {
		t.Errorf("capture argv must use -p and never -i: %q", capture)
	}
	interactive := copilotAsker{}.BuildAskArgv(AskSpec{Prompt: "q"})
	if slices.Contains(interactive, "-p") || !slices.Contains(interactive, "-i") {
		t.Errorf("interactive argv must use -i and never -p: %q", interactive)
	}
	// The prompt is the LAST element in both forms, so no option can swallow it.
	for _, argv := range [][]string{capture, interactive} {
		if argv[len(argv)-1] != "q" {
			t.Errorf("the prompt must be the trailing argv element: %q", argv)
		}
	}
}

// TestCopilotAskerIgnoresUnmeasuredSpecFields pins the "absent, not
// approximated" guardrail: AskSpec carries fields Copilot has no measured flag
// for, and inventing a spelling for them is exactly what this adapter refuses to
// do. A brokered LaunchPosture cannot reach this asker at all (the descriptor
// leaves OneShotReplay nil), and Ephemeral has no Copilot equivalent.
func TestCopilotAskerIgnoresUnmeasuredSpecFields(t *testing.T) {
	plain := copilotAsker{}.BuildAskArgv(AskSpec{Print: true, Prompt: "q"})
	loaded := copilotAsker{}.BuildAskArgv(AskSpec{
		Print:     true,
		Prompt:    "q",
		Ephemeral: true,
		Stream:    true,
		LaunchPosture: &SpawnSpec{
			SandboxMode: SandboxReadOnly, ApprovalPolicy: CopilotApprovalAllowTools,
		},
	})
	if !slices.Equal(plain, loaded) {
		t.Errorf("unmeasured spec fields must not change the argv:\n  %q\nvs\n  %q", plain, loaded)
	}
}

func TestCopilotAskerCapabilityReports(t *testing.T) {
	if !(copilotAsker{}).PreMintsConvID() {
		t.Error("PreMintsConvID() = false, want true: --session-id pins a fresh ask's conv id")
	}
	if !(copilotAsker{}).NoisyCaptureStderr() {
		t.Error("NoisyCaptureStderr() = false, want true: -p writes a run summary to stderr")
	}
	h, ok := Get(CopilotName)
	if !ok {
		t.Fatal("the copilot harness must be registered")
	}
	if !h.SupportsAsk() {
		t.Error("SupportsAsk() = false, want true now that the ask surface is fixture-backed")
	}
	if !h.PreMintsAskConvID() {
		t.Error("PreMintsAskConvID() = false, want true")
	}
	// Buffered only: TCL-994 contracts no event-stream rendering, so the ask
	// flow must keep its plain buffered path.
	if h.SupportsAskStream() {
		t.Error("SupportsAskStream() = true, want false for the buffered Copilot ask wave")
	}
}
