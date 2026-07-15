package agentd

import (
	"slices"
	"testing"

	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// TestSessionNewArgs_EffortOmittedWhenUnset is the acceptance check for
// the spawn path's forked `tclaude session new`: with no effort chosen,
// the argv must carry no --effort flag, so claude uses its own default.
func TestSessionNewArgs_EffortOmittedWhenUnset(t *testing.T) {
	args := sessionNewArgs(clcommon.SpawnArgs{Label: "lbl", Cwd: "/tmp/x"})
	if slices.Contains(args, "--effort") {
		t.Fatalf("unset effort must omit --effort, got %v", args)
	}
}

func TestSessionArgs_ManagedLaunchMarker(t *testing.T) {
	tests := map[string][]string{
		"new":    sessionNewArgs(clcommon.SpawnArgs{Label: "lbl", Cwd: "/tmp/x"}),
		"resume": sessionResumeArgs(clcommon.SpawnArgs{ConvID: "conv-1", Cwd: "/tmp/x"}),
	}
	for name, args := range tests {
		if !slices.Contains(args, "--managed-launch") {
			t.Fatalf("%s: agentd must mark its already-resolved session launch, got %v", name, args)
		}
	}
}

func TestSessionNewArgs_InternalWriteProofFlags(t *testing.T) {
	bare := sessionNewArgs(clcommon.SpawnArgs{Label: "lbl", Cwd: "/tmp/x"})
	if slices.Contains(bare, "--cwd-write-proof") {
		t.Fatalf("unset cwd proof must omit the internal flag, got %v", bare)
	}
	if slices.Contains(bare, "--codex-git-common-dir") {
		t.Fatalf("unset git common dir must omit the internal flag, got %v", bare)
	}
	if slices.Contains(bare, "--codex-git-common-dir-pinned") {
		t.Fatalf("unset git common dir pin must omit the internal flag, got %v", bare)
	}

	withProof := sessionNewArgs(clcommon.SpawnArgs{
		Label: "lbl", Cwd: "/tmp/x", CwdWriteProof: "proof_123", CodexGitCommonDir: "/tmp/repo/.git", CodexGitCommonDirPinned: true,
		GitWorktreeWriteDirs: []string{"/tmp/repo-parent"}, GitWorktreeWriteDirsPinned: true,
	})
	if i := slices.Index(withProof, "--cwd-write-proof"); i < 0 || i+1 >= len(withProof) || withProof[i+1] != "proof_123" {
		t.Fatalf("cwd proof must ride into the forked session launcher, got %v", withProof)
	}
	if i := slices.Index(withProof, "--codex-git-common-dir"); i < 0 || i+1 >= len(withProof) || withProof[i+1] != "/tmp/repo/.git" {
		t.Fatalf("pinned git common dir must ride into the forked session launcher, got %v", withProof)
	}
	if !slices.Contains(withProof, "--codex-git-common-dir-pinned") {
		t.Fatalf("git common dir pin-presence must ride into the forked session launcher, got %v", withProof)
	}
	if i := slices.Index(withProof, "--git-worktree-write-dir"); i < 0 || i+1 >= len(withProof) || withProof[i+1] != "/tmp/repo-parent" {
		t.Fatalf("exact repository write root must ride into the forked session launcher, got %v", withProof)
	}
	if !slices.Contains(withProof, "--git-worktree-write-dirs-pinned") {
		t.Fatalf("repository write-root pin-presence must ride into the forked session launcher, got %v", withProof)
	}

	pinnedEmpty := sessionNewArgs(clcommon.SpawnArgs{Label: "lbl", Cwd: "/tmp/x", CodexGitCommonDirPinned: true})
	if slices.Contains(pinnedEmpty, "--codex-git-common-dir") || !slices.Contains(pinnedEmpty, "--codex-git-common-dir-pinned") {
		t.Fatalf("proved absence must ride as pin-presence without a path, got %v", pinnedEmpty)
	}

	resume := sessionResumeArgs(clcommon.SpawnArgs{
		ConvID: "conv", Cwd: "/tmp/x", DirWriteProof: "proof_456",
		CodexGitCommonDir: "/tmp/repo/.git", CodexGitCommonDirPinned: true,
		GitWorktreeWriteDirs: []string{"/tmp/repo-parent"}, GitWorktreeWriteDirsPinned: true,
	})
	if i := slices.Index(resume, "--dir-write-proof"); i < 0 || i+1 >= len(resume) || resume[i+1] != "proof_456" {
		t.Fatalf("resume repository proof must ride into relaunches, got %v", resume)
	}
	if i := slices.Index(resume, "--codex-git-common-dir"); i < 0 || i+1 >= len(resume) || resume[i+1] != "/tmp/repo/.git" {
		t.Fatalf("resume must forward the pinned git common dir, got %v", resume)
	}
	if !slices.Contains(resume, "--codex-git-common-dir-pinned") {
		t.Fatalf("resume must forward git common dir pin-presence, got %v", resume)
	}
	if i := slices.Index(resume, "--git-worktree-write-dir"); i < 0 || i+1 >= len(resume) || resume[i+1] != "/tmp/repo-parent" {
		t.Fatalf("resume must forward the exact repository write root, got %v", resume)
	}
	if !slices.Contains(resume, "--git-worktree-write-dirs-pinned") {
		t.Fatalf("resume must forward repository write-root pin-presence, got %v", resume)
	}
}

// TestSessionNewArgs_CodexGetsInitialPromptSeed checks the JOH-205 first-turn
// seed: a daemon-spawned Codex carries `--initial-prompt <seed>` so it takes a
// turn (materialising its conv-id) without a human, while Claude Code — which
// reports its conv-id at launch — never gets the seed.
func TestSessionNewArgs_CodexGetsInitialPromptSeed(t *testing.T) {
	codex := sessionNewArgs(clcommon.SpawnArgs{Label: "lbl", Cwd: "/tmp/x", Harness: "codex", Sandbox: "workspace-write", Approval: "never"})
	i := slices.Index(codex, "--initial-prompt")
	if i < 0 || i+1 >= len(codex) || codex[i+1] != codexSpawnSeedPrompt {
		t.Fatalf("codex spawn must carry --initial-prompt %q, got %v", codexSpawnSeedPrompt, codex)
	}

	// Default harness (Claude Code) reports its conv-id at launch — no seed.
	if cc := sessionNewArgs(clcommon.SpawnArgs{Label: "lbl", Cwd: "/tmp/x"}); slices.Contains(cc, "--initial-prompt") {
		t.Fatalf("Claude Code must NOT get an initial-prompt seed, got %v", cc)
	}
}

// TestSessionNewArgs_LaunchEnrollment covers the launch-enrollment fields the
// daemon's efficient CC spawn path forwards: a preset conv-id (--session-id), a
// launch display name (--name), and an explicit first-turn prompt
// (--initial-prompt, the welcome). An explicit InitialPrompt overrides the
// Codex seed fallback; an unset trio omits all three.
func TestSessionNewArgs_LaunchEnrollment(t *testing.T) {
	args := sessionNewArgs(clcommon.SpawnArgs{
		Label:         "lbl",
		Cwd:           "/tmp/x",
		SessionID:     "2567b392-357b-4d6c-9a59-74fd23424cda",
		Name:          "worker",
		InitialPrompt: "[system: welcome]",
	})
	if i := slices.Index(args, "--session-id"); i < 0 || i+1 >= len(args) || args[i+1] != "2567b392-357b-4d6c-9a59-74fd23424cda" {
		t.Fatalf("must append --session-id, got %v", args)
	}
	if i := slices.Index(args, "--name"); i < 0 || i+1 >= len(args) || args[i+1] != "worker" {
		t.Fatalf("must append --name worker, got %v", args)
	}
	if i := slices.Index(args, "--initial-prompt"); i < 0 || i+1 >= len(args) || args[i+1] != "[system: welcome]" {
		t.Fatalf("explicit InitialPrompt must ride through verbatim, got %v", args)
	}

	// Unset trio → none of the three flags (the historical CC argv).
	bare := sessionNewArgs(clcommon.SpawnArgs{Label: "lbl", Cwd: "/tmp/x"})
	if slices.Contains(bare, "--session-id") || slices.Contains(bare, "--name") || slices.Contains(bare, "--initial-prompt") {
		t.Fatalf("an unset launch-enrollment trio must omit all three flags, got %v", bare)
	}
}

// TestSessionNewArgs_EffortIncludedWhenSet verifies an explicit level is
// passed through as `--effort <level>` to the forked session.
func TestSessionNewArgs_EffortIncludedWhenSet(t *testing.T) {
	args := sessionNewArgs(clcommon.SpawnArgs{Label: "lbl", Cwd: "/tmp/x", Effort: "high"})
	i := slices.Index(args, "--effort")
	if i < 0 || i+1 >= len(args) || args[i+1] != "high" {
		t.Fatalf("set effort must append `--effort high`, got %v", args)
	}
}

// TestSessionNewArgs_Harness covers the --harness flag: omitted for the
// default (""/claude) so an untagged spawn keeps the exact pre-JOH-160
// argv, and appended as `--harness codex` for a non-default harness.
func TestSessionNewArgs_Harness(t *testing.T) {
	for _, h := range []string{"", "claude"} {
		if slices.Contains(sessionNewArgs(clcommon.SpawnArgs{Label: "lbl", Cwd: "/tmp/x", Harness: h}), "--harness") {
			t.Fatalf("harness %q must omit --harness (default), got flag", h)
		}
	}
	args := sessionNewArgs(clcommon.SpawnArgs{Label: "lbl", Cwd: "/tmp/x", Harness: "codex"})
	i := slices.Index(args, "--harness")
	if i < 0 || i+1 >= len(args) || args[i+1] != "codex" {
		t.Fatalf("codex harness must append `--harness codex`, got %v", args)
	}
	rargs := sessionResumeArgs(clcommon.SpawnArgs{ConvID: "conv-1", Cwd: "/tmp/x", Harness: "codex"})
	if ri := slices.Index(rargs, "--harness"); ri < 0 || ri+1 >= len(rargs) || rargs[ri+1] != "codex" {
		t.Fatalf("resume must append `--harness codex`, got %v", rargs)
	}
}

// TestSessionNewArgs_Sandbox covers the launch-containment flags. An unset
// mode emits neither flag. The managed-profile pseudo-mode
// (SandboxManagedProfile, the secure default) carries `--permission-profile
// tclaude-agent` INSTEAD of `--sandbox`: that profile gives workspace-write
// containment AND allowlists the agentd socket, so the agent can run `tclaude
// agent …` while sandboxed (JOH-207). The three RAW Codex modes —
// workspace-write, read-only, danger-full-access — fall back to `--sandbox
// <mode>` (no socket access). Modes are resolved + cwd-guarded at the spawn
// boundary, so by the argv builder they are validated enums.
func TestSessionNewArgs_Sandbox(t *testing.T) {
	// Unset → neither flag.
	if a := sessionNewArgs(clcommon.SpawnArgs{Label: "lbl", Cwd: "/tmp/x", Harness: "codex"}); slices.Contains(a, "--sandbox") || slices.Contains(a, "--permission-profile") {
		t.Fatalf("unset sandbox must omit --sandbox and --permission-profile, got %v", a)
	}
	// The managed-profile pseudo-mode → managed permission profile, NOT
	// --sandbox (new + resume).
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"new", sessionNewArgs(clcommon.SpawnArgs{Label: "lbl", Cwd: "/tmp/x", Harness: "codex", Sandbox: harness.SandboxManagedProfile})},
		{"resume", sessionResumeArgs(clcommon.SpawnArgs{ConvID: "conv-1", Cwd: "/tmp/x", Harness: "codex", Sandbox: harness.SandboxManagedProfile})},
	} {
		if slices.Contains(tc.args, "--sandbox") {
			t.Fatalf("%s: codex managed profile must NOT emit --sandbox, got %v", tc.name, tc.args)
		}
		i := slices.Index(tc.args, "--permission-profile")
		if i < 0 || i+1 >= len(tc.args) || tc.args[i+1] != harness.CodexAgentProfile {
			t.Fatalf("%s: codex managed profile must append `--permission-profile %s`, got %v", tc.name, harness.CodexAgentProfile, tc.args)
		}
	}
	// The raw Codex modes (incl. workspace-write now) → `--sandbox <mode>`, no
	// profile.
	for _, mode := range []string{harness.SandboxWorkspaceWrite, harness.SandboxReadOnly, harness.SandboxDangerFull} {
		args := sessionNewArgs(clcommon.SpawnArgs{Label: "lbl", Cwd: "/tmp/x", Harness: "codex", Sandbox: mode})
		if i := slices.Index(args, "--sandbox"); i < 0 || i+1 >= len(args) || args[i+1] != mode {
			t.Fatalf("codex %s must append `--sandbox %s`, got %v", mode, mode, args)
		}
		if slices.Contains(args, "--permission-profile") {
			t.Fatalf("codex %s must NOT emit --permission-profile, got %v", mode, args)
		}
	}
}

// TestSessionNewArgs_Approval covers the --ask-for-approval flag: omitted
// when no policy was resolved (""), and appended as `--ask-for-approval
// <policy>` for a Codex spawn/resume. The policy is resolved at the spawn
// boundary (harness.ResolveApprovalPolicy → "never" for an unattended Codex
// pane), so by the time it reaches the argv builder it is a validated enum
// (JOH-200).
func TestSessionNewArgs_Approval(t *testing.T) {
	if slices.Contains(sessionNewArgs(clcommon.SpawnArgs{Label: "lbl", Cwd: "/tmp/x", Harness: "codex"}), "--ask-for-approval") {
		t.Fatalf("unset approval must omit --ask-for-approval")
	}
	args := sessionNewArgs(clcommon.SpawnArgs{Label: "lbl", Cwd: "/tmp/x", Harness: "codex", Sandbox: "workspace-write", Approval: "never"})
	i := slices.Index(args, "--ask-for-approval")
	if i < 0 || i+1 >= len(args) || args[i+1] != "never" {
		t.Fatalf("set approval must append `--ask-for-approval never`, got %v", args)
	}
	rargs := sessionResumeArgs(clcommon.SpawnArgs{ConvID: "conv-1", Cwd: "/tmp/x", Harness: "codex", Sandbox: "workspace-write", Approval: "never"})
	if ri := slices.Index(rargs, "--ask-for-approval"); ri < 0 || ri+1 >= len(rargs) || rargs[ri+1] != "never" {
		t.Fatalf("resume must append `--ask-for-approval never`, got %v", rargs)
	}
}

// TestSessionNewArgs_AutoReview covers the --auto-review flag: a bare boolean
// flag appended only when the spawn opted in (true), omitted otherwise. The
// opt-in is gated at the spawn boundary (harness.ResolveAutoReview) before it
// reaches the argv builder; relaunch paths always pass false (JOH-200 part 2).
func TestSessionNewArgs_AutoReview(t *testing.T) {
	if slices.Contains(sessionNewArgs(clcommon.SpawnArgs{Label: "lbl", Cwd: "/tmp/x", Harness: "codex", Sandbox: "workspace-write", Approval: "never"}), "--auto-review") {
		t.Fatalf("autoReview=false must omit --auto-review")
	}
	if !slices.Contains(sessionNewArgs(clcommon.SpawnArgs{Label: "lbl", Cwd: "/tmp/x", Harness: "codex", Sandbox: "workspace-write", Approval: "never", AutoReview: true}), "--auto-review") {
		t.Fatalf("autoReview=true must append --auto-review")
	}
	if !slices.Contains(sessionResumeArgs(clcommon.SpawnArgs{ConvID: "conv-1", Cwd: "/tmp/x", Harness: "codex", Sandbox: "workspace-write", Approval: "never", AutoReview: true}), "--auto-review") {
		t.Fatalf("resume autoReview=true must append --auto-review")
	}
}

// TestSessionNewArgs_TrustDir covers the --trust-dir flag (JOH-205 inc4): a
// bare boolean flag appended only when the spawn opted into pre-trusting its
// launch dir for Codex (true), omitted otherwise. The opt-in is gated at the
// spawn boundary (harness.ResolveTrustDir) before it reaches the argv builder;
// relaunch paths (reincarnate/clone) always pass false.
func TestSessionNewArgs_TrustDir(t *testing.T) {
	if slices.Contains(sessionNewArgs(clcommon.SpawnArgs{Label: "lbl", Cwd: "/tmp/x", Harness: "codex", Sandbox: "workspace-write", Approval: "never"}), "--trust-dir") {
		t.Fatalf("trustDir=false must omit --trust-dir")
	}
	if !slices.Contains(sessionNewArgs(clcommon.SpawnArgs{Label: "lbl", Cwd: "/tmp/x", Harness: "codex", Sandbox: "workspace-write", Approval: "never", TrustDir: true}), "--trust-dir") {
		t.Fatalf("trustDir=true must append --trust-dir")
	}
}

// TestSessionNewArgs_RemoteControl covers the --remote-control flag: a bare
// boolean flag appended to the spawn argv only when the caller opted in (true),
// omitted otherwise. It is gated at the spawn boundary
// (harness.ResolveRemoteControl) before reaching the argv builder. JOH-258 added
// it to the FRESH (sessionNewArgs) path; JOH-261 extends it to the RESUME
// (sessionResumeArgs) path so a relaunch can re-arm Remote Access carried from
// the source conv's persisted state.
func TestSessionNewArgs_RemoteControl(t *testing.T) {
	if slices.Contains(sessionNewArgs(clcommon.SpawnArgs{Label: "lbl", Cwd: "/tmp/x"}), "--remote-control") {
		t.Fatalf("remoteControl=false must omit --remote-control")
	}
	if !slices.Contains(sessionNewArgs(clcommon.SpawnArgs{Label: "lbl", Cwd: "/tmp/x", RemoteControl: true}), "--remote-control") {
		t.Fatalf("remoteControl=true must append --remote-control")
	}
	// Resume now CARRIES it when armed — re-arm on relaunch is JOH-261.
	if slices.Contains(sessionResumeArgs(clcommon.SpawnArgs{ConvID: "conv-1", Cwd: "/tmp/x"}), "--remote-control") {
		t.Fatalf("resume with remoteControl=false must omit --remote-control")
	}
	if !slices.Contains(sessionResumeArgs(clcommon.SpawnArgs{ConvID: "conv-1", Cwd: "/tmp/x", RemoteControl: true}), "--remote-control") {
		t.Fatalf("resume with remoteControl=true must carry --remote-control (JOH-261)")
	}
}

// TestSessionNewArgs_AskTimeout: the Claude Code AskUserQuestion idle-timeout is
// appended as `--ask-user-question-timeout <v>` only when set; "" omits it. The
// forked `tclaude session new` re-validates + harness-gates it.
//
// The resume-side case pins the BUILDER's symmetry (given a value, sessionResumeArgs
// appends it too), NOT a preservation guarantee: no production relaunch path
// currently populates SpawnArgs.AskUserQuestionTimeout, so a resume/reincarnate/clone
// reverts to inherit (no override) — the same fail-closed re-default the sandbox /
// approval flags use on relaunch. The scaffolding is here so a future "preserve the
// timeout across reincarnate" only has to populate the field, not touch the argv.
func TestSessionNewArgs_AskTimeout(t *testing.T) {
	flagValueAt := func(args []string, flag string) (string, bool) {
		for i, a := range args {
			if a == flag && i+1 < len(args) {
				return args[i+1], true
			}
		}
		return "", false
	}

	// Unset → no flag on either path.
	if _, ok := flagValueAt(sessionNewArgs(clcommon.SpawnArgs{Label: "lbl", Cwd: "/tmp/x"}), "--ask-user-question-timeout"); ok {
		t.Fatalf("unset timeout must omit --ask-user-question-timeout")
	}
	if _, ok := flagValueAt(sessionResumeArgs(clcommon.SpawnArgs{ConvID: "conv-1", Cwd: "/tmp/x"}), "--ask-user-question-timeout"); ok {
		t.Fatalf("unset timeout must omit --ask-user-question-timeout on resume")
	}

	// Set → the flag carries its value on both paths.
	if v, ok := flagValueAt(sessionNewArgs(clcommon.SpawnArgs{Label: "lbl", Cwd: "/tmp/x", AskUserQuestionTimeout: "5m"}), "--ask-user-question-timeout"); !ok || v != "5m" {
		t.Fatalf("fresh spawn must append --ask-user-question-timeout 5m, got (%q, %v)", v, ok)
	}
	if v, ok := flagValueAt(sessionResumeArgs(clcommon.SpawnArgs{ConvID: "conv-1", Cwd: "/tmp/x", AskUserQuestionTimeout: "10m"}), "--ask-user-question-timeout"); !ok || v != "10m" {
		t.Fatalf("resume must append --ask-user-question-timeout 10m, got (%q, %v)", v, ok)
	}
}
