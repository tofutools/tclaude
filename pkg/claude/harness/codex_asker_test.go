package harness

import (
	"slices"
	"testing"
)

// TestCodexAsker_BuildAskArgv pins the exact `tclaude ask` argv shape for the
// Codex harness (JOH-252) — crucially the command fork (capture uses `codex
// exec` / `codex exec resume`; interactive uses the `codex` TUI / `codex
// resume`) and the safety posture: capture pins a read-only sandbox and skips
// the git-repo check; a captured resume sets the sandbox via `-c sandbox_mode`
// because `codex exec resume` takes no `--sandbox` flag. The prompt is always
// the trailing positional (behind `--` in capture mode), and Codex never
// pre-mints, so SessionID is ignored.
func TestCodexAsker_BuildAskArgv(t *testing.T) {
	eq := func(name string, got, want []string) {
		t.Helper()
		if !slices.Equal(got, want) {
			t.Fatalf("%s:\n got %q\nwant %q", name, got, want)
		}
	}

	// Fresh capture: `codex exec`, skip-git, read-only --sandbox, model +
	// reasoning-effort override, then the `--` guard with the prompt last.
	eq("fresh capture",
		codexAsker{}.BuildAskArgv(AskSpec{
			Print: true, Model: "gpt-5", Effort: "low", Prompt: "q?",
		}),
		[]string{"codex", "exec", "--skip-git-repo-check", "--sandbox", "read-only",
			"--model", "gpt-5", "-c", `model_reasoning_effort="low"`, "--", "q?"})

	// Resume capture: `codex exec resume <id>`, skip-git, and the read-only
	// sandbox asserted via `-c sandbox_mode` (resume takes no --sandbox flag).
	// No model/effort → those flags absent; the prompt stays behind `--`.
	eq("resume capture, no model/effort",
		codexAsker{}.BuildAskArgv(AskSpec{
			Print: true, ResumeID: "rid-1", Prompt: "follow up",
		}),
		[]string{"codex", "exec", "resume", "rid-1", "--skip-git-repo-check",
			"-c", `sandbox_mode="read-only"`, "--", "follow up"})

	// Interactive fresh: the `codex` TUI, no sandbox/skip-git (human present),
	// no `--` (it can suppress submit-at-launch), prompt trailing.
	eq("interactive fresh",
		codexAsker{}.BuildAskArgv(AskSpec{
			Model: "gpt-5", Effort: "high", Prompt: "pair on this",
		}),
		[]string{"codex", "--model", "gpt-5", "-c", `model_reasoning_effort="high"`, "pair on this"})

	// Interactive resume: `codex resume <id>` + trailing prompt.
	eq("interactive resume",
		codexAsker{}.BuildAskArgv(AskSpec{
			ResumeID: "rid-2", Prompt: "more",
		}),
		[]string{"codex", "resume", "rid-2", "more"})

	// effort "max" maps onto Codex's highest scale level (xhigh).
	eq("effort max → xhigh",
		codexAsker{}.BuildAskArgv(AskSpec{
			Print: true, Effort: "max", Prompt: "q",
		}),
		[]string{"codex", "exec", "--skip-git-repo-check", "--sandbox", "read-only",
			"-c", `model_reasoning_effort="xhigh"`, "--", "q"})
}

// TestCodexAsker_IgnoresSessionID locks in that Codex does NOT pre-mint: a
// SessionID on a fresh ask must not leak a `--session-id` (Codex has no such
// flag) and the argv must be identical to the no-SessionID fresh argv. The ask
// flow discovers the id post-run instead.
func TestCodexAsker_IgnoresSessionID(t *testing.T) {
	withID := codexAsker{}.BuildAskArgv(AskSpec{Print: true, SessionID: "sid-x", Prompt: "q"})
	withoutID := codexAsker{}.BuildAskArgv(AskSpec{Print: true, Prompt: "q"})
	if slices.Contains(withID, "--session-id") || slices.Contains(withID, "sid-x") {
		t.Fatalf("codex ask must not emit a preset session id, got %q", withID)
	}
	if !slices.Equal(withID, withoutID) {
		t.Fatalf("SessionID must not change the codex argv:\n with %q\n w/o  %q", withID, withoutID)
	}
}

// TestCodexAsker_CaptureGuardsPrompt asserts the capture-mode ordering
// invariant: every flag precedes the `--` guard and the untrusted prompt is
// the trailing positional — so a leading-dash payload (a piped `git diff`) is
// never parsed as a flag or subcommand.
func TestCodexAsker_CaptureGuardsPrompt(t *testing.T) {
	argv := codexAsker{}.BuildAskArgv(AskSpec{
		Print: true, ResumeID: "rid", Model: "gpt-5", Effort: "low", Prompt: "--oops",
	})
	dashAt := slices.Index(argv, "--")
	if dashAt < 0 {
		t.Fatalf("capture mode must emit a `--` guard, got %q", argv)
	}
	for _, flag := range []string{"--skip-git-repo-check", "--model"} {
		if i := slices.Index(argv, flag); i < 0 || i >= dashAt {
			t.Fatalf("flag %q must appear before the `--` guard (at %d), got %d", flag, dashAt, i)
		}
	}
	if argv[len(argv)-1] != "--oops" {
		t.Fatalf("prompt must be the trailing positional, got %q", argv[len(argv)-1])
	}
	// The session id is trusted (a UUID) and rides before the guard.
	if i := slices.Index(argv, "rid"); i < 0 || i >= dashAt {
		t.Fatalf("resume id must precede the `--` guard, got %q", argv)
	}
}

func TestCodexAsker_PreMintsConvID(t *testing.T) {
	var codex Asker = codexAsker{}
	var claude Asker = claudeAsker{}
	if codex.PreMintsConvID() {
		t.Fatal("codex must not pre-mint a conv-id (id only lands after the first turn)")
	}
	if !claude.PreMintsConvID() {
		t.Fatal("claude must pre-mint a conv-id (--session-id)")
	}
}

func TestCodexAsker_NoisyCaptureStderr(t *testing.T) {
	var codex Asker = codexAsker{}
	var claude Asker = claudeAsker{}
	if !codex.NoisyCaptureStderr() {
		t.Fatal("codex exec writes a verbose transcript to stderr — must be hideable")
	}
	if claude.NoisyCaptureStderr() {
		t.Fatal("claude -p keeps stderr quiet — nothing to hide")
	}
}
