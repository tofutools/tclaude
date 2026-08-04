package harness

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// The catalog's whole justification is that every flag it emits was measured
// against the pinned binary, so these tests are written against the FINDINGS in
// copilotfixture/testdata/1.0.77/permission_contract.json rather than against
// the implementation. Each one names the contract entry it stands on.

func TestCopilotApprovalCatalog(t *testing.T) {
	h, err := Resolve(CopilotName)
	if err != nil {
		t.Fatalf("Resolve(copilot): %v", err)
	}
	if !h.SupportsApproval() {
		t.Fatal("copilot must advertise a launch-time approval catalog")
	}
	if got := h.Approval.DefaultPolicy(); got != CopilotApprovalAllowTools {
		t.Fatalf("DefaultPolicy() = %q, want %q — an unattended pane must not default to a prompting posture", got, CopilotApprovalAllowTools)
	}
	// The default must survive its own validator: ReconstructApprovalPolicy
	// resolves an unrecorded input by validating DefaultPolicy(), and a default
	// that fails validation silently degrades to "no policy at all".
	if got, err := h.Approval.ValidatePolicy(h.Approval.DefaultPolicy()); err != nil || got != CopilotApprovalAllowTools {
		t.Fatalf("ValidatePolicy(DefaultPolicy()) = %q, %v; want the default to validate to itself", got, err)
	}
	modes := h.Approval.Modes()
	if len(modes) != 2 || modes[0] != CopilotApprovalAllowTools || modes[1] != CopilotApprovalInherit {
		t.Fatalf("Modes() = %v, want the default first, then inherit", modes)
	}
	// Mutating the returned slice must not reach the validation source.
	modes[0] = "tampered"
	if again := h.Approval.Modes(); again[0] != CopilotApprovalAllowTools {
		t.Fatal("Modes() handed out the shared backing array")
	}
	for _, mode := range h.Approval.Modes() {
		if strings.TrimSpace(h.Approval.ModeHelp(mode)) == "" {
			t.Errorf("ModeHelp(%q) is empty; the spawn dialog renders it", mode)
		}
	}
	if h.Approval.ModeHelp("no-such-mode") != "" {
		t.Fatal("ModeHelp must return \"\" for an unrecognized policy")
	}
}

// Both mode-help strings must warn, because NEITHER token makes a Copilot pane
// unconditionally nonblocking: `inherit` is Copilot's prompting posture, and
// even `allow-tools` leaves the out-of-grant path dialog and the folder-trust
// gate standing (contract: out-of-cwd-paths, folder-trust). A reassuring
// default would be the exact overstatement this ticket is about.
func TestCopilotApprovalModeHelpWarnsAboutEveryRemainingPromptSource(t *testing.T) {
	h, err := Resolve(CopilotName)
	if err != nil {
		t.Fatalf("Resolve(copilot): %v", err)
	}
	for _, mode := range h.Approval.Modes() {
		help := h.Approval.ModeHelp(mode)
		if !strings.Contains(help, "⚠") {
			t.Errorf("ModeHelp(%q) carries no caveat: %q", mode, help)
		}
	}
	allowTools := h.Approval.ModeHelp(CopilotApprovalAllowTools)
	for _, want := range []string{"granted directory", "trust"} {
		if !strings.Contains(allowTools, want) {
			t.Errorf("ModeHelp(%q) must name the prompt sources it does NOT close (missing %q): %q",
				CopilotApprovalAllowTools, want, allowTools)
		}
	}
}

func TestCopilotApprovalValidatePolicy(t *testing.T) {
	h, err := Resolve(CopilotName)
	if err != nil {
		t.Fatalf("Resolve(copilot): %v", err)
	}
	for _, tc := range []struct {
		in      string
		want    string
		wantErr bool
	}{
		{in: "", want: ""},
		{in: CopilotApprovalAllowTools, want: CopilotApprovalAllowTools},
		{in: "  " + CopilotApprovalInherit + "  ", want: CopilotApprovalInherit},
		// Other harnesses' tokens are not Copilot's, and a validator that let
		// them through would render nothing while recording an authority the
		// launch never had.
		{in: "never", wantErr: true},
		{in: "auto", wantErr: true},
		{in: "allow-all", wantErr: true},
		{in: "yolo", wantErr: true},
		{in: "plan", wantErr: true},
		{in: "ALLOW-TOOLS", wantErr: true},
	} {
		got, err := h.Approval.ValidatePolicy(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("ValidatePolicy(%q) = %q, want an error", tc.in, got)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("ValidatePolicy(%q) = %q, %v; want %q, nil", tc.in, got, err, tc.want)
		}
	}
}

// ResolveApprovalPolicy is the daemon spawn boundary: an unchosen Copilot spawn
// must land on the nonblocking default, while `tclaude session new` (the human
// trust root, who can attach and answer a prompt) must be left alone.
func TestCopilotApprovalResolution(t *testing.T) {
	h, err := Resolve(CopilotName)
	if err != nil {
		t.Fatalf("Resolve(copilot): %v", err)
	}
	got, err := ResolveApprovalPolicy(h, "")
	if err != nil || got != CopilotApprovalAllowTools {
		t.Fatalf("ResolveApprovalPolicy(copilot, \"\") = %q, %v; want %q", got, err, CopilotApprovalAllowTools)
	}
	got, err = ValidateApprovalPolicy(h, "")
	if err != nil || got != "" {
		t.Fatalf("ValidateApprovalPolicy(copilot, \"\") = %q, %v; want \"\" (no forced posture for a human launch)", got, err)
	}
	if _, err := ResolveApprovalPolicy(h, "never"); err == nil {
		t.Fatal("a Codex policy must be refused for copilot rather than silently dropped")
	}
	// Auto-review is a Codex guardian axis; Copilot has no reviewer, and
	// silently dropping the flag would hide the mistake.
	if _, err := ResolveAutoReview(h, true); err == nil {
		t.Fatal("ResolveAutoReview(copilot, true) must be refused")
	}
}

func TestCopilotPermissionArgs(t *testing.T) {
	for _, tc := range []struct {
		name    string
		policy  string
		addDirs []string
		want    []string
	}{
		{
			// contract: default-interactive-blocking (--allow-all-tools closes
			// the tool gate) + no-ask-user (--no-ask-user removes ask_user).
			name:   "the default renders exactly the two measured flags",
			policy: CopilotApprovalAllowTools,
			want:   []string{"--allow-all-tools", "--no-ask-user"},
		},
		{
			name:   "inherit renders nothing at all",
			policy: CopilotApprovalInherit,
		},
		{
			// contract: out-of-cwd-paths — --add-dir <dir> clears the directory
			// dialog for that directory, and nothing wider is emitted.
			name:    "granted directories become one --add-dir each, sorted",
			policy:  CopilotApprovalAllowTools,
			addDirs: []string{"/srv/b", "/srv/a"},
			want: []string{"--allow-all-tools", "--no-ask-user",
				"--add-dir", "/srv/a", "--add-dir", "/srv/b"},
		},
		{
			// The grants come from the sandbox profile, not from the approval
			// token: dropping them under `inherit` would leave Copilot's own
			// path check prompting for a directory tclaude's outer sandbox
			// already opened, and SandboxReadDirs forbids silently dropping
			// them.
			name:    "inherit still renders the profile's directory grants",
			policy:  CopilotApprovalInherit,
			addDirs: []string{"/srv/a"},
			want:    []string{"--add-dir", "/srv/a"},
		},
		{
			name:    "duplicate and relative entries are dropped",
			policy:  CopilotApprovalAllowTools,
			addDirs: []string{"/srv/a", "/srv/a/", "relative/path", "", "  ", "."},
			want:    []string{"--allow-all-tools", "--no-ask-user", "--add-dir", "/srv/a"},
		},
		{
			// A blank policy is what a human `tclaude session new` leaves in
			// the spec — no posture is forced on the trust root. It is NOT an
			// unknown token: every reconstruction path in tclaude maps blank to
			// `inherit`, so it renders inherit's shape. Dropping the grants here
			// made a fresh launch and its own resume disagree.
			name:    "a blank policy renders the grants, like the inherit it reconstructs as",
			policy:  "",
			addDirs: []string{"/srv/a"},
			want:    []string{"--add-dir", "/srv/a"},
		},
		{
			// ...and blank must NOT pick up the default's auto-approval, which
			// is the whole reason the human path leaves it blank.
			name:    "a blank policy never auto-approves",
			policy:  "   ",
			addDirs: nil,
		},
		{
			// Kept as its own row so blank and unknown cannot silently merge:
			// an unknown token renders nothing AT ALL, grants included, because
			// tclaude cannot say what posture the launch would run under.
			name:    "an unrecognized policy renders nothing, not the default and not the grants",
			policy:  "allow-all",
			addDirs: []string{"/srv/a"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := copilotPermissionArgs(tc.policy, tc.addDirs)
			if strings.Join(got, " ") != strings.Join(tc.want, " ") {
				t.Fatalf("copilotPermissionArgs(%q, %v)\n got: %v\nwant: %v", tc.policy, tc.addDirs, got, tc.want)
			}
		})
	}
}

// TestCopilotPermissionArgsNeverEmitDisprovenOrEscalatingFlags is the guard the
// disproven half of the contract earned.
//
// `--deny-tool 'url()'` was the TCL-973 plan's proposed default and is REJECTED
// at argument parse with exit 1 and no provider contact (contract: url-access) —
// it would have killed every Copilot pane at launch. `--allow-all-paths` and
// `--allow-all`/`--yolo` are real and measured, but Copilot's built-in edits are
// not OS-confined, so outside a tclaude-layer sandbox the path check is the only
// boundary on what the agent writes and no token here removes it.
//
// Written as a sweep over every mode plus a few non-modes, rather than as
// assertions on the two tokens that exist today, so a third token cannot be
// added without this guard seeing it.
func TestCopilotPermissionArgsNeverEmitDisprovenOrEscalatingFlags(t *testing.T) {
	h, err := Resolve(CopilotName)
	if err != nil {
		t.Fatalf("Resolve(copilot): %v", err)
	}
	forbidden := []string{
		"--deny-tool", "--allow-tool", // tool governance is a separate contract, still nil
		"--allow-all-paths", "--allow-all", "--yolo",
		"--allow-all-urls", "--allow-url", "--deny-url",
		"--disallow-temp-dir", "--mode", "--plan", "--autopilot",
	}
	policies := append(h.Approval.Modes(), "", "bogus")
	for _, policy := range policies {
		args := copilotPermissionArgs(policy, []string{"/srv/a", "/srv/b"})
		joined := strings.Join(args, " ")
		for _, flag := range forbidden {
			for _, arg := range args {
				if arg == flag {
					t.Errorf("policy %q renders %s, which no measurement in the permission contract supports as a default: %s",
						policy, flag, joined)
				}
			}
		}
		// The empty-paren pattern is the exact spelling the binary rejects, in
		// any kind. Matched as a substring so it is caught however it is built.
		for _, spelling := range []string{"url()", "shell()", "write()"} {
			if strings.Contains(joined, spelling) {
				t.Errorf("policy %q renders the disproven %s pattern, which exits 1 at argument parse: %s", policy, spelling, joined)
			}
		}
	}
}

// TestCopilotBuildCommandScrubsAmbientAllowAll pins the treatment of the one
// environment variable measured to promote a launch on its own.
//
// COPILOT_ALLOW_ALL is strictly stronger than the --allow-all-tools flag it
// documents: exported alone it also cleared the folder-trust gate that no flag
// clears (contract: ambient-allow-all-env). tclaude forwards the operator's
// whole environment into the pane, so without this an operator with it exported
// would run every Copilot pane as allow-all while tclaude recorded `inherit`.
func TestCopilotBuildCommandScrubsAmbientAllowAll(t *testing.T) {
	s := copilotSpawner{}
	for _, tc := range []struct {
		name string
		spec SpawnSpec
	}{
		{name: "bare launch", spec: SpawnSpec{}},
		{name: "inherit", spec: SpawnSpec{ApprovalPolicy: CopilotApprovalInherit}},
		{name: "allow-tools", spec: SpawnSpec{ApprovalPolicy: CopilotApprovalAllowTools}},
		{name: "resume", spec: SpawnSpec{ResumeID: "11111111-2222-3333-4444-555555555555"}},
		{
			name: "an operator who exported it",
			spec: SpawnSpec{EnvExports: "export COPILOT_ALLOW_ALL='true'; export A=1; "},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := s.BuildCommand(tc.spec)
			scrub := strings.Index(cmd, "unset COPILOT_ALLOW_ALL;")
			if scrub < 0 {
				t.Fatalf("the ambient allow-all scrub is missing: %s", cmd)
			}
			// Position is the assertion, not mere presence: an unset BEFORE the
			// operator's exports would be undone by them.
			if exported := strings.Index(cmd, "export COPILOT_ALLOW_ALL"); exported >= 0 && exported > scrub {
				t.Fatalf("the scrub runs before the export that would defeat it: %s", cmd)
			}
			if strings.Index(cmd, "copilot") < scrub {
				t.Fatalf("the scrub must precede the binary: %s", cmd)
			}
			// Unset, never pinned to a falsy value: the value parse is strict
			// equality against "true" TODAY, and a pinned false would be
			// defeated silently if that ever widened.
			if strings.Contains(cmd, "COPILOT_ALLOW_ALL=false") || strings.Contains(cmd, "COPILOT_ALLOW_ALL=''") {
				t.Fatalf("the ambient variable must be unset, not pinned falsy: %s", cmd)
			}
		})
	}
}

// A directory with a shell metacharacter in its name must land as ONE argument.
// --add-dir values come from a resolved sandbox profile, which is operator data
// rather than a hostile input, but the whole command is handed to `sh -c` and
// this file is the only place that quotes it.
func TestCopilotAddDirQuotesHostilePaths(t *testing.T) {
	cmd := copilotSpawner{}.BuildCommand(SpawnSpec{
		ApprovalPolicy:  CopilotApprovalAllowTools,
		SandboxReadDirs: []string{"/srv/a b; rm -rf /"},
	})
	// filepath.Clean has already dropped the trailing separator by this point;
	// what matters is that the space and the `;` stay inside one argument.
	if !strings.Contains(cmd, "--add-dir '/srv/a b; rm -rf '") {
		t.Fatalf("a path with shell metacharacters was not quoted as one argument: %s", cmd)
	}
}

// Read and write roots both feed --add-dir: Copilot's directory check is not
// read/write split (its dialog is a single "Allow directory access"), so
// modelling a distinction the CLI does not have would only invent one. Deny
// roots are NOT rendered — --add-dir has no negative form, and turning a deny
// into an omission would be indistinguishable from never having had the rule.
func TestCopilotSpawnAddDirsMergesReadAndWriteButNotDenies(t *testing.T) {
	got := copilotSpawnAddDirs(SpawnSpec{
		SandboxReadDirs:  []string{"/srv/read"},
		SandboxWriteDirs: []string{"/srv/write"},
		SandboxDenyDirs:  []string{"/srv/secret"},
	})
	if strings.Join(got, " ") != "/srv/read /srv/write" {
		t.Fatalf("copilotSpawnAddDirs() = %v, want the read and write roots only", got)
	}
	cmd := copilotSpawner{}.BuildCommand(SpawnSpec{
		ApprovalPolicy:   CopilotApprovalAllowTools,
		SandboxReadDirs:  []string{"/srv/read"},
		SandboxWriteDirs: []string{"/srv/write"},
		SandboxDenyDirs:  []string{"/srv/secret"},
	})
	if strings.Contains(cmd, "/srv/secret") {
		t.Fatalf("a denied root reached the launch command: %s", cmd)
	}
	if !strings.Contains(cmd, "--add-dir /srv/read --add-dir /srv/write") {
		t.Fatalf("both grant kinds must be rendered: %s", cmd)
	}
}

// TestCopilotLaunchExtraArgsAudit closes the gap between the posture tclaude
// RENDERS and the posture the pane RUNS.
//
// Pass-through args land on the same command line as the catalog's flags, so
// one that moves a permission axis would produce a pane broader than the row
// recording it — and approval lineage and relaunch both reason from that row.
// Ordering is not the defence: nothing in the permission matrix establishes what
// 1.0.77 does with a duplicated or contradictory permission flag, so a launch
// that would depend on those semantics is refused instead of reasoned about.
func TestCopilotLaunchExtraArgsAudit(t *testing.T) {
	h, err := Resolve(CopilotName)
	if err != nil {
		t.Fatalf("Resolve(copilot): %v", err)
	}

	// Every audited flag must be caught in BOTH spellings Copilot's own option
	// table uses, since an audit that knew only one would be bypassed by
	// writing the other. Driven off the audited set itself, so a flag added to
	// it later cannot be added without spelling coverage.
	for flag := range copilotOwnedFlags {
		for _, spelling := range []string{flag, flag + "=x", flag + "=url(https://x?a=b)"} {
			if err := ValidateLaunchExtraArgs(h, []string{"--log-level=debug", spelling}); err == nil {
				t.Errorf("pass-through %q was accepted; it moves a recorded Copilot posture", spelling)
			}
		}
		// The value-in-the-next-arg spelling, which is how the CLI documents
		// --add-dir and the rule flags.
		if err := ValidateLaunchExtraArgs(h, []string{flag, "/srv/elsewhere"}); err == nil {
			t.Errorf("pass-through %q <value> was accepted; it moves a recorded Copilot posture", flag)
		}
	}

	// Headless mode is audited too: the no-TTY tool-approval fallback
	// auto-ALLOWS, and whether `-p` in a tmux pane counts as no-TTY is not
	// measured. The glued-short-value spelling must be caught as well, since the
	// `=` split alone does not reach it.
	for _, spelling := range []string{"-p", "-p=go", "-pgo", "--prompt", "--prompt=go"} {
		if err := ValidateLaunchExtraArgs(h, []string{spelling}); err == nil {
			t.Errorf("pass-through %q was accepted; headless mode has its own unmeasured permission fallbacks", spelling)
		}
	}

	// The audit must name the flag and stay actionable: a bare "refused" leaves
	// the operator guessing which of their args was the problem.
	err = ValidateLaunchExtraArgs(h, []string{"--allow-all-paths"})
	if err == nil {
		t.Fatal("--allow-all-paths must be refused")
	}
	for _, want := range []string{"--allow-all-paths", "directory access", "sandbox profile"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal must contain %q: %v", want, err)
		}
	}

	// tclaude owns the identity, first-turn and metadata options too, not only
	// the permission ones: a pass-through copy of any of them makes the pane
	// disagree with the launch that was recorded. Identity is the sharpest —
	// --resume attaches the pane to a DIFFERENT conversation than the one the
	// daemon enrolled — but a duplicate --model misreports the dashboard and the
	// usage accounting under the same unmeasured duplicate-flag semantics.
	for _, spelling := range []string{
		"--resume", "--resume=other-id", "-r", "-rother-id",
		"--session-id", "--session-id=00000000-0000-0000-0000-000000000000",
		"--continue", "--connect",
		"-i", "-igo", "--interactive",
		"--model", "--model=gpt-5.4", "--effort=high", "--reasoning-effort=high",
		"--name=other", "-n", "-nother",
		// The working directory: tclaude never emits -C, which is exactly why
		// a coverage guard driven off the rendered flags cannot catch it. It
		// moves the recorded cwd AND, since Copilot grants its cwd subtree
		// automatically, opens a directory tree the profile never granted.
		"-C", "-C/srv/other", "-C=/srv/other",
		"-w", "-wfeature", "--worktree", "--worktree=feature",
		// The Copilot home: every tclaude contract that reads COPILOT_HOME —
		// hooks, the conversation store, the trust store, the sandbox and
		// model-transport gates, the telemetry follower — would inspect a
		// different tree than the pane uses.
		"--config-dir", "--config-dir=/srv/other-home",
		// Runtime/protocol selectors: tclaude models a local interactive TUI,
		// and every contract it advertises describes that pane.
		"--cloud", "--server", "--managed-server", "--ui-server", "--headless",
		"--acp", "--stdio", "--host=127.0.0.1", "--port=8080", "--auth-token-env=TOK",
	} {
		if err := ValidateLaunchExtraArgs(h, []string{spelling}); err == nil {
			t.Errorf("pass-through %q was accepted; tclaude renders and records that option itself", spelling)
		}
	}
	// Each refusal must name the dedicated option that does the job honestly —
	// a refusal with no way out trains operators to work around it.
	for _, tc := range []struct{ arg, want string }{
		{"--resume=other", "tclaude conv resume"},
		{"--model=gpt-5.4", "--model"},
		{"--name=other", "--name"},
		{"-i", "initial message"},
		{"--allow-all-tools", "approval policy"},
		{"--add-dir", "sandbox profile"},
		{"-C", "launch directory"},
		{"--config-dir=/other", "COPILOT_HOME"},
		{"--worktree=feature", "worktree option"},
		{"--server", "interactive Copilot TUI"},
		// The NARROWING flags must not be pointed at a mechanism that cannot
		// do what they do: tclaude renders no deny rules and cannot revoke
		// Copilot's automatic temp grant, so saying so plainly is the remedy.
		{"--deny-tool=shell(rm)", "tool-governance contract"},
		{"--excluded-tools=web_fetch", "tool-governance contract"},
		{"--deny-url=github.com", "tool-governance contract"},
		{"--disallow-temp-dir", "tclaude-layer"},
	} {
		err := ValidateLaunchExtraArgs(h, []string{tc.arg})
		if err == nil {
			t.Errorf("%s must be refused", tc.arg)
			continue
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("the refusal for %s must name the honest alternative %q: %v", tc.arg, tc.want, err)
		}
	}

	// Ordinary args keep working — this audits what tclaude owns, and is not a
	// general allowlist.
	for _, args := range [][]string{
		nil,
		{"--log-level=debug"},
		{"--banner", "--no-color"},
		// Near-misses that are NOT the audited flags. A prefix match would
		// wrongly reject these, and a `--add-dir`-shaped value is not the flag.
		{"--allow-all-tools-please"},
		{"--add-dirs"},
		{"/srv/add-dir"},
		// The glued-short-value form is checked only for single-dash flags, so
		// a long flag that merely starts with an audited name stays accepted.
		{"--plans-only"},
	} {
		if err := ValidateLaunchExtraArgs(h, args); err != nil {
			t.Errorf("ordinary pass-through args %v must keep working: %v", args, err)
		}
	}

	// Other harnesses are untouched: they have their own posture plumbing, and
	// widening this audit to them is not TCL-973's business.
	for _, name := range []string{DefaultName, CodexName, OpenCodeName} {
		other, err := Resolve(name)
		if err != nil {
			t.Fatalf("Resolve(%s): %v", name, err)
		}
		if err := ValidateLaunchExtraArgs(other, []string{"--allow-all-paths", "--yolo"}); err != nil {
			t.Errorf("%s must not be gated by the Copilot posture audit: %v", name, err)
		}
	}
	if err := ValidateLaunchExtraArgs(nil, []string{"--yolo"}); err != nil {
		t.Errorf("a nil harness must not panic or refuse: %v", err)
	}
}

// The audit must cover every flag the CATALOG itself can render. If the catalog
// grows a flag the audit does not know, an operator could pass that same flag
// through and the two would disagree about which one produced the posture.
func TestCopilotLaunchExtraArgsAuditCoversEveryRenderedFlag(t *testing.T) {
	h, err := Resolve(CopilotName)
	if err != nil {
		t.Fatalf("Resolve(copilot): %v", err)
	}
	for _, mode := range h.Approval.Modes() {
		for _, arg := range copilotPermissionArgs(mode, []string{"/srv/a"}) {
			if !strings.HasPrefix(arg, "-") {
				continue // an --add-dir value, not a flag
			}
			if _, audited := copilotOwnedFlags[arg]; !audited {
				t.Errorf("the catalog renders %s but the pass-through audit does not know it, so an operator could pass the same flag unnoticed", arg)
			}
		}
	}
}

// TestCopilotAddDirGrantsRefuseADenyInsideAGrant covers the mirror image of the
// reopen-under-deny shape, which Copilot cannot represent at all.
//
// Claude renders denyRead/denyWrite alongside its allows and lets the more
// specific rule win. Copilot's directory check takes grants only, so a profile
// that grants $HOME and denies $HOME/.ssh collapses on Copilot to "read $HOME"
// — and the denied subtree stops prompting. Without an outer wall that is the
// only file boundary the launch has (the built-in edits are not OS-confined),
// so the launch is refused rather than quietly widened.
func TestCopilotAddDirGrantsRefuseADenyInsideAGrant(t *testing.T) {
	for _, tc := range []struct {
		name             string
		cwd, tempDir     string
		read, write, den []string
		outerLayer       bool
		wantRefused      bool
	}{
		{
			// The common case, and the one a rendered-roots-only gate misses
			// entirely: Copilot grants its cwd subtree with NO flag, so a
			// profile denying a file under cwd renders no --add-dir at all and
			// the denied file is still reachable.
			name: "a denied file under the launch directory, which Copilot grants implicitly",
			cwd:  "/srv/repo", den: []string{"/srv/repo/.env"},
			wantRefused: true,
		},
		{
			name:    "a denied path under the system temp directory, also granted implicitly",
			cwd:     "/srv/repo",
			tempDir: "/tmp", den: []string{"/tmp/secrets"},
			wantRefused: true,
		},
		{
			// A deny outside both implicit roots and outside every rendered
			// root is representable by omission and must still launch.
			name: "a deny outside the launch directory and temp",
			cwd:  "/srv/repo", tempDir: "/tmp", den: []string{"/home/op/.ssh"},
			wantRefused: false,
		},
		{
			name: "a denied subtree inside a granted read root",
			read: []string{"/home/op"}, den: []string{"/home/op/.ssh"},
			wantRefused: true,
		},
		{
			name:  "a denied subtree inside a granted write root",
			write: []string{"/srv/repo"}, den: []string{"/srv/repo/secrets"},
			wantRefused: true,
		},
		{
			name: "the granted root and the deny are the same path",
			read: []string{"/srv/repo"}, den: []string{"/srv/repo"},
			wantRefused: true,
		},
		{
			// Under tclaude-layer the outer sandbox enforces the deny whatever
			// Copilot's own check believes, so the launch is admitted — the same
			// reasoning the reopen-under-deny gate applies to harnesses that can
			// enforce it.
			name: "the same profile under the outer layer",
			read: []string{"/home/op"}, den: []string{"/home/op/.ssh"},
			outerLayer: true, wantRefused: false,
		},
		{
			name: "a disjoint deny is representable by omission",
			read: []string{"/srv/repo"}, den: []string{"/home/op/.ssh"},
			wantRefused: false,
		},
		{
			// A sibling whose name merely shares a prefix is not contained.
			name: "a sibling directory with a shared name prefix",
			read: []string{"/srv/repo"}, den: []string{"/srv/repo-secrets"},
			wantRefused: false,
		},
		{
			name:        "no denies at all",
			read:        []string{"/srv/repo"},
			wantRefused: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateCopilotAddDirGrants(CopilotName, tc.cwd, tc.tempDir,
				tc.read, tc.write, tc.den, tc.outerLayer)
			if tc.wantRefused {
				if err == nil {
					t.Fatal("the launch must be refused: the denied path would be opened by an --add-dir root")
				}
				var capErr *SandboxCapabilityError
				if !errors.As(err, &capErr) || capErr.Kind != SandboxCopilotDenyInsideAddDir {
					t.Fatalf("want a typed %s capability refusal, got %v", SandboxCopilotDenyInsideAddDir, err)
				}
				if !strings.Contains(err.Error(), "tclaude-layer") {
					t.Errorf("the refusal must name the posture that works: %v", err)
				}
				// An implicit grant appears nowhere in the launch command, so
				// the refusal has to say where it came from or the operator is
				// left staring at an argv that does not mention the directory.
				if tc.read == nil && tc.write == nil &&
					!strings.Contains(err.Error(), "automatically, with no flag") {
					t.Errorf("a refusal caused by an implicit grant must say so: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("this profile is representable and must launch: %v", err)
			}
		})
	}

	// Other harnesses render their own deny rules and are not gated here.
	for _, name := range []string{DefaultName, CodexName, OpenCodeName} {
		if err := ValidateCopilotAddDirGrants(name, "/srv/repo", "/tmp",
			[]string{"/home/op"}, nil, []string{"/home/op/.ssh"}, false); err != nil {
			t.Errorf("%s must not be gated by the Copilot add-dir shape: %v", name, err)
		}
	}
}

// The gate and the renderer must agree about which roots a launch opens, or the
// gate would be checking a set the command line does not contain.
func TestCopilotAddDirGateAndRendererAgreeOnTheRoots(t *testing.T) {
	dirs := []string{"/srv/b", "/srv/a", "/srv/a/", "relative", "", "."}
	roots := copilotAddDirRoots(dirs)
	args := copilotAddDirArgs(dirs)
	var rendered []string
	for i := 0; i < len(args); i += 2 {
		if args[i] != copilotFlagAddDir {
			t.Fatalf("unexpected argv shape: %v", args)
		}
		rendered = append(rendered, args[i+1])
	}
	if strings.Join(roots, " ") != strings.Join(rendered, " ") {
		t.Fatalf("gate roots %v disagree with rendered roots %v", roots, rendered)
	}
}

// The gate must use the temp directory it is GIVEN, never resolve one itself.
//
// Copilot grants its temp directory with no flag, so the gate's answer is only
// right if the directory it inspects is the one the LAUNCH will see. The caller
// resolves that from the composed launch environment (session.CopilotLaunchTempDir,
// the same resolver the Copilot sandbox baseline uses), and this pins that the
// gate honours it rather than reaching for tclaude's own ambient temp root.
func TestCopilotAddDirGrantsUseTheSuppliedTempDir(t *testing.T) {
	// A deny under the supplied temp root refuses...
	err := ValidateCopilotAddDirGrants(CopilotName, "/srv/repo", "/launch-temp",
		nil, nil, []string{"/launch-temp/secret"}, false)
	if err == nil {
		t.Fatal("a deny under the launch's temp root must refuse: Copilot grants that root with no flag")
	}
	// ...while the same deny under a DIFFERENT temp root does not, which is
	// what would happen if the caller passed the wrong directory.
	if err := ValidateCopilotAddDirGrants(CopilotName, "/srv/repo", "/other-temp",
		nil, nil, []string{"/launch-temp/secret"}, false); err != nil {
		t.Fatalf("only the supplied temp root is treated as granted: %v", err)
	}
	// An empty temp directory grants nothing — the catalog omits the row for a
	// launch whose environment names no temp directory, and so does this gate.
	if err := ValidateCopilotAddDirGrants(CopilotName, "/srv/repo", "",
		nil, nil, []string{"/launch-temp/secret"}, false); err != nil {
		t.Fatalf("an unnamed temp directory grants nothing: %v", err)
	}
}

// TestCopilotAddDirGrantsSeeThroughTheMacOSTempSymlink stages the macOS layout
// on whatever platform the test runs on, so the regression is caught on Linux
// CI too rather than only where /var really is a symlink.
//
// The two sides genuinely arrive spelled differently in production: a deny is
// recorded by sandboxpolicy.Resolve, which walks symlinks, while the temp root
// comes straight from the launch's TMPDIR. On macOS TMPDIR sits under /var,
// which links to /private/var, so before this gate resolved both sides the
// implicit temp grant — one of only two grants Copilot makes with no flag —
// could never match a deny inside it.
func TestCopilotAddDirGrantsSeeThroughTheMacOSTempSymlink(t *testing.T) {
	base := t.TempDir()
	realTemp := filepath.Join(base, "private", "var", "folders", "T")
	if err := os.MkdirAll(realTemp, 0o755); err != nil {
		t.Fatalf("stage the resolved temp tree: %v", err)
	}
	if err := os.Symlink(filepath.Join(base, "private", "var"),
		filepath.Join(base, "var")); err != nil {
		t.Skipf("this platform cannot stage the symlink the case needs: %v", err)
	}
	// What TMPDIR would say: the LINK spelling.
	launchTemp := filepath.Join(base, "var", "folders", "T")
	// What a resolved profile records: the TARGET spelling.
	denied := filepath.Join(realTemp, "secret")
	if err := os.MkdirAll(denied, 0o755); err != nil {
		t.Fatalf("stage the denied directory: %v", err)
	}

	// The cwd deliberately sits OUTSIDE the staged tree, so the refusal can only
	// come from the temp root — the grant this case is about.
	workspace := filepath.Join(base, "workspace")
	err := ValidateCopilotAddDirGrants(CopilotName, workspace, launchTemp,
		nil, nil, []string{denied}, false)
	if err == nil {
		t.Fatal("the deny sits inside the implicit temp grant by real path identity; " +
			"only the spelling differs, and the gate must not be fooled by it")
	}
	if !strings.Contains(err.Error(), denied) {
		t.Fatalf("the refusal must quote the deny as the profile spells it: %v", err)
	}
	if !strings.Contains(err.Error(), "automatically, with no flag") {
		t.Fatalf("the refusal must say the temp root carries no flag: %v", err)
	}
	// The authored temp spelling is what the operator's environment holds, so
	// that is the one the message has to name — not the resolved form, which
	// appears in neither their profile nor their environment.
	if !strings.Contains(err.Error(), launchTemp) {
		t.Fatalf("the refusal must quote the temp root as TMPDIR spells it: %v", err)
	}

	// The symmetric non-match still passes: a sibling of the granted tree is
	// not contained by it under any spelling.
	outside := filepath.Join(base, "private", "elsewhere", "secret")
	if err := ValidateCopilotAddDirGrants(CopilotName, workspace,
		launchTemp, nil, nil, []string{outside}, false); err != nil {
		t.Fatalf("a deny outside every grant must still pass: %v", err)
	}
}

// TestCopilotFreshHumanLaunchRendersTheSameGrantsAsItsOwnResume is the
// regression for a defect two independent cold reviews found on the same head.
//
// `tclaude session new --harness copilot` leaves the spec's ApprovalPolicy
// BLANK on purpose — a human at a terminal is the trust root, so no posture is
// forced on them — while the session row it writes records `inherit`. Resume
// then reads that row. So one conversation had two different argvs: the resume
// rendered the sandbox profile's directory grants and the launch that created
// the conversation did not, leaving the pane to prompt on a directory the
// operator had granted. It also let ValidateCopilotAddDirGrants refuse a launch
// while naming an --add-dir root that launch would never emit.
//
// The invariant is stated as an EQUALITY rather than as an expected string, so
// it cannot rot into asserting whatever the blank arm happens to render.
func TestCopilotFreshHumanLaunchRendersTheSameGrantsAsItsOwnResume(t *testing.T) {
	dirs := []string{"/srv/data", "/srv/work"}

	fresh := copilotPermissionArgs("", dirs)          // what the human launch renders
	resumed := copilotPermissionArgs(CopilotApprovalInherit, dirs) // what its row resumes as
	if strings.Join(fresh, " ") != strings.Join(resumed, " ") {
		t.Fatalf("one conversation must not have two argvs:\n fresh: %v\nresume: %v", fresh, resumed)
	}
	if !slices.Contains(fresh, copilotFlagAddDir) {
		t.Fatalf("the profile's directory grants must reach a human launch: %v", fresh)
	}

	// Blank renders inherit's SHAPE, not the daemon default's: not forcing a
	// posture on the trust root is the entire reason the policy is blank, so
	// picking up auto-approval here would be the opposite of the intent.
	for _, forbidden := range []string{copilotFlagAllowAllTools, copilotFlagNoAskUser} {
		if slices.Contains(fresh, forbidden) {
			t.Fatalf("a blank policy must not auto-approve anything: %v contains %s", fresh, forbidden)
		}
	}

	// End to end through the production spawner, which is where the argv the
	// pane actually receives is built.
	cmd := copilotSpawner{}.BuildCommand(SpawnSpec{SandboxWriteDirs: dirs})
	for _, dir := range dirs {
		if !strings.Contains(cmd, copilotFlagAddDir+" "+dir) {
			t.Fatalf("a blank-policy launch dropped grant %s: %s", dir, cmd)
		}
	}
	if strings.Contains(cmd, copilotFlagAllowAllTools) {
		t.Fatalf("a blank-policy launch must not auto-approve tools: %s", cmd)
	}

	// The add-dir gate and the renderer must still agree on this arm, or the
	// gate would refuse a launch citing a root the launch never emitted.
	if err := ValidateCopilotAddDirGrants(CopilotName, "/srv/cwd", "/srv/tmp",
		nil, dirs, []string{"/srv/data/secret"}, false); err == nil {
		t.Fatal("a deny inside a rendered grant must still refuse")
	}

	// An UNKNOWN token is a different case and must stay fail-closed: nothing
	// at all, grants included, because tclaude cannot say what posture the
	// launch would run under.
	if got := copilotPermissionArgs("allow-all", dirs); len(got) != 0 {
		t.Fatalf("an unknown token must render nothing, got %v", got)
	}
}
